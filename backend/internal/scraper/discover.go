package scraper

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

// Discovery limits: a sweep is read-only, bounded to a private subnet and
// paced, so it never reaches the internet or floods the LAN (RN-17).
const (
	// MaxSweepHosts caps how many addresses a sweep touches.
	MaxSweepHosts = 1024
	// minPrefix is the smallest subnet a sweep accepts: /22 is 1022 hosts.
	minPrefix        = 22
	discoverParallel = 32
	multicastWait    = 2 * time.Second
)

// commonPorts are the ports a sweep tries on each host; a camera answers
// on one of them.
var commonPorts = []int{80, 554, 8000, 8080, 8554, 443}

// Found is a device a discovery turned up: where it is and what answered.
type Found struct {
	Host      string    `json:"host"`
	Via       string    `json:"via"` // sweep, ssdp, ws-discovery, mdns
	OpenPorts []int     `json:"open_ports"`
	Services  []Service `json:"services"`
	Vendor    string    `json:"vendor,omitempty"`
	Server    string    `json:"server,omitempty"`
	URL       string    `json:"url,omitempty"`
}

// DiscoverOptions choose what a discovery does.
type DiscoverOptions struct {
	// CIDR is the subnet to sweep, such as 192.168.1.0/24; empty skips the
	// sweep. It must be a private or link-local range of /22 or smaller.
	CIDR string
	// Multicast sends SSDP and WS-Discovery queries and listens for
	// answers, best-effort where the network allows multicast.
	Multicast bool
	// Rate is the requests a second across the whole discovery.
	Rate float64
	// Ports narrows the ports a sweep tries; the common camera ports when
	// empty.
	Ports []int
}

// ValidateCIDR checks a sweep range: a private or link-local subnet, no
// larger than minPrefix, so a discovery stays on the LAN (RN-17).
func ValidateCIDR(cidr string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(cidr))
	if err != nil {
		return p, fmt.Errorf("not a subnet in CIDR form, such as 192.168.1.0/24")
	}
	p = p.Masked()
	if !p.Addr().Is4() {
		return p, fmt.Errorf("only IPv4 subnets are swept")
	}
	if p.Bits() < minPrefix {
		return p, fmt.Errorf("subnet is too large: use /%d or smaller", minPrefix)
	}
	a := p.Addr()
	if !(a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast()) {
		return p, fmt.Errorf("only private, loopback or link-local subnets are swept, never public ranges")
	}
	return p, nil
}

