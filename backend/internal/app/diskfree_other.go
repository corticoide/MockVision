//go:build !unix

package app

// nodeDiskFree is unknown here: cards are admitted without the disk check.
func nodeDiskFree(string) (uint64, bool) { return 0, false }
