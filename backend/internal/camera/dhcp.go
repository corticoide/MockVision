package camera

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/ipc"
)

// dhcpTimings are the waits of the client; tests shorten them.
type dhcpTimings struct {
	// Waits between DISCOVER retries before the camera gives up and the
	// service takes the profile's factory address (D24): 2, 4 and 8
	// seconds, a shortened RFC 2131 back-off.
	Discover []time.Duration
	// Retry is how long a camera without a lease waits before asking
	// again, as real cameras keep doing.
	Retry time.Duration
	// Decline is the wait after declining an address (RFC 2131: 10 s).
	Decline time.Duration
	// MinLease bounds the lease time a server may impose.
	MinLease time.Duration
}

var defaultDHCPTimings = dhcpTimings{
	Discover: []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second},
	Retry:    time.Minute,
	Decline:  10 * time.Second,
	MinLease: time.Minute,
}

// dhcpClient leases the camera's address over the UDP socket the network
// helper opened for it (port 68, bound to eth0, allowed to broadcast). It
// parses what DHCP servers send, so it runs here, without privileges; the
// service validates the lease and has the helper set the address.
type dhcpClient struct {
	conn     net.PacketConn
	mac      net.HardwareAddr
	hostname string
	server   *net.UDPAddr // where broadcasts go: 255.255.255.255:67
	port     int          // the servers' port for unicast: 67
	t        dhcpTimings
	log      *slog.Logger
	// report sends a message to the service.
	report func(typ string, data any)

	mu       sync.Mutex
	decline  chan string // addresses the service refused
	bound    *dhcpv4.DHCPv4
	reported bool // the failure was reported, until a lease comes
}

func newDHCPClient(conn net.PacketConn, mac net.HardwareAddr, hostname string, log *slog.Logger, report func(string, any)) *dhcpClient {
	return &dhcpClient{
		conn: conn, mac: mac, hostname: hostname, log: log, report: report,
		server:  &net.UDPAddr{IP: net.IPv4bcast, Port: 67},
		port:    67,
		t:       defaultDHCPTimings,
		decline: make(chan string, 1),
	}
}

// Decline asks the client to decline an address the service refused.
func (c *dhcpClient) Decline(ip string) {
	select {
	case c.decline <- ip:
	default:
	}
}

// run leases an address and keeps it until ctx ends, then releases it.
func (c *dhcpClient) run(ctx context.Context) {
	defer c.release()
	for ctx.Err() == nil {
		ack, err := c.acquire(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.mu.Lock()
			first := !c.reported
			c.reported = true
			c.mu.Unlock()
			if first {
				c.report(ipc.TypeDHCPFailed, ipc.DHCPStatus{Reason: err.Error()})
			}
			c.log.Info("no DHCP lease; asking again later", "error", err)
			if !sleep(ctx, c.t.Retry) {
				return
			}
			continue
		}
		c.mu.Lock()
		c.bound, c.reported = ack, false
		c.mu.Unlock()
		c.report(ipc.TypeDHCPLease, c.lease(ack))
		c.keep(ctx, ack)
	}
}

// acquire runs DISCOVER, OFFER, REQUEST and ACK, with retries.
func (c *dhcpClient) acquire(ctx context.Context) (*dhcpv4.DHCPv4, error) {
	lastErr := errors.New("no DHCP server answered")
	for _, wait := range c.t.Discover {
		discover, err := dhcpv4.NewDiscovery(c.mac, append(c.options(), dhcpv4.WithTransactionID(randomXID()))...)
		if err != nil {
			return nil, err
		}
		offer, err := c.exchange(ctx, discover, c.server, wait, dhcpv4.MessageTypeOffer)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		request, err := dhcpv4.NewRequestFromOffer(offer, c.options()...)
		if err != nil {
			return nil, err
		}
		ack, err := c.exchange(ctx, request, c.server, wait, dhcpv4.MessageTypeAck)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		if err := c.check(ack); err != nil {
			lastErr = err
			continue
		}
		return ack, nil
	}
	return nil, lastErr
}

