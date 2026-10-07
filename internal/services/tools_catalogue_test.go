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
