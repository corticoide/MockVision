package scraper

import (
	"fmt"
	"io/fs"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/corticoide/mockvision/profiles"
)

// Program is a read-only capture recipe (D73): a versioned list of steps,
// each a request the scraper sends a device and records. A program only
// lists safe, read-only steps; the scraper never sends anything a program
// does not name (the whitelist per action, RN-17).
type Program struct {
	ID         string     `yaml:"id" json:"id"`
	Version    string     `yaml:"version" json:"version"`
	Name       string     `yaml:"name" json:"name"`
	Compatible Compatible `yaml:"compatible" json:"compatible"`
	Steps      []Step     `yaml:"-" json:"steps"`
}

// Compatible says which devices a program suits.
type Compatible struct {
	Vendors []string `yaml:"vendors" json:"vendors,omitempty"`
}

// Step is one read-only request of a program.
type Step struct {
	ID     string            `yaml:"id" json:"id"`
	Kind   string            `yaml:"kind" json:"kind"` // http | rtsp
	Method string            `yaml:"method" json:"method,omitempty"`
	Port   int               `yaml:"port" json:"port,omitempty"`
	Path   string            `yaml:"path" json:"path,omitempty"`
	Query  map[string]string `yaml:"query" json:"query,omitempty"`
	Auth   bool              `yaml:"auth" json:"auth,omitempty"`
	Binary bool              `yaml:"binary" json:"binary,omitempty"`
	Stream string            `yaml:"stream" json:"stream,omitempty"`
	// Vary names the fixture fields that change on every answer, compared
	// by type when the profile is replayed (serial, mac, ip, timestamp).
	Vary []string `yaml:"vary" json:"vary,omitempty"`
}

type programFile struct {
	Program Program `yaml:"program"`
	Steps   []Step  `yaml:"steps"`
}

var programID = regexp.MustCompile(`^[a-z][a-z0-9-]*/[a-z][a-z0-9-]*$`)

// ParseProgram reads a program recipe and checks that every step is a
// read-only request (RN-17).
func ParseProgram(data []byte) (*Program, error) {
	var pf programFile
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&pf); err != nil {
		return nil, fmt.Errorf("program: %w", err)
	}
	p := pf.Program
	p.Steps = pf.Steps
	if !programID.MatchString(p.ID) {
		return nil, fmt.Errorf("program.id must look like vendor/name")
	}
	if p.Version == "" || p.Name == "" {
		return nil, fmt.Errorf("program needs a version and a name")
	}
	if len(p.Steps) == 0 {
		return nil, fmt.Errorf("program has no steps")
	}
	seen := map[string]bool{}
	for i := range p.Steps {
		s := &p.Steps[i]
		if s.ID == "" || seen[s.ID] {
			return nil, fmt.Errorf("each step needs a unique id")
		}
		seen[s.ID] = true
		switch s.Kind {
		case "http":
			if s.Method == "" {
				s.Method = "GET"
			}
			if !readOnlyHTTP[strings.ToUpper(s.Method)] {
				return nil, fmt.Errorf("step %s: %s is not a read-only method; a program may only read (RN-17)", s.ID, s.Method)
			}
			s.Method = strings.ToUpper(s.Method)
			if s.Port == 0 {
				s.Port = 80
			}
		case "rtsp":
			if s.Method == "" {
				s.Method = "DESCRIBE"
			}
			if s.Method != "OPTIONS" && s.Method != "DESCRIBE" {
				return nil, fmt.Errorf("step %s: RTSP %s is not read-only", s.ID, s.Method)
			}
			if s.Port == 0 {
				s.Port = 554
			}
		default:
			return nil, fmt.Errorf("step %s: unknown kind %q", s.ID, s.Kind)
		}
	}
	return &p, nil
}

// Matches says whether a program suits a device of the detected vendor.
func (p *Program) Matches(vendor string) bool {
	if len(p.Compatible.Vendors) == 0 {
		return true
	}
	for _, v := range p.Compatible.Vendors {
		if strings.EqualFold(v, vendor) {
			return true
		}
	}
	return false
}

// Builtin returns the capture programs shipped in the binary.
func Builtin() []*Program {
	var out []*Program
	entries, _ := fs.ReadDir(profiles.Programs, "programs")
	for _, e := range entries {
		data, err := profiles.Programs.ReadFile("programs/" + e.Name())
		if err != nil {
			continue
		}
		if p, err := ParseProgram(data); err == nil {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// query builds the path with a step's query string, keys in order.
func (s Step) query() string {
	if len(s.Query) == 0 {
		return s.Path
	}
	keys := make([]string, 0, len(s.Query))
	for k := range s.Query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	vals := url.Values{}
	for _, k := range keys {
		vals.Set(k, s.Query[k])
	}
	return s.Path + "?" + vals.Encode()
}
