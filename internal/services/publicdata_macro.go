package services

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Group 2: FRED series and release calendar, central-bank policy rates and
// decision dates, and the Federal Reserve GDP and inflation nowcasts.

const (
	fredHost = "api.stlouisfed.org"
	fredObs  = "/fred/series/observations"
)

// fredSeries is the FRED allowlist: series published by US government
// agencies (public domain), plus the Atlanta Fed's GDPNow. FRED also carries
// third-party series under copyright, which are not served. units is the
// default transform (Pythia's curated choice where it had one).
var fredSeries = map[string]struct{ units, desc string }{
	"GDPC1":    {"pca", "Real GDP, % change SAAR (annualized quarterly growth)"},
	"GDP":      {"lin", "Nominal GDP, billions of dollars SAAR"},
	"CPIAUCSL": {"lin", "CPI-U, all items, index (SA)"},
	"CPILFESL": {"lin", "CPI-U less food and energy, index (SA)"},
	"PCEPI":    {"lin", "PCE price index (SA)"},
	"PCEPILFE": {"lin", "PCE price index less food and energy (SA)"},
	"UNRATE":   {"lin", "Unemployment rate, % (SA)"},
	"PAYEMS":   {"lin", "All employees, total nonfarm, thousands (SA)"},
	"ICSA":     {"lin", "Initial jobless claims (SA)"},
	"INDPRO":   {"lin", "Industrial production index (SA)"},
	"RSAFS":    {"lin", "Advance retail and food services sales, millions of dollars (SA)"},
	"HOUST":    {"lin", "Housing starts, thousands of units SAAR"},
	"FEDFUNDS": {"lin", "Effective federal funds rate, %"},
	"DFF":      {"lin", "Effective federal funds rate, daily, %"},
	"DFEDTARU": {"lin", "Federal funds target range, upper limit, %"},
	"DFEDTARL": {"lin", "Federal funds target range, lower limit, %"},
	"DTB3":     {"lin", "3-month Treasury bill secondary market rate, %"},
	"DGS2":     {"lin", "2-year Treasury constant-maturity yield, %"},
	"DGS10":    {"lin", "10-year Treasury constant-maturity yield, %"},
	"DGS30":    {"lin", "30-year Treasury constant-maturity yield, %"},
	"T10Y2Y":   {"lin", "10-year minus 2-year Treasury spread, percentage points"},
	"T10YIE":   {"lin", "10-year breakeven inflation rate, %"},
	"M2SL":     {"lin", "M2 money stock, billions of dollars (SA)"},
	"WALCL":    {"lin", "Federal Reserve total assets, millions of dollars"},
	"GDPNOW":   {"lin", "Atlanta Fed GDPNow nowcast, % SAAR"},
}

var fredUnits = []string{"lin", "chg", "ch1", "pch", "pc1", "pca", "cch", "cca", "log"}

func fredSeriesIDs() string {
	ids := make([]string, 0, len(fredSeries))
	for id := range fredSeries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, ", ")
}

// parseFREDObs reads series/observations into a series; FRED's "." (and
// empty) values are missing and skipped.
func parseFREDObs(raw []byte) (any, error) {
	var body struct {
		Observations []map[string]any `json:"observations"`
	}
	if err := decodeJSON(raw, &body); err != nil {
		return nil, err
	}
	points := map[string]float64{}
	for _, o := range body.Observations {
		d, ok := o["date"].(string)
		if !ok {
			continue
		}
		if _, err := time.Parse("2006-01-02", d); err != nil {
			continue
		}
		if v := pyFloat(o["value"]); v != nil {
			points[d] = *v
		}
	}
	return seriesFromPoints(points), nil
}

type fredData struct {
	Source       string  `json:"source"`
	Series       string  `json:"series"`
	Units        string  `json:"units"`
	Description  string  `json:"description"`
	Count        int     `json:"count"`
	Latest       *pdObs  `json:"latest"`
	Observations []pdObs `json:"observations"`
	Truncated    bool    `json:"truncated"`
	NextEndDate  *string `json:"next_end_date"`
}

var dsFREDSeries = &pdDataset{
	ID: "fred_series", SchemaVersion: 1,
	Title:       "FRED economic series",
	Description: "An economic time series from FRED (Federal Reserve Bank of St. Louis), from an allowlist of public-domain US series: " + fredSeriesIDs() + ".",
	Output:      "data: source, series, units, description, count (observations in range), latest {date, value}, observations[] (newest first: date, value), truncated, next_end_date",
	Params: []pdParam{
		{Name: "series_id", Kind: "string", Required: true, MaxLen: 16, Norm: pdUpper, Pattern: regexp.MustCompile(`^[A-Z0-9]{1,16}$`), Doc: "one of the allowlisted series"},
		{Name: "units", Kind: "enum", Enum: fredUnits, Norm: pdLower, Doc: "FRED transform; default the series' own (GDPC1: pca, else lin)"},
		dateParam("start_date", "from this date (inclusive)"),
		dateParam("end_date", "to this date (inclusive)"),
		limitParam(16, 1200, true),
	},
	Validate: func(p *pdParams, _ time.Time) bool {
		s, ok := fredSeries[p.Str("series_id")]
		if !ok {
			return false
		}
		if !p.Has("units") {
			p.Set("units", s.units)
		}
		return pdDateWindow(p, 0)
	},
	Run: func(ctx context.Context, r *pdRun, p *pdParams) (any, error) {
		id, units := p.Str("series_id"), p.Str("units")
		raw, err := r.fetch(ctx, pdFetch{Key: id + ":" + units, TTL: 6 * time.Hour, Host: fredHost, Path: fredObs,
			Query: url.Values{"series_id": {id}, "file_type": {"json"}, "units": {units}}, KeyParam: "api_key", MaxBytes: 16 << 20,
			Timeout: 30 * time.Second, Parse: parseFREDObs})
		if err != nil {
			return nil, err
		}
		s, err := unmarshalCached[pdSeries](raw)
		if err != nil {
			return nil, err
		}
		out := fredData{Source: "FRED", Series: id, Units: units, Description: fredSeries[id].desc}
		out.Observations, out.Count, out.NextEndDate = s.window(p.Str("start_date"), p.Str("end_date"), int(intOr(p, "limit", 16)))
		out.Truncated = out.NextEndDate != nil
		if len(out.Observations) > 0 {
			out.Latest = &out.Observations[0]
		}
		if len(s.D) > 0 {
			r.asOf = s.D[len(s.D)-1]
		}
		return out, nil
	},
	Source: "Federal Reserve Bank of St. Louis, FRED", Hosts: []string{fredHost},
	Licence:     "FRED API terms of use; the allowlisted series are US government data (public domain) except GDPNOW (Atlanta Fed)",
	Attribution: "Source: FRED, Federal Reserve Bank of St. Louis", TermsURL: "https://fred.stlouisfed.org/docs/api/terms_of_use.html",
	TermsStatus: "verify before production (FRED API terms; GDPNOW redistribution)",
	Key:         KeyFRED, TTL: 6 * time.Hour, Price: 1,
}

// --- FRED release calendar and the FOMC calendar ----------------------------

// fomcMeetings are the FOMC meetings (start, decision day), from the
// Federal Reserve's published calendar (federalreserve.gov, 2026 and 2027).
var fomcMeetings = [][2]string{
	{"2026-01-27", "2026-01-28"}, {"2026-03-17", "2026-03-18"}, {"2026-04-28", "2026-04-29"}, {"2026-06-16", "2026-06-17"},
	{"2026-07-28", "2026-07-29"}, {"2026-09-15", "2026-09-16"}, {"2026-10-27", "2026-10-28"}, {"2026-12-08", "2026-12-09"},
	{"2027-01-26", "2027-01-27"}, {"2027-03-16", "2027-03-17"}, {"2027-04-27", "2027-04-28"}, {"2027-06-08", "2027-06-09"},
	{"2027-07-27", "2027-07-28"}, {"2027-09-14", "2027-09-15"}, {"2027-10-26", "2027-10-27"}, {"2027-12-07", "2027-12-08"},
}

