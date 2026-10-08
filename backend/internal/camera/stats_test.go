package camera

import (
	"fmt"
	"testing"
	"time"
)

func TestStats(t *testing.T) {
	s := newStats()
	t0 := time.Date(2026, 10, 8, 10, 0, 5, 0, time.UTC)
	for i := 0; i < 100; i++ {
		s.request(t0.Add(time.Duration(i)*100*time.Millisecond), "snapshot", "10.0.0.5", 200, time.Duration(i+1)*time.Millisecond)
	}
	s.request(t0, "auth", "10.0.0.5", 401, time.Millisecond)        // a challenge
	s.request(t0, "auth-failed", "10.0.0.5", 401, time.Millisecond) // credentials refused
	s.request(t0, "info", "10.0.0.6", 500, time.Millisecond)
	// The next minute.
	s.request(t0.Add(time.Minute), "snapshot", "10.0.0.5", 200, time.Millisecond)
	s.connection(t0, "rtsp", "10.0.0.5", true)
	s.connection(t0.Add(1500*time.Millisecond), "rtsp", "10.0.0.5", false)
	s.connection(t0, "rtsp", "10.0.0.5", false) // never seen opening
	s.gap(t0, "http", "10.0.0.5", "GET /x")
	s.gap(t0.Add(time.Second), "http", "10.0.0.5", "GET /x")

	st := s.take()
	minute := t0.Truncate(time.Minute).UnixMilli()
	byKey := map[string]int{}
	for i, r := range st.Requests {
		byKey[fmt.Sprintf("%d %s %s", r.Minute, r.ClientIP, r.Route)] = i
	}
	snap := st.Requests[byKey[fmt.Sprintf("%d 10.0.0.5 snapshot", minute)]]
	if snap.Count != 100 || snap.P50MS != 50 || snap.P95MS != 95 || snap.MaxMS != 100 || snap.Errors != 0 ||
		snap.FirstAt != t0.UnixMilli() || snap.LastAt != t0.Add(9900*time.Millisecond).UnixMilli() {
		t.Fatalf("snapshot = %+v", snap)
	}
	if r := st.Requests[byKey[fmt.Sprintf("%d 10.0.0.5 auth", minute)]]; r.Errors != 0 || r.AuthFailures != 0 {
		t.Fatalf("a challenge counted: %+v", r)
	}
	if r := st.Requests[byKey[fmt.Sprintf("%d 10.0.0.5 auth-failed", minute)]]; r.AuthFailures != 1 || r.Errors != 0 {
		t.Fatalf("refused credentials: %+v", r)
	}
	if r := st.Requests[byKey[fmt.Sprintf("%d 10.0.0.6 info", minute)]]; r.Errors != 1 {
		t.Fatalf("a 500: %+v", r)
	}
	if _, ok := byKey[fmt.Sprintf("%d 10.0.0.5 snapshot", minute+60000)]; !ok || len(st.Requests) != 5 {
		t.Fatalf("requests: %+v", st.Requests)
	}
	if len(st.Connections) != 1 || st.Connections[0].Opened != 1 || st.Connections[0].Closed != 2 || st.Connections[0].DurationMS != 1500 ||
		st.Connections[0].MaxDurationMS != 1500 {
		t.Fatalf("connections: %+v", st.Connections)
	}
	if len(st.Gaps) != 1 || st.Gaps[0].Count != 2 || st.Gaps[0].At != t0.Add(time.Second).UnixMilli() {
		t.Fatalf("gaps: %+v", st.Gaps)
	}
	// A report starts again from nothing.
	if s.take() != nil {
		t.Fatal("a second report repeated the first")
	}

	// Past the keys of a minute, requests count under "other".
	for i := 0; i < statsMaxKeys+10; i++ {
		s.request(t0, fmt.Sprintf("r%d", i), "10.0.0.5", 200, time.Millisecond)
	}
	st = s.take()
	other := 0
	for _, r := range st.Requests {
		if r.Route == "other" {
			other = r.Count
		}
	}
	if len(st.Requests) != statsMaxKeys+1 || other != 10 {
		t.Fatalf("%d keys, %d other", len(st.Requests), other)
	}
}
