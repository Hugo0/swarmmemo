package ledger

import (
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// FuzzLedgerSequence runs random claim, spend, reserve, commit, refund,
// transfer, cancel, lever, sweep, breaker, mint, read and day-change
// sequences and checks invariants 1–5 (RFC0012 §10) after every operation.
func FuzzLedgerSequence(f *testing.F) {
	f.Add([]byte{0, 0, 50, 0, 0, 5, 50, 0, 4, 2, 30, 3, 7, 0, 0, 0})
	f.Add([]byte{10, 2, 0, 1, 4, 2, 90, 1, 5, 0, 0, 9, 8, 0, 255, 0, 7, 0, 0, 0})
	f.Add([]byte{1, 0, 80, 0, 2, 0, 30, 0, 1, 1, 60, 0, 8, 0, 200, 0, 7, 0, 0, 0, 3, 0, 0, 1})
	f.Add([]byte{6, 0, 0, 3, 0, 5, 200, 0, 6, 0, 0, 6, 0, 3, 90, 0, 11, 1, 100, 2, 4, 1, 250, 0, 9, 0, 0, 0, 7, 0, 0, 0})
	f.Add([]byte{11, 0, 255, 0, 11, 0, 255, 1, 12, 0, 0, 0, 1, 0, 255, 0, 9, 0, 0, 0, 9, 0, 0, 0, 7, 0, 0, 0})
	f.Fuzz(runSequence)
}

// TestLedgerSequenceRandom runs long pseudo-random sequences on every test
// run, so the invariants are exercised without -fuzz.
func TestLedgerSequenceRandom(t *testing.T) {
	for seed := int64(1); seed <= 12; seed++ {
		r := rand.New(rand.NewSource(seed))
		data := make([]byte, 4*64)
		r.Read(data)
		t.Run(fmt.Sprint(seed), func(t *testing.T) { runSequence(t, data) })
	}
}

func runSequence(t *testing.T, data []byte) {
	{
		if len(data) > 4*64 {
			data = data[:4*64]
		}
		p := smallParams()
		rp := p.Resources[allowance.PostBytes]
		rp.GrantSharePPM = 200_000
		rp.Floor = []int64{20, 20, 10, 5}
		rp.ClientSharePPM = 600_000
		p.SpikeFloor = 3
		h := newHarness(t, p)
		subjects := []allowance.Subject{
			signedSubject("t1-a"), signedSubject("t2-b"), signedSubject("s3"), signedSubject("root-x-s4"), signedSubject("root-x-s5"),
			anonSubject("n6"), {ID: "anon:n7", Client: "c1"}, {ID: "anon:n7", Client: "c2"},
		}
		var holds, transfers []string
		seq := 0
		for i := 0; i+3 < len(data); i += 4 {
			op, s, amt, arg := data[i]%13, subjects[int(data[i+1])%len(subjects)], int64(data[i+2]), data[i+3]
			seq++
			key := fmt.Sprint("k", seq)
			var err error
			switch op {
			case 0:
				err = h.spend(s, amt)
			case 1:
				err = h.do(func(q *sql.Tx) error {
					hold, err := h.l.Reserve(ctx, q, s, allowance.PostBytes, amt+1, key, Ref{Service: "echo", Method: "echo"}, int64(arg)+1, h.now)
					if err == nil {
						holds = append(holds, hold.ID)
					}
					return err
				})
			case 2, 3:
				if len(holds) == 0 {
					continue
				}
				id := holds[int(arg)%len(holds)]
				err = h.do(func(q *sql.Tx) error {
					if op == 3 {
						return h.l.Refund(ctx, q, id, "fuzz", h.now)
					}
					hr, _, err := loadHoldRow(ctx, q, id)
					if err != nil {
						return err
					}
					_, err = h.l.Commit(ctx, q, id, amt%(hr.Max+1), h.now)
					return err
				})
			case 4:
				if !s.Signed {
					continue
				}
				to := subjects[int(arg)%5].ID
				var tr Transfer
				tr, err = h.transfer(s, to, amt, key)
				if err == nil {
					transfers = append(transfers, tr.ID)
				}
			case 5:
				if len(transfers) == 0 {
					continue
				}
				id := transfers[int(arg)%len(transfers)]
				err = h.do(func(q *sql.Tx) error {
					_, err := h.l.CancelTransfer(ctx, q, id, "fuzz-key", h.now)
					return err
				})
			case 6:
				lv := &h.levers.lv
				switch arg % 7 {
				case 0:
					lv.SignedOnly = !lv.SignedOnly
				case 1:
					lv.ProvenOnly = !lv.ProvenOnly
				case 2:
					lv.FreezeTransfers = !lv.FreezeTransfers
				case 3:
					if lv.BudgetCutPPM == nil {
						lv.BudgetCutPPM = map[allowance.Resource]int64{allowance.PostBytes: amt * 3900}
					} else {
						lv.BudgetCutPPM = nil
					}
				case 4:
					if lv.Tier4SharePPM < 0 {
						lv.Tier4SharePPM = amt * 3900
					} else {
						lv.Tier4SharePPM = -1
					}
				case 5:
					lv.PauseNewKeys = !lv.PauseNewKeys
				default:
					*lv = noLevers()
				}
				continue
			case 7:
				h.sweep()
			case 8:
				h.now += amt * 600
			case 9:
				h.now += 86400
			case 10:
				if !s.Signed {
					continue
				}
				err = h.do(func(q *sql.Tx) error { return h.l.TripBreaker(ctx, q, s.ID, "agent.rotate", "old-"+s.ID, h.now) })
			case 11:
				bucket := []allowance.Bucket{allowance.Granted, allowance.Earned, allowance.Paid}[int(arg)%3]
				err = h.do(func(q *sql.Tx) error {
					return h.l.Mint(ctx, q, s.ID, allowance.PostBytes, bucket, amt+1, "fuzz", h.now)
				})
			case 12:
				before := h.count("SELECT count(*) FROM ledger_entries") + h.count("SELECT count(*) FROM allowance_days") + h.count("SELECT count(*) FROM allowance_claims")
				b := h.balance(s)
				after := h.count("SELECT count(*) FROM ledger_entries") + h.count("SELECT count(*) FROM allowance_days") + h.count("SELECT count(*) FROM allowance_claims")
				if before != after {
					t.Fatalf("a balance read wrote %d rows", after-before)
				}
				if b.Remaining < 0 || b.Entitlement < 0 {
					t.Fatalf("negative balance %+v", b)
				}
			}
			var refusal *allowance.Err
			if err != nil && !errors.As(err, &refusal) {
				t.Fatalf("op %d on %s: %v", op, s.ID, err)
			}
			checkInvariants(t, h.db, h.p)
		}
	}
}

// FuzzParams feeds the strict parser arbitrary bodies: anything accepted
// validates, round-trips byte-identically and runs the waterfall without a
// negative pool or a broken budget identity.
func FuzzParams(f *testing.F) {
	base := DefaultAllowanceParams().Marshal()
	f.Add(base)
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"schema":1}`))
	f.Add([]byte(strings.Replace(string(base), `"dust":64`, `"dust":64,"dust":65`, 1)))
	f.Add([]byte(strings.Replace(string(base), `"budget":67108864`, `"budget":-1`, 1)))
	f.Add([]byte(strings.Replace(string(base), `"cap":[16777216,8388608,4194304,4194304]`, `"cap":[1,2,3]`, 1)))
	f.Add([]byte(strings.Replace(string(base), `"spill_interval":3600`, `"spill_interval":0`, 1)))
	f.Add(append(append([]byte{}, base...), ' ', '{', '}'))
	small := smallParams()
	small.Resources[allowance.PostBytes].GrantSharePPM = 999_999
	f.Add(small.Marshal())
	f.Fuzz(func(t *testing.T, body []byte) {
		p, err := ParseAllowanceParams(body)
		if err != nil {
			return
		}
		if err = p.Validate(); err != nil {
			t.Fatalf("parsed but invalid: %v", err)
		}
		again, err := ParseAllowanceParams(p.Marshal())
		if err != nil || !reflect.DeepEqual(again, p) || string(again.Marshal()) != string(p.Marshal()) {
			t.Fatalf("no round trip: %v", err)
		}
		for r, rp := range p.Resources {
			for _, lv := range []allowance.Levers{noLevers(), {Tier4SharePPM: 1, SignedOnly: true, BudgetCutPPM: map[allowance.Resource]int64{r: 999_999}}} {
				prev := &dayState{}
				for tier := range prev.Pools {
					prev.Pools[tier].Claimed, prev.Pools[tier].ExpectedUnits = rp.Budget, rp.Budget
					prev.Pools[tier].ClaimedWeight = 1_000_000
				}
				d := computeOpen(r, day0, rp, lv, prev, 1, day0*86400)
				for tier := 1; tier <= 4; tier++ {
					st := allowance.Standing{Tier: allowance.Tier(tier), WeightPPM: ppm, Root: "x"}
					draw(&d, rp, tier, entitlement(&d, rp, lv, st, 0))
				}
				spill(&d, rp, day0*86400+86399)
				adjust(&d, rp, noLevers())
				if d.allocated()+d.Unallocated != d.Budget {
					t.Fatalf("%s: budget identity broken %+v", r, d)
				}
				for tier, pl := range d.Pools {
					if pl.avail() < 0 || pl.Size < 0 {
						t.Fatalf("%s tier %d negative: %+v", r, tier, pl)
					}
				}
			}
		}
	})
}
