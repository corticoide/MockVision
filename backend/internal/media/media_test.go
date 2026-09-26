package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncodeRendition(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	lib, err := NewLibrary(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "pattern.jpg")
	if err := lib.TestPattern(ctx, src); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	info, err := ProbeImage(f)
	f.Close()
	if err != nil || info.Width != 1920 || info.Height != 1080 || info.MIME != "image/jpeg" {
		t.Fatalf("ProbeImage = %+v, %v", info, err)
	}

	p := Params{Codec: "h264", Width: 320, Height: 240, FPS: 10, GOP: 10, Bitrate: 256}
	key := p.Key("abc")
	if lib.Ready(key) {
		t.Fatal("nothing encoded yet")
	}
	files, err := lib.Encode(ctx, src, p, key)
	if err != nil {
		t.Fatal(err)
	}
	if !lib.Ready(key) {
		t.Fatal("rendition should be ready")
	}
	data, err := os.ReadFile(files.GOP)
	if err != nil {
		t.Fatal(err)
	}
	gop, err := ParseGOP(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(gop.AccessUnits) != 10 {
		t.Fatalf("expected 10 frames, got %d", len(gop.AccessUnits))
	}
	sf, err := os.Open(files.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer sf.Close()
	if snap, err := ProbeImage(sf); err != nil || snap.Width != 320 || snap.Height != 240 {
		t.Fatalf("snapshot = %+v, %v", snap, err)
	}
}

func TestParamsValidate(t *testing.T) {
	ok := Params{Codec: "h264", Width: 1280, Height: 720, FPS: 15, GOP: 30, Bitrate: 2048}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := ok
	bad.Codec = "h265"
	if bad.Validate() == nil {
		t.Fatal("h265 is not supported yet")
	}
	if ok.Key("a") == ok.Key("b") {
		t.Fatal("keys must depend on the asset")
	}
	if _, err := ParseGOP([]byte{0, 0, 0, 1, 0x41, 0x9a}); err == nil {
		t.Fatal("a stream without keyframe must fail")
	}
}

func TestEncodeInStepsWithProgress(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	lib, err := NewLibrary(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "pattern.jpg")
	if err := lib.TestPattern(ctx, src); err != nil {
		t.Fatal(err)
	}
	p := Params{Codec: "h264", Width: 640, Height: 360, FPS: 25, GOP: 50, Bitrate: 512}
	key := p.Key("steps")
	last := 0
	if err := lib.EncodeStream(ctx, src, p, key, func(frames int) { last = frames }); err != nil {
		t.Fatal(err)
	}
	if last != p.GOP {
		t.Fatalf("last progress %d frames, want %d", last, p.GOP)
	}
	if !lib.StreamReady(key) || lib.Ready(key) {
		t.Fatal("after the stream step only the stream is ready")
	}
	if err := lib.EncodeSnapshot(ctx, src, p, key); err != nil {
		t.Fatal(err)
	}
	if !lib.Ready(key) {
		t.Fatal("both files are ready")
	}

	// Errors name files relative to the data directory.
	err = lib.EncodeStream(ctx, filepath.Join(lib.AssetsDir, "gone.jpg"), p, p.Key("gone"), nil)
	if err == nil || strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), filepath.Join("assets", "gone.jpg")) {
		t.Fatalf("encode of a missing file: %v", err)
	}

	// A canceled context stops FFmpeg with the caller's reason, not a timeout.
	cctx, cancel := context.WithCancelCause(ctx)
	cancel(errors.New("canceled by the user"))
	err = lib.EncodeStream(cctx, src, p, p.Key("canceled"), nil)
	if err == nil || !strings.Contains(err.Error(), "canceled by the user") {
		t.Fatalf("canceled encode: %v", err)
	}
}

func TestProgressWriter(t *testing.T) {
	var got []int
	w := &progressWriter{fn: func(n int) { got = append(got, n) }}
	for _, chunk := range []string{"frame=1\nfps=0.0\nfra", "me=12\nprogress=continue\n", "frame=50\nprogress=end\n"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(got) != "[1 12 50]" {
		t.Fatalf("frames %v", got)
	}
}

func TestSplitAnnexB(t *testing.T) {
	data := []byte{0, 0, 0, 1, 0x67, 1, 2, 0, 0, 1, 0x68, 3, 0, 0, 0, 1, 0x65, 4, 5, 0}
	nalus, err := splitAnnexB(data)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(nalus) != "[[103 1 2] [104 3] [101 4 5]]" {
		t.Fatalf("nalus %v", nalus)
	}
	if _, err := splitAnnexB([]byte{0x65, 1, 2}); err == nil {
		t.Fatal("a stream without a start code was accepted")
	}
	// More NAL units than one access unit may hold (the library's limit is 50).
	var many []byte
	for i := 0; i < 300; i++ {
		many = append(many, 0, 0, 1, 0x41, byte(i)|1)
	}
	if n, err := splitAnnexB(many); err != nil || len(n) != 300 {
		t.Fatalf("300 units: %d, %v", len(n), err)
	}
}
