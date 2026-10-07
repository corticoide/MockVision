package ftpupload

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/corticoide/mockvision/backend/internal/engines/delivery"
)

// ftpClient is a control connection to an FTP server (RFC 959). The camera
// only uploads, in passive mode (EPSV, else PASV) and binary: it cannot
// listen for the active mode's data connection, as many cameras behind NAT.
type ftpClient struct {
	conn net.Conn
	text *textproto.Conn
	host string
}

// ftpError is a negative reply of the server.
type ftpError struct {
	Code int
	Msg  string
}

func (e *ftpError) Error() string { return fmt.Sprintf("%d %s", e.Code, e.Msg) }

// dialFTP connects and logs in within ctx.
func dialFTP(ctx context.Context, hostport, user, password string) (*ftpClient, error) {
	conn, err := delivery.Dial(ctx, "tcp", hostport)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	host, _, _ := net.SplitHostPort(hostport)
	c := &ftpClient{conn: conn, text: textproto.NewConn(conn), host: host}
	if _, _, err := c.text.ReadResponse(220); err != nil {
		conn.Close()
		return nil, replyError(err)
	}
	if user == "" {
		user, password = "anonymous", "anonymous@"
	}
	code, _, err := c.cmd("USER " + user)
	if err == nil && code == 331 {
		code, _, err = c.cmd("PASS " + password)
	}
	if err == nil && code != 230 && code != 202 {
		err = fmt.Errorf("login refused (%d)", code)
	}
	if err == nil {
		_, err = c.expect(200, "TYPE I")
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// cmd sends a command and reads the reply, whatever its code; an error is
// a broken connection or a 4xx or 5xx reply.
func (c *ftpClient) cmd(line string) (int, string, error) {
	if strings.ContainsAny(line, "\r\n") {
		return 0, "", fmt.Errorf("invalid FTP command")
	}
	id, err := c.text.Cmd("%s", line)
	if err != nil {
		return 0, "", err
	}
	c.text.StartResponse(id)
	defer c.text.EndResponse(id)
	code, msg, err := c.text.ReadResponse(0)
	if err != nil {
		return code, msg, replyError(err)
	}
	if code >= 400 {
		return code, msg, &ftpError{code, msg}
	}
	return code, msg, nil
}

// expect sends a command that must answer code, and returns the reply's
// text.
func (c *ftpClient) expect(code int, line string) (string, error) {
	got, msg, err := c.cmd(line)
	if err == nil && got != code {
		err = &ftpError{got, msg}
	}
	return msg, err
}

func replyError(err error) error {
	if te, ok := err.(*textproto.Error); ok {
		return &ftpError{te.Code, te.Msg}
	}
	return err
}

// chdirAll enters dir one segment at a time, creating what is missing.
func (c *ftpClient) chdirAll(dir string) error {
	if strings.HasPrefix(dir, "/") {
		if _, err := c.expect(250, "CWD /"); err != nil {
			return err
		}
	}
	for _, seg := range strings.Split(dir, "/") {
		if seg == "" {
			continue
		}
		if _, err := c.expect(250, "CWD "+seg); err == nil {
			continue
		}
		if _, err := c.expect(257, "MKD "+seg); err != nil {
			return fmt.Errorf("cannot create %s: %w", seg, err)
		}
		if _, err := c.expect(250, "CWD "+seg); err != nil {
			return err
		}
	}
	return nil
}

var pasvAddr = regexp.MustCompile(`(\d+),(\d+),(\d+),(\d+),(\d+),(\d+)`)

// dataConn opens a passive data connection: EPSV, else PASV. With PASV
// the server's address is ignored for its port alone, as clients do since
// servers behind NAT announce private addresses.
func (c *ftpClient) dataConn(ctx context.Context) (net.Conn, error) {
	var port int
	if code, msg, err := c.cmd("EPSV"); err == nil && code == 229 {
		// 229 Entering Extended Passive Mode (|||port|)
		if i := strings.Index(msg, "(|||"); i >= 0 {
			rest := msg[i+4:]
			if j := strings.Index(rest, "|"); j > 0 {
				port, _ = strconv.Atoi(rest[:j])
			}
		}
	}
	if port == 0 {
		msg, err := c.expect(227, "PASV")
		if err != nil {
			return nil, err
		}
		m := pasvAddr.FindStringSubmatch(msg)
		if m == nil {
			return nil, fmt.Errorf("unexpected PASV reply %q", msg)
		}
		hi, _ := strconv.Atoi(m[5])
		lo, _ := strconv.Atoi(m[6])
		port = hi<<8 | lo
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("the server gave data port %d", port)
	}
	return delivery.Dial(ctx, "tcp", net.JoinHostPort(c.host, strconv.Itoa(port)))
}

// store uploads data as name in the current directory.
func (c *ftpClient) store(ctx context.Context, name string, data []byte) error {
	dc, err := c.dataConn(ctx)
	if err != nil {
		return err
	}
	defer dc.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = dc.SetDeadline(dl)
	}
	code, msg, err := c.cmd("STOR " + name)
	if err != nil {
		return err
	}
	if code != 125 && code != 150 {
		return &ftpError{code, msg}
	}
	if _, err := io.Copy(dc, bytes.NewReader(data)); err != nil {
		return err
	}
	if err := dc.Close(); err != nil {
		return err
	}
	code, msg, err = c.text.ReadResponse(0)
	if err != nil {
		return replyError(err)
	}
	if code != 226 && code != 250 {
		return &ftpError{code, msg}
	}
	return nil
}

// Close says QUIT and closes the connection.
func (c *ftpClient) Close() {
	_ = c.conn.SetDeadline(time.Now().Add(time.Second))
	_, _, _ = c.cmd("QUIT")
	c.conn.Close()
}
