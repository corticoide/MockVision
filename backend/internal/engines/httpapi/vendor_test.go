package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/corticoide/mockvision/sdk/engine"
)

// fakeEvents hands the engine's subscription to the test.
type fakeEvents struct {
	mu   sync.Mutex
	subs map[string]func(engine.Dispatch)
}

func (f *fakeEvents) Emit(context.Context, engine.Event) (engine.Event, error) {
	return engine.Event{}, nil
}
func (f *fakeEvents) Subscribe(transport string, fn func(engine.Dispatch)) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs[transport] = fn
	return func() {}
}
func (f *fakeEvents) Report(engine.DeliveryReport)   {}
func (f *fakeEvents) Targets(string) []engine.Target { return nil }

func (f *fakeEvents) dispatch(transport string, d engine.Dispatch) {
	f.mu.Lock()
	fn := f.subs[transport]
	f.mu.Unlock()
	fn(d)
}

const vendorConfig = `{
  "engine": "http-api@^1",
  "auth": {"scheme": "none"},
  "routes": [
    {"id": "get", "match": {"path": "/config.cgi", "query": {"action": "get"}},
     "action": {"handler": "state.get", "from": "query", "key": "name",
       "then": {"body": "{{ range .Result }}{{ .Key }}={{ .Value }}\r\n{{ end }}"},
       "error": {"type": "text/plain", "body": "Error\r\nBad Request!\r\n"}}},
    {"id": "set", "match": {"path": "/config.cgi", "query": {"action": "set"}},
     "action": {"handler": "state.set", "from": "query", "error": {"status": 200, "body": "<Error>{{ .Result }}</Error>"}}},
    {"id": "attach", "match": {"path": "/eventManager.cgi", "query": {"action": "attach"}},
     "action": {"handler": "events.attach", "params": {"codes": "codes", "heartbeat": "heartbeat"}, "boundary": "myboundary"}}
  ]
}`

func startVendor(t *testing.T, events engine.Events) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := New()
	h := fakeHost{state: &fakeState{values: map[string]any{"Image.Brightness": 50}}, events: events}
	if err := e.Start(context.Background(), engine.StartInput{Config: json.RawMessage(vendorConfig), Instance: "http",
		Listeners: map[string]net.Listener{"http": ln}, Host: h}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := e.Stop(ctx); err != nil {
			t.Errorf("stop with an attach open: %v", err)
		}
	})
	return "http://" + ln.Addr().String()
}

// A handler that fails answers in the vendor's words, with its status or
// the action's.
func TestVendorErrors(t *testing.T) {
	base := startVendor(t, nil)
	if code, _, body := fetch(t, "GET", base+"/config.cgi?action=get&name=Nope", ""); code != http.StatusBadRequest || body != "Error\r\nBad Request!\r\n" {
		t.Fatalf("unknown: %d %q", code, body)
	}
	if code, _, body := fetch(t, "GET", base+"/config.cgi?action=get&name=Image.Brightness", ""); code != 200 || body != "Image.Brightness=50\r\n" {
		t.Fatalf("get: %d %q", code, body)
	}
	if code, _, body := fetch(t, "GET", base+"/config.cgi?action=set&Nope=1", ""); code != 200 || body != "<Error>Nope: unknown parameter</Error>" {
		t.Fatalf("set: %d %q", code, body)
	}
	e := New()
	for _, bad := range []string{
		`{"handler": "events.attach", "params": {"start": "s"}}`,
		`{"handler": "events.attach", "then": {"body": "x"}}`,
		`{"handler": "snapshot", "boundary": "b"}`,
		`{"body": "x", "error": {"body": "y"}}`,
		`{"handler": "state.get", "error": {"handler": "snapshot"}}`,
	} {
		cfg := `{"engine": "http-api@^1", "routes": [{"id": "r", "match": {"path": "/x"}, "action": ` + bad + `}]}`
		if probs := e.Validate(json.RawMessage(cfg)); !hasErrors(probs) {
			t.Fatalf("%s was accepted", bad)
		}
	}
}

// An attach request gets the events it asked for as they happen, the end
// of those that last, and a heartbeat; the engine stops with it open.
func TestAttach(t *testing.T) {
	events := &fakeEvents{subs: map[string]func(engine.Dispatch){}}
	base := startVendor(t, events)
	resp, err := http.Get(base + "/eventManager.cgi?action=attach&codes=[VideoMotion]&heartbeat=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "multipart/x-mixed-replace; boundary=myboundary" {
		t.Fatalf("content type %q", ct)
	}
	lines := make(chan string, 64)
	go func() {
		br := bufio.NewReader(resp.Body)
		for {
			l, err := br.ReadString('\n')
			if err != nil {
				close(lines)
				return
			}
			lines <- strings.TrimRight(l, "\r\n")
		}
	}()
	read := func(want ...string) {
		t.Helper()
		for _, w := range want {
			select {
			case l := <-lines:
				if l != w {
					t.Fatalf("got %q, want %q", l, w)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("no %q", w)
			}
		}
	}
	read("--myboundary", "Content-Type: text/plain", "Content-Length: 9", "", "Heartbeat", "")
	ev := engine.Event{ID: "01J0000000000000000000000X", Type: "motion", At: time.Now()}
	transport := json.RawMessage(`{"body": "Code={{ .EventName }};action=Start;index=0", "stop": {"after": "300ms", "body": "Code={{ .EventName }};action=Stop;index=0"}}`)
	events.dispatch(engine.TransportAttach, engine.Dispatch{Event: engine.Event{ID: "x", Type: "tamper"}, VendorName: "VideoBlind", Transport: transport})
	events.dispatch(engine.TransportAttach, engine.Dispatch{Event: ev, VendorName: "VideoMotion", Transport: transport})
	for {
		l := <-lines
		if l == "Code=VideoMotion;action=Start;index=0" {
			break
		}
		if strings.Contains(l, "VideoBlind") {
			t.Fatal("a code the client did not ask for reached it")
		}
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case l := <-lines:
			if l == "Code=VideoMotion;action=Stop;index=0" {
				return
			}
		case <-deadline:
			t.Fatal("no Stop")
		}
	}
}