const fomcCalendarURL = "https://www.federalreserve.gov/monetarypolicy/fomccalendars.htm"

var (
	fredHeadline = []struct {
		name string
		id   int64
	}{{"Gross Domestic Product", 53}, {"Consumer Price Index", 10}, {"Employment Situation", 50}}
	fomcKeywords = []string{"fomc", "federal open market committee", "fed meeting", "fed rate", "rate decision", "federal reserve", "interest rate"}
)

type releaseRow struct {
	Release      string   `json:"release"`
	ReleaseID    *int64   `json:"release_id"`
	NextDates    []string `json:"next_dates"`
	NextMeetings []string `json:"next_meetings,omitempty"`
	LastDate     *string  `json:"last_date"`
	Source       string   `json:"source,omitempty"`
}

type releasesData struct {
	Today          string       `json:"today"`
	ReleaseMatched []releaseRow `json:"release_matched"`
	Note           string       `json:"note,omitempty"`
}

type fredRelease struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

var dsFREDReleases = &pdDataset{
	ID: "fred_release_calendar", SchemaVersion: 1,
	Title:       "US release calendar",
	Description: "Upcoming release dates of US macro prints from the FRED release calendar (default: GDP, CPI, the Employment Situation), or the FOMC meeting schedule for a Fed query (no key needed for that).",
	Output:      "data: today, release_matched[] (release, release_id, next_dates (up to 4, ascending), last_date; FOMC also next_meetings and source)",
	Params: []pdParam{
		{Name: "release", Kind: "string", MaxLen: 100, Pattern: pdWordRE, Norm: pdTrim, Doc: "release-name substring, e.g. GDP, Consumer Price, Employment; or a Fed query such as FOMC"},
		{Name: "days_ahead", Kind: "int", Min: 0, Max: 3650, Doc: "only upcoming dates within this many days"},
	},
	Run: func(ctx context.Context, r *pdRun, p *pdParams) (any, error) {
		today := r.today().Format("2006-01-02")
		cutoff := ""
		if n, ok := p.Int("days_ahead"); ok {
			cutoff = r.today().AddDate(0, 0, int(n)).Format("2006-01-02")
		}
		out := releasesData{Today: today, ReleaseMatched: []releaseRow{}}
		release := p.Str("release")
		q := strings.ToLower(release)
		for _, kw := range fomcKeywords {
			if strings.Contains(q, kw) {
				r.static(fomcCalendarURL)
				out.ReleaseMatched = append(out.ReleaseMatched, fomcSchedule(today, cutoff))
				return out, nil
			}
		}
		if r.key == "" {
			return nil, pdFail("upstream_unavailable", "key_missing")
		}
		targets := fredHeadline
		if release != "" {
			raw, err := r.fetch(ctx, pdFetch{Key: "releases", TTL: 24 * time.Hour, Host: fredHost, Path: "/fred/releases",
				Query: url.Values{"file_type": {"json"}, "limit": {"1000"}}, KeyParam: "api_key", MaxBytes: 2 << 20,
				Parse: func(raw []byte) (any, error) {
					var body struct {
						Releases []map[string]any `json:"releases"`
					}
					if err := decodeJSON(raw, &body); err != nil {
						return nil, err
					}
					out := []fredRelease{}
					for _, rel := range body.Releases {
						id := pyFloat(rel["id"])
						name, ok := rel["name"].(string)
						if id == nil || !ok || *id < 1 || *id > 1e9 {
							continue
						}
						out = append(out, fredRelease{ID: int64(*id), Name: name})
					}
					return out, nil
				}})
			if err != nil {
				return nil, err
			}
			all, err := unmarshalCached[[]fredRelease](raw)
			if err != nil {
				return nil, err
			}
			targets = nil
			for _, rel := range all {
				if strings.Contains(strings.ToLower(rel.Name), q) && len(targets) < 8 {
					targets = append(targets, struct {
						name string
						id   int64
					}{rel.Name, rel.ID})
				}
			}
			if len(targets) == 0 {
				out.Note = "no FRED release name matched " + strconv.Quote(release)
				return out, nil
			}
		}
		for _, t := range targets {
			id := strconv.FormatInt(t.id, 10)
			raw, err := r.fetch(ctx, pdFetch{Key: "dates:" + id, TTL: 12 * time.Hour, Host: fredHost, Path: "/fred/release/dates",
				Query:    url.Values{"release_id": {id}, "file_type": {"json"}, "include_release_dates_with_no_data": {"true"}, "sort_order": {"desc"}, "limit": {"40"}},
				KeyParam: "api_key", MaxBytes: 1 << 20,
				Parse: func(raw []byte) (any, error) {
					var body struct {
						Dates []map[string]any `json:"release_dates"`
					}
					if err := decodeJSON(raw, &body); err != nil {
						return nil, err
					}
					out := []string{}
					for _, d := range body.Dates {
						if s, ok := d["date"].(string); ok && s != "" {
							out = append(out, s)
						}
					}
					sort.Strings(out)
					return out, nil
				}})
			if err != nil {
				return nil, err
			}
			dates, err := unmarshalCached[[]string](raw)
			if err != nil {
				return nil, err
			}
			row := releaseRow{Release: t.name, ReleaseID: &t.id, NextDates: []string{}}
			for _, d := range dates {
				switch {
				case d < today:
					row.LastDate = sptr(d)
				case (cutoff == "" || d <= cutoff) && len(row.NextDates) < 4:
					row.NextDates = append(row.NextDates, d)
				}
			}
			out.ReleaseMatched = append(out.ReleaseMatched, row)
		}
		return out, nil
	},
	Source: "Federal Reserve Bank of St. Louis (FRED release calendar); Federal Reserve Board (FOMC calendar)", Hosts: []string{fredHost},
	Licence: "FRED API terms of use; the FOMC calendar is a US government work", Attribution: "Source: FRED, Federal Reserve Bank of St. Louis; federalreserve.gov",
	TermsURL: "https://fred.stlouisfed.org/docs/api/terms_of_use.html", TermsStatus: "verify before production (FRED API terms)",
	Key: KeyFRED, KeyOptional: true, TTL: 12 * time.Hour, Price: 1,
}

func fomcSchedule(today, cutoff string) releaseRow {
	row := releaseRow{Release: "FOMC meeting (Federal Reserve monetary policy / rate decision)", NextDates: []string{}, NextMeetings: []string{},
		Source: "Federal Reserve FOMC calendar (federalreserve.gov)"}
	for _, m := range fomcMeetings {
		switch {
		case m[1] < today:
			row.LastDate = sptr(m[1])
		case (cutoff == "" || m[1] <= cutoff) && len(row.NextDates) < 4:
			row.NextDates = append(row.NextDates, m[1])
			row.NextMeetings = append(row.NextMeetings, m[0]+" to "+m[1])
		}
	}
	return row
}

// --- Central-bank policy rates and decision dates ---------------------------

const (
	bisHost             = "stats.bis.org"
	bcbHost             = "api.bcb.gov.br"
	cbHistoryObs        = 260
	cbMaxUpcoming       = 4
	cbCalendarValidThru = "2027-12-31"
	cbEffectiveLagDays  = 10
	bisRateSource       = "BIS daily central bank policy rates (stats.bis.org, WS_CBPOL)"
)

type cbBank struct {
	Code, Name, Area, RateName string
	Decisions                  []string
	CalendarSource             string
	CalendarNote               string
	// ScheduleUnknown, when set, says why no future decision date is
	// listed; the API then answers schedule_status "unknown" with this
	// reason instead of silently returning no next_decision.
	ScheduleUnknown string
}

