package services_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
)

// pdKey is the api.data.gov and FRED key the fakes expect. No test may find
// it in a result, a record, the cache or an error.
const pdKey = "pdkey-SECRET-8e1b77aa90"

const fixtures = "testdata/public_data/"

// pdFake is one upstream host, served by its own httptest server.
type pdFake struct {
	host  string
	srv   *httptest.Server
	route func(r *http.Request) (file string, status int)

	mu       sync.Mutex
	hits     int
	mode     string // "", "500", "403", "redirect", "garbage"
	redirect string
	urls     []string
	headers  []http.Header
}

func (f *pdFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits++
	f.urls = append(f.urls, r.URL.String())
	f.headers = append(f.headers, r.Header.Clone())
	mode, redirect := f.mode, f.redirect
	f.mu.Unlock()
	switch mode {
	case "500":
		w.WriteHeader(500)
		return
	case "403":
		w.WriteHeader(403)
		return
	case "redirect":
		http.Redirect(w, r, redirect, http.StatusFound)
		return
	case "garbage":
		fmt.Fprint(w, "<html>not what you expected</html>")
		return
	}
	file, status := f.route(r)
	if status == 0 {
		status = 200
	}
	w.WriteHeader(status)
	if file != "" {
		body, err := os.ReadFile(fixtures + file)
		if err != nil {
			panic(err)
		}
		_, _ = w.Write(body)
	}
}

func (f *pdFake) set(mode string) {
	f.mu.Lock()
	f.mode = mode
	f.mu.Unlock()
}

func (f *pdFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

func (f *pdFake) seen() ([]string, []http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.urls...), append([]http.Header{}, f.headers...)
}

// pdRoutes is each upstream's fixture routing, by path and query, as the
// real service answers.
func pdRoutes(v *pdEnv) map[string]func(r *http.Request) (string, int) {
	keyed := func(r *http.Request) bool {
		return r.Header.Get("X-Api-Key") == pdKey && r.URL.Query().Get("api_key") == ""
	}
	fredKeyed := func(r *http.Request) bool { return r.URL.Query().Get("api_key") == pdKey }
	return map[string]func(r *http.Request) (string, int){
		"www.ncei.noaa.gov": func(r *http.Request) (string, int) {
			if r.URL.Path != "/access/services/data/v1" {
				return "", 404
			}
			v.mu.Lock()
			defer v.mu.Unlock()
			return v.noaaFile, 200
		},
		"noaadata.apps.nsidc.org": func(r *http.Request) (string, int) {
			if r.URL.Path != "/NOAA/G02135/north/daily/data/N_seaice_extent_daily_v4.0.csv" {
				return "", 404
			}
			return "nsidc_extent.csv", 200
		},
		"www.fsis.usda.gov": func(r *http.Request) (string, int) { return "fsis_recalls.json", 200 },
		"api.fda.gov": func(r *http.Request) (string, int) {
			if strings.Contains(strings.ToLower(r.URL.Query().Get("search")), `recalling_firm:"reser`) {
				return "fda_enforcement.json", 200
			}
			return "fda_not_found.json", 404
		},
		"api.congress.gov": func(r *http.Request) (string, int) {
			if !keyed(r) {
				return "", 403
			}
			return "congress_bills.json", 200
		},
		"api.open.fec.gov": func(r *http.Request) (string, int) {
			if !keyed(r) {
				return "", 403
			}
			if strings.HasPrefix(r.URL.Path, "/v1/candidates/search/") {
				return "fec_search.json", 200
			}
			if r.URL.Path == "/v1/candidate/H2TX30123/totals/" {
				return "fec_totals.json", 200
			}
			return "", 404
		},
		"api.coingecko.com": func(r *http.Request) (string, int) { return "coingecko_price.json", 200 },
		"api.stlouisfed.org": func(r *http.Request) (string, int) {
			if !fredKeyed(r) {
				return "", 400
			}
			q := r.URL.Query()
			switch {
			case r.URL.Path == "/fred/releases":
				return "fred_releases.json", 200
			case r.URL.Path == "/fred/release/dates":
				return "fred_release_dates.json", 200
			case q.Get("series_id") == "GDPC1":
				return "fred_gdpc1.json", 200
			case q.Get("series_id") == "DFEDTARU":
				return "fred_dfedtaru.json", 200
			case q.Get("series_id") == "DFEDTARL":
				return "fred_dfedtarl.json", 200
			case q.Get("series_id") == "GDPNOW":
				return "fred_gdpnow_vintages.json", 200
			}
			return "", 400
		},
		"stats.bis.org": func(r *http.Request) (string, int) {
			area := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/data/WS_CBPOL/D."), "/all")
			v.mu.Lock()
			fail := v.bisFail[area]
			v.mu.Unlock()
			if fail {
				return "", 500
			}
			if _, err := os.Stat(fixtures + "bis_" + area + ".csv"); err == nil {
				return "bis_" + area + ".csv", 200
			}
			return "bis_default.csv", 200
		},
		"api.bcb.gov.br":     func(r *http.Request) (string, int) { return "bcb_selic.json", 200 },
		"www.atlantafed.org": func(r *http.Request) (string, int) { return "gdpnow_card.html", 200 },
		"www.newyorkfed.org": func(r *http.Request) (string, int) {
			name := strings.TrimPrefix(r.URL.Path, "/medialibrary/Research/Interactives/Data/NowCast/data/")
			switch name {
			case "nowcast-history-meta-data.csv":
				return "nyfed_nowcast_meta.csv", 200
			case "2026Q3.csv", "2026Q4.csv":
				return "nyfed_nowcast_" + name, 200
			}
			return "", 404
		},
		"www.clevelandfed.org": func(r *http.Request) (string, int) {
			return "cleveland_" + strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/-/media/files/webcharts/inflationnowcasting/nowcast_"), ".json") + ".json", 200
		},
	}
}

type tierClassifier struct{ tier allowance.Tier }

func (c tierClassifier) Classify(context.Context, allowance.Querier, allowance.Subject, int64) (allowance.Standing, error) {
	return allowance.Standing{Tier: c.tier}, nil
}

type pdEnv struct {
	db     *sql.DB
	e      *services.Engine
	meter  *servicestest.Meter
	cfg    *services.PublicDataConfig
	fakes  map[string]*pdFake
	keyDir string
	cls    allowance.Classifier
	n      int

	mu       sync.Mutex
	noaaFile string
	bisFail  map[string]bool
}

