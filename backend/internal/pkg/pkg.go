// Package pkg imports .mvpkg packages: a zip with a manifest, the sha256 of
// every file and a profile, plugin or program. A loose profile.yaml is also
// accepted as a local draft; the importer wraps it in an unsigned package.
//
// Every package goes through the same pipeline and the verification level of
// a profile comes out of it; it is never declared.
package pkg

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/corticoide/mockvision/backend/internal/buildinfo"
	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/profile"
)

// Limits of a package (integrity step).
const (
	MaxPackageBytes  = 50 << 20
	MaxFiles         = 2000
	MaxUnpackedBytes = 200 << 20
	MaxRatio         = 100
)

// FormatVersion is the package format this MockVision reads.
const FormatVersion = 1

// Signature statuses.
const (
	SignatureOfficial = "official"
	SignatureTrusted  = "trusted"
	SignatureUnsigned = "unsigned"
	SignatureInvalid  = "invalid"
)

// Manifest is manifest.yaml.
type Manifest struct {
	Format     int               `json:"format"`
	Kind       string            `json:"kind"`
	ID         string            `json:"id"`
	Version    string            `json:"version"`
	Requires   Requires          `json:"requires"`
	License    string            `json:"license,omitempty"`
	Provenance Provenance        `json:"provenance"`
	Files      map[string]string `json:"files"`
}

// Requires lists what a package needs.
type Requires struct {
	MockVision    string            `json:"mockvision,omitempty"`
	ProfileSchema int               `json:"profile_schema,omitempty"`
	Engines       map[string]string `json:"engines,omitempty"`
}

// Provenance says where the content comes from.
type Provenance struct {
	Source     string   `json:"source,omitempty"` // captured | documented | draft
	Firmware   []string `json:"firmware,omitempty"`
	CapturedAt string   `json:"captured_at,omitempty"`
}

// Step is the outcome of one pipeline step.
type Step struct {
	Name   string `json:"name"`
	Status string `json:"status"` // passed | failed | skipped
	Note   string `json:"note,omitempty"`
}

// Report is what the panel shows after an import.
type Report struct {
	Kind      string            `json:"kind"`
	ID        string            `json:"id"`
	Version   string            `json:"version"`
	SHA256    string            `json:"sha256"`
	Signature string            `json:"signature"`
	Level     string            `json:"level,omitempty"`
	Coverage  map[string]string `json:"coverage,omitempty"`
	Steps     []Step            `json:"steps"`
	Problems  []profile.Problem `json:"problems"`
}

// OK reports whether the package can be installed.
func (r *Report) OK() bool {
	for _, p := range r.Problems {
		if p.Severity == profile.SeverityError {
			return false
		}
	}
	return true
}

// Result is an inspected package.
type Result struct {
	Report   Report          `json:"report"`
	Manifest Manifest        `json:"manifest"`
	Profile  *profile.Meta   `json:"profile,omitempty"`
	Resolved json.RawMessage `json:"resolved,omitempty"`
}

type inspector struct {
	res     *Result
	engines profile.EngineCatalog
}

func (in *inspector) problem(step, file string, line int, format string, args ...any) {
	in.res.Report.Problems = append(in.res.Report.Problems, profile.Problem{
		Step: step, Severity: profile.SeverityError, File: file, Line: line, Message: fmt.Sprintf(format, args...),
	})
}

func (in *inspector) warn(step, file, format string, args ...any) {
	in.res.Report.Problems = append(in.res.Report.Problems, profile.Problem{
		Step: step, Severity: profile.SeverityWarning, File: file, Message: fmt.Sprintf(format, args...),
	})
}

func (in *inspector) step(name, status, note string) {
	in.res.Report.Steps = append(in.res.Report.Steps, Step{Name: name, Status: status, Note: note})
}

