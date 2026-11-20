package board

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// TestFetchRobotsMessage: fetch_robots names the matched robots.txt line
// and the public_data dataset for the site when the run recorded them,
// keeps the generic advice otherwise; the robots line never reaches the
// details, only details.public_data does (C842, C1147).
func TestFetchRobotsMessage(t *testing.T) {
	refuse := func(details any) *Error {
		t.Helper()
		var be *Error
		if !errors.As(ServiceRefusal(&allowance.Err{Code: "fetch_robots", Details: details}), &be) || be.Status != 403 || be.Code != "fetch_robots" {
			t.Fatalf("got %#v", be)
		}
		if !strings.HasSuffix(be.Message, " Nothing was charged.") {
			t.Fatalf("message: %q", be.Message)
		}
		return be
	}
	const call = `public_data.fetch {"dataset":"noaa_station_daily","params":{"station":"USW00014755"}}`
	hint := map[string]any{"dataset": "noaa_station_daily", "call": call}
	be := refuse(map[string]any{"robots_line": "Disallow: /data/", "public_data": hint})
	for _, want := range []string{"its line `Disallow: /data/` covers this path", "public_data reads this site's data as dataset noaa_station_daily: call " + call + " instead (details.public_data)"} {
		if !strings.Contains(be.Message, want) {
			t.Fatalf("missing %q in %q", want, be.Message)
		}
	}
	if strings.Contains(be.Message, "If it is a public_data source") {
		t.Fatalf("generic advice kept: %q", be.Message)
	}
	if !reflect.DeepEqual(be.Details, map[string]any{"public_data": hint}) {
		t.Fatalf("details: %#v", be.Details)
	}
	// An unknown host keeps the generic sentence and carries no details.
	be = refuse(map[string]any{"robots_line": "Disallow: /"})
	if !strings.Contains(be.Message, "(its line `Disallow: /` covers this path)") || !strings.Contains(be.Message, "If it is a public_data source, stamp the raw_sha256 in that item's sources[] with notary instead.") || be.Details != nil {
		t.Fatalf("unknown host: %q %#v", be.Message, be.Details)
	}
	// Nothing recorded (a replay, an unreadable robots.txt): the old text.
	if be = refuse(nil); be.Message != "The site's robots.txt does not let SwarmMemoFetch read this page, so it was not requested. If it is a public_data source, stamp the raw_sha256 in that item's sources[] with notary instead. Nothing was charged." || be.Details != nil {
		t.Fatalf("plain: %q", be.Message)
	}
}

// TestFetchSiteRefusalNamesPublicData: fetch_blocked, fetch_captcha and
// fetch_site_rate_limited on a site public_data reads name the dataset and
// its call in the message and as details.public_data; without one they
// keep their text and carry no details (C1147).
func TestFetchSiteRefusalNamesPublicData(t *testing.T) {
	const call = `public_data.fetch {"dataset":"crypto_spot_price","params":{"coin_id":"bitcoin"}}`
	hint := map[string]any{"dataset": "crypto_spot_price", "call": call}
	for code, status := range map[string]int{"fetch_blocked": 403, "fetch_captcha": 403, "fetch_site_rate_limited": 429} {
		var plain, named *Error
		if !errors.As(ServiceRefusal(&allowance.Err{Code: code}), &plain) || plain.Status != status || plain.Details != nil {
			t.Fatalf("%s plain: %#v", code, plain)
		}
		if !errors.As(ServiceRefusal(&allowance.Err{Code: code, Details: map[string]any{"public_data": hint}}), &named) || named.Status != status || named.Code != code {
			t.Fatalf("%s named: %#v", code, named)
		}
		if !strings.HasPrefix(named.Message, plain.Message+" ") || !strings.Contains(named.Message, "dataset crypto_spot_price: call "+call+" instead") {
			t.Fatalf("%s message: %q", code, named.Message)
		}
		if !reflect.DeepEqual(named.Details, map[string]any{"public_data": hint}) || named.RetryAfter != plain.RetryAfter {
			t.Fatalf("%s details: %#v", code, named.Details)
		}
		if strings.Contains(plain.Message, "public_data") {
			t.Fatalf("%s plain names public_data: %q", code, plain.Message)
		}
	}
}
