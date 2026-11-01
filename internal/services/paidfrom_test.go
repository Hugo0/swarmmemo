package services_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/services"
)

// C516: every call answer names what paid its cost, through the real ledger.
// A call met from the free daily allowance says free_allowance; one that
// bought (or granted) credit paid says credits. Local and remote calls, and a
// remote call read back with status, all agree.
func TestCallPaidFromNamesTheBucket(t *testing.T) {
	ctx := context.Background()
	const now = int64(20000 * 86400)
	db := openDB(t)
	if _, err := db.Exec(ledger.Schema); err != nil {
		t.Fatal(err)
	}
	p := ledger.DefaultAllowanceParams()
	rp := p.Resources[allowance.Credit]
	rp.Budget, rp.SpendCeiling, rp.InboundCap = 1_600_000, 1_600_000, 1_600_000
	rp.Floor, rp.Cap = 30_000, 100_000
	body := p.Marshal()
	l := ledger.New(ledger.Config{Params: ledger.ParamsStore{Overrides: map[string]ledger.Namespace{ledger.AllowanceNamespace: {Body: func() []byte { return body }, Validate: func([]byte) error { return nil }}}}})
	reg := services.NewBuiltinRegistry([]string{"echo"}, services.Deps{EchoSimulate: true})
	e := services.NewEngine(services.Config{DB: db, Registry: reg, Meter: l, Now: func() int64 { return now }})
	t.Cleanup(e.Stop)

	n := 0
	call := func(s allowance.Subject, data string) map[string]any {
		t.Helper()
		n++
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		out, err := e.Call(ctx, tx, services.Request{Service: "echo", Data: data, Subject: s, RequestKey: "id:paid-from-" + string(rune('a'+n))}, now)
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		d := out.Data
		if out.After != nil {
			if d, err = out.After(); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	paidFrom := func(d map[string]any) (string, int64) {
		t.Helper()
		rec, ok := d["call"].(services.CallRecord)
		if !ok {
			t.Fatalf("call is %T", d["call"])
		}
		return rec.PaidFrom, rec.Cost
	}
	// C685: credits_spent and allowance_left come with paid_from.
	spent := func(d map[string]any) (int64, int64) {
		t.Helper()
		rec := d["call"].(services.CallRecord)
		if rec.CreditsSpent == nil || rec.AllowanceLeft == nil {
			t.Fatalf("call without credits_spent/allowance_left: %+v", rec)
		}
		return *rec.CreditsSpent, *rec.AllowanceLeft
	}
	status := func(s allowance.Subject, d map[string]any) string {
		t.Helper()
		id := d["call"].(services.CallRecord).ID
		got, err := e.Read(ctx, db, services.Request{Service: "echo", Data: `{"schema":1,"method":"status","args":{"call":"` + id + `"}}`, Subject: s}, now)
		if err != nil {
			t.Fatal(err)
		}
		pf, _ := paidFrom(got)
		return pf
	}
	const local = `{"schema":1,"method":"echo","args":{"text":"hello"},"max_cost":50}`
	const remote = `{"schema":1,"method":"echo","args":{"text":"hello","simulate":{"mode":"remote","delay_ms":1}},"max_cost":50}`

	free := allowance.Subject{ID: "acct-free", KeyID: "k1", Signed: true}
	for _, data := range []string{local, remote} {
		before, err := l.Balance(ctx, db, free, allowance.Credit, now)
		if err != nil {
			t.Fatal(err)
		}
		d := call(free, data)
		pf, cost := paidFrom(d)
		if pf != services.PaidFromFreeAllowance || cost <= 0 {
			t.Fatalf("a call met from the free allowance: paid_from %q, cost %d", pf, cost)
		}
		if cs, left := spent(d); cs != 0 || left != before.ByBucket[allowance.Free]-cost {
			t.Fatalf("free call: credits_spent %d allowance_left %d, want 0 and %d", cs, left, before.ByBucket[allowance.Free]-cost)
		}
		if raw, _ := json.Marshal(d["call"]); !strings.Contains(string(raw), `"credits_spent":0,`) || !strings.Contains(string(raw), `"allowance_left":`) {
			t.Fatalf("call JSON drops a zero credits_spent: %s", raw)
		}
		if data == remote {
			if pf := status(free, d); pf != services.PaidFromFreeAllowance {
				t.Fatalf("status of a free call: paid_from %q", pf)
			}
		}
	}

	// Spend the whole free allowance, then buy credit: the next calls are
	// paid from it.
	bought := allowance.Subject{ID: "acct-bought", KeyID: "k2", Signed: true}
	b, err := l.Balance(ctx, db, bought, allowance.Credit, now)
	if err != nil || b.Remaining <= 0 {
		t.Fatalf("balance %+v %v", b, err)
	}
	if _, err = l.Spend(ctx, db, bought, allowance.Credit, b.Remaining, ledger.Ref{Service: "test", Op: "drain"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err = l.TopUp(ctx, db, bought.ID, allowance.Credit, 1000, "test top-up", ledger.Ref{}, now); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{local, remote} {
		d := call(bought, data)
		pf, cost := paidFrom(d)
		if pf != services.PaidFromCredits || cost <= 0 {
			t.Fatalf("a call paid with bought credit: paid_from %q, cost %d", pf, cost)
		}
		if cs, left := spent(d); cs != cost || left != 0 {
			t.Fatalf("bought call: credits_spent %d allowance_left %d, want %d and 0", cs, left, cost)
		}
		if data == remote {
			if pf := status(bought, d); pf != services.PaidFromCredits {
				t.Fatalf("status of a bought call: paid_from %q", pf)
			}
		}
	}
}
