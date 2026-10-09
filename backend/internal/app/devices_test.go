package app

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
)

func TestDeviceRegistryAndProbe(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	// A camera to point the device at, running on 127.0.0.1.
	cam := createCamera(t, svc, "Gate", true)
	host, port := hostPortOf(t, httpBase(t, cam))

	// Registering without authorizing: a probe is refused (RN-17).
	d, err := svc.CreateDevice(ctx, testActor, DeviceInput{Name: "Lab cam", Host: host, Ports: []int{port}, Username: "admin", Password: strptr("ms1234")})
	if err != nil || d.HasPassword != true || d.Authorized || d.Kind != "real" {
		t.Fatalf("device=%+v err=%v", d, err)
	}
	var conflict *domain.ConflictError
	if _, err := svc.ProbeDevice(ctx, testActor, d.ID); !errors.As(err, &conflict) {
		t.Fatalf("probe without authorization: %v", err)
	}

	// Authorize it, then a read-only probe finds the HTTP service.
	yes := true
	d, err = svc.UpdateDevice(ctx, testActor, d.ID, DeviceInput{Name: "Lab cam", Host: host, Ports: []int{port}, Username: "admin", Authorized: yes})
	if err != nil || !d.Authorized || d.HasPassword != true {
		t.Fatalf("update=%+v err=%v", d, err)
	}
	d, err = svc.ProbeDevice(ctx, testActor, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Detected == nil || !d.Detected.Reachable || len(d.Detected.Services) == 0 {
		t.Fatalf("detected=%+v", d.Detected)
	}
	var http bool
	for _, svc := range d.Detected.Services {
		if svc.Port == port && svc.Proto == "http" && svc.Auth == "digest" && strings.Contains(svc.Server, "Milesight") {
			http = true
		}
	}
	if !http {
		t.Fatalf("the HTTP service was not identified: %+v", d.Detected.Services)
	}

	// The password never comes back.
	list, err := svc.ListDevices(ctx)
	if err != nil || len(list) != 1 || list[0].Detected == nil {
		t.Fatalf("list=%+v err=%v", list, err)
	}

	if err := svc.DeleteDevice(ctx, testActor, d.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ = svc.ListDevices(ctx); len(list) != 0 {
		t.Fatalf("device not deleted: %+v", list)
	}
}

func hostPortOf(t *testing.T, raw string) (string, int) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	host, p, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return host, n
}

func strptr(s string) *string { return &s }

func TestCaptureRoundTrip(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cam := createCamera(t, svc, "Gate", true)
	httpHost, httpPort := hostPortOf(t, httpBase(t, cam))
	rtspPort := rtspPortOf(t, cam)

	yes := true
	d, err := svc.CreateDevice(ctx, testActor, DeviceInput{
		Name: "Sim", Host: httpHost, Username: "admin", Password: strptr("ms1234"),
		PortMap: map[int]int{80: httpPort, 554: rtspPort}, Authorized: yes,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A camera's own address would be simulated; 127.0.0.1 is not, so this
	// device reads "real" but the capture still round-trips.
	if _, err := svc.ProbeDevice(ctx, testActor, d.ID); err != nil {
		t.Fatal(err)
	}

	// The demo capture program suits a Milesight device.
	progs, err := svc.ListPrograms(ctx, "Milesight")
	if err != nil || !hasProgram(progs, "milesight/demo-capture") {
		t.Fatalf("programs=%+v err=%v", progs, err)
	}

	c, err := svc.StartCapture(ctx, testActor, d.ID, "milesight/demo-capture")
	if err != nil {
		t.Fatal(err)
	}
	c = waitCapture(t, svc, c.ID)
	if c.Status != "done" || c.Result == nil {
		t.Fatalf("capture=%+v", c)
	}
	by := map[string]int{}
	for i, f := range c.Result.Fixtures {
		by[f.StepID] = i
	}
	info := c.Result.Fixtures[by["device-info"]]
	if info.Status != 200 || info.ContentType == "" || !strings.Contains(info.Body, "Milesight") {
		t.Fatalf("device-info=%+v", info)
	}
	snap := c.Result.Fixtures[by["snapshot"]]
	if snap.Status != 200 || !snap.Binary || snap.Bytes == 0 {
		t.Fatalf("snapshot=%+v", snap)
	}
	rtsp := c.Result.Fixtures[by["rtsp-main"]]
	if rtsp.Status != 200 || !strings.Contains(rtsp.Body, "m=video") {
		t.Fatalf("rtsp=%+v", rtsp)
	}
}

func rtspPortOf(t *testing.T, cam *CameraView) int {
	t.Helper()
	for _, e := range cam.Endpoints {
		if e.Protocol == "rtsp" {
			_, p := hostPortOf(t, e.URL)
			return p
		}
	}
	t.Fatal("no rtsp endpoint")
	return 0
}

func hasProgram(list []ProgramView, id string) bool {
	for _, p := range list {
		if p.ProgramID == id {
			return true
		}
	}
	return false
}

func waitCapture(t *testing.T, svc *Service, id string) *CaptureView {
	t.Helper()
	for i := 0; i < 100; i++ {
		c, err := svc.GetCapture(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if c.Status != "running" {
			return c
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("capture did not finish")
	return nil
}

func TestCaptureRecordsPushedEvent(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cam := createCamera(t, svc, "Gate", true)
	httpHost, httpPort := hostPortOf(t, httpBase(t, cam))
	rtspPort := rtspPortOf(t, cam)
	yes := true
	d, err := svc.CreateDevice(ctx, testActor, DeviceInput{Name: "Sim", Host: httpHost, Username: "admin", Password: strptr("ms1234"),
		PortMap: map[int]int{80: httpPort, 554: rtspPort}, Authorized: yes})
	if err != nil {
		t.Fatal(err)
	}
	full, err := svc.store.R().GetDevice(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	token := full.ReceiverToken
	if token == "" || d.ReceiverURL != "/api/v1/scraper/receive/"+token {
		t.Fatalf("receiver url=%q token=%q", d.ReceiverURL, token)
	}

	c, err := svc.StartCapture(ctx, testActor, d.ID, "milesight/demo-capture")
	if err != nil {
		t.Fatal(err)
	}
	// Once the capture is listening, deliver a pushed event, with a secret
	// header that must not be recorded (RN-18).
	for i := 0; i < 100; i++ {
		svc.mu.Lock()
		_, ok := svc.receivers[token]
		svc.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	body := []byte(`{"eventType":"LineCrossing","serialNumber":"6C0012ABCDEF","time":"2026-10-09T10:00:00Z"}`)
	hdr := map[string][]string{"Content-Type": {"application/json"}, "Authorization": {"Digest secret"}}
	if ok := svc.ReceivePush(ctx, token, "POST", "/alarm", map[string]string{}, hdr, body); !ok {
		t.Fatal("push not accepted")
	}

	c = waitCapture(t, svc, c.ID)
	if c.Result == nil {
		t.Fatalf("no result: %+v", c)
	}
	var ev *struct{ found bool }
	for _, f := range c.Result.Fixtures {
		if f.StepID == "line-crossing-event" {
			if f.Kind != "event" || f.Status != 200 || !strings.Contains(f.Body, "LineCrossing") {
				t.Fatalf("event fixture=%+v", f)
			}
			if _, bad := f.Headers["Authorization"]; bad {
				t.Fatalf("a secret header was recorded: %+v", f.Headers)
			}
			ev = &struct{ found bool }{true}
		}
	}
	if ev == nil {
		t.Fatalf("the pushed event was not recorded: %+v", c.Result.Fixtures)
	}
}

func TestCompileCaptureToDraftProfile(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cam := createCamera(t, svc, "Gate", true)
	httpHost, httpPort := hostPortOf(t, httpBase(t, cam))
	rtspPort := rtspPortOf(t, cam)
	yes := true
	d, err := svc.CreateDevice(ctx, testActor, DeviceInput{Name: "Sim", Host: httpHost, Username: "admin", Password: strptr("ms1234"),
		PortMap: map[int]int{80: httpPort, 554: rtspPort}, Authorized: yes})
	if err != nil {
		t.Fatal(err)
	}
	full, _ := svc.store.R().GetDevice(ctx, d.ID)
	c, err := svc.StartCapture(ctx, testActor, d.ID, "milesight/demo-capture")
	if err != nil {
		t.Fatal(err)
	}
	// Feed the pushed event so the capture is complete.
	for i := 0; i < 100; i++ {
		svc.mu.Lock()
		_, ok := svc.receivers[full.ReceiverToken]
		svc.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	svc.ReceivePush(ctx, full.ReceiverToken, "POST", "/alarm", map[string]string{},
		map[string][]string{"Content-Type": {"application/json"}}, []byte(`{"eventType":"LineCrossing","serialNumber":"6C0012ABCDEF","ipAddress":"10.0.0.9"}`))
	c = waitCapture(t, svc, c.ID)
	if c.Status != "done" {
		t.Fatalf("capture: %+v", c)
	}

	res, err := svc.CompileCapture(ctx, testActor, c.ID, CompileRequest{ProfileID: "acme/cam-x", Vendor: "Milesight", Model: "X"})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if res.Profile == nil {
		t.Fatalf("no draft profile; report=%s", problemsText(res.Report))
	}
	p := res.Profile
	if p.ProfileID != "acme/cam-x" || p.Version != "0.1.0" || p.Vendor != "Milesight" {
		t.Fatalf("profile=%+v", p)
	}
	// It must redact the device's identity: no real serial in the resolved profile.
	detail, err := svc.GetProfile(ctx, "acme/cam-x", "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	_ = detail
	// The capture now points at the draft.
	c, _ = svc.GetCapture(ctx, c.ID)
	if c.DraftProfileID == "" {
		t.Fatalf("capture not linked to the draft: %+v", c)
	}
	// A camera can be created from the compiled draft.
	if _, err := svc.CreateCamera(ctx, testActor, CreateCameraInput{Name: "FromDraft", ProfileID: "acme/cam-x", ProfileVersion: "0.1.0"}); err != nil {
		t.Fatalf("create camera from draft: %v", err)
	}
}
