package scraper

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// CompileInput is what the compiler needs besides the recordings: an id
// for the draft, the vendor and model to name it, and the device's own
// address and user, which are redacted out of the profile (RN-18, D77).
type CompileInput struct {
	ProfileID string
	Version   string
	Name      string
	Vendor    string
	Model     string
	Username  string
	Host      string
}

// CompileResult is a draft profile compiled from a capture: the profile
// and its recordings, ready to import. The raw artifacts never go in it.
type CompileResult struct {
	ProfileYAML  []byte   `json:"-"`
	FixturesYAML []byte   `json:"-"`
	Streams      []string `json:"streams"`
	Routes       int      `json:"routes"`
	Events       int      `json:"events"`
	Warnings     []string `json:"warnings"`
}

var (
	ipv4Re   = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	macRe    = regexp.MustCompile(`\b([0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2}\b`)
	serialRe = regexp.MustCompile(`\b[0-9A-F]{10,}\b`)
)

// sanitize removes a device's address, MAC and serial from a captured
// string so nothing identifying enters the profile (RN-18). The fields it
// redacts are declared variable in the fixtures, compared by type.
func sanitize(in CompileInput, s string) (string, map[string]bool) {
	varied := map[string]bool{}
	repl := func(re *regexp.Regexp, placeholder, kind string) {
		if re.MatchString(s) {
			s = re.ReplaceAllString(s, placeholder)
			varied[kind] = true
		}
	}
	if in.Host != "" {
		if strings.Contains(s, in.Host) {
			s = strings.ReplaceAll(s, in.Host, "0.0.0.0")
			varied["ip"] = true
		}
	}
	repl(macRe, "00:00:00:00:00:00", "mac")
	repl(ipv4Re, "0.0.0.0", "ip")
	repl(serialRe, "REDACTEDSERIAL", "serial")
	return s, varied
}

// Compile turns a capture into a draft profile and its recordings. It
// reproduces the HTTP routes and the RTSP stream it saw, declares the
// pushed event, and redacts the device's identity (D75, D77).
func Compile(in CompileInput, cap CaptureResult) (*CompileResult, error) {
	out := &CompileResult{}
	vendor := firstNonEmpty(in.Vendor, cap.Vendor, "Unknown")
	model := firstNonEmpty(in.Model, "Captured")

	// Media from the RTSP SDP.
	streamCodec, streamRes, streamPath := "h264", "1920x1080", "/stream"
	for _, f := range cap.Fixtures {
		if f.Kind == "rtsp" && f.Body != "" {
			streamCodec, streamRes = codecFromSDP(f.Body)
			if f.Path != "" {
				streamPath = f.Path
			}
			break
		}
	}
	out.Streams = []string{"main"}

	routes := []map[string]any{}
	for _, f := range cap.Fixtures {
		if f.Kind != "http" || f.Status == 0 || f.Status >= 400 || f.Error != "" {
			continue
		}
		match := map[string]any{"method": f.Method, "path": f.Path}
		if len(f.Query) > 0 {
			match["query"] = f.Query
		}
		var action map[string]any
		if f.Binary {
			action = map[string]any{"handler": "snapshot", "stream": "main"}
		} else {
			body, _ := sanitize(in, f.Body)
			ct := firstNonEmpty(f.ContentType, "application/octet-stream")
			action = map[string]any{"type": ct, "body": body}
		}
		routes = append(routes, map[string]any{"id": routeID(f.StepID), "match": match, "action": action})
	}
	out.Routes = len(routes)
	if len(routes) == 0 {
		return nil, fmt.Errorf("the capture has no HTTP responses to compile")
	}

	engines := map[string]any{
		"http": map[string]any{
			"engine": "http-api@^1", "port": 80,
			"auth":   map[string]any{"scheme": "digest", "realm": vendor},
			"routes": routes,
		},
		"rtsp": map[string]any{
			"engine": "rtsp@^1", "port": 554,
			"auth":  map[string]any{"scheme": "digest"},
			"paths": map[string]any{"main": streamPath},
		},
	}

	events := map[string]any{}
	for _, f := range cap.Fixtures {
		if f.Kind != "event" || f.Body == "" {
			continue
		}
		body, _ := sanitize(in, f.Body)
		name := firstNonEmpty(f.Stream, "event")
		events[name] = map[string]any{
			"vendor_name": title(name),
			"rule":        "none",
			"transports": map[string]any{
				"http_push": map[string]any{"method": "POST", "content_type": firstNonEmpty(f.ContentType, "application/json"), "body": body},
			},
		}
		out.Events++
	}

	prof := map[string]any{
		"schema": 1,
		"profile": map[string]any{
			"id": in.ProfileID, "version": in.Version, "name": in.Name, "vendor": vendor, "model": model,
			"firmware": []string{"captured"},
		},
		"identity": map[string]any{
			"serial": "REDACTEDSER01",
			"factory": map[string]any{
				"network": map[string]any{"ip": "192.0.2.10", "mask": "255.255.255.0", "gateway": "192.0.2.1"},
				"users":   []map[string]any{{"username": firstNonEmpty(in.Username, "admin"), "password": "REDACTED", "role": "admin"}},
			},
		},
		"media": map[string]any{
			"streams": map[string]any{
				"main": map[string]any{
					"codecs":      []string{streamCodec},
					"resolutions": []string{streamRes},
					"default":     map[string]any{"codec": streamCodec, "resolution": streamRes, "fps": 25},
				},
			},
		},
		"engines": engines,
	}
	if len(events) > 0 {
		prof["events"] = events
		engines["push"] = map[string]any{"engine": "http-push@^1"}
	}
	raw, err := yaml.Marshal(prof)
	if err != nil {
		return nil, err
	}
	out.ProfileYAML = append([]byte("# Draft profile compiled by the scraper from a read-only capture.\n"+
		"# Device address, MAC and serial were redacted (RN-18); edit and\n# verify before publishing.\n"), raw...)

	// Fixtures for the self-test: the HTTP routes, with the redacted fields
	// declared variable so they are compared by type.
	out.FixturesYAML = compileFixtures(in, cap)
	if out.Events == 0 {
		out.Warnings = append(out.Warnings, "no pushed event was captured; the profile has no events")
	}
	return out, nil
}

