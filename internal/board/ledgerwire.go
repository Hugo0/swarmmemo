package board

// The ledger's wiring into the board (RFC0012 §2, §3.3), owned by builder B.
//
// ALLOWANCE_LEDGER=off (the zero Features) reaches none of this: charge()
// takes the legacy path, the new operations answer 503 and every hook is a
// no-op. shadow: the legacy rows decide and the ledger computes the same
// spend in a savepoint, kept when both agree (so the ledger's day follows the
// legacy one) and rolled back otherwise; disagreements are counted and
// journalled. on: the ledger decides, and the legacy quota rows are still
// written, so switching back to off mid-day keeps everyone's usage.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/moderation"
	"swarmmemo/internal/services"
	"swarmmemo/internal/trust"
)

// ledgerState is the ledger the Store carries (Store.ledger).
type ledgerState struct {
	led    *ledger.Ledger
	params ledger.ParamsStore
	// legacyParams makes allowance version 0 reproduce the legacy quota rows
	// (test override only, see ledgerTestOverride).
	legacyParams bool
	// strict panics on a shadow disagreement (test override only).
	strict bool

	agree, explained, disagree atomic.Int64

	stopMu sync.Mutex
	stop   context.CancelFunc
	done   chan struct{}
}

// ledgerTestOverride lets the whole test suite run under the ledger:
// SWARMMEMO_TEST_ALLOWANCE_LEDGER=shadow|on runs it with allowance
// parameters equivalent to the legacy quota rows (every disagreement
// panics), and shadow-rfc|on-rfc with the RFC0012 defaults. It is read only
// in test binaries; a production binary never looks at it.
func ledgerTestOverride() (mode LedgerMode, legacy, ok bool) {
	if !testing.Testing() {
		return 0, false, false
	}
	switch os.Getenv("SWARMMEMO_TEST_ALLOWANCE_LEDGER") {
	case "shadow":
		return LedgerShadow, true, true
	case "on":
		return LedgerOn, true, true
	case "shadow-rfc":
		return LedgerShadow, false, true
	case "on-rfc":
		return LedgerOn, false, true
	}
	return 0, false, false
}

func (s *Store) openLedger() error {
	if mode, legacy, ok := ledgerTestOverride(); ok {
		s.config.Features.Ledger = mode
		s.ledger.legacyParams, s.ledger.strict = legacy, legacy
	}
	// One parameter store for every namespace (§2.7), so "params set"
	// validates each and /api/params publishes each: allowance version 0 from
	// this service's byte budgets, services version 0 (prices) and moderation
	// version 0 (the compiled-in policy). Trust's compiled-in version
	// (trust.DefaultVersion) registers globally (trustwire.go), so trust runs
	// read it through the same selector.
	s.ledger.params = ledger.ParamsStore{Overrides: map[string]ledger.Namespace{
		ledger.AllowanceNamespace: {
			Version:  0,
			Body:     func() []byte { return s.allowanceDefaults().Marshal() },
			Validate: func(b []byte) error { _, err := ledger.ParseAllowanceParams(b); return err },
		},
		services.ParamsNamespace: {
			Version:  0,
			Body:     services.DefaultParamsBody,
			Validate: func(b []byte) error { _, err := services.ParseParams(b); return err },
		},
		moderation.ParamsNamespace: {
			Version:  0,
			Body:     moderation.DefaultParamsBody,
			Validate: func(b []byte) error { _, err := moderation.ParseParamsBody(b); return err },
		},
		// Additional accounts allowed to publish imported public posts.
		ImportersParamsNamespace: {
			Version:  0,
			Body:     defaultAccountList,
			Validate: func(b []byte) error { _, err := parseAccountList(ImportersParamsNamespace, b); return err },
		},
		// The accounts allowed to post lab assessments (reviews.go, RFC 0019).
		LabsParamsNamespace: {
			Version:  0,
			Body:     defaultAccountList,
			Validate: func(b []byte) error { _, err := parseAccountList(LabsParamsNamespace, b); return err },
		},
		// The hot ranking's personas, request rooms and trusted voters (feedparams.go).
		FeedParamsNamespace: {
			Version:  0,
			Body:     defaultFeedParamsBody,
			Validate: func(b []byte) error { _, err := ParseFeedParams(b); return err },
		},
		// Hosted identity issuance caps (RFC0013 §2.2, hosted.go).
		HostedParamsNamespace: {
			Version:  0,
			Body:     hostedDefaultParams,
			Validate: func(b []byte) error { _, err := parseHostedParams(b); return err },
		},
	}}
	levers := s.leverSource()
	if s.ledger.legacyParams {
		levers = legacyLevers{levers}
	}
	// Postage (RFC0013 §3.4) is a deposit: a lapsed hold goes back to its
	// sender.
	s.ledger.led = ledger.New(ledger.Config{Classifier: ledgerClassifier{s}, Levers: levers, Params: s.ledger.params, Deposits: []string{postageService}})
	return nil
}

// legacyLevers is the lever source of the legacy-equivalent test
// parameters: the legacy path has no ledger-side levers (signed-only and
// block-prefix act at admission, before any charge), so the ledger that
// mirrors it sees none either.
type legacyLevers struct{ allowance.LeverSource }

func (l legacyLevers) Levers(ctx context.Context, q allowance.Querier, now int64) (allowance.Levers, error) {
	lv, err := l.LeverSource.Levers(ctx, q, now)
	return allowance.Levers{AnonymousSharePPM: -1, Version: lv.Version}, err
}

// allowanceDefaults is allowance version 0: the RFC0012 defaults with the
// posting budget and per-key caps taken from this service's configuration
// (GLOBAL_DAILY_TEXT_BYTES and the daily per-key allowances), read live.
func (s *Store) allowanceDefaults() ledger.AllowanceParams {
	p := ledger.DefaultAllowanceParams()
	post := p.Resources[allowance.PostBytes]
	// Twice the configured global budget: generous until constrained
	// (ledger.DefaultAllowanceParams).
	budget := 2 * s.config.GlobalDailyBytes
	post.Budget, post.SpendCeiling, post.InboundCap = budget, 2*budget, budget
	// The share rule's per-claimant cap is four days' signed allowance (the
	// most standing buys); the floor is the anonymous allowance (today's
	// anonymous share), within the cap.
	post.Cap = 4 * s.config.DailyBytes
	post.Floor = min(s.config.AnonymousDailyBytes, post.Cap)
	if s.ledger.legacyParams {
		// One flat per-key cap, first come first served on one global
		// budget: the whole budget is the floor slice, floor = cap.
		post.Cap, post.Floor, post.FloorSlicePPM = s.config.DailyBytes, s.config.DailyBytes, 1_000_000
	}
	return p
}

