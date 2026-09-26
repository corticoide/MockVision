package app

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/netctl"
	"github.com/corticoide/mockvision/backend/internal/store"
)

// testExe is the mockvision binary the local runtime starts cameras with.
var testExe string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mockvision-app-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testExe = filepath.Join(dir, "mockvision")
	cmd := exec.Command("go", "build", "-o", testExe, "github.com/corticoide/mockvision/backend/cmd/mockvision")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building mockvision: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

var testActor = Actor{Type: "user", ID: "test", Name: "test", IP: "127.0.0.1"}

// newTestService runs a service in local mode, with the demo profile
// imported. Cameras are real processes answering on 127.0.0.1.
func newTestService(t *testing.T) *Service {
	t.Helper()
	return newTestServiceWith(t, "ffmpeg")
}

// newTestServiceWith is newTestService with another FFmpeg binary.
func newTestServiceWith(t *testing.T, ffmpeg string) *Service {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt := netctl.NewLocalRuntime(testExe, log)
	svc, err := New(Options{DataDir: dir, FFmpeg: ffmpeg, Exe: testExe, Runtime: rt, Log: log}, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = svc.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		rt.Shutdown()
		st.Close()
	})
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "milesight-demo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ImportPackage(ctx, testActor, "milesight-demo.yaml", data); err != nil {
		t.Fatal(err)
	}
	return svc
}

func createCamera(t *testing.T, svc *Service, name string, start bool) *CameraView {
	t.Helper()
	v, err := svc.CreateCamera(context.Background(), testActor, CreateCameraInput{
		Name: name, ProfileID: "milesight/demo", ProfileVersion: "0.2.0", Start: start,
	})
	if err != nil {
		t.Fatal(err)
	}
	if start {
		return waitState(t, svc, v.ID, domain.StateRunning)
	}
	return v
}

