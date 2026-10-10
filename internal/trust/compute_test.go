package trust

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

func TestDefaultParamsValidateAndRoundTrip(t *testing.T) {
	p := DefaultParams()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.Version != 5 || len(p.Seeds) != 6 || p.SeedsReason != SeedsAReason || p.Standing == nil || p.Standing.Mode != StandingShadow || p.Standing.Rule != 2 || len(p.Proofs) != 8 {
		t.Fatalf("version 5 must be version 1 plus standing (rule 2) in shadow plus the assessed rows: %+v", p)
	}
	// Version 4 is byte for byte the body of the C151 release.
	if sum := sha256.Sum256(paramsV4().Body()); hex.EncodeToString(sum[:]) != "d6560eba8a20582d2c4b85ca255c9c302aaee7153db31554108e67710675eb63" {
		t.Fatalf("version 4 body changed: %x %s", sum, paramsV4().Body())
	}
	if _, err := ParseParams(4, paramsV4().Body()); err != nil {
		t.Fatalf("version 4 no longer parses: %v", err)
	}
	// Version 3 is byte for byte the body of the C148 release.
	if sum := sha256.Sum256(paramsV3().Body()); hex.EncodeToString(sum[:]) != "8dcd245248606a6dc266c0e69ffc994b6c071b9ddcd6d73668b6f8d285dfdd17" {
		t.Fatalf("version 3 body changed: %s", paramsV3().Body())
	}
	if _, err := ParseParams(3, paramsV3().Body()); err != nil {
		t.Fatalf("version 3 no longer parses: %v", err)
	}
	// Version 2 is byte for byte the body published as version 2 (1.75.0).
	if sum := sha256.Sum256(paramsV2().Body()); hex.EncodeToString(sum[:]) != "8f16c806712fbbab2ba6cdae4b5dce3374ffddda48ba961100b0f2a5b1c61556" {
		t.Fatalf("version 2 body changed: %s", paramsV2().Body())
	}
	if _, err := ParseParams(2, paramsV2().Body()); err != nil {
		t.Fatalf("version 2 no longer parses: %v", err)
	}
	// Version 1 is version 2 without standing, byte for byte the body
	// published as version 1.
	if sum := sha256.Sum256(paramsV1().Body()); hex.EncodeToString(sum[:]) != "a7db44793ddaa0e98f4dcd934a159aff4ffc4ee5e3e738b2a1935e56c58ac581" {
		t.Fatalf("version 1 body changed: %s", paramsV1().Body())
	}
	if _, err := ParseParams(1, paramsV1().Body()); err != nil {
		t.Fatalf("version 1 no longer parses: %v", err)
	}
	back, err := ParseParams(5, p.Body())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back.Body(), p.Body()) {
		t.Fatal("body does not round-trip")
	}
	for name, mutate := range map[string]func(string) string{
		"unknown field": func(s string) string { return strings.Replace(s, `{`, `{"boolean":true,`, 1) },
		"bad factor": func(s string) string {
			return strings.Replace(s, `"day_factor_ppm":951695`, `"day_factor_ppm":900000`, 1)
		},
		"lambda":   func(s string) string { return strings.Replace(s, `"lambda_ppm":500000`, `"lambda_ppm":2000000`, 1) },
		"trailing": func(s string) string { return s + "{}" },
		"assess":   func(s string) string { return strings.Replace(s, `"assess":"saturating"`, `"assess":"capped"`, 1) },
		"unknown proof": func(s string) string {
			return strings.Replace(s, `"proofs":{`, `"proofs":{"x":{"curve":"none","day_factor_ppm":0,"forge":1,"half_life_days":0,"rent":1},`, 1)
		},
		"fixed row assessed": func(s string) string {
			return strings.Replace(s, `"domain":{`, `"domain":{"assess":"capped","units_per_cent":1,`, 1)
		},
		"vouch over its cap": func(s string) string {
			return strings.Replace(s, `"vouch_weight_max":50`, `"vouch_weight_max":5`, 1)
		},
		"edge kind missing": func(s string) string { return strings.Replace(s, `"witness":10,`, ``, 1) },
		"keep weight zero":  func(s string) string { return strings.Replace(s, `"keep_weight":100`, `"keep_weight":0`, 1) },
		"weights under rule 1": func(s string) string {
			return strings.Replace(s, `"rule":2`, `"rule":1`, 1)
		},
	} {
		if _, err := ParseParams(5, []byte(mutate(string(p.Body())))); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if DayFactor(14) != 951695 {
		t.Fatalf("DayFactor(14) = %d", DayFactor(14))
	}
}