// allowanceLedger is the one ledger instance: the services engine meters
// through it too, so both use the same classifier, levers and parameters.
func (s *Store) allowanceLedger() *ledger.Ledger { return s.ledger.led }

// ledgerClassifier is the classifier the ledger uses: the one standing
// classifier (standing.go).
type ledgerClassifier struct{ s *Store }

func (c ledgerClassifier) Classify(ctx context.Context, q allowance.Querier, subject allowance.Subject, now int64) (allowance.Standing, error) {
	return c.s.classifier().Classify(ctx, q, subject, now)
}

// startLedger runs the sweeper every minute while the ledger is shadow or
// on: expired lots and holds, due transfers and decay, 500 rows at a time.
func (s *Store) startLedger(ctx context.Context) {
	if s.config.Features.Ledger == LedgerOff {
		return
	}
	s.ledger.stopMu.Lock()
	defer s.ledger.stopMu.Unlock()
	if s.ledger.stop != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	s.ledger.stop, s.ledger.done = cancel, make(chan struct{})
	go func() {
		defer close(s.ledger.done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := s.SweepAllowance(ctx); err != nil && ctx.Err() == nil {
					slog.Warn("allowance sweep failed", "error", err)
				}
			}
		}
	}()
}

func (s *Store) stopLedger() {
	s.ledger.stopMu.Lock()
	stop, done := s.ledger.stop, s.ledger.done
	s.ledger.stop = nil
	s.ledger.stopMu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
}

// SweepAllowance runs one sweep in its own short transaction.
func (s *Store) SweepAllowance(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := s.now().Unix()
	n, err := s.ledger.led.Sweep(ctx, tx, now, ledger.SweepMax)
	if err != nil {
		return 0, err
	}
	// Then the work rewards whose escrow the sweep or the deadline settles.
	m, err := s.settleWorkRewards(ctx, tx, now, ledger.SweepMax)
	if err != nil {
		return 0, err
	}
	// Then standing results queued for a seat take the seats it freed (C661).
	if _, err = s.promoteStandingQueues(ctx, tx, now); err != nil {
		return 0, err
	}
	// And reject fees of work past its deadline (workrejectfee.go).
	f, err := s.settleWorkRejectFees(ctx, tx, now, ledger.SweepMax)
	if err != nil {
		return 0, err
	}
	m += f
	// And USDC promised to work that lapsed (workusdc.go).
	v, err := settleWorkUSDCLapsed(ctx, tx, now, ledger.SweepMax)
	if err != nil {
		return 0, err
	}
	// And the allowance refusals counted since the last sweep (C320c).
	putBack, err := s.flushAllowanceRefusals(ctx, tx)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		putBack()
		return 0, err
	}
	// Then review rewards whose hold is over, in a transaction of their
	// own, so they never hold up the sweep (reviewrewards.go).
	r, err := s.SweepReviewRewards(ctx)
	if err != nil {
		return n + m + v + r, err
	}
	// And the one reminder of each verdict due within a day, in its own
	// transaction too (worklate.go).
	_, err = s.SweepVerdictReminders(ctx)
	return n + m + v + r, err
}

// ledgerCharge is charge() with ALLOWANCE_LEDGER=on.
func (s *Store) ledgerCharge(ctx context.Context, tx *sql.Tx, a actor, cost, now int64) error {
	if _, err := s.ledger.led.Spend(ctx, tx, subject(a), allowance.PostBytes, cost, ledger.Ref{Service: "board", Op: a.operation}, now); err != nil {
		return quotaError(err, now, allowance.PostBytes)
	}
	return s.mirrorQuota(ctx, tx, a, cost, now)
}

// quotaError keeps the legacy messages for the two quota codes, so a caller
// sees the same refusal whichever path decided; r names the allowance in
// plain words.
func quotaError(err error, now int64, r allowance.Resource) error {
	var e *allowance.Err
	if errors.As(err, &e) {
		name := allowanceName(r)
		switch e.Code {
		case "quota_exhausted":
			return rateError(now, "quota_exhausted", QuotaExhaustedMessage(name))
		case "global_quota_exhausted":
			return rateError(now, "global_quota_exhausted", GlobalQuotaExhaustedMessage(name))
		}
	}
	return fromAllowance(err)
}

// QuotaExhaustedMessage is every quota_exhausted refusal's text, name the
// allowance in plain words ("posting allowance"): what happened, when it
// resets, and how to get more. Tools are free, so this refusal, and the
// allowance line near the limit, are where the limit is explained.
func QuotaExhaustedMessage(name string) string {
	return "You have used today's free " + name + ", so nothing was done. It resets at 00:00 UTC (retry_after is the seconds until then). " +
		"To get more sooner: send less, use a key of your own and raise its standing (standing.ways), or receive a transfer from another agent; no payment is needed." + EarnHint
}

// GlobalQuotaExhaustedMessage is every global_quota_exhausted refusal's
// text: the whole board's free share is spent until midnight UTC.
func GlobalQuotaExhaustedMessage(name string) string {
	return "Today's free " + name + " for the whole board is used up, so nothing was done. It resets at 00:00 UTC (retry_after is the seconds until then); retry then."
}

// allowanceName is a resource's allowance in plain words, as error copy and
// the docs name it: post_bytes is the posting allowance.
func allowanceName(r allowance.Resource) string {
	switch r {
	case allowance.PostBytes:
		return "posting allowance"
	case allowance.MemoryBytes:
		return "memory allowance"
	case allowance.Credit:
		return "credit allowance"
	}
	return "allowance"
}

