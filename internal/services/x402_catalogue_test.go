package services

// The aggregator: the open catalogue imported from a fake Bazaar, its
// guardrails, search, sub-caps and kill switch; the bundler interface with a
// fake bundler; and the frames adapter against a fake Frames API. Every
// upstream is a fake behind the harness's dialer: no test reaches the
// network.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

const (
	discoveryPath = "/platform/v2/x402/discovery/resources"
	deniedPayTo   = "0x1111111111111111111111111111111111111111"
)

// fakeBazaar serves a discovery list, at most pageCap items a page whatever
// the limit asked (as CDP sometimes does).
type fakeBazaar struct {
	mu      sync.Mutex
	items   []string
	pageCap int
	hits    atomic.Int64
	status  int
}

func (b *fakeBazaar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.hits.Add(1)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.status != 0 {
		w.WriteHeader(b.status)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if r.URL.Query().Get("type") != "http" {
		http.Error(w, "type", 400)
		return
	}
	if b.pageCap > 0 {
		limit = min(limit, b.pageCap)
	}
	end := min(offset+limit, len(b.items))
	page := []string{}
	if offset < len(b.items) {
		page = b.items[offset:end]
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"x402Version":2,"items":[%s],"pagination":{"limit":%d,"offset":%d,"total":%d}}`, strings.Join(page, ","), limit, offset, len(b.items))
}

// bazaarItemJSON is one discovery item paying testPayTo in Base USDC.
func bazaarItemJSON(resource, method string, amount int64, payTo, network, desc, extra string) string {
	input := `"input":{"type":"http","method":"` + method + `","queryParams":{"q":"x"}}`
	if method == "POST" {
		input = `"input":{"type":"http","method":"POST","bodyType":"json"}`
	}
	d, _ := json.Marshal(desc)
	return `{"resource":"` + resource + `","type":"http","x402Version":2,"description":` + string(d) + `,"lastUpdated":"2026-09-28T09:34:55Z",
"accepts":[{"scheme":"exact","network":"` + network + `","amount":"` + strconv.FormatInt(amount, 10) + `","maxAmountRequired":"` + strconv.FormatInt(amount, 10) + `","asset":"` + testUSDC + `","payTo":"` + payTo + `","maxTimeoutSeconds":60,"extra":{"name":"USD Coin","version":"2"}}],
"extensions":{"bazaar":{"info":{` + input + `}}}` + extra + `}`
}

func quality(payers, calls int) string {
	return fmt.Sprintf(`,"quality":{"l30DaysTotalCalls":%d,"l30DaysUniquePayers":%d,"lastCalledAt":"2026-09-28T09:34:55Z"}`, calls, payers)
}

// testBazaarItems are three admissible resources and one of each thing the
// guardrails refuse.
func testBazaarItems() []string {
	return []string{
		bazaarItemJSON("https://example.com/open/search", "GET", 1500, testPayTo, "eip155:8453", "Web search\u0000 API <b>fast</b>", quality(50, 900)),
		bazaarItemJSON("https://example.com/open/scrape", "POST", 1800, testPayTo, "eip155:8453", "Scrape a page to markdown", quality(10, 20)+`,"curated":true`),
		bazaarItemJSON("https://example.com/open/weather", "GET", 1000, testPayTo, "eip155:8453", "Weather forecast", ""), // no quality: another facilitator
		bazaarItemJSON("http://example.com/open/plain", "GET", 1000, testPayTo, "eip155:8453", "plain http", quality(5, 5)),
		bazaarItemJSON("https://10.0.0.1/open/ip", "GET", 1000, testPayTo, "eip155:8453", "ip literal", quality(5, 5)),
		bazaarItemJSON("https://example.com:8443/open/port", "GET", 1000, testPayTo, "eip155:8453", "other port", quality(5, 5)),
		bazaarItemJSON("https://example.com/open/dear", "GET", 30000, testPayTo, "eip155:8453", "over the max price", quality(5, 5)),
		bazaarItemJSON("https://api.example.org/open/denied", "GET", 1000, testPayTo, "eip155:8453", "denied domain", quality(5, 5)),
		bazaarItemJSON("https://example.com/open/deniedpay", "GET", 1000, deniedPayTo, "eip155:8453", "denied recipient", quality(5, 5)),
		bazaarItemJSON("https://example.com/open/adult", "GET", 1000, testPayTo, "eip155:8453", "nsfw pictures", quality(5, 5)),
		bazaarItemJSON("https://example.com/open/nobody", "GET", 1000, testPayTo, "eip155:8453", "nobody pays", quality(0, 0)),
		bazaarItemJSON("https://example.com/open/solana", "GET", 1000, testPayTo, "solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdp", "solana", quality(5, 5)),
		bazaarItemJSON("https://example.com/paid", "GET", 1000, testPayTo, "eip155:8453", "the pinned endpoint again", quality(99, 99)),
		`{"resource":"https://example.com/open/mcp","type":"mcp","x402Version":2,"accepts":[]}`,
		`{"resource":42}`,
	}
}

const testCatalogue = `{"discovery_urls":["https://example.com` + discoveryPath + `"],"page_size":4,"max_price":"0.002","open_daily":"0.004","recipient_daily":"0.002"}`
const testDeny = `{"domains":["example.org"],"pay_to":["` + deniedPayTo + `"],"categories":["adult","gambling"]}`

// catalogueConfig is testX402Config with an open catalogue and a denylist.
func catalogueConfig(t testing.TB, caps, catalogue string) *X402Config {
	t.Helper()
	key, _ := hex.DecodeString(x402TestKey)
	signer, _ := signerFromKeyBytes(key)
	config := `{"schema":1,"enabled":true,"network":"eip155:8453","asset":"` + testUSDC + `","asset_name":"USD Coin","asset_version":"2","decimals":6,
"caps":` + caps + `,"wallet_key_file":"unused","allowlist_file":"unused","catalogue":` + catalogue + `}`
	allowlist := `{"schema":1,"version":7,"resources":` + testResources + `,"deny":` + testDeny + `}`
	cfg, err := ParseX402Config([]byte(config), []byte(allowlist), signer)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Catalogue != nil {
		// The fake discovery stands in for the default one, whose
		// popularity figures ranking trusts.
		cfg.Catalogue.trusted = "https://example.com" + discoveryPath
	}
	return cfg
}

// vetOpen vets open resources as the operator's swarmmemo x402 vet does,
// and reloads the snapshot as the worker would within five minutes.
func vetOpen(t *testing.T, h *x402Harness, x *x402, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := VetX402(context.Background(), h.db, id, true, h.now); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.loadCatalogue(context.Background(), h.now); err != nil {
		t.Fatal(err)
	}
}

func newCatalogueHarness(t *testing.T, fake *fakeX402, bazaar *fakeBazaar, cfg *X402Config) (*x402Harness, *x402) {
	t.Helper()
	fake.extra = map[string]http.Handler{discoveryPath: bazaar}
	h := newX402HarnessCfg(t, fake, cfg)
	return h, h.engine.Registry().providers["x402"].(*x402)
}

func (h *x402Harness) resources(args string) map[string]any {
	h.t.Helper()
	got, err := h.engine.Read(context.Background(), h.db, Request{Service: "x402", Data: `{"schema":1,"method":"resources","args":` + args + `}`, Subject: testSubject}, h.now)
	if err != nil {
		h.t.Fatalf("resources %s: %v", args, err)
	}
	return resultOf(got)
}

func ids(page map[string]any) []string {
	var out []string
	for _, r := range page["resources"].([]any) {
		out = append(out, r.(map[string]any)["id"].(string))
	}
	return out
}

func openIDFor(path, method string) string {
	slug := "example-com-" + strings.ReplaceAll(strings.Trim(path, "/"), "/", "-")
	return openID(slug, method, "https://example.com"+path)
}

func TestCatalogueImportGuardrails(t *testing.T) {
	bazaar := &fakeBazaar{items: testBazaarItems(), pageCap: 3}
	h, x := newCatalogueHarness(t, &fakeX402{price: 1500}, bazaar, catalogueConfig(t, `{}`, testCatalogue))
	n, err := x.importCatalogue(context.Background(), h.now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("imported %d, want 3 (search, scrape, weather)", n)
	}
	// The server capped pages at 3 items: every item was still read.
	if hits := bazaar.hits.Load(); hits != 5 {
		t.Fatalf("discovery pages fetched: %d, want 5 (15 items, 3 a page)", hits)
	}
	snap := x.cat.snap.Load()
	want := []string{openIDFor("/open/scrape", "POST"), openIDFor("/open/search", "GET"), openIDFor("/open/weather", "GET")}
	var got []string
	for _, r := range snap.open {
		got = append(got, r.ID)
		if !r.Open || r.MaxResponseBytes != X402ResponseBytesDefault || r.Timeout != 10*time.Second || r.Bundler != X402Bundler {
			t.Errorf("%s: bounds %+v", r.ID, r)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("open, ranked: %v, want %v (curated, then payers, then unknown)", got, want)
	}
	search := snap.byID[openIDFor("/open/search", "GET")]
	if search.Summary != "Web search API <b>fast</b>" || search.Category != "search" || strings.Join(search.Query, ",") != "q" || search.MaxAmount != 1500 {
		t.Fatalf("search: %+v", search)
	}
	if scrape := snap.byID[openIDFor("/open/scrape", "POST")]; !scrape.Body || scrape.Category != "scraping" {
		t.Fatalf("scrape: %+v", scrape)
	}
	// Re-importing keeps the ids and adds no rows.
	if _, err = x.importCatalogue(context.Background(), h.now+60); err != nil {
		t.Fatal(err)
	}
	var rows int
	_ = h.db.QueryRow("SELECT count(*) FROM x402_catalogue").Scan(&rows)
	if rows != 3 {
		t.Fatalf("rows after a second import: %d", rows)
	}
}

func TestCatalogueDiscoveryFailureKeepsSnapshot(t *testing.T) {
	bazaar := &fakeBazaar{items: testBazaarItems()}
	h, x := newCatalogueHarness(t, &fakeX402{price: 1500}, bazaar, catalogueConfig(t, `{}`, testCatalogue))
	if _, err := x.importCatalogue(context.Background(), h.now); err != nil {
		t.Fatal(err)
	}
	bazaar.status = 500
	if _, err := x.importCatalogue(context.Background(), h.now+3600); err == nil {
		t.Fatal("a failed discovery must report an error")
	}
	if len(x.cat.snap.Load().open) != 3 || *x.cat.lastError.Load() != "import failed" {
		t.Fatal("a failed import must keep the last good catalogue")
	}
}

func TestCatalogueSearch(t *testing.T) {
	h, x := newCatalogueHarness(t, &fakeX402{price: 1500}, &fakeBazaar{items: testBazaarItems()}, catalogueConfig(t, `{}`, testCatalogue))
	if _, err := x.importCatalogue(context.Background(), h.now); err != nil {
		t.Fatal(err)
	}
	all := h.resources(`{}`)
	if got := ids(all); len(got) != 5 || got[0] != "price" || got[1] != "search" {
		t.Fatalf("pinned first, then open: %v", got)
	}
	if all["matched"] != float64(5) || all["text_is_untrusted"] != true || all["catalogue"].(map[string]any)["open"] != float64(3) {
		t.Fatalf("summary: %v", all)
	}
	for _, r := range all["resources"].([]any) {
		m := r.(map[string]any)
		if open := m["pinned"] == false; open != (m["text_is_untrusted"] == true) {
			t.Fatalf("every open resource, and only those, is marked untrusted: %v", m)
		}
	}
	cats := all["categories"].(map[string]any)
	if cats["search"] != float64(2) || cats["scraping"] != float64(1) {
		t.Fatalf("categories: %v", cats)
	}
	if got := ids(h.resources(`{"query":"WEB api"}`)); len(got) != 1 || got[0] != openIDFor("/open/search", "GET") {
		t.Fatalf("query: %v", got)
	}
	if got := ids(h.resources(`{"category":"scraping"}`)); len(got) != 1 || got[0] != openIDFor("/open/scrape", "POST") {
		t.Fatalf("category: %v", got)
	}
	if got := ids(h.resources(`{"max_price":"0.0015"}`)); len(got) != 2 {
		t.Fatalf("max_price: %v", got)
	}
	first := h.resources(`{"limit":2}`)
	second := h.resources(`{"limit":2,"cursor":"` + first["next_cursor"].(string) + `"}`)
	last := h.resources(`{"limit":2,"cursor":"4"}`)
	if strings.Join(ids(first), ",") != "price,search" || len(ids(second)) != 2 || len(ids(last)) != 1 || last["next_cursor"] != nil {
		t.Fatalf("pages: %v %v %v", ids(first), ids(second), ids(last))
	}
	for _, bad := range []string{`{"limit":0}`, `{"limit":51}`, `{"cursor":"x"}`, `{"max_price":"-1"}`, `{"category":"Search!"}`, `{"query":"` + strings.Repeat("a", 201) + `"}`, `{"url":"https://evil"}`} {
		if _, err := h.engine.Read(context.Background(), h.db, Request{Service: "x402", Data: `{"schema":1,"method":"resources","args":` + bad + `}`, Subject: testSubject}, h.now); errCode(err) != "invalid_service_data" {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

func TestCatalogueOpenCallPaysAndMarks(t *testing.T) {
	h, x := newCatalogueHarness(t, &fakeX402{price: 1500}, &fakeBazaar{items: testBazaarItems()}, catalogueConfig(t, `{}`, testCatalogue))
	if _, err := x.importCatalogue(context.Background(), h.now); err != nil {
		t.Fatal(err)
	}
	id := openIDFor("/open/search", "GET")
	vetOpen(t, h, x, id)
	out, err := h.call(testSubject, `{"resource":"`+id+`","query":{"q":"agents"}}`, creditsFor(1500))
	if err != nil {
		t.Fatal(err)
	}
	r := resultOf(out)
	if r["allowlist_version"] != float64(0) || r["text_is_untrusted"] != true || r["bundler"] != "x402" || r["encoding"] != "json" {
		t.Fatalf("result: %v", r)
	}
	var version int
	_ = h.db.QueryRow("SELECT allowlist_version FROM x402_payments").Scan(&version)
	if version != 0 || h.charged() != creditsFor(1500) || h.fake.lastQ != "q=agents" {
		t.Fatalf("version %d charged %d query %q", version, h.charged(), h.fake.lastQ)
	}
	// Arguments stay bounded to what the listing allows.
	if _, err := h.call(testSubject, `{"resource":"`+id+`","query":{"other":"1"}}`, creditsFor(1500)); errCode(err) != "invalid_service_data" {
		t.Fatalf("an unlisted query name: %v", err)
	}
	if _, err := h.call(testSubject, `{"resource":"`+id+`","body":{"a":1}}`, creditsFor(1500)); errCode(err) != "invalid_service_data" {
		t.Fatalf("a body on a GET: %v", err)
	}
	// An upstream asking more than it listed is refused before any payment.
	h.fake.price = 1600
	paid := h.fake.paid
	if _, err := h.call(testSubject, `{"resource":"`+id+`"}`, creditsFor(1600)); errCode(err) != "x402_price_changed" || h.fake.paid != paid {
		t.Fatalf("price over the listing: %v", err)
	}
}

// A catalogue refresh between the quote and the run that raises a price
// never makes the run pay more than the agent's hold: it is refused first.
func TestCatalogueChangeAfterQuote(t *testing.T) {
	h, x := newCatalogueHarness(t, &fakeX402{price: 1500}, &fakeBazaar{items: testBazaarItems()}, catalogueConfig(t, `{}`, testCatalogue))
	if _, err := x.importCatalogue(context.Background(), h.now); err != nil {
		t.Fatal(err)
	}
	id := openIDFor("/open/search", "GET")
	vetOpen(t, h, x, id)
	tx, _ := h.db.Begin()
	out, err := h.engine.Call(context.Background(), tx, Request{Service: "x402", Data: `{"schema":1,"method":"call","args":{"resource":"` + id + `"},"max_cost":100000}`, Subject: testSubject, RequestKey: "id:q1"}, h.now)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	old := x.cat.snap.Load()
	dear := *old.byID[id]
	dear.MaxAmount = 2000
	next := &x402Snapshot{byID: map[string]*X402Resource{id: &dear}, open: []*X402Resource{&dear}, stats: old.stats, loadedAt: old.loadedAt}
	x.cat.snap.Store(next)
	h.fake.price = 2000
	if _, err = out.After(); errCode(err) != "x402_price_changed" || h.fake.paid != 0 || h.charged() != 0 {
		t.Fatalf("after a price rise: %v, paid %d, charged %d", err, h.fake.paid, h.charged())
	}
}

func TestCatalogueSubCaps(t *testing.T) {
	h, x := newCatalogueHarness(t, &fakeX402{price: 1500}, &fakeBazaar{items: testBazaarItems()}, catalogueConfig(t, `{}`, testCatalogue))
	if _, err := x.importCatalogue(context.Background(), h.now); err != nil {
		t.Fatal(err)
	}
	search, weather := openIDFor("/open/search", "GET"), openIDFor("/open/weather", "GET")
	vetOpen(t, h, x, search, weather)
	if _, err := h.call(testSubject, `{"resource":"`+search+`"}`, creditsFor(1500)); err != nil {
		t.Fatal(err)
	}
	// recipient_daily is 0.002: 1,500 + 1,500 to the same payTo would pass it.
	other := allowance.Subject{ID: "acct-2", Signed: true}
	if _, err := h.call(other, `{"resource":"`+search+`"}`, creditsFor(1500)); errCode(err) != "x402_cap_reached" {
		t.Fatalf("recipient cap: %v", err)
	}
	// A pinned resource to the same payTo is not under the open sub-caps.
	if _, err := h.call(other, `{"resource":"price"}`, creditsFor(10000)); err != nil {
		t.Fatalf("pinned: %v", err)
	}
	// open_daily is 0.004. On a new day (testPayTo's recipient cap fresh),
	// 3,500 already paid to two other recipients leaves 500: weather's
	// 1,000 is refused.
	h.now += 86400
	for i, payTo := range []string{deniedPayTo, "0x2222222222222222222222222222222222222222"} {
		if _, err := h.db.Exec(`INSERT INTO x402_payments(id,account,request_key,resource,day,amount,network,asset,pay_to,nonce,valid_before,state,allowlist_version,created_at)
VALUES(?,'acct-9',?,'elsewhere',?,1750,'eip155:8453',?,?,?,0,'paid',0,?)`, fmt.Sprint("p-", i), fmt.Sprint("id:", i), h.now/86400, testUSDC, payTo, fmt.Sprint("0xn", i), h.now); err != nil {
			t.Fatal(err)
		}
	}
	h.fake.price = 1000 // weather's listed price
	if _, err := h.call(other, `{"resource":"`+weather+`"}`, creditsFor(1500)); errCode(err) != "x402_cap_reached" {
		t.Fatalf("open daily cap: %v", err)
	}
}

func TestCatalogueStaleAndReapplied(t *testing.T) {
	h, x := newCatalogueHarness(t, &fakeX402{price: 1500}, &fakeBazaar{items: testBazaarItems()}, catalogueConfig(t, `{}`, testCatalogue))
	if _, err := x.importCatalogue(context.Background(), h.now); err != nil {
		t.Fatal(err)
	}
	// A config change applies on the next load: a lower max price.
	x.cfg.Catalogue.MaxPrice = 1200
	if err := x.loadCatalogue(context.Background(), h.now); err != nil {
		t.Fatal(err)
	}
	if got := x.cat.snap.Load().open; len(got) != 1 || got[0].ID != openIDFor("/open/weather", "GET") {
		t.Fatalf("after lowering max_price: %v", got)
	}
	x.cfg.Catalogue.MaxPrice = 2000
	x.cfg.Deny.Domains = append(x.cfg.Deny.Domains, "example.com")
	_ = x.loadCatalogue(context.Background(), h.now)
	if len(x.cat.snap.Load().open) != 0 {
		t.Fatal("a denied domain must leave the catalogue at once")
	}
	x.cfg.Deny.Domains = x.cfg.Deny.Domains[:len(x.cfg.Deny.Domains)-1]
	// Not seen for three refresh intervals: stale, not listed, not callable,
	// and still stored.
	h.now += 3*6*3600 + 1
	_ = x.loadCatalogue(context.Background(), h.now)
	if len(x.cat.snap.Load().open) != 0 {
		t.Fatal("stale resources must not be listed")
	}
	if _, err := h.call(testSubject, `{"resource":"`+openIDFor("/open/weather", "GET")+`"}`, creditsFor(1500)); errCode(err) != "x402_unknown_resource" {
		t.Fatalf("a stale resource is callable: %v", err)
	}
	var rows int
	_ = h.db.QueryRow("SELECT count(*) FROM x402_catalogue").Scan(&rows)
	if rows != 3 {
		t.Fatalf("stale rows must be kept, not deleted: %d", rows)
	}
}

func TestCatalogueWorkAndKillSwitch(t *testing.T) {
	bazaar := &fakeBazaar{items: testBazaarItems()}
	h, x := newCatalogueHarness(t, &fakeX402{price: 1500}, bazaar, catalogueConfig(t, `{}`, testCatalogue))
	kill := filepath.Join(t.TempDir(), "x402.off")
	h.cfg.KillFile = kill
	_ = os.WriteFile(kill, nil, 0o600)
	if _, err := x.Work(context.Background(), h.db, h.now); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if bazaar.hits.Load() != 0 || x.cat.importing.Load() {
		t.Fatal("a paused relay must not import")
	}
	_ = os.Remove(kill)
	if _, err := x.Work(context.Background(), h.db, h.now); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for x.cat.importing.Load() || len(x.cat.snap.Load().open) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the background import did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Not due again until refresh_minutes have passed.
	hits := bazaar.hits.Load()
	_, _ = x.Work(context.Background(), h.db, h.now+60)
	time.Sleep(50 * time.Millisecond)
	if bazaar.hits.Load() != hits {
		t.Fatal("an import ran before it was due")
	}
	// The kill switch stops open calls exactly like pinned ones.
	vetOpen(t, h, x, openIDFor("/open/search", "GET"))
	_ = os.WriteFile(kill, nil, 0o600)
	if _, err := h.call(testSubject, `{"resource":"`+openIDFor("/open/search", "GET")+`"}`, creditsFor(1500)); errCode(err) != "service_unavailable" {
		t.Fatalf("killed: %v", err)
	}
}

func TestCatalogueRankingDemotesFailures(t *testing.T) {
	h, x := newCatalogueHarness(t, &fakeX402{price: 1500}, &fakeBazaar{items: testBazaarItems()}, catalogueConfig(t, `{}`, testCatalogue))
	if _, err := x.importCatalogue(context.Background(), h.now); err != nil {
		t.Fatal(err)
	}
	scrape := openIDFor("/open/scrape", "POST")
	for range 3 {
		x.countCall(scrape, h.now, refusal("upstream_failed"), true)
	}
	x.countCall(scrape, h.now, refusal("x402_cap_reached"), true) // ours, not the upstream's: not counted
	x.countCall(scrape, h.now, refusal("upstream_failed"), false) // before any payment: the caller's arguments can cause it
	x.countCall(scrape, h.now, refusal("x402_price_changed"), false)
	x.countCall(scrape, h.now, refusal("x402_not_payable"), false)
	x.countCall(scrape, h.now, refusal("x402_unvetted"), false)
	_ = x.loadCatalogue(context.Background(), h.now)
	open := x.cat.snap.Load().open
	if open[len(open)-1].ID != scrape || x.cat.snap.Load().stats[scrape] != (callStats{0, 3}) {
		t.Fatalf("a failing resource must rank last: %v %v", open, x.cat.snap.Load().stats)
	}
}

func TestCatalogueConfigValidation(t *testing.T) {
	key, _ := hex.DecodeString(x402TestKey)
	signer, _ := signerFromKeyBytes(key)
	base := `{"schema":1,"enabled":true,"network":"eip155:8453","asset":"` + testUSDC + `","asset_name":"USD Coin","asset_version":"2","decimals":6,"caps":{},"wallet_key_file":"k","allowlist_file":"a"%s}`
	parse := func(extra, allowlist string) error {
		_, err := ParseX402Config([]byte(fmt.Sprintf(base, extra)), []byte(allowlist), signer)
		return err
	}
	empty := `{"schema":1,"version":1,"resources":[]}`
	if err := parse(`,"catalogue":{}`, empty); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	cfg, _ := ParseX402Config([]byte(fmt.Sprintf(base, `,"catalogue":{}`)), []byte(empty), signer)
	if cc := cfg.Catalogue; cc.MaxPrice != 20000 || cc.OpenDaily != 500000 || cc.RecipientDaily != 100000 || cc.DiscoveryURLs[0] != X402DiscoveryCDP || cc.MinPayers != 1 {
		t.Fatalf("catalogue defaults: %+v", cc)
	}
	for name, c := range map[string][2]string{
		"http discovery":        {`,"catalogue":{"discovery_urls":["http://example.com/d"]}`, empty},
		"private discovery":     {`,"catalogue":{"discovery_urls":["https://127.0.0.1/d"]}`, empty},
		"max price over cap":    {`,"catalogue":{"max_price":"0.06"}`, empty},
		"recipient over open":   {`,"catalogue":{"recipient_daily":"0.6","open_daily":"0.5"}`, empty},
		"open over global":      {`,"catalogue":{"open_daily":"3"}`, empty},
		"huge page":             {`,"catalogue":{"page_size":5000}`, empty},
		"fast refresh":          {`,"catalogue":{"refresh_minutes":1}`, empty},
		"unknown field":         {`,"catalogue":{"trust_everything":true}`, empty},
		"bad deny domain":       {``, `{"schema":1,"version":1,"resources":[],"deny":{"domains":["https://x.com/"]}}`},
		"bad deny pay_to":       {``, `{"schema":1,"version":1,"resources":[],"deny":{"pay_to":["0x12"]}}`},
		"missing domains file":  {``, `{"schema":1,"version":1,"resources":[],"deny":{"domains_file":"/nonexistent"}}`},
		"unconfigured bundler":  {``, `{"schema":1,"version":1,"resources":[{"id":"t","bundler":"frames","tool":"a.b","max_price":"0.01"}]}`},
		"frames without key":    {`,"bundlers":{"frames":{}}`, empty},
		"frames entry with url": {`,"bundlers":{"frames":{"key_file":"k"}}`, `{"schema":1,"version":1,"resources":[{"id":"t","bundler":"frames","tool":"a.b","url":"https://example.com/x","max_price":"0.01"}]}`},
		"tool on x402":          {``, `{"schema":1,"version":1,"resources":[{"id":"t","url":"https://example.com/x","method":"GET","pay_to":"` + testPayTo + `","tool":"a","max_price":"0.01"}]}`},
	} {
		if err := parse(c[0], c[1]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// fakeBundler is a key-based bundler that answers in memory: it reserves
// the maximum and settles charge.
type fakeBundler struct {
	ready  bool
	charge int64
	fail   bool
	calls  atomic.Int64
}

func (f *fakeBundler) Name() string { return "fake" }
func (f *fakeBundler) Ready() bool  { return f.ready }
func (f *fakeBundler) Exchange(ctx context.Context, p x402Plan, pay *payment) (x402Response, *x402Receipt, error) {
	f.calls.Add(1)
	if err := pay.reserve(ctx, p, reservation{amount: p.max, network: "fake", asset: "USD", payTo: "fake:" + p.res.ID, nonce: "fake-" + newCallID()}); err != nil {
		return x402Response{}, nil, err
	}
	if f.fail {
		pay.finish("unknown", 500, 0, "", "http_status") // may have been billed: it keeps counting
		return x402Response{}, nil, refusal("upstream_failed")
	}
	pay.settle(f.charge)
	return x402Response{status: 200, body: []byte(`{"tool":"` + p.res.ID + `","args":` + string(p.body) + `}`), contentType: "application/json"},
		&x402Receipt{Amount: strconv.FormatInt(f.charge, 10), Price: formatUnits(f.charge, 6), Asset: "USD", Network: "fake", PayTo: p.res.ID, Nonce: "n"}, nil
}

func fakeBundlerHarness(t *testing.T, fb *fakeBundler, caps string) *x402Harness {
	cfg := testX402Config(t, caps, `[]`)
	cfg.Bundlers = []Bundler{fb}
	cfg.Resources = append(cfg.Resources, X402Resource{ID: "fake-tool", URL: "https://example.com/fake", Method: http.MethodPost, Body: true,
		MaxAmount: 5000, MaxResponseBytes: 1024, Timeout: 2 * time.Second, Bundler: "fake", Tool: "tool.one", Category: "data"})
	return newX402HarnessCfg(t, &fakeX402{price: 1000}, cfg)
}

func TestBundlerInterface(t *testing.T) {
	fb := &fakeBundler{ready: true, charge: 2000}
	h := fakeBundlerHarness(t, fb, `{"agent_daily":"0.011","per_call":"0.005"}`)
	out, err := h.call(testSubject, `{"resource":"fake-tool","body":{"n":1}}`, creditsFor(5000))
	if err != nil {
		t.Fatal(err)
	}
	r := resultOf(out)
	if r["bundler"] != "fake" || r["text_is_untrusted"] != true || h.charged() != creditsFor(2000) {
		t.Fatalf("result %v charged %d", r, h.charged())
	}
	var amount int64
	var network, state string
	_ = h.db.QueryRow("SELECT amount, network, state FROM x402_payments").Scan(&amount, &network, &state)
	if amount != 2000 || network != "fake" || state != "paid" {
		t.Fatalf("payment: %d %s %s", amount, network, state)
	}
	// A failure with an unknown outcome is refunded in full (the resource is
	// pinned); what was reserved still counts.
	fb.fail = true
	before := h.charged()
	if _, err := h.call(testSubject, `{"resource":"fake-tool"}`, creditsFor(5000)); errCode(err) != "upstream_failed" || h.charged() != before {
		t.Fatalf("failure: %v, charged %d", err, h.charged())
	}
	// The agent's daily cap (0.011) covers every bundler: 2,000 + 5,000
	// reserved, so a third reservation of 5,000 passes it.
	fb.fail = false
	if _, err := h.call(testSubject, `{"resource":"fake-tool"}`, creditsFor(5000)); errCode(err) != "x402_cap_reached" {
		t.Fatalf("cap: %v", err)
	}
	// Not ready: unavailable and unlisted.
	fb.ready = false
	if _, err := h.call(allowance.Subject{ID: "acct-3", Signed: true}, `{"resource":"fake-tool"}`, creditsFor(5000)); errCode(err) != "service_unavailable" {
		t.Fatalf("not ready: %v", err)
	}
	if got := ids(h.resources(`{}`)); strings.Contains(strings.Join(got, ","), "fake-tool") {
		t.Fatalf("a bundler that is not ready lists nothing: %v", got)
	}
}

// fakeFrames is the Frames API's invoke endpoint.
type fakeFrames struct {
	mu      sync.Mutex
	status  int
	charged string
	last    map[string]any
	auth    string
}

func (f *fakeFrames) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = r.Header.Get("Authorization")
	raw, _ := io.ReadAll(r.Body)
	f.last = nil
	_ = json.Unmarshal(raw, &f.last)
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"error":"no"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"results":[{"id":"apollo.people_search","delivered":true,"response":{"people":[]}}],"billing":{"charged_credits":%s,"budget_credits":10,"balance_credits":2997,"percent_remaining":99}}`, f.charged)
}

