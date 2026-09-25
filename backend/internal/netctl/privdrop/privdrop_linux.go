// Package privdrop drops every privilege of the running process: it
// switches to an unprivileged user, clears all capability sets including
// the bounding set, and sets no_new_privs. Camera processes call it before
// reading any input, so no capability ever reaches the code that talks to
// the LAN.
package privdrop

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Drop switches to uid and gid and clears every capability. The binary
// must be built with CGO_ENABLED=0: capability sets are per thread and only
// then can Go change them on every thread at once.
func Drop(uid, gid int) error {
	if uid <= 0 || gid <= 0 {
		return fmt.Errorf("privdrop: refusing to switch to uid %d gid %d", uid, gid)
	}
	if err := allThreads(unix.SYS_PRCTL, unix.PR_SET_NO_NEW_PRIVS, 1, 0); err != nil {
		return fmt.Errorf("privdrop: no_new_privs: %w", err)
	}
	last, err := lastCap()
	if err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		for c := 0; c <= last; c++ {
			if err := allThreads(unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, uintptr(c), 0); err != nil && !errors.Is(err, unix.EINVAL) {
				return fmt.Errorf("privdrop: drop bounding capability %d: %w", c, err)
			}
		}
	}
	if err := allThreads(unix.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0); err != nil && !errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("privdrop: clear ambient capabilities: %w", err)
	}
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("privdrop: setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("privdrop: setgid: %w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("privdrop: setuid: %w", err)
	}
	// setuid cleared the permitted and effective sets; the inheritable set
	// survives it, so clear it explicitly.
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := allThreads(unix.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); err != nil {
		return fmt.Errorf("privdrop: clear inheritable capabilities: %w", err)
	}
	return Verify()
}

func allThreads(trap, a1, a2, a3 uintptr) error {
	_, _, errno := syscall.AllThreadsSyscall(trap, a1, a2, a3)
	if errno == syscall.ENOTSUP {
		return errors.New("the binary was built with cgo; build it with CGO_ENABLED=0")
	}
	if errno != 0 {
		return errno
	}
	return nil
}

func lastCap() (int, error) {
	b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap")
	if err != nil {
		return 40, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("privdrop: cap_last_cap: %w", err)
	}
	return n, nil
}

// Status is the privilege state of a process, from /proc/<pid>/status.
type Status struct {
	Uid, Gid                       string
	CapInh, CapPrm, CapEff, CapBnd string
	CapAmb                         string
	NoNewPrivs                     string
}

// None reports whether the process holds no capability at all.
func (s Status) None() bool {
	for _, v := range []string{s.CapInh, s.CapPrm, s.CapEff, s.CapBnd, s.CapAmb} {
		if strings.Trim(v, "0") != "" {
			return false
		}
	}
	return true
}

// ReadStatus parses /proc/<pid>/status; pid 0 means the current process.
func ReadStatus(pid int) (Status, error) {
	path := "/proc/self/status"
	if pid > 0 {
		path = fmt.Sprintf("/proc/%d/status", pid)
	}
	f, err := os.Open(path)
	if err != nil {
		return Status{}, err
	}
	defer f.Close()
	var s Status
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "Uid":
			s.Uid = v
		case "Gid":
			s.Gid = v
		case "CapInh":
			s.CapInh = v
		case "CapPrm":
			s.CapPrm = v
		case "CapEff":
			s.CapEff = v
		case "CapBnd":
			s.CapBnd = v
		case "CapAmb":
			s.CapAmb = v
		case "NoNewPrivs":
			s.NoNewPrivs = v
		}
	}
	return s, sc.Err()
}

// Verify checks that the current process and all its threads hold no
// capabilities.
func Verify() error {
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return err
	}
	for _, t := range tasks {
		tid, err := strconv.Atoi(t.Name())
		if err != nil {
			continue
		}
		s, err := readTaskStatus(tid)
		if err != nil {
			continue // the thread exited
		}
		if !s.None() {
			return fmt.Errorf("privdrop: thread %d still holds capabilities (eff %s, bnd %s)", tid, s.CapEff, s.CapBnd)
		}
		if s.NoNewPrivs != "1" {
			return fmt.Errorf("privdrop: thread %d lacks no_new_privs", tid)
		}
	}
	return nil
}

func readTaskStatus(tid int) (Status, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/self/task/%d/status", tid))
	if err != nil {
		return Status{}, err
	}
	var s Status
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "CapInh":
			s.CapInh = v
		case "CapPrm":
			s.CapPrm = v
		case "CapEff":
			s.CapEff = v
		case "CapBnd":
			s.CapBnd = v
		case "CapAmb":
			s.CapAmb = v
		case "NoNewPrivs":
			s.NoNewPrivs = v
		}
	}
	return s, nil
}
