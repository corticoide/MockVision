package pkg

import (
	"archive/zip"
	"bufio"
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
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/corticoide/mockvision/backend/internal/minisign"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/backend/internal/sandbox"
)

// KeyPasswordEnv holds the password of an encrypted secret key, for
// scripts and CI; keygen -W writes a key without one.
const KeyPasswordEnv = "MOCKVISION_KEY_PASSWORD"

const usage = `usage: mockvision pkg <command>
  verify <file> [--key <file.pub>]...   run the import pipeline; trust these keys too
  build <dir> -o <file.mvpkg> [-k <key>] zip a package directory, signed with -k
  sign <file.mvpkg> -k <key>            sign a package's manifest
  keygen -o <name> [-W]                 write <name>.pub and <name>.key (-W: no password)
  catalog [--dir profiles] [-k <key>] [-o <dir>]
                                        sign the official catalog, write its packages
  inspect [--name file] < package       the service's validation subprocess

An encrypted key's password comes from ` + KeyPasswordEnv + `.`

// Main is the entry point of "mockvision pkg".
func Main(args []string, engines profile.EngineCatalog) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "verify":
		return verifyCmd(args[1:], engines)
	case "inspect":
		return inspectCmd(args[1:], engines)
	case "build":
		return buildCmd(args[1:])
	case "sign":
		return signCmd(args[1:])
	case "keygen":
		return keygenCmd(args[1:])
	case "catalog":
		return catalogCmd(args[1:])
	}
	fmt.Fprintf(os.Stderr, "unknown pkg command %q\n%s\n", args[0], usage)
	return 2
}

// keyFiles collects repeated --key flags.
type keyFiles []string

func (k *keyFiles) String() string     { return strings.Join(*k, ",") }
func (k *keyFiles) Set(v string) error { *k = append(*k, v); return nil }

func verifyCmd(args []string, engines profile.EngineCatalog) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	var keys keyFiles
	fs.Var(&keys, "key", "a public key (.pub) to trust, besides the official ones")
	file, ok := parseWithFile(fs, args)
	if !ok {
		fmt.Fprintln(os.Stderr, "usage: mockvision pkg verify <file> [--key <file.pub>]...")
		return 2
	}
	opts := Options{Keys: OfficialKeys()}
	for _, k := range keys {
		raw, err := os.ReadFile(k)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		pub, err := minisign.ParsePublicKey(string(raw))
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", k, err)
			return 1
		}
		opts.Keys = append(opts.Keys, TrustedKey{Name: filepath.Base(k), Key: pub.String()})
	}
	data, err := readLimited(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	res := Inspect(data, filepath.Base(file), engines, opts)
	r := res.Report
	signer := ""
	if r.Signer != "" {
		signer = " by " + r.Signer
	}
	fmt.Printf("%s %s@%s  sha256:%s  signature:%s%s\n", r.Kind, r.ID, r.Version, r.SHA256[:12], r.Signature, signer)
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

// parseWithFile parses a command's flags and its one file argument, which
// may come before or after them.
func parseWithFile(fs *flag.FlagSet, args []string) (string, bool) {
	file := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		file, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", false
	}
	switch {
	case file == "" && fs.NArg() == 1:
		file = fs.Arg(0)
	case fs.NArg() != 0:
		return "", false
	}
	return file, file != ""
}

// MaxOptionsBytes bounds the options line of the validation subprocess.
const MaxOptionsBytes = 16 << 20

// inspectCmd is what the service runs in a subprocess: a line of JSON
// options, then the package, on stdin; the Result as JSON on stdout.
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
	in := bufio.NewReaderSize(os.Stdin, 64<<10)
	line, err := readLine(in, MaxOptionsBytes)
	if err != nil {
		fmt.Fprintln(os.Stderr, "options:", err)
		return 1
	}
	var opts Options
	if err := json.Unmarshal(line, &opts); err != nil {
		fmt.Fprintln(os.Stderr, "options:", err)
		return 1
	}
	data, err := io.ReadAll(io.LimitReader(in, MaxPackageBytes+1))
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
		res = Inspect(data, *name, engines, opts)
	}
	if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// readLine reads up to a newline, at most limit bytes.
func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > limit {
			return nil, fmt.Errorf("longer than %d bytes", limit)
		}
		if err == nil {
			return line, nil
		}
		if err != bufio.ErrBufferFull {
			return nil, err
		}
	}
}

