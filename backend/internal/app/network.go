package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
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

// Resolution of the targets' host names for the firewalls.
const (
	// resolveKeep is how long an address a name resolved to stays
	// allowed: round-robin and CDN names answer each query with other
	// addresses, and the camera may connect to any of them.
	resolveKeep = 10 * time.Minute
	// resolveTimeout bounds one query.
	resolveTimeout = 2 * time.Second
	// resolveWorkers is how many queries run at once.
	resolveWorkers = 8
	// maxDestinations bounds a firewall, as the helper does.
	maxDestinations = 512
)

// cameraDNS are the DNS servers a camera uses: its own, else those of its
// lease, else the node's; at most domain.MaxDNS.
func cameraDNS(b *cameraBundle, lease []string) []string {
	dns := b.dns()
	if len(dns) == 0 {
		dns = lease
	}
	if len(dns) == 0 {
		dns = netctl.ReadNodeDNS()
	}
	return slices.Clone(dns[:min(len(dns), domain.MaxDNS)])
}

// targetResolver resolves the targets' host names the way each camera
// does: with the camera's own DNS servers, which may answer otherwise than
// the node's (an internal zone, a lease's DNS). Every address a name
// resolved to in the last resolveKeep stays allowed.
type targetResolver struct {
	mu    sync.Mutex
	names map[resolveKey]*resolved
}

// resolveKey is a name asked to one server; "" is the node's resolver.
type resolveKey struct{ server, host string }

type resolved struct {
	at   time.Time            // last query
	seen map[string]time.Time // address, last time an answer had it
}

// lookup returns the IPv4 addresses of host for a camera with these DNS
// servers, querying the ones not asked within fresh. When none of the
// servers knows the name the node's resolver is asked, so a target the
// node reaches is not dropped for a DNS server the node cannot reach.
func (r *targetResolver) lookup(ctx context.Context, servers []string, host string, fresh time.Duration) []string {
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Is4() {
			return []string{a.String()}
		}
		return nil
	}
	node := []resolveKey{{"", host}}
	if len(servers) == 0 {
		return r.addresses(ctx, node, fresh)
	}
	keys := make([]resolveKey, 0, len(servers))
	for _, sv := range servers {
		keys = append(keys, resolveKey{sv, host})
	}
	if out := r.addresses(ctx, keys, fresh); len(out) > 0 {
		return out
	}
	return r.addresses(ctx, node, fresh)
}

// addresses queries what is stale and returns the union of what the keys
// resolved to within resolveKeep.
func (r *targetResolver) addresses(ctx context.Context, keys []resolveKey, fresh time.Duration) []string {
	now := time.Now()
	var wg sync.WaitGroup
	for _, k := range keys {
		r.mu.Lock()
		e := r.names[k]
		stale := e == nil || now.Sub(e.at) >= fresh
		r.mu.Unlock()
		if !stale {
			continue
		}
		wg.Go(func() {
			ips := query4(ctx, k.server, k.host)
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.names == nil {
				r.names = map[resolveKey]*resolved{}
			}
			e := r.names[k]
			if e == nil {
				e = &resolved{seen: map[string]time.Time{}}
				r.names[k] = e
			}
			e.at = now
			for _, ip := range ips {
				e.seen[ip] = now
			}
		})
	}
	wg.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, k := range keys {
		if e := r.names[k]; e != nil {
			for ip, at := range e.seen {
				if now.Sub(at) < resolveKeep && !slices.Contains(out, ip) {
					out = append(out, ip)
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

// forget drops names nobody asked for in resolveKeep: targets deleted or
// servers no camera uses any more.
func (r *targetResolver) forget(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.names {
		if now.Sub(e.at) >= resolveKeep {
			delete(r.names, k)
		}
	}
}

// query4 asks one DNS server, or the node's resolver when server is "",
// for the IPv4 addresses of host. Like the camera's resolver it reads the
// hosts file first.
func query4(ctx context.Context, server, host string) []string {
	r := net.DefaultResolver
	if server != "" {
		r = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(server, "53"))
		}}
	}
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	addrs, err := r.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.Unmap().String())
	}
	return out
}

