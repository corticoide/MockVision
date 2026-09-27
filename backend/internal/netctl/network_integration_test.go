//go:build linux && integration

package netctl

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// lanDevice is another device on the test LAN: a macvlan in its own
// namespace, with IPv6 on as most devices have it.
func lanDevice(t *testing.T, parent, name, ip string, mac net.HardwareAddr) *namespace {
	t.Helper()
	ns, err := createNamespace(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.delete() })
	_ = inNamespace(ns.fd, func() error {
		// Link-local addresses right away, without duplicate detection.
		_ = os.WriteFile("/proc/sys/net/ipv6/conf/default/accept_dad", []byte("0"), 0o644)
		return os.WriteFile("/proc/sys/net/ipv6/conf/default/disable_ipv6", []byte("0"), 0o644)
	})
	p, err := netlink.LinkByName(parent)
	if err != nil {
		t.Fatal(err)
	}
	tmp := "dv" + name[len(name)-4:]
	attrs := netlink.LinkAttrs{Name: tmp, ParentIndex: p.Attrs().Index}
	if mac != nil {
		attrs.HardwareAddr = mac
	}
	if err := netlink.LinkAdd(&netlink.Macvlan{LinkAttrs: attrs, Mode: netlink.MACVLAN_MODE_BRIDGE}); err != nil {
		t.Fatal(err)
	}
	l, _ := netlink.LinkByName(tmp)
	if err := netlink.LinkSetNsFd(l, int(ns.fd)); err != nil {
		t.Fatal(err)
	}
	configureDevice(t, ns, tmp, ip)
	return ns
}

// configureDevice names a device's interface eth0 and gives it ip/24.
func configureDevice(t *testing.T, ns *namespace, link, ip string) {
	t.Helper()
	nh, err := netlink.NewHandleAt(ns.fd)
	if err != nil {
		t.Fatal(err)
	}
	defer nh.Close()
	l, err := nh.LinkByName(link)
	if err != nil {
		t.Fatal(err)
	}
	_ = nh.LinkSetName(l, "eth0")
	addr, _ := netlink.ParseAddr(ip + "/24")
	if err := nh.AddrAdd(l, addr); err != nil {
		t.Fatal(err)
	}
	if err := nh.LinkSetUp(l); err != nil {
		t.Fatal(err)
	}
	if lo, err := nh.LinkByName("lo"); err == nil {
		_ = nh.LinkSetUp(lo)
	}
}

// listen serves "hello" on a TCP port inside a namespace (the host's with
// ns == -1).
func listen(t *testing.T, ns netns.NsHandle, addr string) {
	t.Helper()
	var ln net.Listener
	do := func() error {
		var err error
		ln, err = net.Listen("tcp4", addr)
		return err
	}
	var err error
	if ns == -1 {
		err = do()
	} else {
		err = inNamespace(ns, do)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("hello"))
			c.Close()
		}
	}()
}

// dial connects from a namespace (the host's with ns == -1) and reads
// the greeting.
func dial(ns netns.NsHandle, addr string) error {
	do := func() error {
		c, err := net.DialTimeout("tcp4", addr, 700*time.Millisecond)
		if err != nil {
			return err
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 5)
		if _, err := io.ReadFull(c, buf); err != nil {
			return err
		}
		if string(buf) != "hello" {
			return errors.New("unexpected greeting")
		}
		return nil
	}
	if ns == -1 {
		return do()
	}
	return inNamespace(ns, do)
}

