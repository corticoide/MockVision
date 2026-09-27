package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/netctl"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
)

// dhcpWait is how long a DHCP camera may take to report a lease or its
// failure before the service takes the profile's factory address (D24);
// the camera itself gives up sooner.
const dhcpWait = 45 * time.Second

// firewallRefresh is how often the firewalls are brought up to date with
// what target host names resolve to.
const firewallRefresh = time.Minute

// cameraDNS are the DNS servers a camera uses: its own, else those of its
// lease, else the node's.
func cameraDNS(b *cameraBundle, lease []string) []string {
	var own []string
	_ = json.Unmarshal([]byte(b.net.DnsJson), &own)
	switch {
	case len(own) > 0:
		return own
	case len(lease) > 0:
		return lease
	}
	return netctl.ReadNodeDNS()
}

// firewallFor is what a camera may connect to: every event target of the
// node, so a target can be tested from any camera, and its DNS servers.
func (s *Service) firewallFor(ctx context.Context, dns []string) *netctl.Firewall {
	fw := &netctl.Firewall{Allow: s.targetDestinations(ctx), DNS: slices.Clone(dns)}
	if len(fw.DNS) > 3 {
		fw.DNS = fw.DNS[:3]
	}
	return fw
}

// targetDestinations resolves the targets' URLs to IPv4 addresses and
// ports. A host name that does not resolve now is skipped until the next
// refresh; deliveries to it fail meanwhile, as they would anyway.
func (s *Service) targetDestinations(ctx context.Context) []netctl.Destination {
	rows, err := s.store.R().ListTargets(ctx)
	if err != nil {
		return nil
	}
	seen := map[netctl.Destination]bool{}
	var out []netctl.Destination
	for _, t := range rows {
		var cfg targetConfig
		if json.Unmarshal([]byte(t.ConfigJson), &cfg) != nil {
			continue
		}
		u, err := url.Parse(cfg.URL)
		if err != nil || u.Hostname() == "" {
			continue
		}
		port := 80
		if u.Scheme == "https" {
			port = 443
		}
		if p := u.Port(); p != "" {
			if port, err = strconv.Atoi(p); err != nil {
				continue
			}
		}
		for _, ip := range resolve4(ctx, u.Hostname()) {
			d := netctl.Destination{IP: ip, Port: port, Proto: "tcp"}
			if !seen[d] && len(out) < 512 {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	return out
}

// resolve4 returns the IPv4 addresses of a host, itself when it is one.
func resolve4(ctx context.Context, host string) []string {
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Is4() {
			return []string{a.String()}
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		out = append(out, a.Unmap().String())
	}
	return out
}

// refreshFirewalls brings every running camera's firewall up to date, after
// the targets changed or periodically.
func (s *Service) refreshFirewalls(ctx context.Context) {
	if s.rt.Kind() != "netns" {
		return
	}
	s.mu.Lock()
	sessions := make([]*session, 0, len(s.sessions))
	for _, ss := range s.sessions {
		sessions = append(sessions, ss)
	}
	s.mu.Unlock()
	allow := s.targetDestinations(ctx)
	for _, ss := range sessions {
		ss.mu.Lock()
		dns, active, applied := ss.dns, ss.state.Active(), ss.firewall
		ss.mu.Unlock()
		if !active || applied == nil {
			continue
		}
		fw := netctl.Firewall{Allow: allow, DNS: dns}
		if firewallEqual(applied, &fw) {
			continue
		}
		if err := s.rt.SetFirewall(ctx, ss.id, fw); err != nil {
			s.log.Warn("cannot update the camera's firewall", "camera", ss.id, "error", err)
			continue
		}
		ss.mu.Lock()
		ss.firewall = &fw
		ss.mu.Unlock()
	}
}

func (s *Service) refreshFirewallsLater() {
	s.goBackground(s.refreshFirewalls)
}

func firewallEqual(a, b *netctl.Firewall) bool {
	return slices.Equal(a.Allow, b.Allow) && slices.Equal(a.DNS, b.DNS)
}

// firewallLoop refreshes the firewalls, for target host names whose
// addresses change.
func (s *Service) firewallLoop(ctx context.Context) {
	t := time.NewTicker(firewallRefresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.refreshFirewalls(ctx)
		}
	}
}

// waitAddress gives a DHCP camera its address before it is configured:
// the one it leased, validated here and probed by the helper, or the
// profile's factory one when no server answered (D24). A refused lease is
// declined and the camera asks again.
func (ss *session) waitAddress(ctx context.Context, b *cameraBundle, conn *ipc.Conn) (addressResult, string) {
	s := ss.s
	ss.setState(domain.StateStarting, "waiting for a DHCP lease")
	deadline := time.NewTimer(dhcpWait)
	defer deadline.Stop()
	for {
		select {
		case l := <-ss.leases:
			res, err := s.acceptLease(ctx, b, l)
			if err == nil {
				return res, ""
			}
			s.log.Info("refused a DHCP lease", "camera", ss.id, "ip", l.IP, "reason", err)
			dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_ = conn.Request(dctx, ipc.TypeDHCPDecline, ipc.DHCPDecline{IP: l.IP, Reason: err.Error()}, nil)
			cancel()
		case reason := <-ss.dhcpFailed:
			s.log.Info("no DHCP lease; taking the factory address", "camera", ss.id, "reason", reason)
			return s.factoryAddress(ctx, b, "no DHCP server answered")
		case <-deadline.C:
			return s.factoryAddress(ctx, b, "no DHCP lease within "+dhcpWait.String())
		case reason := <-ss.stopCh:
			return addressResult{}, "stop:" + reason
		case <-conn.Done():
			return addressResult{}, s.exitReason(ss.id, "the camera process exited while leasing its address")
		}
	}
}

// addressResult is the address a DHCP camera ended up with.
type addressResult struct {
	ip     string
	source domain.IPSource
	dns    []string
}

// acceptLease checks a lease and has the helper set it.
func (s *Service) acceptLease(ctx context.Context, b *cameraBundle, l ipc.Lease) (addressResult, error) {
	ip, err := netip.ParseAddr(l.IP)
	if err != nil || !ip.Is4() {
		return addressResult{}, fmt.Errorf("%q is not an IPv4 address", l.IP)
	}
	if l.Prefix < 1 || l.Prefix > 30 {
		return addressResult{}, fmt.Errorf("prefix /%d is not usable", l.Prefix)
	}
	if err := s.addressFree(ctx, b.cam.ID, ip); err != nil {
		return addressResult{}, err
	}
	a := netctl.AddressSpec{ID: b.cam.ID, IP: ip.String(), Prefix: l.Prefix, Force: store.Bool(b.net.Force)}
	if gw, err := netip.ParseAddr(l.Router); err == nil && gw.Is4() && netip.PrefixFrom(ip, l.Prefix).Masked().Contains(gw) && gw != ip {
		a.Gateway = gw.String()
	}
	if err := s.rt.SetAddress(ctx, a); err != nil {
		return addressResult{}, errors.New(launchReason(err))
	}
	var dns []string
	for _, d := range l.DNS {
		if a, err := netip.ParseAddr(d); err == nil && a.Is4() {
			dns = append(dns, a.String())
		}
	}
	return addressResult{ip: a.IP, source: domain.SourceDHCP, dns: dns}, nil
}

// addressFree refuses an address the node or another camera has.
func (s *Service) addressFree(ctx context.Context, self string, ip netip.Addr) error {
	if info, err := nodeInfo(); err == nil {
		for _, i := range info.Interfaces {
			for _, a := range i.Addrs {
				if p, err := netip.ParsePrefix(a); err == nil && p.Addr() == ip {
					return fmt.Errorf("%s belongs to the node itself", ip)
				}
			}
		}
	}
	q := s.store.R()
	if other, err := q.CameraIDByIP(ctx, ip.String()); err == nil && other != self {
		return fmt.Errorf("%s is the address of another camera", ip)
	}
	if _, err := q.CameraIDByActualIP(ctx, db.CameraIDByActualIPParams{Ip: ip.String(), CameraID: self}); err == nil {
		return fmt.Errorf("%s is used by another camera", ip)
	}
	return nil
}

// factoryAddress takes the profile's factory address, as a real camera does
// when DHCP fails (D24).
func (s *Service) factoryAddress(ctx context.Context, b *cameraBundle, why string) (addressResult, string) {
	f := b.doc.Identity.Factory.Network
	ip, err := netip.ParseAddr(f.IP)
	if err != nil {
		return addressResult{}, why + ", and the profile has no factory address to fall back to"
	}
	prefix := 24
	if f.Mask != "" {
		if p, err := domain.MaskToPrefix(f.Mask); err == nil {
			prefix = p
		}
	}
	if err := s.addressFree(ctx, b.cam.ID, ip); err != nil {
		return addressResult{}, fmt.Sprintf("%s, and the factory address %s cannot be used: %v", why, ip, err)
	}
	a := netctl.AddressSpec{ID: b.cam.ID, IP: ip.String(), Prefix: prefix, Force: store.Bool(b.net.Force)}
	if gw, err := netip.ParseAddr(f.Gateway); err == nil && netip.PrefixFrom(ip, prefix).Masked().Contains(gw) && gw != ip {
		a.Gateway = gw.String()
	}
	if err := s.rt.SetAddress(ctx, a); err != nil {
		return addressResult{}, fmt.Sprintf("%s, and the factory address %s cannot be used: %s", why, ip, launchReason(err))
	}
	return addressResult{ip: a.IP, source: domain.SourceFactory}, ""
}

// saveAddress keeps the address a camera holds, for the views.
func (s *Service) saveAddress(id, ip string, source domain.IPSource) {
	if err := s.store.W().SetCameraAddress(context.Background(), db.SetCameraAddressParams{CameraID: id, Ip: ip, IpSource: string(source)}); err != nil {
		s.log.Warn("cannot save the camera's address", "camera", id, "error", err)
	}
}

// bridgeMu serializes the changes of the node's bridge.
var bridgeMu sync.Mutex

// applyBridge makes the helper's bridge match the settings (D26).
func (s *Service) applyBridge(ctx context.Context) {
	if s.rt.Kind() != "netns" {
		return
	}
	bridgeMu.Lock()
	defer bridgeMu.Unlock()
	set := s.Settings(ctx)
	spec := netctl.BridgeSpec{Enabled: set.NodeBridge, Parent: s.defaultParent(ctx)}
	st, err := s.rt.SetBridge(ctx, spec)
	if err != nil {
		st = netctl.BridgeState{Error: err.Error()}
	}
	if set.NodeBridge && st.Error != "" {
		s.log.Warn("the node cannot reach its cameras", "error", st.Error)
	}
	s.mu.Lock()
	s.bridge = st
	s.mu.Unlock()
}

// BridgeStatus reports the node's access to its cameras.
func (s *Service) BridgeStatus() netctl.BridgeState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bridge
}
