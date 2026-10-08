package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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
	// Parent is the installed profile the document extends, if it does.
	Parent *Parent
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

// warnf reports a warning; only the lint step has them.
func (v *validator) warnf(pointer, format string, args ...any) {
	v.add(StepLint, SeverityWarning, pointer, format, args...)
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

	// A profile that extends another is validated merged with it: its own
	// template files are inlined first, the parent's are already.
	_, removes := root["remove"]
	meta, _ := root["profile"].(map[string]any)
	inherits := removes || meta["extends"] != nil
	if !inherits && meta != nil {
		delete(meta, "lineage") // only the importer writes it
	}
	if inherits {
		v.inlineTemplates(root)
		merged, ok := v.inherit(root)
		if !ok {
			return res
		}
		root, generic = merged, merged
	}
	if !v.schema(generic) {
		return res
	}
	if !inherits {
		v.inlineTemplates(root)
	}

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
			// Events stream only through a route that serves them.
			if t == engine.TransportAttach && !servesAttach(doc.Engines[inst]) {
				continue
			}
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
	regexp.MustCompile(`^media\.(main|sub|third)\.(codec|resolution|width|height|fps|bitrate|gop)$`),
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
		switch {
		case p.DefaultFrom != "":
			if p.Type != TypeString {
				v.errorf(StepLint, ptr+"/default_from", "%s takes its default from the identity and must be a string", key)
			}
			if !contains(DefaultSources, p.DefaultFrom) {
				v.errorf(StepLint, ptr+"/default_from", "unknown identity field %q", p.DefaultFrom)
			}
		case p.Default == nil:
			v.errorf(StepLint, ptr, "parameter %s has no default", key)
		default:
			if err := CheckValue(p, p.Default); err != nil {
				v.errorf(StepLint, ptr+"/default", "default of %s: %v", key, err)
			}
		}
		if len(p.Map) > 0 {
			if p.Bind == "" {
				v.errorf(StepLint, ptr+"/map", "map translates the values of a bound parameter; %s has no bind", key)
			}
			for _, mk := range SortedKeys(p.Map) {
				if p.Type == TypeEnum && !slices.ContainsFunc(p.Values, func(o any) bool { return sameValue(o, mk) }) {
					v.errorf(StepLint, ptr+"/map/"+escapePointer(mk), "%s is not one of the values of %s", mk, key)
				}
			}
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
			v.warnf(ptr, "event %s has no transport and cannot be enabled on a camera", typ)
		}
		for _, t := range SortedKeys(spec.Transports) {
			switch {
			case delivers[t]:
			case t == engine.TransportAttach:
				v.errorf(StepLint, ptr+"/transports/"+escapePointer(t), "no route of the HTTP API streams events: add one with handler: events.attach")
			default:
				v.errorf(StepLint, ptr+"/transports/"+escapePointer(t), "no engine of this profile delivers %s; add an engine instance such as push: {engine: http-push@^1}", t)
			}
		}
		if spec.Record != nil {
			v.lintRecord(doc, typ, *spec.Record, ptr+"/record")
		}
	}
	v.lintVCA(doc)
}

// lintRecord checks what an event records: a stream of the model that can
// be clipped, a clip of 5 minutes at most, and somewhere to keep them.
func (v *validator) lintRecord(doc *Document, typ string, rec RecordSpec, ptr string) {
	stream := rec.Stream
	if stream == "" {
		stream = "main"
	}
	st, ok := doc.Media.Streams[stream]
	if !ok {
		v.errorf(StepLint, ptr+"/stream", "the model has no %s stream", stream)
	}
	d := rec.Clip.D()
	switch {
	case d < 0 || d > media.MaxClip:
		v.errorf(StepLint, ptr+"/clip", "a clip lasts at most %s", media.MaxClip)
	case d > 0 && d < time.Second:
		v.errorf(StepLint, ptr+"/clip", "a clip lasts at least 1s")
	case d > 0 && ok && st.Default.Codec == "mjpeg":
		v.warnf(ptr+"/clip", "%s is MJPEG by default: its clips are only recorded while it runs H.264 or H.265", stream)
	}
	if !rec.Snapshot && d == 0 {
		v.warnf(ptr, "%s records nothing: set snapshot or clip", typ)
	}
	if doc.MaxSDMB() == 0 && len(doc.NASProtocols()) == 0 {
		v.warnf(ptr, "%s records, but the model has no storage: declare storage.sd or storage.nas", typ)
	}
}

