package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/pkg"
)

// helloPackage builds the example plugin and packs it, with its manifest's
// version.
func helloPackage(t *testing.T, version string) []byte {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "bin", "linux-"+runtime.GOARCH, "hello"), "github.com/corticoide/mockvision/examples/plugins/hello")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the example plugin: %v\n%s", err, out)
	}
	man := "format: 1\nkind: plugin\nid: examples/hello\nversion: " + version + "\nrequires: { contract: 1 }\n" +
		"plugin: { engine: hello, executable: hello, permissions: [net.listen, state.read, state.write, events.emit] }\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(man), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := pkg.Build(os.DirFS(dir))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// helloProfile is the demo profile with the example plugin's engine.
func helloProfile(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "milesight-demo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(data), "  id: milesight/demo\n  version: 0.7.0\n", "  id: examples/hello-cam\n  version: 0.1.0\n", 1)
	s = strings.Replace(s, "\nengines:\n", "\nengines:\n  hello:\n    engine: hello@^1\n    port: 7000\n    greeting: HI\n", 1)
	s = strings.Replace(s, "\nevents:\n", "\nevents:\n  motion: { vendor_name: Motion }\n", 1)
	return []byte(s)
}

func TestPluginLifecycle(t *testing.T) {
	// The service's umask: the plugin is still readable by the cameras.
	t.Cleanup(func() { syscall.Umask(syscall.Umask(0o077)) })
	svc := newTestService(t)
	ctx := context.Background()

	// A program that is not what its manifest says is not installed.
	_, err := svc.ImportPackage(ctx, testActor, "hello.mvpkg", helloPackage(t, "1.0.1"))
	var ie *ImportError
	if !errors.As(err, &ie) || !strings.Contains(problemsText(ie.Report), `the program is version "1.0.0", the package "1.0.1"`) {
		t.Fatalf("mismatched plugin: %v %s", err, problemsText(ie.Report))
	}

	res, err := svc.ImportPackage(ctx, testActor, "hello.mvpkg", helloPackage(t, "1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	p := res.Plugin
	if p == nil || res.Profile != nil || p.Engine != "hello" || p.Version != "1.0.0" || p.Enabled || p.SignatureStatus != pkg.SignatureUnsigned ||
		len(p.Sockets) != 1 || p.Sockets[0].DefaultPort != 7000 || strings.Join(p.Permissions, ",") != "net.listen,state.read,state.write,events.emit" {
		t.Fatalf("plugin = %+v", p)
	}

	for _, f := range []string{"", "bin", filepath.Join("bin", "linux-"+runtime.GOARCH, "hello")} {
		fi, err := os.Stat(filepath.Join(svc.opts.DataDir, "plugins", res.Report.SHA256, f))
		if err != nil || fi.Mode().Perm() != 0o755 {
			t.Fatalf("%s: %v %v", f, fi.Mode(), err)
		}
	}

	// Until it is enabled, profiles cannot use its engine.
	if _, err := svc.ImportPackage(ctx, testActor, "hello-cam.yaml", helloProfile(t)); !errors.As(err, &ie) || !strings.Contains(problemsText(ie.Report), "unknown engine") {
		t.Fatalf("profile with a disabled plugin: %v", err)
	}
	// An unsigned plugin is enabled only where the settings allow it.
	var conflict *domain.ConflictError
	if _, err := svc.SetPluginEnabled(ctx, testActor, p.ID, true); !errors.As(err, &conflict) {
		t.Fatalf("enabled an unsigned plugin: %v", err)
	}
	allow := true
	if _, err := svc.UpdateSettings(ctx, testActor, SettingsPatch{AllowUnsignedPlugins: &allow}); err != nil {
		t.Fatal(err)
	}
	if p, err = svc.SetPluginEnabled(ctx, testActor, p.ID, true); err != nil || !p.Enabled || p.ApprovedBy == "" || p.ApprovedAt == nil {
		t.Fatalf("enable: %+v %v", p, err)
	}
	if _, err := svc.ImportPackage(ctx, testActor, "hello-cam.yaml", helloProfile(t)); err != nil {
		t.Fatal(err)
	}

	// A camera of that profile runs the plugin on its port.
	v, err := svc.CreateCamera(ctx, testActor, CreateCameraInput{Name: "Hello", ProfileID: "examples/hello-cam", ProfileVersion: "0.1.0", Start: true})
	if err != nil {
		t.Fatal(err)
	}
	v = waitState(t, svc, v.ID, domain.StateRunning)
	var addr string
	for _, e := range v.Endpoints {
		if e.Engine == "hello" && e.Protocol == "tcp" {
			addr = strings.TrimPrefix(e.URL, "tcp://")
		}
	}
	if addr == "" {
		t.Fatalf("no plugin endpoint: %+v", v.Endpoints)
	}
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	sc := bufio.NewScanner(c)
	if !sc.Scan() || sc.Text() != fmt.Sprintf("HI %s Network Camera", v.Serial) {
		t.Fatalf("greeting: %q %v", sc.Text(), sc.Err())
	}

	// Taking the permission back disables the plugin.
	deny := false
	if _, err := svc.UpdateSettings(ctx, testActor, SettingsPatch{AllowUnsignedPlugins: &deny}); err != nil {
		t.Fatal(err)
	}
	if p, err = svc.GetPlugin(ctx, p.ID); err != nil || p.Enabled {
		t.Fatalf("after the settings: %+v %v", p, err)
	}
	list, err := svc.ListPlugins(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
}