// cbBanks are the covered banks, with decision dates (announcement day)
// from each bank's published calendar.
var cbBanks = func() []cbBank {
	fed := make([]string, 0, len(fomcMeetings))
	for _, m := range fomcMeetings {
		fed = append(fed, m[1])
	}
	return []cbBank{
		{Code: "FED", Name: "US Federal Reserve (FOMC)", Area: "US", RateName: "federal funds target range (BIS publishes the midpoint)", Decisions: fed,
			CalendarSource: "Federal Reserve FOMC calendar, www.federalreserve.gov/monetarypolicy/fomccalendars.htm"},
		{Code: "ECB", Name: "European Central Bank (euro area)", Area: "XM", RateName: "deposit facility rate (ECB steering rate since Sep 2024)",
			Decisions:      []string{"2026-09-10", "2026-10-29", "2026-12-17", "2027-02-04", "2027-03-18", "2027-04-29", "2027-06-10", "2027-07-22", "2027-09-09", "2027-10-28", "2027-12-16"},
			CalendarSource: "ECB Governing Council calendar, www.ecb.europa.eu/press/calendars/mgcgc/html/index.en.html"},
		{Code: "BOJ", Name: "Bank of Japan", Area: "JP", RateName: "uncollateralized overnight call rate (target)",
			Decisions:      []string{"2026-09-18", "2026-10-30", "2026-12-18", "2027-01-22", "2027-03-18", "2027-04-28", "2027-06-11", "2027-07-22", "2027-09-22", "2027-10-29", "2027-12-17"},
			CalendarSource: "BOJ MPM schedule, www.boj.or.jp/en/mopo/mpmsche_minu/index.htm"},
		{Code: "BOE", Name: "Bank of England", Area: "GB", RateName: "Bank Rate",
			Decisions:      []string{"2026-09-17", "2026-11-05", "2026-12-17", "2027-02-04", "2027-03-18", "2027-04-29", "2027-06-17", "2027-07-29", "2027-09-16", "2027-11-04", "2027-12-16"},
			CalendarSource: "BoE MPC dates for 2026 and 2027 (both confirmed), bankofengland.co.uk/monetary-policy/upcoming-mpc-dates"},
		{Code: "BCB", Name: "Banco Central do Brasil (Copom / Selic)", Area: "BR", RateName: "Selic target rate (Meta Selic definida pelo Copom), % a.a.",
			Decisions: []string{"2026-09-16", "2026-11-04", "2026-12-09", "2027-01-27", "2027-03-17", "2027-04-28", "2027-06-16", "2027-08-04", "2027-09-22",
				"2027-10-27", "2027-12-08"},
			CalendarSource: "Copom calendar (BCB Comunicados 43.383 and 45.452), www.bcb.gov.br/en/monetarypolicy/copomdates"},
		{Code: "SNB", Name: "Swiss National Bank", Area: "CH", RateName: "SNB policy rate",
			Decisions:      []string{"2026-09-24", "2026-12-10", "2027-03-18", "2027-06-24", "2027-09-23", "2027-12-16"},
			CalendarSource: "SNB quarterly monetary policy assessment schedule, www.snb.ch/en/services-events/digital-services/event-schedule"},
		{Code: "RBA", Name: "Reserve Bank of Australia", Area: "AU", RateName: "cash rate target",
			Decisions: []string{"2026-08-11", "2026-09-29", "2026-11-03", "2026-12-08", "2027-02-09", "2027-03-23", "2027-05-04", "2027-06-22", "2027-08-10",
				"2027-09-28", "2027-11-02", "2027-12-14"},
			CalendarSource: "RBA Monetary Policy Board meeting dates 2026 and 2027, www.rba.gov.au/schedules-events/board-meeting-schedules.html"},
		{Code: "BOC", Name: "Bank of Canada", Area: "CA", RateName: "target for the overnight rate",
			Decisions: []string{"2026-09-02", "2026-10-28", "2026-12-09", "2027-01-27", "2027-03-03", "2027-04-28", "2027-06-02", "2027-07-21", "2027-09-08",
				"2027-10-27", "2027-12-08"},
			CalendarSource: "Bank of Canada interest-rate announcement schedule (2027 published July 2026), www.bankofcanada.ca"},
		{Code: "BOI", Name: "Bank of Israel", Area: "IL", RateName: "Bank of Israel interest rate",
			Decisions: []string{"2026-09-01"}, CalendarSource: "Bank of Israel monetary committee schedule, www.boi.org.il",
			ScheduleUnknown: "the Bank of Israel's forward decision schedule could not be verified from its own site, so no future date is listed; " +
				"the next decision date is unknown here: check www.boi.org.il (each decision's press release names the next one)"},
	}
}()

var cbAliases = map[string]string{
	"fed": "FED", "fomc": "FED", "federal reserve": "FED", "the fed": "FED", "us fed": "FED", "powell": "FED", "united states": "FED",
	"usa": "FED", "us": "FED", "federal open market committee": "FED", "fed funds": "FED",
	"ecb": "ECB", "euro": "ECB", "eurozone": "ECB", "euro area": "ECB", "european central bank": "ECB", "europe": "ECB", "lagarde": "ECB",
	"boj": "BOJ", "japan": "BOJ", "bank of japan": "BOJ", "ueda": "BOJ",
	"boe": "BOE", "england": "BOE", "bank of england": "BOE", "uk": "BOE", "united kingdom": "BOE", "britain": "BOE", "mpc": "BOE",
	"bcb": "BCB", "brazil": "BCB", "brasil": "BCB", "copom": "BCB", "selic": "BCB", "banco central do brasil": "BCB", "central bank of brazil": "BCB",
	"snb": "SNB", "swiss": "SNB", "switzerland": "SNB", "swiss national bank": "SNB",
	"rba": "RBA", "australia": "RBA", "reserve bank of australia": "RBA",
	"boc": "BOC", "canada": "BOC", "bank of canada": "BOC", "boficanada": "BOC",
	"bank of israel": "BOI", "boi": "BOI", "israel": "BOI",
}

var cbAllKeywords = []string{"all", "any", "every", "all banks", "everyone", "*"}

// resolveBank is Pythia's bank resolution: a code, an alias, then the
// longest alias (over three letters) found inside the text; "ALL" for a
// sweep.
func resolveBank(raw string) (string, bool) {
	q := strings.ToLower(strings.TrimSpace(raw))
	if q == "" {
		return "", false
	}
	if contains(cbAllKeywords, q) {
		return "ALL", true
	}
	for _, b := range cbBanks {
		if strings.ToUpper(q) == b.Code {
			return b.Code, true
		}
	}
	if c, ok := cbAliases[q]; ok {
		return c, true
	}
	aliases := make([]string, 0, len(cbAliases))
	for a := range cbAliases {
		aliases = append(aliases, a)
	}
	sort.SliceStable(aliases, func(i, j int) bool {
		if len(aliases[i]) != len(aliases[j]) {
			return len(aliases[i]) > len(aliases[j])
		}
		return aliases[i] < aliases[j]
	})
	for _, a := range aliases {
		if len(a) > 3 && strings.Contains(q, a) {
			return cbAliases[a], true
		}
	}
	return "", false
}

func resolveWhat(raw string) (string, bool) {
	q := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(raw)), "-", "_")
	m := map[string]string{
		"rate": "rate", "rates": "rate", "policy rate": "rate", "current": "rate", "current rate": "rate", "level": "rate",
		"next_meeting": "next_meeting", "next meeting": "next_meeting", "meeting": "next_meeting", "meetings": "next_meeting",
		"calendar": "next_meeting", "date": "next_meeting", "dates": "next_meeting", "next_decision": "next_meeting",
		"next decision": "next_meeting", "schedule": "next_meeting", "both": "both", "all": "both", "": "both",
	}
	if v, ok := m[q]; ok {
		return v, true
	}
	v, ok := m[strings.ReplaceAll(q, "_", " ")]
	return v, ok
}

// parseBISCSV reads a WS_CBPOL dataonly CSV into (date, value) pairs, oldest
// first; blank and non-finite values (BIS embeds NaN) are dropped.
func parseBISCSV(raw []byte) (any, error) {
	rd := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))))
	rd.FieldsPerRecord, rd.LazyQuotes = -1, true
	head, err := rd.Read()
	if err != nil {
		return nil, err
	}
	iv, it := -1, -1
	for i, h := range head {
		switch h {
		case "OBS_VALUE":
			iv = i
		case "TIME_PERIOD":
			it = i
		}
	}
	if iv < 0 || it < 0 {
		return nil, errors.New("no OBS_VALUE or TIME_PERIOD column")
	}
	type pt struct {
		d string
		v float64
	}
	var pts []pt
	for {
		row, err := rd.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if iv >= len(row) || it >= len(row) || row[it] == "" || row[iv] == "" {
			continue
		}
		if v := pyFloat(row[iv]); v != nil {
			pts = append(pts, pt{row[it], *v})
		}
	}
	sort.SliceStable(pts, func(i, j int) bool {
		if pts[i].d != pts[j].d {
			return pts[i].d < pts[j].d
		}
		return pts[i].v < pts[j].v
	})
	s := pdSeries{D: []string{}, V: []float64{}}
	for _, p := range pts {
		s.D, s.V = append(s.D, p.d), append(s.V, p.v)
	}
	return s, nil
}

