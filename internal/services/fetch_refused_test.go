package services_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
)

var callIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// refusedCallID is the details.call_id of a fetch refusal, "" when it has
// none.
func refusedCallID(t *testing.T, err error) string {
	t.Helper()
	var ae *allowance.Err
	if !errors.As(err, &ae) {
		t.Fatalf("not a refusal: %v", err)
	}
	d, _ := ae.Details.(map[string]any)
	id, _ := d["call_id"].(string)
	if id != "" && !callIDPattern.MatchString(id) {
		t.Fatalf("call_id %q", id)
	}
	return id
}

// refusedEntry is the tool history entry /api/calls/ID serves for id.
func refusedEntry(t *testing.T, r *fetchRig, id string) (map[string]any, string) {
	t.Helper()
	row, ok, err := r.e.HistoryByID(context.Background(), r.db, id, r.now)
	if err != nil || !ok {
		t.Fatalf("no entry for %s: %v %v", id, ok, err)
	}
	raw, _ := json.Marshal(row.Entry)
	return roundTrip(t, row.Entry), string(raw)
}

func serviceCallCount(t *testing.T, r *fetchRig) int {
	t.Helper()
	var n int
	if err := r.db.QueryRow("SELECT count(*) FROM service_calls").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// C1157: a fetch the site itself refuses (a bot challenge, robots.txt,
// 401/403, 429) answers with details.call_id, and that call's public record
// says refused, with the URL's host and path (never its query string), the
// code, the robots.txt line, the time and cost 0; a retry of the same page
// from the cache gets a record of its own. An invalid URL or a private
// address is no site's refusal and keeps no record.
func TestFetchRefusalIsOnRecord(t *testing.T) {
	s := newSite(t, false, map[string]http.HandlerFunc{
		"/robots.txt": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "User-agent: SwarmMemoFetch\nDisallow: /private\n")
		},
		"/challenge": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><head><title>Just a moment...</title></head><body><script src="/cdn-cgi/challenge-platform/x.js"></script></body></html>`)
		},
		"/forbidden": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no bots", http.StatusForbidden) },
	})
	hosts := map[string][]net.IP{"site.test": {publicIP}, "intranet.test": {net.ParseIP("10.1.2.3")}}
	r := newFetchRig(t, s, fetchOpts{hosts: hosts})

	_, err := r.fetch("alice", noScreen("http://site.test/challenge?token=SECRET-TOKEN#frag"))
	if code(err) != "fetch_captcha" {
		t.Fatalf("captcha: %v", err)
	}
	id := refusedCallID(t, err)
	if id == "" {
		t.Fatal("a captcha refusal has no call_id")
	}
	var signedErr *allowance.Err
	if errors.As(err, &signedErr); signedErr.Details.(map[string]any)[services.RefusalPublicKey] != nil {
		t.Fatalf("a signed record is marked public: %#v", signedErr.Details)
	}
	entry, raw := refusedEntry(t, r, id)
	if entry["state"] != services.HistoryStateRefused || entry["tool"] != "fetch.page" || entry["error"] != "fetch_captcha" ||
		entry["target"] != "http://site.test/challenge" || entry["cost"] != float64(0) || entry["at"] != float64(r.now) || entry["cost_note"] != "Free: no cost." {
		t.Fatalf("captcha record: %s", raw)
	}
	var public string
	if err = r.db.QueryRow("SELECT public FROM service_calls WHERE id=?", id).Scan(&public); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{raw, public} {
		if strings.Contains(s, "SECRET") || strings.Contains(s, "token") || strings.Contains(s, "frag") {
			t.Fatalf("the query string or fragment is on record: %s", s)
		}
	}
	// The same page again is served from the cache: a call, and a record,
	// of its own.
	_, err = r.fetch("bob", noScreen("http://site.test/challenge?token=SECRET-TOKEN#frag"))
	if again := refusedCallID(t, err); code(err) != "fetch_captcha" || again == "" || again == id || s.hit("/challenge") != 1 {
		t.Fatalf("cached refusal: %v %q", err, again)
	}

	_, err = r.fetch("alice", noScreen("http://site.test/private/x?q=1"))
	if code(err) != "fetch_robots" {
		t.Fatalf("robots: %v", err)
	}
	var ae *allowance.Err
	errors.As(err, &ae)
	if d, _ := ae.Details.(map[string]any); d["robots_line"] != "Disallow: /private" {
		t.Fatalf("robots details lost: %#v", ae.Details)
	}
	entry, raw = refusedEntry(t, r, refusedCallID(t, err))
	if entry["state"] != services.HistoryStateRefused || entry["error"] != "fetch_robots" || entry["target"] != "http://site.test/private/x" || entry["robots_line"] != "Disallow: /private" || entry["cost"] != float64(0) {
		t.Fatalf("robots record: %s", raw)
	}

	_, err = r.fetch("alice", noScreen("http://site.test/forbidden"))
	if entry, raw = refusedEntry(t, r, refusedCallID(t, err)); code(err) != "fetch_blocked" || entry["error"] != "fetch_blocked" || entry["target"] != "http://site.test/forbidden" {
		t.Fatalf("blocked record: %v %s", err, raw)
	}

	// An invalid URL is refused before any call is made; a private address
	// is a call that keeps no record.
	before := serviceCallCount(t, r)
	_, err = r.fetch("alice", noScreen("http://site.test:8080/"))
	if code(err) != "fetch_invalid_url" || refusedCallID(t, err) != "" || serviceCallCount(t, r) != before {
		t.Fatalf("invalid URL: %v", err)
	}
	_, err = r.fetch("alice", noScreen("http://intranet.test/"))
	if code(err) != "fetch_address_blocked" || refusedCallID(t, err) != "" {
		t.Fatalf("private address: %v", err)
	}
	var state, kept string
	if err = r.db.QueryRow("SELECT state,public FROM service_calls WHERE error='fetch_address_blocked'").Scan(&state, &kept); err != nil || state != "failed" || kept != "{}" {
		t.Fatalf("private address row: %s %s %v", state, kept, err)
	}

	// Nothing was charged for any of them.
	entries, _ := r.meter.Entries(context.Background(), r.db)
	for _, e := range entries {
		if e.State == "committed" && e.Units > 0 {
			t.Fatalf("a refused fetch was charged: %+v", e)
		}
	}
}

// C1157: an unsigned caller's refusals are on record too, under its network's
// pseudonym like its other calls, and each counts toward the method's
// per-network rate: past it the call is refused before it is made, with no
// call and no record.
func TestFetchRefusalCountsTowardTheRate(t *testing.T) {
	s := newSite(t, false, map[string]http.HandlerFunc{
		"/challenge": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><head><title>Just a moment...</title></head></html>`)
		},
	})
	r := newFetchRig(t, s, fetchOpts{})
	call := func() error {
		raw, _ := json.Marshal(map[string]any{"schema": 1, "method": "page", "args": noScreen("http://site.test/challenge"), "max_cost": 1 << 20})
		r.n++
		tx, _ := r.db.Begin()
		out, err := r.e.Call(context.Background(), tx, services.Request{Service: "fetch", Data: string(raw), Subject: allowance.Subject{ID: "anon:net1"}, RequestKey: fmt.Sprintf("id:anon-request-%08d", r.n)}, r.now)
		if err != nil {
			tx.Rollback()
			return err
		}
		tx.Commit()
		_, err = out.After()
		return err
	}
	var perMinute int64
	if _, m, ok := services.LookupMethod(services.Catalog([]string{"fetch"}), "fetch", "page"); ok && m.AnonymousRate != nil {
		perMinute = m.AnonymousRate.CallerPerMinute
	}
	if perMinute <= 0 {
		t.Fatal("fetch.page has no per-network rate")
	}
	for i := range perMinute {
		err := call()
		id := refusedCallID(t, err)
		if code(err) != "fetch_captcha" || id == "" {
			t.Fatalf("refusal %d: %v", i, err)
		}
		var ae *allowance.Err
		if errors.As(err, &ae); ae.Details.(map[string]any)[services.RefusalPublicKey] != true {
			t.Fatalf("refusal %d: an unsigned record is not marked public: %#v", i, ae.Details)
		}
		row, ok, herr := r.e.HistoryByID(context.Background(), r.db, id, r.now)
		if herr != nil || !ok || row.Account != "anon:net1" || row.Entry["state"] != services.HistoryStateRefused {
			t.Fatalf("anonymous record %d: %+v %v %v", i, row, ok, herr)
		}
	}
	before := serviceCallCount(t, r)
	if err := call(); code(err) != services.RefusalAnonymousRate || serviceCallCount(t, r) != before {
		t.Fatalf("past the rate: %v", err)
	}
}
