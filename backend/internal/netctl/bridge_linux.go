package netctl

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// bridgeName is the node's interface toward its macvlan cameras (D26).
const bridgeName = "mv-bridge"

// A node cannot reach the macvlan interfaces on its own NIC: the kernel
// hands their frames to the wire, never back to the NIC's own stack. The
// bridge is one more macvlan of the node on the same parent, which reaches
// its siblings. The node routes each camera through it, using its own
// address on the parent as source, and each camera learns that address at
// the bridge's MAC. The bridge answers no ARP, so the LAN keeps seeing the
// node at the NIC's MAC.
type bridge struct {
	parent string
	link   netlink.Link
	// nodeIPs are the node's IPv4 addresses on the parent, with their
	// prefixes: the sources of its traffic to the cameras.
	nodeIPs []netip.Prefix
}

// createBridge creates the bridge on parent, replacing a stale one.
func createBridge(host netns.NsHandle, parent string) (*bridge, error) {
	h, err := netlink.NewHandleAt(host)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	if old, err := h.LinkByName(bridgeName); err == nil {
		_ = h.LinkDel(old)
	}
	p, err := h.LinkByName(parent)
	if err != nil {
		return nil, errorf(CodeNoInterface, "parent interface %s not found", parent)
	}
	b := &bridge{parent: parent}
	addrs, err := h.AddrList(p, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if ip, ok := netip.AddrFromSlice(a.IP.To4()); ok {
			ones, _ := a.Mask.Size()
			b.nodeIPs = append(b.nodeIPs, netip.PrefixFrom(ip, ones))
		}
	}
	if len(b.nodeIPs) == 0 {
		return nil, errorf(CodeInvalid, "%s has no IPv4 address: the node needs one on the cameras' LAN to reach them", parent)
	}
	mv := &netlink.Macvlan{
		LinkAttrs: netlink.LinkAttrs{Name: bridgeName, ParentIndex: p.Attrs().Index, HardwareAddr: randomMAC(), MTU: p.Attrs().MTU},
		Mode:      netlink.MACVLAN_MODE_BRIDGE,
	}
	if err := h.LinkAdd(mv); err != nil {
		if errors.Is(err, unix.EBUSY) {
			return nil, parentBusy(parent, "macvlan")
		}
		return nil, fmt.Errorf("create %s: %w", bridgeName, err)
	}
	if b.link, err = h.LinkByName(bridgeName); err != nil {
		return nil, err
	}
	// Never answer ARP: cameras get the node's address as a static
	// neighbor, and the LAN keeps a single MAC for the node.
	if err := inNamespace(host, func() error {
		return setInetConf(b.link.Attrs().Index, ipv4DevconfARPIgnore, 8)
	}); err != nil {
		_ = h.LinkDel(b.link)
		return nil, fmt.Errorf("arp_ignore on %s: %w", bridgeName, err)
	}
	if err := h.LinkSetUp(b.link); err != nil {
		_ = h.LinkDel(b.link)
		return nil, err
	}
	return b, nil
}

// addrs lists the node addresses the bridge routes from.
func (b *bridge) addrs() []string {
	out := make([]string, len(b.nodeIPs))
	for i, p := range b.nodeIPs {
		out[i] = p.String()
	}
	return out
}

// remove deletes the bridge, and the routes through it with it.
func (b *bridge) remove(host netns.NsHandle) {
	h, err := netlink.NewHandleAt(host)
	if err != nil {
		return
	}
	defer h.Close()
	_ = h.LinkDel(b.link)
}

// source is the node address that reaches ip: the one in its subnet, else
// the first one.
func (b *bridge) source(ip netip.Addr) netip.Addr {
	for _, p := range b.nodeIPs {
		if p.Masked().Contains(ip) {
			return p.Addr()
		}
	}
	return b.nodeIPs[0].Addr()
}

// attach routes a camera through the bridge and gives it the node's
// addresses as static neighbors.
func (b *bridge) attach(host netns.NsHandle, ns *namespace, ip netip.Addr) error {
	h, err := netlink.NewHandleAt(host)
	if err != nil {
		return err
	}
	defer h.Close()
	route := &netlink.Route{
		LinkIndex: b.link.Attrs().Index,
		Dst:       &net.IPNet{IP: net.IP(ip.AsSlice()), Mask: net.CIDRMask(32, 32)},
		Src:       net.IP(b.source(ip).AsSlice()),
		Scope:     netlink.SCOPE_LINK,
	}
	if err := h.RouteReplace(route); err != nil {
		return fmt.Errorf("route to %s through %s: %w", ip, bridgeName, err)
	}
	nh, err := netlink.NewHandleAt(ns.fd)
	if err != nil {
		return err
	}
	defer nh.Close()
	eth, err := nh.LinkByName("eth0")
	if err != nil {
		return err
	}
	for _, p := range b.nodeIPs {
		if err := nh.NeighSet(&netlink.Neigh{
			LinkIndex: eth.Attrs().Index, Family: netlink.FAMILY_V4, State: netlink.NUD_PERMANENT,
			IP: net.IP(p.Addr().AsSlice()), HardwareAddr: b.link.Attrs().HardwareAddr,
		}); err != nil {
			return fmt.Errorf("node address in the camera: %w", err)
		}
	}
	return nil
}

// detach removes a camera's route and, while its namespace lives, the
// node's addresses from its neighbors.
func (b *bridge) detach(host netns.NsHandle, ns *namespace, ip netip.Addr) {
	if h, err := netlink.NewHandleAt(host); err == nil {
		_ = h.RouteDel(&netlink.Route{
			LinkIndex: b.link.Attrs().Index,
			Dst:       &net.IPNet{IP: net.IP(ip.AsSlice()), Mask: net.CIDRMask(32, 32)},
		})
		h.Close()
	}
	if ns == nil || !ns.fd.IsOpen() {
		return
	}
	nh, err := netlink.NewHandleAt(ns.fd)
	if err != nil {
		return
	}
	defer nh.Close()
	eth, err := nh.LinkByName("eth0")
	if err != nil {
		return
	}
	for _, p := range b.nodeIPs {
		_ = nh.NeighDel(&netlink.Neigh{LinkIndex: eth.Attrs().Index, Family: netlink.FAMILY_V4, IP: net.IP(p.Addr().AsSlice())})
	}
}

// removeStaleBridge deletes a bridge left by a previous run; the service
// creates it again if it is still wanted.
func removeStaleBridge() bool {
	l, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return false
	}
	return netlink.LinkDel(l) == nil
}

// ipv4DevconfARPIgnore is IPV4_DEVCONF_ARP_IGNORE of linux/ip.h.
const ipv4DevconfARPIgnore = 19

// setInetConf sets an IPv4 setting of an interface of the current thread's
// namespace over netlink, as /proc/sys/net/ipv4/conf/<if>/ would, which
// containers mount read-only.
func setInetConf(index, key int, value uint32) error {
	req := nl.NewNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(index)
	req.AddData(msg)
	spec := nl.NewRtAttr(unix.IFLA_AF_SPEC, nil)
	inet := spec.AddRtAttr(unix.AF_INET, nil)
	conf := inet.AddRtAttr(unix.IFLA_INET_CONF, nil)
	conf.AddRtAttr(key, nl.Uint32Attr(value))
	req.AddData(spec)
	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}
