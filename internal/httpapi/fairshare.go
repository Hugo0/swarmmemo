package httpapi

import (
	"net"
	"net/http"
	"sync"
)

// Fair shares of the request pool (C635). Server.inflight is one pool of 128
// slots for every request; a request that finds it full is answered 503 busy.
// On 2026-10-30 a crawler on four address ranges (no User-Agent, GET
// /api/agent/ID and /e/ID) filled it with slow reads queued on the single
// SQLite connection, and 14% of everyone else's requests were shed with it.
// The per-address token bucket (Limiter) did not stop it: each address stayed
// under its rate, and the ranges held many addresses.
//
// So an anonymous read (GET or HEAD without an Authorization header) also
// takes a share for its network: the IPv4 /24 or IPv6 /48, and the wider IPv4
// /16 or IPv6 /32. A network may hold at most half of the slots still free
// for anonymous reads, never less than its floor. An idle pool lets one
// network hold dozens of slots at once; a contended one turns the network
// holding the most away first, with 429 request_rate and Retry-After, while a
// network holding nothing is always let in while any slot is free. The rule
// never touches signed commands (POST), hosted tokens, /health, loopback or
// OWN_NETWORKS, and anonymous reads leave anonReserve slots for them.
const (
	anonReserve     = 16 // pool slots anonymous reads never take
	shareFloor      = 4  // slots a /24 (/48) may always hold while one is free
	shareFloorWide  = 8  // slots a /16 (/32) may always hold while one is free
	shareRetryAfter = 2
)

type shareTable struct {
	mu   sync.Mutex
	held map[string]int
}

// acquire takes one share for each key when every key holds less than its
// floor or less than half of free (the anonymous slots left before this
// request), and reports whether it did. Every true is paired with release.
func (t *shareTable) acquire(narrow, wide string, free int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.held == nil {
		t.held = map[string]int{}
	}
	within := func(key string, floor int) bool {
		h := t.held[key]
		return h < floor || 2*h < free
	}
	if !within(narrow, shareFloor) || (wide != narrow && !within(wide, shareFloorWide)) {
		return false
	}
	t.held[narrow]++
	if wide != narrow {
		t.held[wide]++
	}
	return true
}

func (t *shareTable) release(narrow, wide string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	keys := []string{narrow, wide}
	if wide == narrow {
		keys = keys[:1]
	}
	for _, key := range keys {
		if t.held[key]--; t.held[key] <= 0 {
			delete(t.held, key)
		}
	}
}

// shareKeys are the peer's share keys: its /24 (/48) and /16 (/32), and
// false for a peer the rule exempts (loopback, OWN_NETWORKS). A peer that is
// not an address is one key.
func (s *Server) shareKeys(peer string) (narrow, wide string, ok bool) {
	ip := net.ParseIP(peer)
	if ip != nil && ip.IsLoopback() {
		return "", "", false
	}
	narrow, own := s.networkPrefix(peer)
	if own {
		return "", "", false
	}
	if ip == nil {
		return narrow, narrow, true
	}
	if v4 := ip.To4(); v4 != nil {
		return narrow, v4.Mask(net.CIDRMask(16, 32)).String() + "/16", true
	}
	return narrow, ip.Mask(net.CIDRMask(32, 128)).String() + "/32", true
}

// anonymousRead is a request the fair-share rule applies to.
func anonymousRead(r *http.Request) bool {
	return readMethod(r) && r.Header.Get("Authorization") == "" && r.URL.Path != "/health"
}
