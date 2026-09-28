package netctl

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/corticoide/mockvision/backend/internal/domain"
)

// linkResult is what setupInterface made.
type linkResult struct {
	// MAC is the MAC the camera uses on the LAN.
	MAC string
	// Warnings are conflicts a forced camera started with anyway (D14).
	Warnings []string
}

// setupInterface gives a namespace its interface on the parent NIC:
// macvlan with the camera's own MAC, probed first (RN-06), or ipvlan with
// the parent's. IPv6 is off in the namespace: cameras are IPv4 only in v1,
// and nothing leaves with the camera's MAC before the probes. A static
// camera then gets its address (see setAddress); a DHCP one gets it later.
func setupInterface(host netns.NsHandle, ns *namespace, spec *CameraSpec, cancel <-chan struct{}) (linkResult, error) {
	var res linkResult
	hh, err := netlink.NewHandleAt(host)
	if err != nil {
		return res, err
	}
	defer hh.Close()
	nh, err := netlink.NewHandleAt(ns.fd)
	if err != nil {
		return res, err
	}
	defer nh.Close()

	parent, err := hh.LinkByName(spec.Parent)
	if err != nil {
		return res, errorf(CodeNoInterface, "parent interface %s not found", spec.Parent)
	}
	if parent.Attrs().OperState == netlink.OperDown {
		return res, errorf(CodeNoInterface, "parent interface %s is down", spec.Parent)
	}
	disableIPv6(ns)

	mac, err := net.ParseMAC(spec.MAC)
	if err != nil {
		return res, errorf(CodeInvalid, "invalid MAC %s", spec.MAC)
	}
	probing := spec.Mode == string(domain.NetMacvlan) && !spec.SkipProbe
	tmp := "mv" + strings.ToLower(spec.ID[len(spec.ID)-10:])
	var link netlink.Link
	switch spec.Mode {
	case string(domain.NetIPvlan):
		link = &netlink.IPVlan{
			LinkAttrs: netlink.LinkAttrs{Name: tmp, ParentIndex: parent.Attrs().Index, MTU: parent.Attrs().MTU},
			Mode:      netlink.IPVLAN_MODE_L2,
		}
	default:
		// While probing for its MAC the interface carries a temporary one,
		// so the probe never claims the MAC of a device already using it.
		first := mac
		if probing {
			first = randomMAC()
		}
		link = &netlink.Macvlan{
			LinkAttrs: netlink.LinkAttrs{Name: tmp, ParentIndex: parent.Attrs().Index, HardwareAddr: first, MTU: parent.Attrs().MTU},
			Mode:      netlink.MACVLAN_MODE_BRIDGE,
		}
	}
	if err := hh.LinkAdd(link); err != nil {
		if spec.Mode == string(domain.NetIPvlan) && (errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP)) {
			return res, errorf(CodeUnsupported, "this kernel has no ipvlan support; use macvlan")
		}
		if errors.Is(err, unix.EBUSY) {
			return res, parentBusy(spec.Parent, spec.Mode)
		}
		if errors.Is(err, unix.EADDRINUSE) {
			return res, errorf(CodeMACInUse, "%s", localMACConflict(mac, spec.Parent))
		}
		return res, fmt.Errorf("create %s on %s: %w", spec.Mode, spec.Parent, err)
	}
	added, err := hh.LinkByName(tmp)
	if err == nil {
		err = hh.LinkSetNsFd(added, int(ns.fd))
	}
	if err != nil {
		if added != nil {
			_ = hh.LinkDel(added)
		}
		return res, fmt.Errorf("move %s into %s: %w", spec.Mode, ns.name, err)
	}

	if lo, err := nh.LinkByName("lo"); err == nil {
		_ = nh.LinkSetUp(lo)
	}
	eth, err := nh.LinkByName(tmp)
	if err != nil {
		return res, err
	}
	if err := nh.LinkSetName(eth, "eth0"); err != nil {
		return res, err
	}
	// No IPv6 link-local either where disableIPv6 could not write the
	// namespace's settings (containers mount /proc/sys read-only).
	_ = nh.LinkSetIP6AddrGenMode(eth, ip6AddrGenModeNone)
	if err := nh.LinkSetUp(eth); err != nil {
		return res, err
	}
	res.MAC = mac.String()
	if spec.Mode == string(domain.NetIPvlan) {
		res.MAC = parent.Attrs().HardwareAddr.String()
	}

	if probing {
		err := macProbe(host, ns.fd, eth.Attrs().Index, eth.Attrs().HardwareAddr, mac)
		var ce *MACConflictError
		switch {
		case errors.As(err, &ce) && spec.Force:
			res.Warnings = append(res.Warnings, ce.Error())
		case errors.As(err, &ce):
			return res, errorf(CodeMACInUse, "%s", ce.Error())
		case err != nil:
			return res, fmt.Errorf("MAC probe: %w", err)
		}
		if err := nh.LinkSetDown(eth); err != nil {
			return res, err
		}
		if err := nh.LinkSetHardwareAddr(eth, mac); err != nil {
			// The kernel refuses a MAC another interface on the same
			// parent has: not even a forced camera can take it.
			if errors.Is(err, unix.EADDRINUSE) {
				return res, errorf(CodeMACInUse, "%s", localMACConflict(mac, spec.Parent))
			}
			return res, fmt.Errorf("set MAC %s: %w", mac, err)
		}
		if err := nh.LinkSetUp(eth); err != nil {
			return res, err
		}
	}
	if spec.DHCP() {
		if err := nh.RouteReplace(onLinkDefault(eth.Attrs().Index)); err != nil {
			return res, fmt.Errorf("on-link default route: %w", err)
		}
		return res, nil
	}
	warn, err := setAddress(host, ns, spec, &AddressSpec{ID: spec.ID, IP: spec.IP, Prefix: spec.Prefix, Gateway: spec.Gateway, Force: spec.Force}, cancel)
	res.Warnings = append(res.Warnings, warn...)
	return res, err
}

