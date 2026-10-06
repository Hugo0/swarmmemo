package services_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
)

// fakeLeaker stands in for moderation's Jev behind screen.leak.
type fakeLeaker struct {
	mu     sync.Mutex
	scores map[string]float64
	cost   int64
	err    error
	off    bool
	calls  int
	last   [2]string
}

func (f *fakeLeaker) ScreenAvailable(context.Context) bool { return !f.off }

func (f *fakeLeaker) ScreenLeak(_ context.Context, text, audience string) (services.TextScreen, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = [2]string{text, audience}
	if f.err != nil {
		return services.TextScreen{}, f.err
	}
	scores := map[string]float64{"credentials": 0.01, "personal_data": 0.01, "private_infrastructure": 0.01, "excess_code": 0.01}
	for k, v := range f.scores {
		scores[k] = v
	}
	return services.TextScreen{Scores: scores, Model: "jev-1.13.0", CostMicroUSD: f.cost}, nil
}

type leakRig struct {
	db    *sql.DB
	e     *services.Engine
	meter *servicestest.Meter
	now   int64
}

func newLeakRig(t *testing.T, leaker *fakeLeaker, key ed25519.PrivateKey) *leakRig {
	t.Helper()
	r := &leakRig{db: openDB(t), meter: servicestest.NewMeter(1 << 20), now: 1790640000}
	deps := services.Deps{DB: r.db, NotaryKey: key}
	if leaker != nil {
		deps.LeakScreener = leaker
	}
	r.e = services.NewEngine(services.Config{DB: r.db, Registry: services.NewBuiltinRegistry([]string{"screen"}, deps), Meter: r.meter, Now: func() int64 { return r.now }})
	t.Cleanup(r.e.Stop)
	return r
}

// leak makes one screen.leak call as the board does: the call in a
// transaction, then its After once that committed. stored is what the board
// keeps as the retry receipt (the call's data before After).
func (r *leakRig) leak(t *testing.T, s allowance.Subject, args map[string]any, maxCost int64, key string) (out, stored map[string]any, err error) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"schema": 1, "method": "leak", "args": args, "max_cost": maxCost})
	tx, err := r.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	o, err := r.e.Call(context.Background(), tx, services.Request{Service: "screen", Data: string(raw), Subject: s, RequestKey: key}, r.now)
	if err != nil {
		tx.Rollback()
		return nil, nil, err
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	stored = roundTrip(t, o.Data)
	data, err := o.After()
	if err != nil {
		return nil, stored, err
	}
	return roundTrip(t, data), stored, nil
}

func (r *leakRig) read(t *testing.T, method string, args any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"schema": 1, "method": method, "args": args})
	out, err := r.e.Read(context.Background(), r.db, services.Request{Service: "screen", Data: string(raw), Subject: anon("reader")}, r.now)
	if err != nil {
		t.Fatal(err)
	}
	return roundTrip(t, out)
}

func leakCharged(t *testing.T, r *leakRig) (units int64, holds int) {
	t.Helper()
	entries, err := r.meter.Entries(context.Background(), r.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Service == "screen" && e.Kind == "hold" {
			holds++
			if e.State == "committed" {
				units += e.Units
			}
		}
	}
	return units, holds
}

