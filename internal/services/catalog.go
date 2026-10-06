package services

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The catalogue is the one description of every enabled service: what it does
// in a line, each method with its arguments, price, resource and a working
// example, and the limits it enforces. services.list returns it with the
// current prices, and every discovery surface (/capabilities, /llms.txt,
// /for-agents, the hosted MCP tools, /openapi.json, the transports' help and
// docs/PROTOCOL.md) is generated from it, so a new provider documents itself
// everywhere by filling in its Descriptor.

// Arg is one argument of a method as the catalogue documents it. The names
// are the JSON fields the provider's own parser accepts; a test holds them
// equal.
type Arg struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // a JSON type: string, integer, number, boolean, object, array
	Required bool   `json:"required,omitempty"`
	Note     string `json:"note"`
}

// Limit is a bound a service enforces, published in its catalogue entry.
type Limit struct {
	Key   string `json:"key"`
	Value int64  `json:"value"`
	Unit  string `json:"unit,omitempty"` // bytes, seconds or empty for a count
	Note  string `json:"note"`
}

// SizeText and durationText state a constant as Limit.Text does, so copy
// built from a constant reads like the published limit and cannot drift.
func SizeText(n int64) string { return Limit{Value: n, Unit: "bytes"}.Text() }

func durationText(seconds int64) string { return Limit{Value: seconds, Unit: "seconds"}.Text() }

