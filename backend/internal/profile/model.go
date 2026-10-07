package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// CheckValue validates a value, already of the right Go type, against a
// parameter definition.
func CheckValue(p Param, v any) error {
	_, err := Coerce(p, v)
	return err
}

// Coerce converts v to the parameter's type and validates it. It accepts
// JSON values and the strings that arrive in query strings, so a client can
// write Image.Brightness=70 through the emulated API.
func Coerce(p Param, v any) (any, error) {
	switch p.Type {
	case TypeString:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("must be a string")
		}
		if p.MaxLength > 0 && utf8.RuneCountInString(s) > p.MaxLength {
			return nil, fmt.Errorf("must be at most %d characters", p.MaxLength)
		}
		if strings.ContainsAny(s, "\x00\r\n") {
			return nil, fmt.Errorf("must not contain control characters")
		}
		return s, nil
	case TypeInt:
		i, err := toInt(v)
		if err != nil {
			return nil, err
		}
		if err := checkRange(p, float64(i)); err != nil {
			return nil, err
		}
		return i, nil
	case TypeFloat:
		f, err := toFloat(v)
		if err != nil {
			return nil, err
		}
		if err := checkRange(p, f); err != nil {
			return nil, err
		}
		return f, nil
	case TypeBool:
		return toBool(v)
	case TypeEnum:
		for _, allowed := range p.Values {
			if sameValue(allowed, v) {
				return allowed, nil
			}
		}
		opts := make([]string, len(p.Values))
		for i, a := range p.Values {
			opts[i] = fmt.Sprint(a)
		}
		return nil, fmt.Errorf("must be one of %s", strings.Join(opts, ", "))
	}
	return nil, fmt.Errorf("unknown type %q", p.Type)
}

func checkRange(p Param, f float64) error {
	if p.Min != nil && f < *p.Min {
		return fmt.Errorf("must be at least %s", strconv.FormatFloat(*p.Min, 'f', -1, 64))
	}
	if p.Max != nil && f > *p.Max {
		return fmt.Errorf("must be at most %s", strconv.FormatFloat(*p.Max, 'f', -1, 64))
	}
	return nil
}

func toInt(v any) (int64, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case int:
		return int64(x), nil
	case float64:
		if x != math.Trunc(x) || math.Abs(x) > 1<<53 {
			return 0, fmt.Errorf("must be an integer")
		}
		return int64(x), nil
	case json.Number:
		return toInt(string(x))
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("must be an integer")
		}
		return i, nil
	}
	return 0, fmt.Errorf("must be an integer")
}

func toFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case int64:
		return float64(x), nil
	case int:
		return float64(x), nil
	case json.Number:
		return toFloat(string(x))
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, fmt.Errorf("must be a number")
		}
		return f, nil
	}
	return 0, fmt.Errorf("must be a number")
}

func toBool(v any) (bool, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "1", "on", "yes", "enable", "enabled":
			return true, nil
		case "false", "0", "off", "no", "disable", "disabled":
			return false, nil
		}
	case int64:
		return x != 0, nil
	}
	return false, fmt.Errorf("must be true or false")
}

// sameValue compares an enum option with an input, which may come as a
// string from a query string.
func sameValue(option, v any) bool {
	return fmt.Sprint(option) == fmt.Sprint(v)
}

// Model is a profile compiled for a running camera.
type Model struct {
	Doc   *Document
	binds map[string]string // canonical key -> native key
}

// ErrNotBound is returned for a canonical key no parameter is bound to.
var ErrNotBound = errors.New("no parameter is bound to it")

// CameraIdentity is what a camera's defaults may be taken from.
type CameraIdentity struct {
	Serial, Name, Model, MAC, IP, Firmware string
}

func (id CameraIdentity) field(name string) string {
	switch name {
	case "serial":
		return id.Serial
	case "name":
		return id.Name
	case "model":
		return id.Model
	case "mac":
		return id.MAC
	case "ip":
		return id.IP
	case "firmware":
		return id.Firmware
	}
	return ""
}