// lintVCA checks that the analytics and their events agree: each kind of
// rule raises some event, the events that come from a kind of rule have it,
// reports come from no rule, and the factory rules are valid rules of the
// profile.
func (v *validator) lintVCA(doc *Document) {
	var kinds []string
	if doc.VCA != nil {
		kinds = doc.VCA.Rules
	}
	caps := doc.VCACaps()
	for _, kind := range kinds {
		if len(caps.RuleEvents(domain.RuleType(kind))) == 0 {
			canonical := "line_crossing"
			if kind == string(domain.RuleRegion) {
				canonical = "region_entrance, region_exit, loitering or intrusion"
			}
			v.warnf("/vca/rules", "%s rules raise no event: define %s, or an event with rule: %s", kind, canonical, kind)
		}
	}
	for _, typ := range SortedKeys(doc.Events) {
		spec, ptr := doc.Events[typ], "/events/"+escapePointer(typ)
		if spec.Report && (spec.Rule == "line" || spec.Rule == "region") {
			v.errorf(StepLint, ptr+"/rule", "%s is a report and comes from no rule", typ)
			continue
		}
		if kind := spec.RuleType(typ); kind != "" && !contains(kinds, string(kind)) {
			v.warnf(ptr, "%s events come from %s rules, which vca.rules lacks: cameras cannot raise them; add %s to vca.rules", typ, kind, kind)
		}
	}
	if doc.VCA == nil || len(doc.VCA.FactoryRules) == 0 {
		return
	}
	var verr *domain.ValidationError
	if err := domain.ValidateRules(doc.FactoryRules(), caps); errors.As(err, &verr) {
		for _, f := range verr.Fields {
			// rules[2].points -> /vca/factory_rules/2/points
			ptr := "/vca/factory_rules"
			if rest, ok := strings.CutPrefix(f.Field, "rules["); ok {
				if i, tail, ok := strings.Cut(rest, "]"); ok {
					ptr += "/" + i + strings.ReplaceAll(tail, ".", "/")
				}
			}
			v.errorf(StepLint, ptr, "%s", f.Message)
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
	if parts[0] == "events" {
		// events.<type>.enabled switches the camera's analytics for a type.
		typ := strings.TrimSuffix(strings.TrimPrefix(p.Bind, "events."), ".enabled")
		if p.Type != TypeBool {
			v.errorf(StepLint, ptr+"/type", "%s is bound to %s and must be a bool", key, p.Bind)
		}
		if _, ok := doc.Events[typ]; !ok {
			v.warnf(ptr+"/bind", "%s switches %s events, which the profile does not define", key, typ)
		}
		return
	}
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
			s := fmt.Sprint(canonValue(p, c))
			if !contains(stream.Resolutions, s) {
				v.errorf(StepLint, ptr, "%s allows %v, which stream %s does not support", key, c, parts[1])
			}
		}
	case "width", "height":
		// A resolution split in two parameters needs both, and their
		// defaults make one the stream supports.
		other := map[string]string{"width": "height", "height": "width"}[parts[2]]
		var pair *Param
		for _, q := range doc.State {
			if q.Bind == "media."+parts[1]+"."+other {
				pair = &q
			}
		}
		if pair == nil {
			v.errorf(StepLint, ptr+"/bind", "%s splits the resolution of %s: bind another parameter to media.%s.%s", key, parts[1], parts[1], other)
			return
		}
		if p.Type != TypeInt && p.Type != TypeEnum {
			v.errorf(StepLint, ptr+"/type", "%s is bound to %s and must be an int or an enum", key, p.Bind)
		}
		if parts[2] == "width" {
			res := fmt.Sprintf("%vx%v", canonValue(p, p.Default), canonValue(*pair, pair.Default))
			if !contains(stream.Resolutions, res) {
				v.errorf(StepLint, ptr+"/default", "the defaults make %s, which stream %s does not support", res, parts[1])
			}
		}
		for _, q := range doc.State {
			if q.Bind == "media."+parts[1]+".resolution" {
				v.errorf(StepLint, ptr+"/bind", "stream %s binds its resolution whole and split", parts[1])
				break
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
			s := fmt.Sprint(canonValue(p, c))
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
				Auth struct {
					FailureEvent string `json:"failure_event"`
				} `json:"auth"`
				Routes []struct {
					Action struct {
						Stream string `json:"stream"`
					} `json:"action"`
				} `json:"routes"`
			}
			_ = json.Unmarshal(doc.Engines[inst], &cfg)
			if ev := cfg.Auth.FailureEvent; ev != "" {
				if _, ok := doc.Events[ev]; !ok {
					v.errorf(StepLint, ptr+"/auth/failure_event", "the profile does not define %s events", ev)
				}
			}
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
			v.warnf(ptr+"/resolutions/"+strconv.Itoa(i), "%s cannot be streamed as MJPEG (at most %dx%d in multiples of 8); choosing both fails", r, media.MaxMJPEGSize, media.MaxMJPEGSize)
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

// servesAttach reports whether an engine section has a route that streams
// events, as the HTTP API's events.attach.
// RouteIDs lists the route ids of an engine section, in order.
func RouteIDs(section json.RawMessage) []string {
	var cfg struct {
		Routes []struct {
			ID string `json:"id"`
		} `json:"routes"`
	}
	_ = json.Unmarshal(section, &cfg)
	out := make([]string, 0, len(cfg.Routes))
	for _, r := range cfg.Routes {
		if r.ID != "" {
			out = append(out, r.ID)
		}
	}
	return out
}

func servesAttach(section json.RawMessage) bool {
	var cfg struct {
		Routes []struct {
			Action struct {
				Handler string `json:"handler"`
			} `json:"action"`
		} `json:"routes"`
	}
	_ = json.Unmarshal(section, &cfg)
	for _, r := range cfg.Routes {
		if r.Action.Handler == "events.attach" {
			return true
		}
	}
	return false
}

// transportTemplates lists the fields of each transport that hold a
// template.
var transportTemplates = map[string][]string{
	"http_push": {"body"},
	"mqtt":      {"topic", "body"},
	"ftp":       {"path", "file", "body"},
	"smtp":      {"subject", "body", "attachment"},
	"attach":    {"body"},
}

// eventTemplates compiles the templates of every event's transports and
// checks the streams their snapshots come from.
func (v *validator) eventTemplates(doc *Document) {
	for _, typ := range SortedKeys(doc.Events) {
		for _, transport := range SortedKeys(doc.Events[typ].Transports) {
			fields, known := transportTemplates[transport]
			if !known {
				continue
			}
			var section map[string]any
			if err := json.Unmarshal(doc.Events[typ].Transports[transport], &section); err != nil {
				continue
			}
			base := "/events/" + escapePointer(typ) + "/transports/" + escapePointer(transport)
			file, _ := section["template"].(string)
			for _, field := range fields {
				text, ok := section[field].(string)
				if !ok {
					continue
				}
				v.checkTemplate(typ, base+"/"+field, text, field == "body" && file != "", file)
			}
			if stream, ok := section["stream"].(string); ok {
				if _, exists := doc.Media.Streams[stream]; !exists {
					v.errorf(StepLint, base+"/stream", "the profile has no stream %q", stream)
				}
			}
			if transport == "ftp" && section["content"] == "body" && section["body"] == nil {
				v.errorf(StepLint, base, "content: body needs body or template")
			}
			if stop, ok := section["stop"].(map[string]any); ok && transport == "attach" {
				if text, ok := stop["body"].(string); ok {
					v.checkTemplate(typ, base+"/stop/body", text, false, "")
				}
			}
		}
	}
}

// checkTemplate compiles a template of the profile, reporting errors on
// the line of the profile file, or of the package's template file the
// body came from.
func (v *validator) checkTemplate(name, ptr, text string, fromFile bool, file string) {
	err := tmpl.Check(name, text)
	if err == nil {
		return
	}
	var se *tmpl.SyntaxError
	inner := 0
	msg := err.Error()
	if errors.As(err, &se) {
		inner, msg = se.Line, se.Message
	}
	if fromFile {
		ptr = strings.TrimSuffix(ptr, "/body") + "/template"
		v.res.Problems = append(v.res.Problems, Problem{Step: StepTemplates, Severity: SeverityError, File: file, Line: inner, Pointer: ptr, Message: msg})
		return
	}
	v.res.Problems = append(v.res.Problems, Problem{Step: StepTemplates, Severity: SeverityError, File: v.in.File, Line: v.valueLine(ptr, inner), Pointer: ptr, Message: msg})
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