type bcbSelic struct {
	Rate          *float64 `json:"rate"`
	EffectiveFrom string   `json:"effective_from"`
	ValidThrough  string   `json:"valid_through"`
}

func parseBCB(raw []byte) (any, error) {
	var rows []any
	if err := decodeJSON(raw, &rows); err != nil {
		return nil, err
	}
	points := map[string]float64{}
	var order []string
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			continue
		}
		when, _ := m["data"].(string)
		v := pyFloat(m["valor"])
		parts := strings.Split(when, "/")
		if v == nil || len(parts) != 3 {
			continue
		}
		d := parts[2] + "-" + parts[1] + "-" + parts[0]
		if _, ok := points[d]; !ok {
			order = append(order, d)
		}
		points[d] = *v
	}
	out := bcbSelic{}
	if len(points) == 0 {
		return out, nil
	}
	s := seriesFromPoints(points)
	n := len(s.D)
	out.Rate, out.ValidThrough = fptr(s.V[n-1]), s.D[n-1]
	out.EffectiveFrom = s.D[n-1]
	for i := n - 1; i >= 0 && s.V[i] == s.V[n-1]; i-- {
		out.EffectiveFrom = s.D[i]
	}
	return out, nil
}

type fredLatest struct {
	Value *float64 `json:"value"`
	Date  *string  `json:"date"`
}

func parseFREDLatest(raw []byte) (any, error) {
	var body struct {
		Observations []map[string]any `json:"observations"`
	}
	if err := decodeJSON(raw, &body); err != nil {
		return nil, err
	}
	out := fredLatest{}
	for _, o := range body.Observations {
		if v := pyFloat(o["value"]); v != nil {
			out.Value = v
			if d, ok := o["date"].(string); ok {
				out.Date = &d
			}
		}
	}
	return out, nil
}

// cbRow is one bank's answer; field names follow Pythia's lookup_cb_policy.
type cbRow struct {
	Bank                  string    `json:"bank"`
	PolicyRatePct         *float64  `json:"policy_rate_pct,omitempty"`
	RateAsOf              *string   `json:"rate_as_of,omitempty"`
	RateName              string    `json:"rate_name,omitempty"`
	RateSource            string    `json:"rate_source,omitempty"`
	RateEffectiveFrom     *string   `json:"rate_effective_from,omitempty"`
	PreviousRatePct       *float64  `json:"previous_rate_pct,omitempty"`
	LastChangeBps         *float64  `json:"last_change_bps,omitempty"`
	LastChangeNote        string    `json:"last_change_note,omitempty"`
	RateNote              string    `json:"rate_note,omitempty"`
	RateAvailable         *bool     `json:"rate_available,omitempty"`
	TargetRangePct        []float64 `json:"target_range_pct,omitempty"`
	TargetRangeAsOf       *string   `json:"target_range_as_of,omitempty"`
	TargetRangeSource     string    `json:"target_range_source,omitempty"`
	SelicTargetPct        *float64  `json:"selic_target_pct,omitempty"`
	SelicEffectiveFrom    *string   `json:"selic_effective_from,omitempty"`
	SelicSource           string    `json:"selic_source,omitempty"`
	NextDecision          *string   `json:"next_decision,omitempty"`
	UpcomingDecisions     []string  `json:"upcoming_decisions,omitempty"`
	LastScheduledDecision *string   `json:"last_scheduled_decision_in_table,omitempty"`
	CalendarSource        string    `json:"calendar_source,omitempty"`
	CalendarNote          string    `json:"calendar_note,omitempty"`
	ScheduleStatus        string    `json:"schedule_status,omitempty"`
	RateMayBeStale        bool      `json:"rate_may_be_stale"`
	RateStaleReason       string    `json:"rate_stale_reason,omitempty"`
	hasCalendar           bool
}

type cbData struct {
	Today                    string            `json:"today"`
	What                     string            `json:"what"`
	Banks                    map[string]*cbRow `json:"banks"`
	CalendarStalenessWarning string            `json:"calendar_staleness_warning,omitempty"`
}

var dsCBPolicy = &pdDataset{
	ID: "cb_policy_rates", SchemaVersion: 1,
	Title:       "Central-bank policy rates and decision dates",
	Description: "A central bank's policy rate (BIS, with the date and size of its last change; the Fed's target range from FRED; Brazil's Selic target from the BCB) and its next scheduled decision dates. Covers FED, ECB, BOJ, BOE, BCB, SNB, RBA, BOC, BOI, or all.",
	Output:      "data: today, what, banks {CODE: {bank, policy_rate_pct, rate_as_of, rate_effective_from, previous_rate_pct, last_change_bps, target_range_pct, selic_target_pct, next_decision, upcoming_decisions, last_scheduled_decision_in_table, calendar_source, schedule_status (scheduled, or unknown with the reason in calendar_note), rate_available, rate_may_be_stale, ...}} (one entry, or every bank for all), calendar_staleness_warning",
	Params: []pdParam{
		{Name: "bank", Kind: "string", Required: true, MaxLen: 64, Pattern: pdWordRE, Norm: pdTrim, Doc: "FED, ECB, BOJ, BOE, BCB, SNB, RBA, BOC, BOI, a common name (Brazil, Copom, FOMC), or all"},
		{Name: "what", Kind: "enum", Enum: []string{"rate", "next_meeting", "both"}, Default: "both", Doc: "rate, next_meeting (no upstream request) or both",
			Norm: resolveWhat},
	},
	Validate: func(p *pdParams, _ time.Time) bool {
		code, ok := resolveBank(p.Str("bank"))
		if !ok {
			return false
		}
		p.derived["bank"] = code
		if !p.Has("what") {
			p.Set("what", "both")
		}
		return true
	},
	Run: func(ctx context.Context, r *pdRun, p *pdParams) (any, error) {
		today := r.today().Format("2006-01-02")
		what := p.Str("what")
		out := cbData{Today: today, What: what}
		if today > cbCalendarValidThru && what != "rate" {
			out.CalendarStalenessWarning = "meeting calendars were curated through " + cbCalendarValidThru + " and today is " + today +
				"; the schedules are past their verified window and must be re-verified against each bank's official calendar"
		}
		code := p.derived["bank"]
		out.Banks = map[string]*cbRow{}
		for _, b := range cbBanks {
			if code == "ALL" || b.Code == code {
				out.Banks[b.Code] = cbBankRow(ctx, r, b, what, today)
			}
		}
		return out, nil
	},
	Source:  "Bank for International Settlements (policy rates); Banco Central do Brasil (SGS 432); FRED (Fed target range); each bank's published calendar",
	Hosts:   []string{bisHost, bcbHost, fredHost},
	Licence: "BIS statistics: free reuse with attribution; BCB open data; FRED terms for the target range", Attribution: "BIS central bank policy rates (WS_CBPOL); Banco Central do Brasil; FRED",
	TermsURL: "https://www.bis.org/terms_statistics.htm", TermsStatus: "verify before production (BIS, BCB terms; calendars need a yearly refresh)",
	Key: KeyFRED, KeyOptional: true, TTL: time.Hour, Price: 1,
}

