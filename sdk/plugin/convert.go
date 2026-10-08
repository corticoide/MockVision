package plugin

import (
	"encoding/json"
	"time"

	"github.com/corticoide/mockvision/sdk/engine"
	pb "github.com/corticoide/mockvision/sdk/proto/mockvision/engine/v1"
)

// The conversions both sides of the contract share.

// DescriptorToProto converts a descriptor.
func DescriptorToProto(d engine.Descriptor) *pb.Descriptor {
	out := &pb.Descriptor{Name: d.Name, Version: d.Version, Contract: int32(d.Contract), Role: string(d.Role),
		ConfigSchema: d.ConfigSchema, Emits: d.Emits, Delivers: d.Delivers}
	for _, s := range d.Sockets {
		out.Sockets = append(out.Sockets, &pb.SocketSpec{Name: s.Name, Network: s.Network, DefaultPort: int32(s.DefaultPort)})
	}
	return out
}

// DescriptorFromProto converts a descriptor back.
func DescriptorFromProto(d *pb.Descriptor) engine.Descriptor {
	out := engine.Descriptor{Name: d.GetName(), Version: d.GetVersion(), Contract: int(d.GetContract()), Role: engine.Role(d.GetRole()),
		ConfigSchema: d.GetConfigSchema(), Emits: d.GetEmits(), Delivers: d.GetDelivers()}
	for _, s := range d.GetSockets() {
		out.Sockets = append(out.Sockets, engine.SocketSpec{Name: s.GetName(), Network: s.GetNetwork(), DefaultPort: int(s.GetDefaultPort())})
	}
	return out
}

// IdentityToProto converts a camera identity.
func IdentityToProto(id engine.Identity) *pb.Identity {
	return &pb.Identity{CameraId: id.CameraID, Name: id.Name, Ip: id.IP, Mac: id.MAC, Serial: id.Serial, Vendor: id.Vendor,
		Model: id.Model, Firmware: id.Firmware, ProfileId: id.ProfileID, ProfileVersion: id.ProfileVersion}
}

// IdentityFromProto converts a camera identity back.
func IdentityFromProto(id *pb.Identity) engine.Identity {
	return engine.Identity{CameraID: id.GetCameraId(), Name: id.GetName(), IP: id.GetIp(), MAC: id.GetMac(), Serial: id.GetSerial(),
		Vendor: id.GetVendor(), Model: id.GetModel(), Firmware: id.GetFirmware(), ProfileID: id.GetProfileId(), ProfileVersion: id.GetProfileVersion()}
}

// HealthToProto converts an engine's health.
func HealthToProto(h engine.Health) *pb.HealthResponse {
	return &pb.HealthResponse{State: string(h.State), Detail: h.Detail, Clients: int32(h.Clients), Requests: h.Requests,
		Errors: h.Errors, BytesIn: h.BytesIn, BytesOut: h.BytesOut}
}

// HealthFromProto converts an engine's health back.
func HealthFromProto(h *pb.HealthResponse) engine.Health {
	return engine.Health{State: engine.HealthState(h.GetState()), Detail: h.GetDetail(), Clients: int(h.GetClients()),
		Requests: h.GetRequests(), Errors: h.GetErrors(), BytesIn: h.GetBytesIn(), BytesOut: h.GetBytesOut()}
}

// OriginToProto converts the origin of a change.
func OriginToProto(o engine.Origin) *pb.Origin {
	return &pb.Origin{Kind: o.Kind, Ip: o.IP, Engine: o.Engine}
}

// OriginFromProto converts the origin of a change back.
func OriginFromProto(o *pb.Origin) engine.Origin {
	return engine.Origin{Kind: o.GetKind(), IP: o.GetIp(), Engine: o.GetEngine()}
}

// ChangesToProto converts applied changes.
func ChangesToProto(changes []engine.Change) []*pb.Change {
	out := make([]*pb.Change, 0, len(changes))
	for _, c := range changes {
		raw, _ := json.Marshal(c.Value)
		out = append(out, &pb.Change{Key: c.Key, ValueJson: raw, Bind: c.Bind, Origin: OriginToProto(c.Origin)})
	}
	return out
}

// ChangesFromProto converts applied changes back.
func ChangesFromProto(changes []*pb.Change) []engine.Change {
	out := make([]engine.Change, 0, len(changes))
	for _, c := range changes {
		var v any
		_ = json.Unmarshal(c.GetValueJson(), &v)
		out = append(out, engine.Change{Key: c.GetKey(), Value: v, Bind: c.GetBind(), Origin: OriginFromProto(c.GetOrigin())})
	}
	return out
}

func durationMS(d *time.Duration) *int64 {
	if d == nil {
		return nil
	}
	ms := d.Milliseconds()
	return &ms
}

func msDuration(ms *int64) *time.Duration {
	if ms == nil {
		return nil
	}
	d := time.Duration(*ms) * time.Millisecond
	return &d
}

