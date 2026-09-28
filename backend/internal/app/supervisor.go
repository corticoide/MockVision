package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/netctl"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/telemetry"
	"github.com/corticoide/mockvision/sdk/engine"
)

// Timeouts of the camera lifecycle.
const (
	helloTimeout     = 10 * time.Second
	readyTimeout     = 20 * time.Second
	stopDeadline     = 5 * time.Second
	heartbeatTimeout = 3*time.Duration(ipc.HeartbeatInterval)*time.Millisecond + time.Second
	stableAfter      = time.Minute
	// exitNoticeWait is how long a failure waits for the process's exit
	// status, which may trail the socket closing.
	exitNoticeWait = 100 * time.Millisecond
)

// session supervises one camera process from provisioning to its end.
type session struct {
	s  *Service
	id string

	mu         sync.Mutex
	name       string
	state      domain.CameraState
	reasonCode string
	reason     string
	started    time.Time
	lastHB     time.Time
	netns      string
	pid        int
	ip         string
	endpoints  []ipc.Endpoint
	c          *ipc.Conn
	// applied are the restart-only settings the process was launched
	// with; the view compares them with the stored ones.
	applied map[string]string
	// streams are the ones the process serves.
	streams []ipc.Stream
	// stale is set when a change could not reach the process because it
	// was not running yet; it gets them once it runs.
	stale bool

	// lastSample and notices pace what the camera may send: a heartbeat
	// counts once a second at most, and logs, gaps and client notices
	// share a small budget (audit B1).
	lastSample time.Time
	notices    tokenBucket

	// Network: the address source, the MAC it answers with, the DNS
	// servers it uses and the firewall in place (nil: none).
	ipSource domain.IPSource
	mac      string
	dns      []string
	firewall *netctl.Firewall

	stopCh     chan string
	stopOnce   sync.Once
	done       chan struct{}
	hello      chan ipc.Hello
	ready      chan startReport
	leases     chan ipc.Lease
	dhcpFailed chan string
	dhcpLost   chan string
}

// startReport is how the camera answered its configuration: ready, or
// failed with a reason.
type startReport struct {
	ready  ipc.Ready
	failed string
}

type sessionSnapshot struct {
	state      domain.CameraState
	reasonCode string
	reason     string
	started    time.Time
	lastHB     time.Time
	netns      string
	pid        int
	ip         string
	endpoints  []ipc.Endpoint
	ipSource   domain.IPSource
	mac        string
	firewall   bool
}

func (ss *session) snapshot() sessionSnapshot {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return sessionSnapshot{ss.state, ss.reasonCode, ss.reason, ss.started, ss.lastHB, ss.netns, ss.pid, ss.ip,
		append([]ipc.Endpoint(nil), ss.endpoints...), ss.ipSource, ss.mac, ss.firewall != nil}
}

func (ss *session) active() bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.state.Active()
}

func (ss *session) conn() *ipc.Conn {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.c
}

func (ss *session) stop(reason string) {
	ss.stopOnce.Do(func() { ss.stopCh <- reason })
}

func (ss *session) markStale() {
	ss.mu.Lock()
	ss.stale = true
	ss.mu.Unlock()
}

// takeStale reports whether changes are waiting, and clears the mark.
func (ss *session) takeStale() bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	stale := ss.stale
	ss.stale = false
	return stale
}

func (s *Service) session(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// tellCamera sends a change to a running camera. A camera that is still
// starting gets it once it runs, with everything else that changed
// meanwhile. It reports whether the camera took it now.
func (s *Service) tellCamera(ctx context.Context, id, typ string, data any) bool {
	ss := s.session(id)
	if ss == nil {
		return false
	}
	if !ss.active() {
		ss.markStale()
		return false
	}
	if err := ss.conn().Request(ctx, typ, data, nil); err != nil {
		s.log.Warn("the camera did not apply a change", "camera", id, "type", typ, "error", err)
		return false
	}
	return true
}

// serves reports whether the camera's process serves these streams.
func (ss *session) serves(streams []ipc.Stream) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return slices.Equal(ss.streams, streams)
}

// StartCamera starts a camera; it returns while the camera provisions and
// reports progress through the cameras topic.
func (s *Service) StartCamera(ctx context.Context, actor Actor, id string) (*CameraView, error) {
	lock := s.opLock(id)
	lock.Lock()
	defer lock.Unlock()
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.session(id) == nil {
		if err := s.admitStart(ctx); err != nil {
			return nil, err
		}
	}
	if err := s.setDesired(ctx, id, domain.DesiredRunning); err != nil {
		return nil, err
	}
	s.cancelRetry(id)
	if s.session(id) == nil {
		s.startSession(b)
		s.audit(ctx, actor, "camera.start", "camera", id, nil)
	}
	return s.GetCamera(ctx, id)
}