// The patterns alone need no classifier: findings with byte offsets, a
// redacted copy in the first answer only, free (no credit, no classifier), and a
// swarmmemo-leak/1 receipt that screen.verify accepts and any change breaks.
// Neither the text nor the redacted copy is stored anywhere, and a retry
// answers from the call record, which has neither.
func TestLeakPatternsFindRedactAndSign(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	r := newLeakRig(t, nil, key)
	secret := "sk_" + "live_" + strings.Repeat("4eC3", 6)
	text := "canary-6d0b: deploy with key " + secret + " on db.prod.internal"
	out, stored, err := r.leak(t, subjectOf("alice"), map[string]any{"text": text, "audience": "conversation"}, 1, "id:1")
	if err != nil {
		t.Fatal(err)
	}
	findings, _ := get(out, "result", "findings").([]any)
	if get(out, "result", "verdict") != "hold" || get(out, "result", "mode") != "patterns" || get(out, "call", "cost") != float64(0) || len(findings) != 2 {
		t.Fatalf("leak: %+v", out)
	}
	first := findings[0].(map[string]any)
	if first["rule"] != "stripe_live" || first["category"] != "credentials" || text[int(first["start"].(float64)):int(first["end"].(float64))] != secret {
		t.Fatalf("finding: %+v", first)
	}
	if get(out, "result", "redacted") != "canary-6d0b: deploy with key «REDACTED:stripe_live» on «REDACTED:internal_hostname»" {
		t.Fatalf("redacted: %v", get(out, "result", "redacted"))
	}
	if units, holds := leakCharged(t, r); units != 0 || holds != 0 {
		t.Fatalf("charged %d over %d holds", units, holds)
	}
	rec := receiptFrom(t, get(out, "result", "receipt"))
	public := get(r.read(t, "key", map[string]any{}), "result", "public_key").(string)
	p, ok := services.VerifyLeakReceipt(public, rec)
	if !ok || p.Verdict != "hold" || p.FindingsCount != 2 || p.Mode != "patterns" || p.Audience != "conversation" || p.Threshold != 0.6 || p.TextBytes != len(text) || len(p.Categories) != 0 {
		t.Fatalf("receipt: %+v %v", p, ok)
	}
	if _, ok = services.VerifyScreenReceipt(public, rec); ok {
		t.Fatal("a leak receipt passed as a screen receipt")
	}
	v := r.read(t, "verify", map[string]any{"receipt": rec, "text": text})
	if get(v, "result", "valid") != true || get(v, "result", "text_matches") != true || get(v, "result", "screened", "findings_count") != float64(2) {
		t.Fatalf("verify: %+v", v)
	}
	for _, bad := range []func(*services.ScreenReceipt){
		func(x *services.ScreenReceipt) {
			x.Payload = strings.Replace(x.Payload, `"verdict":"hold"`, `"verdict":"pass"`, 1)
		},
		func(x *services.ScreenReceipt) {
			x.Payload = strings.Replace(x.Payload, `"findings_count":2`, `"findings_count":0`, 1)
		},
		func(x *services.ScreenReceipt) { x.Signature = flipFirst(x.Signature) },
		func(x *services.ScreenReceipt) { x.Schema = services.ScreenSchema },
	} {
		tampered := rec
		bad(&tampered)
		if _, ok := services.VerifyLeakReceipt(public, tampered); ok {
			t.Fatalf("a tampered receipt verified: %+v", tampered)
		}
		if get(r.read(t, "verify", map[string]any{"receipt": tampered}), "result", "valid") != false {
			t.Fatalf("verify accepted a tampered receipt: %+v", tampered)
		}
	}
	// The retry receipt and the call record keep the findings, not the copy.
	if get(stored, "result", "redacted") != nil {
		t.Fatalf("the retry receipt holds the redacted copy: %+v", stored)
	}
	again, err := r.e.Retry(context.Background(), r.db, "alice", stored, r.now)
	if err != nil || get(roundTrip(t, again), "result", "redacted") != nil || get(roundTrip(t, again), "result", "verdict") != "hold" {
		t.Fatalf("retry: %+v %v", again, err)
	}
	assertNoPlaintext(t, r.db, "canary-6d0b")
	// Nothing found: pass, free.
	out, _, err = r.leak(t, subjectOf("alice"), map[string]any{"text": "The meeting moved to 3 pm."}, 1, "id:2")
	if err != nil || get(out, "result", "verdict") != "pass" || get(out, "result", "redacted") != "The meeting moved to 3 pm." || len(get(out, "result", "findings").([]any)) != 0 {
		t.Fatalf("clean text: %+v %v", out, err)
	}
	// Contact details and private infrastructure only warn (leakscan.Actions),
	// in the answer and the receipt.
	out, _, err = r.leak(t, subjectOf("alice"), map[string]any{"text": "Mail ana@example.org about db.prod.internal."}, 1, "id:3")
	if err != nil || get(out, "result", "verdict") != "warn" || len(get(out, "result", "findings").([]any)) != 2 {
		t.Fatalf("contact details: %+v %v", out, err)
	}
	if p, ok := services.VerifyLeakReceipt(public, receiptFrom(t, get(out, "result", "receipt"))); !ok || p.Verdict != "warn" {
		t.Fatalf("warn receipt: %+v %v", p, ok)
	}
}

// Mode full adds the classifier's four categories, at screen.text's price:
// a category at the caller's threshold holds; the receipt's verdict is at
// 0.6 whatever the caller asked.
func TestLeakFullAsksTheClassifier(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	jev := &fakeLeaker{scores: map[string]float64{"excess_code": 0.81234}, cost: 42}
	r := newLeakRig(t, jev, key)
	out, _, err := r.leak(t, subjectOf("alice"), map[string]any{"text": "func main() {}", "audience": "public", "mode": "full"}, 1000, "id:1")
	if err != nil {
		t.Fatal(err)
	}
	if get(out, "result", "verdict") != "hold" || get(out, "result", "categories", "excess_code") != 0.8123 || get(out, "result", "classifier_version") != services.ClassifierVersion || strings.Contains(fmt.Sprint(out), "jev-") ||
		get(out, "call", "cost") != float64(47) || get(out, "call", "max_cost") != float64(190) || jev.last != [2]string{"func main() {}", "public"} {
		t.Fatalf("full: %+v", out)
	}
	out, _, err = r.leak(t, subjectOf("alice"), map[string]any{"text": "func main() {}", "mode": "full", "threshold": 0.9}, 1000, "id:2")
	if err != nil || get(out, "result", "verdict") != "pass" {
		t.Fatalf("threshold 0.9: %+v %v", out, err)
	}
	public := get(r.read(t, "key", map[string]any{}), "result", "public_key").(string)
	if p, ok := services.VerifyLeakReceipt(public, receiptFrom(t, get(out, "result", "receipt"))); !ok || p.Verdict != "hold" || p.Threshold != 0.6 || len(p.Categories) != 4 || p.Model != services.ClassifierVersion {
		t.Fatalf("receipt at threshold 0.9: %+v %v", p, ok)
	}
}

