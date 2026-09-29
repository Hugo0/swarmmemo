package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"swarmmemo/internal/allowance"
)

var ctx = context.Background()

type fakeLevers struct{ lv allowance.Levers }

func (f *fakeLevers) Levers(context.Context, allowance.Querier, int64) (allowance.Levers, error) {
	return f.lv, nil
}
func (f *fakeLevers) PrefixBlocked(context.Context, allowance.Querier, string, int64) (bool, error) {
	return false, nil
}

// tierClassifier reads the tier from the subject id: "t1-…" is tier 1, and
// so on; anything else is Design 0 (signed 3, anonymous 4). "root-X-…"
// shares root X.
type tierClassifier struct{}

func (tierClassifier) Classify(ctx context.Context, q allowance.Querier, s allowance.Subject, now int64) (allowance.Standing, error) {
	st, _ := DefaultClassifier{}.Classify(ctx, q, s, now)
	for tier := 1; tier <= 4; tier++ {
		if strings.HasPrefix(s.ID, fmt.Sprintf("t%d-", tier)) {
			st.Tier = allowance.Tier(tier)
		}
	}
	if rest, ok := strings.CutPrefix(s.ID, "root-"); ok {
		st.Root, _, _ = strings.Cut(rest, "-")
	}
	return st, nil
}

type harness struct {
	t      testing.TB
	db     *sql.DB
	l      *Ledger
	levers *fakeLevers
	p      AllowanceParams
	now    int64
}

func newHarness(t testing.TB, p AllowanceParams) *harness {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err = db.Exec(Schema); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, db: db, levers: &fakeLevers{lv: noLevers()}, p: p, now: day0 * 86400}
	store := ParamsStore{Overrides: map[string]Namespace{AllowanceNamespace: {Body: func() []byte { return h.p.Marshal() }, Validate: func(b []byte) error { _, err := ParseAllowanceParams(b); return err }}}}
	h.l = New(Config{Classifier: tierClassifier{}, Levers: h.levers, Params: store})
	return h
}

// smallParams: post_bytes budget 1000, caps 400/300/200/100, floors 0.
func smallParams() AllowanceParams {
	p := DefaultAllowanceParams()
	rp := p.Resources[allowance.PostBytes]
	rp.Budget, rp.SpendCeiling, rp.InboundCap, rp.TransferFee = 1000, 2000, 1000, 10
	rp.Cap = []int64{400, 300, 200, 100}
	rp.Floor = []int64{0, 0, 0, 0}
	rp.RootCap = []int64{1000, 1000, 1000, 1000}
	return p
}

// do runs fn in a transaction, committing on success and rolling back on
// any error, as the board does with a command.
func (h *harness) do(fn func(q *sql.Tx) error) error {
	h.t.Helper()
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	if err = fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err = tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
	return nil
}

func signedSubject(id string) allowance.Subject {
	return allowance.Subject{ID: id, KeyID: "key-" + id, Signed: true}
}

func anonSubject(id string) allowance.Subject { return allowance.Subject{ID: "anon:" + id} }

func (h *harness) spend(s allowance.Subject, units int64) error {
	return h.do(func(q *sql.Tx) error {
		_, err := h.l.Spend(ctx, q, s, allowance.PostBytes, units, Ref{Service: "board", Op: "post"}, h.now)
		return err
	})
}

func (h *harness) balance(s allowance.Subject) Balance {
	h.t.Helper()
	b, err := h.l.Balance(ctx, h.db, s, allowance.PostBytes, h.now)
	if err != nil {
		h.t.Fatal(err)
	}
	return b
}

func (h *harness) transfer(from allowance.Subject, to string, amount int64, key string) (Transfer, error) {
	var t Transfer
	err := h.do(func(q *sql.Tx) error {
		var err error
		t, err = h.l.Transfer(ctx, q, from, to, allowance.PostBytes, amount, key, h.now)
		return err
	})
	return t, err
}

