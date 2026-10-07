package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"time"
)

// Host is the set of services the camera offers to its engines. Plugins
// reach the same services over gRPC, limited by the permissions the user
// approved.
type Host interface {
	// Accounts are the camera's users. They can change while the camera
	// runs, so engines look them up on every request.
	Accounts() Accounts
	State() State
	Events() Events
	Media() Media
	Templates() Templates
	// Files are the camera's recordings, on its simulated SD card or its
	// NAS share; nil when the camera records nothing.
	Files() Files
	Telemetry() Telemetry
	// Faults are the failures injected into the camera (D43); nil when the
	// host injects none.
	Faults() Faults
}

// Faults are the failures injected into a camera. Engines that answer
// requests ask them on every request, so a fault applies at once.
type Faults interface {
	// Status is the status every request to an engine instance gets while
	// a fault is on, such as 401 or 500; 0 when none. A 401 comes with the
	// engine's usual challenge, as a device that refuses the credentials.
	Status(instance string) int
}

// Accounts are the camera's user accounts (D11): at least one
// administrator, each with a role.
type Accounts interface {
	// List returns every account, sorted by username.
	List() []User
	// Lookup returns the account with that username.
	Lookup(username string) (User, bool)
}

// Origin says who changed a parameter; the audit log keeps it (RN-08).
type Origin struct {
	Kind   string `json:"kind"` // client, panel or system
	IP     string `json:"ip,omitempty"`
	Engine string `json:"engine,omitempty"`
}

// Origins of state changes.
const (
	OriginClient = "client"
	OriginPanel  = "panel"
	OriginSystem = "system"
)

// Param is a native parameter of the camera.
type Param struct {
	Key   string `json:"key"`
	Type  string `json:"type"`
	Value any    `json:"value"`
	// Bind is the canonical key the parameter is bound to; empty when the
	// parameter is declarative (stored and returned, no effect).
	Bind string `json:"bind,omitempty"`
}

// Change is an applied parameter change.
type Change struct {
	Key    string `json:"key"`
	Value  any    `json:"value"`
	Bind   string `json:"bind,omitempty"`
	Origin Origin `json:"origin"`
}

// StateError reports rejected values, keyed by parameter.
type StateError struct {
	Problems map[string]string
}

func (e *StateError) Error() string {
	for k, v := range e.Problems {
		return k + ": " + v
	}
	return "invalid state change"
}

// State reads, writes and observes parameters, with validation and audit.
type State interface {
	Get(key string) (any, bool)
	// List returns parameters whose key starts with prefix, sorted by key.
	List(prefix string) []Param
	// Set validates and applies all values or none of them.
	Set(ctx context.Context, values map[string]any, origin Origin) ([]Change, error)
	// Canon returns the value behind a canonical key such as
	// media.main.resolution.
	Canon(key string) (any, bool)
	// Watch calls fn after every applied change set.
	Watch(fn func([]Change)) (cancel func())
}

// Target types: the kind of receiver, which decides the transports that
// reach it.
const (
	TargetHTTP = "http"
	TargetMQTT = "mqtt"
	TargetFTP  = "ftp"
	TargetSFTP = "sftp"
	TargetSMTP = "smtp"
)

// Transports of events: the sections of a profile event, each delivered by
// an engine to the targets of its types.
const (
	TransportHTTPPush = "http_push"
	TransportMQTT     = "mqtt"
	TransportFTP      = "ftp"
	TransportSMTP     = "smtp"
	// TransportAttach streams events to the clients that keep a request
	// open to the camera, as Dahua's eventManager.cgi attach: it reaches
	// no target, whoever listens gets them.
	TransportAttach = "attach"
)

// TransportTargets lists the target types each transport reaches: an FTP
// upload goes to FTP and SFTP servers alike.
var TransportTargets = map[string][]string{
	TransportHTTPPush: {TargetHTTP},
	TransportMQTT:     {TargetMQTT},
	TransportFTP:      {TargetFTP, TargetSFTP},
	TransportSMTP:     {TargetSMTP},
}

