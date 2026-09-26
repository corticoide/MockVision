// Package camera is the camera process: the same binary started with the
// camera subcommand inside the camera's network namespace. It receives its
// configuration from the main service, runs the engines of its profile on
// sockets opened for it, and reports heartbeats, events and deliveries.
package camera

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/corticoide/mockvision/backend/internal/buildinfo"
	"github.com/corticoide/mockvision/backend/internal/engines"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/backend/internal/sandbox"
	"github.com/corticoide/mockvision/backend/internal/tmpl"
	"github.com/corticoide/mockvision/sdk/engine"
)

// Socket is a socket inherited from the network helper.
type Socket struct {
	Instance string
	Name     string
	Network  string
	Port     int
	File     *os.File
}

// Options configure a camera process.
type Options struct {
	CameraID string
	IPC      net.Conn
	Sockets  []Socket
	// Local opens missing TCP sockets on 127.0.0.1 with ephemeral ports,
	// for development without network namespaces.
	Local bool
	// Confine applies Landlock to the process once the configuration names
	// its files. Only the camera subcommand sets it: it cannot be undone.
	Confine bool
	Log     *slog.Logger
}

// Runtime is a running camera. It implements engine.Host.
type Runtime struct {
	opts    Options
	log     *slog.Logger
	conn    *ipc.Conn
	catalog *engines.Catalog

	model     *profile.Model
	state     *stateStore
	media     *mediaStore
	events    *eventBus
	templates *tmpl.Compiler
	tel       *telemetry
	identity  engine.Identity
	accounts  accountStore

	mu         sync.Mutex
	running    []*runningEngine
	configured chan struct{}
	stopReq    chan time.Duration
}

type runningEngine struct {
	instance  string
	eng       engine.Engine
	endpoints []ipc.Endpoint
}

// NewRuntime prepares a camera runtime.
func NewRuntime(opts Options) *Runtime {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	rt := &Runtime{
		opts:       opts,
		log:        opts.Log,
		catalog:    engines.Builtin(),
		media:      newMediaStore(),
		configured: make(chan struct{}),
		stopReq:    make(chan time.Duration, 1),
	}
	rt.events = newEventBus(rt)
	rt.tel = newTelemetry(rt)
	return rt
}

// Accounts implements engine.Host.
func (r *Runtime) Accounts() engine.Accounts { return &r.accounts }

// State implements engine.Host.
func (r *Runtime) State() engine.State { return r.state }

// Events implements engine.Host.
func (r *Runtime) Events() engine.Events { return r.events }

// Media implements engine.Host.
func (r *Runtime) Media() engine.Media { return r.media }

// Templates implements engine.Host.
func (r *Runtime) Templates() engine.Templates { return r.templates }

// Files implements engine.Host; the simulated SD card is not part of the
// demo, so cameras have none.
func (r *Runtime) Files() engine.Files { return nil }

// Telemetry implements engine.Host.
func (r *Runtime) Telemetry() engine.Telemetry { return r.tel }

// now is the camera clock.
func (r *Runtime) now() time.Time {
	if r.state != nil {
		if v, ok := r.state.Canon("time.offset"); ok {
			if secs, ok := v.(int64); ok {
				return time.Now().Add(time.Duration(secs) * time.Second)
			}
		}
	}
	return time.Now()
}