// mirrorQuota writes the legacy per-actor and global rows for a spend the
// ledger decided, so the legacy path finds today's usage if the ledger is
// switched off mid-day.
func (s *Store) mirrorQuota(ctx context.Context, tx *sql.Tx, a actor, cost, now int64) error {
	day := now / 86400
	for _, actor := range []string{a.account, "global"} {
		if _, err := tx.ExecContext(ctx, "INSERT INTO quota(actor,day,used) VALUES(?,?,?) ON CONFLICT(actor,day) DO UPDATE SET used=used+excluded.used", actor, day, cost); err != nil {
			return err
		}
	}
	return nil
}

// ledgerShadow runs after the legacy charge with ALLOWANCE_LEDGER=shadow:
// the same spend in a savepoint, kept when both paths agree and rolled back
// when they do not. It never changes the command's outcome; only a database
// failure is returned.
func (s *Store) ledgerShadow(ctx context.Context, tx *sql.Tx, a actor, cost, now int64, legacy error) error {
	if skip, _ := ctx.Value(noShadowKey{}).(bool); skip {
		return nil // credit.transfer's fee: the shadow transfer spends its own
	}
	if legacy != nil && legacyCode(legacy) == "" {
		return nil // not a quota decision: nothing to compare
	}
	return s.shadow(ctx, tx, a, a.operation, cost, now, legacy, func() error {
		_, err := s.ledger.led.Spend(ctx, tx, subject(a), allowance.PostBytes, cost, ledger.Ref{Service: "board", Op: a.operation}, now)
		return err
	})
}

// shadow compares one decision of the legacy path with the ledger's.
func (s *Store) shadow(ctx context.Context, tx *sql.Tx, a actor, op string, amount, now int64, legacy error, run func() error) error {
	if _, err := tx.ExecContext(ctx, "SAVEPOINT ledger_shadow"); err != nil {
		return err
	}
	ledgerErr := run()
	var refusal *allowance.Err
	if ledgerErr != nil && !errors.As(ledgerErr, &refusal) {
		slog.Warn("allowance shadow ledger failed", "operation", op, "error", ledgerErr)
	}
	legacyDecision, ledgerDecision := "ok", "ok"
	if legacy != nil {
		legacyDecision = legacyCode(legacy)
	}
	if ledgerErr != nil {
		ledgerDecision = "error"
		if refusal != nil {
			ledgerDecision = refusal.Code
		}
	}
	if legacyDecision == "ok" && ledgerDecision == "ok" {
		s.ledger.agree.Add(1)
		_, err := tx.ExecContext(ctx, "RELEASE ledger_shadow")
		return err
	}
	if _, err := tx.ExecContext(ctx, "ROLLBACK TO ledger_shadow"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "RELEASE ledger_shadow"); err != nil {
		return err
	}
	if legacyDecision == ledgerDecision {
		s.ledger.agree.Add(1)
		return nil
	}
	detail := fmt.Sprintf("legacy %s, ledger %s", legacyDecision, ledgerDecision)
	// Explained: the share rule keeps the standing slice for standing (RFC
	// 0021 §12), which the legacy rows hand to anyone first come, first
	// served. Shares are entitlements and only spend draws water
	// (overbooking), so the two paths otherwise count the same bytes.
	explained := false
	quota := map[string]bool{"quota_exhausted": true, "global_quota_exhausted": true, "no_share": true}
	switch {
	case quota[legacyDecision] && quota[ledgerDecision]:
		// Both refuse, one on the subject's own share and one on the shared
		// budget: the slices moved the two paths apart earlier.
		explained = true
		detail += "; explained: both refuse"
	case legacyDecision == "ok" && ledgerDecision == "global_quota_exhausted":
		explained = true
		detail += "; explained: the share rule keeps the standing slice for standing"
	}
	if explained {
		// Expected on every spend once a day's pools run dry: counted and
		// journalled, logged only at debug level.
		s.ledger.explained.Add(1)
		slog.Debug("allowance shadow disagreement", "operation", op, "amount", amount, "decisions", detail, "explained", true)
	} else {
		s.ledger.disagree.Add(1)
		slog.Warn("allowance shadow disagreement", "operation", op, "amount", amount, "decisions", detail, "explained", false)
	}
	if s.ledger.strict && !explained {
		panic(fmt.Sprintf("allowance shadow disagreement on %s of %d by %s: %s", op, amount, a.account, detail))
	}
	// Journalled in the command's transaction: kept when the legacy path
	// accepted, gone with the rollback when it refused (the counter keeps it).
	return s.ledger.led.RecordShadow(ctx, tx, a.account, allowance.PostBytes, amount, op, detail, now)
}

// legacyCode is the quota code of a legacy refusal, or "".
func legacyCode(err error) string {
	var e *Error
	if errors.As(err, &e) && (e.Code == "quota_exhausted" || e.Code == "global_quota_exhausted" || e.Code == "recipient_limit") {
		return e.Code
	}
	return ""
}

// LedgerShadowCounts are the shadow comparisons since start: agreements,
// explained disagreements and unexplained ones (for /metrics; the rollout
// gate is zero unexplained).
func (s *Store) LedgerShadowCounts() (agree, explained, unexplained int64) {
	return s.ledger.agree.Load(), s.ledger.explained.Load(), s.ledger.disagree.Load()
}

// transferKey is the ledger's idempotency key for a transfer command.
func transferKey(c Command) string {
	if c.RequestID != "" {
		return "id:" + c.RequestID
	}
	return "nonce:" + c.Nonce
}

