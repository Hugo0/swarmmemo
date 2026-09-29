package services_test

// Security review 1.20 regression tests for the services engine and its
// small providers. Each one inverts a proof of concept (TestSecPoC_*, branch
// security-review-1.20): it fails while the weakness is present.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
)

// M4: echo's simulate is off in production, so crashed echo calls can no
// longer fill the board's 64 shared open-hold slots.
func TestSec120_EchoCrashCannotStarveGlobalHolds(t *testing.T) {
	db := openDB(t)
	meter := servicestest.NewMeter(1 << 20)
	reg := services.NewBuiltinRegistry([]string{"echo"}, services.Deps{DB: db}) // as the board builds it
	e := services.NewEngine(services.Config{DB: db, Registry: reg, Meter: meter, Now: func() int64 { return 1000 }})
	t.Cleanup(e.Stop)
	call := func(account, key, args string) error {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.Call(context.Background(), tx, services.Request{Service: "echo", Data: `{"schema":1,"method":"echo","args":` + args + `,"max_cost":10}`,
			Subject: allowance.Subject{ID: account, KeyID: account, Signed: true}, RequestKey: key}, 1000)
		if err != nil {
			tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	for a := 0; a < 2; a++ {
		if err := call(fmt.Sprintf("sybil-%d", a), "id:1", `{"text":"x","simulate":{"mode":"remote","crash":true}}`); code(err) != "invalid_service_data" {
			t.Fatalf("a simulated crash in production: %v", err)
		}
	}
	var running int
	if err := db.QueryRow("SELECT count(*) FROM service_calls WHERE state='running'").Scan(&running); err != nil || running != 0 {
		t.Fatalf("running %d %v", running, err)
	}
	// Plain echo still works.
	if err := call("honest", "id:1", `{"text":"hi"}`); err != nil {
		t.Fatalf("plain echo: %v", err)
	}
}

// L12: keys that differ only in case are duplicates: the strict parser
// refuses them, so no reader sees "private" where the decoder took "public".
func TestSec120_CaseFoldedDuplicateKeysRefused(t *testing.T) {
	if _, err := services.ParseData(`{"schema":1,"method":"put","METHOD":"delete","max_cost":300}`, true); code(err) != "invalid_service_data" {
		t.Fatalf("method and METHOD: %v", err)
	}
	if _, err := services.QuoteForTest("put", json.RawMessage(`{"key":"k","value":"secret","visibility":"private","VISIBILITY":"public"}`)); code(err) != "invalid_service_data" {
		t.Fatalf("visibility and VISIBILITY: %v", err)
	}
	if _, err := services.ParseData(`{"schema":1,"method":"put","args":{"key":"k","value":{"a":1,"b":2}},"max_cost":300}`, true); err != nil {
		t.Fatalf("distinct keys refused: %v", err)
	}
}

// slowRemote is a Remote provider whose Run blocks until released.
type slowRemote struct {
	release chan struct{}
	started *atomic.Int64
}

func (slowRemote) Describe() services.Descriptor {
	return services.Descriptor{ID: "slow", Summary: "test", Mode: services.Remote, MaxDuration: 3 * time.Second,
		Methods: []services.Method{{Name: "go", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: 64, Price: services.Price{Base: 5}}}}
}
func (slowRemote) Quote(c services.Call) (services.Quote, error) {
	return services.Quote{Resource: allowance.Credit, Max: 5}, nil
}
func (p slowRemote) Run(ctx context.Context, _ *sql.Tx, c services.Call) (services.Result, error) {
	p.started.Add(1)
	select {
	case <-p.release:
	case <-ctx.Done():
		return services.Result{}, ctx.Err()
	}
	return services.Result{Body: json.RawMessage(`{"answer":42}`), Used: 1}, nil
}

// M5: a call that waited for a Remote slot until too little of its hold is
// left is refused as busy and refunded before the upstream is asked, instead
// of answering after its hold expired and being charged the maximum with its
// result thrown away.
func TestSec120_SlotWaitCannotOutliveTheHold(t *testing.T) {
	db := openDB(t)
	meter := servicestest.NewMeter(1 << 20)
	var clock atomic.Int64
	clock.Store(1000)
	started := &atomic.Int64{}
	p := slowRemote{release: make(chan struct{}), started: started}
	reg := services.NewRegistry([]string{"slow"})
	reg.Register(p)
	e := services.NewEngine(services.Config{DB: db, Registry: reg, Meter: meter, Now: clock.Load, HoldsPerAccount: 8})
	t.Cleanup(e.Stop)
	call := func(account, key string) services.Outcome {
		tx, _ := db.Begin()
		out, err := e.Call(context.Background(), tx, services.Request{Service: "slow", Data: `{"schema":1,"method":"go","max_cost":5}`,
			Subject: allowance.Subject{ID: account, KeyID: account, Signed: true}, RequestKey: key}, clock.Load())
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	var wg sync.WaitGroup
	for i := 0; i < services.RemoteSlots; i++ {
		out := call("attacker", fmt.Sprintf("id:a%d", i))
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = out.After() }()
	}
	for started.Load() < services.RemoteSlots {
		time.Sleep(10 * time.Millisecond)
	}
	victim := call("victim", "id:v")
	done := make(chan error, 1)
	go func() { _, err := victim.After(); done <- err }()
	time.Sleep(100 * time.Millisecond)
	clock.Add(25) // most of the hold's 33 s TTL spent waiting for a slot
	p.release <- struct{}{}
	if err := <-done; code(err) != "upstream_busy" {
		t.Fatalf("the late call should be refused as busy: %v", err)
	}
	close(p.release)
	wg.Wait()
	if started.Load() != services.RemoteSlots {
		t.Fatal("the upstream was asked with too little of the hold left")
	}
	var state string
	var cost int64
	if err := db.QueryRow("SELECT state, cost FROM service_calls WHERE account='victim'").Scan(&state, &cost); err != nil || state != "failed" || cost != 0 {
		t.Fatalf("victim call %s cost %d %v", state, cost, err)
	}
	// The reconciler, later, finds nothing of the victim's to charge.
	clock.Add(60)
	if _, err := e.Work(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT state, cost FROM service_calls WHERE account='victim'").Scan(&state, &cost); err != nil || state != "failed" || cost != 0 {
		t.Fatalf("victim call after reconcile %s cost %d %v", state, cost, err)
	}
}
