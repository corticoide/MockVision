package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/netctl"
	"github.com/corticoide/mockvision/backend/internal/store/db"
)

// addrRuntime records the addresses the service sets, and may refuse them
// as the helper does when a probe finds the address in use.
type addrRuntime struct {
	netctl.Runtime
	mu   sync.Mutex
	set  []netctl.AddressSpec
	fail map[string]error
}

// newAddrService is a test service whose runtime is an addrRuntime.
func newAddrService(t *testing.T) (*Service, *addrRuntime) {
	rt := &addrRuntime{fail: map[string]error{}}
	svc := newTestServiceRuntime(t, "ffmpeg", func(local netctl.Runtime) netctl.Runtime {
		rt.Runtime = local
		return rt
	})
	return svc, rt
}

func (r *addrRuntime) SetAddress(_ context.Context, a netctl.AddressSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fail[a.IP]; err != nil {
		return err
	}
	r.set = append(r.set, a)
	return nil
}

func (r *addrRuntime) failWith(ip string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.fail, ip)
		return
	}
	r.fail[ip] = err
}

func (r *addrRuntime) last() netctl.AddressSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.set[len(r.set)-1]
}

// A DHCP camera takes a valid lease, refuses one that clashes, and falls
// back to its profile's factory address (D24).
func TestLeaseAndFactoryAddress(t *testing.T) {
	svc, rt := newAddrService(t)
	ctx := context.Background()
	cam := createCamera(t, svc, "Leased", false)
	other := createCamera(t, svc, "Static", false)
	// The other camera holds 10.1.0.60 statically.
	if err := svc.store.W().UpdateCameraNetwork(ctx, db.UpdateCameraNetworkParams{CameraID: other.ID, Mode: "macvlan", Mac: other.Network.MAC,
		IpMode: "static", Ip: "10.1.0.60", Netmask: "255.255.255.0", DnsJson: "[]"}); err != nil {
		t.Fatal(err)
	}
	b, err := svc.loadBundle(ctx, cam.ID)
	if err != nil {
		t.Fatal(err)
	}
	rt.failWith("10.1.0.70", &netctl.Error{Code: netctl.CodeIPInUse, Message: "IP 10.1.0.70 is already in use on the LAN by 02:00:00:00:00:70"})

	res, err := svc.acceptLease(ctx, b, ipc.Lease{IP: "10.1.0.50", Prefix: 24, Router: "10.1.0.1", DNS: []string{"10.1.0.53", "bad"}})
	if err != nil || res.ip != "10.1.0.50" || res.source != domain.SourceDHCP || strings.Join(res.dns, ",") != "10.1.0.53" {
		t.Fatalf("lease: %+v, %v", res, err)
	}
	if a := rt.last(); a.Gateway != "10.1.0.1" || a.Prefix != 24 || a.ID != cam.ID {
		t.Fatalf("address set: %+v", a)
	}
	// A router outside the subnet is left out.
	if _, err := svc.acceptLease(ctx, b, ipc.Lease{IP: "10.1.0.51", Prefix: 24, Router: "10.2.0.1"}); err != nil || rt.last().Gateway != "" {
		t.Fatalf("foreign router: %+v, %v", rt.last(), err)
	}
	for _, bad := range []ipc.Lease{
		{IP: "10.1.0.60", Prefix: 24}, // another camera's
		{IP: "10.1.0.70", Prefix: 24}, // answers on the LAN
		{IP: "10.1.0.80", Prefix: 31}, // no room for hosts
		{IP: "fe80::1", Prefix: 64},   // not IPv4
	} {
		if _, err := svc.acceptLease(ctx, b, bad); err == nil {
			t.Fatalf("lease %+v was accepted", bad)
		}
	}

	// A lease brings at most three DNS servers, as the helper takes.
	res, err = svc.acceptLease(ctx, b, ipc.Lease{IP: "10.1.0.52", Prefix: 24, DNS: []string{"10.1.0.1", "10.1.0.2", "10.1.0.3", "10.1.0.4"}})
	if err != nil || len(res.dns) != 3 {
		t.Fatalf("four DNS servers: %+v, %v", res, err)
	}

	// No server: the demo profile's factory address, 192.168.5.190/24.
	res, f := svc.factoryAddress(ctx, b, "no DHCP server answered")
	if f != nil || res.ip != "192.168.5.190" || res.source != domain.SourceFactory || rt.last().Gateway != "192.168.5.1" {
		t.Fatalf("factory: %+v %v %+v", res, f, rt.last())
	}
	rt.failWith("192.168.5.190", &netctl.Error{Code: netctl.CodeIPInUse, Message: "IP 192.168.5.190 is already in use on the LAN"})
	if _, f := svc.factoryAddress(ctx, b, "no DHCP server answered"); f == nil || f.code != ReasonFactoryInUse ||
		!strings.Contains(f.text, "factory address 192.168.5.190 cannot be used") {
		t.Fatalf("factory in use: %v", f)
	}
	rt.failWith("192.168.5.190", nil)

	// The address a camera holds shows in its view, with its source.
	svc.saveAddress(cam.ID, "192.168.5.190", domain.SourceFactory)
	v, err := svc.GetCamera(ctx, cam.ID)
	if err != nil || v.Status.IP != "192.168.5.190" || v.Status.IPSource != "factory" {
		t.Fatalf("status %+v, %v", v.Status, err)
	}
}

