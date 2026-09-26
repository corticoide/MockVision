package netctl

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// setupInterface gives a namespace its macvlan interface on the parent NIC:
// own MAC, ARP probe (RN-06), static address, default route and a
// gratuitous ARP. A second announcement follows a second later unless
// cancel is closed first: a camera deleted meanwhile must not claim its
// address again.
func setupInterface(host netns.NsHandle, ns *namespace, spec *CameraSpec, cancel <-chan struct{}) error {
	hh, err := netlink.NewHandleAt(host)
	if err != nil {
		return err
	}
	defer hh.Close()
	nh, err := netlink.NewHandleAt(ns.fd)
	if err != nil {
		return err
	}
	defer nh.Close()

	parent, err := hh.LinkByName(spec.Parent)
	if err != nil {
		return errorf(CodeNoInterface, "parent interface %s not found", spec.Parent)
	}
	if parent.Attrs().OperState == netlink.OperDown {
		return errorf(CodeNoInterface, "parent interface %s is down", spec.Parent)
	}
	mac, err := net.ParseMAC(spec.MAC)
	if err != nil {
		return errorf(CodeInvalid, "invalid MAC %s", spec.MAC)
	}
	tmp := "mv" + strings.ToLower(spec.ID[len(spec.ID)-10:])
	mv := &netlink.Macvlan{
		LinkAttrs: netlink.LinkAttrs{Name: tmp, ParentIndex: parent.Attrs().Index, HardwareAddr: mac, MTU: parent.Attrs().MTU},
		Mode:      netlink.MACVLAN_MODE_BRIDGE,
	}
	if err := hh.LinkAdd(mv); err != nil {
		return fmt.Errorf("create macvlan on %s: %w", spec.Parent, err)
	}
	link, err := hh.LinkByName(tmp)
	if err == nil {
		err = hh.LinkSetNsFd(link, int(ns.fd))
	}
	if err != nil {
		if link != nil {
			_ = hh.LinkDel(link)
		}
		return fmt.Errorf("move macvlan into %s: %w", ns.name, err)
	}

	if lo, err := nh.LinkByName("lo"); err == nil {
		_ = nh.LinkSetUp(lo)
	}
	eth, err := nh.LinkByName(tmp)
	if err != nil {
		return err
	}
	if err := nh.LinkSetName(eth, "eth0"); err != nil {
		return err
	}
	if err := nh.LinkSetUp(eth); err != nil {
		return err
	}
	ifindex := eth.Attrs().Index

	ip, err := netip.ParseAddr(spec.IP)
	if err != nil {
		return err
	}
	if !spec.SkipProbe {
		if err := probeIP(ns.fd, ifindex, mac, ip); err != nil {
			var ce *ConflictError
			if errors.As(err, &ce) {
				return errorf(CodeIPInUse, "%s", ce.Error())
			}
			return fmt.Errorf("ARP probe: %w", err)
		}
	}
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: net.IP(ip.AsSlice()), Mask: net.CIDRMask(spec.Prefix, 32)}}
	if err := nh.AddrAdd(eth, addr); err != nil {
		return fmt.Errorf("assign %s/%d: %w", spec.IP, spec.Prefix, err)
	}
	if spec.Gateway != "" {
		gw := net.ParseIP(spec.Gateway)
		if err := nh.RouteAdd(&netlink.Route{LinkIndex: ifindex, Gw: gw}); err != nil {
			return fmt.Errorf("default route via %s: %w", spec.Gateway, err)
		}
	}
	if err := announce(ns.fd, ifindex, mac, ip); err != nil {
		return fmt.Errorf("gratuitous ARP: %w", err)
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
	return nil
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
	if err != nil {
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
