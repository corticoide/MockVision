package mqttpub

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/corticoide/mockvision/backend/internal/engines/delivery"
)

// MQTT 3.1.1 control packet types.
const (
	pktConnect    = 1
	pktConnack    = 2
	pktPublish    = 3
	pktPuback     = 4
	pktPubrec     = 5
	pktPubrel     = 6
	pktPubcomp    = 7
	pktPingreq    = 12
	pktPingresp   = 13
	pktDisconnect = 14
)

// maxPacket bounds what the camera reads from a broker: it only expects
// acknowledgements.
const maxPacket = 64 << 10

// connackReasons are the CONNACK return codes of MQTT 3.1.1.
var connackReasons = map[byte]string{
	1: "unacceptable protocol version",
	2: "client identifier rejected",
	3: "server unavailable",
	4: "bad user name or password",
	5: "not authorized",
}

// Message is an application message.
type Message struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
}

// ConnectOptions open a session with a broker.
type ConnectOptions struct {
	// URL is mqtt://host[:1883] or mqtts://host[:8883].
	URL       string
	ClientID  string
	Username  string
	Password  string
	Insecure  bool
	KeepAlive time.Duration
	Clean     bool
	Will      *Message
}

// Address returns the host and port of a broker URL, with the default port
// of its scheme, and whether it uses TLS.
func Address(raw string) (hostport string, useTLS bool, err error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", false, fmt.Errorf("broker URL must look like mqtt://host:1883")
	}
	port := "1883"
	switch u.Scheme {
	case "mqtt", "tcp":
	case "mqtts", "ssl":
		port, useTLS = "8883", true
	default:
		return "", false, fmt.Errorf("broker URL scheme must be mqtt or mqtts")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "", false, fmt.Errorf("invalid port %q", p)
		}
		port = p
	}
	return net.JoinHostPort(u.Hostname(), port), useTLS, nil
}

// Session is a connection to a broker. It sends PINGREQ when idle for the
// keep-alive interval, and fails when the broker stops answering.
type Session struct {
	conn net.Conn
	keep time.Duration

	wmu    sync.Mutex // one packet written at a time
	mu     sync.Mutex
	nextID uint16
	acks   map[uint16]chan byte // packet ID -> PUBACK, PUBREC or PUBCOMP
	pong   chan struct{}
	err    error
	done   chan struct{}
	used   time.Time
}

// Connect opens a session: TCP or TLS, then CONNECT and CONNACK within ctx.
func Connect(ctx context.Context, o ConnectOptions) (*Session, error) {
	addr, useTLS, err := Address(o.URL)
	if err != nil {
		return nil, err
	}
	conn, err := delivery.Dial(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if useTLS {
		host, _, _ := net.SplitHostPort(addr)
		tc := tls.Client(conn, delivery.TLSConfig(host, o.Insecure))
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		conn = tc
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := writePacket(conn, pktConnect<<4, connectBody(o)); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	typ, body, err := readPacket(br)
	if err != nil {
		conn.Close()
		if errors.Is(err, io.EOF) {
			err = errors.New("the broker closed the connection")
		}
		return nil, err
	}
	if typ>>4 != pktConnack || len(body) != 2 {
		conn.Close()
		return nil, fmt.Errorf("the broker did not answer CONNACK")
	}
	if code := body[1]; code != 0 {
		conn.Close()
		reason := connackReasons[code]
		if reason == "" {
			reason = "refused"
		}
		return nil, fmt.Errorf("broker refused the connection: %s (%d)", reason, code)
	}
	_ = conn.SetDeadline(time.Time{})
	s := &Session{conn: conn, keep: o.KeepAlive, acks: map[uint16]chan byte{}, pong: make(chan struct{}, 1), done: make(chan struct{}), used: time.Now()}
	go s.read(br)
	if s.keep > 0 {
		go s.keepAlive()
	}
	return s, nil
}

func connectBody(o ConnectOptions) []byte {
	var flags byte
	if o.Clean {
		flags |= 0x02
	}
	if o.Will != nil {
		flags |= 0x04 | o.Will.QoS<<3
		if o.Will.Retain {
			flags |= 0x20
		}
	}
	if o.Username != "" {
		flags |= 0x80
		if o.Password != "" {
			flags |= 0x40
		}
	}
	keep := uint16(min(o.KeepAlive/time.Second, 65535))
	b := appendString(nil, "MQTT")
	b = append(b, 4, flags, byte(keep>>8), byte(keep))
	b = appendString(b, o.ClientID)
	if o.Will != nil {
		b = appendString(b, o.Will.Topic)
		b = appendBytes(b, o.Will.Payload)
	}
	if o.Username != "" {
		b = appendString(b, o.Username)
		if o.Password != "" {
			b = appendString(b, o.Password)
		}
	}
	return b
}

// Publish sends a message and, for QoS 1 and 2, waits for the broker's
// acknowledgement within ctx.
func (s *Session) Publish(ctx context.Context, m Message) error {
	if err := s.Err(); err != nil {
		return err
	}
	header := byte(pktPublish<<4) | m.QoS<<1
	if m.Retain {
		header |= 0x01
	}
	body := appendString(nil, m.Topic)
	var id uint16
	var ack chan byte
	if m.QoS > 0 {
		s.mu.Lock()
		for {
			s.nextID++
			if s.nextID != 0 && s.acks[s.nextID] == nil {
				break
			}
		}
		id = s.nextID
		ack = make(chan byte, 2)
		s.acks[id] = ack
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.acks, id)
			s.mu.Unlock()
		}()
		body = binary.BigEndian.AppendUint16(body, id)
	}
	body = append(body, m.Payload...)
	if err := s.write(ctx, header, body); err != nil {
		return err
	}
	switch m.QoS {
	case 0:
		return nil
	case 1:
		return s.await(ctx, ack, pktPuback)
	}
	// QoS 2: PUBREC, then PUBREL and PUBCOMP.
	if err := s.await(ctx, ack, pktPubrec); err != nil {
		return err
	}
	if err := s.write(ctx, pktPubrel<<4|0x02, binary.BigEndian.AppendUint16(nil, id)); err != nil {
		return err
	}
	return s.await(ctx, ack, pktPubcomp)
}