// WriteInspectInput writes what inspect reads: the options line, then the
// package.
func WriteInspectInput(opts Options, data []byte) ([]byte, error) {
	line, err := json.Marshal(opts)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(line)+1+len(data))
	out = append(append(append(out, line...), '\n'), data...)
	return out, nil
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
// its manifest, and signs it with -k.
func buildCmd(args []string) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	out := fs.String("o", "", "output .mvpkg file")
	keyFile := fs.String("k", "", "secret key to sign the package with")
	dir, ok := parseWithFile(fs, args)
	if !ok || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: mockvision pkg build <dir> -o <file.mvpkg> [-k <key>]")
		return 2
	}
	data, err := Build(os.DirFS(dir))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *keyFile != "" {
		key, err := loadSecretKey(*keyFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if data, err = Sign(data, key); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("wrote %s (%d bytes)\n", *out, len(data))
	return 0
}

// Build creates a package from a directory that holds manifest.yaml and the
// package files. The files section of the manifest is regenerated. The
// same directory always gives the same bytes, so a signature made at
// release time still matches a package built again from its sources.
func Build(dir fs.FS) ([]byte, error) {
	manRaw, err := fs.ReadFile(dir, "manifest.yaml")
	if err != nil {
		return nil, fmt.Errorf("package directory needs manifest.yaml: %w", err)
	}
	var man map[string]any
	if err := yaml.Unmarshal(manRaw, &man); err != nil {
		return nil, fmt.Errorf("manifest.yaml: %w", err)
	}
	files := map[string][]byte{}
	err = fs.WalkDir(dir, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link", p)
		}
		if p == "manifest.yaml" || p == "manifest.sig" || strings.HasPrefix(path.Base(p), ".") {
			return nil
		}
		b, err := fs.ReadFile(dir, p)
		if err != nil {
			return err
		}
		files[p] = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pack(man, files)
}

// pack zips a manifest, with the sha256 of every file written into it, and
// the files, in name order and without timestamps.
func pack(man map[string]any, files map[string][]byte) ([]byte, error) {
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

func signCmd(args []string) int {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	keyFile := fs.String("k", "", "secret key")
	file, ok := parseWithFile(fs, args)
	if !ok || *keyFile == "" {
		fmt.Fprintln(os.Stderr, "usage: mockvision pkg sign <file.mvpkg> -k <key>")
		return 2
	}
	key, err := loadSecretKey(*keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	data, err := readLimited(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	signed, err := Sign(data, key)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.WriteFile(file, signed, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("signed %s with key %s\n", file, minisign.KeyID(key.ID))
	return 0
}

func keygenCmd(args []string) int {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := fs.String("o", "", "name of the key files: <name>.pub and <name>.key")
	plain := fs.Bool("W", false, "write the secret key without a password")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: mockvision pkg keygen -o <name> [-W]")
		return 2
	}
	password := []byte(os.Getenv(KeyPasswordEnv))
	if !*plain && len(password) == 0 {
		fmt.Fprintf(os.Stderr, "set %s to encrypt the secret key, or pass -W to write it without a password\n", KeyPasswordEnv)
		return 2
	}
	if *plain {
		password = nil
	}
	for _, f := range []string{*out + ".pub", *out + ".key"} {
		if _, err := os.Stat(f); err == nil {
			fmt.Fprintf(os.Stderr, "%s exists; not overwriting it\n", f)
			return 1
		}
	}
	key, err := minisign.GenerateKey(nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	sec, err := minisign.MarshalPrivateKey(key, password, minisign.DefaultOpsLimit, minisign.DefaultMemLimit)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.WriteFile(*out+".key", sec, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.WriteFile(*out+".pub", key.Public().File(), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("key %s\npublic key: %s\n", minisign.KeyID(key.ID), key.Public().String())
	return 0
}

// catalogCmd signs the official catalog for a release, writing each
// package's signature next to catalog/catalog.yaml, and writes the
// packages themselves for publishing.
func catalogCmd(args []string) int {
	fs := flag.NewFlagSet("catalog", flag.ContinueOnError)
	dir := fs.String("dir", "profiles", "the profiles directory, with "+CatalogIndex)
	keyFile := fs.String("k", "", "catalog key: write the packages' signatures into the directory")
	out := fs.String("o", "", "write the packages into this directory")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || (*keyFile == "" && *out == "") {
		fmt.Fprintln(os.Stderr, "usage: mockvision pkg catalog [--dir profiles] [-k <key>] [-o <dir>]")
		return 2
	}
	if *keyFile != "" {
		key, err := loadSecretKey(*keyFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		sigs, err := signCatalog(os.DirFS(*dir), key)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		for _, name := range sortedNames(sigs) {
			if err := os.WriteFile(filepath.Join(*dir, filepath.FromSlash(name)), sigs[name], 0o644); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			fmt.Printf("signed %s\n", name)
		}
	}
	if *out != "" {
		pkgs, err := BuildCatalog(os.DirFS(*dir))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := os.MkdirAll(*out, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		for _, p := range pkgs {
			if err := os.WriteFile(filepath.Join(*out, p.Name), p.Data, 0o644); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			fmt.Printf("wrote %s (signed: %v)\n", p.Name, p.Signed)
		}
	}
	return 0
}

func sortedNames(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// loadSecretKey reads a minisign secret key, with the password from the
// environment when it is encrypted.
func loadSecretKey(file string) (minisign.PrivateKey, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return minisign.PrivateKey{}, err
	}
	key, err := minisign.ParsePrivateKey(raw, []byte(os.Getenv(KeyPasswordEnv)))
	if err != nil {
		return minisign.PrivateKey{}, fmt.Errorf("%s: %w", file, err)
	}
	return key, nil
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
