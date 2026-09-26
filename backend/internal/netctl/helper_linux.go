package netctl

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/vishvananda/netns"

	"github.com/corticoide/mockvision/backend/internal/ipc"
)

// HelperOptions configure the network helper.
type HelperOptions struct {
	// Exe is the MockVision binary started with the camera subcommand.
	Exe string
	// CameraUID and CameraGID are the unprivileged identity of cameras.
	CameraUID int
	CameraGID int
	Log       *slog.Logger
}

// Helper is the only privileged process: it creates and destroys cameras
// on behalf of the main service.
type Helper struct {
	opts HelperOptions
	host netns.NsHandle
	conn *seqConn

	mu   sync.Mutex
	cams map[string]*camProc
}

// camProc is a camera the helper created or is creating. ns, cmd and
// deleting are guarded by the helper's mutex: create sets them while
// delete and list read them (audit M6).
type camProc struct {
	spec     CameraSpec
	ns       *namespace
	cmd      *exec.Cmd
	done     chan struct{} // closed when the process ends
	created  chan struct{} // closed when create is over, whatever its outcome
	deleting bool
}

// testHookCreating, when set by a test, runs once a camera is registered
// and before anything is created for it.
var testHookCreating func()

// maxHelperRequests bounds the requests the helper handles at once.
const maxHelperRequests = 64

// createResult is the reply to camera.create.
type createResult struct {
	PID   int    `json:"pid"`
	Netns string `json:"netns"`
	Named bool   `json:"named"`
}

// liveCamera is an entry of camera.list.
type liveCamera struct {
	ID    string `json:"id"`
	PID   int    `json:"pid"`
	Netns string `json:"netns"`
	Alive bool   `json:"alive"`
}

// NewHelper prepares a helper serving requests on conn. It must be called
// from the node's network namespace; stale sim-* namespaces left by a
// previous run are removed.
func NewHelper(f *os.File, opts HelperOptions) (*Helper, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	host, err := netns.Get()
	if err != nil {
		return nil, err
	}
	conn, err := newSeqConn(f)
	if err != nil {
		host.Close()
		return nil, err
	}
	if removed := removeStaleNamespaces(); len(removed) > 0 {
		opts.Log.Info("removed namespaces left by a previous run", "namespaces", removed)
	}
	return &Helper{opts: opts, host: host, conn: conn, cams: map[string]*camProc{}}, nil
}

// Serve handles requests until the service closes its end.
func (h *Helper) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		h.conn.close()
	}()
	slots := make(chan struct{}, maxHelperRequests)
	for {
		env, fds, err := h.conn.recv()
		if err != nil {
			return err
		}
		slots <- struct{}{}
		go func() {
			defer func() { <-slots }()
			h.handle(env, fds)
		}()
	}
}

func (h *Helper) handle(env *ipc.Envelope, fds []int) {
	var data any
	var err error
	switch env.Type {
	case msgCreate:
		data, err = h.create(env, fds)
		fds = nil
	case msgDelete:
		var req struct {
			ID string `json:"id"`
		}
		if err = json.Unmarshal(env.Data, &req); err == nil {
			err = h.delete(req.ID)
		}
	case msgList:
		data = h.list()
	case msgPing:
	default:
		err = errorf(CodeUnsupported, "unknown request %q", env.Type)
	}
	closeFDs(fds)
	reply := &ipc.Envelope{V: ipc.Version, Re: env.ID}
	if err != nil {
		var ne *Error
		if !errors.As(err, &ne) {
			ne = &Error{Code: CodeInternal, Message: err.Error()}
		}
		reply.Error = &ipc.Error{Code: ne.Code, Message: ne.Message}
		h.opts.Log.Warn("helper request failed", "type", env.Type, "code", ne.Code, "error", ne.Message)
	} else {
		ok := true
		reply.OK = &ok
		if data != nil {
			reply.Data, _ = json.Marshal(data)
		}
	}
	if err := h.conn.send(reply); err != nil {
		h.opts.Log.Warn("helper reply failed", "error", err)
	}
}

