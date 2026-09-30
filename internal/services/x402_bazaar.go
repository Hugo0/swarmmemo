package services

// Reading an x402 Bazaar discovery response (GET /discovery/resources, on
// the CDP facilitator and others: {"items":[…],"pagination":{…}}). Two
// readers share it: ImportX402Bazaar, which prints allowlist candidates the
// operator reviews and pins by hand, and the open catalogue's import
// (x402_catalogue.go), which admits what passes its guardrails. Bazaar text
// is untrusted data: descriptions are cleaned, truncated, marked and never
// followed; an item that does not parse is skipped, never an error.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// X402Candidate is one allowlist entry in the allowlist file's own format.
type X402Candidate struct {
	ID       string   `json:"id"`
	URL      string   `json:"url"`
	Method   string   `json:"method"`
	PayTo    string   `json:"pay_to"`
	MaxPrice string   `json:"max_price"`
	Query    []string `json:"query,omitempty"`
	Body     bool     `json:"body,omitempty"`
	Summary  string   `json:"summary"`
	Source   string   `json:"source"`
}

// bazaarItem is one discovery item: the fields of the x402 v2 Bazaar, and
// the extras facilitators add (CDP's quality and curated, PayAI's method and
// metadata).
type bazaarItem struct {
	Resource    string            `json:"resource"`
	Type        string            `json:"type"`
	X402Version json.Number       `json:"x402Version"`
	Accepts     []json.RawMessage `json:"accepts"`
	Description string            `json:"description"`
	Method      string            `json:"method"`
	Metadata    struct {
		Description string `json:"description"`
		Category    string `json:"category"`
	} `json:"metadata"`
	Quality struct {
		Calls  json.Number `json:"l30DaysTotalCalls"`
		Payers json.Number `json:"l30DaysUniquePayers"`
	} `json:"quality"`
	Curated    json.RawMessage `json:"curated"`
	Extensions struct {
		Bazaar struct {
			Info struct {
				Input struct {
					Method      string                     `json:"method"`
					QueryParams map[string]json.RawMessage `json:"queryParams"`
					BodyType    string                     `json:"bodyType"`
				} `json:"input"`
			} `json:"info"`
		} `json:"bazaar"`
	} `json:"extensions"`
}

// bazaarSignal is a discovery item's popularity: CDP's last-30-days unique
// payers and calls, and whether it is curated. Zero when not published.
type bazaarSignal struct {
	Payers, Calls int64
	Curated       bool
	Known         bool // the facilitator published quality for the item
}

// bazaarPage is one discovery response: its items, each still raw, and its
// pagination total (0 when absent).
type bazaarPage struct {
	Items []json.RawMessage
	Total int64
}

// bazaarPageBytesMax bounds one discovery response we read, and
// bazaarItemBytesMax one item in it.
const (
	bazaarPageBytesMax = 8 << 20
	bazaarItemBytesMax = 256 << 10
)

