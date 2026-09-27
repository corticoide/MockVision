package netctl

import "golang.org/x/sys/unix"

// socketpairCloexec returns a connected stream pair, close-on-exec from
// the start.
func socketpairCloexec() ([2]int, error) {
	return unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
}