func cbBankRow(ctx context.Context, r *pdRun, b cbBank, what, today string) *cbRow {
	row := &cbRow{Bank: b.Code + " — " + b.Name}
	var liveNext string
	if what != "next_meeting" {
		liveNext = cbRate(ctx, r, b, row)
	}
	if what != "rate" {
		row.hasCalendar = true
		r.static("https://" + b.CalendarSource[strings.LastIndex(b.CalendarSource, ", ")+2:])
		var upcoming []string
		for _, d := range b.Decisions {
			if d >= today {
				if len(upcoming) < cbMaxUpcoming {
					upcoming = append(upcoming, d)
				}
			} else {
				row.LastScheduledDecision = sptr(d)
			}
		}
		row.UpcomingDecisions = append([]string{}, upcoming...)
		if len(upcoming) > 0 {
			row.NextDecision = sptr(upcoming[0])
		}
		row.CalendarSource = b.CalendarSource
		row.ScheduleStatus = "scheduled"
		switch {
		case len(upcoming) == 0 && b.ScheduleUnknown != "":
			row.ScheduleStatus, row.CalendarNote = "unknown", b.ScheduleUnknown
		case len(upcoming) == 0:
			row.ScheduleStatus = "unknown"
			row.CalendarNote = "the decision calendar has no future dates left; verify the next meeting date from the bank's own calendar"
		case b.CalendarNote != "":
			row.CalendarNote = b.CalendarNote
		}
		if liveNext != "" {
			if row.NextDecision == nil || liveNext != *row.NextDecision {
				row.CalendarNote = "next_decision " + liveNext + " comes from BCB's own SGS 432 forward validity window (live); the static Copom table said " +
					deref(row.NextDecision) + "; trust the live value"
				later := []string{liveNext}
				for _, d := range row.UpcomingDecisions {
					if d > liveNext && len(later) < cbMaxUpcoming {
						later = append(later, d)
					}
				}
				row.UpcomingDecisions = later
			}
			row.NextDecision = sptr(liveNext)
			row.ScheduleStatus = "scheduled"
			row.CalendarSource = "BCB SGS 432 forward validity window (live, api.bcb.gov.br); " + b.CalendarSource
		}
	}
	if row.RateAsOf != nil && what != "next_meeting" {
		if missed := possiblyUnreflected(b, *row.RateAsOf, deref(row.RateEffectiveFrom), today); len(missed) > 0 {
			row.RateMayBeStale = true
			row.RateStaleReason = "scheduled decision(s) " + strings.Join(missed, ", ") + " fall after, or within " + strconv.Itoa(cbEffectiveLagDays) +
				" days before, rate_as_of " + *row.RateAsOf + " with no level change since; this rate may not reflect them"
		}
	}
	return row
}

// cbRate fills the live-rate fields; it returns BCB's live next decision
// date when the Selic series gives one.
func cbRate(ctx context.Context, r *pdRun, b cbBank, row *cbRow) string {
	raw, err := r.fetch(ctx, pdFetch{Key: "bis:" + b.Area, TTL: time.Hour, Host: bisHost, Path: "/api/v1/data/WS_CBPOL/D." + b.Area + "/all",
		Query: url.Values{"lastNObservations": {strconv.Itoa(cbHistoryObs)}, "format": {"csv"}, "detail": {"dataonly"}}, Accept: "text/csv",
		MaxBytes: 1 << 20, Parse: parseBISCSV})
	var s pdSeries
	if err == nil {
		s, err = unmarshalCached[pdSeries](raw)
	}
	if err == nil && len(s.D) > 0 {
		n := len(s.D)
		row.PolicyRatePct, row.RateAsOf = fptr(s.V[n-1]), sptr(s.D[n-1])
		row.RateName, row.RateSource = b.RateName, bisRateSource
		for i := 1; i < n; i++ {
			if s.V[i] != s.V[i-1] {
				row.RateEffectiveFrom, row.PreviousRatePct = sptr(s.D[i]), fptr(s.V[i-1])
			}
		}
		if row.PreviousRatePct != nil {
			row.LastChangeBps = fptr(pyRound((s.V[n-1]-*row.PreviousRatePct)*100, 1))
		} else {
			row.LastChangeNote = "policy rate unchanged across the last " + strconv.Itoa(n) + " BIS observations (about a year); the last move predates the window"
		}
		row.RateNote = "policy_rate_pct is the BIS series value as of rate_as_of; the series can lag the latest decision"
		t := true
		row.RateAvailable = &t
	} else {
		f := false
		row.RateAvailable = &f
		row.RateNote = "the live BIS policy rate is unavailable right now; no current rate is returned (the meeting schedule is still valid)"
	}
	switch b.Code {
	case "FED":
		if r.key == "" {
			return ""
		}
		var lo, hi fredLatest
		for _, sid := range []string{"DFEDTARU", "DFEDTARL"} {
			raw, err := r.fetch(ctx, pdFetch{Key: "fred:" + sid, TTL: time.Hour, Host: fredHost, Path: fredObs,
				Query: url.Values{"series_id": {sid}, "file_type": {"json"}, "sort_order": {"desc"}, "limit": {"1"}}, KeyParam: "api_key",
				MaxBytes: 64 << 10, Parse: parseFREDLatest})
			if err != nil {
				return ""
			}
			v, err := unmarshalCached[fredLatest](raw)
			if err != nil {
				return ""
			}
			if sid == "DFEDTARU" {
				hi = v
			} else {
				lo = v
			}
		}
		if lo.Value != nil && hi.Value != nil {
			row.TargetRangePct = []float64{*lo.Value, *hi.Value}
			row.TargetRangeAsOf = lo.Date
			row.TargetRangeSource = "FRED DFEDTARL / DFEDTARU (api.stlouisfed.org); Fed markets are quoted on the range, policy_rate_pct is its midpoint"
		}
	case "BCB":
		today := r.today()
		start, end := today.AddDate(0, 0, -180).Format("02/01/2006"), today.AddDate(0, 0, 180).Format("02/01/2006")
		raw, err := r.fetch(ctx, pdFetch{Key: "selic:" + today.Format("2006-01-02"), TTL: time.Hour, Host: bcbHost, Path: "/dados/serie/bcdata.sgs.432/dados",
			Query: url.Values{"formato": {"json"}, "dataInicial": {start}, "dataFinal": {end}}, MaxBytes: 1 << 20, Parse: parseBCB})
		if err != nil {
			return ""
		}
		v, err := unmarshalCached[bcbSelic](raw)
		if err != nil || v.Rate == nil {
			return ""
		}
		row.SelicTargetPct, row.SelicEffectiveFrom = v.Rate, sptr(v.EffectiveFrom)
		row.SelicSource = "BCB SGS series 432 'Meta Selic definida pelo Copom' (api.bcb.gov.br)"
		if v.ValidThrough > today.Format("2006-01-02") {
			return v.ValidThrough
		}
	}
	return ""
}

// possiblyUnreflected lists scheduled decisions up to today that the rate
// observation may predate: its as-of is before the decision, or within
// cbEffectiveLagDays after it with no level change since.
func possiblyUnreflected(b cbBank, asOf, effectiveFrom, today string) []string {
	var out []string
	for _, d := range b.Decisions {
		if d > today || (effectiveFrom != "" && effectiveFrom >= d) {
			continue
		}
		t, _ := time.Parse("2006-01-02", d)
		if asOf < t.AddDate(0, 0, cbEffectiveLagDays).Format("2006-01-02") {
			out = append(out, d)
		}
	}
	return out
}

// --- Federal Reserve nowcasts -------------------------------------------------

const (
	atlantaHost   = "www.atlantafed.org"
	nyfedHost     = "www.newyorkfed.org"
	clevelandHost = "www.clevelandfed.org"
	gdpnowPath    = "/research-and-data/data/gdpnow"
	nyfedDataPath = "/medialibrary/Research/Interactives/Data/NowCast/data/"
	clevelandPath = "/-/media/files/webcharts/inflationnowcasting/nowcast_"
	clevelandPage = "https://www.clevelandfed.org/indicators-and-data/inflation-nowcasting"
	nyfedPage     = "https://www.newyorkfed.org/research/policy/nowcast"
)

