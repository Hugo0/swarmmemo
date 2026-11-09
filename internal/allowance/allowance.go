// Package allowance holds the types shared by the ledger, the service
// providers, the trust module and the board (RFC0012 §3.1, RFC 0021 §12).
// It is a leaf: it imports only the standard library, so every other
// package may depend on it.
package allowance

import (
	"context"
	"database/sql"
	"strconv"
)

type Resource string // "post_bytes", "memory_bytes", "credit"
type Bucket string   // "free", "granted", "earned", "paid"

// Band is the one coarse reading of standing (RFC 0021 §12), from one trust
// run: anonymous (no key, or a hosted key not yet claimed), signed (a key,
// standing below theta2 = U), proven (standing ≥ theta2) and trusted
// (standing ≥ node_cap). Gates read a band or cents; nothing reads a tier.
type Band uint8

const (
	BandAnonymous Band = 0
	BandSigned    Band = 1
	BandProven    Band = 2
	BandTrusted   Band = 3
)

// BandNames are the bands' public names, by Band: the one spelling every
// wire, page and log uses.
var BandNames = [4]string{"anonymous", "signed", "proven", "trusted"}

// Name is the band's public name (BandNames), or "band N" outside 0-3.
func (b Band) Name() string {
	if int(b) < len(BandNames) {
		return BandNames[b]
	}
	return "band " + strconv.Itoa(int(b))
}

// ParseBand reads a band's public name.
func ParseBand(name string) (Band, bool) {
	for i, n := range BandNames {
		if n == name {
			return Band(i), true
		}
	}
	return 0, false
}

const (
	PostBytes   Resource = "post_bytes"
	MemoryBytes Resource = "memory_bytes"
	Credit      Resource = "credit"
)

const (
	Free    Bucket = "free"
	Granted Bucket = "granted"
	Earned  Bucket = "earned"
	Paid    Bucket = "paid"
)

type Subject struct {
	ID     string // continuity account, or "anon:" + 32 hex of HMAC(salt_d, prefix)
	Client string // anonymous only: HMAC(salt_d, prefix ‖ client signature); "" otherwise
	KeyID  string // signing key fingerprint; "" when anonymous
	Signed bool
	// Hosted is a signed account whose key SwarmMemo holds (RFC0013 §2.4):
	// it gets the floor only (as anonymous callers do) until it is claimed.
	Hosted bool
	// Credential names the delegated credential the command came through
	// ("key:" + a worker key's fingerprint, "token:" + a hosted token's
	// token_id); "" for the account's own key. A credential may carry a
	// spend limit (ledger spend_limits).
	Credential string
}

// Standing is what the allowance and every gate read about a subject: its
// standing in cents from the latest trust run (RFC 0021) and its band.
type Standing struct {
	Band Band
	// Cents is the subject's standing in the latest trust run: 0 for an
	// anonymous subject, a hosted key not yet claimed, or a key the run did
	// not score. Every account of one root reports the root's standing.
	Cents int64
	// ShareCents is what the allowance's standing slice weighs (RFC 0021
	// §12): min(Cents, node_cap); 0 when the subject has no standing share.
	// The ledger shares it once per Root.
	ShareCents int64
	Root       string // saturation root; the account itself when it has none
	NewKey     bool   // first signed write after pause-new-keys
	Source     string // "standing:RUN_ID", or "none" before any run
	Reason     string // one public sentence
}

// Querier is satisfied by *sql.Tx and *sql.DB.
type Querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type Classifier interface {
	Classify(ctx context.Context, q Querier, s Subject, now int64) (Standing, error)
}

type Levers struct {
	// SignedOnly gives anonymous callers nothing; ProvenOnly gives every
	// subject below the proven band nothing.
	SignedOnly, ProvenOnly, FreezeTransfers, PauseNewKeys bool
	// SignedServices is the signed-services lever: unsigned service calls
	// are refused and anonymous callers get no credit.
	SignedServices    bool
	PauseNewKeysSince int64
	// AnonymousSharePPM caps what anonymous callers draw of the day's
	// budget (the anonymous-shrink lever); -1 when the lever is not pulled.
	AnonymousSharePPM int64
	BudgetCutPPM      map[Resource]int64 // absent when not pulled
	Version           int64
}

type LeverSource interface {
	Levers(ctx context.Context, q Querier, now int64) (Levers, error)
	PrefixBlocked(ctx context.Context, q Querier, source string, now int64) (bool, error)
}

type ParamsSource interface {
	Params(ctx context.Context, q Querier, namespace string, now int64) (version int64, body []byte, err error)
}

// Err is the only error the ledger, services and trust packages return for a
// refusal. The board maps Code to its HTTP status and message with
// allowanceError; nothing outside internal/board chooses a status.
type Err struct {
	Code       string
	RetryAfter int
	// NextOpenAt is when the refusal lifts by itself (unix seconds), set
	// only where that time is computed (a UTC day or hour, a window's end),
	// never for a retry hint; RetryAfter is then the seconds until it.
	NextOpenAt int64
	// Sent and Limit are a size refusal's value sent and its limit, in
	// bytes (Limit 0: not a size refusal); the board's message states them.
	Sent, Limit int
	// SpendLimit is a spend_limit refusal's limit: which one ("per_day",
	// "per_call" or "expired") and its value in credits.
	SpendLimit      string
	SpendLimitValue int64
	// Resource and Band are a no_share refusal's: the resource the
	// parameters give the subject no share of, and the subject's band.
	Resource Resource
	Band     Band
	// Details is structured context the refusal must carry to be
	// actionable (doc_conflict's current version); never secrets.
	Details any
	// PricingURL is the seller's own x402 price list (pricing_url), set
	// on a refusal of a call to a seller that declared one; the board sends
	// it as details.pricing_url. "" for any other refusal.
	PricingURL string
	// Message is an argument-level invalid_service_data refusal's own
	// sentence: it names the argument and what it takes, never the value
	// sent. On fetch_invalid_url it is the sentence naming an unfilled
	// placeholder sent as the url (C837; a bare upper-case token, never
	// free text). Empty for any other refusal.
	Message string
}

func (e *Err) Error() string { return "allowance: " + e.Code }

// Until is a refusal that lifts by itself at next (unix seconds), seen at
// now: NextOpenAt next and RetryAfter the seconds until it, at least 1.
func Until(code string, next, now int64) *Err {
	return &Err{Code: code, RetryAfter: int(max(next-now, 1)), NextOpenAt: next}
}

// NextDay is the next 00:00 UTC after now, and NextHour the next whole
// UTC hour: when a daily or hourly cap lifts.
func NextDay(now int64) int64  { return now - now%86400 + 86400 }
func NextHour(now int64) int64 { return now - now%3600 + 3600 }

// Unavailable is the stub refusal every RFC0012 package returns until its
// builder replaces it.
func Unavailable() error { return &Err{Code: "service_unavailable"} }
