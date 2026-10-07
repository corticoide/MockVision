// Package nas writes and reads files on a network share, as a camera
// records to a NAS: NFS version 3 over TCP, or SMB 2 and 3 (feature 10).
package nas

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"
)

// Share is a directory of a network share.
type Share interface {
	// WriteFile writes a whole file, creating its directories; name is a
	// slash path inside the share's directory.
	WriteFile(ctx context.Context, name string, data []byte) error
	// ReadAt reads part of a file and says how large it is.
	ReadAt(ctx context.Context, name string, p []byte, off int64) (n int, size int64, err error)
	Close() error
}

// Dialer opens connections, as the camera's network allows them.
type Dialer func(ctx context.Context, network, address string) (net.Conn, error)

// ErrNotExist is returned for a file the share does not have.
var ErrNotExist = errors.New("no such file on the share")

// Options are who the camera is to the share.
type Options struct {
	Username string
	Password string
	// Machine is the host name NFS credentials carry.
	Machine string
	Dial    Dialer
}

// Open connects to a share: nfs://host[:port]/export[?uid=N&gid=N] mounts
// the export (one port for mount and NFS when given, else the
// portmapper's), smb://host[:port]/share[/dir] logs in with the username
// and password, DOMAIN\user for a domain account.
func Open(ctx context.Context, raw string, o Options) (Share, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("invalid share URL %q", raw)
	}
	if o.Dial == nil {
		var d net.Dialer
		o.Dial = d.DialContext
	}
	switch u.Scheme {
	case "nfs":
		return openNFS(ctx, u, o)
	case "smb":
		return openSMB(ctx, u, o)
	}
	return nil, fmt.Errorf("unknown share protocol %q", u.Scheme)
}

// cleanName checks a file name inside the share.
func cleanName(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || path.Clean(name) != name || strings.HasPrefix(name, "../") || name == ".." {
		return "", fmt.Errorf("invalid file name %q", name)
	}
	return name, nil
}