var (
	gdpnowValueRE   = regexp.MustCompile(`class="data-value">\s*(-?[\d.]+)\s*%`)
	gdpnowQuarterRE = regexp.MustCompile(`GDPNow Estimate for (\d{4}:Q[1-4])`)
	gdpnowUpdatedRE = regexp.MustCompile(`<strong>Updated:</strong>\s*([A-Z][a-z]+ \d{1,2}, \d{4})`)
	gdpnowNextRE    = regexp.MustCompile(`<strong>Next update:</strong>\s*([A-Z][a-z]+ \d{1,2}, \d{4})`)
	spacesRE        = regexp.MustCompile(`\s+`)
	nyYearRE        = regexp.MustCompile(`^[0-9]{4}$`)
	nyQuarterRE     = regexp.MustCompile(`^Q[1-4]$`)
	clevTargetRE    = regexp.MustCompile(`^(\d{4})-(\d{1,2})$`)
	clevLabelRE     = regexp.MustCompile(`^(\d{2})/(\d{2})$`)
	clevTipRE       = regexp.MustCompile(`\{br\}(\d{2}/\d{2})\{br\}`)
	clevSeries      = []string{"CPI", "Core CPI", "PCE", "Core PCE"}
)

func usDate(raw string) *string {
	t, err := time.Parse("January 2, 2006", spacesRE.ReplaceAllString(strings.TrimSpace(raw), " "))
	if err != nil {
		return nil
	}
	return sptr(t.Format("2006-01-02"))
}

type gdpnowPage struct {
	Value      float64 `json:"value"`
	Quarter    string  `json:"quarter"`
	Updated    *string `json:"updated"`
	NextUpdate *string `json:"next_update"`
}

func parseGDPNowPage(raw []byte) (any, error) {
	html := string(raw)
	v, q := gdpnowValueRE.FindStringSubmatch(html), gdpnowQuarterRE.FindStringSubmatch(html)
	if v == nil || q == nil {
		return nil, errors.New("GDPNow page layout not recognised")
	}
	f, err := strconv.ParseFloat(v[1], 64)
	if err != nil {
		return nil, err
	}
	out := gdpnowPage{Value: f, Quarter: q[1]}
	if m := gdpnowUpdatedRE.FindStringSubmatch(html); m != nil {
		out.Updated = usDate(m[1])
	}
	if m := gdpnowNextRE.FindStringSubmatch(html); m != nil {
		out.NextUpdate = usDate(m[1])
	}
	return out, nil
}

type gdpnowVintages struct {
	QuarterStart string   `json:"quarter_start"`
	Latest       float64  `json:"latest"`
	LatestAsOf   string   `json:"latest_as_of"`
	Previous     *float64 `json:"previous"`
	PreviousAsOf *string  `json:"previous_as_of"`
}

func parseGDPNowVintages(raw []byte) (any, error) {
	var body struct {
		Observations []map[string]any `json:"observations"`
	}
	if err := decodeJSON(raw, &body); err != nil {
		return nil, err
	}
	type vint struct {
		date, rt string
		v        float64
	}
	var obs []vint
	quarter := ""
	for _, o := range body.Observations {
		d, _ := o["date"].(string)
		rt, _ := o["realtime_start"].(string)
		v := pyFloat(o["value"])
		if d == "" || v == nil {
			continue
		}
		obs = append(obs, vint{d, rt, *v})
		if d > quarter {
			quarter = d
		}
	}
	if len(obs) == 0 {
		return nil, errors.New("no GDPNOW vintages")
	}
	var vs []vint
	for _, o := range obs {
		if o.date == quarter {
			vs = append(vs, o)
		}
	}
	sort.SliceStable(vs, func(i, j int) bool { return vs[i].rt < vs[j].rt })
	last := vs[len(vs)-1]
	out := gdpnowVintages{QuarterStart: quarter, Latest: last.v, LatestAsOf: last.rt}
	if len(vs) > 1 {
		prev := vs[len(vs)-2]
		out.Previous, out.PreviousAsOf = fptr(prev.v), sptr(prev.rt)
	}
	return out, nil
}

func quarterLabel(start string) string {
	if len(start) < 7 {
		return ""
	}
	y, _ := strconv.Atoi(start[:4])
	m, _ := strconv.Atoi(start[5:7])
	return strconv.Itoa(y) + ":Q" + strconv.Itoa((m-1)/3+1)
}

// nowcastBlock is one nowcast; field names follow Pythia's lookup_nowcast.
type nowcastBlock struct {
	Name                 string                    `json:"name"`
	Measure              string                    `json:"measure"`
	TargetPeriod         *string                   `json:"target_period"`
	LatestValue          *float64                  `json:"latest_value,omitempty"`
	LatestValueUnrounded *float64                  `json:"latest_value_unrounded,omitempty"`
	LatestAsOf           *string                   `json:"latest_as_of"`
	NextUpdate           *string                   `json:"next_update,omitempty"`
	PreviousValue        *float64                  `json:"previous_value"`
	PreviousAsOf         *string                   `json:"previous_as_of,omitempty"`
	PreviousNote         string                    `json:"previous_note,omitempty"`
	BandsPct             map[string][]*float64     `json:"bands_pct,omitempty"`
	SourceStatement      string                    `json:"source_statement,omitempty"`
	Series               map[string]map[string]any `json:"series,omitempty"`
	FREDSeries           string                    `json:"fred_series,omitempty"`
	Warning              string                    `json:"warning,omitempty"`
	PartialErrors        []string                  `json:"partial_errors,omitempty"`
	SourceURL            string                    `json:"source_url"`
}

type nowcastData struct {
	Economy      string              `json:"economy"`
	Measure      string              `json:"measure"`
	Nowcasts     []nowcastBlock      `json:"nowcasts"`
	SourceErrors []map[string]string `json:"source_errors"`
}

var dsNowcasts = &pdDataset{
	ID: "us_nowcasts", SchemaVersion: 1,
	Title:       "US GDP and inflation nowcasts",
	Description: "Model estimates before the official print: Atlanta Fed GDPNow, New York Fed Staff Nowcast (with 50% and 80% bands) and Cleveland Fed inflation nowcasts (CPI, core CPI, PCE, core PCE, month over month and year over year), each with latest and previous values, as-of dates and target period.",
	Output:      "data: economy (US), measure, nowcasts[] (name, measure, target_period, latest_value, latest_as_of, next_update, previous_value, previous_as_of, bands_pct, series, source_url), source_errors[]",
	Params: []pdParam{
		{Name: "measure", Kind: "enum", Enum: []string{"gdp", "inflation", "all"}, Default: "all", Doc: "gdp, inflation or all",
			Norm: func(s string) (string, bool) {
				m := map[string]string{"gdp": "gdp", "growth": "gdp", "activity": "gdp", "output": "gdp", "inflation": "inflation", "cpi": "inflation",
					"pce": "inflation", "prices": "inflation", "all": "all", "both": "all"}
				v, ok := m[strings.ToLower(strings.TrimSpace(s))]
				return v, ok
			}},
	},
	Validate: func(p *pdParams, _ time.Time) bool {
		if !p.Has("measure") {
			p.Set("measure", "all")
		}
		return true
	},
	Run: func(ctx context.Context, r *pdRun, p *pdParams) (any, error) {
		measure := p.Str("measure")
		out := nowcastData{Economy: "US", Measure: measure, Nowcasts: []nowcastBlock{}, SourceErrors: []map[string]string{}}
		type job struct {
			name string
			fn   func() ([]nowcastBlock, error)
		}
		var jobs []job
		if measure != "inflation" {
			jobs = append(jobs, job{"Atlanta Fed GDPNow", func() ([]nowcastBlock, error) { return gdpnowBlock(ctx, r) }},
				job{"NY Fed Staff Nowcast", func() ([]nowcastBlock, error) { return nyfedBlocks(ctx, r) }})
		}
		if measure != "gdp" {
			jobs = append(jobs, job{"Cleveland Fed inflation nowcast", func() ([]nowcastBlock, error) { return clevelandBlocks(ctx, r) }})
		}
		for _, j := range jobs {
			blocks, err := j.fn()
			if err != nil {
				reason := "unavailable"
				var pe *pdErr
				if errors.As(err, &pe) {
					reason = "unavailable: " + pe.Reason
				}
				out.SourceErrors = append(out.SourceErrors, map[string]string{"source": j.name, "error": reason})
				continue
			}
			out.Nowcasts = append(out.Nowcasts, blocks...)
		}
		if len(out.Nowcasts) == 0 {
			return nil, pdFail("upstream_busy", "every_source_failed")
		}
		for _, b := range out.Nowcasts {
			if b.LatestAsOf != nil && *b.LatestAsOf > r.asOf {
				r.asOf = *b.LatestAsOf
			}
		}
		return out, nil
	},
	Source:  "Federal Reserve Banks of Atlanta (GDPNow), New York (Staff Nowcast) and Cleveland (inflation nowcasting); FRED/ALFRED for GDPNow vintages",
	Hosts:   []string{atlantaHost, nyfedHost, clevelandHost, fredHost},
	Licence: "published by the Federal Reserve Banks; model estimates, not official forecasts", Attribution: "Federal Reserve Bank of Atlanta GDPNow; Federal Reserve Bank of New York Staff Nowcast; Federal Reserve Bank of Cleveland inflation nowcasting",
	TermsURL: "https://www.atlantafed.org/research-and-data/data/gdpnow", TermsStatus: "verify before production (each bank's terms of use)",
	Key: KeyFRED, KeyOptional: true, TTL: time.Hour, Price: 1,
}

