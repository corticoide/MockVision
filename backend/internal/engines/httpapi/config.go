package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/corticoide/mockvision/backend/internal/tmpl"
	"github.com/corticoide/mockvision/sdk/engine"
)

// Config is the profile section of an http-api instance.
type Config struct {
	Engine  string  `json:"engine"`
	Port    int     `json:"port,omitempty"`
	Auth    Auth    `json:"auth"`
	Server  string  `json:"server,omitempty"`
	Routes  []Route `json:"routes"`
	Unknown *Action `json:"unknown,omitempty"`
}

// Auth selects the authentication scheme.
type Auth struct {
	Scheme string `json:"scheme"`
	Realm  string `json:"realm,omitempty"`
}

// Route maps a request to an action; the first match in declared order wins.
type Route struct {
	ID     string `json:"id"`
	Match  Match  `json:"match"`
	Action Action `json:"action"`
}

// Match selects requests. Query and header values are exact, "~regex" for a
// regular expression or "*" for presence.
type Match struct {
	Method  string            `json:"method,omitempty"`
	Path    string            `json:"path"`
	Query   map[string]string `json:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Action builds the response: a template (body), a status, or a handler
// followed by a template (then).
type Action struct {
	Status   int               `json:"status,omitempty"`
	Type     string            `json:"type,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Body     string            `json:"body,omitempty"`
	Template string            `json:"template,omitempty"`
	Fixture  string            `json:"fixture,omitempty"`
	Handler  string            `json:"handler,omitempty"`
	From     string            `json:"from,omitempty"`
	Key      string            `json:"key,omitempty"`
	Stream   string            `json:"stream,omitempty"`
	Then     *Action           `json:"then,omitempty"`
}

// Handlers available in this version.
const (
	HandlerStateGet = "state.get"
	HandlerStateSet = "state.set"
	HandlerSnapshot = "snapshot"
)

var plannedHandlers = map[string]bool{
	"stream.attach": true, "sd.search": true, "sd.download": true, "reboot": true, "factory_reset": true,
}

var methodPattern = regexp.MustCompile(`^[A-Z]{3,10}$`)

