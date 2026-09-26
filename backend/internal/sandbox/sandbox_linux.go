//go:build linux && (amd64 || arm64)

// Package sandbox confines the processes that handle untrusted input: the
// cameras, which serve the LAN; FFmpeg, which decodes uploaded images; and
// the package validator. It uses Landlock to limit the files a process can
// reach and a seccomp filter that refuses the system calls none of them
// need (namespaces, mounts, tracing, kernel modules, keyrings, clocks…).
//
// Both are applied to every thread at once, which needs a pure Go binary
// (CGO_ENABLED=0), and both need no_new_privs. Landlock is best effort: on
// a kernel without it the process goes on unconfined and Landlock reports
// false.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Paths lists what a confined process may reach. Anything else in the
// file system is refused.
type Paths struct {
	// Read may be read and listed.
	Read []string
	// ReadExec may also be executed (binaries and libraries).
	ReadExec []string
	// Write may be read, written and have files created and removed.
	Write []string
	// NoBind forbids binding TCP ports (Landlock ABI 4 or later).
	NoBind bool
}

// SystemReadExec are the directories a dynamically linked program such as
// FFmpeg needs to start: its binary, libraries and configuration.
var SystemReadExec = []string{"/usr", "/lib", "/lib32", "/lib64", "/bin", "/sbin", "/etc", "/opt", "/nix"}

// NoNewPrivs sets no_new_privs on every thread; Landlock and seccomp need
// it, and it keeps execve from ever granting privileges.
func NoNewPrivs() error {
	if _, _, errno := syscall.AllThreadsSyscall(unix.SYS_PRCTL, unix.PR_SET_NO_NEW_PRIVS, 1, 0); errno != 0 {
		if errno == syscall.ENOTSUP {
			return errors.New("sandbox: the binary was built with cgo; build it with CGO_ENABLED=0")
		}
		return fmt.Errorf("sandbox: no_new_privs: %w", errno)
	}
	return nil
}

// Landlock rights by ABI version.
const (
	fsRead  = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR
	fsExec  = unix.LANDLOCK_ACCESS_FS_EXECUTE
	fsWrite = unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM
	fsReferV2    = unix.LANDLOCK_ACCESS_FS_REFER
	fsTruncateV3 = unix.LANDLOCK_ACCESS_FS_TRUNCATE
	fsIoctlV5    = unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
)

// Landlock restricts the file system of every thread to paths. It returns
// false, with no error, when the kernel has no Landlock. NoNewPrivs must
// have been called.
func Landlock(paths Paths) (bool, error) {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		if errno == unix.ENOSYS || errno == unix.EOPNOTSUPP {
			return false, nil
		}
		return false, fmt.Errorf("sandbox: landlock version: %w", errno)
	}
	handled := uint64(fsRead | fsExec | fsWrite)
	if abi >= 2 {
		handled |= fsReferV2
	}
	if abi >= 3 {
		handled |= fsTruncateV3
	}
	if abi >= 5 {
		handled |= fsIoctlV5
	}
	attr := unix.LandlockRulesetAttr{Access_fs: handled}
	size := unsafe.Sizeof(attr)
	if abi < 4 {
		// Older kernels reject the network field.
		size = unsafe.Offsetof(attr.Access_net)
	} else if paths.NoBind {
		attr.Access_net = unix.LANDLOCK_ACCESS_NET_BIND_TCP
	}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), size, 0)
	if errno != 0 {
		return false, fmt.Errorf("sandbox: landlock ruleset: %w", errno)
	}
	defer unix.Close(int(fd))

	write := handled &^ uint64(fsExec)
	for _, set := range []struct {
		dirs   []string
		access uint64
	}{
		{paths.Read, uint64(fsRead)},
		{paths.ReadExec, uint64(fsRead | fsExec)},
		{paths.Write, write},
	} {
		for _, p := range set.dirs {
			if err := addPath(int(fd), p, set.access); err != nil {
				return false, err
			}
		}
	}
	if _, _, errno := syscall.AllThreadsSyscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); errno != 0 {
		return false, fmt.Errorf("sandbox: landlock restrict: %w", errno)
	}
	return true, nil
}

// addPath allows access beneath p. A path that does not exist is skipped:
// the lists name every place a system may keep its libraries.
func addPath(ruleset int, p string, access uint64) error {
	f, err := os.OpenFile(filepath.Clean(p), unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("sandbox: %w", err)
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && !st.IsDir() {
		// Rights on a file are only the ones that apply to files.
		access &= uint64(unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
			unix.LANDLOCK_ACCESS_FS_EXECUTE | fsTruncateV3 | fsIoctlV5)
	}
	rule := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(f.Fd())}
	if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), unix.LANDLOCK_RULE_PATH_BENEATH,
		uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("sandbox: landlock rule for %s: %w", p, errno)
	}
	return nil
}

