package enginetest

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"path"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- MQTT ---

// MQTTMessage is a message a broker got.
type MQTTMessage struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
}

// MQTTConnect is a CONNECT a broker got.
type MQTTConnect struct {
	ClientID, Username, Password string
	KeepAlive                    int
	Will                         *MQTTMessage
}

// MQTTBroker is a minimal MQTT 3.1.1 broker: it accepts a CONNECT whose
// password matches (any, when Password is empty), acknowledges PUBLISH as
// its QoS asks and records what it got.
type MQTTBroker struct {
	Password string

	ln       net.Listener
	mu       sync.Mutex
	connects []MQTTConnect
	messages []MQTTMessage
	conns    []net.Conn
	changed  chan struct{}
}

// NewMQTTBroker listens on 127.0.0.1 until the test ends.
func NewMQTTBroker(t testing.TB, password string) *MQTTBroker {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &MQTTBroker{Password: password, ln: ln, changed: make(chan struct{}, 1)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b.mu.Lock()
			b.conns = append(b.conns, c)
			b.mu.Unlock()
			go b.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close(); b.DropAll() })
	return b
}

// URL is the broker's mqtt:// URL.
func (b *MQTTBroker) URL() string { return "mqtt://" + b.ln.Addr().String() }

// Addr is the broker's address.
func (b *MQTTBroker) Addr() string { return b.ln.Addr().String() }

// DropAll closes every client connection, as a broker restart.
func (b *MQTTBroker) DropAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.conns {
		c.Close()
	}
	b.conns = nil
}

// Connects returns the CONNECTs so far.
func (b *MQTTBroker) Connects() []MQTTConnect {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]MQTTConnect(nil), b.connects...)
}

// Messages returns the messages so far.
func (b *MQTTBroker) Messages() []MQTTMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]MQTTMessage(nil), b.messages...)
}

// WaitMessages waits for n messages.
func (b *MQTTBroker) WaitMessages(t testing.TB, n int) []MQTTMessage {
	t.Helper()
	waitFor(t, b.changed, func() bool { return len(b.Messages()) >= n }, "broker messages")
	return b.Messages()
}

// WaitConnects waits for n connections.
func (b *MQTTBroker) WaitConnects(t testing.TB, n int) []MQTTConnect {
	t.Helper()
	waitFor(t, b.changed, func() bool { return len(b.Connects()) >= n }, "broker connections")
	return b.Connects()
}

func (b *MQTTBroker) notify() {
	select {
	case b.changed <- struct{}{}:
	default:
	}
}

func mqttString(p []byte) (string, []byte, error) {
	if len(p) < 2 || len(p) < 2+int(binary.BigEndian.Uint16(p)) {
		return "", nil, errors.New("short string")
	}
	n := int(binary.BigEndian.Uint16(p))
	return string(p[2 : 2+n]), p[2+n:], nil
}

func mqttWrite(w io.Writer, header byte, body []byte) {
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
	_, _ = w.Write(append(buf, body...))
}

func mqttRead(r *bufio.Reader) (byte, []byte, error) {
	typ, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	n, mult := 0, 1
	for i := 0; i < 4; i++ {
		d, err := r.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		n += int(d&0x7f) * mult
		if d&0x80 == 0 {
			break
		}
		mult *= 128
	}
	body := make([]byte, n)
	_, err = io.ReadFull(r, body)
	return typ, body, err
}

func (b *MQTTBroker) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		typ, body, err := mqttRead(r)
		if err != nil {
			return
		}
		switch typ >> 4 {
		case 1: // CONNECT
			_, rest, err := mqttString(body) // "MQTT"
			if err != nil || len(rest) < 4 {
				return
			}
			flags := rest[1]
			info := MQTTConnect{KeepAlive: int(binary.BigEndian.Uint16(rest[2:4]))}
			rest = rest[4:]
			info.ClientID, rest, _ = mqttString(rest)
			if flags&0x04 != 0 {
				w := &MQTTMessage{QoS: flags >> 3 & 3, Retain: flags&0x20 != 0}
				var payload string
				w.Topic, rest, _ = mqttString(rest)
				payload, rest, _ = mqttString(rest)
				w.Payload = []byte(payload)
				info.Will = w
			}
			if flags&0x80 != 0 {
				info.Username, rest, _ = mqttString(rest)
			}
			if flags&0x40 != 0 {
				info.Password, _, _ = mqttString(rest)
			}
			b.mu.Lock()
			b.connects = append(b.connects, info)
			b.mu.Unlock()
			b.notify()
			code := byte(0)
			if b.Password != "" && info.Password != b.Password {
				code = 4
			}
			mqttWrite(c, 2<<4, []byte{0, code})
			if code != 0 {
				return
			}
		case 3: // PUBLISH
			qos := typ >> 1 & 3
			topic, rest, err := mqttString(body)
			if err != nil {
				return
			}
			var id []byte
			if qos > 0 {
				id, rest = rest[:2], rest[2:]
			}
			b.mu.Lock()
			b.messages = append(b.messages, MQTTMessage{Topic: topic, Payload: append([]byte(nil), rest...), QoS: qos, Retain: typ&1 != 0})
			b.mu.Unlock()
			b.notify()
			switch qos {
			case 1:
				mqttWrite(c, 4<<4, id)
			case 2:
				mqttWrite(c, 5<<4, id)
			}
		case 6: // PUBREL
			mqttWrite(c, 7<<4, body)
		case 12: // PINGREQ
			mqttWrite(c, 13<<4, nil)
		case 14: // DISCONNECT
			return
		}
	}
}

