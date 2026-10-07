package services

import (
	"encoding/json"
	"sort"
	"strings"
)

// ParamsNamespace is the parameter namespace that holds service prices
// (RFC0012 §2.7, §3.2). Version 0 is DefaultParamsBody: each method's
// compiled-in Price.
const ParamsNamespace = "services"

// PriceMax bounds each price component, so Price.For cannot overflow for any
// size the engine admits.
const PriceMax = 1 << 32

// Price is a method's price in its resource:
// Base + PerByte × size + PerKiB × ceil(size / 1024), where size is what the
// provider measures (memory: key and value bytes; echo: args bytes).
type Price struct {
	Base    int64 `json:"base"`
	PerByte int64 `json:"per_byte"`
	PerKiB  int64 `json:"per_kib"`
}

// For is the price of a call of size bytes (size is bounded by the caller).
func (p Price) For(size int64) int64 {
	if size < 0 {
		size = 0
	}
	return p.Base + p.PerByte*size + p.PerKiB*((size+1023)/1024)
}

func (p Price) valid() bool {
	return p.Base >= 0 && p.Base <= PriceMax && p.PerByte >= 0 && p.PerByte <= PriceMax && p.PerKiB >= 0 && p.PerKiB <= PriceMax
}

// Prices maps "service.method" to its price.
type Prices map[string]Price

// of is a write method's price: the parameter's, else the method's own.
func (p Prices) of(service string, m Method) Price {
	if price, ok := p[service+"."+m.Name]; ok {
		return price
	}
	return m.Price
}

// DefaultPrices is parameter version 0: every built-in write method's
// compiled-in price. tools.call has none of its own: it routes to a method
// that does.
func DefaultPrices() Prices {
	out := Prices{}
	for _, build := range builtins {
		d := build(Deps{}).Describe()
		if d.ID == ToolsID {
			continue
		}
		for _, m := range d.Methods {
			if m.Write {
				out[d.ID+"."+m.Name] = m.Price
			}
		}
	}
	return out
}

type paramsBody struct {
	Schema json.RawMessage  `json:"schema"`
	Prices map[string]Price `json:"prices"`
}

// DefaultParamsBody is the canonical JSON of parameter version 0.
func DefaultParamsBody() []byte {
	prices := DefaultPrices()
	keys := make([]string, 0, len(prices))
	for k := range prices {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(`{"schema":1,"prices":{`)
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		v, _ := json.Marshal(prices[k])
		b.WriteString(`"` + k + `":`)
		b.Write(v)
	}
	b.WriteString(`}}`)
	return []byte(b.String())
}

// ParseParams validates a "services" parameter body strictly: schema 1,
// only built-in write methods, every component a whole number in
// [0, PriceMax]. Methods it omits keep their compiled-in price.
func ParseParams(body []byte) (Prices, error) {
	var p paramsBody
	if err := StrictObject(body, &p); err != nil {
		return nil, err
	}
	if string(p.Schema) != "1" || p.Prices == nil {
		return nil, refusal("invalid_service_data")
	}
	out := DefaultPrices()
	for k, v := range p.Prices {
		if _, known := out[k]; !known || !v.valid() {
			return nil, refusal("invalid_service_data")
		}
		out[k] = v
	}
	return out, nil
}
