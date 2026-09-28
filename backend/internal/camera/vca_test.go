package camera

import (
	"context"
	"encoding/json"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/sdk/engine"
)

var square = []domain.Point{{X: 0.2, Y: 0.2}, {X: 0.6, Y: 0.2}, {X: 0.6, Y: 0.6}, {X: 0.2, Y: 0.6}}

// vcaProfile is a profile with analytics and nothing else: events pushed
// over HTTP without a minimum interval, and a switch for line crossings.
func vcaProfile(t *testing.T) json.RawMessage {
	t.Helper()
	push := map[string]any{"transports": map[string]any{"http_push": map[string]any{"method": "POST", "body": "{{ json .Event.ID }}"}}}
	raw, err := json.Marshal(map[string]any{
		"schema":   1,
		"profile":  map[string]any{"id": "test/vca", "version": "1.0.0", "name": "VCA", "vendor": "Test", "model": "T1"},
		"identity": map[string]any{"serial": "999", "factory": map[string]any{"network": map[string]any{}, "users": []any{}}},
		"state": map[string]any{
			"Event.Line.Enable": map[string]any{"type": "bool", "default": true, "bind": "events.line_crossing.enabled"},
		},
		"media":   map[string]any{"streams": map[string]any{}},
		"engines": map[string]any{"push": map[string]any{"engine": "http-push@^1"}},
		"events":  map[string]any{"line_crossing": push, "region_entrance": push, "lpr": push},
		"vca":     map[string]any{"rules": []string{"line", "region"}, "object_classes": []string{"person", "car"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// startVCACamera runs a camera with the analytics profile and the given
// rules and triggers, its trigger seconds lasting 20 ms.
func startVCACamera(t *testing.T, ctx context.Context, vca ipc.VCA) (*Runtime, *fakeService, chan error) {
	t.Helper()
	svcSide, camSide := net.Pipe()
	svc := &fakeService{msgs: map[string][]*ipc.Envelope{}, got: make(chan *ipc.Envelope, 256)}
	svc.conn = ipc.NewConn(svcSide, svc.handle, nil)
	go func() { _ = svc.conn.Run(ctx) }()
	rt := NewRuntime(Options{CameraID: "cam1", IPC: camSide, Local: true})
	rt.vca.second = 20 * time.Millisecond
	runDone := make(chan error, 1)
	go func() { runDone <- rt.Run(ctx) }()
	svc.wait(t, ipc.TypeHello, 5*time.Second)
	cfg := ipc.Configure{
		Identity: engine.Identity{CameraID: "cam1", Name: "Gate", IP: "127.0.0.1", MAC: "02:aa:bb:cc:dd:ee", Serial: "999"},
		Profile:  vcaProfile(t),
		Engines:  []ipc.EngineConfig{{Instance: "push", Enabled: true}},
		VCA:      vca,
	}
	if err := svc.conn.Request(ctx, ipc.TypeConfigure, cfg, nil); err != nil {
		t.Fatalf("configure: %v", err)
	}
	svc.wait(t, ipc.TypeReady, 5*time.Second)
	return rt, svc, runDone
}

func nextEvent(t *testing.T, svc *fakeService) ipc.EventMsg {
	t.Helper()
	var ev ipc.EventMsg
	if err := svc.wait(t, ipc.TypeEvent, 3*time.Second).Decode(&ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

// eventWithID waits for the report of an event, skipping the others: the
// reply to a request may overtake the notices sent before it.
func eventWithID(t *testing.T, svc *fakeService, id string) ipc.EventMsg {
	t.Helper()
	for {
		if ev := nextEvent(t, svc); ev.Event.ID == id {
			return ev
		}
	}
}

func (f *fakeService) drop(typ string) {
	f.mu.Lock()
	delete(f.msgs, typ)
	f.mu.Unlock()
}

func (f *fakeService) count(typ string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.msgs[typ])
}

func boxInside(b *engine.Box) bool {
	return b != nil && b.X >= 0 && b.Y >= 0 && b.W > 0 && b.H > 0 && b.X+b.W <= 1.0001 && b.Y+b.H <= 1.0001
}

func TestRandomTriggers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	gate := domain.Rule{ID: "r-gate", Name: "Gate", Type: domain.RuleLine, Points: []domain.Point{{X: 0.1, Y: 0.5}, {X: 0.9, Y: 0.5}},
		Direction: domain.CrossBA, Enabled: true}
	lot := domain.Rule{ID: "r-lot", Name: "Lot", Type: domain.RuleRegion, Points: square, Events: []string{"region_entrance"},
		ObjectClasses: []string{"car"}, Enabled: true}
	traffic := domain.Trigger{ID: "t-traffic", Name: "Traffic", Type: domain.TriggerRandom, EventType: "line_crossing", RuleID: "r-gate",
		MinSeconds: 1, MaxSeconds: 2, Enabled: true}
	rt, svc, runDone := startVCACamera(t, ctx, ipc.VCA{Rules: []domain.Rule{gate, lot}, Triggers: []domain.Trigger{traffic}})

	t.Run("events come on their own", func(t *testing.T) {
		for range 3 {
			ev := nextEvent(t, svc)
			e := ev.Event
			if ev.TriggerID != "t-traffic" || e.Trigger != "random" || e.Type != "line_crossing" || e.Rule == nil || e.Rule.ID != "r-gate" ||
				e.Rule.Name != "Gate" || e.Rule.Type != "line" {
				t.Fatalf("event %+v from trigger %q", e, ev.TriggerID)
			}
			// The rule reports B->A only; the object is just past the line,
			// on side A, above it.
			if e.Direction != "B->A" || e.Object == nil || !boxInside(e.Object.Box) || e.Object.Box.Y+e.Object.Box.H > 0.502 {
				t.Fatalf("crossing %q, object %+v", e.Direction, e.Object)
			}
			if c := e.Object.Class; c != "person" && c != "car" || e.Object.Confidence < 0.8 || e.Object.Confidence > 0.99 {
				t.Fatalf("object %+v", e.Object)
			}
		}
	})

	t.Run("a rule change keeps the loop", func(t *testing.T) {
		rt.vca.mu.Lock()
		before := rt.vca.loops["t-traffic"]
		rt.vca.mu.Unlock()
		both := gate
		both.Direction = domain.CrossAB
		if err := svc.conn.Request(ctx, ipc.TypeReload, ipc.Reload{VCA: &ipc.VCA{Rules: []domain.Rule{both, lot}, Triggers: []domain.Trigger{traffic}}}, nil); err != nil {
			t.Fatal(err)
		}
		rt.vca.mu.Lock()
		after := rt.vca.loops["t-traffic"]
		rt.vca.mu.Unlock()
		if before == nil || before != after {
			t.Fatal("the loop of an unchanged trigger was restarted")
		}
		time.Sleep(60 * time.Millisecond)
		svc.drop(ipc.TypeEvent)
		if ev := nextEvent(t, svc); ev.Event.Direction != "A->B" {
			t.Fatalf("the edited rule is not used: %+v", ev.Event)
		}
	})

	t.Run("disabled trigger stops, and fires on request", func(t *testing.T) {
		off := traffic
		off.Enabled = false
		if err := svc.conn.Request(ctx, ipc.TypeReload, ipc.Reload{VCA: &ipc.VCA{Rules: []domain.Rule{gate, lot}, Triggers: []domain.Trigger{off}}}, nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(60 * time.Millisecond)
		svc.drop(ipc.TypeEvent)
		time.Sleep(200 * time.Millisecond) // five trigger seconds
		if n := svc.count(ipc.TypeEvent); n != 0 {
			t.Fatalf("%d events after disabling the trigger", n)
		}
		var res ipc.TriggerResult
		if err := svc.conn.Request(ctx, ipc.TypeTrigger, ipc.Trigger{TriggerID: "t-traffic"}, &res); err != nil {
			t.Fatal(err)
		}
		ev := eventWithID(t, svc, res.Event.ID)
		if ev.TriggerID != "t-traffic" || ev.Event.Trigger != "manual" || ev.Event.Direction != "B->A" {
			t.Fatalf("fired event %+v from %q", ev.Event, ev.TriggerID)
		}
		if err := svc.conn.Request(ctx, ipc.TypeTrigger, ipc.Trigger{TriggerID: "nope"}, nil); err == nil {
			t.Fatal("an unknown trigger fired")
		}
	})

	t.Run("a disabled rule raises nothing", func(t *testing.T) {
		g := gate
		g.Enabled = false
		off := traffic
		off.Enabled = false
		if err := svc.conn.Request(ctx, ipc.TypeReload, ipc.Reload{VCA: &ipc.VCA{Rules: []domain.Rule{g, lot}, Triggers: []domain.Trigger{off}}}, nil); err != nil {
			t.Fatal(err)
		}
		err := svc.conn.Request(ctx, ipc.TypeTrigger, ipc.Trigger{TriggerID: "t-traffic"}, nil)
		if err == nil || !strings.Contains(err.Error(), "rule Gate is disabled") {
			t.Fatalf("fired on a disabled rule: %v", err)
		}
	})

	t.Run("any rule, a default one, plates", func(t *testing.T) {
		g := gate
		g.Enabled = false
		anyRegion := domain.Trigger{ID: "t-region", Name: "Lot", Type: domain.TriggerRandom, EventType: "region_entrance", MinSeconds: 1, MaxSeconds: 1}
		anyLine := domain.Trigger{ID: "t-line", Name: "Line", Type: domain.TriggerRandom, EventType: "line_crossing", MinSeconds: 1, MaxSeconds: 1}
		plates := domain.Trigger{ID: "t-lpr", Name: "Plates", Type: domain.TriggerRandom, EventType: "lpr", MinSeconds: 1, MaxSeconds: 1,
			PlateMasks: []string{"AA999AA"}, Speed: &domain.SpeedRange{Min: 30, Max: 40, Limit: 35, Unit: "km/h"}}
		if err := svc.conn.Request(ctx, ipc.TypeReload, ipc.Reload{VCA: &ipc.VCA{Rules: []domain.Rule{g, lot},
			Triggers: []domain.Trigger{anyRegion, anyLine, plates}}}, nil); err != nil {
			t.Fatal(err)
		}
		fire := func(id string) engine.Event {
			t.Helper()
			var res ipc.TriggerResult
			if err := svc.conn.Request(ctx, ipc.TypeTrigger, ipc.Trigger{TriggerID: id}, &res); err != nil {
				t.Fatal(err)
			}
			return res.Event
		}
		e := fire("t-region")
		o := e.Object
		// Its center is in the region, give or take the rounding.
		cx, cy := o.Box.X+o.Box.W/2, o.Box.Y+o.Box.H/2
		if e.Rule == nil || e.Rule.ID != "r-lot" || o == nil || o.Class != "car" || o.Color == "" || !boxInside(o.Box) ||
			cx < 0.198 || cx > 0.602 || cy < 0.198 || cy > 0.602 {
			t.Fatalf("region event %+v, object %+v", e, o)
		}
		// No enabled line: a default one.
		if e := fire("t-line"); e.Rule == nil || e.Rule.ID != "1" || e.Rule.Name != "Line 1" || e.Direction == "" {
			t.Fatalf("line event without a line: %+v", e)
		}
		e = fire("t-lpr")
		if e.Rule != nil || e.Plate == nil || !regexp.MustCompile(`^[A-Z]{2}[0-9]{3}[A-Z]{2}$`).MatchString(e.Plate.Text) ||
			e.Plate.Format != "AA999AA" || e.Object == nil || e.Object.Class != "car" {
			t.Fatalf("lpr event %+v, plate %+v, object %+v", e, e.Plate, e.Object)
		}
		if s := e.Speed; s == nil || s.Value < 30 || s.Value > 40 || s.Limit != 35 || s.Unit != "km/h" {
			t.Fatalf("speed %+v", e.Speed)
		}
	})

	t.Run("manual events", func(t *testing.T) {
		var res ipc.TriggerResult
		rule := lot
		err := svc.conn.Request(ctx, ipc.TypeTrigger, ipc.Trigger{Type: "region_entrance", Rule: &rule,
			Object: &engine.Object{Class: "person"}, Custom: map[string]any{"k": "v"}}, &res)
		if err != nil {
			t.Fatal(err)
		}
		e := res.Event
		if e.Trigger != "manual" || e.Rule == nil || e.Rule.Name != "Lot" || e.Object.Class != "person" || e.Object.Color != "" ||
			e.Object.Box == nil || e.Custom["k"] != "v" || e.Plate != nil || e.Speed != nil {
			t.Fatalf("manual event %+v, object %+v", e, e.Object)
		}
		if ev := eventWithID(t, svc, e.ID); ev.TriggerID != "" {
			t.Fatalf("a manual event names trigger %q", ev.TriggerID)
		}
		// The analytics switch of the profile turns crossings off.
		if err := svc.conn.Request(ctx, ipc.TypeStateSet, ipc.StateSet{Values: map[string]any{"Event.Line.Enable": false}}, nil); err != nil {
			t.Fatal(err)
		}
		err = svc.conn.Request(ctx, ipc.TypeTrigger, ipc.Trigger{Type: "line_crossing"}, nil)
		if err == nil || !strings.Contains(err.Error(), "line_crossing detection is turned off") {
			t.Fatalf("a crossing with detection off: %v", err)
		}
	})

	if err := svc.conn.Request(ctx, ipc.TypeStop, ipc.Stop{DeadlineMS: 2000}, nil); err != nil {
		t.Fatal(err)
	}
	svc.wait(t, ipc.TypeBye, 5*time.Second)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	rt.vca.mu.Lock()
	loops := len(rt.vca.loops)
	rt.vca.mu.Unlock()
	if loops != 0 {
		t.Fatalf("%d trigger loops outlived the camera", loops)
	}
}

func TestEventData(t *testing.T) {
	doc, err := profile.DecodeJSON(vcaProfile(t))
	if err != nil {
		t.Fatal(err)
	}
	rt := NewRuntime(Options{})
	rt.model = profile.NewModel(doc)

	line := &domain.Rule{Type: domain.RuleLine, Points: []domain.Point{{X: 0.2, Y: 0.2}, {X: 0.2, Y: 0.8}}, Direction: domain.CrossBoth}
	for range 50 {
		// A vertical line drawn downwards: side A is on its right.
		ab := boxFor("person", line, domain.CrossAB)
		ba := boxFor("person", line, domain.CrossBA)
		if ab.X+ab.W/2 > 0.2 || ba.X+ba.W/2 < 0.2 {
			t.Fatalf("A->B ends at x %.3f, B->A at x %.3f", ab.X+ab.W/2, ba.X+ba.W/2)
		}
		if b := boxFor("truck", nil, ""); !boxInside(&b) {
			t.Fatalf("box %+v", b)
		}
	}
	seen := map[string]bool{}
	for range 200 {
		seen[crossing(line, true)] = true
		if crossing(line, false) != domain.CrossAB {
			t.Fatal("a manual crossing of a two-way line goes A->B")
		}
	}
	if !seen[domain.CrossAB] || !seen[domain.CrossBA] {
		t.Fatalf("random crossings of a two-way line: %v", seen)
	}

	for range 50 {
		p := plate(nil, []string{"LIST1"}, []string{`\A99`})
		if p.Text != "LIST1" && !regexp.MustCompile(`^A[0-9]{2}$`).MatchString(p.Text) || p.Confidence < 0.85 {
			t.Fatalf("plate %+v", p)
		}
	}
	if p := plate(&engine.Plate{Text: "AB123CD"}, nil, nil); p.Text != "AB123CD" || p.Confidence == 0 {
		t.Fatalf("given plate %+v", p)
	}
	if s := speed(&engine.Speed{Value: 88}, nil); s.Value != 88 || s.Unit != "km/h" {
		t.Fatalf("given speed %+v", s)
	}
	// Without a rule, lpr objects are the profile's vehicles.
	if o := rt.vca.object(nil, "lpr", nil, ""); o.Class != "car" || o.Color == "" {
		t.Fatalf("lpr object %+v", o)
	}
	if o := rt.vca.object(&engine.Object{Class: "person", Confidence: 0.5}, "line_crossing", line, domain.CrossAB); o.Confidence != 0.5 || o.Color != "" || o.Box == nil {
		t.Fatalf("given object %+v", o)
	}
}
