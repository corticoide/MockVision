package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

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

func (r *addrRuntime) SetAddress(_ context.Context, a netctl.AddressSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fail[a.IP]; err != nil {
		return err
	}
	r.set = append(r.set, a)
	return nil
}

func (r *addrRuntime) last() netctl.AddressSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.set[len(r.set)-1]
}

// A DHCP camera takes a valid lease, refuses one that clashes, and falls
// back to its profile's factory address (D24).
func TestLeaseAndFactoryAddress(t *testing.T) {
	svc := newTestService(t)
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
	rt := &addrRuntime{Runtime: svc.rt, fail: map[string]error{
		"10.1.0.70":     &netctl.Error{Code: netctl.CodeIPInUse, Message: "IP 10.1.0.70 is already in use on the LAN by 02:00:00:00:00:70"},
		"192.168.5.190": nil,
	}}
	svc.rt = rt
	defer func() { svc.rt = rt.Runtime }()

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

	// No server: the demo profile's factory address, 192.168.5.190/24.
	res, problem := svc.factoryAddress(ctx, b, "no DHCP server answered")
	if problem != "" || res.ip != "192.168.5.190" || res.source != domain.SourceFactory || rt.last().Gateway != "192.168.5.1" {
		t.Fatalf("factory: %+v %q %+v", res, problem, rt.last())
	}
	rt.fail["192.168.5.190"] = &netctl.Error{Code: netctl.CodeIPInUse, Message: "IP 192.168.5.190 is already in use on the LAN"}
	if _, problem := svc.factoryAddress(ctx, b, "no DHCP server answered"); !strings.Contains(problem, "factory address 192.168.5.190 cannot be used") {
		t.Fatalf("factory in use: %q", problem)
	}

	// The address a camera holds shows in its view, with its source.
	svc.saveAddress(cam.ID, "10.1.0.50", domain.SourceDHCP)
	v, err := svc.GetCamera(ctx, cam.ID)
	if err != nil || v.Status.IP != "10.1.0.50" || v.Status.IPSource != "dhcp" {
		t.Fatalf("status %+v, %v", v.Status, err)
	}
	// And no other camera may lease it meanwhile.
	bo, _ := svc.loadBundle(ctx, other.ID)
	if _, err := svc.acceptLease(ctx, bo, ipc.Lease{IP: "10.1.0.50", Prefix: 24}); err == nil {
		t.Fatal("a second camera leased an address in use")
	}
}

// Network settings: DHCP and ipvlan rules, and what an edit leaves out
// stays as it is.
func TestNetworkModes(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	_, err := svc.CreateCamera(ctx, testActor, CreateCameraInput{Name: "Bad", ProfileID: "milesight/demo", ProfileVersion: "0.2.0",
		Network: NetworkInput{Mode: "ipvlan", IPMode: "dhcp"}})
	wantInvalid(t, err, "network.ip_mode")
	_, err = svc.CreateCamera(ctx, testActor, CreateCameraInput{Name: "Bad", ProfileID: "milesight/demo", ProfileVersion: "0.2.0",
		Network: NetworkInput{Mode: "bridge"}})
	wantInvalid(t, err, "network.mode")

	force := true
	v, err := svc.CreateCamera(ctx, testActor, CreateCameraInput{Name: "Leaser", ProfileID: "milesight/demo", ProfileVersion: "0.2.0",
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
	got := map[string]bool{}
	for _, d := range svc.targetDestinations(ctx) {
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
	fw := svc.firewallFor(ctx, []string{"1.1.1.1", "8.8.8.8", "9.9.9.9", "4.4.4.4"})
	if err := fw.Validate(); err != nil || len(fw.DNS) != 3 {
		t.Fatalf("firewall %+v: %v", fw, err)
	}
}
