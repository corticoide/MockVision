package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	for _, tc := range []struct {
		p      Params
		frames int
	}{
		{Params{Codec: CodecH264, Width: 320, Height: 240, FPS: 10, GOP: 10, Bitrate: 256}, 10},
		{Params{Codec: CodecH265, Width: 320, Height: 240, FPS: 10, GOP: 10, Bitrate: 256}, 10},
		{Params{Codec: CodecMJPEG, Width: 320, Height: 240, FPS: 5, GOP: 1, Bitrate: 256}, 1},
	} {
		t.Run(tc.p.Codec, func(t *testing.T) {
			p := tc.p
			key := p.Key("abc")
			if lib.Ready(key, p.Codec) {
				t.Fatal("nothing encoded yet")
			}
			files, err := lib.Encode(ctx, src, p, key)
			if err != nil {
				t.Fatal(err)
			}
			if !lib.Ready(key, p.Codec) || filepath.Base(files.Stream) != "stream."+p.Codec {
				t.Fatalf("rendition should be ready in stream.%s: %+v", p.Codec, files)
			}
			data, err := os.ReadFile(files.Stream)
			if err != nil {
				t.Fatal(err)
			}
			src, err := ParseStream(p.Codec, data)
			if err != nil {
				t.Fatal(err)
			}
			if len(src.AccessUnits) != tc.frames {
				t.Fatalf("expected %d frames, got %d", tc.frames, len(src.AccessUnits))
			}
			switch p.Codec {
			case CodecH265:
				if src.VPS == nil || src.SPS == nil || src.PPS == nil {
					t.Fatal("H.265 parameter sets missing")
				}
			case CodecMJPEG:
				// The quality keeps each frame within the bitrate.
				if budget := p.Bitrate * 1000 / 8 / p.FPS; len(data) > budget {
					t.Fatalf("MJPEG frame of %d bytes exceeds the %d bytes the bitrate allows", len(data), budget)
				}
			}
			sf, err := os.Open(files.Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			defer sf.Close()
			if snap, err := ProbeImage(sf); err != nil || snap.Width != 320 || snap.Height != 240 {
				t.Fatalf("snapshot = %+v, %v", snap, err)
			}
		})
	}
}

func TestRenditionKeys(t *testing.T) {
	// H.264 keys must not change: they name renditions already on disk.
	p := Params{Codec: CodecH264, Width: 320, Height: 240, FPS: 10, GOP: 10, Bitrate: 256}
	sum := sha256.Sum256([]byte("h264-gop-v1|abc|h264|320|240|10|10|256"))
	if p.Key("abc") != hex.EncodeToString(sum[:]) {
		t.Fatal("the H.264 key changed")
	}
	q := p
	q.Codec = CodecH265
	if q.Key("abc") == p.Key("abc") {
		t.Fatal("codecs share a key")
	}
}

func TestParamsValidate(t *testing.T) {
	ok := Params{Codec: "h264", Width: 1280, Height: 720, FPS: 15, GOP: 30, Bitrate: 2048}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := ok
	bad.Codec = "vp9"
	if bad.Validate() == nil {
		t.Fatal("vp9 is not supported")
	}
	if ok.Key("a") == ok.Key("b") {
		t.Fatal("keys must depend on the asset")
	}
	if _, err := ParseStream(CodecH264, []byte{0, 0, 0, 1, 0x41, 0x9a}); err == nil {
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
	if !lib.StreamReady(key, p.Codec) || lib.Ready(key, p.Codec) {
		t.Fatal("after the stream step only the stream is ready")
	}
	if err := lib.EncodeSnapshot(ctx, src, p, key); err != nil {
		t.Fatal(err)
	}
	if !lib.Ready(key, p.Codec) {
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

func TestAccessUnitsFollowPictures(t *testing.T) {
	sc := []byte{0, 0, 0, 1}
	stream := func(nalus ...[]byte) []byte {
		var out []byte
		for _, n := range nalus {
			out = append(append(out, sc...), n...)
		}
		return out
	}
	// H.264: a picture in two slices (first_mb_in_slice 0, then not),
	// then a one-slice picture.
	h264 := stream([]byte{0x67, 1}, []byte{0x68, 2}, []byte{0x09, 0x10}, []byte{0x65, 0x88, 1}, []byte{0x65, 0x40, 2}, []byte{0x06, 5}, []byte{0x41, 0x9a, 3})
	src, err := ParseStream(CodecH264, h264)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.AccessUnits) != 2 || len(src.AccessUnits[0]) != 4 || len(src.AccessUnits[1]) != 2 {
		t.Fatalf("H.264 access units %v", src.AccessUnits)
	}
	// H.265: VPS, SPS, PPS and an IDR, then a trailing picture closed by a
	// suffix SEI.
	h265 := stream([]byte{0x40, 1, 1}, []byte{0x42, 1, 2}, []byte{0x44, 1, 3}, []byte{0x28, 1, 0x80}, []byte{0x02, 1, 0x80}, []byte{0x50, 1, 4})
	src, err = ParseStream(CodecH265, h265)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.AccessUnits) != 2 || len(src.AccessUnits[0]) != 4 || len(src.AccessUnits[1]) != 2 || src.VPS == nil {
		t.Fatalf("H.265 access units %v", src.AccessUnits)
	}
	if _, err := ParseStream(CodecH265, stream([]byte{0x42, 1, 2}, []byte{0x44, 1, 3}, []byte{0x02, 1, 0x80})); err == nil {
		t.Fatal("an H.265 stream without VPS nor keyframe was accepted")
	}
}

func TestMJPEGLimits(t *testing.T) {
	for _, p := range []Params{
		{Codec: CodecMJPEG, Width: 2560, Height: 1440, FPS: 5, GOP: 1, Bitrate: 1024},
		{Codec: CodecMJPEG, Width: 1280, Height: 722, FPS: 5, GOP: 1, Bitrate: 1024},
		{Codec: "h266", Width: 1280, Height: 720, FPS: 5, GOP: 1, Bitrate: 1024},
	} {
		if err := p.Validate(); err == nil {
			t.Fatalf("%+v was accepted", p)
		}
	}
	if err := (Params{Codec: CodecMJPEG, Width: 1920, Height: 1080, FPS: 5, GOP: 1, Bitrate: 4096}).Validate(); err != nil {
		t.Fatal(err)
	}
	// A progressive JPEG cannot travel over RTP.
	progressive := []byte{0xFF, 0xD8, 0xFF, 0xC2, 0, 17, 8, 0, 16, 0, 16, 3, 1, 0x22, 0, 2, 0x11, 1, 3, 0x11, 1, 0xFF, 0xD9}
	if _, err := ParseStream(CodecMJPEG, progressive); err == nil || !strings.Contains(err.Error(), "cannot travel over RTP") {
		t.Fatalf("progressive JPEG: %v", err)
	}
	if _, err := ParseStream(CodecMJPEG, []byte{0xFF, 0xD8, 0xFF, 0xDB, 0, 67}); err == nil {
		t.Fatal("a truncated JPEG was accepted")
	}
}
