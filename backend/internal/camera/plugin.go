package camera

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/sdk/engine"
	"github.com/corticoide/mockvision/sdk/plugin"
	pb "github.com/corticoide/mockvision/sdk/proto/mockvision/engine/v1"
)

// Plugin supervision (D85): a plugin that exits is started again after a
// growing wait; one that keeps exiting fails its camera.
const (
	pluginMaxFailures = 5
	// pluginSteady is how long a plugin runs before its failures are
	// forgotten.
	pluginSteady  = time.Minute
	pluginStartBy = 15 * time.Second
)

// The waits before starting a plugin again; tests shorten them.
var (
	pluginFirstWait = time.Second
	pluginMaxWait   = 16 * time.Second
)

// PluginBinary is the path of a plugin's program for this machine, inside
// its unpacked package.
func PluginBinary(p ipc.Plugin) string {
	return filepath.Join(p.Dir, "bin", "linux-"+runtime.GOARCH, p.Executable)
}

// pluginEngine runs an engine of an external plugin: a process the camera
// starts confined, with the sockets of its ports, and talks to over gRPC
// (D85, D86). It implements engine.Engine like the built-in ones.
type pluginEngine struct {
	spec  ipc.Plugin
	exe   string // the MockVision binary, which confines the plugin
	log   *slog.Logger
	fatal func(reason string)

	mu       sync.Mutex
	in       engine.StartInput
	proc     *pluginProc
	health   engine.Health
	failures int
	stopping bool
}

// pluginProc is one run of a plugin's process.
type pluginProc struct {
	cmd     *exec.Cmd
	cc      *grpc.ClientConn
	client  pb.EngineClient
	srv     *grpc.Server
	exited  chan struct{}
	err     error
	started time.Time
}

func newPluginEngine(spec ipc.Plugin, exe string, log *slog.Logger, fatal func(string)) *pluginEngine {
	return &pluginEngine{spec: spec, exe: exe, log: log.With("plugin", spec.Engine), fatal: fatal}
}

func (p *pluginEngine) Describe() engine.Descriptor { return p.spec.Descriptor }

// Validate is done when the profile is imported, against the plugin's
// configuration schema.
func (p *pluginEngine) Validate(json.RawMessage) []engine.Problem { return nil }

