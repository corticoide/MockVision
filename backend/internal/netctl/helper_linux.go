package netctl

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/vishvananda/netns"

	"github.com/corticoide/mockvision/backend/internal/domain"
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

	// bridgeMu serializes the bridge changes; bridge is guarded by mu.
	bridgeMu sync.Mutex
	bridge   *bridge
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
	stopping chan struct{} // closed when a delete begins
	deleting bool
	// addr is the camera's address: its static one, or the one a DHCP
	// camera got; invalid until then. Guarded by the helper's mutex.
	addr netip.Addr

	// op serializes what changes a running camera (address, firewall,
	// bridge) with its delete, which removes the namespace they work on.
	op sync.Mutex
}

// testHookCreating, when set by a test, runs once a camera is registered
// and before anything is created for it.
var testHookCreating func()

// maxHelperRequests bounds the requests the helper handles at once.
const maxHelperRequests = 64

// createResult is the reply to camera.create.
type createResult struct {
	PID      int    `json:"pid"`
	Netns    string `json:"netns"`
	Named    bool   `json:"named"`
	MAC      string `json:"mac"`
	Firewall bool   `json:"firewall"`
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
	if removeStaleBridge() {
		opts.Log.Info("removed the bridge left by a previous run", "interface", bridgeName)
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
	case msgAddress:
		var req AddressSpec
		if err = json.Unmarshal(env.Data, &req); err == nil {
			err = h.setAddress(&req)
		}
	case msgFirewall:
		var req firewallRequest
		if err = json.Unmarshal(env.Data, &req); err == nil {
			err = h.setFirewall(&req)
		}
	case msgBridge:
		var req BridgeSpec
		if err = json.Unmarshal(env.Data, &req); err == nil {
			data, err = h.setBridge(&req)
		}
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
	cp := &camProc{spec: spec, done: make(chan struct{}), created: make(chan struct{}), stopping: make(chan struct{})}
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
			removeLinks(ns)
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
	link, err := setupInterface(h.host, ns, &spec, cp.stopping)
	if err != nil {
		return fail(err)
	}
	for _, w := range link.Warnings {
		h.opts.Log.Warn("camera started despite a conflict, as forced", "id", spec.ID, "conflict", w)
	}
	if deleted() {
		return fail(errDeleted)
	}
	firewall := false
	if spec.Firewall != nil {
		if err := applyFirewall(int(ns.fd), spec.Sockets, spec.DHCP(), spec.Firewall); err != nil {
			h.opts.Log.Warn("the camera runs without firewall", "id", spec.ID, "error", err)
		} else {
			firewall = true
		}
	}
	files, flags, err := openSockets(ns, spec.Sockets)
	if err != nil {
		return fail(err)
	}
	var dhcp *os.File
	if spec.DHCP() {
		if dhcp, err = openDHCPSocket(ns); err != nil {
			for _, f := range files {
				f.Close()
			}
			return fail(err)
		}
	}
	cmd, err := h.start(ns, &spec, ipcFile, files, flags, dhcp)
	for _, f := range files {
		f.Close()
	}
	if dhcp != nil {
		dhcp.Close()
	}
	if err != nil {
		return fail(err)
	}
	h.mu.Lock()
	cp.cmd = cmd
	if !spec.DHCP() {
		cp.addr, _ = netip.ParseAddr(spec.IP)
	}
	deleting := cp.deleting
	h.mu.Unlock()
	go h.reap(cp)
	if deleting {
		// The waiting delete stops the process and removes the namespace.
		return createResult{}, errDeleted
	}
	h.attachBridge(cp)
	h.opts.Log.Info("camera created", "id", spec.ID, "netns", ns.name, "mode", spec.Mode, "ip", spec.IP, "dhcp", spec.DHCP(),
		"mac", link.MAC, "firewall", firewall, "pid", cmd.Process.Pid, "took", time.Since(start).Round(time.Millisecond))
	return createResult{PID: cmd.Process.Pid, Netns: ns.name, Named: ns.named, MAC: link.MAC, Firewall: firewall}, nil
}

// camera returns a camera that finished its creation and is not being
// deleted, locked for a change; unlock it with cp.op.Unlock.
func (h *Helper) camera(id string) (*camProc, error) {
	h.mu.Lock()
	cp := h.cams[id]
	h.mu.Unlock()
	if cp == nil {
		return nil, errorf(CodeInvalid, "camera %s is not running", id)
	}
	<-cp.created
	cp.op.Lock()
	h.mu.Lock()
	gone := cp.deleting || cp.ns == nil || cp.cmd == nil
	h.mu.Unlock()
	if gone {
		cp.op.Unlock()
		return nil, errorf(CodeInvalid, "camera %s is not running", id)
	}
	return cp, nil
}

// setAddress gives a DHCP camera the address the service chose for it.
func (h *Helper) setAddress(a *AddressSpec) error {
	if err := a.Validate(); err != nil {
		return errorf(CodeInvalid, "%v", err)
	}
	cp, err := h.camera(a.ID)
	if err != nil {
		return err
	}
	defer cp.op.Unlock()
	if !cp.spec.DHCP() {
		return errorf(CodeInvalid, "camera %s has a static address", a.ID)
	}
	warnings, err := setAddress(h.host, cp.ns, &cp.spec, a, cp.stopping)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		h.opts.Log.Warn("camera took its address despite a conflict, as forced", "id", a.ID, "conflict", w)
	}
	ip, _ := netip.ParseAddr(a.IP)
	h.mu.Lock()
	old := cp.addr
	cp.addr = ip
	b := h.bridge
	h.mu.Unlock()
	if b != nil && old.IsValid() && old != ip {
		b.detach(h.host, nil, old)
	}
	h.attachBridgeLocked(cp)
	h.opts.Log.Info("camera address set", "id", a.ID, "ip", a.IP, "prefix", a.Prefix, "gateway", a.Gateway)
	return nil
}

