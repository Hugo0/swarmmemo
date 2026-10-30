package board

// Trust module wiring (RFC 0021, the one trust graph): the paged input
// reader, the nightly runner, trust.get and the standing breakdown, the
// per-viewer view, the public run reads, the penalty log and the
// distribution for /stats. With TRUST off nothing here runs, trust.get
// answers 503, and every standing is 0.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/judge"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/reviews"
	"swarmmemo/internal/trust"
)

// trustState is the trust runner the Store carries (Store.trust).
type trustState struct {
	mu     sync.Mutex // one run at a time in this process
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// voteGen counts votes cast, so the votes' standing weights are read
	// again after one (standingVoteWeights); weights holds them.
	voteGen atomic.Int64
	weights voteWeightCache
	// endorsements reads the endorsement records (E's EndorsementPage);
	// replaced only by in-package tests.
	endorsements func(ctx context.Context, after int64, limit int) ([]EndorsementRecord, int64, error)
	// computed is called between computation and writes; tests only.
	computed func()
	// pages counts input pages read, for tests and the run log.
	pages atomic.Int64
	// params caches the trust parameters.
	params standingCache
	// graph caches the public trust network (TrustGraph).
	graph trustGraphCache
	// views caches per-viewer standing by (viewer, run), and flow the
	// latest run's flow graph it reads.
	views viewerCache
}

// TrustRunAfter is how long after 00:00 UTC the nightly run starts.
const TrustRunAfter = 30 * time.Minute

func (s *Store) openTrust() error { return nil }

