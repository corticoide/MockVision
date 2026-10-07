package camera

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/engines/delivery"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/sdk/engine"
)

// faultCamera starts a camera of the demo profile with its HTTP API and
// push engines and the faults given, and returns its API's address.
func faultCamera(t *testing.T, targetURL string, faults []ipc.Fault) (*fakeService, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	svcSide, camSide := net.Pipe()
	svc := &fakeService{msgs: map[string][]*ipc.Envelope{}, got: make(chan *ipc.Envelope, 64)}
	svc.conn = ipc.NewConn(svcSide, svc.handle, nil)
	go func() { _ = svc.conn.Run(ctx) }()
	rt := NewRuntime(Options{CameraID: "cam1", IPC: camSide, Local: true})
	done := make(chan struct{})
	go func() { _ = rt.Run(ctx); close(done) }()
	t.Cleanup(func() {
		_ = svc.conn.Request(context.Background(), ipc.TypeStop, ipc.Stop{DeadlineMS: 1000}, nil)
		<-done
		delivery.SetOffline(false)
	})
	svc.wait(t, ipc.TypeHello, 5*time.Second)
	cfg := ipc.Configure{
		Identity: engine.Identity{CameraID: "cam1", Name: "Gate", IP: "127.0.0.1", MAC: "02:aa:bb:cc:dd:ee", Serial: "6C0012ABCDEF", Vendor: "Milesight", Model: "MS-DEMO"},
		Profile:  demoResolved(t),
		Engines:  []ipc.EngineConfig{{Instance: "http", Enabled: true, Port: 80}, {Instance: "push", Enabled: true}},
		Users:    []engine.User{{Username: "admin", Password: "ms1234", Role: "admin"}},
		Targets:  []ipc.Target{{Target: engine.Target{ID: "t1", Name: "test", Type: "http", URL: targetURL, Method: "POST"}}},
		Faults:   faults,
	}
	if err := svc.conn.Request(ctx, ipc.TypeConfigure, cfg, nil); err != nil {
		t.Fatalf("configure: %v", err)
	}
	var ready ipc.Ready
	if err := svc.wait(t, ipc.TypeReady, 5*time.Second).Decode(&ready); err != nil {
		t.Fatal(err)
	}
	for _, ep := range ready.Endpoints {
		if ep.Instance == "http" {
			return svc, fmt.Sprintf("127.0.0.1:%d", ep.Port)
		}
	}
	t.Fatal("no http endpoint")
	return nil, ""
}

const infoPath = "/cgi-bin/operator/operator.cgi?action=get.system.information"