// Target is an event receiver as seen by a delivering engine. Which fields
// apply depends on its type.
type Target struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	URL      string            `json:"url"`
	Method   string            `json:"method,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Username string            `json:"username,omitempty"`
	Password string            `json:"password,omitempty"`
	// Auth is how the camera authenticates to an http target: basic, or
	// digest, answering the target's challenge.
	Auth string `json:"auth,omitempty"`
	// Topic and ClientID replace the profile's for an mqtt target. Both
	// are templates, so one broker serves many cameras.
	Topic    string `json:"topic,omitempty"`
	ClientID string `json:"client_id,omitempty"`
	// HostKey pins the key of an sftp server by its SHA256 fingerprint;
	// empty accepts any key, as most cameras do.
	HostKey string `json:"host_key,omitempty"`
	// TLS is how an smtp target is reached: none, starttls or tls.
	TLS string `json:"tls,omitempty"`
	// Insecure accepts a TLS certificate that does not verify (mqtts and
	// smtp), as a test server's self-signed one.
	Insecure bool `json:"insecure,omitempty"`
	// From and To are the sender and recipients of an smtp target.
	From string   `json:"from,omitempty"`
	To   []string `json:"to,omitempty"`
	// Delivery replaces parts of the profile's delivery policy for this
	// target (D42).
	Delivery *DeliveryOverride `json:"delivery,omitempty"`
}

// DeliveryPolicy mimics how the real device retries (D42).
type DeliveryPolicy struct {
	Timeout time.Duration `json:"timeout"`
	Retries int           `json:"retries"`
	Backoff time.Duration `json:"backoff"`
}

// DeliveryOverride is a target's own delivery policy; nil fields keep the
// profile's.
type DeliveryOverride struct {
	Timeout *time.Duration `json:"timeout,omitempty"`
	Retries *int           `json:"retries,omitempty"`
	Backoff *time.Duration `json:"backoff,omitempty"`
}

// With returns the policy with a target's override applied.
func (p DeliveryPolicy) With(o *DeliveryOverride) DeliveryPolicy {
	if o == nil {
		return p
	}
	if o.Timeout != nil {
		p.Timeout = *o.Timeout
	}
	if o.Retries != nil {
		p.Retries = *o.Retries
	}
	if o.Backoff != nil {
		p.Backoff = *o.Backoff
	}
	return p
}

// Dispatch is an event handed to a delivering engine.
type Dispatch struct {
	Event Event
	// VendorName is how the profile calls the event type.
	VendorName string
	// Transport is the profile's section for the transport.
	Transport json.RawMessage
	// Policy is the profile's; each target may override parts of it.
	Policy  DeliveryPolicy
	Targets []Target
}

// PolicyFor is the delivery policy toward one of the dispatch's targets.
func (d Dispatch) PolicyFor(t Target) DeliveryPolicy { return d.Policy.With(t.Delivery) }

// Delivery statuses reported for each attempt.
const (
	DeliveryOK     = "ok"
	DeliveryRetry  = "retry"
	DeliveryFailed = "failed"
	// DeliverySkipped is an event the device chose not to send, such as a
	// mail within its interval.
	DeliverySkipped = "skipped"
)

// DeliveryReport is the outcome of one delivery attempt.
type DeliveryReport struct {
	EventID    string    `json:"event_id"`
	TargetID   string    `json:"target_id"`
	Attempt    int       `json:"attempt"`
	At         time.Time `json:"at"`
	Status     string    `json:"status"`
	HTTPStatus int       `json:"http_status,omitempty"`
	LatencyMS  int64     `json:"latency_ms"`
	Error      string    `json:"error,omitempty"`
}

// Events emits events and routes them to delivering engines.
type Events interface {
	// Emit logs a canonical event and dispatches it to every transport the
	// profile defines for its type. ID and At are filled when empty.
	Emit(ctx context.Context, e Event) (Event, error)
	// Subscribe registers the engine that delivers a transport (http_push,
	// mqtt, ftp, smtp or attach).
	Subscribe(transport string, fn func(Dispatch)) (cancel func())
	// Report records the outcome of a delivery attempt.
	Report(r DeliveryReport)
	// Targets returns the targets the camera delivers a transport to now,
	// for engines that keep a connection open to them, as an MQTT client
	// does with its broker. They change while the camera runs.
	Targets(transport string) []Target
}

// StreamInfo describes one video stream of the camera: main, sub or third.
type StreamInfo struct {
	Name    string `json:"name"`
	Codec   string `json:"codec"` // h264, h265 or mjpeg
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	FPS     int    `json:"fps"`
	GOP     int    `json:"gop"`
	Bitrate int    `json:"bitrate"`
}

// VideoSource is a precoded stream that server engines send in a loop, one
// access unit per frame. For H.264 and H.265 an access unit is a list of
// NAL units without start codes, the first one a keyframe, and the
// parameter sets are also given apart (VPS only for H.265). For MJPEG an
// access unit holds a single element, a whole JPEG image.
type VideoSource struct {
	Info        StreamInfo
	VPS         []byte
	SPS         []byte
	PPS         []byte
	AccessUnits [][][]byte
}

// Media gives access to the camera's streams and snapshots.
type Media interface {
	Streams() []StreamInfo
	Snapshot(stream string) ([]byte, error)
	Source(stream string) (*VideoSource, error)
	// Watch calls fn when a stream is replaced, for example after a
	// resolution change regenerated it.
	Watch(fn func(stream string)) (cancel func())
}

// CameraData is the camera as seen by templates (.Camera).
type CameraData struct {
	ID       string
	Name     string
	IP       string
	MAC      string
	Serial   string
	Vendor   string
	Model    string
	Firmware string
}

// RequestData is the incoming request as seen by templates (.Request).
// Its values are inserted as data and never evaluated as templates.
type RequestData struct {
	Method   string
	Path     string
	Query    map[string]string
	Headers  map[string]string
	Body     string
	ClientIP string
	Params   map[string]string
}

// TemplateData is the input of a render.
type TemplateData struct {
	Camera    CameraData
	Request   *RequestData
	Event     *Event
	EventName string
	Now       time.Time
	Result    any
}

// Template is a compiled profile template.
type Template interface {
	Render(ctx context.Context, data TemplateData) ([]byte, error)
}

// Templates compiles profile templates with the safe function set.
type Templates interface {
	// Compile parses a template. maxBytes bounds the rendered size; zero
	// means the default of 1 MB.
	Compile(name, text string, maxBytes int) (Template, error)
}

// Files are the recordings a camera keeps of its events: snapshots and
// clips on its simulated SD card or its NAS share (D68, D69). The host
// records them; engines search them and read them, as a device's API and
// its playback do.
type Files interface {
	// Status is the storage's state as the device reports it.
	Status() StorageStatus
	// Find lists the recordings that overlap the query's time range,
	// oldest first.
	Find(ctx context.Context, q FileQuery) ([]FileInfo, error)
	// Open reads a recording by its name. ErrNoFile when there is none;
	// ErrStorageUnavailable while the card is out or failing.
	Open(ctx context.Context, name string) (io.ReadCloser, FileInfo, error)
}

// Storage errors.
var (
	ErrNoFile             = errors.New("no such recording")
	ErrStorageUnavailable = errors.New("the storage is unavailable")
)

// Storage states, as devices report them.
const (
	StoragePresent  = "present"
	StorageFull     = "full"
	StorageAbsent   = "absent"
	StorageError    = "error"
	StorageReadOnly = "read_only"
)

// Kinds of recording.
const (
	FileSnapshot = "snapshot"
	FileClip     = "clip"
)

// StorageStatus is a camera's storage as the device reports it.
type StorageStatus struct {
	Kind  string `json:"kind"` // sd or nas
	State string `json:"state"`
	// CapacityBytes is the card's size; 0 for a share.
	CapacityBytes int64 `json:"capacity_bytes"`
	UsedBytes     int64 `json:"used_bytes"`
	Files         int   `json:"files"`
	// Overwrite replaces the oldest recordings when the card is full.
	Overwrite bool `json:"overwrite"`
}

// Usable reports whether recordings can be read.
func (s StorageStatus) Usable() bool {
	return s.State == StoragePresent || s.State == StorageFull || s.State == StorageReadOnly
}

// FileQuery selects recordings: those that overlap From and To, of a kind
// and an event type when set.
type FileQuery struct {
	From  time.Time
	To    time.Time
	Kind  string
	Event string
	Limit int
}

// FileInfo describes a recording.
type FileInfo struct {
	// Name is its path inside the card or the share, as
	// 20260107/143000_motion.jpg.
	Name   string    `json:"name"`
	Kind   string    `json:"kind"`
	Event  string    `json:"event"`
	Stream string    `json:"stream,omitempty"`
	Size   int64     `json:"size"`
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
}

// Duration is how long a clip lasts; 0 for a snapshot.
func (f FileInfo) Duration() time.Duration { return f.End.Sub(f.Start) }

// Telemetry collects logs, per-route statistics and gaps.
type Telemetry interface {
	Log(level slog.Level, msg string, attrs ...any)
	// Request records a served request for per-minute statistics.
	Request(route, clientIP string, status int, dur time.Duration)
	// Gap records a request the profile does not know (D79).
	Gap(protocol, clientIP, summary string)
	// Client records a client connecting or disconnecting.
	Client(protocol, clientIP string, connected bool)
}
