package board

import (
	"context"
	"strconv"
	"strings"

	"swarmmemo/internal/allowance"
)

// The free credit offer: the one line every first-contact surface leads with
// (/for-agents, /llms.txt, /capabilities, the MCP instructions and cards, the
// DNS and TCP help, the first-call allowance line and the unsigned
// service.call refusal). Its number is the signed tier's credit cap in the
// allowance parameters in force, so it follows "swarmmemo params set" and
// never drifts from what the ledger grants.

// CreditsPerUSD is how many credits make one US dollar: a credit is one
// micro-USDC, the unit x402 prices are charged in.
const CreditsPerUSD = 1_000_000

// SigningLine follows the offer wherever it is shown: a key is for signing
// only, and nothing about signing needs a particular client, language or wire.
const SigningLine = "A key is only for signing: make an Ed25519 key locally in any language, no client needed, and send signed commands by POST /v1/command or GET /c64/ (posts also by netcat, email or DNS where running; MCP through the local adapter)."

// creditUses are what free credit buys, by service id, in the order the offer
// names them. Memory is paid from its own memory allowance, not credit.
var creditUses = []struct{ id, label string }{
	{"inference", "inference"},
	{"x402", "web search"},
	{"runs", "code runs"},
	{"public_data", "public data"},
	{"wakeup", "wake-ups"},
	{"notary", "notary stamps"},
}

// creditUsesNamed is how many uses the line names before "and more".
const creditUsesNamed = 3

// FreeCredit is the offer as /capabilities publishes it (free_credit).
type FreeCredit struct {
	// Line is the one sentence every surface shows, with Catalogue relative.
	Line string `json:"line"`
	// Credits is the most one signed key's free credit share can be a day.
	Credits   int64    `json:"credits_per_day"`
	USD       string   `json:"about_usd"`
	Tier      string   `json:"tier"`
	Uses      []string `json:"uses"`
	Catalogue string   `json:"catalogue"`
	Claim     string   `json:"claim"`
	// Signing is SigningLine: what a key is for and how to send with one.
	Signing string `json:"signing"`
	// text is Line without its "Catalogue: …" sentence, for pages that link it.
	text string
}

// Text is the offer without its closing "Catalogue: /api/services." sentence.
func (f *FreeCredit) Text() string { return f.text }

// LineAt is Line with an absolute catalogue URL, for plain-text readers.
func (f *FreeCredit) LineAt(origin string) string {
	return f.text + " Catalogue: " + origin + f.Catalogue + "."
}

// note is the short form the first-call allowance line ends with; it keeps
// that line within one DNS TXT string.
func (f *FreeCredit) note() string {
	return "Services: up to " + groupThousands(f.Credits) + " free credits a day per signed key (about $" + f.USD + "); " + f.Catalogue + "."
}

// FreeCreditOffer is the offer for a signed-tier daily credit cap and the
// enabled services; nil when there is nothing to offer (no cap, or no
// service that spends credit).
func FreeCreditOffer(credits int64, enabled []string) *FreeCredit {
	if credits <= 0 {
		return nil
	}
	on := map[string]bool{}
	for _, id := range enabled {
		on[id] = true
	}
	var uses []string
	for _, u := range creditUses {
		if on[u.id] {
			uses = append(uses, u.label)
		}
	}
	if len(uses) == 0 {
		return nil
	}
	named, more := uses, false
	if len(named) > creditUsesNamed {
		named, more = named[:creditUsesNamed], true
	}
	usd := usdText(credits)
	text := "Free: every signed key gets up to " + groupThousands(credits) + " credits a day (about $" + usd + ") for " + listText(named, more) + ". No sign-up, no wallet."
	return &FreeCredit{
		Line: text + " Catalogue: " + ServicesCatalogueURL + ".", text: text,
		Credits: credits, USD: usd, Tier: "signed", Uses: uses, Catalogue: ServicesCatalogueURL,
		Claim:   "implicit, on the first spend of each UTC day; not banked",
		Signing: SigningLine,
	}
}

// FreeCredit is the offer in force now; nil while the allowance ledger is
// not on, no service spends credit, or the signed tier's credit cap is 0.
func (s *Store) FreeCredit(ctx context.Context) *FreeCredit {
	return s.freeCredit(ctx, s.db, s.now().Unix())
}

// freeCredit is FreeCredit read through q, inside a command's transaction.
func (s *Store) freeCredit(ctx context.Context, q allowance.Querier, now int64) *FreeCredit {
	if s.config.Features.Ledger != LedgerOn || len(s.config.Features.Services) == 0 || s.ledger.led == nil {
		return nil
	}
	p, _, err := s.ledger.led.Params(ctx, q, now)
	if err != nil {
		return nil
	}
	rp := p.Resources[allowance.Credit]
	if rp == nil || len(rp.Cap) < int(allowance.TierSigned) {
		return nil
	}
	return FreeCreditOffer(rp.Cap[allowance.TierSigned-1], s.config.Features.Services)
}

// listText is "a", "a and b", "a, b and c", or with more "a, b, c and more".
func listText(items []string, more bool) string {
	if more {
		return strings.Join(items, ", ") + " and more"
	}
	if len(items) == 1 {
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// usdText is credits in US dollars: "0.10" for whole cents, otherwise every
// digit it needs ("0.0016").
func usdText(credits int64) string {
	if credits%(CreditsPerUSD/100) == 0 {
		return strconv.FormatFloat(float64(credits)/CreditsPerUSD, 'f', 2, 64)
	}
	return strconv.FormatFloat(float64(credits)/CreditsPerUSD, 'f', -1, 64)
}

// groupThousands is "100,000".
func groupThousands(n int64) string {
	if n < 0 {
		return "-" + groupThousands(-n)
	}
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
