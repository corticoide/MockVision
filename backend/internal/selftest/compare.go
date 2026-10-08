package selftest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

func sortStrings(s []string) { sort.Strings(s) }

// Answer is what the camera answered, or sent for an event.
type Answer struct {
	Status  int
	Headers http.Header
	Body    []byte
}

var (
	uuidPattern = regexp.MustCompile(`^(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	intPattern  = regexp.MustCompile(`^-?[0-9]+$`)
)

// timestampLayouts are the forms of a timestamp field, besides seconds or
// milliseconds since the epoch.
var timestampLayouts = []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04:05.000Z07:00",
	"2006-01-02T15:04:05Z0700", "20060102T150405Z", "2006/01/02 15:04:05"}

// checkType says whether a varying value has the type it should.
func checkType(as, value, serial string) error {
	v := strings.TrimSpace(value)
	switch as {
	case TypeAny:
		return nil
	case TypeInt:
		if intPattern.MatchString(v) {
			return nil
		}
	case TypeUUID:
		if uuidPattern.MatchString(v) {
			return nil
		}
	case TypeHTTPDate:
		if _, err := http.ParseTime(v); err == nil {
			return nil
		}
	case TypeTimestamp:
		if intPattern.MatchString(v) && (len(v) == 10 || len(v) == 13) {
			return nil
		}
		for _, l := range timestampLayouts {
			if _, err := time.Parse(l, v); err == nil {
				return nil
			}
		}
	case TypeSerial:
		if v == serial {
			return nil
		}
		return fmt.Errorf("%q is not the camera's serial %s", value, serial)
	case TypeImage:
		if isImage([]byte(value)) {
			return nil
		}
		return errors.New("not a JPEG or PNG image")
	}
	return fmt.Errorf("%q is not a %s", short(value), as)
}

func isImage(b []byte) bool {
	return bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}) || bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n"))
}

func short(s string) string {
	if len(s) > 60 {
		return s[:57] + "..."
	}
	return s
}

// Compare says whether the camera's answer matches the recorded one: the
// status, the recorded headers and the body, with the varying fields
// checked by type. A nil error is a match.
func Compare(want *Response, vary []Vary, got Answer, serial string) error {
	if want.Status != 0 && got.Status != want.Status {
		return fmt.Errorf("status %d, the device answered %d", got.Status, want.Status)
	}
	return compareContent(want.Headers, want.Body, vary, got, serial)
}

// CompareSent says whether what a transport sent matches the recording.
func CompareSent(want Expect, got Answer, serial string) error {
	return compareContent(want.Headers, want.Body, want.Vary, got, serial)
}

func compareContent(headers map[string]string, body string, vary []Vary, got Answer, serial string) error {
	headerVary := map[string]Vary{}
	var bodyVary []Vary
	for _, v := range vary {
		if v.In == "header" {
			headerVary[http.CanonicalHeaderKey(v.Name)] = v
		} else {
			bodyVary = append(bodyVary, v)
		}
	}
	for _, name := range sortedKeys(headers) {
		key := http.CanonicalHeaderKey(name)
		value, present := got.Headers[key]
		if !present {
			return fmt.Errorf("header %s is missing", key)
		}
		if v, ok := headerVary[key]; ok {
			if err := checkType(v.As, strings.Join(value, ", "), serial); err != nil {
				return fmt.Errorf("header %s: %v", key, err)
			}
			continue
		}
		if strings.Join(value, ", ") != headers[name] {
			return fmt.Errorf("header %s is %q, the device sent %q", key, strings.Join(value, ", "), headers[name])
		}
	}
	return compareBody(body, bodyVary, got.Body, serial)
}

// compareBody compares two bodies. A body that is all an image, or
// anything, is checked by type; JSON compares as JSON, with path varies;
// anything else as text, lines ending alike, with regex varies.
func compareBody(want string, vary []Vary, got []byte, serial string) error {
	var regexes, paths []Vary
	for _, v := range vary {
		switch {
		case v.Regex != "":
			regexes = append(regexes, v)
		case v.Path != "":
			paths = append(paths, v)
		default:
			if err := checkType(v.As, string(got), serial); err != nil {
				return fmt.Errorf("body: %v", err)
			}
			return nil
		}
	}
	wantText, gotText := normalize(want), normalize(string(got))
	for i, v := range regexes {
		re := regexp.MustCompile(v.Regex)
		placeholder := fmt.Sprintf("\x00vary%d\x00", i)
		wm := re.FindAllStringSubmatchIndex(wantText, -1)
		gm := re.FindAllStringSubmatchIndex(gotText, -1)
		if len(wm) != len(gm) {
			return fmt.Errorf("body: %q matches %d times, %d in the recording", v.Regex, len(gm), len(wm))
		}
		for _, m := range gm {
			if err := checkType(v.As, gotText[m[2]:m[3]], serial); err != nil {
				return fmt.Errorf("body: %s: %v", v.Regex, err)
			}
		}
		wantText, gotText = replaceGroups(wantText, wm, placeholder), replaceGroups(gotText, gm, placeholder)
	}
	var wantJSON, gotJSON any
	if json.Unmarshal([]byte(wantText), &wantJSON) == nil && isContainer(wantJSON) {
		if err := json.Unmarshal([]byte(gotText), &gotJSON); err != nil {
			return errors.New("body: the device answered JSON, the camera did not")
		}
		for _, v := range paths {
			p, _ := parsePath(v.Path)
			value, ok := p.get(gotJSON)
			if !ok {
				return fmt.Errorf("body: %s is missing", v.Path)
			}
			s, isString := value.(string)
			if !isString {
				b, _ := json.Marshal(value)
				s = string(b)
			}
			if err := checkType(v.As, s, serial); err != nil {
				return fmt.Errorf("body: %s: %v", v.Path, err)
			}
			p.set(wantJSON, "\x00vary\x00")
			p.set(gotJSON, "\x00vary\x00")
		}
		if !reflect.DeepEqual(wantJSON, gotJSON) {
			return fmt.Errorf("body: %s", firstDifference(wantJSON, gotJSON, "$"))
		}
		return nil
	}
	if len(paths) > 0 {
		return errors.New("body: a path vary needs a JSON body")
	}
	if wantText != gotText {
		return fmt.Errorf("body: %s", textDifference(wantText, gotText))
	}
	return nil
}

func isContainer(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

// normalize makes line endings alike and drops the trailing ones: a YAML
// block ends with a newline the device may not have sent.
func normalize(s string) string {
	return strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}

func replaceGroups(s string, matches [][]int, placeholder string) string {
	var b strings.Builder
	last := 0
	for _, m := range matches {
		b.WriteString(s[last:m[2]])
		b.WriteString(placeholder)
		last = m[3]
	}
	b.WriteString(s[last:])
	return b.String()
}

func textDifference(want, got string) string {
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			return fmt.Sprintf("line %d is %q, the device sent %q", i+1, short(clean(g)), short(clean(w)))
		}
	}
	return "it differs"
}

func clean(s string) string { return strings.ReplaceAll(s, "\x00", "") }

func firstDifference(want, got any, at string) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return at + " is not an object"
		}
		for _, k := range sortedKeys(w) {
			if _, has := g[k]; !has {
				return at + "." + k + " is missing"
			}
			if !reflect.DeepEqual(w[k], g[k]) {
				return firstDifference(w[k], g[k], at+"."+k)
			}
		}
		for _, k := range sortedKeys(g) {
			if _, has := w[k]; !has {
				return at + "." + k + " is not in the recording"
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			return at + " is not a list"
		}
		if len(w) != len(g) {
			return fmt.Sprintf("%s has %d items, the device sent %d", at, len(g), len(w))
		}
		for i := range w {
			if !reflect.DeepEqual(w[i], g[i]) {
				return firstDifference(w[i], g[i], fmt.Sprintf("%s[%d]", at, i))
			}
		}
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	return fmt.Sprintf("%s is %s, the device sent %s", at, short(clean(string(gb))), short(clean(string(wb))))
}

// jsonPath is a path such as $.a.b[0].c.
type jsonPath []any // string keys and int indexes

var pathSegment = regexp.MustCompile(`^(?:\.([A-Za-z0-9_@-]+)|\[(\d+)\]|\["([^"]+)"\])`)

func parsePath(p string) (jsonPath, error) {
	if !strings.HasPrefix(p, "$") {
		return nil, errors.New("a path starts with $")
	}
	rest := p[1:]
	var out jsonPath
	for rest != "" {
		m := pathSegment.FindStringSubmatch(rest)
		if m == nil {
			return nil, fmt.Errorf("cannot read %q", rest)
		}
		switch {
		case m[1] != "":
			out = append(out, m[1])
		case m[2] != "":
			i, _ := strconv.Atoi(m[2])
			out = append(out, i)
		default:
			out = append(out, m[3])
		}
		rest = rest[len(m[0]):]
	}
	if len(out) == 0 {
		return nil, errors.New("the path names nothing")
	}
	return out, nil
}

func (p jsonPath) get(doc any) (any, bool) {
	cur := doc
	for _, seg := range p {
		switch s := seg.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			if cur, ok = m[s]; !ok {
				return nil, false
			}
		case int:
			l, ok := cur.([]any)
			if !ok || s >= len(l) {
				return nil, false
			}
			cur = l[s]
		}
	}
	return cur, true
}

func (p jsonPath) set(doc any, value any) {
	cur := doc
	for i, seg := range p {
		last := i == len(p)-1
		switch s := seg.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				return
			}
			if last {
				if _, has := m[s]; has {
					m[s] = value
				}
				return
			}
			cur = m[s]
		case int:
			l, ok := cur.([]any)
			if !ok || s >= len(l) {
				return
			}
			if last {
				l[s] = value
				return
			}
			cur = l[s]
		}
	}
}
