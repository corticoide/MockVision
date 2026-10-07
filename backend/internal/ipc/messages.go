package ipc

import (
	"encoding/json"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
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
	// A DHCP camera reports its lease, that it found no server, or that
	// it lost its lease (D24).
	TypeDHCPLease  = "dhcp.lease"
	TypeDHCPFailed = "dhcp.failed"
	TypeDHCPLost   = "dhcp.lost"
	// TypeFilesFind asks the service for the recordings the camera keeps,
	// as its API searches them.
	TypeFilesFind = "files.find"
	// TypeRecorded reports a recording the camera wrote to its NAS share.
	TypeRecorded = "recorded"
	// TypeNASState reports whether the camera reaches its NAS share.
	TypeNASState = "nas.state"
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
	TypeTargetTest = "target.test"
	// TypeAnalytics asks for what the camera's analytics counted.
	TypeAnalytics = "analytics"
	// The service refuses a leased address someone else uses; the camera
	// declines it and asks again.
	TypeDHCPDecline = "dhcp.decline"
	// TypeStorage replaces the camera's storage and its state.
	TypeStorage = "storage"
	// TypeFileRead reads part of a recording on the camera's NAS share,
	// for the panel to download it.
	TypeFileRead = "file.read"
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

// Lease is an address a DHCP server gave the camera.
type Lease struct {
	IP     string   `json:"ip"`
	Prefix int      `json:"prefix"`
	Router string   `json:"router,omitempty"`
	DNS    []string `json:"dns,omitempty"`
	Server string   `json:"server"`
	// Seconds is the lease time.
	Seconds int `json:"seconds"`
}

// DHCPStatus explains a DHCP failure or a lost lease.
type DHCPStatus struct {
	Reason string `json:"reason"`
}

// DHCPDecline names the leased address the camera must decline.
type DHCPDecline struct {
	IP     string `json:"ip"`
	Reason string `json:"reason"`
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
	StreamPath   string `json:"stream_path"`
	SnapshotPath string `json:"snapshot_path"`
}

// Target is an event receiver linked to the camera.
type Target struct {
	engine.Target
	// EventTypes limits the target to some event types; empty means all.
	EventTypes []string `json:"event_types,omitempty"`
}

// VCA is the camera's video analytics: the rules drawn on its picture and
// its stored triggers, enabled or not (D39, D40).
type VCA struct {
	Rules    []domain.Rule    `json:"rules"`
	Triggers []domain.Trigger `json:"triggers"`
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
	VCA      VCA             `json:"vca"`
	// DNS servers of the camera; empty means the node's.
	DNS []string `json:"dns,omitempty"`
	// Faults are the ones on as the camera starts; they raise no event.
	Faults []Fault `json:"faults,omitempty"`
	// Storage is where the camera keeps its recordings.
	Storage Storage `json:"storage"`
}

// Storage is where a camera keeps its recordings, and its state. The
// service writes the SD card in SDDir, which the camera may only read;
// the camera writes its NAS share itself (D68, D69).
type Storage struct {
	Kind string `json:"kind"` // none, sd or nas
	// SDDir is the camera's card on the node; it exists whatever Kind is,
	// so the camera can read it once it gets a card.
	SDDir  string               `json:"sd_dir"`
	Status engine.StorageStatus `json:"status"`
	NAS    *NAS                 `json:"nas,omitempty"`
}

// NAS is the share a camera records to.
type NAS struct {
	URL      string `json:"url"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// FilesFind asks for the camera's recordings: those that overlap From and
// To, or the one named.
type FilesFind struct {
	Name  string    `json:"name,omitempty"`
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
	Kind  string    `json:"kind,omitempty"`
	Event string    `json:"event,omitempty"`
	Limit int       `json:"limit,omitempty"`
}

// FilesFound is the reply to FilesFind.
type FilesFound struct {
	Files []engine.FileInfo `json:"files"`
}

// Recorded reports a recording written to the NAS share for an event.
type Recorded struct {
	EventID string          `json:"event_id"`
	File    engine.FileInfo `json:"file"`
}

// NASState says whether the camera reaches its share, and why not.
type NASState struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// FileRead asks for Length bytes of a NAS recording from Offset.
type FileRead struct {
	Name   string `json:"name"`
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
}

// FileData is the reply to FileRead; EOF marks the end of the file.
type FileData struct {
	Data []byte `json:"data"`
	Size int64  `json:"size"`
	EOF  bool   `json:"eof"`
}

// MaxFileRead bounds a FileRead, so its reply fits in a message.
const MaxFileRead = 512 << 10

// Fault is a failure injected into the camera (D43), sent with
// fault.start; the camera raises the fault's event, if any, as it starts.
type Fault struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	domain.FaultParams
}

// FaultStop ends a fault.
type FaultStop struct {
	ID string `json:"id"`
}

// Reload replaces parts of the configuration; nil fields stay as they are.
// Targets and VCA are pointers because a camera may be left without any:
// an empty list must reach it, and would vanish as omitempty.
type Reload struct {
	Streams []Stream       `json:"streams,omitempty"`
	Targets *[]Target      `json:"targets,omitempty"`
	Users   []engine.User  `json:"users,omitempty"`
	State   map[string]any `json:"state,omitempty"`
	VCA     *VCA           `json:"vca,omitempty"`
	// DNS replaces the camera's DNS servers, after a lease brought others.
	DNS []string `json:"dns,omitempty"`
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

// EventMsg reports an emitted event, with the stored trigger behind it,
// if any.
type EventMsg struct {
	Event     engine.Event `json:"event"`
	TriggerID string       `json:"trigger_id,omitempty"`
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

// Trigger asks the camera to emit an event now: a manual one, of which
// the camera generates what the message leaves out, or, with TriggerID
// alone, one of a stored trigger's.
type Trigger struct {
	Type      string `json:"type,omitempty"`
	TriggerID string `json:"trigger_id,omitempty"`
	Direction string `json:"direction,omitempty"`
	// Rule is where the event happens: required for the events that come
	// from rules.
	Rule   *domain.Rule   `json:"rule,omitempty"`
	Object *engine.Object `json:"object,omitempty"`
	Plate  *engine.Plate  `json:"plate,omitempty"`
	Speed  *engine.Speed  `json:"speed,omitempty"`
	Custom map[string]any `json:"custom,omitempty"`
}

// TriggerResult is the reply to Trigger.
type TriggerResult struct {
	Event engine.Event `json:"event"`
}

// AnalyticsQuery asks for the camera's counts and its heat map at a size
// (0 columns for none); Reset starts them again from zero.
type AnalyticsQuery struct {
	Cols  int  `json:"cols"`
	Rows  int  `json:"rows"`
	Reset bool `json:"reset,omitempty"`
}

// Analytics is what a camera's analytics counted from the events it
// emitted since Since: crossings of each line, entries, exits and
// occupancy of each region, events of each type, and where objects were.
type Analytics struct {
	Since   time.Time      `json:"since"`
	Lines   []LineCount    `json:"lines"`
	Regions []RegionCount  `json:"regions"`
	Events  map[string]int `json:"events"`
	Heat    *Heat          `json:"heat,omitempty"`
}

// LineCount is the crossings of a line, by direction and object class.
type LineCount struct {
	RuleID  string                   `json:"rule_id"`
	Name    string                   `json:"name"`
	AToB    int                      `json:"a_to_b"`
	BToA    int                      `json:"b_to_a"`
	Classes map[string]DirectionPair `json:"classes"`
}

// DirectionPair counts crossings each way.
type DirectionPair struct {
	AToB int `json:"a_to_b"`
	BToA int `json:"b_to_a"`
}

// RegionCount is what happened in a region: objects that entered and left,
// those inside now (entries minus exits, never below zero) and its events
// by type.
type RegionCount struct {
	RuleID    string         `json:"rule_id"`
	Name      string         `json:"name"`
	Entries   int            `json:"entries"`
	Exits     int            `json:"exits"`
	Occupancy int            `json:"occupancy"`
	Events    map[string]int `json:"events"`
}

// Heat counts, for each cell of a grid over the picture, the objects seen
// there; cells run row by row from the top left corner.
type Heat struct {
	Cols  int   `json:"cols"`
	Rows  int   `json:"rows"`
	Cells []int `json:"cells"`
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

// TargetTest asks the camera to send a test request to a target from its
// own network, as its deliveries would.
type TargetTest struct {
	Target engine.Target `json:"target"`
}

// TargetTestResult is the reply to TargetTest.
type TargetTestResult struct {
	OK         bool   `json:"ok"`
	HTTPStatus int    `json:"http_status,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
	Error      string `json:"error,omitempty"`
}
