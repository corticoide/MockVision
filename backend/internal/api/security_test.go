package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"

	"github.com/corticoide/mockvision/backend/internal/app"
	"github.com/corticoide/mockvision/backend/internal/netctl"
	"github.com/corticoide/mockvision/backend/internal/store"
)

func TestClientIPTrustsOnlyConfiguredProxies(t *testing.T) {
	s := &Server{cfg: Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}}
	cases := []struct {
		remote, xff, want string
	}{
		{"192.0.2.1:5000", "203.0.113.9", "192.0.2.1"},                // not a proxy: the header is ignored
		{"10.1.1.1:5000", "203.0.113.9", "203.0.113.9"},               // proxy: the client it saw
		{"10.1.1.1:5000", "198.51.100.1, 203.0.113.9", "203.0.113.9"}, // the leftmost entry is the client's claim
		{"10.1.1.1:5000", "203.0.113.9, 10.2.2.2", "203.0.113.9"},     // chained proxies
		{"10.1.1.1:5000", "", "10.1.1.1"},                             // no header
		{"10.1.1.1:5000", "not-an-ip", "10.1.1.1"},                    // garbage
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := s.resolveClientIP(r); got != c.want {
			t.Errorf("%s with %q: got %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
}

func TestAllowedHosts(t *testing.T) {
	s := &Server{cfg: Config{AllowedHosts: []string{"mockvision.lab"}}}
	h := s.checkHost(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for host, want := range map[string]int{
		"mockvision.lab:8080": http.StatusNoContent,
		"MockVision.lab":      http.StatusNoContent,
		"192.168.1.20:8080":   http.StatusNoContent,
		"[fe80::1]:8080":      http.StatusNoContent,
		"attacker.example":    http.StatusMisdirectedRequest,
	} {
		r := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%s: %d, want %d", host, w.Code, want)
		}
	}
}

func TestSetupNeedsTheSetupCode(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := NewHub()
	svc, err := app.New(app.Options{DataDir: dir, Runtime: netctl.NewLocalRuntime("/nonexistent", log), Log: log}, st, hub)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(Config{}, svc, hub, fstest.MapFS{}, log).Handler())
	t.Cleanup(srv.Close)
	setup := func(code string) int {
		body := `{"username":"admin","password":"correct horse battery","setup_code":"` + code + `"}`
		req, _ := http.NewRequest("POST", srv.URL+"/api/v1/auth/setup", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-MockVision-Request", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := setup(""); got != http.StatusForbidden {
		t.Fatalf("without the code: %d", got)
	}
	if got := setup("WRONGCODEWRONGCODEWRONGCO"); got != http.StatusForbidden {
		t.Fatalf("with a wrong code: %d", got)
	}
	code, path, err := svc.SetupCode(context.Background())
	if err != nil || code == "" {
		t.Fatalf("code %q: %v", code, err)
	}
	if got := setup(strings.ToLower(code)); got != http.StatusCreated {
		t.Fatalf("with the code, typed in lower case: %d", got)
	}
	if again, _, _ := svc.SetupCode(context.Background()); again != "" {
		t.Fatal("the code must be gone once the administrator exists")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the setup-code file must be removed: %v", err)
	}
}

// A WebSocket ends as soon as the token it was opened with is revoked, or
// the session logs out (audit M4).
func TestWebSocketEndsWithItsCredential(t *testing.T) {
	n := newTestNode(t)
	secret := n.token("read")
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.NewDecoder(n.panel("GET", "/tokens", "").Body).Decode(&list); err != nil || len(list.Items) != 1 {
		t.Fatalf("tokens: %+v %v", list, err)
	}
	wsURL := "ws" + strings.TrimPrefix(n.url, "http") + "/api/v1/ws"
	dial := func(h http.Header) *websocket.Conn {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: h})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.CloseNow() })
		return c
	}
	ended := func(c *websocket.Conn, what string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for {
			if _, _, err := c.Read(ctx); err != nil {
				if ctx.Err() != nil {
					t.Fatalf("%s: the socket stayed open", what)
				}
				return
			}
		}
	}

	byToken := dial(http.Header{"Authorization": {"Bearer " + secret}})
	if r := n.panel("DELETE", "/tokens/"+list.Items[0].ID, ""); r.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: %d", r.StatusCode)
	}
	ended(byToken, "revoked token")

	byCookie := dial(http.Header{"Cookie": {n.cookie.String()}})
	if r := n.panel("POST", "/auth/logout", ""); r.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	ended(byCookie, "logged out session")
}

func TestExtendedSessionRefreshesTheCookie(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("GET", "/api/v1/cameras", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "abc"})
	exp := time.Now().Add(12 * time.Hour)
	w := httptest.NewRecorder()
	s.refreshCookie(w, r, app.Session{ExpiresAt: exp, Extended: true})
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != "abc" || cookies[0].Expires.Unix() != exp.Unix() || !cookies[0].HttpOnly {
		t.Fatalf("cookie: %+v", cookies)
	}
	w = httptest.NewRecorder()
	s.refreshCookie(w, r, app.Session{ExpiresAt: exp})
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("no cookie without an extension")
	}
}

// A client that sends its body a byte at a time is cut off (audit B11).
func TestSlowBodyIsCutOff(t *testing.T) {
	defer func(d time.Duration) { readDeadline = d }(readDeadline)
	readDeadline = 300 * time.Millisecond
	srv := httptest.NewUnstartedServer(deadlines(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusRequestTimeout)
			return
		}
		w.WriteHeader(http.StatusOK)
	})))
	srv.Start()
	defer srv.Close()
	pr, pw := io.Pipe()
	go func() {
		// Never finishes the body.
		_, _ = pw.Write([]byte("{"))
	}()
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/auth/login", pr)
	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow request was not cut off")
	}
	if d := time.Since(start); d < readDeadline {
		t.Fatalf("cut off too early: %s", d)
	}
	pw.Close()
}
