// Package enginetest is a camera host for the tests of single engines: it
// records what an engine reports and lets a test dispatch events to it.
package enginetest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/tmpl"
	"github.com/corticoide/mockvision/sdk/engine"
)

// Snapshot is the picture the fake camera serves: a JPEG start and end
// marker around a recognizable body.
var Snapshot = []byte("\xff\xd8mockvision-snapshot\xff\xd9")

// Host implements engine.Host for one engine under test.
type Host struct {
	T      testing.TB
	Ident  engine.Identity
	Params map[string]any
	// FaultStatus is what Faults().Status answers.
	FaultStatus int

	mu      sync.Mutex
	subs    map[string]func(engine.Dispatch)
	targets []engine.Target
	reports []engine.DeliveryReport
	changed chan struct{}
	logs    []string
}

// NewHost returns a host with a camera identity.
func NewHost(t testing.TB) *Host {
	return &Host{
		T:       t,
		Ident:   engine.Identity{CameraID: "cam1", Name: "Gate", IP: "10.0.0.10", MAC: "02:00:00:00:00:10", Serial: "SN123", Vendor: "Acme", Model: "X1", Firmware: "1.0"},
		Params:  map[string]any{"System.DeviceName": "Gate camera"},
		subs:    map[string]func(engine.Dispatch){},
		changed: make(chan struct{}, 1),
	}
}

// Start starts an engine on the host with a profile section given as JSON.
func (h *Host) Start(e engine.Engine, config string) {
	h.T.Helper()
	if err := e.Start(context.Background(), engine.StartInput{Identity: h.Ident, Instance: "test", Config: json.RawMessage(config), Host: h}); err != nil {
		h.T.Fatal(err)
	}
	h.T.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.Stop(ctx)
	})
}

// SetTargets replaces the camera's targets.
func (h *Host) SetTargets(t ...engine.Target) {
	h.mu.Lock()
	h.targets = t
	h.mu.Unlock()
}

// Dispatch hands an event to the engine subscribed to the transport, as
// the camera's event bus does: transport is the event's section of the
// profile, as JSON.
func (h *Host) Dispatch(transport, section string, ev engine.Event, policy engine.DeliveryPolicy) {
	h.T.Helper()
	h.mu.Lock()
	fn := h.subs[transport]
	var targets []engine.Target
	for _, t := range h.targets {
		if slices.Contains(engine.TransportTargets[transport], t.Type) {
			targets = append(targets, t)
		}
	}
	h.mu.Unlock()
	if fn == nil {
		h.T.Fatalf("no engine delivers %s", transport)
	}
	if ev.ID == "" {
		ev.ID = "01EVENT"
	}
	if ev.At.IsZero() {
		ev.At = time.Date(2026, 10, 7, 14, 30, 5, 0, time.UTC)
	}
	fn(engine.Dispatch{Event: ev, VendorName: "LineCrossing", Transport: json.RawMessage(section), Policy: policy, Targets: targets})
}

// Reports returns the delivery reports so far.
func (h *Host) Reports() []engine.DeliveryReport {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.reports)
}

// WaitReports waits until n reports came, and returns them.
func (h *Host) WaitReports(n int, within time.Duration) []engine.DeliveryReport {
	h.T.Helper()
	deadline := time.After(within)
	for {
		if r := h.Reports(); len(r) >= n {
			return r
		}
		select {
		case <-h.changed:
		case <-deadline:
			h.T.Fatalf("got %d delivery reports, want %d: %+v", len(h.Reports()), n, h.Reports())
		}
	}
}

// Accounts implements engine.Host.
func (h *Host) Accounts() engine.Accounts { return nil }

// State implements engine.Host.
func (h *Host) State() engine.State { return nil }

// Events implements engine.Host.
func (h *Host) Events() engine.Events { return h }

// Media implements engine.Host.
func (h *Host) Media() engine.Media { return h }

// Templates implements engine.Host.
func (h *Host) Templates() engine.Templates {
	return tmpl.NewCompiler(tmpl.Env{
		State:    func(k string) (any, bool) { v, ok := h.Params[k]; return v, ok },
		Canon:    func(string) (any, bool) { return nil, false },
		Snapshot: func() ([]byte, error) { return Snapshot, nil },
	})
}

// Files implements engine.Host.
func (h *Host) Files() engine.Files { return nil }

// Faults implements engine.Host: the status of FaultStatus, for every
// instance.
func (h *Host) Faults() engine.Faults { return h }

// Status implements engine.Faults.
func (h *Host) Status(string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.FaultStatus
}

// Telemetry implements engine.Host.
func (h *Host) Telemetry() engine.Telemetry { return h }

// Emit implements engine.Events.
func (h *Host) Emit(_ context.Context, e engine.Event) (engine.Event, error) { return e, nil }

// Subscribe implements engine.Events.
func (h *Host) Subscribe(transport string, fn func(engine.Dispatch)) func() {
	h.mu.Lock()
	h.subs[transport] = fn
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		delete(h.subs, transport)
		h.mu.Unlock()
	}
}

// Report implements engine.Events.
func (h *Host) Report(r engine.DeliveryReport) {
	h.mu.Lock()
	h.reports = append(h.reports, r)
	h.mu.Unlock()
	select {
	case h.changed <- struct{}{}:
	default:
	}
}

// Targets implements engine.Events.
func (h *Host) Targets(transport string) []engine.Target {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []engine.Target
	for _, t := range h.targets {
		if slices.Contains(engine.TransportTargets[transport], t.Type) {
			out = append(out, t)
		}
	}
	return out
}

// Streams implements engine.Media.
func (h *Host) Streams() []engine.StreamInfo {
	return []engine.StreamInfo{{Name: "main", Codec: "h264"}}
}

// Snapshot implements engine.Media.
func (h *Host) Snapshot(string) ([]byte, error) { return Snapshot, nil }

// Source implements engine.Media.
func (h *Host) Source(string) (*engine.VideoSource, error) { return nil, io.EOF }

// Watch implements engine.Media.
func (h *Host) Watch(func(string)) func() { return func() {} }

// Log implements engine.Telemetry.
func (h *Host) Log(_ slog.Level, msg string, _ ...any) {
	h.mu.Lock()
	h.logs = append(h.logs, msg)
	h.mu.Unlock()
}

// Logs returns the messages logged so far.
func (h *Host) Logs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.logs)
}

// Request implements engine.Telemetry.
func (h *Host) Request(string, string, int, time.Duration) {}

// Gap implements engine.Telemetry.
func (h *Host) Gap(string, string, string) {}

// Client implements engine.Telemetry.
func (h *Host) Client(string, string, bool) {}
