//go:build linux && arm64

package sandbox

import "golang.org/x/sys/unix"

const nativeArch = unix.AUDIT_ARCH_AARCH64

func extraRules() []unix.SockFilter { return nil }

func deniedArch() []uintptr { return nil }
