package trust

import (
	"bytes"
	"context"
	"math"
	"math/rand"
	"strconv"
	"testing"
)

func TestVoteWeight(t *testing.T) {
	st := DefaultStanding()
	for _, c := range []struct{ cents, want int64 }{
		{-5, 0}, {0, 0}, {1, 0}, {49, 0}, // below the floor: no weight
		{50, 250000 + 750000*316227/1000000}, // √(1/10) = 0.316227
		{125, 625000},                        // √(1/4) = 0.5
		{500, 1000000}, {501, 1000000}, {1e9, 1000000},
	} {
		if got := st.VoteWeight(c.cents); got != c.want {
			t.Errorf("v(%d) = %d, want %d", c.cents, got, c.want)
		}
	}
	// Version 2 had no floor: any C > 0 weighed at least v0.
	if v2 := StandingV2(); v2.VoteWeight(1) != 250000+750000*44721/1000000 {
		t.Fatalf("version 2 v(1) = %d", v2.VoteWeight(1))
	}
	// Against the float definition, and monotone.
	prev := int64(0)
	for cents := int64(1); cents <= 600; cents++ {
		v := st.VoteWeight(cents)
		f := 0.25 + 0.75*math.Sqrt(math.Min(1, float64(cents)/500))
		if cents < 50 {
			f = 0
		}
		if math.Abs(float64(v)/1e6-f) > 2e-6 || v < prev || v > 1e6 {
			t.Fatalf("v(%d) = %d, float %.6f", cents, v, f)
		}
		prev = v
	}
	// The floor: shadow is today's rule; active never takes from a seasoned
	// account and admits an unseasoned one only at v ≥ ½.
	if st.FlooredVoteWeightPPM(true, 0) != 1e6 || st.FlooredVoteWeightPPM(false, 1e6) != 0 || st.VoteCounts(false, 1e6) {
		t.Fatal("shadow changes the vote weight")
	}
	st.Mode = StandingActive
	if st.FlooredVoteWeightPPM(true, 0) != 1e6 || st.FlooredVoteWeightPPM(false, 125) != 625000 || !st.VoteCounts(false, 125) || st.VoteCounts(false, 50) || !st.VoteCounts(true, 0) {
		t.Fatal("active floor")
	}
}

func TestShareWeightFloor(t *testing.T) {
	p := DefaultParams()
	if ShareWeightPPM(0, p) != 1e6 || ShareWeightPPM(250, p) != 1250000 || ShareWeightPPM(1e9, p) != 1e6+p.WeightCapPPM {
		t.Fatal("share weight")
	}
	if FlooredShareWeightPPM(1e6, 250, p) != 1e6 {
		t.Fatal("shadow changes the share")
	}
	p.Standing.Mode = StandingActive
	if FlooredShareWeightPPM(1e6, 250, p) != 1250000 || FlooredShareWeightPPM(2e6, 250, p) != 2e6 || FlooredShareWeightPPM(750000, 0, p) != 1e6 {
		t.Fatal("active share floor")
	}
}

func TestIntegerHelpers(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		n := r.Int63n(1 << 62)
		if i%3 == 0 {
			n = r.Int63n(1 << 20)
		}
		s := isqrt(n)
		if s*s > n || (s+1)*(s+1) <= n {
			t.Fatalf("isqrt(%d) = %d", n, s)
		}
	}
	if mulDiv(1<<62, 1<<40, 3) != 1<<63-1 || mulDiv(1e15, 1e6, 1e6) != 1e15 || mulDiv(7, 3, 2) != 10 || mulDiv(0, 5, 1) != 0 {
		t.Fatal("mulDiv")
	}
	if FakeCostText(250) != "about $2.50 to fake" || FakeCostText(5) != "about $0.05 to fake" || FakeCostText(123456789) != "about $1,234,567.89 to fake" {
		t.Fatal(FakeCostText(250))
	}
	if StandingScore(249) != 2.4 || StandingScore(0) != 0 {
		t.Fatal(StandingScore(249))
	}
}

func standingOf(out Output, name string) StandingPart {
	sc := scoreOf(out, name)
	if sc.Parts.Standing == nil {
		return StandingPart{}
	}
	return *sc.Parts.Standing
}

