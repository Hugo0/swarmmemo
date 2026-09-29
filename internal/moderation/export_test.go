package moderation

import (
	"context"
	"net"
)

// withFakeJev points the Jev classifier at a plain-HTTP loopback test server.
// Nothing outside tests can reach these fields.
func withFakeJev(o Options, url string) Options {
	o.jevURL = url
	o.jevInsecure = true
	d := &net.Dialer{}
	o.jevDial = func(ctx context.Context, network, addr string) (netConn, error) {
		return d.DialContext(ctx, network, addr)
	}
	return o
}
