package nas

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
)

// NFS version 3 (RFC 1813), the procedures a camera recording needs, with
// MOUNT version 3 and the portmapper (RFC 1833) to find them.

const (
	pmapProg    = 100000
	pmapVers    = 2
	pmapGetport = 3
	pmapPort    = 111
	protoTCP    = 6

	mountProg = 100005
	mountVers = 3
	mountMnt  = 1

	nfsProg    = 100003
	nfsVers    = 3
	nfsPort    = 2049
	nfsLookup  = 3
	nfsRead    = 6
	nfsWrite   = 7
	nfsCreate  = 8
	nfsMkdir   = 9
	fileSync   = 2
	unchecked  = 0
	fhMax      = 64
	fattrBytes = 84
	// maxIO is the size of a READ or WRITE; every server takes 32 KiB.
	maxIO = 32 << 10
)

var nfsErrors = map[uint32]string{
	1: "not owner", 2: "no such file or directory", 5: "I/O error", 13: "permission denied", 17: "file exists",
	20: "not a directory", 21: "is a directory", 22: "invalid argument", 27: "file too large", 28: "no space left on the share",
	30: "read-only share", 63: "name too long", 69: "quota exceeded", 70: "stale file handle", 10001: "bad file handle",
	10004: "operation not supported", 10006: "server fault", 10008: "the server is busy",
}

// nfsError is a failed NFS procedure.
type nfsError struct{ status uint32 }

func (e *nfsError) Error() string {
	if msg, ok := nfsErrors[e.status]; ok {
		return "nfs: " + msg
	}
	return fmt.Sprintf("nfs: error %d", e.status)
}

func (e *nfsError) Is(target error) bool { return target == ErrNotExist && e.status == 2 }

var mountErrors = map[uint32]string{
	1: "not owner", 2: "no such export", 5: "I/O error",
	13: "access denied: the export must allow this camera's address, and ports above 1023 (the insecure option)",
	20: "not a directory", 22: "invalid argument", 63: "name too long", 10004: "operation not supported", 10006: "server fault",
}

type nfsShare struct {
	c    *rpcConn
	root []byte

	mu   sync.Mutex
	dirs map[string][]byte // handles of the directories made or found
}

func openNFS(ctx context.Context, u *url.URL, o Options) (Share, error) {
	host := u.Hostname()
	export := "/" + strings.Trim(u.Path, "/")
	q := u.Query()
	uid, err1 := strconv.ParseUint(cmpOr(q.Get("uid"), "0"), 10, 32)
	gid, err2 := strconv.ParseUint(cmpOr(q.Get("gid"), "0"), 10, 32)
	if err1 != nil || err2 != nil {
		return nil, errors.New("nfs: uid and gid must be numbers")
	}
	machine := o.Machine
	if machine == "" {
		machine = "camera"
	}
	cred := credentials(machine, uint32(uid), uint32(gid))
	mountPort, filePort := 0, 0
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, errors.New("nfs: invalid port")
		}
		mountPort, filePort = n, n
	} else {
		pm, err := dialRPC(ctx, o.Dial, net.JoinHostPort(host, strconv.Itoa(pmapPort)), pmapProg, pmapVers, nil)
		if err != nil {
			return nil, fmt.Errorf("nfs: portmapper: %w", err)
		}
		mountPort, err = getport(ctx, pm, mountProg, mountVers)
		if err == nil {
			filePort, err = getport(ctx, pm, nfsProg, nfsVers)
		}
		pm.Close()
		if err != nil {
			return nil, err
		}
		if mountPort == 0 {
			return nil, errors.New("nfs: the server runs no mount service")
		}
		if filePort == 0 {
			filePort = nfsPort
		}
	}
	m, err := dialRPC(ctx, o.Dial, net.JoinHostPort(host, strconv.Itoa(mountPort)), mountProg, mountVers, cred)
	if err != nil {
		return nil, fmt.Errorf("nfs: mount: %w", err)
	}
	var e encoder
	e.string(export)
	d, err := m.call(ctx, mountMnt, e.bytes())
	m.Close()
	if err != nil {
		return nil, fmt.Errorf("nfs: mount %s: %w", export, err)
	}
	if st := d.uint32(); st != 0 {
		msg, ok := mountErrors[st]
		if !ok {
			msg = fmt.Sprintf("error %d", st)
		}
		return nil, fmt.Errorf("nfs: mount %s: %s", export, msg)
	}
	root := d.opaque(fhMax)
	if d.err != nil {
		return nil, d.err
	}
	c, err := dialRPC(ctx, o.Dial, net.JoinHostPort(host, strconv.Itoa(filePort)), nfsProg, nfsVers, cred)
	if err != nil {
		return nil, fmt.Errorf("nfs: %w", err)
	}
	return &nfsShare{c: c, root: append([]byte(nil), root...), dirs: map[string][]byte{}}, nil
}

func cmpOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func getport(ctx context.Context, pm *rpcConn, prog, vers uint32) (int, error) {
	var e encoder
	e.uint32(prog)
	e.uint32(vers)
	e.uint32(protoTCP)
	e.uint32(0)
	d, err := pm.call(ctx, pmapGetport, e.bytes())
	if err != nil {
		return 0, fmt.Errorf("nfs: portmapper: %w", err)
	}
	port := d.uint32()
	return int(port), d.err
}

func (s *nfsShare) Close() error { return s.c.Close() }

// status reads a reply's status; past an error only attributes follow.
func status(d *decoder) error {
	if st := d.uint32(); st != 0 {
		return &nfsError{st}
	}
	return d.err
}

