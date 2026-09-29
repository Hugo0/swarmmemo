package services_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
)

// testKey is the upstream key the fakes are configured with. No test may
// find it in an error, a record, a table or the log.
const testKey = "sk-test-SECRET-4f9a2c77e1"

// fakeUpstream is an httptest stand-in for an inference upstream whose
// behaviour a test switches with setMode.
type fakeUpstream struct {
	srv      *httptest.Server
	workers  bool
	name     string
	redirect string

	mu     sync.Mutex
	mode   string
	hits   int
	auth   []string
	bodies []map[string]any
	paths  []string
}

func newFakeUpstream(t *testing.T, name string, workers bool) *fakeUpstream {
	f := &fakeUpstream{name: name, workers: workers, mode: "ok"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) setMode(m string) {
	f.mu.Lock()
	f.mode = m
	f.mu.Unlock()
}

func (f *fakeUpstream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

func (f *fakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.hits++
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.bodies = append(f.bodies, body)
	f.paths = append(f.paths, r.URL.Path)
	mode := f.mode
	f.mu.Unlock()
	auth := r.Header.Get("Authorization")
	// Every failure body echoes the key it was sent, as a careless upstream
	// might: none of it may reach a record or an error.
	leak := `{"error":{"message":"bad things with ` + auth + `"}}`
	switch mode {
	case "ok", "echo_key", "no_usage":
		content := "hello from " + f.name
		if mode == "echo_key" {
			content = "your key is " + strings.TrimPrefix(auth, "Bearer ") + " ok"
		}
		usage := `,"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}`
		if mode == "no_usage" {
			usage = ""
		}
		c, _ := json.Marshal(content)
		if f.workers {
			fmt.Fprintf(w, `{"success":true,"errors":[],"messages":[],"result":{"response":%s%s}}`, c, usage)
			return
		}
		fmt.Fprintf(w, `{"id":"cmpl-1","object":"chat.completion","model":"whatever","choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}]%s}`, c, usage)
	case "escapes", "control":
		// Replies at the output cap that JSON escaping inflates: quotes and
		// newlines double, control characters grow sixfold.
		unit := "\"x\n"
		if mode == "control" {
			unit = "\x01"
		}
		c, _ := json.Marshal(strings.Repeat(unit, services.InferenceOutputBytes/len(unit)))
		fmt.Fprintf(w, `{"id":"cmpl-1","object":"chat.completion","model":"whatever","choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"length"}],"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}}`, c)
	case "timeout":
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	case "oversized":
		fmt.Fprint(w, `{"choices":[{"message":{"content":"`+strings.Repeat("a", services.InferenceResponseBytes)+`"}}]}`)
	case "malformed":
		fmt.Fprint(w, `{"choices":[{"message":{"content":"half`)
	case "redirect":
		http.Redirect(w, r, f.redirect, http.StatusFound)
	default: // an HTTP status code
		var code int
		fmt.Sscanf(mode, "%d", &code)
		w.WriteHeader(code)
		fmt.Fprint(w, leak)
	}
}

type infEnv struct {
	db                   *sql.DB
	e                    *services.Engine
	meter                *servicestest.Meter
	cfg                  *services.InferenceConfig
	primary, backup, cf  *fakeUpstream
	keyFile, missingFile string
	n                    int
}

// inferenceConfig names primary (timeout 1 s) then backup under alias
// "small", and a workers_ai upstream under alias "cf".
func inferenceConfig(primary, backup, cf, keyFile, cfKey string, caps [3]int64) string {
	return fmt.Sprintf(`{"schema":1,
 "upstreams":[
  {"name":"primary","kind":"openai_compat","base_url":%q,"key_file":%q,"timeout_ms":1000,"daily_spend_cap":%d,
   "models":{"m1":{"price":{"base":1,"input_per_mtok":1000000,"output_per_mtok":2000000},"max_output_tokens":100}},
   "extra":{"provider":{"zdr":true}}},
  {"name":"backup","kind":"openai_compat","base_url":%q,"key_file":%q,"timeout_ms":2000,"daily_spend_cap":%d,
   "models":{"org/m2":{"price":{"base":2,"input_per_mtok":1000000,"output_per_mtok":1000000},"max_output_tokens":100}}},
  {"name":"cf","kind":"workers_ai","base_url":%q,"account_id":"abc123","key_file":%q,"daily_spend_cap":%d,
   "models":{"@cf/meta/llama-3.1-8b-instruct":{"price":{"base":1,"input_per_mtok":0,"output_per_mtok":0},"max_output_tokens":64}}}
 ],
 "models":{"small":[{"upstream":"primary","model":"m1"},{"upstream":"backup","model":"org/m2"}],
           "cf":[{"upstream":"cf","model":"@cf/meta/llama-3.1-8b-instruct"}]}}`,
		primary, keyFile, caps[0], backup, keyFile, caps[1], cf, cfKey, caps[2])
}

func newInfEnv(t *testing.T, caps [3]int64) *infEnv {
	t.Helper()
	v := &infEnv{db: openDB(t), meter: servicestest.NewMeter(1 << 20)}
	dir := t.TempDir()
	v.keyFile = filepath.Join(dir, "key")
	v.missingFile = filepath.Join(dir, "absent")
	if err := os.WriteFile(v.keyFile, []byte(testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v.primary, v.backup, v.cf = newFakeUpstream(t, "primary", false), newFakeUpstream(t, "backup", false), newFakeUpstream(t, "cf", true)
	cfg, err := services.InferenceConfigForTest([]byte(inferenceConfig(v.primary.srv.URL, v.backup.srv.URL, v.cf.srv.URL, v.keyFile, v.keyFile, caps)), false)
	if err != nil {
		t.Fatal(err)
	}
	v.cfg = cfg
	v.build(t)
	return v
}

// build makes a fresh engine and provider over the same database, as a
// restart does.
func (v *infEnv) build(t *testing.T) {
	reg := services.NewBuiltinRegistry([]string{"inference"}, services.Deps{DB: v.db, Inference: v.cfg})
	v.e = services.NewEngine(services.Config{DB: v.db, Registry: reg, Meter: v.meter, Now: func() int64 { return 1000 }, HoldsPerAccount: 8})
	t.Cleanup(v.e.Stop)
}

var infSubject = allowance.Subject{ID: "acct", KeyID: "k", Signed: true}

// call runs one service.call to completion: the command's transaction, then
// the after-commit run and settle.
func (v *infEnv) call(t *testing.T, args string, maxCost int64) (map[string]any, string, error) {
	t.Helper()
	v.n++
	key := fmt.Sprintf("id:%d", v.n)
	data := fmt.Sprintf(`{"schema":1,"method":"complete","args":%s,"max_cost":%d}`, args, maxCost)
	tx, err := v.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	out, err := v.e.Call(context.Background(), tx, services.Request{Service: "inference", Data: data, Subject: infSubject, RequestKey: key}, 1000)
	if err != nil {
		tx.Rollback()
		return nil, key, err
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if out.After == nil {
		t.Fatal("inference must run after commit")
	}
	res, err := out.After()
	if err != nil {
		return nil, key, err
	}
	raw, _ := json.Marshal(res)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m, key, nil
}

type callRow struct {
	state, body, public, errCode string
	cost, maxCost                int64
}

func (v *infEnv) row(t *testing.T, key string) callRow {
	t.Helper()
	var r callRow
	if err := v.db.QueryRow("SELECT state,body,public,error,cost,max_cost FROM service_calls WHERE request_key=?", key).Scan(&r.state, &r.body, &r.public, &r.errCode, &r.cost, &r.maxCost); err != nil {
		t.Fatal(err)
	}
	return r
}

func (v *infEnv) hold(t *testing.T, key string) servicestest.Entry {
	t.Helper()
	entries, err := v.meter.Entries(context.Background(), v.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Kind == "hold" && e.RequestKey == key {
			return e
		}
	}
	t.Fatalf("no hold for %s", key)
	return servicestest.Entry{}
}

func (v *infEnv) spent(t *testing.T, upstream string) int64 {
	t.Helper()
	var n int64
	err := v.db.QueryRow("SELECT COALESCE(sum(units),0) FROM inference_spend WHERE upstream=?", upstream).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

const smallArgs = `{"model":"small","messages":[{"role":"user","content":"hello"}],"max_tokens":50}`

// Prices for smallArgs: the input bound is 5 bytes + 16 per message + 64 =
// 85 tokens. primary reserves 1 + 85 + 2×50 = 186, backup 2 + 85 + 50 = 137;
// the quote is the larger. Charged at 20 in, 10 out: primary 1 + 20 + 20 =
// 41, backup 2 + 20 + 10 = 32.
const (
	smallQuote  = 186
	primaryUsed = 41
	backupUsed  = 32
	primaryStep = 186
)

func TestInferenceSuccess(t *testing.T) {
	v := newInfEnv(t, [3]int64{10000, 10000, 10000})
	res, key, err := v.call(t, smallArgs, 1000)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := res["result"].(map[string]any)
	if result["output"] != "hello from primary" || result["model"] != "small" || result["upstream"] != "primary" || result["upstream_model"] != "m1" || result["log"] != "public" {
		t.Fatalf("result: %v", res)
	}
	msgs, _ := result["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "hello" {
		t.Fatalf("the prompt must be in the record: %v", result)
	}
	r := v.row(t, key)
	if r.state != "done" || r.cost != primaryUsed || r.maxCost != smallQuote {
		t.Fatalf("call record: %+v", r)
	}
	if !strings.Contains(r.body, `"content":"hello"`) || !strings.Contains(r.body, `"output":"hello from primary"`) || !strings.Contains(r.body, `"upstream_model":"m1"`) {
		t.Fatalf("the record keeps prompt, output and model: %s", r.body)
	}
	var pub map[string]any
	if err = json.Unmarshal([]byte(r.public), &pub); err != nil || pub["model"] != "small" || pub["input_tokens"] != float64(20) || len(pub["output_sha256"].(string)) != 64 {
		t.Fatalf("public record: %s", r.public)
	}
	if h := v.hold(t, key); h.State != "committed" || h.Units != primaryUsed || h.MaxUnits != smallQuote {
		t.Fatalf("reserve at the maximum, commit the actual: %+v", h)
	}
	if got := v.spent(t, "primary"); got != primaryUsed {
		t.Fatalf("primary's day spend is the actual charge, got %d", got)
	}
	// What the upstream saw: the configured model, the key as a bearer token,
	// non-streaming, the caps, and the operator's extra fields.
	b := v.primary.bodies[0]
	if v.primary.auth[0] != "Bearer "+testKey || b["model"] != "m1" || b["stream"] != false || b["max_tokens"] != float64(50) || v.primary.paths[0] != "/chat/completions" {
		t.Fatalf("upstream request: %v %v %v", v.primary.auth, b, v.primary.paths)
	}
	if p, _ := b["provider"].(map[string]any); p["zdr"] != true {
		t.Fatalf("extra fields must be sent: %v", b)
	}
	if v.backup.count() != 0 {
		t.Fatal("no failover on success")
	}
}

func TestInferenceWorkersAI(t *testing.T) {
	v := newInfEnv(t, [3]int64{10000, 10000, 10000})
	res, key, err := v.call(t, `{"model":"cf","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],"max_tokens":500,"temperature":0.2}`, 100)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := res["result"].(map[string]any)
	if result["output"] != "hello from cf" || result["upstream"] != "cf" {
		t.Fatalf("result: %v", res)
	}
	if v.cf.paths[0] != "/accounts/abc123/ai/run/@cf/meta/llama-3.1-8b-instruct" || v.cf.auth[0] != "Bearer "+testKey {
		t.Fatalf("workers ai request: %v", v.cf.paths)
	}
	b := v.cf.bodies[0]
	if _, has := b["model"]; has || b["max_tokens"] != float64(64) || b["temperature"] != 0.2 {
		t.Fatalf("workers ai body (model in the path, max_tokens clamped to the model's cap): %v", b)
	}
	if r := v.row(t, key); r.cost != 1 {
		t.Fatalf("a model priced at base only costs its base: %+v", r)
	}
}

func TestInferenceFailover(t *testing.T) {
	for _, tc := range []struct {
		mode, outcome string
		keepsReserve  bool // the upstream may have billed: its day count keeps the step's maximum
	}{
		{"500", "http_500", false},
		{"503", "http_503", false},
		{"429", "rate_limited", false},
		{"404", "http_404", false},
		{"timeout", "timeout", true},
		{"401", "suspended", false},
		{"403", "suspended", false},
		{"oversized", "oversized", true},
		{"malformed", "malformed", true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			v := newInfEnv(t, [3]int64{10000, 10000, 10000})
			v.primary.setMode(tc.mode)
			res, key, err := v.call(t, smallArgs, 1000)
			if err != nil {
				t.Fatalf("backup must answer: %v", err)
			}
			result, _ := res["result"].(map[string]any)
			attempts, _ := result["attempts"].([]any)
			if result["upstream"] != "backup" || len(attempts) != 1 || attempts[0].(map[string]any)["outcome"] != tc.outcome {
				t.Fatalf("result: %v", result)
			}
			// Charged the answering upstream's actual usage, plus what the
			// failed step may have cost us when it reached the upstream, up
			// to the quote (security review 1.20, M3).
			wantCost := int64(backupUsed)
			if tc.keepsReserve {
				wantCost = min(backupUsed+primaryStep, smallQuote)
			}
			if r := v.row(t, key); r.cost != wantCost {
				t.Fatalf("charged %d, want %d: %+v", r.cost, wantCost, r)
			}
			want := int64(0)
			if tc.keepsReserve {
				want = primaryStep
			}
			if got := v.spent(t, "primary"); got != want {
				t.Fatalf("primary day count %d, want %d", got, want)
			}
			if tc.outcome == "suspended" {
				// A suspended upstream is skipped, not retried, on the next call.
				v.primary.setMode("ok")
				hits := v.primary.count()
				res, _, err = v.call(t, smallArgs, 1000)
				if err != nil || v.primary.count() != hits {
					t.Fatalf("suspended upstream was called again: %v", err)
				}
				result, _ = res["result"].(map[string]any)
				attempts, _ = result["attempts"].([]any)
				if result["upstream"] != "backup" || attempts[0].(map[string]any)["outcome"] != "suspended" {
					t.Fatalf("result: %v", result)
				}
			}
		})
	}
}

func TestInferenceEveryUpstreamFails(t *testing.T) {
	for _, tc := range []struct {
		mode, code string
		billed     bool // the upstreams got the request and may have billed it
	}{
		{"500", "upstream_busy", false},
		{"429", "upstream_busy", false},
		{"timeout", "upstream_busy", true},
		{"401", "upstream_unavailable", false},
		{"malformed", "upstream_failed", true},
		{"oversized", "upstream_failed", true},
		{"400", "upstream_failed", false}, // the request itself was refused: no failover
	} {
		t.Run(tc.mode, func(t *testing.T) {
			v := newInfEnv(t, [3]int64{10000, 10000, 10000})
			v.primary.setMode(tc.mode)
			v.backup.setMode(tc.mode)
			res, key, err := v.call(t, smallArgs, 1000)
			if tc.billed {
				// Charged up to the quote for what the upstreams may have
				// billed (security review 1.20, M3); the record says why.
				result, _ := res["result"].(map[string]any)
				if err != nil || result["error"] != tc.code || result["output"] != nil {
					t.Fatalf("billed failure: %v %v", res, err)
				}
				if r := v.row(t, key); r.state != "done" || r.cost != smallQuote {
					t.Fatalf("billed failure record: %+v", r)
				}
				return
			}
			if code(err) != tc.code {
				t.Fatalf("got %v, want %s", err, tc.code)
			}
			if h := v.hold(t, key); h.State != "refunded" {
				t.Fatalf("a failed call is refunded in full: %+v", h)
			}
			if r := v.row(t, key); r.state != "failed" || r.cost != 0 || r.body != "" {
				t.Fatalf("failed record: %+v", r)
			}
			if tc.mode == "400" && v.backup.count() != 0 {
				t.Fatal("a 400 ends the route")
			}
		})
	}
}

func TestInferenceNoRedirects(t *testing.T) {
	v := newInfEnv(t, [3]int64{10000, 10000, 10000})
	trap := newFakeUpstream(t, "trap", false)
	v.primary.redirect = trap.srv.URL + "/chat/completions"
	v.primary.setMode("redirect")
	res, _, err := v.call(t, smallArgs, 1000)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := res["result"].(map[string]any)
	if trap.count() != 0 || result["upstream"] != "backup" || !strings.Contains(fmt.Sprint(result["attempts"]), "http_302") {
		t.Fatalf("a redirect must not be followed: trap hits %d, %v", trap.count(), result)
	}
}

func TestInferenceDailyCapFailsClosed(t *testing.T) {
	// primary's cap (250) fits one step maximum (186) over 41 counted but not over 82; backup's cap
	// is below its step maximum, so backup never runs.
	v := newInfEnv(t, [3]int64{250, 100, 10000})
	if _, _, err := v.call(t, smallArgs, 1000); err != nil {
		t.Fatal(err)
	}
	// 41 counted; another 186 still fits under 250.
	if _, _, err := v.call(t, smallArgs, 1000); err != nil {
		t.Fatal(err)
	}
	// 82 counted: a third maximum (186) would pass 250.
	hits, backupHits := v.primary.count(), v.backup.count()
	_, key, err := v.call(t, smallArgs, 1000)
	if code(err) != "upstream_unavailable" || v.primary.count() != hits || v.backup.count() != backupHits {
		t.Fatalf("over the cap nothing may be sent: %v", err)
	}
	if h := v.hold(t, key); h.State != "refunded" {
		t.Fatalf("a capped call is refunded: %+v", h)
	}
	if !strings.Contains(v.row(t, key).errCode, "upstream_unavailable") {
		t.Fatalf("capped call record: %+v", v.row(t, key))
	}
	// The count is in the database: a restart does not reset it.
	v.build(t)
	if _, _, err = v.call(t, smallArgs, 1000); code(err) != "upstream_unavailable" {
		t.Fatalf("the cap must survive a restart: %v", err)
	}
	// A database that cannot be written fails closed too.
	if _, err = v.db.Exec("DROP TABLE inference_spend"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = v.call(t, `{"model":"cf","messages":[{"role":"user","content":"x"}]}`, 100); code(err) != "upstream_unavailable" || v.cf.count() != 0 {
		t.Fatalf("no spend record, no call: %v", err)
	}
}

func TestInferenceUnavailableWithoutKeyFile(t *testing.T) {
	v := newInfEnv(t, [3]int64{10000, 10000, 10000})
	if err := os.Remove(v.keyFile); err != nil {
		t.Fatal(err)
	}
	cat, err := v.e.Catalogue(context.Background(), v.db, 1000)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(cat)
	if !strings.Contains(string(raw), `"available":false`) || strings.Contains(string(raw), `"available":true`) {
		t.Fatalf("with no key file the service lists as unavailable: %s", raw)
	}
	_, key, err := v.call(t, smallArgs, 1000)
	if code(err) != "upstream_unavailable" || v.primary.count()+v.backup.count() != 0 {
		t.Fatalf("no key, no request: %v", err)
	}
	if h := v.hold(t, key); h.State != "refunded" {
		t.Fatalf("refunded: %+v", h)
	}
	// With the key back, it lists as available.
	if err = os.WriteFile(v.keyFile, []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	cat, _ = v.e.Catalogue(context.Background(), v.db, 1000)
	raw, _ = json.Marshal(cat)
	if !strings.Contains(string(raw), `"available":true`) || !strings.Contains(string(raw), `"input_per_mtok":1000000`) {
		t.Fatalf("catalogue: %s", raw)
	}
	// Unconfigured (no INFERENCE_CONFIG), the provider is unavailable too.
	reg := services.NewBuiltinRegistry([]string{"inference"}, services.Deps{DB: v.db})
	e := services.NewEngine(services.Config{DB: v.db, Registry: reg, Meter: v.meter, Now: func() int64 { return 1000 }})
	tx, _ := v.db.Begin()
	defer tx.Rollback()
	if _, err = e.Call(context.Background(), tx, services.Request{Service: "inference", Data: `{"schema":1,"method":"complete","args":` + smallArgs + `,"max_cost":1000}`, Subject: infSubject, RequestKey: "id:x"}, 1000); code(err) != "upstream_unavailable" {
		t.Fatalf("unconfigured: %v", err)
	}
	// Not listed in SERVICES: not callable at all.
	if _, err = services.NewBuiltinRegistry(nil, services.Deps{DB: v.db, Inference: v.cfg}).Lookup("inference"); code(err) != "invalid_service" {
		t.Fatalf("off by default: %v", err)
	}
}

func TestInferenceSafeDialerRefusesLoopback(t *testing.T) {
	v := newInfEnv(t, [3]int64{10000, 10000, 10000})
	cfg, err := services.InferenceConfigForTest([]byte(inferenceConfig(v.primary.srv.URL, v.backup.srv.URL, v.cf.srv.URL, v.keyFile, v.keyFile, [3]int64{10000, 10000, 10000})), true)
	if err != nil {
		t.Fatal(err)
	}
	v.cfg = cfg
	v.build(t)
	_, _, err = v.call(t, smallArgs, 1000)
	if code(err) != "upstream_busy" || v.primary.count()+v.backup.count() != 0 {
		t.Fatalf("a loopback upstream must never be reached through the safe dialer: %v", err)
	}
	if v.spent(t, "primary") != 0 {
		t.Fatal("a refused connect bills nothing")
	}
}

func TestInferenceArgsAndCaps(t *testing.T) {
	v := newInfEnv(t, [3]int64{10000, 10000, 10000})
	q, err := services.InferenceQuoteForTest(v.cfg, json.RawMessage(smallArgs))
	if err != nil || q.Max != smallQuote || q.Resource != allowance.Credit {
		t.Fatalf("quote: %+v %v", q, err)
	}
	// max_tokens defaults to 256, clamped per model (100 on both routes).
	if q, _ = services.InferenceQuoteForTest(v.cfg, json.RawMessage(`{"model":"small","messages":[{"role":"user","content":"hello"}]}`)); q.Max != 1+85+200 {
		t.Fatalf("default max_tokens quote: %+v", q)
	}
	for _, args := range []string{
		`{"model":"nope","messages":[{"role":"user","content":"x"}]}`,
		`{"model":"small","messages":[]}`,
		`{"model":"small"}`,
		`{"model":"small","messages":[{"role":"tool","content":"x"}]}`,
		`{"model":"small","messages":[{"role":"user","content":"x","name":"y"}]}`,
		`{"model":"small","messages":[{"role":"user","content":"x"}],"stream":true}`,
		`{"model":"small","messages":[{"role":"user","content":"x"}],"max_tokens":0}`,
		`{"model":"small","messages":[{"role":"user","content":"x"}],"max_tokens":4097}`,
		`{"model":"small","messages":[{"role":"user","content":"x"}],"max_tokens":"5"}`,
		`{"model":"small","messages":[{"role":"user","content":"x"}],"temperature":2.5}`,
		`{"model":"small","messages":[{"role":"user","content":"x"}],"temperature":-1}`,
		`{"model":"small","messages":[{"role":"user","content":"x"}],"temperature":1e0}`,
		`{"model":"small","messages":[{"role":"user","content":"` + strings.Repeat("a", services.InferencePromptBytes+1) + `"}]}`,
		`{"model":"small","messages":[` + strings.TrimSuffix(strings.Repeat(`{"role":"user","content":"x"},`, services.InferenceMessagesMax+1), ",") + `]}`,
	} {
		if _, err := services.InferenceQuoteForTest(v.cfg, json.RawMessage(args)); code(err) != "invalid_service_data" {
			t.Errorf("%.120s: %v", args, err)
		}
	}
	// A max_cost below the quote spends nothing and sends nothing.
	if _, _, err = v.call(t, smallArgs, smallQuote-1); code(err) != "price_exceeds_max" || v.primary.count() != 0 {
		t.Fatalf("price_exceeds_max: %v", err)
	}
	entries, _ := v.meter.Entries(context.Background(), v.db)
	if len(entries) != 0 {
		t.Fatalf("nothing reserved: %+v", entries)
	}
	// Usage above the bounds is charged at most the step's maximum; a reply
	// without usage is charged the maximum.
	v.primary.setMode("no_usage")
	_, key, err := v.call(t, smallArgs, 1000)
	if err != nil || v.row(t, key).cost != primaryStep {
		t.Fatalf("no usage reported: charged the maximum: %v %+v", err, v.row(t, key))
	}
}

type screen struct {
	hidePrompt, hideOutput bool
	fail                   bool
	seen                   []services.ScreenInput
}

func (s *screen) Screen(_ context.Context, in services.ScreenInput) (services.ScreenVerdict, error) {
	s.seen = append(s.seen, in)
	if s.fail {
		return services.ScreenVerdict{}, errors.New("screen down")
	}
	if (in.Stage == "prompt" && s.hidePrompt) || (in.Stage == "output" && s.hideOutput) {
		return services.ScreenVerdict{Hide: true, Reason: "phishing"}, nil
	}
	return services.ScreenVerdict{}, nil
}

func TestInferenceScreenerHook(t *testing.T) {
	v := newInfEnv(t, [3]int64{10000, 10000, 10000})
	s := &screen{hidePrompt: true}
	v.cfg.Screener = s
	if _, _, err := v.call(t, smallArgs, 1000); code(err) != "content_refused" || v.primary.count() != 0 {
		t.Fatalf("a hidden prompt is never sent: %v", err)
	}
	s.hidePrompt, s.hideOutput = false, true
	res, key, err := v.call(t, smallArgs, 1000)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := res["result"].(map[string]any)
	if result["output"] != "" || result["hidden"] != "phishing" || len(result["output_sha256"].(string)) != 64 || v.row(t, key).cost != primaryUsed {
		t.Fatalf("a hidden output keeps its hash and charge: %v", result)
	}
	if len(s.seen) != 3 || s.seen[1].Stage != "prompt" || !strings.Contains(s.seen[1].Text, "hello") || s.seen[2].Text != "hello from primary" {
		t.Fatalf("screener inputs: %+v", s.seen)
	}
	s.hideOutput, s.fail = false, true
	if _, _, err = v.call(t, smallArgs, 1000); code(err) != "upstream_unavailable" {
		t.Fatalf("a screener error fails closed: %v", err)
	}
}

func TestInferenceConfigStrict(t *testing.T) {
	good := inferenceConfig("https://api.example.com/v1", "https://b.example.com/openai/v1/", "https://api.cloudflare.com/client/v4", "/etc/k", "/etc/c", [3]int64{1, 1, 1})
	if _, err := services.ParseInferenceConfig([]byte(good)); err != nil {
		t.Fatalf("good config: %v", err)
	}
	up := func(fields string) string {
		return `{"schema":1,"upstreams":[{"name":"a","kind":"openai_compat",` + fields + `,"models":{"m":{"price":{"base":1,"input_per_mtok":1,"output_per_mtok":1},"max_output_tokens":10}}}],"models":{"x":[{"upstream":"a","model":"m"}]}}`
	}
	if _, err := services.ParseInferenceConfig([]byte(up(`"base_url":"https://h.example","key_file":"/k","daily_spend_cap":5`))); err != nil {
		t.Fatalf("minimal: %v", err)
	}
	for _, bad := range []string{
		up(`"base_url":"http://h.example","key_file":"/k","daily_spend_cap":5`),
		up(`"base_url":"https://user:pw@h.example","key_file":"/k","daily_spend_cap":5`),
		up(`"base_url":"https://h.example/?a=1","key_file":"/k","daily_spend_cap":5`),
		up(`"base_url":"https://h.example","key_file":"k","daily_spend_cap":5`),
		up(`"base_url":"https://h.example","key_file":"/k"`),
		up(`"base_url":"https://h.example","key_file":"/k","daily_spend_cap":0`),
		up(`"base_url":"https://h.example","key_file":"/k","daily_spend_cap":5,"api_key":"sk-x"`),
		up(`"base_url":"https://h.example","key_file":"/k","daily_spend_cap":5,"timeout_ms":999999`),
		up(`"base_url":"https://h.example","key_file":"/k","daily_spend_cap":5,"extra":{"model":"other"}`),
		up(`"base_url":"https://h.example","key_file":"/k","daily_spend_cap":5,"extra":{"stream":true}`),
		up(`"base_url":"https://h.example","key_file":"/k","daily_spend_cap":5,"account_id":"x"`),
		`{"schema":1,"upstreams":[],"models":{}}`,
		`{"schema":2}`,
		strings.Replace(up(`"base_url":"https://h.example","key_file":"/k","daily_spend_cap":5`), `"model":"m"}`, `"model":"other"}`, 1),
		strings.Replace(up(`"base_url":"https://h.example","key_file":"/k","daily_spend_cap":5`), `"upstream":"a"`, `"upstream":"b"`, 1),
		strings.Replace(up(`"base_url":"https://h.example","key_file":"/k","daily_spend_cap":5`), `"base":1`, `"base":0`, 1),
		strings.Replace(up(`"base_url":"https://h.example","key_file":"/k","daily_spend_cap":5`), `"max_output_tokens":10`, `"max_output_tokens":5000`, 1),
		strings.Replace(up(`"base_url":"https://h.example","key_file":"/k","daily_spend_cap":5`), `"m":{`, `"a/../b":{`, 1),
		strings.Replace(up(`"base_url":"https://h.example","key_file":"/k","daily_spend_cap":5`), `"kind":"openai_compat"`, `"kind":"workers_ai"`, 1),
	} {
		if _, err := services.ParseInferenceConfig([]byte(bad)); err == nil {
			t.Errorf("must be refused: %.200s", bad)
		}
	}
}

// TestInferenceNeverLeaksKey runs every path with upstreams that echo the key
// they were sent, then looks for it in every error, result, table and log line.
func TestInferenceNeverLeaksKey(t *testing.T) {
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prev)
	v := newInfEnv(t, [3]int64{1 << 30, 1 << 30, 1 << 30})
	var seen []string
	for _, mode := range []string{"ok", "echo_key", "500", "429", "400", "malformed", "oversized", "timeout", "401"} {
		v.primary.setMode(mode)
		v.backup.setMode(mode)
		res, _, err := v.call(t, smallArgs, 1000)
		raw, _ := json.Marshal(res)
		seen = append(seen, string(raw), fmt.Sprint(err), fmt.Sprintf("%#v", err))
		if mode == "echo_key" && !strings.Contains(string(raw), "your key is [redacted] ok") {
			t.Fatalf("a key echoed in the output is redacted: %s", raw)
		}
	}
	cat, _ := v.e.Catalogue(context.Background(), v.db, 1000)
	raw, _ := json.Marshal(cat)
	seen = append(seen, string(raw), logs.String())
	// Every row of every table, as text.
	tables, err := v.db.Query("SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var n string
		_ = tables.Scan(&n)
		names = append(names, n)
	}
	tables.Close()
	for _, n := range names {
		rows, err := v.db.Query("SELECT * FROM " + n)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			_ = rows.Scan(ptrs...)
			seen = append(seen, fmt.Sprintf("%s", vals))
		}
		rows.Close()
	}
	if !strings.Contains(logs.String(), "upstream=primary") {
		t.Fatalf("a suspension is logged for the operator: %q", logs.String())
	}
	for _, s := range seen {
		for _, needle := range []string{testKey, "SECRET"} {
			if strings.Contains(s, needle) {
				t.Fatalf("key material leaked: %.300s", s)
			}
		}
	}
}

func FuzzInferenceReply(f *testing.F) {
	for _, seed := range []string{
		`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`,
		`{"choices":[{"message":{"content":null}}]}`,
		`{"choices":[]}`,
		`{"success":true,"result":{"response":"hi","usage":{"prompt_tokens":1,"completion_tokens":2}}}`,
		`{"success":true,"result":{"choices":[{"message":{"content":"x"},"finish_reason":"length"}]}}`,
		`{"success":false,"errors":[{"code":1}]}`,
		`{"usage":{"prompt_tokens":-1,"completion_tokens":1e9}}`, `{"choices":[{"message":{"content":"a"}}]} x`,
		`[`, ``, `null`, `{"choices":[{"message":{"content":"\ud800"}}]}`,
	} {
		f.Add([]byte(seed), false)
		f.Add([]byte(seed), true)
	}
	f.Fuzz(func(t *testing.T, raw []byte, workers bool) {
		out, finish, in, n, reported, err := services.InferenceReplyForTest(raw, workers)
		if err != nil {
			if out != "" || in != 0 || n != 0 || reported {
				t.Fatalf("a refused reply carries nothing: %q", raw)
			}
			return
		}
		if len(raw) > services.InferenceResponseBytes || len(out) > services.InferenceOutputBytes || !utf8.ValidString(out) {
			t.Fatalf("accepted out-of-bounds reply %q", raw)
		}
		if in < 0 || n < 0 || in > 1<<31 || n > 1<<31 || (!reported && (in != 0 || n != 0)) {
			t.Fatalf("token counts out of range: %d %d %v", in, n, reported)
		}
		if len(finish) > 32 || strings.ContainsFunc(finish, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r == '_') }) {
			t.Fatalf("finish reason not sanitised: %q", finish)
		}
		// Parsing is deterministic.
		out2, finish2, in2, n2, reported2, err2 := services.InferenceReplyForTest(raw, workers)
		if err2 != nil || out2 != out || finish2 != finish || in2 != in || n2 != n || reported2 != reported {
			t.Fatal("reparse differs")
		}
	})
}

// A full prompt and a full reply that JSON escaping inflates past 64 KiB are
// recorded, answered and charged; a reply that escapes past the record's bound
// is kept cut, with the full output's sha256 and size, and still charged:
// the upstream answered and was paid.
func TestInferenceLargeRecordsKeepTheCharge(t *testing.T) {
	v := newInfEnv(t, [3]int64{1 << 40, 1 << 40, 1 << 40})
	prompt, _ := json.Marshal(strings.Repeat("\"q\n", services.InferencePromptBytes/3))
	args := `{"model":"small","messages":[{"role":"user","content":` + string(prompt) + `}],"max_tokens":50}`
	for _, mode := range []string{"escapes", "control"} {
		v.primary.setMode(mode)
		res, key, err := v.call(t, args, 1<<30)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		r := v.row(t, key)
		if r.state != "done" || r.cost <= 0 || len(r.body) <= services.StoredBodyMaxBytes || len(r.body) > services.InferenceStoredBodyBytes {
			t.Fatalf("%s: record %s cost %d, %d bytes", mode, r.state, r.cost, len(r.body))
		}
		if h := v.hold(t, key); h.State != "committed" || h.Units != r.cost {
			t.Fatalf("%s: the charge must be committed: %+v", mode, h)
		}
		result, _ := res["result"].(map[string]any)
		out, _ := result["output"].(string)
		cut, _ := result["output_truncated"].(bool)
		if mode == "escapes" && (cut || len(out) != services.InferenceOutputBytes/3*3) {
			t.Fatalf("escapes: the whole output fits: %d bytes, cut %v", len(out), cut)
		}
		if mode == "control" && (!cut || len(out) == 0 || len(out) >= services.InferenceOutputBytes || result["output_bytes"] != float64(services.InferenceOutputBytes)) {
			t.Fatalf("control: the output must be cut and say so: %d bytes, %v", len(out), result["output_bytes"])
		}
		full := sha256.Sum256([]byte(strings.Repeat("\x01", services.InferenceOutputBytes)))
		if mode == "control" && result["output_sha256"] != hex.EncodeToString(full[:]) {
			t.Fatal("control: output_sha256 must be the full output's")
		}
	}
}
