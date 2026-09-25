package domain

import (
	"net/url"
	"strings"
	"time"
)

// EventType is a canonical event type. Profiles decide how each type is
// named and serialized for their vendor.
type EventType string

const (
	EventLineCrossing   EventType = "line_crossing"
	EventRegionEntrance EventType = "region_entrance"
	EventRegionExit     EventType = "region_exit"
	EventLoitering      EventType = "loitering"
	EventIntrusion      EventType = "intrusion"
	EventMotion         EventType = "motion"
	EventLPR            EventType = "lpr"
	EventSpeed          EventType = "speed"
	EventTamper         EventType = "tamper"
)

// CanonicalEventTypes lists the built-in canonical types.
var CanonicalEventTypes = []EventType{
	EventLineCrossing, EventRegionEntrance, EventRegionExit, EventLoitering,
	EventIntrusion, EventMotion, EventLPR, EventSpeed, EventTamper,
}

// CustomEventPrefix marks vendor specific types: custom:<name>.
const CustomEventPrefix = "custom:"

// ValidEventType reports whether t is canonical or a well formed custom type.
func ValidEventType(t string) bool {
	for _, c := range CanonicalEventTypes {
		if string(c) == t {
			return true
		}
	}
	name, ok := strings.CutPrefix(t, CustomEventPrefix)
	if !ok || name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// Direction of a line crossing.
type Direction string

const (
	DirectionAB   Direction = "A->B"
	DirectionBA   Direction = "B->A"
	DirectionNone Direction = "none"
)

// ValidDirection reports whether d is a known direction.
func ValidDirection(d Direction) bool {
	return d == DirectionAB || d == DirectionBA || d == DirectionNone
}

// DeliveryStatus is the outcome of one delivery attempt.
type DeliveryStatus string

const (
	// DeliveryOK means the target accepted the event.
	DeliveryOK DeliveryStatus = "ok"
	// DeliveryRetry means the attempt failed and another one will follow.
	DeliveryRetry DeliveryStatus = "retry"
	// DeliveryFailed means the attempt failed and no retries are left.
	DeliveryFailed DeliveryStatus = "failed"
)

// Event is an occurrence emitted by a camera (RN-13: always logged).
type Event struct {
	ID        string
	CameraID  string
	Type      string
	At        time.Time
	Data      []byte // canonical event as JSON
	RuleID    string
	TriggerID string
}

// Delivery is one attempt to send an event to a target.
type Delivery struct {
	ID         string
	EventID    string
	TargetID   string
	Attempt    int
	At         time.Time
	Status     DeliveryStatus
	HTTPStatus int
	LatencyMS  int64
	Error      string
}

// TargetType is the kind of external receiver.
type TargetType string

// TargetHTTP receives events with an HTTP request (http-push engine).
const TargetHTTP TargetType = "http"

// Target is a reusable event receiver, shared by several cameras (D45).
type Target struct {
	ID        string
	Name      string
	Type      TargetType
	URL       string
	Method    string
	Headers   map[string]string
	Username  string
	HasSecret bool
	Enabled   bool
	CreatedAt time.Time
}

// ValidateTarget checks a target definition.
func ValidateTarget(t Target) error {
	v := &ValidationError{}
	name := strings.TrimSpace(t.Name)
	if name == "" || len(name) > 64 {
		v.Add("name", "is required and must be at most 64 characters")
	}
	if t.Type != TargetHTTP {
		v.Add("type", "must be http")
	}
	u, err := url.Parse(t.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		v.Add("url", "must be an absolute http or https URL")
	} else if u.User != nil {
		v.Add("url", "must not embed credentials; use username and password")
	}
	switch t.Method {
	case "GET", "POST", "PUT":
	default:
		v.Add("method", "must be GET, POST or PUT")
	}
	for k := range t.Headers {
		if k == "" || strings.ContainsAny(k, " :\r\n") {
			v.Add("headers", "invalid header name %q", k)
		}
	}
	return v.Err()
}
