package app

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/netctl"
	"github.com/corticoide/mockvision/backend/internal/store/db"
)

// fakeCamera is a camera process that speaks the real IPC protocol and
// does what the test tells it.
type fakeCamera struct {
	conn    *ipc.Conn
	mu      sync.Mutex
	reloads []ipc.Reload
}

func (c *fakeCamera) handle(_ context.Context, msg *ipc.Envelope) (any, error) {
	switch msg.Type {
	case ipc.TypeConfigure:
		go func() { _ = c.conn.Notify(ipc.TypeReady, ipc.Ready{}) }()
	case ipc.TypeReload:
		var rl ipc.Reload
		if err := msg.Decode(&rl); err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.reloads = append(c.reloads, rl)
		c.mu.Unlock()
	case ipc.TypeStop:
		go func() { _ = c.conn.Close() }()
	}
	return nil, nil
}

func (c *fakeCamera) lastReload() (ipc.Reload, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reloads) == 0 {
		return ipc.Reload{}, false
	}
	return c.reloads[len(c.reloads)-1], true
}

// fakeRuntime launches fake cameras; it reports KindNetns once told to,
// after the cameras were created without a real network card.
type fakeRuntime struct {
	netctl.Runtime
	netns atomic.Bool
	mu    sync.Mutex
	cams  []*fakeCamera
	addrs []netctl.AddressSpec
	fws   []netctl.Firewall
}

func (r *fakeRuntime) Kind() string {
	if r.netns.Load() {
		return netctl.KindNetns
	}
	return netctl.KindLocal
}

func (r *fakeRuntime) Launch(ctx context.Context, spec netctl.LaunchSpec) (*netctl.Launched, error) {
	svcSide, camSide := net.Pipe()
	cam := &fakeCamera{}
	cam.conn = ipc.NewConn(camSide, cam.handle, nil)
	go func() { _ = cam.conn.Run(context.Background()) }()
	go func() {
		_ = cam.conn.Notify(ipc.TypeHello, ipc.Hello{CameraID: spec.Camera.ID})
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-cam.conn.Done():
				return
			case <-t.C:
				_ = cam.conn.Notify(ipc.TypeHeartbeat, ipc.Heartbeat{})
			}
		}
	}()
	r.mu.Lock()
	r.cams = append(r.cams, cam)
	r.mu.Unlock()
	return &netctl.Launched{Conn: svcSide, PID: 4242, Netns: spec.Camera.Netns, IP: spec.Camera.IP, MAC: spec.Camera.MAC, Firewall: true}, nil
}

func (r *fakeRuntime) camera(n int) *fakeCamera {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cams) <= n {
		return nil
	}
	return r.cams[n]
}

func (r *fakeRuntime) launches() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cams)
}

func (r *fakeRuntime) Destroy(context.Context, string) error { return nil }

func (r *fakeRuntime) Live(context.Context) ([]string, error) { return nil, nil }

func (r *fakeRuntime) SetAddress(_ context.Context, a netctl.AddressSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addrs = append(r.addrs, a)
	return nil
}

func (r *fakeRuntime) lastAddress() netctl.AddressSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.addrs[len(r.addrs)-1]
}

func (r *fakeRuntime) SetFirewall(_ context.Context, _ string, f netctl.Firewall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fws = append(r.fws, f)
	return nil
}

// eventually waits for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A DHCP camera's lease while it runs: a renewal with another router or
// DNS servers is applied in place (B-12); another address, or a lease
// that expired, restarts the camera's process as a real camera reboots,
// without it leaving the running state it was asked for (B-03, B-07).
func TestLeaseChangesWhileRunning(t *testing.T) {
	rt := &fakeRuntime{}
	svc := newTestServiceRuntime(t, "ffmpeg", func(local netctl.Runtime) netctl.Runtime {
		rt.Runtime = local
		return rt
	})
	ctx := context.Background()
	cam := createCamera(t, svc, "Leaser", false)
	if err := svc.store.W().UpdateCameraNetwork(ctx, db.UpdateCameraNetworkParams{CameraID: cam.ID, Mode: "macvlan", ParentIf: "lo",
		Mac: cam.Network.MAC, IpMode: "dhcp", DnsJson: "[]"}); err != nil {
		t.Fatal(err)
	}
	rt.netns.Store(true)
	if _, err := svc.StartCamera(ctx, testActor, cam.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the first process", func() bool { return rt.camera(0) != nil })
	first := rt.camera(0)
	eventually(t, "the wait for a lease", func() bool {
		v, _ := svc.GetCamera(ctx, cam.ID)
		return v.Status.ReasonCode == ReasonDHCPWaiting
	})
	lease := ipc.Lease{IP: "10.9.0.50", Prefix: 24, Router: "10.9.0.1", DNS: []string{"10.9.0.53"}}
	if err := first.conn.Notify(ipc.TypeDHCPLease, lease); err != nil {
		t.Fatal(err)
	}
	waitState(t, svc, cam.ID, domain.StateRunning)
	if a := rt.lastAddress(); a.IP != "10.9.0.50" || a.Gateway != "10.9.0.1" {
		t.Fatalf("address %+v", a)
	}

	// A renewal with another router and DNS server: in place.
	lease.Router, lease.DNS = "10.9.0.254", []string{"10.9.0.54"}
	if err := first.conn.Notify(ipc.TypeDHCPLease, lease); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the new DNS server in the camera", func() bool {
		rl, ok := first.lastReload()
		return ok && len(rl.DNS) == 1 && rl.DNS[0] == "10.9.0.54"
	})
	if a := rt.lastAddress(); a.IP != "10.9.0.50" || a.Gateway != "10.9.0.254" {
		t.Fatalf("renewed address %+v", a)
	}
	if rt.launches() != 1 {
		t.Fatal("a renewal restarted the camera")
	}

	// The lease expires: the process starts again and leases anew.
	if err := first.conn.Notify(ipc.TypeDHCPLost, ipc.DHCPStatus{Reason: "the lease expired"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a second process", func() bool { return rt.launches() == 2 })
	second := rt.camera(1)
	eventually(t, "the second wait for a lease", func() bool {
		v, _ := svc.GetCamera(ctx, cam.ID)
		return v.Status.ReasonCode == ReasonDHCPWaiting
	})
	if c, _ := svc.store.R().GetCamera(ctx, cam.ID); c.DesiredState != string(domain.DesiredRunning) {
		t.Fatalf("desired state %s after the restart", c.DesiredState)
	}
	if err := second.conn.Notify(ipc.TypeDHCPLease, ipc.Lease{IP: "10.9.0.60", Prefix: 24}); err != nil {
		t.Fatal(err)
	}
	waitState(t, svc, cam.ID, domain.StateRunning)

	// Another address: one restart, even if the camera says it twice.
	moved := ipc.Lease{IP: "10.9.0.70", Prefix: 24}
	_ = second.conn.Notify(ipc.TypeDHCPLease, moved)
	_ = second.conn.Notify(ipc.TypeDHCPLease, moved)
	eventually(t, "a third process", func() bool { return rt.launches() == 3 })
	time.Sleep(300 * time.Millisecond)
	if n := rt.launches(); n != 3 {
		t.Fatalf("%d launches: the camera restarted twice", n)
	}
	page, err := svc.ListAudit(ctx, AuditFilter{EntityID: cam.ID, Action: "camera.restart"})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("restarts audited: %d, %v", len(page.Items), err)
	}
}
