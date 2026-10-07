package board

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
)

// openServiceTest opens a store with memory and echo enabled, spending
// through the fake ledger.
func openServiceTest(t *testing.T) (*Store, *servicestest.Meter, *servicestest.Params) {
	t.Helper()
	s := openTest(t, Config{Features: Features{Services: []string{"echo", "memory"}}})
	meter, params := servicestest.NewMeter(1<<30), &servicestest.Params{}
	s.UseServiceMeter(meter, params)
	t.Cleanup(s.stopServices)
	return s, meter, params
}

func svcSetNow(s *Store, at int64) { s.now = func() time.Time { return time.Unix(at, 0) } }

func svcData(method string, args any, maxCost int64) string {
	d := map[string]any{"schema": 1, "method": method, "args": args}
	if maxCost >= 0 {
		d["max_cost"] = maxCost
	}
	raw, _ := json.Marshal(d)
	return string(raw)
}

func svcCall(key ed25519.PrivateKey, service, method string, args any, maxCost int64, requestID string) Command {
	return signed(key, Command{Operation: "service.call", Target: service, Data: svcData(method, args, maxCost), RequestID: requestID})
}

func svcRead(key ed25519.PrivateKey, service, method string, args any) Command {
	c := Command{Operation: "service.read", Target: service, Data: svcData(method, args, -1)}
	if key != nil {
		c = signed(key, c)
	}
	return c
}

// field digs a value out of a result's data by path.
func svcField(t *testing.T, v any, path ...string) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var cur any
	_ = json.Unmarshal(raw, &cur)
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("no %q in %s", p, raw)
		}
		cur = m[p]
	}
	return cur
}

func svcErr(err error) (string, int) {
	var e *Error
	if errors.As(err, &e) {
		return e.Code, e.RetryAfter
	}
	return fmt.Sprint(err), 0
}

