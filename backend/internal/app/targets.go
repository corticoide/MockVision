package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/buildinfo"
	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
	"github.com/corticoide/mockvision/sdk/engine"
)

// targetConfig is stored in targets.config_json; the password lives
// encrypted in secret_enc and never comes back from the API.
type targetConfig struct {
	URL      string            `json:"url"`
	Method   string            `json:"method"`
	Headers  map[string]string `json:"headers,omitempty"`
	Username string            `json:"username,omitempty"`
}

// TargetInput creates or changes a target.
type TargetInput struct {
	Name     *string           `json:"name"`
	Type     string            `json:"type"`
	URL      *string           `json:"url"`
	Method   *string           `json:"method"`
	Headers  map[string]string `json:"headers"`
	Username *string           `json:"username"`
	Password *string           `json:"password"`
	Enabled  *bool             `json:"enabled"`
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// CreateTarget stores a reusable event receiver (D45).
func (s *Service) CreateTarget(ctx context.Context, actor Actor, in TargetInput) (*TargetView, error) {
	if in.Type == "" {
		in.Type = string(domain.TargetHTTP)
	}
	method := strings.ToUpper(str(in.Method))
	if method == "" {
		method = http.MethodPost
	}
	t := domain.Target{Name: strings.TrimSpace(str(in.Name)), Type: domain.TargetType(in.Type), URL: strings.TrimSpace(str(in.URL)), Method: method, Headers: in.Headers}
	if err := domain.ValidateTarget(t); err != nil {
		return nil, err
	}
	id := ulid.Make().String()
	cfg, _ := json.Marshal(targetConfig{URL: t.URL, Method: t.Method, Headers: t.Headers, Username: str(in.Username)})
	var secret []byte
	if pw := str(in.Password); pw != "" {
		secret = s.box.Seal([]byte(pw), "targets:"+id)
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	err := s.store.W().InsertTarget(ctx, db.InsertTargetParams{ID: id, Name: t.Name, Type: string(t.Type), ConfigJson: string(cfg), SecretEnc: secret, Enabled: store.Int(enabled), CreatedAt: time.Now().UnixMilli()})
	if err != nil {
		if store.IsUnique(err) {
			return nil, domain.Conflict("name", "a target named %q already exists", t.Name)
		}
		return nil, err
	}
	s.audit(ctx, actor, "target.create", "target", id, map[string]any{"name": t.Name, "url": t.URL})
	return s.GetTarget(ctx, id)
}

// ListTargets returns every target.
func (s *Service) ListTargets(ctx context.Context) ([]TargetView, error) {
	rows, err := s.store.R().ListTargets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]TargetView, 0, len(rows))
	for _, r := range rows {
		out = append(out, targetView(db.Target{ID: r.ID, Name: r.Name, Type: r.Type, ConfigJson: r.ConfigJson, SecretEnc: r.SecretEnc, Enabled: r.Enabled, CreatedAt: r.CreatedAt}, r.CameraCount))
	}
	return out, nil
}

// GetTarget returns one target.
func (s *Service) GetTarget(ctx context.Context, id string) (*TargetView, error) {
	r, err := s.store.R().GetTarget(ctx, id)
	if err != nil {
		return nil, store.NotFound(err)
	}
	cams, _ := s.store.R().CamerasUsingTarget(ctx, id)
	v := targetView(r, int64(len(cams)))
	return &v, nil
}

func targetView(r db.Target, cameras int64) TargetView {
	var cfg targetConfig
	_ = json.Unmarshal([]byte(r.ConfigJson), &cfg)
	if cfg.Headers == nil {
		cfg.Headers = map[string]string{}
	}
	return TargetView{ID: r.ID, Name: r.Name, Type: r.Type, URL: cfg.URL, Method: cfg.Method, Headers: cfg.Headers,
		Username: cfg.Username, HasPassword: len(r.SecretEnc) > 0, Enabled: store.Bool(r.Enabled), CameraCount: int(cameras), CreatedAt: store.Time(r.CreatedAt)}
}

// UpdateTarget changes a target; running cameras get it immediately.
func (s *Service) UpdateTarget(ctx context.Context, actor Actor, id string, in TargetInput) (*TargetView, error) {
	r, err := s.store.R().GetTarget(ctx, id)
	if err != nil {
		return nil, store.NotFound(err)
	}
	var cfg targetConfig
	_ = json.Unmarshal([]byte(r.ConfigJson), &cfg)
	name := r.Name
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
	}
	if in.URL != nil {
		cfg.URL = strings.TrimSpace(*in.URL)
	}
	if in.Method != nil {
		cfg.Method = strings.ToUpper(*in.Method)
	}
	if in.Headers != nil {
		cfg.Headers = in.Headers
	}
	if in.Username != nil {
		cfg.Username = *in.Username
	}
	enabled := store.Bool(r.Enabled)
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	if err := domain.ValidateTarget(domain.Target{Name: name, Type: domain.TargetType(r.Type), URL: cfg.URL, Method: cfg.Method, Headers: cfg.Headers}); err != nil {
		return nil, err
	}
	secret := r.SecretEnc
	if in.Password != nil {
		secret = nil
		if *in.Password != "" {
			secret = s.box.Seal([]byte(*in.Password), "targets:"+id)
		}
	}
	raw, _ := json.Marshal(cfg)
	if err := s.store.W().UpdateTarget(ctx, db.UpdateTargetParams{ID: id, Name: name, ConfigJson: string(raw), SecretEnc: secret, Enabled: store.Int(enabled)}); err != nil {
		if store.IsUnique(err) {
			return nil, domain.Conflict("name", "a target named %q already exists", name)
		}
		return nil, err
	}
	s.audit(ctx, actor, "target.update", "target", id, map[string]any{"name": name, "url": cfg.URL, "enabled": enabled})
	cams, _ := s.store.R().CamerasUsingTarget(ctx, id)
	for _, c := range cams {
		s.reloadTargets(ctx, c)
	}
	return s.GetTarget(ctx, id)
}

