// Package probe runs the connection test of an event target, the same from
// a camera, across the network its deliveries cross, and from the node.
package probe

import (
	"context"
	"fmt"
	"net/http"

	"github.com/corticoide/mockvision/backend/internal/engines/ftpupload"
	"github.com/corticoide/mockvision/backend/internal/engines/httppush"
	"github.com/corticoide/mockvision/backend/internal/engines/mqttpub"
	"github.com/corticoide/mockvision/backend/internal/engines/smtpmail"
	"github.com/corticoide/mockvision/sdk/engine"
)

// Info is what a test says about who tests.
type Info struct {
	// Body is the request body of an http test, sent as JSON.
	Body      []byte
	UserAgent string
	// ClientID names the test at an MQTT broker.
	ClientID string
	// IP is the tester's address, which names it to mail servers.
	IP string
}

// Target tests a target: a request to an http target, a session with an
// MQTT broker, a login and the target's directory on FTP and SFTP servers
// (created if missing; nothing is uploaded), and the sender and recipients
// on a mail server, which gets no mail. It returns the HTTP status of an
// http test. Connections are opened with the dialer of ctx (see
// delivery.WithDialer).
func Target(ctx context.Context, t engine.Target, info Info) (int, error) {
	switch t.Type {
	case engine.TargetHTTP, "":
		method := t.Method
		if method == "" {
			method = http.MethodPost
		}
		h := http.Header{}
		if method != http.MethodGet {
			h.Set("Content-Type", "application/json")
		}
		h.Set("User-Agent", info.UserAgent)
		for k, v := range t.Headers {
			h.Set(k, v)
		}
		status, err := httppush.Do(ctx, httppush.NewClient(), method, t, h, info.Body)
		if err == nil && (status < 200 || status > 299) {
			err = fmt.Errorf("target answered %d", status)
		}
		return status, err
	case engine.TargetMQTT:
		return 0, mqttpub.Probe(ctx, t, info.ClientID)
	case engine.TargetFTP, engine.TargetSFTP:
		return 0, ftpupload.Probe(ctx, t)
	case engine.TargetSMTP:
		return 0, smtpmail.Probe(ctx, t, info.IP)
	}
	return 0, fmt.Errorf("unknown target type %q", t.Type)
}