const framesKey = "fk_test_0123456789abcdef"

func framesConfig(t *testing.T, withKey bool) *X402Config {
	t.Helper()
	key, _ := hex.DecodeString(x402TestKey)
	signer, _ := signerFromKeyBytes(key)
	config := `{"schema":1,"enabled":true,"network":"eip155:8453","asset":"` + testUSDC + `","asset_name":"USD Coin","asset_version":"2","decimals":6,"caps":{},
"wallet_key_file":"k","allowlist_file":"a","bundlers":{"frames":{"key_file":"/etc/frames.key","base_url":"https://example.com/v1"}}}`
	allowlist := `{"schema":1,"version":2,"resources":[{"id":"people","bundler":"frames","tool":"apollo.people_search","max_price":"0.01","timeout_seconds":2,"summary":"People search"}]}`
	var f x402ConfigFile
	if err := StrictObject([]byte(config), &f); err != nil {
		t.Fatal(err)
	}
	absent := func(string) ([]byte, error) { return nil, fs.ErrNotExist }
	cfg, err := parseX402Config(f, []byte(allowlist), signer, absent, func(path string) ([]byte, error) {
		if withKey && path == "/etc/frames.key" {
			return []byte(framesKey + "\n"), nil
		}
		return nil, fs.ErrNotExist
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestFramesBundler(t *testing.T) {
	ff := &fakeFrames{charged: "3"}
	cfg := framesConfig(t, true)
	h := newX402HarnessCfg(t, &fakeX402{extra: map[string]http.Handler{"/v1/tools/invoke": ff}}, cfg)
	if s := fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg.Bundlers); strings.Contains(s, framesKey) {
		t.Fatalf("the Frames key leaks when the config is formatted: %s", s)
	}
	out, err := h.call(testSubject, `{"resource":"people","body":{"title":"CTO"}}`, creditsFor(10000))
	if err != nil {
		t.Fatal(err)
	}
	if ff.auth != "Bearer "+framesKey {
		t.Fatalf("auth header %q", ff.auth)
	}
	calls, _ := ff.last["calls"].([]any)
	call, _ := calls[0].(map[string]any)
	if len(calls) != 1 || call["id"] != "apollo.people_search" || fmt.Sprint(call["args"]) != "map[title:CTO]" || ff.last["max_usd"] != 0.01 ||
		!strings.HasPrefix(fmt.Sprint(ff.last["idempotency_key"]), "swarmmemo-") {
		t.Fatalf("invoke body: %v", ff.last)
	}
	r := resultOf(out)
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), "balance_credits") || !strings.Contains(string(raw), `"people":[]`) || r["bundler"] != "frames" {
		t.Fatalf("result: %s", raw)
	}
	if h.charged() != creditsFor(3000) {
		t.Fatalf("charged %d, want the 3 Frames credits (3,000 micro-USD) priced", h.charged())
	}
	var amount int64
	var network string
	_ = h.db.QueryRow("SELECT amount, network FROM x402_payments").Scan(&amount, &network)
	if amount != 3000 || network != "frames" {
		t.Fatalf("payment %d %s", amount, network)
	}
	// Frames refusing the budget: nothing charged.
	ff.status = http.StatusPaymentRequired
	before := h.charged()
	if _, err := h.call(testSubject, `{"resource":"people"}`, creditsFor(10000)); errCode(err) != "x402_payment_rejected" || h.charged() != before {
		t.Fatalf("402: %v", err)
	}
}