func svcSpends(t *testing.T, s *Store, m *servicestest.Meter) []servicestest.Entry {
	t.Helper()
	entries, err := m.Entries(testContext, s.db)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestServicesOffAnswersAsBefore(t *testing.T) {
	s := openTest(t, Config{})
	fails(t, s, Command{Operation: "services.list"}, "service_unavailable")
	fails(t, s, Command{Operation: "service.read", Target: "memory", Data: svcData("get", map[string]string{"key": "a"}, -1)}, "service_unavailable")
	fails(t, s, Command{Operation: "service.call", Target: "memory", Data: "{}"}, "signature_required")
	fails(t, s, svcCall(keyFor(1), "memory", "put", map[string]string{"key": "a", "value": "b"}, 300, ""), "service_unavailable")
	// The data bound stays today's while services are off.
	fails(t, s, signed(keyFor(1), Command{Operation: "service.call", Target: "memory", Data: strings.Repeat("x", 65537)}), "field_limit")
	if s.services.engine != nil {
		t.Fatal("no engine may be built while SERVICES is empty")
	}
}

func TestMemoryEndToEnd(t *testing.T) {
	s, meter, _ := openServiceTest(t)
	owner, other := keyFor(1), keyFor(2)
	agent := keyID(owner)

	put := run(t, s, svcCall(owner, "memory", "put", map[string]string{"key": "a/notes", "value": "hello"}, 300, "p1"))
	if svcField(t, put.Data, "call", "state") != "done" || svcField(t, put.Data, "call", "cost") != float64(256+7+5) || svcField(t, put.Data, "result", "version") != float64(1) || svcField(t, put.Data, "result", "visibility") != "private" {
		t.Fatalf("put: %+v", put.Data)
	}
	if svcField(t, put.Data, "receipt", "used") != float64(268) {
		t.Fatalf("receipt: %+v", put.Data)
	}
	entries := svcSpends(t, s, meter)
	if len(entries) != 1 || entries[0].Kind != "spend" || entries[0].Resource != "memory_bytes" || entries[0].Units != 268 || entries[0].Service != "memory" || entries[0].Method != "put" || entries[0].Account != agent {
		t.Fatalf("one memory receipt in the ledger: %+v", entries)
	}
	var public string
	if err := s.db.QueryRow("SELECT public FROM service_calls WHERE service='memory'").Scan(&public); err != nil || public != `{"bytes":12}` {
		t.Fatalf("the public call record shows sizes only, never keys: %q %v", public, err)
	}

	got := run(t, s, svcRead(owner, "memory", "get", map[string]string{"key": "a/notes"}))
	if svcField(t, got.Data, "result", "value") != "hello" {
		t.Fatalf("owner get: %+v", got.Data)
	}
	// Private: to anyone else the item does not exist.
	fails(t, s, svcRead(nil, "memory", "get", map[string]string{"key": "a/notes", "agent": agent}), "memory_not_found")
	fails(t, s, svcRead(other, "memory", "get", map[string]string{"key": "a/notes", "agent": agent}), "memory_not_found")
	fails(t, s, svcRead(nil, "memory", "get", map[string]string{"key": "a/notes"}), "signature_required")
	fails(t, s, svcRead(nil, "memory", "get", map[string]string{"key": "a/notes", "agent": strings.Repeat("f", 64)}), "memory_not_found")

	run(t, s, svcCall(owner, "memory", "put", map[string]string{"key": "b", "value": "shared", "visibility": "public"}, 300, "p2"))
	anon := run(t, s, svcRead(nil, "memory", "get", map[string]string{"key": "b", "agent": agent}))
	if svcField(t, anon.Data, "result", "value") != "shared" {
		t.Fatalf("public read: %+v", anon.Data)
	}
	list := run(t, s, svcRead(owner, "memory", "list", map[string]any{}))
	if items := svcField(t, list.Data, "result", "items").([]any); len(items) != 2 || svcField(t, list.Data, "result", "usage", "keys") != float64(2) {
		t.Fatalf("owner list: %+v", list.Data)
	}
	if items := svcField(t, run(t, s, svcRead(nil, "memory", "list", map[string]any{"agent": agent})).Data, "result", "items").([]any); len(items) != 1 {
		t.Fatalf("others list public items only: %v", items)
	}
	page := run(t, s, svcRead(owner, "memory", "list", map[string]any{"limit": 1}))
	if svcField(t, page.Data, "result", "next_after") != "a/notes" {
		t.Fatalf("paging: %+v", page.Data)
	}
	if items := svcField(t, run(t, s, svcRead(owner, "memory", "list", map[string]any{"after": "a/notes"})).Data, "result", "items").([]any); len(items) != 1 {
		t.Fatalf("second page: %v", items)
	}
	if items := svcField(t, run(t, s, svcRead(owner, "memory", "list", map[string]any{"prefix": "a/"})).Data, "result", "items").([]any); len(items) != 1 {
		t.Fatalf("prefix: %v", items)
	}

	// An overwrite without visibility makes the item private again.
	over := run(t, s, svcCall(owner, "memory", "put", map[string]string{"key": "b", "value": "mine"}, 300, "p3"))
	if svcField(t, over.Data, "result", "version") != float64(2) {
		t.Fatalf("overwrite: %+v", over.Data)
	}
	fails(t, s, svcRead(nil, "memory", "get", map[string]string{"key": "b", "agent": agent}), "memory_not_found")

	del := run(t, s, svcCall(owner, "memory", "delete", map[string]string{"key": "a/notes"}, 64, "d1"))
	if svcField(t, del.Data, "call", "cost") != float64(64) || svcField(t, del.Data, "result", "usage", "keys") != float64(1) {
		t.Fatalf("delete: %+v", del.Data)
	}
	fails(t, s, svcRead(owner, "memory", "get", map[string]string{"key": "a/notes"}), "memory_not_found")
	before := len(svcSpends(t, s, meter))
	fails(t, s, svcCall(owner, "memory", "delete", map[string]string{"key": "a/notes"}, 64, "d2"), "memory_not_found")
	if len(svcSpends(t, s, meter)) != before {
		t.Fatal("a refused delete must cost nothing")
	}
	var items, bytes, sumItems, sumBytes int64
	_ = s.db.QueryRow("SELECT items,bytes FROM memory_usage WHERE account=?", agent).Scan(&items, &bytes)
	_ = s.db.QueryRow("SELECT count(*),COALESCE(sum(bytes),0) FROM memory_items WHERE account=?", agent).Scan(&sumItems, &sumBytes)
	if items != sumItems || bytes != sumBytes || items != 1 || bytes != 5 {
		t.Fatalf("usage %d/%d must equal the items %d/%d", items, bytes, sumItems, sumBytes)
	}
}

func TestMemoryCapsWriteNothing(t *testing.T) {
	s, meter, _ := openServiceTest(t)
	owner := keyFor(1)
	agent := keyID(owner)
	nothing := func(label string) {
		t.Helper()
		var n int
		_ = s.db.QueryRow("SELECT count(*) FROM memory_items WHERE key NOT IN ('k','full')").Scan(&n)
		if n != 0 {
			t.Fatalf("%s: %d items written", label, n)
		}
	}
	for _, key := range []string{"../x", "a/../b", "/abs", "a//b", "", "sp ace", strings.Repeat("k", 257)} {
		fails(t, s, svcCall(owner, "memory", "put", map[string]string{"key": key, "value": "v"}, 1000, ""), "invalid_memory_key")
	}
	fails(t, s, svcCall(owner, "memory", "put", map[string]string{"key": "big", "value": strings.Repeat("v", MemoryValueBytes+1)}, 1<<20, ""), "invalid_service_data")
	fails(t, s, svcCall(owner, "memory", "put", map[string]string{"key": "nul", "value": "a\x00b"}, 1000, ""), "invalid_service_data")
	fails(t, s, svcCall(owner, "memory", "put", map[string]any{"key": "x", "value": "v", "extra": 1}, 1000, ""), "invalid_service_data")
	nothing("invalid input")
	if len(svcSpends(t, s, meter)) != 0 {
		t.Fatal("refused puts must cost nothing")
	}
	// A full 64 KiB value fits a command.
	full := run(t, s, svcCall(owner, "memory", "put", map[string]string{"key": "full", "value": strings.Repeat("\"q", MemoryValueBytes/2)}, 1<<20, ""))
	if svcField(t, full.Data, "result", "bytes") != float64(4+MemoryValueBytes) {
		t.Fatalf("full value: %+v", full.Data)
	}
	run(t, s, svcCall(owner, "memory", "put", map[string]string{"key": "k", "value": "v"}, 1000, ""))
	// 1,001 keys: seed the counter at the cap.
	if _, err := s.db.Exec("UPDATE memory_usage SET items=? WHERE account=?", MemoryKeysMax, agent); err != nil {
		t.Fatal(err)
	}
	fails(t, s, svcCall(owner, "memory", "put", map[string]string{"key": "new", "value": "v"}, 1000, ""), "memory_limit")
	run(t, s, svcCall(owner, "memory", "put", map[string]string{"key": "k", "value": "w"}, 1000, "")) // an overwrite adds no key
	// 17 MiB: seed the byte counter near the cap.
	if _, err := s.db.Exec("UPDATE memory_usage SET items=2, bytes=? WHERE account=?", MemoryBytesMax-10, agent); err != nil {
		t.Fatal(err)
	}
	fails(t, s, svcCall(owner, "memory", "put", map[string]string{"key": "new", "value": strings.Repeat("v", 100)}, 1000, ""), "memory_limit")
	nothing("over the caps")
	// Out of allowance: nothing written either.
	meter.Budget[allowance.MemoryBytes] = 0
	fails(t, s, svcCall(owner, "memory", "put", map[string]string{"key": "new", "value": "v"}, 1000, ""), "quota_exhausted")
	nothing("out of allowance")
}

func TestServiceCallIdempotency(t *testing.T) {
	s, meter, _ := openServiceTest(t)
	owner := keyFor(1)
	c := svcCall(owner, "memory", "put", map[string]string{"key": "a", "value": "v"}, 300, "same")
	first := run(t, s, c)
	again := run(t, s, c)
	if !reflect.DeepEqual(svcField(t, first.Data), svcField(t, again.Data)) {
		t.Fatalf("an exact retry returns the stored receipt:\n%+v\n%+v", first.Data, again.Data)
	}
	if n := len(svcSpends(t, s, meter)); n != 1 {
		t.Fatalf("a retry storm charges once, got %d", n)
	}
	fails(t, s, svcCall(owner, "memory", "put", map[string]string{"key": "a", "value": "other"}, 300, "same"), "idempotency_conflict")

	// A remote call that "crashes" stays in flight until its hold expires.
	crash := svcCall(owner, "echo", "echo", map[string]any{"text": "x", "simulate": map[string]any{"mode": "remote", "crash": true}}, 10, "crash")
	_, err := s.Execute(testContext, crash, "test-origin")
	if code, _ := svcErr(err); code != "service_unavailable" {
		t.Fatalf("crash: %v", err)
	}
	_, err = s.Execute(testContext, crash, "test-origin")
	if code, retry := svcErr(err); code != "request_in_flight" || retry <= 0 {
		t.Fatalf("a retry while the call runs: %v", err)
	}
	var id string
	var expires int64
	if err = s.db.QueryRow("SELECT id,expires_at FROM service_calls WHERE request_key='id:crash'").Scan(&id, &expires); err != nil {
		t.Fatal(err)
	}
	status := run(t, s, svcRead(owner, "echo", "status", map[string]string{"call": id}))
	if svcField(t, status.Data, "call", "state") != "running" {
		t.Fatalf("status: %+v", status.Data)
	}
	fails(t, s, svcRead(keyFor(2), "echo", "status", map[string]string{"call": id}), "not_found")
	// The hold expires: the sweeper settles it at its maximum, the worker
	// marks the call unknown, and a retry now answers with that.
	svcSetNow(s, expires)
	if _, err = meter.Sweep(testContext, s.db, expires); err != nil {
		t.Fatal(err)
	}
	if n, err := s.WorkServices(testContext); err != nil || n != 1 {
		t.Fatalf("reconcile: %d %v", n, err)
	}
	after, err := s.Execute(testContext, crash, "test-origin")
	if err != nil || svcField(t, after.Data, "call", "state") != "unknown" || svcField(t, after.Data, "call", "cost") != svcField(t, after.Data, "call", "max_cost") {
		t.Fatalf("after expiry: %+v %v", after.Data, err)
	}
	for _, e := range svcSpends(t, s, meter) {
		if e.Kind == "hold" && (e.State != "expired" || e.Units != e.MaxUnits) {
			t.Fatalf("a crashed hold settles at its maximum: %+v", e)
		}
	}
}

func TestPriceExceedsMaxSpendsNothing(t *testing.T) {
	s, meter, params := openServiceTest(t)
	owner := keyFor(1)
	fails(t, s, svcCall(owner, "memory", "put", map[string]string{"key": "a", "value": "v"}, 257, ""), "price_exceeds_max")
	cat := run(t, s, Command{Operation: "services.list"})
	if svcField(t, cat.Data, "prices_version") != float64(0) {
		t.Fatalf("catalogue: %+v", cat.Data)
	}
	// The price is raised between the caller's quote and its call.
	params.Set(1, []byte(`{"schema":1,"prices":{"memory.put":{"base":1000,"per_byte":1}}}`))
	cat = run(t, s, Command{Operation: "services.list"})
	raw, _ := json.Marshal(cat.Data)
	if svcField(t, cat.Data, "prices_version") != float64(1) || !strings.Contains(string(raw), `"base":1000`) {
		t.Fatalf("catalogue shows the new price: %s", raw)
	}
	fails(t, s, svcCall(owner, "memory", "put", map[string]string{"key": "a", "value": "v"}, 300, ""), "price_exceeds_max")
	if len(svcSpends(t, s, meter)) != 0 {
		t.Fatal("price_exceeds_max must spend nothing")
	}
	var n int
	_ = s.db.QueryRow("SELECT count(*) FROM memory_items").Scan(&n)
	if n != 0 {
		t.Fatal("price_exceeds_max must write nothing")
	}
	ok := run(t, s, svcCall(owner, "memory", "put", map[string]string{"key": "a", "value": "v"}, 1002, ""))
	if svcField(t, ok.Data, "call", "prices_version") != float64(1) || svcField(t, ok.Data, "call", "cost") != float64(1002) {
		t.Fatalf("priced under version 1: %+v", ok.Data)
	}
	params.Set(2, []byte(`{"schema":1,"prices":{"memory.nope":{"base":1}}}`))
	fails(t, s, Command{Operation: "services.list"}, "service_unavailable")
}

func svcHolds(t *testing.T, s *Store, m *servicestest.Meter) map[string]servicestest.Entry {
	out := map[string]servicestest.Entry{}
	for _, e := range svcSpends(t, s, m) {
		if e.Kind == "hold" {
			out[strings.TrimPrefix(e.RequestKey, "id:")] = e
		}
	}
	return out
}

func TestEchoEveryPath(t *testing.T) {
	s, meter, _ := openServiceTest(t)
	owner := keyFor(1)
	sent := map[string]Command{} // one signed command per request ID, so a repeat is an exact retry
	echo := func(simulate map[string]any, id string) Command {
		if c, ok := sent[id]; ok {
			return c
		}
		args := map[string]any{"text": "hi"}
		if simulate != nil {
			args["simulate"] = simulate
		}
		sent[id] = svcCall(owner, "echo", "echo", args, 10, id)
		return sent[id]
	}
	// Local.
	local := run(t, s, echo(nil, "local"))
	if svcField(t, local.Data, "call", "mode") != "local" || svcField(t, local.Data, "result", "echo") != "hi" || svcField(t, local.Data, "call", "cost") != float64(2) {
		t.Fatalf("local: %+v", local.Data)
	}
	fails(t, s, echo(map[string]any{"fail": true}, "local-fail"), "service_unavailable")
	fails(t, s, echo(map[string]any{"delay_ms": 5}, "local-delay"), "invalid_service_data")
	fails(t, s, echo(map[string]any{"mode": "remote", "delay_ms": 5001}, "long"), "invalid_service_data")
	if n := len(svcSpends(t, s, meter)); n != 1 {
		t.Fatalf("only the local success is charged, got %d entries", n)
	}

	// Remote: runs after commit, settles in a second transaction.
	remote := run(t, s, echo(map[string]any{"mode": "remote", "delay_ms": 10}, "remote"))
	if svcField(t, remote.Data, "call", "state") != "done" || svcField(t, remote.Data, "call", "mode") != "remote" || svcField(t, remote.Data, "result", "echo") != "hi" {
		t.Fatalf("remote: %+v", remote.Data)
	}
	if h := svcHolds(t, s, meter)["remote"]; h.State != "committed" || h.Units == 0 || h.Units > h.MaxUnits {
		t.Fatalf("remote hold: %+v", h)
	}
	fails(t, s, echo(map[string]any{"mode": "remote", "fail": true}, "remote-fail"), "service_unavailable")
	if h := svcHolds(t, s, meter)["remote-fail"]; h.State != "refunded" {
		t.Fatalf("a failed remote call is refunded: %+v", h)
	}
	_, err := s.Execute(testContext, echo(map[string]any{"mode": "remote", "fail": true}, "remote-fail"), "test-origin")
	if code, _ := svcErr(err); code != "service_unavailable" {
		t.Fatalf("a retry of a failed call repeats its failure: %v", err)
	}
	// An exact retry of a finished remote call returns its final result.
	again := run(t, s, echo(map[string]any{"mode": "remote", "delay_ms": 10}, "remote"))
	if svcField(t, again.Data, "call", "state") != "done" || svcField(t, again.Data, "result", "echo") != "hi" {
		t.Fatalf("remote retry: %+v", again.Data)
	}

	// Async: reserves now, settles when the worker finds it due.
	async := run(t, s, echo(map[string]any{"mode": "async", "delay_ms": 2000}, "async"))
	if svcField(t, async.Data, "call", "state") != "running" || svcField(t, async.Data, "call", "due_at") != float64(testTime+2) {
		t.Fatalf("async: %+v", async.Data)
	}
	id := svcField(t, async.Data, "call", "id").(string)
	if n, _ := s.WorkServices(testContext); n != 0 {
		t.Fatal("nothing is due yet")
	}
	run(t, s, echo(map[string]any{"mode": "async", "delay_ms": 1000, "fail": true}, "async-fail"))
	fails(t, s, echo(map[string]any{"mode": "remote"}, "third"), "hold_limit")
	svcSetNow(s, testTime+2)
	if n, err := s.WorkServices(testContext); err != nil || n != 2 {
		t.Fatalf("two jobs settle when due: %d %v", n, err)
	}
	status := run(t, s, svcRead(owner, "echo", "status", map[string]string{"call": id}))
	if svcField(t, status.Data, "call", "state") != "done" || svcField(t, status.Data, "result", "echo") != "hi" {
		t.Fatalf("async status: %+v", status.Data)
	}
	h := svcHolds(t, s, meter)
	if h["async"].State != "committed" || h["async-fail"].State != "refunded" {
		t.Fatalf("async holds: %+v", h)
	}
	var failedID string
	_ = s.db.QueryRow("SELECT id FROM service_calls WHERE request_key='id:async-fail'").Scan(&failedID)
	failed := run(t, s, svcRead(owner, "echo", "status", map[string]string{"call": failedID}))
	if svcField(t, failed.Data, "call", "state") != "failed" || svcField(t, failed.Data, "call", "error") != "upstream_failed" || svcField(t, failed.Data, "call", "cost") != float64(0) {
		t.Fatalf("failed status: %+v", failed.Data)
	}
	if retry := run(t, s, echo(map[string]any{"mode": "async", "delay_ms": 2000}, "async")); svcField(t, retry.Data, "result", "echo") != "hi" {
		t.Fatalf("async retry: %+v", retry.Data)
	}

	// An async job whose worker keeps crashing is tried a bounded number of
	// times, then left to expire as unknown at its maximum.
	crash := run(t, s, echo(map[string]any{"mode": "async", "crash": true}, "async-crash"))
	crashID := svcField(t, crash.Data, "call", "id").(string)
	now := testTime + 2
	for i := 0; i < 6; i++ {
		if _, err := s.WorkServices(testContext); err != nil {
			t.Fatal(err)
		}
		now += 21 // just past each 20 s lease
		svcSetNow(s, now)
	}
	var attempts int
	var jobState string
	_ = s.db.QueryRow("SELECT attempts,state FROM service_jobs WHERE call_id=?", crashID).Scan(&attempts, &jobState)
	if attempts != services.SettleAttempts || jobState != "failed" {
		t.Fatalf("crash attempts %d state %s", attempts, jobState)
	}
	fresh := signed(owner, Command{Operation: "service.read", Target: "echo", Data: svcData("status", map[string]string{"call": crashID}, -1), Timestamp: now})
	if st := run(t, s, fresh); svcField(t, st.Data, "call", "state") != "unknown" {
		t.Fatalf("crashed async call: %+v", st.Data)
	}
}

func TestRemoteRetryWhileRunningIsInFlight(t *testing.T) {
	s, meter, _ := openServiceTest(t)
	owner := keyFor(1)
	slow := svcCall(owner, "echo", "echo", map[string]any{"text": "slow", "simulate": map[string]any{"mode": "remote", "delay_ms": 400}}, 10, "slow")
	done := make(chan Result, 1)
	go func() {
		r, err := s.Execute(testContext, slow, "test-origin")
		if err != nil {
			t.Error(err)
		}
		done <- r
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		_ = s.db.QueryRow("SELECT count(*) FROM service_calls WHERE state='running'").Scan(&n)
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the call never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, err := s.Execute(testContext, slow, "test-origin")
	if code, retry := svcErr(err); code != "request_in_flight" || retry <= 0 {
		t.Fatalf("retry while running: %v", err)
	}
	first := <-done
	if svcField(t, first.Data, "result", "echo") != "slow" {
		t.Fatalf("first caller: %+v", first.Data)
	}
	if again := run(t, s, slow); svcField(t, again.Data, "result", "echo") != "slow" {
		t.Fatalf("retry after: %+v", again.Data)
	}
	if h := svcHolds(t, s, meter)["slow"]; h.State != "committed" {
		t.Fatalf("hold: %+v", h)
	}
}

func TestServiceReadsAreRateLimited(t *testing.T) {
	s, _, _ := openServiceTest(t)
	owner := keyFor(1)
	run(t, s, svcCall(owner, "memory", "put", map[string]string{"key": "k", "value": "v", "visibility": "public"}, 300, ""))
	get := Command{Operation: "service.read", Target: "memory", Data: svcData("get", map[string]string{"key": "k", "agent": keyID(owner)}, -1)}
	for i := 0; i < MemoryReadsPerMinute; i++ {
		run(t, s, get)
	}
	_, err := s.Execute(testContext, get, "test-origin")
	if code, retry := svcErr(err); code != "request_rate" || retry <= 0 {
		t.Fatalf("read %d: %v", MemoryReadsPerMinute+1, err)
	}
	if _, err = s.Execute(testContext, get, "other-origin"); err != nil {
		t.Fatalf("another caller has its own budget: %v", err)
	}
	svcSetNow(s, testTime+60)
	run(t, s, get)
}

func TestServiceRefusals(t *testing.T) {
	s, _, _ := openServiceTest(t)
	owner := keyFor(1)
	fails(t, s, svcCall(owner, "search", "q", map[string]any{}, 1, ""), "invalid_service")
	fails(t, s, svcCall(owner, "memory", "get", map[string]string{"key": "a"}, 1, ""), "invalid_service_data")
	fails(t, s, svcCall(owner, "memory", "nope", map[string]any{}, 1, ""), "invalid_service_data")
	fails(t, s, signed(owner, Command{Operation: "service.call", Target: "memory", Data: `{"schema":1,"method":"put","args":{"key":"a","value":"b"}}`}), "invalid_service_data")
	fails(t, s, svcRead(owner, "memory", "put", map[string]string{"key": "a", "value": "b"}), "invalid_service_data")
	fails(t, s, svcRead(owner, "echo", "status", map[string]string{"call": "nope"}), "invalid_service_data")
	fails(t, s, svcRead(nil, "echo", "status", map[string]string{"call": strings.Repeat("a", 32)}), "signature_required")
	fails(t, s, svcRead(owner, "echo", "status", map[string]string{"call": strings.Repeat("a", 32)}), "not_found")
	fails(t, s, Command{Operation: "service.call", Target: "memory", Data: "{}"}, "signature_required")
	// Not delegable: a worker key is refused by the operation table.
	if op, _ := LookupOperation("service.call"); op.Delegable {
		t.Fatal("service.call must not be delegable in v1")
	}
}

func TestServiceLimitsMatchBoard(t *testing.T) {
	pairs := [][2]int{
		{services.MemoryKeyBytes, MemoryKeyBytes}, {services.MemoryValueBytes, MemoryValueBytes},
		{services.MemoryKeysMax, MemoryKeysMax}, {services.MemoryBytesMax, MemoryBytesMax},
		{services.MemoryListPageMax, MemoryListPageMax},
		{services.RunsArgsMax, RunArgsBytes}, {services.RunsCodeBytes, RunCodeBytes}, {services.RunsInputBytes, RunInputBytes},
	}
	for i, p := range pairs {
		if p[0] != p[1] {
			t.Errorf("limit %d: services %d, board %d", i, p[0], p[1])
		}
	}
	if !reflect.DeepEqual(KnownServices, services.Known()) {
		t.Fatalf("SERVICES must name exactly the built-in providers: %v %v", KnownServices, services.Known())
	}
	for _, d := range services.NewBuiltinRegistry(services.Known(), services.Deps{}).List() {
		for _, m := range d.Methods {
			if m.ArgsMax+256 > ServiceDataBytes {
				t.Errorf("%s.%s args bound %d must fit a service.call's data (%d)", d.ID, m.Name, m.ArgsMax, ServiceDataBytes)
			}
			if d.ID == "runs" && m.Name == "run" {
				if m.ArgsMax != RunArgsBytes || m.ArgsMax+256 > ServiceDataBytes {
					t.Errorf("runs.run args bound %d must be the published %d and fit a service.call", m.ArgsMax, RunArgsBytes)
				}
				continue
			}
			published := ServiceArgsBytes
			if d.ID == "inference" {
				published = InferenceArgsBytes
			}
			// memory values, notary and screen texts, pastes and doc versions
			// are bounded by their own published limits; tools.call carries
			// the args of the method it routes to, bounded again as that
			// method's.
			if d.ID != "memory" && d.ID != services.ToolsID && d.ID != "notary" && d.ID != "screen" && d.ID != services.PasteID && d.ID != services.DocsID && m.ArgsMax > published {
				t.Errorf("%s.%s args bound %d exceeds the published %d", d.ID, m.Name, m.ArgsMax, published)
			}
		}
	}
}

func TestServicesSchemaReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.sqlite")
	cfg := Config{Features: Features{Services: []string{"memory"}}}
	for i := 0; i < 2; i++ {
		s, err := Open(path, cfg)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		var version int
		if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
			t.Fatalf("user_version %d, want %d: %v", version, SchemaVersion, err)
		}
		var tables int
		_ = s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('service_calls','service_jobs','memory_items','memory_usage')").Scan(&tables)
		if tables != 4 {
			t.Fatalf("service tables: %d", tables)
		}
		s.Close()
	}
}

