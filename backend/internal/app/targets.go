package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/buildinfo"
	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/engines/delivery"
	"github.com/corticoide/mockvision/backend/internal/engines/probe"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/netctl"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
	"github.com/corticoide/mockvision/backend/internal/tmpl"
	"github.com/corticoide/mockvision/sdk/engine"
)

// targetConfig is stored in targets.config_json; the password lives
// encrypted in secret_enc and never comes back from the API. Which fields
// are set depends on the target's type.
type targetConfig struct {
	URL      string                   `json:"url"`
	Method   string                   `json:"method,omitempty"`
	Headers  map[string]string        `json:"headers,omitempty"`
	Username string                   `json:"username,omitempty"`
	Auth     string                   `json:"auth,omitempty"`
	Topic    string                   `json:"topic,omitempty"`
	ClientID string                   `json:"client_id,omitempty"`
	HostKey  string                   `json:"host_key,omitempty"`
	TLS      string                   `json:"tls,omitempty"`
	From     string                   `json:"from,omitempty"`
	To       []string                 `json:"to,omitempty"`
	Insecure bool                     `json:"insecure,omitempty"`
	Delivery *domain.DeliveryOverride `json:"delivery,omitempty"`
}

// TargetInput creates or changes a target. Absent fields keep their value
// on a change.
type TargetInput struct {
	Name     *string           `json:"name"`
	Type     string            `json:"type"`
	URL      *string           `json:"url"`
	Method   *string           `json:"method"`
	Headers  map[string]string `json:"headers"`
	Username *string           `json:"username"`
	Password *string           `json:"password"`
	Enabled  *bool             `json:"enabled"`
	Auth     *string           `json:"auth"`
	Topic    *string           `json:"topic"`
	ClientID *string           `json:"client_id"`
	HostKey  *string           `json:"host_key"`
	TLS      *string           `json:"tls"`
	From     *string           `json:"from"`
	To       *[]string         `json:"to"`
	Insecure *bool             `json:"insecure"`
	// Delivery replaces the target's override as a whole; an empty object
	// goes back to the profile's policy.
	Delivery *domain.DeliveryOverride `json:"delivery"`
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// apply writes the input's fields into a stored configuration.
func (in TargetInput) apply(cfg *targetConfig) {
	set := func(dst *string, src *string, upper bool) {
		if src == nil {
			return
		}
		v := strings.TrimSpace(*src)
		if upper {
			v = strings.ToUpper(v)
		}
		*dst = v
	}
	set(&cfg.URL, in.URL, false)
	set(&cfg.Method, in.Method, true)
	set(&cfg.Username, in.Username, false)
	set(&cfg.Auth, in.Auth, false)
	set(&cfg.Topic, in.Topic, false)
	set(&cfg.ClientID, in.ClientID, false)
	set(&cfg.HostKey, in.HostKey, false)
	set(&cfg.TLS, in.TLS, false)
	set(&cfg.From, in.From, false)
	if in.Headers != nil {
		cfg.Headers = in.Headers
	}
	if in.To != nil {
		cfg.To = nil
		for _, a := range *in.To {
			if a = strings.TrimSpace(a); a != "" {
				cfg.To = append(cfg.To, a)
			}
		}
	}
	if in.Insecure != nil {
		cfg.Insecure = *in.Insecure
	}
	if in.Delivery != nil {
		cfg.Delivery = in.Delivery
		if cfg.Delivery.Empty() {
			cfg.Delivery = nil
		}
	}
}

// check validates a target with its configuration, templates included.
func (cfg *targetConfig) check(name, typ string) error {
	t := domain.Target{Name: name, Type: domain.TargetType(typ), URL: cfg.URL, Method: cfg.Method, Headers: cfg.Headers, Username: cfg.Username,
		Auth: cfg.Auth, Topic: cfg.Topic, ClientID: cfg.ClientID, HostKey: cfg.HostKey, TLS: cfg.TLS, From: cfg.From, To: cfg.To,
		Insecure: cfg.Insecure, Delivery: cfg.Delivery}
	if err := domain.ValidateTarget(t); err != nil {
		return err
	}
	for field, text := range map[string]string{"topic": cfg.Topic, "client_id": cfg.ClientID} {
		if text == "" {
			continue
		}
		if err := tmpl.Check(field, text); err != nil {
			return domain.Invalid(field, "%v", err)
		}
	}
	return nil
}

// CreateTarget stores a reusable event receiver (D45).
func (s *Service) CreateTarget(ctx context.Context, actor Actor, in TargetInput) (*TargetView, error) {
	if in.Type == "" {
		in.Type = string(domain.TargetHTTP)
	}
	var cfg targetConfig
	if in.Type == string(domain.TargetHTTP) {
		cfg.Method = http.MethodPost
	}
	in.apply(&cfg)
	name := strings.TrimSpace(str(in.Name))
	if err := cfg.check(name, in.Type); err != nil {
		return nil, err
	}
	id := ulid.Make().String()
	raw, _ := json.Marshal(cfg)
	var secret []byte
	if pw := str(in.Password); pw != "" {
		secret = s.box.Seal([]byte(pw), "targets:"+id)
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	err := s.store.W().InsertTarget(ctx, db.InsertTargetParams{ID: id, Name: name, Type: in.Type, ConfigJson: string(raw), SecretEnc: secret, Enabled: store.Int(enabled), CreatedAt: time.Now().UnixMilli()})
	if err != nil {
		if store.IsUnique(err) {
			return nil, domain.Conflict("name", "a target named %q already exists", name)
		}
		return nil, err
	}
	s.audit(ctx, actor, "target.create", "target", id, map[string]any{"name": name, "type": in.Type, "url": cfg.URL})
	s.refreshFirewallsLater()
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
	if cfg.To == nil {
		cfg.To = []string{}
	}
	return TargetView{ID: r.ID, Name: r.Name, Type: r.Type, URL: cfg.URL, Method: cfg.Method, Headers: cfg.Headers,
		Username: cfg.Username, HasPassword: len(r.SecretEnc) > 0, Enabled: store.Bool(r.Enabled), CameraCount: int(cameras), CreatedAt: store.Time(r.CreatedAt),
		Auth: cfg.Auth, Topic: cfg.Topic, ClientID: cfg.ClientID, HostKey: cfg.HostKey, TLS: cfg.TLS, From: cfg.From, To: cfg.To,
		Insecure: cfg.Insecure, Delivery: cfg.Delivery}
}

// UpdateTarget changes a target; running cameras get it immediately.
func (s *Service) UpdateTarget(ctx context.Context, actor Actor, id string, in TargetInput) (*TargetView, error) {
	r, err := s.store.R().GetTarget(ctx, id)
	if err != nil {
		return nil, store.NotFound(err)
	}
	if in.Type != "" && in.Type != r.Type {
		return nil, domain.Invalid("type", "cannot change from %s; create another target", r.Type)
	}
	var cfg targetConfig
	_ = json.Unmarshal([]byte(r.ConfigJson), &cfg)
	name := r.Name
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
	}
	in.apply(&cfg)
	enabled := store.Bool(r.Enabled)
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	if err := cfg.check(name, r.Type); err != nil {
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
	s.refreshFirewallsLater()
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
	s.refreshFirewallsLater()
	return nil
}

// engineTarget is a stored target as engines see it, with its secret.
func (s *Service) engineTarget(id, name, typ, configJSON string, secretEnc []byte) (engine.Target, error) {
	var cfg targetConfig
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return engine.Target{}, err
	}
	var pw string
	if len(secretEnc) > 0 {
		p, err := s.box.Open(secretEnc, "targets:"+id)
		if err != nil {
			return engine.Target{}, errors.New("cannot decrypt the secret of target " + name)
		}
		pw = string(p)
	}
	t := engine.Target{ID: id, Name: name, Type: typ, URL: cfg.URL, Method: cfg.Method, Headers: cfg.Headers, Username: cfg.Username, Password: pw,
		Auth: cfg.Auth, Topic: cfg.Topic, ClientID: cfg.ClientID, HostKey: cfg.HostKey, TLS: cfg.TLS, Insecure: cfg.Insecure, From: cfg.From, To: cfg.To}
	if o := cfg.Delivery; !o.Empty() {
		t.Delivery = &engine.DeliveryOverride{Retries: o.Retries}
		if o.TimeoutMS != nil {
			d := time.Duration(*o.TimeoutMS) * time.Millisecond
			t.Delivery.Timeout = &d
		}
		if o.BackoffMS != nil {
			d := time.Duration(*o.BackoffMS) * time.Millisecond
			t.Delivery.Backoff = &d
		}
	}
	return t, nil
}

// TargetTestResult is the outcome of a connection test.
type TargetTestResult struct {
	OK         bool   `json:"ok"`
	HTTPStatus int    `json:"http_status,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
	Error      string `json:"error,omitempty"`
	// From says where the test left: "camera" (a running camera that uses
	// the target, as its deliveries do) or "node".
	From string `json:"from"`
	// Camera names the camera the test left from.
	Camera string `json:"camera,omitempty"`
}

// errTargetOnNode refuses a test from the node to the node itself.
var errTargetOnNode = errors.New("the address is the node itself or a link-local one; cameras cannot reach it either")

// TestTarget tests a target: a request to an http target, a session with
// an MQTT broker, a login on an FTP or SFTP server, the sender and
// recipients on a mail server. It leaves from a running camera that uses
// the target, so it crosses the same network as the deliveries; with none
// running it leaves from the node, which then refuses its own addresses: a
// camera could not reach them, and they are the node's internal services
// (audit B7).
func (s *Service) TestTarget(ctx context.Context, id string) (*TargetTestResult, error) {
	r, err := s.store.R().GetTarget(ctx, id)
	if err != nil {
		return nil, store.NotFound(err)
	}
	target, err := s.engineTarget(r.ID, r.Name, r.Type, r.ConfigJson, r.SecretEnc)
	if err != nil {
		return nil, err
	}
	cams, _ := s.store.R().CamerasUsingTarget(ctx, id)
	for _, c := range cams {
		ss := s.session(c)
		if ss == nil || !ss.active() {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
		var res ipc.TargetTestResult
		err := ss.conn().Request(cctx, ipc.TypeTargetTest, ipc.TargetTest{Target: target}, &res)
		cancel()
		if err != nil {
			continue // a camera too old or busy: try the next one
		}
		return &TargetTestResult{OK: res.OK, HTTPStatus: res.HTTPStatus, LatencyMS: res.LatencyMS, Error: res.Error, From: "camera", Camera: ss.name}, nil
	}
	return s.testFromNode(ctx, target), nil
}

func (s *Service) testFromNode(ctx context.Context, t engine.Target) *TargetTestResult {
	res := &TargetTestResult{From: "node"}
	body, _ := json.Marshal(map[string]any{"test": true, "source": "MockVision", "target": t.Name, "at": time.Now().UTC()})
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if s.rt.Kind() != netctl.KindLocal {
		// Checked on the address actually dialed, after name resolution,
		// so a name that resolves to the node is refused too; FTP data
		// connections and SMTP alike.
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
	start := time.Now()
	status, err := probe.Target(delivery.WithDialer(ctx, dialer), t, probe.Info{Body: body, UserAgent: buildinfo.UserAgent(), ClientID: "mockvision-test-node"})
	res.LatencyMS = time.Since(start).Milliseconds()
	res.HTTPStatus = status
	if err != nil {
		if errors.Is(err, errTargetOnNode) {
			res.Error = errTargetOnNode.Error()
		} else {
			res.Error = err.Error()
		}
		return res
	}
	res.OK = true
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

// targetPorts are the TCP ports a camera opens toward a target: the port
// of its URL, or its protocol's; an FTP server also takes passive data
// connections on ports it chooses, so its host is open on any port.
func targetPorts(typ, configJSON string) (host string, ports []int, ok bool) {
	var cfg targetConfig
	if json.Unmarshal([]byte(configJSON), &cfg) != nil {
		return "", nil, false
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Hostname() == "" {
		return "", nil, false
	}
	defaults := map[string]int{"http": 80, "https": 443, "mqtt": 1883, "mqtts": 8883, "ftp": 21, "sftp": 22, "smtp": 25, "smtps": 465}
	port, known := defaults[u.Scheme]
	if !known {
		return "", nil, false
	}
	if typ == string(domain.TargetSMTP) && cfg.TLS == domain.TLSImplicit {
		port = 465
	}
	if p := u.Port(); p != "" {
		if port, err = strconv.Atoi(p); err != nil {
			return "", nil, false
		}
	}
	ports = []int{port}
	if typ == string(domain.TargetFTP) {
		ports = append(ports, netctl.AnyPort)
	}
	return u.Hostname(), ports, true
}