func parseBazaarPage(raw []byte) (bazaarPage, error) {
	var w struct {
		Items      []json.RawMessage `json:"items"`
		Pagination struct {
			Total json.Number `json:"total"`
		} `json:"pagination"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if len(raw) > bazaarPageBytesMax || !utf8.Valid(raw) || dec.Decode(&w) != nil || w.Items == nil {
		return bazaarPage{}, errors.New("x402: not a Bazaar discovery response (JSON with items)")
	}
	total, _ := strconv.ParseInt(w.Pagination.Total.String(), 10, 64)
	return bazaarPage{Items: w.Items, Total: max(total, 0)}, nil
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

// bazaarResource reads one discovery item as a resource we can pay: HTTP,
// HTTPS on 443 to a public DNS name, an exact EIP-3009 option on cfg's
// network and asset to a non-zero recipient for at most maxAmount, GET or
// POST. The ID is the host and path as a slug; the summary is the cleaned
// description, untrusted.
func bazaarResource(raw json.RawMessage, cfg *X402Config, maxAmount int64) (X402Resource, bazaarSignal, bool) {
	// Lenient: the item is only read for fields; each payment option is
	// read strictly (boundedJSON, in chooseX402).
	var it bazaarItem
	if len(raw) > bazaarItemBytesMax || json.Unmarshal(raw, &it) != nil {
		return X402Resource{}, bazaarSignal{}, false
	}
	if (it.Type != "" && it.Type != "http") || checkX402URL(it.Resource) != nil {
		return X402Resource{}, bazaarSignal{}, false
	}
	version, _ := strconv.Atoi(it.X402Version.String())
	var choice x402Choice
	found := false
	for i, q := range it.Accepts {
		if i >= x402AcceptsMax {
			break
		}
		var req x402Requirement
		if json.Unmarshal(q, &req) != nil {
			continue
		}
		payTo, ok := ParseEVMAddress(req.PayTo)
		if !ok || payTo == (EVMAddress{}) {
			continue
		}
		c, err := chooseX402(x402Required{Version: max(version, 1), Accepts: []json.RawMessage{q}}, x402Want{
			Network: cfg.Network, Asset: cfg.Asset, AssetName: cfg.AssetName, AssetVersion: cfg.AssetVersion, PayTo: payTo, Max: maxAmount})
		if err == nil {
			choice, found = c, true
			break
		}
	}
	if !found {
		return X402Resource{}, bazaarSignal{}, false
	}
	in := it.Extensions.Bazaar.Info.Input
	method := strings.ToUpper(in.Method)
	if method == "" {
		method = strings.ToUpper(it.Method)
	}
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodPost {
		return X402Resource{}, bazaarSignal{}, false
	}
	u, _ := url.Parse(it.Resource)
	id := strings.Trim(slugRE.ReplaceAllString(strings.ToLower(u.Hostname()+u.Path), "-"), "-")
	if len(id) > 60 {
		id = strings.Trim(id[:60], "-")
	}
	if !x402IDRE.MatchString(id) {
		return X402Resource{}, bazaarSignal{}, false
	}
	r := X402Resource{ID: id, URL: it.Resource, Method: method, PayTo: choice.PayTo, MaxAmount: choice.Amount, Bundler: X402Bundler,
		Body: method == http.MethodPost && (in.BodyType == "" || in.BodyType == "json")}
	for name := range in.QueryParams {
		if x402ParamRE.MatchString(name) && len(r.Query) < X402QueryParamsMax {
			r.Query = append(r.Query, name)
		}
	}
	slices.Sort(r.Query)
	desc := it.Description
	if desc == "" {
		desc = it.Metadata.Description
	}
	r.Summary = cleanUntrusted(desc, 200)
	r.Category = x402Categorize(it.Resource + " " + it.Metadata.Category + " " + desc)
	var sig bazaarSignal
	if it.Quality.Payers != "" || it.Quality.Calls != "" {
		sig.Known = true
		sig.Payers, _ = strconv.ParseInt(it.Quality.Payers.String(), 10, 64)
		sig.Calls, _ = strconv.ParseInt(it.Quality.Calls.String(), 10, 64)
	}
	sig.Curated = string(bytes.TrimSpace(it.Curated)) == "true"
	return r, sig, true
}

// cleanUntrusted is upstream text made safe to show as data: valid UTF-8,
// no control or format characters, whitespace collapsed, at most n bytes.
func cleanUntrusted(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	for len(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// openID is an open resource's id: the slug, cut to leave room for 16 hex
// digits (64 bits) of the method and URL's hash, so it is stable across
// imports and distinct for every endpoint: a look-alike URL cannot be ground
// to collide with a vetted one (security review of the aggregator, L2).
func openID(slug, method, rawURL string) string {
	sum := sha256.Sum256([]byte(method + " " + rawURL))
	if len(slug) > 47 {
		slug = strings.Trim(slug[:47], "-")
	}
	return slug + "-" + hex.EncodeToString(sum[:8])
}

// ImportX402Bazaar turns a discovery response into allowlist candidates for
// cfg's network and asset: exact scheme, HTTPS on 443, a price within the
// per-call cap. It returns the candidates and how many items it skipped.
func ImportX402Bazaar(raw []byte, cfg *X402Config, source string) ([]X402Candidate, int, error) {
	page, err := parseBazaarPage(raw)
	if err != nil {
		return nil, 0, err
	}
	var out []X402Candidate
	skipped := 0
	ids := map[string]bool{}
	for _, item := range page.Items {
		r, _, ok := bazaarResource(item, cfg, cfg.PerCall)
		if !ok {
			skipped++
			continue
		}
		c := X402Candidate{ID: r.ID, URL: r.URL, Method: r.Method, PayTo: r.PayTo.String(), MaxPrice: formatUnits(r.MaxAmount, cfg.Decimals),
			Query: r.Query, Body: r.Body, Summary: "UNREVIEWED Bazaar text: " + r.Summary, Source: source}
		base := c.ID
		for n := 2; ids[c.ID]; n++ {
			c.ID = base + "-" + strconv.Itoa(n)
		}
		ids[c.ID] = true
		out = append(out, c)
	}
	return out, skipped, nil
}
