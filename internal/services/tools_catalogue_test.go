package services

// tools over the paid-API catalogue: one search that mixes SwarmMemo's own
// tools with the catalogue's (stub bundler API), names no provider, and one
// call that runs a catalogue tool exactly as x402 call does.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// newToolsHarness is the bundler harness with the notary beside x402, and
// a hit under the upstream's own namespace.
func newToolsHarness(t *testing.T) (*x402Harness, *fakeBundlerAPI) {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	h, api := newBundlerHarnessWith(t, "", "", []string{"notary"}, Deps{NotaryKey: key})
	api.hits = strings.Replace(testBundlerHits(), "[", `[{"id":"`+toolVendor+`","title":"Coin price","description":"The price of a coin","payment":{"price_hint":"0.001"}},`, 1)
	return h, api
}

// toolsSearch is one tools search as the board runs it: the command's
// transaction, then the after-commit part when it has one.
func (h *x402Harness) toolsSearch(args string) (map[string]any, string) {
	h.t.Helper()
	tx, err := h.db.Begin()
	if err != nil {
		h.t.Fatal(err)
	}
	out, err := h.engine.ReadOutcome(context.Background(), tx, Request{Service: ToolsID, Data: `{"schema":1,"method":"search","args":` + args + `}`, Subject: testSubject}, h.now)
	if err != nil {
		tx.Rollback()
		h.t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
	got := out.Data
	if out.After != nil {
		if got, err = out.After(); err != nil {
			h.t.Fatal(err)
		}
	}
	page := resultOf(got)
	raw, _ := json.Marshal(page)
	return page, string(raw)
}

// toolsCall is one tools.call; maxCost < 0 leaves max_cost out.
func (h *x402Harness) toolsCall(args string, maxCost int64) (map[string]any, error) {
	h.t.Helper()
	h.seq++
	data := `{"schema":1,"method":"call","args":` + args
	if maxCost >= 0 {
		data += fmt.Sprintf(`,"max_cost":%d`, maxCost)
	}
	tx, err := h.db.Begin()
	if err != nil {
		h.t.Fatal(err)
	}
	out, err := h.engine.Call(context.Background(), tx, Request{Service: ToolsID, Data: data + "}", Subject: testSubject, RequestKey: fmt.Sprintf("id:tools-%d", h.seq)}, h.now)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
	if out.After == nil {
		return out.Data, nil
	}
	return out.After()
}

func toolEntries(page map[string]any) []map[string]any {
	var out []map[string]any
	list, _ := page["tools"].([]any)
	for _, e := range list {
		out = append(out, e.(map[string]any))
	}
	return out
}

func TestToolsSearchMixesBothKinds(t *testing.T) {
	h, _ := newToolsHarness(t)
	page, raw := h.toolsSearch(`{"query":"weather notary stamp"}`)
	kinds := map[string]int{}
	byID := map[string]map[string]any{}
	for _, e := range toolEntries(page) {
		kinds[e["kind"].(string)]++
		byID[e["id"].(string)] = e
		for _, field := range []string{"id", "title", "description", "price", "needs_key", "callable"} {
			if _, ok := e[field]; !ok {
				t.Errorf("%s lacks %s", e["id"], field)
			}
		}
	}
	if kinds["swarmmemo"] == 0 || kinds["catalogue"] == 0 || page["catalogue"].(map[string]any)["searched"] != true {
		t.Fatalf("one list of both kinds: %v\n%s", kinds, raw)
	}
	hit := byID["tool:"+toolOK]
	price, _ := hit["price"].(map[string]any)
	if hit == nil || price["resource"] != "credit" || price["max_cost_required"] != true || price["max_cost"].(float64) <= 0 || price["cost"].(float64) <= 0 ||
		hit["input_schema"] == nil || hit["callable"] != true || hit["needs_key"] != true || hit["text_is_untrusted"] != true {
		t.Fatalf("a paid API: %+v", hit)
	}
	if byID["tool:"+toolUnvet]["callable"] != false || byID["tool:"+toolUnvet]["why_not"] != "tool_unvetted" {
		t.Fatalf("an unvetted paid API: %+v", byID["tool:"+toolUnvet])
	}
	if byID["swarmmemo:notary.stamp"]["needs_key"] != false {
		t.Fatalf("the notary: %+v", byID["swarmmemo:notary.stamp"])
	}
	// The upstream's namespace, the relay, the bundler and the facilitator
	// are named nowhere.
	if byID["tool:"+toolVendorP] == nil {
		t.Fatalf("the namespaced hit is listed by its public id: %s", raw)
	}
	for _, vendor := range []string{"frames", "x402", "bundler", "facilitator", "coinbase", "usdc", "host"} {
		if strings.Contains(strings.ToLower(raw), vendor) {
			t.Errorf("the search names %q: %s", vendor, raw)
		}
	}
	// Ranked: the paid APIs in their own order ahead of a partial match of
	// SwarmMemo's own; a full match of SwarmMemo's own ahead of them all.
	list := toolEntries(page)
	if list[0]["kind"] != "catalogue" || list[len(list)-1]["id"] != "swarmmemo:notary.stamp" {
		t.Fatalf("ranking: first %v, last %v", list[0]["id"], list[len(list)-1]["id"])
	}
	page, _ = h.toolsSearch(`{"query":"notary stamp"}`)
	if first := toolEntries(page)[0]; first["id"] != "swarmmemo:notary.stamp" {
		t.Fatalf("a full match of SwarmMemo's own leads: %v", first["id"])
	}
	page, _ = h.toolsSearch(`{"query":"weather","kind":"swarmmemo","limit":3}`)
	if len(toolEntries(page)) != 0 || page["catalogue"].(map[string]any)["searched"] != false {
		t.Fatalf("kind swarmmemo searches no paid API: %+v", page)
	}
	page, _ = h.toolsSearch(`{"query":"weather","limit":2}`)
	if len(toolEntries(page)) != 2 {
		t.Fatalf("limit: %d", len(toolEntries(page)))
	}
	// Without a query: the featured tools and the pointer, no paid-API search.
	page, _ = h.toolsSearch(`{}`)
	if list := toolEntries(page); len(list) != 1 || list[0]["id"] != "swarmmemo:notary.stamp" || list[0]["featured"] != true ||
		!strings.Contains(page["more"].(string), "paid APIs") || page["catalogue"].(map[string]any)["searched"] != false {
		t.Fatalf("no query: %+v", page)
	}
}

// C111: a query that names a capability SwarmMemo has ranks its own tool
// above every paid API, and kind "swarmmemo" finds it.
func TestToolsSearchRanksOwnCapabilityFirst(t *testing.T) {
	h, api := newBundlerHarnessWith(t, "", "", []string{"docs"}, Deps{})
	api.hits = `[{"id":"` + toolOK + `","title":"Pastebin","description":"Paste text, get a link: a pastebin API","payment":{"price_hint":"0.001"}}]`
	for _, query := range []string{"pastebin", "pastebin api", "paste", "share text", "snippet", "gist", "shared docs", "collaborative document", "pastebin weather"} {
		page, raw := h.toolsSearch(`{"query":"` + query + `"}`)
		if list := toolEntries(page); len(list) == 0 || list[0]["id"] != "swarmmemo:docs.create" || !strings.Contains(raw, `"kind":"catalogue"`) {
			t.Fatalf("%q: docs.create first, the paid APIs after: %s", query, raw)
		}
	}
	page, raw := h.toolsSearch(`{"query":"pastebin","kind":"swarmmemo"}`)
	ids := []string{}
	for _, e := range toolEntries(page) {
		ids = append(ids, e["id"].(string))
	}
	if len(ids) < 2 || ids[0] != "swarmmemo:docs.create" || ids[1] != "swarmmemo:docs.open" {
		t.Fatalf("pastebin, kind swarmmemo: %v\n%s", ids, raw)
	}
	// A query that names no capability keeps the catalogue ahead of a
	// partial match.
	page, raw = h.toolsSearch(`{"query":"weather text"}`)
	if first := toolEntries(page)[0]; first["kind"] != "catalogue" {
		t.Fatalf("text alone names no capability: %s", raw)
	}
}

// A paid API through tools.call: max_cost required, then the same call,
// payment, charge and record as x402 call; the direct path is unchanged.
func TestToolsCallsACatalogueTool(t *testing.T) {
	h, api := newToolsHarness(t)
	h.toolsSearch(`{"query":"weather"}`)
	args := `{"id":"tool:` + toolOK + `","args":{"city":"Paris"}}`
	for _, maxCost := range []int64{-1, CallDefaultMaxCost} {
		if _, err := h.toolsCall(args, maxCost); errCode(err) != "invalid_service_data" || !strings.Contains(err.(*allowance.Err).Message, "max_cost is required") {
			t.Fatalf("max_cost %d: %v", maxCost, err)
		}
	}
	if h.charged() != 0 || func() int { _, _, _, n := api.count(); return n }() != 0 {
		t.Fatal("a refused call reached the upstream or was charged")
	}
	out, err := h.toolsCall(args, creditsFor(20000))
	if err != nil {
		t.Fatal(err)
	}
	r := resultOf(out)
	raw, _ := json.Marshal(r)
	if out["service"] != "x402" || out["method"] != "call" || r["resource"] != "tool:"+toolOK || !strings.Contains(string(raw), `"temp_c":14`) {
		t.Fatalf("through tools.call: %v %s", out, raw)
	}
	if h.charged() != creditsFor(2000) {
		t.Fatalf("charged %d, want %d", h.charged(), creditsFor(2000))
	}
	var resource, service string
	if err = h.db.QueryRow("SELECT p.resource, c.service FROM x402_payments p JOIN service_calls c ON c.account=p.account AND c.request_key=p.request_key").Scan(&resource, &service); err != nil || resource != "tool:"+toolOK || service != "x402" {
		t.Fatalf("payment and record: %s %s %v", resource, service, err)
	}
	if fmt.Sprint(api.lastInvoke["calls"].([]any)[0].(map[string]any)["args"]) != "map[city:Paris]" {
		t.Fatalf("the tool's args: %v", api.lastInvoke)
	}
	// A ceiling below the tool's price: refused before any payment.
	_, _, _, invokes := api.count()
	if _, err = h.toolsCall(args, creditsFor(1000)); errCode(err) != "price_exceeds_max" {
		t.Fatalf("a low ceiling: %v", err)
	}
	if _, _, _, n := api.count(); n != invokes || h.charged() != creditsFor(2000) {
		t.Fatal("a refused call was paid or charged")
	}
	// The direct path, unchanged: the same answer, the same charge again.
	direct, err := h.call(testSubject, `{"resource":"tool:`+toolOK+`","body":{"city":"Paris"}}`, creditsFor(20000))
	if err != nil {
		t.Fatal(err)
	}
	a, b := resultOf(out), resultOf(direct)
	delete(a, "payment")
	delete(b, "payment")
	if !reflect.DeepEqual(a, b) || h.charged() != 2*creditsFor(2000) {
		t.Fatalf("tools.call %v, x402 call %v", a, b)
	}
}

// Each featured tool's example is taken by its method's own parser.
func TestFeaturedExamplesParse(t *testing.T) {
	for _, f := range Featured {
		name := strings.TrimPrefix(f.ID, ToolIDPrefix)
		shape, ok := argShapes[name]
		if !ok {
			t.Errorf("%s: no args shape", name)
			continue
		}
		if err := StrictObject(f.Args, reflect.New(reflect.TypeOf(shape)).Interface()); err != nil {
			t.Errorf("%s: %s refused: %v", name, f.Args, err)
		}
		var err error
		switch name {
		case "memory.put":
			_, err = parsePut(f.Args)
		case "notary.stamp":
			_, err = parseStamp(f.Args)
		case "screen.text":
			_, err = parseScreen(f.Args)
		case "wakeup.schedule":
			_, err = parseWakeup(f.Args, 1_760_000_000)
		}
		if err != nil {
			t.Errorf("%s: %s refused: %v", name, f.Args, err)
		}
	}
}

// C116: every paid hit carries a call that runs as it is (id, args from its
// input schema, max_cost from its price.max_cost) and an args summary; a
// call without max_cost is told the value to send.
func TestToolsSearchPaidHitExampleRuns(t *testing.T) {
	h, api := newToolsHarness(t)
	page, raw := h.toolsSearch(`{"query":"weather"}`)
	var hit map[string]any
	for _, e := range toolEntries(page) {
		if e["kind"] == "catalogue" && e["example"] == nil {
			t.Errorf("%v: a paid hit without an example", e["id"])
		}
		if e["id"] == "tool:"+toolOK {
			hit = e
		}
	}
	if hit == nil {
		t.Fatalf("no weather hit: %s", raw)
	}
	ex := hit["example"].(map[string]any)
	maxCost := hit["price"].(map[string]any)["max_cost"].(float64)
	if ex["id"] != "tool:"+toolOK || ex["max_cost"].(float64) != maxCost || fmt.Sprint(ex["args"]) != "map[city:CITY]" {
		t.Fatalf("example: %+v", ex)
	}
	if fmt.Sprint(hit["args"]) != "[map[name:city required:false type:string]]" {
		t.Fatalf("args: %+v", hit["args"])
	}
	// The example, sent as it is, runs.
	args, _ := json.Marshal(map[string]any{"id": ex["id"], "args": ex["args"]})
	if _, err := h.toolsCall(string(args), int64(ex["max_cost"].(float64))); err != nil {
		t.Fatalf("the example: %v", err)
	}
	if fmt.Sprint(api.lastInvoke["calls"].([]any)[0].(map[string]any)["args"]) != "map[city:CITY]" {
		t.Fatalf("the tool's args: %v", api.lastInvoke)
	}
	// Without max_cost: the refusal names the field and the value.
	_, err := h.toolsCall(string(args), -1)
	if errCode(err) != "invalid_service_data" || !strings.Contains(err.(*allowance.Err).Message, fmt.Sprintf("send max_cost: %d", int64(maxCost))) {
		t.Fatalf("no max_cost: %v", err)
	}
}

func TestSchemaArgs(t *testing.T) {
	for _, c := range []struct{ schema, args, example string }{
		{`{"type":"object","properties":{"city":{"type":"string"},"days":{"type":["integer","null"]},"units":{"type":"string","enum":["metric","imperial"]}},"required":["city","units"]}`,
			`[{city string true} {units string true} {days integer false}]`, `map[city:CITY units:metric]`},
		{`{"type":"http","method":"GET","queryParams":{"q":{"type":"string","required":true},"limit":{"type":"number"}}}`,
			`[{q string true} {limit number false}]`, `map[q:Q]`},
		{`{"type":"http","body":{"type":"object","properties":{"coin-id":{"type":"string"}}}}`, `[{coin-id string false}]`, `map[coin-id:COIN_ID]`},
		{`{"type":"http","body":{"n":3,"ok":true,"tags":["a"]}}`, `[{n integer false} {ok boolean false} {tags array false}]`, `map[n:1 ok:true tags:[]]`},
		{`{"type":"http","body":{"bad name\u0000":"x","fine":"y"}}`, `[{fine string false}]`, `map[fine:FINE]`},
		{`[1,2]`, `[]`, `map[]`},
		{``, `[]`, `map[]`},
	} {
		args := schemaArgs(json.RawMessage(c.schema))
		parts := []string{}
		for _, a := range args {
			parts = append(parts, fmt.Sprintf("{%s %s %v}", a.name, a.typ, a.required))
		}
		if got := "[" + strings.Join(parts, " ") + "]"; got != c.args {
			t.Errorf("%s: args %s, want %s", c.schema, got, c.args)
		}
		if got := fmt.Sprint(exampleArgs(args)); got != c.example {
			t.Errorf("%s: example %s, want %s", c.schema, got, c.example)
		}
	}
}
