// Package buildinfo exposes version information stamped at build time.
package buildinfo

// Version is the MockVision release version (semver). It is overridden at
// build time with -ldflags "-X .../buildinfo.Version=1.2.3".
var Version = "0.1.0-dev"

// Commit is the git commit the binary was built from, when known.
var Commit = ""

// UserAgent is the User-Agent used by outbound HTTP requests of the node
// itself (not by simulated cameras, which use their profile's identity).
func UserAgent() string {
	return "MockVision/" + Version
}
