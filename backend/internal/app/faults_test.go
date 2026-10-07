package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
)

// httpPort is the port of a running camera's HTTP API.
func httpPort(t *testing.T, v *CameraView) string {
	t.Helper()
	for _, ep := range v.Endpoints {
		if ep.Instance == "http" {
			return ep.URL
		}
	}
	t.Fatalf("no http endpoint in %+v", v.Endpoints)
	return ""
}

// answers reports whether the camera's HTTP API takes a connection.
func answers(base string) bool {
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get(base + "/snapshot.cgi")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func waitStateReason(t *testing.T, svc *Service, id string, want domain.CameraState, reason string) *CameraView {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		v, err := svc.GetCamera(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if v.Status.State == string(want) && strings.Contains(v.Status.Reason, reason) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("camera is %s (%s), want %s with %q", v.Status.State, v.Status.Reason, want, reason)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestFaultsAreValidated(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cam := createCamera(t, svc, "Gate", false)
	for field, in := range map[string]FaultInput{
		"kind":       {Kind: "meltdown", DurationS: 10},
		"instance":   {Kind: "service_down", DurationS: 10},
		"latency_ms": {Kind: "latency", Instance: "http", DurationS: 10},
		"status":     {Kind: "error_status", Instance: "http", Status: 418, DurationS: 10},
		"skew_s":     {Kind: "clock_skew", DurationS: 10},
		"duration_s": {Kind: "ip_conflict", DurationS: 90000},
	} {
		_, err := svc.InjectFault(ctx, testActor, cam.ID, in)
		wantInvalid(t, err, field)
	}
	// Only protocols that answer clients fail this way, and only the HTTP
	// API and RTSP answer with a status.
	_, err := svc.InjectFault(ctx, testActor, cam.ID, FaultInput{Kind: "service_down", Instance: "push", DurationS: 10})
	wantInvalid(t, err, "instance")
	_, err = svc.InjectFault(ctx, testActor, cam.ID, FaultInput{Kind: "service_down", Instance: "nope", DurationS: 10})
	wantInvalid(t, err, "instance")
	if _, err := svc.InjectFault(ctx, testActor, cam.ID, FaultInput{Kind: "error_status", Instance: "rtsp", Status: 401, DurationS: 10}); err != nil {
		t.Fatal(err)
	}
	// The same kind on the same protocol replaces it.
	f, err := svc.InjectFault(ctx, testActor, cam.ID, FaultInput{Kind: "error_status", Instance: "rtsp", Status: 500})
	if err != nil || f.ExpiresAt != nil || !f.Active {
		t.Fatalf("fault %+v %v", f, err)
	}
	list, err := svc.ListFaults(ctx, cam.ID)
	if err != nil || len(list) != 2 || !list[0].Active || list[0].Status != 500 || list[1].Active || list[1].EndedBy != "replaced" {
		t.Fatalf("faults %+v %v", list, err)
	}
	if err := svc.EndFault(ctx, testActor, cam.ID, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.EndFault(ctx, testActor, cam.ID, f.ID); err == nil {
		t.Fatal("an ended fault ended twice")
	}
}

// A fault applies at once on a running camera, which turns degraded and
// says why; it ends by hand or when it expires, and the camera is running
// again, without a restart.
func TestFaultsDegradeARunningCamera(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cam := createCamera(t, svc, "Gate", true)
	cam = waitState(t, svc, cam.ID, domain.StateRunning)
	base, pid := httpPort(t, cam), cam.Status.PID
	if !answers(base) {
		t.Fatal("the camera does not answer before the fault")
	}

	down, err := svc.InjectFault(ctx, testActor, cam.ID, FaultInput{Kind: "service_down", Instance: "http", DurationS: 2})
	if err != nil {
		t.Fatal(err)
	}
	v := waitStateReason(t, svc, cam.ID, domain.StateDegraded, "http down")
	if v.Status.ReasonCode != ReasonFaults || answers(base) {
		t.Fatalf("degraded %+v; answers: %v", v.Status, answers(base))
	}
	if _, err := svc.InjectFault(ctx, testActor, cam.ID, FaultInput{Kind: "clock_skew", SkewS: -60}); err != nil {
		t.Fatal(err)
	}
	waitStateReason(t, svc, cam.ID, domain.StateDegraded, "clock -60 s")
	active, err := svc.ActiveFaults(ctx)
	if err != nil || len(active) != 2 || active[0].CameraName != "Gate" {
		t.Fatalf("active %+v %v", active, err)
	}

	// The service fault expires on its own.
	waitStateReason(t, svc, cam.ID, domain.StateDegraded, "faults on: clock -60 s")
	if !answers(base) {
		t.Fatal("the HTTP API did not come back when its fault expired")
	}
	list, _ := svc.ListFaults(ctx, cam.ID)
	for _, f := range list {
		if f.ID == down.ID && (f.Active || f.EndedBy != "expired") {
			t.Fatalf("expired fault %+v", f)
		}
	}
	for _, f := range list {
		if f.Active {
			if err := svc.EndFault(ctx, testActor, cam.ID, f.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	v = waitState(t, svc, cam.ID, domain.StateRunning)
	if v.Status.PID != pid {
		t.Fatalf("the camera restarted: pid %d, was %d", v.Status.PID, pid)
	}
}

// network_down raises network_lost while the camera cannot deliver it;
// the profile's retries take it out once the network is back.
func TestNetworkDownDeliversItsEventOnceBack(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	var got atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got.Add(1) }))
	defer srv.Close()
	tg, err := svc.CreateTarget(ctx, testActor, TargetInput{Name: ptr("VMS"), URL: ptr(srv.URL + "/events")})
	if err != nil {
		t.Fatal(err)
	}
	cam := createCamera(t, svc, "Gate", false)
	if _, err := svc.UpdateCamera(ctx, testActor, cam.ID, UpdateCameraInput{TargetIDs: &[]string{tg.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartCamera(ctx, testActor, cam.ID); err != nil {
		t.Fatal(err)
	}
	cam = waitState(t, svc, cam.ID, domain.StateRunning)
	f, err := svc.InjectFault(ctx, testActor, cam.ID, FaultInput{Kind: "network_down"})
	if err != nil {
		t.Fatal(err)
	}
	waitStateReason(t, svc, cam.ID, domain.StateDegraded, "network down")
	var evID string
	deadline := time.Now().Add(10 * time.Second)
	for evID == "" {
		page, err := svc.ListEvents(ctx, EventFilter{CameraID: cam.ID, Type: "network_lost", Limit: 5})
		if err == nil && len(page.Items) > 0 && len(page.Items[0].Deliveries) > 0 {
			evID = page.Items[0].ID
			d := page.Items[0].Deliveries[0]
			if d.Status != "retry" || !strings.Contains(d.Error, "network is unreachable") {
				t.Fatalf("delivery while down %+v", d)
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no network_lost event with a failed delivery")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := svc.EndFault(ctx, testActor, cam.ID, f.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, svc, cam.ID, domain.StateRunning)
	deadline = time.Now().Add(15 * time.Second)
	for {
		ev := storedEvent(t, svc, evID)
		if ev.DeliveryStatus == "ok" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not delivered once back: %+v", ev.Deliveries)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if got.Load() != 1 {
		t.Fatalf("the target got %d requests", got.Load())
	}
}

// A stopped camera starts with the faults that are on; a reboot takes it
// off for its boot time and it comes back with them.
func TestRebootAndFaultsAtStart(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cam := createCamera(t, svc, "Gate", false)
	if _, err := svc.RebootCamera(ctx, testActor, cam.ID, nil); err == nil {
		t.Fatal("a stopped camera rebooted")
	}
	if _, err := svc.InjectFault(ctx, testActor, cam.ID, FaultInput{Kind: "error_status", Instance: "http", Status: 500, DurationS: 600}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartCamera(ctx, testActor, cam.ID); err != nil {
		t.Fatal(err)
	}
	v := waitStateReason(t, svc, cam.ID, domain.StateDegraded, "http answers 500")
	resp, err := http.Get(httpPort(t, v) + "/snapshot.cgi")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d", resp.StatusCode)
	}

	secs := 1
	v, err = svc.RebootCamera(ctx, testActor, cam.ID, &secs)
	if err != nil {
		t.Fatal(err)
	}
	if v.Status.State != string(domain.StateRestarting) || v.Status.ReasonCode != ReasonReboot {
		t.Fatalf("rebooting %+v", v.Status)
	}
	time.Sleep(300 * time.Millisecond)
	if v, _ := svc.GetCamera(ctx, cam.ID); v.Status.State != string(domain.StateRestarting) {
		t.Fatalf("the reconciler cut the reboot short: %s", v.Status.State)
	}
	waitStateReason(t, svc, cam.ID, domain.StateDegraded, "http answers 500")
	bad := -1
	if _, err := svc.RebootCamera(ctx, testActor, cam.ID, &bad); err == nil {
		t.Fatal("a negative boot time passed")
	}
}
