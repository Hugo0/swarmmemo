package services_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"swarmmemo/internal/services"
)

func fixtureSHA256(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(fixtures + name)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// C699: every fetched source record carries the SHA-256 of the exact bytes
// the upstream served, on a miss, a hit (from the cache, also after a
// restart) and a stale copy, so a robots-closed source can still be stamped.
func TestPublicDataSourceRawSHA256(t *testing.T) {
	v := newPDEnv(t, false)
	want := fixtureSHA256(t, "nsidc_extent.csv")
	t0 := unix("2026-07-12")
	item, _ := v.mustFetch(t, "sea_ice_extent", `{}`, t0)
	eq(t, "miss", at(item, "sources", 0, "cache"), "miss")
	eq(t, "miss raw_sha256", at(item, "sources", 0, "raw_sha256"), want)
	item, _ = v.mustFetch(t, "sea_ice_extent", `{"date":"2026-07-09"}`, t0+60)
	eq(t, "hit", at(item, "sources", 0, "cache"), "hit")
	eq(t, "hit raw_sha256", at(item, "sources", 0, "raw_sha256"), want)
	v.build(t)
	item, _ = v.mustFetch(t, "sea_ice_extent", `{}`, t0+120)
	eq(t, "hit after restart raw_sha256", at(item, "sources", 0, "raw_sha256"), want)
	v.fakes["noaadata.apps.nsidc.org"].set("500")
	item, _ = v.mustFetch(t, "sea_ice_extent", `{}`, t0+13*3600)
	eq(t, "stale", at(item, "sources", 0, "cache"), "stale")
	eq(t, "stale raw_sha256", at(item, "sources", 0, "raw_sha256"), want)
	// The cached body is not the raw bytes; only the hash travels with it.
	var body string
	if err := v.db.QueryRow("SELECT body FROM public_data_cache WHERE dataset='sea_ice_extent'").Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, want) {
		t.Errorf("cached row lacks raw_sha256")
	}
}

// A streamed source (FSIS) is hashed whole too.
func TestPublicDataStreamedSourceRawSHA256(t *testing.T) {
	v := newPDEnv(t, false)
	item, _ := v.mustFetch(t, "food_recalls", `{"firm":"Synear"}`, unix("2026-07-20"))
	want := fixtureSHA256(t, "fsis_recalls.json")
	found := false
	for _, s := range item["sources"].([]any) {
		m := s.(map[string]any)
		if strings.Contains(m["url"].(string), "fsis.usda.gov") {
			found = true
			eq(t, "fsis raw_sha256", m["raw_sha256"], want)
		}
	}
	if !found {
		t.Fatalf("no FSIS source: %v", item["sources"])
	}
}

// A cache row written before C699 (a bare body) still reads, without a hash.
func TestPublicDataLegacyCacheRow(t *testing.T) {
	v := newPDEnv(t, false)
	t0 := unix("2026-07-12")
	v.mustFetch(t, "sea_ice_extent", `{}`, t0)
	if _, err := v.db.Exec(`UPDATE public_data_cache SET body=json_extract(body,'$.pd_body') WHERE dataset='sea_ice_extent'`); err != nil {
		t.Fatal(err)
	}
	item, d := v.mustFetch(t, "sea_ice_extent", `{}`, t0+60)
	eq(t, "legacy hit", at(item, "sources", 0, "cache"), "hit")
	eq(t, "legacy data", d["date"], "2026-07-10")
	src := item["sources"].([]any)[0].(map[string]any)
	if _, ok := src["raw_sha256"]; ok {
		t.Errorf("a legacy row has no hash to give: %v", src)
	}
}

// The catalogue says which sources fetch cannot read and what to stamp.
func TestPublicDataSourceFetchable(t *testing.T) {
	v := newPDEnv(t, false)
	for _, e := range services.PublicDataDatasetsForTest(v.cfg)["datasets"].([]any) {
		m := e.(map[string]any)
		f, has := m["source_fetchable"]
		if m["id"] == "sea_ice_extent" {
			if f != false || !strings.Contains(m["source_note"].(string), "raw_sha256") {
				t.Errorf("sea_ice_extent: source_fetchable %v, note %v", f, m["source_note"])
			}
		} else if has {
			t.Errorf("%s: source_fetchable %v", m["id"], f)
		}
	}
}