// firewallRequest replaces a running camera's firewall.
type firewallRequest struct {
	ID       string   `json:"id"`
	Firewall Firewall `json:"firewall"`
}

func (h *Helper) setFirewall(req *firewallRequest) error {
	if !idPattern.MatchString(req.ID) {
		return errorf(CodeInvalid, "invalid camera id %q", req.ID)
	}
	if err := req.Firewall.Validate(); err != nil {
		return errorf(CodeInvalid, "%v", err)
	}
	cp, err := h.camera(req.ID)
	if err != nil {
		return err
	}
	defer cp.op.Unlock()
	return applyFirewall(int(cp.ns.fd), cp.spec.Sockets, cp.spec.DHCP(), &req.Firewall)
}

// setBridge turns the node's access to its cameras on or off (D26).
func (h *Helper) setBridge(req *BridgeSpec) (BridgeState, error) {
	if req.Enabled && !ifacePattern.MatchString(req.Parent) {
		return BridgeState{}, errorf(CodeInvalid, "invalid parent interface %q", req.Parent)
	}
	h.bridgeMu.Lock()
	defer h.bridgeMu.Unlock()
	h.mu.Lock()
	old := h.bridge
	h.bridge = nil
	cams := make([]*camProc, 0, len(h.cams))
	for _, cp := range h.cams {
		cams = append(cams, cp)
	}
	h.mu.Unlock()
	if old != nil {
		for _, cp := range cams {
			if cp.op.TryLock() {
				h.mu.Lock()
				ns, ip := cp.ns, cp.addr
				h.mu.Unlock()
				if ip.IsValid() && cp.spec.Parent == old.parent {
					old.detach(h.host, ns, ip)
				}
				cp.op.Unlock()
			}
		}
		old.remove(h.host)
		h.opts.Log.Info("bridge removed", "interface", bridgeName)
	}
	if !req.Enabled {
		return BridgeState{}, nil
	}
	b, err := createBridge(h.host, req.Parent)
	if err != nil {
		var ne *Error
		if errors.As(err, &ne) {
			return BridgeState{Error: ne.Message}, nil
		}
		return BridgeState{}, err
	}
	h.mu.Lock()
	h.bridge = b
	h.mu.Unlock()
	for _, cp := range cams {
		<-cp.created
		cp.op.Lock()
		h.attachBridgeLocked(cp)
		cp.op.Unlock()
	}
	h.opts.Log.Info("bridge created", "interface", bridgeName, "parent", req.Parent, "node_ips", b.addrs())
	return BridgeState{Enabled: true, Interface: bridgeName, Parent: req.Parent, NodeIPs: b.addrs()}, nil
}

// attachBridge routes a new camera through the bridge, if there is one.
func (h *Helper) attachBridge(cp *camProc) {
	cp.op.Lock()
	defer cp.op.Unlock()
	h.attachBridgeLocked(cp)
}

// attachBridgeLocked does it with cp.op held. Only macvlan cameras on the
// bridge's parent need it; ipvlan ones share the node's MAC and are left
// out (the node cannot reach them).
func (h *Helper) attachBridgeLocked(cp *camProc) {
	h.mu.Lock()
	b, ns, ip, gone := h.bridge, cp.ns, cp.addr, cp.deleting
	h.mu.Unlock()
	if b == nil || gone || ns == nil || !ip.IsValid() || cp.spec.Mode != string(domain.NetMacvlan) || cp.spec.Parent != b.parent {
		return
	}
	if err := b.attach(h.host, ns, ip); err != nil {
		h.opts.Log.Warn("cannot route the camera through the bridge", "id", cp.spec.ID, "error", err)
	}
}

func (h *Helper) start(ns *namespace, spec *CameraSpec, ipcFile *os.File, files []*os.File, flags []string, dhcp *os.File) (*exec.Cmd, error) {
	args := []string{"camera", "--id", spec.ID, "--ipc-fd", "3",
		"--uid", strconv.Itoa(h.opts.CameraUID), "--gid", strconv.Itoa(h.opts.CameraGID)}
	for i, f := range flags {
		args = append(args, "--socket", f+"="+strconv.Itoa(4+i))
	}
	extra := append([]*os.File{ipcFile}, files...)
	if dhcp != nil {
		// A DHCP camera leases its address itself, with its MAC and host
		// name, over the socket opened for it.
		args = append(args, "--dhcp-fd", strconv.Itoa(3+len(extra)), "--mac", spec.MAC, "--hostname", spec.Hostname)
		extra = append(extra, dhcp)
	}
	cmd := exec.Command(h.opts.Exe, args...)
	cmd.ExtraFiles = extra
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
	if !cp.deleting {
		cp.deleting = true
		close(cp.stopping)
	}
	h.mu.Unlock()

	<-cp.created
	cp.op.Lock()
	h.mu.Lock()
	cmd := cp.cmd
	ns := cp.ns
	cp.ns = nil // one delete removes it, even when two run at once
	b, ip := h.bridge, cp.addr
	h.mu.Unlock()
	cp.op.Unlock()
	if b != nil && ip.IsValid() && ns != nil {
		b.detach(h.host, nil, ip)
	}

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
		removeLinks(ns)
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
	h.bridgeMu.Lock()
	if h.bridge != nil {
		h.bridge.remove(h.host)
		h.bridge = nil
	}
	h.bridgeMu.Unlock()
	h.host.Close()
}
