package services_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
)

// TestRobotsDecideNamesTheLine: a refusal names the Disallow line that
// decided it; an allowed path names none (C842).
func TestRobotsDecideNamesTheLine(t *testing.T) {
	const robots = "User-agent: *\nDisallow: /data/\nDisallow: /data/global\nAllow: /data/global/ok\n"
	if ok, line := services.RobotsDecideForTest(robots, "/data/x.csv"); ok || line != "Disallow: /data/" {
		t.Fatalf("/data/x.csv: %v %q", ok, line)
	}
	// The longest match decides, and it is the one named.
	if ok, line := services.RobotsDecideForTest(robots, "/data/global-historical/x.csv"); ok || line != "Disallow: /data/global" {
		t.Fatalf("longest: %v %q", ok, line)
	}
	if ok, line := services.RobotsDecideForTest(robots, "/data/global/ok"); !ok || line != "" {
		t.Fatalf("allowed: %v %q", ok, line)
	}
}

// TestRobotsLineIsSanitised: a hostile Disallow pattern (control bytes,
// backticks, non-ASCII, very long) is named as printable ASCII without
// backticks, at most 80 bytes.
func TestRobotsLineIsSanitised(t *testing.T) {
	hostile := "/x`\x01\x1b[31mé`" + strings.Repeat("A", 500)
	ok, line := services.RobotsDecideForTest("User-agent: *\nDisallow: "+hostile+"\n", hostile)
	if ok || line == "" {
		t.Fatalf("not refused: %v %q", ok, line)
	}
	if len(line) > 80 || strings.ContainsAny(line, "`\x01\x1b") || !strings.HasPrefix(line, "Disallow: /x[31m") || !strings.HasSuffix(line, "...") {
		t.Fatalf("line: %q (%d bytes)", line, len(line))
	}
	for i := 0; i < len(line); i++ {
		if line[i] < 0x20 || line[i] >= 0x7f {
			t.Fatalf("byte %d of %q is not printable ASCII", i, line)
		}
	}
}

// TestPublicDataDatasetsForHost: derived from the catalogue's Hosts.
func TestPublicDataDatasetsForHost(t *testing.T) {
	if ids := services.PublicDataDatasetsForHost("www.ncei.noaa.gov"); !slices.Contains(ids, "noaa_station_daily") {
		t.Fatalf("noaa: %v", ids)
	}
	if ids := services.PublicDataDatasetsForHost("WWW.NCEI.NOAA.GOV"); !slices.Contains(ids, "noaa_station_daily") {
		t.Fatalf("case: %v", ids)
	}
	// The site a dataset host is on, and its other hosts, name it too
	// (C1147): congress.gov's pages, CoinGecko's API and website.
	for host, want := range map[string]string{
		"congress.gov": "congress_bills", "www.congress.gov": "congress_bills", "api.congress.gov": "congress_bills",
		"api.coingecko.com": "crypto_spot_price", "www.coingecko.com": "crypto_spot_price", "coingecko.com.": "crypto_spot_price",
		"fred.stlouisfed.org": "fred_series",
	} {
		if ids := services.PublicDataDatasetsForHost(host); len(ids) == 0 || ids[0] != want {
			t.Fatalf("%s: %v, want %s first", host, ids, want)
		}
	}
	for _, host := range []string{"site.test", "gov", "evil.gov", "congress.gov.evil.test", "notcongress.gov", "coingecko.org"} {
		if ids := services.PublicDataDatasetsForHost(host); len(ids) != 0 {
			t.Fatalf("unrelated host %s: %v", host, ids)
		}
	}
	for _, h := range services.PublicDataHosts() {
		if len(services.PublicDataDatasetsForHost(h)) == 0 {
			t.Fatalf("catalogue host %s names no dataset", h)
		}
		// A host's site is never a bare public suffix (co.uk, gov.br).
		if site := h[strings.IndexByte(h, '.')+1:]; strings.Count(h, ".") >= 2 && strings.Count(site, ".") == 1 {
			switch site[:strings.IndexByte(site, '.')] {
			case "co", "com", "gov", "org", "net", "ac", "edu":
				t.Fatalf("catalogue host %s: its site %s is a public suffix", h, site)
			}
		}
	}
}

