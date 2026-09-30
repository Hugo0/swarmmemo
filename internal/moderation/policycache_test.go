package moderation

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// A parameter store that cannot be read on a refresh keeps the previous
// (stored) policy: it never falls through to the file or version 0, which
// would silently loosen it. It alerts once, and a later good read wins.
func TestPolicyParamsReadErrorKeepsPrevious(t *testing.T) {
	v := newEnv(t, `{"schema":1,"version":1}`)
	o := v.opts
	var fail atomic.Bool
	var version atomic.Int64
	version.Store(7)
	o.Params = paramsFunc(func(ns string) (int64, []byte, error) {
		if fail.Load() {
			return 0, nil, errors.New("database is locked")
		}
		return version.Load(), []byte(`{"schema":1}`), nil
	})
	e, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if p := e.Policy(ctx); p.Version != 7 || p.Source != "params" {
		t.Fatalf("start: %+v", p)
	}
	fail.Store(true)
	for i := 0; i < 2; i++ {
		v.clock.add(policyReloadEvery + time.Second)
		if err := e.ReloadPolicy(ctx); err == nil {
			t.Fatal("a failed params read reloaded without an error")
		}
		if p := e.Policy(ctx); p.Version != 7 || p.Source != "params" {
			t.Fatalf("after a failed read the policy is %+v, want params v7", p)
		}
	}
	if k := v.alertKinds(); len(k) != 1 || k[0] != "policy" {
		t.Fatalf("alerts %v", k)
	}
	fail.Store(false)
	version.Store(8)
	if err := e.ReloadPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	if p := e.Policy(ctx); p.Version != 8 {
		t.Fatalf("after recovery: %+v", p)
	}
}

// Readers never read the parameter store: however stale the policy, however
// slow the store, ScreenAvailable, Policy and Screen answer from memory. A
// request path may hold the only database connection in its transaction.
func TestPolicyReadersDoNoIO(t *testing.T) {
	v := newEnv(t, "")
	o := v.opts
	var reads atomic.Int64
	o.Params = paramsFunc(func(ns string) (int64, []byte, error) {
		reads.Add(1)
		return 0, nil, nil
	})
	e, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	start := reads.Load()
	v.clock.add(10 * policyReloadEvery)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a reader that needed I/O would fail on this context
	if !e.ScreenAvailable(ctx) || e.Policy(ctx) == nil {
		t.Fatal("stale policy not served from memory")
	}
	if n := reads.Load() - start; n != 0 {
		t.Fatalf("readers read the parameter store %d times", n)
	}
}

// A new stored version takes effect on a refresh (Start's goroutine, or
// ReloadPolicy after a params set), not on a read.
func TestPolicyChangesOnlyOnRefresh(t *testing.T) {
	v := newEnv(t, "")
	o := v.opts
	var version atomic.Int64
	version.Store(3)
	o.Params = paramsFunc(func(ns string) (int64, []byte, error) { return version.Load(), []byte(`{"schema":1}`), nil })
	e, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	version.Store(4)
	v.clock.add(policyReloadEvery + time.Second)
	if e.Policy(context.Background()).Version != 3 {
		t.Fatal("policy changed without a refresh")
	}
	if err := e.ReloadPolicy(context.Background()); err != nil || e.Policy(context.Background()).Version != 4 {
		t.Fatalf("reload: %v %d", err, e.Policy(context.Background()).Version)
	}
}
