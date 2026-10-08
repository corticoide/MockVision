package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/corticoide/mockvision/sdk/engine"
	pb "github.com/corticoide/mockvision/sdk/proto/mockvision/engine/v1"
)

// callTimeout bounds a plugin's call to its camera.
const callTimeout = 10 * time.Second

// remoteHost is the camera, as a plugin reaches it: the same engine.Host
// the built-in engines get, over gRPC.
type remoteHost struct {
	c   pb.HostClient
	ctx context.Context
}

// NewHost wraps the Host service a plugin reaches; ctx ends its streams.
func NewHost(ctx context.Context, c pb.HostClient) engine.Host {
	return &remoteHost{c: c, ctx: ctx}
}

func (h *remoteHost) call() (context.Context, context.CancelFunc) {
	return context.WithTimeout(h.ctx, callTimeout)
}

func (h *remoteHost) Accounts() engine.Accounts   { return remoteAccounts{h} }
func (h *remoteHost) State() engine.State         { return remoteState{h} }
func (h *remoteHost) Events() engine.Events       { return remoteEvents{h} }
func (h *remoteHost) Media() engine.Media         { return remoteMedia{h} }
func (h *remoteHost) Templates() engine.Templates { return remoteTemplates{h} }
func (h *remoteHost) Files() engine.Files         { return remoteFiles{h} }
func (h *remoteHost) Telemetry() engine.Telemetry { return remoteTelemetry{h} }
func (h *remoteHost) Faults() engine.Faults       { return remoteFaults{h} }

// ErrNotForPlugins is what the host gives a plugin for what it does not
// offer plugins, such as a stream's frames.
var ErrNotForPlugins = errors.New("plugin: not available to plugins")

type remoteAccounts struct{ h *remoteHost }

func (a remoteAccounts) List() []engine.User {
	ctx, cancel := a.h.call()
	defer cancel()
	res, err := a.h.c.AccountsList(ctx, &pb.AccountsListRequest{})
	if err != nil {
		return nil
	}
	out := make([]engine.User, 0, len(res.GetUsers()))
	for _, u := range res.GetUsers() {
		out = append(out, engine.User{Username: u.GetUsername(), Password: u.GetPassword(), Role: u.GetRole()})
	}
	return out
}

func (a remoteAccounts) Lookup(username string) (engine.User, bool) {
	for _, u := range a.List() {
		if u.Username == username {
			return u, true
		}
	}
	return engine.User{}, false
}

type remoteState struct{ h *remoteHost }

func (s remoteState) Get(key string) (any, bool) {
	ctx, cancel := s.h.call()
	defer cancel()
	res, err := s.h.c.StateGet(ctx, &pb.StateGetRequest{Key: key})
	if err != nil || !res.GetFound() {
		return nil, false
	}
	var v any
	_ = json.Unmarshal(res.GetValueJson(), &v)
	return v, true
}

func (s remoteState) Canon(key string) (any, bool) {
	ctx, cancel := s.h.call()
	defer cancel()
	res, err := s.h.c.StateCanon(ctx, &pb.StateGetRequest{Key: key})
	if err != nil || !res.GetFound() {
		return nil, false
	}
	var v any
	_ = json.Unmarshal(res.GetValueJson(), &v)
	return v, true
}

func (s remoteState) List(prefix string) []engine.Param {
	ctx, cancel := s.h.call()
	defer cancel()
	res, err := s.h.c.StateList(ctx, &pb.StateListRequest{Prefix: prefix})
	if err != nil {
		return nil
	}
	out := make([]engine.Param, 0, len(res.GetParams()))
	for _, p := range res.GetParams() {
		var v any
		_ = json.Unmarshal(p.GetValueJson(), &v)
		out = append(out, engine.Param{Key: p.GetKey(), Type: p.GetType(), Value: v, Bind: p.GetBind()})
	}
	return out
}

func (s remoteState) Set(ctx context.Context, values map[string]any, origin engine.Origin) ([]engine.Change, error) {
	req := &pb.StateSetRequest{ValuesJson: map[string][]byte{}, Origin: OriginToProto(origin)}
	for k, v := range values {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		req.ValuesJson[k] = raw
	}
	res, err := s.h.c.StateSet(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(res.GetProblems()) > 0 {
		return nil, &engine.StateError{Problems: res.GetProblems()}
	}
	// The changes applied come to watchers; a set reports the values.
	out := make([]engine.Change, 0, len(values))
	for k, v := range values {
		out = append(out, engine.Change{Key: k, Value: v, Origin: origin})
	}
	return out, nil
}

func (s remoteState) Watch(fn func([]engine.Change)) func() {
	ctx, cancel := context.WithCancel(s.h.ctx)
	go func() {
		stream, err := s.h.c.StateWatch(ctx, &pb.StateWatchRequest{})
		if err != nil {
			return
		}
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			fn(ChangesFromProto(msg.GetChanges()))
		}
	}()
	return cancel
}

