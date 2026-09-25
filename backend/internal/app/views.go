package app

import (
	"encoding/json"
	"time"

	"github.com/corticoide/mockvision/backend/internal/pkg"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/backend/internal/telemetry"
)

// ProfileRef identifies a profile version.
type ProfileRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Name    string `json:"name"`
	Vendor  string `json:"vendor"`
	Model   string `json:"model"`
	Level   string `json:"level"`
}

// NetworkView is the network identity of a camera.
type NetworkView struct {
	Mode    string   `json:"mode"`
	Parent  string   `json:"parent"`
	MAC     string   `json:"mac"`
	IPMode  string   `json:"ip_mode"`
	IP      string   `json:"ip"`
	Netmask string   `json:"netmask"`
	Prefix  int      `json:"prefix"`
	Gateway string   `json:"gateway"`
	DNS     []string `json:"dns"`
}

// StatusView is the runtime state of a camera.
type StatusView struct {
	State         string     `json:"state"`
	Reason        string     `json:"reason"`
	StartedAt     *time.Time `json:"started_at"`
	LastHeartbeat *time.Time `json:"last_heartbeat"`
	Netns         string     `json:"netns,omitempty"`
	PID           int        `json:"pid,omitempty"`
	Retries       int        `json:"retries"`
	// PendingRestart lists saved changes a running camera applies only
	// when it restarts (RN-09): network, protocols.
	PendingRestart []string `json:"pending_restart"`
}

// ProtocolView is an engine instance of the camera's profile (RN-04).
type ProtocolView struct {
	Instance    string `json:"instance"`
	Engine      string `json:"engine"`
	Role        string `json:"role"`
	Enabled     bool   `json:"enabled"`
	Port        int    `json:"port"`
	DefaultPort int    `json:"default_port"`
}

// EndpointView is where a camera serves a protocol.
type EndpointView struct {
	Instance string `json:"instance"`
	Engine   string `json:"engine"`
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
	URL      string `json:"url"`
}

// StreamView is a camera stream and its rendition.
type StreamView struct {
	Name            string `json:"name"`
	Codec           string `json:"codec"`
	Resolution      string `json:"resolution"`
	FPS             int    `json:"fps"`
	GOP             int    `json:"gop"`
	Bitrate         int    `json:"bitrate"`
	AssetID         string `json:"asset_id"`
	RenditionID     string `json:"rendition_id,omitempty"`
	RenditionStatus string `json:"rendition_status"`
	RenditionError  string `json:"rendition_error,omitempty"`
}

// UserView is a camera account; passwords are write-only.
type UserView struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

// TargetRef is a target linked to a camera.
type TargetRef struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	EventTypes []string `json:"event_types"`
}

// MetricsView is the latest heartbeat of a camera.
type MetricsView struct {
	At         time.Time `json:"at"`
	CPUPercent float64   `json:"cpu_percent"`
	RSSBytes   uint64    `json:"rss_bytes"`
	Clients    int       `json:"clients"`
	BytesIn    uint64    `json:"bytes_in"`
	BytesOut   uint64    `json:"bytes_out"`
	Requests   uint64    `json:"requests"`
}

func metricsView(s telemetry.CameraSample) *MetricsView {
	return &MetricsView{At: s.At, CPUPercent: s.CPUPercent, RSSBytes: s.RSSBytes, Clients: s.Clients, BytesIn: s.BytesIn, BytesOut: s.BytesOut, Requests: s.Requests}
}

// CameraView is a camera as the API returns it.
type CameraView struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Profile      ProfileRef     `json:"profile"`
	Serial       string         `json:"serial"`
	DesiredState string         `json:"desired_state"`
	Autostart    bool           `json:"autostart"`
	Tags         []string       `json:"tags"`
	Network      NetworkView    `json:"network"`
	Status       StatusView     `json:"status"`
	Endpoints    []EndpointView `json:"endpoints"`
	Protocols    []ProtocolView `json:"protocols"`
	Streams      []StreamView   `json:"streams"`
	Users        []UserView     `json:"users"`
	Targets      []TargetRef    `json:"targets"`
	Metrics      *MetricsView   `json:"metrics"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// ParamView is a native parameter of a camera.
type ParamView struct {
	Key         string    `json:"key"`
	Type        string    `json:"type"`
	Value       any       `json:"value"`
	Default     any       `json:"default"`
	Values      []any     `json:"values,omitempty"`
	Min         *float64  `json:"min,omitempty"`
	Max         *float64  `json:"max,omitempty"`
	Bind        string    `json:"bind,omitempty"`
	Effective   bool      `json:"effective"`
	Description string    `json:"description,omitempty"`
	Origin      string    `json:"origin"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ProfileView is an installed profile version.
