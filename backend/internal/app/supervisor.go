package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
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
)

// session supervises one camera process from provisioning to its end.
type session struct {
	s  *Service
	id string

	mu        sync.Mutex
	name      string
	state     domain.CameraState
	reason    string
	started   time.Time
	lastHB    time.Time
	netns     string
	pid       int
	ip        string
	endpoints []ipc.Endpoint
	c         *ipc.Conn
	// applied are the restart-only settings the process was launched
	// with; the view compares them with the stored ones.
	applied map[string]string

	// lastSample and notices pace what the camera may send: a heartbeat
	// counts once a second at most, and logs, gaps and client notices
	// share a small budget (audit B1).
	lastSample time.Time
	notices    tokenBucket

	stopCh   chan string
	stopOnce sync.Once
	done     chan struct{}
	hello    chan ipc.Hello
	ready    chan ipc.Ready
	failed   chan string
}

type sessionSnapshot struct {
	state     domain.CameraState
	reason    string
	started   time.Time
	lastHB    time.Time
	netns     string
	pid       int
	ip        string
	endpoints []ipc.Endpoint
}

func (ss *session) snapshot() sessionSnapshot {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return sessionSnapshot{ss.state, ss.reason, ss.started, ss.lastHB, ss.netns, ss.pid, ss.ip, append([]ipc.Endpoint(nil), ss.endpoints...)}
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

func (s *Service) session(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
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
		_ = s.saveStatus(ctx, id, domain.StateStopped, "", time.Time{}, time.Now())
		s.publishStatus(id, domain.StateStopped, "")
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

func actorName(a Actor) string {
	switch {
	case a.Name != "":
		return a.Name
	case a.ID != "":
		return a.ID
	}
	return a.Type
}

func (s *Service) startSession(b *cameraBundle) *session {
	ss := &session{
		s: s, id: b.cam.ID, name: b.cam.Name, state: domain.StateStopped, applied: restartKeys(b),
		stopCh: make(chan string, 1), done: make(chan struct{}), notices: tokenBucket{tokens: noticeBurst, at: time.Now()},
		hello: make(chan ipc.Hello, 1), ready: make(chan ipc.Ready, 1), failed: make(chan string, 1),
	}
	s.mu.Lock()
	s.sessions[ss.id] = ss
	s.mu.Unlock()
	ss.setState(domain.StateProvisioning, "")
	go ss.run(b)
	return ss
}

func (ss *session) setState(st domain.CameraState, reason string) {
	ss.mu.Lock()
	prev := ss.state
	ss.state = st
	ss.reason = reason
	if st == domain.StateRunning && ss.started.IsZero() {
		ss.started = time.Now()
	}
	started := ss.started
	ss.mu.Unlock()
	if prev != st && !prev.CanTransition(st) && prev != domain.StateStopped {
		ss.s.log.Debug("unusual camera transition", "camera", ss.id, "from", prev, "to", st)
	}
	if err := ss.s.saveStatus(context.Background(), ss.id, st, reason, started, time.Now()); err != nil {
		ss.s.log.Warn("cannot save camera status", "camera", ss.id, "error", err)
	}
	ss.s.publishStatus(ss.id, st, reason)
}

func (s *Service) publishStatus(id string, st domain.CameraState, reason string) {
	msg := map[string]any{"id": id, "state": st, "reason": reason, "at": time.Now()}
	s.pub.Publish("cameras", "status", msg)
	s.pub.Publish("camera:"+id, "status", msg)
}

// run drives a camera from provisioning to the end of its process.
func (ss *session) run(b *cameraBundle) {
	s := ss.s
	defer func() {
		s.mu.Lock()
		if s.sessions[ss.id] == ss {
			delete(s.sessions, ss.id)
		}
		s.mu.Unlock()
		close(ss.done)
	}()
	ctx := s.baseCtx

	// Provisioning: streams and network identity. A stop that comes while
	// the streams encode ends the wait at once; the encoding job goes on
	// for whoever needs it next.
	streams, stopReason, err := ss.prepareStreamsUnlessStopped(ctx, b)
	if stopReason != "" {
		ss.finishStopped(stopReason)
		return
	}
	if err != nil {
		ss.fail(fmt.Sprintf("stream: %v", err), false)
		return
	}
	if reason, stopped := ss.stopRequested(); stopped {
		ss.finishStopped(reason)
		return
	}
	spec, err := s.cameraSpec(b)
	if err != nil {
		ss.fail(err.Error(), false)
		return
	}
	launched, err := s.rt.Launch(ctx, netctl.LaunchSpec{Camera: spec})
	if err != nil {
		ss.fail(launchReason(err), false)
		return
	}
	ss.mu.Lock()
	ss.pid, ss.netns, ss.ip = launched.PID, launched.Netns, launched.IP
	ss.mu.Unlock()

	// Starting: handshake over the private socket.
	ss.setState(domain.StateStarting, "")
	conn := ipc.NewConn(launched.Conn, ss.handle, s.log.With("camera", ss.id))
	ss.mu.Lock()
	ss.c = conn
	ss.mu.Unlock()
	go func() { _ = conn.Run(ctx) }()

	select {
	case <-ss.hello:
	case <-conn.Done():
		ss.fail(s.exitReason(ss.id, "the camera process exited during start-up"), true)
		return
	case reason := <-ss.stopCh:
		ss.gracefulStop(conn, reason)
		return
	case <-time.After(helloTimeout):
		ss.fail("the camera process did not say hello", true)
		return
	}
	cfg, err := s.buildConfigure(b, streams, launched.IP)
	if err != nil {
		ss.fail(err.Error(), true)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, readyTimeout)
	err = conn.Request(cctx, ipc.TypeConfigure, cfg, nil)
	cancel()
	if err != nil {
		ss.fail("configuration rejected: "+err.Error(), true)
		return
	}
	select {
	case r := <-ss.ready:
		ss.mu.Lock()
		ss.endpoints = r.Endpoints
		ss.lastHB = time.Now()
		ss.mu.Unlock()
	case reason := <-ss.failed:
		ss.fail(reason, true)
		return
	case <-conn.Done():
		ss.fail(s.exitReason(ss.id, "the camera process exited during start-up"), true)
		return
	case reason := <-ss.stopCh:
		ss.gracefulStop(conn, reason)
		return
	case <-time.After(readyTimeout):
		ss.fail("the camera did not become ready", true)
		return
	}
	ss.setState(domain.StateRunning, "")
	s.log.Info("camera running", "camera", ss.id, "name", ss.name, "ip", launched.IP, "netns", launched.Netns, "pid", launched.PID)

	// Running: supervise heartbeats until a stop or a failure.
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
			ss.fail(s.exitReason(ss.id, "the camera process exited"), true)
			return
		case <-stable.C:
			s.resetRetries(ss.id)
		case <-tick.C:
			ss.mu.Lock()
			last := ss.lastHB
			ss.mu.Unlock()
			if time.Since(last) > heartbeatTimeout {
				ss.fail("no response: 3 heartbeats missed", true)
				return
			}
		}
	}
}