// Splitting standing over many keys must not multiply vote weight: the v0
// floor applies only from v_floor_cents (the simulation: with v0 for any
// C > 0, 1-cent keys bought 140 times the weight per dollar of one key).
func TestVoteWeightSplitDoesNotPay(t *testing.T) {
	st := DefaultStanding()
	one := st.VoteWeight(500)
	var split int64
	for i := 0; i < 500; i++ {
		split += st.VoteWeight(1)
	}
	if split >= one {
		t.Fatalf("500 keys of 1 cent weigh %d, one key of 500 cents %d", split, one)
	}
	// Version 2 paid 140 times over for the same split.
	if v2 := StandingV2(); 500*v2.VoteWeight(1) < 100*v2.VoteWeight(500) {
		t.Fatalf("version 2 split %d", 500*v2.VoteWeight(1))
	}
}

// Casting votes costs the voter nothing (rule 1): a node with no out-edges
// returns its pass to the seeds instead of keeping it, and standing is
// reported as c / (1 − pass). In version 2 one up vote halved the voter's
// standing. The voter's standing may move by a second-order amount: before
// it votes, its share of the seeds' return includes its own pass (at most
// its seed share × pass × (1 − pass) of it; here 9% × 0.21, 562 → 548
// cents, against 200 → 100 in version 2).
func TestStandingVotingDoesNotCostTheVoter(t *testing.T) {
	build := func(st *StandingParams, votes int) Output {
		b := newBuilder("s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8")
		for i := 1; i <= 8; i++ {
			b.account("s"+strconv.Itoa(i), 100, 10, 0)
		}
		b.account("voter", 100, 10, 0).domain("voter", "voter.example")
		b.snap.Proofs[len(b.snap.Proofs)-1].RegisteredAt = testAsOf - 3*365*day
		for i := 0; i < 50; i++ {
			b.account("t"+strconv.Itoa(i), 30, 3, 0)
		}
		for i := 0; i < votes; i++ {
			b.vote("voter", "t"+strconv.Itoa(i))
		}
		b.snap.Params.Standing = st
		return compute(t, b)
	}
	for _, votes := range []int{1, 50} {
		before := standingOf(build(DefaultStanding(), 0), "voter").Cents
		after := standingOf(build(DefaultStanding(), votes), "voter").Cents
		t.Logf("%d votes: current version %d → %d", votes, before, after)
		if before < 390 || after < before*97/100 {
			t.Fatalf("%d votes: voter %d → %d cents", votes, before, after)
		}
		old0 := standingOf(build(StandingV2(), 0), "voter").Cents
		old1 := standingOf(build(StandingV2(), votes), "voter").Cents
		t.Logf("%d votes: version 2 %d → %d", votes, old0, old1)
		if old1 > old0*6/10 {
			t.Fatalf("version 2 should halve the voter: %d → %d", old0, old1)
		}
	}
}

// Standing is a conserved flow from the seeds: its mass never sums above the
// seed mass (reported standing at most seed / (1 − pass) under rule 1, the
// seed itself, less integer floors, under rule 2), a region with no
// seed and no inbound edge gets nothing however densely it endorses itself,
// and an endorsement from a seeded account carries standing.
func TestStandingConservedAndSeeded(t *testing.T) {
	b := newBuilder("s1", "s2")
	b.account("s1", 100, 10, 0).account("s2", 100, 10, 0).account("x", 50, 5, 0).account("y", 50, 5, 0)
	b.vouchWeight("s1", "x", 50).vouchWeight("x", "y", 50)
	for i := 0; i < 20; i++ { // a sybil ring, no seed, no edge in
		n := "sybil" + strconv.Itoa(i)
		b.account(n, 3, 2, 0)
		b.vouch(n, "sybil"+strconv.Itoa((i+1)%20))
		b.vouch(n, "sybil"+strconv.Itoa((i+7)%20))
	}
	out := compute(t, b)
	st := out.Standing
	if st == nil || st.SeedCents != 1000 || st.StandingCents > st.SeedCents || st.StandingCents < st.SeedCents-st.Accounts {
		t.Fatalf("not conserved: %+v", st)
	}
	for i := 0; i < 20; i++ {
		if c := standingOf(out, "sybil"+strconv.Itoa(i)).Cents; c != 0 {
			t.Fatalf("sybil %d has %d", i, c)
		}
	}
	x, y := standingOf(out, "x"), standingOf(out, "y")
	if x.Cents <= 0 || y.Cents <= 0 || x.ReceivedCents != x.Cents || x.Breakdown[0].Root != "endorsements" {
		t.Fatalf("endorsements carry nothing: x %+v y %+v", x, y)
	}
	// One attack edge into the ring: what the ring gets is bounded by what
	// that edge carries from x, never more.
	b.vouchWeight("x", "sybil0", 50)
	out2 := compute(t, b)
	var ring int64
	for i := 0; i < 20; i++ {
		ring += standingOf(out2, "sybil"+strconv.Itoa(i)).Cents
	}
	if ring == 0 || ring > standingOf(out, "y").Cents+standingOf(out, "x").Cents {
		t.Fatalf("ring %d through one edge from x (%d)", ring, standingOf(out, "x").Cents)
	}
}