// NewModel indexes a document's bindings.
func NewModel(doc *Document) *Model {
	m := &Model{Doc: doc, binds: map[string]string{}}
	for key, p := range doc.State {
		if p.Bind != "" {
			m.binds[p.Bind] = key
		}
	}
	return m
}

// NativeFor returns the native parameter bound to a canonical key.
func (m *Model) NativeFor(canonical string) (string, bool) {
	k, ok := m.binds[canonical]
	return k, ok
}

// Defaults returns the default value of every parameter; those taken from
// the identity are empty.
func (m *Model) Defaults() map[string]any {
	out := make(map[string]any, len(m.Doc.State))
	for k, p := range m.Doc.State {
		out[k] = p.Default
		if p.DefaultFrom != "" && p.Default == nil {
			out[k] = ""
		}
	}
	return out
}

// DefaultsFor returns the defaults of a camera: those taken from its
// identity are its own, cut to the parameter's length.
func (m *Model) DefaultsFor(id CameraIdentity) map[string]any {
	out := m.Defaults()
	for k, p := range m.Doc.State {
		if p.DefaultFrom == "" {
			continue
		}
		v := id.field(p.DefaultFrom)
		if p.MaxLength > 0 && utf8.RuneCountInString(v) > p.MaxLength {
			v = string([]rune(v)[:p.MaxLength])
		}
		out[k] = v
	}
	return out
}

// canonValue translates a vendor's value to the canonical one.
func canonValue(p Param, v any) any {
	if c, ok := p.Map[fmt.Sprint(v)]; ok {
		return c
	}
	return v
}

