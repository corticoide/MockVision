package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/media"
	"github.com/corticoide/mockvision/backend/internal/tmpl"
	"github.com/corticoide/mockvision/profiles"
	"github.com/corticoide/mockvision/sdk/engine"
)

// Import pipeline steps, as named in reports.
const (
	StepIntegrity     = "integrity"
	StepSignature     = "signature"
	StepCompatibility = "compatibility"
	StepYAML          = "yaml"
	StepSchema        = "schema"
	StepLint          = "lint"
	StepInheritance   = "inheritance"
	StepTemplates     = "templates"
	StepSelfTest      = "selftest"
)

// Severities.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// Problem is a finding of the import pipeline, with the file and line it
// refers to.
type Problem struct {
	Step     string `json:"step"`
	Severity string `json:"severity"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Pointer  string `json:"pointer,omitempty"`
	Message  string `json:"message"`
}

// EngineCatalog resolves engine references while validating.
type EngineCatalog interface {
	// Resolve returns an engine satisfying name@rng.
	Resolve(name, rng string) (engine.Engine, error)
}

// Input is a profile document and the files of its package.
type Input struct {
	// File is the name used in problems, normally profile.yaml.
	File string
	Data []byte
	// Files holds the other files of the package (templates/...); nil for a
	// loose profile.yaml.
	Files map[string][]byte
}

// Result is the outcome of Validate.
type Result struct {
	Doc *Document
	// Resolved is the document as JSON with file templates inlined, ready
	// to be stored and sent to cameras.
	Resolved  []byte
	Positions Positions
	Problems  []Problem
}

// OK reports whether validation found no errors.
func (r *Result) OK() bool {
	for _, p := range r.Problems {
		if p.Severity == SeverityError {
			return false
		}
	}
	return true
}

type validator struct {
	in      Input
	res     *Result
	engines EngineCatalog
}

func (v *validator) add(step, severity, pointer, format string, args ...any) {
	v.res.Problems = append(v.res.Problems, Problem{
		Step:     step,
		Severity: severity,
		File:     v.in.File,
		Line:     v.res.Positions.Line(pointer),
		Pointer:  pointer,
		Message:  fmt.Sprintf(format, args...),
	})
}

func (v *validator) errorf(step, pointer, format string, args ...any) {
	v.add(step, SeverityError, pointer, format, args...)
}

func (v *validator) warnf(step, pointer, format string, args ...any) {
	v.add(step, SeverityWarning, pointer, format, args...)
}

// Validate runs the document steps of the import pipeline: safe YAML read,
// compatibility, schema (base and engine schemas), lint and template
// compilation. Integrity, signature, inheritance and self-test belong to
// the package importer.
func Validate(in Input, engines EngineCatalog) *Result {
	if in.File == "" {
		in.File = "profile.yaml"
	}
	res := &Result{Positions: Positions{}}
	v := &validator{in: in, res: res, engines: engines}

	generic, pos, err := ParseYAML(in.Data, DefaultLimits)
	if err != nil {
		var ye *YAMLError
		line := 0
		if errors.As(err, &ye) {
			line = ye.Line
		}
		res.Problems = append(res.Problems, Problem{Step: StepYAML, Severity: SeverityError, File: in.File, Line: line, Message: strings.TrimPrefix(err.Error(), fmt.Sprintf("line %d: ", line))})
		return res
	}
	res.Positions = pos

	root, ok := generic.(map[string]any)
	if !ok {
		v.errorf(StepSchema, "", "the profile must be a mapping")
		return res
	}
	if s, ok := root["schema"].(json.Number); ok && s.String() != strconv.Itoa(SchemaVersion) {
		v.errorf(StepCompatibility, "/schema", "profile schema %s is not supported; this MockVision reads schema %d", s, SchemaVersion)
		return res
	}

	if !v.schema(generic) {
		return res
	}
	v.inlineTemplates(root)

	doc, err := decodeDocument(root)
	if err != nil {
		v.errorf(StepSchema, "", "cannot decode profile: %v", err)
		return res
	}
	res.Doc = doc
	delivers := v.engineSections(doc, root)
	v.lint(doc, delivers)
	v.eventTemplates(doc)

	if res.OK() {
		res.Resolved, err = json.Marshal(root)
		if err != nil {
			v.errorf(StepSchema, "", "cannot encode profile: %v", err)
		}
	}
	sort.SliceStable(res.Problems, func(i, j int) bool { return res.Problems[i].Line < res.Problems[j].Line })
	return res
}

var (
	baseSchemaOnce sync.Once
	baseSchema     *jsonschema.Schema
	baseSchemaErr  error
	printer        = message.NewPrinter(language.English)
)

func compiledBaseSchema() (*jsonschema.Schema, error) {
	baseSchemaOnce.Do(func() {
		baseSchema, baseSchemaErr = compileSchema("profile.schema.json", profiles.Schema)
	})
	return baseSchema, baseSchemaErr
}

func compileSchema(name string, raw []byte) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("schema %s: %w", name, err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	url := "mem:///" + name
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	return c.Compile(url)
}

// schemaProblems flattens a validation error into leaf messages.
func schemaProblems(err error) map[string][]string {
	out := map[string][]string{}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		out[""] = []string{err.Error()}
		return out
	}
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			ptr := ""
			for _, p := range e.InstanceLocation {
				ptr += "/" + escapePointer(p)
			}
			out[ptr] = append(out[ptr], e.ErrorKind.LocalizedString(printer))
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	return out
}

// schema validates against the published profile schema.
func (v *validator) schema(generic any) bool {
	sch, err := compiledBaseSchema()
	if err != nil {
		v.errorf(StepSchema, "", "internal error: %v", err)
		return false
	}
	if err := sch.Validate(generic); err != nil {
		probs := schemaProblems(err)
		for _, ptr := range SortedKeys(probs) {
			for _, msg := range dedupe(probs[ptr]) {
				v.errorf(StepSchema, ptr, "%s", msg)
			}
		}
		return false
	}
	return true
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// inlineTemplates replaces template file references inside engines and
// events with their content, so the resolved profile is self-contained.
func (v *validator) inlineTemplates(root map[string]any) {
	var walk func(node any, ptr string)
	walk = func(node any, ptr string) {
		switch n := node.(type) {
		case map[string]any:
			if ref, ok := n["template"].(string); ok {
				p := ptr + "/template"
				content, err := v.templateFile(ref)
				if err != nil {
					v.errorf(StepTemplates, p, "%v", err)
				} else if _, hasBody := n["body"]; !hasBody {
					n["body"] = content
				}
			}
			for _, k := range SortedKeys(n) {
				walk(n[k], ptr+"/"+escapePointer(k))
			}
		case []any:
			for i, item := range n {
				walk(item, ptr+"/"+strconv.Itoa(i))
			}
		}
	}
	walk(root["engines"], "/engines")
	walk(root["events"], "/events")
}

func (v *validator) templateFile(ref string) (string, error) {
	clean := path.Clean(ref)
	if strings.HasPrefix(clean, "../") || clean == ".." || path.IsAbs(clean) {
		return "", fmt.Errorf("template path %q must stay inside the package", ref)
	}
	if v.in.Files == nil {
		return "", fmt.Errorf("template %q: template files need a .mvpkg package; use body for inline templates in a loose profile.yaml", ref)
	}
	data, ok := v.in.Files[clean]
	if !ok {
		return "", fmt.Errorf("template %q is not in the package", ref)
	}
	if len(data) > tmpl.DefaultMaxBytes {
		return "", fmt.Errorf("template %q is larger than 1 MB", ref)
	}
	return string(data), nil
}

// engineSections resolves every engine instance, validates its section
// with the engine's schema and Validate, and returns the transports the
// profile's engines deliver.
func (v *validator) engineSections(doc *Document, root map[string]any) map[string]bool {
	delivers := map[string]bool{}
	sections, _ := root["engines"].(map[string]any)
	for _, inst := range SortedKeys(doc.Engines) {
		ptr := "/engines/" + escapePointer(inst)
		name, rng, err := EngineName(doc.Engines[inst])
		if err != nil {
			v.errorf(StepCompatibility, ptr+"/engine", "%v", err)
			continue
		}
		if v.engines == nil {
			continue
		}
		eng, err := v.engines.Resolve(name, rng)
		if err != nil {
			v.errorf(StepCompatibility, ptr+"/engine", "%v", err)
			continue
		}
		desc := eng.Describe()
		for _, t := range desc.Delivers {
			delivers[t] = true
		}
		if len(desc.ConfigSchema) > 0 {
			sch, err := compileSchema("engine-"+desc.Name+".json", desc.ConfigSchema)
			if err != nil {
				v.errorf(StepSchema, ptr, "engine %s publishes an invalid schema: %v", desc.Name, err)
				continue
			}
			if err := sch.Validate(sections[inst]); err != nil {
				probs := schemaProblems(err)
				for _, rel := range SortedKeys(probs) {
					for _, msg := range dedupe(probs[rel]) {
						v.errorf(StepSchema, ptr+rel, "%s", msg)
					}
				}
				continue
			}
		}
		for _, p := range eng.Validate(doc.Engines[inst]) {
			step := StepLint
			if p.Line > 0 {
				step = StepTemplates
			}
			pointer := ptr + p.Path
			line := v.valueLine(pointer, p.Line)
			severity := SeverityError
			if p.Warning {
				severity = SeverityWarning
			}
			v.res.Problems = append(v.res.Problems, Problem{Step: step, Severity: severity, File: v.in.File, Line: line, Pointer: pointer, Message: p.Message})
		}
	}
	return delivers
}

// valueLine maps a line inside a multi-line value to a file line.
func (v *validator) valueLine(pointer string, inner int) int {
	pos, ok := v.res.Positions[pointer]
	if !ok {
		return v.res.Positions.Line(pointer)
	}
	if inner <= 0 {
		return pos.Line
	}
	line := pos.Line + inner - 1
	if pos.Block {
		line++
	}
	return line
}

var canonicalPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^media\.(main|sub|third)\.(codec|resolution|fps|bitrate|gop)$`),
	regexp.MustCompile(`^network\.(ip|mask|gateway|dhcp|dns)$`),
	regexp.MustCompile(`^ports\.[a-z0-9-]{1,32}$`),
	regexp.MustCompile(`^users$`),
	regexp.MustCompile(`^time\.(offset|timezone|ntp)$`),
	regexp.MustCompile(`^osd\.(enabled|text)$`),
	regexp.MustCompile(`^events\.[a-z0-9_:.-]{1,64}\.enabled$`),
	regexp.MustCompile(`^sd\.(quota|overwrite)$`),
}

