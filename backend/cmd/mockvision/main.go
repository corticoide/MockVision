// Command mockvision is the single MockVision binary. It runs as three
// kinds of process: "run" starts the network helper (the only privileged
// process) and the main service, "serve" is the unprivileged main service,
// and "camera" is one simulated camera inside its network namespace.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/corticoide/mockvision/backend/internal/buildinfo"
	"github.com/corticoide/mockvision/backend/internal/camera"
	"github.com/corticoide/mockvision/backend/internal/engines"
	"github.com/corticoide/mockvision/backend/internal/netctl"
	"github.com/corticoide/mockvision/backend/internal/pkg"
	"github.com/corticoide/mockvision/backend/internal/sandbox"
)

const usage = `MockVision %s — simulated IP cameras on a real network

Usage:
  mockvision run [flags]      start the node: network helper and main service (root)
  mockvision serve [flags]    start the main service only (--net local for development)
  mockvision pkg <command>    verify, inspect or build packages
  mockvision version          print the version

Environment: MOCKVISION_DATA, MOCKVISION_LISTEN, MOCKVISION_PARENT_IF,
MOCKVISION_NET, MOCKVISION_FFMPEG, MOCKVISION_LOG_LEVEL, MOCKVISION_LOG_FORMAT.
`

func main() {
	log := newLogger()
	slog.SetDefault(log)
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, buildinfo.Version)
		os.Exit(2)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "run":
		os.Exit(netctl.RunMain(args, log))
	case "serve":
		os.Exit(serve(args, log))
	case "camera":
		os.Exit(camera.Main(args))
	case "pkg":
		os.Exit(pkg.Main(args, engines.Builtin()))
	case "sandbox-exec":
		os.Exit(sandbox.ExecMain(args))
	case "version", "--version":
		fmt.Println(buildinfo.Version)
	case "help", "-h", "--help":
		fmt.Printf(usage, buildinfo.Version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n"+usage, os.Args[1], buildinfo.Version)
		os.Exit(2)
	}
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("MOCKVISION_LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.ToLower(os.Getenv("MOCKVISION_LOG_FORMAT")) == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
