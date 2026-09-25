package netctl

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/sys/unix"

	"github.com/corticoide/mockvision/backend/internal/ipc"
)

// HelperRuntime is the service side of the network helper: it implements
// Runtime by sending validated requests over the helper socket.
type HelperRuntime struct {
	conn  *seqConn
	log   *slog.Logger
	exits chan Exit

	mu      sync.Mutex
	pending map[string]chan *ipc.Envelope
	closed  chan struct{}
}

// NewHelperRuntime connects to the helper through an inherited socket.
func NewHelperRuntime(f *os.File, log *slog.Logger) (*HelperRuntime, error) {
	if log == nil {
		log = slog.Default()
	}
	conn, err := newSeqConn(f)
	if err != nil {
		return nil, err
	}
	r := &HelperRuntime{conn: conn, log: log, exits: make(chan Exit, 64), pending: map[string]chan *ipc.Envelope{}, closed: make(chan struct{})}
	go r.readLoop()
	return r, nil
}

// Kind implements Runtime.
func (r *HelperRuntime) Kind() string { return "netns" }

// Exits implements Runtime.
func (r *HelperRuntime) Exits() <-chan Exit { return r.exits }

// Closed is closed when the helper goes away.
func (r *HelperRuntime) Closed() <-chan struct{} { return r.closed }

func (r *HelperRuntime) readLoop() {
	defer close(r.closed)
	for {
		env, fds, err := r.conn.recv()
		if err != nil {
			r.log.Error("network helper connection lost", "error", err)
			return
		}
		closeFDs(fds)
		if env.Re != "" {
			r.mu.Lock()
			ch := r.pending[env.Re]
			delete(r.pending, env.Re)
			r.mu.Unlock()
			if ch != nil {
				ch <- env
			}
			continue
		}
		if env.Type == msgExited {
			var e Exit
			if err := json.Unmarshal(env.Data, &e); err == nil {
				select {
				case r.exits <- e:
				default:
					r.log.Warn("dropped camera exit notice", "camera", e.CameraID)
				}
			}
		}
	}
}

func (r *HelperRuntime) request(ctx context.Context, typ string, data any, fds []int, out any) error {
	env := &ipc.Envelope{V: ipc.Version, ID: ulid.Make().String(), Type: typ, At: time.Now().UnixMilli()}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		env.Data = raw
	}
	ch := make(chan *ipc.Envelope, 1)
	r.mu.Lock()
	r.pending[env.ID] = ch
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.pending, env.ID)
		r.mu.Unlock()
	}()
	if err := r.conn.send(env, fds...); err != nil {
		return err
	}
	select {
	case rep := <-ch:
		if rep.Error != nil {
			return &Error{Code: rep.Error.Code, Message: rep.Error.Message}
		}
		if out != nil && len(rep.Data) > 0 {
			return json.Unmarshal(rep.Data, out)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-r.closed:
		return errors.New("network helper is gone")
	}
}

// Launch implements Runtime.
func (r *HelperRuntime) Launch(ctx context.Context, spec LaunchSpec) (*Launched, error) {
	if err := spec.Camera.Validate(); err != nil {
		return nil, errorf(CodeInvalid, "%v", err)
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	svc := os.NewFile(uintptr(fds[0]), "camera-ipc")
	var res createResult
	err = r.request(ctx, msgCreate, spec.Camera, []int{fds[1]}, &res)
	unix.Close(fds[1])
	if err != nil {
		svc.Close()
		return nil, err
	}
	conn, err := net.FileConn(svc)
	svc.Close()
	if err != nil {
		_ = r.Destroy(context.Background(), spec.Camera.ID)
		return nil, err
	}
	return &Launched{Conn: conn, PID: res.PID, Netns: res.Netns, IP: spec.Camera.IP}, nil
}

// Destroy implements Runtime.
func (r *HelperRuntime) Destroy(ctx context.Context, cameraID string) error {
	return r.request(ctx, msgDelete, map[string]string{"id": cameraID}, nil, nil)
}

// Live implements Runtime.
func (r *HelperRuntime) Live(ctx context.Context) ([]string, error) {
	var list []liveCamera
	if err := r.request(ctx, msgList, nil, nil, &list); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list))
	for _, c := range list {
		ids = append(ids, c.ID)
	}
	return ids, nil
}
