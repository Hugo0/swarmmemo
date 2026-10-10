package trust

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite internal/trust/testdata golden files")

// goldenSnapshot is the recomputability fixture (§4.8): the compiled-in
// parameters (seed set A) plus one service account, and inputs that exercise
// every part of a run: both seed sets, domain and history proofs, replies,
// votes (signed, legacy, unsigned, down), vouches, a breaker, a funnel with
// liability, an earlier penalty and a sponsorship that earns a dividend.
func goldenSnapshot() Snapshot {
	b := newBuilder()
	b.snap.Params = paramsV1()
	b.snap.Params.ServiceAccounts = []string{acct("steward")}
	b.snap.Meta = Meta{Schema: 1, AsOf: testAsOf, PriorRuns: 30, EventsSeq: 5000, EndorsementsSeq: 400, LedgerSeq: 900}
	seeds := DefaultParams().Seeds
	first := map[string]string{} // each account's newest post
	add := func(a string, daysAgo, active int64, prior int64) {
		b.snap.Accounts = append(b.snap.Accounts, Record{Type: "account", Account: a, FirstSeen: testAsOf - daysAgo*day})
		for d := int64(1); d <= active; d++ {
			b.post++
			id := "post" + strconv.Itoa(b.post)
			if d == 1 {
				first[a] = id
			}
			b.snap.Posts = append(b.snap.Posts, Record{Type: "post", ID: id, Account: a, CreatedAt: testAsOf - d*day + 7200})
		}
		if prior > 0 {
			b.snap.Priors = append(b.snap.Priors, Record{Type: "prior", Account: a, FlowSum: prior * 30, Standing: true})
		}
	}
	end := func(kind, from, to, msg string, value, daysAgo int64, sponsor bool) {
		b.seq++
		b.snap.Endorsements = append(b.snap.Endorsements, Record{Type: "endorsement", Seq: b.seq, Kind: kind, Voter: from, Target: to, MessageID: msg,
			Value: value, Sponsor: sponsor, CreatedAt: testAsOf - daysAgo*day - 300})
	}
	reply := func(from, to, parent string, daysAgo int64) {
		b.post++
		b.snap.Posts = append(b.snap.Posts, Record{Type: "post", ID: "post" + strconv.Itoa(b.post), Account: from, CreatedAt: testAsOf - daysAgo*day + 600,
			ReplyTo: parent, ReplyToAccount: to})
	}
	domain := func(a, name string, checkedAgo int64) {
		b.snap.Proofs = append(b.snap.Proofs, Record{Type: "proof", Account: a, Kind: "domain", LinkValue: name, State: "verified",
			CreatedAt: testAsOf - 120*day, CheckedAt: testAsOf - checkedAgo*day})
	}
	for i, s := range seeds {
		add(s, 100, 12, 20)
		if i > 0 {
			end("vouch", seeds[i-1], s, "", 1, int64(i), false)
		}
	}
	// Three anchors (seed set B): a verified domain each, a history of posts
	// drawing replies from seeds, old and active.
	anchors := []string{acct("anchor0"), acct("anchor1"), acct("anchor2")}
	for i, a := range anchors {
		add(a, 60, 8, 15)
		domain(a, "anchor"+strconv.Itoa(i)+".example", 2)
		end("vouch", seeds[i], a, "", 1, 3, false)
		end("vouch", a, anchors[(i+1)%3], "", 1, 2, false)
	}
	// Replies from seeds to anchors' posts qualify history days.
	for _, r := range b.snap.Posts {
		for i, a := range anchors {
			if r.Account == a && r.ReplyTo == "" && r.CreatedAt > testAsOf-6*day {
				reply(seeds[i], a, r.ID, (testAsOf-r.CreatedAt)/day)
			}
		}
	}
	regular := []string{acct("alice"), acct("bob"), acct("carol"), acct("dave")}
	for i, a := range regular {
		add(a, 40, int64(3+i), int64(4*i))
	}
	end("vote", seeds[0], regular[0], first[regular[0]], 1, 0, false)
	end("vote", anchors[0], regular[0], first[regular[0]], 1, 0, false)
	end("vouch", regular[0], regular[1], "", 1, 6, false)
	end("vote", regular[1], regular[2], first[regular[2]], 1, 0, false)
	end("vote", regular[1], regular[2], first[regular[2]], -1, 0, false) // changed to a down vote
	end("legacy_vote", seeds[1], regular[3], first[regular[3]], 1, 0, false)
	end("unsigned", seeds[2], regular[3], first[regular[3]], 1, 0, false)
	reply(regular[2], regular[0], first[regular[0]], 0)
	reply(regular[2], regular[0], first[regular[0]], 0) // one edge per (U, X, day)
	domain(regular[1], "shop.bob.co.uk", 3)
	domain(regular[1], "bob.co.uk", 3) // same root: saturates
	domain(regular[3], "dave.example", 45)
	b.snap.Proofs = append(b.snap.Proofs,
		Record{Type: "proof", Account: regular[2], Kind: "ed25519", LinkValue: "k", State: "proof_attached", CreatedAt: testAsOf - 30*day, LinkAccount: regular[3]},
		Record{Type: "proof", Account: regular[0], Kind: "url", LinkValue: "https://alice.example/", State: "claimed", CreatedAt: testAsOf - 30*day})
	// The steward is a service account: its vouch is no edge.
	add(acct("steward"), 200, 20, 0)
	end("vouch", acct("steward"), regular[3], "", 1, 1, false)
	// A breaker resets carol's outbound edges.
	b.snap.Breakers = append(b.snap.Breakers, Record{Type: "breaker", Account: regular[2], StartedAt: testAsOf - 2*day, TrustUntil: testAsOf + 28*day})
	// A funnel: six fresh keys forward their claims to one collector that
	// bob endorsed.
	collector := acct("collector")
	add(collector, 10, 3, 0)
	end("vouch", regular[1], collector, "", 1, 4, false)
	for i := 0; i < 6; i++ {
		f := acct("farm" + strconv.Itoa(i))
		add(f, 5, 1, 0)
		b.snap.Transfers = append(b.snap.Transfers, Record{Type: "transfer", From: f, To: collector, Amount: 4000, CreatedAt: testAsOf - 3*day})
		b.snap.Claims = append(b.snap.Claims, Record{Type: "claim", Account: f, Day: dayOf(testAsOf) - 3, Claimed: 4194304, Spent: 64})
	}
	// An earlier penalty still running.
	b.snap.Penalties = append(b.snap.Penalties, Record{Type: "penalty", Account: regular[3], Evidence: "funnel-0123456789abcdef01234567", FractionPPM: 250000, EndsAt: testAsOf + 5*day})
	// A sponsorship: seed 4 sponsors a newcomer that others endorse too.
	newcomer := acct("newcomer")
	add(newcomer, 3, 3, 0)
	end("vouch", seeds[4], newcomer, "", 1, 2, true)
	end("vouch", anchors[1], newcomer, "", 1, 1, false)
	b.snap.Sponsorships = append(b.snap.Sponsorships, Record{Type: "sponsorship", Invitee: newcomer, SponsorOf: seeds[4], HighWater: 0})
	return b.snap
}

