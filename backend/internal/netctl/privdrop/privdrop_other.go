//go:build !linux

// Package privdrop drops process privileges; only Linux is supported.
package privdrop

import "errors"

// Drop is only available on Linux.
func Drop(uid, gid int) error {
	return errors.New("privdrop: dropping privileges requires Linux")
}

// Verify is only available on Linux.
func Verify() error { return nil }

// DropBounding is not supported on this platform.
func DropBounding() error { return errors.New("privdrop: not supported on this platform") }

// Harden is not supported on this platform.
func Harden(int, int) error { return errors.New("privdrop: not supported on this platform") }