// Run talks to the service until it asks the camera to stop or the socket
// closes; then it stops the engines and says bye.
func (r *Runtime) Run(ctx context.Context) error {
	r.conn = ipc.NewConn(r.opts.IPC, r.handle, r.log)
	runDone := make(chan error, 1)
	go func() { runDone <- r.conn.Run(ctx) }()

	if err := r.conn.Notify(ipc.TypeHello, ipc.Hello{CameraID: r.opts.CameraID, PID: os.Getpid(), Version: buildinfo.Version}); err != nil {
		return err
	}
	select {
	case <-r.configured:
	case <-time.After(30 * time.Second):
		r.conn.Close()
		return errors.New("no configuration received within 30s")
	case <-r.conn.Done():
		return errors.New("service closed the connection before configuring the camera")
	}

	cpu := &cpuSampler{}
	cpu.sample()
	ticker := time.NewTicker(ipc.HeartbeatInterval * time.Millisecond)
	defer ticker.Stop()
	deadline := 5 * time.Second
	reason := "stopped"
loop:
	for {
		select {
		case <-ticker.C:
			r.heartbeat(cpu)
		case d := <-r.stopReq:
			deadline = d
			break loop
		case <-ctx.Done():
			reason = "signal"
			break loop
		case <-r.conn.Done():
			reason = "service connection closed"
			break loop
		}
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	r.stopEngines(stopCtx)
	_ = r.conn.Notify(ipc.TypeBye, ipc.Bye{Reason: reason})
	r.conn.Close()
	<-runDone
	return nil
}

func (r *Runtime) handle(ctx context.Context, msg *ipc.Envelope) (any, error) {
	switch msg.Type {
	case ipc.TypeConfigure:
		var cfg ipc.Configure
		if err := msg.Decode(&cfg); err != nil {
			return nil, err
		}
		select {
		case <-r.configured:
			return nil, ipc.Errorf("state", "camera already configured; use reload")
		default:
		}
		ready, err := r.configure(ctx, &cfg)
		if err != nil {
			_ = r.conn.Notify(ipc.TypeFailed, ipc.Failed{Reason: err.Error()})
			return nil, err
		}
		close(r.configured)
		_ = r.conn.Notify(ipc.TypeReady, ready)
		return nil, nil
	case ipc.TypeReload:
		var rl ipc.Reload
		if err := msg.Decode(&rl); err != nil {
			return nil, err
		}
		return nil, r.reload(&rl)
	case ipc.TypeTrigger:
		var tr ipc.Trigger
		if err := msg.Decode(&tr); err != nil {
			return nil, err
		}
		ev, err := r.trigger(ctx, &tr)
		if err != nil {
			return nil, ipc.Errorf("trigger", "%v", err)
		}
		return ipc.TriggerResult{Event: ev}, nil
	case ipc.TypeStateSet:
		var ss ipc.StateSet
		if err := msg.Decode(&ss); err != nil {
			return nil, err
		}
		if r.state == nil {
			return nil, ipc.Errorf("state", "camera not configured")
		}
		changes, err := r.state.Set(ctx, ss.Values, ss.Origin)
		if err != nil {
			return nil, ipc.Errorf("invalid", "%v", err)
		}
		return changes, nil
	case ipc.TypeStop:
		var st ipc.Stop
		_ = msg.Decode(&st)
		d := time.Duration(st.DeadlineMS) * time.Millisecond
		if d <= 0 {
			d = 5 * time.Second
		}
		select {
		case r.stopReq <- d:
		default:
		}
		return nil, nil
	case ipc.TypeTargetTest:
		var tt ipc.TargetTest
		if err := msg.Decode(&tt); err != nil {
			return nil, err
		}
		return r.probeTarget(ctx, tt.Target), nil
	case ipc.TypeFaultStart, ipc.TypeFaultStop:
		return nil, ipc.Errorf("unsupported", "fault injection is not available in this version")
	}
	return nil, ipc.Errorf("unsupported", "unknown message type %q", msg.Type)
}

func (r *Runtime) configure(ctx context.Context, cfg *ipc.Configure) (ipc.Ready, error) {
	doc, err := profile.DecodeJSON(cfg.Profile)
	if err != nil {
		return ipc.Ready{}, fmt.Errorf("invalid profile: %w", err)
	}
	r.model = profile.NewModel(doc)
	r.identity = cfg.Identity
	r.accounts.set(cfg.Users)
	if len(cfg.DNS) > 0 {
		net.DefaultResolver = resolverFor(cfg.DNS)
	}
	r.state = newStateStore(r.model, cfg.State, func(changes []engine.Change) {
		if err := r.conn.Notify(ipc.TypeStateChanged, ipc.StateChanged{Changes: changes}); err != nil {
			r.log.Warn("cannot report state change", "error", err)
		}
	})
	r.templates = tmpl.NewCompiler(tmpl.Env{
		State:    r.state.Get,
		Canon:    r.state.Canon,
		Snapshot: func() ([]byte, error) { return r.media.Snapshot("main") },
	})
	if r.opts.Confine {
		if err := r.confine(cfg.Streams); err != nil {
			return ipc.Ready{}, err
		}
	}
	if err := r.media.replace(cfg.Streams); err != nil {
		return ipc.Ready{}, err
	}
	r.events.setTargets(cfg.Targets)

	enabled := map[string]ipc.EngineConfig{}
	for _, ec := range cfg.Engines {
		if ec.Enabled {
			enabled[ec.Instance] = ec
		}
	}
	var ready ipc.Ready
	for _, inst := range profile.SortedKeys(doc.Engines) {
		ec, ok := enabled[inst]
		if !ok {
			continue
		}
		re, err := r.startEngine(ctx, inst, doc.Engines[inst], ec)
		if err != nil {
			r.stopEngines(context.Background())
			return ipc.Ready{}, fmt.Errorf("engine %s: %w", inst, err)
		}
		ready.Endpoints = append(ready.Endpoints, re.endpoints...)
	}
	sort.Slice(ready.Endpoints, func(i, j int) bool { return ready.Endpoints[i].Instance < ready.Endpoints[j].Instance })
	return ready, nil
}

// confine limits the files the camera can reach, before its engines read
// anything from the LAN: its renditions, read only, and the system files
// name resolution and TLS need (audit B10). Outside local mode it cannot
// bind a TCP port either; its sockets are open already.
func (r *Runtime) confine(streams []ipc.Stream) error {
	read := []string{"/etc", "/usr/share/ca-certificates", "/usr/local/share/ca-certificates", "/usr/share/zoneinfo", "/proc"}
	seen := map[string]bool{}
	for _, st := range streams {
		for _, p := range []string{st.StreamPath, st.SnapshotPath} {
			// <data>/renditions/<key>/stream.h264: every rendition,
			// present and future, lives under <data>/renditions.
			root := filepath.Dir(filepath.Dir(p))
			if !seen[root] {
				seen[root] = true
				read = append(read, root)
			}
		}
	}
	applied, err := sandbox.Landlock(sandbox.Paths{Read: read, NoBind: !r.opts.Local})
	if err != nil {
		return err
	}
	if !applied {
		r.log.Warn("the kernel has no Landlock: the camera's files are not confined")
	}
	return nil
}

func (r *Runtime) startEngine(ctx context.Context, instance string, section []byte, ec ipc.EngineConfig) (*runningEngine, error) {
	name, rng, err := profile.EngineName(section)
	if err != nil {
		return nil, err
	}
	eng, err := r.catalog.Resolve(name, rng)
	if err != nil {
		return nil, err
	}
	desc := eng.Describe()
	in := engine.StartInput{
		Identity:    r.identity,
		Instance:    instance,
		Config:      section,
		Port:        ec.Port,
		Listeners:   map[string]net.Listener{},
		PacketConns: map[string]net.PacketConn{},
		Host:        r,
	}
	re := &runningEngine{instance: instance, eng: eng}
	for i, spec := range desc.Sockets {
		port := spec.DefaultPort
		if i == 0 && ec.Port > 0 {
			port = ec.Port
		}
		sock := r.inherited(instance, spec.Name)
		switch spec.Network {
		case "tcp":
			var ln net.Listener
			if sock != nil {
				ln, err = net.FileListener(sock.File)
				sock.File.Close()
				port = sock.Port
			} else if r.opts.Local {
				ln, err = net.Listen("tcp", "127.0.0.1:0")
				if err == nil {
					port = ln.Addr().(*net.TCPAddr).Port
				}
			} else {
				err = fmt.Errorf("socket %s was not opened for the camera", spec.Name)
			}
			if err != nil {
				return nil, err
			}
			in.Listeners[spec.Name] = ln
		case "udp":
			if sock == nil {
				continue // optional: RTSP falls back to TCP
			}
			pc, err := net.FilePacketConn(sock.File)
			sock.File.Close()
			if err != nil {
				return nil, err
			}
			port = sock.Port
			in.PacketConns[spec.Name] = pc
		}
		re.endpoints = append(re.endpoints, ipc.Endpoint{Instance: instance, Engine: desc.Name, Socket: spec.Name, Network: spec.Network, Port: port})
	}
	if err := eng.Start(ctx, in); err != nil {
		for _, ln := range in.Listeners {
			ln.Close()
		}
		for _, pc := range in.PacketConns {
			pc.Close()
		}
		return nil, err
	}
	r.mu.Lock()
	r.running = append(r.running, re)
	r.mu.Unlock()
	return re, nil
}

func (r *Runtime) inherited(instance, name string) *Socket {
	for i := range r.opts.Sockets {
		s := &r.opts.Sockets[i]
		if s.Instance == instance && s.Name == name && s.File != nil {
			return s
		}
	}
	return nil
}

func (r *Runtime) stopEngines(ctx context.Context) {
	r.mu.Lock()
	running := r.running
	r.running = nil
	r.mu.Unlock()
	for i := len(running) - 1; i >= 0; i-- {
		if err := running[i].eng.Stop(ctx); err != nil {
			r.log.Warn("engine stop", "instance", running[i].instance, "error", err)
		}
	}
}

func (r *Runtime) reload(rl *ipc.Reload) error {
	if r.model == nil {
		return ipc.Errorf("state", "camera not configured")
	}
	if rl.State != nil {
		r.state.replace(rl.State)
	}
	if rl.Targets != nil {
		r.events.setTargets(rl.Targets)
	}
	if rl.Users != nil {
		r.accounts.set(rl.Users)
	}
	if rl.Streams != nil {
		if err := r.media.replace(rl.Streams); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) trigger(ctx context.Context, tr *ipc.Trigger) (engine.Event, error) {
	if r.model == nil {
		return engine.Event{}, errors.New("camera not configured")
	}
	e := engine.Event{
		Type:      tr.Type,
		Trigger:   "manual",
		Direction: tr.Direction,
		Rule:      tr.Rule,
		Object:    tr.Object,
		Plate:     tr.Plate,
		Speed:     tr.Speed,
		Custom:    tr.Custom,
	}
	switch tr.Type {
	case "line_crossing":
		if e.Rule == nil {
			e.Rule = &engine.Rule{ID: "1", Name: "Line 1", Type: "line"}
		}
		if e.Direction == "" {
			e.Direction = "A->B"
		}
	case "region_entrance", "region_exit", "loitering", "intrusion":
		if e.Rule == nil {
			e.Rule = &engine.Rule{ID: "1", Name: "Region 1", Type: "region"}
		}
	}
	if e.Object == nil && e.Rule != nil {
		e.Object = &engine.Object{Class: "car", Confidence: 0.92}
	}
	return r.events.Emit(ctx, e)
}

func (r *Runtime) heartbeat(cpu *cpuSampler) {
	hb := ipc.Heartbeat{
		CPUPercent: cpu.sample(),
		RSSBytes:   rssBytes(),
		Requests:   r.tel.requests.Load(),
		Engines:    map[string]engine.Health{},
	}
	r.mu.Lock()
	running := append([]*runningEngine(nil), r.running...)
	r.mu.Unlock()
	for _, re := range running {
		h := re.eng.Health()
		hb.Engines[re.instance] = h
		if re.eng.Describe().Role == engine.RoleServer {
			hb.Clients += h.Clients
		}
		hb.BytesIn += h.BytesIn
		hb.BytesOut += h.BytesOut
	}
	if err := r.conn.Notify(ipc.TypeHeartbeat, hb); err != nil {
		r.log.Debug("heartbeat not sent", "error", err)
	}
}

// ParseSocket parses "instance:name:network:port=fd".
func ParseSocket(s string) (Socket, error) {
	bad := fmt.Errorf("socket %q must look like instance:name:network:port=fd", s)
	eq := strings.LastIndex(s, "=")
	if eq < 0 {
		return Socket{}, bad
	}
	parts := strings.SplitN(s[:eq], ":", 4)
	if len(parts) != 4 {
		return Socket{}, bad
	}
	port, err := strconv.Atoi(parts[3])
	if err != nil {
		return Socket{}, fmt.Errorf("socket %q: invalid port", s)
	}
	fd, err := strconv.Atoi(s[eq+1:])
	if err != nil || fd < 3 {
		return Socket{}, fmt.Errorf("socket %q: invalid descriptor", s)
	}
	return Socket{Instance: parts[0], Name: parts[1], Network: parts[2], Port: port, File: os.NewFile(uintptr(fd), s)}, nil
}
