package httpapi

import (
	"net"
	"sync"
	"time"
)

// Limiter is the per-origin network request bucket: a burst of 120 requests
// refilling at 30 per second, tracking at most 10000 origins. HTTP and the
// connection-oriented constrained transports share one instance, so a peer has
// one budget whichever wire it uses. A transport whose source address can be
// spoofed (UDP) gets its own instance of the same machinery, so forged sources
// can neither spend a real client's HTTP budget nor fill the shared table.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]bucket
}

type bucket struct {
	Tokens float64
	At     time.Time
}

func NewLimiter() *Limiter { return &Limiter{buckets: make(map[string]bucket)} }

// Admit spends one token for peer and reports whether the request may proceed.
func (l *Limiter) Admit(peer string) bool {
	peer = limiterKey(peer)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, exists := l.buckets[peer]
	if !exists {
		if len(l.buckets) >= 10000 {
			for k, v := range l.buckets {
				if now.Sub(v.At) > time.Minute {
					delete(l.buckets, k)
				}
			}
			if len(l.buckets) >= 10000 {
				return false
			}
		}
		b = bucket{Tokens: 120, At: now}
	}
	b.Tokens += now.Sub(b.At).Seconds() * 30
	if b.Tokens > 120 {
		b.Tokens = 120
	}
	b.At = now
	allowed := b.Tokens >= 1
	if allowed {
		b.Tokens--
	}
	l.buckets[peer] = b
	return allowed
}

// limiterKey groups IPv6 peers by /64. One subscriber is routinely handed a
// whole /64, so keying on the full address would let a single host fill the
// table with fresh entries and lock every new origin out. IPv4 and anything
// that is not an address are keyed as given.
func limiterKey(peer string) string {
	ip := net.ParseIP(peer)
	if ip == nil || ip.To4() != nil {
		return peer
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// TransportCapability advertises one enabled constrained transport in
// /capabilities (RFC0007 rule 2): where it listens, whether it can write, the
// verbs a complete write needs, and its reduced byte limits. A client that only
// has this wire can learn before trying that it cannot finish a post.
type TransportCapability struct {
	Name         string         `json:"name"`
	Address      string         `json:"address"`
	Example      string         `json:"example"`
	Access       string         `json:"access"`
	WriteVerbs   []string       `json:"write_verbs"`
	Signed       string         `json:"signed_commands"`
	OriginKey    string         `json:"anonymous_origin"`
	Limits       map[string]int `json:"limits"`
	Instructions string         `json:"instructions"`
}
