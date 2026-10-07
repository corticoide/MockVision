// Package mqttpub implements the mqtt-publish engine: the camera as an
// MQTT 3.1.1 client. Like a real device it connects to each broker it is
// given as soon as it runs and keeps the session up, pinging when idle and
// reconnecting when it drops, and publishes every event with the topic and
// payload of its profile. Birth and will messages announce it online and
// offline.
package mqttpub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/corticoide/mockvision/backend/internal/engines/delivery"
	"github.com/corticoide/mockvision/sdk/engine"
)

// Name and Version identify the engine in profiles (mqtt-publish@^1).
const (
	Name    = "mqtt-publish"
	Version = "1.0.0"
)

// Transport is the name of the transport this engine delivers.
const Transport = engine.TransportMQTT

const configSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["engine"],
  "properties": {
    "engine": {"type": "string"},
    "client_id": {"type": "string", "maxLength": 200, "description": "Template; the camera serial when omitted."},
    "keepalive": {"type": "string", "pattern": "^[0-9]+(ms|s|m)$"},
    "clean_session": {"type": "boolean"},
    "max_parallel": {"type": "integer", "minimum": 1, "maximum": 64},
    "birth": {"$ref": "#/$defs/message"},
    "will": {"$ref": "#/$defs/message"}
  },
  "$defs": {
    "message": {
      "type": "object",
      "additionalProperties": false,
      "required": ["topic", "body"],
      "properties": {
        "topic": {"type": "string", "minLength": 1},
        "body": {"type": "string"},
        "qos": {"type": "integer", "minimum": 0, "maximum": 2},
        "retain": {"type": "boolean"}
      }
    }
  }
}`

type message struct {
	Topic  string `json:"topic"`
	Body   string `json:"body"`
	QoS    byte   `json:"qos"`
	Retain bool   `json:"retain"`
}

type config struct {
	ClientID     string   `json:"client_id"`
	KeepAlive    string   `json:"keepalive"`
	CleanSession *bool    `json:"clean_session"`
	MaxParallel  int      `json:"max_parallel"`
	Birth        *message `json:"birth"`
	Will         *message `json:"will"`
}

// transportConfig is the mqtt section of an event.
type transportConfig struct {
	Topic  string `json:"topic"`
	QoS    byte   `json:"qos"`
	Retain bool   `json:"retain"`
	Body   string `json:"body"`
	Image  bool   `json:"image"`
}

// defaultClientID names the camera at its brokers when nothing else does.
const defaultClientID = "{{ .Camera.Serial }}"

// syncEvery is how often the engine compares its sessions with the
// camera's targets.
var syncEvery = 2 * time.Second

// Engine publishes the events of one camera.
type Engine struct {
	in        engine.StartInput
	cfg       config
	keepAlive time.Duration
	runner    *delivery.Runner
	templates *delivery.Templates
	unsub     func()
	state     atomic.Value
	stop      chan struct{}
	loopDone  chan struct{}

	mu       sync.Mutex
	sessions map[string]*broker // by target ID
}

// broker is the camera's session with one target.
type broker struct {
	key     string // what the session was opened with
	target  engine.Target
	mu      sync.Mutex
	session *Session
	retry   time.Time // no new attempt before
	wait    time.Duration
}

// New returns an engine instance.
func New() engine.Engine {
	e := &Engine{sessions: map[string]*broker{}}
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

// Validate implements engine.Engine.
func (e *Engine) Validate(raw json.RawMessage) []engine.Problem {
	var c config
	if err := json.Unmarshal(raw, &c); err != nil {
		return []engine.Problem{{Path: "", Message: err.Error()}}
	}
	var out []engine.Problem
	if c.KeepAlive != "" {
		if d, err := time.ParseDuration(c.KeepAlive); err != nil || d < time.Second || d > 18*time.Hour {
			out = append(out, engine.Problem{Path: "/keepalive", Message: "must be between 1s and 18h"})
		}
	}
	return out
}

// Start implements engine.Engine.
func (e *Engine) Start(_ context.Context, in engine.StartInput) error {
	e.in = in
	if err := json.Unmarshal(in.Config, &e.cfg); err != nil {
		return err
	}
	e.keepAlive = 60 * time.Second
	if e.cfg.KeepAlive != "" {
		d, err := time.ParseDuration(e.cfg.KeepAlive)
		if err != nil {
			return fmt.Errorf("keepalive: %w", err)
		}
		e.keepAlive = d
	}
	e.runner = delivery.NewRunner(in.Host.Events(), e.cfg.MaxParallel)
	e.templates = delivery.NewTemplates(in.Host.Templates())
	e.unsub = in.Host.Events().Subscribe(Transport, e.dispatch)
	e.stop = make(chan struct{})
	e.loopDone = make(chan struct{})
	go e.loop()
	e.state.Store(engine.HealthOK)
	return nil
}

// Reload implements engine.Engine.
func (e *Engine) Reload(_ context.Context, raw json.RawMessage) error {
	var c config
	return json.Unmarshal(raw, &c)
}

// Health implements engine.Engine: degraded while a broker is unreachable.
func (e *Engine) Health() engine.Health {
	if e.runner == nil {
		return engine.Health{State: engine.HealthStopped}
	}
	h := e.runner.Health(e.state.Load().(engine.HealthState))
	if h.State != engine.HealthOK {
		return h
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var down []string
	for _, b := range e.sessions {
		b.mu.Lock()
		if b.session == nil || b.session.Err() != nil {
			down = append(down, b.target.Name)
		}
		b.mu.Unlock()
	}
	if len(down) > 0 {
		h.State = engine.HealthDegraded
		h.Detail = "not connected to " + strings.Join(down, ", ")
	}
	return h
}

// Stop implements engine.Engine: sessions end with DISCONNECT, so brokers
// drop the will messages, as when a camera is shut down cleanly.
func (e *Engine) Stop(ctx context.Context) error {
	if e.unsub != nil {
		e.unsub()
	}
	if e.stop != nil {
		close(e.stop)
		<-e.loopDone
	}
	if e.runner != nil {
		e.runner.Stop(ctx)
	}
	e.mu.Lock()
	for id, b := range e.sessions {
		b.close()
		delete(e.sessions, id)
	}
	e.mu.Unlock()
	e.state.Store(engine.HealthStopped)
	return nil
}

// loop keeps one session per broker target: it opens missing ones, with a
// growing pause after failures, and closes those of removed targets.
func (e *Engine) loop() {
	defer close(e.loopDone)
	tick := time.NewTicker(syncEvery)
	defer tick.Stop()
	for {
		e.sync()
		select {
		case <-e.stop:
			return
		case <-tick.C:
		}
	}
}

func (e *Engine) sync() {
	want := map[string]engine.Target{}
	for _, t := range e.in.Host.Events().Targets(Transport) {
		if t.Type == engine.TargetMQTT {
			want[t.ID] = t
		}
	}
	e.mu.Lock()
	for id, b := range e.sessions {
		if _, ok := want[id]; !ok {
			b.close()
			delete(e.sessions, id)
		}
	}
	var connect []*broker
	for id, t := range want {
		b := e.sessions[id]
		key := sessionKey(t)
		if b != nil && b.key != key {
			b.close()
			b = nil
		}
		if b == nil {
			b = &broker{key: key, target: t}
			e.sessions[id] = b
		}
		connect = append(connect, b)
	}
	e.mu.Unlock()
	for _, b := range connect {
		go func() {
			ctx, cancel := context.WithTimeout(e.runner.Context(), 10*time.Second)
			defer cancel()
			_, _ = e.session(ctx, b, false)
		}()
	}
}

// sessionKey changes when the target changes how the camera connects.
func sessionKey(t engine.Target) string {
	raw, _ := json.Marshal([]any{t.URL, t.ClientID, t.Username, t.Password, t.Insecure})
	return string(raw)
}

// session returns the broker's open session, connecting when there is
// none. Background reconnections wait after failures; a delivery attempt
// (force) always tries.
func (e *Engine) session(ctx context.Context, b *broker, force bool) (*Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session != nil && b.session.Err() == nil {
		return b.session, nil
	}
	if b.session == nil && b.key == "" {
		return nil, fmt.Errorf("closed")
	}
	if !force && time.Now().Before(b.retry) {
		return nil, fmt.Errorf("waiting to reconnect")
	}
	s, err := e.connect(ctx, b.target)
	if err != nil {
		b.wait = min(max(2*b.wait, 5*time.Second), time.Minute)
		b.retry = time.Now().Add(b.wait)
		if b.session != nil || !force {
			e.in.Host.Telemetry().Log(slog.LevelWarn, "mqtt: cannot connect to the broker", "target", b.target.Name, "error", err.Error())
		}
		b.session = nil
		return nil, err
	}
	b.session, b.wait, b.retry = s, 0, time.Time{}
	return s, nil
}

func (b *broker) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session != nil {
		b.session.Close()
		b.session = nil
	}
	b.key = ""
}

func (e *Engine) connect(ctx context.Context, t engine.Target) (*Session, error) {
	data := engine.TemplateData{Camera: cameraData(e.in.Identity), Now: time.Now()}
	idText := t.ClientID
	if idText == "" {
		idText = e.cfg.ClientID
	}
	if idText == "" {
		idText = defaultClientID
	}
	id, err := e.templates.Render(ctx, "client_id", idText, 0, data)
	if err != nil {
		return nil, fmt.Errorf("client ID: %w", err)
	}
	opts := ConnectOptions{
		URL: t.URL, ClientID: strings.TrimSpace(string(id)), Username: t.Username, Password: t.Password,
		Insecure: t.Insecure, KeepAlive: e.keepAlive, Clean: e.cfg.CleanSession == nil || *e.cfg.CleanSession,
	}
	if w := e.cfg.Will; w != nil {
		m, err := e.render(ctx, *w, data)
		if err != nil {
			return nil, fmt.Errorf("will: %w", err)
		}
		opts.Will = &m
	}
	s, err := Connect(ctx, opts)
	if err != nil {
		return nil, err
	}
	if b := e.cfg.Birth; b != nil {
		m, err := e.render(ctx, *b, data)
		if err == nil {
			err = s.Publish(ctx, m)
		}
		if err != nil {
			e.in.Host.Telemetry().Log(slog.LevelWarn, "mqtt: birth message", "target", t.Name, "error", err.Error())
		}
	}
	return s, nil
}

func (e *Engine) render(ctx context.Context, m message, data engine.TemplateData) (Message, error) {
	topic, err := e.templates.Render(ctx, "topic", m.Topic, 0, data)
	if err != nil {
		return Message{}, err
	}
	body, err := e.templates.Render(ctx, "body", m.Body, 0, data)
	if err != nil {
		return Message{}, err
	}
	return Message{Topic: strings.TrimSpace(string(topic)), Payload: body, QoS: m.QoS, Retain: m.Retain}, nil
}

func cameraData(id engine.Identity) engine.CameraData {
	return engine.CameraData{ID: id.CameraID, Name: id.Name, IP: id.IP, MAC: id.MAC, Serial: id.Serial, Vendor: id.Vendor, Model: id.Model, Firmware: id.Firmware}
}

func (e *Engine) dispatch(d engine.Dispatch) {
	var tc transportConfig
	if err := json.Unmarshal(d.Transport, &tc); err != nil {
		e.in.Host.Telemetry().Log(slog.LevelError, "mqtt: invalid transport", "error", err)
		return
	}
	data := delivery.Data(e.in.Identity, d)
	max := 0
	if tc.Image {
		max = 8 << 20
	}
	payload, perr := e.templates.Render(e.runner.Context(), Transport, tc.Body, max, data)
	for _, target := range d.Targets {
		if target.Type != engine.TargetMQTT {
			continue
		}
		topicText := tc.Topic
		if target.Topic != "" {
			topicText = target.Topic
		}
		topic, err := e.templates.Render(e.runner.Context(), "topic", topicText, 0, data)
		if err == nil {
			err = perr
		}
		if err == nil && strings.TrimSpace(string(topic)) == "" {
			err = fmt.Errorf("empty topic")
		}
		if err != nil {
			e.runner.Fail(d, target, fmt.Errorf("template: %w", err))
			continue
		}
		m := Message{Topic: strings.TrimSpace(string(topic)), Payload: payload, QoS: tc.QoS, Retain: tc.Retain}
		e.runner.Go(d, target, func(ctx context.Context, _ int) delivery.Attempt {
			return e.publish(ctx, target, m)
		})
	}
}

func (e *Engine) publish(ctx context.Context, t engine.Target, m Message) delivery.Attempt {
	e.mu.Lock()
	b := e.sessions[t.ID]
	if b == nil || b.key != sessionKey(t) {
		if b != nil {
			b.close()
		}
		b = &broker{key: sessionKey(t), target: t}
		e.sessions[t.ID] = b
	}
	e.mu.Unlock()
	s, err := e.session(ctx, b, true)
	if err != nil {
		return delivery.Attempt{Err: err}
	}
	if err := s.Publish(ctx, m); err != nil {
		return delivery.Attempt{Err: err}
	}
	return delivery.Attempt{Bytes: len(m.Payload)}
}

// Probe connects to a target's broker and disconnects: the connection test
// of the panel.
func Probe(ctx context.Context, t engine.Target, clientID string) error {
	s, err := Connect(ctx, ConnectOptions{URL: t.URL, ClientID: clientID, Username: t.Username, Password: t.Password, Insecure: t.Insecure, Clean: true})
	if err != nil {
		return err
	}
	s.Close()
	return nil
}