// StopCamera stops a camera and waits until its namespace is gone.
func (s *Service) StopCamera(ctx context.Context, actor Actor, id string) (*CameraView, error) {
	lock := s.opLock(id)
	lock.Lock()
	defer lock.Unlock()
	if _, err := s.store.R().GetCamera(ctx, id); err != nil {
		return nil, store.NotFound(err)
	}
	if err := s.setDesired(ctx, id, domain.DesiredStopped); err != nil {
		return nil, err
	}
	s.cancelRetry(id)
	if ss := s.session(id); ss != nil {
		ss.stop("stopped by " + actorName(actor))
		select {
		case <-ss.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	} else {
		_ = s.rt.Destroy(ctx, id)
		_ = s.saveStatus(ctx, id, domain.StateStopped, "", "", time.Time{}, time.Now())
		s.publishStatus(id, domain.StateStopped, "", "")
	}
	s.audit(ctx, actor, "camera.stop", "camera", id, nil)
	return s.GetCamera(ctx, id)
}

// RestartCamera stops and starts a camera.
func (s *Service) RestartCamera(ctx context.Context, actor Actor, id string) (*CameraView, error) {
	if _, err := s.StopCamera(ctx, actor, id); err != nil {
		return nil, err
	}
	return s.StartCamera(ctx, actor, id)
}

// restartSession restarts a running camera's process, as a real camera
// reboots on a new address: the camera stays wanted running and, since it
// already ran, no admission applies. Only the session that asked is
// restarted, so a second request from it finds a new session and stops
// there.
func (s *Service) restartSession(old *session, code, reason string) {
	s.goBackground(func(ctx context.Context) {
		lock := s.opLock(old.id)
		lock.Lock()
		defer lock.Unlock()
		if s.session(old.id) != old {
			return
		}
		old.setState(domain.StateRestarting, code, reason)
		old.stop(reason)
		<-old.done
		b, err := s.loadBundle(ctx, old.id)
		if err != nil || b.cam.DesiredState != string(domain.DesiredRunning) {
			return
		}
		s.startSession(b)
		s.audit(ctx, Actor{Type: "system", Name: "DHCP"}, "camera.restart", "camera", old.id, map[string]string{"reason": reason})
	})
}

func actorName(a Actor) string {
	switch {
	case a.Name != "":
		return a.Name
	case a.ID != "":
		return a.ID
	}
	return a.Type
}

func (s *Service) startSession(b *cameraBundle) {
	ss := &session{
		s: s, id: b.cam.ID, name: b.cam.Name, state: domain.StateStopped, applied: restartKeys(b),
		stopCh: make(chan string, 1), done: make(chan struct{}), notices: tokenBucket{tokens: noticeBurst, at: time.Now()},
		hello: make(chan ipc.Hello, 1), ready: make(chan startReport, 1),
		leases: make(chan ipc.Lease, 4), dhcpFailed: make(chan string, 1), dhcpLost: make(chan string, 1),
	}
	s.mu.Lock()
	s.sessions[ss.id] = ss
	s.mu.Unlock()
	ss.setState(domain.StateProvisioning, "", "")
	go ss.run(b)
}

func (ss *session) setState(st domain.CameraState, code, reason string) {
	ss.mu.Lock()
	prev := ss.state
	ss.state, ss.reasonCode, ss.reason = st, code, reason
	if st == domain.StateRunning && ss.started.IsZero() {
		ss.started = time.Now()
	}
	started := ss.started
	ss.mu.Unlock()
	if prev != st && !prev.CanTransition(st) && prev != domain.StateStopped {
		ss.s.log.Debug("unusual camera transition", "camera", ss.id, "from", prev, "to", st)
	}
	if err := ss.s.saveStatus(context.Background(), ss.id, st, code, reason, started, time.Now()); err != nil {
		ss.s.log.Warn("cannot save camera status", "camera", ss.id, "error", err)
	}
	ss.s.publishStatus(ss.id, st, code, reason)
}

func (s *Service) publishStatus(id string, st domain.CameraState, code, reason string) {
	msg := map[string]any{"id": id, "state": st, "reason_code": code, "reason": reason, "at": time.Now()}
	s.pub.Publish("cameras", "status", msg)
	s.pub.Publish("camera:"+id, "status", msg)
}

// outcome ends a session before it runs: a stop asked meanwhile, or a
// failure. The zero value lets the session go on.
type outcome struct {
	stopped bool
	stop    string // the stop's reason
	fail    *failure
}

func (o outcome) over() bool { return o.stopped || o.fail != nil }

func stopped(reason string) outcome { return outcome{stopped: true, stop: reason} }

// orFail turns a phase's failure, if any, into its outcome.
func orFail(f *failure) outcome {
	if f == nil {
		return outcome{}
	}
	return outcome{fail: f}
}

// run drives a camera from provisioning to the end of its process, one
// phase after the other; each one returns early on a stop or a failure.
func (ss *session) run(b *cameraBundle) {
	s := ss.s
	defer ss.end()
	ctx := s.baseCtx

	// Before the process exists a stop or a failure has nothing to ask.
	early := func(o outcome) {
		if o.stopped {
			ss.finishStopped(o.stop)
		} else {
			ss.fail(o.fail)
		}
	}
	if o := ss.provision(ctx, b); o.over() {
		early(o)
		return
	}
	launched, spec, f := ss.launch(ctx, b)
	if f != nil {
		early(orFail(f))
		return
	}

	// Starting: handshake over the private socket.
	ss.setState(domain.StateStarting, "", "")
	conn := ipc.NewConn(launched.Conn, ss.handle, s.log.With("camera", ss.id))
	ss.mu.Lock()
	ss.c = conn
	ss.mu.Unlock()
	go func() { _ = conn.Run(ctx) }()
	finish := func(o outcome) {
		if o.stopped {
			ss.gracefulStop(conn, o.stop)
		} else {
			ss.fail(o.fail)
		}
	}
	if _, o := await(ss, conn, ss.hello, helloTimeout, failed(ReasonNoHello, "the camera process did not say hello")); o.over() {
		finish(o)
		return
	}
	ip := launched.IP
	if spec.DHCP() {
		res, o := ss.address(ctx, b, conn)
		if o.over() {
			finish(o)
			return
		}
		ip = res.ip
		ss.mu.Lock()
		ss.ip, ss.ipSource, ss.dns = ip, res.source, cameraDNS(b, res.dns)
		ss.mu.Unlock()
		s.saveAddress(ss.id, ip, res.source)
		// Its DNS servers may be the lease's now: what they resolve the
		// targets to is what its firewall must allow.
		s.refreshFirewalls(ctx, firewallRefresh, ss)
		s.log.Info("camera address set", "camera", ss.id, "ip", ip, "source", res.source)
		ss.setState(domain.StateStarting, "", "")
	}
	b, o := ss.configure(ctx, conn, ip)
	if o.over() {
		finish(o)
		return
	}
	ss.setState(domain.StateRunning, "", "")
	s.log.Info("camera running", "camera", ss.id, "name", ss.name, "ip", ip, "netns", launched.Netns, "pid", launched.PID)
	if ss.takeStale() {
		s.goBackground(func(ctx context.Context) { ss.sync(ctx) })
	}
	ss.supervise(ctx, conn, b)
}

// end forgets a session whose process is gone.
func (ss *session) end() {
	s := ss.s
	s.mu.Lock()
	if s.sessions[ss.id] == ss {
		delete(s.sessions, ss.id)
	}
	s.mu.Unlock()
	s.releaseNetns(ss.id)
	s.releaseAddresses(ss.id)
	close(ss.done)
}

// provision encodes what the camera's streams need before its process
// exists. A stop that comes meanwhile ends the wait at once; the encoding
// job goes on for whoever needs it next.
func (ss *session) provision(ctx context.Context, b *cameraBundle) outcome {
	_, o := ss.prepareStreamsUnlessStopped(ctx, b)
	if o.over() {
		return o
	}
	select {
	case reason := <-ss.stopCh:
		return stopped(reason)
	default:
		return outcome{}
	}
}

// launch has the runtime create the camera's namespace, its interface and
// its process; the firewall comes in place.
func (ss *session) launch(ctx context.Context, b *cameraBundle) (*netctl.Launched, netctl.CameraSpec, *failure) {
	s := ss.s
	spec, err := s.cameraSpec(b)
	if err != nil {
		return nil, spec, failed(ReasonConfig, "%v", err)
	}
	dns := cameraDNS(b, nil)
	if s.rt.Kind() == netctl.KindNetns {
		spec.Firewall = s.firewallFor(ctx, ss.id, dns)
	}
	launched, err := s.rt.Launch(ctx, netctl.LaunchSpec{Camera: spec})
	if err != nil {
		return nil, spec, launchFailure(err)
	}
	ss.mu.Lock()
	ss.pid, ss.netns, ss.ip, ss.mac, ss.dns = launched.PID, launched.Netns, launched.IP, launched.MAC, dns
	if launched.Firewall {
		ss.firewall = spec.Firewall
	}
	if !spec.DHCP() {
		ss.ipSource = domain.SourceStatic
	}
	ss.mu.Unlock()
	if !spec.DHCP() {
		s.saveAddress(ss.id, launched.IP, domain.SourceStatic)
	}
	return launched, spec, nil
}

// await waits during start-up for what ch brings; a stop, the end of the
// process or the timeout end the wait first.
func await[T any](ss *session, conn *ipc.Conn, ch <-chan T, timeout time.Duration, late *failure) (T, outcome) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	var zero T
	select {
	case v := <-ch:
		return v, outcome{}
	case <-conn.Done():
		return zero, outcome{fail: ss.exited("the camera process exited during start-up")}
	case reason := <-ss.stopCh:
		return zero, stopped(reason)
	case <-t.C:
		return zero, outcome{fail: late}
	}
}

