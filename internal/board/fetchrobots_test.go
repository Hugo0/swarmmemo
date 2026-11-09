package board

import (
	"errors"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// TestFetchRobotsMessage: fetch_robots names the matched robots.txt line
// and the public_data dataset for the host when the run recorded them,
// keeps the generic advice otherwise, and never carries details (strict
// clients refuse unknown keys) (C842).
func TestFetchRobotsMessage(t *testing.T) {
	refuse := func(details any) *Error {
		t.Helper()
		var be *Error
		if !errors.As(ServiceRefusal(&allowance.Err{Code: "fetch_robots", Details: details}), &be) || be.Status != 403 || be.Code != "fetch_robots" || be.Details != nil {
			t.Fatalf("got %#v", be)
		}
		if !strings.HasSuffix(be.Message, " Nothing was charged.") {
			t.Fatalf("message: %q", be.Message)
		}
		return be
	}
	be := refuse(map[string]any{"robots_line": "Disallow: /data/", "datasets": []string{"noaa_station_daily"}})
	for _, want := range []string{"its line `Disallow: /data/` covers this path", "public_data dataset noaa_station_daily reads this host: call /call/public_data/fetch?dataset=noaa_station_daily and stamp a sources[] raw_sha256 with notary instead."} {
		if !strings.Contains(be.Message, want) {
			t.Fatalf("missing %q in %q", want, be.Message)
		}
	}
	if strings.Contains(be.Message, "If it is a public_data source") {
		t.Fatalf("generic advice kept: %q", be.Message)
	}
	// Several datasets: the first is the example, at most three are named.
	be = refuse(map[string]any{"datasets": []string{"a", "b", "c", "d"}})
	if !strings.Contains(be.Message, "public_data datasets a, b, c read this host: call /call/public_data/fetch?dataset=a (or another of them)") || strings.Contains(be.Message, "its line") {
		t.Fatalf("several: %q", be.Message)
	}
	// An unknown host keeps the generic sentence.
	be = refuse(map[string]any{"robots_line": "Disallow: /"})
	if !strings.Contains(be.Message, "(its line `Disallow: /` covers this path)") || !strings.Contains(be.Message, "If it is a public_data source, stamp the raw_sha256 in that item's sources[] with notary instead.") {
		t.Fatalf("unknown host: %q", be.Message)
	}
	// Nothing recorded (a replay, an unreadable robots.txt): the old text.
	if be = refuse(nil); be.Message != "The site's robots.txt does not let SwarmMemoFetch read this page, so it was not requested. If it is a public_data source, stamp the raw_sha256 in that item's sources[] with notary instead. Nothing was charged." {
		t.Fatalf("plain: %q", be.Message)
	}
}
