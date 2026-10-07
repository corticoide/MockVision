package nas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"
)

// nfsServer serves a directory over NFS, mount and NFS on one port, as a
// third-party implementation to talk to.
func nfsServer(t *testing.T) (string, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	dir := t.TempDir()
	handler := nfshelper.NewCachingHandler(nfshelper.NewNullAuthHandler(osfs.New(dir)), 1024)
	go func() { _ = nfs.Serve(ln, handler) }()
	return ln.Addr().String(), dir
}

// roundTrip writes a file in a new directory, reads it back in pieces and
// misses another one.
func roundTrip(t *testing.T, s Share) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	data := bytes.Repeat([]byte("0123456789abcdef"), 6000) // 96000 bytes: several writes
	if err := s.WriteFile(ctx, "6C0012ABCDEF/20261007/143000_motion_x7k2pq.ts", data); err != nil {
		t.Fatal(err)
	}
	// Writing again replaces it, and its directory is there already.
	if err := s.WriteFile(ctx, "6C0012ABCDEF/20261007/143000_motion_x7k2pq.ts", data); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 0, len(data))
	buf := make([]byte, 40000)
	for off := int64(0); ; {
		n, size, err := s.ReadAt(ctx, "6C0012ABCDEF/20261007/143000_motion_x7k2pq.ts", buf, off)
		if err != nil {
			t.Fatal(err)
		}
		if size != int64(len(data)) {
			t.Fatalf("size %d, want %d", size, len(data))
		}
		got = append(got, buf[:n]...)
		off += int64(n)
		if n == 0 || off >= size {
			break
		}
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("read %d bytes back, not what was written", len(got))
	}
	if _, _, err := s.ReadAt(ctx, "6C0012ABCDEF/20261007/nope.jpg", buf, 0); !errors.Is(err, ErrNotExist) {
		t.Fatalf("a missing file: %v", err)
	}
	if err := s.WriteFile(ctx, "../escape.jpg", data); err == nil {
		t.Fatal("a name out of the share was accepted")
	}
	return data
}

func TestNFS(t *testing.T) {
	addr, dir := nfsServer(t)
	ctx := context.Background()
	s, err := Open(ctx, "nfs://"+addr+"/export?uid=1000&gid=1000", Options{Machine: "gate"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	data := roundTrip(t, s)
	on, err := os.ReadFile(filepath.Join(dir, "6C0012ABCDEF", "20261007", "143000_motion_x7k2pq.ts"))
	if err != nil || !bytes.Equal(on, data) {
		t.Fatalf("the server holds %d bytes: %v", len(on), err)
	}

	// No server: the error says where.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	if _, err := Open(ctx, "nfs://"+dead+"/export", Options{}); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("no server: %v", err)
	}
	if _, err := Open(ctx, "nfs://"+addr+"/export?uid=x", Options{}); err == nil {
		t.Fatal("a uid that is not a number was accepted")
	}
}

// A canceled call does not hang on a server that does not answer.
func TestNFSTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // reads nothing, answers nothing
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Open(ctx, "nfs://"+ln.Addr().String()+"/export", Options{}); err == nil {
		t.Fatal("a silent server mounted")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
}

// TestSMB talks to Samba, when the machine has it and may run it.
func TestSMB(t *testing.T) {
	smbd, err := exec.LookPath("smbd")
	if err != nil {
		t.Skip("smbd not installed")
	}
	if os.Geteuid() != 0 {
		t.Skip("smbd needs root")
	}
	dir := t.TempDir()
	share := filepath.Join(dir, "share")
	for _, d := range []string{share, filepath.Join(dir, "private"), filepath.Join(dir, "lock"), filepath.Join(dir, "state"), filepath.Join(dir, "cache")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	conf := filepath.Join(dir, "smb.conf")
	if err := os.WriteFile(conf, []byte(fmt.Sprintf(`[global]
smb ports = %d
interfaces = 127.0.0.1
bind interfaces only = yes
private dir = %[2]s/private
lock directory = %[2]s/lock
state directory = %[2]s/state
cache directory = %[2]s/cache
pid directory = %[2]s
ncalrpc dir = %[2]s/ncalrpc
binddns dir = %[2]s/binddns
log file = %[2]s/log.smbd
passdb backend = tdbsam:%[2]s/private/passdb.tdb
disable netbios = yes
server role = standalone server
[cams]
path = %[3]s
read only = no
valid users = root
`, port, dir, share)), 0o644); err != nil {
		t.Fatal(err)
	}
	pw := exec.Command("smbpasswd", "-c", conf, "-a", "-s", "root")
	pw.Stdin = strings.NewReader("s3cret\ns3cret\n")
	if out, err := pw.CombinedOutput(); err != nil {
		t.Skipf("smbpasswd: %v %s", err, out)
	}
	cmd := exec.Command(smbd, "--foreground", "--no-process-group", "--debug-stdout", "-s", conf)
	if err := cmd.Start(); err != nil {
		t.Skipf("smbd: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Skip("smbd did not listen")
		}
		time.Sleep(100 * time.Millisecond)
	}
	ctx := context.Background()
	if _, err := Open(ctx, "smb://"+addr+"/cams", Options{Username: "root", Password: "wrong"}); err == nil {
		t.Fatal("a wrong password logged in")
	}
	s, err := Open(ctx, "smb://"+addr+"/cams/recordings", Options{Username: "root", Password: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	data := roundTrip(t, s)
	on, err := os.ReadFile(filepath.Join(share, "recordings", "6C0012ABCDEF", "20261007", "143000_motion_x7k2pq.ts"))
	if err != nil || !bytes.Equal(on, data) {
		t.Fatalf("the share holds %d bytes: %v", len(on), err)
	}
}