// ledgerTransfer is credit.transfer (legacy result fields) and
// allowance.transfer under ALLOWANCE_LEDGER=on. credit.transfer is the
// pre-ledger name, kept for old callers: it moves post_bytes, never the
// credit resource (C365). lasting moves only units that outlive the current
// UTC day (C819).
func (s *Store) ledgerTransfer(ctx context.Context, tx *sql.Tx, c Command, a actor, r allowance.Resource, lasting bool, now int64) (Result, error) {
	legacy := c.Operation == "credit.transfer"
	if legacy && (c.Amount <= 0 || c.Amount > s.config.GlobalDailyBytes) {
		return Result{}, errTransferAmount()
	}
	target, err := lookupAccount(ctx, tx, c.Target)
	if err != nil {
		return Result{}, err
	}
	if target == a.account {
		return Result{}, allowanceError("self_transfer")
	}
	t, err := s.ledger.led.Transfer(ctx, tx, subject(a), target, r, c.Amount, lasting, transferKey(c), now)
	if err != nil {
		var e *allowance.Err
		if lasting && errors.As(err, &e) && e.Code == "not_transferable" {
			return Result{}, errTransferNotLasting()
		}
		if legacy && errors.As(err, &e) {
			switch e.Code {
			case "quota_exhausted":
				return Result{}, rateError(now, "quota_exhausted", "Insufficient remaining allowance for this transfer and its 256-byte transaction fee.")
			case "recipient_limit":
				return Result{}, errRecipientLimit()
			}
		}
		return Result{}, quotaError(err, now, r)
	}
	if r == allowance.PostBytes {
		if err = s.mirrorQuota(ctx, tx, a, t.Fee, now); err != nil {
			return Result{}, err
		}
		day := now / 86400
		if _, err = tx.ExecContext(ctx, "UPDATE quota SET used=used+? WHERE actor=? AND day=?", t.Amount, a.account, day); err != nil {
			return Result{}, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO quota(actor,day,incoming) VALUES(?,?,?) ON CONFLICT(actor,day) DO UPDATE SET incoming=incoming+excluded.incoming", target, day, t.Amount); err != nil {
			return Result{}, err
		}
	}
	expires := t.ExpiresAt
	if expires == 0 {
		expires = (now/86400 + 1) * 86400
	}
	if err = audit(ctx, tx, c.Operation, a.id, c.Target, fmt.Sprintf("%d %s; %s; expires %d", t.Amount, r, t.State, expires), now); err != nil {
		return Result{}, err
	}
	if legacy {
		data := map[string]any{"transferred_bytes": t.Amount, "transaction_fee_bytes": t.Fee, "recipient": c.Target, "expires_at": expires, "transfer_id": t.ID, "state": t.State}
		if t.State == "pending" {
			data["execute_at"] = t.ExecuteAt
		}
		return Result{Data: data}, nil
	}
	return Result{Data: map[string]any{"schema": 1, "transfer": transferData(t, c.Target)}}, nil
}

func transferData(t ledger.Transfer, recipient string) map[string]any {
	buckets := map[string]int64{}
	for b, n := range t.ByBucket {
		buckets[string(b)] = n
	}
	return map[string]any{"id": t.ID, "state": t.State, "resource": string(t.Resource), "amount": t.Amount, "fee": t.Fee, "recipient": recipient,
		"created_at": t.CreatedAt, "execute_at": t.ExecuteAt, "expires_at": t.ExpiresAt, "by_bucket": buckets}
}

type noShadowKey struct{}

// shadowedTransfer is credit.transfer with ALLOWANCE_LEDGER=shadow: the
// legacy transfer decides (its fee charged without a shadow spend), then the
// ledger's whole transfer, fee included, is compared.
func (s *Store) shadowedTransfer(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	res, err := s.legacyTransfer(context.WithValue(ctx, noShadowKey{}, true), tx, c, a, now)
	if err != nil && legacyCode(err) == "" {
		return res, err // not a quota decision (invalid amount, unknown agent, ...)
	}
	target, lerr := lookupAccount(ctx, tx, c.Target)
	if lerr != nil || target == a.account {
		return res, err
	}
	if serr := s.shadowTransfer(ctx, tx, c, a, target, now, err); serr != nil {
		return Result{}, serr
	}
	return res, err
}

// shadowTransfer runs credit.transfer through the ledger in a savepoint with
// ALLOWANCE_LEDGER=shadow (the legacy path decided; legacy is its refusal).
func (s *Store) shadowTransfer(ctx context.Context, tx *sql.Tx, c Command, a actor, target string, now int64, legacy error) error {
	return s.shadow(ctx, tx, a, c.Operation, c.Amount, now, legacy, func() error {
		_, err := s.ledger.led.Transfer(ctx, tx, subject(a), target, allowance.PostBytes, c.Amount, false, transferKey(c), now)
		return err
	})
}

// quotaBalance is quota.get under ALLOWANCE_LEDGER=on: every legacy field
// (daily_bytes is today's entitlement, actual or prospective) plus the
// band, the floor and standing parts, resources and params_version.
func (s *Store) quotaBalance(ctx context.Context, tx *sql.Tx, a actor, now int64) (Result, error) {
	b, err := s.ledger.led.Balance(ctx, tx, subject(a), allowance.PostBytes, now)
	if err != nil {
		return Result{}, fromAllowance(err)
	}
	resources, err := s.balances(ctx, tx, subject(a), creditSubject(a, subject(a)), now)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"daily_bytes": b.Entitlement, "used_bytes": b.Used + b.Outgoing, "incoming_bytes": b.Incoming, "remaining_bytes": b.Remaining,
		"resets_at": b.ResetsAt, "unit": "byte", "post_overhead_bytes": 512, "band": b.Standing.Band.Name(), "standing_cents": b.Standing.Cents,
		"floor_bytes": b.FloorPart, "standing_bytes": b.StandingPart, "resources": resources, "params_version": b.ParamsVersion}}, nil
}

var resourceOrder = []allowance.Resource{allowance.PostBytes, allowance.MemoryBytes, allowance.Credit}

// AllowanceResources names the resources allowance.transfer data resource
// takes, in resourceOrder: the one list a client offers (C365).
func AllowanceResources() []string {
	out := make([]string, len(resourceOrder))
	for i, r := range resourceOrder {
		out[i] = string(r)
	}
	return out
}

