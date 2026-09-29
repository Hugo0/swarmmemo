package services_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
)

const runsSecret = "0123456789abcdef0123456789abcdef-loader"

// fakeLoader stands in for workers/runs-loader: it checks the request MAC
// exactly as auth.ts does, and answers what the test asks, signed.
type fakeLoader struct {
	mu       sync.Mutex
	secret   []byte // what it verifies requests with
	signWith []byte // what it signs answers with
	reqs     []map[string]any
	answer   func(req map[string]any) (int, []byte, time.Duration)
}

func (f *fakeLoader) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	ts, _ := strconv.ParseInt(r.Header.Get("X-Runs-Timestamp"), 10, 64)
	if r.Header.Get("X-Runs-Signature") != services.SignRunsRequest(f.secret, "req", ts, body) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	answer := f.answer
	f.mu.Unlock()
	status, out, delay := answer(req)
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	now := time.Now().Unix()
	w.Header().Set("X-Runs-Timestamp", strconv.FormatInt(now, 10))
	w.Header().Set("X-Runs-Signature", services.SignRunsRequest(f.signWith, "res", now, out))
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

func (f *fakeLoader) calls() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.reqs...)
}

// okAnswer is a well-formed loader answer for req, changed by edit.
func okAnswer(edit func(*services.LoaderResponse)) func(map[string]any) (int, []byte, time.Duration) {
	return func(req map[string]any) (int, []byte, time.Duration) {
		net := req["network"].(map[string]any)["enabled"].(bool)
		r := services.LoaderResponse{Schema: 1, RunID: req["run_id"].(string), Status: "ok", ResultJSON: `{"ok":true}`, Stdout: "hello\n",
			CPUMs: 12, CPUSource: "tail", WallMs: 30, Outcome: "ok", Network: "off", Egress: services.EgressSummary{Log: []services.EgressEntry{}}}
		if net {
			r.Network = "on"
			r.Egress = services.EgressSummary{Requests: 1, BytesOut: 100, BytesIn: 2000, Log: []services.EgressEntry{
				{Seq: 0, TMs: 3, Method: "GET", Host: "api.example.org", Port: 443, PathSHA256: strings.Repeat("a", 64), BytesOut: 100, BytesIn: 2000, Status: 200, Ms: 20, Verdict: "allowed"},
			}}
		}
		if edit != nil {
			edit(&r)
		}
		b, _ := json.Marshal(r)
		return 200, b, 0
	}
}

// screener is a moderation stand-in: a fixed decision per surface, counted.
type screener struct {
	mu        sync.Mutex
	decisions map[string]services.RunScreenDecision
	seen      map[string]int
}

func (s *screener) Screen(ctx context.Context, surface, subject, content string) services.RunScreenDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]int{}
	}
	s.seen[surface]++
	if d, ok := s.decisions[surface]; ok {
		return d
	}
	return services.RunScreenDecision{Action: "allow"}
}

type runsEnv struct {
	t      *testing.T
	db     *sql.DB
	meter  *servicestest.Meter
	engine *services.Engine
	loader *fakeLoader
	cfg    *services.RunsConfig
	dir    string
	n      int
}

type runsOpts struct {
	config   string // extra top-level config keys, JSON without braces
	screener services.RunScreener
	safe     bool
	grace    time.Duration
}

