package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/ledger"
)

// These tests pin the ledger modes themselves, so they skip when the whole
// suite runs under SWARMMEMO_TEST_ALLOWANCE_LEDGER.
func ledgerTest(t *testing.T, mode LedgerMode) *Store {
	t.Helper()
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	return openTest(t, Config{Features: Features{Ledger: mode}})
}

func TestLedgerOffIsTodaysBehaviour(t *testing.T) {
	s := ledgerTest(t, LedgerOff)
	key, other := keyFor(60), keyFor(61)
	register(t, s, key)
	register(t, s, other)
	run(t, s, signed(key, Command{Operation: "post", Text: "hello"}))
	before := run(t, s, signed(key, Command{Operation: "quota.get"})).Data
	if _, ok := before["tier"]; ok || before["daily_bytes"] != int64(4<<20) {
		t.Fatalf("quota.get changed with the ledger off: %v", before)
	}
	res := run(t, s, signed(key, Command{Operation: "credit.transfer", Target: keyID(other), Amount: 100}))
	if _, ok := res.Data["state"]; ok || len(res.Data) != 4 {
		t.Fatalf("credit.transfer changed with the ledger off: %v", res.Data)
	}
	fails(t, s, signed(key, Command{Operation: "allowance.get"}), "service_unavailable")
	fails(t, s, Command{Operation: "ledger.list"}, "service_unavailable")
	fails(t, s, signed(key, Command{Operation: "allowance.transfer", Target: keyID(other), Amount: 1}), "service_unavailable")
	for _, table := range []string{"allowance_days", "allowance_claims", "ledger_lots", "ledger_entries", "account_breakers"} {
		if n := sqlCount(t, s, "SELECT count(*) FROM "+table); n != 0 {
			t.Fatalf("%s has %d rows with the ledger off", table, n)
		}
	}
	if res.Allowance != nil {
		t.Fatal("an allowance note with the ledger off")
	}
	if s.AllowanceCapabilities(testContext) != nil {
		t.Fatal("an allowance capability with the ledger off")
	}
}

func TestLedgerSchemaKeepsTheVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.sqlite")
	for i := 0; i < 2; i++ {
		s, err := Open(path, Config{})
		if err != nil {
			t.Fatal(err)
		}
		var version int
		if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
			t.Fatalf("user_version %d %v", version, err)
		}
		for _, table := range []string{"params", "allowance_days", "allowance_pools", "allowance_claims", "allowance_client_spend", "allowance_usage", "ledger_lots", "ledger_entries", "ledger_holds", "ledger_hold_parts", "ledger_transfers", "account_breakers"} {
			if sqlCount(t, s, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table) != 1 {
				t.Fatalf("missing %s", table)
			}
		}
		if err = s.Integrity(testContext); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}

func TestLedgerBoundsMatchPublishedLimits(t *testing.T) {
	if ledger.HoldsPerAccount != HoldsPerAccount || ledger.HoldsTotal != HoldsTotal || ledger.TransfersPendingMax != TransfersPendingMax || ledger.JournalPageMax != LedgerPageMax {
		t.Fatal("the ledger enforces bounds other than the published limits")
	}
}