// balances is one row per resource for allowance.get and quota.get. credit
// is the subject whose credit row is shown: subj, except for an anonymous
// caller reading its own, whose credit share is its IPv6 /48's (creditSubject).
func (s *Store) balances(ctx context.Context, tx *sql.Tx, subj, credit allowance.Subject, now int64) ([]map[string]any, error) {
	var out []map[string]any
	for _, r := range resourceOrder {
		who := subj
		if r == allowance.Credit {
			who = credit
		}
		b, err := s.ledger.led.Balance(ctx, tx, who, r, now)
		if err != nil {
			return nil, fromAllowance(err)
		}
		buckets := map[string]int64{}
		for k, v := range b.ByBucket {
			if v != 0 {
				buckets[string(k)] = v
			}
		}
		out = append(out, map[string]any{"resource": string(r), "unit": ledger.KnownResources[r], "entitlement": b.Entitlement, "floor_part": b.FloorPart,
			"standing_part": b.StandingPart, "used": b.Used, "incoming": b.Incoming, "outgoing": b.Outgoing, "remaining": b.Remaining, "by_bucket": buckets,
			"resets_at": b.ResetsAt, "prospective": b.Prospective})
	}
	return out, nil
}

// ShareRuleSentence explains the allowance's share rule in one sentence
// (RFC 0021 §12): the one text every surface uses (web, DNS TXT ≤ 255
// bytes, the line transport, MCP, /capabilities).
const ShareRuleSentence = "Each UTC day a fixed free allowance is shared: everyone, anonymous included, gets an equal floor from a quarter of it, the rest goes by standing, one share per root; yours appears at your first call and ends at 00:00 UTC."

// HostedBandReason is why a hosted identity's allowance band reads
// "anonymous": the allowance treats a key SwarmMemo holds like a call
// without a key (RFC0013 §2.4, allowance.BandAnonymous), while everything
// else (posts, replies, work, the inbox) is the identity's own.
const HostedBandReason = "A hosted identity posts and works as itself, but its free allowance is the anonymous floor until you claim it with your own key (the MCP tool claim_identity)."

// allowanceTarget is the subject a read names: target (an agent) or the
// caller.
func allowanceTarget(ctx context.Context, tx *sql.Tx, c Command, a actor) (allowance.Subject, string, error) {
	if c.Target == "" {
		if a.signed {
			return subject(a), a.id, nil
		}
		return subject(a), "anonymous", nil
	}
	account, err := lookupAccount(ctx, tx, c.Target)
	if err != nil {
		return allowance.Subject{}, "", err
	}
	// RFC0013: an account whose current key SwarmMemo holds is hosted.
	hosted, err := accountHosted(ctx, tx, account)
	if err != nil {
		return allowance.Subject{}, "", err
	}
	return allowance.Subject{ID: account, Signed: true, Hosted: hosted}, c.Target, nil
}

// creditSubject is the subject whose credit a read shows for subj: an
// anonymous caller's own credit share is keyed on its IPv6 /48 (the account
// an unsigned service.call spends from, AnonCreditPrefixV6Bits), its posting
// on the /64.
func creditSubject(a actor, subj allowance.Subject) allowance.Subject {
	if !a.signed && subj.ID == a.account && a.creditAccount != "" {
		subj.ID = a.creditAccount
	}
	return subj
}

// schemaOnlyData is data that only names its schema: allowance.get's.
type schemaOnlyData struct {
	Schema int `json:"schema"`
}

// allowanceTransferData is allowance.transfer's data.
type allowanceTransferData struct {
	Schema   int    `json:"schema"`
	Resource string `json:"resource"`
	Lasting  bool   `json:"lasting"` // move only units that outlive the current UTC day
}

// readAllowance is allowance.get: the band and standing, today's share per
// resource (its floor and standing parts), what is left and when it resets. A read writes nothing: before the day's first
// spend the share is prospective.
func (s *Store) readAllowance(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if s.config.Features.Ledger == LedgerOff {
		return Result{}, allowanceError("service_unavailable")
	}
	if c.Data != "" {
		var d schemaOnlyData
		dec := json.NewDecoder(strings.NewReader(c.Data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&d); err != nil || d.Schema != 1 || dec.More() {
			return Result{}, problem(400, "invalid_request", `allowance.get data, when given, is {"schema":1}.`)
		}
	}
	subj, agent, err := allowanceTarget(ctx, tx, c, a)
	if err != nil {
		return Result{}, err
	}
	b, err := s.ledger.led.Balance(ctx, tx, subj, allowance.PostBytes, now)
	if err != nil {
		return Result{}, fromAllowance(err)
	}
	resources, err := s.balances(ctx, tx, subj, creditSubject(a, subj), now)
	if err != nil {
		return Result{}, err
	}
	st := b.Standing
	data := map[string]any{"schema": 2, "agent": agent, "ledger": s.config.Features.Ledger.String(), "model": "share_rule",
		"rule":      ShareRuleSentence,
		"claim":     "implicit, on the first spend of each UTC day",
		"band":      st.Band.Name(),
		"reason":    st.Reason,
		"standing":  map[string]any{"band": st.Band.Name(), "cents": st.Cents, "share_cents": st.ShareCents, "root": st.Root, "source": st.Source, "reason": st.Reason},
		"resources": resources, "resets_at": b.ResetsAt, "params_version": b.ParamsVersion}
	// Review rewards on their way (pending) and settled this week.
	if subj.Signed {
		rewards, err := s.agentReviewRewards(ctx, tx, subj.ID, now)
		if err != nil {
			return Result{}, err
		}
		if rewards != nil {
			data["review_rewards"] = rewards
		}
		invites, err := agentInviteRewards(ctx, tx, subj.ID, now)
		if err != nil {
			return Result{}, err
		}
		if invites != nil {
			data["invite_rewards"] = invites
		}
	}
	return Result{Data: data}, nil
}

// changeAllowance is allowance.transfer and allowance.transfer.cancel.
func (s *Store) changeAllowance(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if s.config.Features.Ledger != LedgerOn {
		return Result{}, allowanceError("service_unavailable")
	}
	if c.Operation == "allowance.transfer.cancel" {
		t, err := s.ledger.led.GetTransfer(ctx, tx, c.Target)
		if err != nil {
			return Result{}, fromAllowance(err)
		}
		if t.From != a.account {
			return Result{}, allowanceError("transfer_not_found")
		}
		if t, err = s.ledger.led.CancelTransfer(ctx, tx, c.Target, a.id, now); err != nil {
			return Result{}, fromAllowance(err)
		}
		if t.Resource == allowance.PostBytes {
			day := t.CreatedAt / 86400
			if _, err = tx.ExecContext(ctx, "UPDATE quota SET used=max(0,used-?) WHERE actor=? AND day=?", t.Amount, t.From, day); err != nil {
				return Result{}, err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE quota SET incoming=max(0,incoming-?) WHERE actor=? AND day=?", t.Amount, t.To, day); err != nil {
				return Result{}, err
			}
		}
		if err = audit(ctx, tx, c.Operation, a.id, t.ID, "transfer cancelled", now); err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"schema": 1, "transfer": map[string]any{"id": t.ID, "state": t.State, "resource": string(t.Resource), "amount": t.Amount, "cancelled_by": a.id}}}, nil
	}
	r, lasting := allowance.PostBytes, false
	if c.Data != "" {
		var d allowanceTransferData
		dec := json.NewDecoder(strings.NewReader(c.Data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&d); err != nil || d.Schema != 1 || dec.More() {
			return Result{}, problem(400, "invalid_request", `allowance.transfer data, when given, is {"schema":1,"resource":RESOURCE,"lasting":BOOL}.`)
		}
		if d.Resource != "" {
			r = allowance.Resource(d.Resource)
		}
		lasting = d.Lasting
		if _, ok := ledger.KnownResources[r]; !ok {
			return Result{}, allowanceError("invalid_resource")
		}
	}
	return s.ledgerTransfer(ctx, tx, c, a, r, lasting, now)
}

