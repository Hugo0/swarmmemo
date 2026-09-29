// Package ledger is the allowance ledger and the daily waterfall (RFC0012 §2):
// params, pools, claims, lots, holds, transfers, the breaker, the sweeper and
// the public journal.
//
// Every method runs inside the caller's transaction (q is the command's
// *sql.Tx) and never holds it across I/O. The store has one SQLite
// connection, so pool rows cannot be overdrawn by a race: each operation reads
// the day's rows, changes them and writes them back in that one transaction.
// Every loop and query is bounded (§10 Bounds).
package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"math/bits"
	"sync"

	"swarmmemo/internal/allowance"
)

// Ref says what a spend or hold is for, for the journal and the public ledger.
type Ref struct {
	Service   string // "board", "memory", "echo"
	Op        string // the command operation, e.g. "post"
	Method    string // the service method, for service.call
	PublicRef string // an id that may be shown publicly; "" for private objects
}

// Hold is reserved capacity awaiting Commit or Refund (§2.5).
type Hold struct {
	ID        string
	Account   string
	Resource  allowance.Resource
	Max       int64
	ExpiresAt int64
	State     string // "held", "committed", "refunded", "expired"
}

// Receipt is what a committed spend cost.
type Receipt struct {
	HoldID        string
	Resource      allowance.Resource
	Used          int64
	Refunded      int64
	ParamsVersion int64
}

// Balance is a subject's allowance for one resource on one day. Prospective
// is true when the day's claim has not happened yet (a read writes nothing).
type Balance struct {
	Resource      allowance.Resource
	Standing      allowance.Standing
	Entitlement   int64 // what today's claim issued (or would issue)
	Used          int64 // spent today, fees and open holds included
	Incoming      int64 // received by transfer today
	Outgoing      int64 // given by transfer today
	Remaining     int64 // spendable now: live lots less holds, plus a prospective claim
	ByBucket      map[allowance.Bucket]int64
	ResetsAt      int64
	ParamsVersion int64
	Prospective   bool
}

// Transfer is one allowance.transfer, pending or done.
type Transfer struct {
	ID        string
	From, To  string
	Resource  allowance.Resource
	Amount    int64
	Fee       int64
	State     string // "pending", "done", "cancelled"
	CreatedAt int64
	ExecuteAt int64
	ExpiresAt int64 // earliest expiry of the moved units; 0 when none expire
	ByBucket  map[allowance.Bucket]int64
}

// Entry is one public journal line (§8.2 public ledger rules apply).
type Entry struct {
	Seq           int64
	Day           int64
	CreatedAt     int64
	Kind          string
	Account       string
	Counterparty  string
	Resource      allowance.Resource
	Bucket        allowance.Bucket
	Amount        int64
	Service       string
	Op            string
	PublicRef     string
	ParamsVersion int64
	Detail        string
}

// JournalQuery selects a page of the public journal, newest first.
type JournalQuery struct {
	Account string
	Before  int64 // sequence to page from; 0 is the newest
	Limit   int
}

// Stats is the waterfall and transfer state for /stats and /api/stats/allowance.
type Stats struct {
	Days      []DayStats // newest first
	Services  []ServiceStats
	Transfers TransferStats
}

// ServiceStats is what one service spent today from one bucket.
type ServiceStats struct {
	Service  string
	Resource allowance.Resource
	Bucket   allowance.Bucket
	Units    int64
	Calls    int64 // journal lines: one per lot a spend or commit touched
	// Anonymous is true for the part spent by anonymous subjects (tier 4,
	// one per network prefix), false for signed accounts.
	Anonymous bool
}

// DayStats is one resource on one UTC day.
type DayStats struct {
	Resource        allowance.Resource
	Day             int64
	Budget          int64
	BudgetEffective int64
	Unallocated     int64
	Spent           int64 // non-paid and paid
	SpentPaid       int64
	ParamsVersion   int64
	Tiers           []PoolStats // tiers 0–4
	Buckets         map[allowance.Bucket]int64
}

// PoolStats is one tier's pool on one day.
type PoolStats struct {
	Tier      int
	Size      int64
	Want      int64
	SpillIn   int64
	SpillOut  int64
	Claimed   int64
	Lent      int64
	Borrowed  int64
	Claimants int64
}

// TransferStats are today's transfers of one resource.
type TransferStats struct {
	Resource                 allowance.Resource
	Count                    int64
	Volume                   int64
	Pending                  int64
	LargestRecipientSharePPM int64 // of today's budget
}

// Config wires the ledger to the board: the classifier (Design 0, later
// trust), the lever state and the parameter store. A nil Classifier is
// DefaultClassifier; nil Levers means no lever is pulled; nil Params is a
// ParamsStore with the compiled-in defaults.
type Config struct {
	Classifier allowance.Classifier
	Levers     allowance.LeverSource
	Params     allowance.ParamsSource
}

// Bounds (§10).
const (
	HoldsPerAccount     = 2
	HoldsTotal          = 64
	TransfersPendingMax = 8
	LotsPerAccount      = 64
	SweepMax            = 500
	JournalPageMax      = 100
	journalScanMax      = 2000
)

// Ledger is safe for concurrent use; every method runs inside the caller's
// transaction (q is the command's *sql.Tx) and never holds it across I/O.
type Ledger struct {
	cfg Config

	mu    sync.Mutex
	cache map[[32]byte]*AllowanceParams
}

