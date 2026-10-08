package camera

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/corticoide/mockvision/sdk/engine"
	"github.com/corticoide/mockvision/sdk/plugin"
	pb "github.com/corticoide/mockvision/sdk/proto/mockvision/engine/v1"
)

// pluginHost serves the Host service to one plugin process: the camera's
// services, limited to the permissions the user approved for the plugin.
type pluginHost struct {
	pb.UnimplementedHostServer
	host     engine.Host
	instance string
	allowed  map[string]bool
	log      *slog.Logger
	// templates caches what the plugin renders, by its text.
	mu        sync.Mutex
	templates map[string]engine.Template
}

func newPluginHost(host engine.Host, instance string, permissions []string, log *slog.Logger) *pluginHost {
	allowed := map[string]bool{}
	for _, p := range permissions {
		allowed[p] = true
	}
	return &pluginHost{host: host, instance: instance, allowed: allowed, log: log, templates: map[string]engine.Template{}}
}

// need refuses a call the plugin has no permission for.
func (h *pluginHost) need(perm string) error {
	if h.allowed[perm] {
		return nil
	}
	return status.Errorf(codes.PermissionDenied, "the plugin was not granted %s", perm)
}

func (h *pluginHost) AccountsList(context.Context, *pb.AccountsListRequest) (*pb.AccountsListResponse, error) {
	if err := h.need(plugin.PermAccountsRead); err != nil {
		return nil, err
	}
	out := &pb.AccountsListResponse{}
	for _, u := range h.host.Accounts().List() {
		out.Users = append(out.Users, &pb.User{Username: u.Username, Password: u.Password, Role: u.Role})
	}
	return out, nil
}

func (h *pluginHost) StateGet(_ context.Context, r *pb.StateGetRequest) (*pb.StateGetResponse, error) {
	if err := h.need(plugin.PermStateRead); err != nil {
		return nil, err
	}
	v, ok := h.host.State().Get(r.GetKey())
	return valueResponse(v, ok), nil
}

func (h *pluginHost) StateCanon(_ context.Context, r *pb.StateGetRequest) (*pb.StateGetResponse, error) {
	if err := h.need(plugin.PermStateRead); err != nil {
		return nil, err
	}
	v, ok := h.host.State().Canon(r.GetKey())
	return valueResponse(v, ok), nil
}

func valueResponse(v any, ok bool) *pb.StateGetResponse {
	if !ok {
		return &pb.StateGetResponse{}
	}
	raw, _ := json.Marshal(v)
	return &pb.StateGetResponse{Found: true, ValueJson: raw}
}

func (h *pluginHost) StateList(_ context.Context, r *pb.StateListRequest) (*pb.StateListResponse, error) {
	if err := h.need(plugin.PermStateRead); err != nil {
		return nil, err
	}
	out := &pb.StateListResponse{}
	for _, p := range h.host.State().List(r.GetPrefix()) {
		raw, _ := json.Marshal(p.Value)
		out.Params = append(out.Params, &pb.Param{Key: p.Key, Type: p.Type, ValueJson: raw, Bind: p.Bind})
	}
	return out, nil
}

func (h *pluginHost) StateSet(ctx context.Context, r *pb.StateSetRequest) (*pb.StateSetResponse, error) {
	if err := h.need(plugin.PermStateWrite); err != nil {
		return nil, err
	}
	values := map[string]any{}
	for k, raw := range r.GetValuesJson() {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return &pb.StateSetResponse{Problems: map[string]string{k: "not JSON"}}, nil
		}
		values[k] = v
	}
	origin := plugin.OriginFromProto(r.GetOrigin())
	origin.Engine = h.instance // a plugin speaks for itself only
	if _, err := h.host.State().Set(ctx, values, origin); err != nil {
		var se *engine.StateError
		if errors.As(err, &se) {
			return &pb.StateSetResponse{Problems: se.Problems}, nil
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pb.StateSetResponse{}, nil
}

func (h *pluginHost) StateWatch(_ *pb.StateWatchRequest, stream pb.Host_StateWatchServer) error {
	if err := h.need(plugin.PermStateRead); err != nil {
		return err
	}
	ch := make(chan []engine.Change, 64)
	cancel := h.host.State().Watch(func(c []engine.Change) {
		select {
		case ch <- c:
		default:
			h.log.Warn("a plugin reads changes too slowly; dropped some", "instance", h.instance)
		}
	})
	defer cancel()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case c := <-ch:
			if err := stream.Send(&pb.StateChanges{Changes: plugin.ChangesToProto(c)}); err != nil {
				return err
			}
		}
	}
}

