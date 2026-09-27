package netctl

import (
	"encoding/binary"
	"fmt"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// firewallTable is the nftables table of a camera's namespace.
const firewallTable = "mockvision"

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
	policy := nftables.ChainPolicyDrop
	out := c.AddChain(&nftables.Chain{
		Name: "output", Table: t, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookOutput, Priority: nftables.ChainPriorityFilter, Policy: &policy,
	})
	for _, r := range firewallRules(sockets, dhcp, fw) {
		c.AddRule(&nftables.Rule{Table: t, Chain: out, Exprs: r})
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("nftables: %w", err)
	}
	return nil
}

// firewallRules builds the output chain, in order.
func firewallRules(sockets []SocketSpec, dhcp bool, fw *Firewall) [][]expr.Any {
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
	if fw != nil {
		for _, d := range fw.DNS {
			for _, proto := range []string{"udp", "tcp"} {
				rules = append(rules, rule(daddr(d), l4(proto), port(dstPort, 53), accept))
			}
		}
		for _, d := range fw.Allow {
			rules = append(rules, rule(daddr(d.IP), l4(d.Proto), port(dstPort, d.Port), accept))
		}
	}
	// counter drop: the policy drops too, but a counter shows what was
	// refused (nft list ruleset inside the namespace).
	return append(rules, []expr.Any{&expr.Counter{}, &expr.Verdict{Kind: expr.VerdictDrop}})
}

// rule joins the parts of a rule.
func rule(parts ...[]expr.Any) []expr.Any {
	var out []expr.Any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// l4 matches the transport protocol.
func l4(proto string) []expr.Any {
	p := byte(unix.IPPROTO_TCP)
	if proto == "udp" {
		p = unix.IPPROTO_UDP
	}
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{p}},
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

// daddr matches the destination IPv4 address.
func daddr(ip string) []expr.Any {
	a := netip.MustParseAddr(ip).As4()
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: a[:]},
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
