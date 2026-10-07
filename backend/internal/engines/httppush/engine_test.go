package httppush_test

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/engines/enginetest"
	"github.com/corticoide/mockvision/backend/internal/engines/httppush"
	"github.com/corticoide/mockvision/sdk/engine"
)

const section = `{"method": "POST", "content_type": "application/json", "body": "{\"event\": {{ json .EventName }}, \"serial\": {{ json .Camera.Serial }}}"}`

// digestServer challenges with Digest (RFC 7616) and checks the answer
// as a VMS would.
type digestServer struct {
	algorithm string
	qop       string
	mu        sync.Mutex
	bodies    []string
	seen      []string // Authorization headers
}

func (d *digestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	d.mu.Lock()
	d.seen = append(d.seen, auth)
	d.mu.Unlock()
	if !d.valid(r, auth) {
		qop := ""
		if d.qop != "" {
			qop = fmt.Sprintf(`, qop="%s"`, d.qop)
		}
		w.Header().Add("WWW-Authenticate", `Basic realm="vms"`)
		w.Header().Add("WWW-Authenticate", fmt.Sprintf(`Digest realm="vms", nonce="n0nce", opaque="0paque", algorithm=%s%s`, d.algorithm, qop))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	d.mu.Lock()
	d.bodies = append(d.bodies, string(body))
	d.mu.Unlock()
}

func (d *digestServer) valid(r *http.Request, auth string) bool {
	rest, ok := strings.CutPrefix(auth, "Digest ")
	if !ok {
		return false
	}
	p := map[string]string{}
	for _, kv := range strings.Split(rest, ", ") {
		k, v, _ := strings.Cut(kv, "=")
		p[k] = strings.Trim(v, `"`)
	}
	h := md5.New
	if d.algorithm == "SHA-256" {
		h = sha256.New
	}
	sum := func(s string) string { x := h(); x.Write([]byte(s)); return hex.EncodeToString(x.Sum(nil)) }
	ha1 := sum("cam:vms:secret")
	ha2 := sum(r.Method + ":" + r.URL.RequestURI())
	want := sum(ha1 + ":n0nce:" + ha2)
	if d.qop != "" {
		want = sum(strings.Join([]string{ha1, "n0nce", p["nc"], p["cnonce"], p["qop"], ha2}, ":"))
	}
	return p["username"] == "cam" && p["uri"] == r.URL.RequestURI() && p["opaque"] == "0paque" && p["algorithm"] == d.algorithm && p["response"] == want
}

func TestPushAnswersTheTargetsDigestChallenge(t *testing.T) {
	for _, tc := range []struct{ algorithm, qop string }{{"MD5", "auth"}, {"SHA-256", "auth"}, {"MD5", ""}} {
		t.Run(tc.algorithm+"-"+tc.qop, func(t *testing.T) {
			srv := &digestServer{algorithm: tc.algorithm, qop: tc.qop}
			ts := httptest.NewServer(srv)
			defer ts.Close()
			h := enginetest.NewHost(t)
			h.SetTargets(engine.Target{ID: "T1", Name: "VMS", Type: engine.TargetHTTP, URL: ts.URL + "/events?site=1", Method: "POST", Username: "cam", Password: "secret", Auth: httppush.AuthDigest})
			h.Start(httppush.New(), `{"engine": "http-push@^1"}`)
			h.Dispatch(engine.TransportHTTPPush, section, engine.Event{Type: "line_crossing"}, engine.DeliveryPolicy{Timeout: 2 * time.Second})
			reps := h.WaitReports(1, 5*time.Second)
			if reps[0].Status != engine.DeliveryOK || reps[0].HTTPStatus != 200 {
				t.Fatalf("report %+v; server saw %q", reps[0], srv.seen)
			}
			if len(srv.bodies) != 1 || srv.bodies[0] != `{"event": "LineCrossing", "serial": "SN123"}` {
				t.Fatalf("bodies %q", srv.bodies)
			}
			if len(srv.seen) != 2 || srv.seen[0] != "" {
				t.Errorf("the first request must go without credentials: %q", srv.seen)
			}
		})
	}
}

func TestPushSendsBasicAndReportsRefusals(t *testing.T) {
	var got []string
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Site"))
		mu.Unlock()
		if r.URL.Path == "/refuse" {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer ts.Close()
	h := enginetest.NewHost(t)
	h.SetTargets(
		engine.Target{ID: "A", Type: engine.TargetHTTP, URL: ts.URL + "/ok", Method: "PUT", Username: "u", Password: "p", Headers: map[string]string{"X-Site": "lab"}},
		// Digest wanted, but the target answers 401 without a challenge.
		engine.Target{ID: "B", Type: engine.TargetHTTP, URL: ts.URL + "/refuse", Method: "POST", Username: "u", Password: "p", Auth: httppush.AuthDigest},
	)
	h.Start(httppush.New(), `{"engine": "http-push@^1"}`)
	h.Dispatch(engine.TransportHTTPPush, section, engine.Event{Type: "line_crossing"}, engine.DeliveryPolicy{Timeout: 2 * time.Second})
	reps := h.WaitReports(2, 5*time.Second)
	by := map[string]engine.DeliveryReport{}
	for _, r := range reps {
		by[r.TargetID] = r
	}
	if by["A"].Status != engine.DeliveryOK {
		t.Errorf("basic target: %+v", by["A"])
	}
	if b := by["B"]; b.Status != engine.DeliveryFailed || b.HTTPStatus != 401 || !strings.Contains(b.Error, "without a Digest challenge") {
		t.Errorf("refusing target: %+v", b)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(got, ","), "Basic dTpw|lab") {
		t.Errorf("requests %q", got)
	}
}
