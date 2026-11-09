package services_test

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"

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
	if ids := services.PublicDataDatasetsForHost("site.test"); len(ids) != 0 {
		t.Fatalf("unknown host: %v", ids)
	}
	for _, h := range services.PublicDataHosts() {
		if len(services.PublicDataDatasetsForHost(h)) == 0 {
			t.Fatalf("catalogue host %s names no dataset", h)
		}
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
		if d["robots_line"] != "Disallow: /private" || d["datasets"] != nil {
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
	if ids, _ := d["datasets"].([]string); d["robots_line"] != "Disallow: /data/" || !slices.Contains(ids, "noaa_station_daily") {
		t.Fatalf("details: %#v", ae.Details)
	}
}
