package netctl

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// The MAC probe listens this long after its first frames, and sends them
// twice. Most devices answer within a few milliseconds.
const (
	macProbeWait  = 400 * time.Millisecond
	macProbeAgain = 150 * time.Millisecond
)

// MACConflictError reports that another device on the LAN uses the MAC a
// camera would take (RN-06).
type MACConflictError struct {
	MAC string
	// How is how the device showed up.
	How string
}

func (e *MACConflictError) Error() string {
	return "MAC " + e.MAC + " is already in use on the LAN (" + e.How + ")"
}

// macProbe looks for another device using mac before the camera takes it
// (RN-06). IPv4 gives no way to ask who has a MAC, so the probe:
//
//   - looks it up in the node's neighbor tables and among its interfaces;
//   - asks, with an IPv6 neighbor solicitation sent to mac, who has the
//     EUI-64 link-local address of mac, which most embedded Linux devices
//     use and always answer;
//   - pings all IPv6 nodes in a frame sent to mac: a device with another
//     link-local address resolves the sender first, from mac;
//   - listens for any frame that comes from mac meanwhile.
//
// It runs on the camera's interface while that still carries probeMAC, so
// the probe never claims mac itself. A device with IPv6 off that stays
// silent goes unnoticed; the ARP probe of the address still runs.
func macProbe(host, ns netns.NsHandle, ifindex int, probeMAC, mac net.HardwareAddr) error {
	if how := knownMAC(host, mac); how != "" {
		return &MACConflictError{MAC: mac.String(), How: how}
	}
	fd := -1
	err := inNamespace(ns, func() error {
		var err error
		fd, err = unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_ALL)))
		return err
	})
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: ifindex}); err != nil {
		return err
	}
	tv := unix.NsecToTimeval((50 * time.Millisecond).Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return err
	}
	frames := [][]byte{neighborSolicitation(probeMAC, mac), allNodesPing(probeMAC, mac)}
	send := func() error {
		for _, f := range frames {
			var addr [8]byte
			copy(addr[:], mac)
			if err := unix.Sendto(fd, f, 0, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IPV6), Ifindex: ifindex, Halen: 6, Addr: addr}); err != nil {
				return err
			}
		}
		return nil
	}
	if err := send(); err != nil {
		return err
	}
	start := time.Now()
	resent := false
	buf := make([]byte, 1600)
	for time.Since(start) < macProbeWait {
		if !resent && time.Since(start) >= macProbeAgain {
			resent = true
			if err := send(); err != nil {
				return err
			}
		}
		n, from, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		if ll, ok := from.(*unix.SockaddrLinklayer); ok && ll.Pkttype == unix.PACKET_OUTGOING {
			continue
		}
		if n >= 14 && bytes.Equal(buf[6:12], mac) {
			return &MACConflictError{MAC: mac.String(), How: "a device answered from it"}
		}
	}
	return nil
}

// knownMAC reports where the node already sees mac: on one of its own
// interfaces, or in its neighbor tables. Entries on the bridge to the
// cameras are the cameras themselves.
func knownMAC(host netns.NsHandle, mac net.HardwareAddr) string {
	h, err := netlink.NewHandleAt(host)
	if err != nil {
		return ""
	}
	defer h.Close()
	links, err := h.LinkList()
	if err != nil {
		return ""
	}
	skip := map[int]bool{}
	for _, l := range links {
		if l.Attrs().Name == bridgeName {
			skip[l.Attrs().Index] = true
			continue
		}
		if bytes.Equal(l.Attrs().HardwareAddr, mac) {
			return "interface " + l.Attrs().Name + " of this node has it"
		}
	}
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		neigh, err := h.NeighList(0, family)
		if err != nil {
			continue
		}
		for _, n := range neigh {
			if skip[n.LinkIndex] || n.State&(netlink.NUD_FAILED|netlink.NUD_INCOMPLETE|netlink.NUD_NOARP) != 0 {
				continue
			}
			if bytes.Equal(n.HardwareAddr, mac) {
				return "the node's neighbor table has it for " + n.IP.String()
			}
		}
	}
	return ""
}

// eui64 returns the EUI-64 link-local IPv6 address of a MAC.
func eui64(mac net.HardwareAddr) netip.Addr {
	var a [16]byte
	a[0], a[1] = 0xfe, 0x80
	a[8] = mac[0] ^ 0x02
	a[9], a[10] = mac[1], mac[2]
	a[11], a[12] = 0xff, 0xfe
	a[13], a[14], a[15] = mac[3], mac[4], mac[5]
	return netip.AddrFrom16(a)
}

// neighborSolicitation asks, from probeMAC, who has the EUI-64 link-local
// address of mac. It carries the sender's link-layer address, so the owner
// answers without resolving the sender.
func neighborSolicitation(probeMAC, mac net.HardwareAddr) []byte {
	target := eui64(mac)
	body := make([]byte, 0, 32)
	body = append(body, 135, 0, 0, 0, 0, 0, 0, 0) // type, code, checksum, reserved
	t := target.As16()
	body = append(body, t[:]...)
	body = append(body, 1, 1) // source link-layer address, 8 bytes
	body = append(body, probeMAC...)
	return ipv6Frame(probeMAC, mac, eui64(probeMAC), target, body)
}

// allNodesPing pings every IPv6 node, in a frame only mac receives.
func allNodesPing(probeMAC, mac net.HardwareAddr) []byte {
	body := []byte{128, 0, 0, 0, 0x6d, 0x76, 0, 1, 'm', 'o', 'c', 'k', 'v', 'i', 's', 'i', 'o', 'n'}
	return ipv6Frame(probeMAC, mac, eui64(probeMAC), netip.MustParseAddr("ff02::1"), body)
}

// ipv6Frame wraps an ICMPv6 message, computing its checksum.
func ipv6Frame(src, dst net.HardwareAddr, srcIP, dstIP netip.Addr, icmp []byte) []byte {
	s, d := srcIP.As16(), dstIP.As16()
	// Checksum over the pseudo-header and the message.
	var sum uint32
	add := func(b []byte) {
		for i := 0; i+1 < len(b); i += 2 {
			sum += uint32(b[i])<<8 | uint32(b[i+1])
		}
		if len(b)%2 == 1 {
			sum += uint32(b[len(b)-1]) << 8
		}
	}
	add(s[:])
	add(d[:])
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(icmp)))
	add(l[:])
	add([]byte{0, 0, 0, 58})
	icmp[2], icmp[3] = 0, 0
	add(icmp)
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	binary.BigEndian.PutUint16(icmp[2:4], ^uint16(sum))

	f := make([]byte, 0, 14+40+len(icmp))
	f = append(f, dst...)
	f = append(f, src...)
	f = append(f, 0x86, 0xdd)
	f = append(f, 0x60, 0, 0, 0) // version 6, no traffic class, no flow label
	f = binary.BigEndian.AppendUint16(f, uint16(len(icmp)))
	f = append(f, 58, 255) // ICMPv6, hop limit 255 as neighbor discovery requires
	f = append(f, s[:]...)
	f = append(f, d[:]...)
	return append(f, icmp...)
}