func TestServicesStartStop(t *testing.T) {
	s, _, _ := openServiceTest(t)
	ctx, cancel := context.WithCancel(testContext)
	s.StartRFC0012(ctx)
	cancel()
	s.StopRFC0012()
}

// Every size refusal states the value sent against its limit, in one form
// (SizeNote), end to end: a post, a request field, and a service's own
// refusal (the memory service, then the board's mapping of each code).
func TestSizeRefusalsStateTheValueSent(t *testing.T) {
	s, _, _ := openServiceTest(t)
	message := func(c Command, code string) string {
		t.Helper()
		_, err := s.Execute(testContext, c, "test-origin")
		var e *Error
		if !errors.As(err, &e) || e.Code != code {
			t.Fatalf("want %s, got %v", code, err)
		}
		return e.Message
	}
	for _, c := range []struct {
		cmd  Command
		code string
		want []string
	}{
		{Command{Operation: "post", Text: strings.Repeat("x", 20000)}, "text_too_large", []string{"(20000/16384 bytes)"}},
		{Command{Operation: "post", Text: "hi", RequestID: strings.Repeat("r", RequestIDBytes+1)}, "field_limit", []string{"request_id", fmt.Sprintf("(%d/%d bytes)", RequestIDBytes+1, RequestIDBytes)}},
		{signed(keyFor(1), Command{Operation: "service.call", Target: "memory", Data: strings.Repeat("x", ServiceDataBytes+1)}), "field_limit", []string{"data", fmt.Sprintf("(%d/%d bytes)", ServiceDataBytes+1, ServiceDataBytes)}},
		{svcCall(keyFor(1), "memory", "put", map[string]string{"key": strings.Repeat("k", MemoryKeyBytes+1), "value": "v"}, 300, ""), "invalid_memory_key", []string{fmt.Sprintf("(%d/%d bytes)", MemoryKeyBytes+1, MemoryKeyBytes)}},
	} {
		m := message(c.cmd, c.code)
		for _, w := range c.want {
			if !strings.Contains(m, w) {
				t.Errorf("%s: %q lacks %q", c.code, m, w)
			}
		}
	}
	for _, code := range []string{"invalid_service_data", "invalid_memory_key", "screen_text_limit", "anonymous_limit"} {
		var e *Error
		if err := serviceError(&allowance.Err{Code: code, Sent: 5000, Limit: 4096}); !errors.As(err, &e) || !strings.Contains(e.Message, "(5000/4096 bytes)") {
			t.Errorf("%s: %v", code, err)
		}
		if err := serviceError(&allowance.Err{Code: code}); !errors.As(err, &e) || strings.Contains(e.Message, " bytes)") {
			t.Errorf("%s without a size: %v", code, err)
		}
	}
}

