package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

func TestPanelPageNonce(t *testing.T) {
	dist := fstest.MapFS{
		"index.html":     {Data: []byte(`<!doctype html><meta name="csp-nonce" content="__MOCKVISION_CSP_NONCE__"><div id="root"></div>`)},
		"assets/app.css": {Data: []byte("body{}")},
	}
	srv := httptest.NewServer(securityHeaders(spaHandler(dist)))
	defer srv.Close()
	get := func(path string) (*http.Response, string) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	meta := regexp.MustCompile(`content="([A-Za-z0-9+/=]{24})"`)
	seen := map[string]bool{}
	for _, path := range []string{"/", "/cameras/x/diagnostics"} {
		resp, body := get(path)
		m := meta.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s: no nonce in %q", path, body)
		}
		nonce := m[1]
		csp := resp.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "style-src 'self' 'nonce-"+nonce+"';") || strings.Contains(csp, "unsafe-inline") || !strings.Contains(csp, "script-src 'self';") {
			t.Fatalf("%s: CSP %q for nonce %s", path, csp, nonce)
		}
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: cached: %q", path, resp.Header.Get("Cache-Control"))
		}
		if seen[nonce] {
			t.Fatal("a nonce served twice")
		}
		seen[nonce] = true
	}
	// Other files keep the policy without a nonce.
	resp, _ := get("/assets/app.css")
	if csp := resp.Header.Get("Content-Security-Policy"); csp != contentSecurityPolicy {
		t.Fatalf("asset CSP %q", csp)
	}
}
