package httpapi

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/tmpl"
	"github.com/corticoide/mockvision/sdk/engine"
)

// fakeHost is the smallest camera an engine can run in.
type fakeHost struct {
	accounts fakeAccounts
	state    *fakeState
}

func (h fakeHost) Accounts() engine.Accounts   { return h.accounts }
func (h fakeHost) State() engine.State         { return h.state }
func (h fakeHost) Events() engine.Events       { return nil }
func (h fakeHost) Media() engine.Media         { return fakeMedia{} }
func (h fakeHost) Templates() engine.Templates { return tmpl.NewCompiler(tmpl.Env{State: h.state.Get}) }
func (h fakeHost) Files() engine.Files         { return nil }
func (h fakeHost) Telemetry() engine.Telemetry { return fakeTelemetry{} }

type fakeAccounts []engine.User

func (a fakeAccounts) List() []engine.User { return a }
func (a fakeAccounts) Lookup(name string) (engine.User, bool) {
	for _, u := range a {
		if u.Username == name {
			return u, true
		}
	}
	return engine.User{}, false
}

type fakeState struct {
	mu     sync.Mutex
	values map[string]any
}

func (s *fakeState) Get(key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[key]
	return v, ok
}

func (s *fakeState) List(prefix string) []engine.Param {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []engine.Param
	for k, v := range s.values {
		if strings.HasPrefix(k, prefix) {
			out = append(out, engine.Param{Key: k, Value: v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (s *fakeState) Set(_ context.Context, values map[string]any, origin engine.Origin) ([]engine.Change, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []engine.Change
	for k, v := range values {
		if _, ok := s.values[k]; !ok {
			return nil, &engine.StateError{Problems: map[string]string{k: "unknown parameter"}}
		}
		s.values[k] = v
		out = append(out, engine.Change{Key: k, Value: v, Origin: origin})
	}
	return out, nil
}

func (s *fakeState) Canon(string) (any, bool)           { return nil, false }
func (s *fakeState) Watch(func([]engine.Change)) func() { return func() {} }

type fakeTelemetry struct{}

func (fakeTelemetry) Log(slog.Level, string, ...any)             {}
func (fakeTelemetry) Request(string, string, int, time.Duration) {}
func (fakeTelemetry) Gap(string, string, string)                 {}
func (fakeTelemetry) Client(string, string, bool)                {}

type fakeMedia struct{}

func (fakeMedia) Streams() []engine.StreamInfo               { return nil }
func (fakeMedia) Snapshot(string) ([]byte, error)            { return []byte{0xff, 0xd8, 0xff, 0xd9}, nil }
func (fakeMedia) Source(string) (*engine.VideoSource, error) { return nil, fmt.Errorf("none") }
func (fakeMedia) Watch(func(string)) func()                  { return func() {} }

const testConfig = `{
  "engine": "http-api@^1",
  "auth": {"scheme": "digest", "realm": "cam"},
  "routes": [
    {"id": "snap", "match": {"path": "/snapshot.cgi"}, "action": {"handler": "snapshot"}},
    {"id": "get", "match": {"method": "GET", "path": "/param.cgi", "query": {"action": "get"}},
     "action": {"handler": "state.get", "from": "query", "key": "name"}},
    {"id": "get-form", "match": {"method": "POST", "path": "/param.cgi"},
     "action": {"handler": "state.get", "from": "form", "key": "name"}},
    {"id": "set", "match": {"method": "GET", "path": "/param.cgi", "query": {"action": "set"}},
     "action": {"handler": "state.set", "from": "query"}}
  ]
}`

func startEngine(t *testing.T) string {
	t.Helper()
	h := fakeHost{
		accounts: fakeAccounts{
			{Username: "admin", Password: "a", Role: RoleAdmin},
			{Username: "op", Password: "o", Role: RoleOperator},
			{Username: "view", Password: "v", Role: RoleViewer},
		},
		state: &fakeState{values: map[string]any{"Image.Brightness": 50, "Image.Contrast": 40}},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := New()
	if err := e.Start(context.Background(), engine.StartInput{Config: json.RawMessage(testConfig), Instance: "http",
		Listeners: map[string]net.Listener{"http": ln}, Host: h}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop(context.Background()) })
	return "http://" + ln.Addr().String()
}

func md5s(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// digestHeader answers the challenge of a first request, for uri.
func digestHeader(t *testing.T, base, method, target, uri, user, pass string, nc int) string {
	t.Helper()
	resp, err := http.Get(base + target)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	p := parseDigest(strings.TrimPrefix(resp.Header.Get("WWW-Authenticate"), "Digest "))
	ncs := fmt.Sprintf("%08x", nc)
	ha1 := md5s(user + ":" + p["realm"] + ":" + pass)
	ha2 := md5s(method + ":" + uri)
	r := md5s(ha1 + ":" + p["nonce"] + ":" + ncs + ":c0ffee:auth:" + ha2)
	return fmt.Sprintf(`Digest username=%q, realm=%q, nonce=%q, uri=%q, qop=auth, nc=%s, cnonce="c0ffee", response=%q`,
		user, p["realm"], p["nonce"], uri, ncs, r)
}

func send(t *testing.T, base, method, target, auth, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, base+target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Roles apply: a viewer reads, only admin and operator accounts change
// parameters (audit M5).
func TestRoles(t *testing.T) {
	base := startEngine(t)
	for _, c := range []struct {
		user, pass, target string
		want               int
	}{
		{"view", "v", "/snapshot.cgi", http.StatusOK},
		{"view", "v", "/param.cgi?action=get&name=Image.Brightness", http.StatusOK},
		{"view", "v", "/param.cgi?action=set&Image.Brightness=1", http.StatusForbidden},
		{"op", "o", "/param.cgi?action=set&Image.Brightness=2", http.StatusOK},
		{"admin", "a", "/param.cgi?action=set&Image.Brightness=3", http.StatusOK},
	} {
		auth := digestHeader(t, base, "GET", c.target, c.target, c.user, c.pass, 1)
		if code, body := send(t, base, "GET", c.target, auth, ""); code != c.want {
			t.Errorf("%s %s: %d %q, want %d", c.user, c.target, code, body, c.want)
		}
	}
}

// state.get with from: form reads the key from the body (audit B4).
func TestStateGetFromForm(t *testing.T) {
	base := startEngine(t)
	auth := digestHeader(t, base, "POST", "/param.cgi", "/param.cgi", "admin", "a", 1)
	code, body := send(t, base, "POST", "/param.cgi", auth, "name=Image.Contrast")
	if code != http.StatusOK || strings.TrimSpace(body) != "Image.Contrast=40" {
		t.Fatalf("%d %q", code, body)
	}
}

// A Digest header cannot be sent twice, nor used for another query than
// the one it signs (audit B6).
func TestDigestReplayAndURI(t *testing.T) {
	base := startEngine(t)
	target := "/param.cgi?action=get&name=Image.Brightness"
	auth := digestHeader(t, base, "GET", target, target, "admin", "a", 1)
	if code, _ := send(t, base, "GET", target, auth, ""); code != http.StatusOK {
		t.Fatalf("first use: %d", code)
	}
	if code, _ := send(t, base, "GET", target, auth, ""); code != http.StatusUnauthorized {
		t.Fatalf("replay: %d", code)
	}
	// Signed for the path alone, sent with a query that sets a parameter.
	pathOnly := digestHeader(t, base, "GET", "/param.cgi", "/param.cgi", "admin", "a", 2)
	if code, _ := send(t, base, "GET", "/param.cgi?action=set&Image.Brightness=9", pathOnly, ""); code != http.StatusUnauthorized {
		t.Fatalf("path-only uri with a query: %d", code)
	}
}
