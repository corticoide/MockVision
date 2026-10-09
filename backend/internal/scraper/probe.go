package scraper

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/corticoide/mockvision/backend/internal/digest"
)

// readOnlyHTTP are the only HTTP methods the scraper ever sends: none of
// them changes a device (RN-17).
var readOnlyHTTP = map[string]bool{http.MethodGet: true, http.MethodHead: true, http.MethodOptions: true}

// Limits of a probe: a device is read, never stressed (D72, RN-17).
const (
	// DefaultRate is how many requests a second a device gets.
	DefaultRate = 5
	dialTimeout = 3 * time.Second
	readTimeout = 8 * time.Second
	// MaxBody bounds a recorded response body.
	MaxBody = 1 << 20
)

// ErrWriteMethod is returned when something asks for a method that is not
// read-only. It can never happen through the program format, which only
// offers read steps; it guards the primitive itself.
var ErrWriteMethod = errors.New("the scraper only sends read-only requests")

// Prober runs read-only probes against one device, paced by a rate limit.
// It records nothing by itself; a Recorder, when set, keeps each
// request and response as a sanitized fixture (feature 19).
type Prober struct {
	t     Target
	httpc *http.Client
	lim   *limiter
	rec   *Recorder
}

// NewProber returns a prober for a target at the given requests a second.
func NewProber(t Target, perSecond float64) *Prober {
	if perSecond <= 0 {
		perSecond = DefaultRate
	}
	return &Prober{
		t:   t,
		lim: newLimiter(perSecond),
		httpc: &http.Client{
			Timeout: readTimeout,
			// A probe never follows a redirect off the device by itself.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: dialTimeout}).DialContext,
				TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, // reading a self-signed camera cert
				MaxIdleConnsPerHost: 2,
				DisableKeepAlives:   false,
			},
		},
	}
}

// SetRecorder attaches a recorder so probes are kept as fixtures.
func (p *Prober) SetRecorder(r *Recorder) { p.rec = r }

// Detect looks at the device read-only: which ports answer and what the
// services say they are. It changes nothing.
func (p *Prober) Detect(ctx context.Context) Detected {
	d := Detected{At: time.Now().UTC()}
	var banners []string
	for _, port := range p.t.ports() {
		if ctx.Err() != nil {
			break
		}
		open, svc := p.identify(ctx, port)
		if !open {
			continue
		}
		d.OpenPorts = append(d.OpenPorts, port)
		d.Services = append(d.Services, svc)
		banners = append(banners, svc.Server)
	}
	sort.Ints(d.OpenPorts)
	d.Reachable = len(d.OpenPorts) > 0
	d.Vendor = guessVendor(banners...)
	return d
}

// identify connects to a port and reads what it is, read-only.
func (p *Prober) identify(ctx context.Context, port int) (bool, Service) {
	if err := p.lim.wait(ctx); err != nil {
		return false, Service{}
	}
	addr := net.JoinHostPort(p.t.Host, fmt.Sprint(port))
	conn, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return false, Service{}
	}
	conn.Close()
	svc := Service{Port: port, Proto: "unknown"}
	switch port {
	case 554, 8554:
		if r, ok := p.rtspOptions(ctx, port); ok {
			svc.Proto, svc.Server = "rtsp", r
		}
	case 443, 8443:
		if cn, ok := p.tlsPeek(ctx, port); ok {
			svc.Proto, svc.Note = "https", cn
		}
	case 161:
		svc.Proto, svc.Note = "snmp", "no SNMP engine yet; the port is open"
	default:
		if server, auth, ok := p.httpPeek(ctx, port); ok {
			svc.Proto, svc.Server, svc.Auth = "http", server, auth
		}
	}
	return true, svc
}

