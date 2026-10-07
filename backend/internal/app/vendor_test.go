package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A Dahua camera speaks Dahua's values: named after its serial, its
// panel edits written as Dahua's, and a resolution the stream lacks
// refused whole.
func TestVendorValues(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "dahua-ipc-hdbw1230e-s4.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ImportPackage(ctx, testActor, "dahua.yaml", data); err != nil {
		t.Fatal(err)
	}
	cam, err := svc.CreateCamera(ctx, testActor, CreateCameraInput{Name: "Hall", ProfileID: "dahua/ipc-hdbw1230e-s4", ProfileVersion: "0.2.0",
		Stream: StreamInput{Codec: "h265", Resolution: "1280x720"}})
	if err != nil {
		t.Fatal(err)
	}
	param := func(key string) any {
		t.Helper()
		params, err := svc.CameraConfig(ctx, cam.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range params {
			if p.Key == key {
				return p.Value
			}
		}
		t.Fatalf("no %s", key)
		return nil
	}
	if param("General.MachineName") != cam.Serial || param("Encode[0].MainFormat[0].Video.Compression") != "H.265" ||
		param("Encode[0].MainFormat[0].Video.Width") != int64(1280) || param("Encode[0].MainFormat[0].Video.Height") != int64(720) {
		t.Fatalf("created with %v %v %vx%v", param("General.MachineName"), param("Encode[0].MainFormat[0].Video.Compression"),
			param("Encode[0].MainFormat[0].Video.Width"), param("Encode[0].MainFormat[0].Video.Height"))
	}
	v, _ := svc.GetCamera(ctx, cam.ID)
	if v.Streams[0].Codec != "h265" || v.Streams[0].Resolution != "1280x720" {
		t.Fatalf("stream %+v", v.Streams[0])
	}
	// 1920x720 is no resolution of the main stream.
	_, err = svc.UpdateCameraConfig(ctx, testActor, cam.ID, map[string]any{"Encode[0].MainFormat[0].Video.Width": 1920})
	wantInvalid(t, err, "Encode[0].MainFormat[0].Video.Width")
	if _, err := svc.UpdateCameraStream(ctx, testActor, cam.ID, "main", StreamUpdate{Resolution: ptr("1280x960")}); err != nil {
		t.Fatal(err)
	}
	if param("Encode[0].MainFormat[0].Video.Height") != int64(960) {
		t.Fatalf("height %v", param("Encode[0].MainFormat[0].Video.Height"))
	}
	// A copy is named after its own serial.
	cp, err := svc.CloneCamera(ctx, testActor, cam.ID, CloneCameraInput{Name: "Hall 2", Network: NetworkInput{IP: "10.250.0.99"}})
	if err != nil {
		t.Fatal(err)
	}
	params, _ := svc.CameraConfig(ctx, cp.ID)
	for _, p := range params {
		if p.Key == "General.MachineName" && (p.Value != cp.Serial || p.Value == cam.Serial) {
			t.Fatalf("the copy is named %v", p.Value)
		}
	}
}