// startTrust starts the nightly run while TRUST is on.
func (s *Store) startTrust(ctx context.Context) {
	if s.config.Features.Trust == TrustOff {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	s.trust.cancel = cancel
	s.trust.wg.Add(1)
	go func() {
		defer s.trust.wg.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			s.trustNightly(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Store) stopTrust() {
	if s.trust.cancel != nil {
		s.trust.cancel()
	}
	s.trust.wg.Wait()
}

// trustNightly runs today's run once it is due and none was recorded for
// today's as-of time.
func (s *Store) trustNightly(ctx context.Context) {
	now := s.now().UTC()
	asOf := now.Truncate(24 * time.Hour)
	if now.Sub(asOf) < TrustRunAfter {
		return
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM trust_runs WHERE as_of=? AND state<>'running'", asOf.Unix()).Scan(&n); err != nil || n > 0 {
		return
	}
	_, _ = s.RunTrust(ctx)
}

// trustDB gives the trust module short transactions on the one connection.
type trustDB struct{ s *Store }

func (d trustDB) Read(ctx context.Context, fn func(allowance.Querier) error) error {
	tx, err := d.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(tx)
}

func (d trustDB) Write(ctx context.Context, fn func(allowance.Querier) error) error {
	tx, err := d.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	// A penalty log entry is a transparency log leaf in the same commit.
	if _, err = tlogCatchUp(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// The trust namespace's compiled-in version, registered for every
// ParamsStore so /api/params/trust and trust runs share one selector.
func init() {
	ledger.RegisterNamespace(trust.ParamsNamespace, ledger.Namespace{
		Version:  trust.DefaultVersion,
		Body:     func() []byte { return trust.DefaultParams().Body() },
		Validate: func(b []byte) error { _, err := trust.ParseParams(trust.DefaultVersion, b); return err },
	})
}

// trustParams reads the trust parameters in effect, chosen by the one
// selector /api/params/trust reports as "current" (ledger.ParamsStore.Current):
// the newest stored version whose effective time has passed, unless the
// compiled-in version (trust.DefaultVersion) is newer, which then applies:
// deploying the binary that ships a version activates it, with no parameter
// write (C320a). A stored body that does not parse stops the run.
func trustParams(ctx context.Context, q allowance.Querier, now int64) (trust.Params, error) {
	if ok, err := tableExists(ctx, q, "params"); err != nil || !ok {
		return trust.DefaultParams(), err
	}
	v, err := ledger.ParamsStore{}.Current(ctx, q, trust.ParamsNamespace, now)
	if err != nil {
		return trust.Params{}, err
	}
	if v.CompiledIn {
		return trust.DefaultParams(), nil
	}
	return trust.ParseParams(v.Version, v.Body)
}

func tableExists(ctx context.Context, q allowance.Querier, name string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&n)
	return n > 0, err
}

// RunTrust computes and records one run as of today's 00:00 UTC.
func (s *Store) RunTrust(ctx context.Context) (trust.RunSummary, error) {
	if s.config.Features.Trust == TrustOff {
		return trust.RunSummary{}, allowanceError("service_unavailable")
	}
	s.trust.mu.Lock()
	defer s.trust.mu.Unlock()
	now := s.now().Unix()
	asOf := now - now%86400
	db := trustDB{s}
	var p trust.Params
	if err := db.Read(ctx, func(q allowance.Querier) (err error) { p, err = trustParams(ctx, q, now); return err }); err != nil {
		return trust.RunSummary{}, err
	}
	sum, err := trust.Run(ctx, db, trustInputs{s}, p, asOf, now, s.trust.computed)
	s.trust.views.forget()
	return sum, err
}

// WriteTrustInputs writes the snapshot a run as of today's 00:00 UTC would
// read, as JSONL: what a verifier needs to recompute it.
func (s *Store) WriteTrustInputs(ctx context.Context, w io.Writer) error {
	snap, err := s.TrustInputs(ctx)
	if err != nil {
		return err
	}
	return snap.WriteJSONL(w)
}

// TrustInputs reads the snapshot a run as of today's 00:00 UTC would read.
// It only reads, in short read transactions.
func (s *Store) TrustInputs(ctx context.Context) (trust.Snapshot, error) {
	now := s.now().Unix()
	asOf := now - now%86400
	db := trustDB{s}
	var p trust.Params
	if err := db.Read(ctx, func(q allowance.Querier) (err error) { p, err = trustParams(ctx, q, now); return err }); err != nil {
		return trust.Snapshot{}, err
	}
	snap := trust.Snapshot{Params: p, Meta: trust.Meta{Schema: trust.SnapshotSchema, AsOf: asOf}}
	if err := (trustInputs{s}).Read(ctx, asOf, p, snap.Add); err != nil {
		return trust.Snapshot{}, err
	}
	snap.Meta.AsOf, snap.Meta.Schema = asOf, trust.SnapshotSchema
	return snap, nil
}

// trustInputs is the paged input reader (trust.Inputs): every page is at most
// trust.PageRows rows in its own short read transaction.
type trustInputs struct{ s *Store }

func (in trustInputs) page(ctx context.Context, fn func(q allowance.Querier) error) error {
	in.s.trust.pages.Add(1)
	return trustDB{in.s}.Read(ctx, fn)
}

// houseFilter wraps emit so no record of a house agent reaches a trust
// run: a record any of whose accounts is one is dropped, except a post's
// reply_to_account, which is cleared (the reply is the replier's own).
func houseFilter(house map[string]bool, emit func(trust.Record) error) func(trust.Record) error {
	return func(r trust.Record) error {
		for _, account := range []string{r.Account, r.Voter, r.From, r.To, r.LinkAccount} {
			if account != "" && house[account] {
				return nil
			}
		}
		if r.Type != "post" && r.Type != "review" && house[r.Target] {
			return nil
		}
		if house[r.ReplyToAccount] {
			r.ReplyToAccount = ""
		}
		return emit(r)
	}
}

// usdCents is micro-USD (a credit, a USDC atomic unit) as cents, rounded
// down.
func usdCents(micro int64) int64 { return micro / (CreditsPerUSD / 100) }

// Read emits the meta record, then posts, endorsements, proofs, accounts,
// breakers, transfers, claims, payments, acts, reviews and their owners, the
// penalty log and the previous run's carry.
func (in trustInputs) Read(ctx context.Context, asOf int64, p trust.Params, emit func(trust.Record) error) error {
	s := in.s
	const rows = trust.PageRows
	windowStart := asOf - p.WindowDays*86400
	meta := trust.Record{Type: "meta", Schema: trust.SnapshotSchema, AsOf: asOf}
	accounts := map[string]bool{}
	exists := map[string]bool{}
	// C662: house agents (apps.go) accrue no standing and give none: every
	// record of theirs is left out, and a reply to one names no account.
	var house map[string]bool
	if err := in.page(ctx, func(q allowance.Querier) error {
		ok, err := tableExists(ctx, q, "identity_house")
		if err != nil || !ok {
			return err
		}
		house, err = houseAccounts(ctx, q)
		return err
	}); err != nil {
		return err
	}
	if len(house) > 0 {
		emit = houseFilter(house, emit)
	}
	if err := in.page(ctx, func(q allowance.Querier) error {
		for _, name := range []string{"account_breakers", "ledger_transfers", "allowance_claims", "ledger_entries", "domain_registrations",
			"service_calls", "x402_payments", "credit_topups", "standing_assessments", "work_usdc", "offering_calls", "trust_penalty_log", "proof_bonds"} {
			ok, err := tableExists(ctx, q, name)
			if err != nil {
				return err
			}
			exists[name] = ok
		}
		if err := q.QueryRowContext(ctx, "SELECT coalesce(max(seq),0) FROM events WHERE created_at<?", asOf).Scan(&meta.EventsSeq); err != nil {
			return err
		}
		if exists["ledger_entries"] {
			if err := q.QueryRowContext(ctx, "SELECT coalesce(max(seq),0) FROM ledger_entries WHERE created_at<?", asOf).Scan(&meta.LedgerSeq); err != nil {
				return err
			}
		}
		levers, err := s.leverSource().Levers(ctx, q, asOf)
		if err == nil && levers.PauseNewKeys {
			meta.PauseNewKeysSince = levers.PauseNewKeysSince
		}
		return nil
	}); err != nil {
		return err
	}

	// Posts: signed, visible, native, non-edit posts in public rooms.
	var after int64
	if err := in.page(ctx, func(q allowance.Querier) error {
		return q.QueryRowContext(ctx, "SELECT coalesce(min(seq),1)-1 FROM events WHERE created_at>=?", windowStart).Scan(&after)
	}); err != nil {
		return err
	}
	for {
		var batch []trust.Record
		if err := in.page(ctx, func(q allowance.Querier) error {
			r, err := q.QueryContext(ctx, `SELECT e.seq,e.id,e.account,e.created_at,e.reply_to,coalesce(p.account,'') FROM events e JOIN rooms r ON r.name=e.room
 LEFT JOIN events p ON p.id=e.reply_to AND p.hidden=0 AND p.public_key<>''
 WHERE e.seq>? AND r.visibility='public' AND e.hidden=0 AND e.public_key<>'' AND e.kind NOT IN ('imported','simulation') AND e.supersedes=''
 AND e.created_at>=? AND e.created_at<? ORDER BY e.seq LIMIT ?`, after, windowStart, asOf, rows)
			if err != nil {
				return err
			}
			defer r.Close()
			for r.Next() {
				var rec trust.Record
				if err = r.Scan(&after, &rec.ID, &rec.Account, &rec.CreatedAt, &rec.ReplyTo, &rec.ReplyToAccount); err != nil {
					return err
				}
				rec.Type = "post"
				batch = append(batch, rec)
			}
			return r.Err()
		}); err != nil {
			return err
		}
		for _, rec := range batch {
			accounts[rec.Account] = true
			if err := emit(rec); err != nil {
				return err
			}
		}
		if len(batch) < rows {
			break
		}
	}

	// Endorsement records, resolved to accounts, page by page.
	read := s.trust.endorsements
	if read == nil {
		read = s.EndorsementPage
	}
	for after = 0; ; {
		records, next, err := read(ctx, after, rows)
		if err != nil {
			var be *Error
			if errors.As(err, &be) && be.Code == "service_unavailable" {
				break // endorsement records are not built yet: no edges
			}
			return err
		}
		s.trust.pages.Add(1)
		var batch []trust.Record
		if err = in.page(ctx, func(q allowance.Querier) error {
			for _, e := range records {
				if e.CreatedAt >= asOf || e.CreatedAt < windowStart {
					continue
				}
				rec := trust.Record{Type: "endorsement", Seq: e.Seq, Kind: e.Type, Value: int64(e.Value), CreatedAt: e.CreatedAt}
				if e.Type == "vote" && (e.Signature == nil || *e.Signature == "") {
					rec.Kind = "unsigned"
				}
				if rec.Kind != "vote" && rec.Kind != "vouch" && rec.Kind != "unsigned" {
					continue // legacy votes weigh nothing
				}
				var err error
				if rec.Voter, err = resolveAccount(ctx, q, e.Voter); err != nil {
					return err
				}
				if e.MessageID != "" {
					var root string
					err = q.QueryRowContext(ctx, `SELECT o.id,o.account FROM events e JOIN events o ON o.id=coalesce(nullif(e.origin,''),e.id) WHERE e.id=?`, e.MessageID).Scan(&root, &rec.Target)
					if errors.Is(err, sql.ErrNoRows) {
						continue
					}
					if err != nil {
						return err
					}
					rec.MessageID = root
				} else if rec.Target, err = resolveAccount(ctx, q, e.Target); err != nil {
					return err
				}
				if rec.Voter == "" || rec.Target == "" {
					continue
				}
				if rec.Seq > meta.EndorsementsSeq {
					meta.EndorsementsSeq = rec.Seq
				}
				batch = append(batch, rec)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, rec := range batch {
			accounts[rec.Voter], accounts[rec.Target] = true, true
			if err = emit(rec); err != nil {
				return err
			}
		}
		if len(records) == 0 || next <= after {
			break
		}
		after = next
	}

	// Proofs: the links of each public account's current key. An account that
	// agent.get and trust.get treat as not found (private rooms only) is left
	// out, so the published snapshot never names it. A linked ed25519 key's
	// account is named only when it is public too. A domain proof carries its
	// registrable domain's registration time when known (domain_registrations).
	registered := map[string]int64{}
	if exists["domain_registrations"] {
		if err := in.page(ctx, func(q allowance.Querier) error {
			r, err := q.QueryContext(ctx, "SELECT domain,registered_at FROM domain_registrations WHERE registered_at>0 AND registered_at<?", asOf)
			if err != nil {
				return err
			}
			defer r.Close()
			for r.Next() {
				var d string
				var t int64
				if err = r.Scan(&d, &t); err != nil {
					return err
				}
				registered[d] = t
			}
			return r.Err()
		}); err != nil {
			return err
		}
	}
	// A wallet or github proof carries its root and assessment
	// (standing_assessments, standingroots.go), the assessment only while it
	// is no older than proof_fresh_days.
	assessedSQL := "'',0,0"
	assessedJoin := ""
	if exists["standing_assessments"] {
		assessedSQL = "coalesce(sa.root,''),coalesce(sa.assessed,0),coalesce(sa.assessed_at,0)"
		assessedJoin = " LEFT JOIN standing_assessments sa ON sa.agent=l.agent AND sa.kind=l.kind AND sa.value=l.value"
	}
	freshAssessment := asOf - p.ProofFreshDays*86400
	for after = 0; ; {
		var batch []trust.Record
		scanned := 0
		if err := in.page(ctx, func(q allowance.Querier) error {
			r, err := q.QueryContext(ctx, `SELECT l.rowid,i.account,l.kind,l.value,l.state,l.created_at,l.checked_at,
 coalesce((SELECT o.account FROM identities o WHERE l.kind='ed25519' AND o.public_key=l.value AND `+publicAccountSQL("o.account")+`),''),
 `+publicAccountSQL("i.account")+`,`+assessedSQL+`
 FROM identity_links l JOIN identities i ON i.id=l.agent`+assessedJoin+` WHERE l.rowid>? AND i.successor='' AND i.revoked_at=0 AND NOT (l.kind='x25519' AND l.state='lapsed') ORDER BY l.rowid LIMIT ?`, after, rows)
			if err != nil {
				return err
			}
			defer r.Close()
			for r.Next() {
				rec := trust.Record{Type: "proof"}
				var public bool
				var root string
				var assessed, assessedAt int64
				if err = r.Scan(&after, &rec.Account, &rec.Kind, &rec.LinkValue, &rec.State, &rec.CreatedAt, &rec.CheckedAt, &rec.LinkAccount, &public, &root, &assessed, &assessedAt); err != nil {
					return err
				}
				scanned++
				if !public || rec.CreatedAt >= asOf {
					continue
				}
				rec.CheckedAt = linkCheckedAsOf(rec.State, rec.CheckedAt, asOf)
				if rec.Kind == "domain" && len(registered) > 0 {
					rec.RegisteredAt = registered[strings.TrimPrefix(trust.DomainRoot(rec.LinkValue, p.DomainSuffixes), "domain:")]
				}
				if row, ok := p.Proofs[rec.Kind]; ok && row.Assess != "" && rec.Kind != "domain" {
					rec.Root = root
					if rec.Kind == "wallet" {
						rec.Root = walletRoot(rec.LinkValue)
					}
					if assessedAt >= freshAssessment && assessedAt < asOf {
						rec.Assessed = assessed
					}
				}
				batch = append(batch, rec)
			}
			return r.Err()
		}); err != nil {
			return err
		}
		for _, rec := range batch {
			accounts[rec.Account] = true
			if err := emit(rec); err != nil {
				return err
			}
		}
		if scanned < rows {
			break
		}
	}

	// Standing bonds (C326, proofbond.go): each bond held as of the run is a
	// proof of kind bond, its root bond:ID, assessed at its face value in
	// cents, checked at the run (the board holds it); a bond asked back
	// (refund_owed) or refunded counts no more. A penalty burns its root.
	for afterID := ""; p.Proofs["bond"].Assess != "" && exists["proof_bonds"]; {
		var batch []trust.Record
		scanned := 0
		if err := in.page(ctx, func(q allowance.Querier) error {
			r, err := q.QueryContext(ctx, `SELECT b.id,i.account,b.amount,b.settled_at,`+publicAccountSQL("i.account")+`
 FROM proof_bonds b JOIN identities i ON i.id=b.agent WHERE b.id>? AND (b.state='held' OR b.state IN ('refund_owed','refunded') AND b.refund_requested_at>=?)
 AND b.settled_at>0 AND b.settled_at<? ORDER BY b.id LIMIT ?`, afterID, asOf, asOf, rows)
			if err != nil {
				return err
			}
			defer r.Close()
			for r.Next() {
				rec := trust.Record{Type: "proof", Kind: "bond", State: "verified"}
				var amount int64
				var public bool
				if err = r.Scan(&rec.LinkValue, &rec.Account, &amount, &rec.CreatedAt, &public); err != nil {
					return err
				}
				scanned++
				afterID = rec.LinkValue
				if !public {
					continue
				}
				rec.Root, rec.Assessed, rec.CheckedAt = "bond:"+rec.LinkValue, amount/10_000, asOf-1
				batch = append(batch, rec)
			}
			return r.Err()
		}); err != nil {
			return err
		}
		for _, rec := range batch {
			accounts[rec.Account] = true
			if err := emit(rec); err != nil {
				return err
			}
		}
		if scanned < rows {
			break
		}
	}

	// Proof of work: one pow root per current public key, its work as the
	// assessment, checked at its last solution.
	for after = 0; p.Proofs["pow"].Assess != "" && exists["standing_assessments"]; {
		var batch []trust.Record
		scanned := 0
		if err := in.page(ctx, func(q allowance.Querier) error {
			r, err := q.QueryContext(ctx, `SELECT sa.rowid,i.account,sa.root,sa.assessed,sa.created_at,sa.assessed_at,`+publicAccountSQL("i.account")+`
 FROM standing_assessments sa JOIN identities i ON i.id=sa.agent WHERE sa.rowid>? AND sa.kind='pow' AND i.successor='' AND i.revoked_at=0 ORDER BY sa.rowid LIMIT ?`, after, rows)
			if err != nil {
				return err
			}
			defer r.Close()
			for r.Next() {
				rec := trust.Record{Type: "proof", Kind: "pow", LinkValue: PowValue, State: "verified"}
				var at int64
				var public bool
				if err = r.Scan(&after, &rec.Account, &rec.Root, &rec.Assessed, &rec.CreatedAt, &at, &public); err != nil {
					return err
				}
				scanned++
				if !public || rec.CreatedAt >= asOf {
					continue
				}
				if at < asOf {
					rec.CheckedAt = at
				}
				batch = append(batch, rec)
			}
			return r.Err()
		}); err != nil {
			return err
		}
		for _, rec := range batch {
			accounts[rec.Account] = true
			if err := emit(rec); err != nil {
				return err
			}
		}
		if scanned < rows {
			break
		}
	}

	day := asOf / 86400
	fundedFrom := asOf - p.HalfLifeDays*86400
	// paged reads query with (cursor, args..., rows) until a short page.
	paged := func(query string, args []any, scan func(r *sql.Rows) (trust.Record, int64, error)) error {
		var cursor int64
		for {
			var batch []trust.Record
			if err := in.page(ctx, func(q allowance.Querier) error {
				bound := make([]any, 0, len(args)+2)
				bound = append(append(append(bound, cursor), args...), rows)
				r, err := q.QueryContext(ctx, query, bound...)
				if err != nil {
					return err
				}
				defer r.Close()
				for r.Next() {
					rec, next, err := scan(r)
					if err != nil {
						return err
					}
					cursor = next
					if rec.Type != "" {
						batch = append(batch, rec)
					}
				}
				return r.Err()
			}); err != nil {
				return err
			}
			for _, rec := range batch {
				for _, a := range []string{rec.Account, rec.From, rec.To} {
					if a != "" {
						accounts[a] = true
					}
				}
				if err := emit(rec); err != nil {
					return err
				}
			}
			if len(batch) < rows {
				return nil
			}
		}
	}
	if exists["account_breakers"] {
		// kind is the breaker's reason: version 11 sizes its window by it
		// (breaker_trust_days, RFC 0021 §2.2).
		if err := paged("SELECT seq,account,reason,started_at,trust_until FROM account_breakers WHERE seq>? AND trust_until>? AND started_at<? ORDER BY seq LIMIT ?", []any{asOf, asOf},
			func(r *sql.Rows) (trust.Record, int64, error) {
				rec := trust.Record{Type: "breaker"}
				var seq int64
				err := r.Scan(&seq, &rec.Account, &rec.Kind, &rec.StartedAt, &rec.TrustUntil)
				return rec, seq, err
			}); err != nil {
			return err
		}
	}
	// Transfers within funded_days (H ≥ the funnel's window): a transfer
	// links its two accounts (independence, §7) and feeds the funnel.
	if exists["ledger_transfers"] {
		if err := paged("SELECT rowid,from_account,to_account,amount,done_at FROM ledger_transfers WHERE rowid>? AND state='done' AND done_at>=? AND done_at<? ORDER BY rowid LIMIT ?",
			[]any{min(fundedFrom, asOf-p.Detectors.FunnelDays*86400), asOf},
			func(r *sql.Rows) (trust.Record, int64, error) {
				rec := trust.Record{Type: "transfer"}
				var id int64
				err := r.Scan(&id, &rec.From, &rec.To, &rec.Amount, &rec.CreatedAt)
				return rec, id, err
			}); err != nil {
			return err
		}
	}
	// Claims and spends of post_bytes for the funnel detector.
	if exists["allowance_claims"] {
		if err := paged("SELECT rowid,subject,day,granted FROM allowance_claims WHERE rowid>? AND resource='post_bytes' AND day>=? AND day<? AND subject NOT LIKE 'anon:%' ORDER BY rowid LIMIT ?",
			[]any{day - p.Detectors.FunnelDays, day},
			func(r *sql.Rows) (trust.Record, int64, error) {
				rec := trust.Record{Type: "claim"}
				var id int64
				err := r.Scan(&id, &rec.Account, &rec.Day, &rec.Claimed)
				return rec, id, err
			}); err != nil {
			return err
		}
	}
	if exists["ledger_entries"] {
		if err := paged("SELECT seq,account,day,abs(amount) FROM ledger_entries WHERE seq>? AND kind='spend' AND resource='post_bytes' AND day>=? AND day<? AND account NOT LIKE 'anon:%' ORDER BY seq LIMIT ?",
			[]any{day - p.Detectors.FunnelDays, day},
			func(r *sql.Rows) (trust.Record, int64, error) {
				rec := trust.Record{Type: "claim"}
				var seq int64
				err := r.Scan(&seq, &rec.Account, &rec.Day, &rec.Spent)
				return rec, seq, err
			}); err != nil {
			return err
		}
	}
	if err := in.readPayments(ctx, asOf, fundedFrom, exists, paged); err != nil {
		return err
	}
	if err := in.readActs(ctx, asOf, windowStart, paged); err != nil {
		return err
	}
	if err := in.readJudgements(ctx, asOf, p, paged, emit); err != nil {
		return err
	}
	// The public penalty log, whole (bounded: detectors and the operator
	// write it), and the latest run's carry.
	if exists["trust_penalty_log"] {
		for cursor := int64(0); cursor >= 0; {
			var batch []trust.Record
			if err := in.page(ctx, func(q allowance.Querier) (err error) {
				batch, cursor, err = trust.PenaltyRecords(ctx, q, asOf, cursor, rows)
				return err
			}); err != nil {
				return err
			}
			for _, rec := range batch {
				if err := emit(rec); err != nil {
					return err
				}
			}
		}
	}
	var carry []trust.Record
	if err := in.page(ctx, func(q allowance.Querier) (err error) { carry, err = trust.LatestCarry(ctx, q); return err }); err != nil {
		return err
	}
	for _, rec := range carry {
		if err := emit(rec); err != nil {
			return err
		}
	}

	// Accounts: first public appearance, 500 per page.
	list := make([]string, 0, len(accounts))
	for a := range accounts {
		list = append(list, a)
	}
	sort.Strings(list)
	for start := 0; start < len(list); start += 500 {
		chunk := list[start:min(start+500, len(list))]
		var batch []trust.Record
		if err := in.page(ctx, func(q allowance.Querier) error {
			for _, a := range chunk {
				var first int64
				if err := q.QueryRowContext(ctx, `SELECT coalesce(min(t),0) FROM (SELECT min(e.created_at) AS t FROM events e JOIN rooms r ON r.name=e.room WHERE e.account=? AND r.visibility='public' AND e.hidden=0
 UNION ALL SELECT min(au.created_at) FROM audit au JOIN identities ai ON ai.id=au.actor WHERE ai.account=? AND au.operation IN ('agent.register','agent.profile.publish'))`, a, a).Scan(&first); err != nil {
					return err
				}
				if first > 0 && first < asOf {
					batch = append(batch, trust.Record{Type: "account", Account: a, FirstSeen: first})
				}
			}
			return nil
		}); err != nil {
			return err
		}
		for _, rec := range batch {
			if err := emit(rec); err != nil {
				return err
			}
		}
	}
	return emit(meta)
}

// readPayments emits the money of RFC 0021 §7 between public accounts, in
// cents: credit bought from SwarmMemo (credit_topups, to ""), at any time
// (the run counts what came in since its carry); and within funded_days,
// payments to other accounts: USDC work rewards paid (requester → worker,
// with the worker's payout address), paid offering calls (caller → owner,
// with the owner's address) and x402 calls paid to an address another
// account topped up from (readPaidCalls). Private accounts are never named.
func (in trustInputs) readPayments(ctx context.Context, asOf, fundedFrom int64, exists map[string]bool,
	paged func(string, []any, func(*sql.Rows) (trust.Record, int64, error)) error) error {
	if exists["credit_topups"] {
		if err := paged(`SELECT seq,id,account,amount,coalesce(nullif(settled_at,0),created_at) FROM credit_topups WHERE seq>? AND state='credited' AND coalesce(nullif(settled_at,0),created_at)<?
 AND `+publicAccountSQL("account")+` ORDER BY seq LIMIT ?`, []any{asOf}, func(r *sql.Rows) (trust.Record, int64, error) {
			rec := trust.Record{Type: "payment", Kind: "topup"}
			var seq, micro int64
			err := r.Scan(&seq, &rec.ID, &rec.Account, &micro, &rec.CreatedAt)
			if rec.Amount = usdCents(micro); rec.Amount <= 0 {
				rec.Type = ""
			}
			return rec, seq, err
		}); err != nil {
			return err
		}
	}
	if exists["work_usdc"] {
		if err := paged(`SELECT rowid,work_id,requester,worker,amount,pay_to,settled_at FROM work_usdc WHERE rowid>? AND state='paid' AND tx_hash<>'' AND worker<>'' AND settled_at>=? AND settled_at<?
 AND `+publicAccountSQL("requester")+` AND `+publicAccountSQL("worker")+` ORDER BY rowid LIMIT ?`, []any{fundedFrom, asOf}, func(r *sql.Rows) (trust.Record, int64, error) {
			rec := trust.Record{Type: "payment", Kind: "work_usdc"}
			var rowid, micro int64
			var payTo string
			err := r.Scan(&rowid, &rec.ID, &rec.Account, &rec.To, &micro, &payTo, &rec.CreatedAt)
			if payTo != "" {
				rec.LinkValue = walletRoot(payTo)
			}
			if rec.Amount = usdCents(micro); rec.Amount <= 0 {
				rec.Type = ""
			}
			return rec, rowid, err
		}); err != nil {
			return err
		}
	}
	if exists["offering_calls"] {
		if err := paged(`SELECT seq,id,caller,account,amount,pay_to,created_at FROM offering_calls WHERE seq>? AND state IN ('paid','answered','overdue') AND caller<>'' AND tx_hash<>'' AND created_at>=? AND created_at<?
 AND `+publicAccountSQL("caller")+` AND `+publicAccountSQL("account")+` ORDER BY seq LIMIT ?`, []any{fundedFrom, asOf}, func(r *sql.Rows) (trust.Record, int64, error) {
			rec := trust.Record{Type: "payment", Kind: "offering"}
			var seq, micro int64
			var payTo string
			err := r.Scan(&seq, &rec.ID, &rec.Account, &rec.To, &micro, &payTo, &rec.CreatedAt)
			if payTo != "" {
				rec.LinkValue = walletRoot(payTo)
			}
			if rec.Amount = usdCents(micro); rec.Amount <= 0 {
				rec.Type = ""
			}
			return rec, seq, err
		}); err != nil {
			return err
		}
	}
	if exists["service_calls"] && exists["x402_payments"] {
		return in.readPaidCalls(ctx, fundedFrom, asOf, exists, paged)
	}
	return nil
}

// readPaidCalls emits, per public account, day and payee account, the paid
// and earned credit committed to x402 calls whose resource paid an address
// the payee topped up credit from, when naming the payee reveals nothing
// new: the payer itself, or an account a public transfer (or bounty reward)
// linked to the payer within funded_days. Which wallet funded which account
// is not published otherwise.
func (in trustInputs) readPaidCalls(ctx context.Context, fundedFrom, asOf int64, exists map[string]bool,
	paged func(string, []any, func(*sql.Rows) (trust.Record, int64, error)) error) error {
	if !exists["credit_topups"] {
		return nil
	}
	linked := "0"
	if exists["ledger_transfers"] {
		linked = `EXISTS(SELECT 1 FROM ledger_transfers x WHERE x.state='done' AND x.done_at>=` + strconv.FormatInt(fundedFrom, 10) + ` AND x.done_at<` + strconv.FormatInt(asOf, 10) + `
 AND ((x.from_account=le.account AND x.to_account=t.account) OR (x.from_account=t.account AND x.to_account=le.account)))`
	}
	payee := `coalesce((SELECT t.account FROM credit_topups t WHERE t.state='credited' AND lower(t.payer)=lower(p.pay_to)
 AND t.account<>le.account AND ` + linked + ` ORDER BY t.account LIMIT 1),'')`
	return paged(`SELECT le.seq,le.account,`+payee+`,abs(le.amount),le.created_at,lower(p.pay_to)
 FROM ledger_entries le JOIN service_calls sc ON sc.hold_id=le.hold_id AND sc.service='x402' JOIN x402_payments p ON p.account=sc.account AND p.request_key=sc.request_key
 WHERE le.seq>? AND le.kind='commit' AND le.service='x402' AND le.resource='credit' AND le.bucket IN ('paid','earned') AND le.hold_id<>''
 AND le.created_at>=? AND le.created_at<? AND le.account NOT LIKE 'anon:%' AND `+publicAccountSQL("le.account")+` ORDER BY le.seq LIMIT ?`,
		[]any{fundedFrom, asOf}, func(r *sql.Rows) (trust.Record, int64, error) {
			rec := trust.Record{Type: "payment", Kind: "x402"}
			var seq, micro int64
			var payTo string
			err := r.Scan(&seq, &rec.Account, &rec.To, &micro, &rec.CreatedAt, &payTo)
			rec.ID = "x402:" + strconv.FormatInt(seq, 10)
			if rec.Amount = usdCents(micro); rec.Amount <= 0 || rec.To == "" {
				rec.Type = ""
			}
			if payTo != "" {
				rec.LinkValue = walletRoot(payTo)
			}
			return rec, seq, err
		})
}

// workAcceptActsSQL selects cols from every work.accept that is a judgement
// (RFC 0020 §1): the accepting key's account (ia.account) of the worker
// (w.worker), on public, visible, non-simulated work items that stayed
// accepted, between public accounts; t is the transition. where adds
// conditions (" AND ...", binding its own parameters first). The trust
// run's snapshot and the board's reputation read it.
func workAcceptActsSQL(cols, where string) string {
	return "SELECT " + cols + ` FROM work_transitions t JOIN works w ON w.id=t.work_id
 JOIN events e ON e.id=w.id JOIN rooms r ON r.name=e.room JOIN identities ia ON ia.id=t.author
 WHERE t.operation='work.accept' AND w.state='accepted' AND w.worker<>'' AND r.visibility='public' AND e.hidden=0 AND e.kind<>'simulation'` + where +
		" AND " + publicAccountSQL("ia.account") + " AND " + publicAccountSQL("w.worker")
}

// readActs emits the judgements that are not endorsement records: work
// accepts and identity.witness verdicts (verified 1, failed −1, each naming
// its claim), between public accounts, in the window.
func (in trustInputs) readActs(ctx context.Context, asOf, windowStart int64, paged func(string, []any, func(*sql.Rows) (trust.Record, int64, error)) error) error {
	if err := paged(workAcceptActsSQL("t.rowid,t.work_id,ia.account,w.worker,t.accepted_at", " AND t.rowid>? AND t.accepted_at>=? AND t.accepted_at<?")+" ORDER BY t.rowid LIMIT ?",
		[]any{windowStart, asOf}, func(r *sql.Rows) (trust.Record, int64, error) {
			rec := trust.Record{Type: "edge", Kind: "work_accept"}
			var id int64
			err := r.Scan(&id, &rec.ID, &rec.From, &rec.To, &rec.CreatedAt)
			return rec, id, err
		}); err != nil {
		return err
	}
	return paged(`SELECT l.seq,iw.account,ia.account,l.created_at,l.verdict,l.kind,l.value FROM link_witnesses l JOIN identities iw ON iw.id=l.witness JOIN identities ia ON ia.id=l.agent
 WHERE l.seq>? AND l.verdict IN ('verified','failed') AND (l.superseded_at=0 OR l.superseded_at>=?) AND l.created_at>=? AND l.created_at<?
 AND `+publicAccountSQL("iw.account")+` AND `+publicAccountSQL("ia.account")+` ORDER BY l.seq LIMIT ?`,
		[]any{asOf, windowStart, asOf}, func(r *sql.Rows) (trust.Record, int64, error) {
			rec := trust.Record{Type: "edge", Kind: "witness"}
			var seq int64
			var verdict, kind, value string
			err := r.Scan(&seq, &rec.From, &rec.To, &rec.CreatedAt, &verdict, &kind, &value)
			sum := sha256.Sum256([]byte(kind + "\x00" + value))
			rec.ID, rec.Value = "link:"+hex.EncodeToString(sum[:16]), 1
			if verdict == "failed" {
				rec.Value = -1
			}
			return rec, seq, err
		})
}

// readJudgements emits the window's community reviews by public accounts,
// current or superseded (a newer review closes an older one), as "review"
// records (x from judge.FromReview), then the owners of their subjects as
// "owner" records. Lab and outside_source reviews, hidden ones, unverified
// verdicts (no judgement) and reviews of an earlier version of a subject
// are left out, as the community aggregate leaves them out.
func (in trustInputs) readJudgements(ctx context.Context, asOf int64, p trust.Params,
	paged func(string, []any, func(*sql.Rows) (trust.Record, int64, error)) error, emit func(trust.Record) error) error {
	var ok bool
	if err := in.page(ctx, func(q allowance.Querier) (err error) { ok, err = tableExists(ctx, q, "reviews"); return err }); err != nil || !ok {
		return err
	}
	type subjectKey struct{ typ, key, id string }
	seen := map[subjectKey]bool{}
	var subjects []subjectKey
	if err := paged(`SELECT r.rowid,r.event_id,r.account,s.type,s.key,s.id,r.verdict,r.score,r.good,r.evidence_class,
 CASE WHEN r.relationship_raised<>'' THEN r.relationship_raised ELSE r.relationship END,r.created_at
 FROM reviews r JOIN events e ON e.id=r.event_id JOIN subjects s ON s.id=r.subject
 WHERE r.rowid>? AND e.hidden=0 AND r.relationship NOT IN ('lab','outside_source') AND r.verdict<>'unverified' AND `+aboutCurrentVersion+`
 AND r.created_at>=? AND r.created_at<? AND `+publicAccountSQL("r.account")+` ORDER BY r.rowid LIMIT ?`,
		[]any{asOf - p.WindowDays*86400, asOf}, func(r *sql.Rows) (trust.Record, int64, error) {
			rec := trust.Record{Type: "review"}
			var rowid, score, good int64
			var sub subjectKey
			if err := r.Scan(&rowid, &rec.ID, &rec.Account, &sub.typ, &sub.key, &sub.id, &rec.State, &score, &good, &rec.Kind, &rec.LinkValue, &rec.CreatedAt); err != nil {
				return rec, 0, err
			}
			rec.Target = judge.ReviewSubject(sub.typ, sub.key, sub.id)
			rec.Value, _ = judge.FromReview(rec.State, score, good)
			if sub.typ != reviews.TypeAgent && !seen[sub] {
				seen[sub] = true
				subjects = append(subjects, sub)
			}
			return rec, rowid, nil
		}); err != nil {
		return err
	}
	sort.Slice(subjects, func(i, j int) bool { return subjects[i].id < subjects[j].id })
	for start := 0; start < len(subjects); start += 200 {
		var batch []trust.Record
		if err := in.page(ctx, func(q allowance.Querier) error {
			for _, sub := range subjects[start:min(start+200, len(subjects))] {
				owners, err := in.s.subjectOwners(ctx, q, reviews.Subject{Type: sub.typ, Key: sub.key, ID: sub.id})
				if err != nil {
					return err
				}
				for _, o := range owners {
					batch = append(batch, trust.Record{Type: "owner", Target: judge.ReviewSubject(sub.typ, sub.key, sub.id), Account: o.Account})
				}
			}
			return nil
		}); err != nil {
			return err
		}
		for _, rec := range batch {
			if err := emit(rec); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveAccount maps a key fingerprint (or an account) to its continuity
// account; an unknown id is returned as is.
func resolveAccount(ctx context.Context, q allowance.Querier, id string) (string, error) {
	if id == "" {
		return "", nil
	}
	var account string
	err := q.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", id).Scan(&account)
	if errors.Is(err, sql.ErrNoRows) {
		return id, nil
	}
	return account, err
}

// TrustDistribution is the public trust distribution for /stats: the latest
// run's standing in log10 bins of cents (bin i counts 10^i ≤ cents <
// 10^(i+1); bin 0 also holds 0), its accounts per band, and how many are at
// or above the vote floor.
func (s *Store) TrustDistribution(ctx context.Context) (map[string]any, error) {
	if s.config.Features.Trust == TrustOff {
		return nil, allowanceError("service_unavailable")
	}
	now := s.now().Unix()
	var h trust.Histogram
	var sources map[string]int64
	var recent []trust.RunPoint
	if err := (trustDB{s}).Read(ctx, func(q allowance.Querier) error {
		p, err := s.trustParamsAt(ctx, q, now)
		if err != nil {
			return err
		}
		if h, err = trust.Distribution(ctx, q, now, p); err != nil {
			return err
		}
		if sources, err = trust.SourcesByKind(ctx, q, h.RunID); err != nil {
			return err
		}
		recent, err = trust.RecentRuns(ctx, q, TrustHistoryRuns)
		return err
	}); err != nil {
		return nil, err
	}
	// Decentralisation over the recent runs (RFC 0021 §8: the top five
	// sources' share of the flow passed on) and the penalties in force;
	// never who runs an account.
	history := make([]map[string]any, 0, len(recent))
	var penalties, penalised int64
	for _, r := range recent {
		history = append(history, map[string]any{"run": r.Run, "as_of": r.AsOf, "top5_passed_ppm": r.Summary.Top5PassedPPM, "sources": r.Summary.Sources,
			"penalties_open": r.Summary.PenaltiesOpen, "standing_cents": r.Summary.StandingCents})
		if r.Run == h.RunID {
			penalties, penalised = r.Summary.PenaltiesOpen, r.Summary.Penalised
		}
	}
	return map[string]any{"run": h.RunID, "as_of": h.AsOf, "stale": h.Stale, "cents_log10_bins": h.CentsLog10, "bands": h.BandCounts,
		"above_vote_floor": h.AboveFloor, "vote_floor_cents": h.VFloorCents, "sources_cents": sources, "history": history,
		"penalties_open": penalties, "penalised": penalised}, nil
}

// TrustRuns lists runs newest first (/api/trust/runs); next is the before
// cursor for the following page, 0 at the end.
func (s *Store) TrustRuns(ctx context.Context, before int64, limit int) (runs []map[string]any, next int64, err error) {
	if s.config.Features.Trust == TrustOff {
		return nil, 0, allowanceError("service_unavailable")
	}
	err = trustDB{s}.Read(ctx, func(q allowance.Querier) (err error) { runs, err = trust.Runs(ctx, q, before, limit); return err })
	if err == nil && len(runs) == limit {
		next = runs[len(runs)-1]["id"].(int64)
	}
	return runs, next, err
}

// TrustRun is one run with its inputs and parameters.
func (s *Store) TrustRun(ctx context.Context, id int64) (run map[string]any, err error) {
	if s.config.Features.Trust == TrustOff {
		return nil, allowanceError("service_unavailable")
	}
	var ok bool
	err = trustDB{s}.Read(ctx, func(q allowance.Querier) (err error) { run, ok, err = trust.RunDetail(ctx, q, id); return err })
	if err == nil && !ok {
		err = problem(404, "not_found", "No trust run with that ID; /api/trust/runs lists them.")
	}
	return run, err
}

// TrustSnapshot opens run id's published input snapshot, JSONL decompressed
// as it is read (at most trust.SnapshotBytesMax), with its sha256 and size.
func (s *Store) TrustSnapshot(ctx context.Context, id int64) (r io.Reader, sha string, n int64, err error) {
	if s.config.Features.Trust == TrustOff {
		return nil, "", 0, allowanceError("service_unavailable")
	}
	var ok bool
	err = trustDB{s}.Read(ctx, func(q allowance.Querier) (err error) { r, sha, n, ok, err = trust.OpenSnapshot(ctx, q, id); return err })
	if err == nil && !ok {
		err = problem(404, "not_found", "That trust run kept no input snapshot (it is missing, aborted, or over the size bound); /api/trust/runs/ID shows its snapshot field.")
	}
	return r, sha, n, err
}

// linkCheckedAsOf is a proof's checked_at as the run as of asOf reads it.
// identity_links keeps only the last passing check and rechecks every link
// daily, so a verified link is usually rechecked between the as-of midnight
// and the run. A link still verified, checked at or after asOf and linked
// before it, held at asOf too (a failing recheck lapses it, nextLinkState):
// it counts as checked at asOf − 1. Before C320d it read 0, so every
// rechecked wallet and domain went uncounted (a run late in the day priced
// all of them 0, and with no other world source every standing was 0).
// A link in any other state checked after asOf is not yet known then.
func linkCheckedAsOf(state string, checked, asOf int64) int64 {
	switch {
	case checked < asOf:
		return checked
	case state == "verified":
		return asOf - 1
	}
	return 0
}