// --- FTP ---

// FTPServer is a minimal FTP server: the user cam with password secret,
// home /home, passive mode only, uploads kept in memory by path. With
// NoEPSV it answers 500 to EPSV, as old servers do, and the client falls
// back to PASV.
type FTPServer struct {
	NoEPSV bool

	ln      net.Listener
	mu      sync.Mutex
	dirs    map[string]bool
	files   map[string][]byte
	log     []string
	changed chan struct{}
}

// NewFTPServer listens on 127.0.0.1 until the test ends.
func NewFTPServer(t testing.TB) *FTPServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &FTPServer{ln: ln, dirs: map[string]bool{"/": true, "/home": true}, files: map[string][]byte{}, changed: make(chan struct{}, 1)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

// Addr is the server's address.
func (f *FTPServer) Addr() string { return f.ln.Addr().String() }

// File returns an uploaded file by its absolute path.
func (f *FTPServer) File(p string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[p]
	return b, ok
}

// Files returns the paths of the uploaded files.
func (f *FTPServer) Files() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for p := range f.files {
		out = append(out, p)
	}
	return out
}

// WaitFiles waits for n uploaded files.
func (f *FTPServer) WaitFiles(t testing.TB, n int) []string {
	t.Helper()
	waitFor(t, f.changed, func() bool { return len(f.Files()) >= n }, "uploaded files")
	return f.Files()
}

// Log returns the commands the server got.
func (f *FTPServer) Log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *FTPServer) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(format string, args ...any) { fmt.Fprintf(c, format+"\r\n", args...) }
	say("220-Welcome\r\n220 fake FTP")
	cwd, user, logged := "/home", "", false
	var data net.Listener
	defer func() {
		if data != nil {
			data.Close()
		}
	}()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd, arg, _ := strings.Cut(line, " ")
		cmd = strings.ToUpper(cmd)
		f.mu.Lock()
		f.log = append(f.log, line)
		f.mu.Unlock()
		abs := func(p string) string {
			if strings.HasPrefix(p, "/") {
				return path.Clean(p)
			}
			return path.Join(cwd, p)
		}
		switch {
		case cmd == "USER":
			user = arg
			say("331 password please")
		case cmd == "PASS":
			if user == "cam" && arg == "secret" {
				logged = true
				say("230 logged in")
			} else {
				say("530 login incorrect")
			}
		case !logged:
			say("530 please login")
		case cmd == "TYPE":
			say("200 binary")
		case cmd == "CWD":
			f.mu.Lock()
			ok := f.dirs[abs(arg)]
			f.mu.Unlock()
			if !ok {
				say("550 no such directory")
				continue
			}
			cwd = abs(arg)
			say("250 ok")
		case cmd == "MKD":
			f.mu.Lock()
			f.dirs[abs(arg)] = true
			f.mu.Unlock()
			say(`257 "%s" created`, abs(arg))
		case cmd == "EPSV" && f.NoEPSV:
			say("500 unknown command")
		case cmd == "EPSV" || cmd == "PASV":
			if data != nil {
				data.Close()
			}
			data, _ = net.Listen("tcp", "127.0.0.1:0")
			port := data.Addr().(*net.TCPAddr).Port
			if cmd == "EPSV" {
				say("229 Entering Extended Passive Mode (|||%d|)", port)
			} else {
				// A server behind NAT announces a private address: the
				// client keeps the control connection's host.
				say("227 Entering Passive Mode (192,168,99,1,%d,%d)", port>>8, port&0xff)
			}
		case cmd == "STOR":
			if data == nil {
				say("425 use PASV first")
				continue
			}
			say("150 ok to send")
			dc, err := data.Accept()
			data.Close()
			data = nil
			if err != nil {
				say("425 no data connection")
				continue
			}
			body, _ := io.ReadAll(dc)
			dc.Close()
			f.mu.Lock()
			f.files[abs(arg)] = body
			f.mu.Unlock()
			select {
			case f.changed <- struct{}{}:
			default:
			}
			say("226 transfer complete")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("502 not implemented")
		}
	}
}

