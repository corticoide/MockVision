package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/engines"
	"github.com/corticoide/mockvision/backend/internal/minisign"
	"github.com/corticoide/mockvision/backend/internal/netctl"
	"github.com/corticoide/mockvision/backend/internal/pkg"
	"github.com/corticoide/mockvision/backend/internal/store"
)

// runCatalogService runs a service on dir with the official catalog of the
// profiles directory, until the test ends or stop is called.
func runCatalogService(t *testing.T, dir string) (*Service, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt := netctl.NewLocalRuntime(testExe, log)
	svc, err := New(Options{DataDir: dir, FFmpeg: "ffmpeg", Exe: testExe, Runtime: rt, Log: log,
		Catalog: os.DirFS(filepath.Join("..", "..", "..", "profiles"))}, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = svc.Run(ctx)
		close(done)
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		<-done
		rt.Shutdown()
		st.Close()
	}
	t.Cleanup(stop)
	return svc, stop
}

func waitProfiles(t *testing.T, svc *Service, n int) []ProfileView {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		list, err := svc.ListProfiles(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(list) >= n || time.Now().After(deadline) {
			return list
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The node installs the official catalog when it starts, parents first,
// read only; starting again installs nothing twice (D81, D19).
func TestCatalogInstalledAtStart(t *testing.T) {
	dir := t.TempDir()
	svc, stop := runCatalogService(t, dir)
	list := waitProfiles(t, svc, 3)
	want := map[string]string{"milesight/base": "documented", "milesight/demo": "draft", "dahua/ipc-hdbw1230e-s4": "documented"}
	if len(list) != len(want) {
		t.Fatalf("installed %d profiles: %+v", len(list), list)
	}
	for _, p := range list {
		if want[p.ProfileID] != p.Level || p.Source != sourceCatalog || p.SignatureStatus != pkg.SignatureUnsigned {
			t.Fatalf("catalog profile %+v", p)
		}
	}
	stop()

	svc, _ = runCatalogService(t, dir)
	time.Sleep(time.Second)
	if again := waitProfiles(t, svc, 3); len(again) != 3 {
		t.Fatalf("starting again installed %d profiles", len(again))
	}
}

// A key added in Settings makes the packages it signs trusted.
func TestTrustedKeySignsImports(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	key, err := minisign.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddTrustedKey(ctx, testActor, TrustedKeyInput{Name: "Acme", PublicKey: "not a key"}); err == nil {
		t.Fatal("a malformed key was accepted")
	}
	k, err := svc.AddTrustedKey(ctx, testActor, TrustedKeyInput{Name: "Acme", PublicKey: string(key.Public().File())})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddTrustedKey(ctx, testActor, TrustedKeyInput{Name: "Again", PublicKey: key.Public().String()}); err == nil {
		t.Fatal("the same key was added twice")
	}

	data := demoVersionPackage(t, "0.7.1")
	signed, err := pkg.Sign(data, key)
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.ImportPackage(ctx, testActor, "demo.mvpkg", signed)
	if err != nil {
		t.Fatal(err)
	}
	if res.Profile.SignatureStatus != pkg.SignatureTrusted || res.Profile.Signer != "Acme" || res.Profile.Source != sourceUpload {
		t.Fatalf("imported %+v", res.Profile)
	}

	// Changed after signing: rejected.
	if err := svc.DeleteTrustedKey(ctx, testActor, k.ID); err != nil {
		t.Fatal(err)
	}
	keys, _ := svc.ListTrustedKeys(ctx)
	if len(keys) != 0 {
		t.Fatalf("keys left: %+v", keys)
	}
	// With the key gone, the next version signed by it counts as unsigned.
	next, _ := pkg.Sign(demoVersionPackage(t, "0.7.2"), key)
	res, err = svc.ImportPackage(ctx, testActor, "demo.mvpkg", next)
	if err != nil || res.Profile.SignatureStatus != pkg.SignatureUnsigned {
		t.Fatalf("after the key was removed: %+v %v", res, err)
	}
}

// demoVersionPackage packages the demo profile under another version.
func demoVersionPackage(t *testing.T, version string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "milesight-demo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	prof := strings.Replace(string(raw), "  version: 0.7.0\n", "  version: "+version+"\n", 1)
	if err := os.WriteFile(filepath.Join(dir, "profile.yaml"), []byte(prof), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("format: 1\nkind: profile\nid: milesight/demo\nversion: "+version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := pkg.Build(os.DirFS(dir))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A profile that extends another needs it installed, and is stored
// resolved with its parent pinned (D17).
func TestImportExtends(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	child := []byte(`schema: 1
profile:
  id: milesight/c2965
  version: 1.0.0
  name: Milesight C2965
  vendor: Milesight
  model: MS-C2965-PB
  extends: milesight/base@^0.1
`)
	_, err := svc.ImportPackage(ctx, testActor, "c2965.yaml", child)
	var ie *ImportError
	if !errors.As(err, &ie) || !strings.Contains(ie.Error()+problemsText(ie.Report), "import the parent first") {
		t.Fatalf("without its parent: %v", err)
	}
	base, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "milesight-base.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ImportPackage(ctx, testActor, "milesight-base.yaml", base); err != nil {
		t.Fatal(err)
	}
	res, err := svc.ImportPackage(ctx, testActor, "c2965.yaml", child)
	if err != nil {
		t.Fatal(err)
	}
	if res.Profile.Extends != "milesight/base@0.1.0" || res.Profile.Model != "MS-C2965-PB" {
		t.Fatalf("child %+v", res.Profile)
	}
	d, err := svc.GetProfile(ctx, "milesight/c2965", "1.0.0")
	if err != nil || len(d.Streams) == 0 || len(d.Engines) == 0 {
		t.Fatalf("the child has its parent's streams and engines: %+v %v", d, err)
	}
}

func problemsText(r pkg.Report) string {
	var b strings.Builder
	for _, p := range r.Problems {
		b.WriteString(p.Message + "\n")
	}
	return b.String()
}

// demoFixtures are recordings of the demo camera, as a capture of it would
// have them: its serial, address, MAC and times differ from any camera's.
const demoFixtures = `fixtures:
  - id: device-info
    request: { method: GET, path: /cgi-bin/operator/operator.cgi, query: { action: get.system.information } }
    response:
      status: 200
      headers: { Content-Type: application/json }
      body: |
        {"deviceName": "Network Camera", "vendor": "Milesight", "model": "MS-DEMO", "serialNumber": "6C0123456789",
         "macAddress": "1C:C3:16:00:00:01", "ipAddress": "192.168.5.190", "firmwareVersion": "demo-1.0.0",
         "systemTime": "2026-09-20T10:00:00Z"}
    vary:
      - { in: body, path: $.serialNumber, as: serial }
      - { in: body, path: $.macAddress, as: any }
      - { in: body, path: $.ipAddress, as: any }
      - { in: body, path: $.systemTime, as: timestamp }
  - id: brightness
    route: param-set
    steps:
      - request: { method: GET, path: /cgi-bin/operator/param.cgi, query: { action: set, Image.Brightness: "70" } }
        response: { status: 200, body: "OK\n" }
      - request: { method: GET, path: /cgi-bin/operator/param.cgi, query: { action: get, name: Image.Brightness } }
        response: { status: 200, body: "Image.Brightness=70\n" }
  - id: line-crossing
    trigger: { type: line_crossing, direction: "A->B" }
    expect:
      http_push:
        headers: { Content-Type: application/json }
        body: |
          {"eventType": "LineCrossing", "eventId": "x", "time": "2026-09-20T10:00:00.000Z",
           "device": {"name": "Network Camera", "serialNumber": "6C0123456789", "macAddress": "x", "ipAddress": "x"},
           "rule": {"id": "x", "name": "Line 1", "type": "line"}, "direction": "A->B", "object": null}
        vary:
          - { in: body, path: $.eventId, as: any }
          - { in: body, path: $.time, as: timestamp }
          - { in: body, path: $.device.serialNumber, as: serial }
          - { in: body, path: $.device.macAddress, as: any }
          - { in: body, path: $.device.ipAddress, as: any }
          - { in: body, path: $.rule.id, as: any }
          - { in: body, path: $.object, as: any }
`

// demoCaptured packages the demo profile with fixtures, as captured.
func demoCaptured(t *testing.T, version, fixtures string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "milesight-demo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	prof := strings.Replace(string(raw), "  version: 0.7.0\n", "  version: "+version+"\n", 1)
	for name, content := range map[string]string{
		"profile.yaml":           prof,
		"fixtures/recorded.yaml": fixtures,
		"manifest.yaml":          "format: 1\nkind: profile\nid: milesight/demo\nversion: " + version + "\nprovenance: { source: captured }\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := pkg.Build(os.DirFS(dir))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The import replays the fixtures against an ephemeral camera of the
// profile: all matching makes it captured; one that differs keeps it
// where it was and says why (D88).
func TestSelfTestEarnsCaptured(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	res, err := svc.ImportPackage(ctx, testActor, "demo.mvpkg", demoCaptured(t, "0.8.0", demoFixtures))
	if err != nil {
		t.Fatal(err)
	}
	r := res.Report
	if r.SelfTest == nil || r.SelfTest.Passed != 3 || r.Level != "captured" || res.Profile.Level != "captured" {
		t.Fatalf("self-test %+v, level %s", r.SelfTest, r.Level)
	}
	if r.Verified["route:http/device-info"] != "verified" || r.Verified["route:http/param-set"] != "verified" ||
		r.Verified["event:line_crossing"] != "verified" || r.Verified["route:http/snapshot"] != "declared" {
		t.Fatalf("coverage %v", r.Verified)
	}
	svc.mu.Lock()
	left := len(svc.selfTests)
	svc.mu.Unlock()
	if left != 0 {
		t.Fatal("the self-test camera is still registered")
	}

	wrong := strings.Replace(demoFixtures, `"model": "MS-DEMO"`, `"model": "MS-C2964"`, 1)
	res, err = svc.ImportPackage(ctx, testActor, "demo.mvpkg", demoCaptured(t, "0.8.1", wrong))
	if err != nil {
		t.Fatal(err)
	}
	r = res.Report
	if r.Level == "captured" || r.SelfTest.Failed != 1 || r.Verified["route:http/device-info"] != "failed" {
		t.Fatalf("a fixture that differs: level %s, %+v", r.Level, r.SelfTest)
	}
	var detail string
	for _, x := range r.SelfTest.Results {
		if x.ID == "device-info" {
			detail = x.Detail
		}
	}
	if !strings.Contains(detail, `$.model is "MS-DEMO", the device sent "MS-C2964"`) {
		t.Fatalf("detail %q", detail)
	}
}

// A profile exports as the package it was imported as, or wrapped in one;
// a duplicate is a new unsigned profile of its own (D19, D22).
func TestExportAndDuplicate(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	name, data, err := svc.ExportProfile(ctx, "milesight/demo", "0.7.0")
	if err != nil {
		t.Fatal(err)
	}
	if name != "milesight-demo-0.7.0.mvpkg" || !strings.HasPrefix(string(data), "PK") {
		t.Fatalf("export %s", name)
	}
	if res := pkg.Inspect(data, name, engines.Builtin(), pkg.Options{}); !res.Report.OK() || res.Report.ID != "milesight/demo" || res.Report.Version != "0.7.0" {
		t.Fatalf("exported package: %+v", res.Report)
	}

	res, err := svc.DuplicateProfile(ctx, testActor, "milesight/demo", "0.7.0", DuplicateInput{ProfileID: "acme/demo-copy", Name: "Our demo"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Profile.ProfileID != "acme/demo-copy" || res.Profile.Version != "0.1.0" || res.Profile.Name != "Our demo" ||
		res.Profile.Source != sourceDuplicate || res.Profile.SignatureStatus != pkg.SignatureUnsigned {
		t.Fatalf("duplicate %+v", res.Profile)
	}
	if _, err := svc.DuplicateProfile(ctx, testActor, "milesight/demo", "0.7.0", DuplicateInput{ProfileID: "acme/demo-copy"}); err == nil {
		t.Fatal("a second copy under the same version was accepted")
	}
	if _, err := svc.DuplicateProfile(ctx, testActor, "milesight/demo", "0.7.0", DuplicateInput{ProfileID: "not an id"}); err == nil {
		t.Fatal("a bad id was accepted")
	}
	// The copy keeps the comments of the original.
	_, copyData, err := svc.ExportProfile(ctx, "acme/demo-copy", "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	files, err := pkg.Files(copyData)
	if err != nil || !strings.Contains(string(files["profile.yaml"]), "# MockVision demo profile") {
		t.Fatalf("the copy lost its comments: %v", err)
	}
}

// Moving a camera to another version keeps what someone set when the new
// version accepts it, follows the new defaults otherwise, and says so
// first (D05).
func TestUpgradeCameraProfile(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cam := createCamera(t, svc, "Gate", false)
	if _, err := svc.UpdateCameraConfig(ctx, testActor, cam.ID, map[string]any{"Image.Brightness": 70}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "milesight-demo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	next := strings.Replace(string(raw), "  version: 0.7.0\n", "  version: 0.7.1\n", 1)
	next = strings.Replace(next, "    default: \"Network Camera\"\n", "    default: \"IP Camera\"\n", 1)
	next = strings.Replace(next, "    default: 50\n    min: 0\n    max: 100\n", "    default: 50\n    min: 0\n    max: 60\n", 1)
	next = strings.Replace(next, "\nstate:\n", "\nstate:\n  System.Location:\n    type: string\n    default: \"Gate\"\n", 1)
	if _, err := svc.ImportPackage(ctx, testActor, "demo.yaml", []byte(next)); err != nil {
		t.Fatal(err)
	}

	changes, err := svc.DiffProfiles(ctx, "milesight/demo", "0.7.0", "0.7.1")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, c := range changes {
		found[c.Section+":"+c.Key] = c.Kind
	}
	if found["state:System.Location"] != "added" || found["state:Image.Brightness"] != "changed" || found["state:System.DeviceName"] != "changed" {
		t.Fatalf("changes %+v", changes)
	}

	plan, err := svc.PlanProfileUpgrade(ctx, cam.ID, "0.7.1")
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]string{}
	for _, p := range plan.Params {
		actions[p.Key] = p.Action
	}
	if actions["Image.Brightness"] != UpgradeReset || actions["System.DeviceName"] != UpgradeDefault || actions["System.Location"] != UpgradeAdded || plan.Restart {
		t.Fatalf("plan %+v", plan.Params)
	}
	if _, err := svc.PlanProfileUpgrade(ctx, cam.ID, "0.7.0"); err == nil {
		t.Fatal("an upgrade to the version it runs was planned")
	}

	res, err := svc.UpgradeCameraProfile(ctx, testActor, cam.ID, "0.7.1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Camera.Profile.Version != "0.7.1" {
		t.Fatalf("camera %+v", res.Camera)
	}
	params, err := svc.CameraConfig(ctx, cam.ID)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{}
	for _, p := range params {
		values[p.Key] = p.Value
	}
	if fmt.Sprint(values["Image.Brightness"]) != "50" || values["System.DeviceName"] != "IP Camera" || values["System.Location"] != "Gate" {
		t.Fatalf("values after the upgrade: %v", values)
	}
	// It runs on the new version.
	if _, err := svc.StartCamera(ctx, testActor, cam.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, svc, cam.ID, domain.StateRunning)
}
