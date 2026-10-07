package netctl

import (
	"encoding/binary"
	"fmt"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// firewallTable is the nftables table of a camera's namespace.
const firewallTable = "mockvision"

// allowedSet holds what the camera may connect to, as
// address . protocol . port: its targets and its DNS servers.
const allowedSet = "allowed"

// hostsSet holds the hosts the camera may reach on any port of a protocol,
// as address . protocol: FTP servers, whose passive data connections go
// to ports they choose.
const hostsSet = "allowed_hosts"

// applyFirewall replaces the firewall of a camera namespace in one
// transaction. What the camera starts only leaves toward the node's event
// targets, its DNS servers and DHCP servers; answers to its clients and the
// traffic of its own ports (RTP over UDP among them) pass, as does
// loopback. Anything else is counted and dropped. Incoming traffic is not
// filtered: clients reach a camera from anywhere, as a real one.
func applyFirewall(nsfd int, sockets []SocketSpec, dhcp bool, fw *Firewall) error {
	c, err := nftables.New(nftables.WithNetNSFd(nsfd))
	if err != nil {
		return err
	}
	tables, err := c.ListTablesOfFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return fmt.Errorf("nftables unavailable: %w", err)
	}
	for _, t := range tables {
		if t.Name == firewallTable {
			c.DelTable(t)
		}
	}
	t := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: firewallTable})
	// One lookup in a set, however many targets the node has.
	set := &nftables.Set{
		Table: t, Name: allowedSet, Concatenation: true,
		KeyType: nftables.MustConcatSetType(nftables.TypeIPAddr, nftables.TypeInetProto, nftables.TypeInetService),
	}
	if err := c.AddSet(set, allowedElements(fw)); err != nil {
		return fmt.Errorf("nftables set: %w", err)
	}
	hosts := &nftables.Set{
		Table: t, Name: hostsSet, Concatenation: true,
		KeyType: nftables.MustConcatSetType(nftables.TypeIPAddr, nftables.TypeInetProto),
	}
	if err := c.AddSet(hosts, hostElements(fw)); err != nil {
		return fmt.Errorf("nftables set: %w", err)
	}
	policy := nftables.ChainPolicyDrop
	out := c.AddChain(&nftables.Chain{
		Name: "output", Table: t, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookOutput, Priority: nftables.ChainPriorityFilter, Policy: &policy,
	})
	for _, r := range firewallRules(sockets, dhcp, set, hosts) {
		c.AddRule(&nftables.Rule{Table: t, Chain: out, Exprs: r})
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("nftables: %w", err)
	}
	return nil
}

// allowedElements are the set's keys: every destination, and TCP and UDP
// port 53 of every DNS server.
func allowedElements(fw *Firewall) []nftables.SetElement {
	if fw == nil {
		return nil
	}
	var out []nftables.SetElement
	for _, d := range fw.DNS {
		for _, proto := range []string{"udp", "tcp"} {
			out = append(out, nftables.SetElement{Key: allowedKey(d, proto, 53)})
		}
	}
	for _, d := range fw.Allow {
		if d.Port != AnyPort {
			out = append(out, nftables.SetElement{Key: allowedKey(d.IP, d.Proto, d.Port)})
		}
	}
	return out
}

// hostElements are the keys of the hosts set: the destinations open on
// any port.
func hostElements(fw *Firewall) []nftables.SetElement {
	if fw == nil {
		return nil
	}
	var out []nftables.SetElement
	for _, d := range fw.Allow {
		if d.Port == AnyPort {
			out = append(out, nftables.SetElement{Key: hostKey(d.IP, d.Proto)})
		}
	}
	return out
}

// hostKey is an address . protocol key.
func hostKey(ip, proto string) []byte {
	a := netip.MustParseAddr(ip).As4()
	key := make([]byte, 8)
	copy(key[0:4], a[:])
	key[4] = protoNumber(proto)
	return key
}

// allowedKey is an address . protocol . port key. Each field of a
// concatenation takes whole 32-bit registers, zero-padded.
func allowedKey(ip, proto string, port int) []byte {
	a := netip.MustParseAddr(ip).As4()
	key := make([]byte, 12)
	copy(key[0:4], a[:])
	key[4] = protoNumber(proto)
	binary.BigEndian.PutUint16(key[8:10], uint16(port))
	return key
}

