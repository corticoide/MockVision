package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/corticoide/mockvision/backend/internal/app"
	"github.com/corticoide/mockvision/backend/internal/netctl"
	"github.com/corticoide/mockvision/backend/internal/store"
)

// testNode serves the API of a node that runs no cameras.
type testNode struct {
	t      *testing.T
	url    string
	cookie *http.Cookie
}

func newTestNode(t *testing.T) *testNode {
	t.Helper()
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
	static := fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}
	srv := httptest.NewServer(New(Config{}, svc, hub, static, log).Handler())
	t.Cleanup(srv.Close)
	n := &testNode{t: t, url: srv.URL}
	code, _, err := svc.SetupCode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resp := n.do("POST", "/auth/setup", `{"username":"admin","password":"correct horse battery","setup_code":"`+code+`"}`, map[string]string{"X-MockVision-Request": "1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			n.cookie = c
		}
	}
	return n
}

func (n *testNode) do(method, path, body string, headers map[string]string) *http.Response {
	n.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, n.url+"/api/v1"+path, r)
	if err != nil {
		n.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		n.t.Fatal(err)
	}
	n.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// panel makes a request with the session cookie, as the panel does.
func (n *testNode) panel(method, path, body string) *http.Response {
	n.t.Helper()
	req, _ := http.NewRequest(method, n.url+"/api/v1"+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-MockVision-Request", "1")
	req.AddCookie(n.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		n.t.Fatal(err)
	}
	n.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (n *testNode) token(scopes string) string {
	n.t.Helper()
	resp := n.panel("POST", "/tokens", `{"name":"t-`+scopes+`","scopes":["`+strings.ReplaceAll(scopes, ",", `","`)+`"]}`)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		n.t.Fatalf("create token: %d %s", resp.StatusCode, b)
	}
	var out struct {
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		n.t.Fatal(err)
	}
	return out.Secret
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func TestBearerTokens(t *testing.T) {
	n := newTestNode(t)
	write := n.token("write")
	read := n.token("read")

	// No credentials, a wrong token, and a token whose user is fine.
	if r := n.do("GET", "/cameras", "", nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", r.StatusCode)
	}
	r := n.do("GET", "/cameras", "", bearer("mvt_nope"))
	if r.StatusCode != http.StatusUnauthorized || !strings.Contains(r.Header.Get("WWW-Authenticate"), "invalid_token") {
		t.Fatalf("bad token: %d %q", r.StatusCode, r.Header.Get("WWW-Authenticate"))
	}
	if r := n.do("GET", "/cameras", "", bearer(read)); r.StatusCode != http.StatusOK {
		t.Fatalf("read token GET: %d", r.StatusCode)
	}

	// A token needs no CSRF header, but a read token cannot change state.
	patch := `{"max_cameras":9}`
	if r := n.do("PATCH", "/settings", patch, bearer(read)); r.StatusCode != http.StatusForbidden {
		t.Fatalf("read token PATCH: %d", r.StatusCode)
	}
	if r := n.do("PATCH", "/settings", patch, bearer(write)); r.StatusCode != http.StatusOK {
		t.Fatalf("write token PATCH: %d", r.StatusCode)
	}
	// The cookie still needs the CSRF header.
	req, _ := http.NewRequest("PATCH", n.url+"/api/v1/settings", strings.NewReader(patch))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(n.cookie)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cookie without CSRF header: %v %v", resp.StatusCode, err)
	}
	// A request with a token is judged by the token alone, never the cookie.
	req, _ = http.NewRequest("GET", n.url+"/api/v1/cameras", nil)
	req.Header.Set("Authorization", "Bearer mvt_nope")
	req.AddCookie(n.cookie)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token with a good cookie: %v %v", resp.StatusCode, err)
	}

	// Tokens are managed from the panel only.
	if r := n.do("GET", "/tokens", "", bearer(write)); r.StatusCode != http.StatusForbidden {
		t.Fatalf("list tokens with a token: %d", r.StatusCode)
	}
	if r := n.do("POST", "/tokens", `{"name":"x"}`, bearer(write)); r.StatusCode != http.StatusForbidden {
		t.Fatalf("create token with a token: %d", r.StatusCode)
	}

	// /auth/me tells automation which token it holds.
	r = n.do("GET", "/auth/me", "", bearer(read))
	var me struct {
		User  struct{ Username string } `json:"user"`
		Token *struct {
			Name   string   `json:"name"`
			Scopes []string `json:"scopes"`
		} `json:"token"`
		ExpiresAt *string `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if me.User.Username != "admin" || me.Token == nil || me.Token.Name != "t-read" || me.ExpiresAt != nil {
		t.Fatalf("me: %+v", me)
	}

	// The audit shows the change made with the token as coming from the API.
	r = n.do("GET", "/audit?origin=api", "", bearer(read))
	var page struct {
		Items []struct {
			Action string `json:"action"`
			Origin string `json:"origin"`
			Token  *struct {
				Name string `json:"name"`
			} `json:"token"`
		} `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Action != "settings.update" || page.Items[0].Token.Name != "t-write" {
		t.Fatalf("audit via API: %+v", page.Items)
	}

	// Revoking a token from the panel stops it at once.
	var list struct {
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.NewDecoder(n.panel("GET", "/tokens", "").Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	for _, tk := range list.Items {
		if tk.Name == "t-write" {
			if r := n.panel("DELETE", "/tokens/"+tk.ID, ""); r.StatusCode != http.StatusNoContent {
				t.Fatalf("revoke: %d", r.StatusCode)
			}
		}
	}
	if r := n.do("GET", "/cameras", "", bearer(write)); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d", r.StatusCode)
	}
}

func TestBulkEndpointReportsEachCamera(t *testing.T) {
	n := newTestNode(t)
	r := n.panel("POST", "/cameras/actions/bulk", `{"action":"stop","ids":["nope1","nope2"]}`)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("bulk: %d", r.StatusCode)
	}
	var out struct {
		Action    string `json:"action"`
		Succeeded int    `json:"succeeded"`
		Failed    int    `json:"failed"`
		Results   []struct {
			ID    string   `json:"id"`
			OK    bool     `json:"ok"`
			Error *Problem `json:"error"`
		} `json:"results"`
	}
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Action != "stop" || out.Failed != 2 || out.Succeeded != 0 || len(out.Results) != 2 ||
		out.Results[0].Error == nil || out.Results[0].Error.Status != http.StatusNotFound {
		t.Fatalf("bulk result: %+v", out)
	}
	if r := n.panel("POST", "/cameras/actions/bulk", `{"action":"explode","ids":["x"]}`); r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unknown action: %d", r.StatusCode)
	}
}