func newPDEnv(t *testing.T, keys bool) *pdEnv {
	t.Helper()
	v := &pdEnv{db: openDB(t), meter: servicestest.NewMeter(1 << 20), fakes: map[string]*pdFake{}, noaaFile: "noaa_mwn.json", bisFail: map[string]bool{}}
	v.keyDir = t.TempDir()
	if keys {
		for _, name := range []string{services.KeyAPIDataGov, services.KeyFRED} {
			if err := os.WriteFile(filepath.Join(v.keyDir, name), []byte(pdKey+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	remap := map[string]string{}
	routes := pdRoutes(v)
	for _, h := range services.PublicDataHosts() {
		route, ok := routes[h]
		if !ok {
			t.Fatalf("no fake for catalogue host %s", h)
		}
		f := &pdFake{host: h, route: route}
		f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
		t.Cleanup(f.srv.Close)
		v.fakes[h] = f
		remap[h] = strings.TrimPrefix(f.srv.URL, "http://")
	}
	v.cfg = services.PublicDataConfigForTest(v.keyDir, remap, false)
	v.build(t)
	return v
}

func (v *pdEnv) build(t *testing.T) {
	reg := services.NewBuiltinRegistry([]string{"public_data"}, services.Deps{DB: v.db, PublicData: v.cfg, Classifier: v.cls})
	v.e = services.NewEngine(services.Config{DB: v.db, Registry: reg, Meter: v.meter, Now: func() int64 { return 1000 }, HoldsPerAccount: 16})
	t.Cleanup(v.e.Stop)
}

var pdSubject = allowance.Subject{ID: "acct-pd", KeyID: "k", Signed: true}

// call runs one service.call to completion and returns the call data, the
// request key and the refusal, if any.
func (v *pdEnv) call(t *testing.T, method, args string, now int64) (map[string]any, string, error) {
	t.Helper()
	v.n++
	key := fmt.Sprintf("id:pd%d", v.n)
	data := fmt.Sprintf(`{"schema":1,"method":%q,"args":%s,"max_cost":100}`, method, args)
	tx, err := v.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	out, err := v.e.Call(context.Background(), tx, services.Request{Service: "public_data", Data: data, Subject: pdSubject, RequestKey: key}, now)
	if err != nil {
		tx.Rollback()
		return nil, key, err
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if out.After == nil {
		t.Fatal("public_data must run after commit")
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

// fetch is one fetch; it returns the item (the call's result).
func (v *pdEnv) fetch(t *testing.T, dataset, params string, now int64) (map[string]any, error) {
	t.Helper()
	res, _, err := v.call(t, "fetch", fmt.Sprintf(`{"dataset":%q,"params":%s}`, dataset, params), now)
	if err != nil {
		return nil, err
	}
	item, _ := res["result"].(map[string]any)
	if item == nil {
		t.Fatalf("no result: %v", res)
	}
	return item, nil
}

func (v *pdEnv) mustFetch(t *testing.T, dataset, params string, now int64) (map[string]any, map[string]any) {
	t.Helper()
	item, err := v.fetch(t, dataset, params, now)
	if err != nil {
		t.Fatalf("%s %s: %v", dataset, params, err)
	}
	data, _ := item["data"].(map[string]any)
	return item, data
}

func unix(date string) int64 {
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		panic(err)
	}
	return d.Add(12 * time.Hour).Unix()
}

// num reads a JSON number at a path of keys and indexes.
func at(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			l, _ := v.([]any)
			if k >= len(l) {
				return nil
			}
			v = l[k]
		}
	}
	return v
}

func near(t *testing.T, what string, got any, want float64) {
	t.Helper()
	f, ok := got.(float64)
	if !ok || math.Abs(f-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %#v, want %#v", what, got, want)
	}
}

// --- datasets (parity with the Pythia adapters' tests) ------------------------

func TestPublicDataNOAAStation(t *testing.T) {
	v := newPDEnv(t, false)
	item, d := v.mustFetch(t, "noaa_station_daily", `{"station":"USW00014755","start_date":"2026-01-10","end_date":"2026-01-12"}`, unix("2026-01-20"))
	eq(t, "station", d["station"], "USW00014755")
	eq(t, "count", d["count"], 3.0)
	eq(t, "order", []any{at(d, "days", 0, "date"), at(d, "days", 1, "date"), at(d, "days", 2, "date")}, []any{"2026-01-12", "2026-01-11", "2026-01-10"})
	d10 := at(d, "days", 2)
	eq(t, "d10 element", at(d10, "peak_gust_element"), "WSF2")
	near(t, "d10 gust ms", at(d10, "peak_gust_ms"), 52.3)
	near(t, "d10 gust mph", at(d10, "peak_gust_mph"), 117.0)
	near(t, "d10 avg ms", at(d10, "avg_wind_ms"), 23.8)
	near(t, "d10 avg mph", at(d10, "avg_wind_mph"), 53.2)
	near(t, "d10 tmax c", at(d10, "tmax_c"), -1.7)
	near(t, "d10 tmax f", at(d10, "tmax_f"), 28.9)
	near(t, "d10 tmin c", at(d10, "tmin_c"), -9.4)
	near(t, "d10 precip", at(d10, "precip_mm"), 1.5)
	eq(t, "d11 element", at(d, "days", 1, "peak_gust_element"), "WSFG")
	near(t, "d11 gust", at(d, "days", 1, "peak_gust_ms"), 60.0)
	eq(t, "max date", d["peak_gust_max_date"], "2026-01-11")
	near(t, "max mph", d["peak_gust_max_mph"], 134.2)
	eq(t, "schema", item["schema_version"], 1.0)
	eq(t, "as_of", item["as_of"], "2026-01-12")
	if !strings.HasPrefix(item["source_url"].(string), "https://www.ncei.noaa.gov/access/services/data/v1?") {
		t.Errorf("source_url %v", item["source_url"])
	}
	urls, _ := v.fakes["www.ncei.noaa.gov"].seen()
	for _, want := range []string{"dataset=daily-summaries", "stations=USW00014755", "units=metric", "format=json", "startDate=2026-01-10", "endDate=2026-01-12"} {
		if !strings.Contains(urls[0], want) {
			t.Errorf("request %s lacks %s", urls[0], want)
		}
	}

	// An alias resolves to its GHCN id; the request echoes what was asked;
	// the default window is the 30 days to yesterday.
	_, d = v.mustFetch(t, "noaa_station_daily", `{"station":"kmwn"}`, unix("2026-01-20"))
	eq(t, "alias", d["station"], "USW00014755")
	eq(t, "requested", d["requested_station"], "kmwn")
	eq(t, "default end", d["end_date"], "2026-01-19")
	eq(t, "default start", d["start_date"], "2025-12-21")

	// Truncation keeps the newest rows; the summary spans the whole window.
	v.mu.Lock()
	v.noaaFile = "noaa_long.json"
	v.mu.Unlock()
	_, d = v.mustFetch(t, "noaa_station_daily", `{"station":"KMWN","start_date":"2026-03-01","end_date":"2026-04-14"}`, unix("2026-05-01"))
	eq(t, "rows", len(d["days"].([]any)), 31)
	eq(t, "truncated", d["truncated"], true)
	eq(t, "full count", d["count"], 45.0)
	near(t, "full-window max", d["peak_gust_max_mph"], 120.8)
	eq(t, "newest first", at(d, "days", 0, "date"), "2026-04-14")
}

func TestPublicDataSeaIce(t *testing.T) {
	v := newPDEnv(t, false)
	_, d := v.mustFetch(t, "sea_ice_extent", `{"date":"2026-07-10"}`, unix("2026-07-12"))
	eq(t, "km2", d["extent_km2"], 8123000.0)
	eq(t, "n_years", d["n_years"], 3.0)
	eq(t, "rank", d["rank_low_to_high"], 1.0)
	near(t, "anomaly", d["anomaly_million_km2"], -0.385)
	eq(t, "no series without a range", len(d["observations"].([]any)), 0)

	item, d := v.mustFetch(t, "sea_ice_extent", `{}`, unix("2026-07-12"))
	eq(t, "latest", d["date"], "2026-07-10")
	eq(t, "as_of", item["as_of"], "2026-07-10")

	_, d = v.mustFetch(t, "sea_ice_extent", `{"start_date":"2026-07-07","end_date":"2026-07-09","limit":2}`, unix("2026-07-12"))
	eq(t, "window", d["observations"], []any{map[string]any{"date": "2026-07-09", "value": 8.19}, map[string]any{"date": "2026-07-08", "value": 8.254}})
	eq(t, "count", d["count"], 3.0)
	eq(t, "next page", d["next_end_date"], "2026-07-07")
	eq(t, "truncated", d["truncated"], true)

	_, d = v.mustFetch(t, "sea_ice_extent", `{"date":"2026-01-01"}`, unix("2026-07-12"))
	if d["extent_km2"] != nil || !strings.Contains(d["note"].(string), "2026-01-01") {
		t.Errorf("a missing day is null with a note: %v", d)
	}
}

func TestPublicDataRecalls(t *testing.T) {
	v := newPDEnv(t, false)
	item, d := v.mustFetch(t, "food_recalls", `{"firm":"Synear"}`, unix("2026-07-20"))
	eq(t, "count", d["count"], 1.0)
	eq(t, "source", at(d, "recalls", 0, "source"), "FSIS (USDA)")
	eq(t, "firm", at(d, "recalls", 0, "firm"), "Synear Foods USA LLC")
	eq(t, "status", at(d, "recalls", 0, "status"), "Active")
	eq(t, "class", at(d, "recalls", 0, "classification"), "Class I")
	eq(t, "date", at(d, "recalls", 0, "date"), "2026-07-18")
	eq(t, "untrusted", item["text_is_untrusted"], true)

	item, d = v.mustFetch(t, "food_recalls", `{"firm":"Reser","status":"ongoing"}`, unix("2026-07-20"))
	eq(t, "fda count", d["count"], 1.0)
	eq(t, "fda source", at(d, "recalls", 0, "source"), "FDA (openFDA food enforcement)")
	eq(t, "fda date", at(d, "recalls", 0, "date"), "2026-07-10")
	eq(t, "fda status", at(d, "recalls", 0, "status"), "Ongoing")
	eq(t, "status normalised", at(item, "params", "status"), "active")
	_, d = v.mustFetch(t, "food_recalls", `{"firm":"Reser","status":"closed"}`, unix("2026-07-20"))
	eq(t, "closed", d["count"], 0.0)
	_, d = v.mustFetch(t, "food_recalls", `{"firm":"Jerky","status":"closed"}`, unix("2026-07-20"))
	eq(t, "fsis closed", at(d, "recalls", 0, "status"), "Closed")

	// The FSIS list is fetched once and served from the cache for every firm.
	if n := v.fakes["www.fsis.usda.gov"].count(); n != 1 {
		t.Errorf("FSIS fetched %d times, want 1", n)
	}
	// FSIS refusing us still answers from openFDA, saying what is missing.
	v2 := newPDEnv(t, false)
	v2.fakes["www.fsis.usda.gov"].set("403")
	item, d = v2.mustFetch(t, "food_recalls", `{"firm":"Reser"}`, unix("2026-07-20"))
	eq(t, "fda still served", d["count"], 1.0)
	eq(t, "failed", d["sources_failed"], []any{"FSIS"})
	if !strings.Contains(d["note"].(string), "FSIS") {
		t.Errorf("note must name the missing source: %v", d["note"])
	}
	if at(item, "sources", 0, "cache") != "error" {
		t.Errorf("the failed source is listed: %v", item["sources"])
	}
	// Both failing is a failure, refunded.
	v2.fakes["api.fda.gov"].set("500")
	if _, err := v2.fetch(t, "food_recalls", `{"firm":"Nobody"}`, unix("2026-07-20")); code(err) != "upstream_busy" {
		t.Errorf("every source down: %v", err)
	}
}

func TestPublicDataCongressAndFEC(t *testing.T) {
	v := newPDEnv(t, true)
	_, d := v.mustFetch(t, "congress_bills", `{"query":"shutdown","congress":119}`, unix("2026-07-01"))
	eq(t, "count", d["count"], 3.0)
	eq(t, "dates", []any{at(d, "bills", 0, "latest_action_date"), at(d, "bills", 1, "latest_action_date"), at(d, "bills", 2, "latest_action_date")},
		[]any{"2026-06-10", "2026-05-01", "2026-01-15"})
	eq(t, "labels", []any{at(d, "bills", 0, "bill"), at(d, "bills", 1, "bill"), at(d, "bills", 2, "bill")}, []any{"S. 742", "H.R. 5371", "H.Res. 88"})
	title := at(d, "bills", 2, "title").(string)
	if len([]rune(title)) > 90 || !strings.HasSuffix(title, "...") {
		t.Errorf("title truncation: %q", title)
	}
	_, d = v.mustFetch(t, "congress_bills", `{"query":"appropriations shutdown"}`, unix("2026-07-01"))
	eq(t, "and", d["count"], 1.0)
	eq(t, "default congress", d["congress"], 119.0)
	_, d = v.mustFetch(t, "congress_bills", `{"query":"cryptocurrency"}`, unix("2026-07-01"))
	eq(t, "no match", d["bills"], []any{})

	_, d = v.mustFetch(t, "fec_candidate_totals", `{"candidate":"Crockett","cycle":2026}`, unix("2026-07-01"))
	eq(t, "id", d["candidate_id"], "H2TX30123")
	eq(t, "name", d["candidate_name"], "CROCKETT, JASMINE")
	near(t, "receipts", d["receipts"], 1e6)
	near(t, "cash", d["cash_on_hand"], 5e5)
	_, d = v.mustFetch(t, "fec_candidate_totals", `{"candidate_id":"h2tx30123"}`, unix("2026-07-01"))
	near(t, "by id", d["disbursements"], 5e5)

	// The key travels only in the X-Api-Key header.
	for _, h := range []string{"api.congress.gov", "api.open.fec.gov"} {
		urls, headers := v.fakes[h].seen()
		for i := range urls {
			if strings.Contains(urls[i], pdKey) || headers[i].Get("X-Api-Key") != pdKey {
				t.Errorf("%s: key must be in the header only: %s", h, urls[i])
			}
		}
	}
}

func TestPublicDataCryptoAndFRED(t *testing.T) {
	v := newPDEnv(t, true)
	item, d := v.mustFetch(t, "crypto_spot_price", `{"coin_id":"Bitcoin"}`, unix("2026-09-20"))
	near(t, "price", d["price"], 61234.0)
	eq(t, "vs", d["vs_currency"], "usd")
	eq(t, "as_of", item["as_of"], "2026-09-21T14:13:20Z")

	_, d = v.mustFetch(t, "fred_series", `{"series_id":"gdpc1"}`, unix("2026-09-20"))
	eq(t, "count", d["count"], 2.0)
	eq(t, "latest", d["latest"], map[string]any{"date": "2026-04-01", "value": 2.8})
	eq(t, "units", d["units"], "pca")
	if !strings.Contains(d["description"].(string), "GDP") {
		t.Errorf("description %v", d["description"])
	}
	_, d = v.mustFetch(t, "fred_series", `{"series_id":"GDPC1","start_date":"2026-02-01"}`, unix("2026-09-20"))
	eq(t, "range", d["count"], 1.0)
	urls, _ := v.fakes["api.stlouisfed.org"].seen()
	if !strings.Contains(urls[0], "units=pca") || !strings.Contains(urls[0], "series_id=GDPC1") {
		t.Errorf("FRED request %s", urls[0])
	}
}

func TestPublicDataReleaseCalendar(t *testing.T) {
	v := newPDEnv(t, true)
	now := unix("2026-07-16")
	_, d := v.mustFetch(t, "fred_release_calendar", `{}`, now)
	eq(t, "today", d["today"], "2026-07-16")
	eq(t, "names", []any{at(d, "release_matched", 0, "release"), at(d, "release_matched", 1, "release"), at(d, "release_matched", 2, "release")},
		[]any{"Gross Domestic Product", "Consumer Price Index", "Employment Situation"})
	eq(t, "next", at(d, "release_matched", 0, "next_dates"), []any{"2026-07-30", "2026-08-26", "2026-09-30"})
	eq(t, "last", at(d, "release_matched", 0, "last_date"), "2026-06-25")
	_, d = v.mustFetch(t, "fred_release_calendar", `{"release":"gross domestic"}`, now)
	eq(t, "matched", len(d["release_matched"].([]any)), 1)
	eq(t, "id", at(d, "release_matched", 0, "release_id"), 53.0)
	_, d = v.mustFetch(t, "fred_release_calendar", `{"days_ahead":20}`, now)
	eq(t, "window", at(d, "release_matched", 0, "next_dates"), []any{"2026-07-30"})
	_, d = v.mustFetch(t, "fred_release_calendar", `{"release":"nonexistent zzz"}`, now)
	eq(t, "none", d["release_matched"], []any{})

	// The FOMC calendar needs no key and no request.
	k := newPDEnv(t, false)
	for _, q := range []string{"FOMC", "next fed meeting", "fed rate decision schedule 2026"} {
		item, d := k.mustFetch(t, "fred_release_calendar", fmt.Sprintf(`{"release":%q}`, q), now)
		eq(t, "fomc next", at(d, "release_matched", 0, "next_dates", 0), "2026-07-29")
		eq(t, "fomc meeting", at(d, "release_matched", 0, "next_meetings", 0), "2026-07-28 to 2026-07-29")
		eq(t, "fomc last", at(d, "release_matched", 0, "last_date"), "2026-06-17")
		eq(t, "static", item["cache"], "static")
	}
	_, d = k.mustFetch(t, "fred_release_calendar", `{"release":"FOMC","days_ahead":20}`, now)
	eq(t, "fomc window", at(d, "release_matched", 0, "next_dates"), []any{"2026-07-29"})
	if k.fakes["api.stlouisfed.org"].count() != 0 {
		t.Error("the FOMC path must not reach FRED")
	}
	if _, err := k.fetch(t, "fred_release_calendar", `{}`, now); code(err) != "upstream_unavailable" {
		t.Errorf("FRED releases without a key: %v", err)
	}
}

func TestPublicDataCBPolicy(t *testing.T) {
	v := newPDEnv(t, false)
	now := unix("2026-08-15")
	item, d := v.mustFetch(t, "cb_policy_rates", `{"bank":"ECB"}`, now)
	ecb := at(d, "banks", "ECB")
	near(t, "rate", at(ecb, "policy_rate_pct"), 2.25)
	eq(t, "as_of", at(ecb, "rate_as_of"), "2026-08-12")
	eq(t, "effective", at(ecb, "rate_effective_from"), "2026-06-17")
	near(t, "previous", at(ecb, "previous_rate_pct"), 2.0)
	near(t, "bps", at(ecb, "last_change_bps"), 25.0)
	eq(t, "next", at(ecb, "next_decision"), "2026-09-10")
	eq(t, "upcoming", at(ecb, "upcoming_decisions"), []any{"2026-09-10", "2026-10-29", "2026-12-17", "2027-02-04"})
	eq(t, "not stale", at(ecb, "rate_may_be_stale"), false)
	eq(t, "cache", item["cache"], "mixed") // a live rate and a static calendar
	urls, _ := v.fakes["stats.bis.org"].seen()
	if !strings.Contains(urls[0], "/D.XM/all") || !strings.Contains(urls[0], "detail=dataonly") {
		t.Errorf("BIS request %s", urls[0])
	}

	_, d = v.mustFetch(t, "cb_policy_rates", `{"bank":"Bank of England","what":"rate"}`, now)
	boe := at(d, "banks", "BOE")
	near(t, "NaN dropped", at(boe, "previous_rate_pct"), 4.0)
	eq(t, "boe effective", at(boe, "rate_effective_from"), "2026-06-03")
	eq(t, "rate only", at(boe, "next_decision"), nil)
	_, d = v.mustFetch(t, "cb_policy_rates", `{"bank":"swiss","what":"rate"}`, now)
	if !strings.Contains(at(d, "banks", "SNB", "last_change_note").(string), "unchanged") {
		t.Errorf("SNB: %v", at(d, "banks", "SNB"))
	}
	_, d = v.mustFetch(t, "cb_policy_rates", `{"bank":"RBA"}`, now)
	eq(t, "rba stale", at(d, "banks", "RBA", "rate_may_be_stale"), true)
	eq(t, "rba next", at(d, "banks", "RBA", "next_decision"), "2026-09-29")
	eq(t, "rba last", at(d, "banks", "RBA", "last_scheduled_decision_in_table"), "2026-08-11")
	_, d = v.mustFetch(t, "cb_policy_rates", `{"bank":"brazil"}`, now)
	bcb := at(d, "banks", "BCB")
	near(t, "selic", at(bcb, "selic_target_pct"), 14.0)
	eq(t, "selic from", at(bcb, "selic_effective_from"), "2026-08-06")
	eq(t, "live copom", at(bcb, "next_decision"), "2026-09-16")
	if !strings.Contains(at(bcb, "calendar_source").(string), "SGS 432") {
		t.Errorf("BCB calendar source %v", at(bcb, "calendar_source"))
	}
	hits := v.fakes["stats.bis.org"].count()
	_, d = v.mustFetch(t, "cb_policy_rates", `{"bank":"ECB","what":"next_meeting"}`, now)
	if v.fakes["stats.bis.org"].count() != hits || at(d, "banks", "ECB", "policy_rate_pct") != nil {
		t.Error("next_meeting makes no upstream request")
	}
	_, d = v.mustFetch(t, "cb_policy_rates", `{"bank":"ECB","what":"next_meeting"}`, unix("2027-01-05"))
	if !strings.Contains(d["calendar_staleness_warning"].(string), "2026-12-31") {
		t.Errorf("the calendar staleness guard: %v", d)
	}

	// The Fed's target range comes from FRED when the key is there.
	f := newPDEnv(t, true)
	_, d = f.mustFetch(t, "cb_policy_rates", `{"bank":"Federal Reserve","what":"rate"}`, now)
	fed := at(d, "banks", "FED")
	near(t, "midpoint", at(fed, "policy_rate_pct"), 3.625)
	eq(t, "range", at(fed, "target_range_pct"), []any{3.5, 3.75})
	_, d = v.mustFetch(t, "cb_policy_rates", `{"bank":"fed","what":"rate"}`, now)
	eq(t, "no key, no range", at(d, "banks", "FED", "target_range_pct"), nil)

	// A sweep: one bank's dead series costs that bank its rate only.
	v.mu.Lock()
	v.bisFail["JP"] = true
	v.mu.Unlock()
	_, d = v.mustFetch(t, "cb_policy_rates", `{"bank":"all"}`, now)
	if n := len(d["banks"].(map[string]any)); n != 9 {
		t.Fatalf("all: %d banks", n)
	}
	eq(t, "boj rate gone", at(d, "banks", "BOJ", "rate_available"), false)
	eq(t, "boj calendar kept", at(d, "banks", "BOJ", "next_decision"), "2026-09-18")
	near(t, "ecb unharmed", at(d, "banks", "ECB", "policy_rate_pct"), 2.25)
}

func TestPublicDataNowcasts(t *testing.T) {
	now := unix("2026-09-26")
	v := newPDEnv(t, true)
	_, d := v.mustFetch(t, "us_nowcasts", `{"measure":"gdp"}`, now)
	g := at(d, "nowcasts", 0)
	eq(t, "name", at(g, "name"), "Atlanta Fed GDPNow")
	eq(t, "quarter", at(g, "target_period"), "2026:Q3")
	near(t, "value", at(g, "latest_value"), 5.0)
	eq(t, "as_of", at(g, "latest_as_of"), "2026-09-25")
	eq(t, "next", at(g, "next_update"), "2026-09-30")
	near(t, "unrounded", at(g, "latest_value_unrounded"), 5.0163)
	near(t, "previous", at(g, "previous_value"), 5.0796)
	eq(t, "previous as_of", at(g, "previous_as_of"), "2026-09-17")
	urls, _ := v.fakes["api.stlouisfed.org"].seen()
	if !strings.Contains(urls[0], "realtime_end=9999-12-31") || !strings.Contains(urls[0], "series_id=GDPNOW") {
		t.Errorf("vintages request %s", urls[0])
	}
	var q3, q4 map[string]any
	for _, n := range d["nowcasts"].([]any) {
		m := n.(map[string]any)
		if m["name"] == "NY Fed Staff Nowcast" && m["target_period"] == "2026:Q3" {
			q3 = m
		}
		if m["name"] == "NY Fed Staff Nowcast" && m["target_period"] == "2026:Q4" {
			q4 = m
		}
	}
	if q3 == nil || q4 == nil {
		t.Fatalf("NY Fed open quarters: %v", d["nowcasts"])
	}
	near(t, "q3", q3["latest_value"], 2.331)
	eq(t, "q3 as_of", q3["latest_as_of"], "2026-09-25")
	near(t, "q3 previous", q3["previous_value"], 2.331)
	eq(t, "q3 previous as_of", q3["previous_as_of"], "2026-09-18")
	eq(t, "q3 next", q3["next_update"], "2026-10-02")
	eq(t, "q3 50%", at(q3, "bands_pct", "50%"), []any{1.18, 3.46})
	eq(t, "q3 80%", at(q3, "bands_pct", "80%"), []any{0.11, 4.57})
	if !strings.Contains(q3["source_statement"].(string), "2026:Q3 is 2.3%") {
		t.Errorf("statement %v", q3["source_statement"])
	}
	near(t, "q4", q4["latest_value"], 2.592)
	if len(d["nowcasts"].([]any)) != 3 {
		t.Errorf("Q2 (advance print out) is not a nowcast: %v", d["nowcasts"])
	}

	_, d = v.mustFetch(t, "us_nowcasts", `{"measure":"CPI"}`, now)
	eq(t, "measure", d["measure"], "inflation")
	var sep, aug map[string]any
	for _, n := range d["nowcasts"].([]any) {
		m := n.(map[string]any)
		switch m["target_period"] {
		case "2026-09":
			sep = m
		case "2026-08":
			aug = m
		}
	}
	cpi := at(sep, "series", "CPI")
	near(t, "mom", at(cpi, "mom_nowcast"), 0.5)
	near(t, "yoy", at(cpi, "yoy_nowcast"), 3.569)
	eq(t, "mom as_of", at(cpi, "mom_as_of"), "2026-09-25")
	near(t, "mom previous", at(cpi, "mom_previous"), 0.435)
	eq(t, "mom previous as_of", at(cpi, "mom_previous_as_of"), "2026-09-22")
	eq(t, "unchanged since", at(cpi, "mom_unchanged_since"), "2026-09-23")
	near(t, "aug actual", at(aug, "series", "CPI", "mom_actual"), 0.396)
	eq(t, "aug actual date", at(aug, "series", "CPI", "mom_actual_date"), "2026-09-11")
	if at(aug, "series", "PCE", "yoy_actual") != nil {
		t.Error("PCE's print is not out yet")
	}

	// Without the FRED key the previous GDPNow value is null, and says why.
	k := newPDEnv(t, false)
	_, d = k.mustFetch(t, "us_nowcasts", `{"measure":"gdp"}`, now)
	g = at(d, "nowcasts", 0)
	if at(g, "previous_value") != nil || !strings.Contains(at(g, "previous_note").(string), "FRED key") || k.fakes["api.stlouisfed.org"].count() != 0 {
		t.Errorf("no key: %v", g)
	}
	// A source that stops answering is served from its last copy, flagged.
	k.fakes["www.newyorkfed.org"].set("500")
	item, d := k.mustFetch(t, "us_nowcasts", `{"measure":"gdp"}`, unix("2026-09-28"))
	eq(t, "stale kept", len(d["nowcasts"].([]any)), 3)
	eq(t, "flagged", item["stale"], true)
	// One source down with no copy degrades that source only.
	k2 := newPDEnv(t, false)
	k2.fakes["www.newyorkfed.org"].set("500")
	_, d = k2.mustFetch(t, "us_nowcasts", `{"measure":"gdp"}`, now)
	eq(t, "only gdpnow", len(d["nowcasts"].([]any)), 1)
	eq(t, "error named", at(d, "source_errors", 0, "source"), "NY Fed Staff Nowcast")
}

// --- the framework ---------------------------------------------------------------

func TestPublicDataParamValidation(t *testing.T) {
	now := unix("2026-09-29")
	bad := []string{
		`{}`,
		`{"dataset":"nope"}`,
		`{"dataset":"sea_ice_extent","params":[]}`,
		`{"dataset":"sea_ice_extent","params":{"date":"2026-7-1"}}`,
		`{"dataset":"sea_ice_extent","params":{"date":"2026-02-30"}}`,
		`{"dataset":"sea_ice_extent","params":{"date":20260701}}`,
		`{"dataset":"sea_ice_extent","params":{"start_date":"2026-07-02","end_date":"2026-07-01"}}`,
		`{"dataset":"sea_ice_extent","params":{"limit":0}}`,
		`{"dataset":"sea_ice_extent","params":{"limit":1201}}`,
		`{"dataset":"sea_ice_extent","params":{"limit":"5"}}`,
		`{"dataset":"sea_ice_extent","params":{"limit":1.5}}`,
		`{"dataset":"sea_ice_extent","params":{"url":"https://evil.example/"}}`,
		`{"dataset":"sea_ice_extent","params":{"date":"2026-07-01","date":"2026-07-02"}}`,
		`{"dataset":"sea_ice_extent","extra":1}`,
		`{"dataset":"noaa_station_daily","params":{}}`,
		`{"dataset":"noaa_station_daily","params":{"station":"../etc"}}`,
		`{"dataset":"noaa_station_daily","params":{"station":"USW0001475"}}`,
		`{"dataset":"noaa_station_daily","params":{"station":"KMWN","start_date":"2024-01-01","end_date":"2026-01-01"}}`,
		`{"dataset":"noaa_station_daily","params":{"station":"KMWN","elements":"WSFG,EVIL"}}`,
		`{"dataset":"noaa_station_daily","params":{"station":"KMWN","elements":"WSFG,WSFG"}}`,
		`{"dataset":"food_recalls","params":{"firm":"a\"b"}}`,
		`{"dataset":"food_recalls","params":{"firm":"   "}}`,
		`{"dataset":"food_recalls","params":{"firm":"x\u0000y"}}`,
		`{"dataset":"food_recalls","params":{"firm":"Synear","status":"maybe"}}`,
		`{"dataset":"congress_bills","params":{"query":"x","congress":0}}`,
		`{"dataset":"fec_candidate_totals","params":{}}`,
		`{"dataset":"fec_candidate_totals","params":{"candidate":"A B","candidate_id":"H2TX30123"}}`,
		`{"dataset":"fec_candidate_totals","params":{"candidate_id":"../x"}}`,
		`{"dataset":"fec_candidate_totals","params":{"candidate":"Crockett","cycle":2025}}`,
		`{"dataset":"crypto_spot_price","params":{"coin_id":"bit coin"}}`,
		`{"dataset":"crypto_spot_price","params":{"coin_id":"bitcoin","vs_currency":"u"}}`,
		`{"dataset":"fred_series","params":{"series_id":"SP500"}}`,
		`{"dataset":"fred_series","params":{"series_id":"GDPC1","units":"sqrt"}}`,
		`{"dataset":"cb_policy_rates","params":{"bank":"Central Bank of Narnia"}}`,
		`{"dataset":"cb_policy_rates","params":{"bank":"ECB","what":"market_pricing"}}`,
		`{"dataset":"us_nowcasts","params":{"measure":"vibes"}}`,
	}
	for _, b := range bad {
		if _, _, err := services.PublicDataParseForTest("fetch", json.RawMessage(b), now); code(err) != "invalid_service_data" {
			t.Errorf("%s: %v", b, err)
		}
	}
	for _, b := range []string{`{"requests":[]}`, `{"requests":[` + strings.Repeat(`{"dataset":"sea_ice_extent"},`, 10) + `{"dataset":"sea_ice_extent"}]}`} {
		if _, _, err := services.PublicDataParseForTest("bulk", json.RawMessage(b), now); code(err) != "invalid_service_data" {
			t.Errorf("bulk %s: %v", b[:20], err)
		}
	}
	ids, params, err := services.PublicDataParseForTest("fetch", json.RawMessage(`{"dataset":"cb_policy_rates","params":{"bank":" Copom ","what":"next-meeting"}}`), now)
	if err != nil || ids[0] != "cb_policy_rates" || params[0]["what"] != "next_meeting" || params[0]["bank"] != "Copom" {
		t.Errorf("normalised: %v %v %v", ids, params, err)
	}
	_, params, _ = services.PublicDataParseForTest("fetch", json.RawMessage(`{"dataset":"congress_bills","params":{"query":"shutdown"}}`), unix("2027-01-04"))
	if params[0]["congress"] != int64(120) {
		t.Errorf("current congress: %v", params[0])
	}
	// Nothing invalid is reserved or sent.
	v := newPDEnv(t, true)
	if _, _, err := v.call(t, "fetch", `{"dataset":"fred_series","params":{"series_id":"SP500"}}`, now); code(err) != "invalid_service_data" {
		t.Fatal(err)
	}
	entries, _ := v.meter.Entries(context.Background(), v.db)
	if len(entries) != 0 || v.fakes["api.stlouisfed.org"].count() != 0 {
		t.Errorf("a refused call reserves and sends nothing: %v", entries)
	}
}

func TestPublicDataHostAllowlist(t *testing.T) {
	v := newPDEnv(t, false)
	now := unix("2026-07-12")
	// A host outside the dataset's catalogue entry is refused before any
	// request; so is any catalogue host the dataset does not list.
	for _, host := range []string{"evil.example", "api.coingecko.com"} {
		err := services.PublicDataFetchForTest(v.cfg, v.db, "sea_ice_extent", host, "/x", now)
		if err == nil || !strings.Contains(err.Error(), "host_not_in_catalogue") {
			t.Errorf("%s: %v", host, err)
		}
	}
	if v.fakes["api.coingecko.com"].count() != 0 {
		t.Error("a host the dataset does not list must not be reached")
	}
	// A redirect is never followed, even to another catalogue host.
	trap := v.fakes["api.coingecko.com"]
	nsidc := v.fakes["noaadata.apps.nsidc.org"]
	nsidc.mu.Lock()
	nsidc.redirect = trap.srv.URL + "/api/v3/simple/price"
	nsidc.mu.Unlock()
	nsidc.set("redirect")
	item, err := v.fetch(t, "sea_ice_extent", `{}`, now)
	if code(err) != "upstream_failed" || trap.count() != 0 {
		t.Fatalf("redirect: %v %v, trap hits %d", item, err, trap.count())
	}
	// Through the production safe dialer, loopback is refused at connect.
	safe := services.PublicDataConfigForTest(v.keyDir, map[string]string{"noaadata.apps.nsidc.org": strings.TrimPrefix(nsidc.srv.URL, "http://")}, true)
	nsidc.set("")
	hits := nsidc.count()
	err = services.PublicDataFetchForTest(safe, v.db, "sea_ice_extent", "noaadata.apps.nsidc.org", "/NOAA/G02135/north/daily/data/N_seaice_extent_daily_v4.0.csv", now)
	if err == nil || !strings.Contains(err.Error(), "upstream_busy") || nsidc.count() != hits {
		t.Errorf("safe dialer must refuse loopback: %v", err)
	}
	// An oversized or malformed body fails cleanly.
	nsidc.set("garbage")
	if _, err = v.fetch(t, "sea_ice_extent", `{}`, now+86400*365); code(err) != "upstream_failed" {
		t.Errorf("garbage: %v", err)
	}
}

func TestPublicDataCacheAndStale(t *testing.T) {
	v := newPDEnv(t, false)
	f := v.fakes["noaadata.apps.nsidc.org"]
	t0 := unix("2026-07-12")
	item, _ := v.mustFetch(t, "sea_ice_extent", `{}`, t0)
	eq(t, "first", item["cache"], "miss")
	eq(t, "fetched", item["fetched_at"], time.Unix(t0, 0).UTC().Format(time.RFC3339))
	eq(t, "expires", item["expires_at"], time.Unix(t0+12*3600, 0).UTC().Format(time.RFC3339))
	item, _ = v.mustFetch(t, "sea_ice_extent", `{"date":"2026-07-09"}`, t0+60)
	eq(t, "hit", item["cache"], "hit")
	eq(t, "stale", item["stale"], false)
	if f.count() != 1 {
		t.Fatalf("a fresh copy is served from the cache: %d fetches", f.count())
	}
	// A restart keeps the cache (it is in the database).
	v.build(t)
	item, _ = v.mustFetch(t, "sea_ice_extent", `{}`, t0+120)
	eq(t, "hit after restart", item["cache"], "hit")
	// Past the TTL it is fetched again.
	item, _ = v.mustFetch(t, "sea_ice_extent", `{}`, t0+13*3600)
	eq(t, "expired", item["cache"], "miss")
	if f.count() != 2 {
		t.Fatalf("expired copy refetched: %d", f.count())
	}
	// Upstream down past the TTL: the old copy, flagged stale, never silently.
	f.set("500")
	t1 := t0 + 26*3600
	item, d := v.mustFetch(t, "sea_ice_extent", `{}`, t1)
	eq(t, "stale", item["stale"], true)
	eq(t, "stale cache", item["cache"], "stale")
	eq(t, "still data", d["date"], "2026-07-10")
	eq(t, "fetched_at is the old fetch", item["fetched_at"], time.Unix(t0+13*3600, 0).UTC().Format(time.RFC3339))
	if !strings.Contains(at(item, "sources", 0, "stale_reason").(string), "http_500") {
		t.Errorf("stale reason: %v", item["sources"])
	}
	// Too old to serve even stale: a refusal, refunded.
	_, key, err := v.call(t, "fetch", `{"dataset":"sea_ice_extent"}`, t0+31*86400)
	if code(err) != "upstream_busy" {
		t.Fatalf("beyond the stale window: %v", err)
	}
	if h := pdHold(t, v, key); h.State != "refunded" {
		t.Errorf("refunded: %+v", h)
	}
}

func pdHold(t *testing.T, v *pdEnv, key string) servicestest.Entry {
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

func TestPublicDataRateLimitsFollowTier(t *testing.T) {
	now := unix("2026-07-12") - unix("2026-07-12")%60 // the start of a minute
	bulk := `{"requests":[` + strings.TrimSuffix(strings.Repeat(`{"dataset":"sea_ice_extent"},`, 10), ",") + `]}`
	v := newPDEnv(t, false)
	for i := 0; i < 3; i++ {
		if _, _, err := v.call(t, "bulk", bulk, now+int64(i)); err != nil {
			t.Fatalf("bulk %d: %v", i, err)
		}
	}
	before, _ := v.meter.Entries(context.Background(), v.db)
	_, _, err := v.call(t, "fetch", `{"dataset":"sea_ice_extent"}`, now+5)
	var ae *allowance.Err
	if code(err) != "request_rate" || !errorsAs(err, &ae) || ae.RetryAfter != 55 {
		t.Fatalf("a signed caller gets 30 requests a minute: %v", err)
	}
	after, _ := v.meter.Entries(context.Background(), v.db)
	if len(after) != len(before) {
		t.Error("a rate-limited call reserves nothing")
	}
	if _, _, err = v.call(t, "fetch", `{"dataset":"sea_ice_extent"}`, now+60); err != nil {
		t.Fatalf("the next minute: %v", err)
	}
	// A trusted caller is not held to the signed tier's limit.
	v.cls = tierClassifier{allowance.TierTrusted}
	v.build(t)
	for i := 0; i < 5; i++ {
		if _, _, err := v.call(t, "bulk", bulk, now+120+int64(i)); err != nil {
			t.Fatalf("trusted bulk %d: %v", i, err)
		}
	}
}

func errorsAs(err error, target **allowance.Err) bool {
	e, ok := err.(*allowance.Err)
	if ok {
		*target = e
	}
	return ok
}

func TestPublicDataBulkAndPricing(t *testing.T) {
	v := newPDEnv(t, false)
	now := unix("2026-07-12")
	res, key, err := v.call(t, "bulk", `{"requests":[{"dataset":"sea_ice_extent"},{"dataset":"crypto_spot_price","params":{"coin_id":"bitcoin"}},{"dataset":"fred_series","params":{"series_id":"GDPC1"}}]}`, now)
	if err != nil {
		t.Fatal(err)
	}
	results := at(res, "result", "results").([]any)
	eq(t, "items", len(results), 3)
	eq(t, "ok", at(results[0], "error"), nil)
	eq(t, "unavailable without its key", at(results[2], "error", "code"), "upstream_unavailable")
	eq(t, "bulk envelope", at(res, "result", "envelope_version"), 1.0)
	// Charged per successful item: two of three.
	if h := pdHold(t, v, key); h.MaxUnits != 3 || h.Units != 2 || h.State != "committed" {
		t.Errorf("hold %+v", h)
	}
	row := v.db.QueryRow("SELECT cost, public FROM service_calls WHERE request_key=?", key)
	var cost int64
	var public string
	if err = row.Scan(&cost, &public); err != nil || cost != 2 || !strings.Contains(public, `"ok":2`) {
		t.Errorf("record: %d %s %v", cost, public, err)
	}
	// A fetch of an unavailable dataset is refused and refunded.
	_, key, err = v.call(t, "fetch", `{"dataset":"congress_bills","params":{"query":"x"}}`, now)
	if code(err) != "upstream_unavailable" {
		t.Fatalf("no key: %v", err)
	}
	if h := pdHold(t, v, key); h.State != "refunded" {
		t.Errorf("refund: %+v", h)
	}
}

func TestPublicDataCatalogue(t *testing.T) {
	v := newPDEnv(t, false)
	cat := services.PublicDataDatasetsForTest(v.cfg)
	list := cat["datasets"].([]any)
	if len(list) != 10 {
		t.Fatalf("%d datasets", len(list))
	}
	avail := map[string]bool{}
	for _, e := range list {
		m := e.(map[string]any)
		avail[m["id"].(string)] = m["available"].(bool)
		for _, k := range []string{"description", "params", "source", "cache_ttl_seconds", "price", "schema_version", "output"} {
			if m[k] == nil {
				t.Errorf("%s lacks %s", m["id"], k)
			}
		}
		src := m["source"].(map[string]any)
		if src["licence"] == "" || src["terms_url"] == "" || len(src["hosts"].([]string)) == 0 {
			t.Errorf("%s source: %v", m["id"], src)
		}
	}
	for id, want := range map[string]bool{"sea_ice_extent": true, "congress_bills": false, "fec_candidate_totals": false, "fred_series": false,
		"fred_release_calendar": true, "cb_policy_rates": true, "us_nowcasts": true} {
		if avail[id] != want {
			t.Errorf("%s available %v, want %v", id, avail[id], want)
		}
	}
	// The read is the same function.
	tx, _ := v.db.Begin()
	defer tx.Rollback()
	out, err := v.e.Read(context.Background(), tx, services.Request{Service: "public_data", Data: `{"schema":1,"method":"datasets"}`, Subject: allowance.Subject{ID: "anon:x"}}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out["result"])
	want, _ := json.Marshal(cat)
	var got1, want1 any
	_ = json.Unmarshal(raw, &got1)
	_ = json.Unmarshal(want, &want1)
	if !reflect.DeepEqual(got1, want1) {
		t.Error("the datasets read and the catalogue must be one function")
	}
	// Off by default: not in SERVICES, not callable.
	if _, err = services.NewBuiltinRegistry(nil, services.Deps{DB: v.db, PublicData: v.cfg}).Lookup("public_data"); code(err) != "invalid_service" {
		t.Errorf("off by default: %v", err)
	}
	c, _ := v.e.Catalogue(context.Background(), v.db, 1000)
	raw, _ = json.Marshal(c)
	if !strings.Contains(string(raw), `"datasets_available":7`) {
		t.Errorf("services.list: %s", raw)
	}
}

func TestPublicDataNeverLeaksKey(t *testing.T) {
	v := newPDEnv(t, true)
	now := unix("2026-09-26")
	v.mustFetch(t, "fred_series", `{"series_id":"GDPC1"}`, now)
	v.mustFetch(t, "us_nowcasts", `{}`, now)
	v.mustFetch(t, "congress_bills", `{"query":"shutdown"}`, now)
	v.fakes["api.stlouisfed.org"].set("500")
	res, _, err := v.call(t, "fetch", `{"dataset":"fred_release_calendar","params":{"release":"GDP"}}`, now)
	raw, _ := json.Marshal(res)
	if strings.Contains(string(raw), pdKey) || (err != nil && strings.Contains(err.Error(), pdKey)) {
		t.Error("the key leaked into a result or error")
	}
	for _, q := range []string{"SELECT group_concat(body || public || error, '') FROM service_calls", "SELECT group_concat(body || source_url || key, '') FROM public_data_cache"} {
		var all sql.NullString
		if err := v.db.QueryRow(q).Scan(&all); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(all.String, pdKey) || strings.Contains(all.String, "api_key=") {
			t.Errorf("the key leaked: %s", q)
		}
	}
}

// FuzzPublicDataParams feeds arbitrary fetch and bulk args to the parser:
// it never panics, and every accepted request's resolved parameters parse
// again to themselves.
func FuzzPublicDataParams(f *testing.F) {
	for _, s := range []string{
		`{"dataset":"noaa_station_daily","params":{"station":"KMWN","start_date":"2026-01-01","elements":"wsfg,tmax","limit":5}}`,
		`{"dataset":"sea_ice_extent","params":{"date":"2026-07-10","start_date":"2026-01-01","end_date":"2026-07-01"}}`,
		`{"dataset":"food_recalls","params":{"firm":"Reser's Fine Foods","status":"Completed"}}`,
		`{"dataset":"congress_bills","params":{"query":"debt limit","congress":119,"limit":3}}`,
		`{"dataset":"fec_candidate_totals","params":{"candidate_id":"H2TX30123","cycle":2026}}`,
		`{"dataset":"crypto_spot_price","params":{"coin_id":"ethereum","vs_currency":"EUR"}}`,
		`{"dataset":"fred_series","params":{"series_id":"dgs10","units":"CH1"}}`,
		`{"dataset":"fred_release_calendar","params":{"release":"FOMC","days_ahead":30}}`,
		`{"dataset":"cb_policy_rates","params":{"bank":"the Bank of England (BoE)","what":"policy rate"}}`,
		`{"dataset":"us_nowcasts","params":{"measure":"prices"}}`,
		`{"requests":[{"dataset":"sea_ice_extent"},{"dataset":"us_nowcasts"}]}`,
	} {
		f.Add(s, s[2] == 'r')
	}
	now := unix("2026-09-29")
	f.Fuzz(func(t *testing.T, args string, bulk bool) {
		method := "fetch"
		if bulk {
			method = "bulk"
		}
		ids, params, err := services.PublicDataParseForTest(method, json.RawMessage(args), now)
		if err != nil {
			if code(err) != "invalid_service_data" {
				t.Fatalf("unexpected refusal %v", err)
			}
			return
		}
		for i, id := range ids {
			again, _ := json.Marshal(map[string]any{"dataset": id, "params": params[i]})
			ids2, params2, err := services.PublicDataParseForTest("fetch", again, now)
			if err != nil || ids2[0] != id {
				t.Fatalf("resolved params do not parse again: %s: %v", again, err)
			}
			a, _ := json.Marshal(params[i])
			b, _ := json.Marshal(params2[0])
			if string(a) != string(b) {
				t.Fatalf("not canonical: %s then %s", a, b)
			}
		}
	})
}
