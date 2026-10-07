package camera

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/sdk/engine"
)

// eventBus implements engine.Events: every event is logged with the service
// and routed to the engines that deliver its transports.
type eventBus struct {
	rt *Runtime

	mu      sync.Mutex
	subs    map[string]func(engine.Dispatch)
	last    map[string]time.Time
	targets []ipc.Target
	// observe sees every event the camera emits before it goes out, so the
	// counts a template reads include it.
	observe func(engine.Event)
}

func newEventBus(rt *Runtime) *eventBus {
	return &eventBus{rt: rt, subs: map[string]func(engine.Dispatch){}, last: map[string]time.Time{}}
}

func (b *eventBus) setTargets(t []ipc.Target) {
	b.mu.Lock()
	b.targets = t
	b.mu.Unlock()
}

// Emit implements engine.Events.
func (b *eventBus) Emit(ctx context.Context, e engine.Event) (engine.Event, error) {
	return b.emit(ctx, e, "")
}

// emit logs an event with the service, naming the stored trigger behind it,
// and hands it to the engines that deliver its transports.
func (b *eventBus) emit(_ context.Context, e engine.Event, triggerID string) (engine.Event, error) {
	spec, ok := b.rt.model.Doc.Events[e.Type]
	if !ok {
		return e, fmt.Errorf("the profile does not define event type %s", e.Type)
	}
	now := b.rt.now()
	b.mu.Lock()
	if min := spec.MinInterval.D(); min > 0 {
		if last, seen := b.last[e.Type]; seen && now.Sub(last) < min {
			b.mu.Unlock()
			return e, fmt.Errorf("%s events are limited to one every %s by the profile", e.Type, min)
		}
	}
	b.last[e.Type] = now
	targets := append([]ipc.Target(nil), b.targets...)
	subs := make(map[string]func(engine.Dispatch), len(b.subs))
	for k, v := range b.subs {
		subs[k] = v
	}
	b.mu.Unlock()

	if e.ID == "" {
		e.ID = ulid.Make().String()
	}
	if e.At.IsZero() {
		e.At = now
	}
	if b.observe != nil {
		b.observe(e)
	}
	if err := b.rt.conn.Notify(ipc.TypeEvent, ipc.EventMsg{Event: e, TriggerID: triggerID}); err != nil {
		return e, err
	}
	b.rt.files.record(e, spec)

	vendor := spec.VendorName
	if vendor == "" {
		vendor = e.Type
	}
	policy := engine.DeliveryPolicy{Timeout: 5 * time.Second, Backoff: time.Second}
	if d := spec.Delivery.Timeout.D(); d > 0 {
		policy.Timeout = d
	}
	if spec.Delivery.Retries != nil {
		policy.Retries = *spec.Delivery.Retries
	}
	if d := spec.Delivery.Backoff.D(); d > 0 {
		policy.Backoff = d
	}
	for transport, raw := range spec.Transports {
		if transport == engine.TransportAttach {
			// Whoever listens gets it: the transport reaches no target.
			if deliver := subs[transport]; deliver != nil {
				deliver(engine.Dispatch{Event: e, VendorName: vendor, Transport: raw, Policy: policy})
			}
			continue
		}
		var matched []engine.Target
		for _, t := range targets {
			if !slices.Contains(engine.TransportTargets[transport], t.Type) || !wantsType(t.EventTypes, e.Type) {
				continue
			}
			matched = append(matched, t.Target)
		}
		if len(matched) == 0 {
			continue
		}
		deliver := subs[transport]
		if deliver == nil {
			// Its engine is disabled on this camera.
			b.rt.tel.Log(slog.LevelWarn, "no engine delivers transport", "transport", transport)
			continue
		}
		deliver(engine.Dispatch{Event: e, VendorName: vendor, Transport: raw, Policy: policy, Targets: matched})
	}
	return e, nil
}

func wantsType(types []string, t string) bool {
	if len(types) == 0 {
		return true
	}
	for _, x := range types {
		if x == t {
			return true
		}
	}
	return false
}

func (b *eventBus) Subscribe(transport string, fn func(engine.Dispatch)) func() {
	b.mu.Lock()
	b.subs[transport] = fn
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		delete(b.subs, transport)
		b.mu.Unlock()
	}
}

// Targets implements engine.Events.
func (b *eventBus) Targets(transport string) []engine.Target {
	types := engine.TransportTargets[transport]
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []engine.Target
	for _, t := range b.targets {
		if slices.Contains(types, t.Type) {
			out = append(out, t.Target)
		}
	}
	return out
}

func (b *eventBus) Report(r engine.DeliveryReport) {
	if err := b.rt.conn.Notify(ipc.TypeDelivery, r); err != nil {
		b.rt.log.Warn("cannot report delivery", "error", err)
	}
}
