package camera

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"

	"github.com/corticoide/mockvision/backend/internal/ipc"
)

// fakeDHCP answers on loopback like a DHCP server: it offers from a pool
// and acks requests, or naks them once told to.
type fakeDHCP struct {
	t      *testing.T
	conn   *net.UDPConn
	mu     sync.Mutex
	pool   []string
	lease  time.Duration
	router string
	nak    bool
	got    []dhcpv4.MessageType
}

func newFakeDHCP(t *testing.T, pool ...string) *fakeDHCP {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDHCP{t: t, conn: c, pool: pool, lease: time.Hour, router: "10.0.0.1"}
	t.Cleanup(func() { c.Close() })
	go f.serve()
	return f
}

func (f *fakeDHCP) serve() {
	buf := make([]byte, 1500)
	for {
		n, from, err := f.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		req, err := dhcpv4.FromBytes(buf[:n])
		if err != nil {
			continue
		}
		f.mu.Lock()
		f.got = append(f.got, req.MessageType())
		ip, lease, router, nak := f.pool[0], f.lease, f.router, f.nak
		f.mu.Unlock()
		var typ dhcpv4.MessageType
		switch req.MessageType() {
		case dhcpv4.MessageTypeDiscover:
			typ = dhcpv4.MessageTypeOffer
		case dhcpv4.MessageTypeRequest:
			typ = dhcpv4.MessageTypeAck
			if nak {
				typ = dhcpv4.MessageTypeNak
			}
		case dhcpv4.MessageTypeDecline:
			f.mu.Lock()
			if len(f.pool) > 1 {
				f.pool = f.pool[1:]
			}
			f.mu.Unlock()
			continue
		default:
			continue
		}
		reply, _ := dhcpv4.NewReplyFromRequest(req,
			dhcpv4.WithMessageType(typ),
			dhcpv4.WithYourIP(net.ParseIP(ip)),
			dhcpv4.WithNetmask(net.CIDRMask(24, 32)),
			dhcpv4.WithRouter(net.ParseIP(router)),
			dhcpv4.WithDNS(net.ParseIP("10.0.0.53")),
			dhcpv4.WithLeaseTime(uint32(lease/time.Second)),
			dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.IPv4(127, 0, 0, 1))),
		)
		_, _ = f.conn.WriteToUDP(reply.ToBytes(), from)
	}
}

func (f *fakeDHCP) received() []dhcpv4.MessageType {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dhcpv4.MessageType(nil), f.got...)
}

type reportLog struct {
	mu  sync.Mutex
	ch  chan string
	all []any
}

func (r *reportLog) report(typ string, data any) {
	r.mu.Lock()
	r.all = append(r.all, data)
	r.mu.Unlock()
	r.ch <- typ
}

func (r *reportLog) next(t *testing.T, want string) any {
	t.Helper()
	select {
	case typ := <-r.ch:
		if typ != want {
			t.Fatalf("got %s, want %s", typ, want)
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.all[len(r.all)-1]
	case <-time.After(5 * time.Second):
		t.Fatalf("no %s", want)
	}
	return nil
}

func testClient(t *testing.T, server *fakeDHCP) (*dhcpClient, *reportLog) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	mac, _ := net.ParseMAC("02:11:22:33:44:55")
	log := &reportLog{ch: make(chan string, 16)}
	c := newDHCPClient(conn, mac, "Gate-1", slog.New(slog.NewTextHandler(io.Discard, nil)), log.report)
	c.t = dhcpTimings{Discover: []time.Duration{200 * time.Millisecond, 200 * time.Millisecond}, Retry: 300 * time.Millisecond,
		Decline: 50 * time.Millisecond, MinLease: time.Second}
	if server != nil {
		c.server = server.conn.LocalAddr().(*net.UDPAddr)
		c.port = c.server.Port
	}
	return c, log
}

func TestDHCPLeaseDeclineRenewAndRelease(t *testing.T) {
	server := newFakeDHCP(t, "10.0.0.50", "10.0.0.51")
	c, log := testClient(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.run(ctx); close(done) }()

	l := log.next(t, ipc.TypeDHCPLease).(ipc.Lease)
	if l.IP != "10.0.0.50" || l.Prefix != 24 || l.Router != "10.0.0.1" || len(l.DNS) != 1 || l.Server != "127.0.0.1" || l.Seconds != 3600 {
		t.Fatalf("lease %+v", l)
	}
	// The service refuses it: the camera declines and gets another.
	c.Decline("10.0.0.50")
	if l := log.next(t, ipc.TypeDHCPLease).(ipc.Lease); l.IP != "10.0.0.51" {
		t.Fatalf("after decline: %+v", l)
	}

	// A short lease is renewed with the server; a refusal loses it.
	server.mu.Lock()
	server.lease = time.Second
	server.pool = []string{"10.0.0.51", "10.0.0.52"}
	server.mu.Unlock()
	c.Decline("10.0.0.51")
	if l := log.next(t, ipc.TypeDHCPLease).(ipc.Lease); l.IP != "10.0.0.52" || l.Seconds != 1 {
		t.Fatalf("short lease: %+v", l)
	}
	time.Sleep(700 * time.Millisecond) // past T1: renewed, the same address
	server.mu.Lock()
	server.nak = true
	server.mu.Unlock()
	log.next(t, ipc.TypeDHCPLost)
	server.mu.Lock()
	server.nak = false
	server.mu.Unlock()
	log.next(t, ipc.TypeDHCPLease)

	cancel()
	<-done
	time.Sleep(100 * time.Millisecond)
	got := server.received()
	if got[len(got)-1] != dhcpv4.MessageTypeRelease {
		t.Fatalf("the lease was not released on stop: %v", got)
	}
	declines := 0
	for _, m := range got {
		if m == dhcpv4.MessageTypeDecline {
			declines++
		}
	}
	if declines != 2 {
		t.Fatalf("declines %d: %v", declines, got)
	}
}

// A renewal that keeps the address but brings another router is reported,
// so the service applies it; one that changes nothing is not.
func TestDHCPRenewalReportsChanges(t *testing.T) {
	server := newFakeDHCP(t, "10.0.0.60")
	server.mu.Lock()
	server.lease = time.Second
	server.mu.Unlock()
	c, log := testClient(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx)
	if l := log.next(t, ipc.TypeDHCPLease).(ipc.Lease); l.IP != "10.0.0.60" || l.Router != "10.0.0.1" {
		t.Fatalf("lease %+v", l)
	}
	select {
	case typ := <-log.ch:
		t.Fatalf("a renewal without changes reported %s", typ)
	case <-time.After(700 * time.Millisecond): // past T1
	}
	server.mu.Lock()
	server.router = "10.0.0.254"
	server.mu.Unlock()
	if l := log.next(t, ipc.TypeDHCPLease).(ipc.Lease); l.IP != "10.0.0.60" || l.Router != "10.0.0.254" {
		t.Fatalf("renewed lease %+v", l)
	}
}

// Without a server the failure is reported once, and the camera keeps
// asking.
func TestDHCPWithoutServer(t *testing.T) {
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}) // answers nothing
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	c, log := testClient(t, nil)
	c.server = sink.LocalAddr().(*net.UDPAddr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx)
	st := log.next(t, ipc.TypeDHCPFailed).(ipc.DHCPStatus)
	if st.Reason == "" {
		t.Fatal("no reason")
	}
	select {
	case typ := <-log.ch:
		t.Fatalf("reported %s again", typ)
	case <-time.After(1500 * time.Millisecond):
	}
}
