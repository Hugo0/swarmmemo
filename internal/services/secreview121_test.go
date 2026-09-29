package services_test

// Security review 1.21.0: regression tests for service calls without a key
// in the engine and the providers.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
)

// M2: a call refused after admission (the ledger's quota_exhausted) gives
// its place in the anonymous windows back, so one network whose share is
// spent cannot fill the windows every network shares with free refused
// calls; and the notary bounds each network, checked before the shared
// window.
func TestSecReview121RefusedCallsFreeTheAnonymousWindows(t *testing.T) {
	v := newAnonEnv(t, []string{"notary"})
	v.meter.Budget[allowance.Credit] = 3
	_, stamp, _ := services.LookupMethod(services.Catalog([]string{"notary"}), "notary", "stamp")
	all := stamp.AnonymousRate.AllPerMinute
	if stamp.AnonymousRate.CallerPerMinute <= 0 || stamp.AnonymousRate.CallerPerDay <= 0 {
		t.Fatalf("notary.stamp has no per-network bound: %+v", stamp.AnonymousRate)
	}
	// All in one minute: past its share, every call is refused by the
	// ledger, and none of those stays counted, in its own window (it never
	// reaches request_rate) or the shared one.
	hog := anon("hog")
	for i := int64(0); i < 2*all; i++ {
		_, err := v.call(t, hog, "notary", "stamp", fmt.Sprintf(`{"text":"hog %d"}`, i), 1, fmt.Sprintf("id:hog-%d", i))
		switch {
		case i < 3 && err != nil:
			t.Fatalf("call %d: %v", i, err)
		case i >= 3 && code(err) != "quota_exhausted":
			t.Fatalf("call %d: %v, want quota_exhausted", i, err)
		}
	}
	v.meter.Budget[allowance.Credit] = 1 << 20
	if _, err := v.call(t, anon("victim"), "notary", "stamp", `{"text":"victim"}`, 1, "id:v"); err != nil {
		t.Fatalf("another network after %d refused calls: %v", 2*all-3, err)
	}
}

