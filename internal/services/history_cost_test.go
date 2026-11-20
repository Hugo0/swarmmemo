package services_test

import (
	"context"
	"strconv"
	"testing"

	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
)

// notaryCosts is each of account's notary entries' cost, by stamped hash,
// from a service.calls page and from HistoryByID, which must agree.
func notaryCosts(t *testing.T, r *contentRig, account string) map[string]int64 {
	t.Helper()
	rows, _, err := r.e.History(context.Background(), r.db, services.HistoryQuery{Account: account, Tool: "notary", Limit: services.HistoryPageMax, Now: r.now})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, row := range rows {
		cost := row.Entry["cost"].(int64)
		out[row.Hash] = cost
		byID, ok, err := r.e.HistoryByID(context.Background(), r.db, row.ID, r.now)
		if err != nil || !ok || byID.Entry["cost"] != cost || byID.Entry["cost_note"] != row.Entry["cost_note"] {
			t.Fatalf("HistoryByID(%s) = %v %v %v, the page says %v", row.ID, byID.Entry, ok, err, row.Entry)
		}
	}
	return out
}

// C1132 (oren-rot b8fb48b2): a notary entry's cost is what made that very
// receipt, joined on its seq, never a same-second guess. A doc's notary:true
// surcharge read "Free: no cost." (no notary.stamp row that second), and
// then read 1 once an unrelated stamp landed in the same second; another
// agent's stamp, a repeat stamp and a later stamp change nothing.
func TestHistoryNotaryCostIsTheReceipts(t *testing.T) {
	r := &contentRig{t: t, db: openDB(t), meter: servicestest.NewMeter(1 << 30), board: newFakeBoard(), jev: &fakeScreener{cost: 40}, now: wakeT0}
	reg := services.NewBuiltinRegistry([]string{"paste", "docs", "notary"}, services.Deps{DB: r.db, Board: r.board, ServiceID: "swarmmemo.com", TextScreener: r.jev, NotaryKey: testNotaryKey})
	r.e = services.NewEngine(services.Config{DB: r.db, Registry: reg, Meter: r.meter, Now: func() int64 { return r.now }})
	t.Cleanup(r.e.Stop)

	doc := r.mustCall("alice", "docs", "create", map[string]any{"title": "t", "text": "a doc with a receipt", "notary": true})
	docHash := sha256Hex("a doc with a receipt")
	if get(doc, "result", "receipt", "hash") != docHash {
		t.Fatalf("create: %+v", doc)
	}
	if got := notaryCosts(t, r, "alice"); got[docHash] != services.DocNotaryPrice {
		t.Fatalf("doc receipt cost = %v, want the %d-credit notary surcharge", got, services.DocNotaryPrice)
	}

	// The same second: another agent's stamp, alice's own stamp of other
	// text, and her repeat of the doc's hash (a duplicate, charged one unit).
	r.mustCall("bob", "notary", "stamp", map[string]any{"text": "bob's text"})
	r.mustCall("alice", "notary", "stamp", map[string]any{"text": "alice's other text"})
	r.mustCall("alice", "notary", "stamp", map[string]any{"hash": docHash})
	r.mustCall("alice", "paste", "create", map[string]any{"text": "a paste with a receipt", "notary": true})
	want := map[string]int64{docHash: services.DocNotaryPrice, sha256Hex("alice's other text"): 1, sha256Hex("a paste with a receipt"): services.DocNotaryPrice}
	check := func(when string) {
		t.Helper()
		got := notaryCosts(t, r, "alice")
		if len(got) != len(want) {
			t.Fatalf("%s: entries %v, want %v", when, got, want)
		}
		for h, c := range want {
			if got[h] != c {
				t.Errorf("%s: cost of %s… = %d, want %d", when, h[:8], got[h], c)
			}
		}
	}
	check("same second")
	r.now += 5
	r.mustCall("alice", "notary", "stamp", map[string]any{"text": "a later stamp"})
	want[sha256Hex("a later stamp")] = 1
	check("later")

	// A receipt from before its stamp recorded the seq reads that stamp's
	// row, and a new stamp in its second (which records its seq) cannot
	// take its place.
	old := r.now + 10
	var seq int64
	if err := r.db.QueryRow("INSERT INTO notary_receipts(hash,time,account) VALUES(?,?,?) RETURNING seq", sha256Hex("legacy"), old, "alice").Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`INSERT INTO service_calls(id,account,service,method,request_key,hold_id,resource,cost,state,public,prices_version,created_at,finished_at)
VALUES('legacy','alice','notary','stamp','rk-legacy','','credit',2,'done','{}',1,?,?)`, old, old); err != nil {
		t.Fatal(err)
	}
	r.now = old
	r.mustCall("alice", "notary", "stamp", map[string]any{"text": "same second as the legacy one"})
	want[sha256Hex("legacy")], want[sha256Hex("same second as the legacy one")] = 2, 1
	check("legacy (seq " + strconv.FormatInt(seq, 10) + ")")
}