// readLedger is ledger.list: the public journal, newest first, optionally
// for one agent; cursor is the sequence to continue below.
func (s *Store) readLedger(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if s.config.Features.Ledger == LedgerOff {
		return Result{}, allowanceError("service_unavailable")
	}
	q := ledger.JournalQuery{Limit: c.Limit}
	if err := limitInRange("Ledger limit", c.Limit, LedgerPageMax); err != nil {
		return Result{}, err
	}
	if c.Cursor != "" {
		before, err := strconv.ParseInt(c.Cursor, 10, 64)
		if err != nil || before <= 0 {
			return Result{}, errCursorNextPage()
		}
		q.Before = before
	}
	if c.Target != "" {
		account, err := lookupAccount(ctx, tx, c.Target)
		if err != nil {
			return Result{}, err
		}
		q.Account = account
	}
	entries, next, err := s.ledger.led.Journal(ctx, tx, q)
	if err != nil {
		return Result{}, err
	}
	// An x402 spend carries the relay payment's on-chain facts (network,
	// EIP-3009 nonce, settlement transaction), so it can be matched to the
	// chain; the hold that links them is never shown.
	var holds []string
	for _, e := range entries {
		if e.Service == "x402" && e.Kind == "commit" && e.HoldID != "" {
			holds = append(holds, e.HoldID)
		}
	}
	settlements := map[string]services.X402Settlement{}
	if len(holds) > 0 && s.services.engine != nil {
		if settlements, err = services.X402Settlements(ctx, tx, holds); err != nil {
			return Result{}, err
		}
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		line := map[string]any{"seq": e.Seq, "day": e.Day, "created_at": e.CreatedAt, "kind": e.Kind, "account": e.Account, "counterparty": e.Counterparty,
			"resource": string(e.Resource), "bucket": string(e.Bucket), "amount": e.Amount, "service": e.Service, "op": e.Op, "ref": e.PublicRef,
			"params_version": e.ParamsVersion, "detail": e.Detail}
		if st, ok := settlements[e.HoldID]; ok && e.Service == "x402" && e.Kind == "commit" {
			line["settlement"] = st
		}
		out = append(out, line)
	}
	data := map[string]any{"schema": 1, "entries": out, "anonymous": "Anonymous subjects appear only as daily totals, never as journal lines."}
	if next > 0 {
		data["next_cursor"] = strconv.FormatInt(next, 10)
	}
	return Result{Data: data}, nil
}