// goldenStandingV2Snapshot is the standing fixture of parameter version 2
// (phase 1A, published in 1.75.0): the golden inputs plus every standing
// input: work.accept and witness edges, a down vote (already in the golden
// inputs), credit spent, a spend-only account, a service account's spend
// (ignored), and a penalised account. Its output is frozen: version 2 runs
// recompute byte for byte.
func goldenStandingV2Snapshot() Snapshot {
	snap := goldenSnapshot()
	p := paramsV2()
	p.ServiceAccounts = snap.Params.ServiceAccounts
	snap.Params = p
	seeds := p.Seeds
	alice, bob, carol, dave := acct("alice"), acct("bob"), acct("carol"), acct("dave")
	D := dayOf(testAsOf)
	snap.Acts = append(snap.Acts,
		Record{Type: "edge", Kind: "work_accept", ID: "w1", From: acct("anchor0"), To: alice, CreatedAt: testAsOf - 3*day},
		Record{Type: "edge", Kind: "work_accept", ID: "w2", From: bob, To: acct("newcomer"), CreatedAt: testAsOf - 2*day},
		Record{Type: "edge", Kind: "work_accept", ID: "w3", From: alice, To: alice, CreatedAt: testAsOf - 2*day}, // self: no edge
		Record{Type: "edge", Kind: "witness", From: seeds[2], To: dave, CreatedAt: testAsOf - day},
		Record{Type: "edge", Kind: "witness", From: seeds[3], To: carol, CreatedAt: testAsOf + day}) // after the as-of time
	snap.Spends = append(snap.Spends,
		Record{Type: "spend", Account: bob, Day: D - 10, Amount: 2000000},
		Record{Type: "spend", Account: carol, Day: D - 1, Amount: 50000},
		Record{Type: "spend", Account: acct("payer"), Day: D - 30, Amount: 5000000},
		Record{Type: "spend", Account: acct("steward"), Day: D - 1, Amount: 9000000},
		Record{Type: "spend", Account: alice, Day: D, Amount: 7000000}) // today: not yet
	return snap
}