// nativeValue translates a canonical value to the vendor's: the first of
// the enum's values, or of the map's sorted keys, that maps to it.
func nativeValue(p Param, v any) any {
	if len(p.Map) == 0 {
		return v
	}
	keys := make([]string, 0, len(p.Map))
	for _, opt := range p.Values {
		keys = append(keys, fmt.Sprint(opt))
	}
	if len(keys) == 0 {
		for k := range p.Map {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	for _, k := range keys {
		if c, ok := p.Map[k]; ok && fmt.Sprint(c) == fmt.Sprint(v) {
			return k
		}
	}
	return v
}

// splitResolution reports whether a stream's resolution is split in a
// width and a height parameter.
func (m *Model) splitResolution(stream string) (width, height string, ok bool) {
	width, okW := m.binds["media."+stream+".width"]
	height, okH := m.binds["media."+stream+".height"]
	return width, height, okW && okH
}

// Assign writes a canonical value through the parameters bound to it: the
// value is translated to the vendor's, and a resolution goes to the width
// and height parameters when the profile splits it. It returns the native
// values to store, checked.
func (m *Model) Assign(canon string, v any) (map[string]any, error) {
	if native, ok := m.binds[canon]; ok {
		p := m.Doc.State[native]
		cv, err := Coerce(p, nativeValue(p, v))
		if err != nil {
			return nil, err
		}
		return map[string]any{native: cv}, nil
	}
	parts := strings.Split(canon, ".")
	if len(parts) == 3 && parts[0] == "media" && parts[2] == "resolution" {
		if wk, hk, ok := m.splitResolution(parts[1]); ok {
			w, h, cut := strings.Cut(fmt.Sprint(v), "x")
			if !cut {
				return nil, fmt.Errorf("must look like 1920x1080")
			}
			out := map[string]any{}
			for key, raw := range map[string]string{wk: w, hk: h} {
				p := m.Doc.State[key]
				cv, err := Coerce(p, nativeValue(p, raw))
				if err != nil {
					return nil, fmt.Errorf("%s: %w", key, err)
				}
				out[key] = cv
			}
			return out, nil
		}
	}
	return nil, ErrNotBound
}

// Canon resolves a canonical key from native values, translated to its
// canonical value, falling back to the stream defaults of the profile for
// media keys without a parameter. A resolution split in width and height
// joins them.
func (m *Model) Canon(key string, values map[string]any) (any, bool) {
	if native, ok := m.binds[key]; ok {
		v, ok := values[native]
		if !ok {
			v = m.Doc.State[native].Default
		}
		return canonValue(m.Doc.State[native], v), true
	}
	parts := strings.Split(key, ".")
	if len(parts) == 3 && parts[0] == "media" && parts[2] == "resolution" {
		if _, _, ok := m.splitResolution(parts[1]); ok {
			w, _ := m.Canon("media."+parts[1]+".width", values)
			h, _ := m.Canon("media."+parts[1]+".height", values)
			return fmt.Sprintf("%vx%v", w, h), true
		}
	}
	if len(parts) == 3 && parts[0] == "media" {
		s, ok := m.Doc.Media.Streams[parts[1]]
		if !ok {
			return nil, false
		}
		switch parts[2] {
		case "codec":
			return s.Default.Codec, true
		case "resolution":
			return s.Default.Resolution, true
		case "fps":
			return int64(s.Default.FPS), true
		case "bitrate":
			return int64(s.Default.Bitrate), true
		case "gop":
			return int64(s.Default.GOP), true
		}
	}
	return nil, false
}

// CheckStreams checks that the streams the changed parameters drive hold
// together with them: a codec the stream has, a width and a height it
// supports. It returns the problem of each changed key, none when fine.
func (m *Model) CheckStreams(values map[string]any, changed []string) map[string]string {
	problems := map[string]string{}
	for _, key := range changed {
		parts := strings.Split(m.Doc.State[key].Bind, ".")
		if len(parts) != 3 || parts[0] != "media" {
			continue
		}
		if _, err := m.StreamFor(parts[1], values); err != nil {
			problems[key] = err.Error()
		}
	}
	return problems
}

// StreamSettings are the effective settings of a stream.
type StreamSettings struct {
	Codec   string
	Width   int
	Height  int
	FPS     int
	GOP     int
	Bitrate int
}

// StreamFor computes a stream's effective settings from native values.
func (m *Model) StreamFor(stream string, values map[string]any) (StreamSettings, error) {
	s, ok := m.Doc.Media.Streams[stream]
	if !ok {
		return StreamSettings{}, fmt.Errorf("profile has no stream %q", stream)
	}
	get := func(field string) any {
		v, _ := m.Canon("media."+stream+"."+field, values)
		return v
	}
	out := StreamSettings{Codec: fmt.Sprint(get("codec"))}
	res := fmt.Sprint(get("resolution"))
	w, h, ok := strings.Cut(res, "x")
	if !ok {
		return out, fmt.Errorf("invalid resolution %q", res)
	}
	var err error
	if out.Width, err = strconv.Atoi(w); err != nil {
		return out, fmt.Errorf("invalid resolution %q", res)
	}
	if out.Height, err = strconv.Atoi(h); err != nil {
		return out, fmt.Errorf("invalid resolution %q", res)
	}
	fps, _ := toInt(get("fps"))
	out.FPS = int(fps)
	if out.FPS <= 0 {
		out.FPS = 15
	}
	bitrate, _ := toInt(get("bitrate"))
	out.Bitrate = int(bitrate)
	if out.Bitrate <= 0 {
		out.Bitrate = 2048
	}
	gop, _ := toInt(get("gop"))
	out.GOP = int(gop)
	if out.GOP <= 0 {
		out.GOP = 2 * out.FPS
	}
	if s.Default.Codec == "" {
		out.Codec = "h264"
	}
	if !contains(s.Codecs, out.Codec) {
		return out, fmt.Errorf("stream %s does not support codec %s", stream, out.Codec)
	}
	if !contains(s.Resolutions, res) {
		return out, fmt.Errorf("stream %s does not support %s", stream, res)
	}
	// Every MJPEG frame is a whole picture.
	if out.Codec == "mjpeg" {
		out.GOP = 1
	}
	return out, nil
}