// prepareStreamsUnlessStopped prepares the streams, giving up when the
// camera is stopped meanwhile; it then returns the stop's reason.
func (ss *session) prepareStreamsUnlessStopped(ctx context.Context, b *cameraBundle) ([]ipc.Stream, string, error) {
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopped := make(chan string, 1)
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		select {
		case reason := <-ss.stopCh:
			stopped <- reason
			cancel()
		case <-pctx.Done():
		}
	}()
	streams, err := ss.s.prepareStreams(pctx, b)
	cancel()
	<-watching
	select {
	case reason := <-stopped:
		return nil, reason, err
	default:
		return streams, "", err
	}
}

func (ss *session) stopRequested() (string, bool) {
	select {
	case r := <-ss.stopCh:
		return r, true
	default:
		return "", false
	}
}

func (ss *session) finishStopped(reason string) {
	ss.setState(domain.StateStopping, reason)
	_ = ss.s.rt.Destroy(context.Background(), ss.id)
	ss.setState(domain.StateStopped, "")
}

// gracefulStop asks the camera to stop, waits for it, then removes its
// namespace and interface.
func (ss *session) gracefulStop(conn *ipc.Conn, reason string) {
	ss.setState(domain.StateStopping, reason)
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
	ss.setState(domain.StateStopped, "")
}

