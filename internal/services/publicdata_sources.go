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

// The catalogue. Parsing and normalisation follow the Pythia adapters they
// were ported from (field names, units and rounding), so the two can be
// compared value by value (scripts/public_data_parity.py).

var pdCatalogue = []*pdDataset{
	dsNOAAStation, dsSeaIce, dsRecalls, dsCongressBills, dsFECTotals, dsCryptoPrice,
	dsFREDSeries, dsFREDReleases, dsCBPolicy, dsNowcasts,
}

var pdByID = func() map[string]*pdDataset {
	m := map[string]*pdDataset{}
	for _, ds := range pdCatalogue {
		m[ds.ID] = ds
	}
	return m
}()

// pdHostGroup maps an upstream host to the budget it shares.
var pdHostGroup = map[string]string{
	"api.congress.gov": "api.data.gov", "api.open.fec.gov": "api.data.gov",
}

// pdUpstreamLimits are each upstream's own budget, across every caller:
// politeness below the published limits (api.data.gov: 1,000 an hour a key;
// FRED: 120 a minute; openFDA keyless: 240 a minute and 1,000 a day an IP).
var pdUpstreamLimits = map[string]pdLimit{
	"www.ncei.noaa.gov":       {60, 5000},
	"noaadata.apps.nsidc.org": {10, 500},
	"www.fsis.usda.gov":       {10, 500},
	"api.fda.gov":             {40, 900},
	"api.data.gov":            {15, 20000},
	"api.coingecko.com":       {10, 5000},
	"api.stlouisfed.org":      {100, 50000},
	"stats.bis.org":           {30, 5000},
	"api.bcb.gov.br":          {20, 2000},
	"www.atlantafed.org":      {10, 1000},
	"www.newyorkfed.org":      {10, 1000},
	"www.clevelandfed.org":    {6, 500},
}

// --- shared helpers ---------------------------------------------------------

// pyRound is Python's round(x, n) for a float: the exact binary value
// correctly rounded to n decimals, ties to even.
func pyRound(x float64, n int) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', n, 64), 64)
	return v
}

func fptr(x float64) *float64 { return &x }
func sptr(s string) *string   { return &s }

// pyFloat is Python's float() on a JSON value: a number, or a string holding
// one (surrounding spaces allowed); anything else, or a non-finite value, is
// nil.
func pyFloat(v any) *float64 {
	var f float64
	switch t := v.(type) {
	case float64:
		f = t
	case json.Number:
		var err error
		if f, err = t.Float64(); err != nil {
			return nil
		}
	case string:
		var err error
		if f, err = strconv.ParseFloat(strings.TrimSpace(t), 64); err != nil {
			return nil
		}
	default:
		return nil
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return &f
}

// pyStr is Python's str(v or "") for a JSON scalar.
func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return ""
	case float64:
		if t == 0 {
			return ""
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	}
	return ""
}

// decodeJSON decodes a whole upstream body with numbers kept exact.
func decodeJSON(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

// pdSeries is a daily or periodic series, ascending by date, as cached.
type pdSeries struct {
	D []string  `json:"d"`
	V []float64 `json:"v"`
}

type pdObs struct {
	Date  string  `json:"date"`
	Value float64 `json:"value"`
}

// window returns the observations within [start, end] (either may be ""),
// newest first, at most limit of them; count is how many are in range and
// nextEnd the date to pass as end_date for the next (older) page.
func (s pdSeries) window(start, end string, limit int) (obs []pdObs, count int, nextEnd *string) {
	obs = []pdObs{}
	for i := len(s.D) - 1; i >= 0; i-- {
		d := s.D[i]
		if (end != "" && d > end) || (start != "" && d < start) {
			continue
		}
		count++
		if len(obs) < limit {
			obs = append(obs, pdObs{Date: d, Value: s.V[i]})
		} else if nextEnd == nil {
			nextEnd = sptr(d)
		}
	}
	return obs, count, nextEnd
}

// seriesFromPoints sorts points by date; a repeated date keeps its last
// value (an upsert, as Pythia's table does).
func seriesFromPoints(points map[string]float64) pdSeries {
	s := pdSeries{D: make([]string, 0, len(points)), V: make([]float64, 0, len(points))}
	for d := range points {
		s.D = append(s.D, d)
	}
	sort.Strings(s.D)
	for _, d := range s.D {
		s.V = append(s.V, points[d])
	}
	return s
}

func unmarshalCached[T any](raw json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, pdFail("upstream_failed", "cache_corrupt")
	}
	return v, nil
}

func dateParam(name, doc string) pdParam {
	return pdParam{Name: name, Kind: "date", Doc: doc}
}

func limitParam(def, max int64) pdParam {
	return pdParam{Name: "limit", Kind: "int", Min: 1, Max: max, Default: strconv.FormatInt(def, 10),
		Doc: "most rows returned, newest first; next_end_date pages back"}
}

func intOr(p *pdParams, name string, def int64) int64 {
	if n, ok := p.Int(name); ok {
		return n
	}
	return def
}

var pdWordRE = regexp.MustCompile(`^[\p{L}\p{N} .,'&()/+:-]+$`)

// --- NOAA NCEI station daily summaries --------------------------------------

const (
	noaaHost      = "www.ncei.noaa.gov"
	noaaMSToMPH   = 2.236936
	noaaMaxDays   = 370
	noaaDefaultTy = "WSFG,WSF5,WSF2,AWND,TMAX,TMIN,PRCP"
)