// compileFixtures writes the self-test fixtures of the HTTP routes.
func compileFixtures(in CompileInput, cap CaptureResult) []byte {
	var fixtures []map[string]any
	for _, f := range cap.Fixtures {
		if f.Kind != "http" || f.Binary || f.Status == 0 || f.Status >= 400 || f.Error != "" {
			continue
		}
		body, varied := sanitize(in, f.Body)
		req := map[string]any{"method": f.Method, "path": f.Path}
		if len(f.Query) > 0 {
			req["query"] = f.Query
		}
		resp := map[string]any{"status": f.Status, "body": body}
		if f.ContentType != "" {
			resp["headers"] = map[string]any{"Content-Type": f.ContentType}
		}
		fx := map[string]any{"id": routeID(f.StepID), "request": req, "response": resp}
		var vary []map[string]any
		for _, kind := range sortedKeys(varied) {
			vary = append(vary, map[string]any{"in": "body", "regex": placeholders[kind], "as": "any"})
		}
		if len(vary) > 0 {
			fx["vary"] = vary
		}
		fixtures = append(fixtures, fx)
	}
	raw, _ := yaml.Marshal(map[string]any{"fixtures": fixtures})
	return raw
}

var placeholders = map[string]string{"ip": `(0\.0\.0\.0)`, "mac": `(00:00:00:00:00:00)`, "serial": `(REDACTEDSERIAL)`}

// codecFromSDP reads the stream codec and, when present, the resolution
// from an RTSP SDP.
func codecFromSDP(sdp string) (codec, resolution string) {
	codec, resolution = "h264", "1920x1080"
	low := strings.ToLower(sdp)
	switch {
	case strings.Contains(low, "h265") || strings.Contains(low, "hevc"):
		codec = "h265"
	case strings.Contains(low, "jpeg"):
		codec = "mjpeg"
	}
	if m := regexp.MustCompile(`(\d{3,4})[x-](\d{3,4})`).FindStringSubmatch(sdp); m != nil {
		resolution = m[1] + "x" + m[2]
	}
	return codec, resolution
}

var routeIDRe = regexp.MustCompile(`[^a-z0-9-]+`)

func routeID(stepID string) string {
	id := routeIDRe.ReplaceAllString(strings.ToLower(stepID), "-")
	id = strings.Trim(id, "-")
	if id == "" {
		id = "route"
	}
	return id
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func title(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '-' || r == ' ' })
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var _ = time.Now
