package camera

import (
	"context"
	"net"
	"sync/atomic"
)

// resolverFor sends the camera's DNS queries to its own servers instead of
// the node's resolv.conf. Each camera is its own process, so replacing the
// default resolver affects that camera only.
func resolverFor(servers []string) *net.Resolver {
	var next atomic.Uint32
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			server := servers[int(next.Add(1)-1)%len(servers)]
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(server, "53"))
		},
	}
}