func TestLedgerOnEndToEnd(t *testing.T) {
	s := ledgerTest(t, LedgerOn)
	alice, bob := keyFor(62), keyFor(63)
	before := run(t, s, signed(alice, Command{Operation: "quota.get"})).Data
	if before["daily_bytes"] != int64(4<<20) || before["tier"] != 3 || before["params_version"] != int64(0) {
		t.Fatalf("prospective quota.get %v", before)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM allowance_claims"); n != 0 {
		t.Fatal("a read claimed")
	}
	register(t, s, alice)
	register(t, s, bob)
	post := run(t, s, signed(alice, Command{Operation: "post", Text: "first of the day"}))
	if post.Allowance == nil || post.Allowance.Tier != 3 || post.Allowance.Entitlement != 4<<20 || post.Allowance.Line == "" {
		t.Fatalf("allowance note %+v", post.Allowance)
	}
	if strings.Contains(post.Allowance.Line, ServicesCatalogueURL) || post.Allowance.Services != "" {
		t.Fatalf("the line names the catalogue while no service runs: %+v", post.Allowance)
	}
	q := run(t, s, signed(alice, Command{Operation: "quota.get"})).Data
	used := q["used_bytes"].(int64)
	if used <= 0 || q["remaining_bytes"].(int64) != 4<<20-used || len(q["resources"].([]map[string]any)) != 3 {
		t.Fatalf("quota.get %v", q)
	}
	// The legacy rows follow the ledger, so turning it off keeps usage.
	if sqlCount(t, s, "SELECT used FROM quota WHERE actor=?", keyID(alice)) != used {
		t.Fatal("legacy quota rows not written")
	}
	a := run(t, s, Command{Operation: "allowance.get", Target: keyID(alice)}).Data
	if a["tier"] != 3 || a["tier_name"] != "signed" || a["reason"] == "" || a["ledger"] != "on" {
		t.Fatalf("allowance.get %v", a)
	}
	// credit.transfer keeps its fields; allowance.transfer names the resource.
	tr := run(t, s, signed(alice, Command{Operation: "credit.transfer", Target: keyID(bob), Amount: 1000})).Data
	if tr["transferred_bytes"] != int64(1000) || tr["transaction_fee_bytes"] != int64(256) || tr["state"] != "done" || tr["expires_at"] != (testTime/86400+1)*86400 {
		t.Fatalf("credit.transfer %v", tr)
	}
	at := run(t, s, signed(alice, Command{Operation: "allowance.transfer", Target: keyID(bob), Amount: 10, Data: `{"schema":1,"resource":"post_bytes"}`})).Data
	if at["transfer"].(map[string]any)["state"] != "done" {
		t.Fatalf("allowance.transfer %v", at)
	}
	fails(t, s, signed(alice, Command{Operation: "allowance.transfer", Target: keyID(bob), Amount: 10, Data: `{"schema":1,"resource":"gold"}`}), "invalid_resource")
	fails(t, s, signed(alice, Command{Operation: "allowance.transfer", Target: keyID(alice), Amount: 10}), "self_transfer")
	fails(t, s, signed(alice, Command{Operation: "credit.transfer", Target: keyID(bob), Amount: 60 << 20}), "quota_exhausted")
	if b := run(t, s, signed(bob, Command{Operation: "quota.get"})).Data; b["incoming_bytes"] != int64(1010) {
		t.Fatalf("recipient %v", b)
	}
	page := run(t, s, Command{Operation: "ledger.list", Target: keyID(alice), Limit: 2}).Data
	if len(page["entries"].([]map[string]any)) != 2 || page["next_cursor"] == nil {
		t.Fatalf("ledger.list %v", page)
	}
	next := run(t, s, Command{Operation: "ledger.list", Target: keyID(alice), Limit: 2, Cursor: page["next_cursor"].(string)}).Data
	if next["entries"].([]map[string]any)[0]["seq"].(int64) >= page["entries"].([]map[string]any)[1]["seq"].(int64) {
		t.Fatal("cursor did not page down")
	}
	// Anonymous posts spend a tier-4 share and never appear as journal lines.
	run(t, s, Command{Operation: "post", Text: "anonymous"})
	for _, e := range run(t, s, Command{Operation: "ledger.list"}).Data["entries"].([]map[string]any) {
		if strings.HasPrefix(e["account"].(string), "anon:") {
			t.Fatal("an anonymous journal line is public")
		}
	}
	stats, err := s.AllowanceStats(testContext, 7)
	if err != nil || len(stats["resources"].([]map[string]any)) != 1 || stats["transfers"].(map[string]any)["count"] != int64(2) {
		t.Fatalf("stats %v %v", stats, err)
	}
	if caps := s.AllowanceCapabilities(testContext); caps["explanation"] != WaterfallSentence || caps["ledger"] != "on" {
		t.Fatalf("capabilities %v", caps)
	}
	if n, err := s.SweepAllowance(testContext); err != nil || n != 0 {
		t.Fatalf("sweep %d %v", n, err)
	}
	// The next day the share is new and yesterday's free units are gone.
	s.now = func() time.Time { return time.Unix(testTime+86400, 0) }
	if q := run(t, s, signed(bob, Command{Operation: "quota.get", Timestamp: testTime + 86400})).Data; q["remaining_bytes"] != q["daily_bytes"] || q["incoming_bytes"] != int64(0) {
		t.Fatalf("next day %v", q)
	}
}

