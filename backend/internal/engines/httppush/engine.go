// Package httppush implements the http-push engine: it delivers events to
// HTTP targets with the payload the profile's template builds, using the
// device's own timeout and retry behavior.
package httppush

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/corticoide/mockvision/sdk/engine"
)

// Name and Version identify the engine in profiles (http-push@^1).
const (
	Name    = "http-push"
	Version = "1.0.0"
)

// Transport is the name of the transport this engine delivers.
const Transport = "http_push"

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
	in     engine.StartInput
	cfg    config
	client *http.Client
	sem    chan struct{}

	mu        sync.Mutex
	templates map[[32]byte]engine.Template
	unsub     func()
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	state     atomic.Value
	delivered atomic.Uint64
	failures  atomic.Uint64
	bytesOut  atomic.Uint64
	inflight  atomic.Int64
}

// New returns an engine instance.
func New() engine.Engine {
	e := &Engine{templates: map[[32]byte]engine.Template{}}
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
func (e *Engine) Start(ctx context.Context, in engine.StartInput) error {
	e.in = in
	if err := json.Unmarshal(in.Config, &e.cfg); err != nil {
		return err
	}
	if e.cfg.MaxParallel <= 0 {
		e.cfg.MaxParallel = 8
	}
	if e.cfg.UserAgent == "" {
		e.cfg.UserAgent = strings.TrimSpace(in.Identity.Vendor + " " + in.Identity.Model)
	}
	e.sem = make(chan struct{}, e.cfg.MaxParallel)
	e.client = &http.Client{
		Transport: &http.Transport{
			// Cameras talk to targets directly, never through the node's proxy.
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: 5 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.unsub = in.Host.Events().Subscribe(Transport, e.dispatch)
	e.state.Store(engine.HealthOK)
	return nil
}

// Reload implements engine.Engine.
func (e *Engine) Reload(_ context.Context, raw json.RawMessage) error {
	var c config
	return json.Unmarshal(raw, &c)
}

// Health implements engine.Engine.
func (e *Engine) Health() engine.Health {
	return engine.Health{
		State:    e.state.Load().(engine.HealthState),
		Clients:  int(e.inflight.Load()),
		Requests: e.delivered.Load(),
		Errors:   e.failures.Load(),
		BytesOut: e.bytesOut.Load(),
	}
}

// Stop implements engine.Engine; pending deliveries are abandoned.
func (e *Engine) Stop(ctx context.Context) error {
	if e.unsub != nil {
		e.unsub()
	}
	if e.cancel != nil {
		e.cancel()
	}
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
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
	t, err := e.template(tc)
	if err != nil {
		e.in.Host.Telemetry().Log(slog.LevelError, "http-push: template", "error", err)
		return
	}
	data := engine.TemplateData{
		Camera: engine.CameraData{
			ID: e.in.Identity.CameraID, Name: e.in.Identity.Name, IP: e.in.Identity.IP, MAC: e.in.Identity.MAC,
			Serial: e.in.Identity.Serial, Vendor: e.in.Identity.Vendor, Model: e.in.Identity.Model, Firmware: e.in.Identity.Firmware,
		},
		Event:     &d.Event,
		EventName: d.VendorName,
		Now:       d.Event.At,
	}
	body, err := t.Render(e.ctx, data)
	if err != nil {
		for _, target := range d.Targets {
			e.in.Host.Events().Report(engine.DeliveryReport{
				EventID: d.Event.ID, TargetID: target.ID, Attempt: 1, At: time.Now(),
				Status: engine.DeliveryFailed, Error: "template: " + err.Error(),
			})
		}
		return
	}
	for _, target := range d.Targets {
		if target.Type != "http" {
			continue
		}
		e.wg.Add(1)
		go func(target engine.Target) {
			defer e.wg.Done()
			e.deliver(d, tc, target, body)
		}(target)
	}
}

func (e *Engine) template(tc transportConfig) (engine.Template, error) {
	key := sha256.Sum256([]byte(tc.Body))
	e.mu.Lock()
	defer e.mu.Unlock()
	if t, ok := e.templates[key]; ok {
		return t, nil
	}
	max := 0
	if tc.Image {
		max = 8 << 20
	}
	t, err := e.in.Host.Templates().Compile(Transport, tc.Body, max)
	if err != nil {
		return nil, err
	}
	e.templates[key] = t
	return t, nil
}

func (e *Engine) deliver(d engine.Dispatch, tc transportConfig, target engine.Target, body []byte) {
	policy := d.Policy
	if policy.Timeout <= 0 {
		policy.Timeout = 5 * time.Second
	}
	attempts := policy.Retries + 1
	for attempt := 1; attempt <= attempts; attempt++ {
		select {
		case e.sem <- struct{}{}:
		case <-e.ctx.Done():
			return
		}
		e.inflight.Add(1)
		start := time.Now()
		status, err := e.send(tc, target, body, policy.Timeout)
		latency := time.Since(start)
		e.inflight.Add(-1)
		<-e.sem

		r := engine.DeliveryReport{
			EventID: d.Event.ID, TargetID: target.ID, Attempt: attempt, At: start,
			HTTPStatus: status, LatencyMS: latency.Milliseconds(),
		}
		if err == nil && status >= 200 && status < 300 {
			r.Status = engine.DeliveryOK
			e.delivered.Add(1)
			e.in.Host.Events().Report(r)
			return
		}
		e.failures.Add(1)
		switch {
		case err != nil:
			r.Error = err.Error()
		default:
			r.Error = fmt.Sprintf("target answered %d", status)
		}
		if attempt < attempts {
			r.Status = engine.DeliveryRetry
		} else {
			r.Status = engine.DeliveryFailed
		}
		e.in.Host.Events().Report(r)
		if attempt < attempts {
			select {
			case <-time.After(policy.Backoff):
			case <-e.ctx.Done():
				return
			}
		}
	}
}

func (e *Engine) send(tc transportConfig, target engine.Target, body []byte, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(e.ctx, timeout)
	defer cancel()
	method := target.Method
	if method == "" {
		method = tc.Method
	}
	if method == "" {
		method = http.MethodPost
	}
	var reader io.Reader
	if method != http.MethodGet {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.URL, reader)
	if err != nil {
		return 0, err
	}
	if tc.ContentType != "" && method != http.MethodGet {
		req.Header.Set("Content-Type", tc.ContentType)
	}
	req.Header.Set("User-Agent", e.cfg.UserAgent)
	for k, v := range tc.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range target.Headers {
		req.Header.Set(k, v)
	}
	if target.Username != "" {
		req.SetBasicAuth(target.Username, target.Password)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, fmt.Errorf("timeout after %s", timeout)
		}
		return 0, simplify(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	e.bytesOut.Add(uint64(len(body)))
	return resp.StatusCode, nil
}

// simplify drops the URL that net/http repeats in its errors.
func simplify(err error) error {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		if inner := ue.Unwrap(); inner != nil {
			return inner
		}
	}
	return err
}