// postOpAttr reads attributes that may follow, and the size they give.
func postOpAttr(d *decoder) (int64, bool) {
	if !d.bool() {
		return 0, false
	}
	b := d.take(fattrBytes)
	if b == nil {
		return 0, false
	}
	sub := &decoder{b: b[20:28]}
	return int64(sub.uint64()), true
}

func wccData(d *decoder) {
	if d.bool() { // pre-operation attributes: size and two times
		d.skip(24)
	}
	postOpAttr(d)
}

// lookup finds a name in a directory.
func (s *nfsShare) lookup(ctx context.Context, dir []byte, name string) ([]byte, int64, error) {
	var e encoder
	e.opaque(dir)
	e.string(name)
	d, err := s.c.call(ctx, nfsLookup, e.bytes())
	if err != nil {
		return nil, 0, err
	}
	if err := status(d); err != nil {
		return nil, 0, err
	}
	fh := d.opaque(fhMax)
	size, _ := postOpAttr(d)
	return append([]byte(nil), fh...), size, d.err
}

func sattr(e *encoder, mode uint32, truncate bool) {
	e.bool(true) // mode
	e.uint32(mode)
	e.bool(false) // uid
	e.bool(false) // gid
	e.bool(truncate)
	if truncate {
		e.uint64(0)
	}
	e.uint32(0) // atime and mtime: as the server likes
	e.uint32(0)
}

// made reads what CREATE and MKDIR return: the new handle, when the server
// gives it.
func made(d *decoder) ([]byte, error) {
	if err := status(d); err != nil {
		return nil, err
	}
	var fh []byte
	if d.bool() {
		fh = append([]byte(nil), d.opaque(fhMax)...)
	}
	postOpAttr(d)
	wccData(d)
	return fh, d.err
}

func (s *nfsShare) mkdir(ctx context.Context, dir []byte, name string) ([]byte, error) {
	var e encoder
	e.opaque(dir)
	e.string(name)
	sattr(&e, 0o755, false)
	d, err := s.c.call(ctx, nfsMkdir, e.bytes())
	if err != nil {
		return nil, err
	}
	fh, err := made(d)
	var ne *nfsError
	if (errors.As(err, &ne) && ne.status == 17) || (err == nil && fh == nil) {
		fh, _, err = s.lookup(ctx, dir, name)
	}
	return fh, err
}

// dir returns the handle of a directory under the root, making what is
// missing when asked.
func (s *nfsShare) dir(ctx context.Context, p string, create bool) ([]byte, error) {
	if p == "." || p == "" {
		return s.root, nil
	}
	s.mu.Lock()
	fh, ok := s.dirs[p]
	s.mu.Unlock()
	if ok {
		return fh, nil
	}
	parent, err := s.dir(ctx, path.Dir(p), create)
	if err != nil {
		return nil, err
	}
	base := path.Base(p)
	fh, _, err = s.lookup(ctx, parent, base)
	if errors.Is(err, ErrNotExist) && create {
		fh, err = s.mkdir(ctx, parent, base)
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.dirs[p] = fh
	s.mu.Unlock()
	return fh, nil
}

func (s *nfsShare) WriteFile(ctx context.Context, name string, data []byte) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	dir, err := s.dir(ctx, path.Dir(name), true)
	if err != nil {
		return err
	}
	var e encoder
	e.opaque(dir)
	e.string(path.Base(name))
	e.uint32(unchecked)
	sattr(&e, 0o644, true)
	d, err := s.c.call(ctx, nfsCreate, e.bytes())
	if err != nil {
		return err
	}
	fh, err := made(d)
	if err == nil && fh == nil {
		fh, _, err = s.lookup(ctx, dir, path.Base(name))
	}
	if err != nil {
		return err
	}
	for off := 0; off < len(data); {
		chunk := data[off:min(off+maxIO, len(data))]
		var e encoder
		e.opaque(fh)
		e.uint64(uint64(off))
		e.uint32(uint32(len(chunk)))
		e.uint32(fileSync)
		e.opaque(chunk)
		d, err := s.c.call(ctx, nfsWrite, e.bytes())
		if err != nil {
			return err
		}
		if err := status(d); err != nil {
			return err
		}
		wccData(d)
		n := int(d.uint32())
		if d.err != nil {
			return d.err
		}
		if n <= 0 || n > len(chunk) {
			return errors.New("nfs: the server wrote nothing")
		}
		off += n
	}
	return nil
}

func (s *nfsShare) ReadAt(ctx context.Context, name string, p []byte, off int64) (int, int64, error) {
	name, err := cleanName(name)
	if err != nil {
		return 0, 0, err
	}
	dir, err := s.dir(ctx, path.Dir(name), false)
	if err != nil {
		return 0, 0, err
	}
	fh, size, err := s.lookup(ctx, dir, path.Base(name))
	if err != nil {
		return 0, 0, err
	}
	n := 0
	for n < len(p) && off+int64(n) < size {
		var e encoder
		e.opaque(fh)
		e.uint64(uint64(off) + uint64(n))
		e.uint32(uint32(min(len(p)-n, maxIO)))
		d, err := s.c.call(ctx, nfsRead, e.bytes())
		if err != nil {
			return n, size, err
		}
		if err := status(d); err != nil {
			return n, size, err
		}
		if sz, ok := postOpAttr(d); ok {
			size = sz
		}
		d.uint32() // count
		eof := d.bool()
		data := d.opaque(maxIO)
		if d.err != nil {
			return n, size, d.err
		}
		n += copy(p[n:], data)
		if eof || len(data) == 0 {
			break
		}
	}
	return n, size, nil
}
