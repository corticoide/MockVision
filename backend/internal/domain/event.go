package domain

import (
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strconv"
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
	// System events: the device's own state, as its faults raise them.
	EventNetworkLost EventType = "network_lost"
	EventIPConflict  EventType = "ip_conflict"
)

// CanonicalEventTypes lists the built-in canonical types.
var CanonicalEventTypes = []EventType{
	EventLineCrossing, EventRegionEntrance, EventRegionExit, EventLoitering,
	EventIntrusion, EventMotion, EventLPR, EventSpeed, EventTamper,
	EventNetworkLost, EventIPConflict,
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
	// DeliverySkipped means the camera chose not to send, as a real one
	// within its mail interval.
	DeliverySkipped DeliveryStatus = "skipped"
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

// Target types: each is reached by an engine of the profile (D31).
const (
	// TargetHTTP receives events with an HTTP request (http-push).
	TargetHTTP TargetType = "http"
	// TargetMQTT is a broker the camera publishes its events to.
	TargetMQTT TargetType = "mqtt"
	// TargetFTP and TargetSFTP are servers the camera uploads files to.
	TargetFTP  TargetType = "ftp"
	TargetSFTP TargetType = "sftp"
	// TargetSMTP is a mail server the camera mails its events through.
	TargetSMTP TargetType = "smtp"
)

// TargetTypes lists the target types.
var TargetTypes = []TargetType{TargetHTTP, TargetMQTT, TargetFTP, TargetSFTP, TargetSMTP}

// Authentication of the camera toward an http target.
const (
	TargetAuthBasic  = "basic"
	TargetAuthDigest = "digest"
)

// TLS modes of an smtp target.
const (
	TLSNone     = "none"
	TLSStartTLS = "starttls"
	TLSImplicit = "tls"
)

// MaxRecipients bounds the recipients of an smtp target, as devices do.
const MaxRecipients = 5

// DeliveryOverride is a target's own delivery policy, replacing the
// profile's where set (D42).
type DeliveryOverride struct {
	TimeoutMS *int64 `json:"timeout_ms,omitempty"`
	Retries   *int   `json:"retries,omitempty"`
	BackoffMS *int64 `json:"backoff_ms,omitempty"`
}

// Empty reports whether the override changes nothing.
func (o *DeliveryOverride) Empty() bool {
	return o == nil || (o.TimeoutMS == nil && o.Retries == nil && o.BackoffMS == nil)
}

// Target is a reusable event receiver, shared by several cameras (D45).
// Which fields apply depends on its type.
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
	// http: basic or digest.
	Auth string
	// mqtt: templates replacing the profile's topic and client ID.
	Topic    string
	ClientID string
	// sftp: the pinned SHA256 fingerprint of the server key.
	HostKey string
	// smtp: TLS mode, sender and recipients.
	TLS  string
	From string
	To   []string
	// mqtt and smtp: accept a TLS certificate that does not verify.
	Insecure bool
	Delivery *DeliveryOverride
}

var hostKeyPattern = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)

// ValidateTarget checks a target definition.
func ValidateTarget(t Target) error {
	v := &ValidationError{}
	name := strings.TrimSpace(t.Name)
	if name == "" || len(name) > 64 {
		v.Add("name", "is required and must be at most 64 characters")
	}
	u, err := url.Parse(t.URL)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		u = nil
	} else if u.User != nil {
		v.Add("url", "must not embed credentials; use username and password")
	} else if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			v.Add("url", "has an invalid port")
		}
	}
	scheme := func(want ...string) {
		if u == nil || !slices.Contains(want, u.Scheme) {
			v.Add("url", "must be an absolute %s URL", strings.Join(want, " or "))
		}
	}
	switch t.Type {
	case TargetHTTP:
		scheme("http", "https")
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
		switch t.Auth {
		case "", TargetAuthBasic, TargetAuthDigest:
		default:
			v.Add("auth", "must be basic or digest")
		}
	case TargetMQTT:
		scheme("mqtt", "mqtts")
		if u != nil && strings.Trim(u.Path, "/") != "" {
			v.Add("url", "a broker URL has no path; the topic comes from the profile or the topic field")
		}
		if len(t.Topic) > 512 || strings.ContainsAny(t.Topic, "\x00#+") {
			v.Add("topic", "must be at most 512 characters, without # or + wildcards")
		}
		if len(t.ClientID) > 200 {
			v.Add("client_id", "must be at most 200 characters")
		}
	case TargetFTP:
		scheme("ftp")
	case TargetSFTP:
		scheme("sftp")
		if t.HostKey != "" && !hostKeyPattern.MatchString(t.HostKey) {
			v.Add("host_key", "must be a SHA256 fingerprint such as SHA256:%s", strings.Repeat("A", 43))
		}
	case TargetSMTP:
		scheme("smtp", "smtps")
		if u != nil && strings.Trim(u.Path, "/") != "" {
			v.Add("url", "a mail server URL has no path")
		}
		switch t.TLS {
		case "", TLSNone, TLSStartTLS, TLSImplicit:
		default:
			v.Add("tls", "must be none, starttls or tls")
		}
		if !validAddress(t.From) {
			v.Add("from", "must be an e-mail address")
		}
		if len(t.To) == 0 || len(t.To) > MaxRecipients {
			v.Add("to", "must list 1 to %d recipients", MaxRecipients)
		}
		for _, a := range t.To {
			if !validAddress(a) {
				v.Add("to", "%q is not an e-mail address", a)
			}
		}
	default:
		v.Add("type", "must be one of http, mqtt, ftp, sftp or smtp")
	}
	if o := t.Delivery; o != nil {
		if o.TimeoutMS != nil && (*o.TimeoutMS < 100 || *o.TimeoutMS > 120_000) {
			v.Add("delivery.timeout_ms", "must be between 100 and 120000")
		}
		if o.Retries != nil && (*o.Retries < 0 || *o.Retries > 10) {
			v.Add("delivery.retries", "must be between 0 and 10")
		}
		if o.BackoffMS != nil && (*o.BackoffMS < 0 || *o.BackoffMS > 600_000) {
			v.Add("delivery.backoff_ms", "must be between 0 and 600000")
		}
	}
	return v.Err()
}

// validAddress reports whether s is a bare e-mail address, as SMTP's MAIL
// and RCPT commands take it.
func validAddress(s string) bool {
	if s == "" || len(s) > 254 || strings.ContainsAny(s, "<>\r\n \t,;") {
		return false
	}
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && a.Name == ""
}