// TargetToProto converts an event target.
func TargetToProto(t engine.Target) *pb.Target {
	out := &pb.Target{Id: t.ID, Name: t.Name, Type: t.Type, Url: t.URL, Method: t.Method, Headers: t.Headers, Username: t.Username,
		Password: t.Password, Auth: t.Auth, Topic: t.Topic, ClientId: t.ClientID, HostKey: t.HostKey, Tls: t.TLS, Insecure: t.Insecure,
		From: t.From, To: t.To}
	if d := t.Delivery; d != nil {
		out.Delivery = &pb.DeliveryOverride{TimeoutMs: durationMS(d.Timeout), BackoffMs: durationMS(d.Backoff)}
		if d.Retries != nil {
			r := int32(*d.Retries)
			out.Delivery.Retries = &r
		}
	}
	return out
}

// TargetFromProto converts an event target back.
func TargetFromProto(t *pb.Target) engine.Target {
	out := engine.Target{ID: t.GetId(), Name: t.GetName(), Type: t.GetType(), URL: t.GetUrl(), Method: t.GetMethod(), Headers: t.GetHeaders(),
		Username: t.GetUsername(), Password: t.GetPassword(), Auth: t.GetAuth(), Topic: t.GetTopic(), ClientID: t.GetClientId(),
		HostKey: t.GetHostKey(), TLS: t.GetTls(), Insecure: t.GetInsecure(), From: t.GetFrom(), To: t.GetTo()}
	if d := t.GetDelivery(); d != nil {
		out.Delivery = &engine.DeliveryOverride{Timeout: msDuration(d.TimeoutMs), Backoff: msDuration(d.BackoffMs)}
		if d.Retries != nil {
			r := int(*d.Retries)
			out.Delivery.Retries = &r
		}
	}
	return out
}

// DispatchToProto converts an event handed to a delivering engine.
func DispatchToProto(d engine.Dispatch) *pb.Dispatch {
	ev, _ := json.Marshal(d.Event)
	out := &pb.Dispatch{EventJson: ev, VendorName: d.VendorName, TransportJson: d.Transport, TimeoutMs: d.Policy.Timeout.Milliseconds(),
		Retries: int32(d.Policy.Retries), BackoffMs: d.Policy.Backoff.Milliseconds()}
	for _, t := range d.Targets {
		out.Targets = append(out.Targets, TargetToProto(t))
	}
	return out
}

// DispatchFromProto converts a dispatch back.
func DispatchFromProto(d *pb.Dispatch) engine.Dispatch {
	var ev engine.Event
	_ = json.Unmarshal(d.GetEventJson(), &ev)
	out := engine.Dispatch{Event: ev, VendorName: d.GetVendorName(), Transport: d.GetTransportJson(),
		Policy: engine.DeliveryPolicy{Timeout: time.Duration(d.GetTimeoutMs()) * time.Millisecond, Retries: int(d.GetRetries()),
			Backoff: time.Duration(d.GetBackoffMs()) * time.Millisecond}}
	for _, t := range d.GetTargets() {
		out.Targets = append(out.Targets, TargetFromProto(t))
	}
	return out
}

// FileInfoToProto converts a recording's description.
func FileInfoToProto(f engine.FileInfo) *pb.FileInfo {
	return &pb.FileInfo{Name: f.Name, Kind: f.Kind, Event: f.Event, Stream: f.Stream, Size: f.Size,
		StartUnixMs: f.Start.UnixMilli(), EndUnixMs: f.End.UnixMilli()}
}

// FileInfoFromProto converts a recording's description back.
func FileInfoFromProto(f *pb.FileInfo) engine.FileInfo {
	return engine.FileInfo{Name: f.GetName(), Kind: f.GetKind(), Event: f.GetEvent(), Stream: f.GetStream(), Size: f.GetSize(),
		Start: time.UnixMilli(f.GetStartUnixMs()), End: time.UnixMilli(f.GetEndUnixMs())}
}

// StorageToProto converts a storage status.
func StorageToProto(s engine.StorageStatus) *pb.StorageStatus {
	return &pb.StorageStatus{Kind: s.Kind, State: s.State, CapacityBytes: s.CapacityBytes, UsedBytes: s.UsedBytes,
		Files: int32(s.Files), Overwrite: s.Overwrite}
}

// StorageFromProto converts a storage status back.
func StorageFromProto(s *pb.StorageStatus) engine.StorageStatus {
	return engine.StorageStatus{Kind: s.GetKind(), State: s.GetState(), CapacityBytes: s.GetCapacityBytes(), UsedBytes: s.GetUsedBytes(),
		Files: int(s.GetFiles()), Overwrite: s.GetOverwrite()}
}

// StreamToProto converts a stream's description.
func StreamToProto(s engine.StreamInfo) *pb.StreamInfo {
	return &pb.StreamInfo{Name: s.Name, Codec: s.Codec, Width: int32(s.Width), Height: int32(s.Height), Fps: int32(s.FPS),
		Gop: int32(s.GOP), Bitrate: int32(s.Bitrate)}
}

// StreamFromProto converts a stream's description back.
func StreamFromProto(s *pb.StreamInfo) engine.StreamInfo {
	return engine.StreamInfo{Name: s.GetName(), Codec: s.GetCodec(), Width: int(s.GetWidth()), Height: int(s.GetHeight()),
		FPS: int(s.GetFps()), GOP: int(s.GetGop()), Bitrate: int(s.GetBitrate())}
}
