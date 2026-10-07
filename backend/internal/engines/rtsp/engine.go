// Package rtsp implements the rtsp engine: an RTSP server that loops a
// precoded stream per camera stream: an H.264 or H.265 group of pictures,
// or MJPEG frames. The stream is encoded once from an image, so a still
// picture costs almost no CPU; timestamps grow continuously across loops so
// clients never see a jump.
package rtsp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/auth"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/headers"
	"github.com/pion/rtp"

	"github.com/corticoide/mockvision/backend/internal/tmpl"
	"github.com/corticoide/mockvision/sdk/engine"
)

// Name and Version identify the engine in profiles (rtsp@^1).
const (
	Name    = "rtsp"
	Version = "1.0.0"
)

// Config is the profile section of an rtsp instance.
type Config struct {
	Engine string `json:"engine"`
	Port   int    `json:"port,omitempty"`
	Auth   struct {
		Scheme string `json:"scheme"`
		// Realm is the realm of the challenge, ipcam if empty. It may
		// name the camera, as Dahua's "Login to {{ .Camera.Serial }}".
		Realm string `json:"realm,omitempty"`
	} `json:"auth"`
	// Server is the Server header of the responses, gortsplib if empty.
	Server string `json:"server,omitempty"`
	// Paths maps stream names to request paths. A path may carry a query,
	// as Dahua does: /cam/realmonitor?channel=1&subtype=0.
	Paths map[string]string `json:"paths"`
}

const configSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["engine", "paths"],
  "properties": {
    "engine": {"type": "string"},
    "port": {"type": "integer", "minimum": 1, "maximum": 65535},
    "auth": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "scheme": {"enum": ["digest", "basic", "none"]},
        "realm": {"type": "string", "maxLength": 64}
      }
    },
    "server": {"type": "string", "maxLength": 128},
    "paths": {
      "type": "object",
      "minProperties": 1,
      "propertyNames": {"enum": ["main", "sub", "third"]},
      "additionalProperties": {"type": "string", "pattern": "^/"}
    }
  }
}`

// Engine is an RTSP server for one camera.
type Engine struct {
	in     engine.StartInput
	cfg    Config
	server *gortsplib.Server

	mu         sync.RWMutex
	streams    map[string]*streamer // by stream name
	realm      string
	serverName string
	nonces     sync.Map // digest nonce by *gortsplib.ServerConn
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	unwatch    func()

	state    atomic.Value
	sessions atomic.Int64
}

// sessionState is what the engine keeps about an RTSP session: whether
// its SETUP was authorized, and the stream it plays.
type sessionState struct {
	mu         sync.Mutex
	authorized bool
	playing    *streamer
}

func stateOf(ss *gortsplib.ServerSession) *sessionState {
	if st, ok := ss.UserData().(*sessionState); ok {
		return st
	}
	st := &sessionState{}
	ss.SetUserData(st)
	return st
}

// New returns an engine instance.
func New() engine.Engine {
	e := &Engine{streams: map[string]*streamer{}}
	e.state.Store(engine.HealthStopped)
	return e
}

// Describe implements engine.Engine.
func (e *Engine) Describe() engine.Descriptor {
	return engine.Descriptor{
		Name:         Name,
		Version:      Version,
		Contract:     engine.Contract,
		Role:         engine.RoleServer,
		ConfigSchema: json.RawMessage(configSchema),
		Sockets: []engine.SocketSpec{
			{Name: "rtsp", Network: "tcp", DefaultPort: 554},
			{Name: "rtp", Network: "udp", DefaultPort: 6970},
			{Name: "rtcp", Network: "udp", DefaultPort: 6971},
		},
	}
}

// Validate implements engine.Engine.
func (e *Engine) Validate(config json.RawMessage) []engine.Problem {
	var c Config
	if err := json.Unmarshal(config, &c); err != nil {
		return []engine.Problem{{Message: err.Error()}}
	}
	var probs []engine.Problem
	seen := map[string]string{}
	for name, p := range c.Paths {
		if other, dup := seen[p]; dup {
			probs = append(probs, engine.Problem{Path: "/paths/" + name, Message: fmt.Sprintf("path %s is also used by stream %s", p, other)})
		}
		seen[p] = name
	}
	if strings.Contains(c.Auth.Realm, "{{") {
		if err := tmpl.Check("realm", c.Auth.Realm); err != nil {
			probs = append(probs, engine.Problem{Path: "/auth/realm", Message: err.Error()})
		}
	} else if err := checkRealm(c.Auth.Realm); err != nil {
		probs = append(probs, engine.Problem{Path: "/auth/realm", Message: err.Error()})
	}
	return probs
}

// defaultRealm is the realm of cameras whose profile names none.
const defaultRealm = "ipcam"

// renderRealm renders the realm of the challenge, which may name the
// camera; it is rendered when the engine starts or reloads.
func (e *Engine) renderRealm(text string) (string, error) {
	realm := text
	if strings.Contains(text, "{{") {
		t, err := e.in.Host.Templates().Compile("realm", text, 256)
		if err != nil {
			return "", fmt.Errorf("rtsp: auth realm: %w", err)
		}
		id := e.in.Identity
		out, err := t.Render(context.Background(), engine.TemplateData{Now: time.Now(), Camera: engine.CameraData{
			ID: id.CameraID, Name: id.Name, IP: id.IP, MAC: id.MAC, Serial: id.Serial,
			Vendor: id.Vendor, Model: id.Model, Firmware: id.Firmware,
		}})
		if err != nil {
			return "", fmt.Errorf("rtsp: auth realm: %w", err)
		}
		realm = strings.TrimSpace(string(out))
	}
	if err := checkRealm(realm); err != nil {
		return "", fmt.Errorf("rtsp: %w", err)
	}
	if realm == "" {
		realm = defaultRealm
	}
	return realm, nil
}

// checkRealm rejects what a quoted challenge parameter cannot carry.
func checkRealm(realm string) error {
	if strings.ContainsAny(realm, "\"\\\r\n") {
		return fmt.Errorf("realm %q: quotes, backslashes and line breaks cannot go in a challenge", realm)
	}
	return nil
}

// Start implements engine.Engine.
func (e *Engine) Start(ctx context.Context, in engine.StartInput) error {
	e.in = in
	if err := json.Unmarshal(in.Config, &e.cfg); err != nil {
		return err
	}
	realm, err := e.renderRealm(e.cfg.Auth.Realm)
	if err != nil {
		return err
	}
	e.realm, e.serverName = realm, e.cfg.Server
	ln := in.Listeners["rtsp"]
	if ln == nil {
		return errors.New("rtsp: no listener for socket rtsp")
	}
	s := &gortsplib.Server{
		Handler:     e,
		RTSPAddress: ln.Addr().String(),
		Listen: func(string, string) (net.Listener, error) {
			return ln, nil
		},
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		AuthMethods:  authMethods(e.cfg.Auth.Scheme),
	}
	rtp, rtcp := in.PacketConns["rtp"], in.PacketConns["rtcp"]
	if rtp != nil && rtcp != nil {
		s.UDPRTPAddress = rtp.LocalAddr().String()
		s.UDPRTCPAddress = rtcp.LocalAddr().String()
		s.ListenPacket = func(_ string, address string) (net.PacketConn, error) {
			switch address {
			case s.UDPRTPAddress:
				return rtp, nil
			case s.UDPRTCPAddress:
				return rtcp, nil
			}
			return nil, fmt.Errorf("unexpected UDP address %s", address)
		}
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("rtsp: %w", err)
	}
	e.server = s

	runCtx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	for name := range e.cfg.Paths {
		if err := e.startStream(runCtx, name); err != nil {
			cancel()
			s.Close()
			return err
		}
	}
	e.unwatch = in.Host.Media().Watch(func(stream string) {
		e.mu.RLock()
		_, ok := e.cfg.Paths[stream]
		e.mu.RUnlock()
		if !ok {
			return
		}
		if err := e.startStream(runCtx, stream); err != nil {
			in.Host.Telemetry().Log(slog.LevelError, "rtsp: cannot reload stream", "stream", stream, "error", err)
		}
	})
	e.state.Store(engine.HealthOK)
	return nil
}

func authMethods(scheme string) []auth.VerifyMethod {
	switch scheme {
	case "basic":
		return []auth.VerifyMethod{auth.VerifyMethodBasic}
	case "none":
		return []auth.VerifyMethod{auth.VerifyMethodBasic, auth.VerifyMethodDigestMD5}
	default:
		return []auth.VerifyMethod{auth.VerifyMethodDigestMD5}
	}
}

// startStream (re)creates the server stream of one camera stream. Readers
// of a replaced stream are disconnected, as on a real camera whose encoder
// settings changed.
func (e *Engine) startStream(ctx context.Context, name string) error {
	src, err := e.in.Host.Media().Source(name)
	if err != nil {
		return fmt.Errorf("rtsp: stream %s: %w", name, err)
	}
	forma, err := formatFor(src)
	if err != nil {
		return fmt.Errorf("rtsp: stream %s: %w", name, err)
	}
	desc := &description.Session{
		Medias: []*description.Media{{Type: description.MediaTypeVideo, Formats: []format.Format{forma}}},
	}
	ss := &gortsplib.ServerStream{Server: e.server, Desc: desc}
	if err := ss.Initialize(); err != nil {
		return err
	}
	st := &streamer{engine: e, stream: ss, src: src, done: make(chan struct{})}
	sctx, cancel := context.WithCancel(ctx)
	st.cancel = cancel

	e.mu.Lock()
	old := e.streams[name]
	e.streams[name] = st
	e.mu.Unlock()
	if old != nil {
		old.stop()
	}

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		st.run(sctx)
	}()
	return nil
}

// Reload implements engine.Engine. Paths can change in place; port changes
// restart the camera.
func (e *Engine) Reload(ctx context.Context, config json.RawMessage) error {
	var c Config
	if err := json.Unmarshal(config, &c); err != nil {
		return err
	}
	realm, err := e.renderRealm(c.Auth.Realm)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.cfg.Paths = c.Paths
	e.realm, e.serverName = realm, c.Server
	e.mu.Unlock()
	return nil
}

// Health implements engine.Engine.
func (e *Engine) Health() engine.Health {
	var out uint64
	e.mu.RLock()
	for _, st := range e.streams {
		out += st.stream.Stats().OutboundBytes
	}
	e.mu.RUnlock()
	return engine.Health{
		State:    e.state.Load().(engine.HealthState),
		Clients:  int(e.sessions.Load()),
		BytesOut: out,
	}
}

// Stop implements engine.Engine.
func (e *Engine) Stop(context.Context) error {
	if e.unwatch != nil {
		e.unwatch()
	}
	if e.cancel != nil {
		e.cancel()
	}
	e.mu.Lock()
	for _, st := range e.streams {
		st.stop()
	}
	e.mu.Unlock()
	e.wg.Wait()
	if e.server != nil {
		e.server.Close()
	}
	e.state.Store(engine.HealthStopped)
	return nil
}

// streamFor finds the stream served at a request path and query.
func (e *Engine) streamFor(path, query string) *streamer {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for name, p := range e.cfg.Paths {
		wantPath, wantQuery, _ := strings.Cut(p, "?")
		if strings.TrimRight(path, "/") != strings.TrimRight(wantPath, "/") {
			continue
		}
		if wantQuery != "" && !sameQuery(wantQuery, query) {
			continue
		}
		return e.streams[name]
	}
	return nil
}

func sameQuery(want, got string) bool {
	w := strings.Split(want, "&")
	g := map[string]bool{}
	for _, kv := range strings.Split(got, "&") {
		g[kv] = true
	}
	for _, kv := range w {
		if !g[kv] {
			return false
		}
	}
	return true
}

// authorize checks the request credentials against the camera users. It
// returns nil for an authorized request, else the 401 that challenges the
// client with the camera's realm, as the camera answers every failed
// attempt.
func (e *Engine) authorize(conn *gortsplib.ServerConn, req *base.Request) *base.Response {
	if e.cfg.Auth.Scheme == "none" {
		return nil
	}
	e.mu.RLock()
	realm := e.realm
	e.mu.RUnlock()
	methods := authMethods(e.cfg.Auth.Scheme)
	nonce := e.nonce(conn)
	// Accounts are read on every request: an edited password applies at
	// once.
	var h headers.Authorization
	if err := h.Unmarshal(req.Header["Authorization"]); err == nil {
		if u, ok := e.in.Host.Accounts().Lookup(h.Username); ok && auth.Verify(req, u.Username, u.Password, methods, realm, nonce) == nil {
			return nil
		}
	}
	return &base.Response{
		StatusCode: base.StatusUnauthorized,
		Header:     base.Header{"WWW-Authenticate": auth.GenerateWWWAuthenticate(methods, realm, nonce)},
	}
}

// nonce is the digest nonce of a connection, made at its first request.
func (e *Engine) nonce(conn *gortsplib.ServerConn) string {
	if n, ok := e.nonces.Load(conn); ok {
		return n.(string)
	}
	n, _ := auth.GenerateNonce()
	actual, _ := e.nonces.LoadOrStore(conn, n)
	return actual.(string)
}

// OnConnOpen implements gortsplib.ServerHandlerOnConnOpen.
func (e *Engine) OnConnOpen(ctx *gortsplib.ServerHandlerOnConnOpenCtx) {
	e.in.Host.Telemetry().Client("rtsp", hostOnly(ctx.Conn.NetConn().RemoteAddr().String()), true)
}

// OnConnClose implements gortsplib.ServerHandlerOnConnClose.
func (e *Engine) OnConnClose(ctx *gortsplib.ServerHandlerOnConnCloseCtx) {
	e.nonces.Delete(ctx.Conn)
	e.in.Host.Telemetry().Client("rtsp", hostOnly(ctx.Conn.NetConn().RemoteAddr().String()), false)
}

// OnSessionOpen implements gortsplib.ServerHandlerOnSessionOpen.
func (e *Engine) OnSessionOpen(*gortsplib.ServerHandlerOnSessionOpenCtx) {
	e.sessions.Add(1)
}

// OnSessionClose implements gortsplib.ServerHandlerOnSessionClose.
func (e *Engine) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	e.sessions.Add(-1)
	st := stateOf(ctx.Session)
	st.mu.Lock()
	if st.playing != nil {
		st.playing.viewers.Add(-1)
		st.playing = nil
	}
	st.mu.Unlock()
}

// OnDescribe implements gortsplib.ServerHandlerOnDescribe.
func (e *Engine) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if res := e.authorize(ctx.Conn, ctx.Request); res != nil {
		return res, nil, nil
	}
	st := e.streamFor(ctx.Path, ctx.Query)
	if st == nil {
		e.in.Host.Telemetry().Gap("rtsp", hostOnly(ctx.Conn.NetConn().RemoteAddr().String()), "DESCRIBE "+ctx.Path)
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, st.stream, nil
}

// OnSetup implements gortsplib.ServerHandlerOnSetup.
func (e *Engine) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if res := e.authorize(ctx.Conn, ctx.Request); res != nil {
		return res, nil, nil
	}
	st := e.streamFor(ctx.Path, ctx.Query)
	if st == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	sess := stateOf(ctx.Session)
	sess.mu.Lock()
	sess.authorized = true
	sess.mu.Unlock()
	return &base.Response{StatusCode: base.StatusOK}, st.stream, nil
}

// OnPlay implements gortsplib.ServerHandlerOnPlay. A session plays only
// if its SETUP was authorized, or with credentials of its own (audit
// B13). Like a camera encoder, the stream restarts at a keyframe for the
// new viewer: H.265 decoders cannot start in the middle of a group of
// pictures. The server adds the session to the stream's readers after
// OnPlay returns, so the stream holds its frames until the response
// (OnResponse): a frame sent in between would reach the viewer before
// its keyframe.
func (e *Engine) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	sess := stateOf(ctx.Session)
	sess.mu.Lock()
	authorized := sess.authorized
	sess.mu.Unlock()
	if !authorized {
		if res := e.authorize(ctx.Conn, ctx.Request); res != nil {
			return res, nil
		}
	}
	st := e.streamFor(ctx.Path, ctx.Query)
	if st != nil {
		sess.mu.Lock()
		if sess.playing != st {
			if sess.playing != nil {
				sess.playing.viewers.Add(-1)
			}
			st.viewers.Add(1)
			sess.playing = st
		}
		sess.mu.Unlock()
		st.hold()
		ctx.Conn.SetUserData(st)
	}
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// OnResponse implements gortsplib.ServerHandlerOnResponse. It follows
// every request of a connection and names the camera's server; after a
// PLAY, the server has added the session to the stream's readers, and the
// stream goes on from its keyframe.
func (e *Engine) OnResponse(sc *gortsplib.ServerConn, res *base.Response) {
	e.mu.RLock()
	if e.serverName != "" {
		res.Header["Server"] = base.HeaderValue{e.serverName}
	}
	e.mu.RUnlock()
	if st, ok := sc.UserData().(*streamer); ok {
		sc.SetUserData(nil)
		st.release()
	}
}

// streamer loops a GOP into a server stream at the stream's frame rate.
type streamer struct {
	engine *Engine
	stream *gortsplib.ServerStream
	src    *engine.VideoSource
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	// mu keeps each frame whole on one side of a hold: a frame goes out
	// before a PLAY holds the stream, or after the stream is released.
	mu sync.Mutex
	// joining counts the PLAY requests being answered; the stream sends
	// nothing until they are.
	joining int
	// restart asks for the loop to start again at its keyframe.
	restart bool
	// viewers counts the sessions playing this stream; a stream nobody
	// plays skips the packetizing work.
	viewers atomic.Int64
}

// hold stops the frames while a session joins the stream's readers.
func (s *streamer) hold() {
	s.mu.Lock()
	s.joining++
	s.mu.Unlock()
}

// release lets the frames go again, from the keyframe.
func (s *streamer) release() {
	s.mu.Lock()
	s.joining--
	s.restart = true
	s.mu.Unlock()
}

func (s *streamer) stop() {
	s.once.Do(func() {
		s.cancel()
		<-s.done
		s.stream.Close()
	})
}

func (s *streamer) run(ctx context.Context) {
	defer close(s.done)
	media := s.stream.Desc.Medias[0]
	encode, err := encoderFor(media.Formats[0], s.src)
	if err != nil {
		s.engine.in.Host.Telemetry().Log(slog.LevelError, "rtsp: encoder", "error", err)
		return
	}
	fps := s.src.Info.FPS
	if fps <= 0 {
		fps = 15
	}
	frame := time.Second / time.Duration(fps)
	var rb [4]byte
	_, _ = rand.Read(rb[:])
	rtpBase := binary.BigEndian.Uint32(rb[:])
	start := time.Now()
	timer := time.NewTimer(0)
	defer timer.Stop()

	var (
		n int64 // frames sent since start; drives continuous timestamps
		i int   // position in the loop
	)
	for {
		due := start.Add(time.Duration(n) * frame)
		if wait := time.Until(due); wait > 0 {
			timer.Reset(wait)
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
		} else if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		if s.joining == 0 {
			if s.restart {
				s.restart, i = false, 0
			}
			// Nobody is watching: keep the clock running, skip the work.
			if s.viewers.Load() > 0 {
				pkts, err := encode(s.src.AccessUnits[i], i == 0)
				if err == nil {
					ts := rtpBase + uint32(n*90000/int64(fps))
					for _, p := range pkts {
						p.Timestamp = ts
						_ = s.stream.WritePacketRTPWithNTP(media, p, due)
					}
				}
			}
		}
		s.mu.Unlock()
		n++
		i = (i + 1) % len(s.src.AccessUnits)
	}
}

// formatFor describes the RTP format of a source.
func formatFor(src *engine.VideoSource) (format.Format, error) {
	switch src.Info.Codec {
	case "h264":
		return &format.H264{PayloadTyp: 96, PacketizationMode: 1, SPS: src.SPS, PPS: src.PPS}, nil
	case "h265":
		return &format.H265{PayloadTyp: 96, VPS: src.VPS, SPS: src.SPS, PPS: src.PPS}, nil
	case "mjpeg":
		return &format.MJPEG{}, nil
	}
	return nil, fmt.Errorf("codec %s is not supported", src.Info.Codec)
}

// frameEncoder turns an access unit into RTP packets; keyframe marks the
// first frame of the loop.
type frameEncoder func(au [][]byte, keyframe bool) ([]*rtp.Packet, error)

// encoderFor creates the RTP packetizer of a format. Keyframes of H.264
// and H.265 carry their parameter sets, so clients joining at any loop can
// decode.
func encoderFor(f format.Format, src *engine.VideoSource) (frameEncoder, error) {
	switch f := f.(type) {
	case *format.H264:
		enc, err := f.CreateEncoder()
		if err != nil {
			return nil, err
		}
		return func(au [][]byte, keyframe bool) ([]*rtp.Packet, error) {
			if keyframe {
				au = withParameterSets(au, func(n []byte) bool { return n[0]&0x1f == 7 }, src.SPS, src.PPS)
			}
			return enc.Encode(au)
		}, nil
	case *format.H265:
		enc, err := f.CreateEncoder()
		if err != nil {
			return nil, err
		}
		return func(au [][]byte, keyframe bool) ([]*rtp.Packet, error) {
			if keyframe {
				au = withParameterSets(au, func(n []byte) bool { return (n[0]>>1)&0x3f == 32 }, src.VPS, src.SPS, src.PPS)
			}
			return enc.Encode(au)
		}, nil
	case *format.MJPEG:
		enc, err := f.CreateEncoder()
		if err != nil {
			return nil, err
		}
		return func(au [][]byte, _ bool) (pkts []*rtp.Packet, err error) {
			if len(au) != 1 {
				return nil, errors.New("an MJPEG frame is one image")
			}
			// The packetizer panics on images RTP cannot carry; the
			// media layer checks them, this guards the camera anyway.
			defer func() {
				if r := recover(); r != nil {
					pkts, err = nil, fmt.Errorf("mjpeg: %v", r)
				}
			}()
			return enc.Encode(au[0])
		}, nil
	}
	return nil, fmt.Errorf("unsupported format %T", f)
}

// withParameterSets puts the parameter sets in front of a keyframe when the
// frame does not carry them already.
func withParameterSets(au [][]byte, isFirstSet func([]byte) bool, sets ...[]byte) [][]byte {
	for _, n := range au {
		if len(n) > 0 && isFirstSet(n) {
			return au
		}
	}
	out := make([][]byte, 0, len(au)+len(sets))
	for _, ps := range sets {
		if ps == nil {
			return au
		}
		out = append(out, ps)
	}
	return append(out, au...)
}

func hostOnly(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