// Inspect runs the import pipeline on a package or a loose profile.yaml.
func Inspect(data []byte, filename string, engines profile.EngineCatalog) *Result {
	sum := sha256.Sum256(data)
	in := &inspector{res: &Result{Report: Report{SHA256: hex.EncodeToString(sum[:]), Signature: SignatureUnsigned, Problems: []profile.Problem{}}}, engines: engines}
	if isZip(data) {
		in.inspectPackage(data)
	} else {
		in.inspectLoose(data, filename)
	}
	return in.res
}

func isZip(data []byte) bool {
	return len(data) >= 4 && bytes.Equal(data[:4], []byte("PK\x03\x04"))
}

// inspectLoose wraps a loose profile.yaml in an unsigned draft package.
func (in *inspector) inspectLoose(data []byte, filename string) {
	if filename == "" {
		filename = "profile.yaml"
	}
	if len(data) > profile.DefaultLimits.MaxBytes {
		in.problem(profile.StepIntegrity, filename, 0, "profile is %d bytes, the limit is %d", len(data), profile.DefaultLimits.MaxBytes)
		in.step(profile.StepIntegrity, "failed", "")
		return
	}
	in.step(profile.StepIntegrity, "passed", "loose profile.yaml, wrapped as a local draft")
	in.step(profile.StepSignature, "skipped", "loose profiles are never signed")
	in.res.Manifest = Manifest{Format: FormatVersion, Kind: "profile", Provenance: Provenance{Source: "draft"}}
	in.res.Report.Kind = "profile"
	in.validateProfile(profile.Input{File: path.Base(filename), Data: data}, true)
}

func (in *inspector) inspectPackage(data []byte) {
	files, ok := in.integrity(data)
	if !ok {
		return
	}
	man, ok := in.manifest(files)
	if !ok {
		return
	}
	in.res.Manifest = *man
	in.res.Report.Kind = man.Kind
	in.res.Report.ID = man.ID
	in.res.Report.Version = man.Version
	if _, signed := files["manifest.sig"]; signed {
		in.step(profile.StepSignature, "skipped", "signature verification is not available yet; treated as unsigned")
		in.warn(profile.StepSignature, "manifest.sig", "signatures are not verified in this version")
	} else {
		in.step(profile.StepSignature, "passed", "unsigned")
	}
	if !in.compatibility(man) {
		return
	}
	if man.Kind != "profile" {
		in.problem(profile.StepCompatibility, "manifest.yaml", 0, "%s packages are not supported yet", man.Kind)
		return
	}
	prof, ok := files["profile.yaml"]
	if !ok {
		in.problem(profile.StepIntegrity, "manifest.yaml", 0, "a profile package needs profile.yaml")
		return
	}
	others := map[string][]byte{}
	for name, b := range files {
		if name != "profile.yaml" && name != "manifest.yaml" && name != "manifest.sig" {
			others[name] = b
		}
	}
	in.validateProfile(profile.Input{File: "profile.yaml", Data: prof, Files: others}, false)
	if in.res.Profile != nil && man.ID != "" && in.res.Profile.ID != "" && in.res.Profile.ID != man.ID {
		in.problem(profile.StepCompatibility, "profile.yaml", 0, "profile.id %s does not match the manifest id %s", in.res.Profile.ID, man.ID)
	}
}

