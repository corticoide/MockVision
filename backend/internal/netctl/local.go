//go:build unix

package netctl

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"
)

// LocalRuntime runs cameras as ordinary processes on 127.0.0.1, with
// ephemeral ports and no network namespaces. It needs no privileges and
// exists for development, for example to work on the panel on a laptop.
type LocalRuntime struct {
	exe   string
	log   *slog.Logger
	exits chan Exit

	mu    sync.Mutex
	procs map[string]*localProc
}

type localProc struct {
	cmd      *exec.Cmd
	done     chan struct{}
	stopping bool
}

// NewLocalRuntime starts cameras from the given binary.
func NewLocalRuntime(exe string, log *slog.Logger) *LocalRuntime {
	if log == nil {
		log = slog.Default()
	}
	return &LocalRuntime{exe: exe, log: log, exits: make(chan Exit, 64), procs: map[string]*localProc{}}
}

// Kind implements Runtime.
func (l *LocalRuntime) Kind() string { return "local" }

// Exits implements Runtime.
func (l *LocalRuntime) Exits() <-chan Exit { return l.exits }

// Launch implements Runtime.
func (l *LocalRuntime) Launch(_ context.Context, spec LaunchSpec) (*Launched, error) {
	id := spec.Camera.ID
	l.mu.Lock()
	if _, ok := l.procs[id]; ok {
		l.mu.Unlock()
		return nil, errorf(CodeAlreadyExist, "camera %s is already running", id)
	}
	l.mu.Unlock()

	// Both ends are close-on-exec from the start: a process forked at the
	// same moment, such as FFmpeg or another camera, must not inherit them
	// (audit B16). ExtraFiles hands the camera its end anyway.
	fds, err := socketpairCloexec()
	if err != nil {
		return nil, err
	}
	svc := os.NewFile(uintptr(fds[0]), "camera-ipc")
	cam := os.NewFile(uintptr(fds[1]), "ipc")
	cmd := exec.Command(l.exe, "camera", "--id", id, "--ipc-fd", "3", "--local")
	cmd.ExtraFiles = []*os.File{cam}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	cam.Close()
	if err != nil {
		svc.Close()
		return nil, err
	}
	conn, err := net.FileConn(svc)
	svc.Close()
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	p := &localProc{cmd: cmd, done: make(chan struct{})}
	l.mu.Lock()
	l.procs[id] = p
	l.mu.Unlock()
	go l.reap(id, p)
	return &Launched{Conn: conn, PID: cmd.Process.Pid, IP: "127.0.0.1"}, nil
}

func (l *LocalRuntime) reap(id string, p *localProc) {
	err := p.cmd.Wait()
	close(p.done)
	l.mu.Lock()
	stopping := p.stopping
	delete(l.procs, id)
	l.mu.Unlock()
	if stopping {
		return
	}
	e := Exit{CameraID: id, PID: p.cmd.Process.Pid}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		e.Code = ee.ExitCode()
	}
	select {
	case l.exits <- e:
	default:
	}
}

// Destroy implements Runtime.
func (l *LocalRuntime) Destroy(_ context.Context, id string) error {
	l.mu.Lock()
	p := l.procs[id]
	if p != nil {
		p.stopping = true
	}
	l.mu.Unlock()
	if p == nil {
		return nil
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
	return nil
}

// Live implements Runtime.
func (l *LocalRuntime) Live(context.Context) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ids := make([]string, 0, len(l.procs))
	for id := range l.procs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// Shutdown stops every camera.
func (l *LocalRuntime) Shutdown() {
	ids, _ := l.Live(context.Background())
	for _, id := range ids {
		_ = l.Destroy(context.Background(), id)
	}
}