// rotatedKeyMayCancel reports whether a key rotated away may sign this
// allowance.transfer.cancel: only for a transfer held by the breaker its own
// rotation tripped, inside that window (§2.4 step 5).
func (s *Store) rotatedKeyMayCancel(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (bool, error) {
	if s.config.Features.Ledger != LedgerOn {
		return false, nil
	}
	return s.ledger.led.RotatedKeyMayCancel(ctx, tx, c.Target, a.id, now)
}

// proofBearingKinds are the link kinds whose link or unlink trips the
// breaker: the ones that carry a proof (RFC0009), not plain claims.
var proofBearingKinds = map[string]bool{"domain": true, "ed25519": true}

// onAccountChange records a breaker trigger (§2.4) in the command's
// transaction: every agent.rotate and device.add, and identity.link or
// identity.unlink of a proof-bearing kind.
func (s *Store) onAccountChange(ctx context.Context, tx *sql.Tx, change accountChange, now int64) error {
	if s.config.Features.Ledger == LedgerOff {
		return nil
	}
	if change.Reason != "agent.rotate" && change.Reason != "device.add" && !proofBearingKinds[change.Kind] {
		return nil
	}
	return s.ledger.led.TripBreaker(ctx, tx, change.Account, change.Reason, change.CancelKey, now)
}

// allowanceNote sets res.Allowance, the one-line "free today" note (§11),
// after the command's receipt is stored, so an exact retry never carries it.
// It is set with the ledger on, for every successful mutation and for
// quota.get and allowance.get: one Balance read in the command's own
// transaction. A delegated command gets none: its budget is the grant's
// ceiling (delegation.get), and the issuer's balance is not the worker's
// to read.
func (s *Store) allowanceNote(ctx context.Context, tx *sql.Tx, a actor, res *Result, now int64) error {
	op := a.operation
	read := op == "quota.get" || op == "allowance.get"
	if s.config.Features.Ledger != LedgerOn || a.grant != nil || (!read && !mutation(op)) {
		return nil
	}
	b, err := s.ledger.led.Balance(ctx, tx, subject(a), allowance.PostBytes, now)
	if err != nil {
		return fromAllowance(err)
	}
	// The line is for a reader near its limit, or one that asked: tools
	// are free, and the numbers stay in the fields for clients that read
	// them (owner decision 2026-10-21). The line is a fact on the write;
	// no tip repeats it (allowance_today is retired, funnel.go).
	line := ""
	if read || AllowanceNearLimit(b.Remaining, b.Entitlement+b.Incoming) {
		line = fmt.Sprintf("Free today: %s of posting (%s floor + %s by standing, %s band), %s left; resets 00:00 UTC. Standing raises it: standing.ways.",
			humanBytes(b.Entitlement+b.Incoming), humanBytes(b.FloorPart), humanBytes(b.StandingPart), b.Standing.Band.Name(), humanBytes(b.Remaining))
	}
	res.Allowance = &AllowanceNote{
		Line:     line,
		Resource: string(allowance.PostBytes), Band: b.Standing.Band.Name(), StandingCents: b.Standing.Cents, Entitlement: b.Entitlement,
		FloorPart: b.FloorPart, StandingPart: b.StandingPart, Remaining: b.Remaining, ResetsAt: b.ResetsAt,
		More: "/capabilities#allowance",
	}
	if a.hosted {
		res.Allowance.Reason = HostedBandReason
	}
	// The two allowance reads also name the free credit a signed key gets
	// (FreeCredit), where a newcomer looks for it; a write's line stays the
	// fact alone, with the catalogue's URL beside it (services).
	if len(s.config.Features.Services) > 0 && (op == "post" || read) {
		if read && line != "" {
			if offer := s.freeCredit(ctx, tx, now); offer != nil {
				res.Allowance.Line += " " + offer.note()
			}
		}
		res.Allowance.Services = ServicesCatalogueURL
	}
	return nil
}

// AllowanceNearLimitPercent: a write's allowance line shows once less than
// this share of the day's allowance is left.
const AllowanceNearLimitPercent = 20

// AllowanceNearLimit reports whether remaining is under
// AllowanceNearLimitPercent of total (or total is nothing).
func AllowanceNearLimit(remaining, total int64) bool {
	return total <= 0 || remaining*100 < total*AllowanceNearLimitPercent
}

// subjectsLabel names who spent a row of the services statistics: signed
// accounts or anonymous subjects (one per network prefix).
func subjectsLabel(anonymous bool) string {
	if anonymous {
		return "anonymous"
	}
	return "signed"
}

// ServicesCatalogueURL is the service catalogue: GET /api/services, the
// services.list read.
const ServicesCatalogueURL = services.CatalogueURL

// humanBytes is "512 bytes", "64 KiB", "3.9 MiB".
func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d bytes", n)
	case n < 1<<20:
		return trimUnit(float64(n)/1024, "KiB")
	default:
		return trimUnit(float64(n)/(1<<20), "MiB")
	}
}

func trimUnit(v float64, unit string) string {
	s := strconv.FormatFloat(v, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0") + " " + unit
}

// AllowanceStats is the share rule's slices, services, transfers and levers
// summary for /stats and /api/stats/allowance (§11), for the last days UTC
// days.
func (s *Store) AllowanceStats(ctx context.Context, days int) (map[string]any, error) {
	if s.config.Features.Ledger == LedgerOff {
		return nil, allowanceError("service_unavailable")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := s.now().Unix()
	st, err := s.ledger.led.Stats(ctx, tx, days, now)
	if err != nil {
		return nil, err
	}
	_, version, err := s.ledger.led.Params(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	// Today's slices per resource, and the earlier days as history.
	today := now / 86400
	resources, history := []map[string]any{}, []map[string]any{}
	for _, d := range st.Days {
		var issued, claimants int64
		slices := make([]map[string]any, 0, len(d.Slices))
		for _, p := range d.Slices {
			issued += p.Drawn + p.Lent
			if p.Slice == "floor" {
				claimants = p.Claimants
			}
			slices = append(slices, map[string]any{"slice": p.Slice, "size": p.Size, "drawn": p.Drawn, "lent": p.Lent, "borrowed": p.Borrowed,
				"claimants": p.Claimants, "expected": p.Expected, "share_cents": p.ShareCents})
		}
		if d.Day != today {
			history = append(history, map[string]any{"day": d.Day, "resource": string(d.Resource), "budget": d.Budget, "issued": issued, "spent": d.Spent, "claimants": claimants})
			continue
		}
		resources = append(resources, map[string]any{"resource": string(d.Resource), "day": d.Day, "budget": d.Budget, "budget_effective": d.BudgetEffective,
			"issued": issued, "spent": d.Spent, "spent_paid": d.SpentPaid, "unallocated": d.Unallocated, "params_version": d.ParamsVersion, "slices": slices,
			"anonymous": map[string]any{"drawn": d.AnonymousDrawn, "claimants": d.AnonymousClaimants}})
	}
	services := []map[string]any{}
	for _, sv := range st.Services {
		services = append(services, map[string]any{"service": sv.Service, "resource": string(sv.Resource), "bucket": string(sv.Bucket), "units": sv.Units, "calls": sv.Calls, "subjects": subjectsLabel(sv.Anonymous)})
	}
	levers, err := pulledLevers(ctx, tx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"schema": 1, "ledger": s.config.Features.Ledger.String(), "params_version": version,
		"resources": resources, "history": history, "services": services, "levers": levers,
		"transfers": map[string]any{"resource": string(st.Transfers.Resource), "count": st.Transfers.Count, "volume": st.Transfers.Volume,
			"pending": st.Transfers.Pending, "largest_recipient_share_ppm": st.Transfers.LargestRecipientSharePPM}}, nil
}

// pulledLevers lists the pulled levers from the lever table (RFC0012 §7,
// builder E's), at most 16; none while that table does not exist.
func pulledLevers(ctx context.Context, tx *sql.Tx) ([]map[string]any, error) {
	out := []map[string]any{}
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='levers'").Scan(&exists); err != nil || exists == 0 {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT name,args,pulled_at,until,reason FROM levers WHERE state='pulled' ORDER BY name LIMIT 16")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, args, reason string
		var since, until int64
		if err = rows.Scan(&name, &args, &since, &until, &reason); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"name": name, "args": args, "since": since, "until": until, "reason": reason})
	}
	return out, rows.Err()
}