// keep renews the lease at T1, rebinds at T2 and gives it up at expiry,
// or declines it when the service asks.
func (c *dhcpClient) keep(ctx context.Context, ack *dhcpv4.DHCPv4) {
	for {
		lease := c.leaseTime(ack)
		t1 := ack.IPAddressRenewalTime(lease / 2)
		t2 := ack.IPAddressRebindingTime(lease * 7 / 8)
		if t1 <= 0 || t1 >= lease {
			t1 = lease / 2
		}
		if t2 <= t1 || t2 >= lease {
			t2 = lease * 7 / 8
		}
		start := time.Now()
		select {
		case <-ctx.Done():
			return
		case ip := <-c.decline:
			c.sendDecline(ack, ip)
			c.mu.Lock()
			c.bound = nil
			c.mu.Unlock()
			sleep(ctx, c.t.Decline)
			return
		case <-time.After(t1):
		}
		// Renew with the server that leased it, then with any server.
		next, err := c.renew(ctx, ack, start.Add(t2), start.Add(lease))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.mu.Lock()
			c.bound = nil
			c.mu.Unlock()
			c.report(ipc.TypeDHCPLost, ipc.DHCPStatus{Reason: err.Error()})
			return
		}
		// The service acts on any change: another address restarts the
		// camera, another prefix, router or DNS server is applied in place.
		if l := c.lease(next); !sameLease(l, c.lease(ack)) {
			c.report(ipc.TypeDHCPLease, l)
		}
		c.mu.Lock()
		c.bound = next
		c.mu.Unlock()
		ack = next
	}
}

// renew asks for the same address until rebind, unicast to the server,
// then broadcast until expiry.
func (c *dhcpClient) renew(ctx context.Context, ack *dhcpv4.DHCPv4, rebind, expiry time.Time) (*dhcpv4.DHCPv4, error) {
	server := &net.UDPAddr{IP: ack.ServerIdentifier(), Port: c.port}
	for time.Now().Before(expiry) {
		to := server
		if time.Now().After(rebind) || server.IP == nil {
			to = c.server
		}
		req, err := dhcpv4.NewRenewFromAck(ack, dhcpv4.WithOption(dhcpv4.OptHostName(c.hostname)), dhcpv4.WithOption(dhcpv4.OptClientIdentifier(c.clientID())))
		if err != nil {
			return nil, err
		}
		wait := time.Until(expiry) / 2
		wait = max(min(wait, time.Minute), time.Second)
		next, err := c.exchange(ctx, req, to, wait, dhcpv4.MessageTypeAck)
		if err == nil {
			if err := c.check(next); err != nil {
				return nil, err
			}
			return next, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var nak *nakError
		if errors.As(err, &nak) {
			return nil, err
		}
	}
	return nil, errors.New("the lease expired without an answer from any server")
}

type nakError struct{ msg string }

func (e *nakError) Error() string { return "the server refused the address: " + e.msg }

// exchange sends a message and waits for the answer of the wanted type to
// the same transaction.
func (c *dhcpClient) exchange(ctx context.Context, msg *dhcpv4.DHCPv4, to *net.UDPAddr, wait time.Duration, want dhcpv4.MessageType) (*dhcpv4.DHCPv4, error) {
	if _, err := c.conn.WriteTo(msg.ToBytes(), to); err != nil {
		return nil, fmt.Errorf("send %s: %w", msg.MessageType(), err)
	}
	deadline := time.Now().Add(wait)
	buf := make([]byte, 1500)
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Short reads so a canceled context is noticed.
		step := min(time.Until(deadline), 250*time.Millisecond)
		if step <= 0 {
			return nil, fmt.Errorf("no answer to %s", msg.MessageType())
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(step))
		n, _, err := c.conn.ReadFrom(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return nil, err
		}
		reply, err := dhcpv4.FromBytes(buf[:n])
		if err != nil || reply.OpCode != dhcpv4.OpcodeBootReply || reply.TransactionID != msg.TransactionID ||
			!strings.EqualFold(reply.ClientHWAddr.String(), c.mac.String()) {
			continue // another client's, or not DHCP
		}
		switch reply.MessageType() {
		case want:
			return reply, nil
		case dhcpv4.MessageTypeNak:
			return nil, &nakError{msg: reply.Message()}
		}
	}
}