// firewallRules builds the output chain, in order.
func firewallRules(sockets []SocketSpec, dhcp bool, set, hosts *nftables.Set) [][]expr.Any {
	accept := []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}
	rules := [][]expr.Any{
		// oifname "lo" accept
		rule([]expr.Any{&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname("lo")}}, accept),
		// ct state established,related accept
		rule([]expr.Any{
			&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4,
				Mask: native32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED), Xor: native32(0)},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: native32(0)},
		}, accept),
	}
	// Traffic from the camera's own ports: RTP to the client ports an RTSP
	// client asked for starts on the camera side.
	for _, s := range sockets {
		rules = append(rules, rule(l4(s.Network), port(srcPort, s.Port), accept))
	}
	if dhcp {
		rules = append(rules, rule(l4("udp"), port(dstPort, 67), accept))
	}
	// ip daddr . meta l4proto . th dport @allowed accept: the address in
	// register 1, the protocol and the port in the 32-bit registers after it.
	rules = append(rules, rule([]expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: reg32(1)},
		&expr.Payload{DestRegister: reg32(2), Base: expr.PayloadBaseTransportHeader, Offset: dstPort, Len: 2},
		&expr.Lookup{SourceRegister: 1, SetName: set.Name, SetID: set.ID},
	}, accept))
	// ip daddr . meta l4proto @allowed_hosts accept
	rules = append(rules, rule([]expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: reg32(1)},
		&expr.Lookup{SourceRegister: 1, SetName: hosts.Name, SetID: hosts.ID},
	}, accept))
	// counter drop: the policy drops too, but a counter shows what was
	// refused (nft list ruleset inside the namespace).
	return append(rules, []expr.Any{&expr.Counter{}, &expr.Verdict{Kind: expr.VerdictDrop}})
}

// reg32 is the n-th 32-bit register (NFT_REG32_00 is 8): the ones that
// follow the first four bytes of register 1.
func reg32(n uint32) uint32 { return 8 + n }

// rule joins the parts of a rule.
func rule(parts ...[]expr.Any) []expr.Any {
	var out []expr.Any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// protoNumber is the IP protocol number of tcp or udp.
func protoNumber(proto string) byte {
	if proto == "udp" {
		return unix.IPPROTO_UDP
	}
	return unix.IPPROTO_TCP
}

// l4 matches the transport protocol.
func l4(proto string) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{protoNumber(proto)}},
	}
}

// Offsets of the ports in the TCP and UDP headers.
const (
	srcPort = 0
	dstPort = 2
)

// port matches the source or destination port.
func port(offset uint32, p int) []expr.Any {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, uint16(p))
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: offset, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: b},
	}
}

// ifname pads an interface name as the kernel stores it.
func ifname(n string) []byte {
	b := make([]byte, 16)
	copy(b, n)
	return b
}

func native32(v uint32) []byte {
	b := make([]byte, 4)
	binary.NativeEndian.PutUint32(b, v)
	return b
}

// offlineTable is the table that takes a camera off the network.
const offlineTable = "mockvision_offline"

// setOffline takes a camera namespace off the network, or back: an ip
// table drops everything in and out but loopback, and its interface stops
// speaking ARP, so the camera answers nobody, not even ARP, and reaches
// nobody, as a device whose cable was pulled. Its addresses and routes
// stay as they were.
func setOffline(nsfd int, offline bool) error {
	c, err := nftables.New(nftables.WithNetNSFd(nsfd))
	if err != nil {
		return err
	}
	tables, err := c.ListTablesOfFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return fmt.Errorf("nftables unavailable: %w", err)
	}
	for _, t := range tables {
		if t.Name == offlineTable {
			c.DelTable(t)
		}
	}
	if offline {
		t := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: offlineTable})
		policy := nftables.ChainPolicyDrop
		for _, hook := range []*nftables.ChainHook{nftables.ChainHookInput, nftables.ChainHookOutput} {
			name, iface := "input", expr.MetaKeyIIFNAME
			if hook == nftables.ChainHookOutput {
				name, iface = "output", expr.MetaKeyOIFNAME
			}
			ch := c.AddChain(&nftables.Chain{
				Name: name, Table: t, Type: nftables.ChainTypeFilter,
				Hooknum: hook, Priority: nftables.ChainPriorityFilter, Policy: &policy,
			})
			// meta iifname/oifname "lo" accept
			c.AddRule(&nftables.Rule{Table: t, Chain: ch, Exprs: []expr.Any{
				&expr.Meta{Key: iface, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname("lo")},
				&expr.Verdict{Kind: expr.VerdictAccept},
			}})
		}
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("nftables: %w", err)
	}
	nh, err := netlink.NewHandleAt(netns.NsHandle(nsfd))
	if err != nil {
		return err
	}
	defer nh.Close()
	eth, err := nh.LinkByName("eth0")
	if err != nil {
		return err
	}
	if offline {
		return nh.LinkSetARPOff(eth)
	}
	return nh.LinkSetARPOn(eth)
}