// destinations is what a camera with these DNS servers may connect to:
// every event target and NAS share of the node, so a target can be tested
// from any camera. Names are resolved in parallel, and those asked within fresh
// are not asked again. It fails only when the targets cannot be read, so
// a passing database error never empties the firewalls.
func (s *Service) destinations(ctx context.Context, dns []string, fresh time.Duration) ([]netctl.Destination, error) {
	rows, err := s.store.R().ListTargets(ctx)
	if err != nil {
		return nil, err
	}
	type hostPort struct {
		host  string
		ports []int
	}
	var targets []hostPort
	for _, t := range rows {
		if host, ports, ok := targetPorts(t.Type, t.ConfigJson); ok {
			targets = append(targets, hostPort{host, ports})
		}
	}
	// NAS shares: NFS finds its ports through the portmapper, so any port
	// of the host.
	shares, err := s.store.R().ListNASHosts(ctx)
	if err != nil {
		return nil, err
	}
	for _, raw := range shares {
		if host := nasHost(raw); host != "" {
			targets = append(targets, hostPort{host, []int{netctl.AnyPort}})
		}
	}
	ips := make([][]string, len(targets))
	sem := make(chan struct{}, resolveWorkers)
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			ips[i] = s.resolver.lookup(ctx, dns, t.host, fresh)
		})
	}
	wg.Wait()
	var out []netctl.Destination
	for i, t := range targets {
		for _, ip := range ips[i] {
			for _, port := range t.ports {
				d := netctl.Destination{IP: ip, Port: port, Proto: "tcp"}
				if !slices.Contains(out, d) && len(out) < maxDestinations {
					out = append(out, d)
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b netctl.Destination) int {
		return cmp.Or(cmp.Compare(a.IP, b.IP), cmp.Compare(a.Port, b.Port), cmp.Compare(a.Proto, b.Proto))
	})
	return out, nil
}

// firewallFor is the firewall a camera starts with. Starting many cameras
// reuses what the others resolved within the last minute.
func (s *Service) firewallFor(ctx context.Context, id string, dns []string) *netctl.Firewall {
	allow, err := s.destinations(ctx, dns, firewallRefresh)
	if err != nil {
		// The refresh adds the targets once the database answers.
		s.log.Warn("the camera starts with no target in its firewall", "camera", id, "error", err)
	}
	return &netctl.Firewall{Allow: allow, DNS: dns}
}

// refreshFirewalls brings the firewalls of the running cameras (every one,
// or only those given) up to date: after the targets changed, a camera's
// DNS servers changed, or periodically. fresh is how recent a name's
// resolution may be to be reused.
func (s *Service) refreshFirewalls(ctx context.Context, fresh time.Duration, only ...*session) {
	if s.rt.Kind() != netctl.KindNetns {
		return
	}
	sessions := only
	if len(only) == 0 {
		s.mu.Lock()
		for _, ss := range s.sessions {
			sessions = append(sessions, ss)
		}
		s.mu.Unlock()
	}
	allowFor := map[string][]netctl.Destination{} // by DNS servers
	sem := make(chan struct{}, resolveWorkers)
	var wg sync.WaitGroup
	for _, ss := range sessions {
		ss.mu.Lock()
		dns, active, applied := ss.dns, ss.state.Active(), ss.firewall
		ss.mu.Unlock()
		if !active || applied == nil {
			continue
		}
		key := strings.Join(dns, ",")
		allow, ok := allowFor[key]
		if !ok {
			var err error
			if allow, err = s.destinations(ctx, dns, fresh); err != nil {
				s.log.Warn("cannot read the targets; the firewalls stay as they are", "error", err)
				return
			}
			allowFor[key] = allow
		}
		fw := netctl.Firewall{Allow: allow, DNS: dns}
		if firewallEqual(applied, &fw) {
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			ss.setFirewall(ctx, fw)
		})
	}
	wg.Wait()
}

func (s *Service) refreshFirewallsLater() {
	s.goBackground(func(ctx context.Context) { s.refreshFirewalls(ctx, firewallRefresh) })
}

func firewallEqual(a, b *netctl.Firewall) bool {
	return slices.Equal(a.Allow, b.Allow) && slices.Equal(a.DNS, b.DNS)
}

// setFirewall replaces a running camera's firewall and remembers it.
func (ss *session) setFirewall(ctx context.Context, fw netctl.Firewall) {
	if err := ss.s.rt.SetFirewall(ctx, ss.id, fw); err != nil {
		ss.s.log.Warn("cannot update the camera's firewall", "camera", ss.id, "error", err)
		return
	}
	ss.mu.Lock()
	ss.firewall = &fw
	ss.mu.Unlock()
}