func TestDecayIsIntegerAndMonotone(t *testing.T) {
	cv := curves{}
	f := DayFactor(14)
	prev := int64(1e6)
	for d := int64(1); d <= 200; d++ {
		v := cv.decay(f, d)
		if v > prev || v < 0 {
			t.Fatalf("day %d: %d after %d", d, v, prev)
		}
		prev = v
	}
	if v := cv.decay(f, 14); v < 499000 || v > 501000 {
		t.Fatalf("half-life: %d", v)
	}
	if cv.ramp(f, 0) != 0 {
		t.Fatal("ramp at 0")
	}
}

func TestDomainRoot(t *testing.T) {
	s := map[string]bool{"co.uk": true, "github.io": true}
	for in, want := range map[string]string{
		"example.com": "domain:example.com", "a.b.example.com": "domain:example.com",
		"shop.example.co.uk": "domain:example.co.uk", "me.github.io": "domain:me.github.io", "EXAMPLE.org.": "domain:example.org",
	} {
		if got := domainRoot(in, s); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// A small honest graph: seed s endorses a; a (with history) endorses b; c is
// endorsed only by d, whom nobody endorses.
func honest() *builder {
	b := newBuilder("s")
	b.account("s", 60, 20, 20).account("a", 60, 20, 10).account("b", 60, 10, 0).account("c", 60, 10, 0).account("d", 2, 2, 0)
	b.vouch("s", "a").vouch("a", "b").vouch("d", "c")
	return b
}

func TestFlowFollowsTrust(t *testing.T) {
	out := compute(t, honest())
	s, a, bb, c, d := scoreOf(out, "s"), scoreOf(out, "a"), scoreOf(out, "b"), scoreOf(out, "c"), scoreOf(out, "d")
	if a.Flow <= 0 || bb.Flow <= 0 {
		t.Fatalf("endorsed accounts get flow: a=%d b=%d", a.Flow, bb.Flow)
	}
	if bb.Flow > a.Parts.Transit {
		t.Fatalf("b receives %d, more than a's transit %d", bb.Flow, a.Parts.Transit)
	}
	// Invariant 8: a node with no inbound flow has flow 0.
	if c.Flow != 0 || d.Flow != 0 {
		t.Fatalf("no seed path: c=%d d=%d", c.Flow, d.Flow)
	}
	if s.Parts.Seed != "a" || s.Flow != s.FlowA {
		t.Fatalf("seed: %+v", s)
	}
	if a.FlowB != nil || out.SeedsBUsed {
		t.Fatal("seed set B with no anchors must be unused")
	}
	if a.Collateral != a.ProofCollateral+a.Flow*DefaultParams().UnitPrice {
		t.Fatalf("collateral parts: %+v", a)
	}
	if len(a.Parts.Endorsers) != 1 || a.Parts.Endorsers[0].Agent != acct("s") || a.Parts.Endorsers[0].Kinds[0] != "vouch" {
		t.Fatalf("endorsers: %+v", a.Parts.Endorsers)
	}
}

func TestNewcomerPassesNothing(t *testing.T) {
	b := honest()
	b.snap.Priors = nil // nobody has published history: transit is 0 everywhere
	b.snap.Meta.PriorRuns = 0
	out := compute(t, b)
	if scoreOf(out, "a").Flow == 0 {
		t.Fatal("a seed's direct endorsement carries flow")
	}
	if scoreOf(out, "b").Flow != 0 {
		t.Fatal("a newcomer passed flow on")
	}
}

func TestZeroWeightSources(t *testing.T) {
	b := honest()
	// The steward is a service account: its upvotes are no edge.
	b.account("steward", 60, 20, 20).vote("steward", "c").vouch("s", "steward")
	b.snap.Params.ServiceAccounts = []string{acct("steward")}
	// Unsigned and legacy votes count 0; down votes are shown, never an edge.
	b.endorse("unsigned", "s", "c", 1, 1).endorse("legacy_vote", "s", "c", 1, 1).endorse("vote", "s", "c", -1, 1)
	out := compute(t, b)
	c := scoreOf(out, "c")
	if c.Flow != 0 || c.Parts.EndorsersTotal != 1 || c.Parts.DownVotes != 1 {
		t.Fatalf("c: flow=%d endorsers=%d down=%d", c.Flow, c.Parts.EndorsersTotal, c.Parts.DownVotes)
	}
	for _, sc := range out.Scores {
		if sc.Account == acct("steward") {
			t.Fatal("service accounts are not scored")
		}
		for _, e := range sc.Parts.Endorsers {
			if e.Agent == acct("steward") {
				t.Fatal("steward edge")
			}
		}
	}
}

func TestFleetOnOneDomain(t *testing.T) {
	b := honest()
	fleet := []string{"f1", "f2", "f3", "f4", "f5"}
	for _, f := range fleet {
		b.account(f, 60, 20, 20).domain(f, f+".fleet.example")
	}
	for _, x := range fleet {
		for _, y := range fleet {
			if x != y {
				b.vouch(x, y)
			}
		}
		b.vouch("s", x)
	}
	out := compute(t, b)
	var total int64
	for _, f := range fleet {
		sc := scoreOf(out, f)
		if sc.Root != "domain:fleet.example" {
			t.Fatalf("root %s", sc.Root)
		}
		if sc.Parts.EndorsersTotal != 1 {
			t.Fatalf("same-root edges must weigh 0: %+v", sc.Parts.Endorsers)
		}
		total += sc.Flow
	}
	if cap := DefaultParams().RootCapShares * DefaultParams().UnitPerShare; total > cap {
		t.Fatalf("fleet flow %d above one root cap %d", total, cap)
	}
	// Proofs on one root saturate: a second link to the same domain adds nothing.
	b.snap.Proofs = append(b.snap.Proofs, Record{Type: "proof", Account: acct("f1"), Kind: "domain", LinkValue: "www.fleet.example", State: "verified", CreatedAt: testAsOf - 300*day, CheckedAt: testAsOf - day})
	out2 := compute(t, b)
	if scoreOf(out2, "f1").ProofCollateral != scoreOf(out, "f1").ProofCollateral {
		t.Fatal("saturation within a root")
	}
	saturated := false
	for _, part := range scoreOf(out2, "f1").Parts.Proofs {
		saturated = saturated || part.SaturatedBy != ""
	}
	if !saturated {
		t.Fatal("the weaker proof names what saturated it")
	}
}

func TestDomainPricing(t *testing.T) {
	b := honest().domain("b", "b.example")
	out := compute(t, b)
	sc := scoreOf(out, "b")
	part := sc.Parts.Proofs[0]
	if part.Kind != "domain" || part.WeightPPM != 500000 || part.Contribution != 200 || sc.ProofCollateral != 200 || sc.Tier != 2 {
		t.Fatalf("domain: %+v tier %d", part, sc.Tier)
	}
	// A lapsed or stale link counts 0, and is still shown.
	b.snap.Proofs[0].CheckedAt = testAsOf - 40*day
	sc = scoreOf(compute(t, b), "b")
	if sc.ProofCollateral != 0 || len(sc.Parts.Proofs) != 1 {
		t.Fatalf("stale domain: %+v", sc)
	}
}

func TestDeterministicAcrossOrder(t *testing.T) {
	b := honest().domain("b", "b.example")
	for i := 0; i < 8; i++ {
		b.vote("b", "c")
	}
	want := compute(t, b).Canonical()
	for i := 0; i < 5; i++ {
		r := rand.New(rand.NewSource(int64(i)))
		c := *b
		c.snap.Posts = append([]Record(nil), b.snap.Posts...)
		c.snap.Endorsements = append([]Record(nil), b.snap.Endorsements...)
		c.snap.Accounts = append([]Record(nil), b.snap.Accounts...)
		r.Shuffle(len(c.snap.Posts), func(i, j int) { c.snap.Posts[i], c.snap.Posts[j] = c.snap.Posts[j], c.snap.Posts[i] })
		r.Shuffle(len(c.snap.Endorsements), func(i, j int) {
			c.snap.Endorsements[i], c.snap.Endorsements[j] = c.snap.Endorsements[j], c.snap.Endorsements[i]
		})
		r.Shuffle(len(c.snap.Accounts), func(i, j int) { c.snap.Accounts[i], c.snap.Accounts[j] = c.snap.Accounts[j], c.snap.Accounts[i] })
		if got := compute(t, &c).Canonical(); !bytes.Equal(got, want) {
			t.Fatalf("order %d changed the output", i)
		}
	}
}

func TestAbortOnMaxWork(t *testing.T) {
	b := honest()
	b.snap.Params.MaxWork = 10
	out, err := Compute(context.Background(), b.snap)
	if !errors.Is(err, ErrWorkExceeded) || out.Work <= 10 {
		t.Fatalf("err=%v work=%d", err, out.Work)
	}
}

// Abuse case: a sybil region of 2,000 nodes behind 20 bought edges captures
// at most the published bound.
func TestSybilRegionWithinBound(t *testing.T) {
	b := newBuilder("s0", "s1", "s2")
	var honestNames []string
	for i := 0; i < 60; i++ {
		name := "h" + strconv.Itoa(i)
		honestNames = append(honestNames, name)
		b.account(name, 90, 10, 20)
	}
	for i := 0; i < 3; i++ {
		b.account("s"+strconv.Itoa(i), 90, 20, 20)
		for j := 0; j < 20; j++ {
			b.vouch("s"+strconv.Itoa(i), honestNames[i*20+j])
		}
	}
	for i, name := range honestNames {
		b.vote(name, honestNames[(i+7)%len(honestNames)])
	}
	var sybils []string
	for i := 0; i < 2000; i++ {
		name := "x" + strconv.Itoa(i)
		sybils = append(sybils, name)
		b.account(name, 1, 1, 0)
	}
	for i, x := range sybils {
		b.vouch(x, sybils[(i+1)%len(sybils)])
		b.vouch(x, sybils[(i+13)%len(sybils)])
	}
	for i := 0; i < 20; i++ { // 20 bought endorsements
		b.vouch(honestNames[i*3], sybils[i*97])
	}
	out := compute(t, b)
	var captured int64
	for _, x := range sybils {
		captured += scoreOf(out, x).Flow
	}
	bound := min(20*out.CaptureBound.EdgeCapUnits, 20*out.CaptureBound.MaxTransitUnits)
	if captured > bound {
		t.Fatalf("captured %d above bound %d", captured, bound)
	}
	t.Logf("sybil capture %d units of bound %d (pool %d)", captured, bound, out.PoolUnits)
}

// Abuse case: a trading ring of 30 accounts voting for each other has no
// flow without seed inflow, and ring evidence only with a funnel.
func TestTradingRing(t *testing.T) {
	b := honest()
	var ring []string
	for i := 0; i < 30; i++ {
		name := "r" + strconv.Itoa(i)
		ring = append(ring, name)
		b.account(name, 40, 10, 20)
	}
	for _, x := range ring {
		for _, y := range ring {
			if x != y {
				b.vote(x, y)
			}
		}
	}
	out := compute(t, b)
	for _, x := range ring {
		if scoreOf(out, x).Flow != 0 {
			t.Fatal("ring flow without seed inflow")
		}
	}
	if len(out.Evidence) != 0 {
		t.Fatalf("ring without a funnel is not evidence: %+v", out.Evidence)
	}
	// Now the ring funnels transfers to one collector inside it.
	for i := 1; i <= 6; i++ {
		b.snap.Transfers = append(b.snap.Transfers, Record{Type: "transfer", From: acct(ring[i]), To: acct(ring[0]), Amount: 1000, CreatedAt: testAsOf - day})
		b.snap.Claims = append(b.snap.Claims, Record{Type: "claim", Account: acct(ring[i]), Day: dayOf(testAsOf) - 1, Claimed: 4000, Spent: 10})
	}
	out = compute(t, b)
	kinds := map[string]int{}
	for _, ev := range out.Evidence {
		kinds[ev.Kind]++
	}
	if kinds["funnel"] != 1 || kinds["ring"] != 1 {
		t.Fatalf("evidence: %+v", out.Evidence)
	}
	for _, x := range ring {
		if scoreOf(out, x).WeightPPM != 0 {
			t.Fatal("ring members get weight 0")
		}
	}
}

// Abuse case: a claim-and-forward farm into one collector is a funnel, and
// endorsers of the farm carry liability.
func TestFunnelLiability(t *testing.T) {
	b := honest()
	b.account("collector", 30, 5, 0).vouch("a", "collector")
	for i := 0; i < 20; i++ {
		f := "farm" + strconv.Itoa(i)
		b.account(f, 3, 1, 0)
		b.snap.Transfers = append(b.snap.Transfers, Record{Type: "transfer", From: acct(f), To: acct("collector"), Amount: 4000, CreatedAt: testAsOf - 2*day})
		b.snap.Claims = append(b.snap.Claims, Record{Type: "claim", Account: acct(f), Day: dayOf(testAsOf) - 2, Claimed: 4 << 20, Spent: 100})
	}
	// An honest sender that spends its own claims is not part of the farm.
	b.snap.Transfers = append(b.snap.Transfers, Record{Type: "transfer", From: acct("b"), To: acct("collector"), Amount: 10, CreatedAt: testAsOf - 2*day})
	b.snap.Claims = append(b.snap.Claims, Record{Type: "claim", Account: acct("b"), Day: dayOf(testAsOf) - 2, Claimed: 1000, Spent: 900})
	out := compute(t, b)
	if len(out.Evidence) != 1 || out.Evidence[0].Kind != "funnel" || len(out.Evidence[0].Members) != 21 {
		t.Fatalf("evidence: %+v", out.Evidence)
	}
	pen := map[string]int64{}
	for _, p := range out.Penalties {
		pen[p.Account] = p.FractionPPM
	}
	if pen[acct("collector")] != 1e6 || pen[acct("farm0")] != 1e6 || pen[acct("b")] != 0 {
		t.Fatalf("members: %v", pen)
	}
	if pen[acct("a")] <= 0 || pen[acct("a")] >= 1e6 {
		t.Fatalf("endorser liability: %d", pen[acct("a")])
	}
	if scoreOf(out, "a").WeightPPM >= scoreOf(compute(t, honest()), "a").WeightPPM {
		t.Fatal("liability lowers the endorser's weight")
	}
}

// Abuse case: a sponsor inviting keys only its own clique endorses earns no
// dividend; independent endorsement earns one.
func TestSponsorDividend(t *testing.T) {
	b := honest()
	b.account("inv", 3, 3, 0)
	b.seq++
	b.snap.Endorsements = append(b.snap.Endorsements, Record{Type: "endorsement", Seq: b.seq, Kind: "vouch", Voter: acct("a"), Target: acct("inv"), Value: 1, Sponsor: true, CreatedAt: testAsOf - 2*day})
	b.vouch("a", "b") // b is in a's clique: a endorses b
	b.vote("b", "inv")
	out := compute(t, b)
	if len(out.Sponsorships) != 1 || len(out.Dividends) != 0 {
		t.Fatalf("clique-only invitee: %+v %+v", out.Sponsorships, out.Dividends)
	}
	// The seed, independent of the sponsor, endorses the invitee.
	b.vouch("s", "inv")
	out = compute(t, b)
	if len(out.Dividends) != 1 || out.Dividends[0].Sponsor != acct("a") || out.Dividends[0].Units <= 0 {
		t.Fatalf("independent inflow: %+v %+v", out.Sponsorships, out.Dividends)
	}
	// The same high pays nothing twice.
	b.snap.Sponsorships = []Record{{Type: "sponsorship", Invitee: acct("inv"), SponsorOf: acct("a"), HighWater: out.Sponsorships[0].HighWater}}
	if again := compute(t, b); len(again.Dividends) != 0 {
		t.Fatal("dividend paid twice for one high")
	}
}

func TestBreakerResetsOutboundEdges(t *testing.T) {
	b := honest()
	b.snap.Breakers = []Record{{Type: "breaker", Account: acct("a"), StartedAt: testAsOf - day, TrustUntil: testAsOf + 29*day}}
	out := compute(t, b)
	if scoreOf(out, "b").Flow != 0 || !scoreOf(out, "a").Parts.Reset {
		t.Fatal("a reset account passes nothing on")
	}
}

func TestSnapshotJSONLRoundTrip(t *testing.T) {
	b := honest().domain("b", "b.example")
	var buf bytes.Buffer
	if err := b.snap.WriteJSONL(&buf); err != nil {
		t.Fatal(err)
	}
	back, err := ReadJSONL(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(compute(t, b).Canonical(), compute(t, &builder{snap: back}).Canonical()) {
		t.Fatal("JSONL round trip changed the output")
	}
	if _, err := ReadJSONL(strings.NewReader(`{"type":"post","id":"x","boolean":true}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
}
