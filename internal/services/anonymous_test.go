package services_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
)

// Service calls without a key: only methods the catalogue marks anonymous
// take one, only from an anonymous subject with a request_id, and each is
// narrowed and rate-limited per network and for every network together.

func anon(id string) allowance.Subject { return allowance.Subject{ID: "anon:" + id} }

type anonEnv struct {
	db    *sql.DB
	e     *services.Engine
	meter *servicestest.Meter
	now   int64
}

func newAnonEnv(t *testing.T, enabled []string, extra ...services.Provider) *anonEnv {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	v := &anonEnv{db: openDB(t), meter: servicestest.NewMeter(1 << 20), now: 1790640000}
	reg := services.NewBuiltinRegistry(enabled, services.Deps{DB: v.db, NotaryKey: key})
	for _, p := range extra {
		reg.Register(p)
	}
	v.e = services.NewEngine(services.Config{DB: v.db, Registry: reg, Meter: v.meter, Now: func() int64 { return v.now }})
	t.Cleanup(v.e.Stop)
	return v
}

// call runs one service.call in its own transaction (not its after-commit
// part) and returns the answer.
func (v *anonEnv) call(t *testing.T, s allowance.Subject, service, method, args string, maxCost int64, key string) (services.Outcome, error) {
	t.Helper()
	tx, err := v.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"schema":1,"method":%q,"args":%s,"max_cost":%d}`, method, args, maxCost)
	out, err := v.e.Call(context.Background(), tx, services.Request{Service: service, Data: data, Subject: s, RequestKey: key}, v.now)
	if err != nil {
		tx.Rollback()
		return out, err
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return out, nil
}

func TestAnonymousAllowlistIsTheCatalogue(t *testing.T) {
	all := services.Catalog(services.Known())
	got := services.AnonymousMethods(all)
	want := []string{"notary.stamp", "inference.complete", "public_data.fetch", "public_data.bulk"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("methods without a key: %v, want %v", got, want)
	}
	for _, e := range all {
		for _, m := range e.Methods {
			if m.Anonymous && (!m.Write() || m.Resource != string(allowance.Credit) || m.AnonymousNote == "" || m.AnonymousRate == nil || m.AnonymousRate.AllPerDay <= 0 || m.AnonymousRate.AllPerMinute <= 0) {
				t.Errorf("%s.%s: an anonymous method is a credit write with a note and a rate for every network together: %+v", e.ID, m.Name, m)
			}
			if m.Anonymous != (m.Access() == "service.call, signed or no key") {
				t.Errorf("%s.%s: access %q", e.ID, m.Name, m.Access())
			}
		}
	}
	if line := services.NoKeyLine(all, 2000); line != "No key needed for the notary, small-model inference and public data: 2,000 credits a day per network." {
		t.Fatalf("line: %q", line)
	}
	if ex := services.NoKeyExample("https://swarmmemo.com", all); ex != "https://swarmmemo.com/call/public_data/fetch?dataset=sea_ice_extent&max_cost=5&request_id=RANDOM_16_CHARS" {
		t.Fatalf("example: %q", ex)
	}
	on := services.NoKeyFor("", all, 2000, 160000, "")
	off := services.NoKeyFor("", all, 2000, 160000, "paused by a public lever")
	zero := services.NoKeyFor("", all, 0, 0, "")
	if !on.Available || on.Line == "" || off.Available || off.Line != "" || off.Example != "" || zero.Available || zero.Why == "" {
		t.Fatalf("availability: %+v %+v %+v", on, off, zero)
	}
}

func TestAnonymousCallsOnlyTheAllowlist(t *testing.T) {
	v := newAnonEnv(t, []string{"notary", "memory", "echo"})
	a := anon("net-a")
	if _, err := v.call(t, a, "memory", "put", `{"key":"k","value":"v"}`, 1000, "id:m1"); code(err) != "anonymous_not_allowed" {
		t.Fatalf("memory put without a key: %v", err)
	}
	if _, err := v.call(t, a, "echo", "echo", `{"text":"hi"}`, 100, "id:e1"); code(err) != "anonymous_not_allowed" {
		t.Fatalf("echo without a key: %v", err)
	}
	// Only an anonymous subject, and only with a request_id as its key.
	if _, err := v.call(t, allowance.Subject{ID: "acct"}, "notary", "stamp", `{"text":"x"}`, 1, "id:n0"); code(err) != "signature_required" {
		t.Fatalf("an unsigned non-anonymous subject: %v", err)
	}
	if _, err := v.call(t, a, "notary", "stamp", `{"text":"x"}`, 1, "nonce:n0"); code(err) != "signature_required" {
		t.Fatalf("an unsigned call keyed by a nonce: %v", err)
	}
	// max_cost below the price refuses and spends nothing.
	if _, err := v.call(t, a, "notary", "stamp", `{"text":"x"}`, 0, "id:n1"); code(err) != "price_exceeds_max" {
		t.Fatalf("max_cost 0: %v", err)
	}
	out, err := v.call(t, a, "notary", "stamp", `{"text":"x"}`, 1, "id:n2")
	if err != nil {
		t.Fatal(err)
	}
	rec := out.Data["call"].(services.CallRecord)
	if rec.Cost != 1 || rec.State != "done" {
		t.Fatalf("stamp without a key: %+v", rec)
	}
	entries, err := v.meter.Entries(context.Background(), v.db)
	if err != nil || len(entries) != 1 || entries[0].Account != "anon:net-a" || entries[0].Units != 1 {
		t.Fatalf("billed to the network's pseudonym, once: %+v %v", entries, err)
	}
	// A read that needs a key still needs one.
	tx, _ := v.db.Begin()
	defer tx.Rollback()
	if _, err := v.e.Read(context.Background(), tx, services.Request{Service: "memory", Data: `{"schema":1,"method":"list","args":{}}`, Subject: a}, v.now); code(err) == "" {
		t.Fatal("a private memory list without a key must be refused")
	}
}

func TestAnonymousNotaryPerNetworkDay(t *testing.T) {
	v := newAnonEnv(t, []string{"notary"})
	a := anon("net-a")
	for i := 0; i < services.NotaryPerAnonymousDay; i++ {
		v.now += 7 // under the notary's per-network rate (10 a minute)
		if _, err := v.call(t, a, "notary", "stamp", fmt.Sprintf(`{"text":"t%d"}`, i), 1, fmt.Sprintf("id:%d", i)); err != nil {
			t.Fatalf("stamp %d: %v", i, err)
		}
	}
	if _, err := v.call(t, a, "notary", "stamp", `{"text":"one more"}`, 1, "id:x"); code(err) != "notary_limit" {
		t.Fatalf("stamp past the network's day: %v", err)
	}
	if _, err := v.call(t, anon("net-b"), "notary", "stamp", `{"text":"one more"}`, 1, "id:x"); err != nil {
		t.Fatalf("another network: %v", err)
	}
}

func TestAnonymousRateForEveryNetwork(t *testing.T) {
	v := newAnonEnv(t, []string{"notary"})
	desc := services.Catalog([]string{"notary"})[0].Methods[0]
	perMinute := desc.AnonymousRate.AllPerMinute
	// Every network together: many networks, one call each, in one minute.
	for i := int64(0); i < perMinute; i++ {
		if _, err := v.call(t, anon(fmt.Sprintf("n%d", i)), "notary", "stamp", fmt.Sprintf(`{"text":"r%d"}`, i), 1, "id:1"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := v.call(t, anon("late"), "notary", "stamp", `{"text":"late"}`, 1, "id:1"); code(err) != services.RefusalAnonymousRateAll {
		t.Fatalf("past the per-minute bound for every network: %v", err)
	}
	// Signed callers are not counted against it.
	if _, err := v.call(t, allowance.Subject{ID: "acct", KeyID: "k", Signed: true}, "notary", "stamp", `{"text":"signed"}`, 1, "nonce:s1"); err != nil {
		t.Fatalf("signed call: %v", err)
	}
	v.now += 60
	if _, err := v.call(t, anon("late"), "notary", "stamp", `{"text":"late"}`, 1, "id:1"); err != nil {
		t.Fatalf("next minute: %v", err)
	}
}

// heldRemote is a remote test provider with one anonymous method whose run
// never happens (the test does not run the after-commit part), so every
// call stays running and holds a slot.
type heldRemote struct{}

func (heldRemote) Describe() services.Descriptor {
	return services.Descriptor{ID: "slow", Title: "Slow", Mode: services.Remote, MaxDuration: time.Minute, Methods: []services.Method{{
		Name: "go", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: 64, Price: services.Price{Base: 1},
		Anonymous: true, AnonymousLabel: "slow things", AnonymousNote: "none", AnonymousRate: services.AnonRate{AllPerMinute: 1000, AllPerDay: 1000},
	}}}
}
func (heldRemote) Quote(c services.Call) (services.Quote, error) {
	return services.Quote{Resource: allowance.Credit, Max: 1}, nil
}
func (heldRemote) Run(context.Context, *sql.Tx, services.Call) (services.Result, error) {
	return services.Result{Body: json.RawMessage(`{}`), Used: 1}, nil
}

func TestAnonymousRunningCallsAreBounded(t *testing.T) {
	v := newAnonEnv(t, []string{"slow"}, heldRemote{})
	for i := 0; i < services.AnonymousHoldsTotal; i++ {
		if _, err := v.call(t, anon(fmt.Sprintf("n%d", i)), "slow", "go", `{}`, 1, "id:1"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := v.call(t, anon("one-more"), "slow", "go", `{}`, 1, "id:1"); code(err) != "hold_limit" {
		t.Fatalf("unsigned calls past their share of open calls: %v", err)
	}
	if _, err := v.call(t, allowance.Subject{ID: "acct", KeyID: "k", Signed: true}, "slow", "go", `{}`, 1, "nonce:1"); err != nil {
		t.Fatalf("a signed call still finds a slot: %v", err)
	}
}

func TestAnonymousInferenceIsNarrowAndScreened(t *testing.T) {
	v := newInfEnv(t, [3]int64{100000, 100000, 100000})
	a := anon("net-a")
	n := 0
	call := func(args string) error {
		n++
		tx, err := v.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		data := fmt.Sprintf(`{"schema":1,"method":"complete","args":%s,"max_cost":1000}`, args)
		out, err := v.e.Call(context.Background(), tx, services.Request{Service: "inference", Data: data, Subject: a, RequestKey: fmt.Sprintf("id:a%d", n)}, 1000)
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		_, err = out.After()
		return err
	}
	// Moderation is not running: an unsigned call fails closed, and nothing
	// reaches an upstream.
	if err := call(smallArgs); code(err) != "anonymous_unscreened" || v.primary.count() != 0 {
		t.Fatalf("without a screener: %v", err)
	}
	s := &screen{}
	v.cfg.Screener = s
	if err := call(`{"model":"cf","messages":[{"role":"user","content":"hi"}],"max_tokens":50}`); code(err) != "anonymous_limit" {
		t.Fatalf("another model without a key: %v", err)
	}
	if err := call(`{"model":"small","messages":[{"role":"user","content":"hi"}],"max_tokens":257}`); code(err) != "anonymous_limit" {
		t.Fatalf("max_tokens above the anonymous bound: %v", err)
	}
	if err := call(smallArgs); err != nil {
		t.Fatalf("small, screened: %v", err)
	}
	s.hidePrompt = true
	if err := call(smallArgs); code(err) != "content_refused" {
		t.Fatalf("a prompt the screen hides: %v", err)
	}
	s.hidePrompt, s.fail = false, true
	if err := call(smallArgs); code(err) != "upstream_unavailable" {
		t.Fatalf("a screen error fails closed: %v", err)
	}
	s.fail = false
	// The per-network rate: 5 a minute (the refused ones above were counted
	// only once they reached the engine's admission).
	var last error
	for i := 0; i < 10 && last == nil; i++ {
		last = call(smallArgs)
	}
	if code(last) != services.RefusalAnonymousRate {
		t.Fatalf("per-network rate: %v", last)
	}
	if v.primary.count() > 5 {
		t.Fatalf("upstream calls without a key: %d", v.primary.count())
	}
}

func TestCallDataFromURLFields(t *testing.T) {
	all := services.Catalog(services.Known())
	for _, id := range services.AnonymousMethods(all) {
		svc, name, _ := strings.Cut(id, ".")
		e, m, ok := services.LookupMethod(all, svc, name)
		if !ok {
			t.Fatal(id)
		}
		path, err := services.CallPath(e, m, "r1")
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		u, err := url.Parse(path)
		if err != nil || u.Path != services.CallPathPrefix+svc+"/"+name {
			t.Fatalf("%s: %q %v", id, path, err)
		}
		data, rid, err := services.CallData(m, u.Query())
		if err != nil || rid != "r1" {
			t.Fatalf("%s: %v %q", id, err, rid)
		}
		d, err := services.ParseData(data, true)
		if err != nil || d.Method != name || d.MaxCost != m.MaxCost() {
			t.Fatalf("%s: data %s: %+v %v", id, data, d, err)
		}
		// The URL carries the example's arguments (an empty params object
		// is left out).
		var got, want map[string]any
		_ = json.Unmarshal(d.Args, &got)
		_ = json.Unmarshal([]byte(services.FillPlaceholders(string(m.Example))), &want)
		for k, v := range want {
			if b, _ := json.Marshal(v); string(b) == "{}" {
				delete(want, k)
			}
		}
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		if string(gb) != string(wb) {
			t.Fatalf("%s: args %s, want %s", id, gb, wb)
		}
	}
	_, fetch, _ := services.LookupMethod(all, "public_data", "fetch")
	_, complete, _ := services.LookupMethod(all, "inference", "complete")
	for _, c := range []struct {
		m     services.MethodEntry
		query string
	}{
		{fetch, "dataset=x"},                                // no max_cost
		{fetch, "dataset=x&max_cost=-1"},                    // negative
		{fetch, "dataset=x&max_cost=1.5"},                   // fraction
		{fetch, "dataset=x&dataset=y&max_cost=1"},           // repeated
		{fetch, "dataset=x&nope=1&max_cost=1"},              // unknown argument
		{fetch, "dataset=x&params={bad&max_cost=1"},         // params not JSON
		{complete, "model=small&max_tokens=1e3&max_cost=1"}, // integer as float
		{complete, "model=small&temperature=abc&max_cost=1"},
	} {
		q, _ := url.ParseQuery(c.query)
		if _, _, err := services.CallData(c.m, q); err == nil {
			t.Errorf("%s %q: accepted", c.m.Name, c.query)
		}
	}
	q := url.Values{"model": {"small"}, "messages": {`[{"role":"user","content":"hi & bye"}]`}, "max_tokens": {"64"}, "max_cost": {"300"}, "request_id": {"x"}}
	q, _ = url.ParseQuery(q.Encode()) // as a URL carries it
	data, _, err := services.CallData(complete, q)
	if err != nil || data != `{"schema":1,"method":"complete","args":{"model":"small","messages":[{"role":"user","content":"hi & bye"}],"max_tokens":64},"max_cost":300}` {
		t.Fatalf("inference from a URL: %s %v", data, err)
	}
}

func TestThousands(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 2000: "2,000", 160000: "160,000", 1600000: "1,600,000", -1234: "-1,234"} {
		if got := services.Thousands(n); got != want {
			t.Errorf("%d: %q", n, got)
		}
	}
}

// Any URL query becomes either a refusal or a data field the strict parser
// accepts, naming the method in the path.
func FuzzCallData(f *testing.F) {
	all := services.Catalog(services.Known())
	for _, q := range []string{"dataset=sea_ice_extent&max_cost=5&request_id=r", "model=small&messages=%5B%5D&max_tokens=9&max_cost=1", "text=a&max_cost=1", "requests=[{}]&max_cost=2", "max_cost=99999999999999999999"} {
		f.Add(q)
	}
	f.Fuzz(func(t *testing.T, query string) {
		fields, err := url.ParseQuery(query)
		if err != nil {
			return
		}
		for _, id := range services.AnonymousMethods(all) {
			svc, name, _ := strings.Cut(id, ".")
			_, m, _ := services.LookupMethod(all, svc, name)
			data, _, err := services.CallData(m, fields)
			if err != nil {
				continue
			}
			if d, err := services.ParseData(data, true); err != nil || d.Method != name {
				t.Fatalf("%s: %q gave data %s the parser refuses: %v", id, query, data, err)
			}
		}
	})
}
