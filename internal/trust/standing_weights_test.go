package trust

import (
	"strconv"
	"testing"
)

// Tests of parameter version 4: absolute edge weights and the keep weight
// (rule 2, C151).

// weightsWorld is eight arbiter seeds, thirty accounts with a spend seed,
// a voter with a 400-cent domain and fifty fresh targets.
func weightsWorld(st *StandingParams) *builder {
	b := newBuilder("s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8")
	for i := 1; i <= 8; i++ {
		b.account("s"+strconv.Itoa(i), 100, 10, 0)
	}
	for i := 0; i < 30; i++ {
		n := "p" + strconv.Itoa(i)
		b.account(n, 60, 5, 0)
		b.snap.Spends = append(b.snap.Spends, Record{Type: "spend", Account: acct(n), Day: dayOf(testAsOf) - 1, Amount: 2000000})
	}
	b.account("voter", 100, 10, 0).domain("voter", "voter.example")
	b.snap.Proofs[len(b.snap.Proofs)-1].RegisteredAt = testAsOf - 3*365*day
	for i := 0; i < 120; i++ {
		b.account("t"+strconv.Itoa(i), 30, 3, 0)
	}
	b.snap.Params.Standing = st
	return b
}

// A vote counts more when the voter has more standing, and voting does not
// spend the voter's standing: between casting no vote and 1, 10 or 100
// votes the voter's standing moves by less than 1%. (What its votes carry is
// taken from every seed in proportion; the voter pays only its seed share.)
func TestStandingVoterKeepsStanding(t *testing.T) {
	cents := func(votes int) int64 {
		b := weightsWorld(DefaultStanding())
		for i := 0; i < votes; i++ {
			b.vote("voter", "t"+strconv.Itoa(i))
		}
		return standingOf(compute(t, b), "voter").Cents
	}
	before := cents(0)
	if before < 380 || before > 400 {
		t.Fatalf("voter with no votes: %d cents", before)
	}
	for _, votes := range []int{1, 10, 100} {
		after := cents(votes)
		t.Logf("%d votes: %d → %d cents", votes, before, after)
		if d := before - after; d < 0 || d*100 >= before {
			t.Fatalf("%d votes moved the voter %d → %d cents", votes, before, after)
		}
	}
}

// One vote passes at most 1/(K+1) of the voter's pass (K = 100: under 1%),
// a default vouch about ten times that, and a vouch's chosen weight is
// capped at vouch_weight_max.
func TestStandingOneVotePassesLittle(t *testing.T) {
	received := func(kind string, weight int64) (recipient, voter int64) {
		st := StandingV4()
		st.ArbiterSeedCents = 100000 // resolution: the voter passes 30,000 cents a step
		b := newBuilder("v", "s2")
		b.account("v", 100, 10, 0).account("s2", 100, 10, 0).account("r", 30, 3, 0)
		b.snap.Params.Standing = st
		b.endorse(kind, "v", "r", 1, 1)
		b.snap.Endorsements[len(b.snap.Endorsements)-1].Weight = weight
		out := compute(t, b)
		return standingOf(out, "r").Cents, standingOf(out, "v").Cents
	}
	vote, v := received("vote", 0)
	pass := v * StandingV4().PassPPM / 1e6
	t.Logf("one vote: %d cents of the voter's %d (pass %d)", vote, v, pass)
	if vote <= 0 || vote*101 > pass+101 || vote*110 < pass {
		t.Fatalf("one vote carried %d cents of a %d-cent pass", vote, pass)
	}
	vouch, _ := received("vouch", 0)
	if vouch < 8*vote || vouch > 11*vote {
		t.Fatalf("a default vouch carried %d cents, a vote %d", vouch, vote)
	}
	if one, _ := received("vouch", 1); one < vote-1 || one > vote+1 {
		t.Fatalf("a vouch of weight 1 carried %d cents, a vote %d", one, vote)
	}
	capped, _ := received("vouch", 50)
	over, _ := received("vouch", 1000)
	t.Logf("vouch: default %d, at the cap %d, over it %d", vouch, capped, over)
	if over != capped || capped*3 > pass+3 || capped*3 < pass*9/10 {
		t.Fatalf("vouch weight cap: at 50 %d, at 1000 %d, pass %d", capped, over, pass)
	}
}

// A ring of fresh keys that endorse each other gains less under version 4
// than under version 3 (where one edge carried the whole pass), whichever
// way it endorses: a vouch ring at the cap or a clique of votes.
func TestStandingRingGainsNothingOverV3(t *testing.T) {
	gain := func(st *StandingParams, vouch bool) (silent, ring int64) {
		total := func(endorse bool) int64 {
			b := weightsWorld(st)
			const n = 10
			for i := 0; i < n; i++ {
				k := "ring" + strconv.Itoa(i)
				b.account(k, 30, 3, 0).domain(k, k+".example")
				b.snap.Proofs[len(b.snap.Proofs)-1].RegisteredAt = testAsOf - 3*365*day
			}
			if endorse {
				for i := 0; i < n; i++ {
					k := "ring" + strconv.Itoa(i)
					if vouch {
						b.vouchWeight(k, "ring"+strconv.Itoa((i+1)%n), 50)
						continue
					}
					for j := 0; j < n; j++ {
						if j != i {
							for v := 0; v < 5; v++ {
								b.vote(k, "ring"+strconv.Itoa(j))
							}
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
		return total(false), total(true)
	}
	for _, vouch := range []bool{true, false} {
		s3, r3 := gain(StandingV3(), vouch)
		s4, r4 := gain(StandingV4(), vouch)
		t.Logf("vouch ring %v: version 3 %d → %d, version 4 %d → %d", vouch, s3, r3, s4, r4)
		if s3 <= 0 || s4 <= 0 || (r4-s4)*s3 >= (r3-s3)*s4 {
			t.Fatalf("ring gains more in version 4: v3 %d → %d, v4 %d → %d", s3, r3, s4, r4)
		}
	}
}
