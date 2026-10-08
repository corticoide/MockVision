package camera

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/engines"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/sdk/engine"
	"github.com/corticoide/mockvision/sdk/plugin"
)

var (
	pluginBuild    sync.Once
	pluginExe      string // the MockVision binary
	pluginDir      string // the example plugin, unpacked
	pluginBuildErr error
)

// buildPlugin builds MockVision and the example plugin once.
func buildPlugin(t *testing.T) (string, string) {
	t.Helper()
	pluginBuild.Do(func() {
		dir, err := os.MkdirTemp("", "mockvision-plugin-test")
		if err != nil {
			pluginBuildErr = err
			return
		}
		pluginExe = filepath.Join(dir, "mockvision")
		pluginDir = filepath.Join(dir, "hello")
		for _, b := range [][2]string{
			{pluginExe, "github.com/corticoide/mockvision/backend/cmd/mockvision"},
			{filepath.Join(pluginDir, "bin", "linux-"+runtime.GOARCH, "hello"), "github.com/corticoide/mockvision/examples/plugins/hello"},
		} {
			cmd := exec.Command("go", "build", "-o", b[0], b[1])
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				pluginBuildErr = fmt.Errorf("building %s: %v\n%s", b[1], err, out)
				return
			}
		}
	})
	if pluginBuildErr != nil {
		t.Fatal(pluginBuildErr)
	}
	return pluginExe, pluginDir
}