// goldenStandingSnapshot is the standing fixture of the current version
// (3, the simulation's fixes): version 2's inputs under DefaultParams, plus
// what rule 1 reads: spend over the cap, self-dealt spend (to an account in
// the spender's root, to a host on the spender's verified domain, to an
// account the spender funded by a transfer within funded_days, and recycled
// through a bounty reward), spend to an unrelated payee (counted), and
// domains with and without a known registration date.
func goldenStandingSnapshot() Snapshot {
	snap := goldenStandingV2Snapshot()
	p := DefaultParams()
	p.ServiceAccounts = snap.Params.ServiceAccounts
	snap.Params = p
	alice, bob, carol, dave := acct("alice"), acct("bob"), acct("carol"), acct("dave")
	payer, collector := acct("payer"), acct("collector")
	D := dayOf(testAsOf)
	for i, r := range snap.Proofs {
		switch {
		case r.Kind == "domain" && r.LinkValue == "anchor0.example":
			snap.Proofs[i].RegisteredAt = testAsOf - 3*365*day // old: full price from day one
		case r.Kind == "domain" && r.LinkValue == "bob.co.uk":
			snap.Proofs[i].RegisteredAt = testAsOf - 20*day
		}
	}
	snap.Transfers = append(snap.Transfers,
		Record{Type: "transfer", From: payer, To: carol, Amount: 500000, CreatedAt: testAsOf - 20*day}, // payer funded carol
		Record{Type: "transfer", From: dave, To: alice, Amount: 300000, CreatedAt: testAsOf - 40*day})  // too old to count
	snap.Spends = append(snap.Spends,
		Record{Type: "spend", Account: payer, Day: D - 2, Amount: 3000000, To: carol},                     // funded payee: self-dealt
		Record{Type: "spend", Account: carol, Day: D - 2, Amount: 3000000, To: payer},                     // and back: self-dealt
		Record{Type: "spend", Account: bob, Day: D - 3, Amount: 1000000, LinkValue: "api.shop.bob.co.uk"}, // own domain: self-dealt
		Record{Type: "spend", Account: carol, Day: D - 4, Amount: 1000000, To: dave},                      // carol's root holds dave's key: self-dealt
		Record{Type: "spend", Account: dave, Day: D - 5, Amount: 400000, To: alice},                       // funded 40 days ago: counts
		Record{Type: "spend", Account: collector, Day: D - 1, Amount: 900000, LinkValue: "api.unrelated.example"},
		Record{Type: "spend", Account: acct("steward"), Day: D - 1, Amount: 100000, To: alice}) // service: ignored
	return snap
}