// Fails closed: mode full without a classifier that can answer, or with an
// answer out of shape or no usage, fails and charges nothing; the patterns
// still run without one; nothing runs without the notary key.
func TestLeakFailsClosed(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	full := map[string]any{"text": "hello", "mode": "full"}
	for _, jev := range []*fakeLeaker{nil, {off: true, cost: 10}} {
		r := newLeakRig(t, jev, key)
		if _, _, err := r.leak(t, subjectOf("alice"), full, 1000, "id:1"); code(err) != "upstream_unavailable" {
			t.Fatalf("no classifier: %v", err)
		}
		if _, _, err := r.leak(t, subjectOf("alice"), map[string]any{"text": "hello"}, 1, "id:2"); err != nil {
			t.Fatalf("patterns without a classifier: %v", err)
		}
		cat, _ := r.e.Catalogue(context.Background(), r.db, r.now)
		if modes := cat["services"].([]services.Entry)[0].Extra["leak"].(map[string]any)["modes"]; strings.Join(modes.([]string), ",") != "patterns" {
			t.Fatalf("leak modes listed: %v", modes)
		}
	}
	for i, jev := range []*fakeLeaker{
		{err: errors.New("moderation: jev daily spend cap reached")},
		{scores: map[string]float64{"credentials": 1.5}, cost: 10},
		{cost: 0},
	} {
		r := newLeakRig(t, jev, key)
		out, _, err := r.leak(t, subjectOf("alice"), full, 1000, "id:x")
		want := "upstream_unavailable"
		if i > 0 {
			want = "upstream_failed"
		}
		if code(err) != want || out != nil {
			t.Fatalf("case %d: %v %+v, want %s", i, err, out, want)
		}
		if units, holds := leakCharged(t, r); units != 0 || holds != 1 {
			t.Fatalf("case %d: charged %d", i, units)
		}
	}
	none := newLeakRig(t, &fakeLeaker{cost: 1}, nil)
	if _, _, err := none.leak(t, subjectOf("alice"), map[string]any{"text": "hello"}, 1, "id:1"); code(err) != "upstream_unavailable" {
		t.Fatalf("no notary key: %v", err)
	}
}

// Strict arguments, 16 KiB signed and 2 KiB without a key, and a max_cost
// below the quote refused before anything is reserved.
func TestLeakLimits(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	r := newLeakRig(t, &fakeLeaker{cost: 1 << 30}, key)
	long := strings.Repeat("a", services.ScreenTextBytes)
	out, _, err := r.leak(t, subjectOf("alice"), map[string]any{"text": long, "mode": "full"}, 1<<20, "id:1")
	if err != nil || get(out, "call", "cost") != float64(110+80*16) {
		t.Fatalf("16 KiB full, charged at the ceiling: %+v %v", out, err)
	}
	for i, args := range []map[string]any{
		{"text": long + "a"}, {"text": ""}, {}, {"text": "x", "audience": "everyone"}, {"text": "x", "mode": "deep"},
		{"text": "x", "threshold": 2}, {"text": "x", "source": "web"},
	} {
		if _, _, err := r.leak(t, subjectOf("alice"), args, 1<<20, "id:bad"); code(err) != "invalid_service_data" {
			t.Fatalf("case %d %v: %v", i, args, err)
		}
	}
	if _, _, err := r.leak(t, subjectOf("alice"), map[string]any{"text": "x", "mode": "full"}, 189, "id:cheap"); code(err) != "price_exceeds_max" {
		t.Fatalf("max_cost below the full ceiling: %v", err)
	}
	// The patterns are free: a max_cost of 0 is enough, and nothing is charged.
	if out, _, err := r.leak(t, subjectOf("alice"), map[string]any{"text": "x"}, 0, "id:free"); err != nil || get(out, "call", "cost") != float64(0) {
		t.Fatalf("max_cost 0: %+v %v", out, err)
	}
	if _, _, err := r.leak(t, anon("net"), map[string]any{"text": long[:services.ScreenAnonymousTextBytes+1]}, 1, "id:a1"); code(err) != "screen_text_limit" {
		t.Fatalf("2 KiB + 1 without a key: %v", err)
	}
	if out, _, err = r.leak(t, anon("net"), map[string]any{"text": long[:services.ScreenAnonymousTextBytes]}, 1, "id:a2"); err != nil || get(out, "call", "cost") != float64(0) {
		t.Fatalf("2 KiB without a key: %+v %v", out, err)
	}
}