// ValidCanonicalKey reports whether key is a canonical binding.
func ValidCanonicalKey(key string) bool {
	for _, re := range canonicalPatterns {
		if re.MatchString(key) {
			return true
		}
	}
	return false
}

func (v *validator) lint(doc *Document, delivers map[string]bool) {
	if doc.Profile.Extends != "" {
		v.errorf(StepInheritance, "/profile/extends", "extends is not supported yet: publish the profile resolved")
	}

	// Identity.
	if err := domain.ValidateMask(doc.Identity.Serial); err != nil {
		v.errorf(StepLint, "/identity/serial", "serial %v", err)
	}
	if doc.Identity.OUI != "" {
		if _, err := domain.ParseOUI(doc.Identity.OUI); err != nil {
			v.errorf(StepLint, "/identity/oui", "%v", err)
		}
	}
	users := make([]domain.CameraUser, 0, len(doc.Identity.Factory.Users))
	for _, u := range doc.Identity.Factory.Users {
		users = append(users, domain.CameraUser{Username: u.Username, Password: u.Password, Role: u.Role})
	}
	if err := domain.ValidateCameraUsers(users); err != nil {
		v.errorf(StepLint, "/identity/factory/users", "%v", err)
	}
	if n := doc.Identity.Factory.Network; n.IP != "" || n.Mask != "" || n.Gateway != "" {
		v.lintFactoryNetwork(n)
	}

	// State.
	binds := map[string]string{}
	for _, key := range SortedKeys(doc.State) {
		p := doc.State[key]
		ptr := "/state/" + escapePointer(key)
		if p.Default == nil {
			v.errorf(StepLint, ptr, "parameter %s has no default", key)
		} else if err := CheckValue(p, p.Default); err != nil {
			v.errorf(StepLint, ptr+"/default", "default of %s: %v", key, err)
		}
		if p.Type == TypeEnum && len(p.Values) == 0 {
			v.errorf(StepLint, ptr, "enum parameter %s needs values", key)
		}
		if p.Min != nil && p.Max != nil && *p.Min > *p.Max {
			v.errorf(StepLint, ptr, "min is greater than max")
		}
		if p.Bind != "" {
			if !ValidCanonicalKey(p.Bind) {
				v.errorf(StepLint, ptr+"/bind", "unknown canonical key %q", p.Bind)
			} else if other, dup := binds[p.Bind]; dup {
				v.errorf(StepLint, ptr+"/bind", "%s is already bound to %s", p.Bind, other)
			} else {
				binds[p.Bind] = key
				v.lintBind(doc, key, p, ptr)
			}
		}
	}

	// Media.
	for _, name := range SortedKeys(doc.Media.Streams) {
		v.lintStream(name, doc.Media.Streams[name])
	}
	v.lintStreamRefs(doc)

	// Events.
	for _, typ := range SortedKeys(doc.Events) {
		ptr := "/events/" + escapePointer(typ)
		if !domain.ValidEventType(typ) {
			v.errorf(StepLint, ptr, "%q is not a canonical event type; use custom:<name> for vendor events", typ)
			continue
		}
		spec := doc.Events[typ]
		if len(spec.Transports) == 0 {
			v.warnf(StepLint, ptr, "event %s has no transport and cannot be enabled on a camera", typ)
		}
		for _, t := range SortedKeys(spec.Transports) {
			if !delivers[t] {
				v.errorf(StepLint, ptr+"/transports/"+escapePointer(t), "no engine of this profile delivers %s; add an engine instance such as push: {engine: http-push@^1}", t)
			}
		}
	}
}