func newRunsEnv(t *testing.T, o runsOpts) *runsEnv {
	t.Helper()
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret")
	if err := os.WriteFile(secretFile, []byte(runsSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fl := &fakeLoader{secret: []byte(runsSecret), signWith: []byte(runsSecret), answer: okAnswer(nil)}
	srv := httptest.NewServer(fl)
	t.Cleanup(srv.Close)
	extra := o.config
	if !strings.Contains(extra, `"screen"`) {
		extra += `,"screen":"static"`
	}
	body := fmt.Sprintf(`{"loader_url":%q,"secret_file":%q,"languages":["javascript","python"],"network":"allowed","network_off_file":%q,
"limits":{"cpu_ms_max":1000,"wall_ms_max":2000,"stdout_bytes":32768}%s}`, srv.URL+"/run", secretFile, filepath.Join(dir, "network-off"), extra)
	cfg, err := services.RunsConfigForTest([]byte(body), o.safe, nil, o.screener, o.grace)
	if err != nil {
		t.Fatal(err)
	}
	db := openDB(t)
	meter := servicestest.NewMeter(1_000_000)
	reg := services.NewBuiltinRegistry([]string{"runs"}, services.Deps{DB: db, Runs: cfg, NotaryKey: testNotaryKey})
	e := services.NewEngine(services.Config{DB: db, Registry: reg, Meter: meter, Now: func() int64 { return time.Now().Unix() }})
	return &runsEnv{t: t, db: db, meter: meter, engine: e, loader: fl, cfg: cfg, dir: dir}
}

var alice = allowance.Subject{ID: "acct-alice", KeyID: "k-alice", Signed: true}

// call makes one service.call runs run and returns the provider's result
// body, as the engine hands it to the caller once settled.
func (env *runsEnv) call(s allowance.Subject, args string) (map[string]any, error) {
	env.t.Helper()
	env.n++
	ctx := context.Background()
	tx, err := env.db.Begin()
	if err != nil {
		env.t.Fatal(err)
	}
	data := fmt.Sprintf(`{"schema":1,"method":"run","args":%s,"max_cost":100000}`, args)
	out, err := env.engine.Call(ctx, tx, services.Request{Service: "runs", Data: data, Subject: s, RequestKey: fmt.Sprintf("id:r%d", env.n)}, time.Now().Unix())
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		env.t.Fatal(err)
	}
	if out.After == nil {
		env.t.Fatal("runs must be a Remote call")
	}
	final, err := out.After()
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(final["result"])
	var body map[string]any
	if err = json.Unmarshal(raw, &body); err != nil {
		env.t.Fatalf("result body: %s %v", raw, err)
	}
	return body, nil
}

// lastHold is the newest hold in the fake ledger.
func (env *runsEnv) lastHold() servicestest.Entry {
	env.t.Helper()
	entries, err := env.meter.Entries(context.Background(), env.db)
	if err != nil || len(entries) == 0 {
		env.t.Fatalf("no ledger entries: %v", err)
	}
	return entries[len(entries)-1]
}

func (env *runsEnv) logRow(runID string) map[string]any {
	env.t.Helper()
	row := env.db.QueryRow("SELECT status, network, network_note, cpu_ms, egress_bytes, cost, flagged, output, output_sha256, egress_sha256, code_decision, egress_decision FROM runs_log WHERE run_id=?", runID)
	var status, network, note, output, outHash, egHash, codeD, egD string
	var cpu, egress, cost, flagged int64
	if err := row.Scan(&status, &network, &note, &cpu, &egress, &cost, &flagged, &output, &outHash, &egHash, &codeD, &egD); err != nil {
		env.t.Fatalf("runs_log %s: %v", runID, err)
	}
	return map[string]any{"status": status, "network": network, "note": note, "cpu": cpu, "egress": egress, "cost": cost, "flagged": flagged,
		"output": output, "output_sha256": outHash, "egress_sha256": egHash, "code_decision": codeD, "egress_decision": egD}
}

const jsArgs = `{"language":"javascript","code":"export function run(i) { return {ok: true} }","input":{"n":1}}`

func TestRunsSuccessNetworkOffSignedReceiptAndLog(t *testing.T) {
	env := newRunsEnv(t, runsOpts{})
	body, err := env.call(alice, jsArgs)
	if err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" || body["network"] != "off" || body["stdout"] != "hello\n" || fmt.Sprint(body["result"]) != "map[ok:true]" {
		t.Fatalf("body: %v", body)
	}
	reqs := env.loader.calls()
	if len(reqs) != 1 {
		t.Fatalf("one loader call, got %d", len(reqs))
	}
	req := reqs[0]
	if net := req["network"].(map[string]any); net["enabled"] != false || net["allow_connect"] != false {
		t.Fatalf("network must be off and raw TCP never allowed: %v", net)
	}
	if deny := fmt.Sprint(req["network"].(map[string]any)["deny_hosts"]); deny != "[127.0.0.1]" {
		t.Fatalf("the loader's own host is always denied: %s", deny)
	}
	if lim := req["limits"].(map[string]any); lim["cpu_ms"] != float64(1000) || lim["stdout_bytes"] != float64(32768) {
		t.Fatalf("limits: %v", lim)
	}
	// Metering: reserve at the max (10 + 1000 CPU ms), commit the actual (10 + 12).
	h := env.lastHold()
	if h.Kind != "hold" || h.State != "committed" || h.MaxUnits != 1010 || h.Units != 22 || h.Resource != "credit" {
		t.Fatalf("hold: %+v", h)
	}
	// The receipt verifies offline and binds the stored output and egress.
	raw, _ := json.Marshal(body["receipt"])
	var rc services.RunReceipt
	if err = json.Unmarshal(raw, &rc); err != nil || !services.VerifyRunReceipt(rc.PublicKey, rc) {
		t.Fatalf("receipt must verify: %s %v", raw, err)
	}
	var payload services.RunReceiptPayload
	_ = json.Unmarshal([]byte(rc.Payload), &payload)
	row := env.logRow(body["run_id"].(string))
	sum := sha256.Sum256([]byte(row["output"].(string)))
	codeSum := sha256.Sum256([]byte("export function run(i) { return {ok: true} }"))
	if payload.OutputSHA256 != hex.EncodeToString(sum[:]) || payload.OutputSHA256 != row["output_sha256"] || payload.EgressSHA256 != row["egress_sha256"] ||
		payload.CodeSHA256 != hex.EncodeToString(codeSum[:]) || payload.CPUMs != 12 || payload.Network || payload.ServiceID != "swarmmemo.com" {
		t.Fatalf("receipt payload %+v vs log %v", payload, row)
	}
	if string(services.RunOutputForTest("ok", "", `{"ok":true}`, "hello\n", "", services.RunTruncated{})) != row["output"] {
		t.Fatalf("stored output is the canonical document: %v", row["output"])
	}
	tampered := rc
	tampered.Payload = strings.Replace(rc.Payload, `"cpu_ms":12`, `"cpu_ms":1`, 1)
	if services.VerifyRunReceipt(rc.PublicKey, tampered) {
		t.Fatal("a tampered receipt must not verify")
	}
	// The full log is readable by its owner only.
	logData := fmt.Sprintf(`{"schema":1,"method":"log","args":{"run":%q}}`, body["run_id"])
	got, err := env.engine.Read(context.Background(), env.db, services.Request{Service: "runs", Data: logData, Subject: alice}, time.Now().Unix())
	if err != nil || !strings.Contains(string(got["result"].(json.RawMessage)), `export function run(i)`) {
		t.Fatalf("log read: %v %v", got, err)
	}
	bob := allowance.Subject{ID: "acct-bob", KeyID: "k-bob", Signed: true}
	if _, err = env.engine.Read(context.Background(), env.db, services.Request{Service: "runs", Data: logData, Subject: bob}, time.Now().Unix()); code(err) != "call_not_found" {
		t.Fatalf("another account's run log: %v", err)
	}
}

