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
	mu          sync.Mutex
	buckets     map[string]bucket
	burst, rate float64       // tokens; tokens per second
	idle        time.Duration // an entry this idle is full again and may be pruned
}

type bucket struct {
	Tokens float64
	At     time.Time
}

func NewLimiter() *Limiter { return NewLimiterRate(120, 30) }

// NewLimiterRate is the same machinery with another burst and refill rate, for
// a key space that is not network origins (the Nostr bridge keys on relays and
// on Nostr keys).
func NewLimiterRate(burst, perSecond float64) *Limiter {
	idle := time.Duration(burst / perSecond * float64(time.Second))
	if idle < time.Minute {
		idle = time.Minute
	}
	return &Limiter{buckets: make(map[string]bucket), burst: burst, rate: perSecond, idle: idle}
}

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
				if now.Sub(v.At) > l.idle {
					delete(l.buckets, k)
				}
			}
			if len(l.buckets) >= 10000 {
				return false
			}
		}
		b = bucket{Tokens: l.burst, At: now}
	}
	b.Tokens += now.Sub(b.At).Seconds() * l.rate
	if b.Tokens > l.burst {
		b.Tokens = l.burst
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
	// Relays and PublishKey describe a bridge (Nostr): the relays it reads and
	// writes, and the key its mirrored events are signed with (an npub), so a
	// reader can check a mirror came from this service.
	Relays     []string `json:"relays,omitempty"`
	PublishKey string   `json:"publish_key,omitempty"`
}
