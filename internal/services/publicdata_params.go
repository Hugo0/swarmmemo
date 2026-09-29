package services

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// pdDataset is one catalogue entry. Its parameters are validated by
// parseParams before anything is reserved; Run turns them into the dataset's
// data, reading upstream documents only through pdRun.fetch.
type pdDataset struct {
	ID            string
	Title         string
	Description   string
	SchemaVersion int
	Params        []pdParam
	// Validate checks cross-parameter rules and fills defaults (dates that
	// depend on today); it is pure.
	Validate func(p *pdParams, today time.Time) bool
	Run      func(ctx context.Context, r *pdRun, p *pdParams) (any, error)

	Source      string   // who publishes the data
	Hosts       []string // the only hosts it may reach
	Licence     string
	Attribution string
	TermsURL    string
	TermsStatus string // "public domain (US government)", or "verify before production"
	Key         string // key file name; "" when keyless
	KeyOptional bool   // the key only adds fields
	TTL         time.Duration
	Price       int64 // credit per request
	Untrusted   bool  // carries free text from the source (titles, reasons)
	Output      string
}

// pdParam is one parameter's rule.
type pdParam struct {
	Name     string
	Kind     string // string, date, int, enum
	Required bool
	Default  string // documented default
	Doc      string
	MaxLen   int            // string: bytes (publicDataStringMax when 0)
	Pattern  *regexp.Regexp // string: after Norm
	Enum     []string       // enum: canonical values
	Min, Max int64          // int
	// Norm maps an accepted spelling to its canonical value; ok false
	// refuses it.
	Norm func(string) (string, bool)
}

// pdParams are one request's validated parameters: strings and int64s.
type pdParams struct {
	vals    map[string]any
	derived map[string]string // resolved values that are not parameters (a station alias's id)
}

func (p *pdParams) Str(name string) string {
	s, _ := p.vals[name].(string)
	return s
}

func (p *pdParams) Has(name string) bool {
	_, ok := p.vals[name]
	return ok
}

func (p *pdParams) Int(name string) (int64, bool) {
	n, ok := p.vals[name].(int64)
	return n, ok
}

func (p *pdParams) Set(name string, v any) { p.vals[name] = v }

// echo is the parameters as resolved (defaults filled), for the response.
func (p *pdParams) echo() map[string]any {
	out := make(map[string]any, len(p.vals))
	for k, v := range p.vals {
		out[k] = v
	}
	return out
}

var pdDateRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

// pdDate parses a YYYY-MM-DD date between 1800 and 2200.
func pdDate(s string) (time.Time, bool) {
	if !pdDateRE.MatchString(s) {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil || t.Year() < 1800 || t.Year() > 2200 {
		return time.Time{}, false
	}
	return t, true
}

// pdText is printable text: no control characters, not only spaces.
func pdText(s string) bool {
	if strings.TrimSpace(s) == "" || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029 {
			return false
		}
	}
	return true
}

// parseParams validates a request's params object strictly: only the
// dataset's parameters, each at most once, of its kind and within its rule,
// every required one present; then the dataset's own Validate.
func (ds *pdDataset) parseParams(raw json.RawMessage, now int64) (*pdParams, error) {
	p := &pdParams{vals: map[string]any{}, derived: map[string]string{}}
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	var obj map[string]json.RawMessage
	if err := StrictObject(raw, &obj); err != nil || obj == nil {
		return nil, refusal("invalid_service_data")
	}
	for name, v := range obj {
		spec := ds.param(name)
		if spec == nil {
			return nil, refusal("invalid_service_data")
		}
		val, ok := spec.parse(v)
		if !ok {
			return nil, refusal("invalid_service_data")
		}
		p.vals[name] = val
	}
	for _, spec := range ds.Params {
		if spec.Required && !p.Has(spec.Name) {
			return nil, refusal("invalid_service_data")
		}
	}
	if ds.Validate != nil && !ds.Validate(p, time.Unix(now, 0).UTC().Truncate(24*time.Hour)) {
		return nil, refusal("invalid_service_data")
	}
	return p, nil
}