// A camera does not take a MAC another device uses (RN-06), unless forced
// (D14) where the kernel allows it.
func TestMACProbe(t *testing.T) {
	host := requireRoot(t)
	testParent(t, "mvtest2")
	try := func(t *testing.T, id, mac string, force bool) (linkResult, error) {
		t.Helper()
		spec := testSpec(id, "10.94.0.10")
		spec.Parent, spec.MAC, spec.Force = "mvtest2", mac, force
		ns, err := createNamespace(spec.Netns)
		if err != nil {
			t.Fatal(err)
		}
		defer ns.delete()
		return setupInterface(host, ns, spec, nil)
	}

	t.Run("in the node's neighbor table", func(t *testing.T) {
		p, _ := netlink.LinkByName("mvtest2")
		mac, _ := net.ParseMAC("02:42:ac:11:00:91")
		n := &netlink.Neigh{LinkIndex: p.Attrs().Index, Family: netlink.FAMILY_V4, State: netlink.NUD_PERMANENT,
			IP: net.ParseIP("10.94.0.91"), HardwareAddr: mac}
		if err := netlink.NeighSet(n); err != nil {
			t.Fatal(err)
		}
		defer netlink.NeighDel(n)
		if _, err := try(t, "01J8Z3QK0000000000000M0001", mac.String(), false); !isCode(err, CodeMACInUse) {
			t.Fatalf("expected mac_in_use, got %v", err)
		}
		if res, err := try(t, "01J8Z3QK0000000000000M0001", mac.String(), true); err != nil || len(res.Warnings) == 0 {
			t.Fatalf("forced: %+v, %v", res, err)
		}
	})

	t.Run("another interface on the parent", func(t *testing.T) {
		mac, _ := net.ParseMAC("02:42:ac:11:00:92")
		lanDevice(t, "mvtest2", "sim-itest-dev2", "10.94.0.20", mac)
		if _, err := try(t, "01J8Z3QK0000000000000M0002", mac.String(), false); !isCode(err, CodeMACInUse) {
			t.Fatalf("expected mac_in_use, got %v", err)
		}
	})

	t.Run("a device across the wire answers over IPv6", func(t *testing.T) {
		if _, err := os.Stat("/proc/sys/net/ipv6"); err != nil {
			t.Skip("IPv6 is disabled on this kernel")
		}
		// The veth peer is a device of its own, as one behind a switch.
		dev, err := createNamespace("sim-itest-dev5")
		if err != nil {
			t.Fatal(err)
		}
		defer dev.delete()
		_ = inNamespace(dev.fd, func() error {
			return os.WriteFile("/proc/sys/net/ipv6/conf/default/accept_dad", []byte("0"), 0o644)
		})
		mac, _ := net.ParseMAC("02:42:ac:11:00:93")
		peer, _ := netlink.LinkByName("mvtest2p")
		_ = netlink.LinkSetDown(peer)
		if err := netlink.LinkSetHardwareAddr(peer, mac); err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetNsFd(peer, int(dev.fd)); err != nil {
			t.Fatal(err)
		}
		configureDevice(t, dev, "mvtest2p", "10.94.0.30")
		time.Sleep(200 * time.Millisecond) // link-local address
		_, err = try(t, "01J8Z3QK0000000000000M0003", mac.String(), false)
		if !isCode(err, CodeMACInUse) {
			t.Fatalf("expected mac_in_use, got %v", err)
		}
		t.Logf("refused: %v", err)
	})

	t.Run("a free MAC", func(t *testing.T) {
		free := testSpec("01J8Z3QK0000000000000M0004", "10.94.0.11")
		free.Parent = "mvtest2"
		ns, err := createNamespace(free.Netns)
		if err != nil {
			t.Fatal(err)
		}
		defer ns.delete()
		if res, err := setupInterface(host, ns, free, nil); err != nil || res.MAC != free.MAC {
			t.Fatalf("free MAC: %+v, %v", res, err)
		}
		nh, _ := netlink.NewHandleAt(ns.fd)
		defer nh.Close()
		eth, _ := nh.LinkByName("eth0")
		if eth.Attrs().HardwareAddr.String() != free.MAC {
			t.Fatalf("eth0 has %s, want %s", eth.Attrs().HardwareAddr, free.MAC)
		}
	})
}