// Two DHCP cameras of a profile share its factory address: one holds it
// at a time, and a camera that stopped holds nothing, whatever the
// database keeps to show it (B-01).
func TestFactoryAddressOfStoppedCamera(t *testing.T) {
	svc, rt := newAddrService(t)
	ctx := context.Background()
	lobby := createCamera(t, svc, "Lobby", false)
	lobby2 := createCamera(t, svc, "Lobby 2", false)
	b1, _ := svc.loadBundle(ctx, lobby.ID)
	b2, _ := svc.loadBundle(ctx, lobby2.ID)

	if _, f := svc.factoryAddress(ctx, b1, "no DHCP server answered"); f != nil {
		t.Fatal(f)
	}
	svc.saveAddress(lobby.ID, "192.168.5.190", domain.SourceFactory)
	if _, f := svc.factoryAddress(ctx, b2, "no DHCP server answered"); f == nil || !strings.Contains(f.text, "another running camera") {
		t.Fatalf("a second camera took an address in use: %v", f)
	}
	if _, err := svc.acceptLease(ctx, b2, ipc.Lease{IP: "192.168.5.190", Prefix: 24}); err == nil {
		t.Fatal("a second camera leased an address in use")
	}
	// Lobby stops: its session ends and frees the address.
	svc.releaseAddresses(lobby.ID)
	if _, f := svc.factoryAddress(ctx, b2, "no DHCP server answered"); f != nil {
		t.Fatalf("the address of a stopped camera stays taken: %v", f)
	}
	// A refused address gives back the one the camera held.
	rt.failWith("10.1.0.99", &netctl.Error{Code: netctl.CodeIPInUse, Message: "IP 10.1.0.99 is already in use on the LAN"})
	if _, err := svc.acceptLease(ctx, b2, ipc.Lease{IP: "10.1.0.99", Prefix: 24}); err == nil {
		t.Fatal("a lease the helper refused was accepted")
	}
	if _, f := svc.factoryAddress(ctx, b1, "no DHCP server answered"); f == nil {
		t.Fatal("the address Lobby 2 still holds was given to Lobby")
	}
}

