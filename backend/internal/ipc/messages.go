package ipc

import (
	"encoding/json"

	"github.com/corticoide/mockvision/sdk/engine"
)

// Camera to service.
const (
	TypeHello        = "hello"
	TypeReady        = "ready"
	TypeFailed       = "failed"
	TypeBye          = "bye"
	TypeHeartbeat    = "heartbeat"
	TypeEvent        = "event"
	TypeDelivery     = "delivery"
	TypeStateChanged = "state.changed"
	TypeClient       = "client"
	TypeRequestStats = "request.stats"
	TypeGap          = "gap"
	TypeLog          = "log"
)

// Service to camera.
const (
	TypeConfigure  = "configure"
	TypeReload     = "reload"
	TypeTrigger    = "trigger"
	TypeFaultStart = "fault.start"
	TypeFaultStop  = "fault.stop"
	TypeStateSet   = "state.set"
	TypeStop       = "stop"
)

// HeartbeatInterval is how often cameras report; three missed heartbeats
// mark the camera as not responding.
const HeartbeatInterval = 2_000 // ms

// Hello is the first message of a camera process.
type Hello struct {
	CameraID string `json:"camera_id"`
	PID      int    `json:"pid"`
	Version  string `json:"version"`
}

// EngineConfig enables an engine instance of the profile on a port.
type EngineConfig struct {
	Instance string `json:"instance"`
	Enabled  bool   `json:"enabled"`
	Port     int    `json:"port"`
}

// Stream is a precoded stream ready to be served.
type Stream struct {
	Name         string `json:"name"`
	Codec        string `json:"codec"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FPS          int    `json:"fps"`
	GOP          int    `json:"gop"`
	Bitrate      int    `json:"bitrate"`
	GOPPath      string `json:"gop_path"`
	SnapshotPath string `json:"snapshot_path"`
}

// Target is an event receiver linked to the camera.
type Target struct {
	engine.Target
	// EventTypes limits the target to some event types; empty means all.
	EventTypes []string `json:"event_types,omitempty"`
}

// Configure is the full configuration of a camera.
type Configure struct {
	Identity engine.Identity `json:"identity"`
	Profile  json.RawMessage `json:"profile"`
	State    map[string]any  `json:"state"`
	Engines  []EngineConfig  `json:"engines"`
	Users    []engine.User   `json:"users"`
	Streams  []Stream        `json:"streams"`
	Targets  []Target        `json:"targets"`
}

// Reload replaces parts of the configuration; nil fields stay as they are.
type Reload struct {
	Streams []Stream       `json:"streams,omitempty"`
	Targets []Target       `json:"targets,omitempty"`
	Users   []engine.User  `json:"users,omitempty"`
	State   map[string]any `json:"state,omitempty"`
}

// Endpoint is where an engine listens.
type Endpoint struct {
	Instance string `json:"instance"`
	Engine   string `json:"engine"`
	Socket   string `json:"socket"`
	Network  string `json:"network"`
	Port     int    `json:"port"`
}

// Ready reports that every engine started.
type Ready struct {
	Endpoints []Endpoint `json:"endpoints"`
}

// Failed reports why a camera could not start.
type Failed struct {
	Reason string `json:"reason"`
}

// Bye is the last message of a camera process.
type Bye struct {
	Reason string `json:"reason"`
}

// Heartbeat carries the camera's metrics.
type Heartbeat struct {
	CPUPercent float64                  `json:"cpu_percent"`
	RSSBytes   uint64                   `json:"rss_bytes"`
	Clients    int                      `json:"clients"`
	BytesIn    uint64                   `json:"bytes_in"`
	BytesOut   uint64                   `json:"bytes_out"`
	Requests   uint64                   `json:"requests"`
	Engines    map[string]engine.Health `json:"engines"`
}

// EventMsg reports an emitted event.
type EventMsg struct {
	Event engine.Event `json:"event"`
}

// StateChanged reports changes applied by clients of the emulated API.
type StateChanged struct {
	Changes []engine.Change `json:"changes"`
}

// ClientMsg reports a client connecting or disconnecting.
type ClientMsg struct {
	Protocol  string `json:"protocol"`
	IP        string `json:"ip"`
	Connected bool   `json:"connected"`
}

// Gap reports a request the profile does not know (D79).
type Gap struct {
	Protocol string `json:"protocol"`
	ClientIP string `json:"client_ip"`
	Summary  string `json:"summary"`
	Count    int    `json:"count"`
}

// Log is a log line of the camera.
type Log struct {
	Level string         `json:"level"`
	Msg   string         `json:"msg"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// Trigger asks the camera to emit an event.
type Trigger struct {
	Type      string         `json:"type"`
	Direction string         `json:"direction,omitempty"`
	Rule      *engine.Rule   `json:"rule,omitempty"`
	Object    *engine.Object `json:"object,omitempty"`
	Plate     *engine.Plate  `json:"plate,omitempty"`
	Speed     *engine.Speed  `json:"speed,omitempty"`
	Custom    map[string]any `json:"custom,omitempty"`
}

// TriggerResult is the reply to Trigger.
type TriggerResult struct {
	Event engine.Event `json:"event"`
}

// StateSet changes parameters from the panel or the API.
type StateSet struct {
	Values map[string]any `json:"values"`
	Origin engine.Origin  `json:"origin"`
}

// Stop asks the camera to shut down within a deadline.
type Stop struct {
	DeadlineMS int64 `json:"deadline_ms"`
}
