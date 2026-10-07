package camera

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/base"

	"github.com/corticoide/mockvision/backend/internal/engines"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/media"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/sdk/engine"
)

// A camera of the Dahua draft answers as the unit does: its realm names
// it, the CGI programs answer key=value lines, configManager reads and
// writes its tables by name, and RTSP serves Dahua's paths.
func TestDahuaDraftCamera(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	data, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "dahua-ipc-hdbw1230e-s4.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	res := profile.Validate(profile.Input{Data: data}, engines.Builtin())
	if !res.OK() {
		t.Fatalf("Dahua draft invalid: %+v", res.Problems)
	}

	// Small renditions: the test checks the paths, not the picture.
	dir := t.TempDir()
	lib, err := media.NewLibrary(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "pattern.jpg")
	if err := lib.TestPattern(ctx, src); err != nil {
		t.Fatal(err)
	}
	var streams []ipc.Stream
	for _, st := range []struct {
		name string
		p    media.Params
	}{
		{"main", media.Params{Codec: "h264", Width: 640, Height: 360, FPS: 15, GOP: 30, Bitrate: 1024}},
		{"sub", media.Params{Codec: "h264", Width: 352, Height: 240, FPS: 15, GOP: 30, Bitrate: 256}},
	} {
		files, err := lib.Encode(ctx, src, st.p, st.p.Key("test"))
		if err != nil {
			t.Fatal(err)
		}
		streams = append(streams, ipc.Stream{Name: st.name, Codec: st.p.Codec, Width: st.p.Width, Height: st.p.Height, FPS: st.p.FPS,
			GOP: st.p.GOP, Bitrate: st.p.Bitrate, StreamPath: files.Stream, SnapshotPath: files.Snapshot})
	}

	svcSide, camSide := net.Pipe()
	svc := &fakeService{msgs: map[string][]*ipc.Envelope{}, got: make(chan *ipc.Envelope, 64)}
	svc.conn = ipc.NewConn(svcSide, svc.handle, nil)
	go func() { _ = svc.conn.Run(ctx) }()
	rt := NewRuntime(Options{CameraID: "cam1", IPC: camSide, Local: true})
	go func() { _ = rt.Run(ctx) }()
	svc.wait(t, ipc.TypeHello, 5*time.Second)

	const serial = "4E0AB2EPAG00B3B"
	cfg := ipc.Configure{
		Identity: engine.Identity{CameraID: "cam1", Name: "Hall", IP: "127.0.0.1", MAC: "3c:ef:8c:01:02:03", Serial: serial,
			Vendor: "Dahua", Model: "IPC-HDBW1230E-S4", Firmware: "2.800.0000000.25.R"},
		Profile: res.Resolved,
		Engines: []ipc.EngineConfig{{Instance: "http", Enabled: true, Port: 80}, {Instance: "rtsp", Enabled: true, Port: 554}},
		Users:   []engine.User{{Username: "admin", Password: "admin1234", Role: "admin"}},
		Streams: streams,
	}
	if err := svc.conn.Request(ctx, ipc.TypeConfigure, cfg, nil); err != nil {
		t.Fatalf("configure: %v", err)
	}
	var ready ipc.Ready
	if err := svc.wait(t, ipc.TypeReady, 5*time.Second).Decode(&ready); err != nil {
		t.Fatal(err)
	}
	ports := map[string]int{}
	for _, ep := range ready.Endpoints {
		ports[ep.Instance+"/"+ep.Socket] = ep.Port
	}
	httpBase := fmt.Sprintf("http://127.0.0.1:%d", ports["http/http"])
	rtspAddr := fmt.Sprintf("127.0.0.1:%d", ports["rtsp/rtsp"])
	get := func(t *testing.T, target string) (int, string) {
		t.Helper()
		resp := digestGet(t, httpBase+target, "admin", "admin1234")
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	t.Run("realm", func(t *testing.T) {
		resp, err := http.Get(httpBase + "/cgi-bin/magicBox.cgi?action=getDeviceType")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if ch := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(ch, `Digest realm="Login to `+serial+`"`) {
			t.Fatalf("challenge %q", ch)
		}
	})

	t.Run("magicBox", func(t *testing.T) {
		for target, want := range map[string]string{
			"/cgi-bin/magicBox.cgi?action=getDeviceType":      "type=IPC-HDBW1230E-S4\r\n",
			"/cgi-bin/magicBox.cgi?action=getSerialNo":        "sn=" + serial + "\r\n",
			"/cgi-bin/magicBox.cgi?action=getSoftwareVersion": "version=2.800.0000000.25.R,build:2020-06-18\r\n",
			"/cgi-bin/magicBox.cgi?action=getMachineName":     "name=" + serial + "\r\n",
		} {
			if code, body := get(t, target); code != http.StatusOK || body != want {
				t.Errorf("%s: %d %q, want %q", target, code, body, want)
			}
		}
		if code, body := get(t, "/cgi-bin/magicBox.cgi?action=nope"); code != http.StatusBadRequest || body != "Error\r\nBad Request!\r\n" {
			t.Errorf("unknown action: %d %q", code, body)
		}
	})

	t.Run("clock", func(t *testing.T) {
		code, body := get(t, "/cgi-bin/global.cgi?action=getCurrentTime")
		if code != http.StatusOK || !regexp.MustCompile(`^result=\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\r\n$`).MatchString(body) {
			t.Fatalf("%d %q", code, body)
		}
	})

	t.Run("configManager", func(t *testing.T) {
		code, body := get(t, "/cgi-bin/configManager.cgi?action=getConfig&name=Encode[0].MainFormat[0].Video")
		for _, line := range []string{"table.Encode[0].MainFormat[0].Video.Compression=H.264\r\n", "table.Encode[0].MainFormat[0].Video.FPS=30\r\n",
			"table.Encode[0].MainFormat[0].Video.Width=1920\r\n"} {
			if code != http.StatusOK || !strings.Contains(body, line) {
				t.Fatalf("getConfig Encode: %d %q lacks %q", code, body, line)
			}
		}
		if strings.Contains(body, "ExtraFormat") {
			t.Fatalf("getConfig Encode[0].MainFormat[0].Video answered the sub stream: %q", body)
		}
		if code, body := get(t, "/cgi-bin/configManager.cgi?action=getConfig&name=General"); code != http.StatusOK || body != "table.General.MachineName="+serial+"\r\n" {
			t.Fatalf("the unit is named after its serial: %d %q", code, body)
		}
		if code, body := get(t, "/cgi-bin/configManager.cgi?action=setConfig&General.MachineName=Hall&Encode[0].MainFormat[0].Video.FPS=15"); code != http.StatusOK || body != "OK\r\n" {
			t.Fatalf("setConfig: %d %q", code, body)
		}
		var sc ipc.StateChanged
		if err := svc.wait(t, ipc.TypeStateChanged, 2*time.Second).Decode(&sc); err != nil || len(sc.Changes) != 2 {
			t.Fatalf("state.changed = %+v, %v", sc, err)
		}
		if code, body := get(t, "/cgi-bin/magicBox.cgi?action=getMachineName"); code != http.StatusOK || body != "name=Hall\r\n" {
			t.Fatalf("renamed: %d %q", code, body)
		}
		if code, body := get(t, "/cgi-bin/configManager.cgi?action=getConfig&name=VideoColor"); code != http.StatusOK || !strings.Contains(body, "table.VideoColor[0][0].Brightness=50\r\n") {
			t.Fatalf("getConfig VideoColor: %d %q", code, body)
		}
	})

	t.Run("Dahua's values and errors", func(t *testing.T) {
		if code, body := get(t, "/cgi-bin/configManager.cgi?action=getConfig&name=Nope"); code != http.StatusBadRequest || body != "Error\r\nBad Request!\r\n" {
			t.Fatalf("an unknown table: %d %q", code, body)
		}
		// A width the main stream has, with a height it lacks for it.
		if code, body := get(t, "/cgi-bin/configManager.cgi?action=setConfig&Encode[0].ExtraFormat[0].Video.Height=240"); code != http.StatusBadRequest || body != "Error\r\nBad Request!\r\n" {
			t.Fatalf("704x240: %d %q", code, body)
		}
		target := "/cgi-bin/configManager.cgi?action=setConfig&Encode[0].MainFormat[0].Video.Compression=H.265&Encode[0].MainFormat[0].Video.Width=1280&Encode[0].MainFormat[0].Video.Height=720"
		if code, body := get(t, target); code != http.StatusOK || body != "OK\r\n" {
			t.Fatalf("H.265 at 1280x720: %d %q", code, body)
		}
		var sc ipc.StateChanged
		if err := svc.wait(t, ipc.TypeStateChanged, 2*time.Second).Decode(&sc); err != nil || len(sc.Changes) != 3 || sc.Changes[0].Bind != "media.main.codec" {
			t.Fatalf("state.changed = %+v, %v", sc, err)
		}
	})

	t.Run("attach", func(t *testing.T) {
		resp := digestGet(t, httpBase+"/cgi-bin/eventManager.cgi?action=attach&codes=[VideoMotion,VideoBlind]&heartbeat=1", "admin", "admin1234")
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || ct != "multipart/x-mixed-replace; boundary=myboundary" {
			t.Fatalf("%d %q", resp.StatusCode, ct)
		}
		parts := make(chan string, 16)
		go func() {
			br := bufio.NewReader(resp.Body)
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					close(parts)
					return
				}
				if strings.HasPrefix(line, "Code=") || strings.HasPrefix(line, "Heartbeat") {
					parts <- strings.TrimSpace(line)
				}
			}
		}()
		next := func(want string) {
			t.Helper()
			deadline := time.After(8 * time.Second)
			for {
				select {
				case p, ok := <-parts:
					if !ok {
						t.Fatalf("the stream ended waiting for %q", want)
					}
					if strings.HasPrefix(p, "Code=IPConflict") {
						t.Fatalf("a code the client did not ask for reached it: %q", p)
					}
					if p == want {
						return
					}
				case <-deadline:
					t.Fatalf("no %q", want)
				}
			}
		}
		next("Heartbeat")
		// A code the client did not ask for does not reach it.
		if err := svc.conn.Request(ctx, ipc.TypeTrigger, ipc.Trigger{Type: "ip_conflict"}, nil); err != nil {
			t.Fatal(err)
		}
		if err := svc.conn.Request(ctx, ipc.TypeTrigger, ipc.Trigger{Type: "motion"}, nil); err != nil {
			t.Fatal(err)
		}
		next("Code=VideoMotion;action=Start;index=0")
		next("Code=VideoMotion;action=Stop;index=0")
	})

	t.Run("login failure", func(t *testing.T) {
		resp := digestGet(t, httpBase+"/cgi-bin/magicBox.cgi?action=getDeviceType", "admin", "wrong")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong password: %d", resp.StatusCode)
		}
		for {
			var ev ipc.EventMsg
			if err := svc.wait(t, ipc.TypeEvent, 3*time.Second).Decode(&ev); err != nil {
				t.Fatal(err)
			}
			if ev.Event.Type == "custom:login_failure" {
				if ev.Event.Custom["client"] != "127.0.0.1" {
					t.Fatalf("event %+v", ev.Event)
				}
				break
			}
		}
	})

	t.Run("rtsp challenge", func(t *testing.T) {
		conn, err := net.Dial("tcp", rtspAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprintf(conn, "DESCRIBE rtsp://%s/cam/realmonitor?channel=1&subtype=0 RTSP/1.0\r\nCSeq: 1\r\n\r\n", rtspAddr)
		var res base.Response
		if err := res.Unmarshal(bufio.NewReader(conn)); err != nil {
			t.Fatal(err)
		}
		challenge := strings.Join(res.Header["WWW-Authenticate"], " ")
		if res.StatusCode != base.StatusUnauthorized || !strings.HasPrefix(challenge, `Digest realm="Login to `+serial+`"`) ||
			len(res.Header["Server"]) != 1 || res.Header["Server"][0] != "Rtsp Server/3.0" {
			t.Fatalf("%d, challenge %q, server %q", res.StatusCode, challenge, res.Header["Server"])
		}
	})

	t.Run("rtsp paths", func(t *testing.T) {
		if _, err := exec.LookPath("ffprobe"); err != nil {
			t.Skip("ffprobe not installed")
		}
		for subtype, want := range map[string]string{"0": "h264,640,360", "1": "h264,352,240"} {
			url := fmt.Sprintf("rtsp://admin:admin1234@%s/cam/realmonitor?channel=1&subtype=%s", rtspAddr, subtype)
			out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-rtsp_transport", "tcp", "-select_streams", "v:0",
				"-show_entries", "stream=codec_name,width,height", "-of", "csv=p=0", url).CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != want {
				t.Fatalf("subtype=%s: %q, %v", subtype, out, err)
			}
		}
	})
}
