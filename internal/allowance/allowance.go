// Package allowance holds the types shared by the ledger, the service
// providers, the trust module and the board (RFC0012 §3.1). It is a leaf: it
// imports only the standard library, so every other package may depend on it.
package allowance

import (
	"context"
	"database/sql"
)

type Tier uint8      // 0 grant pool, 1 trusted, 2 proven, 3 signed, 4 anonymous
type Resource string // "post_bytes", "memory_bytes", "credit"
type Bucket string   // "free", "granted", "earned", "paid"

const (
	TierPool      Tier = 0
	TierTrusted   Tier = 1
	TierProven    Tier = 2
	TierSigned    Tier = 3
	TierAnonymous Tier = 4
)

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
	// it shares the anonymous tier until it is claimed.
	Hosted bool
	// Credential names the delegated credential the command came through
	// ("key:" + a worker key's fingerprint, "token:" + a hosted token's
	// token_id); "" for the account's own key. A credential may carry a
	// spend limit (ledger spend_limits).
	Credential string
}

type Standing struct {
	Tier      Tier
	WeightPPM int64  // share weight inside the tier; 1e6 is one ordinary share
	Root      string // saturation root; the account itself when it has none
	NewKey    bool   // first signed write after pause-new-keys
	Source    string // "design0" or "trust:RUN_ID"
	Reason    string // one public sentence
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
	SignedOnly, ProvenOnly, FreezeTransfers, PauseNewKeys bool
	// SignedServices is the signed-services lever: unsigned service calls
	// are refused and the anonymous tier gets no credit (its credit pool and
	// every anonymous credit share are zero until it is released).
	SignedServices    bool
	PauseNewKeysSince int64
	Tier4SharePPM     int64              // -1 when the lever is not pulled
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
	// Sent and Limit are a size refusal's value sent and its limit, in
	// bytes (Limit 0: not a size refusal); the board's message states them.
	Sent, Limit int
	// SpendLimit is a spend_limit refusal's limit: which one ("per_day",
	// "per_call" or "expired") and its value in credits.
	SpendLimit      string
	SpendLimitValue int64
	// Resource and Tier are a tier_has_no_share refusal's: the resource the
	// parameters give the subject's tier no share of, and that tier (1–4).
	Resource Resource
	Tier     int
	// Details is structured context the refusal must carry to be
	// actionable (doc_conflict's current version); never secrets.
	Details any
	// Message is an argument-level invalid_service_data refusal's own
	// sentence: it names the argument and what it takes, never the value
	// sent. Empty for any other refusal.
	Message string
}

func (e *Err) Error() string { return "allowance: " + e.Code }

// Unavailable is the stub refusal every RFC0012 package returns until its
// builder replaces it.
func Unavailable() error { return &Err{Code: "service_unavailable"} }
