package trust

import (
	"strconv"
	"testing"
)

// Tests of parameter version 5: endorsements are stakes (rule 3, C152).

// stakesWorld is six arbiter seeds (500 cents each, plus any more named), an
// author with five posts and twelve fresh accounts.
func stakesWorld(more ...string) *builder {
	b := newBuilder(append([]string{"s1", "s2", "s3", "s4", "s5", "s6"}, more...)...)
	for i := 1; i <= 6; i++ {
		b.account("s"+strconv.Itoa(i), 100, 10, 0)
	}
	for _, m := range more {
		b.account(m, 100, 10, 0)
	}
	b.account("author", 100, 5, 0)
	for i := 0; i < 12; i++ {
		b.account("f"+strconv.Itoa(i), 30, 3, 0)
	}
	return b
}

func (b *builder) voteOn(from, to, msg string, minutesAgo int64) *builder {
	b.seq++
	b.snap.Endorsements = append(b.snap.Endorsements, Record{Type: "endorsement", Seq: b.seq, Kind: "vote", Voter: acct(from), Target: acct(to),
		MessageID: msg, Value: 1, CreatedAt: testAsOf - minutesAgo*60})
	return b
}

func (b *builder) witness(from, to, claim string, verified bool, minutesAgo int64) *builder {
	v := int64(1)
	if !verified {
		v = -1
	}
	b.snap.Acts = append(b.snap.Acts, Record{Type: "edge", Kind: "witness", ID: claim, From: acct(from), To: acct(to), Value: v, CreatedAt: testAsOf - minutesAgo*60})
	return b
}

func judgement(p StandingPart) int64 {
	for _, r := range p.Breakdown {
		if r.Root == "judgement" {
			return r.Contribution
		}
	}
	return 0
}

func totalStanding(out Output) int64 { return out.Standing.StandingCents }

// A vouch moves the voucher's own standing: 2.5% at the default weight,
// 12.5% at 50, of what the voucher holds after it (12 and 55 cents of 500).
// Nothing comes from a pool, so the total stays the seed mass (less floors),
// and a vote moves nothing.
func TestStakesVouchIsATransfer(t *testing.T) {
	b := stakesWorld()
	before := compute(t, b)
	s1 := standingOf(before, "s1").Cents
	b.vouch("s1", "f0").vouchWeight("s2", "f1", 50).vote("s3", "f2")
	out := compute(t, b)
	f0, f1, f2 := standingOf(out, "f0").Cents, standingOf(out, "f1").Cents, standingOf(out, "f2").Cents
	t.Logf("s1 %d → %d, f0 %d, f1 %d, f2 %d", s1, standingOf(out, "s1").Cents, f0, f1, f2)
	if f0 < 12 || f0 > 13 || f1 < 54 || f1 > 56 || f2 != 0 {
		t.Fatalf("vouch 10 moved %d, vouch 50 moved %d, a vote %d (of 500)", f0, f1, f2)
	}
	if d := s1 - standingOf(out, "s1").Cents - f0; d < 0 || d > 1 || standingOf(out, "s3").Cents != standingOf(before, "s3").Cents {
		t.Fatalf("the voucher did not pay: s1 %d → %d", s1, standingOf(out, "s1").Cents)
	}
	if d := out.Standing.SeedCents - totalStanding(out); d < 0 || d > out.Standing.Accounts || totalStanding(before) != before.Standing.SeedCents {
		t.Fatalf("total %d, seed %d", totalStanding(out), out.Standing.SeedCents)
	}
}

// A ring of seeded keys that vouches for itself and votes for its own posts
// early and late ends with what it started with (within floors): transfers
// inside it and positions on its own objects are zero-sum.
func TestStakesRingGainsNothing(t *testing.T) {
	ring := func(endorse bool) int64 {
		b := stakesWorld()
		const n = 8
		for i := 0; i < n; i++ {
			k := "ring" + strconv.Itoa(i)
			b.account(k, 30, 3, 0).domain(k, k+".example")
			b.snap.Proofs[len(b.snap.Proofs)-1].RegisteredAt = testAsOf - 3*365*day
		}
		if endorse {
			for i := 0; i < n; i++ {
				k := "ring" + strconv.Itoa(i)
				b.vouchWeight(k, "ring"+strconv.Itoa((i+1)%n), 50)
				for j := 0; j < n; j++ {
					if j != i {
						b.voteOn(k, "ring"+strconv.Itoa(j), "post-"+strconv.Itoa(j), int64(1000-100*i))
					}
				}
			}
		}
		out := compute(t, b)
		var sum int64
		for i := 0; i < n; i++ {
			sum += standingOf(out, "ring"+strconv.Itoa(i)).Cents
		}
		return sum
	}
	silent, endorsing := ring(false), ring(true)
	t.Logf("ring: silent %d, endorsing itself %d", silent, endorsing)
	if silent <= 0 || endorsing > silent || endorsing < silent-8 {
		t.Fatalf("ring %d → %d", silent, endorsing)
	}
}

