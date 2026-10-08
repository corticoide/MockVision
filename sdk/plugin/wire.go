// Package plugin runs an engine as an external MockVision plugin (D71,
// D85): a program in a .mvpkg package that the camera process starts
// sandboxed, one process per engine instance, and talks to over gRPC.
//
// A plugin implements the same engine.Engine interface as the engines built
// into MockVision and hands it to Serve:
//
//	func main() { os.Exit(plugin.Serve(hello.New())) }
//
// The camera starts the plugin with two connected sockets and the sockets
// of its ports already open: the plugin serves the Engine service on the
// first, and reaches the camera's Host service on the second, limited to
// the permissions the user approved. Calls the plugin may not make answer
// PermissionDenied.
package plugin

import (
	"context"
	"errors"
	"net"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Descriptors the camera hands a plugin process.
const (
	// EngineFD is the socket the plugin serves the Engine service on.
	EngineFD = 3
	// HostFD is the socket the plugin reaches the Host service on.
	HostFD = 4
	// FirstSocketFD is the first of the engine's sockets; StartRequest's
	// socket_fds says which is which.
	FirstSocketFD = 5
	// EnvPlugin is set in a plugin's environment.
	EnvPlugin = "MOCKVISION_PLUGIN"
)

// MaxMessage bounds a gRPC message between a camera and a plugin.
const MaxMessage = 16 << 20

// oneConn is a listener that hands out one connection, then waits until
// it is closed: a gRPC server over a socket pair.
type oneConn struct {
	mu     sync.Mutex
	conn   net.Conn
	closed chan struct{}
	once   sync.Once
}

// Listener serves gRPC on an already connected socket.
func Listener(conn net.Conn) net.Listener {
	return &oneConn{conn: conn, closed: make(chan struct{})}
}

func (l *oneConn) Accept() (net.Conn, error) {
	l.mu.Lock()
	c := l.conn
	l.conn = nil
	l.mu.Unlock()
	if c != nil {
		return c, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneConn) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *oneConn) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "plugin" }

// Dial makes a gRPC client over an already connected socket. It does not
// dial again: when the other side goes away, calls fail.
func Dial(conn net.Conn) (*grpc.ClientConn, error) {
	var mu sync.Mutex
	used := false
	return grpc.NewClient("passthrough:///plugin",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxMessage), grpc.MaxCallSendMsgSize(MaxMessage)),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			mu.Lock()
			defer mu.Unlock()
			if used {
				return nil, errors.New("plugin: the connection is gone")
			}
			used = true
			return conn, nil
		}))
}

// ServerOptions are the options of both sides' servers.
func ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{grpc.MaxRecvMsgSize(MaxMessage), grpc.MaxSendMsgSize(MaxMessage)}
}