// exited explains the end of the camera process.
func (ss *session) exited(base string) *failure {
	return failed(ReasonExited, "%s", ss.s.exitReason(ss.id, base))
}

// address gives a DHCP camera its address before it is configured: the
// one it leased, validated here and probed by the helper, or the profile's
// factory one when no server answered (D24). A refused lease is declined
// and the camera asks again.
func (ss *session) address(ctx context.Context, b *cameraBundle, conn *ipc.Conn) (addressResult, outcome) {
	s := ss.s
	ss.setState(domain.StateStarting, ReasonDHCPWaiting, "waiting for a DHCP lease")
	deadline := time.NewTimer(dhcpWait)
	defer deadline.Stop()
	for {
		select {
		case l := <-ss.leases:
			res, err := s.acceptLease(ctx, b, l)
			if err == nil {
				return res, outcome{}
			}
			s.log.Info("refused a DHCP lease", "camera", ss.id, "ip", l.IP, "reason", err)
			dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_ = conn.Request(dctx, ipc.TypeDHCPDecline, ipc.DHCPDecline{IP: l.IP, Reason: err.Error()}, nil)
			cancel()
		case reason := <-ss.dhcpFailed:
			s.log.Info("no DHCP lease; taking the factory address", "camera", ss.id, "reason", reason)
			res, f := s.factoryAddress(ctx, b, "no DHCP server answered")
			return res, orFail(f)
		case <-deadline.C:
			res, f := s.factoryAddress(ctx, b, "no DHCP lease within "+dhcpWait.String())
			return res, orFail(f)
		case reason := <-ss.stopCh:
			return addressResult{}, stopped(reason)
		case <-conn.Done():
			return addressResult{}, outcome{fail: ss.exited("the camera process exited while leasing its address")}
		}
	}
}

