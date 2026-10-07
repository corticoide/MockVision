package rtsp

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph265"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/pion/rtp"

	"github.com/corticoide/mockvision/sdk/engine"
)

// fakeFiles is a card with clips.
type fakeFiles struct {
	state string
	clips []engine.FileInfo
}

func (f *fakeFiles) Status() engine.StorageStatus {
	return engine.StorageStatus{Kind: "sd", State: f.state}
}

func (f *fakeFiles) Find(_ context.Context, q engine.FileQuery) ([]engine.FileInfo, error) {
	if !f.Status().Usable() {
		return nil, engine.ErrStorageUnavailable
	}
	var out []engine.FileInfo
	for _, c := range f.clips {
		if !c.Start.After(q.To) && !c.End.Before(q.From) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeFiles) Open(context.Context, string) (io.ReadCloser, engine.FileInfo, error) {
	return nil, engine.FileInfo{}, engine.ErrNoFile
}

const playbackConfig = `{"engine": "rtsp@^1", "auth": {"scheme": "none"}, "paths": {"sub": "/sub"},
	"playback": {"path": "/cam/playback?channel=1", "start": "starttime", "end": "endtime", "time_format": "2006_01_02_15_04_05"}}`

// playToEnd plays a URL until the camera ends the session, and returns
// the frames it got.
func playToEnd(t *testing.T, url string) [][][]byte {
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
	dec, _ := forma.CreateDecoder()
	if _, err := c.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		t.Fatal(err)
	}
	frames := make(chan [][]byte, 1000)
	c.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
		if au, err := dec.Decode(pkt); err == nil {
			frames <- au
		} else if !errors.Is(err, rtph265.ErrMorePacketsNeeded) {
			t.Errorf("decoding: %v", err)
		}
	})
	if _, err := c.Play(nil); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { ended <- c.Wait() }()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the playback did not end")
	}
	close(frames)
	var out [][][]byte
	for au := range frames {
		out = append(out, au)
	}
	return out
}

func describeStatus(t *testing.T, url string) base.StatusCode {
	t.Helper()
	u, _ := base.ParseURL(url)
	c := gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, res, err := c.Describe(u)
	if res == nil {
		t.Fatalf("describe: %v", err)
	}
	return res.StatusCode
}

// A playback plays the clips of the range, from its keyframe, for as long
// as they last; then the camera ends it.
func TestPlayback(t *testing.T) {
	at := time.Date(2026, 10, 7, 14, 30, 0, 0, time.UTC)
	files := &fakeFiles{state: engine.StoragePresent, clips: []engine.FileInfo{
		{Name: "a.ts", Kind: engine.FileClip, Stream: "sub", Start: at, End: at.Add(time.Second)},
	}}
	addr := startEngineWith(t, fakeHost{src: gop(25, 10), files: files}, engine.Identity{}, playbackConfig)
	url := "rtsp://" + addr + "/cam/playback?channel=1&starttime=2026_10_07_14_29_00&endtime=2026_10_07_14_31_00"
	start := time.Now()
	aus := playToEnd(t, url)
	if len(aus) < 23 || len(aus) > 25 || !h265.IsRandomAccess(aus[0]) {
		t.Fatalf("%d frames, the first a keyframe: %v", len(aus), len(aus) > 0 && h265.IsRandomAccess(aus[0]))
	}
	if d := time.Since(start); d < 900*time.Millisecond {
		t.Fatalf("a second of recording played in %s", d)
	}
	// Only the part in the range plays.
	half := "rtsp://" + addr + "/cam/playback?channel=1&starttime=2026_10_07_14_30_00&endtime=2026_10_07_14_30_00"
	if code := describeStatus(t, half); code != base.StatusBadRequest {
		t.Fatalf("an empty range: %d", code)
	}
	for url, want := range map[string]base.StatusCode{
		"/cam/playback?channel=1&starttime=2026_10_07_15_00_00&endtime=2026_10_07_16_00_00": base.StatusNotFound,
		"/cam/playback?channel=1&starttime=yesterday&endtime=2026_10_07_16_00_00":           base.StatusBadRequest,
		"/cam/playback?channel=2&starttime=2026_10_07_14_29_00&endtime=2026_10_07_14_31_00": base.StatusNotFound,
	} {
		if code := describeStatus(t, "rtsp://"+addr+url); code != want {
			t.Fatalf("%s: %d, want %d", url, code, want)
		}
	}
	files.state = engine.StorageError
	if code := describeStatus(t, url); code != base.StatusServiceUnavailable {
		t.Fatalf("a failing card: %d", code)
	}
	// Live goes on.
	play(t, addr, 2)
}

func TestPlaybackIsValidated(t *testing.T) {
	e := New()
	for _, bad := range []string{
		`"playback": {"path": "/pb", "start": "s", "end": "s"}`,
		`"playback": {"path": "/pb", "start": "s", "end": "e", "time_format": "dd/mm"}`,
		`"playback": {"path": "/sub", "start": "s", "end": "e"}`,
	} {
		cfg := `{"engine": "rtsp@^1", "paths": {"sub": "/sub"}, ` + bad + `}`
		if probs := e.Validate([]byte(cfg)); len(probs) == 0 {
			t.Fatalf("%s was accepted", bad)
		}
	}
	if probs := e.Validate([]byte(playbackConfig)); len(probs) != 0 {
		t.Fatalf("%+v", probs)
	}
}
