package board

import (
	"bytes"
	"testing"

	"swarmmemo/internal/trust"
)

// From trust parameter version 3 the snapshot names a paid call's payee
// when the relation is already public (the spender itself, or an account a
// transfer linked to it) and the resource's host, carries a domain's
// registration time, and the run leaves self-dealt spend out of the seed.
func TestTrustSnapshotPaidCallPayees(t *testing.T) {
	s, seed, alice, bob, _ := trustFixture(t, nil)
	accountOf := func(id string) string {
		var a string
		if err := s.db.QueryRow("SELECT account FROM identities WHERE id=?", id).Scan(&a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	a, b, c := accountOf(keyID(alice)), accountOf(keyID(bob)), accountOf(keyID(seed))
	day := int64(testTime/86400 - 2)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec("INSERT INTO identity_links(agent,kind,value,state,created_at,checked_at) VALUES(?,'domain','www.alice.example','verified',?,?)", keyID(alice), testTime-86400, testTime-3600)
	// The RDAP lookup that will create this table is not built yet (C148).
	exec("CREATE TABLE IF NOT EXISTS domain_registrations (domain TEXT PRIMARY KEY, registered_at INTEGER NOT NULL, source TEXT NOT NULL, checked_at INTEGER NOT NULL)")
	exec("INSERT INTO domain_registrations VALUES('alice.example',?,'test',?)", testTime-3*365*86400, testTime-3600)
	// alice tops up from wallet A and bob from wallet B.
	topup := func(n int, account, payer string) {
		exec(`INSERT INTO credit_topups(id,account,agent,amount,day,network,asset,pay_to,payer,auth_nonce,valid_before,state,created_at)
 VALUES(?,?,?,1000000,?,'eip155:8453','usdc','0xus',?,?,0,'credited',?)`, "tu"+string(rune('0'+n)), account, account, day, payer, "nonce"+string(rune('0'+n)), testTime-5*86400)
	}
	topup(1, a, "0xAAAA")
	topup(2, b, "0xBBBB")
	topup(3, c, "0xCCCC")
	exec("INSERT INTO x402_catalogue(id,bundler,url,method,pay_to,amount,first_seen,last_seen) VALUES('res-alice','b','https://api.alice.example/x','GET','0xaaaa',1,0,0)")
	exec("INSERT INTO x402_catalogue(id,bundler,url,method,pay_to,amount,first_seen,last_seen) VALUES('res-bob','b','https://bob.example/y','GET','0xbbbb',1,0,0)")
	exec("INSERT INTO x402_catalogue(id,bundler,url,method,pay_to,amount,first_seen,last_seen) VALUES('res-seed','b','https://seed.example/z','GET','0xcccc',1,0,0)")
	call := func(n int, account, resource, payTo string, credit int64) {
		hold, key := "hold"+string(rune('0'+n)), "rk"+string(rune('0'+n))
		exec("INSERT INTO service_calls(id,account,service,method,request_key,hold_id,resource,state,prices_version,created_at) VALUES(?,?,'x402','call',?,?,'credit','done',0,?)",
			"call"+string(rune('0'+n)), account, key, hold, testTime-2*86400)
		exec(`INSERT INTO x402_payments(id,account,request_key,resource,day,amount,network,asset,pay_to,nonce,valid_before,state,allowlist_version,created_at)
 VALUES(?,?,?,?,?,10000,'eip155:8453','usdc',?,?,0,'paid',0,?)`, "pay"+string(rune('0'+n)), account, key, resource, day, payTo, "pn"+string(rune('0'+n)), testTime-2*86400)
		exec("INSERT INTO ledger_entries(day,created_at,kind,account,resource,bucket,amount,hold_id,service,params_version) VALUES(?,?,'commit',?,'credit','paid',?,?,'x402',0)",
			day, testTime-2*86400, account, -credit, hold)
	}
	call(1, a, "res-alice", "0xAAAA", 3000000) // alice pays her own resource
	call(2, a, "res-bob", "0xBBBB", 2000000)   // alice pays bob, whom she funded
	call(3, b, "res-alice", "0xAAAA", 1000000) // and bob pays alice back
	call(4, a, "res-seed", "0xCCCC", 5000000)  // the seed is not linked to alice: not named
	exec("INSERT INTO ledger_transfers(id,from_account,to_account,resource,amount,fee,state,created_at,execute_at,done_at) VALUES('tr1',?,?,'credit',500,0,'done',?,?,?)",
		a, b, testTime-10*86400, testTime-10*86400, testTime-10*86400)
	exec("INSERT INTO ledger_entries(day,created_at,kind,account,resource,bucket,amount,service,params_version) VALUES(?,?,'spend',?,'credit','paid',-4000000,'fetch',0)",
		day, testTime-2*86400, b)
	var buf bytes.Buffer
	if err := s.WriteTrustInputs(testContext, &buf); err != nil {
		t.Fatal(err)
	}
	snap, err := trust.ReadJSONL(&buf)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, r := range snap.Spends {
		got[r.Account+">"+r.To+"@"+r.LinkValue] += r.Amount
	}
	want := map[string]int64{
		a + ">" + a + "@api.alice.example": 3000000,
		a + ">" + b + "@bob.example":       2000000,
		b + ">" + a + "@api.alice.example": 1000000,
		a + ">@seed.example":               5000000,
		b + ">@":                           4000000,
	}
	if len(got) != len(want) {
		t.Fatalf("spend records %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("spend records %v, want %v", got, want)
		}
	}
	registered := false
	for _, r := range snap.Proofs {
		registered = registered || (r.Kind == "domain" && r.LinkValue == "www.alice.example" && r.RegisteredAt == testTime-3*365*86400)
	}
	if !registered || len(snap.Transfers) == 0 {
		t.Fatalf("registration %v, transfers %d", registered, len(snap.Transfers))
	}
	out, err := trust.Compute(testContext, snap)
	if err != nil {
		t.Fatal(err)
	}
	// Self-dealt: alice's own resource (her wallet and her domain), and both
	// payments between alice and bob, linked by the transfer.
	if n := out.Standing.Inputs["spend_self_dealt"]; n != 3 {
		t.Fatalf("self-dealt %d: %v", n, out.Standing.Inputs)
	}
	// Version 2 snapshots stay as they were: no payees, no registration.
	p := trust.DefaultParams()
	p.Version, p.Standing = trust.StandingVersion, trust.StandingV2()
	p.Seeds = []string{keyID(seed)}
	if _, err := s.db.Exec("UPDATE params SET body=? WHERE namespace='trust'", string(p.Body())); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := s.WriteTrustInputs(testContext, &buf); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte(`"registered_at"`)) || bytes.Contains(buf.Bytes(), []byte(`"type":"spend","to"`)) || bytes.Contains(buf.Bytes(), []byte(`seed.example`)) {
		t.Fatal("a version 2 snapshot carries version 3 fields")
	}
}