func (h *harness) sweep() int {
	h.t.Helper()
	var n int
	if err := h.do(func(q *sql.Tx) error {
		var err error
		n, err = h.l.Sweep(ctx, q, h.now, SweepMax)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func code(err error) string {
	if e, ok := err.(*allowance.Err); ok {
		return e.Code
	}
	if err == nil {
		return ""
	}
	return "error: " + err.Error()
}

func (h *harness) count(query string, args ...any) int64 {
	h.t.Helper()
	var n int64
	if err := h.db.QueryRow(query, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// checkInvariants asserts RFC0012 §10 invariants 1–5 over the whole store.
func checkInvariants(t testing.TB, db *sql.DB, p AllowanceParams) {
	t.Helper()
	fail := func(format string, args ...any) { t.Helper(); t.Fatalf("invariant: "+format, args...) }
	scanAll := func(query string, dest func(scan func(...any) error) error, args ...any) {
		t.Helper()
		rows, err := db.Query(query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			if err = dest(rows.Scan); err != nil {
				t.Fatal(err)
			}
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	// 1. Budget: Σ pools + unallocated = B; no pool overdrawn, so what the
	// pools gave (free spend and held free units, grants) ≤ B; non-paid spend
	// ≤ the spend ceiling. Entitlements are not bounded by B (overbooking).
	type dayKey struct {
		r   string
		day int64
	}
	var days []dayKey
	scanAll("SELECT resource,day FROM allowance_days", func(scan func(...any) error) error {
		var k dayKey
		days = append(days, k)
		return scan(&days[len(days)-1].r, &days[len(days)-1].day)
	})
	for _, k := range days {
		d, _, err := loadDay(ctx, db, allowance.Resource(k.r), k.day)
		if err != nil {
			t.Fatal(err)
		}
		if d.allocated()+d.Unallocated != d.Budget {
			fail("%s day %d: pools %d + unallocated %d != budget %d", k.r, k.day, d.allocated(), d.Unallocated, d.Budget)
		}
		var issued, lent, borrowed int64
		for tier, pl := range d.Pools {
			if pl.avail() < 0 {
				fail("%s day %d tier %d overdrawn: %+v", k.r, k.day, tier, pl)
			}
			issued += pl.Claimed + pl.Lent
			lent += pl.Lent
			borrowed += pl.Borrowed
		}
		if issued > d.Budget {
			fail("%s day %d issued %d over budget %d", k.r, k.day, issued, d.Budget)
		}
		// The pools of tiers 1–4 gave exactly the free units spent that day
		// (spends and fees) plus those held by the day's service holds, open or
		// committed; tier 0 exactly the day's grants and dividends.
		var spent, held, committed, grants int64
		if err = db.QueryRow("SELECT coalesce(sum(amount),0) FROM ledger_entries WHERE resource=? AND day=? AND bucket='free' AND kind IN ('spend','fee')", k.r, k.day).Scan(&spent); err != nil {
			t.Fatal(err)
		}
		if err = db.QueryRow("SELECT coalesce(sum(hp.units),0) FROM ledger_hold_parts hp JOIN ledger_holds h ON h.id=hp.hold_id JOIN ledger_lots l ON l.id=hp.lot_id WHERE h.resource=? AND h.created_at/86400=? AND h.state='held' AND h.service<>'ledger' AND l.bucket='free'", k.r, k.day).Scan(&held); err != nil {
			t.Fatal(err)
		}
		if err = db.QueryRow("SELECT coalesce(sum(e.amount),0) FROM ledger_entries e JOIN ledger_holds h ON h.id=e.hold_id WHERE e.kind='commit' AND e.bucket='free' AND h.resource=? AND h.created_at/86400=?", k.r, k.day).Scan(&committed); err != nil {
			t.Fatal(err)
		}
		if err = db.QueryRow("SELECT coalesce(sum(amount),0) FROM ledger_entries WHERE resource=? AND day=? AND kind IN ('grant','earn')", k.r, k.day).Scan(&grants); err != nil {
			t.Fatal(err)
		}
		if drawn := issued - d.Pools[0].Claimed; drawn != spent+held+committed || d.Pools[0].Claimed != grants {
			fail("%s day %d: pools gave %d (grants %d) but free spend %d + held %d + committed %d, grants %d", k.r, k.day, drawn, d.Pools[0].Claimed, spent, held, committed, grants)
		}
		// 4. Priority: a tier never borrows from a higher tier.
		if lent != borrowed || d.Pools[1].Lent != 0 || d.Pools[4].Borrowed != 0 || d.Pools[0].Lent != 0 || d.Pools[0].Borrowed != 0 {
			fail("%s day %d: lending out of order %+v", k.r, k.day, d.Pools)
		}
		var lentUpTo, borrowedBelow int64
		for tier := 1; tier <= 4; tier++ {
			lentUpTo += d.Pools[tier].Lent
			if lentUpTo > borrowedBelow {
				fail("%s day %d: tiers ≤ %d lent %d but tiers above borrowed %d", k.r, k.day, tier, lentUpTo, borrowedBelow)
			}
			borrowedBelow += d.Pools[tier].Borrowed
		}
		if rp := p.Resources[allowance.Resource(k.r)]; rp != nil && d.SpentNonpaid > rp.SpendCeiling {
			fail("%s day %d: non-paid spend %d over the ceiling %d", k.r, k.day, d.SpentNonpaid, rp.SpendCeiling)
		}
		if d.SpentNonpaid < 0 || d.SpentPaid < 0 {
			fail("%s day %d: negative spend", k.r, k.day)
		}
	}
	// 2. Conservation: each lot 0 ≤ held ≤ remaining ≤ initial; a lot's
	// journal credits equal its initial and its debits initial − remaining;
	// held equals its open holds' parts.
	scanAll("SELECT l.id,l.bucket,l.origin_tier,l.issued_day,l.expires_at,l.half_life_days,l.initial,l.remaining,l.held,l.state,"+
		"(SELECT coalesce(sum(amount),0) FROM ledger_entries e WHERE e.lot_id=l.id AND e.account=l.account AND e.kind IN ('claim','grant','earn','topup','transfer_in')),"+
		"(SELECT coalesce(sum(amount),0) FROM ledger_entries e WHERE e.lot_id=l.id AND e.account=l.account AND e.kind IN ('spend','commit','fee','transfer_out','expire','decay','forfeit')),"+
		"(SELECT coalesce(sum(hp.units),0) FROM ledger_hold_parts hp JOIN ledger_holds h ON h.id=hp.hold_id WHERE hp.lot_id=l.id AND h.state='held') FROM ledger_lots l",
		func(scan func(...any) error) error {
			var id, tier, issued, expires, hl, initial, remaining, held, credits, debits, holds int64
			var bucket, state string
			if err := scan(&id, &bucket, &tier, &issued, &expires, &hl, &initial, &remaining, &held, &state, &credits, &debits, &holds); err != nil {
				return err
			}
			if held < 0 || held > remaining || remaining > initial {
				fail("lot %d: held %d remaining %d initial %d", id, held, remaining, initial)
			}
			if credits != initial || debits != initial-remaining {
				fail("lot %d (%s): credits %d debits %d vs initial %d remaining %d", id, bucket, credits, debits, initial, remaining)
			}
			if holds != held {
				fail("lot %d: held %d but open holds reserve %d", id, held, holds)
			}
			if state != "live" && (remaining != 0 || held != 0) {
				fail("lot %d is %s with %d remaining", id, state, remaining)
			}
			// 3. No banking: tier 3–4 free lots expire at the end of their
			// issue day, whoever holds them; paid lots never decay.
			if bucket == "free" && tier >= 3 && expires != (issued+1)*86400 {
				fail("lot %d: tier-%d free lot issued day %d expires %d", id, tier, issued, expires)
			}
			if bucket == "paid" && (hl != 0 || expires != 0) {
				fail("lot %d: paid lot decays (half-life %d, expiry %d)", id, hl, expires)
			}
			return nil
		})
	// Holds: a settled hold's parts equal its maximum; an open one's too.
	scanAll("SELECT h.id,h.max_units,h.used_units,h.state,(SELECT coalesce(sum(units),0) FROM ledger_hold_parts WHERE hold_id=h.id) FROM ledger_holds h", func(scan func(...any) error) error {
		var id, state string
		var maxUnits, used, parts int64
		if err := scan(&id, &maxUnits, &used, &state, &parts); err != nil {
			return err
		}
		if parts != maxUnits || used < 0 || used > maxUnits {
			fail("hold %s (%s): parts %d used %d max %d", id, state, parts, used, maxUnits)
		}
		return nil
	})
	// Transfers: a done transfer moved or expired exactly its amount.
	scanAll("SELECT t.id,t.amount,t.state,(SELECT coalesce(sum(amount),0) FROM ledger_entries e WHERE e.public_ref=t.id AND e.kind IN ('transfer_out','expire') AND e.account=t.from_account),(SELECT coalesce(sum(amount),0) FROM ledger_entries e WHERE e.public_ref=t.id AND e.kind='transfer_in' AND e.account=t.to_account),(SELECT coalesce(sum(amount),0) FROM ledger_entries e WHERE e.public_ref=t.id AND e.kind='transfer_out' AND e.account=t.from_account) FROM ledger_transfers t", func(scan func(...any) error) error {
		var id, state string
		var amount, out, in, moved int64
		if err := scan(&id, &amount, &state, &out, &in, &moved); err != nil {
			return err
		}
		if state == "done" && out != amount || in != moved {
			fail("transfer %s (%s): amount %d, out+expired %d, moved %d, in %d", id, state, amount, out, moved, in)
		}
		return nil
	})
	// 5. One claim per (resource, day, subject) is the primary key; a claim's
	// lot holds what it granted.
	scanAll("SELECT c.granted,c.lot_id,coalesce((SELECT initial FROM ledger_lots WHERE id=c.lot_id),0) FROM allowance_claims c", func(scan func(...any) error) error {
		var granted, lotID, initial int64
		if err := scan(&granted, &lotID, &initial); err != nil {
			return err
		}
		if granted > 0 && (lotID == 0 || initial < granted) || granted == 0 && lotID != 0 {
			fail("claim granted %d but lot %d has %d", granted, lotID, initial)
		}
		return nil
	})
}
