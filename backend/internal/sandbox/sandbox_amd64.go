//go:build linux && amd64

package sandbox

import "golang.org/x/sys/unix"

const nativeArch = unix.AUDIT_ARCH_X86_64

// x32 system calls share the x86-64 architecture value; they are refused
// by number: bit 30 set.
func extraRules() []unix.SockFilter {
	return []unix.SockFilter{
		{Code: bpfJmp | bpfJset | bpfK, Jt: 0, Jf: 1, K: 0x40000000},
		{Code: bpfRet | bpfK, K: retErrno | uint32(unix.EPERM)},
	}
}

func deniedArch() []uintptr {
	return []uintptr{unix.SYS_IOPL, unix.SYS_IOPERM, unix.SYS_USELIB, unix.SYS_CREATE_MODULE, unix.SYS_MODIFY_LDT}
}