// TestPublicDataCallsValidate: the call a refusal names for each dataset
// (C1147) is one public_data.fetch accepts as sent.
func TestPublicDataCallsValidate(t *testing.T) {
	for _, id := range services.PublicDataIDsForTest() {
		call := services.PublicDataCallForTest(id)
		args, ok := strings.CutPrefix(call, "public_data.fetch ")
		if !ok {
			t.Fatalf("%s: %q", id, call)
		}
		ids, _, err := services.PublicDataParseForTest("fetch", json.RawMessage(args), time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC).Unix())
		if err != nil || len(ids) != 1 || ids[0] != id {
			t.Fatalf("%s: %s: %v %v", id, call, ids, err)
		}
	}
}

// TestFetchRefusalNamesPublicData: a site refusal on a site public_data
// reads carries details.public_data {dataset, call}, also from the cache; an
// unrelated host carries nothing (C1147).
func TestFetchRefusalNamesPublicData(t *testing.T) {
	s := newSite(t, false, map[string]http.HandlerFunc{
		"/robots.txt": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "User-agent: *\nDisallow: /api/v3/coins/\n")
		},
		"/bill/119th-congress/house-bill/1": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `<html><head><title>Just a moment...</title></head><body><script src="/cdn-cgi/challenge-platform/x.js"></script></body></html>`)
		},
		"/forbidden": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no bots", http.StatusForbidden) },
		"/slow-down": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "later", http.StatusTooManyRequests) },
		"/gone":      func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
	})
	r := newFetchRig(t, s, fetchOpts{hosts: map[string][]net.IP{
		"api.coingecko.com": {publicIP}, "www.congress.gov": {publicIP}, "site.test": {publicIP},
	}})
	refused := func(url, code string) map[string]any {
		t.Helper()
		_, err := r.fetch("alice", noScreen(url))
		var ae *allowance.Err
		if !errors.As(err, &ae) || ae.Code != code {
			t.Fatalf("%s: got %v, want %s", url, err, code)
		}
		d, _ := ae.Details.(map[string]any)
		return d
	}
	hint := func(d map[string]any) (string, string) {
		h, _ := d["public_data"].(map[string]any)
		ds, _ := h["dataset"].(string)
		call, _ := h["call"].(string)
		return ds, call
	}
	for i := 0; i < 2; i++ { // the second answer is the cached refusal
		d := refused("http://api.coingecko.com/api/v3/coins/bitcoin", "fetch_robots")
		if ds, call := hint(d); ds != "crypto_spot_price" || call != `public_data.fetch {"dataset":"crypto_spot_price","params":{"coin_id":"bitcoin"}}` || d["robots_line"] != "Disallow: /api/v3/coins/" {
			t.Fatalf("coingecko try %d: %#v", i, d)
		}
	}
	d := refused("http://www.congress.gov/bill/119th-congress/house-bill/1", "fetch_captcha")
	if ds, call := hint(d); ds != "congress_bills" || !strings.HasPrefix(call, `public_data.fetch {"dataset":"congress_bills","params":{"query":`) {
		t.Fatalf("congress: %#v", d)
	}
	for _, c := range []struct{ path, code string }{{"/forbidden", "fetch_blocked"}, {"/slow-down", "fetch_site_rate_limited"}} {
		if ds, _ := hint(refused("http://api.coingecko.com"+c.path, c.code)); ds != "crypto_spot_price" {
			t.Fatalf("%s: no hint", c.code)
		}
		// Only the refused call on record (C1157), no hint.
		if d := refused("http://site.test"+c.path, c.code); len(d) != 1 || d["call_id"] == nil {
			t.Fatalf("unrelated %s: %#v", c.code, d)
		}
	}
	// Not a site refusal: no hint.
	if d := refused("http://api.coingecko.com/gone", "fetch_not_found"); d != nil {
		t.Fatalf("not found: %#v", d)
	}
}