// seccomp filter instructions.
const (
	bpfLd   = 0x00
	bpfJmp  = 0x05
	bpfRet  = 0x06
	bpfW    = 0x00
	bpfAbs  = 0x20
	bpfJeq  = 0x10
	bpfJset = 0x40
	bpfK    = 0x00

	retAllow = 0x7fff0000 // SECCOMP_RET_ALLOW
	retErrno = 0x00050000 // SECCOMP_RET_ERRNO

	offNr   = 0  // seccomp_data.nr
	offArch = 4  // seccomp_data.arch
	offArg0 = 16 // seccomp_data.args[0], low 32 bits on little-endian
)

// namespaceFlags are the clone flags that create namespaces. CLONE_NEWTIME
// is left out: clone keeps the exit signal in those bits, and only clone3
// and unshare accept it, which are refused anyway.
const namespaceFlags = unix.CLONE_NEWNS | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC | unix.CLONE_NEWUSER |
	unix.CLONE_NEWPID | unix.CLONE_NEWNET | unix.CLONE_NEWCGROUP

// Seccomp installs, on every thread, a filter that refuses with EPERM the
// system calls in denied and clone calls that create namespaces, and makes
// clone3 fail with ENOSYS so callers fall back to clone. NoNewPrivs must
// have been called.
func Seccomp() error {
	f := []unix.SockFilter{
		{Code: bpfLd | bpfW | bpfAbs, K: offArch},
		{Code: bpfJmp | bpfJeq | bpfK, Jt: 1, K: nativeArch},
		{Code: bpfRet | bpfK, K: retErrno | uint32(unix.EPERM)},
		{Code: bpfLd | bpfW | bpfAbs, K: offNr},
	}
	f = append(f, extraRules()...)
	f = append(f,
		unix.SockFilter{Code: bpfJmp | bpfJeq | bpfK, Jt: 0, Jf: 1, K: unix.SYS_CLONE3},
		unix.SockFilter{Code: bpfRet | bpfK, K: retErrno | uint32(unix.ENOSYS)},
		unix.SockFilter{Code: bpfJmp | bpfJeq | bpfK, Jt: 0, Jf: 3, K: unix.SYS_CLONE},
		unix.SockFilter{Code: bpfLd | bpfW | bpfAbs, K: offArg0},
		unix.SockFilter{Code: bpfJmp | bpfJset | bpfK, Jt: 0, Jf: 1, K: namespaceFlags},
		unix.SockFilter{Code: bpfRet | bpfK, K: retErrno | uint32(unix.EPERM)},
		// The accumulator may hold the clone flags now: load nr again.
		unix.SockFilter{Code: bpfLd | bpfW | bpfAbs, K: offNr},
	)
	for _, nr := range denied() {
		f = append(f,
			unix.SockFilter{Code: bpfJmp | bpfJeq | bpfK, Jt: 0, Jf: 1, K: uint32(nr)},
			unix.SockFilter{Code: bpfRet | bpfK, K: retErrno | uint32(unix.EPERM)},
		)
	}
	f = append(f, unix.SockFilter{Code: bpfRet | bpfK, K: retAllow})
	prog := unix.SockFprog{Len: uint16(len(f)), Filter: &f[0]}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC,
		uintptr(unsafe.Pointer(&prog))); errno != 0 {
		return fmt.Errorf("sandbox: seccomp: %w", errno)
	}
	return nil
}

// denied lists the system calls no confined process needs, on every
// architecture; deniedArch adds the ones of this architecture.
func denied() []uintptr {
	return append([]uintptr{
		unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_CHROOT, unix.SYS_UNSHARE, unix.SYS_SETNS,
		unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV,
		unix.SYS_KEXEC_LOAD, unix.SYS_KEXEC_FILE_LOAD, unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE,
		unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY,
		unix.SYS_SWAPON, unix.SYS_SWAPOFF, unix.SYS_REBOOT, unix.SYS_ACCT, unix.SYS_SETTIMEOFDAY,
		unix.SYS_CLOCK_SETTIME, unix.SYS_CLOCK_ADJTIME, unix.SYS_ADJTIMEX, unix.SYS_USERFAULTFD,
		unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_NAME_TO_HANDLE_AT, unix.SYS_QUOTACTL, unix.SYS_LOOKUP_DCOOKIE, unix.SYS_SYSLOG,
		unix.SYS_FSOPEN, unix.SYS_FSMOUNT, unix.SYS_FSCONFIG, unix.SYS_MOVE_MOUNT, unix.SYS_OPEN_TREE,
	}, deniedArch()...)
}
