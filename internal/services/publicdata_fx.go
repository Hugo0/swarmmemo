package services

import (
	"context"
	"encoding/xml"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// --- ECB euro foreign-exchange reference rates (C876) ------------------------

const (
	ecbFXDailyPath = "/stats/eurofxref/eurofxref-daily.xml"
	ecbFXHistPath  = "/stats/eurofxref/eurofxref-hist-90d.xml"
	// fxHistDays is how far back date may reach: the ECB's 90-day file.
	fxHistDays = 90
	// fxStaleDays: a rate_date more than this many days before the day asked
	// for is flagged rate_may_be_stale (a long weekend is at most 4).
	fxStaleDays = 4
)

// fxCurrencies are the currencies the ECB publishes reference rates for,
// and EUR itself (BGN ended when Bulgaria joined the euro on 2026-01-01).
var fxCurrencies = []string{"EUR", "AUD", "BRL", "CAD", "CHF", "CNY", "CZK", "DKK", "GBP", "HKD", "HUF", "IDR", "ILS", "INR", "ISK",
	"JPY", "KRW", "MXN", "MYR", "NOK", "NZD", "PHP", "PLN", "RON", "SEK", "SGD", "THB", "TRY", "USD", "ZAR"}

var fxCurrencyDoc = strings.Join(fxCurrencies, ", ")

// fxDay is one day of the ECB file: units of each currency per 1 EUR.
type fxDay struct {
	Date  string             `json:"date"`
	Rates map[string]float64 `json:"rates"`
}

type fxRow struct {
	Currency string  `json:"currency"`
	Rate     float64 `json:"rate"`
	RateDate string  `json:"rate_date"`
	Derived  bool    `json:"derived"`
}

type fxData struct {
	Base            string   `json:"base"`
	RateDate        *string  `json:"rate_date"`
	RequestedDate   string   `json:"requested_date,omitempty"`
	DateNote        string   `json:"date_note,omitempty"`
	Count           int      `json:"count"`
	Rates           []fxRow  `json:"rates"`
	RateMayBeStale  bool     `json:"rate_may_be_stale"`
	RateStaleReason string   `json:"rate_stale_reason,omitempty"`
	Missing         []string `json:"missing,omitempty"`
	Note            string   `json:"note,omitempty"`
}

var dsFXRates = &pdDataset{
	ID: "fx_rates", SchemaVersion: 1,
	Title:       "Currency exchange rates (ECB reference rates)",
	Description: "The European Central Bank's euro foreign-exchange reference rates (published each TARGET business day around 16:00 CET), for the latest day or a date in the last 90 days. A non-EUR base is a cross rate computed from the two EUR legs and marked derived.",
	Output:      "data: base, rate_date, requested_date, date_note (when date had no rates and an earlier day answers), count, rates[] (currency, rate = units of currency per 1 base, rate_date, derived), rate_may_be_stale (rate_date more than 4 days before today or date), rate_stale_reason, missing (asked currencies without a rate that day), note",
	Params: []pdParam{
		{Name: "base", Kind: "string", MaxLen: 8, Pattern: fxCodeRE, Norm: pdUpper, Default: "EUR", Doc: "ISO 4217 code, one of " + fxCurrencyDoc},
		{Name: "symbols", Kind: "string", MaxLen: 4 * 32, Pattern: fxSymbolsRE, Norm: fxNormSymbols,
			Doc: "comma-separated ISO 4217 codes to return, each at most once, from " + fxCurrencyDoc + "; all when unset"},
		dateParam("date", "YYYY-MM-DD within the last 90 days; a day without rates (weekend, TARGET holiday) answers the latest earlier day and says so in date_note; the latest day when unset"),
	},
	Validate: func(p *pdParams, today time.Time) bool {
		if !p.Has("base") {
			p.Set("base", "EUR")
		}
		if !contains(fxCurrencies, p.Str("base")) {
			return p.reject("base")
		}
		if s := p.Str("symbols"); s != "" {
			seen := map[string]bool{}
			for _, c := range strings.Split(s, ",") {
				if seen[c] || !contains(fxCurrencies, c) {
					return p.reject("symbols")
				}
				seen[c] = true
			}
		}
		if d, ok := pdDate(p.Str("date")); ok && (d.After(today) || today.Sub(d) > fxHistDays*24*time.Hour) {
			return p.reject("date")
		}
		return true
	},
	Run: func(ctx context.Context, r *pdRun, p *pdParams) (any, error) {
		f := pdFetch{Key: "daily", TTL: time.Hour, Host: ecbHost, Path: ecbFXDailyPath, Accept: "application/xml", MaxBytes: 64 << 10, Parse: parseECBFX}
		if p.Has("date") {
			f.Key, f.Path, f.MaxBytes = "hist90d", ecbFXHistPath, 1<<20
		}
		raw, err := r.fetch(ctx, f)
		if err != nil {
			return nil, err
		}
		days, err := unmarshalCached[[]fxDay](raw)
		if err != nil {
			return nil, err
		}
		var symbols []string
		if s := p.Str("symbols"); s != "" {
			symbols = strings.Split(s, ",")
		}
		out := fxAnswer(days, p.Str("base"), symbols, p.Str("date"), r.today())
		if out.RateDate != nil {
			r.asOf = *out.RateDate
		}
		return out, nil
	},
	Source: "European Central Bank, euro foreign exchange reference rates", Hosts: []string{ecbHost},
	Licence: "ECB statistics: free reuse with the source acknowledged", Attribution: "Source: European Central Bank (ECB euro foreign exchange reference rates)",
	TermsURL: "https://www.ecb.europa.eu/stats/exchange/eurofxref/html/index.en.html", TermsStatus: "free reuse with attribution; reference rates are for information only, not for transactions",
	TTL: time.Hour, Price: 1,
}

var (
	fxCodeRE    = regexp.MustCompile(`^[A-Z]{3}$`)
	fxSymbolsRE = regexp.MustCompile(`^[A-Z]{3}(,[A-Z]{3}){0,31}$`)
)

// fxNormSymbols upper-cases a symbols list and drops spaces around codes.
func fxNormSymbols(s string) (string, bool) {
	parts := strings.Split(strings.ToUpper(s), ",")
	for i, c := range parts {
		parts[i] = strings.TrimSpace(c)
	}
	return strings.Join(parts, ","), true
}

// parseECBFX reads the ECB's eurofxref XML into days, newest first.
func parseECBFX(raw []byte) (any, error) {
	var doc struct {
		Cube struct {
			Days []struct {
				Time  string `xml:"time,attr"`
				Rates []struct {
					Currency string `xml:"currency,attr"`
					Rate     string `xml:"rate,attr"`
				} `xml:"Cube"`
			} `xml:"Cube"`
		} `xml:"Cube"`
	}
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	days := make([]fxDay, 0, len(doc.Cube.Days))
	for _, d := range doc.Cube.Days {
		if _, ok := pdDate(d.Time); !ok {
			return nil, errors.New("ECB reference rates: unrecognised day")
		}
		day := fxDay{Date: d.Time, Rates: map[string]float64{}}
		for _, c := range d.Rates {
			v, err := strconv.ParseFloat(c.Rate, 64)
			if err != nil || v <= 0 || !fxCodeRE.MatchString(c.Currency) {
				return nil, errors.New("ECB reference rates: unrecognised rate")
			}
			day.Rates[c.Currency] = v
		}
		days = append(days, day)
	}
	if len(days) == 0 {
		return nil, errors.New("ECB reference rates: no days")
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Date > days[j].Date })
	return days, nil
}

