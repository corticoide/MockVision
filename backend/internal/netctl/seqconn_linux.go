package netctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/corticoide/mockvision/backend/internal/ipc"
)

// Helper protocol message types.
const (
	msgCreate = "camera.create"
	msgDelete = "camera.delete"
	msgList   = "camera.list"
	msgExited = "camera.exited"
	msgPing   = "ping"
)

// maxHelperMessage bounds a helper protocol message.
const maxHelperMessage = 64 << 10

// seqConn carries one JSON envelope per SOCK_SEQPACKET message, with file
// descriptors attached as SCM_RIGHTS.
type seqConn struct {
	c   *net.UnixConn
	wmu sync.Mutex
}

func newSeqConn(f *os.File) (*seqConn, error) {
	c, err := net.FileConn(f)
	if err != nil {
		return nil, err
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		c.Close()
		return nil, errors.New("helper socket is not a Unix socket")
	}
	return &seqConn{c: uc}, nil
}

// seqPair returns two connected SOCK_SEQPACKET files.
func seqPair() (*os.File, *os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[0]), "helper"), os.NewFile(uintptr(fds[1]), "service"), nil
}

func (s *seqConn) send(env *ipc.Envelope, fds ...int) error {
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	if len(raw) > maxHelperMessage {
		return fmt.Errorf("helper message too large (%d bytes)", len(raw))
	}
	var oob []byte
	if len(fds) > 0 {
		oob = unix.UnixRights(fds...)
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, _, err = s.c.WriteMsgUnix(raw, oob, nil)
	return err
}

func (s *seqConn) recv() (*ipc.Envelope, []int, error) {
	buf := make([]byte, maxHelperMessage)
	oob := make([]byte, unix.CmsgSpace(8*4))
	n, oobn, flags, _, err := s.c.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, nil, err
	}
	if n == 0 && oobn == 0 {
		return nil, nil, net.ErrClosed
	}
	var fds []int
	if oobn > 0 {
		msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
		if err == nil {
			for i := range msgs {
				if got, err := unix.ParseUnixRights(&msgs[i]); err == nil {
					fds = append(fds, got...)
				}
			}
		}
	}
	if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
		closeFDs(fds)
		return nil, nil, errors.New("helper message truncated")
	}
	var env ipc.Envelope
	if err := json.Unmarshal(buf[:n], &env); err != nil {
		closeFDs(fds)
		return nil, nil, fmt.Errorf("invalid helper message: %w", err)
	}
	if env.V != ipc.Version {
		closeFDs(fds)
		return nil, nil, fmt.Errorf("unsupported helper protocol version %d", env.V)
	}
	return &env, fds, nil
}

func (s *seqConn) close() error { return s.c.Close() }

func closeFDs(fds []int) {
	for _, fd := range fds {
		unix.Close(fd)
	}
}
