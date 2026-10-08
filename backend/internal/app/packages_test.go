package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
