package profile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/corticoide/mockvision/backend/internal/engines"
	"github.com/corticoide/mockvision/backend/internal/profile"
)

func demoProfile(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "milesight-demo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDemoProfileIsValid(t *testing.T) {
	res := profile.Validate(profile.Input{Data: demoProfile(t)}, engines.Builtin())
	for _, p := range res.Problems {
		t.Logf("%s:%d [%s/%s] %s (%s)", p.File, p.Line, p.Step, p.Severity, p.Message, p.Pointer)
	}
	if !res.OK() {
		t.Fatal("the demo profile must validate")
	}
	if res.Doc.Profile.ID != "milesight/demo" || len(res.Resolved) == 0 {
		t.Fatalf("unexpected result: %+v", res.Doc.Profile)
	}
	m := profile.NewModel(res.Doc)
	settings, err := m.StreamFor("main", m.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if settings.Width != 1280 || settings.Height != 720 || settings.FPS != 15 || settings.GOP != 30 || settings.Codec != "h264" {
		t.Fatalf("unexpected stream settings %+v", settings)
	}
	values := m.Defaults()
	values["Encode.Main.Resolution"] = "640x360"
	values["Encode.Main.Codec"] = "h265"
	if s, _ := m.StreamFor("main", values); s.Width != 640 || s.Codec != "h265" {
		t.Fatalf("bound parameters must drive the stream, got %+v", s)
	}
	// Sub and third streams; every MJPEG frame is a keyframe.
	if s, err := m.StreamFor("sub", m.Defaults()); err != nil || s.Codec != "h264" || s.Width != 640 || s.FPS != 10 || s.GOP != 20 {
		t.Fatalf("sub stream %+v, %v", s, err)
	}
	if s, err := m.StreamFor("third", m.Defaults()); err != nil || s.Codec != "mjpeg" || s.GOP != 1 || s.Bitrate != 1024 {
		t.Fatalf("third stream %+v, %v", s, err)
	}
	values["Encode.Main.Codec"] = "mjpeg"
	if _, err := m.StreamFor("main", values); err == nil {
		t.Fatal("a codec the stream does not support was accepted")
	}
}

func replace(t *testing.T, data []byte, old, new string) []byte {
	t.Helper()
	s := string(data)
	if !strings.Contains(s, old) {
		t.Fatalf("fixture does not contain %q", old)
	}
	return []byte(strings.Replace(s, old, new, 1))
}

func lineOf(data []byte, needle string) int {
	idx := strings.Index(string(data), needle)
	return strings.Count(string(data[:idx]), "\n") + 1
}

func expectProblem(t *testing.T, res *profile.Result, step string, line int, contains string) {
	t.Helper()
	for _, p := range res.Problems {
		if p.Step == step && (line == 0 || p.Line == line) && strings.Contains(p.Message, contains) {
			return
		}
	}
	for _, p := range res.Problems {
		t.Logf("got %s:%d [%s] %s", p.File, p.Line, p.Step, p.Message)
	}
	t.Fatalf("expected a %s problem at line %d containing %q", step, line, contains)
}

func TestProblemsCarryLines(t *testing.T) {
	base := demoProfile(t)
	cat := engines.Builtin()

	t.Run("yaml syntax", func(t *testing.T) {
		data := replace(t, base, "  model: MS-DEMO", "  model: [MS-DEMO")
		res := profile.Validate(profile.Input{Data: data}, cat)
		if res.OK() || res.Problems[0].Step != profile.StepYAML || res.Problems[0].Line == 0 {
			t.Fatalf("expected a YAML error with a line, got %+v", res.Problems)
		}
	})

	t.Run("schema", func(t *testing.T) {
		data := replace(t, base, "    max_length: 32", "    max_length: -1")
		res := profile.Validate(profile.Input{Data: data}, cat)
		expectProblem(t, res, profile.StepSchema, lineOf(data, "max_length: -1"), "")
	})

	t.Run("unknown engine", func(t *testing.T) {
		data := replace(t, base, "engine: rtsp@^1", "engine: rtsp@^2")
		res := profile.Validate(profile.Input{Data: data}, cat)
		expectProblem(t, res, profile.StepCompatibility, lineOf(data, "engine: rtsp@^2"), "does not satisfy")
	})

	t.Run("route template", func(t *testing.T) {
		data := replace(t, base, `"model": {{ json .Camera.Model }},`, `"model": {{ json .Camera.Model },`)
		res := profile.Validate(profile.Input{Data: data}, cat)
		expectProblem(t, res, profile.StepTemplates, lineOf(data, `"model": {{ json .Camera.Model },`), "")
	})

	t.Run("event template", func(t *testing.T) {
		data := replace(t, base, `"eventId": {{ json .Event.ID }},`, `"eventId": {{ nope .Event.ID }},`)
		res := profile.Validate(profile.Input{Data: data}, cat)
		expectProblem(t, res, profile.StepTemplates, lineOf(data, `{{ nope .Event.ID }}`), "not defined")
	})

	t.Run("bind", func(t *testing.T) {
		data := replace(t, base, "bind: media.main.fps", "bind: media.main.framerate")
		res := profile.Validate(profile.Input{Data: data}, cat)
		expectProblem(t, res, profile.StepLint, lineOf(data, "bind: media.main.framerate"), "unknown canonical key")
	})

	t.Run("transport without engine", func(t *testing.T) {
		data := replace(t, base, "  push:\n    engine: http-push@^1\n", "")
		res := profile.Validate(profile.Input{Data: data}, cat)
		expectProblem(t, res, profile.StepLint, 0, "no engine of this profile delivers http_push")
	})

	t.Run("duplicated route", func(t *testing.T) {
		data := replace(t, base, "- id: param-set", "- id: param-get")
		res := profile.Validate(profile.Input{Data: data}, cat)
		expectProblem(t, res, profile.StepLint, 0, "duplicated")
	})

	t.Run("default outside range", func(t *testing.T) {
		data := replace(t, base, "    default: 50\n", "    default: 500\n")
		res := profile.Validate(profile.Input{Data: data}, cat)
		expectProblem(t, res, profile.StepLint, lineOf(data, "default: 500"), "at most 100")
	})

	t.Run("stream served but not defined", func(t *testing.T) {
		data := replace(t, base, "    third:\n      codecs: [mjpeg]", "    fourth_unused:\n      codecs: [mjpeg]")
		res := profile.Validate(profile.Input{Data: data}, cat)
		if res.OK() {
			t.Fatal("a stream name outside main, sub and third was accepted")
		}
		data = replace(t, base, "    third:\n      codecs: [mjpeg]\n      resolutions: [\"640x360\", \"352x288\"]\n      fps: { min: 1, max: 15 }\n      bitrate: { min: 64, max: 4096 }\n      default: { codec: mjpeg, resolution: 640x360, fps: 5, bitrate: 1024 }\n", "")
		data = replace(t, data, "  Encode.Third.Resolution:\n    type: enum\n    values: [\"640x360\", \"352x288\"]\n    default: \"640x360\"\n    bind: media.third.resolution\n", "")
		data = replace(t, data, "  Encode.Third.FrameRate:\n    type: int\n    default: 5\n    min: 1\n    max: 15\n    bind: media.third.fps\n", "")
		res = profile.Validate(profile.Input{Data: data}, cat)
		expectProblem(t, res, profile.StepLint, lineOf(data, "third: /third"), "media.streams does not define it")
	})

	t.Run("mjpeg too large", func(t *testing.T) {
		data := replace(t, base, `resolutions: ["640x360", "352x288"]`, `resolutions: ["2560x1440", "352x288"]`)
		data = replace(t, data, "default: { codec: mjpeg, resolution: 640x360,", "default: { codec: mjpeg, resolution: 2560x1440,")
		data = replace(t, data, `values: ["640x360", "352x288"]`, `values: ["2560x1440", "352x288"]`)
		data = replace(t, data, `default: "640x360"
    bind: media.third.resolution`, `default: "2560x1440"
    bind: media.third.resolution`)
		res := profile.Validate(profile.Input{Data: data}, cat)
		expectProblem(t, res, profile.StepLint, 0, "MJPEG over RTSP carries at most 2040x2040")
		// Allowed alongside another codec, it only warns.
		data = replace(t, base, "codecs: [mjpeg]", "codecs: [mjpeg, h264]")
		data = replace(t, data, `resolutions: ["640x360", "352x288"]`, `resolutions: ["640x360", "352x288", "2560x1440"]`)
		res = profile.Validate(profile.Input{Data: data}, cat)
		if !res.OK() {
			t.Fatalf("a warning must not fail: %+v", res.Problems)
		}
		warned := false
		for _, p := range res.Problems {
			warned = warned || (p.Severity == profile.SeverityWarning && strings.Contains(p.Message, "cannot be streamed as MJPEG"))
		}
		if !warned {
			t.Fatalf("expected a warning: %+v", res.Problems)
		}
	})

	t.Run("codec bound to a string", func(t *testing.T) {
		data := replace(t, base, "  Encode.Main.Codec:\n    type: enum\n    values: [\"h264\", \"h265\"]", "  Encode.Main.Codec:\n    type: string")
		res := profile.Validate(profile.Input{Data: data}, cat)
		expectProblem(t, res, profile.StepLint, 0, "must be an enum of the stream's codecs")
	})

	t.Run("template file in loose yaml", func(t *testing.T) {
		data := replace(t, base, "          body: |\n            {\n              \"deviceName\"", "          template: templates/info.json\n          x: |\n            {\n              \"deviceName\"")
		res := profile.Validate(profile.Input{Data: data}, cat)
		if res.OK() {
			t.Fatal("expected problems")
		}
	})
}

func TestAliasesAreBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("a: &a [x, x, x, x, x, x, x, x, x, x]\n")
	prev := "a"
	for i := 0; i < 9; i++ {
		name := string(rune('b' + i))
		b.WriteString(name + ": &" + name + " [*" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + "]\n")
		prev = name
	}
	if _, _, err := profile.ParseYAML([]byte(b.String()), profile.DefaultLimits); err == nil {
		t.Fatal("a billion laughs document must be rejected")
	}
}

func TestCoerce(t *testing.T) {
	min, max := 0.0, 100.0
	p := profile.Param{Type: profile.TypeInt, Min: &min, Max: &max}
	if v, err := profile.Coerce(p, "70"); err != nil || v != int64(70) {
		t.Fatalf("Coerce = %v, %v", v, err)
	}
	if _, err := profile.Coerce(p, "170"); err == nil {
		t.Fatal("out of range must fail")
	}
	e := profile.Param{Type: profile.TypeEnum, Values: []any{"1920x1080", "1280x720"}}
	if v, err := profile.Coerce(e, "1280x720"); err != nil || v != "1280x720" {
		t.Fatalf("enum Coerce = %v, %v", v, err)
	}
	b := profile.Param{Type: profile.TypeBool}
	if v, _ := profile.Coerce(b, "on"); v != true {
		t.Fatal("on must be true")
	}
}
