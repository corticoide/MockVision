// Package smtpmail implements the smtp-mail engine: the camera mails its
// events, with the subject and text its profile renders and the event's
// snapshot attached, to the recipients of each smtp target. Like a real
// device it sends at most one mail per interval to each target.
package smtpmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/corticoide/mockvision/backend/internal/engines/delivery"
	"github.com/corticoide/mockvision/sdk/engine"
)

// Name and Version identify the engine in profiles (smtp-mail@^1).
const (
	Name    = "smtp-mail"
	Version = "1.0.0"
)

// Transport is the name of the transport this engine delivers.
const Transport = engine.TransportSMTP

const configSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["engine"],
  "properties": {
    "engine": {"type": "string"},
    "interval": {"type": "string", "pattern": "^[0-9]+(ms|s|m|h)$", "description": "Shortest time between two mails to a target."},
    "max_parallel": {"type": "integer", "minimum": 1, "maximum": 16},
    "mailer": {"type": "string", "maxLength": 200, "description": "X-Mailer header; omitted when empty."}
  }
}`

type config struct {
	Interval    string `json:"interval"`
	MaxParallel int    `json:"max_parallel"`
	Mailer      string `json:"mailer"`
}

// transportConfig is the smtp section of an event.
type transportConfig struct {
	Subject     string `json:"subject"`
	Body        string `json:"body"`
	ContentType string `json:"content_type"`
	// Snapshot attaches the picture of a stream, main when Stream is empty.
	Snapshot   bool   `json:"snapshot"`
	Stream     string `json:"stream"`
	Attachment string `json:"attachment"`
}

// Engine mails the events of one camera.
type Engine struct {
	in        engine.StartInput
	cfg       config
	interval  time.Duration
	runner    *delivery.Runner
	templates *delivery.Templates
	unsub     func()
	state     atomic.Value

	mu   sync.Mutex
	last map[string]time.Time // by target ID: when its last mail went out
}

// New returns an engine instance.
func New() engine.Engine {
	e := &Engine{last: map[string]time.Time{}}
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
		return []engine.Problem{{Message: err.Error()}}
	}
	if c.Interval != "" {
		if d, err := time.ParseDuration(c.Interval); err != nil || d < 0 || d > 24*time.Hour {
			return []engine.Problem{{Path: "/interval", Message: "must be between 0s and 24h"}}
		}
	}
	return nil
}

// Start implements engine.Engine.
func (e *Engine) Start(_ context.Context, in engine.StartInput) error {
	e.in = in
	if err := json.Unmarshal(in.Config, &e.cfg); err != nil {
		return err
	}
	if e.cfg.Interval != "" {
		d, err := time.ParseDuration(e.cfg.Interval)
		if err != nil {
			return fmt.Errorf("interval: %w", err)
		}
		e.interval = d
	}
	if e.cfg.MaxParallel <= 0 {
		e.cfg.MaxParallel = 2
	}
	e.runner = delivery.NewRunner(in.Host.Events(), e.cfg.MaxParallel)
	e.templates = delivery.NewTemplates(in.Host.Templates())
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
	if e.runner == nil {
		return engine.Health{State: engine.HealthStopped}
	}
	return e.runner.Health(e.state.Load().(engine.HealthState))
}

// Stop implements engine.Engine; pending mails are abandoned.
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

// due reports whether a target may get a mail now, and marks it sent.
func (e *Engine) due(targetID string, now time.Time) bool {
	if e.interval <= 0 {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if last, ok := e.last[targetID]; ok && now.Sub(last) < e.interval {
		return false
	}
	e.last[targetID] = now
	return true
}

func (e *Engine) dispatch(d engine.Dispatch) {
	var tc transportConfig
	if err := json.Unmarshal(d.Transport, &tc); err != nil {
		e.in.Host.Telemetry().Log(slog.LevelError, "smtp: invalid transport", "error", err)
		return
	}
	var targets []engine.Target
	now := time.Now()
	for _, t := range d.Targets {
		if t.Type != engine.TargetSMTP {
			continue
		}
		if !e.due(t.ID, now) {
			e.in.Host.Events().Report(engine.DeliveryReport{EventID: d.Event.ID, TargetID: t.ID, Attempt: 1, At: now,
				Status: engine.DeliverySkipped, Error: fmt.Sprintf("one mail every %s at most", e.interval)})
			continue
		}
		targets = append(targets, t)
	}
	if len(targets) == 0 {
		return
	}
	content, err := e.content(d, tc)
	for _, t := range targets {
		if err != nil {
			e.runner.Fail(d, t, err)
			continue
		}
		msg, err := e.message(d, t, content)
		if err != nil {
			e.runner.Fail(d, t, err)
			continue
		}
		e.runner.Go(d, t, func(ctx context.Context, _ int) delivery.Attempt {
			if err := Send(ctx, t, heloName(e.in.Identity.IP), msg); err != nil {
				return delivery.Attempt{Err: err}
			}
			return delivery.Attempt{Bytes: len(msg)}
		})
	}
}

// content is what every target's mail of an event holds.
type content struct {
	subject     string
	text        []byte
	contentType string
	image       []byte
	imageName   string
}

func (e *Engine) content(d engine.Dispatch, tc transportConfig) (content, error) {
	ctx := e.runner.Context()
	data := delivery.Data(e.in.Identity, d)
	subject, err := e.templates.Render(ctx, "subject", tc.Subject, 0, data)
	if err != nil {
		return content{}, fmt.Errorf("template: %w", err)
	}
	text, err := e.templates.Render(ctx, Transport, tc.Body, 0, data)
	if err != nil {
		return content{}, fmt.Errorf("template: %w", err)
	}
	c := content{subject: oneLine(string(subject)), text: text, contentType: tc.ContentType}
	if c.contentType == "" {
		c.contentType = "text/plain"
	}
	if tc.Snapshot {
		stream := tc.Stream
		if stream == "" {
			stream = "main"
		}
		if c.image, err = e.in.Host.Media().Snapshot(stream); err != nil {
			return content{}, fmt.Errorf("snapshot: %w", err)
		}
		name := tc.Attachment
		if name == "" {
			name = `{{ fmtTime .Event.At "20060102150405" }}.jpg`
		}
		raw, err := e.templates.Render(ctx, "attachment", name, 0, data)
		if err != nil {
			return content{}, fmt.Errorf("template: %w", err)
		}
		c.imageName = strings.NewReplacer("/", "_", `\`, "_", `"`, "_").Replace(oneLine(string(raw)))
	}
	return c, nil
}

