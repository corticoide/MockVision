// Package selftest replays a profile's recordings of the real device, its
// fixtures, against a camera of that profile, and says which ones the
// camera answers as the device did (D77, D88). Fields that change on every
// answer, such as the time, a nonce or the serial, are compared by type,
// not by value.
//
// A fixture is a request and the answer the device gave, or a sequence of
// them, or an event and what the device sent for it on each transport:
//
//	id: device-info
//	request: { method: GET, path: /cgi-bin/magicBox.cgi, query: { action: getSystemInfo } }
//	response:
//	  status: 200
//	  headers: { Content-Type: text/plain }
//	  body: |
//	    serialNumber=4E0AB2EPAG00B3B
//	vary:
//	  - { in: body, regex: "serialNumber=(.*)", as: serial }
package selftest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/corticoide/mockvision/backend/internal/profile"
)

// Dir is where a package keeps its fixtures.
const Dir = "fixtures/"

// Limits of the fixtures of a package.
const (
	MaxFixtures = 500
	MaxBodySize = 1 << 20
)

// Vary types: how a field that changes is checked.
const (
	TypeTimestamp = "timestamp"
	TypeHTTPDate  = "http-date"
	TypeUUID      = "uuid"
	TypeInt       = "int"
	TypeSerial    = "serial"
	TypeImage     = "image"
	TypeAny       = "any"
)

var varyTypes = map[string]bool{TypeTimestamp: true, TypeHTTPDate: true, TypeUUID: true, TypeInt: true, TypeSerial: true, TypeImage: true, TypeAny: true}

// Fixture is one recording.
type Fixture struct {
	ID string `json:"id"`
	// File is the fixture file it came from, set when it is read.
	File string `json:"file,omitempty"`
	// Engine is the engine instance the request goes to; the profile's
	// first http-api instance when empty.
	Engine string `json:"engine,omitempty"`
	// Route is the route the fixture covers; its id when a route has it.
	Route    string    `json:"route,omitempty"`
	Request  *Request  `json:"request,omitempty"`
	Response *Response `json:"response,omitempty"`
	Vary     []Vary    `json:"vary,omitempty"`
	// Steps chains requests, for flows with state such as writing a
	// parameter and reading it back.
	Steps []Step `json:"steps,omitempty"`
	// Trigger raises an event; Expect is what each transport sends.
	Trigger *Trigger          `json:"trigger,omitempty"`
	Expect  map[string]Expect `json:"expect,omitempty"`
}

// Step is one request of a sequence.
type Step struct {
	Request  *Request  `json:"request"`
	Response *Response `json:"response"`
	Vary     []Vary    `json:"vary,omitempty"`
}