type remoteEvents struct{ h *remoteHost }

func (e remoteEvents) Emit(ctx context.Context, ev engine.Event) (engine.Event, error) {
	raw, err := json.Marshal(ev)
	if err != nil {
		return ev, err
	}
	res, err := e.h.c.EventEmit(ctx, &pb.EventEmitRequest{EventJson: raw})
	if err != nil {
		return ev, err
	}
	ev.ID = res.GetEventId()
	return ev, nil
}

func (e remoteEvents) Subscribe(transport string, fn func(engine.Dispatch)) func() {
	ctx, cancel := context.WithCancel(e.h.ctx)
	ready := make(chan struct{})
	go func() {
		stream, err := e.h.c.EventSubscribe(ctx, &pb.EventSubscribeRequest{Transport: transport})
		close(ready)
		if err != nil {
			return
		}
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			fn(DispatchFromProto(msg))
		}
	}()
	<-ready
	return cancel
}

func (e remoteEvents) Report(r engine.DeliveryReport) {
	ctx, cancel := e.h.call()
	defer cancel()
	_, _ = e.h.c.DeliveryReport(ctx, &pb.DeliveryReportRequest{EventId: r.EventID, TargetId: r.TargetID, Attempt: int32(r.Attempt),
		AtMs: r.At.UnixMilli(), Status: r.Status, HttpStatus: int32(r.HTTPStatus), LatencyMs: r.LatencyMS, Error: r.Error})
}

func (e remoteEvents) Targets(transport string) []engine.Target {
	ctx, cancel := e.h.call()
	defer cancel()
	res, err := e.h.c.EventTargets(ctx, &pb.EventSubscribeRequest{Transport: transport})
	if err != nil {
		return nil
	}
	out := make([]engine.Target, 0, len(res.GetTargets()))
	for _, t := range res.GetTargets() {
		out = append(out, TargetFromProto(t))
	}
	return out
}

type remoteMedia struct{ h *remoteHost }

func (m remoteMedia) Streams() []engine.StreamInfo {
	ctx, cancel := m.h.call()
	defer cancel()
	res, err := m.h.c.MediaStreams(ctx, &pb.MediaStreamsRequest{})
	if err != nil {
		return nil
	}
	out := make([]engine.StreamInfo, 0, len(res.GetStreams()))
	for _, s := range res.GetStreams() {
		out = append(out, StreamFromProto(s))
	}
	return out
}

func (m remoteMedia) Snapshot(stream string) ([]byte, error) {
	ctx, cancel := m.h.call()
	defer cancel()
	res, err := m.h.c.MediaSnapshot(ctx, &pb.MediaSnapshotRequest{Stream: stream})
	if err != nil {
		return nil, err
	}
	return res.GetJpeg(), nil
}

func (remoteMedia) Source(string) (*engine.VideoSource, error) { return nil, ErrNotForPlugins }
func (remoteMedia) Watch(func(string)) func()                  { return func() {} }

type remoteTemplates struct{ h *remoteHost }

func (t remoteTemplates) Compile(name, text string, maxBytes int) (engine.Template, error) {
	return &remoteTemplate{h: t.h, name: name, text: text, max: maxBytes}, nil
}

// remoteTemplate renders in the camera, which compiles it once.
type remoteTemplate struct {
	h    *remoteHost
	name string
	text string
	max  int
}

func (t *remoteTemplate) Render(ctx context.Context, data engine.TemplateData) ([]byte, error) {
	raw, err := json.Marshal(struct {
		Request   *engine.RequestData `json:"Request,omitempty"`
		Event     *engine.Event       `json:"Event,omitempty"`
		EventName string              `json:"EventName,omitempty"`
		Result    any                 `json:"Result,omitempty"`
	}{data.Request, data.Event, data.EventName, data.Result})
	if err != nil {
		return nil, err
	}
	res, err := t.h.c.TemplateRender(ctx, &pb.TemplateRenderRequest{Name: t.name, Text: t.text, MaxBytes: int32(t.max), DataJson: raw})
	if err != nil {
		return nil, errors.New(status.Convert(err).Message())
	}
	return res.GetOutput(), nil
}

