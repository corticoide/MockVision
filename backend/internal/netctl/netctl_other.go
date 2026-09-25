//go:build !linux

package netctl

import (
	"errors"
	"log/slog"
	"os"
)

var errLinuxOnly = errors.New("network namespaces require Linux; use serve --net local for development")

// RunMain is only available on Linux.
func RunMain(_ []string, log *slog.Logger) int {
	log.Error(errLinuxOnly.Error())
	return 2
}

// HelperRuntime is only available on Linux.
type HelperRuntime struct{ LocalRuntime }

// NewHelperRuntime is only available on Linux.
func NewHelperRuntime(*os.File, *slog.Logger) (*HelperRuntime, error) {
	return nil, errLinuxOnly
}

// Closed is only available on Linux.
func (r *HelperRuntime) Closed() <-chan struct{} { return nil }
