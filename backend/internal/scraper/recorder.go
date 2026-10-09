package scraper

import "time"

// Fixture is one read-only request a capture made and what the device
// answered, kept so a profile can be compiled from it and replayed. A
// binary body (a snapshot) is kept base64-encoded.
type Fixture struct {
	StepID      string            `json:"step_id"`
	Kind        string            `json:"kind"` // http | rtsp | event
	Method      string            `json:"method,omitempty"`
	Port        int               `json:"port,omitempty"`
	Path        string            `json:"path,omitempty"`
	Query       map[string]string `json:"query,omitempty"`
	Status      int               `json:"status"`
	ContentType string            `json:"content_type,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        string            `json:"body,omitempty"`
	Binary      bool              `json:"binary,omitempty"`
	Bytes       int               `json:"bytes"`
	Stream      string            `json:"stream,omitempty"`
	Vary        []string          `json:"vary,omitempty"`
	At          time.Time         `json:"at"`
	Error       string            `json:"error,omitempty"`
}

// Recorder is the optional seam a prober writes ad-hoc probes to; a capture
// builds its fixtures through RunProgram instead. It is kept for probes
// made outside a program.
type Recorder struct {
	fixtures []Fixture
}

func (r *Recorder) http(method, url string, resp *Response) {}

func (r *Recorder) rtsp(method, url string, res *RTSPResult) {}

// Fixtures returns what the recorder kept.
func (r *Recorder) Fixtures() []Fixture { return r.fixtures }