// get asks the device information with fresh connections; it returns the
// status, the body, or the error a client sees.
func get(addr string) (int, string, error) {
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get("http://" + addr + infoPath)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func fault(t *testing.T, svc *fakeService, f ipc.Fault, on bool) {
	t.Helper()
	var err error
	if on {
		err = svc.conn.Request(context.Background(), ipc.TypeFaultStart, f, nil)
	} else {
		err = svc.conn.Request(context.Background(), ipc.TypeFaultStop, ipc.FaultStop{ID: f.ID}, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestFaultsOnTheHTTPAPI(t *testing.T) {
	svc, addr := faultCamera(t, "http://127.0.0.1:9/none", nil)
	if code, _, err := get(addr); err != nil || code != http.StatusUnauthorized {
		t.Fatalf("before: %d %v", code, err)
	}

	// The service goes down: a connection open before is cut, new ones
	// are reset.
	open, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	time.Sleep(50 * time.Millisecond) // the server accepted it
	down := ipc.Fault{ID: "f1", Kind: string(domain.FaultServiceDown), FaultParams: domain.FaultParams{Instance: "http"}}
	fault(t, svc, down, true)
	_ = open.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := open.Read(make([]byte, 1)); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("the open connection survived: %v", err)
	}
	if _, _, err := get(addr); err == nil {
		t.Fatal("a down service answered")
	}
	fault(t, svc, down, false)
	if code, _, err := get(addr); err != nil || code != http.StatusUnauthorized {
		t.Fatalf("back up: %d %v", code, err)
	}

	// Late answers.
	slow := ipc.Fault{ID: "f2", Kind: string(domain.FaultLatency), FaultParams: domain.FaultParams{Instance: "http", LatencyMS: 400}}
	fault(t, svc, slow, true)
	start := time.Now()
	if _, _, err := get(addr); err != nil || time.Since(start) < 400*time.Millisecond {
		t.Fatalf("latency: %s %v", time.Since(start), err)
	}
	fault(t, svc, slow, false)

	// A status for every request.
	broken := ipc.Fault{ID: "f3", Kind: string(domain.FaultErrorStatus), FaultParams: domain.FaultParams{Instance: "http", Status: 503}}
	fault(t, svc, broken, true)
	if code, body, err := get(addr); err != nil || code != http.StatusServiceUnavailable || body != "503 Service Unavailable\n" {
		t.Fatalf("status: %d %q %v", code, body, err)
	}
	fault(t, svc, broken, false)

	// A moved clock: the device's time is an hour ahead.
	skew := ipc.Fault{ID: "f4", Kind: string(domain.FaultClockSkew), FaultParams: domain.FaultParams{SkewS: 3600}}
	fault(t, svc, skew, true)
	resp := digestGet(t, "http://"+addr+infoPath, "admin", "ms1234")
	var info map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	at, err := time.Parse(time.RFC3339, info["systemTime"])
	if ahead := time.Until(at); err != nil || ahead < 59*time.Minute || ahead > 61*time.Minute {
		t.Fatalf("clock %q, %v", info["systemTime"], err)
	}
}

func TestNetworkDownRaisesItsEventAndCutsTheCamera(t *testing.T) {
	var got atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got.Add(1) }))
	defer target.Close()
	svc, addr := faultCamera(t, target.URL+"/events", nil)

	offline := ipc.Fault{ID: "n1", Kind: string(domain.FaultNetworkDown)}
	fault(t, svc, offline, true)
	var ev ipc.EventMsg
	if err := svc.wait(t, ipc.TypeEvent, 5*time.Second).Decode(&ev); err != nil || ev.Event.Type != "network_lost" || ev.Event.Trigger != "fault" {
		t.Fatalf("event %+v %v", ev, err)
	}
	// Its push cannot leave, and nobody reaches it.
	var rep engine.DeliveryReport
	if err := svc.wait(t, ipc.TypeDelivery, 5*time.Second).Decode(&rep); err != nil || rep.Status != engine.DeliveryRetry || !strings.Contains(rep.Error, "network is unreachable") {
		t.Fatalf("delivery %+v %v", rep, err)
	}
	if _, _, err := get(addr); err == nil {
		t.Fatal("a camera off the network answered")
	}
	fault(t, svc, offline, false)
	if code, _, err := get(addr); err != nil || code != http.StatusUnauthorized {
		t.Fatalf("back on the network: %d %v", code, err)
	}
	if got.Load() != 0 {
		t.Fatal("the target got the event while the camera was off the network")
	}

	// An IP conflict raises its event and the camera keeps answering.
	fault(t, svc, ipc.Fault{ID: "c1", Kind: string(domain.FaultIPConflict)}, true)
	if err := svc.wait(t, ipc.TypeEvent, 5*time.Second).Decode(&ev); err != nil || ev.Event.Type != "ip_conflict" {
		t.Fatalf("event %+v %v", ev, err)
	}
	if code, _, err := get(addr); err != nil || code != http.StatusUnauthorized {
		t.Fatalf("during the conflict: %d %v", code, err)
	}
}

// A camera starting with faults on has them from its first connection,
// and raises no event for them.
func TestCameraStartsWithItsFaults(t *testing.T) {
	svc, addr := faultCamera(t, "http://127.0.0.1:9/none", []ipc.Fault{
		{ID: "f1", Kind: string(domain.FaultServiceDown), FaultParams: domain.FaultParams{Instance: "http"}},
		{ID: "f2", Kind: string(domain.FaultIPConflict)},
	})
	if _, _, err := get(addr); err == nil {
		t.Fatal("a down service answered")
	}
	time.Sleep(200 * time.Millisecond)
	svc.mu.Lock()
	n := len(svc.msgs[ipc.TypeEvent])
	svc.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d events raised at start", n)
	}
}
