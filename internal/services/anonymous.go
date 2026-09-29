package services

// Service calls without a key. A catalogue method marked Anonymous takes an
// unsigned service.call: the caller is the anonymous subject of its network
// prefix ("anon:" + a salted daily hash of the IPv4 /24 or, for credit, the
// IPv6 /48, RFC0012 §6.2), billed in credit from that subject's free daily share (tier 4 of the
// waterfall), under the same max_cost, idempotency and moderation rules as a
// signed call. Everything that says so (/api/services, /capabilities,
// /llms.txt, /for-agents, the hosted MCP tools, the TCP and DNS help, the
// protocol) is generated from the Method fields and the helpers here.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Anonymous call bounds, beyond each method's AnonymousRate.
const (
	// AnonymousHoldsTotal bounds unsigned remote calls running at once, for
	// all anonymous callers together, so they never hold more than a quarter
	// of the board's open-call slots.
	AnonymousHoldsTotal = 16
	// AnonymousRemoteSlots bounds unsigned remote calls talking to an
	// upstream at once (of RemoteSlots), so signed calls always find a slot.
	AnonymousRemoteSlots = 2
	// AnonymousRequestIDMin is the shortest request_id an unsigned call
	// takes: every caller on one network shares its request_id namespace,
	// so a short or shared one could be taken by a neighbour first (security
	// review 1.21, L3).
	AnonymousRequestIDMin = 16
	// AnonymousRequestIDExample is the request_id the examples show: a
	// placeholder, shorter than AnonymousRequestIDMin so it is refused with
	// the rule if pasted as it is, never an id every reader would share.
	AnonymousRequestIDExample = "RANDOM_16_CHARS"
)

// CallPathPrefix is the HTTP route of a service call as one URL:
// /call/SERVICE/METHOD?ARG=VALUE&max_cost=N&request_id=ID. Only unsigned
// calls use it; a signed call is a JSON command.
const CallPathPrefix = "/call/"

// AnonRate bounds a method's unsigned calls in fixed one-minute and UTC-day
// windows: per anonymous caller (one network) and for every anonymous caller
// together. A zero bound is not enforced by the engine (the provider may
// enforce its own, as public_data does per caller).
type AnonRate struct {
	CallerPerMinute int64 `json:"caller_per_minute,omitempty"`
	CallerPerDay    int64 `json:"caller_per_day,omitempty"`
	AllPerMinute    int64 `json:"all_per_minute"`
	AllPerDay       int64 `json:"all_per_day"`
}

// AnonymousChecker is a provider whose unsigned calls take narrower
// arguments than signed ones (inference: one small model, few tokens). It is
// pure and runs before the quote; a refusal spends nothing.
type AnonymousChecker interface {
	CheckAnonymous(c Call) error
}

// NoKey says what an agent without a key can call and how much: the
// "without_key" object of services.list and /capabilities, and the one line
// every discovery surface prints.
type NoKey struct {
	// Available is false while the anonymous tier has no credit (the
	// parameters give it none, the ledger is off, or the signed-services
	// lever is pulled); Line is then empty and nothing advertises it.
	Available bool `json:"available"`
	// CreditsPerDay is one network's share a UTC day (the anonymous credit
	// cap); AllCreditsPerDay is the most every anonymous caller together may
	// spend (the anonymous tier's share of the day's credit budget).
	CreditsPerDay    int64    `json:"credits_per_day"`
	AllCreditsPerDay int64    `json:"all_credits_per_day"`
	Network          string   `json:"network"`
	Methods          []string `json:"methods"`
	Line             string   `json:"line,omitempty"`
	Example          string   `json:"example,omitempty"`
	RequestID        string   `json:"request_id"`
	Wires            []string `json:"wires"`
	Why              string   `json:"why,omitempty"`
}

// NoKeyNetwork says what one anonymous share covers.
const NoKeyNetwork = "one credit share per network: an IPv4 /24 or an IPv6 /48, keyed by a salted hash that changes daily"

// NoKeyRequestID says how an unsigned call is retried.
const NoKeyRequestID = "required, at least 16 characters and random (everyone on your network shares one namespace): an exact retry with the same request_id returns the first answer and is never charged twice; use a new one for a new call"

