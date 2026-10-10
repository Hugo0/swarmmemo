package board

import (
	"bytes"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/trust"
)

// Standing in shadow is computed and shown (trust.get, the record) and
// changes nothing; one parameter version switching it active raises the
// allowance share and admits an unseasoned vote backed by standing, never
// lowering anyone below today's rules.
func TestStandingShadowThenActive(t *testing.T) {
	newcomer := keyFor(64)
	s, _, alice, bob, bobPost := trustFixture(t, func(p *trust.Params) { p.Seeds = append(p.Seeds, keyID(keyFor(64))) })
	run(t, s, signed(newcomer, Command{Operation: "agent.register", Timestamp: s.now().Unix()}))
	sum, err := s.RunTrust(testContext)
	if err != nil || sum.State != "done" {
		t.Fatalf("run: %+v %v", sum, err)
	}
	a := trustGet(t, s, keyID(alice))
	v, ok := a["standing"].(*trust.StandingView)
	if !ok || v == nil || v.StandingCents <= 0 || v.Mode != trust.StandingShadow || v.Run != sum.ID || len(v.Breakdown) == 0 || v.Breakdown[0].Root != "endorsements" {
		t.Fatalf("alice's standing: %#v", a["standing"])
	}
	if _, err = s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	rec, err := s.ReadLogRecord(testContext, keyID(alice))
	if err != nil || rec.Record.Standing == nil || rec.Record.Standing.StandingCents != v.StandingCents || !bytes.Contains([]byte(rec.Note), []byte(`"standing_cents":`)) {
		t.Fatalf("record standing: %+v %v", rec.Record.Standing, err)
	}
	n, err := trust.CurrentStanding(testContext, s.db, keyID(newcomer))
	if err != nil || n == nil || n.StandingCents < 100 {
		t.Fatalf("the newcomer is on the seed list: %+v %v", n, err)
	}

	// Shadow: today's rules.
	postAs(t, s, newcomer, Command{Room: "lobby", Text: "new here", RequestID: "t-new"})
	if _, err = voteAs(s, newcomer, bobPost, "1", "v-shadow"); errCode(err) != "vote_not_eligible" {
		t.Fatalf("an unseasoned vote in shadow: %v", err)
	}
	share := func(subject allowance.Subject) int64 {
		st, err := s.classifier().Classify(testContext, s.db, subject, s.now().Unix())
		if err != nil {
			t.Fatal(err)
		}
		return st.WeightPPM
	}
	if w := share(allowance.Subject{ID: keyID(alice), KeyID: keyID(alice), Signed: true}); w != 1e6 {
		t.Fatalf("shadow share %d", w)
	}
	if w := share(allowance.Subject{ID: "anon:0123456789abcdef0123456789abcdef"}); w != 1e6 {
		t.Fatalf("shadow anonymous share %d", w)
	}

	// Active: one new parameter version.
	p, err := trustParams(testContext, s.db, s.now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	p.Standing.Mode = trust.StandingActive
	if _, err = s.db.Exec("INSERT INTO params VALUES('trust',3,?,'',0,0,'test','standing active')", string(p.Body())); err != nil {
		t.Fatal(err)
	}
	s.trust.forgetParams()
	if w := share(allowance.Subject{ID: keyID(alice), KeyID: keyID(alice), Signed: true}); w != trust.ShareWeightPPM(v.StandingCents, p) || w <= 1e6 {
		t.Fatalf("active share %d for %d cents", w, v.StandingCents)
	}
	if w := share(allowance.Subject{ID: keyID(bob), KeyID: keyID(bob), Signed: true}); w < 1e6 {
		t.Fatalf("active share below the floor: %d", w)
	}
	if w := share(allowance.Subject{ID: "anon:0123456789abcdef0123456789abcdef"}); w != 1e6+p.Standing.AnonSeedCents*p.WeightPerUnit {
		t.Fatalf("active anonymous share %d", w)
	}
	if _, err = voteAs(s, newcomer, bobPost, "1", "v-active"); err != nil {
		t.Fatalf("an unseasoned vote backed by standing: %v", err)
	}
	// Nobody is refused who votes today.
	if _, err = voteAs(s, alice, bobPost, "1", "v-alice"); err != nil {
		t.Fatalf("a seasoned vote: %v", err)
	}
	// An unseasoned key with no standing is still refused.
	stranger := keyFor(65)
	postAs(t, s, stranger, Command{Room: "lobby", Text: "hi", RequestID: "t-stranger"})
	if _, err = voteAs(s, stranger, bobPost, "1", "v-stranger"); errCode(err) != "vote_not_eligible" {
		t.Fatalf("an unseasoned vote with no standing: %v", err)
	}
}

// The standing inputs are read from the work and ledger tables: accepted
// public work becomes an edge, and credit held is never read; the queries
// run on a store with every table present.
func TestStandingInputsRead(t *testing.T) {
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	s := openTest(t, Config{Features: Features{Ledger: LedgerOn, Trust: TrustShadow}})
	key, worker := keyFor(70), keyFor(71)
	s.now = func() time.Time { return time.Unix(testTime-3*86400, 0) }
	postAs(t, s, key, Command{Room: "lobby", Text: "hello", RequestID: "t-hello"})
	postAs(t, s, worker, Command{Room: "lobby", Text: "for hire", RequestID: "t-hire"})
	// Accepted public work: an edge from the accepting key to the worker; a
	// simulation is none.
	item := createTestWork(t, s, key, "lobby", "request", 0)
	run(t, s, workCommand(s, key, Command{Operation: "work.accept", MessageID: item, Amount: claimAndSubmit(t, s, worker, item, "lobby")}))
	sim := createTestWork(t, s, key, "lobby", "simulation", 0)
	run(t, s, workCommand(s, key, Command{Operation: "work.accept", MessageID: sim, Amount: claimAndSubmit(t, s, worker, sim, "lobby")}))
	if err := s.GrantAllowance(testContext, keyID(key), allowance.Credit, allowance.Paid, 5_000_000, "a top-up"); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime+3600, 0) }
	var buf bytes.Buffer
	if err := s.WriteTrustInputs(testContext, &buf); err != nil {
		t.Fatal(err)
	}
	snap, err := trust.ReadJSONL(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte(`"type":"balance"`)) || len(snap.Spends) != 0 {
		t.Fatalf("credit held is published: %+v", snap.Spends)
	}
	if len(snap.Acts) != 1 || snap.Acts[0].Kind != "work_accept" || snap.Acts[0].ID != item || snap.Acts[0].From != keyID(key) || snap.Acts[0].To != keyID(worker) {
		t.Fatalf("edges: %+v", snap.Acts)
	}
	out, err := trust.Compute(testContext, snap)
	if err != nil || out.Standing == nil || out.Standing.Inputs["work_accept"] != 1 {
		t.Fatalf("standing: %+v %v", out.Standing, err)
	}
}
