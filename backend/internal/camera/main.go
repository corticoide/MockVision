package camera

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/corticoide/mockvision/backend/internal/netctl/privdrop"
)

type socketFlags []string

func (s *socketFlags) String() string     { return strings.Join(*s, ",") }
func (s *socketFlags) Set(v string) error { *s = append(*s, v); return nil }

// Main is the entry point of the camera subcommand.
func Main(args []string) int {
	fs := flag.NewFlagSet("camera", flag.ContinueOnError)
	id := fs.String("id", "", "camera ID")
	ipcFD := fs.Int("ipc-fd", 3, "descriptor of the socket connected to the main service")
	uid := fs.Int("uid", -1, "user to switch to; all privileges are dropped before reading any input")
	gid := fs.Int("gid", -1, "group to switch to")
	local := fs.Bool("local", false, "open sockets on 127.0.0.1 (development mode, no namespaces)")
	verbose := fs.Bool("v", false, "debug logging")
	var sockets socketFlags
	fs.Var(&sockets, "socket", "inherited socket as instance:name:network:port=fd (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// First thing: drop every privilege. The network helper starts cameras
	// as root inside their namespace; nothing below needs privileges.
	if *uid >= 0 {
		if err := privdrop.Drop(*uid, *gid); err != nil {
			fmt.Fprintln(os.Stderr, "camera:", err)
			return 1
		}
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})).With("camera", *id)

	f := os.NewFile(uintptr(*ipcFD), "ipc")
	if f == nil {
		log.Error("invalid ipc descriptor")
		return 1
	}
	conn, err := net.FileConn(f)
	f.Close()
	if err != nil {
		log.Error("ipc socket", "error", err)
		return 1
	}
	var socks []Socket
	for _, s := range sockets {
		sock, err := ParseSocket(s)
		if err != nil {
			log.Error("bad socket flag", "error", err)
			return 2
		}
		socks = append(socks, sock)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	rt := NewRuntime(Options{CameraID: *id, IPC: conn, Sockets: socks, Local: *local, Log: log})
	if err := rt.Run(ctx); err != nil {
		log.Error("camera stopped", "error", err)
		return 1
	}
	return 0
}
