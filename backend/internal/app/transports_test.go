package app

import (
	"bytes"
	"context"
	"net/mail"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/engines/enginetest"
	"github.com/corticoide/mockvision/backend/internal/netctl"
)

func TestTargetsOfEveryTypeAreValidated(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	create := func(in TargetInput) (*TargetView, error) { return svc.CreateTarget(ctx, testActor, in) }

	m, err := create(TargetInput{Name: ptr("Broker"), Type: "mqtt", URL: ptr("mqtts://broker.lab:8884"), Username: ptr("cam"), Password: ptr("pw"),
		Topic: ptr("site/{{ .Camera.Serial }}"), ClientID: ptr("cam-{{ .Camera.Serial }}"), Insecure: ptr(true)})
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != "mqtt" || m.Topic != "site/{{ .Camera.Serial }}" || m.ClientID != "cam-{{ .Camera.Serial }}" || !m.Insecure || !m.HasPassword || m.Method != "" {
		t.Fatalf("mqtt target %+v", m)
	}
	s, err := create(TargetInput{Name: ptr("Mail"), Type: "smtp", URL: ptr("smtp://mail.lab:587"), TLS: ptr("starttls"), From: ptr("cams@lab.local"),
		To: &[]string{"ops@lab.local", " ", "guard@lab.local"}, Delivery: &domain.DeliveryOverride{Retries: ptr(0), TimeoutMS: ptr(int64(15000))}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.To, []string{"ops@lab.local", "guard@lab.local"}) || s.TLS != "starttls" || *s.Delivery.Retries != 0 || *s.Delivery.TimeoutMS != 15000 {
		t.Fatalf("smtp target %+v", s)
	}
	if _, err := create(TargetInput{Name: ptr("Files"), Type: "sftp", URL: ptr("sftp://nas.lab/srv/cams"), Username: ptr("cam"), HostKey: ptr("SHA256:" + strings.Repeat("b", 43))}); err != nil {
		t.Fatal(err)
	}
	h, err := create(TargetInput{Name: ptr("VMS"), URL: ptr("http://vms.lab/events"), Auth: ptr("digest")})
	if err != nil || h.Type != "http" || h.Method != "POST" || h.Auth != "digest" {
		t.Fatalf("http target %+v %v", h, err)
	}

	for field, in := range map[string]TargetInput{
		"url":                 {Name: ptr("x1"), Type: "mqtt", URL: ptr("mqtt://broker.lab/topic")},
		"topic":               {Name: ptr("x2"), Type: "mqtt", URL: ptr("mqtt://broker.lab"), Topic: ptr("cams/#")},
		"client_id":           {Name: ptr("x3"), Type: "mqtt", URL: ptr("mqtt://broker.lab"), ClientID: ptr("{{ .Camera.Serial")},
		"to":                  {Name: ptr("x4"), Type: "smtp", URL: ptr("smtp://mail.lab"), From: ptr("a@lab.local")},
		"from":                {Name: ptr("x5"), Type: "smtp", URL: ptr("smtp://mail.lab"), From: ptr("Cams <a@lab.local>"), To: &[]string{"b@lab.local"}},
		"tls":                 {Name: ptr("x6"), Type: "smtp", URL: ptr("smtp://mail.lab"), From: ptr("a@lab.local"), To: &[]string{"b@lab.local"}, TLS: ptr("ssl")},
		"host_key":            {Name: ptr("x7"), Type: "sftp", URL: ptr("sftp://nas.lab"), HostKey: ptr("md5:aa")},
		"auth":                {Name: ptr("x8"), URL: ptr("http://vms.lab"), Auth: ptr("ntlm")},
		"type":                {Name: ptr("x9"), Type: "snmp", URL: ptr("udp://x")},
		"delivery.retries":    {Name: ptr("x10"), Type: "ftp", URL: ptr("ftp://nas.lab"), Delivery: &domain.DeliveryOverride{Retries: ptr(11)}},
		"delivery.timeout_ms": {Name: ptr("x11"), Type: "ftp", URL: ptr("ftp://nas.lab"), Delivery: &domain.DeliveryOverride{TimeoutMS: ptr(int64(5))}},
	} {
		_, err := create(in)
		wantInvalid(t, err, field)
	}
	// An FTP target is not an HTTP one.
	_, err = create(TargetInput{Name: ptr("x12"), Type: "ftp", URL: ptr("https://nas.lab")})
	wantInvalid(t, err, "url")

	// The type stays; an empty delivery override goes back to the profile's.
	_, err = svc.UpdateTarget(ctx, testActor, m.ID, TargetInput{Type: "http"})
	wantInvalid(t, err, "type")
	s, err = svc.UpdateTarget(ctx, testActor, s.ID, TargetInput{Delivery: &domain.DeliveryOverride{}, To: &[]string{"night@lab.local"}})
	if err != nil || s.Delivery != nil || !slices.Equal(s.To, []string{"night@lab.local"}) || s.From != "cams@lab.local" {
		t.Fatalf("update %+v %v", s, err)
	}
}

// A camera delivers its events over every transport of its profile to the
// targets of the matching types: published to a broker it connected to as
// it started, uploaded to an FTP server and mailed with its snapshot; the
// mail interval skips the next one.
func TestCameraDeliversOverEveryTransport(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	broker := enginetest.NewMQTTBroker(t, "pw")
	ftp := enginetest.NewFTPServer(t)
	smtp := enginetest.NewSMTPServer(t, false)

	var ids []string
	for _, in := range []TargetInput{
		{Name: ptr("Broker"), Type: "mqtt", URL: ptr(broker.URL()), Username: ptr("cam"), Password: ptr("pw")},
		{Name: ptr("NAS"), Type: "ftp", URL: ptr("ftp://" + ftp.Addr() + "/cams"), Username: ptr("cam"), Password: ptr("secret")},
		{Name: ptr("Mail"), Type: "smtp", URL: ptr("smtp://" + smtp.Addr()), Username: ptr("cam"), Password: ptr("secret"), From: ptr("gate@lab.local"), To: &[]string{"ops@lab.local"}},
	} {
		tg, err := svc.CreateTarget(ctx, testActor, in)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, tg.ID)
	}
	cam := createCamera(t, svc, "Gate 1", false)
	if _, err := svc.UpdateCamera(ctx, testActor, cam.ID, UpdateCameraInput{TargetIDs: &ids}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartCamera(ctx, testActor, cam.ID); err != nil {
		t.Fatal(err)
	}
	cam = waitState(t, svc, cam.ID, domain.StateRunning)
	serial := cam.Serial

	// Connected and online before any event.
	birth := broker.WaitMessages(t, 1)[0]
	if c := broker.Connects()[0]; c.ClientID != serial || c.Will == nil || string(c.Will.Payload) != "offline" ||
		birth.Topic != "milesight/"+serial+"/status" || string(birth.Payload) != "online" || !birth.Retain {
		t.Fatalf("connect %+v, birth %+v", c, birth)
	}

	ev, err := svc.TriggerEvent(ctx, testActor, cam.ID, ManualEventInput{Type: "line_crossing"})
	if err != nil {
		t.Fatal(err)
	}
	msgs := broker.WaitMessages(t, 2)
	if m := msgs[1]; m.Topic != "milesight/"+serial+"/event/LineCrossing" || m.QoS != 1 || !bytes.Contains(m.Payload, []byte(ev.ID)) {
		t.Fatalf("event message %+v", m)
	}
	files := ftp.WaitFiles(t, 1)
	day := ev.At.UTC().Format("2006-01-02")
	if !strings.HasPrefix(files[0], "/home/cams/"+serial+"/"+day+"/") || !strings.HasSuffix(files[0], "_LineCrossing.jpg") {
		t.Fatalf("uploaded %v", files)
	}
	if jpeg, _ := ftp.File(files[0]); len(jpeg) < 100 || jpeg[0] != 0xff || jpeg[1] != 0xd8 {
		t.Fatalf("the upload is not the snapshot: %d bytes", len(jpeg))
	}
	mails := smtp.WaitMails(t, 1)
	msg, err := mail.ReadMessage(bytes.NewReader(mails[0].Data))
	if err != nil || !strings.Contains(msg.Header.Get("Content-Type"), "multipart/mixed") || mails[0].User != "cam" {
		t.Fatalf("mail %+v %v", msg.Header, err)
	}

	stored := waitDeliveries(t, svc, ev.ID, 3)
	if stored.DeliveryStatus != "ok" {
		t.Fatalf("delivery status %s: %+v", stored.DeliveryStatus, stored.Deliveries)
	}

	// The next mail within the profile's 10 s interval is skipped; the rest
	// go out.
	time.Sleep(1100 * time.Millisecond) // the event's own minimum interval
	ev2, err := svc.TriggerEvent(ctx, testActor, cam.ID, ManualEventInput{Type: "line_crossing"})
	if err != nil {
		t.Fatal(err)
	}
	stored = waitDeliveries(t, svc, ev2.ID, 3)
	var skipped int
	for _, d := range stored.Deliveries {
		if d.Status == string(domain.DeliverySkipped) {
			skipped++
			if d.TargetName != "Mail" || !strings.Contains(d.Error, "one mail every 10s") {
				t.Fatalf("skipped %+v", d)
			}
		}
	}
	if skipped != 1 || stored.DeliveryStatus != "ok" || len(smtp.Mails()) != 1 {
		t.Fatalf("second event: %+v, %d mails", stored.Deliveries, len(smtp.Mails()))
	}

	// Each target is tested from the camera: a session, a login, the
	// recipients; no mail goes out.
	for _, id := range ids {
		res, err := svc.TestTarget(ctx, id)
		if err != nil || !res.OK || res.From != "camera" {
			t.Fatalf("test of %s: %+v %v", id, res, err)
		}
	}
	if len(smtp.Mails()) != 1 {
		t.Fatal("the connection test sent a mail")
	}
}

func waitDeliveries(t *testing.T, svc *Service, eventID string, n int) *EventView {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		ev := storedEvent(t, svc, eventID)
		if len(ev.Deliveries) >= n {
			return ev
		}
		if time.Now().After(deadline) {
			t.Fatalf("event %s has %d deliveries, want %d: %+v", eventID, len(ev.Deliveries), n, ev.Deliveries)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestTargetTestFromTheNodeSpeaksEachProtocol(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	broker := enginetest.NewMQTTBroker(t, "pw")
	ftp := enginetest.NewFTPServer(t)
	smtp := enginetest.NewSMTPServer(t, false)
	for _, tc := range []struct {
		in TargetInput
		ok bool
	}{
		{TargetInput{Name: ptr("Broker"), Type: "mqtt", URL: ptr(broker.URL()), Username: ptr("cam"), Password: ptr("pw")}, true},
		{TargetInput{Name: ptr("Broker bad"), Type: "mqtt", URL: ptr(broker.URL()), Username: ptr("cam"), Password: ptr("no")}, false},
		{TargetInput{Name: ptr("NAS"), Type: "ftp", URL: ptr("ftp://" + ftp.Addr() + "/probe"), Username: ptr("cam"), Password: ptr("secret")}, true},
		{TargetInput{Name: ptr("Mail"), Type: "smtp", URL: ptr("smtp://" + smtp.Addr()), From: ptr("a@lab.local"), To: &[]string{"nobody@lab.local"}}, false},
	} {
		tg, err := svc.CreateTarget(ctx, testActor, tc.in)
		if err != nil {
			t.Fatal(err)
		}
		res, err := svc.TestTarget(ctx, tg.ID)
		if err != nil || res.OK != tc.ok || res.From != "node" {
			t.Errorf("%s: %+v %v", *tc.in.Name, res, err)
		}
	}
}

func TestFirewallOpensTheProtocolsPorts(t *testing.T) {
	for _, tc := range []struct {
		typ, cfg string
		host     string
		ports    []int
	}{
		{"http", `{"url":"https://vms.lab/x"}`, "vms.lab", []int{443}},
		{"mqtt", `{"url":"mqtt://10.0.0.5"}`, "10.0.0.5", []int{1883}},
		{"mqtt", `{"url":"mqtts://10.0.0.5"}`, "10.0.0.5", []int{8883}},
		{"ftp", `{"url":"ftp://10.0.0.6:2121/up"}`, "10.0.0.6", []int{2121, netctl.AnyPort}},
		{"sftp", `{"url":"sftp://10.0.0.7/up"}`, "10.0.0.7", []int{22}},
		{"smtp", `{"url":"smtp://10.0.0.8","tls":"tls"}`, "10.0.0.8", []int{465}},
		{"smtp", `{"url":"smtp://10.0.0.8:587","tls":"starttls"}`, "10.0.0.8", []int{587}},
	} {
		host, ports, ok := targetPorts(tc.typ, tc.cfg)
		if !ok || host != tc.host || !slices.Equal(ports, tc.ports) {
			t.Errorf("%s %s: %s %v %v", tc.typ, tc.cfg, host, ports, ok)
		}
	}
}