func (p *pluginEngine) Start(ctx context.Context, in engine.StartInput) error {
	p.mu.Lock()
	p.in = in
	p.mu.Unlock()
	proc, err := p.launch(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.proc = proc
	p.health = engine.Health{State: engine.HealthOK}
	p.mu.Unlock()
	go p.supervise(proc)
	return nil
}

// launch starts the process and its engine.
func (p *pluginEngine) launch(ctx context.Context) (*pluginProc, error) {
	p.mu.Lock()
	in := p.in
	p.mu.Unlock()
	granted := map[string]bool{}
	for _, perm := range p.spec.Permissions {
		granted[perm] = true
	}
	var socks []*os.File
	fds := map[string]int32{}
	closeAll := func() {
		for _, f := range socks {
			f.Close()
		}
	}
	if len(in.Listeners)+len(in.PacketConns) > 0 && !granted[plugin.PermNetListen] {
		return nil, fmt.Errorf("plugin %s needs its sockets but was not granted %s", p.spec.Engine, plugin.PermNetListen)
	}
	for _, name := range sortedListenerNames(in) {
		f, err := socketFile(in, name)
		if err != nil {
			closeAll()
			return nil, err
		}
		fds[name] = int32(plugin.FirstSocketFD + len(socks))
		socks = append(socks, f)
	}
	cmd, engineCam, hostCam, out, err := startPlugin(p.exe, p.spec, socks, !granted[plugin.PermNetConnect])
	closeAll()
	if err != nil {
		return nil, err
	}
	proc := &pluginProc{cmd: cmd, exited: make(chan struct{}), started: time.Now()}
	go p.logOutput(out)
	go func() {
		proc.err = cmd.Wait()
		close(proc.exited)
	}()
	hostConn, err1 := net.FileConn(hostCam)
	engineConn, err2 := net.FileConn(engineCam)
	hostCam.Close()
	engineCam.Close()
	if err := errors.Join(err1, err2); err != nil {
		p.kill(proc)
		return nil, err
	}
	proc.srv = grpc.NewServer(plugin.ServerOptions()...)
	pb.RegisterHostServer(proc.srv, newPluginHost(in.Host, in.Instance, p.spec.Permissions, p.log))
	go func() { _ = proc.srv.Serve(plugin.Listener(hostConn)) }()
	if proc.cc, err = plugin.Dial(engineConn); err != nil {
		p.kill(proc)
		return nil, err
	}
	proc.client = pb.NewEngineClient(proc.cc)
	sctx, cancel := context.WithTimeout(ctx, pluginStartBy)
	defer cancel()
	_, err = proc.client.Start(sctx, &pb.StartRequest{Identity: plugin.IdentityToProto(in.Identity), Instance: in.Instance,
		Config: in.Config, Port: int32(in.Port), SocketFds: fds})
	if err != nil {
		p.kill(proc)
		return nil, fmt.Errorf("plugin %s did not start: %w", p.spec.Engine, err)
	}
	p.log.Info("plugin started", "pid", cmd.Process.Pid, "instance", in.Instance)
	return proc, nil
}

// startPlugin starts a plugin's program confined by sandbox-exec, with
// the camera's ends of its Engine and Host connections.
func startPlugin(exe string, spec ipc.Plugin, socks []*os.File, noConnect bool) (*exec.Cmd, *os.File, *os.File, io.Reader, error) {
	engineCam, enginePlug, err := socketPair()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	hostCam, hostPlug, err := socketPair()
	if err != nil {
		engineCam.Close()
		enginePlug.Close()
		return nil, nil, nil, nil, err
	}
	args := []string{"sandbox-exec", "--read", spec.Dir}
	if noConnect {
		args = append(args, "--no-connect")
	}
	args = append(args, "--", PluginBinary(spec))
	cmd := exec.Command(exe, args...)
	cmd.ExtraFiles = append([]*os.File{enginePlug, hostPlug}, socks...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", plugin.EnvPlugin + "=1"}
	cmd.Dir = spec.Dir
	// A plugin dies with its camera.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	out, err := cmd.StdoutPipe()
	if err == nil {
		cmd.Stderr = cmd.Stdout
		err = cmd.Start()
	}
	enginePlug.Close()
	hostPlug.Close()
	if err != nil {
		engineCam.Close()
		hostCam.Close()
		return nil, nil, nil, nil, fmt.Errorf("plugin %s: %w", spec.Engine, err)
	}
	return cmd, engineCam, hostCam, out, nil
}

// DescribePlugin runs a plugin's program, confined and unable to connect
// anywhere, only to ask its descriptor: how the service checks a plugin
// package it installs against its manifest.
func DescribePlugin(ctx context.Context, exe string, spec ipc.Plugin) (engine.Descriptor, error) {
	cmd, engineCam, hostCam, out, err := startPlugin(exe, spec, nil, true)
	if err != nil {
		return engine.Descriptor{}, err
	}
	var output strings.Builder
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(&limitedWriter{w: &output, n: 4096}, out)
		close(copied)
	}()
	defer func() {
		_ = cmd.Process.Kill()
		<-copied
		_ = cmd.Wait()
	}()
	defer hostCam.Close()
	conn, err := net.FileConn(engineCam)
	engineCam.Close()
	if err != nil {
		return engine.Descriptor{}, err
	}
	cc, err := plugin.Dial(conn)
	if err != nil {
		return engine.Descriptor{}, err
	}
	defer cc.Close()
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	d, err := pb.NewEngineClient(cc).Describe(dctx, &pb.DescribeRequest{}, grpc.WaitForReady(true))
	if err != nil {
		_ = cmd.Process.Kill()
		<-copied
		if msg := strings.TrimSpace(output.String()); msg != "" {
			return engine.Descriptor{}, fmt.Errorf("the plugin did not describe itself: %w (it printed: %s)", err, msg)
		}
		return engine.Descriptor{}, fmt.Errorf("the plugin did not describe itself: %w", err)
	}
	return plugin.DescriptorFromProto(d), nil
}

// limitedWriter keeps the first n bytes written to it.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(b []byte) (int, error) {
	if l.n > 0 {
		k := min(len(b), l.n)
		_, _ = l.w.Write(b[:k])
		l.n -= k
	}
	return len(b), nil
}

func sortedListenerNames(in engine.StartInput) []string {
	var out []string
	for name := range in.Listeners {
		out = append(out, name)
	}
	for name := range in.PacketConns {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// socketFile duplicates a socket of the engine for the plugin's process.
func socketFile(in engine.StartInput, name string) (*os.File, error) {
	if ln, ok := in.Listeners[name]; ok {
		if g, ok := ln.(*gatedListener); ok {
			ln = g.Listener
		}
		if tl, ok := ln.(*net.TCPListener); ok {
			return tl.File()
		}
		return nil, fmt.Errorf("socket %s cannot be handed to a plugin", name)
	}
	if uc, ok := in.PacketConns[name].(*net.UDPConn); ok {
		return uc.File()
	}
	return nil, fmt.Errorf("socket %s cannot be handed to a plugin", name)
}

// socketPair returns a connected pair, close-on-exec until handed over.
func socketPair() (*os.File, *os.File, error) {
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[0]), "camera"), os.NewFile(uintptr(fds[1]), "plugin"), nil
}

