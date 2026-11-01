package board

import (
	"context"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// TestTrustParamsCacheNeverHoldsItsLockAcrossARead is the 11-01 04:09Z stall:
// the find rebuild took the params cache lock and waited on the pool for the
// one connection, which a front-page read held in a tx while it waited for
// that lock. Every request queued behind both until contexts expired (502s).
func TestTrustParamsCacheNeverHoldsItsLockAcrossARead(t *testing.T) {
	s := openTest(t, Config{})
	now := s.now().Unix()
	for _, read := range []func(ctx context.Context, q allowance.Querier) error{
		func(ctx context.Context, q allowance.Querier) error {
			_, err := s.trustParamsAt(ctx, q, now)
			return err
		},
		func(ctx context.Context, q allowance.Querier) error {
			_, _, err := s.voteWeightMaps(ctx, q, now)
			return err
		},
	} {
		s.trust.forgetParams()
		tx, err := s.db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		// The pool reader: it needs the connection the tx holds.
		poolCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = read(poolCtx, s.db)
		}()
		time.Sleep(100 * time.Millisecond)
		// The tx holder must get through without waiting for the pool reader.
		start := time.Now()
		if err := read(context.Background(), tx); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited > time.Second {
			t.Fatalf("the tx holder waited %v behind a pool reader holding the cache lock", waited)
		}
		_ = tx.Rollback()
		<-done
		cancel()
	}
}
