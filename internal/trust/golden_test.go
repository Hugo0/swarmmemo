package trust

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"strconv"
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
	b.snap.Params = DefaultParams()
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

func TestGoldenFixture(t *testing.T) {
	inPath := filepath.Join("testdata", "golden_inputs.jsonl")
	outPath := filepath.Join("testdata", "golden_output.json")
	if *updateGolden {
		snap := goldenSnapshot()
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