func TestSecReview121AnonymousWindowsPerNetworkFirst(t *testing.T) {
	v := newAnonEnv(t, []string{"notary"})
	_, stamp, _ := services.LookupMethod(services.Catalog([]string{"notary"}), "notary", "stamp")
	r := stamp.AnonymousRate
	// One network: its own bound, in its own words.
	busy := anon("busy")
	for i := int64(0); i < r.CallerPerMinute; i++ {
		if _, err := v.call(t, busy, "notary", "stamp", fmt.Sprintf(`{"text":"b%d"}`, i), 1, fmt.Sprintf("id:b%d", i)); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := v.call(t, busy, "notary", "stamp", `{"text":"one more"}`, 1, "id:bx"); code(err) != services.RefusalAnonymousRate {
		t.Fatalf("past the network's bound: %v", err)
	}
	// Every network together: the shared bound, in its own words; a network
	// past its own bound hears about its own.
	n := r.CallerPerMinute
	for net := 0; n < r.AllPerMinute; net++ {
		for i := int64(0); i < r.CallerPerMinute && n < r.AllPerMinute; i++ {
			if _, err := v.call(t, anon(fmt.Sprint("net-", net)), "notary", "stamp", fmt.Sprintf(`{"text":"n%d-%d"}`, net, i), 1, fmt.Sprintf("id:%d-%d", net, i)); err != nil {
				t.Fatalf("network %d call %d: %v", net, i, err)
			}
			n++
		}
	}
	if _, err := v.call(t, anon("late"), "notary", "stamp", `{"text":"late"}`, 1, "id:late"); code(err) != services.RefusalAnonymousRateAll {
		t.Fatalf("past the shared bound: %v", err)
	}
	if _, err := v.call(t, busy, "notary", "stamp", `{"text":"busy again"}`, 1, "id:by"); code(err) != services.RefusalAnonymousRate {
		t.Fatalf("a network past its own bound while the shared one is full: %v", err)
	}
}

// openWrite is a Local provider whose one write method is neither Signed nor
// Anonymous.
type openWrite struct{}

func (openWrite) Describe() services.Descriptor {
	return services.Descriptor{ID: "open", Title: "Open", Mode: services.Local, Methods: []services.Method{{
		Name: "go", Write: true, Resource: allowance.Credit, ArgsMax: 64, Price: services.Price{Base: 1},
	}}}
}
func (openWrite) Quote(services.Call) (services.Quote, error) {
	return services.Quote{Resource: allowance.Credit, Max: 1}, nil
}
func (openWrite) Run(context.Context, *sql.Tx, services.Call) (services.Result, error) {
	return services.Result{Body: json.RawMessage(`{}`), Used: 1}, nil
}

// L7: only a method the catalogue marks Anonymous takes an unsigned call,
// whether or not it is marked Signed.
func TestSecReview121UnsignedWriteNeedsAnonymous(t *testing.T) {
	v := newAnonEnv(t, []string{"open"}, openWrite{})
	if _, err := v.call(t, anon("net"), "open", "go", `{}`, 1, "id:open-1"); code(err) != "anonymous_not_allowed" {
		t.Fatalf("an unsigned call of a write that is not anonymous: %v", err)
	}
	if _, err := v.call(t, allowance.Subject{ID: "acct", KeyID: "k", Signed: true}, "open", "go", `{}`, 1, "nonce:1"); err != nil {
		t.Fatalf("signed: %v", err)
	}
}

// L2: an unsigned inference call takes at most 2 KiB of message text.
func TestSecReview121AnonymousInferencePromptBound(t *testing.T) {
	v := newInfEnv(t, [3]int64{100000, 100000, 100000})
	v.cfg.Screener = &screen{}
	call := func(content string, s allowance.Subject, key string) error {
		args, _ := json.Marshal(map[string]any{"model": "small", "messages": []map[string]string{{"role": "user", "content": content}}, "max_tokens": 50})
		tx, err := v.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		_, err = v.e.Call(context.Background(), tx, services.Request{Service: "inference", Data: fmt.Sprintf(`{"schema":1,"method":"complete","args":%s,"max_cost":100000}`, args), Subject: s, RequestKey: key}, 1000)
		return err
	}
	long := strings.Repeat("a", services.InferenceAnonymousPromptBytes+1)
	if err := call(long, anon("net"), "id:long"); code(err) != "anonymous_limit" {
		t.Fatalf("a %d-byte prompt without a key: %v", len(long), err)
	}
	if err := call(long[:services.InferenceAnonymousPromptBytes], anon("net"), "id:fits"); err != nil {
		t.Fatalf("a prompt at the bound: %v", err)
	}
	if err := call(long, allowance.Subject{ID: "acct", KeyID: "k", Signed: true}, "nonce:1"); err != nil {
		t.Fatalf("a signed call keeps the full prompt bound: %v", err)
	}
}

// outputDown is a screener that answers every prompt and cannot screen any
// output (the classifier went down between the two).
type outputDown struct{ seen []services.ScreenInput }

func (s *outputDown) Screen(_ context.Context, in services.ScreenInput) (services.ScreenVerdict, error) {
	s.seen = append(s.seen, in)
	if in.Stage == "output" {
		return services.ScreenVerdict{}, errors.New("screen down")
	}
	return services.ScreenVerdict{}, nil
}

// L1: an unsigned call's output is never returned unscreened: refused as
// anonymous_unscreened and refunded; the screener is told the call is
// unsigned, so moderation fails it closed too.
func TestSecReview121AnonymousOutputFailsClosed(t *testing.T) {
	v := newInfEnv(t, [3]int64{100000, 100000, 100000})
	s := &outputDown{}
	v.cfg.Screener = s
	tx, err := v.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	out, err := v.e.Call(context.Background(), tx, services.Request{Service: "inference", Data: `{"schema":1,"method":"complete","args":` + smallArgs + `,"max_cost":1000}`, Subject: anon("net"), RequestKey: "id:out-1"}, 1000)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = out.After(); code(err) != "anonymous_unscreened" {
		t.Fatalf("an unsigned call whose output could not be screened: %v", err)
	}
	for _, in := range s.seen {
		if in.Signed {
			t.Fatalf("the screener was told an unsigned call is signed: %+v", in)
		}
	}
	entries, err := v.meter.Entries(context.Background(), v.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Account == "anon:net" && (e.Kind == "spend" || e.State == "committed") {
			t.Fatalf("an unscreened output was charged: %+v", entries)
		}
	}
}