// integrity checks zip limits, safe paths and the sha256 of every file.
func (in *inspector) integrity(data []byte) (map[string][]byte, bool) {
	fail := func(format string, args ...any) (map[string][]byte, bool) {
		in.problem(profile.StepIntegrity, "", 0, format, args...)
		in.step(profile.StepIntegrity, "failed", "")
		return nil, false
	}
	if len(data) > MaxPackageBytes {
		return fail("package is %d bytes, the limit is %d", len(data), MaxPackageBytes)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fail("not a valid zip: %v", err)
	}
	if len(zr.File) > MaxFiles {
		return fail("package has %d files, the limit is %d", len(zr.File), MaxFiles)
	}
	files := map[string][]byte{}
	var total uint64
	for _, f := range zr.File {
		name := f.Name
		if f.FileInfo().IsDir() {
			continue
		}
		clean := path.Clean(name)
		if strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || clean != name || strings.HasPrefix(clean, "../") || clean == ".." {
			return fail("unsafe path %q", name)
		}
		if f.Mode()&fs.ModeSymlink != 0 {
			return fail("%s is a symbolic link", name)
		}
		if !f.Mode().IsRegular() {
			return fail("%s is not a regular file", name)
		}
		if _, dup := files[name]; dup {
			return fail("%s appears twice", name)
		}
		total += f.UncompressedSize64
		if total > MaxUnpackedBytes {
			return fail("unpacked size exceeds %d bytes", MaxUnpackedBytes)
		}
		if f.CompressedSize64 > 0 && f.UncompressedSize64/f.CompressedSize64 > MaxRatio {
			return fail("%s has a compression ratio above %d:1", name, MaxRatio)
		}
		rc, err := f.Open()
		if err != nil {
			return fail("%s: %v", name, err)
		}
		b, err := io.ReadAll(io.LimitReader(rc, int64(f.UncompressedSize64)+1))
		rc.Close()
		if err != nil {
			return fail("%s: %v", name, err)
		}
		if uint64(len(b)) != f.UncompressedSize64 {
			return fail("%s: size does not match the zip header", name)
		}
		files[name] = b
	}
	in.step(profile.StepIntegrity, "passed", fmt.Sprintf("%d files", len(files)))
	return files, true
}

func (in *inspector) manifest(files map[string][]byte) (*Manifest, bool) {
	raw, ok := files["manifest.yaml"]
	if !ok {
		in.problem(profile.StepIntegrity, "manifest.yaml", 0, "package has no manifest.yaml")
		return nil, false
	}
	v, pos, err := profile.ParseYAML(raw, profile.DefaultLimits)
	if err != nil {
		var ye *profile.YAMLError
		line := 0
		if errors.As(err, &ye) {
			line = ye.Line
		}
		in.problem(profile.StepYAML, "manifest.yaml", line, "%v", err)
		return nil, false
	}
	b, _ := json.Marshal(v)
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		in.problem(profile.StepSchema, "manifest.yaml", 0, "invalid manifest: %v", err)
		return nil, false
	}
	bad := false
	if m.Format != FormatVersion {
		in.problem(profile.StepCompatibility, "manifest.yaml", pos.Line("/format"), "package format %d is not supported; this MockVision reads format %d", m.Format, FormatVersion)
		bad = true
	}
	if !domain.ValidProfileID(m.ID) {
		in.problem(profile.StepSchema, "manifest.yaml", pos.Line("/id"), "id must look like vendor/model")
		bad = true
	}
	if _, err := semver.StrictNewVersion(m.Version); err != nil {
		in.problem(profile.StepSchema, "manifest.yaml", pos.Line("/version"), "version must be semver")
		bad = true
	}
	names := make([]string, 0, len(files))
	for name := range files {
		if name != "manifest.yaml" && name != "manifest.sig" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		want, listed := m.Files[name]
		if !listed {
			in.problem(profile.StepIntegrity, name, 0, "%s is not listed in the manifest", name)
			bad = true
			continue
		}
		sum := sha256.Sum256(files[name])
		if strings.TrimPrefix(want, "sha256:") != hex.EncodeToString(sum[:]) {
			in.problem(profile.StepIntegrity, name, 0, "sha256 of %s does not match the manifest", name)
			bad = true
		}
	}
	for name := range m.Files {
		if _, ok := files[name]; !ok {
			in.problem(profile.StepIntegrity, "manifest.yaml", 0, "%s is listed in the manifest but missing", name)
			bad = true
		}
	}
	return &m, !bad
}

