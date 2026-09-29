package services

// Importing allowlist candidates from an x402 Bazaar discovery response
// (GET /discovery/resources). The import never writes the allowlist: it
// prints candidates the operator reviews, edits and adds by hand, raising the
// allowlist version. Bazaar text (descriptions) is untrusted data: it is
// truncated, kept only as a summary for the operator to rewrite, and never
// followed.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
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

type bazaarItem struct {
	Resource    string            `json:"resource"`
	Type        string            `json:"type"`
	X402Version json.Number       `json:"x402Version"`
	Accepts     []json.RawMessage `json:"accepts"`
	Description string            `json:"description"`
	Extensions  struct {
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

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

// ImportX402Bazaar turns a discovery response into allowlist candidates for
// cfg's network and asset: exact scheme, HTTPS on 443, a price within the
// per-call cap. It returns the candidates and how many items it skipped.
func ImportX402Bazaar(raw []byte, cfg *X402Config, source string) ([]X402Candidate, int, error) {
	var page struct {
		Items []bazaarItem `json:"items"`
	}
	if len(raw) > 64<<20 || json.Unmarshal(raw, &page) != nil {
		return nil, 0, errors.New("x402: not a Bazaar discovery response (JSON with items)")
	}
	var out []X402Candidate
	skipped := 0
	ids := map[string]bool{}
	for _, it := range page.Items {
		c, ok := bazaarCandidate(it, cfg, source)
		if !ok {
			skipped++
			continue
		}
		base := c.ID
		for n := 2; ids[c.ID]; n++ {
			c.ID = base + "-" + strconv.Itoa(n)
		}
		ids[c.ID] = true
		out = append(out, c)
	}
	return out, skipped, nil
}

func bazaarCandidate(it bazaarItem, cfg *X402Config, source string) (X402Candidate, bool) {
	if (it.Type != "" && it.Type != "http") || checkX402URL(it.Resource) != nil {
		return X402Candidate{}, false
	}
	version, _ := strconv.Atoi(it.X402Version.String())
	var choice x402Choice
	found := false
	for _, raw := range it.Accepts {
		var q x402Requirement
		if json.Unmarshal(raw, &q) != nil {
			continue
		}
		payTo, ok := ParseEVMAddress(q.PayTo)
		if !ok {
			continue
		}
		c, err := chooseX402(x402Required{Version: max(version, 1), Accepts: []json.RawMessage{raw}}, x402Want{
			Network: cfg.Network, Asset: cfg.Asset, AssetName: cfg.AssetName, AssetVersion: cfg.AssetVersion, PayTo: payTo, Max: cfg.PerCall})
		if err == nil {
			choice, found = c, true
			break
		}
	}
	if !found {
		return X402Candidate{}, false
	}
	in := it.Extensions.Bazaar.Info.Input
	method := strings.ToUpper(in.Method)
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodPost {
		return X402Candidate{}, false
	}
	u, _ := url.Parse(it.Resource)
	id := strings.Trim(slugRE.ReplaceAllString(strings.ToLower(u.Hostname()+u.Path), "-"), "-")
	if len(id) > 60 {
		id = strings.Trim(id[:60], "-")
	}
	if !x402IDRE.MatchString(id) {
		return X402Candidate{}, false
	}
	c := X402Candidate{ID: id, URL: it.Resource, Method: method, PayTo: choice.PayTo.String(), MaxPrice: formatUnits(choice.Amount, cfg.Decimals), Source: source,
		Body: method == http.MethodPost && (in.BodyType == "" || in.BodyType == "json")}
	for name := range in.QueryParams {
		if x402ParamRE.MatchString(name) && len(c.Query) < X402QueryParamsMax {
			c.Query = append(c.Query, name)
		}
	}
	slices.Sort(c.Query)
	summary := strings.Join(strings.Fields(it.Description), " ")
	if !utf8.ValidString(summary) {
		summary = ""
	}
	for len(summary) > 200 {
		_, size := utf8.DecodeLastRuneInString(summary)
		summary = summary[:len(summary)-size]
	}
	c.Summary = "UNREVIEWED Bazaar text: " + summary
	return c, true
}