func TestJobsEndpoints(t *testing.T) {
	n := newTestNode(t) // the node does not run jobs: they stay queued
	read := n.token("read")
	if r := n.do("GET", "/jobs?status=active", "", bearer(read)); r.StatusCode != http.StatusOK {
		t.Fatalf("list with a read token: %d", r.StatusCode)
	}
	if r := n.do("POST", "/jobs", `{"type":"renditions.prepare"}`, bearer(read)); r.StatusCode != http.StatusForbidden {
		t.Fatalf("create with a read token: %d", r.StatusCode)
	}
	if r := n.panel("POST", "/jobs", `{"type":"import"}`); r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("import through /jobs: %d", r.StatusCode)
	}
	if r := n.panel("GET", "/jobs?status=bogus", ""); r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unknown status filter: %d", r.StatusCode)
	}

	r := n.panel("POST", "/jobs", `{"type":"renditions.prepare"}`)
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d", r.StatusCode)
	}
	var job struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Title  string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	if job.Status != "queued" || job.Title == "" {
		t.Fatalf("created: %+v", job)
	}
	// Asking for the same work again joins the open job.
	var again struct{ ID string }
	_ = json.NewDecoder(n.panel("POST", "/jobs", `{"type":"renditions.prepare","params":{}}`).Body).Decode(&again)
	if again.ID != job.ID {
		t.Fatalf("second request: %s, want %s", again.ID, job.ID)
	}

	var detail struct {
		ID     string `json:"id"`
		Events []struct {
			Kind string `json:"kind"`
		} `json:"events"`
	}
	if err := json.NewDecoder(n.panel("GET", "/jobs/"+job.ID, "").Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	if detail.ID != job.ID || len(detail.Events) != 1 || detail.Events[0].Kind != "status" {
		t.Fatalf("detail: %+v", detail)
	}
	for action, want := range map[string]int{"answer": http.StatusConflict, "resume": http.StatusConflict, "explode": http.StatusNotFound} {
		body := ""
		if action == "answer" {
			body = `{"answer":"skip"}`
		}
		if r := n.panel("POST", "/jobs/"+job.ID+"/actions/"+action, body); r.StatusCode != want {
			t.Fatalf("%s: %d, want %d", action, r.StatusCode, want)
		}
	}
	if r := n.panel("POST", "/jobs/"+job.ID+"/actions/cancel", ""); r.StatusCode != http.StatusOK {
		t.Fatalf("cancel: %d", r.StatusCode)
	}
	_ = json.NewDecoder(n.panel("GET", "/jobs/"+job.ID, "").Body).Decode(&job)
	if job.Status != "canceled" {
		t.Fatalf("after cancel: %s", job.Status)
	}
	if r := n.panel("GET", "/jobs/01J8Z3QK0000000000000000AB", ""); r.StatusCode != http.StatusNotFound {
		t.Fatalf("missing job: %d", r.StatusCode)
	}
	// The audit names the job.
	var audit struct {
		Items []struct {
			Action string `json:"action"`
			Entity struct {
				Name string `json:"name"`
			} `json:"entity"`
		} `json:"items"`
	}
	_ = json.NewDecoder(n.panel("GET", "/audit?action=job", "").Body).Decode(&audit)
	if len(audit.Items) != 2 || audit.Items[0].Action != "job.cancel" || audit.Items[0].Entity.Name != job.Title {
		t.Fatalf("audit: %+v", audit.Items)
	}
}
