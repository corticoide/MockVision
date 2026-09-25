package netctl

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"time"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// ARP probing follows RFC 5227, shortened to keep camera start-up under two
// seconds: three probes 150 ms apart, then 300 ms of listening.
const (
	probeCount    = 3
	probeInterval = 150 * time.Millisecond
	probeWait     = 300 * time.Millisecond
)

var broadcastMAC = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// arpSocket opens a raw ARP socket on an interface of a namespace.
func arpSocket(ns netns.NsHandle, ifindex int) (int, error) {
	fd := -1
	err := inNamespace(ns, func() error {
		var err error
		fd, err = unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_ARP)))
		return err
	})
	if err != nil {
		return -1, err
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ARP), Ifindex: ifindex}); err != nil {
		unix.Close(fd)
		return -1, err
	}
	tv := unix.NsecToTimeval((50 * time.Millisecond).Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// arpFrame builds an Ethernet frame carrying an ARP packet.
func arpFrame(op uint16, sha net.HardwareAddr, spa netip.Addr, tha net.HardwareAddr, tpa netip.Addr, dst net.HardwareAddr) []byte {
	var b bytes.Buffer
	b.Write(dst)
	b.Write(sha)
	_ = binary.Write(&b, binary.BigEndian, uint16(unix.ETH_P_ARP))
	_ = binary.Write(&b, binary.BigEndian, uint16(1))      // Ethernet
	_ = binary.Write(&b, binary.BigEndian, uint16(0x0800)) // IPv4
	b.WriteByte(6)
	b.WriteByte(4)
	_ = binary.Write(&b, binary.BigEndian, op)
	b.Write(sha)
	s4 := spa.As4()
	b.Write(s4[:])
	b.Write(tha)
	t4 := tpa.As4()
	b.Write(t4[:])
	return b.Bytes()
}

type arpPacket struct {
	op  uint16
	sha net.HardwareAddr
	spa netip.Addr
	tpa netip.Addr
}

func parseARP(frame []byte) (arpPacket, bool) {
	// 14 bytes of Ethernet header + 28 bytes of ARP.
	if len(frame) < 42 || binary.BigEndian.Uint16(frame[12:14]) != unix.ETH_P_ARP {
		return arpPacket{}, false
	}
	a := frame[14:]
	if binary.BigEndian.Uint16(a[0:2]) != 1 || binary.BigEndian.Uint16(a[2:4]) != 0x0800 || a[4] != 6 || a[5] != 4 {
		return arpPacket{}, false
	}
	return arpPacket{
		op:  binary.BigEndian.Uint16(a[6:8]),
		sha: net.HardwareAddr(append([]byte(nil), a[8:14]...)),
		spa: netip.AddrFrom4([4]byte(a[14:18])),
		tpa: netip.AddrFrom4([4]byte(a[24:28])),
	}, true
}

func sendFrame(fd, ifindex int, frame []byte) error {
	var addr [8]byte
	copy(addr[:], broadcastMAC)
	return unix.Sendto(fd, frame, 0, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ARP), Ifindex: ifindex, Halen: 6, Addr: addr})
}

// ConflictError reports that another device answers for an address (RN-06).
type ConflictError struct {
	IP  string
	MAC string
}

func (e *ConflictError) Error() string {
	return "IP " + e.IP + " is already in use on the LAN by " + e.MAC
}

// probeIP asks the LAN whether anyone uses ip before the camera takes it.
func probeIP(ns netns.NsHandle, ifindex int, mac net.HardwareAddr, ip netip.Addr) error {
	fd, err := arpSocket(ns, ifindex)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	probe := arpFrame(1, mac, netip.IPv4Unspecified(), make(net.HardwareAddr, 6), ip, broadcastMAC)
	buf := make([]byte, 1500)
	start := time.Now()
	deadline := start.Add(probeCount*probeInterval + probeWait)
	sent := 0
	for time.Now().Before(deadline) {
		if sent < probeCount && time.Since(start) >= time.Duration(sent)*probeInterval {
			if err := sendFrame(fd, ifindex, probe); err != nil {
				return err
			}
			sent++
		}
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		p, ok := parseARP(buf[:n])
		if !ok || bytes.Equal(p.sha, mac) {
			continue
		}
		// Someone already uses the address, or probes for it right now.
		if p.spa == ip || (p.op == 1 && p.spa.IsUnspecified() && p.tpa == ip) {
			return &ConflictError{IP: ip.String(), MAC: p.sha.String()}
		}
	}
	return nil
}

// announce sends a gratuitous ARP, as real cameras do when they start.
func announce(ns netns.NsHandle, ifindex int, mac net.HardwareAddr, ip netip.Addr) error {
	fd, err := arpSocket(ns, ifindex)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return sendFrame(fd, ifindex, arpFrame(1, mac, ip, make(net.HardwareAddr, 6), ip, broadcastMAC))
}
