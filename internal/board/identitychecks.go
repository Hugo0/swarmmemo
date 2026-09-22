package board

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strings"
	"time"
)

// The rechecker re-runs live link checks on a schedule. Like webhook delivery it
// is a bounded worker pool over a table: a fixed number of workers, one global
// lookup rate, a lease on each row, and nothing at all unless
// StartIdentityChecks is called. A lookup never runs inside a database
// transaction, so a slow resolver cannot hold the single writer.

const (
	linkMinInterval   = 600       // seconds between lookups of one link, however often it is linked again
	linkRecheckEvery  = 86400     // a passing link is rechecked about daily
	linkConfirmAfter  = 3600      // a verified link that failed once is rechecked soon
	linkRetryUnknown  = 900       // after a resolver error, before trying again
	linkLapseFailures = 2         // consecutive definite failures before a verified link lapses
	linkStaleAfter    = 3 * 86400 // a verified link with no conclusive answer this long lapses anyway
	linkLeaseSeconds  = 120
	linkCheckWorkers  = 4 // global concurrency cap
	linkCheckSpacing  = 500 * time.Millisecond
	linkCheckPoll     = 5 * time.Second
	linkLookupTimeout = 4 * time.Second
	linkTXTMaxRecords = 32
	linkTXTMaxBytes   = 512
	// Per key: a burst of one lookup per link slot, refilling over an hour.
	linkKeyLookupsHour = float64(IdentityLinkMaxPerKey)
	linkKeyRateEntries = 8192
)

type linkOutcome int

const (
	linkPassed      linkOutcome = iota // the live state proves the link
	linkFailed                         // a definite answer that does not prove it
	linkUnreachable                    // no conclusive answer (timeout, SERVFAIL, cancelled)
)

// defaultTXTLookup uses Go's own resolver against the host's configured
// nameserver. The name is absolute, so no search domain is appended; the
// resolver bounds the message itself (UDP with EDNS, TCP fallback capped at
// 64 KiB), and txtAuthorizes bounds what is examined.
func defaultTXTLookup(ctx context.Context, name string) ([]string, error) {
	resolver := &net.Resolver{PreferGo: true, StrictErrors: true}
	return resolver.LookupTXT(ctx, name)
}

// txtAuthorizes reports whether the TXT answer names this fingerprint. Records
// are hostile input: only the first linkTXTMaxRecords are examined, any record
// longer than linkTXTMaxBytes is skipped unread, and a match must be the whole
// record apart from surrounding whitespace. Case is ignored in both the key and
// the hex value, since DNS tooling varies. A domain may list several keys.
func txtAuthorizes(records []string, fingerprint string) bool {
	if !fingerprintRE.MatchString(fingerprint) {
		return false
	}
	for i, record := range records {
		if i >= linkTXTMaxRecords {
			break
		}
		if len(record) > linkTXTMaxBytes {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimSpace(record), "=")
		if ok && strings.EqualFold(key+"=", IdentityLinkTXTPrefix) && len(value) == 64 && strings.EqualFold(value, fingerprint) {
			return true
		}
	}
	return false
}

func (s *Store) checkDomainLink(ctx context.Context, fingerprint, domain string) linkOutcome {
	ctx, cancel := context.WithTimeout(ctx, linkLookupTimeout)
	defer cancel()
	records, err := s.identityTXT(ctx, "_swarmmemo."+domain+".")
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return linkFailed
		}
		return linkUnreachable
	}
	if txtAuthorizes(records, fingerprint) {
		return linkPassed
	}
	return linkFailed
}

// linkState is the stored state a check reads and writes.
type linkState struct {
	State                       string
	Failures, CheckedAt, Lapsed int64
	Next                        int64
	Reason                      string
}

// nextLinkState is the whole state machine, kept pure so every transition is
// tested without a resolver. jitter is in [0,1) and spreads the daily recheck
// over ±10% so links made together are not all looked up together.
func nextLinkState(current linkState, outcome linkOutcome, now int64, jitter float64) linkState {
	daily := now + int64(float64(linkRecheckEvery)*(0.9+0.2*jitter))
	next := current
	switch outcome {
	case linkPassed:
		return linkState{State: "verified", CheckedAt: now, Next: daily}
	case linkFailed:
		next.Failures++
		next.Reason = "no matching record"
		switch current.State {
		case "verified":
			next.Next = now + linkConfirmAfter
			if next.Failures >= linkLapseFailures {
				next.State, next.Lapsed, next.Next = "lapsed", now, daily
			}
		case "lapsed":
			next.Next = daily
		default:
			// Never verified: back off from ten minutes to a day.
			delay := int64(linkMinInterval)
			for i := int64(1); i < next.Failures && delay < linkRecheckEvery; i++ {
				delay *= 6
			}
			next.Next = now + min(delay, linkRecheckEvery)
		}
	default:
		// An inconclusive lookup is not evidence either way, so it does not count
		// toward lapsing; but a verified link nobody could confirm for days is not
		// shown as verified any longer.
		next.Reason = "lookup failed"
		next.Next = now + linkConfirmAfter
		if current.State == "verified" {
			next.Next = now + linkRetryUnknown
			if now-current.CheckedAt >= linkStaleAfter {
				next.State, next.Lapsed, next.Next = "lapsed", now, daily
			}
		}
	}
	return next
}