// fxAnswer picks the day (date, or the latest earlier one; the newest when
// date is unset) and quotes symbols per 1 base. EUR-base rates are the ECB's
// own; any other base is a cross rate from the EUR legs, marked derived.
func fxAnswer(days []fxDay, base string, symbols []string, date string, today time.Time) fxData {
	out := fxData{Base: base, RequestedDate: date, Rates: []fxRow{}}
	var day *fxDay
	for i := range days {
		if date == "" || days[i].Date <= date {
			day = &days[i]
			break
		}
	}
	if day == nil {
		out.Note = "the ECB's 90-day file has no reference rates on or before " + date
		return out
	}
	out.RateDate = sptr(day.Date)
	if date != "" && day.Date != date {
		out.DateNote = date + " has no ECB reference rates (weekend or TARGET holiday); answering the latest earlier day, " + day.Date
	}
	ref := today
	if t, ok := pdDate(date); ok {
		ref = t
	}
	if rd, _ := pdDate(day.Date); ref.Sub(rd) > fxStaleDays*24*time.Hour {
		out.RateMayBeStale = true
		out.RateStaleReason = "rate_date " + day.Date + " is more than " + strconv.Itoa(fxStaleDays) + " days before " + ref.Format("2006-01-02") +
			"; the ECB has published nothing newer in this copy"
	}
	legs := map[string]float64{"EUR": 1}
	for c, v := range day.Rates {
		legs[c] = v
	}
	per, ok := legs[base]
	if !ok {
		out.Note = "the ECB published no " + base + " reference rate on " + day.Date + ", so no rates against it"
		return out
	}
	if symbols == nil {
		for c := range legs {
			symbols = append(symbols, c)
		}
		sort.Strings(symbols)
	}
	for _, c := range symbols {
		if c == base {
			continue
		}
		v, ok := legs[c]
		if !ok {
			out.Missing = append(out.Missing, c)
			continue
		}
		row := fxRow{Currency: c, Rate: v, RateDate: day.Date}
		if base != "EUR" {
			row.Rate, row.Derived = fxSig(v/per), true
		}
		out.Rates = append(out.Rates, row)
	}
	out.Count = len(out.Rates)
	if len(out.Missing) > 0 {
		out.Note = "the ECB published no reference rate on " + day.Date + " for " + strings.Join(out.Missing, ", ")
	}
	return out
}

// fxSig rounds a derived cross rate to 6 significant digits, the ECB's own
// precision at most.
func fxSig(x float64) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'g', 6, 64), 64)
	return v
}
