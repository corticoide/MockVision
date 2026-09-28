package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/sdk/engine"
)

// receiver is an event target that keeps what cameras push.
type receiver struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var body map[string]any
	raw, _ := io.ReadAll(req.Body)
	_ = json.Unmarshal(raw, &body)
	r.mu.Lock()
	r.bodies = append(r.bodies, body)
	r.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// payload waits for the payload of an event.
func (r *receiver) payload(t *testing.T, eventID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, b := range r.bodies {
			if b["eventId"] == eventID {
				r.mu.Unlock()
				return b
			}
		}
		r.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the target did not receive event %s", eventID)
	return nil
}

// fromTrigger counts the stored events of a camera's trigger.
func fromTrigger(t *testing.T, svc *Service, cameraID, triggerID string) int {
	t.Helper()
	page, err := svc.ListEvents(context.Background(), EventFilter{CameraID: cameraID, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range page.Items {
		if e.TriggerID == triggerID {
			n++
		}
	}
	return n
}

// storedEvent waits for the service to store an event the camera reported:
// the API answers with the event before the camera's notice arrives.
func storedEvent(t *testing.T, svc *Service, id string) *EventView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ev, err := svc.GetEvent(context.Background(), id)
		if err == nil {
			return ev
		}
		if !errors.Is(err, domain.ErrNotFound) || time.Now().After(deadline) {
			t.Fatalf("event %s: %v", id, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func wantConflict(t *testing.T, err error, contains string) {
	t.Helper()
	var ce *domain.ConflictError
	if !errors.As(err, &ce) || !strings.Contains(ce.Error(), contains) {
		t.Fatalf("want a conflict about %q, got %v", contains, err)
	}
}

func TestRulesAndTriggers(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	rcv := &receiver{}
	srv := httptest.NewServer(rcv)
	defer srv.Close()
	name, url := "Receiver", srv.URL+"/events"
	tg, err := svc.CreateTarget(ctx, testActor, TargetInput{Name: &name, URL: &url})
	if err != nil {
		t.Fatal(err)
	}
	cam := createCamera(t, svc, "Gate 1", false)
	ids := []string{tg.ID}
	if _, err := svc.UpdateCamera(ctx, testActor, cam.ID, UpdateCameraInput{TargetIDs: &ids}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartCamera(ctx, testActor, cam.ID); err != nil {
		t.Fatal(err)
	}
	cam = waitState(t, svc, cam.ID, domain.StateRunning)
	pid := cam.Status.PID

	// A new camera has its profile's factory rules, and knows the events it
	// can send and where they come from.
	if len(cam.Rules) != 2 || cam.Rules[0].Name != "Line 1" || cam.Rules[1].Name != "Region 1" || cam.Rules[0].ID == "" {
		t.Fatalf("factory rules %+v", cam.Rules)
	}
	kinds := map[string]CameraEventView{}
	for _, e := range cam.EventTypes {
		kinds[e.Type] = e
	}
	if kinds["line_crossing"].Rule != "line" || kinds["loitering"].Rule != "region" || !kinds["custom:people_counting"].Report ||
		kinds["custom:people_counting"].Rule != "" || len(cam.EventTypes) != 6 {
		t.Fatalf("event types %+v", cam.EventTypes)
	}

	// Without rules, events that come from rules cannot happen.
	if _, err := svc.SetCameraRules(ctx, testActor, cam.ID, []RuleInput{}); err != nil {
		t.Fatal(err)
	}
	_, err = svc.TriggerEvent(ctx, testActor, cam.ID, ManualEventInput{Type: "line_crossing"})
	wantInvalid(t, err, "rule_id")

	// Rules are checked against the profile.
	_, err = svc.SetCameraRules(ctx, testActor, cam.ID, []RuleInput{{Name: "Lot", Type: "region", Points: []domain.Point{{X: 0.1, Y: 0.1}, {X: 0.5, Y: 0.5}},
		Events: []string{"loitering"}}})
	wantInvalid(t, err, "rules[0].points")
	_, err = svc.SetCameraRules(ctx, testActor, cam.ID, []RuleInput{{ID: "made-up", Name: "Gate", Type: "line", Points: []domain.Point{{X: 0.1, Y: 0.5}, {X: 0.9, Y: 0.5}}}})
	wantInvalid(t, err, "rules[0].id")

	v, err := svc.SetCameraRules(ctx, testActor, cam.ID, []RuleInput{
		{Name: " Gate ", Type: "line", Points: []domain.Point{{X: 0.1, Y: 0.5}, {X: 0.9, Y: 0.5}}, Direction: domain.CrossAB},
		{Name: "Lot", Type: "region", Points: []domain.Point{{X: 0.2, Y: 0.2}, {X: 0.7, Y: 0.2}, {X: 0.7, Y: 0.8}, {X: 0.2, Y: 0.8}},
			Events: []string{"region_entrance", "loitering"}, ObjectClasses: []string{"car"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Rules) != 2 || v.Rules[0].Name != "Gate" || !v.Rules[0].Enabled || v.Rules[0].ID == "" || v.Rules[1].Direction != "" {
		t.Fatalf("rules %+v", v.Rules)
	}
	gate, lot := v.Rules[0], v.Rules[1]

	t.Run("manual events on rules", func(t *testing.T) {
		ev, err := svc.TriggerEvent(ctx, testActor, cam.ID, ManualEventInput{Type: "line_crossing", RuleID: gate.ID})
		if err != nil {
			t.Fatal(err)
		}
		if ev.RuleID != gate.ID {
			t.Fatalf("event rule %q", ev.RuleID)
		}
		p := rcv.payload(t, ev.ID)
		rule, _ := p["rule"].(map[string]any)
		object, _ := p["object"].(map[string]any)
		if rule["id"] != gate.ID || rule["name"] != "Gate" || rule["type"] != "line" || p["direction"] != "A->B" || object["box"] == nil {
			t.Fatalf("payload %v", p)
		}
		stored := storedEvent(t, svc, ev.ID)
		if stored.RuleID != gate.ID || stored.TriggerID != "" {
			t.Fatalf("stored event %+v", stored)
		}
		// Without a rule, the first enabled one that reports the type.
		ev, err = svc.TriggerEvent(ctx, testActor, cam.ID, ManualEventInput{Type: "region_entrance"})
		if err != nil || ev.RuleID != lot.ID {
			t.Fatalf("region event on rule %q, %v", ev.RuleID, err)
		}
		for _, c := range []struct {
			in    ManualEventInput
			field string
		}{
			{ManualEventInput{Type: "line_crossing", RuleID: gate.ID, Direction: "B->A"}, "direction"},
			{ManualEventInput{Type: "region_exit", RuleID: lot.ID}, "rule_id"},
			{ManualEventInput{Type: "loitering", RuleID: gate.ID}, "rule_id"},
			{ManualEventInput{Type: "loitering", RuleID: "nope"}, "rule_id"},
			{ManualEventInput{Type: "loitering", RuleID: lot.ID, Direction: "A->B"}, "direction"},
			{ManualEventInput{Type: "loitering", RuleID: lot.ID, Object: &engine.Object{Class: "person"}}, "object.class"},
			{ManualEventInput{Type: "line_crossing", Object: &engine.Object{Class: "boat"}}, "object.class"},
			{ManualEventInput{Type: "line_crossing", Plate: &engine.Plate{Text: strings.Repeat("A", 17)}}, "plate.text"},
			{ManualEventInput{Type: "line_crossing", Speed: &engine.Speed{Value: 80, Unit: "knots"}}, "speed.unit"},
		} {
			_, err := svc.TriggerEvent(ctx, testActor, cam.ID, c.in)
			wantInvalid(t, err, c.field)
		}
	})

	t.Run("analytics", func(t *testing.T) {
		// One crossing A->B on Gate and one entry into Lot so far.
		a, err := svc.CameraAnalytics(ctx, cam.ID, 8, 4)
		if err != nil {
			t.Fatal(err)
		}
		if len(a.Lines) != 1 || a.Lines[0].RuleID != gate.ID || a.Lines[0].Name != "Gate" || a.Lines[0].AToB != 1 || a.Lines[0].BToA != 0 {
			t.Fatalf("lines %+v", a.Lines)
		}
		if len(a.Regions) != 1 || a.Regions[0].Entries != 1 || a.Regions[0].Occupancy != 1 || a.Events["line_crossing"] != 1 {
			t.Fatalf("regions %+v, events %v", a.Regions, a.Events)
		}
		sum := 0
		for _, n := range a.Heat.Cells {
			sum += n
		}
		if a.Heat.Cols != 8 || a.Heat.Rows != 4 || sum != 2 {
			t.Fatalf("heat %+v", a.Heat)
		}
		// A report carries the counts to the targets.
		ev, err := svc.TriggerEvent(ctx, testActor, cam.ID, ManualEventInput{Type: "custom:people_counting"})
		if err != nil {
			t.Fatal(err)
		}
		p := rcv.payload(t, ev.ID)
		lines, _ := p["lines"].([]any)
		first, _ := lines[0].(map[string]any)
		if p["eventType"] != "PeopleCounting" || first["name"] != "Gate" || first["in"] != 1.0 || ev.RuleID != "" {
			t.Fatalf("report %v", p)
		}
		_, err = svc.CameraAnalytics(ctx, cam.ID, 0, 4)
		wantInvalid(t, err, "cols")
		_, err = svc.CameraAnalytics(ctx, cam.ID, 200, 4)
		wantInvalid(t, err, "cols")
		a, err = svc.ResetCameraAnalytics(ctx, testActor, cam.ID)
		if err != nil || a.Lines[0].AToB != 0 || a.Regions[0].Entries != 0 || a.Heat != nil {
			t.Fatalf("after a reset %+v, %v", a, err)
		}
	})

	var traffic domain.Trigger
	t.Run("random triggers", func(t *testing.T) {
		_, err := svc.SetCameraTriggers(ctx, testActor, cam.ID, []TriggerInput{{Name: "Fast", EventType: "line_crossing", MinSeconds: 1, MaxSeconds: 1, RuleID: lot.ID}})
		wantInvalid(t, err, "triggers[0].rule_id")
		v, err := svc.SetCameraTriggers(ctx, testActor, cam.ID, []TriggerInput{
			{Name: "Traffic", EventType: "line_crossing", RuleID: gate.ID, MinSeconds: 1, MaxSeconds: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		traffic = v.Triggers[0]
		if traffic.Type != domain.TriggerRandom || !traffic.Enabled || traffic.ID == "" {
			t.Fatalf("trigger %+v", traffic)
		}
		deadline := time.Now().Add(15 * time.Second)
		for fromTrigger(t, svc, cam.ID, traffic.ID) < 2 {
			if time.Now().After(deadline) {
				t.Fatalf("the random trigger raised %d events", fromTrigger(t, svc, cam.ID, traffic.ID))
			}
			time.Sleep(100 * time.Millisecond)
		}
		page, _ := svc.ListEvents(ctx, EventFilter{CameraID: cam.ID, Limit: 1})
		var data engine.Event
		_ = json.Unmarshal(page.Items[0].Data, &data)
		if data.Trigger != "random" || data.Rule == nil || data.Rule.Name != "Gate" || data.Direction != "A->B" {
			t.Fatalf("random event %+v", data)
		}
		// A rule a trigger fires on stays.
		_, err = svc.SetCameraRules(ctx, testActor, cam.ID, []RuleInput{{ID: lot.ID, Name: lot.Name, Type: "region", Points: lot.Points, Events: lot.Events}})
		wantConflict(t, err, "trigger Traffic fires on rule Gate")

		// Disabled, it stops; fired on request, it raises one event.
		off := false
		if _, err := svc.SetCameraTriggers(ctx, testActor, cam.ID, []TriggerInput{{ID: traffic.ID, Name: "Traffic", EventType: "line_crossing",
			RuleID: gate.ID, MinSeconds: 1, MaxSeconds: 1, Enabled: &off}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		before := fromTrigger(t, svc, cam.ID, traffic.ID)
		time.Sleep(2200 * time.Millisecond)
		if after := fromTrigger(t, svc, cam.ID, traffic.ID); after != before {
			t.Fatalf("a disabled trigger raised %d events", after-before)
		}
		ev, err := svc.FireTrigger(ctx, testActor, cam.ID, traffic.ID)
		if err != nil {
			t.Fatal(err)
		}
		if ev.TriggerID != traffic.ID || ev.RuleID != gate.ID {
			t.Fatalf("fired event %+v", ev)
		}
		var fired engine.Event
		_ = json.Unmarshal(ev.Data, &fired)
		if fired.Trigger != "manual" {
			t.Fatalf("a trigger fired by hand is %q", fired.Trigger)
		}
		if _, err := svc.FireTrigger(ctx, testActor, cam.ID, "nope"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("an unknown trigger: %v", err)
		}
	})

	t.Run("detection switched off", func(t *testing.T) {
		if _, err := svc.UpdateCameraConfig(ctx, testActor, cam.ID, map[string]any{"Event.LineCrossing.Enable": false}); err != nil {
			t.Fatal(err)
		}
		_, err := svc.TriggerEvent(ctx, testActor, cam.ID, ManualEventInput{Type: "line_crossing"})
		wantConflict(t, err, "line_crossing detection is turned off")
		if _, err := svc.UpdateCameraConfig(ctx, testActor, cam.ID, map[string]any{"Event.LineCrossing.Enable": true}); err != nil {
			t.Fatal(err)
		}
	})

	if v, _ := svc.GetCamera(ctx, cam.ID); v.Status.PID != pid {
		t.Fatal("rules and triggers restarted the camera")
	}

	t.Run("clone and reset", func(t *testing.T) {
		clone, err := svc.CloneCamera(ctx, testActor, cam.ID, CloneCameraInput{Name: "Gate 2"})
		if err != nil {
			t.Fatal(err)
		}
		if len(clone.Rules) != 2 || len(clone.Triggers) != 1 || clone.Rules[0].ID == gate.ID || clone.Triggers[0].ID == traffic.ID ||
			clone.Triggers[0].RuleID != clone.Rules[0].ID || clone.Rules[1].ObjectClasses[0] != "car" {
			t.Fatalf("clone rules %+v, triggers %+v", clone.Rules, clone.Triggers)
		}
		v, err := svc.ResetCamera(ctx, testActor, clone.ID, ResetSettings)
		if err != nil {
			t.Fatal(err)
		}
		// Its rules go back to the profile's; its triggers stay, unbound.
		if len(v.Rules) != 2 || v.Rules[0].Name != "Line 1" || v.Rules[0].ID == clone.Rules[0].ID || len(v.Triggers) != 1 ||
			v.Triggers[0].RuleID != "" || v.Triggers[0].ID != clone.Triggers[0].ID {
			t.Fatalf("after a reset: rules %+v, triggers %+v", v.Rules, v.Triggers)
		}
	})

	if _, err := svc.StopCamera(ctx, testActor, cam.ID); err != nil {
		t.Fatal(err)
	}
	_, err = svc.FireTrigger(ctx, testActor, cam.ID, traffic.ID)
	wantConflict(t, err, "is not running")
	_, err = svc.CameraAnalytics(ctx, cam.ID, 0, 0)
	wantConflict(t, err, "is not running")
}

// A profile without regions refuses them; one without analytics refuses
// every rule.
func TestRulesFollowTheProfile(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cam := createCamera(t, svc, "Gate 1", false)
	b, err := svc.loadBundle(ctx, cam.ID)
	if err != nil {
		t.Fatal(err)
	}
	caps := vcaCaps(b.doc)
	if len(caps.RuleTypes) != 2 || caps.Events["line_crossing"] != time.Second || len(caps.ObjectClasses) != 6 {
		t.Fatalf("demo capabilities %+v", caps)
	}
	bare := *b.doc // the service caches the document: change a copy
	bare.VCA = nil
	if err := domain.ValidateRules([]domain.Rule{{Name: "L", Type: domain.RuleLine, Points: []domain.Point{{X: 0.1, Y: 0.1}, {X: 0.9, Y: 0.9}},
		Direction: domain.CrossBoth}}, vcaCaps(&bare)); err == nil {
		t.Fatal("a profile without analytics accepted a line")
	}
	d, err := svc.GetProfile(ctx, "milesight/demo", "0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.VCA.Rules) != 2 || len(d.EventSpecs) != 6 || d.EventSpecs[2].Type != "line_crossing" || d.EventSpecs[2].MinIntervalMS != 1000 ||
		d.EventSpecs[2].Transports[0] != "http_push" || d.EventSpecs[2].Rule != "line" || !d.EventSpecs[0].Report || d.EventSpecs[0].Rule != "" {
		t.Fatalf("profile detail vca %+v, events %+v", d.VCA, d.EventSpecs)
	}

	// A profile without line crossings: its cameras cannot send them, nor
	// draw lines that report them.
	noCross := *b.doc
	noCross.Events = map[string]profile.EventSpec{}
	for typ, spec := range b.doc.Events {
		if typ != "line_crossing" {
			noCross.Events[typ] = spec
		}
	}
	for _, e := range cameraEvents(&noCross) {
		if e.Type == "line_crossing" {
			t.Fatal("a camera of a profile without crossings offers them")
		}
	}
	err = domain.ValidateRules([]domain.Rule{domain.Rule{Name: "L", Type: domain.RuleLine, Points: []domain.Point{{X: 0.1, Y: 0.1}, {X: 0.9, Y: 0.9}}}.Normalized()}, vcaCaps(&noCross))
	if err == nil || !strings.Contains(err.Error(), "does not deliver line_crossing") {
		t.Fatalf("a line on a profile without crossings: %v", err)
	}

	// Factory rules come once: rules removed later stay removed at boot.
	if _, err := svc.SetCameraRules(ctx, testActor, cam.ID, []RuleInput{}); err != nil {
		t.Fatal(err)
	}
	if err := svc.applyFactoryRules(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := svc.GetCamera(ctx, cam.ID); len(v.Rules) != 0 {
		t.Fatalf("factory rules came back: %+v", v.Rules)
	}
}