// check validates what a server leased before it reaches the service.
func (c *dhcpClient) check(ack *dhcpv4.DHCPv4) error {
	ip, err := netip.ParseAddr(ack.YourIPAddr.String())
	if err != nil || !ip.Is4() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
		return fmt.Errorf("the server leased an unusable address %q", ack.YourIPAddr)
	}
	return nil
}

// sameLease reports whether two leases give the camera the same
// addressing; the lease time and the server do not count.
func sameLease(a, b ipc.Lease) bool {
	return a.IP == b.IP && a.Prefix == b.Prefix && a.Router == b.Router && slices.Equal(a.DNS, b.DNS)
}

// lease turns an ACK into what the service needs.
func (c *dhcpClient) lease(ack *dhcpv4.DHCPv4) ipc.Lease {
	l := ipc.Lease{IP: ack.YourIPAddr.String(), Prefix: 24, Seconds: int(c.leaseTime(ack) / time.Second)}
	if m := ack.SubnetMask(); m != nil {
		if ones, bits := m.Size(); bits == 32 && ones > 0 {
			l.Prefix = ones
		}
	}
	if r := ack.Router(); len(r) > 0 && r[0].To4() != nil {
		l.Router = r[0].String()
	}
	for _, d := range ack.DNS() {
		if d.To4() != nil && len(l.DNS) < domain.MaxDNS {
			l.DNS = append(l.DNS, d.String())
		}
	}
	if s := ack.ServerIdentifier(); s != nil {
		l.Server = s.String()
	}
	return l
}

func (c *dhcpClient) leaseTime(ack *dhcpv4.DHCPv4) time.Duration {
	d := ack.IPAddressLeaseTime(time.Hour)
	return min(max(d, c.t.MinLease), 7*24*time.Hour)
}

func (c *dhcpClient) clientID() []byte { return append([]byte{1}, c.mac...) }

// options are what the camera tells servers about itself. A request keeps
// the transaction of the offer it answers.
func (c *dhcpClient) options() []dhcpv4.Modifier {
	return []dhcpv4.Modifier{
		dhcpv4.WithBroadcast(true), // answer by broadcast: the camera has no address yet
		dhcpv4.WithOption(dhcpv4.OptHostName(c.hostname)),
		dhcpv4.WithOption(dhcpv4.OptClientIdentifier(c.clientID())),
		dhcpv4.WithOption(dhcpv4.OptMaxMessageSize(1500)),
		dhcpv4.WithRequestedOptions(dhcpv4.OptionSubnetMask, dhcpv4.OptionRouter, dhcpv4.OptionDomainNameServer,
			dhcpv4.OptionIPAddressLeaseTime, dhcpv4.OptionRenewTimeValue, dhcpv4.OptionRebindingTimeValue),
	}
}

func (c *dhcpClient) sendDecline(ack *dhcpv4.DHCPv4, ip string) {
	msg, err := dhcpv4.New(
		dhcpv4.WithHwAddr(c.mac),
		dhcpv4.WithMessageType(dhcpv4.MessageTypeDecline),
		dhcpv4.WithTransactionID(randomXID()),
		dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(net.ParseIP(ip))),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(ack.ServerIdentifier())),
		dhcpv4.WithOption(dhcpv4.OptClientIdentifier(c.clientID())),
	)
	if err == nil {
		_, _ = c.conn.WriteTo(msg.ToBytes(), c.server)
	}
	c.log.Info("declined a leased address", "ip", ip)
}

// release gives the lease back when the camera stops.
func (c *dhcpClient) release() {
	c.mu.Lock()
	ack := c.bound
	c.bound = nil
	c.mu.Unlock()
	if ack == nil {
		return
	}
	msg, err := dhcpv4.NewReleaseFromACK(ack, dhcpv4.WithOption(dhcpv4.OptClientIdentifier(c.clientID())))
	if err != nil {
		return
	}
	to := &net.UDPAddr{IP: ack.ServerIdentifier(), Port: c.port}
	if to.IP == nil {
		to = c.server
	}
	_, _ = c.conn.WriteTo(msg.ToBytes(), to)
}

func randomXID() dhcpv4.TransactionID {
	var x dhcpv4.TransactionID
	v := rand.Uint32()
	x[0], x[1], x[2], x[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
	return x
}

// sleep waits d unless ctx ends first; it reports whether it slept.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