func TestStandingOpposePenaltyAndSeeds(t *testing.T) {
	b := newBuilder("s1", "d")
	b.account("s1", 100, 10, 0).account("t", 50, 5, 0).account("d", 50, 5, 0).account("payer", 0, 0, 0)
	b.vouch("s1", "t").vouch("s1", "d")
	D := dayOf(testAsOf)
	b.snap.Spends = append(b.snap.Spends, Record{Type: "spend", Account: acct("payer"), Day: D - 1, Amount: 1000000}, // $1
		Record{Type: "spend", Account: acct("payer"), Day: D, Amount: 1000000}) // today: not yet
	before := compute(t, b)
	pay := standingOf(before, "payer")
	// Spent at cost, decayed one day; a silent account keeps its seed less
	// its seed share of what s1's vouches pass (rule 2: what recipients
	// gain is taken from every seed in proportion).
	if pay.SeedCents != 99 || pay.Cents > pay.SeedCents || pay.Cents < pay.SeedCents*9/10 || scoreOf(before, "payer").Tier != 3 {
		t.Fatalf("payer: %+v", pay)
	}
	// A down vote from d takes from t locally and is spent by d.
	b.endorse("vote", "d", "t", -1, 1)
	after := compute(t, b)
	if standingOf(after, "t").OpposedCents == 0 || standingOf(after, "t").Cents >= standingOf(before, "t").Cents {
		t.Fatalf("oppose: before %+v after %+v", standingOf(before, "t"), standingOf(after, "t"))
	}
	// A penalty scales standing.
	b.snap.Penalties = append(b.snap.Penalties, Record{Type: "penalty", Account: acct("t"), Evidence: "ring-x", FractionPPM: 500000, EndsAt: testAsOf + day})
	pen := compute(t, b)
	if p := standingOf(pen, "t"); p.PenaltyPPM != 500000 || p.Cents > standingOf(after, "t").Cents/2+1 {
		t.Fatalf("penalty: %+v", p)
	}
	// Version 1 parameters: no standing anywhere.
	b.snap.Params.Standing = nil
	v1 := compute(t, b)
	if v1.Standing != nil || scoreOf(v1, "s1").Parts.Standing != nil || bytes.Contains(v1.Canonical(), []byte(`"standing"`)) {
		t.Fatal("standing in a version 1 run")
	}
}

func TestStandingDeterministicAcrossOrder(t *testing.T) {
	snap := goldenStandingSnapshot()
	one, err := Compute(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewSource(7))
	for _, list := range [][]Record{snap.Acts, snap.Spends, snap.Endorsements, snap.Posts, snap.Proofs} {
		r.Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] })
	}
	two, err := Compute(context.Background(), snap)
	if err != nil || !bytes.Equal(one.Canonical(), two.Canonical()) {
		t.Fatal("standing depends on record order")
	}
}

func TestStandingParamsValidate(t *testing.T) {
	for name, mutate := range map[string]func(*StandingParams){
		"mode":      func(s *StandingParams) { s.Mode = "on" },
		"factor":    func(s *StandingParams) { s.DayFactorPPM++; s.DayFactorPPM++ },
		"pass":      func(s *StandingParams) { s.PassPPM = 1e6 },
		"iterate":   func(s *StandingParams) { s.Iterations = 0 },
		"bands":     func(s *StandingParams) { s.Theta2Cents = s.Theta1Cents + 1 },
		"v0":        func(s *StandingParams) { s.V0PPM = 2e6 },
		"cref":      func(s *StandingParams) { s.CRefCents = 0 },
		"per cent":  func(s *StandingParams) { s.CreditsPerCent = 0 },
		"anon seed": func(s *StandingParams) { s.AnonSeedCents = -1 },
	} {
		p := DefaultParams()
		mutate(p.Standing)
		if p.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	p := DefaultParams()
	p.Standing.Mode = StandingActive
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}
