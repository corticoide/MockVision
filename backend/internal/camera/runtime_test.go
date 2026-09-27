package camera

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/engines"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/media"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/sdk/engine"
)

// fakeService is the service end of the IPC socket.
type fakeService struct {
	conn *ipc.Conn
	mu   sync.Mutex
	msgs map[string][]*ipc.Envelope
	got  chan *ipc.Envelope
}

func (f *fakeService) handle(_ context.Context, m *ipc.Envelope) (any, error) {
	f.mu.Lock()
	f.msgs[m.Type] = append(f.msgs[m.Type], m)
	f.mu.Unlock()
	select {
	case f.got <- m:
	default:
	}
	return nil, nil
}

func (f *fakeService) wait(t *testing.T, typ string, timeout time.Duration) *ipc.Envelope {
	t.Helper()
	deadline := time.After(timeout)
	for {
		f.mu.Lock()
		if list := f.msgs[typ]; len(list) > 0 {
			m := list[0]
			f.msgs[typ] = list[1:]
			f.mu.Unlock()
			return m
		}
		f.mu.Unlock()
		select {
		case <-f.got:
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			t.Fatalf("no %s message within %s", typ, timeout)
		}
	}
}

func demoResolved(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "milesight-demo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	res := profile.Validate(profile.Input{Data: data}, engines.Builtin())
	if !res.OK() {
		t.Fatalf("demo profile invalid: %+v", res.Problems)
	}
	return res.Resolved
}

func digestGet(t *testing.T, url, user, pass string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 challenge, got %d", resp.StatusCode)
	}
	ch := resp.Header.Get("WWW-Authenticate")
	params := map[string]string{}
	for _, kv := range strings.Split(strings.TrimPrefix(ch, "Digest "), ", ") {
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
	return resp
}

func TestCameraEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Media: a test pattern encoded as a 640x360 rendition.
	dir := t.TempDir()
	lib, err := media.NewLibrary(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "pattern.jpg")
	if err := lib.TestPattern(ctx, src); err != nil {
		t.Fatal(err)
	}
	// One rendition per stream of the demo profile: main in H.264, sub in
	// H.265 and third in MJPEG.
	var streams []ipc.Stream
	for _, st := range []struct {
		name string
		p    media.Params
	}{
		{"main", media.Params{Codec: "h264", Width: 640, Height: 360, FPS: 15, GOP: 30, Bitrate: 1024}},
		{"sub", media.Params{Codec: "h265", Width: 320, Height: 180, FPS: 10, GOP: 20, Bitrate: 256}},
		{"third", media.Params{Codec: "mjpeg", Width: 352, Height: 288, FPS: 5, GOP: 1, Bitrate: 512}},
	} {
		files, err := lib.Encode(ctx, src, st.p, st.p.Key("test"))
		if err != nil {
			t.Fatal(err)
		}
		streams = append(streams, ipc.Stream{Name: st.name, Codec: st.p.Codec, Width: st.p.Width, Height: st.p.Height, FPS: st.p.FPS,
			GOP: st.p.GOP, Bitrate: st.p.Bitrate, StreamPath: files.Stream, SnapshotPath: files.Snapshot})
	}

	// Event target.
	received := make(chan []byte, 4)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received <- b
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	svcSide, camSide := net.Pipe()
	svc := &fakeService{msgs: map[string][]*ipc.Envelope{}, got: make(chan *ipc.Envelope, 64)}
	svc.conn = ipc.NewConn(svcSide, svc.handle, nil)
	go svc.conn.Run(ctx)

	rt := NewRuntime(Options{CameraID: "cam1", IPC: camSide, Local: true})
	runDone := make(chan error, 1)
	go func() { runDone <- rt.Run(ctx) }()

	svc.wait(t, ipc.TypeHello, 5*time.Second)
	cfg := ipc.Configure{
		Identity: engine.Identity{CameraID: "cam1", Name: "Gate", IP: "127.0.0.1", MAC: "02:aa:bb:cc:dd:ee", Serial: "6C0012ABCDEF", Vendor: "Milesight", Model: "MS-DEMO", Firmware: "demo-1.0.0"},
		Profile:  demoResolved(t),
		State:    map[string]any{"Encode.Main.Resolution": "640x360"},
		Engines:  []ipc.EngineConfig{{Instance: "http", Enabled: true, Port: 80}, {Instance: "rtsp", Enabled: true, Port: 554}, {Instance: "push", Enabled: true}},
		Users:    []engine.User{{Username: "admin", Password: "ms1234", Role: "admin"}},
		Streams:  streams,
		Targets:  []ipc.Target{{Target: engine.Target{ID: "t1", Name: "test", Type: "http", URL: target.URL + "/events"}}},
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

	t.Run("snapshot", func(t *testing.T) {
		resp := digestGet(t, httpBase+"/snapshot.cgi", "admin", "ms1234")
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/jpeg" || len(body) < 100 || body[0] != 0xff || body[1] != 0xd8 {
			t.Fatalf("snapshot: %d %s %d bytes", resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		resp := digestGet(t, httpBase+"/snapshot.cgi", "admin", "nope")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", resp.StatusCode)
		}
	})

	t.Run("device info", func(t *testing.T) {
		resp := digestGet(t, httpBase+"/cgi-bin/operator/operator.cgi?action=get.system.information", "admin", "ms1234")
		defer resp.Body.Close()
		var info map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
			t.Fatal(err)
		}
		if info["serialNumber"] != "6C0012ABCDEF" || info["macAddress"] != "02:AA:BB:CC:DD:EE" || info["deviceName"] != "Network Camera" {
			t.Fatalf("device info = %v", info)
		}
	})

	t.Run("parameters", func(t *testing.T) {
		resp := digestGet(t, httpBase+"/cgi-bin/operator/param.cgi?action=set&Image.Brightness=70", "admin", "ms1234")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "OK\n" {
			t.Fatalf("set: %d %q", resp.StatusCode, body)
		}
		var sc ipc.StateChanged
		if err := svc.wait(t, ipc.TypeStateChanged, 2*time.Second).Decode(&sc); err != nil || len(sc.Changes) != 1 || sc.Changes[0].Origin.Kind != engine.OriginClient {
			t.Fatalf("state.changed = %+v, %v", sc, err)
		}
		resp = digestGet(t, httpBase+"/cgi-bin/operator/param.cgi?action=get&name=Image.Brightness", "admin", "ms1234")
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "Image.Brightness=70\n" {
			t.Fatalf("get: %q", body)
		}
		resp = digestGet(t, httpBase+"/cgi-bin/operator/param.cgi?action=set&Image.Brightness=700", "admin", "ms1234")
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("out of range must be rejected, got %d", resp.StatusCode)
		}
	})

	t.Run("unknown route", func(t *testing.T) {
		resp := digestGet(t, httpBase+"/cgi-bin/nope.cgi?secret=1", "admin", "ms1234")
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected the profile's 404, got %d", resp.StatusCode)
		}
		var gap ipc.Gap
		if err := svc.wait(t, ipc.TypeGap, 2*time.Second).Decode(&gap); err != nil || gap.Summary != "GET /cgi-bin/nope.cgi?secret" {
			t.Fatalf("gap = %+v, %v", gap, err)
		}
	})

	t.Run("rtsp", func(t *testing.T) {
		if _, err := exec.LookPath("ffprobe"); err != nil {
			t.Skip("ffprobe not installed")
		}
		url := fmt.Sprintf("rtsp://admin:ms1234@127.0.0.1:%d/main", ports["rtsp/rtsp"])
		out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-rtsp_transport", "tcp", "-select_streams", "v:0",
			"-show_entries", "stream=codec_name,width,height", "-of", "csv=p=0", url).CombinedOutput()
		if err != nil {
			t.Fatalf("ffprobe: %v: %s", err, out)
		}
		if strings.TrimSpace(string(out)) != "h264,640,360" {
			t.Fatalf("ffprobe = %q", out)
		}
		// Decode more than one GOP: timestamps must keep growing across the
		// loop, or FFmpeg reports non-monotonic DTS.
		out, err = exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-rtsp_transport", "tcp", "-i", url,
			"-frames:v", "45", "-f", "null", "-").CombinedOutput()
		if err != nil || len(strings.TrimSpace(string(out))) > 0 {
			t.Fatalf("decoding the stream: %v: %s", err, out)
		}
	})

	t.Run("sub and third streams", func(t *testing.T) {
		if _, err := exec.LookPath("ffprobe"); err != nil {
			t.Skip("ffprobe not installed")
		}
		for _, c := range []struct {
			path, want string
			frames     int
		}{
			{"/sub", "hevc,320,180", 25},   // more than one GOP of 20
			{"/third", "mjpeg,352,288", 8}, // every frame the same JPEG
		} {
			url := fmt.Sprintf("rtsp://admin:ms1234@127.0.0.1:%d%s", ports["rtsp/rtsp"], c.path)
			out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-rtsp_transport", "tcp", "-select_streams", "v:0",
				"-show_entries", "stream=codec_name,width,height", "-of", "csv=p=0", url).CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != c.want {
				t.Fatalf("ffprobe %s = %q, %v", c.path, out, err)
			}
			out, err = exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-rtsp_transport", "tcp", "-i", url,
				"-frames:v", fmt.Sprint(c.frames), "-f", "null", "-").CombinedOutput()
			if err != nil || len(strings.TrimSpace(string(out))) > 0 {
				t.Fatalf("decoding %s: %v: %s", c.path, err, out)
			}
		}
	})

	t.Run("line crossing", func(t *testing.T) {
		var res ipc.TriggerResult
		if err := svc.conn.Request(ctx, ipc.TypeTrigger, ipc.Trigger{Type: "line_crossing"}, &res); err != nil {
			t.Fatal(err)
		}
		var ev ipc.EventMsg
		if err := svc.wait(t, ipc.TypeEvent, 2*time.Second).Decode(&ev); err != nil || ev.Event.ID != res.Event.ID {
			t.Fatalf("event = %+v, %v", ev, err)
		}
		select {
		case body := <-received:
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("payload is not JSON: %s", body)
			}
			if payload["eventType"] != "LineCrossing" || payload["direction"] != "A->B" || payload["eventId"] != res.Event.ID {
				t.Fatalf("payload = %s", body)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("target received nothing")
		}
		var d engine.DeliveryReport
		if err := svc.wait(t, ipc.TypeDelivery, 5*time.Second).Decode(&d); err != nil || d.Status != engine.DeliveryOK || d.HTTPStatus != 200 {
			t.Fatalf("delivery = %+v, %v", d, err)
		}
		if err := svc.conn.Request(ctx, ipc.TypeTrigger, ipc.Trigger{Type: "line_crossing"}, nil); err == nil {
			t.Fatal("min_interval must rate limit a second event")
		}
	})

	var hb ipc.Heartbeat
	if err := svc.wait(t, ipc.TypeHeartbeat, 5*time.Second).Decode(&hb); err != nil || hb.RSSBytes == 0 {
		t.Fatalf("heartbeat = %+v, %v", hb, err)
	}

	if err := svc.conn.Request(ctx, ipc.TypeStop, ipc.Stop{DeadlineMS: 2000}, nil); err != nil {
		t.Fatal(err)
	}
	svc.wait(t, ipc.TypeBye, 5*time.Second)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}