// setAddress gives the camera's interface its IPv4 address, replacing any
// other: ARP probe (RN-06), address, default route and a gratuitous ARP. A
// second announcement follows a second later unless cancel is closed
// first: a camera deleted meanwhile must not claim its address again.
//
// A macvlan camera probes from its own interface. An ipvlan one probes
// from the parent, which shares its MAC: ipvlan hands the parent the
// answers to a probe, which carry no address of the camera.
func setAddress(host netns.NsHandle, ns *namespace, spec *CameraSpec, a *AddressSpec, cancel <-chan struct{}) ([]string, error) {
	var warnings []string
	nh, err := netlink.NewHandleAt(ns.fd)
	if err != nil {
		return nil, err
	}
	defer nh.Close()
	eth, err := nh.LinkByName("eth0")
	if err != nil {
		return nil, err
	}
	ifindex := eth.Attrs().Index
	mac := eth.Attrs().HardwareAddr
	ip, err := netip.ParseAddr(a.IP)
	if err != nil {
		return nil, err
	}
	want := &net.IPNet{IP: net.IP(ip.AsSlice()), Mask: net.CIDRMask(a.Prefix, 32)}
	addrs, err := nh.AddrList(eth, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	// An address the interface already has is the camera's own: a renewal
	// that changes the prefix or the router is not probed again.
	held := false
	for _, old := range addrs {
		if old.IP.Equal(want.IP) {
			held = true
		}
	}
	if !spec.SkipProbe && !held {
		probeNS, probeIf, probeMAC := ns.fd, ifindex, mac
		if spec.Mode == string(domain.NetIPvlan) {
			hh, err := netlink.NewHandleAt(host)
			if err != nil {
				return nil, err
			}
			parent, err := hh.LinkByName(spec.Parent)
			hh.Close()
			if err != nil {
				return nil, errorf(CodeNoInterface, "parent interface %s not found", spec.Parent)
			}
			probeNS, probeIf, probeMAC = host, parent.Attrs().Index, parent.Attrs().HardwareAddr
		}
		err := probeIP(probeNS, probeIf, probeMAC, ip)
		var ce *ConflictError
		var me *MACConflictError
		switch {
		case (errors.As(err, &ce) || errors.As(err, &me)) && a.Force:
			warnings = append(warnings, err.Error())
		case errors.As(err, &ce):
			return nil, errorf(CodeIPInUse, "%s", ce.Error())
		case errors.As(err, &me):
			return nil, errorf(CodeMACInUse, "%s", me.Error())
		case err != nil:
			return nil, fmt.Errorf("ARP probe: %w", err)
		}
	}
	have := false
	for _, old := range addrs {
		if old.IPNet.String() == want.String() {
			have = true
			continue
		}
		_ = nh.AddrDel(eth, &old)
	}
	if !have {
		if err := nh.AddrAdd(eth, &netlink.Addr{IPNet: want}); err != nil {
			return nil, fmt.Errorf("assign %s/%d: %w", a.IP, a.Prefix, err)
		}
	}
	def := &netlink.Route{LinkIndex: ifindex, Dst: nil}
	if a.Gateway != "" {
		def.Gw = net.ParseIP(a.Gateway)
		if err := nh.RouteReplace(def); err != nil {
			return nil, fmt.Errorf("default route via %s: %w", a.Gateway, err)
		}
	} else if routes, err := nh.RouteList(eth, netlink.FAMILY_V4); err == nil {
		// A lease without a router drops the previous one's route; a DHCP
		// camera keeps its on-link one.
		for _, r := range routes {
			if isDefault(r.Dst) && r.Gw != nil {
				_ = nh.RouteDel(&r)
			}
		}
	}
	if spec.DHCP() {
		// Deleting the interface's last address flushed its routes.
		if err := nh.RouteReplace(onLinkDefault(ifindex)); err != nil {
			return nil, fmt.Errorf("on-link default route: %w", err)
		}
	}
	if err := announce(ns.fd, ifindex, mac, ip); err != nil {
		return warnings, fmt.Errorf("gratuitous ARP: %w", err)
	}
	// A second announcement a second later, like most devices do. The
	// namespace descriptor is duplicated so a quick delete cannot recycle it.
	if dup, err := unix.Dup(int(ns.fd)); err == nil {
		go func() {
			defer unix.Close(dup)
			select {
			case <-cancel:
			case <-time.After(time.Second):
				_ = announce(netns.NsHandle(dup), ifindex, mac, ip)
			}
		}()
	}
	return warnings, nil
}

// onLinkMetric ranks a DHCP camera's on-link default route below any
// router's.
const onLinkMetric = 0xffff

// onLinkDefault is the default route through the camera's interface that
// a DHCP camera keeps. Where reverse path filtering is on, as Ubuntu and
// Debian set it and new namespaces inherit it, the kernel drops a packet
// from a source it has no route back to: without an address, or at a
// factory address without a router, the camera would never hear the DHCP
// server. The route is set over netlink, since containers mount
// /proc/sys read-only and rp_filter cannot be turned off there.
func onLinkDefault(ifindex int) *netlink.Route {
	return &netlink.Route{
		LinkIndex: ifindex,
		Dst:       &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
		Scope:     netlink.SCOPE_LINK,
		Priority:  onLinkMetric,
	}
}

// isDefault reports whether dst is the IPv4 default route, which the
// netlink package lists as 0.0.0.0/0.
func isDefault(dst *net.IPNet) bool {
	if dst == nil {
		return true
	}
	ones, _ := dst.Mask.Size()
	return ones == 0 && dst.IP.IsUnspecified()
}

// parentBusy explains an EBUSY from the kernel: a network card takes
// macvlan or ipvlan children, not both, and one in a bridge or a bond
// takes neither.
func parentBusy(parent, mode string) *Error {
	if mode == string(domain.NetIPvlan) {
		return errorf(CodeParentBusy, "%s cannot take an ipvlan interface: it already has macvlan interfaces (cameras or the node bridge), or belongs to a bridge or a bond", parent)
	}
	return errorf(CodeParentBusy, "%s cannot take a macvlan interface: it already has ipvlan cameras, or belongs to a bridge or a bond", parent)
}

// localMACConflict describes a MAC another interface on the parent has.
func localMACConflict(mac net.HardwareAddr, parent string) string {
	return (&MACConflictError{MAC: mac.String(), How: "another interface on " + parent + " of this node has it"}).Error()
}

// disableIPv6 turns IPv6 off in a namespace before its interface moves in:
// the interface takes the namespace's defaults.
func disableIPv6(ns *namespace) {
	_ = inNamespace(ns.fd, func() error {
		for _, conf := range []string{"all", "default"} {
			_ = os.WriteFile("/proc/sys/net/ipv6/conf/"+conf+"/disable_ipv6", []byte("1"), 0o644)
		}
		return nil
	})
}

// ip6AddrGenModeNone is IN6_ADDR_GEN_MODE_NONE of linux/if_link.h.
const ip6AddrGenModeNone = 1

// randomMAC returns a random locally administered unicast MAC.
func randomMAC() net.HardwareAddr {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	b[0] = b[0]&0xfc | 0x02
	return net.HardwareAddr(b)
}

// openDHCPSocket opens the socket a DHCP camera leases its address with:
// UDP port 68 on eth0, allowed to broadcast. The camera, unprivileged,
// could not bind a port below 1024 nor to a device.
func openDHCPSocket(ns *namespace) (*os.File, error) {
	var f *os.File
	err := inNamespace(ns.fd, func() error {
		lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
			var serr error
			err := c.Control(func(fd uintptr) {
				if serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); serr != nil {
					return
				}
				if serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1); serr != nil {
					return
				}
				serr = unix.BindToDevice(int(fd), "eth0")
			})
			if err != nil {
				return err
			}
			return serr
		}}
		pc, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:68")
		if err != nil {
			return fmt.Errorf("listen udp/68: %w", err)
		}
		defer pc.Close()
		f, err = pc.(*net.UDPConn).File()
		return err
	})
	return f, err
}

