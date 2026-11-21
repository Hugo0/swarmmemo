package board

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
)

// seedRefusedCall is a fetch.page call the site refused, as the engine
// settles it (C1157): failed, free, its public record the refusal.
func seedRefusedCall(t *testing.T, s *Store, id, account, code, record string, created int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO service_calls(id,account,service,method,request_key,hold_id,resource,cost,state,error,body,public,prices_version,created_at,finished_at)
VALUES(?,?,'fetch','page',?,'','credit',0,'failed',?,'',?,1,?,?)`, id, account, "rk-"+id, code, record, created, created); err != nil {
		t.Fatal(err)
	}
}

// C1157: a refused fetch's record follows the rule every call's entry does:
// a signed caller's is its own until it cites it (service.cite), then public
// at /api/calls/ID (state refused, the target, the code and the robots.txt
// line, cost 0), and private again once uncited; owner erasure clears it.
// An unsigned caller's, which no one can cite, is public from the start and
// names no one. A failed call with no record stays private.
func TestRefusedFetchIsOnRecord(t *testing.T) {
	s := toolHistoryStore(t)
	u := keyFor(181)
	register(t, s, u)
	run(t, s, signed(u, Command{Operation: "agent.register", Handle: "trier"}))
	me := keyID(u)
	hex := func(n int) string { return fmt.Sprintf("%032x", n) }
	seedRefusedCall(t, s, hex(1), me, "fetch_robots", `{"refused":"fetch_robots","target":"https://a.example/private/x","robots_line":"Disallow: /private"}`, testTime-100)
	seedRefusedCall(t, s, hex(2), "anon:"+strings.Repeat("7", 32), "fetch_captcha", `{"refused":"fetch_captcha","target":"https://b.example/"}`, testTime-90)
	seedRefusedCall(t, s, hex(3), me, "fetch_address_blocked", `{}`, testTime-80)

	if _, ok, err := s.CitedCall(testContext, hex(1)); ok || err != nil {
		t.Fatalf("an uncited signed refusal is public: %v", err)
	}
	calls, _ := historyPage(t, s, u, Command{Target: "fetch"})
	if len(calls) != 2 || calls[0]["call_id"] != hex(3) || calls[0]["state"] != "failed" || calls[1]["state"] != "refused" || calls[1]["error"] != "fetch_robots" || calls[1]["cited"] != false || calls[1]["url"] != nil {
		t.Fatalf("listed: %v", calls)
	}

	res := run(t, s, signed(u, Command{Operation: "service.cite", Target: hex(1), Data: `{"schema":1,"public":true}`, RequestID: randomID()}))
	if res.Data["cited"] != true || res.Data["changed"] != true || res.Data["url"] != CitedPath+hex(1) {
		t.Fatalf("cite a refusal: %v", res.Data)
	}
	page, ok, err := s.CitedCall(testContext, hex(1))
	if err != nil || !ok {
		t.Fatalf("a cited refusal is not public: %v %v", ok, err)
	}
	raw, _ := json.Marshal(page)
	var got struct {
		Call  map[string]any `json:"call"`
		Agent map[string]any `json:"agent"`
		Note  string         `json:"note"`
	}
	json.Unmarshal(raw, &got)
	c := got.Call
	if c["state"] != "refused" || c["tool"] != "fetch.page" || c["error"] != "fetch_robots" || c["target"] != "https://a.example/private/x" ||
		c["robots_line"] != "Disallow: /private" || c["cost"] != float64(0) || c["at"] != float64(testTime-100) || c["cited"] != true || c["url"] != CitedPath+hex(1) {
		t.Fatalf("refused record: %s", raw)
	}
	if got.Agent["handle"] != "trier" || got.Agent["fingerprint"] != me || !strings.HasPrefix(got.Note, "A fetch the site refused") {
		t.Fatalf("agent or note: %s", raw)
	}
	run(t, s, signed(u, Command{Operation: "service.cite", Target: hex(1), Data: `{"schema":1,"public":false}`, RequestID: randomID()}))
	if _, ok, _ := s.CitedCall(testContext, hex(1)); ok {
		t.Fatal("an uncited refusal is still public")
	}

	page, ok, err = s.CitedCall(testContext, hex(2))
	raw, _ = json.Marshal(page)
	if err != nil || !ok || !strings.Contains(string(raw), `"error":"fetch_captcha"`) || !strings.Contains(string(raw), `"anonymous":true`) || strings.Contains(string(raw), "anon:") {
		t.Fatalf("an unsigned refusal: %v %v %s", ok, err, raw)
	}
	if _, ok, _ := s.CitedCall(testContext, hex(3)); ok {
		t.Fatal("a failed call with no record is public")
	}

	run(t, s, signed(u, Command{Operation: "service.cite", Target: hex(1), Data: `{"schema":1,"public":true}`, RequestID: randomID()}))
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err = EraseToolHistory(testContext, tx, me); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.CitedCall(testContext, hex(1)); ok {
		t.Fatal("an erased account's refusal is still public")
	}
}

// C1157: a site refusal on record says so and carries details.call_id,
// beside details.public_data when the site has a dataset; the robots.txt
// line stays in the message alone.
func TestFetchRefusalNamesItsRecord(t *testing.T) {
	id := strings.Repeat("ab", 16)
	const call = `public_data.fetch {"dataset":"crypto_spot_price","params":{"coin_id":"bitcoin"}}`
	hint := map[string]any{"dataset": "crypto_spot_price", "call": call}
	for _, c := range []struct {
		code    string
		details map[string]any
		keys    int
	}{
		{"fetch_captcha", map[string]any{"call_id": id}, 1},
		{"fetch_blocked", map[string]any{"call_id": id, "public_data": hint}, 2},
		{"fetch_site_rate_limited", map[string]any{"call_id": id}, 1},
		{"fetch_robots", map[string]any{"call_id": id, "robots_line": "Disallow: /x"}, 1},
		{"fetch_robots", map[string]any{"call_id": id, "robots_line": "Disallow: /x", "public_data": hint}, 2},
	} {
		var be *Error
		if !errors.As(ServiceRefusal(&allowance.Err{Code: c.code, Details: c.details}), &be) || be.Code != c.code {
			t.Fatalf("%s: %#v", c.code, be)
		}
		d, _ := be.Details.(map[string]any)
		if d["call_id"] != id || len(d) != c.keys || !strings.Contains(be.Message, "yours alone until you cite it") ||
			!strings.Contains(be.Message, "service.cite with target "+id+` and data {"schema":1,"public":true}`) {
			t.Fatalf("%s: %q %#v", c.code, be.Message, be.Details)
		}
		// An unsigned caller's record is public already: the message says so,
		// and the flag that says it never reaches the details.
		anon := map[string]any{services.RefusalPublicKey: true}
		for k, v := range c.details {
			anon[k] = v
		}
		if !errors.As(ServiceRefusal(&allowance.Err{Code: c.code, Details: anon}), &be) {
			t.Fatalf("%s anonymous: %v", c.code, be)
		}
		if d, _ = be.Details.(map[string]any); d["call_id"] != id || len(d) != c.keys || !strings.Contains(be.Message, "public at "+CitedPath+id) || strings.Contains(be.Message, "service.cite") {
			t.Fatalf("%s anonymous: %q %#v", c.code, be.Message, be.Details)
		}
		if c.code == "fetch_robots" && (!strings.HasSuffix(be.Message, " Nothing was charged.") || !strings.Contains(be.Message, "`Disallow: /x`")) {
			t.Fatalf("robots message: %q", be.Message)
		}
	}
	// A call_id that is not one is dropped.
	var be *Error
	if errors.As(ServiceRefusal(&allowance.Err{Code: "fetch_captcha", Details: map[string]any{"call_id": "notary:1"}}), &be); be.Details != nil || strings.Contains(be.Message, "on record") {
		t.Fatalf("bad call_id: %q %#v", be.Message, be.Details)
	}
}