// httpPeek does an unauthenticated GET / to read the server banner and the
// authentication scheme, without touching anything.
func (p *Prober) httpPeek(ctx context.Context, port int) (server, auth string, ok bool) {
	resp, _, err := p.HTTP(ctx, http.MethodGet, port, "/", false)
	if err != nil {
		return "", "", false
	}
	server = resp.Header.Get("Server")
	if resp.Status == http.StatusUnauthorized {
		if _, ok := digest.ParseChallenge(resp.Header.Values("WWW-Authenticate")); ok {
			auth = "digest"
		} else if len(resp.Header.Values("WWW-Authenticate")) > 0 {
			auth = "basic"
		}
	}
	return server, auth, true
}

// Response is the recordable result of an HTTP probe.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// HTTP sends a read-only HTTP request to the device and returns the
// response. With auth it answers a Digest or Basic challenge using the
// device's credentials. The body is bounded to MaxBody.
func (p *Prober) HTTP(ctx context.Context, method string, port int, path string, auth bool) (*Response, string, error) {
	if !readOnlyHTTP[method] {
		return nil, "", ErrWriteMethod
	}
	if err := p.lim.wait(ctx); err != nil {
		return nil, "", err
	}
	scheme := "http"
	if port == 443 || port == 8443 {
		scheme = "https"
	}
	url := fmt.Sprintf("%s://%s%s", scheme, net.JoinHostPort(p.t.Host, fmt.Sprint(port)), path)
	resp, err := p.do(ctx, method, url, "")
	if err != nil {
		return nil, url, err
	}
	// One Digest/Basic round, only for an authenticated read.
	if auth && resp.StatusCode == http.StatusUnauthorized && p.t.Username != "" {
		hdr := resp.Header.Values("WWW-Authenticate")
		resp.Body.Close()
		var authz string
		if ch, ok := digest.ParseChallenge(hdr); ok {
			authz, _ = ch.Authorize(method, path, p.t.Username, p.t.Password, nil)
		} else if len(hdr) > 0 {
			authz = basic(p.t.Username, p.t.Password)
		}
		if err := p.lim.wait(ctx); err != nil {
			return nil, url, err
		}
		if resp, err = p.do(ctx, method, url, authz); err != nil {
			return nil, url, err
		}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
	out := &Response{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: body}
	if p.rec != nil {
		p.rec.http(method, url, out)
	}
	return out, url, nil
}

func (p *Prober) do(ctx context.Context, method, url, authz string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "MockVision-Scraper/1 (read-only)")
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	return p.httpc.Do(req)
}

// rtspOptions sends RTSP OPTIONS, which lists what the server supports and
// changes nothing, and returns its Server header.
func (p *Prober) rtspOptions(ctx context.Context, port int) (string, bool) {
	line, hdr, ok := p.rtsp(ctx, "OPTIONS", port, "/")
	if !ok {
		return "", false
	}
	_ = line
	return hdr.Get("Server"), true
}

// RTSPResult is a recordable RTSP exchange.
type RTSPResult struct {
	Status int
	Header map[string]string
	Body   string // the SDP of a DESCRIBE, if any
}

// Describe asks the SDP of a stream with RTSP DESCRIBE, a read-only
// request. It answers one authentication challenge with the device's
// credentials.
func (p *Prober) Describe(ctx context.Context, port int, path string) (*RTSPResult, bool) {
	status, hdr, body, ok := p.rtspExchange(ctx, "DESCRIBE", port, path, true)
	if !ok {
		return nil, false
	}
	m := map[string]string{}
	for k := range hdr {
		m[k] = hdr.Get(k)
	}
	res := &RTSPResult{Status: status, Header: m, Body: body}
	if p.rec != nil {
		p.rec.rtsp("DESCRIBE", fmt.Sprintf("rtsp://%s%s", net.JoinHostPort(p.t.Host, fmt.Sprint(port)), path), res)
	}
	return res, true
}

func (p *Prober) rtsp(ctx context.Context, method string, port int, path string) (int, http.Header, bool) {
	s, h, _, ok := p.rtspExchange(ctx, method, port, path, false)
	return s, h, ok
}