// The firewall lets a camera reach its targets and its clients, and
// nothing else.
func TestFirewall(t *testing.T) {
	host := requireRoot(t)
	testParent(t, "mvtest3")
	dev := lanDevice(t, "mvtest3", "sim-itest-dev3", "10.96.0.20", nil)
	listen(t, dev.fd, "10.96.0.20:9000")
	listen(t, dev.fd, "10.96.0.20:9001")

	spec := testSpec("01J8Z3QK0000000000000F0001", "10.96.0.10")
	spec.Parent = "mvtest3"
	spec.Sockets = []SocketSpec{{Instance: "http", Name: "http", Network: "tcp", Port: 80}}
	ns, err := createNamespace(spec.Netns)
	if err != nil {
		t.Fatal(err)
	}
	defer ns.delete()
	if _, err := setupInterface(host, ns, spec, nil); err != nil {
		t.Fatal(err)
	}
	fw := &Firewall{Allow: []Destination{{IP: "10.96.0.20", Port: 9000, Proto: "tcp"}}}
	if err := applyFirewall(int(ns.fd), spec.Sockets, false, fw); err != nil {
		t.Fatal(err)
	}
	listen(t, ns.fd, ":80")

	if err := dial(ns.fd, "10.96.0.20:9000"); err != nil {
		t.Fatalf("the camera cannot reach its target: %v", err)
	}
	if err := dial(ns.fd, "10.96.0.20:9001"); err == nil {
		t.Fatal("the camera reached a port that is not a target")
	}
	if err := dial(dev.fd, "10.96.0.10:80"); err != nil {
		t.Fatalf("a client cannot reach the camera: %v", err)
	}
	// Applying again replaces the rules.
	fw.Allow = append(fw.Allow, Destination{IP: "10.96.0.20", Port: 9001, Proto: "tcp"})
	if err := applyFirewall(int(ns.fd), spec.Sockets, false, fw); err != nil {
		t.Fatal(err)
	}
	if err := dial(ns.fd, "10.96.0.20:9001"); err != nil {
		t.Fatalf("the new target is still refused: %v", err)
	}
	// A DNS server is reachable on port 53 only.
	listen(t, dev.fd, "10.96.0.20:53")
	if err := dial(ns.fd, "10.96.0.20:53"); err == nil {
		t.Fatal("the camera reached port 53 of a host that is not its DNS server")
	}
	fw.DNS = []string{"10.96.0.20"}
	if err := applyFirewall(int(ns.fd), spec.Sockets, false, fw); err != nil {
		t.Fatal(err)
	}
	if err := dial(ns.fd, "10.96.0.20:53"); err != nil {
		t.Fatalf("the camera cannot reach its DNS server: %v", err)
	}
}

