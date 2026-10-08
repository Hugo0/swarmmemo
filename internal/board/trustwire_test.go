package board

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"swarmmemo/internal/trust"
)

// trustFixture opens a store with TRUST=shadow and three signed agents with
// public posts two days before the run: seed, alice and bob. The seed vouches
// for alice, alice votes for bob's post. The trust parameters name the seed.
func trustFixture(t *testing.T, mutate func(*trust.Params)) (s *Store, seed, alice, bob ed25519.PrivateKey, bobPost string) {
	t.Helper()
	s = openTest(t, Config{Features: Features{Trust: TrustShadow}})
	seed, alice, bob = keyFor(61), keyFor(62), keyFor(63)
	s.now = func() time.Time { return time.Unix(testTime-2*86400, 0) }
	postAs(t, s, seed, Command{Room: "lobby", Text: "seed here", RequestID: "t-seed"})
	alicePost := postAs(t, s, alice, Command{Room: "lobby", Text: "alice here", RequestID: "t-alice"})
	bobPost = postAs(t, s, bob, Command{Room: "lobby", Text: "bob here", RequestID: "t-bob"})
	postAs(t, s, bob, Command{Room: "lobby", Text: "thanks alice", ReplyTo: alicePost, RequestID: "t-bob-reply"})
	s.now = func() time.Time { return time.Unix(testTime+3600, 0) } // 01:00 UTC, run day
	sig := "sig"
	yes := false
	records := []EndorsementRecord{
		{Type: "vouch", Seq: 1, Voter: keyID(seed), Target: keyID(alice), Value: 1, Sponsor: &yes, CreatedAt: testTime - 86400, Signature: &sig},
		{Type: "vote", Seq: 2, Voter: keyID(alice), MessageID: bobPost, Value: 1, CreatedAt: testTime - 86400, Signature: &sig},
		{Type: "vote", Seq: 3, Voter: keyID(seed), MessageID: bobPost, Value: 1, CreatedAt: testTime - 86400}, // unsigned: weight 0
	}
	s.trust.endorsements = func(ctx context.Context, after int64, limit int) ([]EndorsementRecord, int64, error) {
		var out []EndorsementRecord
		for _, r := range records {
			if r.Seq > after && len(out) < limit {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			return nil, after, nil
		}
		return out, out[len(out)-1].Seq, nil
	}
	p := trust.DefaultParams()
	p.Seeds = []string{keyID(seed)}
	if mutate != nil {
		mutate(&p)
	}
	// The params table is the ledger's (§2.7); its schema is fixed in the RFC.
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS params (namespace TEXT NOT NULL, version INTEGER NOT NULL, body TEXT NOT NULL, sha256 TEXT NOT NULL,
 effective_at INTEGER NOT NULL, created_at INTEGER NOT NULL, actor TEXT NOT NULL, reason TEXT NOT NULL, PRIMARY KEY(namespace,version))`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO params VALUES('trust',2,?,'',0,0,'test','test seeds')", string(p.Body())); err != nil {
		t.Fatal(err)
	}
	return s, seed, alice, bob, bobPost
}

func trustGet(t *testing.T, s *Store, target string) map[string]any {
	t.Helper()
	res, err := s.Execute(testContext, Command{Operation: "trust.get", Target: target}, "test-origin")
	if err != nil {
		t.Fatal(err)
	}
	return res.Data
}

func TestTrustOffIsUnchanged(t *testing.T) {
	s := openTest(t, Config{})
	if _, err := s.Execute(testContext, Command{Operation: "trust.get", Target: keyID(keyFor(1))}, "test-origin"); errCode(err) != "service_unavailable" {
		t.Fatalf("trust.get with TRUST off: %v", err)
	}
	if _, err := s.RunTrust(testContext); errCode(err) != "service_unavailable" {
		t.Fatalf("run with TRUST off: %v", err)
	}
	if _, err := s.TrustDistribution(testContext); errCode(err) != "service_unavailable" {
		t.Fatalf("distribution with TRUST off: %v", err)
	}
	s.StartRFC0012(testContext)
	s.StopRFC0012()
	var runs int
	if err := s.db.QueryRow("SELECT count(*) FROM trust_runs").Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("runs with TRUST off: %d %v", runs, err)
	}
}

func TestTrustShadowRunAndAnswer(t *testing.T) {
	s, seed, alice, bob, _ := trustFixture(t, nil)
	held := false
	s.trust.computed = func() {
		// The computation holds no connection: another query gets one at once.
		ctx, cancel := context.WithTimeout(testContext, 2*time.Second)
		defer cancel()
		var one int
		held = s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one) != nil
	}
	before := s.trust.pages.Load()
	sum, err := s.RunTrust(testContext)
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("the run held the database connection while computing")
	}
	if s.trust.pages.Load()-before < 5 {
		t.Fatal("inputs are read in pages")
	}
	if sum.State != "done" || sum.AsOf != testTime || sum.ParamsVersion != 2 || len(sum.OutputSHA256) != 64 {
		t.Fatalf("summary: %+v", sum)
	}
	a := trustGet(t, s, keyID(alice))
	if a["mode"] != "shadow" || a["boolean"] != false || a["run"] != sum.ID || a["stale"] != false || a["params_version"] != int64(2) {
		t.Fatalf("answer frame: %+v", a)
	}
	flow := a["endorsements"].(map[string]any)["flow"].(map[string]any)
	if flow["effective"].(int64) <= 0 {
		t.Fatalf("alice is vouched by the seed: %+v", flow)
	}
	if tier := a["tier"].(map[string]any); tier["would_be"] == nil || tier["reason"] == "" {
		t.Fatalf("tier: %+v", tier)
	}
	// bob: alice's vote carries nothing on the first run (no published
	// history, so alice's transit is 0); the unsigned seed vote is no edge.
	b := trustGet(t, s, keyID(bob))
	if f := b["endorsements"].(map[string]any)["flow"].(map[string]any)["effective"].(int64); f != 0 {
		t.Fatalf("bob flow on the first run: %d", f)
	}
	if n := b["endorsements"].(map[string]any)["endorsers_total"].(int64); n != 1 {
		t.Fatalf("bob endorsers: %d", n)
	}
	// Distribution, runs and the run detail read the same run.
	dist, err := s.TrustDistribution(testContext)
	if err != nil || dist["run"] != sum.ID || dist["stale"] != false || dist["mode"] != "shadow" {
		t.Fatalf("distribution: %+v %v", dist, err)
	}
	var binned, wouldBe int64
	for _, n := range dist["collateral_log10_bins"].([]int64) {
		binned += n
	}
	for _, n := range dist["would_be_counts"].(map[string]int64) {
		wouldBe += n
	}
	if binned == 0 || binned != wouldBe {
		t.Fatalf("every scored account is binned once and has a would-be tier: %d %d", binned, wouldBe)
	}
	if _, ok := dist["tier_counts"].(map[string]int64); !ok {
		t.Fatalf("tier_counts: %+v", dist["tier_counts"])
	}
	runs, _, err := s.TrustRuns(testContext, 0, 10)
	if err != nil || len(runs) != 1 || runs[0]["output_sha256"] != sum.OutputSHA256 {
		t.Fatalf("runs: %+v %v", runs, err)
	}
	detail, err := s.TrustRun(testContext, sum.ID)
	if err != nil || detail["capture_bound"] == nil {
		t.Fatalf("detail: %+v %v", detail, err)
	}
	if _, err = s.TrustRun(testContext, 999); errCode(err) != "not_found" {
		t.Fatalf("missing run: %v", err)
	}
	// The seed is a node; an unknown agent is not found.
	if _, err = s.Execute(testContext, Command{Operation: "trust.get", Target: keyID(keyFor(99))}, "test-origin"); errCode(err) != "not_found" {
		t.Fatalf("unknown agent: %v", err)
	}
	_ = seed
}

// A run recomputed from the inputs a verifier would rebuild gives the same
// output hash (§4.8).
func TestTrustRunIsRecomputable(t *testing.T) {
	s, _, _, _, _ := trustFixture(t, nil)
	var inputs bytes.Buffer
	if err := s.WriteTrustInputs(testContext, &inputs); err != nil {
		t.Fatal(err)
	}
	sum, err := s.RunTrust(testContext)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := trust.ReadJSONL(&inputs)
	if err != nil {
		t.Fatal(err)
	}
	out, err := trust.Compute(testContext, snap)
	if err != nil {
		t.Fatal(err)
	}
	if got := sha256hex(out.Canonical()); got != sum.OutputSHA256 {
		t.Fatalf("recomputed %s, recorded %s", got, sum.OutputSHA256)
	}
}

func TestTrustAbortKeepsPreviousRun(t *testing.T) {
	s, _, alice, _, _ := trustFixture(t, nil)
	first, err := s.RunTrust(testContext)
	if err != nil {
		t.Fatal(err)
	}
	p := trust.DefaultParams()
	p.Seeds = []string{keyID(keyFor(61))}
	p.MaxWork = 1
	if _, err = s.db.Exec("INSERT INTO params VALUES('trust',3,?,'',0,0,'test','tiny work bound')", string(p.Body())); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime+86400+3600, 0) }
	if _, err = s.RunTrust(testContext); !errors.Is(err, trust.ErrWorkExceeded) {
		t.Fatalf("expected an abort: %v", err)
	}
	a := trustGet(t, s, keyID(alice))
	if a["run"] != first.ID || a["stale"] != true {
		t.Fatalf("after abort: run=%v stale=%v", a["run"], a["stale"])
	}
	var state string
	if err = s.db.QueryRow("SELECT state FROM trust_runs ORDER BY id DESC LIMIT 1").Scan(&state); err != nil || state != "aborted" {
		t.Fatalf("aborted run recorded as %q %v", state, err)
	}
}

func TestTrustNightlyRunsOncePerDay(t *testing.T) {
	s, _, _, _, _ := trustFixture(t, nil)
	s.now = func() time.Time { return time.Unix(testTime+600, 0) } // 00:10: not yet due
	s.trustNightly(testContext)
	s.now = func() time.Time { return time.Unix(testTime+2400, 0) } // 00:40
	s.trustNightly(testContext)
	s.trustNightly(testContext)
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM trust_runs WHERE as_of=?", testTime).Scan(&n); err != nil || n != 1 {
		t.Fatalf("runs today: %d %v", n, err)
	}
}

func TestTrustLiftOnlyLifts(t *testing.T) {
	s, _, _, _, _ := trustFixture(t, nil)
	if _, err := s.RunTrust(testContext); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO trust_evidence(id,run_id,kind,members,detail,detector_version,created_at) VALUES('funnel-x',1,'funnel','[]','{}',1,0)"); err != nil {
		t.Fatal(err)
	}
	if err := s.LiftTrustEvidence(testContext, "funnel-x", ""); err == nil {
		t.Fatal("a lift needs a public reason")
	}
	if err := s.LiftTrustEvidence(testContext, "funnel-x", "false positive: shared office network"); err != nil {
		t.Fatal(err)
	}
	if err := s.LiftTrustEvidence(testContext, "funnel-x", "again"); err == nil {
		t.Fatal("lifted twice")
	}
	ev, _, err := s.TrustEvidence(testContext, 0, 10)
	if err != nil || len(ev) != 1 || ev[0]["lifted"] != true {
		t.Fatalf("evidence: %+v %v", ev, err)
	}
}

// Fragment D only creates tables and indexes: an earlier database (schema
// 15) without them migrates in place to schema 16 and passes integrity.
func TestTrustSchemaFragmentMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema14.sqlite")
	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	postAs(t, s, keyFor(64), Command{Room: "lobby", Text: "before trust", RequestID: "pre"})
	for _, table := range []string{"trust_dividends", "trust_sponsorships", "trust_penalties", "trust_evidence", "trust_current", "trust_scores", "trust_runs"} {
		if _, err = s.db.Exec("DROP TABLE " + table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.Exec("PRAGMA user_version=15"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for i := 0; i < 2; i++ { // the second open finds everything in place
		if s, err = Open(path, Config{}); err != nil {
			t.Fatal(err)
		}
		if got := sqlCount(t, s, "PRAGMA user_version"); got != int64(SchemaVersion) || SchemaVersion != 19 {
			t.Fatalf("user_version %d", got)
		}
		if n := sqlCount(t, s, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name LIKE 'trust_%'"); n != 8 {
			t.Fatalf("%d trust tables", n)
		}
		if err = s.Integrity(testContext); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
