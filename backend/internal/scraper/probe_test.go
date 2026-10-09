package scraper

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeIsReadOnly(t *testing.T) {
	p := NewProber(Target{Host: "127.0.0.1"}, 100)
	if _, _, err := p.HTTP(context.Background(), http.MethodPost, 80, "/", false); err != ErrWriteMethod {
		t.Fatalf("POST was allowed: %v", err)
	}
	if _, _, err := p.HTTP(context.Background(), http.MethodPut, 80, "/", false); err != ErrWriteMethod {
		t.Fatalf("PUT was allowed: %v", err)
	}
}

func TestProbeDigest(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Digest realm="cam", nonce="abc", qop="auth", algorithm=MD5`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Server", "Milesight/1.0")
		_, _ = w.Write([]byte(`{"model":"MS-X"}`))
	}))
	defer srv.Close()
	port := portOf(t, srv.URL)
	p := NewProber(Target{Host: "127.0.0.1", Username: "admin", Password: "ms1234"}, 100)
	resp, _, err := p.HTTP(context.Background(), http.MethodGet, port, "/x", true)
	if err != nil || resp.Status != 200 || string(resp.Body) != `{"model":"MS-X"}` {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	// All read-only, and the credentials went only after the challenge.
	for _, m := range methods {
		if m != http.MethodGet {
			t.Fatalf("a non-GET request: %v", methods)
		}
	}
}

func TestLimiterPaces(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.Add(1) }))
	defer srv.Close()
	port := portOf(t, srv.URL)
	p := NewProber(Target{Host: "127.0.0.1"}, 20) // 20/s
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	for ctx.Err() == nil {
		if _, _, err := p.HTTP(ctx, http.MethodGet, port, "/", false); err != nil {
			break
		}
	}
	// 20/s over ~250 ms is about 5 to 6, never dozens.
	if got := n.Load(); got == 0 || got > 12 {
		t.Fatalf("rate not enforced: %d requests in 250ms", got)
	}
}

func portOf(t *testing.T, url string) int {
	t.Helper()
	var p int
	if _, err := fmtSscanf(url, &p); err != nil {
		t.Fatalf("port of %s: %v", url, err)
	}
	return p
}

// fmtSscanf pulls the port out of an http://127.0.0.1:PORT URL.
func fmtSscanf(url string, p *int) (int, error) {
	var a, b, c, d int
	return fmt.Sscanf(url, "http://%d.%d.%d.%d:%d", &a, &b, &c, &d, p)
}
