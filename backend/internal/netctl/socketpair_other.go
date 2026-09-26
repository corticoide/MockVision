//go:build unix && !linux

package netctl

import "syscall"

// socketpairCloexec returns a connected stream pair, close-on-exec before
// any other goroutine can fork.
func socketpairCloexec() ([2]int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return fds, err
	}
	syscall.CloseOnExec(fds[0])
	syscall.CloseOnExec(fds[1])
	return fds, nil
}