func (h *Helper) create(env *ipc.Envelope, fds []int) (createResult, error) {
	if len(fds) != 1 {
		closeFDs(fds)
		return createResult{}, errorf(CodeInvalid, "camera.create needs exactly one socket")
	}
	ipcFile := os.NewFile(uintptr(fds[0]), "ipc")
	defer ipcFile.Close()
	var spec CameraSpec
	if err := json.Unmarshal(env.Data, &spec); err != nil {
		return createResult{}, errorf(CodeInvalid, "invalid camera spec: %v", err)
	}
	if err := spec.Validate(); err != nil {
		return createResult{}, errorf(CodeInvalid, "%v", err)
	}

	h.mu.Lock()
	if _, exists := h.cams[spec.ID]; exists {
		h.mu.Unlock()
		return createResult{}, errorf(CodeAlreadyExist, "camera %s already exists", spec.ID)
	}
	cp := &camProc{spec: spec, done: make(chan struct{}), created: make(chan struct{})}
	h.cams[spec.ID] = cp
	h.mu.Unlock()
	defer close(cp.created)
	if testHookCreating != nil {
		testHookCreating()
	}

	// fail undoes a creation that did not finish. A delete that came
	// meanwhile waits for it and finds nothing left to do.
	fail := func(err error) (createResult, error) {
		h.mu.Lock()
		ns := cp.ns
		cp.ns = nil
		if h.cams[spec.ID] == cp {
			delete(h.cams, spec.ID)
		}
		h.mu.Unlock()
		if ns != nil {
			_ = ns.delete()
		}
		return createResult{}, err
	}
	// deleted reports whether a delete came while the camera was created.
	deleted := func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return cp.deleting
	}
	errDeleted := errorf(CodeInvalid, "camera %s was deleted while it was being created", spec.ID)

	start := time.Now()
	ns, err := createNamespace(spec.Netns)
	if err != nil {
		return fail(err)
	}
	h.mu.Lock()
	cp.ns = ns
	h.mu.Unlock()
	if deleted() {
		return fail(errDeleted)
	}
	if err := setupInterface(h.host, ns, &spec); err != nil {
		return fail(err)
	}
	if deleted() {
		return fail(errDeleted)
	}
	files, flags, err := openSockets(ns, spec.Sockets)
	if err != nil {
		return fail(err)
	}
	cmd, err := h.start(ns, &spec, ipcFile, files, flags)
	for _, f := range files {
		f.Close()
	}
	if err != nil {
		return fail(err)
	}
	h.mu.Lock()
	cp.cmd = cmd
	deleting := cp.deleting
	h.mu.Unlock()
	go h.reap(cp)
	if deleting {
		// The waiting delete stops the process and removes the namespace.
		return createResult{}, errDeleted
	}
	h.opts.Log.Info("camera created", "id", spec.ID, "netns", ns.name, "ip", spec.IP, "mac", spec.MAC, "pid", cmd.Process.Pid, "took", time.Since(start).Round(time.Millisecond))
	return createResult{PID: cmd.Process.Pid, Netns: ns.name, Named: ns.named}, nil
}

func (h *Helper) start(ns *namespace, spec *CameraSpec, ipcFile *os.File, files []*os.File, flags []string) (*exec.Cmd, error) {
	args := []string{"camera", "--id", spec.ID, "--ipc-fd", "3",
		"--uid", strconv.Itoa(h.opts.CameraUID), "--gid", strconv.Itoa(h.opts.CameraGID)}
	for i, f := range flags {
		args = append(args, "--socket", f+"="+strconv.Itoa(4+i))
	}
	cmd := exec.Command(h.opts.Exe, args...)
	cmd.ExtraFiles = append([]*os.File{ipcFile}, files...)
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	// Cameras start as their unprivileged user, never as root: with the
	// helper's bounding set empty they hold no capability at any time
	// (audit B10). Each one gets its own PID namespace, so a camera cannot
	// signal the others although they share a user. Cameras die with the
	// helper; they also stop on their own when the service socket closes.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:    true,
		Pdeathsig:  syscall.SIGKILL,
		Credential: &syscall.Credential{Uid: uint32(h.opts.CameraUID), Gid: uint32(h.opts.CameraGID), Groups: []uint32{}},
		Cloneflags: syscall.CLONE_NEWPID,
	}
	if err := inNamespace(ns.fd, cmd.Start); err != nil {
		return nil, err
	}
	return cmd, nil
}

func (h *Helper) reap(cp *camProc) {
	err := cp.cmd.Wait()
	exit := Exit{CameraID: cp.spec.ID, PID: cp.cmd.Process.Pid}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			exit.Code = ws.ExitStatus()
			if ws.Signaled() {
				exit.Signal = ws.Signal().String()
			}
		}
	}
	close(cp.done)
	h.mu.Lock()
	deleting := cp.deleting
	h.mu.Unlock()
	if deleting {
		return
	}
	h.opts.Log.Warn("camera process exited", "id", exit.CameraID, "code", exit.Code, "signal", exit.Signal)
	raw, _ := json.Marshal(exit)
	_ = h.conn.send(&ipc.Envelope{V: ipc.Version, ID: ulid.Make().String(), Type: msgExited, At: time.Now().UnixMilli(), Data: raw})
}

// delete stops a camera process and removes its namespace. It is
// idempotent. A delete that comes while the camera is being created waits
// for the creation to end, so nothing it made is left behind.
func (h *Helper) delete(id string) error {
	h.mu.Lock()
	cp := h.cams[id]
	if cp == nil {
		h.mu.Unlock()
		return nil
	}
	cp.deleting = true
	h.mu.Unlock()

	<-cp.created
	h.mu.Lock()
	cmd := cp.cmd
	ns := cp.ns
	cp.ns = nil // one delete removes it, even when two run at once
	h.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-cp.done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-cp.done
		}
	}
	var err error
	if ns != nil {
		err = ns.delete()
	}
	h.mu.Lock()
	if h.cams[id] == cp {
		delete(h.cams, id)
	}
	h.mu.Unlock()
	h.opts.Log.Info("camera deleted", "id", id)
	return err
}

func (h *Helper) list() []liveCamera {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]liveCamera, 0, len(h.cams))
	for id, cp := range h.cams {
		lc := liveCamera{ID: id}
		if cp.ns != nil {
			lc.Netns = cp.ns.name
		}
		if cp.cmd != nil && cp.cmd.Process != nil {
			lc.PID = cp.cmd.Process.Pid
			select {
			case <-cp.done:
			default:
				lc.Alive = true
			}
		}
		out = append(out, lc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Shutdown stops every camera and removes their namespaces.
func (h *Helper) Shutdown() {
	h.mu.Lock()
	ids := make([]string, 0, len(h.cams))
	for id := range h.cams {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_ = h.delete(id)
		}(id)
	}
	wg.Wait()
	h.host.Close()
}