// An unknown method, or one sent as the wrong operation, names the service's
// methods; a method name the envelope does not admit is never echoed.
func TestUnknownMethodNamesTheMethods(t *testing.T) {
	s, _, _ := openServiceTest(t)
	owner := keyFor(1)
	message := func(c Command) string {
		t.Helper()
		_, err := s.Execute(testContext, c, "test-origin")
		var e *Error
		if !errors.As(err, &e) || e.Code != "invalid_service_data" {
			t.Fatalf("want invalid_service_data, got %v", err)
		}
		return e.Message
	}
	got := message(svcCall(owner, "memory", "set", map[string]any{}, 1, ""))
	if want := `memory has no method "set"; its methods are put, delete, get, list. Each method's args are in services.list.`; got != want {
		t.Errorf("unknown method: %q, want %q", got, want)
	}
	if got = message(svcRead(owner, "memory", "nope", map[string]any{})); !strings.HasPrefix(got, `memory has no method "nope"; its methods are `) {
		t.Errorf("unknown read: %q", got)
	}
	if got = message(svcCall(owner, "memory", "get", map[string]string{"key": "a"}, 1, "")); !strings.Contains(got, `memory method "get" is a read; send it as service.read.`) {
		t.Errorf("read sent as a call: %q", got)
	}
	if got = message(svcRead(owner, "memory", "put", map[string]string{"key": "a", "value": "b"})); !strings.Contains(got, `memory method "put" is a write; send it as service.call.`) {
		t.Errorf("write sent as a read: %q", got)
	}
	long := strings.Repeat("Z", 200) + "\x1b[2J"
	got = message(svcCall(owner, "memory", long, map[string]any{}, 1, ""))
	if strings.Contains(got, "ZZZZ") || strings.Contains(got, "\x1b") || !strings.Contains(got, "method takes a method name") {
		t.Errorf("bad method name: %q", got)
	}
}
