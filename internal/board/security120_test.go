package board

// Security review 1.20 regression tests. Each one inverts a proof of concept
// (TestSecPoC_*, branch security-review-1.20) or covers a fix the report
// found by reading: it fails while the weakness is present.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services/servicestest"
	"swarmmemo/internal/trust"
)

// M10: Design 0 roots a proven account at its registrable domain, as trust
// does, so subdomains of one domain (or one wildcard TXT) share one tier-2
// root and its cap; different registrable domains stay apart, and listed
// multi-label suffixes (co.uk) are honoured.
func TestSec120_SubdomainsShareOneProvenRoot(t *testing.T) {
	s := openTest(t, Config{Features: Features{AllowanceTiers: true, NameGate: true}})
	now := testTime
	root := func(n byte, domain string) string {
		t.Helper()
		key := keyFor(n)
		id := d0Identity(t, s, key)
		if _, err := s.db.Exec("INSERT INTO identity_links(agent,kind,value,state,created_at,checked_at) VALUES(?,?,?,?,?,?)", id, "domain", domain, "verified", now-86400, now-3600); err != nil {
			t.Fatal(err)
		}
		st, err := design0Classifier{s}.Classify(testContext, s.db, allowance.Subject{ID: id, KeyID: id, Signed: true}, now)
		if err != nil || st.Tier != allowance.TierProven {
			t.Fatalf("%s: %+v %v", domain, st, err)
		}
		return st.Root
	}
	roots := map[string]bool{}
	for i, sub := range []string{"a1.attacker.example", "a2.attacker.example", "deep.a3.attacker.example", "attacker.example"} {
		roots[root(byte(200+i), sub)] = true
	}
	if len(roots) != 1 || !roots["domain:attacker.example"] {
		t.Fatalf("subdomains of one domain: roots %v", roots)
	}
	if a, b := root(210, "x.shop.co.uk"), root(211, "y.other.co.uk"); a != "domain:shop.co.uk" || b != "domain:other.co.uk" {
		t.Fatalf("co.uk roots %s %s", a, b)
	}
}

// M11: the public trust snapshot no longer names an agent that is active
// only in private rooms (agent.get and trust.get treat it as not found),
// while a public agent's proofs are still there.
func TestSec120_TrustSnapshotOmitsPrivateOnlyAgent(t *testing.T) {
	s := openTest(t, Config{Features: Features{Trust: TrustShadow}})
	hidden, shown := keyFor(210), keyFor(211)
	run(t, s, signed(hidden, Command{Operation: "room.create", Room: "secret-club", Visibility: "private"}))
	postAs(t, s, hidden, Command{Room: "secret-club", Text: "members only", RequestID: "poc-private"})
	run(t, s, signed(hidden, Command{Operation: "identity.link", Data: linkJSON("url", "https://private-lab.example.org/agent")}))
	postAs(t, s, shown, Command{Text: "hello, public", RequestID: "public-post"})
	run(t, s, signed(shown, Command{Operation: "identity.link", Data: linkJSON("url", "https://public-lab.example.org/agent")}))
	if _, err := s.Execute(testContext, Command{Operation: "agent.get", Target: keyID(hidden)}, "other-origin"); err == nil {
		t.Fatal("agent.get found a private-only agent; precondition failed")
	}
	s.now = func() time.Time { return time.Unix(testTime+86400+3600, 0) }
	var buf bytes.Buffer
	if err := s.WriteTrustInputs(testContext, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "private-lab.example.org") {
		t.Fatalf("the snapshot carries the private-only agent's links:\n%s", out)
	}
	// Its ledger claims and spends may appear (with the ledger on): the
	// public ledger journal lists them already. Nothing else names it.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(line, keyID(hidden)) && !strings.Contains(line, `"type":"claim"`) {
			t.Fatalf("the snapshot names the private-only agent: %s", line)
		}
	}
	if !strings.Contains(out, "public-lab.example.org") {
		t.Fatalf("the public agent's proof is missing:\n%s", out)
	}
}