// configure sends the camera its configuration, read again now: what was
// edited while it encoded, launched or leased its address is in it. What
// is edited from here until it runs marks it stale, and reaches it then.
func (ss *session) configure(ctx context.Context, conn *ipc.Conn, ip string) (*cameraBundle, outcome) {
	s := ss.s
	ss.takeStale()
	b, err := s.loadBundle(ctx, ss.id)
	if err != nil {
		return nil, outcome{fail: failed(ReasonLaunch, "cannot read the camera: %v", err)}
	}
	// Encoded already, unless a stream changed meanwhile.
	streams, o := ss.prepareStreamsUnlessStopped(ctx, b)
	if o.over() {
		return b, o
	}
	cfg, err := s.buildConfigure(b, streams, ip)
	if err != nil {
		return b, outcome{fail: failed(ReasonConfig, "%v", err)}
	}
	ss.mu.Lock()
	cfg.DNS = ss.dns
	ss.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, readyTimeout)
	err = conn.Request(cctx, ipc.TypeConfigure, cfg, nil)
	cancel()
	if err != nil {
		return b, outcome{fail: failed(ReasonRejected, "configuration rejected: %v", err)}
	}
	r, o := await(ss, conn, ss.ready, readyTimeout, failed(ReasonNotReady, "the camera did not become ready"))
	if o.over() {
		return b, o
	}
	if r.failed != "" {
		return b, outcome{fail: failed(ReasonCameraFailed, "%s", r.failed)}
	}
	ss.mu.Lock()
	ss.endpoints, ss.streams, ss.lastHB = r.ready.Endpoints, streams, time.Now()
	ss.mu.Unlock()
	return b, outcome{}
}

// sync sends a camera that just started what was edited while it started:
// accounts, targets, parameters, rules and triggers and, when their
// settings changed, streams.
func (ss *session) sync(ctx context.Context) {
	s := ss.s
	b, err := s.loadBundle(ctx, ss.id)
	if err != nil {
		return
	}
	cfg, err := s.buildConfigure(b, nil, "")
	if err != nil {
		s.log.Warn("cannot bring the camera up to date", "camera", ss.id, "error", err)
		return
	}
	s.tellCamera(ctx, ss.id, ipc.TypeReload, ipc.Reload{Users: cfg.Users, Targets: &cfg.Targets, State: cfg.State, VCA: &cfg.VCA})
	s.regenerateStreamsLater(ss.id)
}

// supervise watches a running camera until a stop or a failure: its
// heartbeats and what its DHCP client reports.
func (ss *session) supervise(ctx context.Context, conn *ipc.Conn, b *cameraBundle) {
	s := ss.s
	tick := time.NewTicker(time.Duration(ipc.HeartbeatInterval) * time.Millisecond)
	defer tick.Stop()
	stable := time.NewTimer(stableAfter)
	defer stable.Stop()
	for {
		select {
		case reason := <-ss.stopCh:
			ss.gracefulStop(conn, reason)
			return
		case <-conn.Done():
			ss.fail(ss.exited("the camera process exited"))
			return
		case l := <-ss.leases:
			ss.leaseChanged(ctx, b, l)
		case reason := <-ss.dhcpLost:
			// RFC 2131: an expired lease's address is not used any more.
			// The camera starts again and leases one, or takes its factory
			// address, as when it booted.
			s.log.Warn("the camera lost its DHCP lease; restarting it", "camera", ss.id, "reason", reason)
			s.restartSession(ss, ReasonLeaseLost, "the DHCP lease expired: "+reason)
		case <-stable.C:
			s.resetRetries(ss.id)
		case <-tick.C:
			ss.mu.Lock()
			last := ss.lastHB
			ss.mu.Unlock()
			if time.Since(last) > heartbeatTimeout {
				ss.fail(failed(ReasonNoHeartbeat, "no response: 3 heartbeats missed"))
				return
			}
		}
	}
}

// leaseChanged applies a lease a running camera reports. Another address,
// or a lease after the factory fallback, is a new identity: the camera
// restarts with it, as a real one does. The same address with another
// prefix, router or DNS servers is applied in place.
func (ss *session) leaseChanged(ctx context.Context, b *cameraBundle, l ipc.Lease) {
	s := ss.s
	ss.mu.Lock()
	same := l.IP == ss.ip && ss.ipSource == domain.SourceDHCP
	oldDNS := ss.dns
	ss.mu.Unlock()
	if !same {
		s.log.Info("the camera's lease changed; restarting it", "camera", ss.id, "ip", l.IP)
		s.restartSession(ss, ReasonLeaseRestart, "the DHCP server leased it "+l.IP)
		return
	}
	res, err := s.acceptLease(ctx, b, l)
	if err != nil {
		s.log.Warn("cannot apply the renewed lease", "camera", ss.id, "error", err)
		return
	}
	dns := cameraDNS(b, res.dns)
	if slices.Equal(dns, oldDNS) {
		return
	}
	ss.mu.Lock()
	ss.dns = dns
	ss.mu.Unlock()
	s.tellCamera(ctx, ss.id, ipc.TypeReload, ipc.Reload{DNS: dns})
	s.refreshFirewalls(ctx, firewallRefresh, ss)
	s.log.Info("the camera's DNS servers changed with its lease", "camera", ss.id, "dns", dns)
}