var (
	noaaElements = []string{"WSFG", "WSF5", "WSF2", "AWND", "TMAX", "TMIN", "PRCP"}
	noaaGust     = []string{"WSFG", "WSF5", "WSF2"}
	noaaAliases  = map[string]string{
		"KMWN": "USW00014755", "MOUNT WASHINGTON": "USW00014755", "MT WASHINGTON": "USW00014755",
		"KJFK": "USW00094789", "KORD": "USW00094846", "KDEN": "USW00003017", "KLAX": "USW00023174",
	}
	ghcnRE        = regexp.MustCompile(`^[A-Z]{2}[A-Z0-9]{9}$`)
	noaaStationRE = regexp.MustCompile(`^[A-Za-z0-9 ]{2,24}$`)
)

type noaaDay struct {
	Date            *string  `json:"date"`
	PeakGustMPH     *float64 `json:"peak_gust_mph"`
	PeakGustMS      *float64 `json:"peak_gust_ms"`
	PeakGustElement *string  `json:"peak_gust_element"`
	AvgWindMPH      *float64 `json:"avg_wind_mph"`
	AvgWindMS       *float64 `json:"avg_wind_ms"`
	TmaxC           *float64 `json:"tmax_c"`
	TmaxF           *float64 `json:"tmax_f"`
	TminC           *float64 `json:"tmin_c"`
	TminF           *float64 `json:"tmin_f"`
	PrecipMM        *float64 `json:"precip_mm"`
}

type noaaData struct {
	Station          string    `json:"station"`
	RequestedStation string    `json:"requested_station"`
	StartDate        string    `json:"start_date"`
	EndDate          string    `json:"end_date"`
	Count            int       `json:"count"`
	PeakGustMaxMPH   *float64  `json:"peak_gust_max_mph"`
	PeakGustMaxDate  *string   `json:"peak_gust_max_date"`
	Days             []noaaDay `json:"days"`
	Truncated        bool      `json:"truncated"`
	Note             string    `json:"note,omitempty"`
}

func round1(v any) *float64 {
	f := pyFloat(v)
	if f == nil {
		return nil
	}
	return fptr(pyRound(*f, 1))
}

func msToMPH(ms *float64) *float64 {
	if ms == nil {
		return nil
	}
	return fptr(pyRound(*ms*noaaMSToMPH, 1))
}

func cToF(c *float64) *float64 {
	if c == nil {
		return nil
	}
	return fptr(pyRound(*c*9.0/5.0+32.0, 1))
}

// parseNOAA reads NCEI's JSON array of per-day objects (element codes as
// strings) into tidy rows, newest first.
func parseNOAA(raw []byte) (any, error) {
	var obs []any
	if err := decodeJSON(raw, &obs); err != nil {
		return nil, err
	}
	if obs == nil {
		return nil, errors.New("not an array")
	}
	days := []noaaDay{}
	for _, o := range obs {
		m, ok := o.(map[string]any)
		if !ok {
			continue
		}
		var d noaaDay
		if s, ok := m["DATE"].(string); ok {
			d.Date = sptr(s)
		}
		for _, code := range noaaGust {
			if v := round1(m[code]); v != nil {
				d.PeakGustMS, d.PeakGustElement = v, sptr(code)
				break
			}
		}
		d.PeakGustMPH = msToMPH(d.PeakGustMS)
		d.AvgWindMS = round1(m["AWND"])
		d.AvgWindMPH = msToMPH(d.AvgWindMS)
		d.TmaxC, d.TminC = round1(m["TMAX"]), round1(m["TMIN"])
		d.TmaxF, d.TminF = cToF(d.TmaxC), cToF(d.TminC)
		d.PrecipMM = round1(m["PRCP"])
		days = append(days, d)
	}
	key := func(d noaaDay) string {
		if d.Date == nil {
			return ""
		}
		return *d.Date
	}
	sort.SliceStable(days, func(i, j int) bool { return key(days[i]) > key(days[j]) })
	return days, nil
}

