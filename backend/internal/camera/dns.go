package camera

import (
	"context"
	"net"
	"sync/atomic"
)

// dnsServers sends the camera's DNS queries to its own servers instead of
// the node's resolv.conf. Each camera is its own process, so replacing the
// default resolver affects that camera only. The servers can change while
// the camera runs (a DHCP renewal), so the resolver reads them on every
// query; with none it uses the address resolv.conf gave.
type dnsServers struct {
	list atomic.Pointer[[]string]
	next atomic.Uint32
}

func (d *dnsServers) set(servers []string) {
	list := append([]string(nil), servers...)
	d.list.Store(&list)
}

func (d *dnsServers) resolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			if list := d.list.Load(); list != nil && len(*list) > 0 {
				address = net.JoinHostPort((*list)[int(d.next.Add(1)-1)%len(*list)], "53")
			}
			var dl net.Dialer
			return dl.DialContext(ctx, network, address)
		},
	}
}
