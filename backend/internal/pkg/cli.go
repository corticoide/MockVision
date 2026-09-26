package pkg

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/backend/internal/sandbox"
)

// Main is the entry point of "mockvision pkg".
func Main(args []string, engines profile.EngineCatalog) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: mockvision pkg verify <file> | build <dir> -o <file.mvpkg> | inspect [--name file] < package")
		return 2
	}
	switch args[0] {
	case "verify":
		return verifyCmd(args[1:], engines)
	case "inspect":
		return inspectCmd(args[1:], engines)
	case "build":
		return buildCmd(args[1:])
	}
	fmt.Fprintf(os.Stderr, "unknown pkg command %q\n", args[0])
	return 2
}

func verifyCmd(args []string, engines profile.EngineCatalog) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mockvision pkg verify <file>")
		return 2
	}
	data, err := readLimited(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	res := Inspect(data, filepath.Base(args[0]), engines)
	r := res.Report
	fmt.Printf("%s %s@%s  sha256:%s  signature:%s\n", r.Kind, r.ID, r.Version, r.SHA256[:12], r.Signature)
	for _, s := range r.Steps {
		note := ""
		if s.Note != "" {
			note = " (" + s.Note + ")"
		}
		fmt.Printf("  %-13s %s%s\n", s.Name, s.Status, note)
	}
	for _, p := range r.Problems {
		loc := p.File
		if p.Line > 0 {
			loc = fmt.Sprintf("%s:%d", p.File, p.Line)
		}
		fmt.Printf("%s: %s [%s] %s\n", p.Severity, loc, p.Step, p.Message)
	}
	if !r.OK() {
		fmt.Println("result: rejected")
		return 1
	}
	fmt.Printf("result: accepted, level %s\n", r.Level)
	return 0
}

// inspectCmd is what the service runs in a subprocess: package on stdin,
// Result as JSON on stdout.
func inspectCmd(args []string, engines profile.EngineCatalog) int {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	name := fs.String("name", "profile.yaml", "original file name")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// The package is hostile until proven otherwise: before reading it,
	// the validator gives up the file system and the system calls it does
	// not need, so taking it over reaches nothing (audit B9).
	if err := confine(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, MaxPackageBytes+1))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var res *Result
	if len(data) > MaxPackageBytes {
		res = &Result{Report: Report{Signature: SignatureUnsigned, Problems: []profile.Problem{{
			Step: profile.StepIntegrity, Severity: profile.SeverityError, Message: fmt.Sprintf("package exceeds %d bytes", MaxPackageBytes),
		}}}}
	} else {
		res = Inspect(data, *name, engines)
	}
	if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxPackageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxPackageBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, MaxPackageBytes)
	}
	return data, nil
}

// buildCmd zips a package directory, writing the sha256 of every file into
// its manifest.
func buildCmd(args []string) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	out := fs.String("o", "", "output .mvpkg file")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: mockvision pkg build <dir> -o <file.mvpkg>")
		return 2
	}
	data, err := Build(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("wrote %s (%d bytes)\n", *out, len(data))
	return 0
}

// Build creates a package from a directory that holds manifest.yaml and the
// package files. The files section of the manifest is regenerated.
func Build(dir string) ([]byte, error) {
	manRaw, err := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	if err != nil {
		return nil, fmt.Errorf("package directory needs manifest.yaml: %w", err)
	}
	var man map[string]any
	if err := yaml.Unmarshal(manRaw, &man); err != nil {
		return nil, fmt.Errorf("manifest.yaml: %w", err)
	}
	files := map[string][]byte{}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link", p)
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == "manifest.yaml" || rel == "manifest.sig" || strings.HasPrefix(filepath.Base(rel), ".") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[rel] = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, ok := files["profile.yaml"]; !ok && man["kind"] == "profile" {
		return nil, errors.New("a profile package needs profile.yaml")
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	sums := map[string]string{}
	for _, n := range names {
		sum := sha256.Sum256(files[n])
		sums[n] = "sha256:" + hex.EncodeToString(sum[:])
	}
	man["files"] = sums
	manOut, err := yaml.Marshal(man)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name string, data []byte) error {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	if err := write("manifest.yaml", manOut); err != nil {
		return nil, err
	}
	for _, n := range names {
		if err := write(n, files[n]); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// confine applies no_new_privs, the seccomp filter and a Landlock ruleset
// that allows no file at all; stdin and stdout are open already.
func confine() error {
	if err := sandbox.NoNewPrivs(); err != nil {
		return err
	}
	if err := sandbox.Seccomp(); err != nil {
		return err
	}
	_, err := sandbox.Landlock(sandbox.Paths{NoBind: true})
	return err
}
