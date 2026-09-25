package domain

import (
	"crypto/sha256"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// NetMode is how a camera attaches to the physical network.
type NetMode string

const (
	// NetMacvlan gives the camera its own MAC on the parent interface (default).
	NetMacvlan NetMode = "macvlan"
	// NetIPvlan shares the node's MAC; used on Wi-Fi or MAC-limited switches.
	NetIPvlan NetMode = "ipvlan"
)

// IPMode is how a camera gets its address.
type IPMode string

const (
	IPStatic IPMode = "static"
	IPDHCP   IPMode = "dhcp"
)

// NetIdentity is the network identity of a camera: mode, MAC and addressing.
type NetIdentity struct {
	Mode     NetMode
	ParentIf string
	MAC      string
	IPMode   IPMode
	IP       netip.Addr
	Prefix   int
	Gateway  netip.Addr
	DNS      []netip.Addr
}

// Validate checks the identity for internal consistency.
func (n NetIdentity) Validate() error {
	v := &ValidationError{}
	switch n.Mode {
	case NetMacvlan, NetIPvlan:
	default:
		v.Add("network.mode", "must be macvlan or ipvlan")
	}
	if _, err := ParseMAC(n.MAC); err != nil {
		v.Add("network.mac", "%v", err)
	}
	switch n.IPMode {
	case IPStatic:
		if !n.IP.Is4() {
			v.Add("network.ip", "must be an IPv4 address")
		} else if n.IP.IsUnspecified() || n.IP.IsLoopback() || n.IP.IsMulticast() || n.IP.IsLinkLocalUnicast() {
			v.Add("network.ip", "must be a unicast address usable on the LAN")
		}
		if n.Prefix < 1 || n.Prefix > 30 {
			v.Add("network.netmask", "prefix length must be between 1 and 30")
		}
		if n.Gateway.IsValid() {
			if !n.Gateway.Is4() {
				v.Add("network.gateway", "must be an IPv4 address")
			} else if n.IP.Is4() && n.Prefix >= 1 && n.Prefix <= 30 {
				p := netip.PrefixFrom(n.IP, n.Prefix).Masked()
				if !p.Contains(n.Gateway) {
					v.Add("network.gateway", "must be inside %s", p)
				}
				if n.Gateway == n.IP {
					v.Add("network.gateway", "must differ from the camera IP")
				}
			}
		}
		if n.IP.Is4() && n.Prefix >= 1 && n.Prefix <= 30 {
			p := netip.PrefixFrom(n.IP, n.Prefix).Masked()
			if n.IP == p.Addr() || n.IP == LastAddr(p) {
				v.Add("network.ip", "must not be the network or broadcast address of %s", p)
			}
		}
	case IPDHCP:
		v.Add("network.ip_mode", "dhcp is not supported yet")
	default:
		v.Add("network.ip_mode", "must be static or dhcp")
	}
	for _, d := range n.DNS {
		if !d.Is4() {
			v.Add("network.dns", "%s is not an IPv4 address", d)
		}
	}
	return v.Err()
}

// LastAddr returns the broadcast address of an IPv4 prefix.
func LastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	u := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	u |= uint32(1)<<(32-p.Bits()) - 1
	return netip.AddrFrom4([4]byte{byte(u >> 24), byte(u >> 16), byte(u >> 8), byte(u)})
}

// MaskToPrefix converts a dotted netmask ("255.255.255.0") or a prefix length
// ("24", "/24") into a prefix length.
func MaskToPrefix(mask string) (int, error) {
	mask = strings.TrimPrefix(strings.TrimSpace(mask), "/")
	if mask == "" {
		return 0, fmt.Errorf("empty netmask")
	}
	if !strings.Contains(mask, ".") {
		var n int
		if _, err := fmt.Sscanf(mask, "%d", &n); err != nil || n < 0 || n > 32 {
			return 0, fmt.Errorf("invalid prefix length %q", mask)
		}
		return n, nil
	}
	addr, err := netip.ParseAddr(mask)
	if err != nil || !addr.Is4() {
		return 0, fmt.Errorf("invalid netmask %q", mask)
	}
	ones, bits := net.IPMask(addr.AsSlice()).Size()
	if bits == 0 {
		return 0, fmt.Errorf("netmask %q is not contiguous", mask)
	}
	return ones, nil
}

// PrefixToMask converts a prefix length into a dotted netmask.
func PrefixToMask(prefix int) string {
	return net.IP(net.CIDRMask(prefix, 32)).String()
}

// ParseMAC parses and normalizes a unicast Ethernet MAC address.
func ParseMAC(s string) (net.HardwareAddr, error) {
	hw, err := net.ParseMAC(s)
	if err != nil {
		return nil, fmt.Errorf("invalid MAC address %q", s)
	}
	if len(hw) != 6 {
		return nil, fmt.Errorf("MAC address %q must have 6 bytes", s)
	}
	if hw[0]&0x01 != 0 {
		return nil, fmt.Errorf("MAC address %q is multicast", s)
	}
	zero := true
	for _, b := range hw {
		if b != 0 {
			zero = false
		}
	}
	if zero {
		return nil, fmt.Errorf("MAC address %q is all zeros", s)
	}
	return hw, nil
}

// DeriveMAC returns a stable MAC for a camera ID (D25). Without an OUI the
// address is locally administered and unicast; with a vendor OUI (3 bytes)
// the first half is the vendor's and the rest comes from the ID.
func DeriveMAC(cameraID string, oui []byte) net.HardwareAddr {
	sum := sha256.Sum256([]byte("mockvision-mac:" + cameraID))
	mac := net.HardwareAddr(sum[:6])
	if len(oui) == 3 {
		copy(mac[:3], oui)
		mac[0] &^= 0x01 // unicast
		return mac
	}
	mac[0] = (mac[0] | 0x02) &^ 0x01 // locally administered, unicast
	return mac
}

// ParseOUI parses "AA:BB:CC" or "AA-BB-CC".
func ParseOUI(s string) ([]byte, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), "-", ":")
	hw, err := net.ParseMAC(s + ":00:00:00")
	if err != nil {
		return nil, fmt.Errorf("invalid OUI %q", s)
	}
	return []byte(hw[:3]), nil
}