// DeleteTarget removes an unused target (RN-12: targets in use are
// disabled instead).
func (s *Service) DeleteTarget(ctx context.Context, actor Actor, id string) error {
	t, err := s.store.R().GetTarget(ctx, id)
	if err != nil {
		return store.NotFound(err)
	}
	if cams, _ := s.store.R().CamerasUsingTarget(ctx, id); len(cams) > 0 {
		return domain.Conflict("", "the target is used by %d cameras; disable it or unlink it first", len(cams))
	}
	if _, err := s.store.W().DeleteTarget(ctx, id); err != nil {
		if store.IsForeignKey(err) {
			return domain.Conflict("", "the target is in use")
		}
		return err
	}
	s.audit(ctx, actor, "target.delete", "target", id, map[string]string{"name": t.Name})
	return nil
}

// TargetTestResult is the outcome of a connection test.
type TargetTestResult struct {
	OK         bool   `json:"ok"`
	HTTPStatus int    `json:"http_status,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
	Error      string `json:"error,omitempty"`
	// From says where the request left: "camera" (a running camera that
	// uses the target, as its deliveries do) or "node".
	From string `json:"from"`
	// Camera names the camera the request left from.
	Camera string `json:"camera,omitempty"`
}

// errTargetOnNode refuses a test from the node to the node itself.
var errTargetOnNode = errors.New("the address is the node itself or a link-local one; cameras cannot reach it either")

// TestTarget sends a test request to a target. It leaves from a running
// camera that uses the target, so it crosses the same network as the
// deliveries; with none running it leaves from the node, which then
// refuses its own addresses: a camera could not reach them, and they are
// the node's internal services (audit B7).
func (s *Service) TestTarget(ctx context.Context, id string) (*TargetTestResult, error) {
	r, err := s.store.R().GetTarget(ctx, id)
	if err != nil {
		return nil, store.NotFound(err)
	}
	var cfg targetConfig
	_ = json.Unmarshal([]byte(r.ConfigJson), &cfg)
	pw := ""
	if len(r.SecretEnc) > 0 {
		if p, err := s.box.Open(r.SecretEnc, "targets:"+id); err == nil {
			pw = string(p)
		}
	}
	target := engine.Target{ID: r.ID, Name: r.Name, Type: r.Type, URL: cfg.URL, Method: cfg.Method, Headers: cfg.Headers, Username: cfg.Username, Password: pw}

	cams, _ := s.store.R().CamerasUsingTarget(ctx, id)
	for _, c := range cams {
		ss := s.session(c)
		if ss == nil || !ss.active() {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		var res ipc.TargetTestResult
		err := ss.conn().Request(cctx, ipc.TypeTargetTest, ipc.TargetTest{Target: target}, &res)
		cancel()
		if err != nil {
			continue // a camera too old or busy: try the next one
		}
		return &TargetTestResult{OK: res.OK, HTTPStatus: res.HTTPStatus, LatencyMS: res.LatencyMS, Error: res.Error, From: "camera", Camera: ss.name}, nil
	}
	return s.testFromNode(ctx, r.Name, target), nil
}

func (s *Service) testFromNode(ctx context.Context, name string, t engine.Target) *TargetTestResult {
	res := &TargetTestResult{From: "node"}
	body, _ := json.Marshal(map[string]any{"test": true, "source": "MockVision", "target": name, "at": time.Now().UTC()})
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, t.Method, t.URL, bytes.NewReader(body))
	if err != nil {
		res.Error = err.Error()
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
	if t.Username != "" {
		req.SetBasicAuth(t.Username, t.Password)
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if s.rt.Kind() != "local" {
		// Checked on the address actually dialed, after name resolution,
		// so a name that resolves to the node is refused too.
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return err
			}
			if ip = ip.Unmap(); nodeAddress(ip) {
				return errTargetOnNode
			}
			return nil
		}
	}
	client := &http.Client{
		Transport:     &http.Transport{Proxy: nil, DialContext: dialer.DialContext},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	start := time.Now()
	resp, err := client.Do(req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		if errors.Is(err, errTargetOnNode) {
			res.Error = errTargetOnNode.Error()
		} else {
			res.Error = err.Error()
		}
		return res
	}
	resp.Body.Close()
	res.HTTPStatus = resp.StatusCode
	res.OK = resp.StatusCode >= 200 && resp.StatusCode < 300
	if !res.OK {
		res.Error = fmt.Sprintf("target answered %d", resp.StatusCode)
	}
	return res
}

// nodeAddress reports whether ip is the node itself: loopback, an address
// of one of its interfaces, or one no LAN device has (unspecified,
// link-local, multicast).
func nodeAddress(ip netip.Addr) bool {
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsLinkLocalMulticast() {
		return true
	}
	info, err := nodeInfo()
	if err != nil {
		return false
	}
	for _, i := range info.Interfaces {
		for _, a := range i.Addrs {
			if p, err := netip.ParsePrefix(a); err == nil && p.Addr().Unmap() == ip {
				return true
			}
		}
	}
	return false
}
