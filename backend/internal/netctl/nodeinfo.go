package netctl

import (
	"bufio"
	"encoding/hex"
	"net"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Interface is a network interface of the node.
type Interface struct {
	Name     string   `json:"name"`
	MAC      string   `json:"mac"`
	Up       bool     `json:"up"`
	Loopback bool     `json:"loopback"`
	Addrs    []string `json:"addrs"`
	// Default marks the interface of the default route, the parent used
	// for cameras unless another one is chosen (D23).
	Default bool   `json:"default"`
	Gateway string `json:"gateway,omitempty"`
}

// NodeInfo describes the node's network.
type NodeInfo struct {
	Interfaces       []Interface `json:"interfaces"`
	DefaultInterface string      `json:"default_interface"`
	DefaultGateway   string      `json:"default_gateway"`
}

// ReadNodeInfo lists interfaces and the default route. It needs no
// privileges.
func ReadNodeInfo() (NodeInfo, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return NodeInfo{}, err
	}
	defIf, defGW := defaultRoute()
	info := NodeInfo{DefaultInterface: defIf, DefaultGateway: defGW}
	for _, i := range ifs {
		it := Interface{
			Name:     i.Name,
			MAC:      i.HardwareAddr.String(),
			Up:       i.Flags&net.FlagUp != 0,
			Loopback: i.Flags&net.FlagLoopback != 0,
			Default:  i.Name == defIf,
		}
		if it.Default {
			it.Gateway = defGW
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().Is4() {
				it.Addrs = append(it.Addrs, p.String())
			}
		}
		info.Interfaces = append(info.Interfaces, it)
	}
	sort.Slice(info.Interfaces, func(a, b int) bool {
		x, y := info.Interfaces[a], info.Interfaces[b]
		if x.Default != y.Default {
			return x.Default
		}
		return x.Name < y.Name
	})
	return info, nil
}

// Lookup returns the interface with the given name.
func (n NodeInfo) Lookup(name string) (Interface, bool) {
	for _, i := range n.Interfaces {
		if i.Name == name {
			return i, true
		}
	}
	return Interface{}, false
}

// defaultRoute reads the IPv4 default route from /proc/net/route.
func defaultRoute() (iface, gateway string) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	best := uint32(1<<32 - 1)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 || fields[1] != "00000000" || fields[7] != "00000000" {
			continue
		}
		metric, err := strconv.ParseUint(fields[6], 10, 32)
		if err != nil {
			continue
		}
		if uint32(metric) <= best {
			best = uint32(metric)
			iface = fields[0]
			gateway = hexIP(fields[2])
		}
	}
	return iface, gateway
}

// hexIP converts the hex of /proc/net/route, in the host's little-endian
// byte order, to dotted form.
func hexIP(s string) string {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 4 {
		return ""
	}
	a := netip.AddrFrom4([4]byte{b[3], b[2], b[1], b[0]})
	if a.IsUnspecified() {
		return ""
	}
	return a.String()
}
