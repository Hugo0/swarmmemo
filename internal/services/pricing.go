package services

// Seller price lists (C793, GitHub #49): a seller of a paid API may have
// the operator declare its own x402 price list, pricing_url, for its host,
// in the allowlist file's sellers. When a call to that host cannot go on
// for free (the upstream answers 402, its free tier or preview spent, or
// SwarmMemo's own allowance or budget refuses the call), the refusal
// carries details.pricing_url, so the agent can pay the seller itself. It
// is opt-in and additive: a host with none declared answers as before.
// Nothing here is special to one seller.

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"swarmmemo/internal/allowance"
)

const (
	// PricingURLBytes bounds a declared pricing_url.
	PricingURLBytes = 256
	// x402SellersMax bounds the allowlist file's sellers.
	x402SellersMax = X402ResourcesMax
)

// X402PricingNote says where pricing_url appears, on the resources read
// and /capabilities: the one copy.
const X402PricingNote = "pricing_url, where a seller declared one, is its own x402 price list: a resource on its host lists it, and a refusal of a call to it (an upstream 402, its free tier or preview spent, or SwarmMemo's allowance or budget) carries it as details.pricing_url, so you can pay the seller directly."

// x402SellerFile is one entry of the allowlist file's sellers.
type x402SellerFile struct {
	Host       string `json:"host"`
	PricingURL string `json:"pricing_url"`
}

// CheckPricingURL admits a pricing_url: an https URL on 443 to a public
// DNS name (checkX402URL, the check every declared endpoint passes), at
// most PricingURLBytes.
func CheckPricingURL(raw string) error {
	if len(raw) > PricingURLBytes {
		return fmt.Errorf("longer than %d bytes", PricingURLBytes)
	}
	return checkX402URL(raw)
}

// parseSellers reads the allowlist file's sellers into c.Sellers: one
// pricing_url per host, matched exactly (lower case, no port).
func (c *X402Config) parseSellers(list []x402SellerFile) error {
	if len(list) > x402SellersMax {
		return fmt.Errorf("x402: at most %d sellers", x402SellersMax)
	}
	for i, s := range list {
		bad := func(field, why string) error {
			return fmt.Errorf("x402: allowlist seller %d (%q): %s %s", i, s.Host, field, why)
		}
		host := strings.ToLower(strings.TrimSpace(s.Host))
		if host == "" || strings.ContainsAny(host, "/:@?#") || checkX402URL("https://"+host+"/") != nil {
			return bad("host", "is not a public DNS name")
		}
		if c.Sellers[host] != "" {
			return bad("host", "is listed twice")
		}
		if err := CheckPricingURL(s.PricingURL); err != nil {
			return bad("pricing_url", "("+err.Error()+")")
		}
		if c.Sellers == nil {
			c.Sellers = map[string]string{}
		}
		c.Sellers[host] = s.PricingURL
	}
	return nil
}

// PricingURL is the pricing_url declared for rawURL's host, "" when none
// is (or c is nil).
func (c *X402Config) PricingURL(rawURL string) string {
	if c == nil || len(c.Sellers) == 0 {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return c.Sellers[strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")]
}

// Pricer is a provider whose call may go to a seller's host: PricingURL is
// that seller's declared pricing_url for the call, "" for none. The
// engine adds it to a refusal of the call's reservation or spend.
type Pricer interface {
	PricingURL(c Call) string
}

// withPricingURL is err with pricingURL added when err is a refusal
// (*allowance.Err) and pricingURL is set; err itself otherwise. The
// refusal is copied, never changed in place.
func withPricingURL(err error, pricingURL string) error {
	var ae *allowance.Err
	if pricingURL == "" || !errors.As(err, &ae) {
		return err
	}
	out := *ae
	out.PricingURL = pricingURL
	return &out
}

// callPricingURL is p's pricing_url for c, "" when p is no Pricer.
func callPricingURL(p Provider, c Call) string {
	if pr, ok := p.(Pricer); ok {
		return pr.PricingURL(c)
	}
	return ""
}

// PricingURL is the declared pricing_url of the resource c calls.
func (x *x402) PricingURL(c Call) string {
	if x.cfg == nil || len(x.cfg.Sellers) == 0 {
		return ""
	}
	var a x402Args
	if StrictObject(c.Args, &a) != nil {
		return ""
	}
	res, ok := x.lookup(a.Resource)
	if !ok {
		return ""
	}
	return x.cfg.PricingURL(res.URL)
}

// x402PricedRefusal reports whether a call's refusal says the seller's
// price applies: its 402 was refused or not payable by us, or our budget
// for it is spent.
func x402PricedRefusal(err error) bool {
	var ae *allowance.Err
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.Code {
	case "x402_payment_rejected", "x402_not_payable", "x402_cap_reached":
		return true
	}
	return false
}

// PricingURL is the declared pricing_url of the page c fetches.
func (f *fetch) PricingURL(c Call) string {
	if f.sellers == nil {
		return ""
	}
	p, err := parseFetch(c.Args)
	if err != nil {
		return ""
	}
	return f.sellers.PricingURL(p.u.String())
}
