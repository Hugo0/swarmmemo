package board

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"swarmmemo/internal/services"
)

// toolHistoryStore is a board with the free tools on and the clock at
// testTime.
func toolHistoryStore(t *testing.T) *Store {
	t.Helper()
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	s := openTest(t, historyConfig(t, Features{Services: []string{"notary", "memory", "fetch", "screen", "public_data"}, Ledger: LedgerOn}))
	t.Cleanup(s.stopServices)
	setRewardParams(t, s)
	s.now = at(testTime)
	return s
}

// historyConfig turns fetch and public data on without upstreams.
func historyConfig(t testing.TB, f Features) Config {
	t.Helper()
	cfg, err := services.ParseFetchConfig([]byte(`{"schema":1,"screen":"off"}`))
	if err != nil {
		t.Fatal(err)
	}
	f.PublicDataKeyDir = t.TempDir()
	return Config{Features: f, Fetch: cfg}
}

func seedHistoryCall(t *testing.T, s *Store, id, account, service, method, body string, created int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO service_calls(id,account,service,method,request_key,hold_id,resource,cost,state,body,public,prices_version,created_at,finished_at)
VALUES(?,?,?,?,?,'','credit',3,'done',?,'{"bytes":5}',1,?,?)`, id, account, service, method, "rk-"+id, body, created, created); err != nil {
		t.Fatal(err)
	}
}

func seedNotaryReceipt(t *testing.T, s *Store, hash, account string, at int64) int64 {
	t.Helper()
	var seq int64
	if err := s.db.QueryRow("INSERT INTO notary_receipts(hash,time,account) VALUES(?,?,?) RETURNING seq", hash, at, account).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

func historyPage(t *testing.T, s *Store, key ed25519.PrivateKey, c Command) ([]map[string]any, string) {
	t.Helper()
	c.Operation = "service.calls"
	res := run(t, s, signed(key, c))
	raw, _ := json.Marshal(res.Data)
	var out struct {
		Calls []map[string]any `json:"calls"`
		Next  string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out.Calls, out.Next
}

func historyIDs(calls []map[string]any) []string {
	ids := make([]string, len(calls))
	for i, c := range calls {
		ids[i] = c["call_id"].(string)
	}
	return ids
}

// C1003: service.calls lists only the caller's own free tool calls, newest
// first, fetch, public data, screen and notary in one entry shape; never a
// keyless call, another key's, or a tool it does not list; the stored text
// and values never reach an entry; filters, paging and stale at 90 days.
func TestToolHistoryList(t *testing.T) {
	s := toolHistoryStore(t)
	u, o := keyFor(171), keyFor(172)
	register(t, s, u)
	register(t, s, o)
	me, other := keyID(u), keyID(o)
	hex := func(n int) string { return fmt.Sprintf("%032x", n) }
	seedHistoryCall(t, s, hex(1), me, "fetch", "page", `{"url":"https://a.example/","final_url":"https://a.example/x","status":200,"raw_sha256":"`+strings.Repeat("a", 64)+`","text_bytes":42,"text":"SECRET-TEXT","field_value":"SECRET-FIELD","verdict":{"Verdict":"pass"}}`, testTime-100)
	seedHistoryCall(t, s, hex(2), me, "public_data", "fetch", `{"dataset":"sea_ice_extent","params":{"region":"arctic"},"data":["SECRET-DATA"],"data_sha256":"`+strings.Repeat("b", 64)+`","source_url":"https://src.example/","licence":"CC0"}`, testTime-90)
	seedHistoryCall(t, s, hex(3), me, "screen", "text", `{"verdict":"flag","source":"email","text_sha256":"`+strings.Repeat("c", 64)+`","text_bytes":7,"text":"SECRET-SCREENED"}`, testTime-80)
	seedHistoryCall(t, s, hex(4), other, "fetch", "page", `{"url":"https://other.example/"}`, testTime-70)
	seedHistoryCall(t, s, hex(5), "anon:198.51.100.0/24", "fetch", "page", `{"url":"https://anon.example/"}`, testTime-60)
	seedHistoryCall(t, s, hex(6), me, "memory", "set", `{"key":"SECRET-MEMORY"}`, testTime-50)
	seedHistoryCall(t, s, hex(7), me, "fetch", "page", `{"url":"https://old.example/","raw_sha256":"`+strings.Repeat("d", 64)+`"}`, testTime-91*86400)
	seq := seedNotaryReceipt(t, s, strings.Repeat("e", 64), me, testTime-40)
	seedNotaryReceipt(t, s, strings.Repeat("f", 64), other, testTime-30)
	notaryID := fmt.Sprintf("notary:%d", seq)

	all, next := historyPage(t, s, u, Command{})
	if want := []string{notaryID, hex(3), hex(2), hex(1), hex(7)}; strings.Join(historyIDs(all), " ") != strings.Join(want, " ") || next != "" {
		t.Fatalf("history = %v (next %q), want %v", historyIDs(all), next, want)
	}
	raw, _ := json.Marshal(all)
	if strings.Contains(string(raw), "SECRET") {
		t.Fatalf("an entry carries stored content: %s", raw)
	}
	byID := map[string]map[string]any{}
	for _, c := range all {
		byID[c["call_id"].(string)] = c
	}
	for id, want := range map[string][3]any{
		hex(1):   {"fetch.page", "https://a.example/x", strings.Repeat("a", 64)},
		hex(2):   {"public_data.fetch", `sea_ice_extent {"region":"arctic"}`, strings.Repeat("b", 64)},
		hex(3):   {"screen.text", "email", strings.Repeat("c", 64)},
		notaryID: {"notary.stamp", strings.Repeat("e", 64), strings.Repeat("e", 64)},
	} {
		e := byID[id]
		if e["tool"] != want[0] || e["target"] != want[1] || e["digest"] != want[2] || e["stale"] != false {
			t.Errorf("%s = %v, want tool/target/digest %v", id, e, want)
		}
	}
	if byID[hex(1)]["verdict"] != "pass" || byID[hex(1)]["bytes"] != float64(42) || byID[hex(3)]["verdict"] != "flag" || byID[hex(2)]["licence"] != "CC0" {
		t.Errorf("extras: fetch %v, screen %v, public data %v", byID[hex(1)], byID[hex(3)], byID[hex(2)])
	}
	if byID[hex(7)]["stale"] != true || byID[notaryID]["cited"] != true || byID[notaryID]["url"] != "/api/notary/"+strings.Repeat("e", 64) || byID[hex(1)]["cited"] != false {
		t.Errorf("stale/cited: old %v, notary %v, fetch %v", byID[hex(7)], byID[notaryID], byID[hex(1)])
	}

	if got, _ := historyPage(t, s, u, Command{Target: "fetch"}); strings.Join(historyIDs(got), " ") != hex(1)+" "+hex(7) {
		t.Errorf("target fetch = %v", historyIDs(got))
	}
	if got, _ := historyPage(t, s, u, Command{Target: "notary.stamp"}); strings.Join(historyIDs(got), " ") != notaryID {
		t.Errorf("target notary.stamp = %v", historyIDs(got))
	}
	if got, _ := historyPage(t, s, u, Command{Data: fmt.Sprintf(`{"schema":1,"since":%d}`, testTime-85)}); strings.Join(historyIDs(got), " ") != notaryID+" "+hex(3) {
		t.Errorf("since = %v", historyIDs(got))
	}
	var paged []string
	cursor := ""
	for range 10 {
		got, next := historyPage(t, s, u, Command{Limit: 2, Cursor: cursor})
		paged = append(paged, historyIDs(got)...)
		if cursor = next; cursor == "" {
			break
		}
	}
	if strings.Join(paged, " ") != strings.Join(historyIDs(all), " ") {
		t.Errorf("paged = %v, want %v", paged, historyIDs(all))
	}
	fails(t, s, signed(u, Command{Operation: "service.calls", Limit: services.HistoryPageMax + 1}), "invalid_limit")
	fails(t, s, signed(u, Command{Operation: "service.calls", Cursor: "nope"}), "invalid_cursor")
	fails(t, s, signed(u, Command{Operation: "service.calls", Target: "memory"}), "invalid_request")
	fails(t, s, signed(u, Command{Operation: "service.calls", Data: `{"schema":1,"text":true}`}), "invalid_request")
	fails(t, s, Command{Operation: "service.calls"}, "signature_required")
	if got, _ := historyPage(t, s, o, Command{}); len(got) != 2 {
		t.Errorf("the other key's history = %v", historyIDs(got))
	}
}

// C1003: service.cite makes one of your own calls public at /api/calls/ID
// (entry only) and uncites it; another key's call reads as none; a notary
// receipt is public already; the day's cap and the cited maximum hold;
// owner erasure clears the stored results and ends the cites.
func TestToolHistoryCite(t *testing.T) {
	s := toolHistoryStore(t)
	u, o := keyFor(173), keyFor(174)
	register(t, s, u)
	register(t, s, o)
	run(t, s, signed(u, Command{Operation: "agent.register", Handle: "citer"}))
	me := keyID(u)
	id, theirs := fmt.Sprintf("%032x", 11), fmt.Sprintf("%032x", 12)
	seedHistoryCall(t, s, id, me, "fetch", "page", `{"url":"https://a.example/","raw_sha256":"`+strings.Repeat("a", 64)+`","text":"SECRET-TEXT"}`, testTime-100)
	seedHistoryCall(t, s, theirs, keyID(o), "fetch", "page", `{"url":"https://b.example/"}`, testTime-100)
	seq := seedNotaryReceipt(t, s, strings.Repeat("e", 64), me, testTime-40)
	if _, ok, _ := s.CitedCall(testContext, id); ok {
		t.Fatal("an uncited call is public")
	}
	fails(t, s, signed(u, Command{Operation: "service.cite", Target: theirs, Data: `{"schema":1,"public":true}`, RequestID: randomID()}), "not_found")
	fails(t, s, signed(u, Command{Operation: "service.cite", Target: "nonsense", Data: `{"schema":1,"public":true}`, RequestID: randomID()}), "not_found")
	fails(t, s, signed(u, Command{Operation: "service.cite", Target: id, Data: `{"schema":1}`, RequestID: randomID()}), "invalid_request")
	res := run(t, s, signed(u, Command{Operation: "service.cite", Target: id, Data: `{"schema":1,"public":true}`, RequestID: randomID()}))
	if res.Data["cited"] != true || res.Data["url"] != CitedPath+id {
		t.Fatalf("cite = %v", res.Data)
	}
	page, ok, err := s.CitedCall(testContext, id)
	if err != nil || !ok {
		t.Fatalf("cited call: %v %v", ok, err)
	}
	raw, _ := json.Marshal(page)
	if strings.Contains(string(raw), "SECRET") || !strings.Contains(string(raw), `"handle":"citer"`) || !strings.Contains(string(raw), strings.Repeat("a", 64)) {
		t.Fatalf("cited page = %s", raw)
	}
	if got, _ := historyPage(t, s, u, Command{Target: "fetch"}); got[0]["cited"] != true || got[0]["url"] != CitedPath+id {
		t.Errorf("listed after cite: %v", got[0])
	}
	if _, ok, _ := s.CitedCall(testContext, theirs); ok {
		t.Error("another key's uncited call is public")
	}
	run(t, s, signed(u, Command{Operation: "service.cite", Target: id, Data: `{"schema":1,"public":false}`, RequestID: randomID()}))
	if _, ok, _ := s.CitedCall(testContext, id); ok {
		t.Error("an uncited call is still public")
	}
	notary := fmt.Sprintf("notary:%d", seq)
	if res := run(t, s, signed(u, Command{Operation: "service.cite", Target: notary, Data: `{"schema":1,"public":true}`, RequestID: randomID()})); res.Data["url"] != "/api/notary/"+strings.Repeat("e", 64) {
		t.Errorf("notary cite = %v", res.Data)
	}
	fails(t, s, signed(u, Command{Operation: "service.cite", Target: notary, Data: `{"schema":1,"public":false}`, RequestID: randomID()}), "invalid_request")

	// The cited maximum, then the day's cap.
	for i := range CitedMax {
		if _, err := s.db.Exec("INSERT INTO counters(scope,value) VALUES(?,?)", citeKey(me, fmt.Sprintf("%032x", 1000+i)), testTime); err != nil {
			t.Fatal(err)
		}
	}
	fails(t, s, signed(u, Command{Operation: "service.cite", Target: id, Data: `{"schema":1,"public":true}`, RequestID: randomID()}), "cite_limit")
	if _, err := s.db.Exec("INSERT INTO counters(scope,value) VALUES(?,?) ON CONFLICT(scope) DO UPDATE SET value=excluded.value", citeDayScope+me+":"+fmt.Sprint(testTime/86400), CitesPerDay); err != nil {
		t.Fatal(err)
	}
	fails(t, s, signed(u, Command{Operation: "service.cite", Target: id, Data: `{"schema":1,"public":true}`, RequestID: randomID()}), "request_rate")

	// Owner erasure: bodies and public records cleared, receipts unnamed,
	// cites ended; the rows stay.
	if _, err := s.db.Exec("UPDATE counters SET value=? WHERE scope=?", testTime, citeKey(me, id)); err != nil {
		t.Fatal(err)
	}
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
	var body, public, account string
	if err = s.db.QueryRow("SELECT body,public FROM service_calls WHERE id=?", id).Scan(&body, &public); err != nil || body != "" || public != "{}" {
		t.Errorf("after erasure: body %q public %q (%v)", body, public, err)
	}
	if err = s.db.QueryRow("SELECT account FROM notary_receipts WHERE seq=?", seq).Scan(&account); err != nil || account != "" {
		t.Errorf("after erasure: notary account %q (%v)", account, err)
	}
	if _, ok, _ := s.CitedCall(testContext, id); ok {
		t.Error("an erased account's call is still cited")
	}
}

// C1003: an entry is built from named fields only: whatever a stored
// body holds in its content fields (text, data, field values, a verdict's
// text) never reaches it, for every tool.
func FuzzToolHistoryEntryKeepsNoContent(f *testing.F) {
	f.Add("SECRET-TEXT")
	f.Add("a\u0000b")
	f.Add(`"},"final_url":"x`)
	s := openTest(f, historyConfig(f, Features{Services: []string{"notary", "fetch", "screen", "public_data"}}))
	f.Cleanup(s.stopServices)
	e := s.services.engine
	f.Fuzz(func(t *testing.T, secret string) {
		quoted, _ := json.Marshal(secret)
		inner := strings.Trim(string(quoted), `"`)
		if len(inner) < 6 {
			return
		}
		content, _ := json.Marshal(map[string]any{"url": "https://u.example/", "dataset": "d", "source": "web", "text": secret, "data": secret, "field_value": secret, "title": secret,
			"verdict": map[string]any{"verdict": "pass", "text": secret}, "results": []any{map[string]any{"dataset": "d", "data": secret}, map[string]any{"dataset": "e", "data": []string{secret}}}})
		for i, svc := range []string{"fetch", "public_data", "screen"} {
			id := fmt.Sprintf("%032x", i+1)
			if _, err := s.db.Exec("INSERT OR REPLACE INTO service_calls(id,account,service,method,request_key,hold_id,resource,state,body,prices_version,created_at) VALUES(?,?,?,'x',?,'','credit','done',?,1,1)", id, "acct", svc, id, string(content)); err != nil {
				t.Fatal(err)
			}
			row, ok, err := e.HistoryByID(testContext, s.db, id, 2)
			if err != nil || !ok {
				t.Fatalf("%s: %v %v", svc, ok, err)
			}
			raw, _ := json.Marshal(row.Entry)
			// The same entry's own words (its id, tool, target) may hold
			// the string by chance; only content would put it anywhere else.
			skeleton, _ := json.Marshal(map[string]any{"id": id, "svc": svc, "fixed": "https://u.example/ d web pass e fetch.x public_data.x screen.x done"})
			if strings.Contains(string(raw), inner) && !strings.Contains(string(skeleton), inner) && !strings.Contains("false true 0 1 2 ", inner) {
				t.Fatalf("%s entry carries content: %s", svc, raw)
			}
		}
	})
}
