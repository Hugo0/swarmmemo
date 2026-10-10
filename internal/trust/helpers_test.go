package trust

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"
)

// testAsOf is 2026-09-28 00:00 UTC.
const testAsOf = 1790553600

func acct(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

// builder assembles snapshots for tests.
type builder struct {
	snap Snapshot
	seq  int64
	post int
}

func newBuilder(seeds ...string) *builder {
	p := DefaultParams()
	if len(seeds) > 0 {
		p.Seeds = nil
		for _, s := range seeds {
			p.Seeds = append(p.Seeds, acct(s))
		}
	}
	return &builder{snap: Snapshot{Params: p, Meta: Meta{Schema: 1, AsOf: testAsOf, PriorRuns: 30}}}
}

// account registers name, first seen daysAgo, active (one post) on each of
// the last activeDays days, and with prior flow of priorFlow units per run.
func (b *builder) account(name string, daysAgo, activeDays, priorFlow int64) *builder {
	a := acct(name)
	b.snap.Accounts = append(b.snap.Accounts, Record{Type: "account", Account: a, FirstSeen: testAsOf - daysAgo*day})
	for d := int64(1); d <= activeDays; d++ {
		b.post++
		b.snap.Posts = append(b.snap.Posts, Record{Type: "post", ID: "p" + strconv.Itoa(b.post), Account: a, CreatedAt: testAsOf - d*day + 3600})
	}
	if priorFlow > 0 {
		b.snap.Priors = append(b.snap.Priors, Record{Type: "prior", Account: a, FlowSum: priorFlow * b.snap.Meta.PriorRuns, Standing: true})
	}
	return b
}

func (b *builder) endorse(kind, from, to string, value, daysAgo int64) *builder {
	b.seq++
	b.snap.Endorsements = append(b.snap.Endorsements, Record{Type: "endorsement", Seq: b.seq, Kind: kind, Voter: acct(from), Target: acct(to),
		MessageID: "m" + strconv.FormatInt(b.seq, 10), Value: value, CreatedAt: testAsOf - daysAgo*day - 60})
	return b
}

func (b *builder) vouch(from, to string) *builder { return b.endorse("vouch", from, to, 1, 1) }
func (b *builder) vote(from, to string) *builder  { return b.endorse("vote", from, to, 1, 1) }

// vouchWeight is a vouch with an author-chosen weight (parameter version 4).
func (b *builder) vouchWeight(from, to string, weight int64) *builder {
	b.vouch(from, to)
	b.snap.Endorsements[len(b.snap.Endorsements)-1].Weight = weight
	return b
}

func (b *builder) domain(name, domain string) *builder {
	b.snap.Proofs = append(b.snap.Proofs, Record{Type: "proof", Account: acct(name), Kind: "domain", LinkValue: domain, State: "verified",
		CreatedAt: testAsOf - 200*day, CheckedAt: testAsOf - day})
	return b
}

func compute(t testing.TB, b *builder) Output {
	t.Helper()
	out, err := Compute(context.Background(), b.snap)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func scoreOf(out Output, name string) ScoreOut {
	a := acct(name)
	for _, s := range out.Scores {
		if s.Account == a {
			return s
		}
	}
	return ScoreOut{Account: a}
}
