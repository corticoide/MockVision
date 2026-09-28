package rtsp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph265"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/pion/rtp"

	"github.com/corticoide/mockvision/backend/internal/tmpl"
	"github.com/corticoide/mockvision/sdk/engine"
)

// fakeHost is the smallest camera the engine runs in: one stream and the
// accounts the test asks for.
type fakeHost struct {
	src   *engine.VideoSource
	users fakeAccounts
}

func (h fakeHost) Accounts() engine.Accounts   { return h.users }
func (h fakeHost) State() engine.State         { return nil }
func (h fakeHost) Events() engine.Events       { return nil }
func (h fakeHost) Media() engine.Media         { return fakeMedia{h.src} }
func (h fakeHost) Templates() engine.Templates { return tmpl.NewCompiler(tmpl.Env{}) }
func (h fakeHost) Files() engine.Files         { return nil }
func (h fakeHost) Telemetry() engine.Telemetry { return fakeTelemetry{} }

type fakeAccounts []engine.User

func (a fakeAccounts) List() []engine.User { return a }
func (a fakeAccounts) Lookup(name string) (engine.User, bool) {
	for _, u := range a {
		if u.Username == name {
			return u, true
		}
	}
	return engine.User{}, false
}

type fakeMedia struct{ src *engine.VideoSource }

func (m fakeMedia) Streams() []engine.StreamInfo               { return []engine.StreamInfo{m.src.Info} }
func (m fakeMedia) Snapshot(string) ([]byte, error)            { return nil, errors.New("none") }
func (m fakeMedia) Source(string) (*engine.VideoSource, error) { return m.src, nil }
func (m fakeMedia) Watch(func(string)) func()                  { return func() {} }

type fakeTelemetry struct{}

func (fakeTelemetry) Log(slog.Level, string, ...any)             {}
func (fakeTelemetry) Request(string, string, int, time.Duration) {}
func (fakeTelemetry) Gap(string, string, string)                 {}
func (fakeTelemetry) Client(string, string, bool)                {}

// gop is a made-up H.265 group of pictures: a keyframe, then predicted
// frames. The engine only packetizes NAL units, so only their headers and
// the PPS, which clients parse, must be real.
func gop(fps, frames int) *engine.VideoSource {
	nalu := func(typ h265.NALUType, size int) []byte {
		n := make([]byte, size)
		n[0], n[1] = byte(typ)<<1, 1
		return n
	}
	src := &engine.VideoSource{
		Info: engine.StreamInfo{Name: "sub", Codec: "h265", Width: 320, Height: 180, FPS: fps, GOP: frames},
		VPS:  nalu(h265.NALUType_VPS_NUT, 24),
		SPS:  nalu(h265.NALUType_SPS_NUT, 40),
		PPS:  []byte{0x44, 0x01, 0xc0},
	}
	src.AccessUnits = append(src.AccessUnits, [][]byte{nalu(h265.NALUType_IDR_W_RADL, 6000)})
	for range frames - 1 {
		src.AccessUnits = append(src.AccessUnits, [][]byte{nalu(h265.NALUType_TRAIL_R, 40)})
	}
	return src
}

func startEngine(t *testing.T, src *engine.VideoSource) string {
	t.Helper()
	return startEngineWith(t, fakeHost{src: src}, engine.Identity{},
		`{"engine": "rtsp@^1", "auth": {"scheme": "none"}, "paths": {"sub": "/sub"}}`)
}

func startEngineWith(t *testing.T, h fakeHost, id engine.Identity, config string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := New()
	err = e.Start(context.Background(), engine.StartInput{
		Instance:  "rtsp",
		Identity:  id,
		Config:    []byte(config),
		Listeners: map[string]net.Listener{"rtsp": ln},
		Host:      h,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop(context.Background()) })
	return ln.Addr().String()
}

// play watches a stream over TCP until it has n frames.
func play(t *testing.T, addr string, n int) [][][]byte {
	t.Helper()
	return playURL(t, "rtsp://"+addr+"/sub", n)
}

