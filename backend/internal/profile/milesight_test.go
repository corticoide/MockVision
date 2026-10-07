package profile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/corticoide/mockvision/backend/internal/engines"
	"github.com/corticoide/mockvision/backend/internal/profile"
)

// The Milesight base validates without a problem and answers Milesight's
// values: H.264 and 1920*1080 make the stream's.
func TestMilesightBaseProfile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "milesight-base.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	res := profile.Validate(profile.Input{Data: data}, engines.Builtin())
	if !res.OK() || len(res.Problems) != 0 {
		t.Fatalf("the Milesight base must validate cleanly: %+v", res.Problems)
	}
	m := profile.NewModel(res.Doc)
	values := m.DefaultsFor(profile.CameraIdentity{Model: "MS-C2964"})
	main, err := m.StreamFor("main", values)
	if err != nil || main.Codec != "h264" || main.Width != 1920 || main.Height != 1080 || main.FPS != 25 {
		t.Fatalf("main %+v %v", main, err)
	}
	if values["System.DeviceName"] != "MS-C2964" {
		t.Fatalf("name %v", values["System.DeviceName"])
	}
	set, err := m.Assign("media.sub.resolution", "704x576")
	if err != nil || set["Encode.Sub.Resolution"] != "704*576" {
		t.Fatalf("sub %v %v", set, err)
	}
	values["Encode.Main.Codec"] = "H.265"
	if main, _ := m.StreamFor("main", values); main.Codec != "h265" {
		t.Fatalf("H.265 drives %s", main.Codec)
	}
}