// pluginCamera starts a camera of the demo profile with the example
// plugin's engine, granted perms, and returns its port's address.
func pluginCamera(t *testing.T, perms []string) (*fakeService, string, ipc.Plugin) {
	t.Helper()
	exe, dir := buildPlugin(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	spec := ipc.Plugin{Engine: "hello", Version: "1.0.0", Dir: dir, Executable: "hello", Permissions: perms}
	desc, err := DescribePlugin(ctx, exe, spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.Descriptor = desc

	data, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "milesight-demo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.Replace(string(data), "\nengines:\n",
		"\nengines:\n  hello:\n    engine: hello@^1\n    port: 7000\n    greeting: HI\n    param: System.DeviceName\n", 1)
	data = []byte(strings.Replace(doc, "\nevents:\n", "\nevents:\n  motion: { vendor_name: Motion }\n", 1))
	res := profile.Validate(profile.Input{Data: data}, engines.Builtin().With(map[string]engine.Factory{"hello": engines.Described(desc)}))
	if !res.OK() {
		t.Fatalf("profile invalid: %+v", res.Problems)
	}

	svcSide, camSide := net.Pipe()
	svc := &fakeService{msgs: map[string][]*ipc.Envelope{}, got: make(chan *ipc.Envelope, 64)}
	svc.conn = ipc.NewConn(svcSide, svc.handle, nil)
	go func() { _ = svc.conn.Run(ctx) }()
	rt := NewRuntime(Options{CameraID: "cam1", IPC: camSide, Local: true, Exe: exe})
	done := make(chan struct{})
	go func() { _ = rt.Run(ctx); close(done) }()
	t.Cleanup(func() {
		_ = svc.conn.Request(context.Background(), ipc.TypeStop, ipc.Stop{DeadlineMS: 2000}, nil)
		<-done
	})
	svc.wait(t, ipc.TypeHello, 5*time.Second)
	cfg := ipc.Configure{
		Identity: engine.Identity{CameraID: "cam1", Name: "Gate", IP: "127.0.0.1", MAC: "02:aa:bb:cc:dd:ee", Serial: "6C0012ABCDEF", Vendor: "Milesight", Model: "MS-DEMO"},
		Profile:  res.Resolved,
		Engines:  []ipc.EngineConfig{{Instance: "hello", Enabled: true, Port: 7000}},
		Users:    []engine.User{{Username: "admin", Password: "ms1234", Role: "admin"}},
		Plugins:  []ipc.Plugin{spec},
	}
	if err := svc.conn.Request(ctx, ipc.TypeConfigure, cfg, nil); err != nil {
		t.Fatalf("configure: %v", err)
	}
	var ready ipc.Ready
	if err := svc.wait(t, ipc.TypeReady, 20*time.Second).Decode(&ready); err != nil {
		t.Fatal(err)
	}
	for _, ep := range ready.Endpoints {
		if ep.Instance == "hello" {
			return svc, fmt.Sprintf("127.0.0.1:%d", ep.Port), spec
		}
	}
	t.Fatal("no hello endpoint")
	return nil, "", spec
}

// helloClient is a connection to the example plugin's port.
type helloClient struct {
	c  net.Conn
	sc *bufio.Scanner
}

// dialHello connects and returns the greeting, retrying while the plugin
// starts again.
func dialHello(t *testing.T, addr string, within time.Duration) (*helloClient, string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = c.SetDeadline(time.Now().Add(3 * time.Second))
			h := &helloClient{c: c, sc: bufio.NewScanner(c)}
			if h.sc.Scan() {
				return h, h.sc.Text()
			}
			c.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("no greeting from %s within %s: %v", addr, within, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (h *helloClient) say(t *testing.T, line string) string {
	t.Helper()
	fmt.Fprintln(h.c, line)
	if !h.sc.Scan() {
		t.Fatalf("no answer to %q: %v", line, h.sc.Err())
	}
	return h.sc.Text()
}

func TestPluginEngine(t *testing.T) {
	svc, addr, spec := pluginCamera(t, []string{plugin.PermNetListen, plugin.PermStateRead, plugin.PermStateWrite, plugin.PermEventsEmit})
	if spec.Descriptor.Name != "hello" || len(spec.Descriptor.Sockets) != 1 || spec.Descriptor.Sockets[0].DefaultPort != 7000 {
		t.Fatalf("descriptor: %+v", spec.Descriptor)
	}

	// The greeting reads the camera's identity and state through the host.
	h, greeting := dialHello(t, addr, 5*time.Second)
	if !strings.HasPrefix(greeting, "HI 6C0012ABCDEF ") || strings.HasSuffix(greeting, "<nil>") {
		t.Fatalf("greeting: %q", greeting)
	}
	if got := h.say(t, "set Lobby"); got != "OK" {
		t.Fatalf("set: %q", got)
	}
	if got := h.say(t, "event"); !strings.HasPrefix(got, "EMITTED ") {
		t.Fatalf("event: %q", got)
	}
	var ev ipc.EventMsg
	if err := svc.wait(t, ipc.TypeEvent, 5*time.Second).Decode(&ev); err != nil || ev.Event.Type != "motion" {
		t.Fatalf("event: %+v %v", ev, err)
	}
	if got := h.say(t, "nonsense"); got != "?" {
		t.Fatalf("unknown command: %q", got)
	}
	h.c.Close()
	h, greeting = dialHello(t, addr, 5*time.Second)
	if greeting != "HI 6C0012ABCDEF Lobby" {
		t.Fatalf("greeting after set: %q", greeting)
	}

	// The plugin dies: the camera starts it again on the same socket.
	fmt.Fprintln(h.c, "crash")
	h.c.Close()
	h, greeting = dialHello(t, addr, 10*time.Second)
	if greeting != "HI 6C0012ABCDEF Lobby" {
		t.Fatalf("greeting after a restart: %q", greeting)
	}
	h.c.Close()
}

func TestPluginPermissions(t *testing.T) {
	_, addr, _ := pluginCamera(t, []string{plugin.PermNetListen})
	h, greeting := dialHello(t, addr, 5*time.Second)
	defer h.c.Close()
	// Without state.read the value is not there.
	if greeting != "HI 6C0012ABCDEF <nil>" {
		t.Fatalf("greeting: %q", greeting)
	}
	if got := h.say(t, "set Lobby"); !strings.HasPrefix(got, "DENIED ") || !strings.Contains(got, plugin.PermStateWrite) {
		t.Fatalf("set: %q", got)
	}
	if got := h.say(t, "event"); !strings.HasPrefix(got, "DENIED ") || !strings.Contains(got, plugin.PermEventsEmit) {
		t.Fatalf("event: %q", got)
	}
}

func TestPluginKeepsFailing(t *testing.T) {
	first, max := pluginFirstWait, pluginMaxWait
	pluginFirstWait, pluginMaxWait = 10*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() { pluginFirstWait, pluginMaxWait = first, max })
	svc, addr, _ := pluginCamera(t, []string{plugin.PermNetListen})
	for i := 0; i <= pluginMaxFailures; i++ {
		h, _ := dialHello(t, addr, 10*time.Second)
		fmt.Fprintln(h.c, "crash")
		_, _ = h.c.Read(make([]byte, 1))
		h.c.Close()
	}
	var f ipc.Failed
	if err := svc.wait(t, ipc.TypeFailed, 10*time.Second).Decode(&f); err != nil || !strings.Contains(f.Reason, "6 times in a row") {
		t.Fatalf("failed: %+v %v", f, err)
	}
}