// NoKeyFor describes the catalogue's anonymous methods at credits a network
// a day and allCredits for every anonymous caller. origin prefixes the
// example URL; why is the reason when nothing is available.
func NoKeyFor(origin string, catalog []Entry, credits, allCredits int64, why string) NoKey {
	n := NoKey{CreditsPerDay: credits, AllCreditsPerDay: allCredits, Network: NoKeyNetwork, Methods: AnonymousMethods(catalog), RequestID: NoKeyRequestID,
		Wires: []string{"HTTP GET or POST " + CallPathPrefix + "SERVICE/METHOD", "POST /v1/command (unsigned service.call)", "hosted MCP tools", "TCP: CALL SERVICE.METHOD ARGS"}}
	n.Available = why == "" && credits > 0 && allCredits > 0 && len(n.Methods) > 0
	if !n.Available {
		n.Why = why
		if n.Why == "" {
			n.Why = "the anonymous tier has no credit today"
		}
		return n
	}
	n.Line = NoKeyLine(catalog, credits)
	n.Example = NoKeyExample(origin, catalog)
	return n
}

// AnonymousMethods is "service.method" for every method callable without a
// key, in catalogue order.
func AnonymousMethods(catalog []Entry) []string {
	out := []string{}
	for _, e := range catalog {
		for _, m := range e.Methods {
			if m.Write() && m.Anonymous {
				out = append(out, e.ID+"."+m.Name)
			}
		}
	}
	return out
}

// NoKeyLine is the one line: "No key needed for public data, small-model
// inference and the notary: 2,000 credits a day per network."
func NoKeyLine(catalog []Entry, credits int64) string {
	var labels []string
	seen := map[string]bool{}
	for _, e := range catalog {
		for _, m := range e.Methods {
			if m.Write() && m.Anonymous && m.anonymousLabel != "" && !seen[m.anonymousLabel] {
				seen[m.anonymousLabel] = true
				labels = append(labels, m.anonymousLabel)
			}
		}
	}
	if len(labels) == 0 {
		return ""
	}
	list := labels[0]
	if len(labels) > 1 {
		list = strings.Join(labels[:len(labels)-1], ", ") + " and " + labels[len(labels)-1]
	}
	return "No key needed for " + list + ": " + Thousands(credits) + " credits a day per network."
}

// NoKeyExample is the example URL: a public_data fetch of sea ice extent
// when that runs, else the first anonymous method's example. Its request_id
// is the placeholder AnonymousRequestIDExample, for the caller to replace
// with a random one.
func NoKeyExample(origin string, catalog []Entry) string {
	var first string
	for _, e := range catalog {
		for _, m := range e.Methods {
			if !m.Write() || !m.Anonymous {
				continue
			}
			path, err := CallPath(e, m, AnonymousRequestIDExample)
			if err != nil {
				continue
			}
			if e.ID == "public_data" && m.Name == "fetch" {
				return origin + path
			}
			if first == "" {
				first = origin + path
			}
		}
	}
	return first
}

// CallPath is m's example as one URL path with its query:
// /call/SERVICE/METHOD?ARG=VALUE&...&max_cost=N&request_id=ID. A string
// argument is its text, a number or boolean its literal, an object or array
// its JSON; an empty object is left out. Argument placeholders are filled
// with their stand-ins; requestID is written as given.
func CallPath(e Entry, m MethodEntry, requestID string) (string, error) {
	var args map[string]json.RawMessage
	if err := json.Unmarshal([]byte(FillPlaceholders(string(m.Example))), &args); err != nil {
		return "", err
	}
	var q []string
	for _, a := range m.Args {
		raw, ok := args[a.Name]
		if !ok {
			continue
		}
		var text string
		switch a.Type {
		case "string":
			if err := json.Unmarshal(raw, &text); err != nil {
				return "", err
			}
		default:
			if string(raw) == "{}" {
				continue
			}
			text = string(raw)
		}
		q = append(q, url.QueryEscape(a.Name)+"="+url.QueryEscape(text))
	}
	if m.Write() {
		q = append(q, "max_cost="+strconv.FormatInt(m.MaxCost(), 10), "request_id="+url.QueryEscape(requestID))
	}
	path := CallPathPrefix + e.ID + "/" + m.Name
	if len(q) > 0 {
		path += "?" + strings.Join(q, "&")
	}
	return path, nil
}