func (ds *pdDataset) param(name string) *pdParam {
	for i := range ds.Params {
		if ds.Params[i].Name == name {
			return &ds.Params[i]
		}
	}
	return nil
}

func (spec *pdParam) parse(raw json.RawMessage) (any, bool) {
	if spec.Kind == "int" {
		n, ok := Integer(raw, spec.Max)
		return n, ok && n >= spec.Min
	}
	if len(raw) == 0 || raw[0] != '"' {
		return nil, false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return nil, false
	}
	max := spec.MaxLen
	if max == 0 {
		max = publicDataStringMax
	}
	if len(s) > max || !pdText(s) {
		return nil, false
	}
	if spec.Norm != nil {
		var ok bool
		if s, ok = spec.Norm(s); !ok {
			return nil, false
		}
	}
	switch spec.Kind {
	case "date":
		_, ok := pdDate(s)
		return s, ok
	case "enum":
		for _, e := range spec.Enum {
			if s == e {
				return s, true
			}
		}
		return nil, false
	case "string":
		if spec.Pattern != nil && !spec.Pattern.MatchString(s) {
			return nil, false
		}
		return s, true
	}
	return nil, false
}

// describe is the dataset's catalogue entry.
func (ds *pdDataset) describe(available bool) map[string]any {
	params := make([]any, 0, len(ds.Params))
	for _, sp := range ds.Params {
		e := map[string]any{"name": sp.Name, "type": sp.Kind, "required": sp.Required, "doc": sp.Doc}
		if sp.Default != "" {
			e["default"] = sp.Default
		}
		switch sp.Kind {
		case "int":
			e["min"], e["max"] = sp.Min, sp.Max
		case "enum":
			e["enum"] = sp.Enum
		case "date":
			e["format"] = "YYYY-MM-DD"
		case "string":
			if sp.Pattern != nil {
				e["pattern"] = sp.Pattern.String()
			}
			max := sp.MaxLen
			if max == 0 {
				max = publicDataStringMax
			}
			e["max_bytes"] = max
		}
		params = append(params, e)
	}
	key := any(nil)
	if ds.Key != "" {
		key = map[string]any{"file": ds.Key, "optional": ds.KeyOptional}
	}
	return map[string]any{
		"id": ds.ID, "title": ds.Title, "description": ds.Description, "schema_version": ds.SchemaVersion,
		"params": params, "output": ds.Output,
		"source": map[string]any{"publisher": ds.Source, "hosts": ds.Hosts, "licence": ds.Licence, "attribution": ds.Attribution,
			"terms_url": ds.TermsURL, "terms_status": ds.TermsStatus},
		"cache_ttl_seconds": int64(ds.TTL / time.Second), "price": ds.Price, "resource": "credit",
		"key": key, "available": available, "text_is_untrusted": ds.Untrusted,
	}
}

// pdDateWindow validates start_date/end_date: both optional, start <= end,
// and a span of at most maxDays when maxDays > 0.
func pdDateWindow(p *pdParams, maxDays int) bool {
	s, e := p.Str("start_date"), p.Str("end_date")
	if s != "" && e != "" {
		st, _ := pdDate(s)
		et, _ := pdDate(e)
		if st.After(et) {
			return false
		}
		if maxDays > 0 && et.Sub(st) > time.Duration(maxDays)*24*time.Hour {
			return false
		}
	}
	return true
}

// pdLower trims and lower-cases.
func pdLower(s string) (string, bool) { return strings.ToLower(strings.TrimSpace(s)), true }

// pdUpper trims and upper-cases.
func pdUpper(s string) (string, bool) { return strings.ToUpper(strings.TrimSpace(s)), true }

// pdTrim trims surrounding spaces.
func pdTrim(s string) (string, bool) { return strings.TrimSpace(s), true }
