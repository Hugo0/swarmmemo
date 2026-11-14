package services

import (
	"encoding/json"
	"reflect"
	"testing"
)

// An integer parameter takes its digits quoted too, as a /call/ query field
// gives them; anything else quoted stays refused.
func TestPublicDataIntParamFromQueryString(t *testing.T) {
	spec := pdParam{Name: "limit", Kind: "int", Min: 1, Max: 100}
	for raw, want := range map[string]bool{`7`: true, `"7"`: true, `"100"`: true, `"101"`: false, `"0"`: false, `"7.5"`: false, `"1e2"`: false, `" 7"`: false, `""`: false, `"x"`: false} {
		if _, ok := spec.parse(json.RawMessage(raw)); ok != want {
			t.Errorf("parse(%s) = %v, want %v", raw, ok, want)
		}
	}
}

// The peak gust comes from WSFG, else WSF5; WSF2 (fastest 2-minute wind) is
// a sustained wind and only fills max_2min_wind_* (C961, key:23335423).
func TestNOAAGustElements(t *testing.T) {
	v, err := parseNOAA([]byte(`[
		{"DATE": "2026-01-01", "WSFG": "30.0", "WSF5": "28.0", "WSF2": "20.0"},
		{"DATE": "2026-01-02", "WSF5": "25.0", "WSF2": "18.0"},
		{"DATE": "2026-01-03", "WSF2": "15.0"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	days := v.([]noaaDay)
	if len(days) != 3 {
		t.Fatalf("%d days", len(days))
	}
	str := func(p *string) any {
		if p == nil {
			return nil
		}
		return *p
	}
	num := func(p *float64) any {
		if p == nil {
			return nil
		}
		return *p
	}
	for _, c := range []struct {
		day          noaaDay
		element      any
		gustMS, wind any
	}{
		{days[2], "WSFG", 30.0, 20.0}, // WSFG present: the peak gust
		{days[1], "WSF5", 25.0, 18.0}, // no WSFG: the fastest 5-second wind
		{days[0], nil, nil, 15.0},     // WSF2 only: no gust, a 2-minute wind
	} {
		date := str(c.day.Date)
		got := []any{str(c.day.PeakGustElement), num(c.day.PeakGustMS), num(c.day.Max2MinWindMS)}
		if want := []any{c.element, c.gustMS, c.wind}; !reflect.DeepEqual(got, want) {
			t.Errorf("%v: element, gust ms, 2min ms = %v, want %v", date, got, want)
		}
		if (c.gustMS == nil) != (c.day.PeakGustMPH == nil) {
			t.Errorf("%v: gust mph %v disagrees with gust ms %v", date, num(c.day.PeakGustMPH), c.gustMS)
		}
		if c.day.Max2MinWindMPH == nil {
			t.Errorf("%v: no max_2min_wind_mph", date)
		}
	}
}
