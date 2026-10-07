// Package httppush implements the http-push engine: it delivers events to
// HTTP targets with the payload the profile's template builds, using the
// device's own timeout and retry behavior. A target authenticates the
// camera with Basic, or with Digest, answering the target's challenge.
package httppush

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/corticoide/mockvision/backend/internal/engines/delivery"
	"github.com/corticoide/mockvision/sdk/engine"
)

// Name and Version identify the engine in profiles (http-push@^1).
const (
	Name    = "http-push"
	Version = "1.1.0"
)

// Transport is the name of the transport this engine delivers.
const Transport = engine.TransportHTTPPush

// Auth schemes toward a target.
const (
	AuthBasic  = "basic"
	AuthDigest = "digest"
)

// transportConfig is the http_push section of an event.
type transportConfig struct {
	Method      string            `json:"method"`
	ContentType string            `json:"content_type"`
	Headers     map[string]string `json:"headers"`
	Body        string            `json:"body"`
	Image       bool              `json:"image"`
}

const configSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["engine"],
  "properties": {
    "engine": {"type": "string"},
    "user_agent": {"type": "string", "maxLength": 200},
    "max_parallel": {"type": "integer", "minimum": 1, "maximum": 64}
  }
}`

type config struct {
	UserAgent   string `json:"user_agent"`
	MaxParallel int    `json:"max_parallel"`
}

// Engine delivers events of one camera.
type Engine struct {
	in        engine.StartInput
	cfg       config
	client    *http.Client
	runner    *delivery.Runner
	templates *delivery.Templates
	unsub     func()
	state     atomic.Value
}

// New returns an engine instance.
func New() engine.Engine {
	e := &Engine{}
	e.state.Store(engine.HealthStopped)
	return e
}

// Describe implements engine.Engine.
func (e *Engine) Describe() engine.Descriptor {
	return engine.Descriptor{
		Name:         Name,
		Version:      Version,
		Contract:     engine.Contract,
		Role:         engine.RoleClient,
		ConfigSchema: json.RawMessage(configSchema),
		Delivers:     []string{Transport},
	}
}

// Validate implements engine.Engine. Event templates are checked by the
// profile importer, which owns the events section.
func (e *Engine) Validate(json.RawMessage) []engine.Problem { return nil }

// Start implements engine.Engine.
func (e *Engine) Start(_ context.Context, in engine.StartInput) error {
	e.in = in
	if err := json.Unmarshal(in.Config, &e.cfg); err != nil {
		return err
	}
	if e.cfg.UserAgent == "" {
		e.cfg.UserAgent = strings.TrimSpace(in.Identity.Vendor + " " + in.Identity.Model)
	}
	e.client = NewClient()
	e.runner = delivery.NewRunner(in.Host.Events(), e.cfg.MaxParallel)
	e.templates = delivery.NewTemplates(in.Host.Templates())
	e.unsub = in.Host.Events().Subscribe(Transport, e.dispatch)
	e.state.Store(engine.HealthOK)
	return nil
}

// NewClient is the HTTP client of a camera toward its targets: direct,
// never through the node's proxy, and without following redirects.
func NewClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         delivery.Dial,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: 5 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Reload implements engine.Engine.
func (e *Engine) Reload(_ context.Context, raw json.RawMessage) error {
	var c config
	return json.Unmarshal(raw, &c)
}

// Health implements engine.Engine.
func (e *Engine) Health() engine.Health {
	if e.runner == nil {
		return engine.Health{State: engine.HealthStopped}
	}
	return e.runner.Health(e.state.Load().(engine.HealthState))
}

// Stop implements engine.Engine; pending deliveries are abandoned.
func (e *Engine) Stop(ctx context.Context) error {
	if e.unsub != nil {
		e.unsub()
	}
	if e.runner != nil {
		e.runner.Stop(ctx)
	}
	e.state.Store(engine.HealthStopped)
	return nil
}

func (e *Engine) dispatch(d engine.Dispatch) {
	var tc transportConfig
	if err := json.Unmarshal(d.Transport, &tc); err != nil {
		e.in.Host.Telemetry().Log(slog.LevelError, "http-push: invalid transport", "error", err)
		return
	}
	max := 0
	if tc.Image {
		max = 8 << 20
	}
	body, err := e.templates.Render(e.runner.Context(), Transport, tc.Body, max, delivery.Data(e.in.Identity, d))
	for _, target := range d.Targets {
		if target.Type != engine.TargetHTTP {
			continue
		}
		if err != nil {
			e.runner.Fail(d, target, fmt.Errorf("template: %w", err))
			continue
		}
		e.runner.Go(d, target, func(ctx context.Context, _ int) delivery.Attempt {
			return e.send(ctx, tc, target, body)
		})
	}
}

func (e *Engine) send(ctx context.Context, tc transportConfig, target engine.Target, body []byte) delivery.Attempt {
	method := target.Method
	if method == "" {
		method = tc.Method
	}
	if method == "" {
		method = http.MethodPost
	}
	headers := http.Header{}
	if tc.ContentType != "" && method != http.MethodGet {
		headers.Set("Content-Type", tc.ContentType)
	}
	headers.Set("User-Agent", e.cfg.UserAgent)
	for k, v := range tc.Headers {
		headers.Set(k, v)
	}
	for k, v := range target.Headers {
		headers.Set(k, v)
	}
	status, err := Do(ctx, e.client, method, target, headers, body)
	if err != nil {
		return delivery.Attempt{Status: status, Err: err}
	}
	if status < 200 || status > 299 {
		return delivery.Attempt{Status: status, Err: fmt.Errorf("target answered %d", status)}
	}
	return delivery.Attempt{Status: status, Bytes: len(body)}
}

// Do sends one request to an http target with its credentials: Basic, or
// Digest after the target's challenge. It returns the final status.
func Do(ctx context.Context, client *http.Client, method string, target engine.Target, headers http.Header, body []byte) (int, error) {
	build := func(authorization string) (*http.Request, error) {
		var reader io.Reader
		if method != http.MethodGet {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target.URL, reader)
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header[k] = v
		}
		switch {
		case authorization != "":
			req.Header.Set("Authorization", authorization)
		case target.Username != "" && target.Auth != AuthDigest:
			req.SetBasicAuth(target.Username, target.Password)
		}
		return req, nil
	}
	req, err := build("")
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, delivery.Unwrap(err)
	}
	drain(resp)
	if resp.StatusCode != http.StatusUnauthorized || target.Auth != AuthDigest || target.Username == "" {
		return resp.StatusCode, nil
	}
	ch, ok := parseChallenge(resp.Header.Values("WWW-Authenticate"))
	if !ok {
		return resp.StatusCode, fmt.Errorf("target answered 401 without a Digest challenge")
	}
	auth, err := ch.authorize(method, req.URL.RequestURI(), target.Username, target.Password, body)
	if err != nil {
		return resp.StatusCode, err
	}
	if req, err = build(auth); err != nil {
		return 0, err
	}
	if resp, err = client.Do(req); err != nil {
		return 0, delivery.Unwrap(err)
	}
	drain(resp)
	return resp.StatusCode, nil
}

// drain reads a bounded part of the answer so the connection can be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}