// checkLinkOnce leases one due link, checks it and records the outcome. It
// reports whether it found work.
func (s *Store) checkLinkOnce(ctx context.Context) (bool, error) {
	now := s.now().Unix()
	var agent, kind, value string
	var current linkState
	err := s.db.QueryRowContext(ctx, `UPDATE identity_links SET next_check_at=?,attempted_at=? WHERE rowid=(
 SELECT rowid FROM identity_links WHERE next_check_at>0 AND next_check_at<=? ORDER BY next_check_at LIMIT 1)
 RETURNING agent,kind,value,state,failures,checked_at,lapsed_at`, now+linkLeaseSeconds, now, now).
		Scan(&agent, &kind, &value, &current.State, &current.Failures, &current.CheckedAt, &current.Lapsed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	check := linkKinds[kind].check
	if check == nil {
		_, err = s.db.ExecContext(ctx, "UPDATE identity_links SET next_check_at=0 WHERE agent=? AND kind=? AND value=?", agent, kind, value)
		return true, err
	}
	if !s.admitLinkLookup(agent) {
		// Deferred, not failed: nothing was looked up, so the state is unchanged.
		_, err = s.db.ExecContext(ctx, "UPDATE identity_links SET next_check_at=? WHERE agent=? AND kind=? AND value=?", now+linkMinInterval, agent, kind, value)
		return true, err
	}
	outcome := check(s, ctx, agent, value)
	if ctx.Err() != nil {
		// Shutting down is not a failed check; the lease expires and the row is
		// picked up again after restart.
		return true, nil
	}
	next := nextLinkState(current, outcome, s.now().Unix(), s.identityJitter())
	settle, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err = s.db.ExecContext(settle, "UPDATE identity_links SET state=?,failures=?,checked_at=?,lapsed_at=?,next_check_at=?,last_error=? WHERE agent=? AND kind=? AND value=?",
		next.State, next.Failures, next.CheckedAt, next.Lapsed, next.Next, next.Reason, agent, kind, value)
	return true, err
}

// admitLinkLookup is the per-key bound on actual lookups. The per-link
// interval alone would let a key unlink and link again to buy a fresh lookup
// each time; this bucket counts lookups per key however its links churn.
func (s *Store) admitLinkLookup(agent string) bool {
	s.identityRateMu.Lock()
	defer s.identityRateMu.Unlock()
	now := s.now()
	previous, known := s.identityRates[agent]
	bucket := privateBucket(now, previous, linkKeyLookupsHour/3600.0, linkKeyLookupsHour)
	if bucket.tokens < 1 {
		s.identityRates[agent] = bucket
		return false
	}
	if !known && len(s.identityRates) >= linkKeyRateEntries {
		// An entry idle for an hour has fully refilled, so forgetting it changes
		// nothing; if none has, new keys wait rather than the map growing.
		for key, b := range s.identityRates {
			if now.Sub(b.seen) >= time.Hour {
				delete(s.identityRates, key)
			}
		}
		if len(s.identityRates) >= linkKeyRateEntries {
			return false
		}
	}
	bucket.tokens--
	s.identityRates[agent] = bucket
	return true
}

// StartIdentityChecks runs the rechecker until ctx is cancelled; pair it with
// StopIdentityChecks. A single ticker hands out lookup permits, so the global
// rate is fixed whatever the backlog, and at most linkCheckWorkers lookups are
// ever in flight.
func (s *Store) StartIdentityChecks(ctx context.Context) {
	s.identityChecks.Store(true)
	permits := make(chan struct{})
	s.identityWG.Add(1)
	go func() {
		defer s.identityWG.Done()
		ticker := time.NewTicker(linkCheckSpacing)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				select {
				case permits <- struct{}{}:
				default: // no idle worker; permits never accumulate into a burst
				}
			}
		}
	}()
	for i := 0; i < linkCheckWorkers; i++ {
		s.identityWG.Add(1)
		go func() {
			defer s.identityWG.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case <-permits:
				}
				worked, err := survive(func() (bool, error) { return s.checkLinkOnce(ctx) })
				if err != nil || !worked {
					select {
					case <-ctx.Done():
						return
					case <-time.After(linkCheckPoll):
					}
				}
			}
		}()
	}
}

// survive runs one unit of background work and turns a panic into an error, so
// a single hostile input costs one item rather than the worker that met it.
// Recovering once per goroutine instead would let a handful of panics retire
// every worker, silently stopping the job for good.
func survive(step func() (bool, error)) (worked bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			worked, err = false, errors.New("background step panicked")
		}
	}()
	return step()
}

// StopIdentityChecks waits for in-flight lookups to return.
func (s *Store) StopIdentityChecks() { s.identityWG.Wait() }
