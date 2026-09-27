package engine

import (
	"context"
	"encoding/json"
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
	// Files is the camera's simulated SD card; nil when the camera has none.
	Files() Files
	Telemetry() Telemetry
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

// Target is an event receiver as seen by a delivering engine.
type Target struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	URL      string            `json:"url"`
	Method   string            `json:"method,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Username string            `json:"username,omitempty"`
	Password string            `json:"password,omitempty"`
}

// DeliveryPolicy mimics how the real device retries (D42).
type DeliveryPolicy struct {
	Timeout time.Duration `json:"timeout"`
	Retries int           `json:"retries"`
	Backoff time.Duration `json:"backoff"`
}

// Dispatch is an event handed to a delivering engine.
type Dispatch struct {
	Event Event
	// VendorName is how the profile calls the event type.
	VendorName string
	// Transport is the profile's section for the transport.
	Transport json.RawMessage
	Policy    DeliveryPolicy
	Targets   []Target
}

// Delivery statuses reported for each attempt.
const (
	DeliveryOK     = "ok"
	DeliveryRetry  = "retry"
	DeliveryFailed = "failed"
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
	// Subscribe registers the engine that delivers a transport (http_push).
	Subscribe(transport string, fn func(Dispatch)) (cancel func())
	// Report records the outcome of a delivery attempt.
	Report(r DeliveryReport)
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

// Files is the simulated SD card of a camera.
type Files interface {
	Put(ctx context.Context, name string, r io.Reader) error
	Open(name string) (io.ReadCloser, error)
}

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