func New(cfg Config) *Ledger {
	if cfg.Classifier == nil {
		cfg.Classifier = DefaultClassifier{}
	}
	if cfg.Params == nil {
		cfg.Params = ParamsStore{}
	}
	return &Ledger{cfg: cfg, cache: map[[32]byte]*AllowanceParams{}}
}

// DefaultClassifier is the minimal Design 0 rule the ledger falls back on:
// signed accounts are tier 3, anonymous subjects tier 4, one share each.
type DefaultClassifier struct{}

func (DefaultClassifier) Classify(_ context.Context, _ allowance.Querier, s allowance.Subject, _ int64) (allowance.Standing, error) {
	if s.Signed {
		return allowance.Standing{Tier: allowance.TierSigned, WeightPPM: ppm, Root: s.ID, Source: "design0", Reason: "A signed account."}, nil
	}
	return allowance.Standing{Tier: allowance.TierAnonymous, WeightPPM: ppm, Root: s.ID, Source: "design0", Reason: "An anonymous caller, one subject per network."}, nil
}

func refuse(code string) error { return &allowance.Err{Code: code} }

// refuseDay is a refusal that clears at the next UTC midnight.
func refuseDay(code string, now int64) error {
	return &allowance.Err{Code: code, RetryAfter: int(86400 - mod(now, 86400))}
}

// ErrNotFound is a missing parameter version, hold or transfer.
var ErrNotFound = errors.New("ledger: not found")

// params is the allowance parameter set in effect, parsed once per body.
func (l *Ledger) params(ctx context.Context, q allowance.Querier, now int64) (*AllowanceParams, int64, error) {
	version, body, err := l.cfg.Params.Params(ctx, q, AllowanceNamespace, now)
	if err != nil {
		return nil, 0, err
	}
	key := sha256.Sum256(body)
	l.mu.Lock()
	p, ok := l.cache[key]
	l.mu.Unlock()
	if ok {
		return p, version, nil
	}
	parsed, err := ParseAllowanceParams(body)
	if err != nil {
		return nil, 0, err
	}
	l.mu.Lock()
	if len(l.cache) >= 16 {
		clear(l.cache)
	}
	l.cache[key] = &parsed
	l.mu.Unlock()
	return &parsed, version, nil
}

// Params is the allowance parameter set in effect at now and its version.
func (l *Ledger) Params(ctx context.Context, q allowance.Querier, now int64) (AllowanceParams, int64, error) {
	p, v, err := l.params(ctx, q, now)
	if err != nil {
		return AllowanceParams{}, 0, err
	}
	return p.Clone(), v, nil
}

func (l *Ledger) levers(ctx context.Context, q allowance.Querier, now int64) (allowance.Levers, error) {
	if l.cfg.Levers == nil {
		return allowance.Levers{Tier4SharePPM: -1}, nil
	}
	lv, err := l.cfg.Levers.Levers(ctx, q, now)
	if err != nil {
		return allowance.Levers{}, err
	}
	return lv, nil
}

// classify asks the classifier and normalises its answer: an unsigned subject
// is always tier 4, a signed one never is, weights are 0–1e9.
func (l *Ledger) classify(ctx context.Context, q allowance.Querier, s allowance.Subject, now int64) (allowance.Standing, error) {
	st, err := l.cfg.Classifier.Classify(ctx, q, s, now)
	if err != nil {
		return allowance.Standing{}, err
	}
	switch {
	case !s.Signed:
		st.Tier = allowance.TierAnonymous
	case st.Tier < allowance.TierTrusted || st.Tier > allowance.TierSigned:
		st.Tier = allowance.TierSigned
	}
	st.WeightPPM = clamp(st.WeightPPM, 0, 1000*ppm)
	if st.Root == "" {
		st.Root = s.ID
	}
	return st, nil
}

func (l *Ledger) resource(p *AllowanceParams, r allowance.Resource) (*ResourceParams, error) {
	rp, ok := p.Resources[r]
	if !ok {
		return nil, refuse("invalid_resource")
	}
	return rp, nil
}

// holdID is deterministic, so a retried Reserve finds its hold.
func holdID(account, requestKey string) string {
	h := sha256.Sum256([]byte("hold\x00" + account + "\x00" + requestKey))
	return hex.EncodeToString(h[:16])
}

func transferID(account, requestKey string) string {
	h := sha256.Sum256([]byte("transfer\x00" + account + "\x00" + requestKey))
	return hex.EncodeToString(h[:16])
}

// Integer helpers. Every amount is an int64 in the resource's unit.

func mod(a, b int64) int64 { return ((a % b) + b) % b }

func clamp(v, lo, hi int64) int64 { return max(lo, min(v, hi)) }

// mulDiv is floor(a × b / c) for a, b ≥ 0 and c > 0, saturating.
func mulDiv(a, b, c int64) int64 {
	if a <= 0 || b <= 0 || c <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi >= uint64(c) {
		return math.MaxInt64
	}
	q, _ := bits.Div64(hi, lo, uint64(c))
	if q > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(q)
}

// mulDivCeil is ceil(a × b / c) for a, b ≥ 0 and c > 0, saturating.
func mulDivCeil(a, b, c int64) int64 {
	if a <= 0 || b <= 0 || c <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi >= uint64(c) {
		return math.MaxInt64
	}
	q, r := bits.Div64(hi, lo, uint64(c))
	if r > 0 {
		q++
	}
	if q > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(q)
}
