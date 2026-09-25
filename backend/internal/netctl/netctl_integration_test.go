//go:build linux && integration

// Integration tests for camera networking. They need root (or NET_ADMIN,
// NET_RAW and SYS_ADMIN) and run in CI with sudo:
//
//	go test -tags integration ./backend/internal/netctl/
package netctl

import (
	"bytes"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/corticoide/mockvision/backend/internal/domain"
)

func requireRoot(t *testing.T) netns.NsHandle {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	host, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.Close() })
	return host
}

// testParent creates an isolated parent NIC (one end of a veth pair);
// macvlan interfaces in bridge mode on it reach each other like devices on
// a switch.
func testParent(t *testing.T, name string) {
	t.Helper()
	v := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: name}, PeerName: name + "p"}
	if err := netlink.LinkAdd(v); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(v) })
	for _, n := range []string{name, name + "p"} {
		l, err := netlink.LinkByName(n)
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetUp(l); err != nil {
			t.Fatal(err)
		}
	}
}

func testSpec(id, ip string) *CameraSpec {
	return &CameraSpec{
		ID:     id,
		Netns:  "sim-itest-" + strings.ToLower(id[len(id)-4:]),
		Mode:   "macvlan",
		Parent: "mvtest0",
		MAC:    domain.DeriveMAC(id, nil).String(),
		IP:     ip,
		Prefix: 24,
	}
}

func TestMacvlanNamespaces(t *testing.T) {
	host := requireRoot(t)
	testParent(t, "mvtest0")

	specA := testSpec("01J8Z3QK00000000000000AAAA", "10.99.0.10")
	nsA, err := createNamespace(specA.Netns)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nsA.delete() })
	start := time.Now()
	if err := setupInterface(host, nsA, specA); err != nil {
		t.Fatal(err)
	}
	t.Logf("namespace %s ready in %s (named=%v)", nsA.name, time.Since(start).Round(time.Millisecond), nsA.named)

	// RN-06: a second camera with the same IP must see the first one.
	specDup := testSpec("01J8Z3QK00000000000000DDDD", "10.99.0.10")
	nsDup, err := createNamespace(specDup.Netns)
	if err != nil {
		t.Fatal(err)
	}
	err = setupInterface(host, nsDup, specDup)
	_ = nsDup.delete()
	var ne *Error
	if !errors.As(err, &ne) || ne.Code != CodeIPInUse {
		t.Fatalf("expected an ip_in_use error, got %v", err)
	}

	specB := testSpec("01J8Z3QK00000000000000BBBB", "10.99.0.11")
	nsB, err := createNamespace(specB.Netns)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nsB.delete() })
	if err := setupInterface(host, nsB, specB); err != nil {
		t.Fatal(err)
	}

	// The camera's sockets are opened inside its namespace.
	files, flags, err := openSockets(nsA, []SocketSpec{{Instance: "http", Name: "http", Network: "tcp", Port: 80}})
	if err != nil {
		t.Fatal(err)
	}
	if len(flags) != 1 || flags[0] != "http:http:tcp:80" {
		t.Fatalf("flags = %v", flags)
	}
	ln, err := net.FileListener(files[0])
	files[0].Close()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Write([]byte("hello"))
			c.Close()
		}
	}()

	// Another device on the LAN reaches camera A through its own MAC.
	var got []byte
	err = inNamespace(nsB.fd, func() error {
		c, err := net.DialTimeout("tcp", "10.99.0.10:80", 2*time.Second)
		if err != nil {
			return err
		}
		defer c.Close()
		buf := make([]byte, 5)
		_, err = c.Read(buf)
		got = buf
		return err
	})
	if err != nil || string(got) != "hello" {
		t.Fatalf("B cannot reach A: %v %q", err, got)
	}
	nh, err := netlink.NewHandleAt(nsB.fd)
	if err != nil {
		t.Fatal(err)
	}
	defer nh.Close()
	neigh, err := nh.NeighList(0, netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	wantMAC, _ := net.ParseMAC(specA.MAC)
	parent, _ := netlink.LinkByName("mvtest0")
	found := false
	for _, n := range neigh {
		if n.IP.Equal(net.ParseIP("10.99.0.10")) {
			found = true
			if !bytes.Equal(n.HardwareAddr, wantMAC) || bytes.Equal(n.HardwareAddr, parent.Attrs().HardwareAddr) {
				t.Fatalf("neighbor MAC %s, want the camera's own %s", n.HardwareAddr, wantMAC)
			}
		}
	}
	if !found {
		t.Fatal("camera A is not in B's neighbor table")
	}

	// Deleting the namespace removes the interface with it.
	if err := nsA.delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(nsA.path()); nsA.named && !os.IsNotExist(err) {
		t.Fatalf("namespace file still exists: %v", err)
	}
}

func TestSpecValidation(t *testing.T) {
	ok := testSpec("01J8Z3QK00000000000000AAAA", "192.168.1.20")
	ok.Sockets = []SocketSpec{{Instance: "http", Name: "http", Network: "tcp", Port: 80}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []func(s *CameraSpec){
		func(s *CameraSpec) { s.Netns = "../etc" },
		func(s *CameraSpec) { s.Parent = "eth0; rm -rf /" },
		func(s *CameraSpec) { s.ID = "x" },
		func(s *CameraSpec) { s.Sockets = append(s.Sockets, s.Sockets[0]) },
		func(s *CameraSpec) { s.Mode = "bridge" },
	}
	for i, mutate := range bad {
		s := *ok
		s.Sockets = append([]SocketSpec(nil), ok.Sockets...)
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("case %d should be rejected", i)
		}
	}
}