// configSchema is published through Describe; the importer validates the
// instance section with it.
const configSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["engine", "routes"],
  "properties": {
    "engine": {"type": "string"},
    "port": {"type": "integer", "minimum": 1, "maximum": 65535},
    "auth": {
      "type": "object",
      "additionalProperties": false,
      "required": ["scheme"],
      "properties": {
        "scheme": {"enum": ["digest", "basic", "none"]},
        "realm": {"type": "string", "maxLength": 64}
      }
    },
    "server": {"type": "string", "maxLength": 128},
    "routes": {"type": "array", "items": {"$ref": "#/$defs/route"}},
    "unknown": {"$ref": "#/$defs/action"}
  },
  "$defs": {
    "route": {
      "type": "object",
      "additionalProperties": false,
      "required": ["id", "match", "action"],
      "properties": {
        "id": {"type": "string", "pattern": "^[A-Za-z0-9._-]{1,64}$"},
        "match": {
          "type": "object",
          "additionalProperties": false,
          "required": ["path"],
          "properties": {
            "method": {"type": "string"},
            "path": {"type": "string", "pattern": "^/"},
            "query": {"type": "object", "additionalProperties": {"type": "string"}},
            "headers": {"type": "object", "additionalProperties": {"type": "string"}}
          }
        },
        "action": {"$ref": "#/$defs/action"}
      }
    },
    "action": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "status": {"type": "integer", "minimum": 100, "maximum": 599},
        "type": {"type": "string"},
        "headers": {"type": "object", "additionalProperties": {"type": "string"}},
        "body": {"type": "string"},
        "template": {"type": "string"},
        "fixture": {"type": "string"},
        "handler": {"type": "string"},
        "from": {"enum": ["query", "form", "json"]},
        "key": {"type": "string"},
        "stream": {"enum": ["main", "sub", "third"]},
        "then": {"$ref": "#/$defs/action"}
      }
    }
  }
}`

func parseConfig(raw json.RawMessage) (*Config, error) {
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if c.Auth.Scheme == "" {
		c.Auth.Scheme = SchemeDigest
	}
	return &c, nil
}

// validate checks the section and compiles its templates.
func validate(raw json.RawMessage) []engine.Problem {
	c, err := parseConfig(raw)
	if err != nil {
		return []engine.Problem{{Message: err.Error()}}
	}
	var probs []engine.Problem
	add := func(path, format string, args ...any) {
		probs = append(probs, engine.Problem{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	switch c.Auth.Scheme {
	case SchemeDigest, SchemeBasic, SchemeNone:
	default:
		add("/auth/scheme", "unknown scheme %q", c.Auth.Scheme)
	}
	ids := map[string]int{}
	matches := map[string]string{}
	for i, r := range c.Routes {
		base := "/routes/" + strconv.Itoa(i)
		if prev, dup := ids[r.ID]; dup {
			add(base+"/id", "route id %q is duplicated (first used by route %d)", r.ID, prev)
		}
		ids[r.ID] = i
		if r.Match.Method != "" && !methodPattern.MatchString(r.Match.Method) {
			add(base+"/match/method", "invalid method %q", r.Match.Method)
		}
		for k, v := range r.Match.Query {
			if err := checkMatcher(v); err != nil {
				add(base+"/match/query/"+k, "%v", err)
			}
		}
		for k, v := range r.Match.Headers {
			if err := checkMatcher(v); err != nil {
				add(base+"/match/headers/"+k, "%v", err)
			}
		}
		key := matchKey(r.Match)
		if other, dup := matches[key]; dup {
			probs = append(probs, engine.Problem{Path: base + "/match", Message: fmt.Sprintf("route %s can never match: route %s has the same match", r.ID, other), Warning: true})
		} else {
			matches[key] = r.ID
		}
		probs = append(probs, validateAction(base+"/action", r.ID, r.Action, false)...)
	}
	if c.Unknown != nil {
		if c.Unknown.Handler != "" {
			add("/unknown/handler", "unknown requests cannot use a handler")
		}
		probs = append(probs, validateAction("/unknown", "unknown", *c.Unknown, false)...)
	}
	return probs
}

func validateAction(path, name string, a Action, nested bool) []engine.Problem {
	var probs []engine.Problem
	add := func(p, format string, args ...any) {
		probs = append(probs, engine.Problem{Path: p, Message: fmt.Sprintf(format, args...)})
	}
	if a.Fixture != "" {
		add(path+"/fixture", "fixtures are not supported yet; use body")
	}
	if a.Handler != "" {
		if nested {
			add(path+"/handler", "a then action cannot run another handler")
		}
		switch a.Handler {
		case HandlerStateGet, HandlerStateSet, HandlerSnapshot:
		default:
			if plannedHandlers[a.Handler] {
				add(path+"/handler", "handler %s is not available in this version", a.Handler)
			} else {
				add(path+"/handler", "unknown handler %q", a.Handler)
			}
		}
		if a.Body != "" {
			add(path+"/body", "an action with a handler renders its response in then")
		}
		if a.Handler == HandlerStateSet && a.From == "" {
			add(path+"/from", "state.set needs from: query, form or json")
		}
		if a.Then != nil {
			probs = append(probs, validateAction(path+"/then", name, *a.Then, true)...)
		}
	} else if a.Then != nil {
		add(path+"/then", "then is only valid after a handler")
	}
	if a.Body != "" {
		if err := tmpl.Check(name, a.Body); err != nil {
			var se *tmpl.SyntaxError
			p := engine.Problem{Path: path + "/body", Message: err.Error()}
			if errors.As(err, &se) {
				p.Line = se.Line
				p.Message = se.Message
				if p.Line == 0 {
					p.Line = 1
				}
			}
			probs = append(probs, p)
		}
	}
	return probs
}

func checkMatcher(v string) error {
	if re, ok := strings.CutPrefix(v, "~"); ok {
		if _, err := regexp.Compile(re); err != nil {
			return fmt.Errorf("invalid regular expression: %v", err)
		}
	}
	return nil
}

func matchKey(m Match) string {
	var b strings.Builder
	b.WriteString(strings.ToUpper(m.Method) + " " + m.Path)
	for _, k := range sortedKeys(m.Query) {
		b.WriteString("&" + k + "=" + m.Query[k])
	}
	for _, k := range sortedKeys(m.Headers) {
		b.WriteString("|" + strings.ToLower(k) + "=" + m.Headers[k])
	}
	return b.String()
}