type remoteFiles struct{ h *remoteHost }

func (f remoteFiles) Status() engine.StorageStatus {
	ctx, cancel := f.h.call()
	defer cancel()
	res, err := f.h.c.FileStatus(ctx, &pb.FileStatusRequest{})
	if err != nil {
		return engine.StorageStatus{State: engine.StorageError}
	}
	return StorageFromProto(res)
}

func (f remoteFiles) Find(ctx context.Context, q engine.FileQuery) ([]engine.FileInfo, error) {
	res, err := f.h.c.FileFind(ctx, &pb.FileFindRequest{FromUnixMs: q.From.UnixMilli(), ToUnixMs: q.To.UnixMilli(), Kind: q.Kind,
		Event: q.Event, Limit: int32(q.Limit)})
	if err != nil {
		return nil, fileError(err)
	}
	out := make([]engine.FileInfo, 0, len(res.GetFiles()))
	for _, fi := range res.GetFiles() {
		out = append(out, FileInfoFromProto(fi))
	}
	return out, nil
}

func (f remoteFiles) Open(ctx context.Context, name string) (io.ReadCloser, engine.FileInfo, error) {
	ctx, cancel := context.WithCancel(ctx)
	stream, err := f.h.c.FileGet(ctx, &pb.FileGetRequest{Name: name})
	if err != nil {
		cancel()
		return nil, engine.FileInfo{}, fileError(err)
	}
	first, err := stream.Recv()
	if err != nil {
		cancel()
		return nil, engine.FileInfo{}, fileError(err)
	}
	r := &chunkReader{stream: stream, buf: first.GetData(), cancel: cancel}
	return r, FileInfoFromProto(first.GetInfo()), nil
}

func fileError(err error) error {
	switch status.Code(err) {
	case codes.NotFound:
		return engine.ErrNoFile
	case codes.Unavailable:
		return engine.ErrStorageUnavailable
	}
	return err
}

// chunkReader reads a file streamed in chunks.
type chunkReader struct {
	mu     sync.Mutex
	stream interface{ Recv() (*pb.FileChunk, error) }
	buf    []byte
	cancel context.CancelFunc
	done   bool
}

func (r *chunkReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.buf) == 0 {
		if r.done {
			return 0, io.EOF
		}
		msg, err := r.stream.Recv()
		if err == io.EOF {
			r.done = true
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		r.buf = msg.GetData()
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *chunkReader) Close() error {
	r.cancel()
	return nil
}

type remoteTelemetry struct{ h *remoteHost }

func (t remoteTelemetry) Log(level slog.Level, msg string, attrs ...any) {
	ctx, cancel := t.h.call()
	defer cancel()
	m := map[string]string{}
	for i := 0; i+1 < len(attrs); i += 2 {
		m[fmt.Sprint(attrs[i])] = fmt.Sprint(attrs[i+1])
	}
	_, _ = t.h.c.Log(ctx, &pb.LogRequest{Level: level.String(), Message: msg, Attrs: m})
}

func (t remoteTelemetry) Request(route, clientIP string, st int, dur time.Duration) {
	ctx, cancel := t.h.call()
	defer cancel()
	_, _ = t.h.c.RequestStat(ctx, &pb.RequestStatRequest{Route: route, ClientIp: clientIP, Status: int32(st), DurationUs: dur.Microseconds()})
}

func (t remoteTelemetry) Gap(protocol, clientIP, summary string) {
	ctx, cancel := t.h.call()
	defer cancel()
	_, _ = t.h.c.Gap(ctx, &pb.GapRequest{Protocol: protocol, ClientIp: clientIP, Summary: summary})
}

func (t remoteTelemetry) Client(protocol, clientIP string, connected bool) {
	ctx, cancel := t.h.call()
	defer cancel()
	_, _ = t.h.c.Client(ctx, &pb.ClientRequest{Protocol: protocol, ClientIp: clientIP, Connected: connected})
}

type remoteFaults struct{ h *remoteHost }

func (f remoteFaults) Status(instance string) int {
	ctx, cancel := f.h.call()
	defer cancel()
	res, err := f.h.c.FaultStatus(ctx, &pb.FaultStatusRequest{Instance: instance})
	if err != nil {
		return 0
	}
	return int(res.GetStatus())
}
