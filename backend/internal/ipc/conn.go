// Package ipc implements the protocol between the main service and each
// camera process: one JSON document per line over a private Unix socket.
//
// Every message is {"v":1,"id":"<ulid>","type":"...","at":<ms>,"data":{...}}
// and every reply is {"v":1,"re":"<id>","ok":true} or carries an error.
// Messages larger than 1 MB are rejected, and v is checked even though both
// ends are the same binary.
package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// Version is the protocol version.
const Version = 1

// MaxMessageSize is the largest accepted message, newline excluded.
const MaxMessageSize = 1 << 20

// Envelope is a message or a reply.
type Envelope struct {
	V     int             `json:"v"`
	ID    string          `json:"id,omitempty"`
	Re    string          `json:"re,omitempty"`
	Type  string          `json:"type,omitempty"`
	At    int64           `json:"at,omitempty"`
	OK    *bool           `json:"ok,omitempty"`
	Error *Error          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Error is carried by failed replies.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// Errorf builds an Error with a code.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Decode unmarshals the payload of a message.
func (e *Envelope) Decode(v any) error {
	if len(e.Data) == 0 {
		return nil
	}
	return json.Unmarshal(e.Data, v)
}

// Handler processes an incoming message. The returned value becomes the
// reply payload; a returned error becomes a failed reply.
type Handler func(ctx context.Context, msg *Envelope) (any, error)

// ErrClosed is returned when the connection is closed.
var ErrClosed = errors.New("ipc: connection closed")

// Conn is one end of the protocol. Replies are routed by the read loop;
// incoming messages are handled one at a time, in order, by a separate
// goroutine, so a handler may itself send requests.
type Conn struct {
	rwc     io.ReadWriteCloser
	handler Handler
	log     *slog.Logger

	wmu     sync.Mutex
	pmu     sync.Mutex
	pending map[string]chan *Envelope

	closeOnce sync.Once
	closed    chan struct{}
	err       error
}

// NewConn wraps a connected stream socket.
func NewConn(rwc io.ReadWriteCloser, h Handler, log *slog.Logger) *Conn {
	if log == nil {
		log = slog.Default()
	}
	return &Conn{rwc: rwc, handler: h, log: log, pending: map[string]chan *Envelope{}, closed: make(chan struct{})}
}

// Run reads messages until the connection closes or ctx is done.
func (c *Conn) Run(ctx context.Context) error {
	inbox := make(chan *Envelope, 256)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		for msg := range inbox {
			c.dispatch(ctx, msg)
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-c.closed:
		}
	}()

	r := bufio.NewReaderSize(c.rwc, 64<<10)
	var err error
	for {
		var line []byte
		line, err = readLine(r)
		if errors.Is(err, errTooLarge) {
			c.log.Warn("ipc: dropped a message larger than 1 MB")
			continue
		}
		if err != nil {
			break
		}
		var msg Envelope
		if jerr := json.Unmarshal(line, &msg); jerr != nil {
			c.log.Warn("ipc: invalid message", "error", jerr)
			continue
		}
		if msg.V != Version {
			if msg.ID != "" && msg.Re == "" {
				_ = c.reply(msg.ID, nil, Errorf("version", "unsupported protocol version %d", msg.V))
			}
			continue
		}
		if msg.Re != "" {
			c.pmu.Lock()
			ch := c.pending[msg.Re]
			delete(c.pending, msg.Re)
			c.pmu.Unlock()
			if ch != nil {
				ch <- &msg
			}
			continue
		}
		inbox <- &msg
	}
	close(inbox)
	<-workerDone
	c.shutdown(err)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return c.err
}

var errTooLarge = errors.New("message too large")

// readLine reads one newline-terminated line of at most MaxMessageSize
// bytes; longer lines are skipped entirely.
func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > MaxMessageSize+1 {
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = r.ReadSlice('\n')
			}
			if err != nil {
				return nil, err
			}
			return nil, errTooLarge
		}
		buf = append(buf, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return buf[:len(buf)-1], nil
	}
}

func (c *Conn) dispatch(ctx context.Context, msg *Envelope) {
	var data any
	var err error
	if c.handler == nil {
		err = Errorf("unsupported", "no handler")
	} else {
		data, err = c.handler(ctx, msg)
	}
	if msg.ID == "" {
		return
	}
	if rerr := c.reply(msg.ID, data, err); rerr != nil && !errors.Is(rerr, ErrClosed) {
		c.log.Debug("ipc: reply failed", "error", rerr)
	}
}

func (c *Conn) reply(id string, data any, err error) error {
	env := Envelope{V: Version, Re: id}
	if err != nil {
		var ie *Error
		if !errors.As(err, &ie) {
			ie = &Error{Code: "error", Message: err.Error()}
		}
		env.Error = ie
	} else {
		ok := true
		env.OK = &ok
		if data != nil {
			raw, merr := json.Marshal(data)
			if merr != nil {
				return merr
			}
			env.Data = raw
		}
	}
	return c.write(&env)
}

func (c *Conn) write(env *Envelope) error {
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	if len(raw) > MaxMessageSize {
		return fmt.Errorf("ipc: %s message is %d bytes, the limit is %d", env.Type, len(raw), MaxMessageSize)
	}
	raw = append(raw, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	_, err = c.rwc.Write(raw)
	if err != nil {
		c.shutdown(err)
		return ErrClosed
	}
	return nil
}

func newEnvelope(typ string, data any) (*Envelope, error) {
	env := &Envelope{V: Version, ID: ulid.Make().String(), Type: typ, At: time.Now().UnixMilli()}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		env.Data = raw
	}
	return env, nil
}

// Notify sends a message without waiting for its reply.
func (c *Conn) Notify(typ string, data any) error {
	env, err := newEnvelope(typ, data)
	if err != nil {
		return err
	}
	return c.write(env)
}

// Request sends a message and waits for its reply, decoding the reply
// payload into out when it is not nil.
func (c *Conn) Request(ctx context.Context, typ string, data, out any) error {
	env, err := newEnvelope(typ, data)
	if err != nil {
		return err
	}
	ch := make(chan *Envelope, 1)
	c.pmu.Lock()
	c.pending[env.ID] = ch
	c.pmu.Unlock()
	defer func() {
		c.pmu.Lock()
		delete(c.pending, env.ID)
		c.pmu.Unlock()
	}()
	if err := c.write(env); err != nil {
		return err
	}
	select {
	case rep := <-ch:
		if rep.Error != nil {
			return rep.Error
		}
		if out != nil {
			return rep.Decode(out)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return ErrClosed
	}
}

// Close closes the connection.
func (c *Conn) Close() error {
	c.shutdown(nil)
	return nil
}

// Done is closed when the connection is closed.
func (c *Conn) Done() <-chan struct{} { return c.closed }

func (c *Conn) shutdown(err error) {
	c.closeOnce.Do(func() {
		c.err = err
		close(c.closed)
		_ = c.rwc.Close()
	})
}
