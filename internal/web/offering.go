package web

// An agent offering's page (RFC 0017): /@HANDLE/NAME, the signed listing a
// caller reads before paying. The same path answers JSON to an agent and
// takes a keyless call with POST (httpapi/offerings.go).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"swarmmemo/internal/board"
)

// offeringPage is the listing as the page shows it.
type offeringPage struct {
	Path, Name, Title, Description, Price, PayTo, Network, Refund, State string
	Handle, Fingerprint, Input, PublicKey, Signature, SignedPayload      string
	Example                                                              string // an input the schema takes, for the curl line
	Rev, ClaimWindow, SLA                                                int64
	ScreenAnswer                                                         bool
	Record                                                               map[string]any
}

// loadOffering fills p from offering.get; the status is the page's.
func loadOffering(r *http.Request, p *page, execute func(board.Command) (board.Result, error)) int {
	alias, name, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/@"), "/")
	missing := func(status int) int {
		p.View, p.NoIndex, p.Title = "missing", true, "Offering not found"
		return status
	}
	if strings.Contains(name, "/") {
		return missing(404)
	}
	res, err := execute(board.Command{Operation: "offering.get", Target: alias + "/" + name})
	if err != nil {
		var be *board.Error
		if errors.As(err, &be) && be.Status < 500 {
			return missing(404)
		}
		return missing(503)
	}
	o, ok := res.Data["offering"].(map[string]any)
	if !ok {
		return missing(404)
	}
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	num := func(k string) int64 {
		switch v := o[k].(type) {
		case int64:
			return v
		case float64:
			return int64(v)
		}
		return 0
	}
	v := &offeringPage{Path: str(o, "path"), Name: str(o, "name"), Title: str(o, "title"), Description: str(o, "description"), Price: str(o, "price"),
		PayTo: str(o, "pay_to"), Network: str(o, "network"), Refund: str(o, "refund"), State: str(o, "state"), Rev: num("rev"), ClaimWindow: num("claim_window"), SLA: num("sla")}
	v.ScreenAnswer, _ = o["screen_answer"].(bool)
	if pr, ok := o["provider"].(map[string]any); ok {
		v.Handle, v.Fingerprint = str(pr, "handle"), str(pr, "fingerprint")
	}
	if sig, ok := o["signed"].(map[string]any); ok {
		v.PublicKey, v.Signature, v.SignedPayload = str(sig, "public_key"), str(sig, "signature"), str(sig, "signed_payload")
	}
	v.Example = "{}"
	if in, ok := o["input"]; ok {
		raw, _ := json.MarshalIndent(in, "", "  ")
		v.Input = string(raw)
		v.Example = exampleInput(raw)
	}
	v.Record, _ = o["record"].(map[string]any)
	p.View, p.OfferingView = "offering", v
	p.Title = v.Title
	p.Description = fmt.Sprintf("%s: %s USDC, paid to the provider over x402 and settled only when it claims the call.", v.Title, v.Price)
	p.Canonical = v.Path
	if v.State != "active" {
		p.NoIndex = true
	}
	return 200
}

// exampleInput is an input schema's required properties (or, without any,
// its first) with a value of each one's type: "…" for a string, the
// enum's first value when it has one. Single quotes are left out so the
// curl line stays one shell word.
func exampleInput(schema []byte) string {
	var s struct {
		Properties map[string]struct {
			Type string            `json:"type"`
			Enum []json.RawMessage `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if json.Unmarshal(schema, &s) != nil || len(s.Properties) == 0 {
		return "{}"
	}
	names := s.Required
	if len(names) == 0 {
		for name := range s.Properties {
			if len(names) == 0 || name < names[0] {
				names = []string{name}
			}
		}
	}
	out := map[string]json.RawMessage{}
	for _, name := range names {
		p := s.Properties[name]
		switch {
		case len(p.Enum) > 0:
			out[name] = p.Enum[0]
		case p.Type == "number" || p.Type == "integer":
			out[name] = json.RawMessage("1")
		case p.Type == "boolean":
			out[name] = json.RawMessage("true")
		default:
			out[name] = json.RawMessage(`"…"`)
		}
	}
	raw, _ := json.Marshal(out)
	return strings.ReplaceAll(string(raw), "'", "")
}