// Discover looks for cameras on the LAN, read-only: an optional connect
// sweep of a private subnet and best-effort multicast queries.
func Discover(ctx context.Context, opts DiscoverOptions) ([]Found, error) {
	rate := opts.Rate
	if rate <= 0 {
		rate = DefaultRate
	}
	lim := newLimiter(rate)
	found := map[string]*Found{}
	var mu sync.Mutex
	add := func(f Found) {
		mu.Lock()
		defer mu.Unlock()
		if ex, ok := found[f.Host]; ok {
			if ex.Via == "sweep" || f.Via != "sweep" {
				return
			}
		}
		found[f.Host] = &f
	}

	if opts.Multicast {
		for _, a := range multicast(ctx) {
			add(Found{Host: a.From, Via: a.Proto, Server: a.Server, URL: a.URL, Vendor: guessVendor(a.Server)})
		}
	}

	if opts.CIDR != "" {
		prefix, err := ValidateCIDR(opts.CIDR)
		if err != nil {
			return nil, err
		}
		hosts := hostsOf(prefix)
		if len(hosts) > MaxSweepHosts {
			return nil, fmt.Errorf("subnet has %d hosts, more than the limit of %d", len(hosts), MaxSweepHosts)
		}
		ports := opts.Ports
		if len(ports) == 0 {
			ports = commonPorts
		}
		sweep(ctx, hosts, ports, lim, add)
	}

	out := make([]Found, 0, len(found))
	for _, f := range found {
		sort.Ints(f.OpenPorts)
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

// sweep connect-scans the hosts for the common camera ports and reads the
// banner of each responder, read-only.
func sweep(ctx context.Context, hosts []string, ports []int, lim *limiter, add func(Found)) {
	sem := make(chan struct{}, discoverParallel)
	var wg sync.WaitGroup
	for _, host := range hosts {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(host string) {
			defer wg.Done()
			defer func() { <-sem }()
			p := NewProber(Target{Host: host}, 1)
			p.lim = lim
			var f Found
			for _, port := range ports {
				if lim.wait(ctx) != nil {
					return
				}
				c, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
				if err != nil {
					continue
				}
				c.Close()
				_, svc := p.identify(ctx, port)
				f.OpenPorts = append(f.OpenPorts, port)
				f.Services = append(f.Services, svc)
				if f.Server == "" {
					f.Server = svc.Server
				}
			}
			if len(f.OpenPorts) > 0 {
				f.Host, f.Via = host, "sweep"
				f.Vendor = guessVendor(f.Server)
				add(f)
			}
		}(host)
	}
	wg.Wait()
}

// hostsOf lists the usable addresses of a prefix (without network and
// broadcast for a subnet larger than /31).
func hostsOf(p netip.Prefix) []string {
	var out []string
	addr := p.Addr()
	for p.Contains(addr) {
		out = append(out, addr.String())
		next := addr.Next()
		if !next.IsValid() {
			break
		}
		addr = next
		if len(out) > MaxSweepHosts+2 {
			break
		}
	}
	if p.Bits() <= 30 && len(out) > 2 {
		out = out[1 : len(out)-1] // drop network and broadcast
	}
	return out
}

// multicast sends SSDP and WS-Discovery queries and collects answers,
// best-effort: it works only where the network delivers multicast.
func multicast(ctx context.Context) []Announce {
	var out []Announce
	out = append(out, ssdp(ctx)...)
	out = append(out, wsDiscovery(ctx)...)
	return out
}

func ssdp(ctx context.Context) []Announce {
	msg := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 1\r\n" +
		"ST: urn:schemas-upnp-org:device:Basic:1\r\n\r\n"
	replies := multicastQuery(ctx, "239.255.255.250:1900", []byte(msg))
	var out []Announce
	for from, body := range replies {
		a := Announce{From: from, Proto: "ssdp"}
		for _, line := range strings.Split(body, "\r\n") {
			if k, v, ok := strings.Cut(line, ":"); ok {
				switch strings.ToUpper(strings.TrimSpace(k)) {
				case "SERVER":
					a.Server = strings.TrimSpace(v)
				case "LOCATION":
					a.URL = strings.TrimSpace(line[len(k)+1:])
				}
			}
		}
		out = append(out, a)
	}
	return out
}

func wsDiscovery(ctx context.Context) []Announce {
	probe := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<e:Envelope xmlns:e="http://www.w3.org/2003/05/soap-envelope" xmlns:w="http://schemas.xmlsoap.org/ws/2004/08/addressing" ` +
		`xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery" xmlns:dn="http://www.onvif.org/ver10/network/wsdl">` +
		`<e:Header><w:MessageID>uuid:` + "mockvision-probe" + `</w:MessageID>` +
		`<w:To>urn:schemas-xmlsoap-org:ws:2005:04:discovery</w:To>` +
		`<w:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</w:Action></e:Header>` +
		`<e:Body><d:Probe><d:Types>dn:NetworkVideoTransmitter</d:Types></d:Probe></e:Body></e:Envelope>`
	replies := multicastQuery(ctx, "239.255.255.250:3702", []byte(probe))
	var out []Announce
	for from := range replies {
		out = append(out, Announce{From: from, Proto: "ws-discovery"})
	}
	return out
}

// multicastQuery sends one datagram to a multicast group and gathers the
// unicast replies for a short window.
func multicastQuery(ctx context.Context, group string, payload []byte) map[string]string {
	out := map[string]string{}
	gaddr, err := net.ResolveUDPAddr("udp4", group)
	if err != nil {
		return out
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return out
	}
	defer conn.Close()
	if _, err := conn.WriteToUDP(payload, gaddr); err != nil {
		return out
	}
	deadline := time.Now().Add(multicastWait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)
	buf := make([]byte, 8192)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		host := from.IP.String()
		if _, ok := out[host]; !ok {
			out[host] = string(buf[:n])
		}
	}
	return out
}
