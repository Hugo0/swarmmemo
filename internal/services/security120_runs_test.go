package services_test

// Security review 1.20 regression tests for the runs provider. Each one
// inverts a proof of concept (TestSecPoC_*, branch security-review-1.20): it
// fails while the weakness is present.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/services"
)

// H4: a network run whose loader answer never arrives in time is no longer
// free and unscreened. It is charged at its limits (CPU and egress), its
// maximum egress counts against the global egress cap, the egress screen
// runs, and the log keeps a signed receipt.
func TestSec120_StalledLoaderRunIsChargedAndScreened(t *testing.T) {
	sc := &screener{decisions: map[string]services.RunScreenDecision{
		services.RunsSurfaceEgress: {Action: "block", Reason: "reached a mining pool"},
	}}
	env := newRunsEnv(t, runsOpts{screener: sc, config: `,"screen":"required"`, grace: 200 * time.Millisecond})
	env.loader.answer = func(req map[string]any) (int, []byte, time.Duration) {
		_, b, _ := okAnswer(nil)(req)
		return 200, b, 2 * time.Second
	}
	body, err := env.call(alice, `{"language":"javascript","code":"x","wall_ms":100,"network":{}}`)
	if err != nil || body["status"] != "loader_error" || body["receipt"] == nil || body["flagged"] != true {
		t.Fatalf("a stalled run: %v %v", body, err)
	}
	if env.loader.calls()[0]["network"].(map[string]any)["enabled"] != true {
		t.Fatal("the run had the network")
	}
	h := env.lastHold()
	if h.State != "committed" || h.Units != h.MaxUnits || h.MaxUnits <= 1010 {
		t.Fatalf("the run is charged at its limits, egress included: %+v", h)
	}
	var egress int64
	_ = env.db.QueryRow("SELECT egress_bytes FROM runs_usage WHERE account=''").Scan(&egress)
	if egress == 0 {
		t.Fatal("the run's egress never reached the global egress counter")
	}
	sc.mu.Lock()
	screened := sc.seen[services.RunsSurfaceEgress]
	sc.mu.Unlock()
	if screened != 1 {
		t.Fatalf("the egress screen ran %d times", screened)
	}
	var blocks int
	_ = env.db.QueryRow("SELECT count(*) FROM runs_network_blocks").Scan(&blocks)
	if blocks != 1 {
		t.Fatal("the egress screen's block did not suspend the account's network")
	}
	row := env.logRow(body["run_id"].(string))
	if row["status"] != "loader_error" || row["egress"] != egress || row["egress_sha256"] == "" || row["flagged"] != int64(1) {
		t.Fatalf("log: %v", row)
	}
	var receipt string
	_ = env.db.QueryRow("SELECT receipt FROM runs_log WHERE run_id=?", body["run_id"]).Scan(&receipt)
	if receipt == "" {
		t.Fatal("no receipt was kept")
	}
}

type contentScreener struct {
	screener
	contents []string
}

func (r *contentScreener) Screen(ctx context.Context, surface, subject, content string) services.RunScreenDecision {
	r.mu.Lock()
	r.contents = append(r.contents, surface+" "+content)
	r.mu.Unlock()
	return r.screener.Screen(ctx, surface, subject, content)
}

// M8: the code screen sees the run's input with the code, so a small
// interpreter can no longer pass the screen once and take its payload as
// input; and an approval of code with one input does not carry over to
// another.
func TestSec120_RunsCodeScreenSeesInput(t *testing.T) {
	rs := &contentScreener{}
	env := newRunsEnv(t, runsOpts{screener: rs, config: `,"screen":"required"`})
	args := `{"language":"javascript","code":"export async function run(i) { return interpret(i.program) }","input":{"program":"stratum+tcp://pool.example xmrig payload"}}`
	if _, err := env.call(alice, args); err != nil {
		t.Fatal(err)
	}
	rs.mu.Lock()
	seen := strings.Join(rs.contents, "\n")
	rs.mu.Unlock()
	if !strings.Contains(seen, "xmrig") {
		t.Fatalf("the input was not screened: %s", seen)
	}
	// The built-in screen now blocks the payload arriving as input.
	env = newRunsEnv(t, runsOpts{})
	body, err := env.call(alice, args)
	if err != nil || body["status"] != "blocked" {
		t.Fatalf("a miner payload as input: %v %v", body, err)
	}
	if services.RunScreenKey("c", json.RawMessage(`{"a":1}`)) == services.RunScreenKey("c", json.RawMessage(`{"a":2}`)) ||
		services.RunScreenKey("c", json.RawMessage(`null`)) != services.RunScreenKey("c", nil) {
		t.Fatal("screen keys must follow the input")
	}
}

// H4: a loader that refused the request up front (it never ran) still
// charges nothing.
func TestSec120_RefusedLoaderRequestIsStillFree(t *testing.T) {
	env := newRunsEnv(t, runsOpts{})
	env.loader.answer = func(req map[string]any) (int, []byte, time.Duration) {
		return 409, []byte(`{"error":"replayed"}`), 0
	}
	if _, err := env.call(alice, jsArgs); code(err) != "upstream_failed" {
		t.Fatalf("a refused request: %v", err)
	}
	if h := env.lastHold(); h.State != "refunded" {
		t.Fatalf("nothing ran, nothing is charged: %+v", h)
	}
}
