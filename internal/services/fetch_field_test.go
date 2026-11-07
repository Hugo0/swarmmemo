package services_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
)

// field (C790): a JSON pointer selects one value of a JSON body, and
// field_sha256 stays the same while the rest of the body changes per call.
func TestFetchFieldSelectsOneValue(t *testing.T) {
	var calls atomic.Int64
	s := newSite(t, false, map[string]http.HandlerFunc{
		"/rate": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			n := calls.Add(1)
			fmt.Fprintf(w, `{"served":%d,"nonce":"n%d","data":{"rate":{"usd":2,"note":"a<b & c"},"items":[10,{"price":3}],"a/b":{"m~n":"esc"}}}`, n, n)
		},
		"/big": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"blob":%q,"ok":1}`, strings.Repeat("x", services.FetchFieldValueBytes))
		},
		"/page": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><p>not json</p></body></html>`)
		},
	})
	r := newFetchRig(t, s, fetchOpts{})
	with := func(url, field string) map[string]any {
		a := noScreen(url)
		a["field"] = field
		return a
	}
	res := func(url, field string) map[string]any {
		t.Helper()
		return get(r.must("alice", with(url, field)), "result").(map[string]any)
	}
	hashOf := func(v any) string {
		b, _ := json.Marshal(v)
		return sha(b)
	}
	// Nested object: two fetches with a different counter and nonce agree
	// on field_sha256, while raw_sha256 differs; the hash is of field_value
	// as delivered.
	a, b := res("http://site.test/rate?x=1", "/data/rate"), res("http://site.test/rate?x=2", "/data/rate")
	if a["raw_sha256"] == b["raw_sha256"] {
		t.Fatal("the bodies were meant to differ")
	}
	if a["field"] != "/data/rate" || a["field_sha256"] != b["field_sha256"] || a["field_sha256"] != hashOf(a["field_value"]) {
		t.Fatalf("field: %v %v %v", a["field"], a["field_sha256"], b["field_sha256"])
	}
	if a["field_sha256"] != sha([]byte(`{"note":"a\u003cb \u0026 c","usd":2}`)) {
		t.Fatalf("canonical value: %v", a["field_value"])
	}
	// Array index, and the ~1 ~0 escapes.
	if v := res("http://site.test/rate?x=1", "/data/items/1/price"); v["field_value"] != float64(3) || v["field_sha256"] != sha([]byte("3")) {
		t.Fatalf("array index: %v", v["field_value"])
	}
	if v := res("http://site.test/rate?x=1", "/data/a~1b/m~0n"); v["field_value"] != "esc" || v["field_sha256"] != sha([]byte(`"esc"`)) {
		t.Fatalf("escapes: %v", v["field_value"])
	}
	// Without field the answer has none of its keys, and the same keys as
	// before C790.
	plain := res("http://site.test/rate?x=1", "")
	keys := make([]string, 0, len(plain))
	for k := range plain {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"body_limit_bytes", "bytes", "cached", "content_type", "cost_note", "cost_split", "fetched_at", "final_url", "format", "max_cost_split", "note", "offset", "page_bytes", "raw_bytes", "raw_sha256", "screen", "screened", "status", "text", "text_bytes", "text_limit_bytes", "truncated", "truncated_by", "untrusted", "url"}
	if !slices.Equal(keys, want) {
		t.Fatalf("keys without field: %v", keys)
	}
	// field costs nothing extra: the same page's answer is priced the same.
	if fa, fp := fmt.Sprint(res("http://site.test/rate?x=1", "/data/rate")["cost_split"]), fmt.Sprint(plain["cost_split"]); fa != fp {
		t.Fatalf("field changed the price: %s vs %s", fa, fp)
	}
	// Refusals: a pointer that does not resolve, a body that is not JSON, a
	// value over the cap, a malformed pointer; none is charged.
	for _, c := range []struct{ url, field, code, detail string }{
		{"http://site.test/rate?x=1", "/data/missing", "fetch_field_not_found", "/data/missing"},
		{"http://site.test/rate?x=1", "/data/items/2/x", "fetch_field_not_found", "/data/items/2"},
		{"http://site.test/rate?x=1", "/data/items/01", "fetch_field_not_found", "/data/items/01"},
		{"http://site.test/rate?x=1", "/data/a~1b/m~0x", "fetch_field_not_found", "/data/a~1b/m~0x"},
		{"http://site.test/page", "/a", "fetch_field_not_json", "markdown"},
		{"http://site.test/big", "/blob", "fetch_field_too_large", ""},
	} {
		_, err := r.fetch("alice", with(c.url, c.field))
		var ae *allowance.Err
		if !errors.As(err, &ae) || ae.Code != c.code {
			t.Errorf("%s %s: %v, want %s", c.url, c.field, err, c.code)
			continue
		}
		d, _ := ae.Details.(map[string]any)
		switch c.code {
		case "fetch_field_not_found":
			if d["missing_at"] != c.detail || d["field"] != c.field {
				t.Errorf("%s: details %v", c.field, d)
			}
		case "fetch_field_not_json":
			if d["format"] != c.detail {
				t.Errorf("not json: details %v", d)
			}
		case "fetch_field_too_large":
			if n, _ := d["value_bytes"].(int); n <= services.FetchFieldValueBytes || d["limit_bytes"] != services.FetchFieldValueBytes {
				t.Errorf("too large: details %v", d)
			}
		}
	}
	// A malformed pointer is refused before anything is read.
	for _, f := range []string{"data", "/a~2", "/a~", strings.Repeat("/a", services.FetchFieldBytes)} {
		if _, err := r.fetch("alice", with("http://site.test/rate?x=1", f)); code(err) != "invalid_service_data" {
			t.Errorf("pointer %q: %v", f, err)
		}
	}
	entries, _ := r.meter.Entries(context.Background(), r.db)
	charged := 0
	for _, e := range entries {
		if e.State == "committed" && e.Units > 0 {
			charged++
		}
	}
	if charged != 6 { // the six answers above, each priced as a plain fetch
		t.Fatalf("committed charges: %d, want 6", charged)
	}
	if v := res("http://site.test/big", "/ok"); v["field_value"] != float64(1) {
		t.Fatalf("a small value of a large body: %v", v["field_value"])
	}
}
