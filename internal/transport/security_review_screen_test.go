package transport

// Security review of screen.text (branch screen-service, 5a09344): proof of
// concept. It asserts the safe behaviour, so it fails while the finding is
// open.

import (
	"context"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

// M: help.ZONE with the production services plus screen, the free credit
// offer and calls without a key available no longer fits the 2048-byte TCP
// answer: the "Screening" give line and "text screening" in the no-key line
// add ~139 bytes (main: 2007, this branch: 2146), and the answer comes back
// empty with TC set. TestDNSAndLineHelpListTheCatalogue and
// TestHelpLeadsWithTheFreeCredit never set noKeyOf, so they miss it.
func TestReviewHelpFitsWithScreenAndNoKey(t *testing.T) {
	f := board.Features{Ledger: board.LedgerOn, Services: []string{"memory", "wakeup", "notary", "screen", "public_data", "inference", "x402", "runs"}}
	offer := board.FreeCreditOffer(100_000, f.Services)
	help := newCatalogHelp(f, "https://swarmmemo.com", nil)
	help.offer = func() *board.FreeCredit { return offer }
	nk := services.NoKeyFor("https://swarmmemo.com", help.catalog, 20_000, 2_000_000, "")
	help.noKeyOf = &noKeyCache{read: func(context.Context) services.NoKey { return nk }}
	d := testDNS(t)
	d.allowance, d.help = true, help
	budget := (*dns).Limits(nil).Response
	full := answerOnce(t, d, dnsQueryBytes("help.q.swarmmemo.com", dnsTypeTXT), board.Result{}, nil, 1<<16)
	if raw := answerOnce(t, d, dnsQueryBytes("help.q.swarmmemo.com", dnsTypeTXT), board.Result{}, nil, budget); flag(raw, 0x0200) {
		t.Fatalf("help.q is %d bytes, over the %d-byte TCP answer: truncated to %d bytes with TC", len(full), budget, len(raw))
	}
}
