package domain

import (
	"slices"
	"time"
)

// FaultKind is a failure that can be injected into a camera (D43).
type FaultKind string

const (
	// FaultServiceDown makes a protocol stop answering: its connections are
	// reset and new ones refused, as a service that crashed.
	FaultServiceDown FaultKind = "service_down"
	// FaultLatency delays everything a protocol reads.
	FaultLatency FaultKind = "latency"
	// FaultErrorStatus answers every request of the HTTP API or RTSP with a
	// status: 401 (refused credentials), 403, 500 or 503.
	FaultErrorStatus FaultKind = "error_status"
	// FaultClockSkew moves the camera's clock.
	FaultClockSkew FaultKind = "clock_skew"
	// FaultNetworkDown takes the camera off the network: nobody reaches it
	// and it reaches nobody. It raises network_lost.
	FaultNetworkDown FaultKind = "network_down"
	// FaultIPConflict makes the camera see another device with its
	// address. It raises ip_conflict and keeps answering.
	FaultIPConflict FaultKind = "ip_conflict"
)

// FaultKinds lists the kinds of fault.
var FaultKinds = []FaultKind{FaultServiceDown, FaultLatency, FaultErrorStatus, FaultClockSkew, FaultNetworkDown, FaultIPConflict}

// FaultEvents are the canonical events a fault raises as it starts, when
// the camera's profile defines them.
var FaultEvents = map[FaultKind]EventType{
	FaultNetworkDown: EventNetworkLost,
	FaultIPConflict:  EventIPConflict,
}

// Limits of faults.
const (
	MaxActiveFaults  = 16
	MaxFaultDuration = 24 * time.Hour
	MaxLatency       = 30 * time.Second
	MaxClockSkew     = 366 * 24 * time.Hour
)

// FaultStatuses are the statuses an error_status fault may answer with.
var FaultStatuses = []int{401, 403, 404, 500, 503}

// FaultParams are what a kind of fault needs: the engine instance it hits
// (service_down, latency, error_status), the status, the delay, the skew.
type FaultParams struct {
	Instance  string `json:"instance,omitempty"`
	Status    int    `json:"status,omitempty"`
	LatencyMS int    `json:"latency_ms,omitempty"`
	SkewS     int64  `json:"skew_s,omitempty"`
}

// Fault is an injected failure. Every fault ends when it expires or by
// hand (RN-14); one without ExpiresAt lasts until ended.
type Fault struct {
	ID        string
	CameraID  string
	Kind      FaultKind
	Params    FaultParams
	StartedAt time.Time
	ExpiresAt *time.Time
	EndedAt   *time.Time
	EndedBy   string
	CreatedBy string
}

// Active reports whether the fault is on at now.
func (f Fault) Active(now time.Time) bool {
	return f.EndedAt == nil && (f.ExpiresAt == nil || now.Before(*f.ExpiresAt))
}

// Key identifies what a fault hits: a new fault of the same kind on the
// same instance replaces the one in place.
func (f Fault) Key() string { return string(f.Kind) + "/" + f.Params.Instance }

// FaultTarget describes an engine instance of a camera for fault checks.
type FaultTarget struct {
	Instance string
	Engine   string // engine name, as http-api or rtsp
	Server   bool   // it listens for clients
}

// ValidateFault checks a fault against the camera's engine instances, and
// its duration: zero means until ended by hand.
func ValidateFault(kind FaultKind, p FaultParams, duration time.Duration, instances []FaultTarget) error {
	v := &ValidationError{}
	if !slices.Contains(FaultKinds, kind) {
		v.Add("kind", "must be one of service_down, latency, error_status, clock_skew, network_down or ip_conflict")
		return v
	}
	if duration < 0 || duration > MaxFaultDuration {
		v.Add("duration_s", "must be between 1 second and 24 hours, or 0 to last until ended")
	}
	needsInstance := kind == FaultServiceDown || kind == FaultLatency || kind == FaultErrorStatus
	if needsInstance {
		var found *FaultTarget
		for i := range instances {
			if instances[i].Instance == p.Instance {
				found = &instances[i]
			}
		}
		switch {
		case p.Instance == "":
			v.Add("instance", "is required: the protocol the fault hits")
		case found == nil:
			v.Add("instance", "the camera has no enabled protocol %q", p.Instance)
		case !found.Server:
			v.Add("instance", "%s sends data out; only protocols that answer clients fail this way", p.Instance)
		case kind == FaultErrorStatus && found.Engine != "http-api" && found.Engine != "rtsp":
			v.Add("instance", "only the HTTP API and RTSP answer with a status")
		}
	} else if p.Instance != "" {
		v.Add("instance", "does not apply to %s", kind)
	}
	switch kind {
	case FaultErrorStatus:
		if !slices.Contains(FaultStatuses, p.Status) {
			v.Add("status", "must be 401, 403, 404, 500 or 503")
		}
	case FaultLatency:
		if p.LatencyMS < 1 || time.Duration(p.LatencyMS)*time.Millisecond > MaxLatency {
			v.Add("latency_ms", "must be between 1 and 30000")
		}
	case FaultClockSkew:
		if p.SkewS == 0 || time.Duration(abs(p.SkewS))*time.Second > MaxClockSkew {
			v.Add("skew_s", "must not be 0 and at most a year either way")
		}
	}
	if kind != FaultErrorStatus && p.Status != 0 {
		v.Add("status", "does not apply to %s", kind)
	}
	if kind != FaultLatency && p.LatencyMS != 0 {
		v.Add("latency_ms", "does not apply to %s", kind)
	}
	if kind != FaultClockSkew && p.SkewS != 0 {
		v.Add("skew_s", "does not apply to %s", kind)
	}
	return v.Err()
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