// logOutput logs what the plugin prints, a line at a time.
func (p *pluginEngine) logOutput(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 4096)
	for sc.Scan() {
		p.log.Info("plugin output", "line", strings.TrimSpace(sc.Text()))
	}
}

func (p *pluginEngine) kill(proc *pluginProc) {
	release(proc)
	_ = proc.cmd.Process.Kill()
	<-proc.exited
}

// supervise starts the plugin again when it exits, after a growing wait;
// past pluginMaxFailures in a row it fails the camera.
func (p *pluginEngine) supervise(proc *pluginProc) {
	for {
		<-proc.exited
		release(proc)
		reason, started := "exited", proc.started
		if proc.err != nil {
			reason = "exited: " + proc.err.Error()
		}
		for {
			wait, again := p.failed(reason, started)
			if !again {
				return
			}
			time.Sleep(wait)
			p.mu.Lock()
			stopping := p.stopping
			p.mu.Unlock()
			if stopping {
				return
			}
			next, err := p.launch(context.Background())
			if err == nil {
				p.mu.Lock()
				p.proc = next
				p.health = engine.Health{State: engine.HealthOK}
				p.mu.Unlock()
				proc = next
				break
			}
			reason, started = "could not start: "+err.Error(), time.Now()
		}
	}
}

// failed counts a failure: the wait before starting the plugin again, or
// false when it is stopping or failed too many times in a row, which fails
// the camera.
func (p *pluginEngine) failed(reason string, started time.Time) (time.Duration, bool) {
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		return 0, false
	}
	if time.Since(started) > pluginSteady {
		p.failures = 0
	}
	p.failures++
	n := p.failures
	if n > pluginMaxFailures {
		p.health = engine.Health{State: engine.HealthFailed, Detail: fmt.Sprintf("%s, %d times in a row", reason, n)}
		p.mu.Unlock()
		p.log.Error("the plugin keeps failing", "failures", n, "reason", reason)
		p.fatal(fmt.Sprintf("plugin %s %s, %d times in a row", p.spec.Engine, reason, n))
		return 0, false
	}
	wait := min(pluginFirstWait<<(n-1), pluginMaxWait)
	p.health = engine.Health{State: engine.HealthFailed, Detail: fmt.Sprintf("%s; starting again in %s", reason, wait)}
	p.mu.Unlock()
	p.log.Warn("the plugin stopped; starting it again", "reason", reason, "in", wait)
	return wait, true
}

// release closes what a run of the plugin held.
func release(proc *pluginProc) {
	if proc.srv != nil {
		proc.srv.Stop()
	}
	if proc.cc != nil {
		proc.cc.Close()
	}
}

func (p *pluginEngine) Reload(ctx context.Context, config json.RawMessage) error {
	p.mu.Lock()
	p.in.Config = config
	proc := p.proc
	p.mu.Unlock()
	if proc == nil {
		return errors.New("the plugin is not running")
	}
	_, err := proc.client.Reload(ctx, &pb.ReloadRequest{Config: config})
	return err
}

func (p *pluginEngine) Health() engine.Health {
	p.mu.Lock()
	proc, h := p.proc, p.health
	p.mu.Unlock()
	if proc == nil || h.State != engine.HealthOK {
		return h
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := proc.client.Health(ctx, &pb.HealthRequest{})
	if err != nil {
		return engine.Health{State: engine.HealthDegraded, Detail: "the plugin does not answer"}
	}
	return plugin.HealthFromProto(res)
}

func (p *pluginEngine) Stop(ctx context.Context) error {
	p.mu.Lock()
	p.stopping = true
	proc := p.proc
	p.mu.Unlock()
	if proc == nil {
		return nil
	}
	deadline, ok := ctx.Deadline()
	ms := int64(5000)
	if ok {
		ms = time.Until(deadline).Milliseconds()
	}
	_, _ = proc.client.Stop(ctx, &pb.StopRequest{DeadlineMs: ms})
	select {
	case <-proc.exited:
	case <-ctx.Done():
		_ = proc.cmd.Process.Kill()
		<-proc.exited
	}
	release(proc)
	return nil
}