// prepareStreamsUnlessStopped prepares the streams, giving up when the
// camera is stopped meanwhile.
func (ss *session) prepareStreamsUnlessStopped(ctx context.Context, b *cameraBundle) ([]ipc.Stream, outcome) {
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stoppedBy := make(chan string, 1)
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		select {
		case reason := <-ss.stopCh:
			stoppedBy <- reason
			cancel()
		case <-pctx.Done():
		}
	}()
	streams, err := ss.s.prepareStreams(pctx, b)
	cancel()
	<-watching
	select {
	case reason := <-stoppedBy:
		return nil, stopped(reason)
	default:
	}
	if err != nil {
		return nil, outcome{fail: failed(ReasonStream, "stream: %v", err)}
	}
	return streams, outcome{}
}

func (ss *session) finishStopped(reason string) {
	ss.setState(domain.StateStopping, "", reason)
	_ = ss.s.rt.Destroy(context.Background(), ss.id)
	ss.setState(domain.StateStopped, "", "")
}

// gracefulStop asks the camera to stop, waits for it, then removes its
// namespace and interface.
func (ss *session) gracefulStop(conn *ipc.Conn, reason string) {
	ss.mu.Lock()
	code := ss.reasonCode // a restart keeps its own
	if ss.state != domain.StateRestarting {
		code = ""
	}
	ss.mu.Unlock()
	ss.setState(domain.StateStopping, code, reason)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = conn.Request(ctx, ipc.TypeStop, ipc.Stop{DeadlineMS: stopDeadline.Milliseconds()}, nil)
	cancel()
	select {
	case <-conn.Done():
	case <-time.After(stopDeadline + 2*time.Second):
		conn.Close()
	}
	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := ss.s.rt.Destroy(dctx, ss.id); err != nil {
		ss.s.log.Warn("cannot remove camera namespace", "camera", ss.id, "error", err)
	}
	dcancel()
	ss.s.metrics.Remove(ss.id)
	ss.setState(domain.StateStopped, "", "")
}

// fail marks the camera in Error with its reason, cleans up and, when the
// camera should be running and trying again may help, schedules a retry
// (D13).
func (ss *session) fail(f *failure) {
	s := ss.s
	s.log.Warn("camera failed", "camera", ss.id, "name", ss.name, "code", f.code, "reason", f.text)
	if c := ss.conn(); c != nil {
		c.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = s.rt.Destroy(ctx, ss.id)
	cancel()
	s.metrics.Remove(ss.id)
	ss.setState(domain.StateError, f.code, f.text)
	if retryable(f.code) {
		s.afterFailure(ss.id)
	} else {
		s.giveUp(ss.id)
	}
}

// exitReason explains why a camera process ended, with its exit status
// when the runtime reports it in time.
func (s *Service) exitReason(id, base string) string {
	wait := time.NewTimer(exitNoticeWait)
	defer wait.Stop()
	for {
		s.mu.Lock()
		e, ok := s.exits[id]
		delete(s.exits, id)
		changed := s.exitsChanged
		s.mu.Unlock()
		switch {
		case ok && e.Signal != "":
			return fmt.Sprintf("%s (%s)", base, e.Signal)
		case ok:
			return fmt.Sprintf("%s (exit code %d)", base, e.Code)
		}
		select {
		case <-changed:
		case <-wait.C:
			return base
		}
	}
}

func (s *Service) watchExits(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-s.rt.Exits():
			s.mu.Lock()
			s.exits[e.CameraID] = e
			close(s.exitsChanged)
			s.exitsChanged = make(chan struct{})
			s.mu.Unlock()
		}
	}
}

// handle processes messages from a camera.
func (ss *session) handle(ctx context.Context, msg *ipc.Envelope) (any, error) {
	s := ss.s
	switch msg.Type {
	case ipc.TypeHello:
		var h ipc.Hello
		_ = msg.Decode(&h)
		select {
		case ss.hello <- h:
		default:
		}
	case ipc.TypeReady:
		var r ipc.Ready
		if err := msg.Decode(&r); err != nil {
			return nil, err
		}
		select {
		case ss.ready <- startReport{ready: r}:
		default:
		}
	case ipc.TypeFailed:
		var f ipc.Failed
		_ = msg.Decode(&f)
		select {
		case ss.ready <- startReport{failed: truncate(f.Reason, 512)}:
		default:
		}
	case ipc.TypeDHCPLease:
		var l ipc.Lease
		if err := msg.Decode(&l); err != nil {
			return nil, err
		}
		select {
		case ss.leases <- l:
		default:
			s.log.Warn("dropped a DHCP lease report", "camera", ss.id)
		}
	case ipc.TypeDHCPFailed:
		var st ipc.DHCPStatus
		_ = msg.Decode(&st)
		select {
		case ss.dhcpFailed <- truncate(st.Reason, 200):
		default:
		}
	case ipc.TypeDHCPLost:
		var st ipc.DHCPStatus
		_ = msg.Decode(&st)
		select {
		case ss.dhcpLost <- truncate(st.Reason, 200):
		default:
		}
	case ipc.TypeHeartbeat:
		var hb ipc.Heartbeat
		if err := msg.Decode(&hb); err != nil {
			return nil, err
		}
		now := time.Now()
		ss.mu.Lock()
		ss.lastHB = now
		sample := now.Sub(ss.lastSample) >= time.Second
		if sample {
			ss.lastSample = now
		}
		ss.mu.Unlock()
		if sample {
			s.metrics.Add(ss.id, s.clampSample(telemetry.CameraSample{At: now, CPUPercent: hb.CPUPercent, RSSBytes: hb.RSSBytes, Clients: hb.Clients,
				BytesIn: hb.BytesIn, BytesOut: hb.BytesOut, Requests: hb.Requests}))
		}
	case ipc.TypeEvent:
		var ev ipc.EventMsg
		if err := msg.Decode(&ev); err != nil {
			return nil, err
		}
		s.recordEvent(ctx, ss.id, ev.Event, ev.TriggerID)
	case ipc.TypeDelivery:
		var d engine.DeliveryReport
		if err := msg.Decode(&d); err != nil {
			return nil, err
		}
		s.recordDelivery(ctx, ss.id, d)
	case ipc.TypeStateChanged:
		var sc ipc.StateChanged
		if err := msg.Decode(&sc); err != nil {
			return nil, err
		}
		s.clientChanges(ctx, ss.id, sc.Changes)
	case ipc.TypeClient, ipc.TypeGap, ipc.TypeLog:
		if !ss.allowNotice() {
			return nil, nil // over budget: dropped
		}
		ss.notice(ctx, msg)
	case ipc.TypeBye:
	default:
		return nil, ipc.Errorf("unsupported", "unknown message type %q", msg.Type)
	}
	return nil, nil
}

