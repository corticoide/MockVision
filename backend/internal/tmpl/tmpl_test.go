package tmpl

import (
	"context"
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
	if r := render(t, c, `{{ rand 5 10 }}`, data); r < "5" || len(r) > 2 {
		t.Errorf("rand = %q", r)
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
	tpl, err := c.Compile("big", `{{ range 2000000 }}xxxxxxxxxx{{ end }}`, 1024)
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
