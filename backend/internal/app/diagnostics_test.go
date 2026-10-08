package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
)

func TestDiagnostics(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cam := createCamera(t, svc, "Gate", true)
	base := httpBase(t, cam)
	info := base + "/cgi-bin/operator/operator.cgi?action=get.system.information"
	for i := 0; i < 3; i++ {
		if code, _ := digestGet(t, info, "admin", "ms1234"); code != http.StatusOK {
			t.Fatalf("info: %d", code)
		}
	}
	if code, _ := digestGet(t, info, "admin", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("a wrong password: %d", code)
	}
	if code, _ := digestGet(t, base+"/cgi-bin/nothing.cgi?x=1", "admin", "ms1234"); code == http.StatusOK {
		t.Fatalf("an unknown request answered 200")
	}

	// The camera reports every 10 s.
	var reqs *RequestsView
	deadline := time.Now().Add(25 * time.Second)
	for {
		var err error
		if reqs, err = svc.CameraRequests(ctx, cam.ID, "1h"); err != nil {
			t.Fatal(err)
		}
		if routeCount(reqs, "device-info") >= 3 && routeCount(reqs, "auth-failed") >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("requests: %+v", reqs.Routes)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(reqs.Minutes) == 0 || reqs.Minutes[0].Count < 5 {
		t.Fatalf("minutes: %+v", reqs.Minutes)
	}
	for _, r := range reqs.Routes {
		if r.Route == "auth-failed" && r.AuthFailures < 1 {
			t.Fatalf("auth-failed: %+v", r)
		}
		if r.Route == "auth" && (r.Errors != 0 || r.AuthFailures != 0) {
			t.Fatalf("a challenge counted as a failure: %+v", r)
		}
	}

	clients, err := svc.CameraClients(ctx, cam.ID, "1h")
	if err != nil || len(clients) != 1 {
		t.Fatalf("clients: %+v %v", clients, err)
	}
	c := clients[0]
	if c.IP != "127.0.0.1" || strings.Join(c.Protocols, ",") != "http" || c.Connections < 1 || c.AuthFailures < 1 || c.Requests < 5 || c.Gaps < 1 {
		t.Fatalf("client: %+v", c)
	}
	var info3 RouteStat
	for _, r := range c.Routes {
		if r.Route == "device-info" {
			info3 = r
		}
	}
	if info3.Count != 3 || info3.IntervalMS <= 0 || info3.P95MS <= 0 {
		t.Fatalf("device-info: %+v", info3)
	}

	gaps, err := svc.CameraGaps(ctx, cam.ID)
	if err != nil || len(gaps) != 1 || gaps[0].Protocol != "http" || !strings.Contains(gaps[0].Summary, "/cgi-bin/nothing.cgi") || gaps[0].Count != 1 {
		t.Fatalf("gaps: %+v %v", gaps, err)
	}

	csv, err := svc.ExportRequests(ctx, cam.ID, "1h", "csv")
	if err != nil || !strings.HasPrefix(string(csv), "minute,client_ip,route,count") || !strings.Contains(string(csv), ",127.0.0.1,device-info,3,") {
		t.Fatalf("csv: %s %v", csv, err)
	}

	logs, err := svc.CameraLogs(ctx, cam.ID, 0, 0)
	if err != nil || len(logs) == 0 || logs[0].Source != "service" || logs[0].Msg != "running" {
		t.Fatalf("logs: %+v %v", logs, err)
	}

	// Metrics past the memory window, every 10 s (the loop may have
	// stored some already).
	svc.persistMetrics(ctx, time.Now().Add(10*time.Second))
	samples, err := svc.CameraMetricsRange(ctx, cam.ID, "1h")
	if err != nil || len(samples) == 0 || samples[len(samples)-1].RSSBytes == 0 || samples[len(samples)-1].Requests == 0 {
		t.Fatalf("metrics: %+v %v", samples, err)
	}
	if _, err := svc.CameraMetricsRange(ctx, cam.ID, "1y"); err == nil {
		t.Fatal("a range of a year was accepted")
	}

	prom, err := svc.PrometheusMetrics(ctx)
	if err != nil || !strings.Contains(string(prom), fmt.Sprintf(`mockvision_camera_up{camera_id=%q,name="Gate",state="running"} 1`, cam.ID)) ||
		!strings.Contains(string(prom), "# TYPE mockvision_camera_requests_total counter") {
		t.Fatalf("prometheus: %s %v", prom, err)
	}

	// Past their retention, they go.
	svc.applyDiagnosticsRetention(ctx, time.Now().Add(31*24*time.Hour))
	if reqs, _ = svc.CameraRequests(ctx, cam.ID, "7d"); len(reqs.Routes) != 0 {
		t.Fatalf("requests after the retention: %+v", reqs.Routes)
	}
	if gaps, _ = svc.CameraGaps(ctx, cam.ID); len(gaps) != 0 {
		t.Fatalf("gaps after the retention: %+v", gaps)
	}
	if samples, _ = svc.CameraMetricsRange(ctx, cam.ID, "24h"); len(samples) != 0 {
		t.Fatalf("metrics after the retention: %+v", samples)
	}
	if _, err := svc.StopCamera(ctx, testActor, cam.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, svc, cam.ID, domain.StateStopped)
}

func routeCount(v *RequestsView, route string) int64 {
	var n int64
	for _, r := range v.Routes {
		if r.Route == route {
			n += r.Count
		}
	}
	return n
}