// Budget of the notices a camera sends: logs, gaps and client changes.
const (
	noticeBurst = 50
	noticeRate  = 10.0 // per second
)

// tokenBucket paces a stream of messages.
type tokenBucket struct {
	tokens float64
	at     time.Time
}

func (b *tokenBucket) take(now time.Time, burst, rate float64) bool {
	b.tokens = min(burst, b.tokens+now.Sub(b.at).Seconds()*rate)
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (ss *session) allowNotice() bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.notices.take(time.Now(), noticeBurst, noticeRate)
}

// notice logs and publishes a client, gap or log message of the camera,
// with its strings cut to a sane size.
func (ss *session) notice(ctx context.Context, msg *ipc.Envelope) {
	s := ss.s
	switch msg.Type {
	case ipc.TypeClient:
		var c ipc.ClientMsg
		_ = msg.Decode(&c)
		c.Protocol, c.IP = truncate(c.Protocol, 16), truncate(c.IP, 64)
		s.pub.Publish("camera:"+ss.id, "client", c)
	case ipc.TypeGap:
		var g ipc.Gap
		_ = msg.Decode(&g)
		g.Protocol, g.ClientIP, g.Summary = truncate(g.Protocol, 16), truncate(g.ClientIP, 64), truncate(g.Summary, 512)
		s.log.Info("request unknown to the profile", "camera", ss.id, "protocol", g.Protocol, "client", g.ClientIP, "request", g.Summary, "count", g.Count)
		s.pub.Publish("camera:"+ss.id, "gap", g)
	case ipc.TypeLog:
		var l ipc.Log
		_ = msg.Decode(&l)
		level := slog.LevelInfo
		_ = level.UnmarshalText([]byte(l.Level))
		l.Msg = truncate(l.Msg, 1024)
		if raw, _ := json.Marshal(l.Attrs); len(raw) > 4096 {
			l.Attrs = map[string]any{"attrs": "dropped: larger than 4 KiB"}
		}
		s.log.Log(ctx, level, l.Msg, "camera", ss.id, "attrs", l.Attrs)
		s.pub.Publish("camera:"+ss.id, "log", l)
	}
}

// clampSample keeps a camera's reported metrics within what the node can
// hold, so a camera cannot skew admission or the dashboard.
func (s *Service) clampSample(c telemetry.CameraSample) telemetry.CameraSample {
	cpus := max(s.node.CPUCount(), 1)
	c.CPUPercent = min(max(c.CPUPercent, 0), float64(100*cpus))
	if total := s.node.Latest().MemTotal; total > 0 {
		c.RSSBytes = min(c.RSSBytes, total)
	}
	c.Clients = max(c.Clients, 0)
	return c
}

// maxChanges bounds the parameter changes of one message.
const maxChanges = 256

// clientChanges stores changes a client made through the emulated API.
// The camera's report is checked against the profile again: unknown keys
// and invalid values are dropped, and the binding comes from the profile,
// not from the camera (audit B1).
func (s *Service) clientChanges(ctx context.Context, id string, reported []engine.Change) {
	if len(reported) == 0 {
		return
	}
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return
	}
	if len(reported) > maxChanges {
		reported = reported[:maxChanges]
	}
	var changes []engine.Change
	for _, c := range reported {
		p, ok := b.doc.State[c.Key]
		if !ok {
			s.log.Warn("camera reported a change of an unknown parameter", "camera", id, "key", truncate(c.Key, 64))
			continue
		}
		v, err := profile.Coerce(p, c.Value)
		if err != nil {
			s.log.Warn("camera reported an invalid value", "camera", id, "key", c.Key, "error", err)
			continue
		}
		changes = append(changes, engine.Change{Key: c.Key, Value: v, Bind: p.Bind, Origin: c.Origin})
	}
	if len(changes) == 0 {
		return
	}
	ip := ""
	if a, err := netip.ParseAddr(changes[0].Origin.IP); err == nil {
		ip = a.String()
	}
	if err := s.persistChanges(ctx, id, changes, "client:"+ip); err != nil {
		s.log.Warn("cannot store camera change", "camera", id, "error", err)
		return
	}
	diff := map[string]any{}
	for _, c := range changes {
		diff[c.Key] = c.Value
	}
	s.audit(ctx, Actor{Type: "camera", ID: id, IP: ip}, "camera.config", "camera", id, diff)
	s.afterChanges(id, changes)
}

