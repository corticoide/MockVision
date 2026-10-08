package plugin

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/corticoide/mockvision/sdk/engine"
	pb "github.com/corticoide/mockvision/sdk/proto/mockvision/engine/v1"
)

// Serve runs an engine as a plugin, until its camera stops it or goes
// away, and returns the exit code. Run without a camera, it prints the
// engine's descriptor, which is how MockVision describes a plugin when it
// installs it.
func Serve(e engine.Engine) int {
	if os.Getenv(EnvPlugin) == "" {
		d := DescriptorToProto(e.Describe())
		fmt.Printf("%s %s, contract %d: a MockVision plugin; the camera process runs it.\n", d.Name, d.Version, d.Contract)
		return 2
	}
	engineConn, err := fileConn(EngineFD, "engine")
	if err != nil {
		fmt.Fprintln(os.Stderr, "plugin:", err)
		return 1
	}
	hostConn, err := fileConn(HostFD, "host")
	if err != nil {
		fmt.Fprintln(os.Stderr, "plugin:", err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	cc, err := Dial(hostConn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "plugin:", err)
		return 1
	}
	defer cc.Close()
	srv := grpc.NewServer(ServerOptions()...)
	es := &engineServer{e: e, host: NewHost(ctx, pb.NewHostClient(cc)), stopped: make(chan struct{})}
	pb.RegisterEngineServer(srv, es)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(Listener(engineConn)) }()
	select {
	case <-ctx.Done():
	case <-es.stopped:
	case <-served:
		// The camera went away: stop what the engine runs.
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = e.Stop(sctx)
		scancel()
	}
	// Stop answers before the process ends.
	go func() {
		time.Sleep(200 * time.Millisecond)
		srv.Stop()
	}()
	srv.GracefulStop()
	return 0
}

// fileConn turns an inherited socket into a connection.
func fileConn(fd int, name string) (net.Conn, error) {
	f := os.NewFile(uintptr(fd), name)
	if f == nil {
		return nil, fmt.Errorf("descriptor %d (%s) is not open", fd, name)
	}
	defer f.Close()
	return net.FileConn(f)
}

// engineServer serves the Engine service for one engine instance.
type engineServer struct {
	pb.UnimplementedEngineServer
	e       engine.Engine
	host    engine.Host
	mu      sync.Mutex
	started bool
	stopped chan struct{}
	once    sync.Once
}

func (s *engineServer) Describe(context.Context, *pb.DescribeRequest) (*pb.Descriptor, error) {
	return DescriptorToProto(s.e.Describe()), nil
}

func (s *engineServer) Validate(_ context.Context, r *pb.ValidateRequest) (*pb.ValidateResponse, error) {
	out := &pb.ValidateResponse{}
	for _, p := range s.e.Validate(r.GetConfig()) {
		out.Problems = append(out.Problems, &pb.Problem{Path: p.Path, Message: p.Message, Warning: p.Warning})
	}
	return out, nil
}

func (s *engineServer) Start(ctx context.Context, r *pb.StartRequest) (*pb.StartResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil, status.Error(codes.FailedPrecondition, "the engine runs already")
	}
	in := engine.StartInput{Identity: IdentityFromProto(r.GetIdentity()), Instance: r.GetInstance(), Config: r.GetConfig(),
		Port: int(r.GetPort()), Listeners: map[string]net.Listener{}, PacketConns: map[string]net.PacketConn{}, Host: s.host}
	for name, fd := range r.GetSocketFds() {
		f := os.NewFile(uintptr(fd), name)
		if f == nil {
			return nil, status.Errorf(codes.InvalidArgument, "socket %s: descriptor %d is not open", name, fd)
		}
		if ln, err := net.FileListener(f); err == nil {
			in.Listeners[name] = ln
		} else if pc, err := net.FilePacketConn(f); err == nil {
			in.PacketConns[name] = pc
		} else {
			f.Close()
			return nil, status.Errorf(codes.InvalidArgument, "socket %s: %v", name, err)
		}
		f.Close()
	}
	// The engine outlives the call: it runs until Stop.
	if err := s.e.Start(context.Background(), in); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	s.started = true
	return &pb.StartResponse{}, nil
}

func (s *engineServer) Reload(ctx context.Context, r *pb.ReloadRequest) (*pb.ReloadResponse, error) {
	if err := s.e.Reload(ctx, r.GetConfig()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pb.ReloadResponse{}, nil
}

func (s *engineServer) Health(context.Context, *pb.HealthRequest) (*pb.HealthResponse, error) {
	return HealthToProto(s.e.Health()), nil
}

func (s *engineServer) Stop(ctx context.Context, r *pb.StopRequest) (*pb.StopResponse, error) {
	d := time.Duration(r.GetDeadlineMs()) * time.Millisecond
	if d <= 0 {
		d = 5 * time.Second
	}
	sctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	err := s.e.Stop(sctx)
	s.once.Do(func() { close(s.stopped) })
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pb.StopResponse{}, nil
}