func (v *validator) lintFactoryNetwork(n FactoryNetwork) {
	ip, err := netip.ParseAddr(n.IP)
	if err != nil || !ip.Is4() {
		v.errorf(StepLint, "/identity/factory/network/ip", "factory IP must be IPv4")
		return
	}
	prefix, err := domain.MaskToPrefix(n.Mask)
	if err != nil {
		v.errorf(StepLint, "/identity/factory/network/mask", "%v", err)
		return
	}
	id := domain.NetIdentity{Mode: domain.NetMacvlan, MAC: "02:00:00:00:00:01", IPMode: domain.IPStatic, IP: ip, Prefix: prefix}
	if n.Gateway != "" {
		gw, err := netip.ParseAddr(n.Gateway)
		if err != nil {
			v.errorf(StepLint, "/identity/factory/network/gateway", "invalid gateway")
			return
		}
		id.Gateway = gw
	}
	if err := id.Validate(); err != nil {
		v.errorf(StepLint, "/identity/factory/network", "%v", err)
	}
}

func (v *validator) lintBind(doc *Document, key string, p Param, ptr string) {
	parts := strings.Split(p.Bind, ".")
	if parts[0] != "media" {
		return
	}
	stream, ok := doc.Media.Streams[parts[1]]
	if !ok {
		v.errorf(StepLint, ptr+"/bind", "%s binds stream %s, which the profile does not define", key, parts[1])
		return
	}
	switch parts[2] {
	case "resolution":
		candidates := p.Values
		if len(candidates) == 0 {
			candidates = []any{p.Default}
		}
		for _, c := range candidates {
			s, _ := c.(string)
			if !contains(stream.Resolutions, s) {
				v.errorf(StepLint, ptr, "%s allows %v, which stream %s does not support", key, c, parts[1])
			}
		}
	case "fps", "bitrate", "gop":
		if p.Type != TypeInt {
			v.errorf(StepLint, ptr+"/type", "%s is bound to %s and must be an int", key, p.Bind)
		}
	case "codec":
		if p.Type != TypeEnum {
			v.errorf(StepLint, ptr+"/type", "%s is bound to %s and must be an enum of the stream's codecs", key, p.Bind)
		}
		for _, c := range p.Values {
			s, _ := c.(string)
			if !contains(stream.Codecs, s) {
				v.errorf(StepLint, ptr, "%s allows codec %v, which stream %s does not support", key, c, parts[1])
			}
		}
	}
}

