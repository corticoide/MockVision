package tmpl

import (
	"context"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/corticoide/mockvision/sdk/engine"
)

func render(t *testing.T, c *Compiler, text string, data engine.TemplateData) string {
	t.Helper()
	tpl, err := c.Compile("t", text, 0)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	out, err := tpl.Render(context.Background(), data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return string(out)
}

func TestFunctions(t *testing.T) {
	c := NewCompiler(Env{
		State: func(k string) (any, bool) {
			if k == "System.DeviceName" {
				return "Gate", true
			}
			return nil, false
		},
		Canon: func(k string) (any, bool) {
			return "1280x720", k == "media.main.resolution"
		},
		Snapshot: func() ([]byte, error) { return []byte{0xff, 0xd8}, nil },
	})
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	data := engine.TemplateData{
		Camera:  engine.CameraData{Serial: "6C0012AB"},
		Request: &engine.RequestData{Query: map[string]string{"q": `a"b<c>`}},
		Now:     now,
	}
	cases := map[string]string{
		`{{ .Camera.Serial }}`:                        "6C0012AB",
		`{{ state "System.DeviceName" }}`:             "Gate",
		`{{ state "Missing" }}`:                       "",
		`{{ canon "media.main.resolution" }}`:         "1280x720",
		`{{ fmtTime .Now "rfc3339" }}`:                "2026-09-25T10:00:00Z",
		`{{ fmtTime .Now "unix" }}`:                   "1790330400",
		`{{ fmtTime .Now "2006/01/02" }}`:             "2026/09/25",
		`{{ json .Request.Query.q }}`:                 `"a\"b<c>"`,
		`{{ xml .Request.Query.q }}`:                  "a&#34;b&lt;c&gt;",
		`{{ b64 "hi" }}`:                              "aGk=",
		`{{ snapshot }}`:                              "/9g=",
		`{{ default "none" .Request.Query.missing }}`: "none",
		`{{ upper "abc" }}{{ lower "DEF" }}`:          "ABCdef",
	}
	for text, want := range cases {
		if got := render(t, c, text, data); got != want {
			t.Errorf("%s = %q, want %q", text, got, want)
		}
	}
	if u := render(t, c, `{{ uuid }}`, data); len(u) != 36 || u[14] != '4' {
		t.Errorf("uuid = %q", u)
	}
	for i := 0; i < 50; i++ {
		n, err := strconv.Atoi(render(t, c, `{{ rand 5 10 }}`, data))
		if err != nil || n < 5 || n > 10 {
			t.Fatalf("rand 5 10 = %d, %v", n, err)
		}
	}
}

func TestRequestDataIsNotEvaluated(t *testing.T) {
	c := NewCompiler(Env{})
	data := engine.TemplateData{Request: &engine.RequestData{Query: map[string]string{"x": "{{ uuid }}"}}}
	if got := render(t, c, `{{ .Request.Query.x }}`, data); got != "{{ uuid }}" {
		t.Fatalf("request data must be inserted as a value, got %q", got)
	}
}

func TestLimits(t *testing.T) {
	c := NewCompiler(Env{})
	tpl, err := c.Compile("big", `{{ range 50000 }}xxxxxxxxxx{{ end }}`, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tpl.Render(context.Background(), engine.TemplateData{}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size limit error, got %v", err)
	}
	if err := Check("bad", `{{ .Camera.Serial `); err == nil {
		t.Fatal("syntax errors must be reported at compile time")
	}
	if err := Check("unknown", `{{ exec "rm -rf /" }}`); err == nil {
		t.Fatal("unknown functions must be rejected")
	}
}

// Renders that loop or recurse without writing stop at the step budget
// instead of running on after their timeout (audit M2).
func TestRendersAreBoundedInSteps(t *testing.T) {
	c := NewCompiler(Env{State: func(string) (any, bool) { return int64(1 << 40), true }})
	for name, text := range map[string]string{
		"range":     `{{ range 2000000000 }}{{ end }}`,
		"nested":    `{{ range 1000 }}{{ range 1000 }}{{ end }}{{ end }}`,
		"state":     `{{ range state "n" }}{{ end }}`,
		"recursion": `{{ define "a" }}{{ template "a" . }}{{ template "a" . }}{{ end }}{{ template "a" . }}`,
		"block":     `{{ define "b" }}{{ block "c" . }}{{ template "b" }}{{ end }}{{ end }}{{ template "b" }}`,
	} {
		tpl, err := c.Compile(name, text, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		start := time.Now()
		_, err = tpl.Render(context.Background(), engine.TemplateData{})
		if err == nil {
			t.Fatalf("%s: rendered without limit", name)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("%s: took %s", name, d)
		}
	}
	// Ordinary loops still work, and each render has its own budget.
	tpl, err := c.Compile("ok", `{{ range $i := 3 }}{{ $i }}{{ end }};{{ range $k, $v := .Request.Query }}{{ $k }}={{ $v }}{{ end }}`, 0)
	if err != nil {
		t.Fatal(err)
	}
	data := engine.TemplateData{Request: &engine.RequestData{Query: map[string]string{"a": "1"}}}
	for range 3 {
		if out, err := tpl.Render(context.Background(), data); err != nil || string(out) != "012;a=1" {
			t.Fatalf("got %q, %v", out, err)
		}
	}
}

// A render past its timeout does not keep a goroutine busy.
func TestTimedOutRenderStops(t *testing.T) {
	c := NewCompiler(Env{})
	// Just under the budget, but slow: every step formats a large number.
	tpl, err := c.Compile("slow", `{{ range 99999 }}{{ $x := printf "%0999999d" 1 }}{{ end }}`, 0)
	if err != nil {
		t.Fatal(err)
	}
	before := runtime.NumGoroutine()
	if _, err := tpl.Render(context.Background(), engine.TemplateData{}); err == nil {
		t.Fatal("the render should time out")
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("the render goroutine is still running: %d > %d", runtime.NumGoroutine(), before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