// M6: a service.call that would cost nothing (a repeated stamp) is charged
// the least a write costs, so an account with no credit left can no longer
// write call records for free, and each fresh key pays for its own.
func TestSec120_ZeroCostServiceCallsAreMetered(t *testing.T) {
	c := updatesConfig()
	c.Features = Features{Services: []string{"notary", "wakeup"}}
	s := openTest(t, c)
	s.UseServiceMeter(servicestest.NewMeter(1), &servicestest.Params{}) // 1 credit a day each
	t.Cleanup(s.stopServices)
	stamp := run(t, s, svcCall(keyFor(1), "notary", "stamp", map[string]any{"text": "public text"}, 1, "h1"))
	hash := svcField(t, stamp.Data, "result", "receipt", "hash").(string)
	attacker := keyFor(2)
	run(t, s, svcCall(attacker, "notary", "stamp", map[string]any{"text": "mine"}, 1, "a0"))
	// Broke: the free duplicate is refused like any other spend.
	fails(t, s, svcCall(attacker, "notary", "stamp", map[string]any{"hash": hash}, 1, "a1"), "quota_exhausted")
	// A fresh key pays for its duplicate.
	_, fresh, _ := ed25519.GenerateKey(rand.Reader)
	r := run(t, s, svcCall(fresh, "notary", "stamp", map[string]any{"hash": hash}, 1, "f1"))
	if svcField(t, r.Data, "result", "duplicate") != true || svcField(t, r.Data, "call", "cost") != float64(1) {
		t.Fatalf("a duplicate stamp is not free: %+v", r.Data)
	}
	fails(t, s, svcCall(fresh, "notary", "stamp", map[string]any{"hash": hash}, 1, "f2"), "quota_exhausted")
}

// H3 (dividend path): a sponsor whose mint is refused for its own reasons
// (here: its lot slots are full) no longer stops every later sponsor's
// dividend; it alone stays unpaid.
func TestSec120_RefusedSponsorDoesNotStopLaterDividends(t *testing.T) {
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	s := openTest(t, Config{Features: Features{Ledger: LedgerOn, Trust: TrustAllocation, TrustDividends: true}})
	ap := s.allowanceDefaults()
	ap.Resources["post_bytes"].GrantSharePPM = 100_000 // a tier-0 pool to pay from
	if _, err := s.SetAllowanceParams(testContext, "allowance", ap.Marshal(), "open the grant pool", 0); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime+86400, 0) } // pools are sized when a day opens
	now := s.now().Unix()
	p := trust.DefaultParams()
	p.Sponsor.Resource = "post_bytes"
	for i := 0; i < 64; i++ { // "aaa" sorts first; its 64 live lots never merge
		if _, err := s.db.Exec("INSERT INTO ledger_lots(account,resource,bucket,origin_tier,origin_account,hops,issued_day,expires_at,half_life_days,decayed_day,initial,remaining,held,state,created_at) VALUES('aaa',?,'paid',0,'aaa',0,?,0,0,?,1,1,0,'live',?)",
			p.Sponsor.Resource, now/86400-int64(i+1), now/86400, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, sponsor := range []string{"aaa", "bbb"} {
		if _, err := s.db.Exec("INSERT INTO trust_dividends(run_id,sponsor,invitee,units) VALUES(1,?,'invitee',5)", sponsor); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.payTrustDividends(testContext, 1, p, now); err != nil {
		t.Fatal(err)
	}
	paid := map[string]int64{}
	rows, err := s.db.Query("SELECT sponsor,paid FROM trust_dividends")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var sp string
		var n int64
		if err = rows.Scan(&sp, &n); err != nil {
			t.Fatal(err)
		}
		paid[sp] = n
	}
	if paid["aaa"] != 0 || paid["bbb"] != 5 {
		t.Fatalf("dividends paid %v: the refused first sponsor stopped the rest", paid)
	}
}
