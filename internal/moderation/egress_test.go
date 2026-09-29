package moderation

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"testing"
)

func TestClassifyAddr(t *testing.T) {
	cases := map[string]string{
		// Public.
		"93.184.216.34":        "",
		"2606:4700:4700::1111": "",
		"8.8.8.8":              "",
		// IPv4 private and reserved.
		"10.1.2.3":        "private_address",
		"127.0.0.1":       "private_address",
		"172.31.255.255":  "private_address",
		"192.168.1.1":     "private_address",
		"100.64.0.1":      "private_address",
		"0.0.0.0":         "private_address",
		"255.255.255.255": "private_address",
		"198.18.0.1":      "private_address",
		"192.0.2.10":      "private_address",
		"224.0.0.1":       "private_address",
		"240.0.0.1":       "private_address",
		// Metadata endpoints, in every spelling.
		"169.254.169.254":                      "metadata",
		"169.254.170.2":                        "metadata",
		"100.100.100.200":                      "metadata",
		"192.0.0.192":                          "metadata",
		"fd00:ec2::254":                        "metadata",
		"::ffff:169.254.169.254":               "metadata", // IPv4-mapped
		"::ffff:a9fe:a9fe":                     "metadata", // the same, in hex
		"64:ff9b::a9fe:a9fe":                   "metadata", // NAT64
		"2002:a9fe:a9fe::1":                    "metadata", // 6to4
		"::a9fe:a9fe":                          "metadata", // IPv4-compatible
		"2001:0:4136:e378:8000:63bf:5601:5601": "metadata", // Teredo: client 169.254.169.254 inverted
		// IPv6 private and reserved.
		"::1":              "private_address",
		"::":               "private_address",
		"::ffff:127.0.0.1": "private_address",
		"::ffff:10.0.0.1":  "private_address",
		"fe80::1":          "private_address",
		"fe80::1%eth0":     "private_address", // zone
		"fc00::1":          "private_address",
		"fd12:3456::1":     "private_address",
		"fec0::1":          "private_address",
		"ff02::1":          "private_address",
		"2001:db8::1":      "private_address",
		"64:ff9b::808:808": "private_address", // NAT64 to a public address: refused whole
		"2002:808:808::1":  "private_address", // 6to4
		"2001::1":          "private_address", // Teredo
		"100::1":           "private_address", // discard
		"3fff::1":          "private_address", // documentation (RFC 9637)
		"::127.0.0.1":      "private_address",
	}
	for in, want := range cases {
		a, err := netip.ParseAddr(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := ClassifyAddr(a); got != want {
			t.Errorf("%s: got %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeHostRefusesAmbiguousLiterals(t *testing.T) {
	for host, want := range map[string]string{
		"example.com":        "",
		"EXAMPLE.com.":       "",
		"[::1]":              "",
		"127.1":              "ambiguous_address",
		"2130706433":         "ambiguous_address",
		"0x7f000001":         "ambiguous_address",
		"0177.0.0.1":         "ambiguous_address",
		"0x7f.0.0.1":         "ambiguous_address",
		"10.0.0.0x1":         "ambiguous_address",
		"":                   "invalid_host",
		"exa mple.com":       "invalid_host",
		"exämple.com":        "invalid_host",
		"a..b":               "invalid_host",
		"host:80":            "invalid_host",
		"-bad.example":       "invalid_host",
		"xn--exmple-cua.com": "",
	} {
		_, _, _, got := normalizeHost(host)
		if got != want {
			t.Errorf("%q: got %q, want %q", host, got, want)
		}
	}
}

func egress(v *env, agent, host string, port int, ips ...string) Decision {
	d := &Destination{Host: host, Port: port}
	for _, ip := range ips {
		d.IPs = append(d.IPs, netip.MustParseAddr(ip))
	}
	return v.e.Screen(context.Background(), SurfaceRunEgress, Subject{ID: "conn", Agent: agent}, Content{Egress: d})
}

func TestEgressDefaults(t *testing.T) {
	v := newEnv(t, "")
	pub := "93.184.216.34"
	cases := []struct {
		host string
		port int
		ips  []string
		want Action
		cat  string
	}{
		{"api.example.com", 443, []string{pub}, Flag, "new_domain"},
		{"www.example.com", 443, []string{pub}, Allow, ""}, // same site, seen now
		{"10.0.0.5", 80, nil, Block, "private_address"},
		{"169.254.169.254", 80, nil, Block, "metadata"},
		{"[fd00:ec2::254]", 80, nil, Block, "metadata"},
		{"[::ffff:169.254.169.254]", 80, nil, Block, "metadata"},
		{"metadata.google.internal", 80, []string{pub}, Block, "metadata"},
		{"localhost", 80, []string{"127.0.0.1"}, Block, "private_address"},
		{"db.svc", 5432, []string{pub}, Block, "private_address"},
		{"2130706433", 80, nil, Block, "ambiguous_address"},
		{"rebind.example.org", 80, []string{pub, "127.0.0.1"}, Block, "private_address"}, // one bad answer blocks all
		{"unresolved.example.org", 80, nil, Block, "invalid_host"},
		{"xmr.pool.minexmr.com", 443, []string{pub}, Block, "mining_pool"},
		{"other.example.net", 3333, []string{pub}, Flag, "mining_port"},
		{"8.8.8.8", 53, nil, Flag, "ip_literal"},
		{"api.example.com", 0, []string{pub}, Block, "invalid_host"},
	}
	for i, c := range cases {
		d := egress(v, fmt.Sprint("agent", i), c.host, c.port, c.ips...)
		if d.Action != c.want || (c.cat != "" && d.Category != c.cat) {
			t.Errorf("%s:%d: %s %s %+v", c.host, c.port, d.Action, d.Category, d.Scores)
		}
	}
}

func TestEgressFloorCannotBeSoftened(t *testing.T) {
	body := `{"schema":1,"version":2,"surfaces":{"run.egress":{"classifiers":["egress"],"on_unavailable":"block","categories":{"metadata":{"thresholds":[{"at":1,"action":"allow"}]}}}}}`
	if _, err := ParsePolicy([]byte(body)); err == nil {
		t.Fatal("a policy allowing metadata addresses was accepted")
	}
	// A policy that simply leaves the floor out still blocks it.
	v := newEnv(t, `{"schema":1,"version":2,"surfaces":{"run.egress":{"classifiers":["egress"],"on_unavailable":"block","categories":{}}}}`)
	if d := egress(v, "a", "169.254.169.254", 80); d.Action != Block || d.Category != "metadata" {
		t.Fatalf("%+v", d)
	}
	if d := egress(v, "a", "10.0.0.1", 80); d.Action != Block {
		t.Fatalf("%+v", d)
	}
}

func TestEgressRateLimitPerAgent(t *testing.T) {
	v := newEnv(t, `{"schema":1,"version":1,"egress":{"allow_domains":["example.com"]},"surfaces":{"run.egress":{"classifiers":["egress","rate"],"on_unavailable":"block","rate":{"window_seconds":60,"max":3},"categories":{"rate":{"hard":true,"thresholds":[{"at":1,"action":"block"}]}}}}}`)
	for i := 0; i < 3; i++ {
		if d := egress(v, "busy", "api.example.com", 443, "93.184.216.34"); d.Action != Allow {
			t.Fatalf("#%d: %+v", i, d)
		}
	}
	if d := egress(v, "busy", "api.example.com", 443, "93.184.216.34"); d.Action != Block || d.Category != "rate" {
		t.Fatalf("over the rate: %+v", d)
	}
	if d := egress(v, "quiet", "api.example.com", 443, "93.184.216.34"); d.Action != Allow {
		t.Fatalf("another agent: %+v", d)
	}
}

func TestEgressNewDomainReview(t *testing.T) {
	v := newEnv(t, "")
	ctx := context.Background()
	if d := egress(v, "a", "c2.evil.example", 443, "93.184.216.34"); d.Action != Flag || d.Category != "new_domain" {
		t.Fatalf("%+v", d)
	}
	items, _ := v.e.Queue(ctx, QueueQuery{Surface: SurfaceRunEgress})
	if len(items) != 1 {
		t.Fatalf("queue %+v", items)
	}
	if _, err := v.e.Reject(ctx, items[0].ID, "steward", "c2"); err != nil {
		t.Fatal(err)
	}
	// Rejected: the whole site is denied from now on.
	if d := egress(v, "b", "other.evil.example", 443, "93.184.216.34"); d.Action != Block || d.Category != "denied_domain" {
		t.Fatalf("after reject: %+v", d)
	}
	// Approved: allowed, and not flagged again.
	egress(v, "a", "docs.fine.example", 443, "93.184.216.34")
	items, _ = v.e.Queue(ctx, QueueQuery{Surface: SurfaceRunEgress})
	if _, err := v.e.Approve(ctx, items[0].ID, "steward", ""); err != nil {
		t.Fatal(err)
	}
	if d := egress(v, "c", "api.fine.example", 443, "93.184.216.34"); d.Action != Allow {
		t.Fatalf("after approve: %+v", d)
	}
}

// rebinder answers a public address the first time and loopback after, the
// DNS rebinding pattern.
type rebinder struct {
	mu    sync.Mutex
	calls int
}

func (r *rebinder) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.calls == 1 {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}

type fixed []netip.Addr

func (f fixed) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f, nil
}

func TestEgressDialerPinsTheScreenedAddress(t *testing.T) {
	v := newEnv(t, `{"schema":1,"version":1,"egress":{"allow_domains":["rebind.example"]}}`)
	r := &rebinder{}
	var dialed []string
	fakeDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		c1, c2 := net.Pipe()
		c2.Close()
		return c1, nil
	}
	d := &EgressDialer{Engine: v.e, Resolver: r, Dial: fakeDial}
	subj := Subject{ID: "run", Agent: "a"}
	// First connection: one resolution (public), screened, and exactly that
	// address dialed.
	conn, dec, err := d.DialContext(context.Background(), subj, "tcp", "x.rebind.example:443")
	if err != nil || dec.Action != Allow || r.calls != 1 || len(dialed) != 1 || dialed[0] != "93.184.216.34:443" {
		t.Fatalf("first: %v %+v calls %d dialed %v", err, dec, r.calls, dialed)
	}
	conn.Close()
	// The name now rebinds to loopback. The next connection resolves once
	// more, screens that answer and refuses it: nothing is dialed.
	dialed = nil
	_, dec, err = d.DialContext(context.Background(), subj, "tcp", "x.rebind.example:443")
	if !errors.Is(err, ErrEgressRefused) || dec.Action != Block || r.calls != 2 || len(dialed) != 0 {
		t.Fatalf("rebound: %v %+v calls %d dialed %v", err, dec, r.calls, dialed)
	}
	// A mixed answer is refused whole, whatever order the resolver uses.
	d.Resolver = fixed{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.1")}
	if _, dec, err := d.DialContext(context.Background(), Subject{ID: "run", Agent: "a"}, "tcp", "x.rebind.example:443"); err == nil || dec.Action != Block || len(dialed) != 0 {
		t.Fatalf("mixed answer: %v %+v", err, dec)
	}
	// Literals are dialed as given, never resolved.
	d.Resolver = fixed{}
	if _, dec, err := d.DialContext(context.Background(), Subject{ID: "run", Agent: "a"}, "tcp", "[::ffff:169.254.169.254]:80"); err == nil || dec.Category != "metadata" {
		t.Fatalf("mapped metadata literal: %v %+v", err, dec)
	}
}

// Jev's dialer is internal/safenet's: a loopback or private target is Jev
// being unavailable, and nothing is dialled.
func TestJevDialRefusesNonPublic(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	for _, addr := range []string{ln.Addr().String(), "[::1]:443", "10.0.0.1:443", "169.254.169.254:80"} {
		if conn, err := safeDial(context.Background(), "tcp", addr); !errors.Is(err, errJevUnavailable) {
			if conn != nil {
				conn.Close()
			}
			t.Fatalf("%s: %v", addr, err)
		}
	}
}
