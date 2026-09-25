// Package tmpl compiles and renders profile templates with Go's text/template
// and a closed set of safe functions: state, canon, uuid, rand, b64,
// snapshot, fmtTime, json, xml, default, lower and upper. Templates have no
// access to files, network, environment or processes, each render is bounded
// in time and size, and request data is inserted as values, never evaluated
// as a template.
package tmpl

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/corticoide/mockvision/sdk/engine"
)

const (
	// DefaultMaxBytes bounds a render's output.
	DefaultMaxBytes = 1 << 20
	// ImageMaxBytes bounds renders of event templates that embed images.
	ImageMaxBytes = 8 << 20
	// Timeout bounds a render's duration.
	Timeout = 50 * time.Millisecond
)

// Env binds the functions that depend on the camera.
type Env struct {
	State    func(key string) (any, bool)
	Canon    func(key string) (any, bool)
	Snapshot func() ([]byte, error)
}

// Compiler implements engine.Templates for one camera.
type Compiler struct {
	funcs template.FuncMap
}

// NewCompiler returns a compiler whose functions read from env.
func NewCompiler(env Env) *Compiler {
	return &Compiler{funcs: funcMap(env)}
}

// Compile parses a template.
func (c *Compiler) Compile(name, text string, maxBytes int) (engine.Template, error) {
	t, err := parse(name, text, c.funcs)
	if err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &compiled{t: t, maxBytes: maxBytes}, nil
}

// SyntaxError is a template parse error. Line is the line inside the
// template, starting at 1, or 0 when unknown.
type SyntaxError struct {
	Line    int
	Message string
}

func (e *SyntaxError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d: %s", e.Line, e.Message)
	}
	return e.Message
}

// Check parses a template without executing it, as the importer does.
func Check(name, text string) error {
	_, err := parse(name, text, funcMap(Env{}))
	return err
}

func parse(name, text string, funcs template.FuncMap) (*template.Template, error) {
	t, err := template.New(name).Option("missingkey=zero").Funcs(funcs).Parse(text)
	if err != nil {
		return nil, syntaxError(name, err)
	}
	return t, nil
}

// syntaxError turns "template: NAME:LINE: message" into a SyntaxError.
func syntaxError(name string, err error) error {
	msg := strings.TrimPrefix(err.Error(), "template: ")
	rest, ok := strings.CutPrefix(msg, name+":")
	if !ok {
		return &SyntaxError{Message: msg}
	}
	num, text, ok := strings.Cut(rest, ":")
	line, convErr := strconv.Atoi(num)
	if !ok || convErr != nil {
		return &SyntaxError{Message: msg}
	}
	return &SyntaxError{Line: line, Message: strings.TrimSpace(text)}
}

// cleanError drops the "template: " prefix text/template adds.
func cleanError(err error) error {
	return errors.New(strings.TrimPrefix(err.Error(), "template: "))
}

type compiled struct {
	t        *template.Template
	maxBytes int
}

var errTooLarge = errors.New("output too large")

// Render executes the template within the time and size limits.
func (c *compiled) Render(ctx context.Context, data engine.TemplateData) ([]byte, error) {
	w := &limitedBuffer{max: c.maxBytes}
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("template panicked: %v", r)
			}
		}()
		done <- c.t.Execute(w, data)
	}()
	timer := time.NewTimer(Timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			if errors.Is(err, errTooLarge) {
				return nil, fmt.Errorf("render %s: output exceeds %d bytes", c.t.Name(), c.maxBytes)
			}
			return nil, fmt.Errorf("render: %w", cleanError(err))
		}
		return w.buf.Bytes(), nil
	case <-timer.C:
		w.abort.Store(true)
		return nil, fmt.Errorf("render %s: exceeded %s", c.t.Name(), Timeout)
	case <-ctx.Done():
		w.abort.Store(true)
		return nil, ctx.Err()
	}
}

type limitedBuffer struct {
	buf   bytes.Buffer
	max   int
	abort atomic.Bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.abort.Load() {
		return 0, errors.New("aborted")
	}
	if b.buf.Len()+len(p) > b.max {
		return 0, errTooLarge
	}
	return b.buf.Write(p)
}

func funcMap(env Env) template.FuncMap {
	return template.FuncMap{
		"state": func(key string) any {
			if env.State == nil {
				return ""
			}
			v, ok := env.State(key)
			if !ok {
				return ""
			}
			return v
		},
		"canon": func(key string) any {
			if env.Canon == nil {
				return ""
			}
			v, ok := env.Canon(key)
			if !ok {
				return ""
			}
			return v
		},
		"uuid": newUUID,
		"rand": randFunc,
		"b64":  b64,
		"snapshot": func() (string, error) {
			if env.Snapshot == nil {
				return "", nil
			}
			jpeg, err := env.Snapshot()
			if err != nil {
				return "", err
			}
			return base64.StdEncoding.EncodeToString(jpeg), nil
		},
		"fmtTime": fmtTime,
		"json":    toJSON,
		"xml":     toXML,
		"default": defaultFunc,
		"lower":   func(v any) string { return strings.ToLower(toString(v)) },
		"upper":   func(v any) string { return strings.ToUpper(toString(v)) },
	}
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// randFunc: rand -> uint32, rand n -> [0,n), rand min max -> [min,max].
func randFunc(args ...int) (int64, error) {
	switch len(args) {
	case 0:
		var b [4]byte
		_, _ = rand.Read(b[:])
		return int64(binary.BigEndian.Uint32(b[:])), nil
	case 1:
		if args[0] <= 0 {
			return 0, errors.New("rand: n must be positive")
		}
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(args[0])))
		return n.Int64(), nil
	case 2:
		lo, hi := args[0], args[1]
		if hi < lo {
			return 0, errors.New("rand: max must be >= min")
		}
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(hi-lo)+1))
		return int64(lo) + n.Int64(), nil
	}
	return 0, errors.New("rand takes at most two arguments")
}

func b64(v any) string {
	switch x := v.(type) {
	case []byte:
		return base64.StdEncoding.EncodeToString(x)
	default:
		return base64.StdEncoding.EncodeToString([]byte(toString(v)))
	}
}

// fmtTime formats a time with a Go layout or one of: rfc3339, rfc3339ms,
// iso8601, unix, unixms, http.
func fmtTime(t time.Time, layout string) string {
	switch strings.ToLower(layout) {
	case "rfc3339":
		return t.Format(time.RFC3339)
	case "rfc3339ms":
		return t.Format("2006-01-02T15:04:05.000Z07:00")
	case "iso8601":
		return t.Format("2006-01-02T15:04:05-0700")
	case "unix":
		return strconv.FormatInt(t.Unix(), 10)
	case "unixms":
		return strconv.FormatInt(t.UnixMilli(), 10)
	case "http":
		return t.UTC().Format(time.RFC1123)
	}
	return t.Format(layout)
}

// toJSON encodes v without HTML escaping, as devices do.
func toJSON(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

func toXML(v any) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(toString(v)))
	return buf.String()
}

func defaultFunc(def any, v ...any) any {
	if len(v) == 0 || isEmpty(v[0]) {
		return def
	}
	return v[0]
}

func isEmpty(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String, reflect.Slice, reflect.Map, reflect.Array:
		return rv.Len() == 0
	case reflect.Pointer, reflect.Interface:
		return rv.IsNil()
	case reflect.Bool:
		return !rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return rv.Float() == 0
	}
	return false
}

func toString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	case fmt.Stringer:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}
