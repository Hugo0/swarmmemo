package services_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	_ "modernc.org/sqlite"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
)

func code(err error) string {
	var e *allowance.Err
	if errors.As(err, &e) {
		return e.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func TestParseDataStrict(t *testing.T) {
	good := []struct {
		raw  string
		call bool
	}{
		{`{"schema":1,"method":"put","args":{"key":"a","value":"b"},"max_cost":300}`, true},
		{`{"schema":1,"method":"get","args":{"key":"a"}}`, false},
		{`{"schema":1,"method":"list"}`, false},
		{`{"max_cost":0,"method":"echo","schema":1}`, true},
	}
	for _, g := range good {
		if _, err := services.ParseData(g.raw, g.call); err != nil {
			t.Errorf("%s: %v", g.raw, err)
		}
	}
	bad := []struct {
		raw  string
		call bool
	}{
		{``, false},
		{`[]`, false},
		{`{"schema":1,"method":"get"} {}`, false},
		{`{"schema":1,"method":"get"}x`, false},
		{`{"schema":2,"method":"get"}`, false},
		{`{"schema":1.0,"method":"get"}`, false},
		{`{"schema":"1","method":"get"}`, false},
		{`{"method":"get"}`, false},
		{`{"schema":1}`, false},
		{`{"schema":1,"method":"Get"}`, false},
		{`{"schema":1,"method":"get","args":[]}`, false},
		{`{"schema":1,"method":"get","args":null}`, false},
		{`{"schema":1,"method":"get","args":"x"}`, false},
		{`{"schema":1,"method":"get","extra":1}`, false},
		{`{"schema":1,"method":"get","method":"list"}`, false},
		{`{"schema":1,"method":"get","args":{"key":"a","key":"b"}}`, false},
		{`{"schema":1,"method":"get","args":{"a":{"b":{"c":1,"c":2}}}}`, false},
		{`{"schema":1,"method":"get","max_cost":1}`, false},
		{`{"schema":1,"method":"put"}`, true},
		{`{"schema":1,"method":"put","max_cost":-1}`, true},
		{`{"schema":1,"method":"put","max_cost":1.5}`, true},
		{`{"schema":1,"method":"put","max_cost":1e3}`, true},
		{`{"schema":1,"method":"put","max_cost":"5"}`, true},
		{`{"schema":1,"method":"put","max_cost":01}`, true},
		{`{"schema":1,"method":"put","max_cost":99999999999999999999}`, true},
		{"{\"schema\":1,\"method\":\"get\",\"args\":{\"k\":\"\xff\"}}", false},
		{`{"schema":1,"method":"get","args":` + strings.Repeat(`{"a":`, 20) + `1` + strings.Repeat(`}`, 20) + `}`, false},
	}
	for _, b := range bad {
		if _, err := services.ParseData(b.raw, b.call); code(err) != "invalid_service_data" {
			t.Errorf("%q must be invalid_service_data, got %v", b.raw, err)
		}
	}
}

func FuzzServiceData(f *testing.F) {
	for _, seed := range []string{
		`{"schema":1,"method":"put","args":{"key":"a","value":"b"},"max_cost":300}`,
		`{"schema":1,"method":"get","args":{"key":"a","agent":"` + strings.Repeat("a", 64) + `"}}`,
		`{"schema":1,"method":"echo","args":{"text":"hi","simulate":{"mode":"remote","delay_ms":5}},"max_cost":2}`,
		`{"schema":1,"method":"list","args":{"limit":5,"prefix":"a/"}}`,
		`{"schema":1,"method":"status","args":{"call":"00112233445566778899aabbccddeeff"}}`,
		`{"a":{"a":{"a":[1,2,{"x":null}]}}}`, `{}`, `[`, `{"schema":1,"method":"x","args":{"k":1,"k":2}}`,
	} {
		f.Add(seed, true)
		f.Add(seed, false)
	}
	f.Fuzz(func(t *testing.T, raw string, call bool) {
		d, err := services.ParseData(raw, call)
		if err != nil {
			if code(err) != "invalid_service_data" {
				t.Fatalf("refusal must be invalid_service_data: %v", err)
			}
			return
		}
		if !utf8.ValidString(raw) || !json.Valid([]byte(raw)) || !json.Valid(d.Args) || d.Args[0] != '{' {
			t.Fatalf("accepted malformed data %q", raw)
		}
		if !regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`).MatchString(d.Method) || d.MaxCost < 0 || d.MaxCost > services.MaxCostMax || (!call && d.MaxCost != 0) {
			t.Fatalf("accepted out-of-range data %q: %+v", raw, d)
		}
		// Accepted data parses the same way twice (no hidden state).
		again, err := services.ParseData(raw, call)
		if err != nil || again.Method != d.Method || string(again.Args) != string(d.Args) || again.MaxCost != d.MaxCost {
			t.Fatalf("reparse differs: %+v %+v %v", d, again, err)
		}
		// Every provider's argument parser tolerates any accepted args.
		for _, m := range []string{"put", "delete", "echo", "complete"} {
			_, _ = services.QuoteForTest(m, d.Args)
		}
	})
}

func TestValidMemoryKey(t *testing.T) {
	for _, k := range []string{"a", "notes/2026-09-28", "A.b_c-d/e", strings.Repeat("k", 256), "x.y/z.w"} {
		if !services.ValidMemoryKey(k) {
			t.Errorf("%q must be valid", k)
		}
	}
	for _, k := range []string{"", "../x", "a/../b", "a..b", "/a", "a/", "a//b", ".", "a/./b", "a b", "a%2Fb", "é", "a\x00", strings.Repeat("k", 257), "a?b", "a#b", "a\\b"} {
		if services.ValidMemoryKey(k) {
			t.Errorf("%q must be invalid", k)
		}
	}
}

func FuzzMemoryKey(f *testing.F) {
	for _, s := range []string{"a", "../x", "a/b/c", "a//b", ".", "a/./b", "x..y", strings.Repeat("z", 256), "%2e%2e"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, k string) {
		if !services.ValidMemoryKey(k) {
			return
		}
		if len(k) == 0 || len(k) > services.MemoryKeyBytes || strings.Contains(k, "..") {
			t.Fatalf("accepted %q", k)
		}
		for _, r := range k {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._/-", r)) {
				t.Fatalf("accepted %q with %q", k, r)
			}
		}
		// A valid key survives a URL path unchanged: GET /api/memory/AGENT/KEY
		// reads exactly the key, whatever normalises the path on the way.
		p := "/api/memory/" + strings.Repeat("a", 64) + "/" + k
		if path.Clean(p) != p {
			t.Fatalf("%q changes under path cleaning", k)
		}
		u, err := url.Parse(p)
		if err != nil || u.Path != p {
			t.Fatalf("%q changes in a URL: %v", k, err)
		}
	})
}

func TestParamsStrict(t *testing.T) {
	body := services.DefaultParamsBody()
	prices, err := services.ParseParams(body)
	if err != nil {
		t.Fatalf("version 0 must parse: %s %v", body, err)
	}
	if p := prices["memory.put"]; p.For(10) != 266 {
		t.Fatalf("memory put price: %+v", p)
	}
	if p := prices["echo.echo"]; p.For(1025) != 3 || p.For(0) != 1 {
		t.Fatalf("echo price: %+v", p)
	}
	for _, raw := range []string{
		`{"schema":1,"prices":{"memory.get":{"base":1}}}`,
		`{"schema":1,"prices":{"nope.put":{"base":1}}}`,
		`{"schema":1,"prices":{"memory.put":{"base":-1}}}`,
		`{"schema":1,"prices":{"memory.put":{"base":4294967297}}}`,
		`{"schema":1,"prices":{"memory.put":{"base":1,"extra":2}}}`,
		`{"schema":2,"prices":{}}`,
		`{"schema":1}`,
	} {
		if _, err := services.ParseParams([]byte(raw)); err == nil {
			t.Errorf("%s must be refused", raw)
		}
	}
}

// probe is a provider defined entirely in this test: plugging it into a
// registry is the whole integration, which is the point.
type probe struct{}

func (probe) Describe() services.Descriptor {
	return services.Descriptor{ID: "probe", Summary: "test", Mode: services.Local, Methods: []services.Method{
		{Name: "ping", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: 64, Price: services.Price{Base: 3}},
		{Name: "peek", ArgsMax: 64},
	}}
}
func (probe) Quote(c services.Call) (services.Quote, error) {
	return services.Quote{Resource: allowance.Credit, Max: c.Price.For(0)}, nil
}
func (probe) Run(ctx context.Context, tx *sql.Tx, c services.Call) (services.Result, error) {
	return services.Result{Body: json.RawMessage(`{"pong":true}`), Used: 2}, nil
}
func (probe) Read(ctx context.Context, q allowance.Querier, c services.Call) (json.RawMessage, error) {
	return json.RawMessage(`{"peeked":true}`), nil
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err = db.Exec(services.Schema); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(services.Schema); err != nil {
		t.Fatalf("schema must be idempotent: %v", err)
	}
	return db
}

func TestNewProviderPlugsIn(t *testing.T) {
	db := openDB(t)
	reg := services.NewRegistry([]string{"probe"})
	reg.Register(probe{})
	meter := servicestest.NewMeter(100)
	e := services.NewEngine(services.Config{DB: db, Registry: reg, Meter: meter, Now: func() int64 { return 1000 }})
	ctx := context.Background()
	cat, err := e.Catalogue(ctx, db, 1000)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(cat)
	if !strings.Contains(string(raw), `"id":"probe"`) || !strings.Contains(string(raw), `"base":3`) {
		t.Fatalf("catalogue lacks the new provider: %s", raw)
	}
	subject := allowance.Subject{ID: "acct", KeyID: "k", Signed: true}
	tx, _ := db.Begin()
	out, err := e.Call(ctx, tx, services.Request{Service: "probe", Data: `{"schema":1,"method":"ping","max_cost":3}`, Subject: subject, RequestKey: "id:1"}, 1000)
	if err != nil || out.After != nil {
		t.Fatalf("local call: %+v %v", out, err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	entries, _ := meter.Entries(ctx, db)
	if len(entries) != 1 || entries[0].Units != 2 || entries[0].Service != "probe" || entries[0].Method != "ping" {
		t.Fatalf("the ledger must hold one receipt for the used units: %+v", entries)
	}
	got, err := e.Read(ctx, db, services.Request{Service: "probe", Data: `{"schema":1,"method":"peek"}`, Subject: allowance.Subject{ID: "anon:x"}}, 1000)
	if err != nil || string(got["result"].(json.RawMessage)) != `{"peeked":true}` {
		t.Fatalf("read: %v %v", got, err)
	}
	if _, err = e.Read(ctx, db, services.Request{Service: "probe", Data: `{"schema":1,"method":"ping"}`, Subject: subject}, 1000); code(err) != "invalid_service_data" {
		t.Fatalf("a write method is not readable: %v", err)
	}
	if _, err = e.Read(ctx, db, services.Request{Service: "memory", Data: `{"schema":1,"method":"get"}`, Subject: subject}, 1000); code(err) != "invalid_service" {
		t.Fatalf("an unregistered service: %v", err)
	}
}

func TestBuiltinsListedAndKnown(t *testing.T) {
	if got := strings.Join(services.Known(), ","); got != "echo,fetch,inference,memory,notary,public_data,receiver,runs,screen,wakeup,x402" {
		t.Fatalf("Known = %s", got)
	}
	reg := services.NewBuiltinRegistry([]string{"memory"}, services.Deps{})
	if got := reg.Enabled(); len(got) != 1 || got[0] != "memory" {
		t.Fatalf("only enabled providers are listed: %v", got)
	}
	if _, err := reg.Lookup("echo"); code(err) != "invalid_service" {
		t.Fatalf("a disabled provider cannot be called: %v", err)
	}
	if !strings.Contains(services.Schema, "service_calls") || !strings.Contains(services.Schema, "memory_items") || !strings.Contains(services.Schema, "memory_usage") {
		t.Fatal("a provider's own tables must join Schema")
	}
}
