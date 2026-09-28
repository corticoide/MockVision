package profile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/corticoide/mockvision/backend/internal/engines"
	"github.com/corticoide/mockvision/backend/internal/profile"
)

// The Dahua draft validates; its only warnings are the events that have no
// transport until MockVision serves eventManager.cgi attach.
func TestDahuaDraftProfile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "dahua-ipc-hdbw1230e-s4.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	res := profile.Validate(profile.Input{Data: data}, engines.Builtin())
	if !res.OK() {
		t.Fatalf("the Dahua draft must validate: %+v", res.Problems)
	}
	for _, p := range res.Problems {
		if !strings.HasSuffix(p.Message, "has no transport and cannot be enabled on a camera") {
			t.Errorf("unexpected problem %s: %s", p.Pointer, p.Message)
		}
	}
	if len(res.Problems) != len(res.Doc.Events) {
		t.Errorf("%d problems for %d events", len(res.Problems), len(res.Doc.Events))
	}
	m := profile.NewModel(res.Doc)
	main, err := m.StreamFor("main", m.Defaults())
	if err != nil || main.Codec != "h264" || main.Width != 1920 || main.Height != 1080 || main.FPS != 30 || main.GOP != 60 || main.Bitrate != 4096 {
		t.Fatalf("main stream %+v, %v", main, err)
	}
	values := m.Defaults()
	values["Encode[0].ExtraFormat[0].Video.FPS"] = int64(15)
	if sub, err := m.StreamFor("sub", values); err != nil || sub.Width != 704 || sub.Height != 480 || sub.FPS != 15 {
		t.Fatalf("the sub stream follows Dahua's frame rate: %+v, %v", sub, err)
	}
}