// cameraSpec builds the network helper request of a camera.
func (s *Service) cameraSpec(b *cameraBundle) (netctl.CameraSpec, error) {
	spec := netctl.CameraSpec{
		ID: b.cam.ID, Mode: b.net.Mode, Parent: b.net.ParentIf, MAC: b.net.Mac, IPMode: string(domain.IPStatic),
		IP: b.net.Ip, Gateway: b.net.Gateway, Force: store.Bool(b.net.Force),
	}
	if spec.Mode == "" {
		spec.Mode = string(domain.NetMacvlan)
	}
	if s.rt.Kind() == netctl.KindNetns {
		if spec.Parent == "" {
			spec.Parent = s.defaultParent(context.Background())
		}
		spec.Netns = s.netnsName(b)
		if b.net.IpMode == string(domain.IPDHCP) {
			spec.IPMode, spec.IP, spec.Gateway = string(domain.IPDHCP), "", ""
			spec.Hostname = domain.DHCPHostname(b.cam.Name)
		} else {
			if spec.IP == "" {
				return spec, errors.New("the camera has no IP address; edit its network")
			}
			prefix, err := domain.MaskToPrefix(b.net.Netmask)
			if err != nil {
				return spec, err
			}
			spec.Prefix = prefix
		}
	}
	for _, p := range b.protos {
		if !store.Bool(p.Enabled) || p.Port == 0 {
			continue
		}
		name, rng, err := engineOf(b, p.EngineKey)
		if err != nil {
			return spec, err
		}
		eng, err := s.catalog.Resolve(name, rng)
		if err != nil {
			return spec, err
		}
		for i, so := range eng.Describe().Sockets {
			port := so.DefaultPort
			if i == 0 {
				port = int(p.Port)
			}
			spec.Sockets = append(spec.Sockets, netctl.SocketSpec{Instance: p.EngineKey, Name: so.Name, Network: so.Network, Port: port})
		}
	}
	return spec, nil
}

func engineOf(b *cameraBundle, instance string) (string, string, error) {
	section, ok := b.doc.Engines[instance]
	if !ok {
		return "", "", fmt.Errorf("profile has no engine instance %s", instance)
	}
	var head struct {
		Engine string `json:"engine"`
	}
	if err := json.Unmarshal(section, &head); err != nil {
		return "", "", err
	}
	name, rng, _ := strings.Cut(head.Engine, "@")
	return name, rng, nil
}