// networkLoop keeps what depends on the LAN up to date: the firewalls, for
// target host names whose addresses change, and the node bridge, for the
// node's own addresses.
func (s *Service) networkLoop(ctx context.Context) {
	t := time.NewTicker(firewallRefresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.refreshFirewalls(ctx, 0)
			s.resolver.forget(time.Now())
			s.checkBridge(ctx)
		}
	}
}

// addressResult is the address a DHCP camera ended up with.
type addressResult struct {
	ip     string
	source domain.IPSource
	dns    []string
}

// acceptLease checks a lease and has the helper set it. A lease that
// keeps the camera's address and changes its prefix or router is applied
// the same way: the helper does not probe an address the camera holds.
func (s *Service) acceptLease(ctx context.Context, b *cameraBundle, l ipc.Lease) (addressResult, error) {
	ip, err := netip.ParseAddr(l.IP)
	if err != nil || !ip.Is4() {
		return addressResult{}, fmt.Errorf("%q is not an IPv4 address", l.IP)
	}
	if l.Prefix < 1 || l.Prefix > 30 {
		return addressResult{}, fmt.Errorf("prefix /%d is not usable", l.Prefix)
	}
	if err := s.takeAddress(ctx, b, ip, l.Prefix, l.Router); err != nil {
		return addressResult{}, err
	}
	var dns []string
	for _, d := range l.DNS {
		if a, err := netip.ParseAddr(d); err == nil && a.Is4() && len(dns) < domain.MaxDNS {
			dns = append(dns, a.String())
		}
	}
	return addressResult{ip: ip.String(), source: domain.SourceDHCP, dns: dns}, nil
}

// factoryAddress takes the profile's factory address, as a real camera does
// when DHCP fails (D24).
func (s *Service) factoryAddress(ctx context.Context, b *cameraBundle, why string) (addressResult, *failure) {
	f := b.doc.Identity.Factory.Network
	ip, err := netip.ParseAddr(f.IP)
	if err != nil || !ip.Is4() {
		return addressResult{}, failed(ReasonNoFactory, "%s, and the profile has no factory address to fall back to", why)
	}
	prefix := 24
	if p, err := domain.MaskToPrefix(f.Mask); err == nil && p >= 1 && p <= 30 {
		prefix = p
	}
	if err := s.takeAddress(ctx, b, ip, prefix, f.Gateway); err != nil {
		return addressResult{}, failed(ReasonFactoryInUse, "%s, and the factory address %s cannot be used: %v", why, ip, err)
	}
	return addressResult{ip: ip.String(), source: domain.SourceFactory}, nil
}

// takeAddress gives a DHCP camera an address and its router, when the
// router is in the subnet. The address must not be the node's, another
// camera's static one or one another running camera holds.
func (s *Service) takeAddress(ctx context.Context, b *cameraBundle, ip netip.Addr, prefix int, router string) error {
	if err := s.addressFree(ctx, b.cam.ID, ip); err != nil {
		return err
	}
	undo, err := s.claimAddress(b.cam.ID, ip)
	if err != nil {
		return err
	}
	a := netctl.AddressSpec{ID: b.cam.ID, IP: ip.String(), Prefix: prefix, Force: store.Bool(b.net.Force)}
	if gw, ok := domain.GatewayIn(ip, prefix, router); ok {
		a.Gateway = gw.String()
	}
	if err := s.rt.SetAddress(ctx, a); err != nil {
		undo()
		return errors.New(launchFailure(err).text)
	}
	return nil
}

// addressFree refuses the node's own addresses and another camera's static
// one; a static camera may start at any time.
func (s *Service) addressFree(ctx context.Context, self string, ip netip.Addr) error {
	if info, err := nodeInfo(); err == nil {
		if name, ok := nodeInterfaceWith(info, ip); ok {
			return fmt.Errorf("%s belongs to the node itself (%s)", ip, name)
		}
	}
	if other, err := s.store.R().CameraIDByIP(ctx, ip.String()); err == nil && other != self {
		return fmt.Errorf("%s is the static address of another camera", ip)
	}
	return nil
}

// nodeInterfaceWith returns the node's interface that has ip.
func nodeInterfaceWith(info netctl.NodeInfo, ip netip.Addr) (string, bool) {
	for _, i := range info.Interfaces {
		for _, a := range i.Addrs {
			if p, err := netip.ParsePrefix(a); err == nil && p.Addr() == ip {
				return i.Name, true
			}
		}
	}
	return "", false
}