// Request is what a client sent the device.
type Request struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   map[string]string `json:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	// Auth answers the camera's challenge with its factory administrator;
	// false sends no credentials, to check the challenge itself.
	Auth *bool `json:"auth,omitempty"`
}

// Response is what the device answered.
type Response struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	// BodyFile holds the body, relative to the fixture file; the pipeline
	// reads it into Body.
	BodyFile string `json:"body_file,omitempty"`
}

// Vary is a field that changes from one answer to the next: located with
// a regular expression with one group (text), a path such as $.a.b[0]
// (JSON) or a header name, and checked by type.
type Vary struct {
	In    string `json:"in"` // body or header
	Regex string `json:"regex,omitempty"`
	Path  string `json:"path,omitempty"`
	Name  string `json:"name,omitempty"`
	As    string `json:"as"`
}

// Trigger is the canonical event the camera raises: on the rule named, or
// on the first rule of the camera that reports it, for the events that
// come from rules.
type Trigger struct {
	Type      string         `json:"type"`
	Rule      string         `json:"rule,omitempty"`
	Direction string         `json:"direction,omitempty"`
	Object    map[string]any `json:"object,omitempty"`
	Plate     map[string]any `json:"plate,omitempty"`
	Custom    map[string]any `json:"custom,omitempty"`
}

// Expect is what one transport sent for the event.
type Expect struct {
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Vary    []Vary            `json:"vary,omitempty"`
}

// sequence returns the requests of a fixture, one or several.
func (f *Fixture) sequence() []Step {
	if len(f.Steps) > 0 {
		return f.Steps
	}
	if f.Request == nil {
		return nil
	}
	return []Step{{Request: f.Request, Response: f.Response, Vary: f.Vary}}
}

// IsEvent reports whether the fixture raises an event.
func (f *Fixture) IsEvent() bool { return f.Trigger != nil }

// Read reads the fixtures of a package: every YAML file under fixtures/,
// holding one fixture or a list of them under "fixtures". A body_file is
// read into the body. Problems carry the file and line.
func Read(files map[string][]byte) ([]Fixture, []profile.Problem) {
	var out []Fixture
	var probs []profile.Problem
	problem := func(file string, line int, format string, args ...any) {
		probs = append(probs, profile.Problem{Step: profile.StepSelfTest, Severity: profile.SeverityError, File: file, Line: line, Message: fmt.Sprintf(format, args...)})
	}
	seen := map[string]string{}
	for _, name := range sortedNames(files) {
		if !strings.HasPrefix(name, Dir) || !(strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")) {
			continue
		}
		generic, pos, err := profile.ParseYAML(files[name], profile.DefaultLimits)
		if err != nil {
			line := 0
			var ye *profile.YAMLError
			if errors.As(err, &ye) {
				line = ye.Line
			}
			problem(name, line, "%v", err)
			continue
		}
		raw, _ := json.Marshal(generic)
		var list []Fixture
		var wrapper struct {
			Fixtures []Fixture `json:"fixtures"`
		}
		if m, ok := generic.(map[string]any); ok && m["fixtures"] != nil {
			if err := strictDecode(raw, &wrapper); err != nil {
				problem(name, 0, "invalid fixtures: %v", err)
				continue
			}
			list = wrapper.Fixtures
		} else {
			var one Fixture
			if err := strictDecode(raw, &one); err != nil {
				problem(name, 0, "invalid fixture: %v", err)
				continue
			}
			list = []Fixture{one}
		}
		for i := range list {
			f := &list[i]
			f.File = name
			ptr := ""
			if len(list) > 1 || wrapper.Fixtures != nil {
				ptr = fmt.Sprintf("/fixtures/%d", i)
			}
			for _, msg := range check(f, files, name) {
				problem(name, pos.Line(ptr), "%s", msg)
			}
			if f.ID != "" {
				if other, dup := seen[f.ID]; dup {
					problem(name, pos.Line(ptr+"/id"), "fixture %s is also in %s", f.ID, other)
				}
				seen[f.ID] = name
			}
			out = append(out, *f)
		}
	}
	if len(out) > MaxFixtures {
		problem(Dir, 0, "%d fixtures, the limit is %d", len(out), MaxFixtures)
	}
	return out, probs
}

func strictDecode(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// check validates a fixture and reads its body files.
func check(f *Fixture, files map[string][]byte, file string) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	if !idPattern.MatchString(f.ID) {
		add("a fixture needs an id of letters, digits, dots, dashes or underscores")
	}
	steps := f.sequence()
	switch {
	case f.Trigger != nil && (len(steps) > 0):
		add("fixture %s: a fixture is a request or an event, not both", f.ID)
	case f.Trigger == nil && len(steps) == 0:
		add("fixture %s: it needs a request and its response, steps, or a trigger", f.ID)
	case len(f.Steps) > 0 && (f.Request != nil || f.Response != nil):
		add("fixture %s: request and response go inside its steps", f.ID)
	}
	for i := range steps {
		st := &steps[i]
		where := f.ID
		if len(f.Steps) > 0 {
			where = fmt.Sprintf("%s, step %d", f.ID, i+1)
		}
		if st.Request == nil || st.Response == nil {
			add("fixture %s: a request needs its response", where)
			continue
		}
		if !validMethod(st.Request.Method) {
			add("fixture %s: method %q is not an HTTP method", where, st.Request.Method)
		}
		if !strings.HasPrefix(st.Request.Path, "/") {
			add("fixture %s: the path must start with /", where)
		}
		if st.Response.Status < 100 || st.Response.Status > 599 {
			add("fixture %s: status %d is not an HTTP status", where, st.Response.Status)
		}
		if st.Response.BodyFile != "" {
			if st.Response.Body != "" {
				add("fixture %s: body and body_file are exclusive", where)
			}
			p := path.Clean(path.Join(path.Dir(file), st.Response.BodyFile))
			b, ok := files[p]
			switch {
			case !ok:
				add("fixture %s: body_file %s is not in the package", where, p)
			case len(b) > MaxBodySize:
				add("fixture %s: body_file %s is larger than 1 MB", where, p)
			default:
				st.Response.Body, st.Response.BodyFile = string(b), ""
			}
		}
		out = append(out, checkVary(where, st.Vary)...)
	}
	if f.Trigger != nil {
		if f.Trigger.Type == "" {
			add("fixture %s: the trigger needs an event type", f.ID)
		}
		if len(f.Expect) == 0 {
			add("fixture %s: expect says what the event sends, by transport", f.ID)
		}
		for _, t := range sortedKeys(f.Expect) {
			out = append(out, checkVary(f.ID+", "+t, f.Expect[t].Vary)...)
		}
	}
	return out
}

func checkVary(where string, vary []Vary) []string {
	var out []string
	for _, v := range vary {
		if !varyTypes[v.As] {
			out = append(out, fmt.Sprintf("fixture %s: vary as %q: use timestamp, http-date, uuid, int, serial, image or any", where, v.As))
		}
		switch v.In {
		case "header":
			if v.Name == "" {
				out = append(out, fmt.Sprintf("fixture %s: a header vary needs its name", where))
			}
		case "body":
			n := 0
			if v.Regex != "" {
				n++
				re, err := regexp.Compile(v.Regex)
				if err != nil {
					out = append(out, fmt.Sprintf("fixture %s: vary regex %q: %v", where, v.Regex, err))
				} else if re.NumSubexp() != 1 {
					out = append(out, fmt.Sprintf("fixture %s: vary regex %q needs exactly one group", where, v.Regex))
				}
			}
			if v.Path != "" {
				n++
				if _, err := parsePath(v.Path); err != nil {
					out = append(out, fmt.Sprintf("fixture %s: vary path %q: %v", where, v.Path, err))
				}
			}
			if n > 1 {
				out = append(out, fmt.Sprintf("fixture %s: a vary has a regex or a path, not both", where))
			}
			if n == 0 && v.As != TypeImage && v.As != TypeAny {
				out = append(out, fmt.Sprintf("fixture %s: a body vary needs a regex or a path, unless the whole body is an image or anything", where))
			}
		default:
			out = append(out, fmt.Sprintf("fixture %s: vary in %q: use body or header", where, v.In))
		}
	}
	return out
}

func validMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return true
	}
	return false
}

func sortedNames(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}
