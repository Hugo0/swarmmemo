package services

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services/servicestest"
)

const testPricingURL = "https://example.com/pricing"

// withSellers is testResources with a sellers list declaring example.com's
// price list, as the allowlist file carries it.
func withSellers(resources string) string {
	return resources + `,"sellers":[{"host":"example.com","pricing_url":"` + testPricingURL + `"}]`
}

func pricingOf(err error) string {
	var e *allowance.Err
	if errors.As(err, &e) {
		return e.PricingURL
	}
	return ""
}

func TestSellerPricingURLDeclaration(t *testing.T) {
	key, _ := hex.DecodeString(x402TestKey)
	signer, _ := signerFromKeyBytes(key)
	config := []byte(`{"schema":1,"enabled":true,"network":"eip155:8453","asset":"` + testUSDC + `","asset_name":"USD Coin","asset_version":"2","decimals":6,"caps":{},"wallet_key_file":"k","allowlist_file":"a"}`)
	parse := func(sellers string) (*X402Config, error) {
		allowlist := `{"schema":1,"version":1,"resources":[]` + sellers + `}`
		return ParseX402Config(config, []byte(allowlist), signer)
	}
	// An allowlist from before sellers parses, with none declared.
	cfg, err := parse("")
	if err != nil || len(cfg.Sellers) != 0 || cfg.PricingURL("https://api.axiomworks.co.uk/x") != "" {
		t.Fatalf("old allowlist: %v %v", cfg, err)
	}
	cfg, err = parse(`,"sellers":[{"host":"API.Seller.example","pricing_url":"https://seller.example/x402/prices"}]`)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.PricingURL("https://api.seller.example/companies?q=1"); got != "https://seller.example/x402/prices" {
		t.Fatalf("declared host: %q", got)
	}
	for _, other := range []string{"https://seller.example/x", "https://x.api.seller.example/x", "not a url"} {
		if got := cfg.PricingURL(other); got != "" {
			t.Fatalf("%s matched %q", other, got)
		}
	}
	long := "https://seller.example/" + strings.Repeat("a", PricingURLBytes)
	for name, sellers := range map[string]string{
		"http":          `,"sellers":[{"host":"api.seller.example","pricing_url":"http://seller.example/prices"}]`,
		"oversize":      `,"sellers":[{"host":"api.seller.example","pricing_url":"` + long + `"}]`,
		"ip literal":    `,"sellers":[{"host":"api.seller.example","pricing_url":"https://192.0.2.1/prices"}]`,
		"other port":    `,"sellers":[{"host":"api.seller.example","pricing_url":"https://seller.example:8443/prices"}]`,
		"credentials":   `,"sellers":[{"host":"api.seller.example","pricing_url":"https://u:p@seller.example/prices"}]`,
		"empty":         `,"sellers":[{"host":"api.seller.example","pricing_url":""}]`,
		"bad host":      `,"sellers":[{"host":"https://api.seller.example/","pricing_url":"https://seller.example/prices"}]`,
		"no dot host":   `,"sellers":[{"host":"localhost","pricing_url":"https://seller.example/prices"}]`,
		"twice":         `,"sellers":[{"host":"a.example","pricing_url":"https://a.example/p"},{"host":"A.example","pricing_url":"https://a.example/q"}]`,
		"unknown field": `,"sellers":[{"host":"a.example","pricing_url":"https://a.example/p","note":"x"}]`,
	} {
		if _, err := parse(sellers); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := CheckPricingURL("https://seller.example/" + strings.Repeat("a", PricingURLBytes-len("https://seller.example/"))); err != nil {
		t.Fatalf("%d bytes refused: %v", PricingURLBytes, err)
	}
}

func TestX402RefusalCarriesThePricingURL(t *testing.T) {
	// The seller's 402 is refused by its facilitator: its price list goes
	// with the refusal.
	h := newX402HarnessWith(t, &fakeX402{price: 1000, reject: "insufficient_funds"}, `{}`, withSellers(testResources))
	_, err := h.call(testSubject, `{"resource":"price"}`, creditsFor(10000))
	if errCode(err) != "x402_payment_rejected" || pricingOf(err) != testPricingURL {
		t.Fatalf("rejected: %v %q", err, pricingOf(err))
	}
	// Undeclared, the same refusal carries none.
	h = newX402Harness(t, &fakeX402{price: 1000, reject: "insufficient_funds"}, `{}`)
	if _, err = h.call(testSubject, `{"resource":"price"}`, creditsFor(10000)); errCode(err) != "x402_payment_rejected" || pricingOf(err) != "" {
		t.Fatalf("undeclared: %v %q", err, pricingOf(err))
	}
	// Our own allowance refuses the reservation: the price list goes with it.
	h = newX402HarnessWith(t, &fakeX402{price: 1000}, `{}`, withSellers(testResources))
	h.meter = servicestest.NewMeter(1)
	h.engine = NewEngine(Config{DB: h.db, Registry: h.engine.cfg.Registry, Meter: h.meter, Now: func() int64 { return h.now }})
	t.Cleanup(h.engine.Stop)
	_, err = h.call(testSubject, `{"resource":"price"}`, creditsFor(10000))
	if err == nil || pricingOf(err) != testPricingURL {
		t.Fatalf("over the allowance: %v %q", err, pricingOf(err))
	}
	if h.fake.payHdrs != 0 {
		t.Fatal("paid past a refused reservation")
	}
}

func TestX402ResourcesListThePricingURL(t *testing.T) {
	read := func(h *x402Harness) string {
		got, err := h.engine.Read(context.Background(), h.db, Request{Service: "x402", Data: `{"schema":1,"method":"resources"}`, Subject: testSubject}, h.now)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(got)
		return string(raw)
	}
	raw := read(newX402HarnessWith(t, &fakeX402{price: 1000}, `{}`, withSellers(testResources)))
	if strings.Count(raw, `"pricing_url":"`+testPricingURL+`"`) != 2 || !strings.Contains(raw, `"pricing":`) {
		t.Fatalf("declared: %s", raw)
	}
	if raw = read(newX402Harness(t, &fakeX402{price: 1000}, `{}`)); strings.Contains(raw, "pricing") {
		t.Fatalf("undeclared: %s", raw)
	}
}