// TestFetchRobotsCarriesTheLine: a fetch refused by robots.txt carries the
// matched line for the board's message, and still does from the cache.
func TestFetchRobotsCarriesTheLine(t *testing.T) {
	s := newSite(t, false, map[string]http.HandlerFunc{
		"/robots.txt": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "User-agent: *\nDisallow: /private\n")
		},
	})
	r := newFetchRig(t, s, fetchOpts{hosts: map[string][]net.IP{"site.test": {publicIP}, "www.ncei.noaa.gov": {publicIP}}})
	for i := 0; i < 2; i++ {
		_, err := r.fetch("alice", noScreen("http://site.test/private/x"))
		var ae *allowance.Err
		if !errors.As(err, &ae) || ae.Code != "fetch_robots" {
			t.Fatalf("got %v", err)
		}
		d, _ := ae.Details.(map[string]any)
		if d["robots_line"] != "Disallow: /private" || d["public_data"] != nil {
			t.Fatalf("try %d details: %#v", i, ae.Details)
		}
	}
}

// TestFetchRobotsNamesTheDataset: on a public_data host the refusal also
// carries the datasets that read it.
func TestFetchRobotsNamesTheDataset(t *testing.T) {
	s := newSite(t, false, map[string]http.HandlerFunc{
		"/robots.txt": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "User-agent: *\nDisallow: /data/\n") },
	})
	r := newFetchRig(t, s, fetchOpts{hosts: map[string][]net.IP{"www.ncei.noaa.gov": {publicIP}}})
	_, err := r.fetch("alice", noScreen("http://www.ncei.noaa.gov/data/global-historical-climatology-network-daily/access/USW00014755.csv"))
	var ae *allowance.Err
	if !errors.As(err, &ae) || ae.Code != "fetch_robots" {
		t.Fatalf("got %v", err)
	}
	d, _ := ae.Details.(map[string]any)
	if h, _ := d["public_data"].(map[string]any); d["robots_line"] != "Disallow: /data/" || h["dataset"] != "noaa_station_daily" {
		t.Fatalf("details: %#v", ae.Details)
	}
}

// TestFetchRobotsUnreachable: robots.txt answering 5xx refuses with the
// status (RFC 9309); a host that drops the robots.txt connection is
// fetch_upstream_error, retryable, never fetch_robots; a 404 allows (C1052).
func TestFetchRobotsUnreachable(t *testing.T) {
	five := newSite(t, false, map[string]http.HandlerFunc{
		"/robots.txt": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) },
	})
	r := newFetchRig(t, five, fetchOpts{hosts: map[string][]net.IP{"site.test": {publicIP}}})
	_, err := r.fetch("alice", noScreen("http://site.test/page"))
	var ae *allowance.Err
	if !errors.As(err, &ae) || ae.Code != "fetch_robots" {
		t.Fatalf("5xx: %v", err)
	}
	if d, _ := ae.Details.(map[string]any); d["robots_status"] != 503 || d["robots_line"] != nil {
		t.Fatalf("5xx details: %#v", ae.Details)
	}
	drop := newSite(t, false, map[string]http.HandlerFunc{
		"/robots.txt": func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		},
	})
	r = newFetchRig(t, drop, fetchOpts{hosts: map[string][]net.IP{"site.test": {publicIP}}})
	_, err = r.fetch("alice", noScreen("http://site.test/page"))
	if !errors.As(err, &ae) || ae.Code != "fetch_upstream_error" {
		t.Fatalf("unreachable: %v", err)
	}
	if drop.hit("/page") != 0 {
		t.Fatal("page requested without robots.txt")
	}
}
