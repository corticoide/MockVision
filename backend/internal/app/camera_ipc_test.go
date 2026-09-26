package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/telemetry"
	"github.com/corticoide/mockvision/sdk/engine"
)

func TestDeliveryStatus(t *testing.T) {
	ok := DeliveryView{TargetID: "a", Attempt: 1, Status: engine.DeliveryOK}
	retry := DeliveryView{TargetID: "b", Attempt: 1, Status: engine.DeliveryRetry}
	failed := DeliveryView{TargetID: "b", Attempt: 2, Status: engine.DeliveryFailed}
	for _, c := range []struct {
		dels     []DeliveryView
		expected int
		want     string
	}{
		{nil, 0, "none"},
		{nil, -1, "none"}, // stored before the count was kept
		{nil, 2, "pending"},
		{[]DeliveryView{ok}, 1, "ok"},
		{[]DeliveryView{ok}, 2, "pending"}, // the other target has not answered yet
		{[]DeliveryView{ok, retry}, 2, "pending"},
		{[]DeliveryView{ok, retry, failed}, 2, "failed"},
		{[]DeliveryView{ok}, -1, "ok"},
	} {
		if got := deliveryStatus(c.dels, c.expected); got != c.want {
			t.Errorf("%d deliveries, %d expected: %s, want %s", len(c.dels), c.expected, got, c.want)
		}
	}
}

// What a camera reports is checked again by the service: a camera serves
// the LAN and must not be able to write what it likes (audit B1).
func TestCameraReportsAreChecked(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	camA := createCamera(t, svc, "Reporter", false)
	camB := createCamera(t, svc, "Other", false)

	event := func(id, typ string) engine.Event {
		return engine.Event{ID: id, Type: typ, At: time.Now()}
	}
	good := ulid.Make().String()
	svc.recordEvent(ctx, camA.ID, event(good, "line_crossing"))
	if _, err := svc.GetEvent(ctx, good); err != nil {
		t.Fatalf("a valid event must be stored: %v", err)
	}
	stored, _ := svc.GetEvent(ctx, good)
	if stored.DeliveryStatus != "none" {
		t.Fatalf("an event of a camera without targets: %s, want none", stored.DeliveryStatus)
	}
	old := ulid.MustNew(ulid.Timestamp(time.Now().Add(-48*time.Hour)), ulid.DefaultEntropy()).String()
	for name, e := range map[string]engine.Event{
		"not a ULID":     event("ZZZZZZZZZZZZZZZZZZZZZZZZZZ-", "line_crossing"),
		"old ID":         event(old, "line_crossing"),
		"unknown type":   event(ulid.Make().String(), "custom:nope"),
		"oversized data": {ID: ulid.Make().String(), Type: "line_crossing", At: time.Now(), Custom: map[string]any{"x": string(make([]byte, maxEventBytes))}},
	} {
		svc.recordEvent(ctx, camA.ID, e)
		if _, err := svc.GetEvent(ctx, e.ID); err == nil {
			t.Errorf("%s: the event was stored", name)
		}
	}

	// A delivery must be about an event of the camera that reports it.
	svc.recordDelivery(ctx, camB.ID, engine.DeliveryReport{EventID: good, TargetID: "x", Attempt: 1, At: time.Now(), Status: engine.DeliveryOK})
	if v, _ := svc.GetEvent(ctx, good); len(v.Deliveries) != 0 {
		t.Fatal("a camera wrote a delivery on another camera's event")
	}

	// Parameter changes are checked against the profile: unknown keys and
	// invalid values are dropped, and the binding comes from the profile.
	svc.clientChanges(ctx, camA.ID, []engine.Change{
		{Key: "Image.Brightness", Value: "70", Bind: "media.main.codec", Origin: engine.Origin{Kind: engine.OriginClient, IP: "10.0.0.9"}},
		{Key: "No.Such.Param", Value: 1},
		{Key: "Encode.Main.FrameRate", Value: 999},
	})
	params, err := svc.CameraConfig(ctx, camA.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range params {
		switch p.Key {
		case "Image.Brightness":
			if raw, _ := json.Marshal(p.Value); string(raw) != "70" || p.Origin != "client:10.0.0.9" {
				t.Errorf("brightness: %s from %s", raw, p.Origin)
			}
		case "Encode.Main.FrameRate":
			if p.Origin == "client:10.0.0.9" {
				t.Error("an out of range value was stored")
			}
		case "No.Such.Param":
			t.Error("an unknown parameter was stored")
		}
	}
	svc.mu.Lock()
	regen := svc.regens[camA.ID]
	svc.mu.Unlock()
	if regen != nil {
		t.Error("a binding claimed by the camera triggered a re-encoding")
	}

	// Retention goes by when the node received an event, not by the
	// camera's clock, which its clients can move (audit B15).
	skewed := ulid.Make().String()
	svc.recordEvent(ctx, camA.ID, engine.Event{ID: skewed, Type: "line_crossing", At: time.Now().AddDate(-1, 0, 0)})
	svc.applyRetention(ctx)
	if _, err := svc.GetEvent(ctx, skewed); err != nil {
		t.Fatal("an event just received was deleted because of the camera's clock")
	}
}

// A camera that floods heartbeats keeps one sample a second.
func TestHeartbeatSamplesAreBounded(t *testing.T) {
	c := telemetry.NewCameras()
	start := time.Now()
	for i := range 10 * telemetry.MaxSamples {
		c.Add("cam", telemetry.CameraSample{At: start.Add(time.Duration(i) * time.Millisecond)})
	}
	if n := len(c.History("cam", time.Time{})); n > telemetry.MaxSamples {
		t.Fatalf("%d samples kept", n)
	}
}