var dsNOAAStation = &pdDataset{
	ID: "noaa_station_daily", SchemaVersion: 1,
	Title:       "NOAA station daily weather",
	Description: "Daily summaries for a GHCN weather station (NOAA NCEI): peak gust, average wind, max/min temperature, precipitation.",
	Output:      "data: station, requested_station, start_date, end_date, count, peak_gust_max_mph, peak_gust_max_date, days[] (newest first: date, peak_gust_mph, peak_gust_ms, peak_gust_element, avg_wind_mph, avg_wind_ms, tmax_c, tmax_f, tmin_c, tmin_f, precip_mm), truncated",
	Params: []pdParam{
		{Name: "station", Kind: "string", Required: true, Pattern: noaaStationRE, Norm: pdTrim,
			Doc: "GHCN id (USW00014755) or alias: KMWN, MOUNT WASHINGTON, MT WASHINGTON, KJFK, KORD, KDEN, KLAX"},
		dateParam("start_date", "first day; default end_date minus 29 days"),
		dateParam("end_date", "last day; default yesterday (UTC)"),
		{Name: "elements", Kind: "string", Pattern: regexp.MustCompile(`^[A-Z0-9]{4}(,[A-Z0-9]{4}){0,6}$`), Norm: pdUpper,
			Default: noaaDefaultTy, Doc: "comma-separated subset of " + noaaDefaultTy},
		limitParam(31, 200),
	},
	Validate: func(p *pdParams, today time.Time) bool {
		key := strings.ToUpper(p.Str("station"))
		id := noaaAliases[key]
		if id == "" && ghcnRE.MatchString(key) {
			id = key
		}
		if id == "" {
			return false
		}
		p.derived["station_id"] = id
		if p.Has("elements") {
			seen := map[string]bool{}
			for _, e := range strings.Split(p.Str("elements"), ",") {
				if seen[e] || !contains(noaaElements, e) {
					return false
				}
				seen[e] = true
			}
		}
		end := today.AddDate(0, 0, -1)
		if p.Has("end_date") {
			end, _ = pdDate(p.Str("end_date"))
		}
		start := end.AddDate(0, 0, -29)
		if p.Has("start_date") {
			start, _ = pdDate(p.Str("start_date"))
		}
		if start.After(end) || end.Sub(start) > noaaMaxDays*24*time.Hour {
			return false
		}
		p.Set("start_date", start.Format("2006-01-02"))
		p.Set("end_date", end.Format("2006-01-02"))
		return true
	},
	Run: func(ctx context.Context, r *pdRun, p *pdParams) (any, error) {
		station := p.derived["station_id"]
		types := p.Str("elements")
		if types == "" {
			types = noaaDefaultTy
		}
		q := url.Values{"dataset": {"daily-summaries"}, "stations": {station}, "startDate": {p.Str("start_date")},
			"endDate": {p.Str("end_date")}, "dataTypes": {types}, "units": {"metric"}, "format": {"json"}}
		raw, err := r.fetch(ctx, pdFetch{Key: station + "|" + p.Str("start_date") + "|" + p.Str("end_date") + "|" + types,
			TTL: 6 * time.Hour, Host: noaaHost, Path: "/access/services/data/v1", Query: q, MaxBytes: 2 << 20, Parse: parseNOAA})
		if err != nil {
			return nil, err
		}
		all, err := unmarshalCached[[]noaaDay](raw)
		if err != nil {
			return nil, err
		}
		out := noaaData{Station: station, RequestedStation: p.Str("station"), StartDate: p.Str("start_date"), EndDate: p.Str("end_date"),
			Count: len(all), Days: []noaaDay{}}
		if len(all) == 0 {
			out.Note = "no data for this station and window; check the GHCN id or widen the dates"
			return out, nil
		}
		for _, d := range all {
			if g := d.PeakGustMPH; g != nil && (out.PeakGustMaxMPH == nil || *g > *out.PeakGustMaxMPH) {
				out.PeakGustMaxMPH, out.PeakGustMaxDate = g, d.Date
			}
		}
		limit := int(intOr(p, "limit", 31))
		out.Days = all[:min(limit, len(all))]
		if len(all) > limit {
			out.Truncated = true
			out.Note = "showing the " + strconv.Itoa(limit) + " most recent of " + strconv.Itoa(len(all)) + " days; peak_gust_max_mph covers the full window"
		}
		if all[0].Date != nil {
			r.asOf = *all[0].Date
		}
		return out, nil
	},
	Source: "NOAA National Centers for Environmental Information (GHCN-Daily)", Hosts: []string{noaaHost},
	Licence: "US government work, public domain", Attribution: "NOAA NCEI, Global Historical Climatology Network daily",
	TermsURL: "https://www.ncei.noaa.gov/support/access-data-service-api-user-documentation", TermsStatus: "public domain (US government)",
	TTL: 6 * time.Hour, Price: 1,
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// --- NSIDC Arctic sea-ice extent --------------------------------------------

const (
	nsidcHost = "noaadata.apps.nsidc.org"
	nsidcPath = "/NOAA/G02135/north/daily/data/N_seaice_extent_daily_v4.0.csv"
)

// parseSeaIce reads the G02135 daily-extent CSV (Year, Month, Day, Extent,
// Missing, Source Data; a header and a units row) into a series in million
// km².
func parseSeaIce(raw []byte) (any, error) {
	rd := csv.NewReader(bytes.NewReader(raw))
	rd.FieldsPerRecord, rd.LazyQuotes, rd.TrimLeadingSpace = -1, true, true
	points := map[string]float64{}
	for {
		row, err := rd.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(row) < 4 {
			continue
		}
		y, e1 := strconv.Atoi(strings.TrimSpace(row[0]))
		m, e2 := strconv.Atoi(strings.TrimSpace(row[1]))
		d, e3 := strconv.Atoi(strings.TrimSpace(row[2]))
		v := pyFloat(strings.TrimSpace(row[3]))
		if e1 != nil || e2 != nil || e3 != nil || v == nil || y < 0 || y > 9999 || m < 1 || m > 12 || d < 1 || d > 31 {
			continue
		}
		points[pad4(y)+"-"+pad2(m)+"-"+pad2(d)] = *v
	}
	if len(points) == 0 {
		return nil, errors.New("no rows")
	}
	return seriesFromPoints(points), nil
}

func pad2(n int) string { return pad(n, 2) }
func pad4(n int) string { return pad(n, 4) }
func pad(n, w int) string {
	s := strconv.Itoa(n)
	for len(s) < w {
		s = "0" + s
	}
	return s
}

type seaIceData struct {
	SeriesID          string   `json:"series_id"`
	Units             string   `json:"units"`
	Date              *string  `json:"date"`
	ExtentKM2         *int64   `json:"extent_km2"`
	ExtentMillionKM2  *float64 `json:"extent_million_km2"`
	RankLowToHigh     *int     `json:"rank_low_to_high"`
	NYears            int      `json:"n_years"`
	AnomalyMillionKM2 *float64 `json:"anomaly_million_km2"`
	Observations      []pdObs  `json:"observations"`
	Count             int      `json:"count"`
	Truncated         bool     `json:"truncated"`
	NextEndDate       *string  `json:"next_end_date"`
	Note              string   `json:"note,omitempty"`
}

var dsSeaIce = &pdDataset{
	ID: "sea_ice_extent", SchemaVersion: 1,
	Title:       "Arctic sea-ice extent",
	Description: "NSIDC Sea Ice Index daily Arctic extent: a day's extent, its rank among the same day in every year (1 = record low) and its anomaly against that day's mean; optionally the series over a date range.",
	Output:      "data: series_id, units (million km2), date, extent_km2, extent_million_km2, rank_low_to_high, n_years, anomaly_million_km2, observations[] (newest first: date, value; only with start_date or end_date), count, truncated, next_end_date",
	Params: []pdParam{
		dateParam("date", "the day to rank; default the latest available"),
		dateParam("start_date", "series from this day (inclusive)"),
		dateParam("end_date", "series to this day (inclusive)"),
		limitParam(366, 1200),
	},
	Validate: func(p *pdParams, _ time.Time) bool { return pdDateWindow(p, 0) },
	Run: func(ctx context.Context, r *pdRun, p *pdParams) (any, error) {
		raw, err := r.fetch(ctx, pdFetch{Key: "north_daily_v4", TTL: 12 * time.Hour, Host: nsidcHost, Path: nsidcPath,
			Accept: "text/csv", MaxBytes: 8 << 20, Parse: parseSeaIce})
		if err != nil {
			return nil, err
		}
		s, err := unmarshalCached[pdSeries](raw)
		if err != nil {
			return nil, err
		}
		out := seaIceData{SeriesID: "N_seaice_extent", Units: "million km2", Observations: []pdObs{}}
		if len(s.D) > 0 {
			r.asOf = s.D[len(s.D)-1]
		}
		date := p.Str("date")
		idx := -1
		if date == "" && len(s.D) > 0 {
			idx = len(s.D) - 1
		} else {
			idx = sort.SearchStrings(s.D, date)
			if idx >= len(s.D) || s.D[idx] != date {
				idx = -1
			}
		}
		if idx < 0 {
			out.Note = "no sea-ice observation for " + date
		} else {
			value := s.V[idx]
			mmdd := s.D[idx][5:10]
			var sum float64
			n, below := 0, 0
			for i, d := range s.D {
				if d[5:10] == mmdd {
					sum += s.V[i]
					n++
					if s.V[i] < value {
						below++
					}
				}
			}
			km2 := int64(math.RoundToEven(value * 1e6))
			rank := below + 1
			out.Date, out.ExtentKM2, out.ExtentMillionKM2, out.RankLowToHigh, out.NYears = sptr(s.D[idx]), &km2, fptr(value), &rank, n
			out.AnomalyMillionKM2 = fptr(pyRound(value-sum/float64(n), 3))
		}
		if p.Has("start_date") || p.Has("end_date") {
			out.Observations, out.Count, out.NextEndDate = s.window(p.Str("start_date"), p.Str("end_date"), int(intOr(p, "limit", 366)))
			out.Truncated = out.NextEndDate != nil
		}
		return out, nil
	},
	Source: "National Snow and Ice Data Center (NSIDC), Sea Ice Index v4 (G02135)", Hosts: []string{nsidcHost},
	Licence: "free to use; citation requested", Attribution: "NSIDC Sea Ice Index, Version 4 (G02135), National Snow and Ice Data Center, Boulder, Colorado",
	TermsURL: "https://nsidc.org/data/g02135/versions/4", TermsStatus: "citation requested; verify before production",
	TTL: 12 * time.Hour, Price: 1,
}

// --- US food recalls: FSIS and openFDA --------------------------------------

const (
	fsisHost      = "www.fsis.usda.gov"
	fdaHost       = "api.fda.gov"
	recallTextMax = 220
	fdaLimit      = 25
)

type fsisRecall struct {
	Establishment  string `json:"e"`
	Title          string `json:"t"`
	ProductItems   string `json:"p"`
	RecallDate     string `json:"d"`
	Active         bool   `json:"a"`
	Classification string `json:"c"`
	Reason         string `json:"r"`
}

// streamFSIS reads FSIS's recall list (one JSON array, large) record by
// record, keeping only the fields the dataset uses.
func streamFSIS(r io.Reader) (any, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, errors.New("not an array")
	}
	out := []fsisRecall{}
	for dec.More() {
		var rec struct {
			Establishment  any `json:"field_establishment"`
			Title          any `json:"field_title"`
			ProductItems   any `json:"field_product_items"`
			RecallDate     any `json:"field_recall_date"`
			Active         any `json:"field_active_notice"`
			Classification any `json:"field_recall_classification"`
			Reason         any `json:"field_recall_reason"`
		}
		if err := dec.Decode(&rec); err != nil {
			return nil, err
		}
		out = append(out, fsisRecall{Establishment: pyStr(rec.Establishment), Title: pyStr(rec.Title), ProductItems: pyStr(rec.ProductItems),
			RecallDate: pyStr(rec.RecallDate), Active: strings.ToLower(strings.TrimSpace(pyStr(rec.Active))) == "true",
			Classification: pyStr(rec.Classification), Reason: pyStr(rec.Reason)})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

type fdaRecall struct {
	Firm           *string `json:"firm"`
	Product        string  `json:"product"`
	Reason         string  `json:"reason"`
	Status         string  `json:"status"`
	Classification string  `json:"classification"`
	Date           string  `json:"date"`
}

func parseFDA(raw []byte) (any, error) {
	out := []fdaRecall{}
	if raw == nil {
		return out, nil // openFDA's 404: no match
	}
	var body struct {
		Results []map[string]any `json:"results"`
	}
	if err := decodeJSON(raw, &body); err != nil {
		return nil, err
	}
	for _, r := range body.Results {
		rec := fdaRecall{Product: pyStr(r["product_description"]), Reason: pyStr(r["reason_for_recall"]), Status: pyStr(r["status"]),
			Classification: pyStr(r["classification"]), Date: pyStr(r["recall_initiation_date"])}
		if s, ok := r["recalling_firm"].(string); ok {
			rec.Firm = &s
		}
		out = append(out, rec)
	}
	return out, nil
}

// clip is Pythia's _clip: None when empty, else stripped and cut to limit
// code points with an ellipsis.
func clip(text string, limit int) *string {
	if text == "" {
		return nil
	}
	t := strings.TrimSpace(text)
	if r := []rune(t); len(r) > limit {
		t = string(r[:limit-1]) + "…"
	}
	return &t
}

func isoDate(raw string) *string {
	if raw == "" {
		return nil
	}
	s := strings.TrimSpace(raw)
	if len(s) == 8 && strings.Trim(s, "0123456789") == "" {
		s = s[:4] + "-" + s[4:6] + "-" + s[6:]
	}
	return &s
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type recallRow struct {
	Source         string  `json:"source"`
	Firm           *string `json:"firm"`
	Product        *string `json:"product"`
	Date           *string `json:"date"`
	Status         *string `json:"status"`
	Classification *string `json:"classification"`
	Reason         *string `json:"reason"`
}

type recallsData struct {
	FirmQuery      string      `json:"firm_query"`
	StatusFilter   string      `json:"status_filter"`
	Count          int         `json:"count"`
	TotalMatches   int         `json:"total_matches"`
	Recalls        []recallRow `json:"recalls"`
	SourcesChecked []string    `json:"sources_checked"`
	SourcesFailed  []string    `json:"sources_failed"`
	Note           string      `json:"note,omitempty"`
}

var dsRecalls = &pdDataset{
	ID: "food_recalls", SchemaVersion: 1,
	Title:       "US food recalls",
	Description: "US food recalls for a company: USDA FSIS (meat, poultry, egg products) and FDA openFDA food enforcement, matched by firm name, with status, class, product, reason and date.",
	Output:      "data: firm_query, status_filter, count, total_matches, recalls[] (newest first: source, firm, product, date, status, classification, reason), sources_checked, sources_failed",
	Params: []pdParam{
		{Name: "firm", Kind: "string", Required: true, MaxLen: 100, Pattern: pdWordRE, Norm: pdTrim, Doc: "recalling company name, matched as a substring (FSIS) and a phrase (openFDA)"},
		{Name: "status", Kind: "enum", Enum: []string{"active", "closed"}, Doc: "active (FDA: Ongoing) or closed (Completed, Terminated); default both",
			Norm: func(s string) (string, bool) {
				s = strings.ToLower(strings.TrimSpace(s))
				switch s {
				case "ongoing":
					s = "active"
				case "completed", "terminated":
					s = "closed"
				}
				return s, true
			}},
		limitParam(10, 50),
	},
	Run: func(ctx context.Context, r *pdRun, p *pdParams) (any, error) {
		firm, status := p.Str("firm"), p.Str("status")
		out := recallsData{FirmQuery: firm, StatusFilter: "any", Recalls: []recallRow{}, SourcesChecked: []string{}, SourcesFailed: []string{}}
		if status != "" {
			out.StatusFilter = status
		}
		all := []recallRow{}
		var notes []string
		if raw, err := r.fetch(ctx, pdFetch{Key: "fsis", TTL: time.Hour, Host: fsisHost, Path: "/fsis/api/recall/v/1", MaxBytes: 32 << 20,
			Timeout: 40 * time.Second, Stream: streamFSIS}); err != nil {
			out.SourcesFailed = append(out.SourcesFailed, "FSIS")
			notes = append(notes, "FSIS (USDA meat and poultry) recalls not checked: the endpoint did not answer; that coverage is missing.")
		} else {
			list, err := unmarshalCached[[]fsisRecall](raw)
			if err != nil {
				return nil, err
			}
			out.SourcesChecked = append(out.SourcesChecked, "FSIS")
			lc := strings.ToLower(firm)
			for _, rec := range list {
				if !strings.Contains(strings.ToLower(rec.Establishment+" "+rec.Title), lc) {
					continue
				}
				if (status == "active" && !rec.Active) || (status == "closed" && rec.Active) {
					continue
				}
				row := recallRow{Source: "FSIS (USDA)", Firm: nonEmpty(rec.Establishment), Product: clip(rec.ProductItems, recallTextMax),
					Date: isoDate(rec.RecallDate), Status: sptr("Closed"), Classification: nonEmpty(rec.Classification), Reason: clip(rec.Reason, recallTextMax)}
				if row.Firm == nil {
					row.Firm = clip(rec.Title, 120)
				}
				if row.Product == nil {
					row.Product = clip(rec.Title, recallTextMax)
				}
				if rec.Active {
					row.Status = sptr("Active")
				}
				all = append(all, row)
			}
		}
		if raw, err := r.fetch(ctx, pdFetch{Key: "fda:" + strings.ToLower(firm), TTL: time.Hour, Host: fdaHost, Path: "/food/enforcement.json",
			Query: url.Values{"search": {`recalling_firm:"` + firm + `"`}, "limit": {strconv.Itoa(fdaLimit)}}, NotFound404: true, MaxBytes: 2 << 20, Parse: parseFDA}); err != nil {
			out.SourcesFailed = append(out.SourcesFailed, "openFDA")
			notes = append(notes, "openFDA food-enforcement query failed.")
		} else {
			list, err := unmarshalCached[[]fdaRecall](raw)
			if err != nil {
				return nil, err
			}
			out.SourcesChecked = append(out.SourcesChecked, "openFDA")
			for _, rec := range list {
				st := strings.ToLower(rec.Status)
				if (status == "active" && st != "ongoing") || (status == "closed" && st != "completed" && st != "terminated") {
					continue
				}
				all = append(all, recallRow{Source: "FDA (openFDA food enforcement)", Firm: rec.Firm, Product: clip(rec.Product, recallTextMax),
					Date: isoDate(rec.Date), Status: nonEmpty(rec.Status), Classification: nonEmpty(rec.Classification), Reason: clip(rec.Reason, recallTextMax)})
			}
		}
		if len(out.SourcesChecked) == 0 {
			return nil, pdFail("upstream_busy", "every_source_failed")
		}
		sort.SliceStable(all, func(i, j int) bool { return deref(all[i].Date) > deref(all[j].Date) })
		out.TotalMatches = len(all)
		out.Recalls = all[:min(len(all), int(intOr(p, "limit", 10)))]
		out.Count = len(out.Recalls)
		if len(out.Recalls) > 0 && out.Recalls[0].Date != nil {
			r.asOf = *out.Recalls[0].Date
		}
		if len(all) == 0 {
			notes = append([]string{"no recalls matching this firm in the sources checked; absence means something only for those sources"}, notes...)
		}
		out.Note = strings.Join(notes, " ")
		return out, nil
	},
	Source: "USDA Food Safety and Inspection Service; US Food and Drug Administration (openFDA)", Hosts: []string{fsisHost, fdaHost},
	Licence: "US government works, public domain (openFDA: CC0)", Attribution: "USDA FSIS recall API; openFDA food enforcement reports",
	TermsURL: "https://open.fda.gov/terms/", TermsStatus: "public domain (US government)",
	TTL: time.Hour, Price: 1, Untrusted: true,
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// --- Congress.gov bills ------------------------------------------------------

const congressHost = "api.congress.gov"

var billTypeLabels = map[string]string{
	"HR": "H.R.", "S": "S.", "HRES": "H.Res.", "SRES": "S.Res.", "HJRES": "H.J.Res.", "SJRES": "S.J.Res.",
	"HCONRES": "H.Con.Res.", "SCONRES": "S.Con.Res.",
}

type congressBill struct {
	Type       string `json:"type"`
	Number     string `json:"number"`
	Congress   any    `json:"congress"`
	Title      string `json:"title"`  // Python's str(): "None" for a null
	ActionText string `json:"action"` // likewise
	ActionDate string `json:"action_date"`
	HasText    bool   `json:"has_text"` // latestAction.text was a string
}

// pyGet is Python's d.get(k, "") rendered by an f-string: missing is "",
// null is "None".
func pyGet(m map[string]any, k string) string {
	v, ok := m[k]
	if !ok {
		return ""
	}
	if v == nil {
		return "None"
	}
	return pyStr(v)
}

func parseCongress(raw []byte) (any, error) {
	var body struct {
		Bills []map[string]any `json:"bills"`
	}
	if err := decodeJSON(raw, &body); err != nil {
		return nil, err
	}
	out := []congressBill{}
	for _, b := range body.Bills {
		la, _ := b["latestAction"].(map[string]any)
		bill := congressBill{Type: pyStr(b["type"]), Number: pyStr(b["number"]), Congress: b["congress"], Title: pyGet(b, "title")}
		if la != nil {
			bill.ActionText = pyGet(la, "text")
			_, bill.HasText = la["text"].(string)
			bill.ActionDate = pyStr(la["actionDate"])
		}
		out = append(out, bill)
	}
	return out, nil
}

type billRow struct {
	Bill             string  `json:"bill"`
	Title            string  `json:"title"`
	LatestAction     *string `json:"latest_action"`
	LatestActionDate *string `json:"latest_action_date"`
	Congress         any     `json:"congress"`
}

type billsData struct {
	Query    string    `json:"query"`
	Congress int64     `json:"congress"`
	Count    int       `json:"count"`
	Scanned  int       `json:"scanned"`
	Bills    []billRow `json:"bills"`
	Note     string    `json:"note,omitempty"`
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-3]) + "..."
	}
	return s
}

var dsCongressBills = &pdDataset{
	ID: "congress_bills", SchemaVersion: 1,
	Title:       "US Congress bills",
	Description: "Recently active bills in a Congress whose title or latest action contains every query word (Congress.gov has no full-text search: the 250 most recently updated bills are scanned), with the latest action and its date.",
	Output:      "data: query, congress, count (matches), scanned, bills[] (newest action first: bill, title, latest_action, latest_action_date, congress)",
	Params: []pdParam{
		{Name: "query", Kind: "string", Required: true, MaxLen: 200, Pattern: pdWordRE, Norm: pdTrim, Doc: "words that must all appear in the title or latest action"},
		{Name: "congress", Kind: "int", Min: 1, Max: 999, Default: "the current Congress", Doc: "Congress number, e.g. 119"},
		limitParam(8, 50),
	},
	Validate: func(p *pdParams, today time.Time) bool {
		if !p.Has("congress") {
			p.Set("congress", int64((today.Year()-1789)/2+1))
		}
		return true
	},
	Run: func(ctx context.Context, r *pdRun, p *pdParams) (any, error) {
		congress, _ := p.Int("congress")
		c := strconv.FormatInt(congress, 10)
		raw, err := r.fetch(ctx, pdFetch{Key: "bills:" + c, TTL: time.Hour, Host: congressHost, Path: "/v3/bill/" + c,
			Query: url.Values{"format": {"json"}, "limit": {"250"}, "sort": {"updateDate+desc"}}, KeyHeader: "X-Api-Key", MaxBytes: 4 << 20, Parse: parseCongress})
		if err != nil {
			return nil, err
		}
		bills, err := unmarshalCached[[]congressBill](raw)
		if err != nil {
			return nil, err
		}
		terms := strings.Fields(strings.ToLower(p.Str("query")))
		var matches []congressBill
		for _, b := range bills {
			hay := strings.ToLower(b.Title + " " + b.ActionText)
			all := true
			for _, t := range terms {
				if !strings.Contains(hay, t) {
					all = false
					break
				}
			}
			if all {
				matches = append(matches, b)
			}
		}
		sort.SliceStable(matches, func(i, j int) bool { return matches[i].ActionDate > matches[j].ActionDate })
		out := billsData{Query: p.Str("query"), Congress: congress, Count: len(matches), Scanned: len(bills), Bills: []billRow{}}
		for _, b := range matches[:min(len(matches), int(intOr(p, "limit", 8)))] {
			t := strings.ToUpper(b.Type)
			label := t
			if l, ok := billTypeLabels[t]; ok {
				label = l
			}
			title := b.Title
			if title == "None" {
				title = ""
			}
			row := billRow{Bill: strings.TrimSpace(label + " " + b.Number), Title: truncateRunes(title, 90), Congress: b.Congress, LatestActionDate: nonEmpty(b.ActionDate)}
			if b.HasText {
				row.LatestAction = sptr(b.ActionText)
			}
			out.Bills = append(out.Bills, row)
		}
		if len(out.Bills) > 0 && out.Bills[0].LatestActionDate != nil {
			r.asOf = *out.Bills[0].LatestActionDate
		}
		if len(matches) == 0 {
			out.Note = "no match among the most recently updated bills of this Congress; dormant bills are not visible here"
		}
		return out, nil
	},
	Source: "Library of Congress, Congress.gov API v3", Hosts: []string{congressHost},
	Licence: "US government work, public domain", Attribution: "Congress.gov",
	TermsURL: "https://api.congress.gov/", TermsStatus: "public domain (US government); api.data.gov key terms apply",
	Key: KeyAPIDataGov, TTL: time.Hour, Price: 1, Untrusted: true,
}

// --- FEC candidate totals ----------------------------------------------------

const fecHost = "api.open.fec.gov"

var (
	fecIDRE   = regexp.MustCompile(`^[HSP][0-9A-Z]{8}$`)
	fecNameRE = regexp.MustCompile(`^[\p{L} .,'-]{2,100}$`)
)

type fecCandidate struct {
	ID   string  `json:"id"`
	Name *string `json:"name"`
}

func parseFECSearch(raw []byte) (any, error) {
	var body struct {
		Results []map[string]any `json:"results"`
	}
	if err := decodeJSON(raw, &body); err != nil {
		return nil, err
	}
	out := []fecCandidate{}
	if len(body.Results) > 0 {
		c := body.Results[0]
		id, _ := c["candidate_id"].(string)
		if !fecIDRE.MatchString(id) {
			return nil, errors.New("bad candidate id")
		}
		fc := fecCandidate{ID: id}
		if n, ok := c["name"].(string); ok {
			fc.Name = &n
		}
		out = append(out, fc)
	}
	return out, nil
}

type fecTotals struct {
	Found         bool     `json:"found"`
	Cycle         any      `json:"cycle"`
	Receipts      *float64 `json:"receipts"`
	Disbursements *float64 `json:"disbursements"`
	CashOnHand    *float64 `json:"cash_on_hand"`
	Debts         *float64 `json:"debts"`
}

func parseFECTotals(raw []byte) (any, error) {
	var body struct {
		Results []map[string]any `json:"results"`
	}
	if err := decodeJSON(raw, &body); err != nil {
		return nil, err
	}
	if len(body.Results) == 0 {
		return fecTotals{}, nil
	}
	t := body.Results[0]
	return fecTotals{Found: true, Cycle: t["cycle"], Receipts: pyFloat(t["receipts"]), Disbursements: pyFloat(t["disbursements"]),
		CashOnHand: pyFloat(t["last_cash_on_hand_end_period"]), Debts: pyFloat(t["last_debts_owed_by_committee"])}, nil
}

type fecData struct {
	CandidateName *string  `json:"candidate_name"`
	CandidateID   *string  `json:"candidate_id"`
	Cycle         any      `json:"cycle"`
	Receipts      *float64 `json:"receipts"`
	Disbursements *float64 `json:"disbursements"`
	CashOnHand    *float64 `json:"cash_on_hand"`
	Debts         *float64 `json:"debts"`
	Count         int      `json:"count"`
	Note          string   `json:"note,omitempty"`
}

var dsFECTotals = &pdDataset{
	ID: "fec_candidate_totals", SchemaVersion: 1,
	Title:       "FEC campaign finance totals",
	Description: "A US federal candidate's campaign-finance top line from the FEC: receipts, disbursements, cash on hand and debts, for the latest or a given two-year cycle.",
	Output:      "data: candidate_name, candidate_id, cycle, receipts, disbursements, cash_on_hand, debts (US dollars), count (1 found, 0 not)",
	Params: []pdParam{
		{Name: "candidate", Kind: "string", MaxLen: 100, Pattern: fecNameRE, Norm: pdTrim, Doc: "candidate name (the best FEC match is used); or give candidate_id"},
		{Name: "candidate_id", Kind: "string", Pattern: fecIDRE, Norm: pdUpper, Doc: "FEC candidate id, e.g. H2TX30123"},
		{Name: "cycle", Kind: "int", Min: 1976, Max: 2100, Doc: "even election cycle year; default the latest"},
	},
	Validate: func(p *pdParams, _ time.Time) bool {
		if p.Has("candidate") == p.Has("candidate_id") {
			return false
		}
		c, ok := p.Int("cycle")
		return !ok || c%2 == 0
	},
	Run: func(ctx context.Context, r *pdRun, p *pdParams) (any, error) {
		out := fecData{}
		id := p.Str("candidate_id")
		if id == "" {
			name := p.Str("candidate")
			out.CandidateName = sptr(name)
			raw, err := r.fetch(ctx, pdFetch{Key: "search:" + strings.ToLower(name), TTL: 24 * time.Hour, Host: fecHost, Path: "/v1/candidates/search/",
				Query: url.Values{"q": {name}, "per_page": {"1"}, "sort": {"-election_years"}}, KeyHeader: "X-Api-Key", MaxBytes: 1 << 20, Parse: parseFECSearch})
			if err != nil {
				return nil, err
			}
			found, err := unmarshalCached[[]fecCandidate](raw)
			if err != nil {
				return nil, err
			}
			if len(found) == 0 {
				out.Note = "no FEC candidate match"
				return out, nil
			}
			id, out.CandidateName = found[0].ID, found[0].Name
		}
		out.CandidateID = sptr(id)
		q := url.Values{"per_page": {"1"}, "sort": {"-cycle"}}
		key := "totals:" + id
		if c, ok := p.Int("cycle"); ok {
			q.Set("cycle", strconv.FormatInt(c, 10))
			key += ":" + strconv.FormatInt(c, 10)
		}
		raw, err := r.fetch(ctx, pdFetch{Key: key, TTL: 6 * time.Hour, Host: fecHost, Path: "/v1/candidate/" + id + "/totals/", Query: q,
			KeyHeader: "X-Api-Key", MaxBytes: 1 << 20, Parse: parseFECTotals})
		if err != nil {
			return nil, err
		}
		t, err := unmarshalCached[fecTotals](raw)
		if err != nil {
			return nil, err
		}
		if !t.Found {
			out.Note = "candidate found but no finance totals for that cycle"
			return out, nil
		}
		out.Count, out.Cycle, out.Receipts, out.Disbursements, out.CashOnHand, out.Debts = 1, t.Cycle, t.Receipts, t.Disbursements, t.CashOnHand, t.Debts
		return out, nil
	},
	Source: "US Federal Election Commission, OpenFEC API", Hosts: []string{fecHost},
	Licence: "US government work, public domain", Attribution: "Federal Election Commission (api.open.fec.gov)",
	TermsURL: "https://api.open.fec.gov/developers/", TermsStatus: "public domain (US government); api.data.gov key terms apply",
	Key: KeyAPIDataGov, TTL: 6 * time.Hour, Price: 1,
}

// --- CoinGecko spot price ------------------------------------------------------

const coingeckoHost = "api.coingecko.com"

type cgPrice struct {
	Price     *float64 `json:"price"`
	UpdatedAt *int64   `json:"updated_at"`
}

type cryptoData struct {
	CoinID        string   `json:"coin_id"`
	VsCurrency    string   `json:"vs_currency"`
	Price         *float64 `json:"price"`
	LastUpdatedAt *string  `json:"last_updated_at"`
	Note          string   `json:"note,omitempty"`
}

var dsCryptoPrice = &pdDataset{
	ID: "crypto_spot_price", SchemaVersion: 1,
	Title:       "Crypto spot price",
	Description: "A cryptocurrency's current spot price from CoinGecko's public simple-price endpoint.",
	Output:      "data: coin_id, vs_currency, price, last_updated_at (RFC 3339, CoinGecko's own timestamp)",
	Params: []pdParam{
		{Name: "coin_id", Kind: "string", Required: true, MaxLen: 64, Pattern: regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`), Norm: pdLower, Doc: "CoinGecko coin id, e.g. bitcoin"},
		{Name: "vs_currency", Kind: "string", MaxLen: 8, Pattern: regexp.MustCompile(`^[a-z]{2,8}$`), Norm: pdLower, Default: "usd", Doc: "quote currency"},
	},
	Validate: func(p *pdParams, _ time.Time) bool {
		if !p.Has("vs_currency") {
			p.Set("vs_currency", "usd")
		}
		return true
	},
	Run: func(ctx context.Context, r *pdRun, p *pdParams) (any, error) {
		coin, vs := p.Str("coin_id"), p.Str("vs_currency")
		raw, err := r.fetch(ctx, pdFetch{Key: coin + ":" + vs, TTL: time.Minute, Host: coingeckoHost, Path: "/api/v3/simple/price",
			Query: url.Values{"ids": {coin}, "vs_currencies": {vs}, "include_last_updated_at": {"true"}}, MaxBytes: 64 << 10,
			Parse: func(raw []byte) (any, error) {
				var body map[string]map[string]any
				if err := decodeJSON(raw, &body); err != nil {
					return nil, err
				}
				out := cgPrice{}
				if e := body[coin]; e != nil {
					out.Price = pyFloat(e[vs])
					if u := pyFloat(e["last_updated_at"]); u != nil {
						n := int64(*u)
						out.UpdatedAt = &n
					}
				}
				return out, nil
			}})
		if err != nil {
			return nil, err
		}
		c, err := unmarshalCached[cgPrice](raw)
		if err != nil {
			return nil, err
		}
		out := cryptoData{CoinID: coin, VsCurrency: vs, Price: c.Price}
		if c.UpdatedAt != nil {
			out.LastUpdatedAt = pdTime(*c.UpdatedAt)
			r.asOf = *out.LastUpdatedAt
		}
		if c.Price == nil {
			out.Note = "no price for " + coin + "/" + vs
		}
		return out, nil
	},
	Source: "CoinGecko public API", Hosts: []string{coingeckoHost},
	Licence: "CoinGecko API terms; attribution required", Attribution: "Data provided by CoinGecko",
	TermsURL: "https://www.coingecko.com/en/api_terms", TermsStatus: "verify before production: commercial use may need a paid plan",
	TTL: time.Minute, Price: 1,
}