// lintStreamRefs checks that the streams the built-in engines serve are
// defined under media.streams.
func (v *validator) lintStreamRefs(doc *Document) {
	for _, inst := range SortedKeys(doc.Engines) {
		name, _, err := EngineName(doc.Engines[inst])
		if err != nil {
			continue
		}
		ptr := "/engines/" + escapePointer(inst)
		switch name {
		case "rtsp":
			var cfg struct {
				Paths map[string]string `json:"paths"`
			}
			_ = json.Unmarshal(doc.Engines[inst], &cfg)
			for _, stream := range SortedKeys(cfg.Paths) {
				if _, ok := doc.Media.Streams[stream]; !ok {
					v.errorf(StepLint, ptr+"/paths/"+stream, "stream %s is served here but media.streams does not define it", stream)
				}
			}
		case "http-api":
			var cfg struct {
				Routes []struct {
					Action struct {
						Stream string `json:"stream"`
					} `json:"action"`
				} `json:"routes"`
			}
			_ = json.Unmarshal(doc.Engines[inst], &cfg)
			for i, r := range cfg.Routes {
				if s := r.Action.Stream; s != "" {
					if _, ok := doc.Media.Streams[s]; !ok {
						v.errorf(StepLint, ptr+"/routes/"+strconv.Itoa(i)+"/action/stream", "stream %s is served here but media.streams does not define it", s)
					}
				}
			}
		}
	}
}

