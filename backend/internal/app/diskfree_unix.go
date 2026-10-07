//go:build unix

package app

import "golang.org/x/sys/unix"

// nodeDiskFree is the space an unprivileged user may still take on the file
// system that holds dir.
func nodeDiskFree(dir string) (uint64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true //nolint:unconvert // Bsize is int64 on Linux, uint32 on macOS
}
