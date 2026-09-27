package netctl

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv6"
)

// The frames of the MAC probe must be valid IPv6 neighbor discovery and
// echo: a device drops a bad checksum or hop limit without a word. They are
// checked against golang.org/x/net, an independent implementation.
func TestMACProbeFrames(t *testing.T) {
	probe, _ := net.ParseMAC("02:aa:bb:cc:dd:ee")
	target, _ := net.ParseMAC("00:11:22:33:44:55")
	if got := eui64(target).String(); got != "fe80::211:22ff:fe33:4455" {
		t.Fatalf("EUI-64 %s", got)
	}
	for name, frame := range map[string][]byte{
		"solicitation": neighborSolicitation(probe, target),
		"echo":         allNodesPing(probe, target),
	} {
		t.Run(name, func(t *testing.T) {
			if !bytes.Equal(frame[0:6], target) || !bytes.Equal(frame[6:12], probe) || binary.BigEndian.Uint16(frame[12:14]) != 0x86dd {
				t.Fatalf("Ethernet header %x", frame[:14])
			}
			h, err := ipv6.ParseHeader(frame[14:54])
			if err != nil {
				t.Fatal(err)
			}
			if h.Version != 6 || h.NextHeader != 58 || h.HopLimit != 255 || h.PayloadLen != len(frame)-54 {
				t.Fatalf("IPv6 header %+v", h)
			}
			if h.Src.String() != eui64(probe).String() {
				t.Fatalf("source %s", h.Src)
			}
			body := frame[54:]
			m, err := icmp.ParseMessage(58, body)
			if err != nil {
				t.Fatal(err)
			}
			// Marshal again with the pseudo-header: x/net computes the
			// checksum; it must match ours.
			src, _ := netip.AddrFromSlice(h.Src)
			dst, _ := netip.AddrFromSlice(h.Dst)
			again, err := m.Marshal(icmp.IPv6PseudoHeader(net.IP(src.AsSlice()), net.IP(dst.AsSlice())))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(again[2:4], body[2:4]) {
				t.Fatalf("checksum %x, x/net computes %x", body[2:4], again[2:4])
			}
			switch name {
			case "solicitation":
				if m.Type != ipv6.ICMPTypeNeighborSolicitation || h.Dst.String() != eui64(target).String() {
					t.Fatalf("solicitation %v to %s", m.Type, h.Dst)
				}
				// Target address, then the source link-layer option.
				if !bytes.Equal(body[8:24], h.Dst) || body[24] != 1 || body[25] != 1 || !bytes.Equal(body[26:32], probe) {
					t.Fatalf("solicitation body %x", body)
				}
			case "echo":
				if m.Type != ipv6.ICMPTypeEchoRequest || h.Dst.String() != "ff02::1" {
					t.Fatalf("echo %v to %s", m.Type, h.Dst)
				}
			}
		})
	}
}
