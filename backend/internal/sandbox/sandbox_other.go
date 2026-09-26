//go:build !linux || !(amd64 || arm64)

// Package sandbox confines the processes that handle untrusted input. Only
// Linux on amd64 and arm64 has it; elsewhere nothing is confined.
package sandbox

// Paths lists what a confined process may reach.
type Paths struct {
	Read, ReadExec, Write []string
	NoBind                bool
}

// SystemReadExec are the directories a dynamically linked program needs.
var SystemReadExec []string

// NoNewPrivs does nothing on this platform.
func NoNewPrivs() error { return nil }

// Landlock reports that nothing was confined.
func Landlock(Paths) (bool, error) { return false, nil }

// Seccomp does nothing on this platform.
func Seccomp() error { return nil }