func TestFramesWithoutKeyIsUnavailable(t *testing.T) {
	ff := &fakeFrames{charged: "1"}
	h := newX402HarnessCfg(t, &fakeX402{extra: map[string]http.Handler{"/v1/tools/invoke": ff}}, framesConfig(t, false))
	if _, err := h.call(testSubject, `{"resource":"people"}`, creditsFor(10000)); errCode(err) != "service_unavailable" {
		t.Fatalf("no key: %v", err)
	}
	if ff.last != nil || h.dials.Load() != 0 {
		t.Fatal("a bundler without its key must never be called")
	}
	page := h.resources(`{}`)
	if len(ids(page)) != 0 || strings.Contains(fmt.Sprint(page["bundlers"]), "frames") {
		t.Fatalf("listed without a key: %v", page)
	}
}

// Whatever a discovery item says, what the open catalogue admits is within
// the guardrails: https on 443 to a DNS name, a payable price at most the
// maximum, a non-zero recipient, a valid id, bounded query names and a clean,
// bounded summary.
func FuzzBazaarResource(f *testing.F) {
	for _, s := range testBazaarItems() {
		f.Add([]byte(s))
	}
	f.Add([]byte(`{"resource":"https://a.example/x","accepts":[{"scheme":"exact","network":"base","maxAmountRequired":"5","asset":"` + testUSDC + `","payTo":"` + testPayTo + `"}],"x402Version":1,"method":"post","metadata":{"description":"‮​hi"}}`))
	cfg := catalogueConfig(f, `{}`, testCatalogue)
	f.Fuzz(func(t *testing.T, raw []byte) {
		r, _, ok := bazaarResource(raw, cfg, cfg.Catalogue.MaxPrice)
		if !ok {
			return
		}
		r.ID = openID(r.ID, r.Method, r.URL)
		if !cfg.admitOpen(&r) {
			return
		}
		if checkX402URL(r.URL) != nil || r.MaxAmount <= 0 || r.MaxAmount > cfg.Catalogue.MaxPrice || r.PayTo == (EVMAddress{}) || !x402IDRE.MatchString(r.ID) ||
			len(r.Query) > X402QueryParamsMax || len(r.Summary) > 200 || strings.ContainsAny(r.Summary, "\x00\n\r‮​") || !r.Open || cfg.Deny.blocks(&r) {
			t.Fatalf("admitted outside the guardrails: %+v", r)
		}
	})
}

