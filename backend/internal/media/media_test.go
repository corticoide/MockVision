package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