// --- SMTP ---

// Mail is a message a mail server got, with its envelope.
type Mail struct {
	From, User, Helo string
	To               []string
	Data             []byte
	TLS              bool
}

// SMTPServer is a mail server: STARTTLS with a self-signed certificate
// when TLS is set, AUTH PLAIN and LOGIN for cam/secret, and recipients
// starting with nobody@ refused.
type SMTPServer struct {
	ln      net.Listener
	tls     *tls.Config
	mu      sync.Mutex
	mails   []Mail
	log     []string
	changed chan struct{}
}

// NewSMTPServer listens on 127.0.0.1 until the test ends; withTLS offers
// STARTTLS.
func NewSMTPServer(t testing.TB, withTLS bool) *SMTPServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &SMTPServer{ln: ln, changed: make(chan struct{}, 1)}
	if withTLS {
		s.tls = selfSigned(t)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

// Addr is the server's address.
func (s *SMTPServer) Addr() string { return s.ln.Addr().String() }

// Mails returns the messages so far.
func (s *SMTPServer) Mails() []Mail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Mail(nil), s.mails...)
}

// WaitMails waits for n messages.
func (s *SMTPServer) WaitMails(t testing.TB, n int) []Mail {
	t.Helper()
	waitFor(t, s.changed, func() bool { return len(s.Mails()) >= n }, "mails")
	return s.Mails()
}

// Log returns the commands the server got.
func (s *SMTPServer) Log() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.log...)
}

func selfSigned(t testing.TB) *tls.Config {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mail.lab"}, DNSNames: []string{"mail.lab"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

func (s *SMTPServer) serve(c net.Conn) {
	defer func() { c.Close() }()
	r := bufio.NewReader(c)
	say := func(line string) { fmt.Fprintf(c, "%s\r\n", line) }
	say("220 mail.lab ESMTP fake")
	var m Mail
	read := func() (string, bool) {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", false
		}
		line = strings.TrimRight(line, "\r\n")
		s.mu.Lock()
		s.log = append(s.log, line)
		s.mu.Unlock()
		return line, true
	}
	reset := func() { m = Mail{TLS: m.TLS, User: m.User, Helo: m.Helo} }
	for {
		line, ok := read()
		if !ok {
			return
		}
		cmd, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(cmd) {
		case "EHLO":
			m.Helo = arg
			say("250-mail.lab")
			if s.tls != nil && !m.TLS {
				say("250-STARTTLS")
			}
			say("250-AUTH PLAIN LOGIN")
			say("250 8BITMIME")
		case "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(c, s.tls)
			if tc.Handshake() != nil {
				return
			}
			c, r, m.TLS = tc, bufio.NewReader(tc), true
		case "AUTH":
			mech, rest, _ := strings.Cut(arg, " ")
			var user, pw string
			if mech == "PLAIN" {
				raw, _ := base64.StdEncoding.DecodeString(rest)
				if parts := strings.Split(string(raw), "\x00"); len(parts) == 3 {
					user, pw = parts[1], parts[2]
				}
			} else {
				say("334 VXNlcm5hbWU6")
				l, _ := read()
				u, _ := base64.StdEncoding.DecodeString(l)
				say("334 UGFzc3dvcmQ6")
				l, _ = read()
				p, _ := base64.StdEncoding.DecodeString(l)
				user, pw = string(u), string(p)
			}
			if user != "cam" || pw != "secret" {
				say("535 5.7.8 authentication credentials invalid")
				continue
			}
			m.User = user
			say("235 2.7.0 accepted")
		case "MAIL":
			m.From = strings.Trim(strings.TrimPrefix(arg, "FROM:"), "<>")
			say("250 ok")
		case "RCPT":
			to := strings.Trim(strings.TrimPrefix(arg, "TO:"), "<>")
			if strings.HasPrefix(to, "nobody@") {
				say("550 5.1.1 no such user")
				continue
			}
			m.To = append(m.To, to)
			say("250 ok")
		case "DATA":
			say("354 end with .")
			var b bytes.Buffer
			for {
				l, ok := read()
				if !ok {
					return
				}
				if l == "." {
					break
				}
				b.WriteString(strings.TrimPrefix(l, ".") + "\r\n")
			}
			m.Data = b.Bytes()
			s.mu.Lock()
			s.mails = append(s.mails, m)
			s.mu.Unlock()
			select {
			case s.changed <- struct{}{}:
			default:
			}
			reset()
			say("250 queued")
		case "RSET":
			reset()
			say("250 ok")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("502 unknown")
		}
	}
}

func waitFor(t testing.TB, changed chan struct{}, done func() bool, what string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !done() {
		select {
		case <-changed:
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}