type ProfileView struct {
	ID              string            `json:"id"`
	ProfileID       string            `json:"profile_id"`
	Version         string            `json:"version"`
	Name            string            `json:"name"`
	Vendor          string            `json:"vendor"`
	Model           string            `json:"model"`
	Firmware        []string          `json:"firmware"`
	Level           string            `json:"level"`
	SignatureStatus string            `json:"signature_status"`
	Archived        bool              `json:"archived"`
	CameraCount     int               `json:"camera_count"`
	CreatedAt       time.Time         `json:"created_at"`
	Coverage        map[string]string `json:"coverage,omitempty"`
}

// ProfileStreamView describes what a profile stream supports.
type ProfileStreamView struct {
	Name        string   `json:"name"`
	Codecs      []string `json:"codecs"`
	Resolutions []string `json:"resolutions"`
	FPSMin      int      `json:"fps_min"`
	FPSMax      int      `json:"fps_max"`
	Default     struct {
		Codec      string `json:"codec"`
		Resolution string `json:"resolution"`
		FPS        int    `json:"fps"`
		Bitrate    int    `json:"bitrate"`
		GOP        int    `json:"gop"`
	} `json:"default"`
}

// ProfileEngineView is an engine instance of a profile.
type ProfileEngineView struct {
	Instance string `json:"instance"`
	Engine   string `json:"engine"`
	Port     int    `json:"port,omitempty"`
}

// ProfileDetail adds what the camera wizard needs.
type ProfileDetail struct {
	ProfileView
	Streams      []ProfileStreamView `json:"streams"`
	Engines      []ProfileEngineView `json:"engines"`
	Events       []string            `json:"events"`
	FactoryUsers []UserView          `json:"factory_users"`
	FactoryIP    string              `json:"factory_ip,omitempty"`
	Params       []ParamView         `json:"params"`
}

// ImportResult is the outcome of a package import.
type ImportResult struct {
	Profile ProfileView `json:"profile"`
	Report  pkg.Report  `json:"report"`
	Created bool        `json:"created"`
}

// ImportError carries the report of a rejected package.
type ImportError struct {
	Report pkg.Report
}

func (e *ImportError) Error() string {
	for _, p := range e.Report.Problems {
		if p.Severity == profile.SeverityError {
			return "package rejected: " + p.Message
		}
	}
	return "package rejected"
}

// AssetView is a media asset.
type AssetView struct {
	ID          string    `json:"id"`
	Filename    string    `json:"filename"`
	MIME        string    `json:"mime"`
	Width       int       `json:"width"`
	Height      int       `json:"height"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	Builtin     bool      `json:"builtin"`
	CameraCount int       `json:"camera_count"`
	CreatedAt   time.Time `json:"created_at"`
}

// TargetView is an event receiver.
type TargetView struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	URL         string            `json:"url"`
	Method      string            `json:"method"`
	Headers     map[string]string `json:"headers"`
	Username    string            `json:"username"`
	HasPassword bool              `json:"has_password"`
	Enabled     bool              `json:"enabled"`
	CameraCount int               `json:"camera_count"`
	CreatedAt   time.Time         `json:"created_at"`
}

// DeliveryView is one delivery attempt.
type DeliveryView struct {
	ID         string    `json:"id"`
	EventID    string    `json:"event_id"`
	TargetID   string    `json:"target_id"`
	TargetName string    `json:"target_name"`
	Attempt    int       `json:"attempt"`
	At         time.Time `json:"at"`
	Status     string    `json:"status"`
	HTTPStatus int       `json:"http_status,omitempty"`
	LatencyMS  int64     `json:"latency_ms"`
	Error      string    `json:"error,omitempty"`
}

// EventView is an event with its deliveries.
type EventView struct {
	ID             string          `json:"id"`
	CameraID       string          `json:"camera_id"`
	CameraName     string          `json:"camera_name"`
	Type           string          `json:"type"`
	At             time.Time       `json:"at"`
	Data           json.RawMessage `json:"data"`
	Deliveries     []DeliveryView  `json:"deliveries"`
	DeliveryStatus string          `json:"delivery_status"`
	LatencyMS      *int64          `json:"latency_ms"`
}

// Page is a cursor-paginated list.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}
