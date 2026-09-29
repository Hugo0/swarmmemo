package board

// Anonymous keying per salted network prefix (RFC0012 §6.2), owned by builder
// A. While ANON_PREFIX is off every function here keeps today's keying exactly:
// "anon:" + sha256(source), and no client descriptor.
//
// With ANON_PREFIX on, the anonymous account is
//
//	"anon:" + hex(HMAC-SHA256(salt_d, prefix))[:32]
//
// where prefix is the caller's IPv6 /64 or IPv4 /24 (an IPv4-mapped IPv6
// address counts as IPv4, a zone is dropped), or for an unsigned service.call
// (which spends credit) the IPv6 /48, and any source that is not an
// address ("nostr-bridge", "web-public-read") is hashed whole. salt_d is 32
// random bytes made at the first anonymous request of each UTC day. It lives
// in memory only, never in the database, so no backup can recompute a
// pseudonym; a restart mid-day makes a new salt, and anonymous callers get new
// pseudonyms (and a fresh share) for the rest of that day, which is
// acceptable. The previous day's salt is kept only until 01:00 UTC, so an
// exact retry across midnight finds its receipt, and is then destroyed by the
// next anonymous request or by a daily timer, whichever comes first. No raw
// address, prefix, unsalted hash or salt is written.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"sync"
	"time"
)

const (
	// AnonPrefixV6Bits and AnonPrefixV4Bits are the network prefix one
	// anonymous subject covers.
	AnonPrefixV6Bits = 64
	AnonPrefixV4Bits = 24
	// AnonCreditPrefixV6Bits is the IPv6 prefix one anonymous caller of a
	// service (an unsigned service.call, billed in credit) covers: a /48,
	// the smallest allocation a tunnel broker or ISP hands out for free,
	// so holding one does not multiply the credit share by 65,536 /64s
	// (security review 1.21, M3). Posting stays keyed on the /64.
	AnonCreditPrefixV6Bits = 48
	// AnonSaltGraceSeconds is how long into a UTC day the previous day's
	// salt survives, for exact retries across midnight.
	AnonSaltGraceSeconds = 3600
	// anonSaltBytes is the salt's length.
	anonSaltBytes = 32
	// ClientProductMaxChars bounds the client descriptor an adapter records.
	ClientProductMaxChars = 32
)

// anonSalts is the daily salt state (part of Store.design0), in memory only.
// mu guards it and is never held across I/O.
type anonSalts struct {
	mu    sync.Mutex
	day   int64  // UTC day of cur
	cur   []byte // nil until the first anonymous request of a day
	prev  []byte // the previous day's salt until AnonSaltGraceSeconds, else nil
	timer *time.Timer
}

// anonymousAccount is the continuity account of an unsigned caller at now
// (the command's clock, so the salt and the day agree), keyed on v6Bits of an
// IPv6 address: AnonPrefixV6Bits, or AnonCreditPrefixV6Bits for an unsigned
// service.call.
func (s *Store) anonymousAccount(source string, now int64, v6Bits int) string {
	if !s.config.Features.AnonPrefix {
		if v6Bits != AnonPrefixV6Bits {
			// Credit is keyed on the network even while posting is keyed on
			// the whole address: one address per share would let an IPv6
			// caller multiply it without bound.
			return "anon:" + fingerprint([]byte("credit\x00"+anonPrefixKeyBits(source, v6Bits)))
		}
		return "anon:" + fingerprint([]byte(source))
	}
	cur, _ := s.anonSalt(now)
	return anonPseudonym(cur, anonPrefixKeyBits(source, v6Bits))
}

// anonymousActor names an unsigned caller at the command's clock now: its
// pseudonym (keyed on the /48 for a service call, else the /64) and its
// client descriptor. A signed actor is left as it is.
func (s *Store) anonymousActor(ctx context.Context, a *actor, cmd Command, source string, now int64) {
	if a.signed {
		return
	}
	a.account = s.anonymousAccount(source, now, anonymousV6Bits(cmd.Operation))
	a.creditAccount = s.anonymousAccount(source, now, AnonCreditPrefixV6Bits)
	a.client = s.anonymousClient(ctx, source, now)
}

// anonymousV6Bits is the IPv6 prefix an unsigned command of operation op is
// keyed on: the /48 for a service call (it spends credit), else the /64.
func anonymousV6Bits(op string) int {
	if op == "service.call" {
		return AnonCreditPrefixV6Bits
	}
	return AnonPrefixV6Bits
}

// previousAnonymousAccount is the caller's pseudonym (keyed on v6Bits, as
// anonymousAccount) under the previous day's salt while that salt survives
// (until 01:00 UTC), else "". It does no I/O, so it is safe inside a
// transaction.
func (s *Store) previousAnonymousAccount(source string, now int64, v6Bits int) string {
	if !s.config.Features.AnonPrefix {
		return ""
	}
	st := &s.design0.salts
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.prev == nil || st.day != now/86400 || now%86400 >= AnonSaltGraceSeconds {
		return ""
	}
	return anonPseudonym(st.prev, anonPrefixKeyBits(source, v6Bits))
}