func playURL(t *testing.T, url string, n int) [][][]byte {
	t.Helper()
	u, err := base.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	tcp := gortsplib.ProtocolTCP
	c := gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: &tcp}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatal(err)
	}
	var forma *format.H265
	medi := desc.FindFormat(&forma)
	if medi == nil {
		t.Fatal("no H.265 media")
	}
	dec, err := forma.CreateDecoder()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		t.Fatal(err)
	}
	frames := make(chan [][]byte, n)
	failed := make(chan error, 1)
	c.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
		au, err := dec.Decode(pkt)
		switch {
		case errors.Is(err, rtph265.ErrMorePacketsNeeded):
		case err != nil: // the viewer joined in the middle of a frame
			select {
			case failed <- err:
			default:
			}
		default:
			select {
			case frames <- au:
			default:
			}
		}
	})
	if _, err := c.Play(nil); err != nil {
		t.Fatal(err)
	}
	var out [][][]byte
	timeout := time.After(5 * time.Second)
	for len(out) < n {
		select {
		case au := <-frames:
			out = append(out, au)
		case err := <-failed:
			t.Fatalf("decoding the stream: %v", err)
		case <-timeout:
			t.Fatalf("%d frames in 5 s, want %d", len(out), n)
		}
	}
	return out
}

// Every viewer starts at a keyframe, whenever its PLAY falls between two
// frames: H.265 decoders cannot start on a predicted frame.
func TestViewersStartAtAKeyframe(t *testing.T) {
	const fps = 25
	addr := startEngine(t, gop(fps, 10))
	for i := range 20 {
		// A play ends right after a frame: wait a different part of the
		// interval each time, so the PLAY falls anywhere between frames.
		time.Sleep(time.Second / fps * time.Duration(i) / 20)
		aus := play(t, addr, 3)
		if !h265.IsRandomAccess(aus[0]) {
			t.Fatalf("play %d: the first frame is not a keyframe", i)
		}
		if h265.IsRandomAccess(aus[1]) || h265.IsRandomAccess(aus[2]) {
			t.Fatalf("play %d: the stream must go on with predicted frames", i)
		}
	}
}

// The camera challenges with its own realm and names its server, as a
// Dahua does with "Login to <serial>" and "Rtsp Server/3.0"; clients sign
// with that realm, and a wrong password gets a new challenge.
func TestChallengeNamesTheCamera(t *testing.T) {
	h := fakeHost{src: gop(25, 10), users: fakeAccounts{{Username: "admin", Password: "secret", Role: "admin"}}}
	addr := startEngineWith(t, h, engine.Identity{Serial: "4F0ABCDPAG12345"}, `{"engine": "rtsp@^1",
		"auth": {"scheme": "digest", "realm": "Login to {{ .Camera.Serial }}"}, "server": "Rtsp Server/3.0",
		"paths": {"sub": "/sub"}}`)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	for cseq, auth := range []string{"", `Authorization: Digest username="admin", realm="Login to 4F0ABCDPAG12345", nonce="x", uri="rtsp://` + addr + `/sub", response="0"` + "\r\n"} {
		fmt.Fprintf(conn, "DESCRIBE rtsp://%s/sub RTSP/1.0\r\nCSeq: %d\r\n%s\r\n", addr, cseq+1, auth)
		var res base.Response
		if err := res.Unmarshal(br); err != nil {
			t.Fatal(err)
		}
		challenge := strings.Join(res.Header["WWW-Authenticate"], " ")
		if res.StatusCode != base.StatusUnauthorized || !strings.HasPrefix(challenge, `Digest realm="Login to 4F0ABCDPAG12345", nonce="`) {
			t.Fatalf("request %d: %d, challenge %q", cseq+1, res.StatusCode, challenge)
		}
		if got := res.Header["Server"]; len(got) != 1 || got[0] != "Rtsp Server/3.0" {
			t.Fatalf("request %d: server %q", cseq+1, got)
		}
	}
	playURL(t, "rtsp://admin:secret@"+addr+"/sub", 1)
}