func waitState(t *testing.T, svc *Service, id string, want domain.CameraState) *CameraView {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		v, err := svc.GetCamera(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if v.Status.State == string(want) {
			return v
		}
		if v.Status.State == string(domain.StateError) {
			t.Fatalf("camera failed: %s", v.Status.Reason)
		}
		if time.Now().After(deadline) {
			t.Fatalf("camera did not reach %s (last %s)", want, v.Status.State)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func httpBase(t *testing.T, v *CameraView) string {
	t.Helper()
	for _, e := range v.Endpoints {
		if e.Protocol == "http" {
			return strings.TrimSuffix(e.URL, "/")
		}
	}
	t.Fatalf("camera has no HTTP endpoint: %+v", v.Endpoints)
	return ""
}

// digestGet requests url with HTTP Digest credentials and returns the
// status code and body.
func digestGet(t *testing.T, url, user, pass string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected a 401 challenge, got %d", resp.StatusCode)
	}
	params := map[string]string{}
	for _, kv := range strings.Split(strings.TrimPrefix(resp.Header.Get("WWW-Authenticate"), "Digest "), ", ") {
		k, v, _ := strings.Cut(kv, "=")
		params[k] = strings.Trim(v, `"`)
	}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	uri := req.URL.RequestURI()
	h := func(s string) string { sum := md5.Sum([]byte(s)); return hex.EncodeToString(sum[:]) }
	ha1 := h(user + ":" + params["realm"] + ":" + pass)
	ha2 := h("GET:" + uri)
	cnonce, nc := "0a4f113b", "00000001"
	response := h(ha1 + ":" + params["nonce"] + ":" + nc + ":" + cnonce + ":auth:" + ha2)
	req.Header.Set("Authorization", fmt.Sprintf(`Digest username=%q, realm=%q, nonce=%q, uri=%q, qop=auth, nc=%s, cnonce=%q, response=%q, opaque=%q, algorithm=MD5`,
		user, params["realm"], params["nonce"], uri, nc, cnonce, response, params["opaque"]))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func wantInvalid(t *testing.T, err error, field string) {
	t.Helper()
	var ve *domain.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want a validation error on %s, got %v", field, err)
	}
	for _, f := range ve.Fields {
		if f.Field == field {
			return
		}
	}
	t.Fatalf("want a validation error on %s, got %v", field, err)
}

func TestSetCameraUsersAppliesWithoutRestart(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cam := createCamera(t, svc, "Gate 1", true)
	base := httpBase(t, cam)
	pid := cam.Status.PID

	if code, _ := digestGet(t, base+"/snapshot.cgi", "admin", "ms1234"); code != http.StatusOK {
		t.Fatalf("factory account: %d", code)
	}

	_, err := svc.SetCameraUsers(ctx, testActor, cam.ID, []UserInput{{Username: "admin"}, {Username: "viewer", Role: "viewer"}})
	wantInvalid(t, err, "users[1].password")
	_, err = svc.SetCameraUsers(ctx, testActor, cam.ID, []UserInput{{Username: "op", Password: "x", Role: "operator"}})
	wantInvalid(t, err, "users")

	v, err := svc.SetCameraUsers(ctx, testActor, cam.ID, []UserInput{
		{Username: "admin", Password: "n3w-pass", Role: "admin"},
		{Username: "viewer", Password: "v-pass", Role: "viewer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Users) != 2 || v.Status.PID != pid || len(v.Status.PendingRestart) != 0 {
		t.Fatalf("users %+v, pid %d (was %d), pending %v", v.Users, v.Status.PID, pid, v.Status.PendingRestart)
	}
	if code, _ := digestGet(t, base+"/snapshot.cgi", "admin", "ms1234"); code != http.StatusUnauthorized {
		t.Fatalf("old password still accepted: %d", code)
	}
	if code, _ := digestGet(t, base+"/snapshot.cgi", "admin", "n3w-pass"); code != http.StatusOK {
		t.Fatalf("new password: %d", code)
	}
	if code, _ := digestGet(t, base+"/snapshot.cgi", "viewer", "v-pass"); code != http.StatusOK {
		t.Fatalf("new account: %d", code)
	}

	// An empty password keeps the current one; a missing account is removed.
	if _, err := svc.SetCameraUsers(ctx, testActor, cam.ID, []UserInput{{Username: "admin", Role: "admin"}}); err != nil {
		t.Fatal(err)
	}
	if code, _ := digestGet(t, base+"/snapshot.cgi", "admin", "n3w-pass"); code != http.StatusOK {
		t.Fatalf("kept password: %d", code)
	}
	if code, _ := digestGet(t, base+"/snapshot.cgi", "viewer", "v-pass"); code != http.StatusUnauthorized {
		t.Fatalf("removed account still accepted: %d", code)
	}
}

func TestUpdateCameraStreamRegeneratesWithoutRestart(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cam := createCamera(t, svc, "Gate 1", true)
	base := httpBase(t, cam)

	_, err := svc.UpdateCameraStream(ctx, testActor, cam.ID, "main", StreamUpdate{Resolution: ptr("123x45")})
	wantInvalid(t, err, "resolution")
	_, err = svc.UpdateCameraStream(ctx, testActor, cam.ID, "main", StreamUpdate{})
	wantInvalid(t, err, "")

	v, err := svc.UpdateCameraStream(ctx, testActor, cam.ID, "main", StreamUpdate{Resolution: ptr("640x360"), FPS: ptr(10)})
	if err != nil {
		t.Fatal(err)
	}
	if s := v.Streams[0]; s.Resolution != "640x360" || s.FPS != 10 || len(v.Status.PendingRestart) != 0 {
		t.Fatalf("stream %+v, pending %v", s, v.Status.PendingRestart)
	}
	params, err := svc.CameraConfig(ctx, cam.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range params {
		if p.Key == "Encode.Main.Resolution" && (p.Value != "640x360" || p.Origin != "panel") {
			t.Fatalf("bound parameter: %+v", p)
		}
	}

	// The running camera switches to the new rendition: its snapshot shrinks.
	deadline := time.Now().Add(60 * time.Second)
	for {
		code, body := digestGet(t, base+"/snapshot.cgi", "admin", "ms1234")
		if code == http.StatusOK {
			if cfg, err := jpeg.DecodeConfig(strings.NewReader(string(body))); err == nil && cfg.Width == 640 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the camera did not switch to the 640x360 stream")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if v, _ := svc.GetCamera(ctx, cam.ID); v.Status.PID != cam.Status.PID {
		t.Fatalf("the camera restarted: pid %d, was %d", v.Status.PID, cam.Status.PID)
	}
}

func TestNetworkAndProtocolsWaitForRestart(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cam := createCamera(t, svc, "Gate 1", true)

	_, err := svc.UpdateCamera(ctx, testActor, cam.ID, UpdateCameraInput{Network: &NetworkInput{DNS: []string{"not-an-ip"}}})
	wantInvalid(t, err, "network.dns")
	v, err := svc.UpdateCamera(ctx, testActor, cam.ID, UpdateCameraInput{Network: &NetworkInput{DNS: []string{"9.9.9.9"}}})
	if err != nil {
		t.Fatal(err)
	}
	if v.Network.MAC != cam.Network.MAC || len(v.Network.DNS) != 1 {
		t.Fatalf("network %+v", v.Network)
	}
	if strings.Join(v.Status.PendingRestart, ",") != "network" {
		t.Fatalf("pending restart %v", v.Status.PendingRestart)
	}

	_, err = svc.SetCameraProtocols(ctx, testActor, cam.ID, []ProtocolInput{{Instance: "http", Port: ptr(554)}})
	wantInvalid(t, err, "protocols")
	_, err = svc.SetCameraProtocols(ctx, testActor, cam.ID, []ProtocolInput{{Instance: "push", Port: ptr(5)}})
	wantInvalid(t, err, "protocols[0].port")
	_, err = svc.SetCameraProtocols(ctx, testActor, cam.ID, []ProtocolInput{{Instance: "onvif"}})
	wantInvalid(t, err, "protocols[0].instance")
	v, err = svc.SetCameraProtocols(ctx, testActor, cam.ID, []ProtocolInput{{Instance: "http", Port: ptr(8081)}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(v.Status.PendingRestart, ",") != "network,protocols" {
		t.Fatalf("pending restart %v", v.Status.PendingRestart)
	}

	if _, err := svc.RestartCamera(ctx, testActor, cam.ID); err != nil {
		t.Fatal(err)
	}
	v = waitState(t, svc, cam.ID, domain.StateRunning)
	if len(v.Status.PendingRestart) != 0 {
		t.Fatalf("pending restart after the restart: %v", v.Status.PendingRestart)
	}
	for _, p := range v.Protocols {
		if p.Instance == "http" && (p.Port != 8081 || p.DefaultPort != 80 || p.Role != "server") {
			t.Fatalf("protocol %+v", p)
		}
	}

	// Disabling the HTTP API removes its endpoint after a restart.
	if _, err := svc.SetCameraProtocols(ctx, testActor, cam.ID, []ProtocolInput{{Instance: "http", Enabled: ptr(false)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RestartCamera(ctx, testActor, cam.ID); err != nil {
		t.Fatal(err)
	}
	v = waitState(t, svc, cam.ID, domain.StateRunning)
	for _, e := range v.Endpoints {
		if e.Instance == "http" {
			t.Fatalf("disabled protocol still served: %+v", e)
		}
	}
}

func TestResetAndCloneCamera(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	src := createCamera(t, svc, "Gate 1", false)

	if _, err := svc.UpdateCameraConfig(ctx, testActor, src.ID, map[string]any{"Image.Brightness": 70}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetCameraUsers(ctx, testActor, src.ID, []UserInput{{Username: "root", Password: "x-pass", Role: "admin"}}); err != nil {
		t.Fatal(err)
	}

	_, err := svc.CloneCamera(ctx, testActor, src.ID, CloneCameraInput{Name: "Gate 1"})
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("clone with a taken name: %v", err)
	}
	clone, err := svc.CloneCamera(ctx, testActor, src.ID, CloneCameraInput{Name: "Gate 2"})
	if err != nil {
		t.Fatal(err)
	}
	if clone.ID == src.ID || clone.Network.MAC == src.Network.MAC || clone.Serial == src.Serial {
		t.Fatalf("clone shares identity: %+v", clone)
	}
	if len(clone.Users) != 1 || clone.Users[0].Username != "root" || clone.Streams[0].AssetID != src.Streams[0].AssetID {
		t.Fatalf("clone users %+v streams %+v", clone.Users, clone.Streams)
	}
	if got := paramValue(t, svc, clone.ID, "Image.Brightness"); fmt.Sprint(got) != "70" {
		t.Fatalf("clone brightness %v", got)
	}
	clone = waitState(t, svc, clone.ID, domain.StateStopped)
	if _, err := svc.StartCamera(ctx, testActor, clone.ID); err != nil {
		t.Fatal(err)
	}
	clone = waitState(t, svc, clone.ID, domain.StateRunning)
	if code, _ := digestGet(t, httpBase(t, clone)+"/snapshot.cgi", "root", "x-pass"); code != http.StatusOK {
		t.Fatalf("clone account: %d", code)
	}

	_, err = svc.ResetCamera(ctx, testActor, src.ID, "everything")
	wantInvalid(t, err, "scope")
	v, err := svc.ResetCamera(ctx, testActor, src.ID, ResetSettings)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Users) != 1 || v.Users[0].Username != "admin" {
		t.Fatalf("users after reset: %+v", v.Users)
	}
	if got := paramValue(t, svc, src.ID, "Image.Brightness"); fmt.Sprint(got) != "50" {
		t.Fatalf("brightness after reset: %v", got)
	}

	// A reset reboots a running camera, which then takes the factory account.
	v, err = svc.ResetCamera(ctx, testActor, clone.ID, ResetFull)
	if err != nil {
		t.Fatal(err)
	}
	v = waitState(t, svc, clone.ID, domain.StateRunning)
	if code, _ := digestGet(t, httpBase(t, v)+"/snapshot.cgi", "admin", "ms1234"); code != http.StatusOK {
		t.Fatalf("factory account after a full reset: %d", code)
	}
	if v.Network.MAC != clone.Network.MAC {
		t.Fatalf("full reset changed the MAC derived from the ID: %s, was %s", v.Network.MAC, clone.Network.MAC)
	}
}

func paramValue(t *testing.T, svc *Service, id, key string) any {
	t.Helper()
	params, err := svc.CameraConfig(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range params {
		if p.Key == key {
			return p.Value
		}
	}
	t.Fatalf("no parameter %s", key)
	return nil
}

func ptr[T any](v T) *T { return &v }