// anonymousClient is Subject.Client for an unsigned caller: HMAC(salt_d,
// prefix ‖ client signature), where the prefix is always the /64 (or /24)
// and the client signature is the channel and, over HTTP, the User-Agent
// product token. Empty while ANON_PREFIX is off. It can only narrow a
// prefix's allowance, never widen it: the prefix stays the subject.
func (s *Store) anonymousClient(ctx context.Context, source string, now int64) string {
	if !s.config.Features.AnonPrefix {
		return ""
	}
	cur, _ := s.anonSalt(now)
	mac := hmac.New(sha256.New, cur)
	mac.Write([]byte("client\x00" + anonPrefixKey(source) + "\x00" + ViaFrom(ctx) + "\x00" + clientFrom(ctx)))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

type clientKey struct{}

// WithClient records the caller's client descriptor (over HTTP, the
// User-Agent product token). Only adapters call it. It can only narrow an
// anonymous prefix's allowance, never widen it.
func WithClient(ctx context.Context, product string) context.Context {
	return context.WithValue(ctx, clientKey{}, product)
}

// clientFrom is the descriptor an adapter recorded, or "" when none was or it
// is longer than ClientProductMaxChars.
func clientFrom(ctx context.Context) string {
	product, _ := ctx.Value(clientKey{}).(string)
	if len(product) > ClientProductMaxChars {
		return ""
	}
	return product
}

// anonPrefixKey is the salted hash's input for source: "ip:" and the masked
// prefix for an address, "src:" and the whole string otherwise. The two forms
// cannot collide, nor can prefixes of different lengths.
func anonPrefixKey(source string) string { return anonPrefixKeyBits(source, AnonPrefixV6Bits) }

// anonPrefixKeyBits is anonPrefixKey with an IPv6 address masked to v6Bits.
func anonPrefixKeyBits(source string, v6Bits int) string {
	addr, err := netip.ParseAddr(source)
	if err != nil {
		return "src:" + source
	}
	addr = addr.WithZone("").Unmap()
	bits := v6Bits
	if addr.Is4() {
		bits = AnonPrefixV4Bits
	}
	prefix, err := addr.Prefix(bits)
	if err != nil {
		return "src:" + source
	}
	return "ip:" + prefix.String()
}

func anonPseudonym(salt []byte, key string) string {
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(key))
	return "anon:" + hex.EncodeToString(mac.Sum(nil))[:32]
}

// anonSalt returns today's salt, making it at the first call of a UTC day, and
// the previous day's salt while it survives (empty otherwise), as copies the
// caller owns. It does no I/O.
func (s *Store) anonSalt(now int64) (cur, prev []byte) {
	st := &s.design0.salts
	day := now / 86400
	st.mu.Lock()
	defer st.mu.Unlock()
	st.expireLocked(now)
	if st.cur == nil || st.day != day {
		fresh := make([]byte, anonSaltBytes)
		if _, err := rand.Read(fresh); err != nil {
			// crypto/rand does not fail on supported platforms; never fall
			// back to a predictable salt.
			panic("anonymous salt: " + err.Error())
		}
		wipe(st.prev)
		st.prev = nil
		if st.cur != nil && st.day == day-1 {
			// expireLocked kept yesterday's salt, so this is inside the grace.
			st.prev = st.cur
		} else {
			wipe(st.cur)
		}
		st.day, st.cur = day, fresh
	}
	// Callers get copies: expiry and rotation wipe the state's own slices.
	return append([]byte(nil), st.cur...), append([]byte(nil), st.prev...)
}

// expireLocked drops every salt that may no longer be used at now: the
// previous salt after 01:00 UTC, and a current salt from an earlier day unless
// it is yesterday's and still inside the grace. It reports a change.
func (st *anonSalts) expireLocked(now int64) bool {
	day, inGrace := now/86400, now%86400 < AnonSaltGraceSeconds
	changed := false
	if st.prev != nil && (!inGrace || st.day != day) {
		wipe(st.prev)
		st.prev, changed = nil, true
	}
	if st.cur != nil && st.day != day && !(st.day == day-1 && inGrace) {
		wipe(st.cur)
		st.cur, changed = nil, true
	}
	return changed
}

// expireSalt destroys expired salts without waiting for an anonymous request.
// The daily timer calls it just after 01:00 UTC.
func (s *Store) expireSalt(now int64) {
	st := &s.design0.salts
	st.mu.Lock()
	defer st.mu.Unlock()
	st.expireLocked(now)
}

// scheduleSaltExpiry arms the daily timer that destroys expired salts shortly
// after 01:00 UTC (wall clock), then re-arms itself.
func (s *Store) scheduleSaltExpiry() {
	now := time.Now().UTC()
	next := now.Truncate(24 * time.Hour).Add(AnonSaltGraceSeconds*time.Second + time.Second)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	st := &s.design0.salts
	st.mu.Lock()
	defer st.mu.Unlock()
	st.timer = time.AfterFunc(next.Sub(now), func() {
		s.expireSalt(s.now().Unix())
		s.scheduleSaltExpiry()
	})
}

// openSalt runs at Open. It destroys any salt an earlier build stored in meta
// (a stored salt would let a backup recompute pseudonyms) and, with
// ANON_PREFIX on, arms the daily expiry timer.
func (s *Store) openSalt() error {
	if _, err := s.db.Exec("DELETE FROM meta WHERE key IN ('anon_salt','anon_salt_day','anon_salt_prev')"); err != nil {
		return err
	}
	if s.config.Features.AnonPrefix {
		s.scheduleSaltExpiry()
	}
	return nil
}

// wipe zeroes a secret before it is dropped.
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