// rtspExchange speaks enough RTSP to send OPTIONS or DESCRIBE and read the
// answer; it opens a fresh connection and never sends a method that
// changes state.
func (p *Prober) rtspExchange(ctx context.Context, method string, port int, path string, auth bool) (int, http.Header, string, bool) {
	if method != "OPTIONS" && method != "DESCRIBE" {
		return 0, nil, "", false
	}
	if err := p.lim.wait(ctx); err != nil {
		return 0, nil, "", false
	}
	conn, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", net.JoinHostPort(p.t.Host, fmt.Sprint(port)))
	if err != nil {
		return 0, nil, "", false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(readTimeout))
	uri := fmt.Sprintf("rtsp://%s%s", net.JoinHostPort(p.t.Host, fmt.Sprint(port)), path)
	br := bufio.NewReader(conn)
	send := func(cseq int, authz string) error {
		b := &strings.Builder{}
		fmt.Fprintf(b, "%s %s RTSP/1.0\r\nCSeq: %d\r\nUser-Agent: MockVision-Scraper/1 (read-only)\r\n", method, uri, cseq)
		if method == "DESCRIBE" {
			b.WriteString("Accept: application/sdp\r\n")
		}
		if authz != "" {
			fmt.Fprintf(b, "Authorization: %s\r\n", authz)
		}
		b.WriteString("\r\n")
		_, err := conn.Write([]byte(b.String()))
		return err
	}
	if send(1, "") != nil {
		return 0, nil, "", false
	}
	status, hdr, body := readRTSP(br)
	if auth && status == 401 && p.t.Username != "" {
		var authz string
		if ch, ok := digest.ParseChallenge(hdr.Values("WWW-Authenticate")); ok {
			authz, _ = ch.Authorize(method, uri, p.t.Username, p.t.Password, nil)
		} else if len(hdr.Values("WWW-Authenticate")) > 0 {
			authz = basic(p.t.Username, p.t.Password)
		}
		if send(2, authz) == nil {
			status, hdr, body = readRTSP(br)
		}
	}
	return status, hdr, body, status != 0
}

// readRTSP reads an RTSP response: status, headers and a body of
// Content-Length bytes (an SDP).
func readRTSP(br *bufio.Reader) (int, http.Header, string) {
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return 0, nil, ""
	}
	var ver string
	var code int
	if _, err := fmt.Sscanf(statusLine, "%s %d", &ver, &code); err != nil {
		return 0, nil, ""
	}
	hdr := http.Header{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			hdr.Add(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
	body := ""
	if n := hdr.Get("Content-Length"); n != "" {
		var length int
		_, _ = fmt.Sscanf(n, "%d", &length)
		if length > 0 && length < MaxBody {
			buf := make([]byte, length)
			if _, err := io.ReadFull(br, buf); err == nil {
				body = string(buf)
			}
		}
	}
	return code, hdr, body
}

// tlsPeek reads the certificate of a TLS port, read-only.
func (p *Prober) tlsPeek(ctx context.Context, port int) (string, bool) {
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: dialTimeout}, Config: &tls.Config{InsecureSkipVerify: true}}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(p.t.Host, fmt.Sprint(port)))
	if err != nil {
		return "", false
	}
	defer conn.Close()
	cs := conn.(*tls.Conn).ConnectionState()
	if len(cs.PeerCertificates) == 0 {
		return "certificate present", true
	}
	return "subject " + cs.PeerCertificates[0].Subject.CommonName, true
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// limiter paces probes at a steady rate per device, with a small burst so
// a capture never floods the equipment it reads (D72, RN-17).
type limiter struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	at     time.Time
}

func newLimiter(perSecond float64) *limiter {
	burst := minf(perSecond, 2)
	return &limiter{rate: perSecond, burst: burst, tokens: burst, at: time.Now()}
}

func (l *limiter) wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		now := time.Now()
		l.tokens = minf(l.burst, l.tokens+now.Sub(l.at).Seconds()*l.rate)
		l.at = now
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		wait := time.Duration((1 - l.tokens) / l.rate * float64(time.Second))
		l.mu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
