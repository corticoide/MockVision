package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/buildinfo"
	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
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
	if _, err := s.store.R().GetTarget(ctx, id); err != nil {
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
	s.audit(ctx, actor, "target.delete", "target", id, nil)
	return nil
}

// TargetTestResult is the outcome of a connection test.
type TargetTestResult struct {
	OK         bool   `json:"ok"`
	HTTPStatus int    `json:"http_status,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
	Error      string `json:"error,omitempty"`
}

// TestTarget sends a test request from the node to a target.
func (s *Service) TestTarget(ctx context.Context, id string) (*TargetTestResult, error) {
	r, err := s.store.R().GetTarget(ctx, id)
	if err != nil {
		return nil, store.NotFound(err)
	}
	var cfg targetConfig
	_ = json.Unmarshal([]byte(r.ConfigJson), &cfg)
	body, _ := json.Marshal(map[string]any{"test": true, "source": "MockVision", "target": r.Name, "at": time.Now().UTC()})
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, cfg.Method, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return &TargetTestResult{Error: err.Error()}, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	if cfg.Username != "" {
		pw := ""
		if len(r.SecretEnc) > 0 {
			if p, err := s.box.Open(r.SecretEnc, "targets:"+id); err == nil {
				pw = string(p)
			}
		}
		req.SetBasicAuth(cfg.Username, pw)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start := time.Now()
	resp, err := client.Do(req)
	res := &TargetTestResult{LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		res.Error = err.Error()
		return res, nil
	}
	resp.Body.Close()
	res.HTTPStatus = resp.StatusCode
	res.OK = resp.StatusCode >= 200 && resp.StatusCode < 300
	if !res.OK {
		res.Error = fmt.Sprintf("target answered %d", resp.StatusCode)
	}
	return res, nil
}