func (h *pluginHost) EventEmit(ctx context.Context, r *pb.EventEmitRequest) (*pb.EventEmitResponse, error) {
	if err := h.need(plugin.PermEventsEmit); err != nil {
		return nil, err
	}
	var ev engine.Event
	if err := json.Unmarshal(r.GetEventJson(), &ev); err != nil {
		return nil, status.Error(codes.InvalidArgument, "the event is not JSON")
	}
	out, err := h.host.Events().Emit(ctx, ev)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &pb.EventEmitResponse{EventId: out.ID}, nil
}

func (h *pluginHost) EventSubscribe(r *pb.EventSubscribeRequest, stream pb.Host_EventSubscribeServer) error {
	if err := h.need(plugin.PermEventsDeliver); err != nil {
		return err
	}
	ch := make(chan engine.Dispatch, 64)
	cancel := h.host.Events().Subscribe(r.GetTransport(), func(d engine.Dispatch) {
		select {
		case ch <- d:
		default:
			h.log.Warn("a plugin takes events too slowly; dropped one", "instance", h.instance, "event", d.Event.ID)
		}
	})
	defer cancel()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case d := <-ch:
			if err := stream.Send(plugin.DispatchToProto(d)); err != nil {
				return err
			}
		}
	}
}

func (h *pluginHost) DeliveryReport(_ context.Context, r *pb.DeliveryReportRequest) (*pb.DeliveryReportResponse, error) {
	if err := h.need(plugin.PermEventsDeliver); err != nil {
		return nil, err
	}
	h.host.Events().Report(engine.DeliveryReport{EventID: r.GetEventId(), TargetID: r.GetTargetId(), Attempt: int(r.GetAttempt()),
		At: time.UnixMilli(r.GetAtMs()), Status: r.GetStatus(), HTTPStatus: int(r.GetHttpStatus()), LatencyMS: r.GetLatencyMs(), Error: r.GetError()})
	return &pb.DeliveryReportResponse{}, nil
}

func (h *pluginHost) EventTargets(_ context.Context, r *pb.EventSubscribeRequest) (*pb.EventTargetsResponse, error) {
	if err := h.need(plugin.PermEventsDeliver); err != nil {
		return nil, err
	}
	out := &pb.EventTargetsResponse{}
	for _, t := range h.host.Events().Targets(r.GetTransport()) {
		out.Targets = append(out.Targets, plugin.TargetToProto(t))
	}
	return out, nil
}

func (h *pluginHost) MediaStreams(context.Context, *pb.MediaStreamsRequest) (*pb.MediaStreamsResponse, error) {
	if err := h.need(plugin.PermMediaRead); err != nil {
		return nil, err
	}
	out := &pb.MediaStreamsResponse{}
	for _, s := range h.host.Media().Streams() {
		out.Streams = append(out.Streams, plugin.StreamToProto(s))
	}
	return out, nil
}

func (h *pluginHost) MediaSnapshot(_ context.Context, r *pb.MediaSnapshotRequest) (*pb.MediaSnapshotResponse, error) {
	if err := h.need(plugin.PermMediaRead); err != nil {
		return nil, err
	}
	jpeg, err := h.host.Media().Snapshot(r.GetStream())
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &pb.MediaSnapshotResponse{Jpeg: jpeg}, nil
}

// maxPluginTemplates bounds the templates a plugin keeps compiled.
const maxPluginTemplates = 256

func (h *pluginHost) TemplateRender(ctx context.Context, r *pb.TemplateRenderRequest) (*pb.TemplateRenderResponse, error) {
	h.mu.Lock()
	t, ok := h.templates[r.GetText()]
	h.mu.Unlock()
	if !ok {
		var err error
		if t, err = h.host.Templates().Compile(r.GetName(), r.GetText(), int(r.GetMaxBytes())); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		h.mu.Lock()
		if len(h.templates) >= maxPluginTemplates {
			h.templates = map[string]engine.Template{}
		}
		h.templates[r.GetText()] = t
		h.mu.Unlock()
	}
	var data struct {
		Request   *engine.RequestData `json:"Request"`
		Event     *engine.Event       `json:"Event"`
		EventName string              `json:"EventName"`
		Result    any                 `json:"Result"`
	}
	if raw := r.GetDataJson(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, status.Error(codes.InvalidArgument, "the template data is not JSON")
		}
	}
	out, err := t.Render(ctx, engine.TemplateData{Request: data.Request, Event: data.Event, EventName: data.EventName, Result: data.Result})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &pb.TemplateRenderResponse{Output: out}, nil
}

