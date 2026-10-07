package mqttpub

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/engines/enginetest"
	"github.com/corticoide/mockvision/sdk/engine"
)

const engineConfig = `{"engine": "mqtt-publish@^1", "keepalive": "30s",
  "birth": {"topic": "acme/{{ .Camera.Serial }}/status", "body": "online", "qos": 1, "retain": true},
  "will": {"topic": "acme/{{ .Camera.Serial }}/status", "body": "offline", "qos": 1, "retain": true}}`

func TestEngineConnectsAtStartAndPublishesEvents(t *testing.T) {
	syncEvery = 50 * time.Millisecond
	b := enginetest.NewMQTTBroker(t, "pw")
	h := enginetest.NewHost(t)
	h.SetTargets(engine.Target{ID: "T1", Name: "Broker", Type: engine.TargetMQTT, URL: b.URL(), Username: "cam", Password: "pw"})
	h.Start(New(), engineConfig)

	// Connected before any event, announcing itself.
	msgs := b.WaitMessages(t, 1)
	if m := msgs[0]; m.Topic != "acme/SN123/status" || string(m.Payload) != "online" || !m.Retain || m.QoS != 1 {
		t.Fatalf("birth %+v", m)
	}
	c := b.Connects()[0]
	if c.ClientID != "SN123" || c.Username != "cam" || c.KeepAlive != 30 || c.Will == nil || string(c.Will.Payload) != "offline" || !c.Will.Retain {
		t.Fatalf("connect %+v", c)
	}

	for _, qos := range []string{"0", "1", "2"} {
		h.Dispatch(engine.TransportMQTT, `{"topic": "acme/{{ .Camera.Serial }}/{{ .EventName }}", "qos": `+qos+`, "body": "{\"id\": {{ json .Event.ID }}}"}`,
			engine.Event{ID: "E" + qos, Type: "line_crossing"}, engine.DeliveryPolicy{Timeout: 2 * time.Second})
	}
	msgs = b.WaitMessages(t, 4)
	reps := h.WaitReports(3, 5*time.Second)
	for _, r := range reps {
		if r.Status != engine.DeliveryOK {
			t.Fatalf("report %+v", r)
		}
	}
	// Deliveries run in parallel: the three come in any order.
	qos := map[byte]string{}
	for _, m := range msgs[1:] {
		if m.Topic != "acme/SN123/LineCrossing" {
			t.Fatalf("message %+v", m)
		}
		qos[m.QoS] = string(m.Payload)
	}
	for q := byte(0); q <= 2; q++ {
		if qos[q] != fmt.Sprintf(`{"id": "E%d"}`, q) {
			t.Fatalf("QoS %d message %q", q, qos[q])
		}
	}
	if n := len(b.Connects()); n != 1 {
		t.Errorf("%d connections for three events, want the one session", n)
	}
}

func TestEngineReconnectsAndTargetsReplaceTopicAndClientID(t *testing.T) {
	syncEvery = 50 * time.Millisecond
	b := enginetest.NewMQTTBroker(t, "")
	h := enginetest.NewHost(t)
	h.SetTargets(engine.Target{ID: "T1", Name: "Broker", Type: engine.TargetMQTT, URL: b.URL(), Topic: "site/{{ .Camera.Name }}", ClientID: "cam-{{ .Camera.Serial }}"})
	h.Start(New(), `{"engine": "mqtt-publish@^1"}`)
	b.WaitConnects(t, 1)
	// The broker restarts: the camera connects again on its own.
	b.DropAll()
	b.WaitConnects(t, 2)

	h.Dispatch(engine.TransportMQTT, `{"topic": "ignored", "body": "x"}`, engine.Event{Type: "motion"}, engine.DeliveryPolicy{Timeout: 2 * time.Second})
	msgs := b.WaitMessages(t, 1)
	if msgs[0].Topic != "site/Gate" {
		t.Fatalf("topic %q", msgs[0].Topic)
	}
	if id := b.Connects()[0].ClientID; id != "cam-SN123" {
		t.Fatalf("client ID %q", id)
	}
}

func TestEngineReportsARefusedConnection(t *testing.T) {
	syncEvery = time.Hour // only the delivery connects
	b := enginetest.NewMQTTBroker(t, "right")
	h := enginetest.NewHost(t)
	h.SetTargets(engine.Target{ID: "T1", Name: "Broker", Type: engine.TargetMQTT, URL: b.URL(), Username: "cam", Password: "wrong"})
	e := New()
	h.Start(e, `{"engine": "mqtt-publish@^1"}`)
	h.Dispatch(engine.TransportMQTT, `{"topic": "t", "body": "x"}`, engine.Event{Type: "motion"}, engine.DeliveryPolicy{Timeout: time.Second, Retries: 1, Backoff: 10 * time.Millisecond})
	reps := h.WaitReports(2, 5*time.Second)
	if reps[1].Status != engine.DeliveryFailed || !strings.Contains(reps[1].Error, "bad user name or password (4)") {
		t.Fatalf("reports %+v", reps)
	}
	if hl := e.Health(); hl.State != engine.HealthDegraded || !strings.Contains(hl.Detail, "Broker") {
		t.Errorf("health %+v", hl)
	}
}

func TestProbe(t *testing.T) {
	b := enginetest.NewMQTTBroker(t, "pw")
	ctxTarget := engine.Target{Type: engine.TargetMQTT, URL: b.URL(), Username: "u", Password: "pw"}
	if err := Probe(t.Context(), ctxTarget, "probe-1"); err != nil {
		t.Fatal(err)
	}
	ctxTarget.Password = "nope"
	if err := Probe(t.Context(), ctxTarget, "probe-2"); err == nil {
		t.Fatal("a refused login passed the test")
	}
	if _, _, err := Address("http://x"); err == nil {
		t.Fatal("an http URL was taken for a broker")
	}
	if a, tls, err := Address("mqtts://broker.lan"); err != nil || a != "broker.lan:8883" || !tls {
		t.Fatalf("mqtts default: %s %v %v", a, tls, err)
	}
}
