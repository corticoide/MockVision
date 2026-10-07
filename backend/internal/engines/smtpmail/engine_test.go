package smtpmail

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/engines/enginetest"
	"github.com/corticoide/mockvision/sdk/engine"
)

const section = `{"subject": "{{ .EventName }} on {{ state \"System.DeviceName\" }}", "snapshot": true,
  "attachment": "{{ .EventName }}.jpg", "body": "Event {{ .EventName }} at {{ fmtTime .Event.At \"rfc3339\" }}\nCafé ✓"}`

func TestMailsTheEventWithItsSnapshot(t *testing.T) {
	srv := enginetest.NewSMTPServer(t, true)
	h := enginetest.NewHost(t)
	h.SetTargets(engine.Target{ID: "T1", Name: "Mail", Type: engine.TargetSMTP, URL: "smtp://" + srv.Addr(), TLS: TLSStartTLS, Insecure: true,
		Username: "cam", Password: "secret", From: "gate@lab.local", To: []string{"ops@lab.local", "guard@lab.local"}})
	h.Start(New(), `{"engine": "smtp-mail@^1", "mailer": "Acme-Mailer"}`)
	h.Dispatch(engine.TransportSMTP, section, engine.Event{ID: "01ABC", Type: "line_crossing"}, engine.DeliveryPolicy{Timeout: 3 * time.Second})
	reps := h.WaitReports(1, 5*time.Second)
	if reps[0].Status != engine.DeliveryOK {
		t.Fatalf("report %+v; server log %q", reps[0], srv.Log())
	}
	mails := srv.Mails()
	if len(mails) != 1 {
		t.Fatalf("%d mails", len(mails))
	}
	m := mails[0]
	if !m.TLS || m.User != "cam" || m.From != "gate@lab.local" || strings.Join(m.To, ",") != "ops@lab.local,guard@lab.local" || m.Helo != "[10.0.0.10]" {
		t.Fatalf("envelope %+v", m)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(m.Data))
	if err != nil {
		t.Fatal(err)
	}
	subject, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	from, _ := mail.ParseAddress(msg.Header.Get("From"))
	if subject != "LineCrossing on Gate camera" || from.Name != "Gate" || from.Address != "gate@lab.local" || msg.Header.Get("X-Mailer") != "Acme-Mailer" ||
		msg.Header.Get("Date") != "Wed, 07 Oct 2026 14:30:05 +0000" || !strings.HasPrefix(msg.Header.Get("Message-Id"), "<01abc.t1@") {
		t.Fatalf("headers %v", msg.Header)
	}
	_, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	mr := multipart.NewReader(msg.Body, params["boundary"])
	text, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(text) // multipart decodes quoted-printable
	if string(body) != "Event LineCrossing at 2026-10-07T14:30:05Z\r\nCafé ✓" {
		t.Fatalf("text %q", body)
	}
	img, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(base64.NewDecoder(base64.StdEncoding, img))
	if img.FileName() != "LineCrossing.jpg" || img.Header.Get("Content-Type") != `image/jpeg; name=LineCrossing.jpg` || !bytes.Equal(raw, enginetest.Snapshot) {
		t.Fatalf("attachment %q %v %q", img.FileName(), img.Header, raw)
	}
}

func TestMailIntervalAndRefusals(t *testing.T) {
	srv := enginetest.NewSMTPServer(t, false)
	h := enginetest.NewHost(t)
	h.SetTargets(
		engine.Target{ID: "A", Type: engine.TargetSMTP, URL: "smtp://" + srv.Addr(), From: "gate@lab.local", To: []string{"ops@lab.local"}},
		engine.Target{ID: "B", Type: engine.TargetSMTP, URL: "smtp://" + srv.Addr(), From: "gate@lab.local", To: []string{"nobody@lab.local"}},
		engine.Target{ID: "C", Type: engine.TargetSMTP, URL: "smtp://" + srv.Addr(), From: "gate@lab.local", To: []string{"ops@lab.local"}, Username: "cam", Password: "wrong"},
		// The server offers no STARTTLS.
		engine.Target{ID: "D", Type: engine.TargetSMTP, URL: "smtp://" + srv.Addr(), TLS: TLSStartTLS, From: "gate@lab.local", To: []string{"ops@lab.local"}},
	)
	h.Start(New(), `{"engine": "smtp-mail@^1", "interval": "1h"}`)
	h.Dispatch(engine.TransportSMTP, `{"subject": "s", "body": "b"}`, engine.Event{ID: "E1", Type: "motion"}, engine.DeliveryPolicy{Timeout: 3 * time.Second})
	reps := h.WaitReports(4, 5*time.Second)
	by := map[string]engine.DeliveryReport{}
	for _, r := range reps {
		by[r.TargetID] = r
	}
	if by["A"].Status != engine.DeliveryOK {
		t.Errorf("A: %+v", by["A"])
	}
	if b := by["B"]; b.Status != engine.DeliveryFailed || !strings.Contains(b.Error, "recipient nobody@lab.local refused: 550") {
		t.Errorf("B: %+v", b)
	}
	if c := by["C"]; c.Status != engine.DeliveryFailed || !strings.Contains(c.Error, "authentication failed: 535") {
		t.Errorf("C: %+v", c)
	}
	if d := by["D"]; d.Status != engine.DeliveryFailed || !strings.Contains(d.Error, "does not offer STARTTLS") {
		t.Errorf("D: %+v", d)
	}
	// Within the interval every target is skipped, as a real device.
	h.Dispatch(engine.TransportSMTP, `{"subject": "s", "body": "b"}`, engine.Event{ID: "E2", Type: "motion"}, engine.DeliveryPolicy{Timeout: 3 * time.Second})
	reps = h.WaitReports(8, 5*time.Second)
	for _, r := range reps[4:] {
		if r.Status != engine.DeliverySkipped || r.EventID != "E2" || r.Error != "one mail every 1h0m0s at most" {
			t.Fatalf("second event: %+v", r)
		}
	}
	if n := len(srv.Mails()); n != 1 {
		t.Fatalf("%d mails, want 1", n)
	}
}

func TestProbeChecksRecipientsWithoutMailing(t *testing.T) {
	srv := enginetest.NewSMTPServer(t, false)
	target := engine.Target{Type: engine.TargetSMTP, URL: "smtp://" + srv.Addr(), From: "gate@lab.local", To: []string{"ops@lab.local"}, Username: "cam", Password: "secret"}
	if err := Probe(t.Context(), target, "10.0.0.5"); err != nil {
		t.Fatal(err)
	}
	target.To = []string{"nobody@lab.local"}
	if err := Probe(t.Context(), target, "10.0.0.5"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("refused recipient: %v", err)
	}
	if n := len(srv.Mails()); n != 0 {
		t.Fatalf("the test sent %d mails", n)
	}
	if a, err := Address(engine.Target{URL: "smtps://mail.lab"}); err != nil || a != "mail.lab:465" {
		t.Fatalf("smtps default port: %s %v", a, err)
	}
}
