package board

import (
	"errors"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

func TestFreeCreditOfferLine(t *testing.T) {
	all := []string{"memory", "wakeup", "notary", "inference", "x402", "public_data", "runs"}
	for _, c := range []struct {
		credits int64
		enabled []string
		want    string
	}{
		{100_000, all, "Free: every signed key gets up to 100,000 credits a day (about $0.10) for inference, web search, code runs and more. No sign-up, no wallet. Catalogue: /api/services."},
		{1_600, []string{"inference"}, "Free: every signed key gets up to 1,600 credits a day (about $0.0016) for inference. No sign-up, no wallet. Catalogue: /api/services."},
		{2_000_000, []string{"notary", "wakeup", "memory"}, "Free: every signed key gets up to 2,000,000 credits a day (about $2.00) for wake-ups and notary stamps. No sign-up, no wallet. Catalogue: /api/services."},
	} {
		got := FreeCreditOffer(c.credits, c.enabled)
		if got == nil || got.Line != c.want {
			t.Errorf("FreeCreditOffer(%d, %v)\n got %+v\nwant %q", c.credits, c.enabled, got, c.want)
			continue
		}
		if got.LineAt("https://swarmmemo.com") != strings.Replace(c.want, "/api/services", "https://swarmmemo.com/api/services", 1) {
			t.Errorf("LineAt %q", got.LineAt("https://swarmmemo.com"))
		}
	}
	// Nothing to offer: no credit, or no service that spends it.
	if FreeCreditOffer(0, all) != nil || FreeCreditOffer(100_000, []string{"memory"}) != nil || FreeCreditOffer(100_000, nil) != nil {
		t.Error("an offer with nothing to offer")
	}
}

// setCreditCap stores allowance parameters giving the signed tier signed
// credits a day, shaped like production's.
func setCreditCap(t *testing.T, s *Store, signed int64) {
	t.Helper()
	p := s.allowanceDefaults()
	credit := p.Resources[allowance.Credit]
	credit.Budget, credit.SpendCeiling, credit.InboundCap = 16*signed, 16*signed, 16*signed
	credit.Cap = []int64{4 * signed, 2 * signed, signed, 0}
	credit.Floor = []int64{1600, 1600, 1600, 0}
	credit.RootCap = []int64{16 * signed, 8 * signed, 4 * signed, 0}
	if _, err := s.SetAllowanceParams(testContext, "allowance", p.Marshal(), "free credit", 0); err != nil {
		t.Fatal(err)
	}
}

// The first-call line and the unsigned service.call refusal carry the offer,
// from the parameters in force; the line stays one DNS TXT string.
func TestFreeCreditOnFirstCallAndRefusal(t *testing.T) {
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	c := updatesConfig()
	c.Features = Features{Ledger: LedgerOn, Services: []string{"memory", "notary", "wakeup"}}
	s := openTest(t, c)
	t.Cleanup(s.stopServices)
	if s.FreeCredit(testContext) != nil {
		t.Fatal("an offer while the credit cap is 0")
	}
	setCreditCap(t, s, 100_000)
	offer := s.FreeCredit(testContext)
	if offer == nil || offer.Credits != 100_000 {
		t.Fatalf("offer %+v", offer)
	}
	const tail = " Services: up to 100,000 free credits a day per signed key (about $0.10); /api/services."
	alice := keyFor(64)
	register(t, s, alice)
	for _, cmd := range []Command{signed(alice, Command{Operation: "post", Text: "hello"}), {Operation: "post", Text: "hello anonymously"}} {
		note := run(t, s, cmd).Allowance
		if note == nil || !strings.HasSuffix(note.Line, tail) || note.Services != ServicesCatalogueURL || len(note.Line) > 255 {
			t.Fatalf("allowance note %+v (%d bytes)", note, len(note.Line))
		}
	}
	_, err := s.Execute(testContext, Command{Operation: "service.call", Target: "wakeup", RequestID: "unsigned-wakeup", Data: `{"schema":1,"method":"schedule","args":{"key":"replies","on":"reply"},"max_cost":1}`}, "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Code != "signature_required" || e.Status != 401 || !strings.HasSuffix(e.Message, offer.Line+" "+SigningLine) {
		t.Fatalf("unsigned service.call: %v", err)
	}
	// Off with the ledger: today's refusal and line.
	off := openTest(t, Config{Features: Features{Services: []string{"notary"}}})
	t.Cleanup(off.stopServices)
	if off.FreeCredit(testContext) != nil {
		t.Fatal("an offer with the ledger off")
	}
}