func TestRunsNetworkOnMetersEgress(t *testing.T) {
	env := newRunsEnv(t, runsOpts{})
	body, err := env.call(alice, `{"language":"javascript","code":"x","network":{"max_requests":4,"max_bytes_in":10000}}`)
	if err != nil {
		t.Fatal(err)
	}
	net := env.loader.calls()[0]["network"].(map[string]any)
	if net["enabled"] != true || net["max_requests"] != float64(4) || net["max_bytes_in"] != float64(10000) || net["max_bytes_out"] != float64(256<<10) || net["resolve"] != true {
		t.Fatalf("network policy sent: %v", net)
	}
	if body["network"] != "on" || body["flagged"] != false {
		t.Fatalf("body: %v", body)
	}
	// 10 + 12 CPU ms + ceil(2100 / 1024) = 3 KiB; the max was 10 + 1000 + ceil((262144 + 10000) / 1024).
	h := env.lastHold()
	if h.Units != 25 || h.MaxUnits != 10+1000+(262144+10000+1023)/1024 {
		t.Fatalf("hold: %+v", h)
	}
	if row := env.logRow(body["run_id"].(string)); row["egress"] != int64(2100) || row["network"] != "on" {
		t.Fatalf("log: %v", row)
	}
}

func TestRunsNetworkOffLever(t *testing.T) {
	env := newRunsEnv(t, runsOpts{})
	if err := os.WriteFile(filepath.Join(env.dir, "network-off"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := env.call(alice, `{"language":"javascript","code":"x","network":{}}`)
	if err != nil {
		t.Fatal(err)
	}
	if env.loader.calls()[0]["network"].(map[string]any)["enabled"] != false || body["network"] != "off" || body["network_note"] != "lever" {
		t.Fatalf("the lever must force the network off: %v", body)
	}
	if h := env.lastHold(); h.Units != 22 {
		t.Fatalf("no egress is charged with the network off: %+v", h)
	}
	// Released, the next run gets the network again.
	os.Remove(filepath.Join(env.dir, "network-off"))
	if body, err = env.call(alice, `{"language":"javascript","code":"x","network":{}}`); err != nil || body["network"] != "on" {
		t.Fatalf("released: %v %v", body, err)
	}
}

func TestRunsConfigNetworkOffNeverQuotesEgress(t *testing.T) {
	env := newRunsEnv(t, runsOpts{})
	env.cfg.Network = "off"
	body, err := env.call(alice, `{"language":"javascript","code":"x","network":{}}`)
	if err != nil || body["network"] != "off" || body["network_note"] != "config" {
		t.Fatalf("network off by config: %v %v", body, err)
	}
	if h := env.lastHold(); h.MaxUnits != 1010 {
		t.Fatalf("with the network off by config the quote has no egress: %+v", h)
	}
}

// A loader that had the run but whose answer is lost (here: a timeout after
// the request was sent) settles the run at its limits, never refunds it
// (security review 1.20, H4).
func TestRunsLoaderTimeoutChargesTheLimits(t *testing.T) {
	env := newRunsEnv(t, runsOpts{grace: 200 * time.Millisecond})
	env.loader.answer = func(req map[string]any) (int, []byte, time.Duration) {
		_, b, _ := okAnswer(nil)(req)
		return 200, b, 2 * time.Second
	}
	body, err := env.call(alice, `{"language":"javascript","code":"x","wall_ms":100}`)
	if err != nil || body["status"] != "loader_error" || body["flagged"] != false || body["receipt"] == nil {
		t.Fatalf("a loader that does not answer in time: %v %v", body, err)
	}
	if h := env.lastHold(); h.State != "committed" || h.Units != 1010 {
		t.Fatalf("charged at the limits: %+v", h)
	}
	var runs, cpu int64
	_ = env.db.QueryRow("SELECT runs, cpu_ms FROM runs_usage WHERE account=?", alice.ID).Scan(&runs, &cpu)
	if runs != 1 || cpu != 1000 {
		t.Fatalf("a failed run counts against the caps at its limit: runs %d cpu %d", runs, cpu)
	}
	if row := env.logRow(body["run_id"].(string)); row["status"] != "loader_error" || row["cost"] != int64(1010) {
		t.Fatalf("the run log keeps the failure: %v", row)
	}
}

func TestRunsStatusesFromTheLoader(t *testing.T) {
	for _, c := range []struct {
		name  string
		edit  func(*services.LoaderResponse)
		units int64
	}{
		{"cpu exceeded", func(r *services.LoaderResponse) {
			r.Status, r.CPUMs, r.ResultJSON, r.Stdout = "cpu_exceeded", 1000, "", ""
		}, 1010},
		{"run timed out", func(r *services.LoaderResponse) { r.Status, r.CPUMs, r.ResultJSON = "timeout", 40, "" }, 50},
		{"cpu unknown", func(r *services.LoaderResponse) { r.CPUSource, r.CPUMs = "limit", 3 }, 1010},
		{"cpu over the limit", func(r *services.LoaderResponse) { r.CPUMs = 5000 }, 1010},
	} {
		env := newRunsEnv(t, runsOpts{})
		env.loader.answer = okAnswer(c.edit)
		body, err := env.call(alice, jsArgs)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if h := env.lastHold(); h.Units != c.units || h.State != "committed" {
			t.Fatalf("%s: charged %+v, want %d", c.name, h, c.units)
		}
		if c.name == "cpu exceeded" && body["status"] != "cpu_exceeded" {
			t.Fatalf("%s: %v", c.name, body)
		}
	}
}

func TestRunsOversizedOutput(t *testing.T) {
	// A loader answer larger than the cap is refused whole: nothing charged.
	env := newRunsEnv(t, runsOpts{})
	env.loader.answer = func(req map[string]any) (int, []byte, time.Duration) {
		_, b, _ := okAnswer(nil)(req)
		return 200, append(b, strings.Repeat(" ", services.RunsResponseBytes)...), 0
	}
	if body, err := env.call(alice, jsArgs); err != nil || body["status"] != "loader_error" {
		t.Fatalf("oversized answer: %v %v", body, err)
	}
	if h := env.lastHold(); h.State != "committed" || h.Units != h.MaxUnits {
		t.Fatalf("an answer not believed is charged at the limits: %+v", h)
	}
	// A large but valid output is cut in the caller's answer and kept whole in the log.
	env = newRunsEnv(t, runsOpts{})
	big := strings.Repeat("x", 30<<10)
	env.loader.answer = okAnswer(func(r *services.LoaderResponse) { r.Stdout, r.ResultJSON = big, `"`+strings.Repeat("r", 10<<10)+`"` })
	body, err := env.call(alice, jsArgs)
	if err != nil {
		t.Fatal(err)
	}
	trunc := body["truncated"].(map[string]any)
	if len(body["stdout"].(string)) > 4<<10 || trunc["stdout"] != true || body["result"] != nil || trunc["result"] != true {
		t.Fatalf("body must be cut: stdout %d, truncated %v", len(body["stdout"].(string)), trunc)
	}
	if row := env.logRow(body["run_id"].(string)); !strings.Contains(row["output"].(string), big) {
		t.Fatal("the run log keeps the whole output")
	}
	// Output that JSON escaping inflates still fits the stored body.
	env = newRunsEnv(t, runsOpts{})
	env.loader.answer = okAnswer(func(r *services.LoaderResponse) {
		r.Stdout, r.Stderr = strings.Repeat("\x01", 30<<10), strings.Repeat("<", 16<<10)
	})
	if _, err = env.call(alice, jsArgs); err != nil {
		t.Fatalf("escaped output must still fit: %v", err)
	}
}

func TestRunsBadHMAC(t *testing.T) {
	// Our request signed with a secret the loader does not share: 401.
	env := newRunsEnv(t, runsOpts{})
	env.loader.secret = []byte("another-secret-another-secret-another")
	if _, err := env.call(alice, jsArgs); code(err) != "upstream_failed" {
		t.Fatalf("refused request: %v", err)
	}
	if len(env.loader.calls()) != 0 || env.lastHold().State != "refunded" {
		t.Fatal("nothing ran, nothing is charged")
	}
	// An answer signed with the wrong secret is not believed.
	env = newRunsEnv(t, runsOpts{})
	env.loader.signWith = []byte("forged-forged-forged-forged-forged-forged")
	if body, err := env.call(alice, jsArgs); err != nil || body["status"] != "loader_error" || body["result"] != nil {
		t.Fatalf("forged answer: %v %v", body, err)
	}
	if h := env.lastHold(); h.State != "committed" || h.Units != h.MaxUnits {
		t.Fatalf("a forged answer is not believed, and the run it stands for is charged at its limits: %+v", h)
	}
	// An answer for another run is not believed either.
	env = newRunsEnv(t, runsOpts{})
	env.loader.answer = okAnswer(func(r *services.LoaderResponse) { r.RunID = strings.Repeat("0", 32) })
	if body, err := env.call(alice, jsArgs); err != nil || body["status"] != "loader_error" {
		t.Fatalf("mismatched answer: %v %v", body, err)
	}
}

func TestRunsModerationBlock(t *testing.T) {
	s := &screener{decisions: map[string]services.RunScreenDecision{services.RunsSurfaceCode: {Action: "block", Reason: "malware"}}}
	env := newRunsEnv(t, runsOpts{config: `,"screen":"required"`, screener: s})
	body, err := env.call(alice, jsArgs)
	if err != nil {
		t.Fatal(err)
	}
	mod := body["moderation"].(map[string]any)
	if body["status"] != "blocked" || mod["action"] != "block" || mod["surface"] != "run.code" || body["receipt"] != nil {
		t.Fatalf("body: %v", body)
	}
	if len(env.loader.calls()) != 0 {
		t.Fatal("blocked code never reaches the loader")
	}
	if h := env.lastHold(); h.Units != 0 || h.State != "committed" {
		t.Fatalf("a blocked run costs nothing: %+v", h)
	}
	// The same code again is blocked from the review table, without a second screen.
	if body, err = env.call(alice, jsArgs); err != nil || body["status"] != "blocked" || s.seen[services.RunsSurfaceCode] != 1 {
		t.Fatalf("second submission: %v %v seen %v", body, err, s.seen)
	}
}

func TestRunsModerationHoldThenApproval(t *testing.T) {
	s := &screener{decisions: map[string]services.RunScreenDecision{services.RunsSurfaceCode: {Action: "hold", Reason: "needs a look"}}}
	env := newRunsEnv(t, runsOpts{config: `,"screen":"required"`, screener: s})
	body, err := env.call(alice, jsArgs)
	if err != nil || body["status"] != "held" || len(env.loader.calls()) != 0 {
		t.Fatalf("held: %v %v", body, err)
	}
	var state string
	_ = env.db.QueryRow("SELECT state FROM runs_reviews").Scan(&state)
	if state != "held" {
		t.Fatalf("review row: %q", state)
	}
	// An action the provider does not know is a hold too.
	s.decisions[services.RunsSurfaceCode] = services.RunScreenDecision{Action: "maybe"}
	if body, _ = env.call(alice, `{"language":"javascript","code":"other"}`); body["status"] != "held" {
		t.Fatalf("an unknown action holds: %v", body)
	}
	// The steward approves the exact code: it runs.
	if _, err = env.db.Exec("UPDATE runs_reviews SET state='approved', decided_at=1 WHERE state='held'"); err != nil {
		t.Fatal(err)
	}
	if body, err = env.call(alice, jsArgs); err != nil || body["status"] != "ok" || len(env.loader.calls()) != 1 {
		t.Fatalf("approved: %v %v", body, err)
	}
}

func TestRunsScreenRequiredWithoutScreenerFailsClosed(t *testing.T) {
	env := newRunsEnv(t, runsOpts{config: `,"screen":"required"`})
	if _, err := env.call(alice, jsArgs); code(err) != "service_unavailable" {
		t.Fatalf("no screener, no runs: %v", err)
	}
}

func TestRunsStaticScreenerBlocksMiners(t *testing.T) {
	env := newRunsEnv(t, runsOpts{})
	body, err := env.call(alice, `{"language":"javascript","code":"fetch('stratum+tcp://pool.example:3333')"}`)
	if err != nil || body["status"] != "blocked" {
		t.Fatalf("miner marker: %v %v", body, err)
	}
}

func TestRunsEgressFlagging(t *testing.T) {
	env := newRunsEnv(t, runsOpts{})
	env.loader.answer = okAnswer(func(r *services.LoaderResponse) {
		if r.Network == "on" {
			r.Egress.Blocked = 1
			r.Egress.Log = append(r.Egress.Log, services.EgressEntry{Seq: 1, TMs: 9, Method: "GET", Host: "xmr.nanopool.org", Port: 443, PathSHA256: strings.Repeat("b", 64), Verdict: "blocked", Reason: "mining_pool"})
		}
	})
	body, err := env.call(alice, `{"language":"javascript","code":"x","network":{}}`)
	if err != nil {
		t.Fatal(err)
	}
	mod, _ := body["moderation"].(map[string]any)
	if body["flagged"] != true || mod["surface"] != "run.egress" || mod["action"] != "block" || body["receipt"] == nil {
		t.Fatalf("flagged body: %v", body)
	}
	row := env.logRow(body["run_id"].(string))
	if row["flagged"] != int64(1) || !strings.HasPrefix(row["egress_decision"].(string), "block") {
		t.Fatalf("log: %v", row)
	}
	// A block suspends the account's network: the next run is network-off.
	body, err = env.call(alice, `{"language":"javascript","code":"x","network":{}}`)
	if err != nil || body["network"] != "off" || body["network_note"] != "account_suspended" {
		t.Fatalf("suspended: %v %v", body, err)
	}
	// Other accounts keep theirs.
	bob := allowance.Subject{ID: "acct-bob", KeyID: "k-bob", Signed: true}
	env.loader.answer = okAnswer(nil)
	if body, err = env.call(bob, `{"language":"javascript","code":"x","network":{}}`); err != nil || body["network"] != "on" {
		t.Fatalf("bob: %v %v", body, err)
	}
	// A hold flags without suspending.
	s := &screener{decisions: map[string]services.RunScreenDecision{services.RunsSurfaceEgress: {Action: "hold", Reason: "odd"}}}
	env = newRunsEnv(t, runsOpts{config: `,"screen":"required"`, screener: s})
	if body, err = env.call(alice, `{"language":"javascript","code":"x","network":{}}`); err != nil || body["flagged"] != true {
		t.Fatalf("held egress: %v %v", body, err)
	}
	if body, err = env.call(alice, `{"language":"javascript","code":"x","network":{}}`); err != nil || body["network"] != "on" {
		t.Fatalf("a hold does not suspend: %v %v", body, err)
	}
	if s.seen[services.RunsSurfaceEgress] != 2 {
		t.Fatalf("every networked run's egress is screened: %v", s.seen)
	}
}

func TestRunsDailyCaps(t *testing.T) {
	env := newRunsEnv(t, runsOpts{config: `,"caps":{"account_runs_day":2,"global_runs_day":3}`})
	for i := 0; i < 2; i++ {
		if body, err := env.call(alice, jsArgs); err != nil || body["status"] != "ok" {
			t.Fatalf("run %d: %v %v", i, body, err)
		}
	}
	body, err := env.call(alice, jsArgs)
	if err != nil || body["status"] != "cap_reached" {
		t.Fatalf("third run: %v %v", body, err)
	}
	if h := env.lastHold(); h.Units != 0 {
		t.Fatalf("a capped run costs nothing: %+v", h)
	}
	bob := allowance.Subject{ID: "acct-bob", KeyID: "k-bob", Signed: true}
	if body, _ = env.call(bob, jsArgs); body["status"] != "ok" {
		t.Fatalf("bob's first: %v", body)
	}
	if body, _ = env.call(bob, jsArgs); body["status"] != "cap_reached" {
		t.Fatalf("the global cap: %v", body)
	}
	if n := len(env.loader.calls()); n != 3 {
		t.Fatalf("capped runs never reach the loader: %d calls", n)
	}
	// The CPU cap reserves the limit at admission.
	env = newRunsEnv(t, runsOpts{config: `,"caps":{"account_cpu_ms_day":1500}`})
	if body, _ = env.call(alice, jsArgs); body["status"] != "ok" {
		t.Fatalf("first: %v", body)
	}
	if body, _ = env.call(alice, jsArgs); body["status"] != "ok" {
		t.Fatalf("12 ms used of 1500, a 1000 ms run fits: %v", body)
	}
	if body, _ = env.call(alice, `{"language":"javascript","code":"x","cpu_ms":1000}`); body["status"] != "ok" {
		t.Fatalf("24 ms used: %v", body)
	}
	if body, _ = env.call(alice, `{"language":"javascript","code":"x","cpu_ms":1000}`); body["status"] != "ok" {
		t.Fatalf("36 ms used: %v", body)
	}
}

func TestRunsSafeDialerRefusesLoopback(t *testing.T) {
	env := newRunsEnv(t, runsOpts{safe: true})
	if _, err := env.call(alice, jsArgs); code(err) != "upstream_failed" {
		t.Fatalf("the safe dialer must refuse a loopback loader: %v", err)
	}
	if len(env.loader.calls()) != 0 {
		t.Fatal("nothing reached the loopback loader")
	}
}

func TestRunsArgsAndPrice(t *testing.T) {
	env := newRunsEnv(t, runsOpts{})
	for _, args := range []string{
		`{"language":"ruby","code":"x"}`,
		`{"language":"javascript"}`,
		`{"language":"javascript","code":""}`,
		`{"language":"javascript","code":"x","cpu_ms":1001}`,
		`{"language":"javascript","code":"x","cpu_ms":0}`,
		`{"language":"javascript","code":"x","wall_ms":50}`,
		`{"language":"javascript","code":"x","network":{"max_requests":17}}`,
		`{"language":"javascript","code":"x","network":{"connect":true}}`,
		`{"language":"javascript","code":"x","extra":1}`,
		`{"language":"javascript","code":"` + strings.Repeat("a", services.RunsCodeBytes+1) + `"}`,
		`{"language":"javascript","code":"x","input":"` + strings.Repeat("a", services.RunsInputBytes) + `"}`,
	} {
		if _, err := env.call(alice, args); code(err) != "invalid_service_data" {
			t.Errorf("%.80s: %v", args, err)
		}
	}
	tx, _ := env.db.Begin()
	defer tx.Rollback()
	_, err := env.engine.Call(context.Background(), tx, services.Request{Service: "runs", Data: `{"schema":1,"method":"run","args":` + jsArgs + `,"max_cost":1009}`, Subject: alice, RequestKey: "id:cheap"}, time.Now().Unix())
	if code(err) != "price_exceeds_max" {
		t.Fatalf("a max_cost below the quote: %v", err)
	}
	if len(env.loader.calls()) != 0 {
		t.Fatal("refused calls never reach the loader")
	}
}

func TestRunsConfigValidation(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "s")
	short := filepath.Join(dir, "short")
	_ = os.WriteFile(secret, []byte(runsSecret), 0o600)
	_ = os.WriteFile(short, []byte("tooshort"), 0o600)
	base := `"secret_file":%q,"languages":["javascript"],"network":"off"`
	good := fmt.Sprintf(`{"loader_url":"https://runs.example.workers.dev",`+base+`}`, secret)
	path := filepath.Join(dir, "runs.json")
	_ = os.WriteFile(path, []byte(good), 0o600)
	cfg, err := services.LoadRunsConfig(path)
	if err != nil || cfg.Limits.CPUMsMax != 10_000 || cfg.Caps.AccountRunsDay != 50 || cfg.Screen != "required" || !*cfg.Egress.Resolve {
		t.Fatalf("good config with defaults: %+v %v", cfg, err)
	}
	// The network is off unless the config allows it.
	_ = os.WriteFile(path, []byte(fmt.Sprintf(`{"loader_url":"https://runs.example.workers.dev","secret_file":%q,"languages":["javascript"]}`, secret)), 0o600)
	if cfg, err = services.LoadRunsConfig(path); err != nil || cfg.Network != "off" {
		t.Fatalf("network defaults to off: %+v %v", cfg, err)
	}
	for _, bad := range []string{
		fmt.Sprintf(`{"loader_url":"http://runs.example",`+base+`}`, secret),
		fmt.Sprintf(`{"loader_url":"https://u:p@runs.example",`+base+`}`, secret),
		fmt.Sprintf(`{"loader_url":"https://runs.example/run?x=1",`+base+`}`, secret),
		fmt.Sprintf(`{"loader_url":"https://runs.example",`+base+`}`, short),
		fmt.Sprintf(`{"loader_url":"https://runs.example",`+base+`,"extra":1}`, secret),
		fmt.Sprintf(`{"loader_url":"https://runs.example","secret_file":%q,"languages":["ruby"],"network":"off"}`, secret),
		fmt.Sprintf(`{"loader_url":"https://runs.example","secret_file":%q,"languages":["javascript"],"network":"on"}`, secret),
		fmt.Sprintf(`{"loader_url":"https://runs.example",`+base+`,"limits":{"cpu_ms_max":30001}}`, secret),
		fmt.Sprintf(`{"loader_url":"https://runs.example",`+base+`,"limits":{"cpu_ms_max":100,"cpu_ms_default":200}}`, secret),
		fmt.Sprintf(`{"loader_url":"https://runs.example",`+base+`,"egress":{"deny_hosts":["Bad Host"]}}`, secret),
		fmt.Sprintf(`{"loader_url":"https://runs.example",`+base+`,"egress":{"deny_cidrs":["10.0.0.0/33"]}}`, secret),
		fmt.Sprintf(`{"loader_url":"https://runs.example",`+base+`,"screen":"none"}`, secret),
	} {
		_ = os.WriteFile(path, []byte(bad), 0o600)
		if _, err := services.LoadRunsConfig(path); err == nil {
			t.Errorf("must refuse %s", bad)
		} else if strings.Contains(err.Error(), runsSecret) {
			t.Fatalf("an error must never carry the secret: %v", err)
		}
	}
}

// slowScreener never answers before its context ends.
type slowScreener struct{}

func (slowScreener) Screen(ctx context.Context, surface, subject, content string) services.RunScreenDecision {
	<-ctx.Done()
	return services.RunScreenDecision{Action: "allow"}
}

func TestRunsScreenTimeoutHoldsWithoutRecording(t *testing.T) {
	env := newRunsEnv(t, runsOpts{config: `,"screen":"required"`, screener: slowScreener{}})
	body, err := env.call(alice, jsArgs)
	if err != nil || body["status"] != "held" || len(env.loader.calls()) != 0 {
		t.Fatalf("a screen that does not answer holds the run: %v %v", body, err)
	}
	var n int
	_ = env.db.QueryRow("SELECT count(*) FROM runs_reviews").Scan(&n)
	if n != 0 {
		t.Fatal("a timed-out screen must not hold the code for good")
	}
}

func TestRunsUnconfiguredIsUnavailable(t *testing.T) {
	db := openDB(t)
	reg := services.NewBuiltinRegistry([]string{"runs"}, services.Deps{DB: db})
	e := services.NewEngine(services.Config{DB: db, Registry: reg, Meter: servicestest.NewMeter(100), Now: func() int64 { return 1 }})
	tx, _ := db.Begin()
	defer tx.Rollback()
	_, err := e.Call(context.Background(), tx, services.Request{Service: "runs", Data: `{"schema":1,"method":"run","args":` + jsArgs + `,"max_cost":10}`, Subject: alice, RequestKey: "id:1"}, 1)
	if code(err) != "service_unavailable" {
		t.Fatalf("no RUNS_CONFIG: %v", err)
	}
}

func FuzzLoaderResponse(f *testing.F) {
	for _, net := range []bool{false, true} {
		_, b, _ := okAnswer(nil)(map[string]any{"run_id": strings.Repeat("a", 32), "network": map[string]any{"enabled": net}})
		f.Add(b)
	}
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"schema":1,"run_id":"` + strings.Repeat("0", 32) + `","status":"ok","cpu_source":"tail","network":"off","egress":{"log":[{"method":"GET","verdict":"allowed","path_sha256":""}]}}`))
	f.Add([]byte(`{"schema":1,"egress":{"log":[` + strings.Repeat(`{},`, 10) + `{}]}}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		r, err := services.ParseLoaderResponse(raw)
		if err != nil {
			return
		}
		if r.Schema != 1 || len(r.RunID) != 32 || r.CPUMs < 0 || r.WallMs < 0 || len(r.Stdout) > services.RunsOutputBytesMax ||
			len(r.Stderr) > services.RunsOutputBytesMax || len(r.Egress.Log) > services.RunsEgressLogMax ||
			(r.ResultJSON != "" && !json.Valid([]byte(r.ResultJSON))) || (r.Network == "off" && r.Egress.BytesIn+r.Egress.BytesOut > 0) {
			t.Fatalf("accepted out-of-bounds answer: %q", raw)
		}
		// An accepted answer survives a round trip unchanged.
		again, _ := json.Marshal(r)
		r2, err := services.ParseLoaderResponse(again)
		if err != nil {
			t.Fatalf("re-encoded answer refused: %v\n%s", err, again)
		}
		b1, _ := json.Marshal(r2)
		if string(b1) != string(again) {
			t.Fatalf("round trip differs:\n%s\n%s", again, b1)
		}
	})
}
