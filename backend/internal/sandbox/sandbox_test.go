//go:build linux && (amd64 || arm64)

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// The test binary runs itself as the confined child: sandboxing cannot be
// undone, so it must not happen in the test process.
func TestMain(m *testing.M) {
	if dir := os.Getenv("SANDBOX_CHILD"); dir != "" {
		os.Exit(child(dir))
	}
	os.Exit(m.Run())
}

func child(dir string) int {
	if err := NoNewPrivs(); err != nil {
		fmt.Println("nnp:", err)
		return 1
	}
	if err := Seccomp(); err != nil {
		fmt.Println("seccomp:", err)
		return 1
	}
	landlock, err := Landlock(Paths{Read: []string{filepath.Join(dir, "allowed")}, Write: []string{filepath.Join(dir, "out")}})
	if err != nil {
		fmt.Println("landlock:", err)
		return 1
	}
	var report []string
	check := func(name string, err error) {
		report = append(report, fmt.Sprintf("%s=%v", name, err == nil))
	}
	_, err = os.ReadFile(filepath.Join(dir, "allowed", "f"))
	check("read-allowed", err)
	_, err = os.ReadFile(filepath.Join(dir, "denied", "f"))
	check("read-denied", err)
	check("write-out", os.WriteFile(filepath.Join(dir, "out", "g"), []byte("x"), 0o600))
	check("unshare", unix.Unshare(unix.CLONE_NEWUSER))
	check("mount", unix.Mount("none", dir, "tmpfs", 0, ""))
	_, _, errno := unix.Syscall(unix.SYS_KEYCTL, 0, 0, 0)
	check("keyctl", errnoErr(errno))
	fmt.Printf("landlock=%v %s\n", landlock, strings.Join(report, " "))
	return 0
}

func errnoErr(e unix.Errno) error {
	if e == 0 {
		return nil
	}
	return e
}

func TestConfinedChild(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"allowed", "denied", "out"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, d, "f"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "SANDBOX_CHILD="+dir)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(out))
	t.Log(got)
	for _, want := range []string{"read-allowed=true", "write-out=true", "unshare=false", "mount=false", "keyctl=false"} {
		if !strings.Contains(got, want) {
			t.Fatalf("want %s in %q", want, got)
		}
	}
	if strings.Contains(got, "landlock=true") && !strings.Contains(got, "read-denied=false") {
		t.Fatalf("landlock did not confine the child: %q", got)
	}
}
