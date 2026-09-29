// Package safenet is the outbound-request boundary shared by every feature
// that connects to a host it did not choose at build time: webhook delivery
// (internal/board) and the inference upstreams (internal/services). It decides
// which addresses are public and dials only those, after resolution, so a
// hostname that rebinds to a private address between a check and the connect
// is refused at the connect.
package safenet

import (
	"context"
	"errors"
	"net"
	"time"
)

var (
	// ErrBlocked is an address that is private, loopback, link-local,
	// multicast, carrier-NAT, documentation or otherwise not public.
	ErrBlocked = errors.New("safenet: address is not public")
	// ErrUnresolved is a host with no usable address.
	ErrUnresolved = errors.New("safenet: host does not resolve")
)

// blocked holds the ranges IsGlobalUnicast and friends do not already cover,
// or cover inconsistently between families. The predicate checks stay
// alongside it; both run on every address.
var blocked = func() []*net.IPNet {
	nets := []*net.IPNet{}
	for _, cidr := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8",
		"2001::/32", "2002::/16", "64:ff9b::/96", "100::/64", "2001:db8::/32",
		// IPv6 forms that embed an IPv4 address a NAT64 or SIIT gateway would
		// translate, and the deprecated site-local range (security review
		// 1.20, L11): the same list as moderation/egress.go and egress.ts.
		"::/96", "64:ff9b:1::/48", "::ffff:0:0:0/96", "fec0::/10",
	} {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(err)
		}
		nets = append(nets, n)
	}
	return nets
}()

// PublicIP is the single address decision: nil for a public unicast address,
// ErrBlocked or ErrUnresolved otherwise. An IPv4-mapped IPv6 literal is
// unwrapped first, so ::ffff:127.0.0.1 cannot slip past the v4 ranges.
func PublicIP(ip net.IP) error {
	if ip == nil {
		return ErrUnresolved
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return ErrBlocked
	}
	for _, n := range blocked {
		if n.Contains(ip) {
			return ErrBlocked
		}
	}
	return nil
}

// Dial resolves addr's host and connects to the first address that answers,
// refusing the host outright when any answer is not public: a resolver that
// returns a public and a private address together is exactly the rebinding
// shape. The returned error is ErrBlocked, ErrUnresolved or the dial error.
func Dial(ctx context.Context, network, addr string, timeout time.Duration) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: timeout}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, ErrUnresolved
	}
	for _, ip := range ips {
		if err = PublicIP(ip); err != nil {
			return nil, err
		}
	}
	var last error = ErrUnresolved
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}