func TestLedgerBreakerHoldsTransferAndRotatedKeyCancels(t *testing.T) {
	s := ledgerTest(t, LedgerOn)
	old, next, bob := keyFor(64), keyFor(65), keyFor(66)
	register(t, s, old)
	register(t, s, bob)
	rotate := signed(old, Command{Operation: "agent.rotate", Target: base64.RawURLEncoding.EncodeToString(next.Public().(ed25519.PublicKey))})
	rotate.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(next, Canonical("swarmmemo.com", rotate)))
	run(t, s, rotate)
	if n := sqlCount(t, s, "SELECT count(*) FROM account_breakers WHERE reason='agent.rotate' AND cancel_key=?", keyID(old)); n != 1 {
		t.Fatalf("%d breakers after rotation", n)
	}
	// A thief holding the new key moves the allowance: it waits 48 h.
	tr := run(t, s, signed(next, Command{Operation: "credit.transfer", Target: keyID(bob), Amount: 5000})).Data
	if tr["state"] != "pending" || tr["execute_at"] != testTime+48*3600 {
		t.Fatalf("transfer under the breaker %v", tr)
	}
	id := tr["transfer_id"].(string)
	// The rotated-away key may sign exactly this one command.
	fails(t, s, signed(old, Command{Operation: "post", Text: "still me"}), "key_rotated")
	cancelled := run(t, s, signed(old, Command{Operation: "allowance.transfer.cancel", Target: id})).Data
	if cancelled["transfer"].(map[string]any)["state"] != "cancelled" {
		t.Fatalf("cancel %v", cancelled)
	}
	fails(t, s, signed(next, Command{Operation: "allowance.transfer.cancel", Target: id}), "transfer_not_pending")
	fails(t, s, signed(bob, Command{Operation: "allowance.transfer.cancel", Target: id}), "transfer_not_found")
	// Outside a pending transfer of its own account, the old key stays rotated.
	fails(t, s, signed(old, Command{Operation: "allowance.transfer.cancel", Target: "0000"}), "key_rotated")
	if q := run(t, s, signed(bob, Command{Operation: "quota.get"})).Data; q["incoming_bytes"] != int64(0) {
		t.Fatalf("the recipient still counts a cancelled transfer: %v", q)
	}
	// Proof-bearing link kinds trip the breaker; plain claims do not.
	for _, change := range []accountChange{{Account: "x", Reason: "identity.link", Kind: "url"}, {Account: "x", Reason: "identity.link", Kind: "domain"}} {
		tx, _ := s.db.Begin()
		if err := s.onAccountChange(testContext, tx, change, testTime); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM account_breakers WHERE account='x'"); n != 1 {
		t.Fatalf("%d breakers for link changes", n)
	}
}