func TestGoldenFixture(t *testing.T) {
	testGolden(t, "golden", goldenSnapshot)
	testGolden(t, "golden_standing_v2", goldenStandingV2Snapshot)
	testGolden(t, "golden_standing", goldenStandingSnapshot)
}

func testGolden(t *testing.T, name string, build func() Snapshot) {
	inPath := filepath.Join("testdata", name+"_inputs.jsonl")
	outPath := filepath.Join("testdata", name+"_output.json")
	if *updateGolden {
		snap := build()
		var buf bytes.Buffer
		if err := snap.WriteJSONL(&buf); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(inPath, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := Compute(context.Background(), snap)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(outPath, out.Canonical(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(inPath)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := ReadJSONL(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var again bytes.Buffer
	if err = snap.WriteJSONL(&again); err != nil || !bytes.Equal(again.Bytes(), raw) {
		t.Fatalf("the fixture inputs are not in canonical JSONL form (%v); rerun with -update", err)
	}
	out, err := Compute(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Canonical(); !bytes.Equal(got, want) {
		t.Fatalf("golden output changed; if intended, rerun with -update and bump the parameter or detector version\n got %.400s", got)
	}
	// The fixture must exercise what it claims to.
	if !out.SeedsBUsed || len(out.Evidence) == 0 || len(out.Dividends) == 0 || len(out.Penalties) == 0 {
		t.Fatalf("fixture coverage: seeds_b=%v evidence=%d dividends=%d penalties=%d", out.SeedsBUsed, len(out.Evidence), len(out.Dividends), len(out.Penalties))
	}
	if strings.HasPrefix(name, "golden_standing") {
		if out.Standing == nil || out.Standing.Inputs["work_accept"] == 0 || out.Standing.Inputs["witness"] == 0 || out.Standing.Inputs["down_vote"] == 0 ||
			out.Standing.Inputs["spend"] == 0 || len(out.Standing.LargestMoves) == 0 {
			t.Fatalf("standing fixture coverage: %+v", out.Standing)
		}
		testRecomputePy(t, raw, out.Canonical())
	} else if out.Standing != nil {
		t.Fatal("a version 1 run has a standing summary")
	}
	history := false
	for _, sc := range out.Scores {
		for _, p := range sc.Parts.Proofs {
			history = history || p.Kind == "history"
		}
	}
	if !history {
		t.Fatal("fixture has no history proof")
	}
}

// paramsV1 is trust parameter version 1: version 2 without standing. Its
// body is the one published as version 1 (sha256 a7db4479…).
func paramsV1() Params {
	p := DefaultParams()
	p.Version, p.Standing = SeedsAVersion, nil
	return p
}

// paramsV2 is trust parameter version 2: phase 1A's standing, as published.
func paramsV2() Params {
	p := DefaultParams()
	p.Version, p.Standing = StandingVersion, StandingV2()
	return p
}

// testRecomputePy runs the independent verifier (scripts/trust/recompute.py
// run) on the inputs and compares the sha256 of its output with Go's.
func testRecomputePy(t *testing.T, inputs, want []byte) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	file := filepath.Join(t.TempDir(), "inputs.jsonl")
	if err = os.WriteFile(file, inputs, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := exec.Command(python, "-B", filepath.Join("..", "..", "scripts", "trust", "recompute.py"), "run", file).Output()
	if err != nil {
		t.Fatalf("recompute.py run: %v", err)
	}
	g, w := sha256.Sum256(got), sha256.Sum256(want)
	if g != w {
		t.Fatalf("recompute.py output sha256 %s, Go %s", hex.EncodeToString(g[:]), hex.EncodeToString(w[:]))
	}
}