// Call fields a /call/ URL or a TCP CALL line may carry besides the
// method's own arguments.
const (
	CallFieldMaxCost   = "max_cost"
	CallFieldRequestID = "request_id"
)

var callIntegerRE = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,17})$`)

// ErrCallArgs is a /call/ URL or CALL line whose fields do not fit the method.
var ErrCallArgs = errors.New("services: call fields do not fit the method")

// CallData builds a service.call or service.read data field from a /call/
// URL's fields (or a TCP CALL line's), typed by the method's documented
// arguments: a string argument is taken as text, an integer, number or
// boolean must be its JSON literal, an object or array its JSON. max_cost is
// required for a write and refused for a read; request_id is returned apart.
// Unknown fields are refused. The provider's strict parser checks the rest.
func CallData(m MethodEntry, fields url.Values, skip ...string) (data, requestID string, err error) {
	bad := func(format string, a ...any) (string, string, error) {
		return "", "", fmt.Errorf("%w: "+format, append([]any{ErrCallArgs}, a...)...)
	}
	types := map[string]string{}
	for _, a := range m.Args {
		types[a.Name] = a.Type
	}
	args := map[string]json.RawMessage{}
	var maxCost int64 = -1
	for key, values := range fields {
		if len(values) != 1 {
			return bad("%s is given more than once", key)
		}
		v := values[0]
		switch key {
		case CallFieldRequestID:
			requestID = v
			continue
		case CallFieldMaxCost:
			if !m.Write() {
				return bad("max_cost is only for a service.call")
			}
			n, perr := strconv.ParseInt(v, 10, 64)
			if perr != nil || !integerRE.MatchString(v) || n > MaxCostMax {
				return bad("max_cost must be a whole number of %s", m.Resource)
			}
			maxCost = n
			continue
		}
		skipped := false
		for _, s := range skip {
			skipped = skipped || key == s
		}
		if skipped {
			continue
		}
		typ, ok := types[key]
		if !ok {
			return bad("%s is not an argument of %s", key, m.Name)
		}
		var raw json.RawMessage
		switch typ {
		case "string":
			raw, _ = json.Marshal(v)
		case "integer":
			if !callIntegerRE.MatchString(v) {
				return bad("%s must be a whole number", key)
			}
			raw = json.RawMessage(v)
		case "number":
			if _, perr := strconv.ParseFloat(v, 64); perr != nil || !json.Valid([]byte(v)) {
				return bad("%s must be a number", key)
			}
			raw = json.RawMessage(v)
		case "boolean":
			if v != "true" && v != "false" {
				return bad("%s must be true or false", key)
			}
			raw = json.RawMessage(v)
		default: // object, array, any: JSON
			if !json.Valid([]byte(v)) {
				return bad("%s must be JSON", key)
			}
			raw = json.RawMessage(v)
		}
		args[key] = raw
	}
	if m.Write() && maxCost < 0 {
		return bad("max_cost is required: your ceiling, in %s", m.Resource)
	}
	var b strings.Builder
	b.WriteString(`{"schema":1,"method":`)
	name, _ := json.Marshal(m.Name)
	b.Write(name)
	b.WriteString(`,"args":{`)
	i := 0
	for _, a := range m.Args { // catalogue order, so the data is deterministic
		raw, ok := args[a.Name]
		if !ok {
			continue
		}
		if i > 0 {
			b.WriteString(",")
		}
		key, _ := json.Marshal(a.Name)
		b.Write(key)
		b.WriteString(":")
		b.Write(raw)
		i++
	}
	b.WriteString("}")
	if m.Write() {
		fmt.Fprintf(&b, `,"max_cost":%d`, maxCost)
	}
	b.WriteString("}")
	return b.String(), requestID, nil
}

// LookupMethod finds service id's method name in catalog.
func LookupMethod(catalog []Entry, id, name string) (Entry, MethodEntry, bool) {
	for _, e := range catalog {
		if e.ID != id {
			continue
		}
		for _, m := range e.Methods {
			if m.Name == name {
				return e, m, true
			}
		}
	}
	return Entry{}, MethodEntry{}, false
}

// Thousands writes n with comma separators: 2000 → "2,000".
func Thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
