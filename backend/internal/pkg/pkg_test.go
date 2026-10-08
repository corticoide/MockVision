package pkg

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/corticoide/mockvision/backend/internal/engines"
	"github.com/corticoide/mockvision/backend/internal/minisign"
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
	res := Inspect(demo(t), "milesight-demo.yaml", engines.Builtin(), Options{})
	if !res.Report.OK() {
		t.Fatalf("problems: %+v", res.Report.Problems)
	}
	r := res.Report
	if r.Level != "draft" || r.Signature != SignatureUnsigned || r.ID != "milesight/demo" || r.Version != "0.7.0" || len(res.Resolved) == 0 {
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
	for _, err := range []error{
		os.MkdirAll(filepath.Join(dir, "templates"), 0o755),
		os.WriteFile(filepath.Join(dir, "profile.yaml"), []byte(prof), 0o644),
		os.WriteFile(filepath.Join(dir, "templates", "info.json.tmpl"), []byte(`{"serialNumber": {{ json .Camera.Serial }}}`), 0o644),
		os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("format: 1\nkind: profile\nid: milesight/demo\nversion: 0.7.0\nprovenance: { source: documented }\n"), 0o644),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}

	data, err := Build(os.DirFS(dir))
	if err != nil {
		t.Fatal(err)
	}
	res := Inspect(data, "demo.mvpkg", engines.Builtin(), Options{})
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
	res = Inspect(tampered, "demo.mvpkg", engines.Builtin(), Options{})
	if res.Report.OK() || !hasProblem(res.Report, profile.StepIntegrity, "sha256") {
		t.Fatalf("tampering not detected: %+v", res.Report.Problems)
	}
}

func TestUnsafePaths(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("../evil.yaml")
	_, _ = w.Write([]byte("x: 1"))
	zw.Close()
	res := Inspect(buf.Bytes(), "evil.mvpkg", engines.Builtin(), Options{})
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
			_, _ = w.Write(content)
			continue
		}
		rc, _ := f.Open()
		_, _ = io.Copy(w, rc)
		rc.Close()
	}
	zw.Close()
	return buf.Bytes()
}

// demoPackage builds the demo profile as a package.
func demoPackage(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "profile.yaml"), demo(t), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("format: 1\nkind: profile\nid: milesight/demo\nversion: 0.7.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := Build(os.DirFS(dir))
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Build(os.DirFS(dir))
	if !bytes.Equal(data, again) {
		t.Fatal("building the same directory twice must give the same bytes")
	}
	return data
}

// A signature by a trusted key makes the package trusted, by an official
// key official; by a key the node does not know it proves nothing; a
// package changed after signing is rejected (D83, D84).
func TestSignatures(t *testing.T) {
	data := demoPackage(t)
	key, err := minisign.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := Sign(data, key)
	if err != nil {
		t.Fatal(err)
	}
	trusted := Options{Keys: []TrustedKey{{Name: "Acme", Key: key.Public().String()}}}

	res := Inspect(signed, "demo.mvpkg", engines.Builtin(), trusted)
	if !res.Report.OK() || res.Report.Signature != SignatureTrusted || res.Report.Signer != "Acme" || res.Report.KeyID != minisign.KeyID(key.ID) {
		t.Fatalf("trusted: %+v", res.Report)
	}
	official := Options{Keys: []TrustedKey{{Name: "MockVision", Key: key.Public().String(), Official: true}}}
	if res := Inspect(signed, "demo.mvpkg", engines.Builtin(), official); res.Report.Signature != SignatureOfficial {
		t.Fatalf("official: %+v", res.Report)
	}
	res = Inspect(signed, "demo.mvpkg", engines.Builtin(), Options{})
	if !res.Report.OK() || res.Report.Signature != SignatureUnsigned || !hasProblem(res.Report, profile.StepSignature, "which this node does not trust") {
		t.Fatalf("unknown key: %+v", res.Report)
	}
	if res := Inspect(data, "demo.mvpkg", engines.Builtin(), trusted); res.Report.Signature != SignatureUnsigned ||
		!hasProblem(res.Report, profile.StepSignature, "not signed") {
		t.Fatalf("unsigned: %+v", res.Report)
	}

	// Signing again replaces the signature.
	other, _ := minisign.GenerateKey(nil)
	resigned, err := Sign(signed, other)
	if err != nil {
		t.Fatal(err)
	}
	if res := Inspect(resigned, "demo.mvpkg", engines.Builtin(), trusted); res.Report.KeyID != minisign.KeyID(other.ID) {
		t.Fatalf("re-signed: %+v", res.Report)
	}

	// A manifest changed after signing does not match its signature.
	zr, _ := zip.NewReader(bytes.NewReader(signed), int64(len(signed)))
	var manifest []byte
	for _, f := range zr.File {
		if f.Name == "manifest.yaml" {
			rc, _ := f.Open()
			manifest, _ = io.ReadAll(rc)
			rc.Close()
		}
	}
	changed := rezip(t, signed, "manifest.yaml", append(manifest, []byte("license: MIT\n")...))
	res = Inspect(changed, "demo.mvpkg", engines.Builtin(), trusted)
	if res.Report.OK() || res.Report.Signature != SignatureInvalid || !hasProblem(res.Report, profile.StepSignature, "changed after it was signed") {
		t.Fatalf("changed: %+v", res.Report)
	}
	broken := rezip(t, signed, "manifest.sig", []byte("not a signature"))
	if res := Inspect(broken, "demo.mvpkg", engines.Builtin(), trusted); res.Report.OK() || res.Report.Signature != SignatureInvalid {
		t.Fatalf("broken: %+v", res.Report)
	}
}

// A profile that extends another asks for its parent, then validates
// merged with it.
func TestInheritance(t *testing.T) {
	base, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "milesight-base.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	parent := Inspect(base, "milesight-base.yaml", engines.Builtin(), Options{})
	if !parent.Report.OK() {
		t.Fatalf("base: %+v", parent.Report.Problems)
	}
	child := []byte(`schema: 1
profile:
  id: milesight/c2965
  version: 1.0.0
  name: Milesight C2965
  vendor: Milesight
  model: MS-C2965-PB
  extends: milesight/base@^0
`)
	res := Inspect(child, "c2965.yaml", engines.Builtin(), Options{})
	if res.Needs == nil || res.Needs.ID != "milesight/base" || res.Needs.Range != "^0" || res.Report.Level != "" {
		t.Fatalf("needs: %+v %+v", res.Needs, res.Report)
	}
	ref := "milesight/base@" + parent.Report.Version
	opts := Options{Parents: map[string]*profile.Parent{"milesight/base": {Ref: ref, Resolved: parent.Resolved}}}
	res = Inspect(child, "c2965.yaml", engines.Builtin(), opts)
	if !res.Report.OK() || res.Needs != nil || res.Profile.Extends != ref || len(res.Profile.Lineage) != 1 {
		t.Fatalf("child: %+v %+v", res.Report.Problems, res.Profile)
	}
	if !bytes.Contains(res.Resolved, []byte(`"engines"`)) || !bytes.Contains(res.Resolved, []byte(`MS-C2965-PB`)) {
		t.Fatal("the child must carry its parent's engines and its own model")
	}
}