// message builds the RFC 5322 message for a target: text alone, or text
// and the snapshot in a multipart/mixed.
func (e *Engine) message(d engine.Dispatch, t engine.Target, c content) ([]byte, error) {
	var b bytes.Buffer
	hdr := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	from := t.From
	if name := oneLine(e.in.Identity.Name); name != "" {
		from = mime.QEncoding.Encode("utf-8", name) + " <" + t.From + ">"
	}
	hdr("From", from)
	hdr("To", strings.Join(t.To, ", "))
	hdr("Subject", mime.QEncoding.Encode("utf-8", c.subject))
	hdr("Date", d.Event.At.Format(time.RFC1123Z))
	hdr("Message-ID", fmt.Sprintf("<%s.%s@%s>", strings.ToLower(d.Event.ID), strings.ToLower(t.ID), strings.Trim(heloName(e.in.Identity.IP), "[]")))
	if e.cfg.Mailer != "" {
		hdr("X-Mailer", oneLine(e.cfg.Mailer))
	}
	hdr("MIME-Version", "1.0")
	textType := c.contentType + "; charset=utf-8"
	if c.image == nil {
		hdr("Content-Type", textType)
		hdr("Content-Transfer-Encoding", "quoted-printable")
		b.WriteString("\r\n")
		if err := writeQP(&b, c.text); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	}
	mw := multipart.NewWriter(&b)
	hdr("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
	b.WriteString("\r\n")
	part, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {textType}, "Content-Transfer-Encoding": {"quoted-printable"}})
	if err != nil {
		return nil, err
	}
	if err := writeQP(part, c.text); err != nil {
		return nil, err
	}
	part, err = mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {mime.FormatMediaType("image/jpeg", map[string]string{"name": c.imageName})},
		"Content-Disposition":       {mime.FormatMediaType("attachment", map[string]string{"filename": c.imageName})},
		"Content-Transfer-Encoding": {"base64"},
	})
	if err != nil {
		return nil, err
	}
	enc := base64.StdEncoding.EncodeToString(c.image)
	for len(enc) > 76 {
		fmt.Fprintf(part, "%s\r\n", enc[:76])
		enc = enc[76:]
	}
	fmt.Fprintf(part, "%s\r\n", enc)
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeQP(w interface{ Write([]byte) (int, error) }, text []byte) error {
	qp := quotedprintable.NewWriter(w)
	if _, err := qp.Write(bytes.ReplaceAll(text, []byte("\n"), []byte("\r\n"))); err != nil {
		return err
	}
	return qp.Close()
}

func oneLine(s string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

// Send mails a message to a target's recipients.
func Send(ctx context.Context, t engine.Target, helo string, msg []byte) error {
	c, err := dial(ctx, t, helo)
	if err != nil {
		return err
	}
	defer c.close()
	return c.send(t.From, t.To, msg)
}

// Probe connects, authenticates and checks the sender and recipients, then
// resets: the connection test of the panel, which sends no mail.
func Probe(ctx context.Context, t engine.Target, ip string) error {
	c, err := dial(ctx, t, heloName(ip))
	if err != nil {
		return err
	}
	defer c.close()
	return c.verify(t.From, t.To)
}