// netnsName is sim-<camera>, readable for "ip netns exec". The name is
// reserved under the service lock until the camera's session ends, so two
// cameras whose names give the same slug cannot ask for it at once (audit
// B12); the one that comes second gets a suffix from its ID.
func (s *Service) netnsName(b *cameraBundle) string {
	base := "sim-" + domain.Slug(b.cam.Name, 30)
	if base == "sim-" {
		base = "sim-cam"
	}
	id := strings.ToLower(b.cam.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, owner := range s.netnsNames {
		if owner == b.cam.ID {
			delete(s.netnsNames, name)
		}
	}
	for _, name := range []string{base, base + "-" + id[len(id)-4:], base + "-" + id[len(id)-8:]} {
		if owner, taken := s.netnsNames[name]; !taken || owner == b.cam.ID {
			s.netnsNames[name] = b.cam.ID
			return name
		}
	}
	// Eight characters of a ULID's random part do not repeat in practice;
	// the whole ID always is unique.
	name := "sim-" + id
	s.netnsNames[name] = b.cam.ID
	return name
}

// releaseNetns frees the namespace name of a camera whose session ended.
func (s *Service) releaseNetns(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, owner := range s.netnsNames {
		if owner == id {
			delete(s.netnsNames, name)
		}
	}
}

// buildConfigure assembles the full configuration sent to the camera.
func (s *Service) buildConfigure(b *cameraBundle, streams []ipc.Stream, ip string) (ipc.Configure, error) {
	doc := b.doc
	firmware := doc.Identity.Device["firmware"]
	if firmware == "" && len(doc.Profile.Firmware) > 0 {
		firmware = doc.Profile.Firmware[0]
	}
	model := doc.Identity.Device["model"]
	if model == "" {
		model = doc.Profile.Model
	}
	cfg := ipc.Configure{
		Identity: engine.Identity{
			CameraID: b.cam.ID, Name: b.cam.Name, IP: ip, MAC: b.net.Mac, Serial: b.cam.Serial,
			Vendor: doc.Profile.Vendor, Model: model, Firmware: firmware,
			ProfileID: b.prof.ProfileID, ProfileVersion: b.prof.Version,
		},
		Profile: json.RawMessage(b.prof.ResolvedJson),
		State:   b.values(),
		Streams: streams,
	}
	for _, p := range b.protos {
		cfg.Engines = append(cfg.Engines, ipc.EngineConfig{Instance: p.EngineKey, Enabled: store.Bool(p.Enabled), Port: int(p.Port)})
	}
	for _, u := range b.users {
		pw, err := s.box.Open(u.PasswordEnc, "camera_users:"+b.cam.ID+":"+u.Username)
		if err != nil {
			return cfg, fmt.Errorf("cannot decrypt the password of %s: %w", u.Username, err)
		}
		cfg.Users = append(cfg.Users, engine.User{Username: u.Username, Password: string(pw), Role: u.Role})
	}
	targets, err := s.ipcTargets(b)
	if err != nil {
		return cfg, err
	}
	cfg.Targets = targets
	cfg.VCA = vcaConfig(b)
	return cfg, nil
}

// endpointViews converts the endpoints a camera reported.
func (s *Service) endpointViews(b *cameraBundle, ip string, eps []ipc.Endpoint) []EndpointView {
	conv := make([]ipcEndpoint, 0, len(eps))
	for _, e := range eps {
		if e.Network != "tcp" {
			continue
		}
		conv = append(conv, ipcEndpoint{instance: e.Instance, socket: e.Socket, network: e.Network, port: e.Port})
	}
	return s.endpointViewsFrom(b, ip, conv)
}

// afterFailure schedules an automatic restart with a growing wait, up to
// MaxAutoRetries attempts, when the camera should be running (D13).
func (s *Service) afterFailure(id string) {
	cam, err := s.store.R().GetCamera(context.Background(), id)
	if err != nil || cam.DesiredState != string(domain.DesiredRunning) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.retries[id]
	if r == nil {
		r = &retryState{}
		s.retries[id] = r
	}
	r.attempts++
	if r.attempts > domain.MaxAutoRetries {
		s.log.Warn("camera gave up after automatic retries", "camera", id, "retries", domain.MaxAutoRetries)
		return
	}
	wait := domain.RetryBackoff(r.attempts)
	r.timer = time.AfterFunc(wait, func() { s.retryStart(id) })
	s.log.Info("camera will retry", "camera", id, "attempt", r.attempts, "in", wait)
}

func (s *Service) retryStart(id string) {
	lock := s.opLock(id)
	lock.Lock()
	defer lock.Unlock()
	if s.session(id) != nil {
		return
	}
	ctx := s.baseCtx
	b, err := s.loadBundle(ctx, id)
	if err != nil || b.cam.DesiredState != string(domain.DesiredRunning) {
		return
	}
	if err := s.admitStart(ctx); err != nil {
		_ = s.saveStatus(ctx, id, domain.StateError, ReasonAdmission, err.Error(), time.Time{}, time.Now())
		s.publishStatus(id, domain.StateError, ReasonAdmission, err.Error())
		return
	}
	s.startSession(b)
}

// giveUp ends the automatic retries of a camera whose failure needs a
// change first, such as a busy network card: the reconciler leaves it in
// error until someone starts it again.
func (s *Service) giveUp(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.retries[id]
	if r == nil {
		r = &retryState{}
		s.retries[id] = r
	}
	if r.timer != nil {
		r.timer.Stop()
	}
	r.stopped = true
}

// gaveUp reports whether automatic retries are exhausted or stopped.
func (s *Service) gaveUp(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.retries[id]
	return r != nil && (r.stopped || r.attempts > domain.MaxAutoRetries)
}

func (s *Service) retryPending(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.retries[id]
	return r != nil && r.timer != nil && !r.stopped && r.attempts <= domain.MaxAutoRetries
}

func (s *Service) cancelRetry(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.retries[id]; r != nil && r.timer != nil {
		r.timer.Stop()
	}
	delete(s.retries, id)
}

func (s *Service) resetRetries(id string) {
	s.mu.Lock()
	delete(s.retries, id)
	s.mu.Unlock()
}

// reconcileLoop compares desired and actual state at start-up and every 10
// seconds: it starts what should run, stops what should not and removes
// orphans.
func (s *Service) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		s.Reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Reconcile runs one reconciliation pass.
func (s *Service) Reconcile(ctx context.Context) {
	cams, err := s.cameraIDs(ctx)
	if err != nil {
		return
	}
	known := map[string]bool{}
	for _, c := range cams {
		known[c.ID] = true
		ss := s.session(c.ID)
		switch domain.DesiredState(c.DesiredState) {
		case domain.DesiredRunning:
			if ss == nil && !s.retryPending(c.ID) && !s.gaveUp(c.ID) {
				s.retryStart(c.ID)
			}
		case domain.DesiredStopped:
			if ss != nil {
				ss.stop("desired state is stopped")
			}
		}
	}
	live, err := s.rt.Live(ctx)
	if err != nil {
		return
	}
	for _, id := range live {
		if s.session(id) == nil {
			s.log.Info("removing orphan camera", "camera", id, "known", known[id])
			_ = s.rt.Destroy(ctx, id)
		}
	}
}

// ipcTargets returns the enabled targets of a camera with their secrets.
func (s *Service) ipcTargets(b *cameraBundle) ([]ipc.Target, error) {
	out := []ipc.Target{}
	for _, t := range b.targets {
		if !store.Bool(t.Enabled) {
			continue
		}
		var cfg targetConfig
		if err := json.Unmarshal([]byte(t.ConfigJson), &cfg); err != nil {
			return nil, err
		}
		var pw string
		if len(t.SecretEnc) > 0 {
			p, err := s.box.Open(t.SecretEnc, "targets:"+t.ID)
			if err != nil {
				return nil, fmt.Errorf("cannot decrypt the secret of target %s", t.Name)
			}
			pw = string(p)
		}
		var types []string
		_ = json.Unmarshal([]byte(t.EventTypesJson), &types)
		out = append(out, ipc.Target{
			Target: engine.Target{ID: t.ID, Name: t.Name, Type: t.Type, URL: cfg.URL, Method: cfg.Method,
				Headers: cfg.Headers, Username: cfg.Username, Password: pw},
			EventTypes: types,
		})
	}
	return out, nil
}