// Good judgement confirmed independently earns standing: the first voter on
// a post that draws far more than its author's usual reception earns, paid
// by the voters on the author's posts nobody joined. A late voter never
// pays. Confirmation from accounts linked to the voter counts for nothing.
func TestStakesEarlyJudgementConfirmedIndependently(t *testing.T) {
	build := func(linked bool) Output {
		b := stakesWorld("judge", "late", "dud")
		b.snap.Params.Standing.ArbiterSeedCents = 100000 // one vote stakes 250 cents
		// Linked: the judge funded each early confirmer within funded_days.
		if linked {
			for i := 1; i <= 5; i++ {
				b.snap.Transfers = append(b.snap.Transfers, Record{Type: "transfer", From: acct("judge"), To: acct("s" + strconv.Itoa(i)), Amount: 1000, CreatedAt: testAsOf - 2*day})
			}
		}
		// The author's usual reception: four posts, one vote each.
		for i := 0; i < 4; i++ {
			b.voteOn("s"+strconv.Itoa(i+1), "author", "usual-"+strconv.Itoa(i), int64(5000+i))
		}
		b.voteOn("judge", "author", "hit", 3000) // first on a post that takes off
		for i := 0; i < 5; i++ {
			b.voteOn("s"+strconv.Itoa(i+1), "author", "hit", int64(2000-10*i))
		}
		b.voteOn("late", "author", "hit", 100)
		b.voteOn("dud", "author", "dud", 3000) // first on a post nobody joins
		return compute(t, b)
	}
	indep := build(false)
	j, late, dud := standingOf(indep, "judge"), standingOf(indep, "late"), standingOf(indep, "dud")
	t.Logf("judge %d (judgement %d), late %d, dud %d, seed %d", j.Cents, judgement(j), late.Cents, dud.Cents, j.SeedCents)
	if judgement(j) <= 0 || j.Cents <= j.SeedCents {
		t.Fatalf("independently confirmed early judgement earned nothing: %+v", j)
	}
	if late.Cents < late.SeedCents-1 || dud.Cents >= dud.SeedCents-1 {
		t.Fatalf("late voter %d, lone voter %d (seed %d)", late.Cents, dud.Cents, dud.SeedCents)
	}
	if totalStanding(indep) > indep.Standing.SeedCents {
		t.Fatalf("minted: %d > %d", totalStanding(indep), indep.Standing.SeedCents)
	}
	// Linked to the five early confirmers, only the late voter confirms it.
	if jo := standingOf(build(true), "judge"); (jo.Cents-jo.SeedCents)*10 > j.Cents-j.SeedCents {
		t.Fatalf("linked confirmation earned: %+v", jo)
	}
}

// A claim contradicted by independent witnesses costs the claimant; the
// contradicting witnesses earn it. A claim nobody independent checked
// settles nothing.
func TestStakesContradictedClaimCosts(t *testing.T) {
	b := stakesWorld()
	b.witness("s1", "f0", "domain:f0.example", true, 3000)
	b.witness("s2", "f0", "domain:f0.example", false, 2000)
	b.witness("s3", "f0", "domain:f0.example", false, 1000)
	b.witness("s4", "f1", "domain:f1.example", true, 1000) // unchecked
	b.witness("s5", "f2", "url:f2", true, 3000)
	b.witness("s6", "f2", "url:f2", true, 1000) // reproduced
	out := compute(t, b)
	s := func(n string) StandingPart { return standingOf(out, n) }
	t.Logf("claimant %d, contradictors %d %d, unchecked %d, reproduced %d %d", s("s1").Cents, s("s2").Cents, s("s3").Cents, s("s4").Cents, s("s5").Cents, s("s6").Cents)
	if s("s1").Cents >= s("s4").Cents || judgement(s("s2"))+judgement(s("s3")) <= 0 {
		t.Fatalf("contradicted claimant %d, unchecked %d", s("s1").Cents, s("s4").Cents)
	}
	if out.Standing.Inputs["witness_failed"] != 2 || totalStanding(out) > out.Standing.SeedCents {
		t.Fatalf("summary %+v", out.Standing)
	}
}

// Vouchers of a penalised account lose more than the vouch itself: their
// position scores −1 and half their stake times the penalty returns to the
// seeds. A penalised account's standing is scaled by the penalty.
func TestStakesPenalisedVouchee(t *testing.T) {
	b := stakesWorld()
	b.vouch("s1", "f0").vouch("s2", "f1")
	b.snap.Penalties = append(b.snap.Penalties, Record{Type: "penalty", Account: acct("f0"), Evidence: "ring-x", FractionPPM: 1000000, EndsAt: testAsOf + day})
	out := compute(t, b)
	if f0 := standingOf(out, "f0"); f0.Cents != 0 || f0.PenaltyPPM != 1000000 {
		t.Fatalf("penalised: %+v", f0)
	}
	if a, c := standingOf(out, "s1").Cents, standingOf(out, "s2").Cents; a >= c {
		t.Fatalf("the penalised account's voucher %d, the other %d", a, c)
	}
}