// A network card takes macvlan or ipvlan cameras, not both, and the node
// bridge is a macvlan: the service says so instead of the kernel's EBUSY
// (B-02).
func TestParentModes(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	wired := createCamera(t, svc, "Wired", false)
	wifi := createCamera(t, svc, "Wireless", false)
	follower := createCamera(t, svc, "Follower", false)
	for _, c := range []struct {
		cam          *CameraView
		parent, mode string
	}{{wired, "eth9", "macvlan"}, {wifi, "eth8", "ipvlan"}, {follower, "", "ipvlan"}} {
		if err := svc.store.W().UpdateCameraNetwork(ctx, db.UpdateCameraNetworkParams{CameraID: c.cam.ID, ParentIf: c.parent, Mode: c.mode,
			Mac: c.cam.Network.MAC, IpMode: "static", DnsJson: "[]"}); err != nil {
			t.Fatal(err)
		}
	}
	set := svc.Settings(ctx)
	set.ParentInterface = "eth7"
	use, err := svc.parentsInUse(ctx, set, "")
	if err != nil {
		t.Fatal(err)
	}
	if msg := use["eth9"].modeConflict("eth9", "ipvlan"); !strings.Contains(msg, "already has macvlan cameras (Wired)") {
		t.Fatalf("ipvlan next to macvlan: %q", msg)
	}
	if msg := use["eth8"].modeConflict("eth8", "macvlan"); !strings.Contains(msg, "already has ipvlan cameras (Wireless)") {
		t.Fatalf("macvlan next to ipvlan: %q", msg)
	}
	if msg := use["eth9"].modeConflict("eth9", "macvlan"); msg != "" {
		t.Fatalf("macvlan next to macvlan: %q", msg)
	}
	// The camera being edited does not conflict with itself.
	if use, _ := svc.parentsInUse(ctx, set, wired.ID); use["eth9"].modeConflict("eth9", "ipvlan") != "" {
		t.Fatal("a camera conflicts with itself")
	}

	// Settings: the default card may not mix kinds, nor carry the bridge
	// beside ipvlan cameras.
	check := func(set Settings) *domain.ValidationError {
		v := &domain.ValidationError{}
		if err := svc.checkNetworkSettings(ctx, set, v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	set.ParentInterface = "eth9" // Follower, ipvlan, would join Wired
	wantInvalid(t, check(set).Err(), "parent_interface")
	set.ParentInterface, set.NodeBridge = "eth8", true // the bridge beside Wireless and Follower
	wantInvalid(t, check(set).Err(), "node_bridge")
	set.ParentInterface, set.NodeBridge = "eth8", false
	if err := check(set).Err(); err != nil {
		t.Fatalf("ipvlan cameras together: %v", err)
	}

	if retryable(netctl.CodeParentBusy) || !retryable(netctl.CodeIPInUse) {
		t.Fatal("a busy network card is retried, or an address in use is not")
	}
}

// Network settings: DHCP and ipvlan rules, and what an edit leaves out
// stays as it is.
func TestNetworkModes(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	_, err := svc.CreateCamera(ctx, testActor, CreateCameraInput{Name: "Bad", ProfileID: "milesight/demo", ProfileVersion: "0.5.0",
		Network: NetworkInput{Mode: "ipvlan", IPMode: "dhcp"}})
	wantInvalid(t, err, "network.ip_mode")
	_, err = svc.CreateCamera(ctx, testActor, CreateCameraInput{Name: "Bad", ProfileID: "milesight/demo", ProfileVersion: "0.5.0",
		Network: NetworkInput{Mode: "bridge"}})
	wantInvalid(t, err, "network.mode")

	force := true
	v, err := svc.CreateCamera(ctx, testActor, CreateCameraInput{Name: "Leaser", ProfileID: "milesight/demo", ProfileVersion: "0.5.0",
		Network: NetworkInput{IPMode: "dhcp", Force: &force}})
	if err != nil || v.Network.IPMode != "dhcp" || v.Network.Mode != "macvlan" || !v.Network.Force {
		t.Fatalf("created %+v, %v", v.Network, err)
	}
	name := "Leaser 2"
	v, err = svc.UpdateCamera(ctx, testActor, v.ID, UpdateCameraInput{Name: &name, Network: &NetworkInput{DNS: []string{"10.0.0.53"}}})
	if err != nil || v.Network.IPMode != "dhcp" || !v.Network.Force || strings.Join(v.Network.DNS, ",") != "10.0.0.53" {
		t.Fatalf("updated %+v, %v", v.Network, err)
	}
	b, _ := svc.loadBundle(ctx, v.ID)
	if dns := cameraDNS(b, []string{"10.9.9.9"}); strings.Join(dns, ",") != "10.0.0.53" {
		t.Fatalf("own DNS first: %v", dns)
	}
	b.net.DnsJson = "[]"
	if dns := cameraDNS(b, []string{"10.9.9.9"}); strings.Join(dns, ",") != "10.9.9.9" {
		t.Fatalf("then the lease's: %v", dns)
	}
}

// The firewall lets cameras reach every target: IPs, host names and
// default ports.
func TestTargetDestinations(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	for name, url := range map[string]string{
		"vms":   "http://10.0.0.20:8080/events",
		"cloud": "https://127.0.0.1/hook",
		"local": "http://localhost:9000/x",
	} {
		n, u := name, url
		if _, err := svc.CreateTarget(ctx, testActor, TargetInput{Name: &n, URL: &u}); err != nil {
			t.Fatal(err)
		}
	}
	dests, err := svc.destinations(ctx, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, d := range dests {
		raw, _ := json.Marshal(d)
		got[string(raw)] = true
	}
	for _, want := range []string{
		`{"ip":"10.0.0.20","port":8080,"proto":"tcp"}`,
		`{"ip":"127.0.0.1","port":443,"proto":"tcp"}`,
		`{"ip":"127.0.0.1","port":9000,"proto":"tcp"}`,
	} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	fw := svc.firewallFor(ctx, "cam", cameraDNS(&cameraBundle{net: db.CameraNetwork{DnsJson: `["1.1.1.1","8.8.8.8","9.9.9.9","4.4.4.4"]`}}, nil))
	if err := fw.Validate(); err != nil || len(fw.DNS) != 3 {
		t.Fatalf("firewall %+v: %v", fw, err)
	}
}

// An address a name resolved to stays allowed for a while: round-robin
// names answer each query with other addresses (B-05).
func TestResolvedAddressesAreKept(t *testing.T) {
	var r targetResolver
	now := time.Now()
	key := resolveKey{"", "cdn.example"}
	r.names = map[resolveKey]*resolved{key: {at: now, seen: map[string]time.Time{
		"203.0.113.1": now.Add(-time.Minute),
		"203.0.113.2": now,
		"203.0.113.3": now.Add(-resolveKeep - time.Second), // gone
	}}}
	got := r.addresses(context.Background(), []resolveKey{key}, time.Hour)
	if strings.Join(got, ",") != "203.0.113.1,203.0.113.2" {
		t.Fatalf("addresses %v", got)
	}
	r.forget(now.Add(resolveKeep))
	if len(r.names) != 0 {
		t.Fatal("a name nobody asked for is kept")
	}
}

// busyRuntime refuses to launch cameras as the helper does when their
// network card carries interfaces of the other kind.
type busyRuntime struct{ netctl.Runtime }

func (busyRuntime) Launch(context.Context, netctl.LaunchSpec) (*netctl.Launched, error) {
	return nil, &netctl.Error{Code: netctl.CodeParentBusy, Message: "eth0 cannot take an ipvlan interface: it already has macvlan interfaces"}
}

// A failure that only a change can fix ends in error with its code and is
// not retried, by the retry timer or by the reconciler.
func TestPermanentFailureIsNotRetried(t *testing.T) {
	svc := newTestServiceRuntime(t, "ffmpeg", func(rt netctl.Runtime) netctl.Runtime { return busyRuntime{rt} })
	ctx := context.Background()
	cam := createCamera(t, svc, "Busy", false)
	if _, err := svc.StartCamera(ctx, testActor, cam.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		v, err := svc.GetCamera(ctx, cam.ID)
		if err != nil {
			t.Fatal(err)
		}
		if v.Status.State == string(domain.StateError) {
			if v.Status.ReasonCode != netctl.CodeParentBusy || !strings.Contains(v.Status.Reason, "cannot take an ipvlan interface") {
				t.Fatalf("status %+v", v.Status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the camera did not fail: %s", v.Status.State)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !svc.gaveUp(cam.ID) || svc.retryPending(cam.ID) {
		t.Fatal("a camera on a busy network card is retried")
	}
	if v, _ := svc.GetCamera(ctx, cam.ID); v.Status.Retries != 0 {
		t.Fatalf("%d retries shown, none made", v.Status.Retries)
	}
	svc.Reconcile(ctx)
	if svc.session(cam.ID) != nil {
		t.Fatal("the reconciler started it again")
	}
}