// The aggregator stays signed-only: an unsigned call, open resource or not,
// is refused before anything is quoted, fetched or paid.
func TestX402IsSignedOnly(t *testing.T) {
	h, x := newCatalogueHarness(t, &fakeX402{price: 1500}, &fakeBazaar{items: testBazaarItems()}, catalogueConfig(t, `{}`, testCatalogue))
	if _, err := x.importCatalogue(context.Background(), h.now); err != nil {
		t.Fatal(err)
	}
	dials := h.dials.Load()
	anon := allowance.Subject{ID: "anon:203.0.113.0/24", Signed: false}
	for _, res := range []string{"price", openIDFor("/open/search", "GET")} {
		tx, _ := h.db.Begin()
		_, err := h.engine.Call(context.Background(), tx, Request{Service: "x402", Data: `{"schema":1,"method":"call","args":{"resource":"` + res + `"},"max_cost":100000}`, Subject: anon, RequestKey: "id:0123456789abcdef"}, h.now)
		_ = tx.Rollback()
		if errCode(err) != "anonymous_not_allowed" {
			t.Fatalf("%s unsigned: %v", res, err)
		}
	}
	if h.dials.Load() != dials || h.charged() != 0 {
		t.Fatal("an unsigned call reached an upstream or was charged")
	}
	for _, m := range x402Methods(t) {
		if m.Name == "call" && m.Anonymous {
			t.Fatal("x402.call must not be marked anonymous")
		}
	}
}

func x402Methods(t *testing.T) []MethodEntry {
	t.Helper()
	return Catalog([]string{"x402"})[0].Methods
}

func TestX402StatsRead(t *testing.T) {
	h := newX402Harness(t, &fakeX402{price: 1500}, `{}`)
	if _, err := h.call(testSubject, `{"resource":"price"}`, creditsFor(10000)); err != nil {
		t.Fatal(err)
	}
	st, err := h.engine.Registry().ReadX402Stats(context.Background(), h.db, h.now, 7)
	if err != nil || st == nil {
		t.Fatal(st, err)
	}
	last := st.Days[len(st.Days)-1]
	if len(st.Days) != 7 || last.Paid != 1500 || last.Calls != 1 || last.Day != time.Unix(h.now, 0).UTC().Format("2006-01-02") || st.GlobalDaily != "2" || st.Pinned != 2 {
		t.Fatalf("stats: %+v", st)
	}
	if none, err := NewBuiltinRegistry(nil, Deps{}).ReadX402Stats(context.Background(), h.db, h.now, 7); none != nil || err != nil {
		t.Fatal("without x402 there are no stats")
	}
}
