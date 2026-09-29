package moderation

// Egress screening for code runs (surface run.egress).
//
// DNS rebinding is answered by construction, not by a second check: the
// caller resolves a host once, the engine judges every address that
// resolution returned (one blocked answer blocks the host), and the caller
// dials only those judged addresses, never the name again. EgressDialer is
// that sequence. A hostname whose resolver answers a public address now and
// a private one later therefore reaches, at most, the public address it was
// judged on.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Resolver resolves a host name; *net.Resolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// reserved are the address ranges no run may reach, beyond what the netip
// predicates already cover. Transition ranges that embed an IPv4 address
// (NAT64, 6to4, Teredo, IPv4-compatible) are refused whole: their embedded
// address is checked too, only to name a metadata target as such.
var reserved = func() []netip.Prefix {
	var out []netip.Prefix
	for _, c := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/96", "::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64",
		"2001::/32", "2001:2::/48", "2001:10::/28", "2001:20::/28", "2001:db8::/32", "2002::/16",
		"3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}()

// metadataAddrs are cloud instance-metadata endpoints (AWS, GCP, Azure,
// DigitalOcean, OpenStack: 169.254.169.254; AWS ECS 169.254.170.2; AWS DNS and
// NTP; Alibaba 100.100.100.200; Oracle 192.0.0.192; AWS IPv6 fd00:ec2::254).
// All are inside reserved ranges already; naming them makes the reason exact.
var metadataAddrs = func() map[netip.Addr]bool {
	m := map[netip.Addr]bool{}
	for _, a := range []string{"169.254.169.254", "169.254.170.2", "169.254.169.253", "169.254.169.123",
		"100.100.100.200", "192.0.0.192", "fd00:ec2::254", "fd00:ec2::23", "fd00:ec2::123"} {
		m[netip.MustParseAddr(a)] = true
	}
	return m
}()

// metadataHosts are names that resolve to a metadata service inside clouds.
var metadataHosts = map[string]bool{
	"metadata": true, "metadata.google.internal": true, "metadata.goog": true, "instance-data": true,
	"instance-data.ec2.internal": true, "metadata.azure.internal": true, "metadata.tencentyun.com": true,
}

// internalSuffixes are names that only resolve inside a private network.
var internalSuffixes = []string{"localhost", "local", "internal", "lan", "home.arpa", "localdomain", "intranet", "corp", "private", "svc", "cluster.local"}

// ClassifyAddr is the single address decision: "" for a public unicast
// address, "metadata" for a cloud metadata endpoint (including one embedded
// in an IPv6 transition address), "private_address" for anything else not
// public. Zones are dropped and IPv4-mapped addresses unwrapped first.
func ClassifyAddr(a netip.Addr) string {
	if !a.IsValid() {
		return "invalid_host"
	}
	a = a.WithZone("").Unmap()
	if metadataAddrs[a] {
		return "metadata"
	}
	if v4, ok := embeddedV4(a); ok && metadataAddrs[v4] {
		return "metadata"
	}
	if !a.IsGlobalUnicast() || a.IsLoopback() || a.IsPrivate() || a.IsUnspecified() || a.IsMulticast() ||
		a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() {
		return "private_address"
	}
	for _, p := range reserved {
		if p.Contains(a) {
			return "private_address"
		}
	}
	return ""
}

// embeddedV4 extracts the IPv4 address inside NAT64, 6to4, Teredo and
// IPv4-compatible IPv6 addresses.
var (
	nat64Prefix      = netip.MustParsePrefix("64:ff9b::/96")
	nat64LocalPrefix = netip.MustParsePrefix("64:ff9b:1::/48")
	compatPrefix     = netip.MustParsePrefix("::/96")
	sixToFourPrefix  = netip.MustParsePrefix("2002::/16")
	teredoPrefix     = netip.MustParsePrefix("2001::/32")
)

func embeddedV4(a netip.Addr) (netip.Addr, bool) {
	if !a.Is6() {
		return netip.Addr{}, false
	}
	b := a.As16()
	switch {
	case nat64Prefix.Contains(a), nat64LocalPrefix.Contains(a), compatPrefix.Contains(a):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case sixToFourPrefix.Contains(a):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	case teredoPrefix.Contains(a): // Teredo: the client address is inverted
		return netip.AddrFrom4([4]byte{b[12] ^ 0xff, b[13] ^ 0xff, b[14] ^ 0xff, b[15] ^ 0xff}), true
	}
	return netip.Addr{}, false
}

// normalizeHost lowercases, drops one trailing dot and IPv6 brackets, and
// says whether host is an address literal.
func normalizeHost(host string) (string, netip.Addr, bool, string) {
	h := strings.ToLower(strings.TrimSpace(host))
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	h = strings.TrimSuffix(h, ".")
	if h == "" || len(h) > 253 {
		return h, netip.Addr{}, false, "invalid_host"
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return h, a, true, ""
	}
	if strings.ContainsAny(h, ":%") {
		return h, netip.Addr{}, false, "invalid_host"
	}
	// A name whose last label is numeric, or hex, is an address in a form
	// some resolvers accept and netip does not (127.1, 2130706433,
	// 0x7f000001, 0177.0.0.1): never a real DNS name, always refused.
	last := h[strings.LastIndexByte(h, '.')+1:]
	if isDigits(last) || strings.HasPrefix(last, "0x") {
		return h, netip.Addr{}, false, "ambiguous_address"
	}
	if !domainRE.MatchString(h) {
		return h, netip.Addr{}, false, "invalid_host"
	}
	return h, netip.Addr{}, false, ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// site is the name "new domain" is judged by: the last two labels, or three
// under a short second-level label such as co.uk, so one site's endless
// subdomains count once.
func site(host string) string {
	labels := strings.Split(host, ".")
	n := len(labels)
	if n <= 2 {
		return host
	}
	if len(labels[n-1]) == 2 && len(labels[n-2]) <= 3 {
		return strings.Join(labels[n-3:], ".")
	}
	return strings.Join(labels[n-2:], ".")
}

func suffixMatch(host string, list []string) bool {
	for _, d := range list {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// classifyEgress scores one destination. Every finding scores 1.
func (e *Engine) classifyEgress(ctx context.Context, pol *Policy, s Surface, subj Subject, c Content, now int64) (classResult, error) {
	r := classResult{scores: map[string]float64{}, model: "egress"}
	d := c.Egress
	if d == nil || d.Port < 1 || d.Port > 65535 {
		r.scores["invalid_host"] = 1
		return r, nil
	}
	host, literal, isLiteral, bad := normalizeHost(d.Host)
	if bad != "" {
		r.scores[bad] = 1
		return r, nil
	}
	ips := d.IPs
	if isLiteral {
		ips = []netip.Addr{literal}
		r.scores["ip_literal"] = 1
	} else {
		labels := strings.Count(host, ".") + 1
		switch {
		case metadataHosts[host]:
			r.scores["metadata"] = 1
		case labels == 1 || suffixMatch(host, internalSuffixes):
			r.scores["private_address"] = 1
		}
		if suffixMatch(host, *pol.Egress.MiningPools) {
			r.scores["mining_pool"] = 1
		}
		if suffixMatch(host, *pol.Egress.DenyDomains) {
			r.scores["denied_domain"] = 1
		}
		if len(ips) == 0 {
			r.scores["invalid_host"] = 1 // unresolved: nothing the caller could safely dial
		}
	}
	for _, ip := range ips {
		if cat := ClassifyAddr(ip); cat != "" {
			r.scores[cat] = 1
		}
		for _, p := range pol.Egress.denyCIDRs {
			if p.Contains(ip.WithZone("").Unmap()) {
				r.scores["denied_address"] = 1
			}
		}
	}
	if slices.Contains(*pol.Egress.MiningPorts, d.Port) {
		r.scores["mining_port"] = 1
	}
	if isLiteral || len(r.scores) > 0 || suffixMatch(host, *pol.Egress.AllowDomains) {
		return r, nil
	}
	// A site no run has reached before is flagged; a reviewer's reject denies it.
	st := site(host)
	var state string
	err := e.db.QueryRowContext(ctx, "SELECT state FROM moderation_domains WHERE site=?", st).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		r.scores["new_domain"] = 1
		if _, err = e.db.ExecContext(ctx, "INSERT INTO moderation_domains(site,state,first_seen,first_agent,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(site) DO NOTHING", st, "seen", now, bound(subj.Agent, 128), now); err != nil {
			return r, err
		}
	case err != nil:
		return r, err
	case state == "denied":
		r.scores["denied_domain"] = 1
	}
	return r, nil
}

// EgressDialer is the dial path a code runner's egress proxy uses: resolve
// once, screen every answer, dial only a screened address. Its DialContext
// never resolves a name twice, so a rebinding resolver cannot swap a private
// address in after the check.
type EgressDialer struct {
	Engine   *Engine
	Resolver Resolver // default net.DefaultResolver
	Dial     func(ctx context.Context, network, addr string) (net.Conn, error)
	Timeout  time.Duration // per resolution and per dial; default 10 s
}

// ErrEgressRefused is returned (wrapped) when a decision refuses a connection.
var ErrEgressRefused = errors.New("moderation: egress refused")

// DialContext screens network/addr for subj and connects. The decision is
// returned whether or not the connection was allowed.
func (d *EgressDialer) DialContext(ctx context.Context, subj Subject, network, addr string) (net.Conn, Decision, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, Decision{Action: Block}, fmt.Errorf("%w: %v", ErrEgressRefused, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		port = 0
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dest := &Destination{Host: host, Port: port}
	if norm, _, literal, bad := normalizeHost(host); bad == "" && !literal {
		resolver := d.Resolver
		if resolver == nil {
			resolver = net.DefaultResolver
		}
		rctx, cancel := context.WithTimeout(ctx, timeout)
		ips, _ := resolver.LookupNetIP(rctx, "ip", norm)
		cancel()
		dest.IPs = ips
	}
	dec := d.Engine.Screen(ctx, SurfaceRunEgress, subj, Content{Egress: dest})
	if !dec.Action.Proceed() {
		return nil, dec, fmt.Errorf("%w: %s", ErrEgressRefused, dec.Reason)
	}
	targets := dest.IPs
	if _, a, literal, _ := normalizeHost(host); literal {
		targets = []netip.Addr{a}
	}
	dial := d.Dial
	if dial == nil {
		dialer := &net.Dialer{Timeout: timeout}
		dial = dialer.DialContext
	}
	var last error = fmt.Errorf("%w: no address", ErrEgressRefused)
	for _, ip := range targets {
		conn, err := dial(ctx, network, net.JoinHostPort(ip.WithZone("").Unmap().String(), strconv.Itoa(port)))
		if err == nil {
			return conn, dec, nil
		}
		last = err
	}
	return nil, dec, last
}