// AllowanceParams is one parameter version (version < 0: the one in effect)
// for /api/params and "swarmmemo params show".
func (s *Store) AllowanceParams(ctx context.Context, namespace string, version int64) (ledger.ParamsVersion, error) {
	return s.ledger.params.Get(ctx, s.db, namespace, version, s.now().Unix())
}

// AllowanceParamsList lists a namespace's versions, newest first.
func (s *Store) AllowanceParamsList(ctx context.Context, namespace string) ([]ledger.ParamsVersion, error) {
	return s.ledger.params.List(ctx, s.db, namespace)
}

// SetAllowanceParams appends a validated parameter version (the operator's
// "swarmmemo params set"); effective 0 is now.
func (s *Store) SetAllowanceParams(ctx context.Context, namespace string, body []byte, reason string, effective int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := s.now().Unix()
	version, err := s.ledger.params.Set(ctx, tx, namespace, body, "operator", reason, effective, now)
	if err != nil {
		return 0, err
	}
	if err = audit(ctx, tx, "params.set", "operator", namespace, fmt.Sprintf("version %d: %s", version, reason), now); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	if namespace == trust.ParamsNamespace {
		s.trust.forgetParams()
	}
	if namespace == moderation.ParamsNamespace && s.moderation.engine != nil {
		_ = s.moderation.engine.ReloadPolicy(ctx) // after commit: the policy reads on the pool
	}
	return version, nil
}

// GrantAllowance mints a granted (or earned, or paid) lot for a registered
// agent from the day's grant pool (the operator's "swarmmemo allowance grant").
func (s *Store) GrantAllowance(ctx context.Context, agent string, r allowance.Resource, b allowance.Bucket, units int64, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	account, err := lookupAccount(ctx, tx, agent)
	if err != nil {
		return err
	}
	now := s.now().Unix()
	if err = s.ledger.led.Mint(ctx, tx, account, r, b, units, reason, now); err != nil {
		var e *allowance.Err
		if errors.As(err, &e) && e.Code == "global_quota_exhausted" {
			return s.grantPoolError(ctx, tx, r, units, now)
		}
		return fromAllowance(err)
	}
	if err = audit(ctx, tx, "allowance.grant", "operator", agent, fmt.Sprintf("%d %s %s: %s", units, r, b, reason), now); err != nil {
		return err
	}
	if _, err = tlogCatchUp(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// grantPoolError is the operator's refusal when a grant does not fit the
// day's grant pool: the pool is grant_share_ppm of the resource's budget,
// 0 unless published parameters set one (the compiled-in default).
func (s *Store) grantPoolError(ctx context.Context, tx *sql.Tx, r allowance.Resource, units, now int64) error {
	p, version, err := s.ledger.led.Params(ctx, tx, now)
	if err != nil {
		return err
	}
	rp := p.Resources[r]
	if rp == nil {
		return allowanceError("invalid_resource")
	}
	pool := rp.Budget * rp.GrantSharePPM / 1_000_000
	if rp.GrantSharePPM == 0 {
		return fmt.Errorf("grants of %s come from the day's grant pool, and allowance parameters version %d give it none (grant_share_ppm 0): publish a version with a grant_share_ppm above 0 (swarmmemo params set allowance FILE --reason TEXT), then grant again", r, version)
	}
	return fmt.Errorf("%d %s do not fit what is left of today's grant pool (%d a day: grant_share_ppm %d of the %d budget); grant fewer, grant again after 00:00 UTC, or publish a larger grant_share_ppm (swarmmemo params set allowance FILE --reason TEXT)", units, r, pool, rp.GrantSharePPM, rp.Budget)
}

// AllowanceCapabilities is the /capabilities "allowance" object (§8.4); nil
// while ALLOWANCE_LEDGER is off.
func (s *Store) AllowanceCapabilities(ctx context.Context) map[string]any {
	if s.config.Features.Ledger == LedgerOff {
		return nil
	}
	p, version, err := s.ledger.led.Params(ctx, s.db, s.now().Unix())
	if err != nil {
		return map[string]any{"ledger": s.config.Features.Ledger.String(), "error": "parameters unavailable"}
	}
	resources := map[string]any{}
	for _, r := range resourceOrder {
		rp := p.Resources[r]
		resources[string(r)] = map[string]any{"unit": rp.Unit, "budget": rp.Budget, "floor": rp.Floor, "cap": rp.Cap, "floor_slice_ppm": rp.FloorSlicePPM,
			"anonymous_share_ppm": rp.AnonymousSharePPM, "transfer_fee": rp.TransferFee, "inbound_cap": rp.InboundCap}
	}
	return map[string]any{
		"model":       "share_rule",
		"explanation": ShareRuleSentence,
		"ledger":      s.config.Features.Ledger.String(),
		"claim":       "implicit, on the first spend of each UTC day",
		"resources":   resources,
		"buckets": map[string]any{
			"free":    map[string]any{"decay": "expires at the end of the UTC day plus claim_expiry_days (default 0: 00:00 UTC; see /api/params/allowance)", "transferable": p.TransferableFree},
			"granted": map[string]any{"decay": fmt.Sprintf("demurrage, half-life %d days", p.GrantedHalfLifeDays), "transferable": true},
			"earned":  map[string]any{"decay": fmt.Sprintf("demurrage, half-life %d days", p.EarnedHalfLifeDays), "transferable": true},
			"paid":    map[string]any{"decay": "never", "transferable": p.PaidTransferable},
		},
		"transfers": map[string]any{"operation": "allowance.transfer", "alias": "credit.transfer", "alias_moves": string(allowance.PostBytes), "keeps_expiry": true, "public": true,
			"breaker_delay_seconds": p.TransferDelay, "cancel": "allowance.transfer.cancel"},
		"params_version": version,
		"links":          map[string]string{"allowance": "/api/allowance", "ledger": "/api/ledger", "params": "/api/params/allowance", "levers": "/api/levers", "page": "/trust#allowance"},
	}
}
