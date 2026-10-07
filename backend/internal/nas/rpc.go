package nas

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// ONC RPC over TCP (RFC 5531), as NFS speaks it: calls with AUTH_SYS
// credentials, one at a time on a connection, in record-marked fragments.

const (
	rpcCall       = 0
	rpcReply      = 1
	rpcVersion    = 2
	authNone      = 0
	authSys       = 1
	msgAccepted   = 0
	acceptSuccess = 0
	lastFragment  = 1 << 31
	// maxRecord bounds a reply: a READ of maxIO bytes and its attributes.
	maxRecord = 1 << 20
)

var acceptErrors = map[uint32]string{
	1: "program unavailable", 2: "program version mismatch", 3: "procedure unavailable", 4: "garbage arguments", 5: "system error",
}

// rpcConn is a connection to one RPC program.
type rpcConn struct {
	mu   sync.Mutex
	conn net.Conn
	prog uint32
	vers uint32
	cred []byte
	xid  uint32
}

// credentials is an AUTH_SYS credential body.
func credentials(machine string, uid, gid uint32) []byte {
	var e encoder
	e.uint32(uint32(time.Now().Unix()))
	e.string(machine)
	e.uint32(uid)
	e.uint32(gid)
	e.uint32(0) // no other groups
	return e.bytes()
}

func dialRPC(ctx context.Context, dial Dialer, addr string, prog, vers uint32, cred []byte) (*rpcConn, error) {
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return &rpcConn{conn: conn, prog: prog, vers: vers, cred: cred, xid: uint32(time.Now().UnixNano())}, nil
}

func (c *rpcConn) Close() error { return c.conn.Close() }

// call runs a procedure and returns its results.
func (c *rpcConn) call(ctx context.Context, proc uint32, args []byte) (*decoder, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.conn.SetDeadline(dl)
	} else {
		_ = c.conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	// A canceled context cuts the call short.
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	c.xid++
	var e encoder
	e.uint32(c.xid)
	e.uint32(rpcCall)
	e.uint32(rpcVersion)
	e.uint32(c.prog)
	e.uint32(c.vers)
	e.uint32(proc)
	if c.cred != nil {
		e.uint32(authSys)
		e.opaque(c.cred)
	} else {
		e.uint32(authNone)
		e.uint32(0)
	}
	e.uint32(authNone) // verifier
	e.uint32(0)
	e.raw(args)
	msg := e.bytes()
	frame := make([]byte, 4, 4+len(msg))
	binary.BigEndian.PutUint32(frame, uint32(len(msg))|lastFragment)
	if _, err := c.conn.Write(append(frame, msg...)); err != nil {
		return nil, c.fail(ctx, err)
	}
	for {
		rec, err := c.record()
		if err != nil {
			return nil, c.fail(ctx, err)
		}
		d := &decoder{b: rec}
		if xid := d.uint32(); xid != c.xid {
			continue // a late reply to a call that timed out
		}
		if d.uint32() != rpcReply {
			return nil, errors.New("rpc: not a reply")
		}
		if d.uint32() != msgAccepted {
			return nil, errors.New("rpc: call denied: the server refused the credentials")
		}
		d.uint32()   // verifier flavor
		d.opaque(400) // verifier body
		if st := d.uint32(); st != acceptSuccess {
			if msg, ok := acceptErrors[st]; ok {
				return nil, fmt.Errorf("rpc: %s", msg)
			}
			return nil, fmt.Errorf("rpc: call refused (%d)", st)
		}
		if d.err != nil {
			return nil, d.err
		}
		return d, nil
	}
}

func (c *rpcConn) fail(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// record reads one record, joining its fragments.
func (c *rpcConn) record() ([]byte, error) {
	var out []byte
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(c.conn, hdr[:]); err != nil {
			return nil, err
		}
		h := binary.BigEndian.Uint32(hdr[:])
		n := int(h &^ lastFragment)
		if len(out)+n > maxRecord {
			return nil, errors.New("rpc: reply too large")
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(c.conn, buf); err != nil {
			return nil, err
		}
		out = append(out, buf...)
		if h&lastFragment != 0 {
			return out, nil
		}
	}
}

// encoder writes XDR (RFC 4506).
type encoder struct{ buf bytes.Buffer }

func (e *encoder) bytes() []byte { return e.buf.Bytes() }

func (e *encoder) raw(b []byte) { e.buf.Write(b) }

func (e *encoder) uint32(v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	e.buf.Write(b[:])
}

func (e *encoder) uint64(v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	e.buf.Write(b[:])
}

func (e *encoder) bool(v bool) {
	if v {
		e.uint32(1)
	} else {
		e.uint32(0)
	}
}

func (e *encoder) opaque(b []byte) {
	e.uint32(uint32(len(b)))
	e.buf.Write(b)
	if pad := (4 - len(b)%4) % 4; pad > 0 {
		e.buf.Write(make([]byte, pad))
	}
}

func (e *encoder) string(s string) { e.opaque([]byte(s)) }

// decoder reads XDR; the first error sticks and later reads return zero.
type decoder struct {
	b   []byte
	err error
}

func (d *decoder) take(n int) []byte {
	if d.err != nil {
		return nil
	}
	if n < 0 || n > len(d.b) {
		d.err = errors.New("xdr: short reply")
		return nil
	}
	out := d.b[:n]
	d.b = d.b[n:]
	return out
}

func (d *decoder) uint32() uint32 {
	b := d.take(4)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

func (d *decoder) uint64() uint64 {
	b := d.take(8)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

func (d *decoder) bool() bool { return d.uint32() != 0 }

func (d *decoder) opaque(limit int) []byte {
	n := int(d.uint32())
	if d.err == nil && n > limit {
		d.err = errors.New("xdr: opaque too long")
		return nil
	}
	b := d.take(n)
	d.take((4 - n%4) % 4)
	return b
}

func (d *decoder) skip(n int) { d.take(n) }
