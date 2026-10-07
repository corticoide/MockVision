package nas

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"path"
	"strings"

	"github.com/hirochachacha/go-smb2"
)

type smbShare struct {
	conn net.Conn
	sess *smb2.Session
	fs   *smb2.Share
	dir  string // inside the share, with backslashes
}

func openSMB(ctx context.Context, u *url.URL, o Options) (Share, error) {
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "445"
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if segs[0] == "" {
		return nil, errors.New("smb: the URL names no share")
	}
	if o.Username == "" {
		return nil, errors.New("smb: a username is required (guest for guest access)")
	}
	user, domain := o.Username, ""
	if d, name, ok := strings.Cut(o.Username, `\`); ok {
		domain, user = d, name
	}
	conn, err := o.Dial(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("smb: %w", err)
	}
	d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: user, Password: o.Password, Domain: domain}}
	sess, err := d.DialContext(ctx, conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("smb: %w", err)
	}
	share, err := sess.WithContext(ctx).Mount(fmt.Sprintf(`\\%s\%s`, host, segs[0]))
	if err != nil {
		_ = sess.Logoff()
		conn.Close()
		return nil, fmt.Errorf("smb: share %s: %w", segs[0], err)
	}
	return &smbShare{conn: conn, sess: sess, fs: share, dir: strings.Join(segs[1:], `\`)}, nil
}

// path is a file's path inside the share.
func (s *smbShare) path(name string) string {
	p := strings.ReplaceAll(name, "/", `\`)
	if s.dir != "" {
		p = s.dir + `\` + p
	}
	return p
}

func (s *smbShare) WriteFile(ctx context.Context, name string, data []byte) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	fsys := s.fs.WithContext(ctx)
	if dir := path.Dir(name); dir != "." || s.dir != "" {
		target := s.dir
		if dir != "." {
			target = s.path(dir)
		}
		if err := fsys.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("smb: %w", err)
		}
	}
	if err := fsys.WriteFile(s.path(name), data, 0o644); err != nil {
		return fmt.Errorf("smb: %w", err)
	}
	return nil
}

func (s *smbShare) ReadAt(ctx context.Context, name string, p []byte, off int64) (int, int64, error) {
	name, err := cleanName(name)
	if err != nil {
		return 0, 0, err
	}
	f, err := s.fs.WithContext(ctx).Open(s.path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, ErrNotExist
	}
	if err != nil {
		return 0, 0, fmt.Errorf("smb: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, 0, fmt.Errorf("smb: %w", err)
	}
	size := st.Size()
	if off >= size {
		return 0, size, nil
	}
	n, err := f.ReadAt(p[:min(int64(len(p)), size-off)], off)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, size, fmt.Errorf("smb: %w", err)
	}
	return n, size, nil
}

func (s *smbShare) Close() error {
	_ = s.fs.Umount()
	_ = s.sess.Logoff()
	return s.conn.Close()
}
