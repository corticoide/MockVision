package camera

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/engines/delivery"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/sdk/engine"
)

// faultSet holds the failures injected into the camera (D43) and applies
// those that live in the process: protocols that stop answering or answer
// late, statuses, a moved clock and a camera off the network. The service
// takes the network down too, where it runs in namespaces.
type faultSet struct {
	rt *Runtime

	mu     sync.RWMutex
	active map[string]ipc.Fault
	conns  map[string]map[*gatedConn]struct{} // by engine instance
}

func newFaultSet(rt *Runtime) *faultSet {
	return &faultSet{rt: rt, active: map[string]ipc.Fault{}, conns: map[string]map[*gatedConn]struct{}{}}
}

// Status implements engine.Faults.
func (f *faultSet) Status(instance string) int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, ft := range f.active {
		if ft.Kind == string(domain.FaultErrorStatus) && ft.Instance == instance {
			return ft.Status
		}
	}
	return 0
}

// down reports whether an instance refuses its clients: its service is
// down, or the whole camera is off the network.
func (f *faultSet) down(instance string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.downLocked(instance)
}

func (f *faultSet) downLocked(instance string) bool {
	for _, ft := range f.active {
		switch domain.FaultKind(ft.Kind) {
		case domain.FaultNetworkDown:
			return true
		case domain.FaultServiceDown:
			if ft.Instance == instance {
				return true
			}
		}
	}
	return false
}

// latency is the delay of everything an instance reads.
func (f *faultSet) latency(instance string) time.Duration {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, ft := range f.active {
		if ft.Kind == string(domain.FaultLatency) && ft.Instance == instance {
			return time.Duration(ft.LatencyMS) * time.Millisecond
		}
	}
	return 0
}

// has reports whether a fault of a kind is on.
func (f *faultSet) has(kind domain.FaultKind) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, ft := range f.active {
		if ft.Kind == string(kind) {
			return true
		}
	}
	return false
}

// skew is how far the camera's clock is moved.
func (f *faultSet) skew() time.Duration {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, ft := range f.active {
		if ft.Kind == string(domain.FaultClockSkew) {
			return time.Duration(ft.SkewS) * time.Second
		}
	}
	return 0
}

// set replaces the faults on, as the camera starts with them: they raise
// no event.
func (f *faultSet) set(list []ipc.Fault) {
	for _, ft := range list {
		f.start(context.Background(), ft, false)
	}
}

// start puts a fault on: connections it cuts are closed at once, and the
// fault's event goes out when raise is set and the profile defines it.
func (f *faultSet) start(ctx context.Context, ft ipc.Fault, raise bool) {
	f.mu.Lock()
	f.active[ft.ID] = ft
	var cut []*gatedConn
	for inst, set := range f.conns {
		if f.downLocked(inst) {
			for c := range set {
				cut = append(cut, c)
			}
		}
	}
	f.mu.Unlock()
	for _, c := range cut {
		c.reset()
	}
	f.applyOffline()
	f.rt.log.Info("fault on", "kind", ft.Kind, "instance", ft.Instance)
	if !raise {
		return
	}
	ev, ok := domain.FaultEvents[domain.FaultKind(ft.Kind)]
	if !ok {
		return
	}
	if _, defined := f.rt.model.Doc.Events[string(ev)]; !defined {
		return
	}
	if _, err := f.rt.events.Emit(ctx, engine.Event{Type: string(ev), Trigger: "fault", Custom: map[string]any{"fault": ft.Kind}}); err != nil {
		f.rt.tel.Log(slog.LevelWarn, "the fault's event was not raised", "kind", ft.Kind, "error", err.Error())
	}
}

// stop takes a fault off.
func (f *faultSet) stop(id string) {
	f.mu.Lock()
	ft, ok := f.active[id]
	delete(f.active, id)
	f.mu.Unlock()
	if ok {
		f.applyOffline()
		f.rt.log.Info("fault off", "kind", ft.Kind, "instance", ft.Instance)
	}
}

// applyOffline makes deliveries fail while the camera is off the network,
// also where the service does not take its link down (local mode).
func (f *faultSet) applyOffline() {
	f.mu.RLock()
	off := false
	for _, ft := range f.active {
		if ft.Kind == string(domain.FaultNetworkDown) {
			off = true
		}
	}
	f.mu.RUnlock()
	delivery.SetOffline(off)
}

func (f *faultSet) track(instance string, c *gatedConn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conns[instance] == nil {
		f.conns[instance] = map[*gatedConn]struct{}{}
	}
	f.conns[instance][c] = struct{}{}
}

func (f *faultSet) untrack(instance string, c *gatedConn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.conns[instance], c)
}

// gatedListener is an engine's listener under the camera's faults: while
// the instance is down, a client's connection is reset as soon as it is
// accepted, as by a service that crashed.
type gatedListener struct {
	net.Listener
	instance string
	faults   *faultSet
}

// Accept implements net.Listener.
func (l *gatedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.faults.down(l.instance) {
			resetConn(c)
			continue
		}
		gc := &gatedConn{Conn: c, instance: l.instance, faults: l.faults}
		l.faults.track(l.instance, gc)
		return gc, nil
	}
}

// gatedConn delays what it reads while a latency fault is on.
type gatedConn struct {
	net.Conn
	instance string
	faults   *faultSet
	once     sync.Once
}

// Read implements net.Conn.
func (c *gatedConn) Read(p []byte) (int, error) {
	if d := c.faults.latency(c.instance); d > 0 {
		time.Sleep(d)
	}
	return c.Conn.Read(p)
}

// Close implements net.Conn.
func (c *gatedConn) Close() error {
	c.once.Do(func() { c.faults.untrack(c.instance, c) })
	return c.Conn.Close()
}

// reset closes the connection with a reset, as a crashed service's
// kernel does.
func (c *gatedConn) reset() {
	c.once.Do(func() { c.faults.untrack(c.instance, c) })
	resetConn(c.Conn)
}

func resetConn(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = c.Close()
}