func (in *inspector) compatibility(m *Manifest) bool {
	ok := true
	if m.Requires.MockVision != "" {
		c, err := semver.NewConstraint(m.Requires.MockVision)
		v, verr := semver.NewVersion(buildinfo.Version)
		switch {
		case err != nil:
			in.problem(profile.StepCompatibility, "manifest.yaml", 0, "invalid mockvision requirement %q", m.Requires.MockVision)
			ok = false
		case verr == nil && !c.Check(v):
			in.problem(profile.StepCompatibility, "manifest.yaml", 0, "the package needs MockVision %s; this is %s", m.Requires.MockVision, buildinfo.Version)
			ok = false
		}
	}
	if s := m.Requires.ProfileSchema; s != 0 && s != profile.SchemaVersion {
		in.problem(profile.StepCompatibility, "manifest.yaml", 0, "the package needs profile schema %d; this MockVision reads %d", s, profile.SchemaVersion)
		ok = false
	}
	for name, rng := range m.Requires.Engines {
		if in.engines == nil {
			break
		}
		if _, err := in.engines.Resolve(name, rng); err != nil {
			in.problem(profile.StepCompatibility, "manifest.yaml", 0, "%v", err)
			ok = false
		}
	}
	status := "passed"
	if !ok {
		status = "failed"
	}
	in.step(profile.StepCompatibility, status, "")
	return ok
}

func (in *inspector) validateProfile(input profile.Input, loose bool) {
	res := profile.Validate(input, in.engines)
	in.res.Report.Problems = append(in.res.Report.Problems, res.Problems...)
	if res.Doc != nil {
		meta := res.Doc.Profile
		in.res.Profile = &meta
		if loose {
			if meta.ID == "" {
				in.problem(profile.StepSchema, input.File, res.Positions.Line("/profile"), "profile.id is required in a loose profile.yaml")
			}
			if meta.Version == "" {
				in.problem(profile.StepSchema, input.File, res.Positions.Line("/profile"), "profile.version is required in a loose profile.yaml")
			}
			in.res.Manifest.ID = meta.ID
			in.res.Manifest.Version = meta.Version
			in.res.Report.ID = meta.ID
			in.res.Report.Version = meta.Version
		} else {
			if meta.Version != "" && meta.Version != in.res.Manifest.Version {
				in.problem(profile.StepCompatibility, input.File, res.Positions.Line("/profile/version"), "profile.version %s does not match the manifest version %s", meta.Version, in.res.Manifest.Version)
			}
			in.res.Profile.ID = in.res.Manifest.ID
			in.res.Profile.Version = in.res.Manifest.Version
		}
		if loose && meta.ID != "" && !domain.ValidProfileID(meta.ID) {
			in.problem(profile.StepSchema, input.File, res.Positions.Line("/profile/id"), "profile.id must look like vendor/model")
		}
		in.res.Report.Coverage = res.Doc.Coverage
	}
	for _, s := range []string{profile.StepYAML, profile.StepSchema, profile.StepLint, profile.StepInheritance, profile.StepTemplates} {
		status := "passed"
		for _, p := range res.Problems {
			if p.Step == s && p.Severity == profile.SeverityError {
				status = "failed"
			}
		}
		in.step(s, status, "")
	}
	in.step(profile.StepSelfTest, "skipped", "the package has no fixtures of a real device")
	if in.res.Report.OK() {
		in.res.Resolved = res.Resolved
		in.res.Report.Level = string(domain.LevelDraft)
		if in.res.Manifest.Provenance.Source == "documented" {
			in.res.Report.Level = string(domain.LevelDocumented)
		}
		// Adjust the resolved document so it carries the final identity.
		if in.res.Profile != nil {
			in.res.Resolved = withIdentity(res.Resolved, in.res.Profile.ID, in.res.Profile.Version)
		}
	}
}

func withIdentity(resolved []byte, id, version string) json.RawMessage {
	var doc map[string]any
	if err := json.Unmarshal(resolved, &doc); err != nil {
		return resolved
	}
	if p, ok := doc["profile"].(map[string]any); ok {
		p["id"] = id
		p["version"] = version
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return resolved
	}
	return out
}