func (h *pluginHost) files() (engine.Files, error) {
	if err := h.need(plugin.PermSD); err != nil {
		return nil, err
	}
	f := h.host.Files()
	if f == nil {
		return nil, status.Error(codes.Unavailable, "the camera records nothing")
	}
	return f, nil
}

func (h *pluginHost) FileStatus(context.Context, *pb.FileStatusRequest) (*pb.StorageStatus, error) {
	f, err := h.files()
	if err != nil {
		return nil, err
	}
	return plugin.StorageToProto(f.Status()), nil
}

func (h *pluginHost) FileFind(ctx context.Context, r *pb.FileFindRequest) (*pb.FileFindResponse, error) {
	f, err := h.files()
	if err != nil {
		return nil, err
	}
	list, err := f.Find(ctx, engine.FileQuery{From: time.UnixMilli(r.GetFromUnixMs()), To: time.UnixMilli(r.GetToUnixMs()),
		Kind: r.GetKind(), Event: r.GetEvent(), Limit: int(r.GetLimit())})
	if err != nil {
		return nil, fileStatus(err)
	}
	out := &pb.FileFindResponse{}
	for _, fi := range list {
		out.Files = append(out.Files, plugin.FileInfoToProto(fi))
	}
	return out, nil
}

func (h *pluginHost) FileGet(r *pb.FileGetRequest, stream pb.Host_FileGetServer) error {
	f, err := h.files()
	if err != nil {
		return err
	}
	rc, info, err := f.Open(stream.Context(), r.GetName())
	if err != nil {
		return fileStatus(err)
	}
	defer rc.Close()
	buf := make([]byte, 256<<10)
	first := true
	for {
		n, err := rc.Read(buf)
		if n > 0 || first {
			msg := &pb.FileChunk{Data: append([]byte(nil), buf[:n]...)}
			if first {
				msg.Info, first = plugin.FileInfoToProto(info), false
			}
			if serr := stream.Send(msg); serr != nil {
				return serr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
	}
}

func fileStatus(err error) error {
	switch {
	case errors.Is(err, engine.ErrNoFile):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, engine.ErrStorageUnavailable):
		return status.Error(codes.Unavailable, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

func (h *pluginHost) FaultStatus(_ context.Context, r *pb.FaultStatusRequest) (*pb.FaultStatusResponse, error) {
	f := h.host.Faults()
	if f == nil || r.GetInstance() != h.instance {
		return &pb.FaultStatusResponse{}, nil
	}
	return &pb.FaultStatusResponse{Status: int32(f.Status(h.instance))}, nil
}

func (h *pluginHost) Log(_ context.Context, r *pb.LogRequest) (*pb.LogResponse, error) {
	var level slog.Level
	_ = level.UnmarshalText([]byte(strings.ToUpper(r.GetLevel())))
	attrs := []any{"plugin", h.instance}
	for k, v := range r.GetAttrs() {
		attrs = append(attrs, k, v)
	}
	h.host.Telemetry().Log(level, truncateText(r.GetMessage(), 1024), attrs...)
	return &pb.LogResponse{}, nil
}

func (h *pluginHost) RequestStat(_ context.Context, r *pb.RequestStatRequest) (*pb.RequestStatResponse, error) {
	h.host.Telemetry().Request(r.GetRoute(), r.GetClientIp(), int(r.GetStatus()), time.Duration(r.GetDurationUs())*time.Microsecond)
	return &pb.RequestStatResponse{}, nil
}

func (h *pluginHost) Gap(_ context.Context, r *pb.GapRequest) (*pb.GapResponse, error) {
	h.host.Telemetry().Gap(r.GetProtocol(), r.GetClientIp(), truncateText(r.GetSummary(), 512))
	return &pb.GapResponse{}, nil
}

func (h *pluginHost) Client(_ context.Context, r *pb.ClientRequest) (*pb.ClientResponse, error) {
	h.host.Telemetry().Client(r.GetProtocol(), r.GetClientIp(), r.GetConnected())
	return &pb.ClientResponse{}, nil
}

func truncateText(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