func TestLedgerShadowComparesWithoutDeciding(t *testing.T) {
	s := ledgerTest(t, LedgerShadow)
	alice, bob := keyFor(67), keyFor(68)
	register(t, s, alice)
	register(t, s, bob)
	run(t, s, signed(alice, Command{Operation: "post", Text: "shadowed"}))
	res := run(t, s, signed(alice, Command{Operation: "credit.transfer", Target: keyID(bob), Amount: 100}))
	if _, ok := res.Data["state"]; ok || res.Allowance != nil {
		t.Fatal("shadow mode changed the answers")
	}
	fails(t, s, signed(alice, Command{Operation: "credit.transfer", Target: keyID(bob), Amount: 60 << 20}), "quota_exhausted")
	agree, explained, unexplained := s.LedgerShadowCounts()
	if agree < 4 || explained != 0 || unexplained != 0 {
		t.Fatalf("shadow counts %d %d %d", agree, explained, unexplained)
	}
	// The ledger's day follows the legacy one: one fee, spent once.
	if n := sqlCount(t, s, "SELECT count(*) FROM ledger_entries WHERE kind='fee'"); n != 1 {
		t.Fatalf("%d fee lines", n)
	}
	if sqlCount(t, s, "SELECT coalesce(sum(spent),0) FROM allowance_usage WHERE subject=?", keyID(alice)) != sqlCount(t, s, "SELECT used FROM quota WHERE actor=?", keyID(alice))-100 {
		t.Fatal("the ledger's spend differs from the legacy rows")
	}
	fails(t, s, signed(alice, Command{Operation: "allowance.transfer", Target: keyID(bob), Amount: 1}), "service_unavailable")
	if a := run(t, s, Command{Operation: "allowance.get", Target: keyID(alice)}).Data; a["ledger"] != "shadow" {
		t.Fatalf("allowance.get in shadow %v", a)
	}
}

func TestAllowanceParamsAndGrant(t *testing.T) {
	s := ledgerTest(t, LedgerOn)
	key := keyFor(69)
	register(t, s, key)
	cur, err := s.AllowanceParams(testContext, "allowance", -1)
	if err != nil || cur.Version != 0 || !cur.CompiledIn {
		t.Fatalf("%+v %v", cur, err)
	}
	p := s.allowanceDefaults()
	p.Resources["post_bytes"].GrantSharePPM = 100_000
	if _, err = s.SetAllowanceParams(testContext, "allowance", []byte(`{"schema":1}`), "partial", 0); err == nil {
		t.Fatal("a partial body was stored")
	}
	v, err := s.SetAllowanceParams(testContext, "allowance", p.Marshal(), "open the grant pool", 0)
	if err != nil || v != 1 {
		t.Fatalf("set %d %v", v, err)
	}
	// Pools are sized when a day opens: the new grant share applies from the
	// next day.
	s.now = func() time.Time { return time.Unix(testTime+86400, 0) }
	if err = s.GrantAllowance(testContext, keyID(key), "post_bytes", "granted", 1000, "welcome"); err != nil {
		t.Fatal(err)
	}
	if err = s.GrantAllowance(testContext, keyID(key), "post_bytes", "granted", 64<<20, "too much"); err == nil {
		t.Fatal("a grant beyond the pool")
	}
	q := run(t, s, signed(key, Command{Operation: "quota.get", Timestamp: testTime + 86400})).Data
	if q["params_version"] != int64(1) || q["remaining_bytes"].(int64) != q["daily_bytes"].(int64)-q["used_bytes"].(int64)+1000 {
		t.Fatalf("after a grant %v", q)
	}
}

// While a service runs, the first-call line also names the catalogue, and
// stays short enough for a DNS TXT string.
func TestAllowanceLineNamesTheCatalogue(t *testing.T) {
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	s := openTest(t, Config{Features: Features{Ledger: LedgerOn, Services: []string{"memory"}}})
	alice := keyFor(64)
	register(t, s, alice)
	note := run(t, s, signed(alice, Command{Operation: "post", Text: "hello"})).Allowance
	if note == nil || !strings.HasSuffix(note.Line, " Services: "+ServicesCatalogueURL+".") || note.Services != ServicesCatalogueURL || len(note.Line) > 255 {
		t.Fatalf("allowance note %+v", note)
	}
}
