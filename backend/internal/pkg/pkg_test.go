package pkg

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/corticoide/mockvision/backend/internal/engines"
	"github.com/corticoide/mockvision/backend/internal/profile"
)

func demo(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "milesight-demo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLooseProfileIsADraft(t *testing.T) {
	res := Inspect(demo(t), "milesight-demo.yaml", engines.Builtin())
	if !res.Report.OK() {
		t.Fatalf("problems: %+v", res.Report.Problems)
	}
	r := res.Report
	if r.Level != "draft" || r.Signature != SignatureUnsigned || r.ID != "milesight/demo" || r.Version != "0.1.0" || len(res.Resolved) == 0 {
		t.Fatalf("report = %+v", r)
	}
}

func TestPackageRoundTrip(t *testing.T) {
	dir := t.TempDir()
	prof := strings.Replace(string(demo(t)), `          type: application/json
          body: |
            {
              "deviceName": {{ json (state "System.DeviceName") }},`, `          type: application/json
          template: templates/info.json.tmpl
          x-removed: |
            {
              "deviceName": {{ json (state "System.DeviceName") }},`, 1)
	// Keep the test honest: the replaced block must exist.
	if prof == string(demo(t)) {
		t.Fatal("fixture changed")
	}
	// Remove the leftover key so the schema stays valid.
	prof = removeBlock(prof, "          x-removed: |")
	os.MkdirAll(filepath.Join(dir, "templates"), 0o755)
	os.WriteFile(filepath.Join(dir, "profile.yaml"), []byte(prof), 0o644)
	os.WriteFile(filepath.Join(dir, "templates", "info.json.tmpl"), []byte(`{"serialNumber": {{ json .Camera.Serial }}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("format: 1\nkind: profile\nid: milesight/demo\nversion: 0.1.0\nprovenance: { source: documented }\n"), 0o644)

	data, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	res := Inspect(data, "demo.mvpkg", engines.Builtin())
	if !res.Report.OK() {
		t.Fatalf("problems: %+v", res.Report.Problems)
	}
	if res.Report.Level != "documented" {
		t.Fatalf("documented provenance must give level documented, got %s", res.Report.Level)
	}
	if !bytes.Contains(res.Resolved, []byte(`serialNumber`)) {
		t.Fatal("file templates must be inlined in the resolved profile")
	}

	// Tampering with a file breaks its hash.
	tampered := rezip(t, data, "templates/info.json.tmpl", []byte(`{"evil": true}`))
	res = Inspect(tampered, "demo.mvpkg", engines.Builtin())
	if res.Report.OK() || !hasProblem(res.Report, profile.StepIntegrity, "sha256") {
		t.Fatalf("tampering not detected: %+v", res.Report.Problems)
	}
}

func TestUnsafePaths(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("../evil.yaml")
	w.Write([]byte("x: 1"))
	zw.Close()
	res := Inspect(buf.Bytes(), "evil.mvpkg", engines.Builtin())
	if res.Report.OK() || !hasProblem(res.Report, profile.StepIntegrity, "unsafe path") {
		t.Fatalf("unsafe path accepted: %+v", res.Report.Problems)
	}
}

func hasProblem(r Report, step, contains string) bool {
	for _, p := range r.Problems {
		if p.Step == step && strings.Contains(p.Message, contains) {
			return true
		}
	}
	return false
}

func removeBlock(s, header string) string {
	lines := strings.Split(s, "\n")
	var out []string
	skipping := false
	indent := 0
	for _, l := range lines {
		if strings.HasPrefix(l, header) {
			skipping = true
			indent = len(l) - len(strings.TrimLeft(l, " "))
			continue
		}
		if skipping {
			cur := len(l) - len(strings.TrimLeft(l, " "))
			if strings.TrimSpace(l) == "" || cur > indent {
				continue
			}
			skipping = false
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func rezip(t *testing.T, data []byte, name string, content []byte) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		w, _ := zw.Create(f.Name)
		if f.Name == name {
			w.Write(content)
			continue
		}
		rc, _ := f.Open()
		b := new(bytes.Buffer)
		b.ReadFrom(rc)
		rc.Close()
		w.Write(b.Bytes())
	}
	zw.Close()
	return buf.Bytes()
}