func gdpnowBlock(ctx context.Context, r *pdRun) ([]nowcastBlock, error) {
	var page *gdpnowPage
	var vint *gdpnowVintages
	var errs []string
	raw, err := r.fetch(ctx, pdFetch{Key: "gdpnow_page", TTL: time.Hour, Host: atlantaHost, Path: gdpnowPath, Accept: "text/html", MaxBytes: 4 << 20, Parse: parseGDPNowPage})
	if err == nil {
		var pg gdpnowPage
		if pg, err = unmarshalCached[gdpnowPage](raw); err == nil {
			page = &pg
		}
	}
	if err != nil {
		errs = append(errs, "atlantafed.org page unavailable")
	}
	if r.key != "" {
		today := r.today()
		raw, err := r.fetch(ctx, pdFetch{Key: "gdpnow_vintages:" + today.Format("2006-01-02"), TTL: time.Hour, Host: fredHost, Path: fredObs,
			Query: url.Values{"series_id": {"GDPNOW"}, "file_type": {"json"}, "realtime_start": {today.AddDate(0, 0, -150).Format("2006-01-02")},
				"realtime_end": {"9999-12-31"}, "observation_start": {today.AddDate(0, 0, -200).Format("2006-01-02")}},
			KeyParam: "api_key", MaxBytes: 4 << 20, Parse: parseGDPNowVintages})
		if err == nil {
			var v gdpnowVintages
			if v, err = unmarshalCached[gdpnowVintages](raw); err == nil {
				vint = &v
			}
		}
		if err != nil {
			errs = append(errs, "FRED GDPNOW vintages unavailable")
		}
	}
	if page == nil && vint == nil {
		return nil, pdFail("upstream_busy", "gdpnow_unavailable")
	}
	b := nowcastBlock{Name: "Atlanta Fed GDPNow", Measure: "real GDP growth, % q/q SAAR (model estimate, not an official Atlanta Fed forecast)",
		SourceURL: "https://" + atlantaHost + gdpnowPath}
	if page != nil {
		b.TargetPeriod, b.LatestValue, b.LatestAsOf, b.NextUpdate = sptr(page.Quarter), fptr(page.Value), page.Updated, page.NextUpdate
	}
	switch {
	case vint != nil:
		b.FREDSeries = "FRED/ALFRED GDPNOW vintages (api.stlouisfed.org)"
		q := quarterLabel(vint.QuarterStart)
		if page != nil && q != page.Quarter {
			b.Warning = "FRED's newest GDPNOW quarter is " + q + " but the page shows " + page.Quarter + "; previous value omitted"
		} else {
			if b.TargetPeriod == nil {
				b.TargetPeriod = sptr(q)
			}
			b.LatestValueUnrounded = fptr(vint.Latest)
			if b.LatestValue == nil {
				b.LatestValue = fptr(pyRound(vint.Latest, 1))
			}
			if b.LatestAsOf == nil {
				b.LatestAsOf = sptr(vint.LatestAsOf)
			}
			if page != nil && math.Abs(pyRound(vint.Latest, 1)-page.Value) > 0.051 && page.Updated != nil && vint.LatestAsOf == *page.Updated {
				b.Warning = "the page shows " + strconv.FormatFloat(page.Value, 'f', -1, 64) + " but FRED's same-day vintage is " + strconv.FormatFloat(vint.Latest, 'f', -1, 64)
			}
			b.PreviousValue, b.PreviousAsOf = vint.Previous, vint.PreviousAsOf
		}
	case r.key == "":
		b.PreviousNote = "the previous estimate needs FRED vintages: the FRED key is not configured"
	}
	b.PartialErrors = errs
	return []nowcastBlock{b}, nil
}

// parseNYFedCSV reads one of the NY Fed nowcast data files (UTF-8 with a BOM,
// ragged rows) into rows keyed by header.
func parseNYFedCSV(raw []byte) (any, error) {
	rd := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))))
	rd.FieldsPerRecord, rd.LazyQuotes = -1, true
	head, err := rd.Read()
	if err != nil {
		return nil, err
	}
	rows := []map[string]string{}
	for {
		rec, err := rd.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		m := map[string]string{}
		for i, h := range head {
			if i < len(rec) && rec[i] != "" {
				m[h] = rec[i]
			}
		}
		rows = append(rows, m)
	}
	return rows, nil
}

func nyfedCSV(ctx context.Context, r *pdRun, name string) ([]map[string]string, error) {
	raw, err := r.fetch(ctx, pdFetch{Key: "nyfed:" + name, TTL: time.Hour, Host: nyfedHost, Path: nyfedDataPath + name, Accept: "text/csv",
		MaxBytes: 4 << 20, Parse: parseNYFedCSV})
	if err != nil {
		return nil, err
	}
	return unmarshalCached[[]map[string]string](raw)
}

func nyDate(label, year string) *string {
	t, err := time.Parse("2-Jan-2006", strings.TrimSpace(label)+"-"+strings.TrimSpace(year))
	if err != nil {
		return nil
	}
	return sptr(t.Format("2006-01-02"))
}

func nyFloat(s string) *float64 {
	if s == "" || s == "." {
		return nil
	}
	return pyFloat(s)
}

func nyfedBlocks(ctx context.Context, r *pdRun) ([]nowcastBlock, error) {
	meta, err := nyfedCSV(ctx, r, "nowcast-history-meta-data.csv")
	if err != nil {
		return nil, err
	}
	var open [][2]string
	for _, m := range meta {
		if strings.TrimSpace(m["concluded"]) != "0" || strings.TrimSpace(m["advancedGDP"]) != "" {
			continue
		}
		k := [2]string{strings.TrimSpace(m["year"]), strings.TrimSpace(m["quarter"])}
		if nyYearRE.MatchString(k[0]) && nyQuarterRE.MatchString(k[1]) && !containsPair(open, k) {
			open = append(open, k)
		}
	}
	if len(open) == 0 {
		return nil, pdFail("upstream_failed", "no_open_quarter")
	}
	var blocks []nowcastBlock
	var last error
	for _, k := range open {
		b, err := nyfedQuarter(ctx, r, k[0], k[1])
		if err != nil {
			last = err
			continue
		}
		blocks = append(blocks, b)
	}
	if len(blocks) == 0 {
		return nil, last
	}
	return blocks, nil
}

func containsPair(list [][2]string, k [2]string) bool {
	for _, v := range list {
		if v == k {
			return true
		}
	}
	return false
}

