package smtpmail

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/corticoide/mockvision/backend/internal/engines/delivery"
	"github.com/corticoide/mockvision/sdk/engine"
)

// TLS modes of an smtp target.
const (
	TLSNone     = "none"
	TLSStartTLS = "starttls"
	TLSImplicit = "tls"
)

// Address returns the host and port of an smtp target, with the default
// port of its TLS mode: 465 for implicit TLS, 25 otherwise.
func Address(t engine.Target) (string, error) {
	u, err := url.Parse(t.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "smtp" && u.Scheme != "smtps") {
		return "", fmt.Errorf("URL must look like smtp://host:25")
	}
	port := "25"
	if mode(t) == TLSImplicit {
		port = "465"
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("invalid port %q", p)
		}
		port = p
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

// mode is the TLS mode of a target; smtps:// means implicit TLS.
func mode(t engine.Target) string {
	if t.TLS != "" {
		return t.TLS
	}
	if strings.HasPrefix(t.URL, "smtps:") {
		return TLSImplicit
	}
	return TLSNone
}

// smtpError is a negative reply of the server.
type smtpError struct {
	Code int
	Msg  string
}

func (e *smtpError) Error() string { return fmt.Sprintf("%d %s", e.Code, e.Msg) }

// client is a session with a mail server (RFC 5321).
type client struct {
	conn net.Conn
	text *textproto.Conn
	host string
	ext  map[string]string
}

// dial connects, greets with helo and, as the target asks, switches to TLS
// and authenticates, all within ctx.
func dial(ctx context.Context, t engine.Target, helo string) (*client, error) {
	addr, err := Address(t)
	if err != nil {
		return nil, err
	}
	conn, err := delivery.Dial(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	host, _, _ := net.SplitHostPort(addr)
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	m := mode(t)
	if m == TLSImplicit {
		tc := tls.Client(conn, delivery.TLSConfig(host, t.Insecure))
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		conn = tc
	}
	c := &client{conn: conn, text: textproto.NewConn(conn), host: host}
	if _, _, err := c.text.ReadResponse(220); err != nil {
		conn.Close()
		return nil, replyError(err)
	}
	if err := c.hello(helo); err != nil {
		c.close()
		return nil, err
	}
	if m == TLSStartTLS {
		if _, ok := c.ext["STARTTLS"]; !ok {
			c.close()
			return nil, errors.New("the server does not offer STARTTLS")
		}
		if _, _, err := c.cmd(220, "STARTTLS"); err != nil {
			c.close()
			return nil, err
		}
		tc := tls.Client(conn, delivery.TLSConfig(host, t.Insecure))
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		c.conn, c.text = tc, textproto.NewConn(tc)
		if err := c.hello(helo); err != nil {
			c.close()
			return nil, err
		}
	}
	if t.Username != "" {
		if err := c.auth(t.Username, t.Password); err != nil {
			c.close()
			return nil, err
		}
	}
	return c, nil
}

// hello sends EHLO, or HELO to a server that does not know it, and reads
// the extensions.
func (c *client) hello(name string) error {
	code, msg, err := c.cmd(250, "EHLO "+name)
	if err != nil {
		if code >= 500 {
			_, _, err = c.cmd(250, "HELO "+name)
		}
		c.ext = map[string]string{}
		return err
	}
	c.ext = map[string]string{}
	lines := strings.Split(msg, "\n")
	for _, l := range lines[1:] {
		k, v, _ := strings.Cut(strings.TrimSpace(l), " ")
		c.ext[strings.ToUpper(k)] = v
	}
	return nil
}

// auth logs in with PLAIN or LOGIN, the mechanisms cameras use.
func (c *client) auth(user, password string) error {
	mechs, ok := c.ext["AUTH"]
	if !ok {
		return errors.New("the server does not offer authentication")
	}
	has := func(m string) bool {
		for _, x := range strings.Fields(strings.ToUpper(mechs)) {
			if x == m {
				return true
			}
		}
		return false
	}
	enc := base64.StdEncoding.EncodeToString
	switch {
	case has("PLAIN"):
		_, _, err := c.cmd(235, "AUTH PLAIN "+enc([]byte("\x00"+user+"\x00"+password)))
		return authError(err)
	case has("LOGIN"):
		if _, _, err := c.cmd(334, "AUTH LOGIN"); err != nil {
			return authError(err)
		}
		if _, _, err := c.cmd(334, enc([]byte(user))); err != nil {
			return authError(err)
		}
		_, _, err := c.cmd(235, enc([]byte(password)))
		return authError(err)
	}
	return fmt.Errorf("the server offers authentication by %s, the camera only PLAIN and LOGIN", mechs)
}

func authError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("authentication failed: %w", err)
}

func (c *client) cmd(expect int, line string) (int, string, error) {
	id, err := c.text.Cmd("%s", line)
	if err != nil {
		return 0, "", err
	}
	c.text.StartResponse(id)
	defer c.text.EndResponse(id)
	code, msg, err := c.text.ReadResponse(expect)
	return code, msg, replyError(err)
}

func replyError(err error) error {
	var te *textproto.Error
	if errors.As(err, &te) {
		return &smtpError{te.Code, te.Msg}
	}
	return err
}

// send delivers a message to its recipients.
func (c *client) send(from string, to []string, msg []byte) error {
	if _, _, err := c.cmd(250, "MAIL FROM:<"+from+">"); err != nil {
		return err
	}
	for _, rcpt := range to {
		code, msg, err := c.cmd(0, "RCPT TO:<"+rcpt+">")
		if err != nil {
			return err
		}
		if code != 250 && code != 251 {
			return fmt.Errorf("recipient %s refused: %d %s", rcpt, code, msg)
		}
	}
	if _, _, err := c.cmd(354, "DATA"); err != nil {
		return err
	}
	w := c.text.DotWriter()
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	_, _, err := c.text.ReadResponse(250)
	return replyError(err)
}

// verify checks the sender and recipients without sending anything: the
// connection test of the panel.
func (c *client) verify(from string, to []string) error {
	if _, _, err := c.cmd(250, "MAIL FROM:<"+from+">"); err != nil {
		return err
	}
	for _, rcpt := range to {
		code, msg, err := c.cmd(0, "RCPT TO:<"+rcpt+">")
		if err != nil {
			return err
		}
		if code != 250 && code != 251 {
			return fmt.Errorf("recipient %s refused: %d %s", rcpt, code, msg)
		}
	}
	_, _, err := c.cmd(250, "RSET")
	return err
}

func (c *client) close() {
	_ = c.conn.SetDeadline(time.Now().Add(time.Second))
	_, _, _ = c.cmd(221, "QUIT")
	c.conn.Close()
}

// heloName is how the camera names itself to mail servers: its address
// literal, as embedded devices without a domain do.
func heloName(ip string) string {
	if ip == "" {
		return "localhost"
	}
	if a := net.ParseIP(ip); a != nil {
		return "[" + a.String() + "]"
	}
	return ip
}