func (s *Session) await(ctx context.Context, ack chan byte, want byte) error {
	select {
	case got := <-ack:
		if got != want {
			return fmt.Errorf("the broker answered packet type %d instead of %d", got, want)
		}
		return nil
	case <-s.done:
		return s.Err()
	case <-ctx.Done():
		s.fail(fmt.Errorf("no acknowledgement from the broker"))
		return ctx.Err()
	}
}

func (s *Session) write(ctx context.Context, header byte, body []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if dl, ok := ctx.Deadline(); ok {
		_ = s.conn.SetWriteDeadline(dl)
	} else {
		_ = s.conn.SetWriteDeadline(time.Now().Add(delivery.DefaultTimeout))
	}
	if err := writePacket(s.conn, header, body); err != nil {
		s.fail(err)
		return err
	}
	s.mu.Lock()
	s.used = time.Now()
	s.mu.Unlock()
	return nil
}

func (s *Session) read(br *bufio.Reader) {
	for {
		typ, body, err := readPacket(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("the broker closed the connection")
			}
			s.fail(err)
			return
		}
		switch typ >> 4 {
		case pktPingresp:
			select {
			case s.pong <- struct{}{}:
			default:
			}
		case pktPuback, pktPubrec, pktPubcomp:
			if len(body) < 2 {
				continue
			}
			id := binary.BigEndian.Uint16(body)
			s.mu.Lock()
			ack := s.acks[id]
			s.mu.Unlock()
			if ack != nil {
				select {
				case ack <- typ >> 4:
				default:
				}
			}
		}
		// A broker sends nothing else to a client that never subscribes.
	}
}

// keepAlive pings the broker when nothing was sent for the interval and
// fails the session when the answer does not come within it either.
func (s *Session) keepAlive() {
	tick := time.NewTicker(s.keep / 2)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
		}
		s.mu.Lock()
		idle := time.Since(s.used)
		s.mu.Unlock()
		if idle < s.keep {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.keep)
		err := s.write(ctx, pktPingreq<<4, nil)
		if err == nil {
			select {
			case <-s.pong:
			case <-s.done:
			case <-ctx.Done():
				s.fail(errors.New("the broker stopped answering"))
			}
		}
		cancel()
	}
}

func (s *Session) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return
	}
	s.err = err
	close(s.done)
	s.conn.Close()
}

// Err is why the session ended, nil while it is up.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.done }

// Close sends DISCONNECT and closes the connection; the broker then drops
// the will message.
func (s *Session) Close() {
	if s.Err() == nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = s.write(ctx, pktDisconnect<<4, nil)
		cancel()
	}
	s.fail(errors.New("closed"))
}

func appendString(b []byte, s string) []byte { return appendBytes(b, []byte(s)) }

func appendBytes(b, s []byte) []byte {
	b = binary.BigEndian.AppendUint16(b, uint16(len(s)))
	return append(b, s...)
}

func writePacket(w io.Writer, header byte, body []byte) error {
	buf := []byte{header}
	n := len(body)
	for {
		d := byte(n % 128)
		n /= 128
		if n > 0 {
			d |= 0x80
		}
		buf = append(buf, d)
		if n == 0 {
			break
		}
	}
	_, err := w.Write(append(buf, body...))
	return err
}

func readPacket(r *bufio.Reader) (byte, []byte, error) {
	typ, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	n, mult := 0, 1
	for i := 0; ; i++ {
		d, err := r.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		n += int(d&0x7f) * mult
		if d&0x80 == 0 {
			break
		}
		if i == 3 {
			return 0, nil, errors.New("malformed packet length")
		}
		mult *= 128
	}
	if n > maxPacket {
		return 0, nil, fmt.Errorf("packet of %d bytes from the broker", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return typ, body, nil
}
