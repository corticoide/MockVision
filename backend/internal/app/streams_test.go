package app

import (
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
)

// A camera gets every stream of its profile; codec, bitrate and GOP change
// while it runs, and each stream has its snapshot.
func TestCameraStreamsAndCodecs(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cam := createCamera(t, svc, "Streams", true)

	want := map[string]string{"main": "h264 1280x720 30", "sub": "h264 640x360 20", "third": "mjpeg 640x360 1"}
	if len(cam.Streams) != 3 {
		t.Fatalf("streams %+v", cam.Streams)
	}
	for _, s := range cam.Streams {
		got := fmt.Sprintf("%s %s %d", s.Codec, s.Resolution, s.GOP)
		if got != want[s.Name] || !strings.HasSuffix(s.URL, "/"+s.Name) || s.RenditionStatus != "ready" {
			t.Fatalf("stream %s: %s, url %s, %s", s.Name, got, s.URL, s.RenditionStatus)
		}
	}

	for _, name := range []string{"main", "sub", "third"} {
		path, err := svc.SnapshotFile(ctx, cam.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := jpeg.DecodeConfig(f)
		f.Close()
		if err != nil || (name == "main" && cfg.Width != 1280) || (name != "main" && cfg.Width != 640) {
			t.Fatalf("snapshot of %s: %+v, %v", name, cfg, err)
		}
	}
	if _, err := svc.SnapshotFile(ctx, cam.ID, "fourth"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("snapshot of a missing stream: %v", err)
	}

	// Only what the profile binds can change, within its values.
	_, err := svc.UpdateCameraStream(ctx, testActor, cam.ID, "third", StreamUpdate{Codec: ptr("h264")})
	wantInvalid(t, err, "codec")
	_, err = svc.UpdateCameraStream(ctx, testActor, cam.ID, "main", StreamUpdate{Codec: ptr("mjpeg")})
	wantInvalid(t, err, "codec")

	v, err := svc.UpdateCameraStream(ctx, testActor, cam.ID, "main", StreamUpdate{Codec: ptr("h265"), Bitrate: ptr(1024), GOP: ptr(15)})
	if err != nil {
		t.Fatal(err)
	}
	if s := v.Streams[0]; s.Codec != "h265" || s.Bitrate != 1024 || s.GOP != 15 || len(v.Status.PendingRestart) != 0 {
		t.Fatalf("main stream %+v, pending %v", s, v.Status.PendingRestart)
	}

	// The running camera switches to H.265 without restarting.
	if _, err := exec.LookPath("ffprobe"); err == nil {
		deadline := time.Now().Add(60 * time.Second)
		for {
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			out, _ := exec.CommandContext(pctx, "ffprobe", "-v", "error", "-rtsp_transport", "tcp", "-select_streams", "v:0",
				"-show_entries", "stream=codec_name", "-of", "csv=p=0", withCredentials(v.Streams[0].URL)).Output()
			cancel()
			if strings.TrimSpace(string(out)) == "hevc" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the main stream is still %q", out)
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	if v, _ := svc.GetCamera(ctx, cam.ID); v.Status.PID != cam.Status.PID {
		t.Fatalf("the camera restarted: pid %d, was %d", v.Status.PID, cam.Status.PID)
	}
}

// Cameras created when only the main stream was served get the others.
func TestCompleteStreams(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cam := createCamera(t, svc, "Old", false)
	b, err := svc.loadBundle(ctx, cam.ID)
	if err != nil {
		t.Fatal(err)
	}
	b.streams = b.streams[:1]
	if err := svc.completeStreams(ctx, b); err != nil {
		t.Fatal(err)
	}
	if len(b.streams) != 3 || b.streams[1].Stream != "sub" || b.streams[2].AssetID != b.streams[0].AssetID {
		t.Fatalf("streams %+v", b.streams)
	}
}

func withCredentials(url string) string {
	return strings.Replace(url, "rtsp://", "rtsp://admin:ms1234@", 1)
}

// Asking at creation for what a profile fixes is not an error; asking for
// something else is.
func TestCreateWithFixedSettings(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	data := strings.Replace(string(demoProfile(t, "0.9.0")), "    bind: media.main.resolution\n", "", 1)
	if _, err := svc.ImportPackage(ctx, testActor, "fixed.yaml", []byte(data)); err != nil {
		t.Fatal(err)
	}
	in := CreateCameraInput{Name: "Fixed", ProfileID: "milesight/demo", ProfileVersion: "0.9.0", Stream: StreamInput{Resolution: "1280x720", Codec: "h264"}}
	if _, err := svc.CreateCamera(ctx, testActor, in); err != nil {
		t.Fatalf("the profile's own resolution: %v", err)
	}
	in.Name, in.Stream.Resolution = "Fixed 2", "640x360"
	_, err := svc.CreateCamera(ctx, testActor, in)
	wantInvalid(t, err, "stream.resolution")
}