// removeLinks deletes the interfaces of a namespace but its loopback. The
// kernel destroys a namespace, and its interfaces, only once nothing
// refers to it and then asynchronously: deleting the macvlan first frees
// the camera's address on the LAN at once.
func removeLinks(ns *namespace) {
	if !ns.fd.IsOpen() {
		return
	}
	nh, err := netlink.NewHandleAt(ns.fd)
	if err != nil {
		return
	}
	defer nh.Close()
	links, err := nh.LinkList()
	if err != nil && !errors.Is(err, netlink.ErrDumpInterrupted) {
		return
	}
	for _, l := range links {
		if l.Attrs().Flags&net.FlagLoopback == 0 {
			_ = nh.LinkDel(l)
		}
	}
}

// openSockets opens the camera's listening sockets inside its namespace, so
// the camera can serve ports such as 80 and 554 without privileges. It
// returns the files and the matching --socket flags for the camera.
func openSockets(ns *namespace, specs []SocketSpec) ([]*os.File, []string, error) {
	var files []*os.File
	var flags []string
	err := inNamespace(ns.fd, func() error {
		for _, s := range specs {
			var f *os.File
			switch s.Network {
			case "tcp":
				ln, err := net.ListenTCP("tcp4", &net.TCPAddr{Port: s.Port})
				if err != nil {
					return fmt.Errorf("listen tcp/%d: %w", s.Port, err)
				}
				f, err = ln.File()
				ln.Close()
				if err != nil {
					return err
				}
			case "udp":
				pc, err := net.ListenUDP("udp4", &net.UDPAddr{Port: s.Port})
				if err != nil {
					return fmt.Errorf("listen udp/%d: %w", s.Port, err)
				}
				f, err = pc.File()
				pc.Close()
				if err != nil {
					return err
				}
			}
			files = append(files, f)
			flags = append(flags, s.Instance+":"+s.Name+":"+s.Network+":"+strconv.Itoa(s.Port))
		}
		return nil
	})
	if err != nil {
		for _, f := range files {
			f.Close()
		}
		return nil, nil, err
	}
	return files, flags, nil
}