func (v *validator) lintStream(name string, s Stream) {
	ptr := "/media/streams/" + name
	for i, r := range s.Resolutions {
		if _, err := domain.ParseResolution(r); err != nil {
			v.errorf(StepLint, ptr+"/resolutions/"+strconv.Itoa(i), "%s: %v", r, err)
		}
	}
	d := s.Default
	if !contains(s.Codecs, d.Codec) {
		v.errorf(StepLint, ptr+"/default/codec", "default codec %s is not in codecs", d.Codec)
	}
	if contains(s.Codecs, media.CodecMJPEG) {
		for i, r := range s.Resolutions {
			res, err := domain.ParseResolution(r)
			if err != nil || media.MJPEGFits(res.Width, res.Height) {
				continue
			}
			if d.Codec == media.CodecMJPEG && r == d.Resolution {
				v.errorf(StepLint, ptr+"/default/resolution", "MJPEG over RTSP carries at most %dx%d in multiples of 8; the default %s does not fit", media.MaxMJPEGSize, media.MaxMJPEGSize, r)
				continue
			}
			v.warnf(StepLint, ptr+"/resolutions/"+strconv.Itoa(i), "%s cannot be streamed as MJPEG (at most %dx%d in multiples of 8); choosing both fails", r, media.MaxMJPEGSize, media.MaxMJPEGSize)
		}
	}
	if !contains(s.Resolutions, d.Resolution) {
		v.errorf(StepLint, ptr+"/default/resolution", "default resolution %s is not in resolutions", d.Resolution)
	}
	if s.FPS != nil && (d.FPS < s.FPS.Min || (s.FPS.Max > 0 && d.FPS > s.FPS.Max)) {
		v.errorf(StepLint, ptr+"/default/fps", "default fps %d is outside %d-%d", d.FPS, s.FPS.Min, s.FPS.Max)
	}
	if s.Bitrate != nil && d.Bitrate > 0 && (d.Bitrate < s.Bitrate.Min || (s.Bitrate.Max > 0 && d.Bitrate > s.Bitrate.Max)) {
		v.errorf(StepLint, ptr+"/default/bitrate", "default bitrate %d is outside %d-%d", d.Bitrate, s.Bitrate.Min, s.Bitrate.Max)
	}
}

// eventTemplates compiles the http_push templates of every event.
func (v *validator) eventTemplates(doc *Document) {
	for _, typ := range SortedKeys(doc.Events) {
		raw, ok := doc.Events[typ].Transports["http_push"]
		if !ok {
			continue
		}
		var hp HTTPPush
		if err := json.Unmarshal(raw, &hp); err != nil {
			continue
		}
		ptr := "/events/" + escapePointer(typ) + "/transports/http_push/body"
		if hp.Template != "" {
			ptr = "/events/" + escapePointer(typ) + "/transports/http_push/template"
		}
		if err := tmpl.Check(typ, hp.Body); err != nil {
			var se *tmpl.SyntaxError
			inner := 0
			msg := err.Error()
			if errors.As(err, &se) {
				inner, msg = se.Line, se.Message
			}
			line := v.valueLine(ptr, inner)
			if hp.Template != "" {
				v.res.Problems = append(v.res.Problems, Problem{Step: StepTemplates, Severity: SeverityError, File: hp.Template, Line: inner, Pointer: ptr, Message: msg})
				continue
			}
			v.res.Problems = append(v.res.Problems, Problem{Step: StepTemplates, Severity: SeverityError, File: v.in.File, Line: line, Pointer: ptr, Message: msg})
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