func nyfedQuarter(ctx context.Context, r *pdRun, year, quarter string) (nowcastBlock, error) {
	rows, err := nyfedCSV(ctx, r, year+quarter+".csv")
	if err != nil {
		return nowcastBlock{}, err
	}
	var updates []string
	byUpdate, statement, yearOf := map[string]map[string]string{}, map[string]string{}, map[string]string{}
	for _, row := range rows {
		label := strings.TrimSpace(row["Update"])
		if label == "" {
			continue
		}
		if !contains(updates, label) {
			updates = append(updates, label)
		}
		if y := strings.TrimSpace(row["Year"]); y != "" {
			yearOf[label] = y
		}
		if nyFloat(row["Nowcast"]) != nil {
			byUpdate[label] = row
		}
		if note := strings.TrimSpace(row["Note"]); strings.Contains(note, "Staff Nowcast for") {
			statement[label] = note
		}
	}
	var with []string
	for _, u := range updates {
		if byUpdate[u] != nil {
			with = append(with, u)
		}
	}
	if len(with) == 0 {
		return nowcastBlock{}, pdFail("upstream_failed", "no_nowcast_yet")
	}
	latestLabel := with[len(with)-1]
	latest := byUpdate[latestLabel]
	yr := yearOf[latestLabel]
	if yr == "" {
		yr = year
	}
	b := nowcastBlock{Name: "NY Fed Staff Nowcast", Measure: "real GDP growth, % q/q SAAR", TargetPeriod: sptr(year + ":" + quarter),
		LatestValue: nyFloat(latest["Nowcast"]), LatestAsOf: nyDate(latestLabel, yr),
		BandsPct:  map[string][]*float64{"50%": {nyFloat(latest["50_lb"]), nyFloat(latest["50_ub"])}, "80%": {nyFloat(latest["80_lb"]), nyFloat(latest["80_ub"])}},
		SourceURL: "https://" + nyfedHost + nyfedDataPath + year + quarter + ".csv (page: " + nyfedPage + ")"}
	if len(with) > 1 {
		prev := with[len(with)-2]
		py := yearOf[prev]
		if py == "" {
			py = yr
		}
		b.PreviousValue, b.PreviousAsOf = nyFloat(byUpdate[prev]["Nowcast"]), nyDate(prev, py)
	}
	for i, u := range updates {
		if u == latestLabel && i+1 < len(updates) {
			ny := yearOf[updates[i+1]]
			if ny == "" {
				ny = yr
			}
			b.NextUpdate = nyDate(updates[i+1], ny)
		}
	}
	b.SourceStatement = statement[latestLabel]
	return b, nil
}

// streamCleveland keeps the last two elements (the two most recent target
// months) of a Cleveland Fed chart JSON (several MB), parsed as it streams.
func streamCleveland(rd io.Reader) (any, error) {
	dec := json.NewDecoder(rd)
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, errors.New("not an array")
	}
	var tail []json.RawMessage
	for dec.More() {
		var el json.RawMessage
		if err := dec.Decode(&el); err != nil {
			return nil, err
		}
		tail = append(tail, el)
		if len(tail) > 2 {
			tail = tail[1:]
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if len(tail) == 0 {
		return nil, errors.New("empty")
	}
	return tail, nil
}

type clevElement struct {
	Chart struct {
		Comment    string `json:"_comment"`
		Subcaption string `json:"subcaption"`
	} `json:"chart"`
	Categories []struct {
		Category []map[string]any `json:"category"`
	} `json:"categories"`
	Dataset []struct {
		SeriesName string           `json:"seriesname"`
		Data       []map[string]any `json:"data"`
	} `json:"dataset"`
}

func clevLabelDate(label, update string) *string {
	m := clevLabelRE.FindStringSubmatch(strings.TrimSpace(label))
	upd, err := time.Parse("2006-01-02", update)
	if m == nil || err != nil {
		return nil
	}
	month, _ := strconv.Atoi(m[1])
	day, _ := strconv.Atoi(m[2])
	year := upd.Year()
	if month > int(upd.Month()) {
		year--
	}
	t := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if t.Day() != day || int(t.Month()) != month {
		return nil
	}
	return sptr(t.Format("2006-01-02"))
}

func clevParse(el clevElement) (target, update string, series map[string]map[string]any) {
	if m := clevTargetRE.FindStringSubmatch(strings.TrimSpace(el.Chart.Subcaption)); m != nil {
		mo, _ := strconv.Atoi(m[2])
		target = m[1] + "-" + pad2(mo)
	}
	update = el.Chart.Comment
	if len(update) > 10 {
		update = update[:10]
	}
	var labels []string
	if len(el.Categories) > 0 {
		for _, c := range el.Categories[0].Category {
			if strings.ToLower(pyStr(c["vline"])) != "true" {
				labels = append(labels, pyStr(c["label"]))
			}
		}
	}
	series = map[string]map[string]any{}
	for _, ds := range el.Dataset {
		name := strings.TrimSpace(strings.ReplaceAll(ds.SeriesName, " Inflation", ""))
		actual := strings.HasPrefix(name, "Actual ")
		base := strings.TrimSpace(strings.TrimPrefix(name, "Actual "))
		if !contains(clevSeries, base) || update == "" {
			continue
		}
		type pt struct {
			when *string
			v    float64
		}
		var pts []pt
		for i, d := range ds.Data {
			v := pyFloat(d["value"])
			if v == nil {
				continue
			}
			label := ""
			if m := clevTipRE.FindStringSubmatch(pyStr(d["tooltext"])); m != nil {
				label = m[1]
			} else if i < len(labels) {
				label = labels[i]
			}
			pts = append(pts, pt{clevLabelDate(label, update), *v})
		}
		entry := series[base]
		if entry == nil {
			entry = map[string]any{}
			series[base] = entry
		}
		if len(pts) == 0 {
			continue
		}
		last := pts[len(pts)-1]
		if actual {
			entry["actual"], entry["actual_date"] = pyRound(last.v, 3), last.when
			continue
		}
		since := last.when
		var prev *pt
		for i := len(pts) - 1; i >= 0; i-- {
			if math.Abs(pts[i].v-last.v) > 1e-9 {
				prev = &pts[i]
				break
			}
			since = pts[i].when
		}
		entry["nowcast"], entry["as_of"], entry["unchanged_since"] = pyRound(last.v, 3), last.when, since
		if prev != nil {
			entry["previous"], entry["previous_as_of"] = pyRound(prev.v, 3), prev.when
		}
	}
	return target, update, series
}

func clevelandBlocks(ctx context.Context, r *pdRun) ([]nowcastBlock, error) {
	perTarget := map[string]map[string]map[string]any{}
	stamp := ""
	for _, f := range [][2]string{{"month", "mom"}, {"year", "yoy"}} {
		raw, err := r.fetch(ctx, pdFetch{Key: "cleveland:" + f[0], TTL: time.Hour, Host: clevelandHost, Path: clevelandPath + f[0] + ".json",
			MaxBytes: 24 << 20, Timeout: 45 * time.Second, Stream: streamCleveland})
		if err != nil {
			return nil, err
		}
		els, err := unmarshalCached[[]json.RawMessage](raw)
		if err != nil {
			return nil, err
		}
		for _, rawEl := range els {
			var el clevElement
			if json.Unmarshal(rawEl, &el) != nil {
				continue
			}
			target, update, series := clevParse(el)
			if target == "" {
				continue
			}
			if stamp == "" {
				stamp = update
			}
			block := perTarget[target]
			if block == nil {
				block = map[string]map[string]any{}
				perTarget[target] = block
			}
			for name, vals := range series {
				if len(vals) == 0 {
					continue
				}
				if block[name] == nil {
					block[name] = map[string]any{}
				}
				for k, v := range vals {
					block[name][f[1]+"_"+k] = v
				}
			}
		}
	}
	if len(perTarget) == 0 {
		return nil, pdFail("upstream_failed", "no_target_months")
	}
	targets := make([]string, 0, len(perTarget))
	for t := range perTarget {
		targets = append(targets, t)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(targets)))
	var blocks []nowcastBlock
	for _, t := range targets {
		blocks = append(blocks, nowcastBlock{Name: "Cleveland Fed inflation nowcast",
			Measure:      "inflation, % change: mom_* month over month, yoy_* year over year; *_actual once the official print is out",
			TargetPeriod: sptr(t), LatestAsOf: nonEmpty(stamp), Series: perTarget[t],
			SourceURL: "https://" + clevelandHost + clevelandPath + "month.json ; https://" + clevelandHost + clevelandPath + "year.json (page: " + clevelandPage + ")"})
	}
	return blocks, nil
}