// claimAddress reserves ip for a running camera, in place of what it held,
// so two cameras never take one address; what cameras held when they
// stopped does not count (the database keeps it only to show it). undo
// gives back the previous address when the new one could not be set.
func (s *Service) claimAddress(id string, ip netip.Addr) (undo func(), err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner, ok := s.claims[ip]; ok && owner != id {
		return nil, fmt.Errorf("%s is used by another running camera", ip)
	}
	var prev netip.Addr
	for a, owner := range s.claims {
		if owner == id {
			prev = a
			delete(s.claims, a)
		}
	}
	s.claims[ip] = id
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.claims[ip] == id {
			delete(s.claims, ip)
		}
		if _, taken := s.claims[prev]; prev.IsValid() && !taken {
			s.claims[prev] = id
		}
	}, nil
}

// releaseAddresses frees the address of a camera whose session ended.
func (s *Service) releaseAddresses(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for a, owner := range s.claims {
		if owner == id {
			delete(s.claims, a)
		}
	}
}

// saveAddress keeps the address a camera holds, for the views.
func (s *Service) saveAddress(id, ip string, source domain.IPSource) {
	if err := s.store.W().SetCameraAddress(context.Background(), db.SetCameraAddressParams{CameraID: id, Ip: ip, IpSource: string(source)}); err != nil {
		s.log.Warn("cannot save the camera's address", "camera", id, "error", err)
	}
}

// defaultParent is the network card cameras without one of their own
// attach to: the settings' choice, else MOCKVISION_PARENT_IF, else the
// default route's.
func (s *Service) defaultParent(ctx context.Context) string {
	return s.defaultParentFor(s.Settings(ctx))
}

func (s *Service) defaultParentFor(set Settings) string {
	if p := cmp.Or(set.ParentInterface, s.opts.ParentInterface); p != "" {
		return p
	}
	info, _ := nodeInfo()
	return info.DefaultInterface
}

// parentUse is who uses a network card: its cameras, by mode, and whether
// the node bridge is on it.
type parentUse struct {
	cams   map[string][]string // mode, camera names
	bridge bool
}

// parentsInUse places every camera but skip on its network card, with the
// default one that set gives.
func (s *Service) parentsInUse(ctx context.Context, set Settings, skip string) (map[string]*parentUse, error) {
	q := s.store.R()
	nets, err := q.ListCameraNetworks(ctx)
	if err != nil {
		return nil, err
	}
	cams, err := q.ListCameras(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(cams))
	for _, c := range cams {
		names[c.ID] = c.Name
	}
	def := s.defaultParentFor(set)
	out := map[string]*parentUse{}
	use := func(parent string) *parentUse {
		if out[parent] == nil {
			out[parent] = &parentUse{cams: map[string][]string{}}
		}
		return out[parent]
	}
	for _, n := range nets {
		if n.CameraID == skip {
			continue
		}
		u := use(cmp.Or(n.ParentIf, def))
		mode := cmp.Or(n.Mode, string(domain.NetMacvlan))
		u.cams[mode] = append(u.cams[mode], names[n.CameraID])
	}
	if set.NodeBridge {
		use(def).bridge = true
	}
	return out, nil
}

// modeConflict explains why a camera cannot take mode on parent: the
// kernel gives a network card macvlan or ipvlan children, not both, and
// the node bridge is a macvlan (D26, D27). "" when it can.
func (u *parentUse) modeConflict(parent, mode string) string {
	if u == nil {
		return ""
	}
	other := string(domain.NetIPvlan)
	if mode == string(domain.NetIPvlan) {
		other = string(domain.NetMacvlan)
	}
	if names := u.cams[other]; len(names) > 0 {
		return fmt.Sprintf("%s already has %s cameras (%s): a network card takes macvlan or ipvlan cameras, not both", parent, other, nameList(names))
	}
	if mode == string(domain.NetIPvlan) && u.bridge {
		return fmt.Sprintf("the node bridge, a macvlan, is on %s: turn it off or give ipvlan cameras another network card", parent)
	}
	return ""
}

// nameList names up to three cameras.
func nameList(names []string) string {
	slices.Sort(names)
	if len(names) > 3 {
		return fmt.Sprintf("%s and %d more", strings.Join(names[:3], ", "), len(names)-3)
	}
	return strings.Join(names, ", ")
}

