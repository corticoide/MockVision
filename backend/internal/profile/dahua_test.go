package profile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/corticoide/mockvision/backend/internal/engines"
	"github.com/corticoide/mockvision/backend/internal/profile"
)

// The Dahua draft validates without a problem, and Dahua's values drive
// its streams: codecs by name, the resolution from a width and a height.
func TestDahuaDraftProfile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "dahua-ipc-hdbw1230e-s4.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	res := profile.Validate(profile.Input{Data: data}, engines.Builtin())
	if !res.OK() || len(res.Problems) != 0 {
		t.Fatalf("the Dahua draft must validate cleanly: %+v", res.Problems)
	}
	m := profile.NewModel(res.Doc)
	main, err := m.StreamFor("main", m.Defaults())
	if err != nil || main.Codec != "h264" || main.Width != 1920 || main.Height != 1080 || main.FPS != 30 || main.GOP != 60 || main.Bitrate != 4096 {
		t.Fatalf("main stream %+v, %v", main, err)
	}
	values := m.Defaults()
	values["Encode[0].MainFormat[0].Video.Compression"] = "H.265"
	values["Encode[0].MainFormat[0].Video.Width"] = int64(1280)
	values["Encode[0].MainFormat[0].Video.Height"] = int64(720)
	values["Encode[0].ExtraFormat[0].Video.Compression"] = "MJPG"
	values["Encode[0].ExtraFormat[0].Video.FPS"] = int64(15)
	if main, err := m.StreamFor("main", values); err != nil || main.Codec != "h265" || main.Width != 1280 || main.Height != 720 {
		t.Fatalf("Dahua's H.265 at 1280x720: %+v, %v", main, err)
	}
	if sub, err := m.StreamFor("sub", values); err != nil || sub.Codec != "mjpeg" || sub.Width != 704 || sub.Height != 480 || sub.FPS != 15 {
		t.Fatalf("the sub stream follows Dahua's values: %+v, %v", sub, err)
	}
	values["Encode[0].ExtraFormat[0].Video.Height"] = int64(240)
	if probs := m.CheckStreams(values, []string{"Encode[0].ExtraFormat[0].Video.Height"}); probs["Encode[0].ExtraFormat[0].Video.Height"] == "" {
		t.Fatal("704x240, which the sub stream lacks, was accepted")
	}

	// The panel's canonical values are written in Dahua's.
	set, err := m.Assign("media.main.codec", "h265")
	if err != nil || set["Encode[0].MainFormat[0].Video.Compression"] != "H.265" {
		t.Fatalf("codec %v %v", set, err)
	}
	set, err = m.Assign("media.main.resolution", "1280x960")
	if err != nil || set["Encode[0].MainFormat[0].Video.Width"] != int64(1280) || set["Encode[0].MainFormat[0].Video.Height"] != int64(960) {
		t.Fatalf("resolution %v %v", set, err)
	}
	if _, err := m.Assign("media.main.resolution", "800x600"); err == nil {
		t.Fatal("a width the unit lacks was accepted")
	}
	if got, _ := m.Canon("media.main.resolution", m.Defaults()); got != "1920x1080" {
		t.Fatalf("resolution %v", got)
	}

	// The device name is the serial until renamed.
	if d := m.DefaultsFor(profile.CameraIdentity{Serial: "4E0AB2EPAG00B3B"}); d["General.MachineName"] != "4E0AB2EPAG00B3B" {
		t.Fatalf("name %v", d["General.MachineName"])
	}
}

// Vendor values that cannot work are refused where they are written.
func TestVendorValueProblems(t *testing.T) {
	base, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "dahua-ipc-hdbw1230e-s4.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cat := engines.Builtin()
	cases := []struct {
		name, old, new, at, step, want string
	}{
		{"map without bind", "    bind: media.main.codec\n    map: { H.264: h264, H.265: h265 }", "    map: { H.264: h264, H.265: h265 }",
			"", profile.StepLint, "map translates the values of a bound parameter"},
		{"map of a value the enum lacks", "map: { H.264: h264, H.265: h265 }", "map: { H.264: h264, H.265: h265, H.266: h265 }",
			"", profile.StepLint, "H.266 is not one of the values"},
		{"unknown identity field", "default_from: serial", "default_from: hostname", "default_from: hostname", profile.StepSchema, "value must be one of 'serial'"},
		{"identity default of an int", "    type: string\n    default_from: serial", "    type: int\n    default_from: serial", "", profile.StepLint, "must be a string"},
		{"width without its height", "    default: 1080\n    bind: media.main.height", "    default: 1080",
			"", profile.StepLint, "splits the resolution of main: bind another parameter to media.main.height"},
		{"defaults the stream lacks", "    values: [1920, 1280]\n    default: 1920", "    values: [1920, 1280]\n    default: 1280",
			"", profile.StepLint, "the defaults make 1280x1080, which stream main does not support"},
		{"attach without its route", "      - id: event-attach\n        match: { method: GET, path: /cgi-bin/eventManager.cgi, query: { action: attach } }\n" +
			"        action: { handler: events.attach, params: { codes: codes, heartbeat: heartbeat }, boundary: myboundary }\n", "",
			"", profile.StepLint, "no route of the HTTP API streams events: add one with handler: events.attach"},
		{"failure event the profile lacks", `failure_event: "custom:login_failure"`, `failure_event: "custom:nope"`, "", profile.StepLint, "the profile does not define custom:nope events"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := replace(t, base, c.old, c.new)
			res := profile.Validate(profile.Input{Data: data}, cat)
			line := 0
			if c.at != "" {
				line = lineOf(data, c.at)
			}
			expectProblem(t, res, c.step, line, c.want)
			if res.OK() {
				t.Fatal("the profile must fail")
			}
		})
	}
}