// A DHCP camera's socket broadcasts before the camera has an address, and
// receives the broadcast answers.
func TestDHCPSocket(t *testing.T) {
	host := requireRoot(t)
	testParent(t, "mvtest4")
	dev := lanDevice(t, "mvtest4", "sim-itest-dev4", "10.92.0.1", nil)

	spec := testSpec("01J8Z3QK0000000000000D0001", "")
	spec.Parent, spec.IPMode, spec.IP, spec.Prefix, spec.Hostname = "mvtest4", "dhcp", "", 0, "Gate-1"
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	ns, err := createNamespace(spec.Netns)
	if err != nil {
		t.Fatal(err)
	}
	defer ns.delete()
	if _, err := setupInterface(host, ns, spec, nil); err != nil {
		t.Fatal(err)
	}
	if err := applyFirewall(int(ns.fd), nil, true, &Firewall{}); err != nil {
		t.Fatal(err)
	}
	f, err := openDHCPSocket(ns)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := net.FilePacketConn(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	var server net.PacketConn
	if err := inNamespace(dev.fd, func() error {
		lc := net.ListenConfig{Control: broadcastControl}
		var err error
		server, err = lc.ListenPacket(t.Context(), "udp4", "0.0.0.0:67")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	bcast := &net.UDPAddr{IP: net.IPv4bcast, Port: 67}
	if _, err := pc.WriteTo([]byte("discover"), bcast); err != nil {
		t.Fatalf("broadcast without an address: %v", err)
	}
	_ = server.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, from, err := server.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "discover" {
		t.Fatalf("server got %q from %v: %v", buf[:n], from, err)
	}
	if !strings.HasPrefix(from.String(), "0.0.0.0:68") {
		t.Fatalf("sent from %s", from)
	}
	if _, err := server.WriteTo([]byte("offer"), &net.UDPAddr{IP: net.IPv4bcast, Port: 68}); err != nil {
		t.Fatal(err)
	}
	_ = pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err = pc.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "offer" {
		t.Fatalf("camera got %q: %v", buf[:n], err)
	}

	// The address the service then sets, probed first.
	a := &AddressSpec{ID: spec.ID, IP: "10.92.0.50", Prefix: 24, Gateway: "10.92.0.1"}
	if _, err := setAddress(host, ns, spec, a, nil); err != nil {
		t.Fatal(err)
	}
	a.IP = "10.92.0.1" // the server's own address
	if _, err := setAddress(host, ns, spec, a, nil); !isCode(err, CodeIPInUse) {
		t.Fatalf("expected ip_in_use, got %v", err)
	}
}

func broadcastControl(_, _ string, c syscall.RawConn) error {
	return c.Control(func(fd uintptr) {
		_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
		_ = unix.BindToDevice(int(fd), "eth0")
	})
}

func isCode(err error, code string) bool {
	var ne *Error
	return errors.As(err, &ne) && ne.Code == code
}

// With the bridge, the node reaches its macvlan cameras and they reach it;
// without it they cannot (D26).
func TestBridge(t *testing.T) {
	host := requireRoot(t)
	testParent(t, "mvtest6")
	p, _ := netlink.LinkByName("mvtest6")
	addr, _ := netlink.ParseAddr("10.91.0.1/24")
	if err := netlink.AddrAdd(p, addr); err != nil {
		t.Fatal(err)
	}
	listen(t, -1, "10.91.0.1:9100")

	spec := testSpec("01J8Z3QK0000000000000B0001", "10.91.0.10")
	spec.Parent = "mvtest6"
	ns, err := createNamespace(spec.Netns)
	if err != nil {
		t.Fatal(err)
	}
	defer ns.delete()
	if _, err := setupInterface(host, ns, spec, nil); err != nil {
		t.Fatal(err)
	}
	listen(t, ns.fd, ":80")
	if err := dial(-1, "10.91.0.10:80"); err == nil {
		t.Fatal("the node reached a macvlan camera without the bridge")
	}

	b, err := createBridge(host, "mvtest6")
	if err != nil {
		t.Fatal(err)
	}
	// Set over netlink, which works where /proc/sys is read-only.
	if v, err := os.ReadFile("/proc/sys/net/ipv4/conf/" + bridgeName + "/arp_ignore"); err != nil || strings.TrimSpace(string(v)) != "8" {
		t.Fatalf("arp_ignore of the bridge: %q, %v", v, err)
	}
	ip := netip.MustParseAddr("10.91.0.10")
	if err := b.attach(host, ns, ip); err != nil {
		t.Fatal(err)
	}
	if err := dial(-1, "10.91.0.10:80"); err != nil {
		t.Fatalf("the node cannot reach the camera through the bridge: %v", err)
	}
	if err := dial(ns.fd, "10.91.0.1:9100"); err != nil {
		t.Fatalf("the camera cannot reach the node through the bridge: %v", err)
	}
	b.detach(host, ns, ip)
	b.remove(host)
	if _, err := netlink.LinkByName(bridgeName); err == nil {
		t.Fatal("the bridge is still there")
	}
	if err := dial(-1, "10.91.0.10:80"); err == nil {
		t.Fatal("the node still reaches the camera after the bridge is gone")
	}
}

// An ipvlan camera answers on the LAN with the parent's MAC and probes its
// address from the parent (D27). Skipped where the kernel has no ipvlan.
func TestIPvlan(t *testing.T) {
	host := requireRoot(t)
	testParent(t, "mvtest7")
	// The other end of the veth is the LAN device: a parent cannot carry
	// macvlan and ipvlan interfaces at once.
	dev, err := createNamespace("sim-itest-dev7")
	if err != nil {
		t.Fatal(err)
	}
	defer dev.delete()
	peer, _ := netlink.LinkByName("mvtest7p")
	if err := netlink.LinkSetNsFd(peer, int(dev.fd)); err != nil {
		t.Fatal(err)
	}
	configureDevice(t, dev, "mvtest7p", "10.93.0.20")

	spec := testSpec("01J8Z3QK0000000000000I0001", "10.93.0.10")
	spec.Parent, spec.Mode = "mvtest7", "ipvlan"
	ns, err := createNamespace(spec.Netns)
	if err != nil {
		t.Fatal(err)
	}
	defer ns.delete()
	res, err := setupInterface(host, ns, spec, nil)
	if isCode(err, CodeUnsupported) {
		t.Skip("the kernel has no ipvlan")
	}
	if err != nil {
		t.Fatal(err)
	}
	parent, _ := netlink.LinkByName("mvtest7")
	if res.MAC != parent.Attrs().HardwareAddr.String() {
		t.Fatalf("ipvlan MAC %s, want the parent's %s", res.MAC, parent.Attrs().HardwareAddr)
	}
	listen(t, ns.fd, ":80")
	if err := dial(dev.fd, "10.93.0.10:80"); err != nil {
		t.Fatalf("the LAN cannot reach the ipvlan camera: %v", err)
	}

	dup := testSpec("01J8Z3QK0000000000000I0002", "10.93.0.20")
	dup.Parent, dup.Mode = "mvtest7", "ipvlan"
	ns2, err := createNamespace(dup.Netns)
	if err != nil {
		t.Fatal(err)
	}
	defer ns2.delete()
	if _, err := setupInterface(host, ns2, dup, nil); !isCode(err, CodeIPInUse) {
		t.Fatalf("expected ip_in_use from the parent's probe, got %v", err)
	}
}
