// Package profile reads, validates and compiles profile documents
// (profile.yaml). A profile is data, not code: it parameterizes the engines
// with the vendor's names, routes, payloads and defaults.
package profile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is the profile schema this MockVision understands.
const SchemaVersion = 1

// Document is a profile.yaml after validation.
type Document struct {
	Schema   int                        `json:"schema"`
	Profile  Meta                       `json:"profile"`
	Identity Identity                   `json:"identity"`
	State    map[string]Param           `json:"state,omitempty"`
	Media    Media                      `json:"media"`
	Engines  map[string]json.RawMessage `json:"engines"`
	Events   map[string]EventSpec       `json:"events,omitempty"`
	VCA      *VCA                       `json:"vca,omitempty"`
	Coverage map[string]string          `json:"coverage,omitempty"`
}

// Meta identifies the profile.
type Meta struct {
	ID       string   `json:"id,omitempty"`
	Version  string   `json:"version,omitempty"`
	Name     string   `json:"name"`
	Vendor   string   `json:"vendor"`
	Model    string   `json:"model"`
	Firmware []string `json:"firmware,omitempty"`
	Extends  string   `json:"extends,omitempty"`
}

// Identity holds the serial pattern and factory values of the device.
type Identity struct {
	Serial  string            `json:"serial"`
	OUI     string            `json:"oui,omitempty"`
	Factory Factory           `json:"factory"`
	Device  map[string]string `json:"device,omitempty"`
}

// Factory values are what a camera gets when reset to factory defaults.
type Factory struct {
	Network FactoryNetwork `json:"network"`
	Users   []FactoryUser  `json:"users"`
}

// FactoryNetwork is the factory addressing (D24: DHCP fallback, RN-10).
type FactoryNetwork struct {
	IP      string `json:"ip,omitempty"`
	Mask    string `json:"mask,omitempty"`
	Gateway string `json:"gateway,omitempty"`
}

// FactoryUser is a factory account.
type FactoryUser struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

// Param types.
const (
	TypeString = "string"
	TypeInt    = "int"
	TypeFloat  = "float"
	TypeBool   = "bool"
	TypeEnum   = "enum"
)

// Param is a native parameter. With Bind it is effective (it drives the
// simulation through a canonical key); without it, declarative (RN-07).
type Param struct {
	Type        string   `json:"type"`
	Default     any      `json:"default,omitempty"`
	Values      []any    `json:"values,omitempty"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	MaxLength   int      `json:"max_length,omitempty"`
	Bind        string   `json:"bind,omitempty"`
	Description string   `json:"description,omitempty"`
}

// Media lists the streams of the device.
type Media struct {
	Streams map[string]Stream `json:"streams"`
}

// Stream describes what a stream supports and its defaults.
type Stream struct {
	Codecs      []string      `json:"codecs"`
	Resolutions []string      `json:"resolutions"`
	FPS         *Range        `json:"fps,omitempty"`
	Bitrate     *Range        `json:"bitrate,omitempty"`
	Default     StreamDefault `json:"default"`
}

// Range is an inclusive integer range.
type Range struct {
	Min int `json:"min,omitempty"`
	Max int `json:"max,omitempty"`
}

// StreamDefault are the default stream settings.
type StreamDefault struct {
	Codec      string `json:"codec"`
	Resolution string `json:"resolution"`
	FPS        int    `json:"fps"`
	Bitrate    int    `json:"bitrate,omitempty"`
	GOP        int    `json:"gop,omitempty"`
}

// EventSpec is how the profile names and delivers a canonical event type.
type EventSpec struct {
	VendorName  string                     `json:"vendor_name,omitempty"`
	MinInterval Duration                   `json:"min_interval,omitempty"`
	Transports  map[string]json.RawMessage `json:"transports,omitempty"`
	Delivery    DeliverySpec               `json:"delivery,omitempty"`
}

// DeliverySpec is the device's own delivery behavior (D42).
type DeliverySpec struct {
	Timeout Duration `json:"timeout,omitempty"`
	Retries *int     `json:"retries,omitempty"`
	Backoff Duration `json:"backoff,omitempty"`
}

// VCA lists supported analytics.
type VCA struct {
	Rules         []string `json:"rules,omitempty"`
	ObjectClasses []string `json:"object_classes,omitempty"`
	PlateFormat   string   `json:"plate_format,omitempty"`
}

// HTTPPush is the http_push transport of an event.
type HTTPPush struct {
	Method      string            `json:"method,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Template    string            `json:"template,omitempty"`
	Body        string            `json:"body,omitempty"`
	Path        string            `json:"path,omitempty"`
	Image       bool              `json:"image,omitempty"`
}

// Duration is a JSON string such as "5s" or "500ms".
type Duration time.Duration

// UnmarshalJSON parses a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like 5s")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// D returns the time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// decodeDocument converts validated generic values into a Document.
func decodeDocument(v any) (*Document, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return DecodeJSON(raw)
}

// DecodeJSON decodes a stored, resolved profile.
func DecodeJSON(raw []byte) (*Document, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc Document
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	for k, p := range doc.State {
		p.Default = normalizeNumber(p.Default)
		for i := range p.Values {
			p.Values[i] = normalizeNumber(p.Values[i])
		}
		doc.State[k] = p
	}
	return &doc, nil
}

func normalizeNumber(v any) any {
	n, ok := v.(json.Number)
	if !ok {
		return v
	}
	if i, err := n.Int64(); err == nil {
		return i
	}
	f, _ := n.Float64()
	return f
}

// EngineRef splits "http-api@^1" into name and range.
func EngineRef(ref string) (name, rng string, err error) {
	name, rng, ok := strings.Cut(ref, "@")
	if !ok || name == "" || rng == "" {
		return "", "", fmt.Errorf("engine reference %q must look like name@range", ref)
	}
	return name, rng, nil
}

// EngineName returns the engine name of an instance section.
func EngineName(section json.RawMessage) (name, rng string, err error) {
	var head struct {
		Engine string `json:"engine"`
	}
	if err := json.Unmarshal(section, &head); err != nil {
		return "", "", err
	}
	return EngineRef(head.Engine)
}

// SortedKeys returns the keys of a map in order.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
