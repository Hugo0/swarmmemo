package trust

import (
	"strings"
	"testing"
)

// Tests of parameter version 3: the trust-model simulation's fixes (C148).

// Spend is seed at cost, capped at spend_cap_cents (theta2) per account, and
// spend paid to oneself is not seed (rule 1): paying a resource on one's own
// verified domain, paying an account in one's own root, and paying an
// account one funded (or that funded one) within funded_days. A bounty
// reward is a transfer, so credit recycled through one's own bounties counts
// once: the poster's escrow is not spend, and the worker's spend back to the
// poster is self-dealt.
func TestStandingSpendSelfDealtAndCapped(t *testing.T) {
	D := dayOf(testAsOf)
	build := func(spends []Record, transfers []Record) Output {
		b := newBuilder("s1")
		b.account("s1", 100, 10, 0).account("a", 50, 5, 0).account("b", 50, 5, 0).account("c", 50, 5, 0).account("sock", 50, 5, 0)
		b.domain("a", "a.example").domain("sock", "a.example") // a and sock share a root
		b.snap.Spends = spends
		b.snap.Transfers = transfers
		return compute(t, b)
	}
	spend := func(from, to, host string, dollars int64) Record {
		return Record{Type: "spend", Account: acct(from), Day: D - 1, Amount: dollars * 1000000, To: to, LinkValue: host}
	}
	seedOf := func(out Output, name string) int64 {
		for _, r := range standingOf(out, name).Breakdown {
			if r.Source == "spend" {
				return r.SeedCents
			}
		}
		return 0
	}
	// Honest spend counts at cost (decayed one day), up to the cap.
	out := build([]Record{spend("b", "", "", 1), spend("c", "", "api.other.example", 50)}, nil)
	if seedOf(out, "b") != 99 || seedOf(out, "c") != 200 || out.Standing.Inputs["spend"] != 2 || out.Standing.Inputs["spend_self_dealt"] != 0 {
		t.Fatalf("honest spend: b %d c %d inputs %v", seedOf(out, "b"), seedOf(out, "c"), out.Standing.Inputs)
	}
	// Self-pay: a's own domain (any host under it), an account in a's root,
	// a itself.
	out = build([]Record{spend("a", "", "pay.a.example", 1), spend("a", acct("sock"), "", 1), spend("a", acct("a"), "", 1)}, nil)
	if seedOf(out, "a") != 0 || out.Standing.Inputs["spend_self_dealt"] != 3 {
		t.Fatalf("self-pay counted: %d %v", seedOf(out, "a"), out.Standing.Inputs)
	}
	// Bounty recycling: b posts a bounty, c (b's sock, another root) earns
	// it (a transfer b → c), then each pays the other's resource. Neither
	// payment is seed while the funding is within funded_days.
	reward := Record{Type: "transfer", From: acct("b"), To: acct("c"), Amount: 1000000, CreatedAt: testAsOf - 10*day}
	recycle := []Record{spend("c", acct("b"), "", 1), spend("b", acct("c"), "", 1)}
	out = build(recycle, []Record{reward})
	if seedOf(out, "b") != 0 || seedOf(out, "c") != 0 || out.Standing.Inputs["spend_self_dealt"] != 2 {
		t.Fatalf("recycled credit counted: b %d c %d", seedOf(out, "b"), seedOf(out, "c"))
	}
	// Twenty rounds count no more than none.
	var rounds []Record
	for i := 0; i < 20; i++ {
		rounds = append(rounds, recycle...)
	}
	if out = build(rounds, []Record{reward}); seedOf(out, "b") != 0 || seedOf(out, "c") != 0 {
		t.Fatal("rounds of recycling counted")
	}
	// Funding older than funded_days no longer links them.
	reward.CreatedAt = testAsOf - 31*day
	if out = build(recycle, []Record{reward}); seedOf(out, "b") != 99 || seedOf(out, "c") != 99 {
		t.Fatalf("stale funding still excludes: b %d c %d", seedOf(out, "b"), seedOf(out, "c"))
	}
	// Version 2 counted all of it, uncapped.
	b := newBuilder("s1")
	b.account("s1", 100, 10, 0).account("a", 50, 5, 0).domain("a", "a.example")
	b.snap.Params.Standing = StandingV2()
	b.snap.Spends = []Record{spend("a", "", "pay.a.example", 50)}
	if seed := standingOf(compute(t, b), "a").SeedCents; seed < 4900 {
		t.Fatalf("version 2 spend seed %d", seed)
	}
}

// Rule 1 prices a domain by its registration age when known: an old domain
// counts in full on the day it is linked; without a registration date the
// link's age bounds it at half, and the breakdown says which.
func TestStandingDomainRegistrationAge(t *testing.T) {
	b := newBuilder("s1")
	b.account("s1", 100, 10, 0).account("old", 1, 1, 0).account("unknown", 1, 1, 0)
	b.snap.Proofs = append(b.snap.Proofs,
		Record{Type: "proof", Account: acct("old"), Kind: "domain", LinkValue: "old.example", State: "verified", CreatedAt: testAsOf - 3600, CheckedAt: testAsOf - 3600,
			RegisteredAt: testAsOf - 3*365*day},
		Record{Type: "proof", Account: acct("unknown"), Kind: "domain", LinkValue: "unknown.example", State: "verified", CreatedAt: testAsOf - 3600, CheckedAt: testAsOf - 3600})
	out := compute(t, b)
	old, unknown := standingOf(out, "old"), standingOf(out, "unknown")
	var oldState, unknownState string
	for _, r := range old.Breakdown {
		if r.Source == "domain" {
			oldState = r.State
		}
	}
	for _, r := range unknown.Breakdown {
		if r.Source == "domain" {
			unknownState = r.State
		}
	}
	if old.SeedCents < 390 || !strings.Contains(oldState, "(registration age)") {
		t.Fatalf("old domain on day one: %+v", old)
	}
	if unknown.SeedCents > 2 || !strings.Contains(unknownState, "link age") {
		t.Fatalf("unknown registration: %+v", unknown)
	}
	// Design 0's proof collateral is unchanged: the link's age (one day).
	if c := scoreOf(out, "old").ProofCollateral; c > 2 {
		t.Fatalf("proof collateral %d", c)
	}
	// A registration date after the as-of time is not known yet.
	b.snap.Proofs[len(b.snap.Proofs)-2].RegisteredAt = testAsOf + day
	if s := standingOf(compute(t, b), "old"); s.SeedCents > 2 {
		t.Fatalf("future registration date counted: %+v", s)
	}
}