// checkNetworkSettings refuses a default network card or a bridge that
// would put the cameras where the kernel or the access point cannot take
// them: macvlan and ipvlan on one card (or ipvlan beside the bridge), or
// macvlan cameras on Wi-Fi.
func (s *Service) checkNetworkSettings(ctx context.Context, set Settings, v *domain.ValidationError) error {
	use, err := s.parentsInUse(ctx, set, "")
	if err != nil {
		return err
	}
	for _, parent := range slices.Sorted(maps.Keys(use)) {
		u := use[parent]
		macvlan, ipvlan := u.cams[string(domain.NetMacvlan)], u.cams[string(domain.NetIPvlan)]
		switch {
		case len(macvlan) > 0 && len(ipvlan) > 0:
			v.Add("parent_interface", "%s would carry macvlan cameras (%s) and ipvlan cameras (%s): a network card takes one kind", parent, nameList(macvlan), nameList(ipvlan))
		case u.bridge && len(ipvlan) > 0:
			v.Add("node_bridge", "the ipvlan cameras %s use %s, and the bridge is a macvlan: a network card takes one kind", nameList(ipvlan), parent)
		}
	}
	def := s.defaultParentFor(set)
	info, _ := nodeInfo()
	if iface, ok := info.Lookup(def); ok && iface.Wireless {
		nets, err := s.store.R().ListCameraNetworks(ctx)
		if err != nil {
			return err
		}
		var following []string
		for _, n := range nets {
			if n.ParentIf == "" && cmp.Or(n.Mode, string(domain.NetMacvlan)) == string(domain.NetMacvlan) {
				following = append(following, n.CameraID)
			}
		}
		if len(following) > 0 {
			names, err := s.cameraNames(ctx, following)
			if err != nil {
				return err
			}
			v.Add("parent_interface", "%s is a Wi-Fi interface and the macvlan cameras %s follow the default network card: give them another one or switch them to ipvlan", def, nameList(names))
		}
	}
	return nil
}

// cameraNames returns the names of some cameras.
func (s *Service) cameraNames(ctx context.Context, ids []string) ([]string, error) {
	cams, err := s.store.R().ListCameras(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range cams {
		if slices.Contains(ids, c.ID) {
			out = append(out, c.Name)
		}
	}
	return out, nil
}

// applyBridge makes the helper's bridge match the settings (D26).
func (s *Service) applyBridge(ctx context.Context) {
	if s.rt.Kind() != netctl.KindNetns {
		return
	}
	s.bridgeMu.Lock()
	defer s.bridgeMu.Unlock()
	set := s.Settings(ctx)
	parent := s.defaultParentFor(set)
	spec := netctl.BridgeSpec{Enabled: set.NodeBridge, Parent: parent}
	seen := parentIPv4(parent)
	st, err := s.rt.SetBridge(ctx, spec)
	if err != nil {
		st = netctl.BridgeState{Error: err.Error()}
	}
	if set.NodeBridge && st.Error != "" {
		s.log.Warn("the node cannot reach its cameras", "error", st.Error)
	}
	s.mu.Lock()
	s.bridge, s.bridgeSeen = st, seen
	s.mu.Unlock()
}

// checkBridge sets the bridge again when the node's addresses on its
// network card changed since it was set: its routes and the cameras'
// neighbor entries use them. A bridge that failed for want of an address
// gets another try once the card has one.
func (s *Service) checkBridge(ctx context.Context) {
	if s.rt.Kind() != netctl.KindNetns || !s.Settings(ctx).NodeBridge {
		return
	}
	parent := s.defaultParent(ctx)
	s.mu.Lock()
	seen := s.bridgeSeen
	s.mu.Unlock()
	if now := parentIPv4(parent); !slices.Equal(now, seen) {
		s.log.Info("the node's addresses changed; setting the bridge again", "interface", parent, "addresses", now)
		s.applyBridge(ctx)
	}
}

// parentIPv4 lists the IPv4 addresses of a network card, sorted.
func parentIPv4(name string) []string {
	info, err := nodeInfo()
	if err != nil {
		return nil
	}
	iface, ok := info.Lookup(name)
	if !ok {
		return nil
	}
	var out []string
	for _, a := range iface.Addrs {
		if p, err := netip.ParsePrefix(a); err == nil && p.Addr().Is4() {
			out = append(out, p.String())
		}
	}
	slices.Sort(out)
	return out
}

// BridgeStatus reports the node's access to its cameras.
func (s *Service) BridgeStatus() netctl.BridgeState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bridge
}