// Text is the limit as a reader should see it: "64 KiB", "30 days", "16".
func (l Limit) Text() string {
	v := l.Value
	plural := func(n int64, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	switch l.Unit {
	case "bytes":
		switch {
		case v >= 1<<20 && v%(1<<20) == 0:
			return fmt.Sprintf("%d MiB", v>>20)
		case v >= 1<<10 && v%(1<<10) == 0:
			return fmt.Sprintf("%d KiB", v>>10)
		}
		return plural(v, "byte")
	case "seconds":
		switch {
		case v >= 86400 && v%86400 == 0:
			return plural(v/86400, "day")
		case v >= 3600 && v%3600 == 0:
			return plural(v/3600, "hour")
		case v >= 60 && v%60 == 0:
			return plural(v/60, "minute")
		}
		return plural(v, "second")
	}
	return fmt.Sprintf("%d", v)
}

// Entry is one enabled service in the catalogue.
type Entry struct {
	ID      string        `json:"id"`
	Title   string        `json:"title"`
	Line    string        `json:"line"`
	Summary string        `json:"summary"`
	Mode    string        `json:"mode"`
	Methods []MethodEntry `json:"methods"`
	Limits  []Limit       `json:"limits"`
	// Docs is the service's section of the protocol.
	Docs string `json:"docs"`
	// Topic groups the service under a heading of "What SwarmMemo gives
	// agents"; empty keeps it out of that list (a test service).
	Topic string `json:"-"`
	// Extra is the provider's CatalogueExtra (availability, models), merged
	// into the JSON object without replacing a field above.
	Extra map[string]any `json:"-"`
}

// MethodEntry is one method of an Entry.
type MethodEntry struct {
	Name      string `json:"name"`
	Operation string `json:"operation"` // service.call or service.read
	Signed    bool   `json:"signed"`
	ArgsMax   int    `json:"args_max"`
	Resource  string `json:"resource,omitempty"`
	// Price is the current Price of a write, "free" for a read.
	Price     any    `json:"price"`
	PriceNote string `json:"price_note,omitempty"`
	Line      string `json:"line"`
	Args      []Arg  `json:"args"`
	// Example is a valid args object for this method.
	Example json.RawMessage `json:"example"`
	// Anonymous is true for a write callable without a key (anonymous.go);
	// AnonymousNote and AnonymousRate are its extra bounds when unsigned.
	Anonymous     bool      `json:"anonymous"`
	AnonymousNote string    `json:"anonymous_note,omitempty"`
	AnonymousRate *AnonRate `json:"anonymous_rate,omitempty"`
	// exampleMaxCost overrides the max_cost the examples send.
	exampleMaxCost int64
	// anonymousLabel names the service in the "no key needed" line.
	anonymousLabel string
}

// MarshalJSON flattens Extra into the entry.
func (e Entry) MarshalJSON() ([]byte, error) {
	type plain Entry
	raw, err := json.Marshal(plain(e))
	if err != nil || len(e.Extra) == 0 {
		return raw, err
	}
	var m map[string]json.RawMessage
	if err = json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	for k, v := range e.Extra {
		if _, taken := m[k]; taken {
			continue
		}
		if m[k], err = json.Marshal(v); err != nil {
			return nil, err
		}
	}
	return json.Marshal(m)
}

// Data is the method's data field with its example args: the envelope a
// service.call (with maxCost) or service.read carries.
func (m MethodEntry) Data(maxCost int64) string {
	args := m.Example
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	var b strings.Builder
	b.WriteString(`{"schema":1,"method":`)
	name, _ := json.Marshal(m.Name)
	b.Write(name)
	b.WriteString(`,"args":`)
	b.Write(args)
	if m.Operation == "service.call" {
		fmt.Fprintf(&b, `,"max_cost":%d`, maxCost)
	}
	b.WriteString("}")
	return b.String()
}

// Access is how the method is called: "service.call, signed", "service.call,
// signed or no key",
// "service.read, public" or "service.read, signed, your own".
func (m MethodEntry) Access() string {
	switch {
	case m.Operation == "service.call" && m.Anonymous:
		return "service.call, signed or no key"
	case m.Operation == "service.call":
		return "service.call, signed"
	case m.Signed:
		return "service.read, signed, your own"
	}
	return "service.read, public"
}

// Write reports whether the method is a service.call.
func (m MethodEntry) Write() bool { return m.Operation == "service.call" }

// PriceText is the price in words: "free", "1 credit", "256 + 1 per byte
// memory_bytes", or the provider's note when the price depends on the call.
func (m MethodEntry) PriceText() string {
	p, ok := m.Price.(Price)
	if !ok {
		return "free"
	}
	if m.PriceNote != "" {
		return m.PriceNote
	}
	if p == (Price{}) {
		return "free"
	}
	return p.Words() + " " + m.Resource
}

// Words is a price without its resource: "5", "1 + 1 per KiB".
func (p Price) Words() string {
	parts := []string{}
	if p.Base > 0 || (p.PerByte == 0 && p.PerKiB == 0) {
		parts = append(parts, fmt.Sprintf("%d", p.Base))
	}
	if p.PerByte > 0 {
		parts = append(parts, fmt.Sprintf("%d per byte", p.PerByte))
	}
	if p.PerKiB > 0 {
		parts = append(parts, fmt.Sprintf("%d per KiB", p.PerKiB))
	}
	return strings.Join(parts, " + ")
}

// MaxCost is the max_cost the examples send: enough for the example at the
// method's price, or the provider's own figure when the price depends on
// more than the arguments. A ceiling, never a quote.
func (m MethodEntry) MaxCost() int64 {
	if m.exampleMaxCost > 0 {
		return m.exampleMaxCost
	}
	p, _ := m.Price.(Price)
	return max(p.For(int64(len(m.Example))), 1)
}

// Primary is the method an example shows first: the first write, else the
// first read.
func (e Entry) Primary() MethodEntry {
	for _, m := range e.Methods {
		if m.Write() {
			return m
		}
	}
	return e.Methods[0]
}

// PublicRead is the first read anyone may make unsigned, if there is one.
func (e Entry) PublicRead() (MethodEntry, bool) {
	for _, m := range e.Methods {
		if !m.Write() && !m.Signed {
			return m, true
		}
	}
	return MethodEntry{}, false
}

// Catalog is the static catalogue of the named services at the compiled-in
// prices (parameter version 0), in catalogue order, without the providers'
// live extras: what documentation built without a database states. The live
// catalogue, with current prices, is services.list (Engine.Catalogue).
func Catalog(enabled []string) []Entry {
	return NewBuiltinRegistry(enabled, Deps{}).catalog(DefaultPrices(), false)
}

// catalog is the registry's enabled providers as catalogue entries.
func (r *Registry) catalog(prices Prices, extras bool) []Entry {
	out := []Entry{}
	for _, d := range r.List() {
		e := Entry{ID: d.ID, Title: d.Title, Line: d.Line, Summary: d.Summary, Mode: d.Mode.String(), Topic: d.Topic, Limits: d.Limits, Docs: "/protocol.md#" + d.Anchor(), Methods: []MethodEntry{}}
		if e.Limits == nil {
			e.Limits = []Limit{}
		}
		for _, m := range d.Methods {
			me := MethodEntry{Name: m.Name, Operation: "service.read", Signed: m.Signed, ArgsMax: m.ArgsMax, Price: "free", Line: m.Line, Args: m.Args, Example: m.Example}
			if m.Write {
				me.Operation, me.Resource, me.Price, me.PriceNote, me.exampleMaxCost = "service.call", string(m.Resource), prices.of(d.ID, m), m.PriceNote, m.ExampleMaxCost
				if m.Anonymous {
					rate := m.AnonymousRate
					me.Anonymous, me.AnonymousNote, me.AnonymousRate, me.anonymousLabel = true, m.AnonymousNote, &rate, m.AnonymousLabel
				}
			}
			if me.Args == nil {
				me.Args = []Arg{}
			}
			if len(me.Example) == 0 {
				me.Example = json.RawMessage("{}")
			}
			e.Methods = append(e.Methods, me)
		}
		if c, ok := r.providers[d.ID].(Cataloguer); ok && extras {
			e.Extra = c.CatalogueExtra()
		}
		out = append(out, e)
	}
	return out
}

// Anchor is the service's heading anchor in docs/PROTOCOL.md: its title,
// lowercased, spaces as hyphens.
func (d Descriptor) Anchor() string {
	return strings.ReplaceAll(strings.ToLower(d.Title), " ", "-")
}

// Placeholders are the values an example leaves for the caller to fill in,
// each with a well-formed stand-in the example tests substitute before
// parsing.
var Placeholders = map[string]string{
	"AGENT_FINGERPRINT": strings.Repeat("ab", 32),
	"SHA256_HEX":        "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	"RUN_ID":            strings.Repeat("0f", 16),
	"MODEL_ALIAS":       "small",
	"RESOURCE_ID":       "search",
	"RECEIVER_ID":       strings.Repeat("0e", 16),
	"PASTE_ID":          strings.Repeat("0d", 16),
	"DOC_ID":            strings.Repeat("0c", 16),
}

// FillPlaceholders replaces every placeholder in s with its stand-in.
func FillPlaceholders(s string) string {
	for k, v := range Placeholders {
		s = strings.ReplaceAll(s, k, v)
	}
	return s
}