// fail marks the camera in Error with its reason, cleans up and schedules
// a retry when the camera should be running (D13).
func (ss *session) fail(reason string, launched bool) {
	s := ss.s
	s.log.Warn("camera failed", "camera", ss.id, "name", ss.name, "reason", reason)
	if c := ss.conn(); c != nil {
		c.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = s.rt.Destroy(ctx, ss.id)
	cancel()
	s.metrics.Remove(ss.id)
	ss.setState(domain.StateError, reason)
	s.afterFailure(ss.id)
}

func launchReason(err error) string {
	var ne *netctl.Error
	if errors.As(err, &ne) {
		return ne.Message
	}
	return err.Error()
}

// exitReason explains why a camera process ended, with its exit status
// when the runtime reported it.
func (s *Service) exitReason(id, base string) string {
	time.Sleep(100 * time.Millisecond) // the exit notice may trail the socket closing
	s.mu.Lock()
	e, ok := s.exits[id]
	delete(s.exits, id)
	s.mu.Unlock()
	if !ok {
		return base
	}
	if e.Signal != "" {
		return fmt.Sprintf("%s (%s)", base, e.Signal)
	}
	return fmt.Sprintf("%s (exit code %d)", base, e.Code)
}

func (s *Service) watchExits(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-s.rt.Exits():
			s.mu.Lock()
			s.exits[e.CameraID] = e
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
		case ss.ready <- r:
		default:
		}
	case ipc.TypeFailed:
		var f ipc.Failed
		_ = msg.Decode(&f)
		select {
		case ss.failed <- f.Reason:
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
		s.recordEvent(ctx, ss.id, ev.Event)
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
		ID: b.cam.ID, Mode: string(domain.NetMacvlan), Parent: b.net.ParentIf, MAC: b.net.Mac,
		IP: b.net.Ip, Gateway: b.net.Gateway,
	}
	if s.rt.Kind() == "netns" {
		if spec.IP == "" {
			return spec, errors.New("the camera has no IP address; edit its network")
		}
		prefix, err := domain.MaskToPrefix(b.net.Netmask)
		if err != nil {
			return spec, err
		}
		spec.Prefix = prefix
		if spec.Parent == "" {
			spec.Parent = s.defaultParent(context.Background())
		}
		spec.Netns = s.netnsName(b)
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

func (s *Service) defaultParent(ctx context.Context) string {
	if p := s.Settings(ctx).ParentInterface; p != "" {
		return p
	}
	if s.opts.ParentInterface != "" {
		return s.opts.ParentInterface
	}
	info, _ := nodeInfo()
	return info.DefaultInterface
}

// netnsName is sim-<camera>, readable for "ip netns exec" (unique among
// running cameras).
func (s *Service) netnsName(b *cameraBundle) string {
	base := "sim-" + domain.Slug(b.cam.Name, 30)
	if base == "sim-" {
		base = "sim-cam"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ss := range s.sessions {
		if id == b.cam.ID {
			continue
		}
		ss.mu.Lock()
		taken := ss.netns == base
		ss.mu.Unlock()
		if taken {
			return base + "-" + strings.ToLower(b.cam.ID[len(b.cam.ID)-4:])
		}
	}
	return base
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
	_ = json.Unmarshal([]byte(b.net.DnsJson), &cfg.DNS)
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
		_ = s.saveStatus(ctx, id, domain.StateError, err.Error(), time.Time{}, time.Now())
		s.publishStatus(id, domain.StateError, err.Error())
		return
	}
	s.startSession(b)
}

// gaveUp reports whether automatic retries are exhausted.
func (s *Service) gaveUp(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.retries[id]
	return r != nil && r.attempts > domain.MaxAutoRetries
}

func (s *Service) retryPending(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.retries[id]
	return r != nil && r.timer != nil && r.attempts <= domain.MaxAutoRetries
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
