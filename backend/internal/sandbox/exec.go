package sandbox

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// stringList is a repeatable flag.
type stringList []string

func (s *stringList) String() string     { return fmt.Sprint(*s) }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// ExecMain is "mockvision sandbox-exec": it confines itself and then
// becomes the given program, which keeps the confinement. The service runs
// FFmpeg through it, so a hostile image that took FFmpeg over could still
// not read the database or the node key (audit B9):
//
//	mockvision sandbox-exec --read <in> --write <dir> -- ffmpeg <args>
func ExecMain(args []string) int {
	fs := flag.NewFlagSet("sandbox-exec", flag.ContinueOnError)
	var read, write stringList
	fs.Var(&read, "read", "path the program may read (repeatable)")
	fs.Var(&write, "write", "path the program may write (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "usage: mockvision sandbox-exec [--read path] [--write path] -- program [args]")
		return 2
	}
	path, err := exec.LookPath(rest[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandbox-exec:", err)
		return 127
	}
	if path, err = filepath.Abs(path); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox-exec:", err)
		return 1
	}
	if err := Confine(path, read, write); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox-exec:", err)
		return 1
	}
	err = syscall.Exec(path, rest, os.Environ())
	fmt.Fprintln(os.Stderr, "sandbox-exec:", err)
	return 126
}

// Confine applies no_new_privs, the seccomp filter and Landlock for a
// program at path that reads and writes the given paths.
func Confine(path string, read, write []string) error {
	if err := NoNewPrivs(); err != nil {
		return err
	}
	if err := Seccomp(); err != nil {
		return err
	}
	rx := append([]string{filepath.Dir(path)}, SystemReadExec...)
	// What media libraries probe: CPU features, and the null device.
	read = append(read, "/proc", "/sys/devices/system/cpu", "/dev/urandom")
	write = append(write, "/dev/null")
	_, err := Landlock(Paths{ReadExec: rx, Read: read, Write: write, NoBind: true})
	return err
}
