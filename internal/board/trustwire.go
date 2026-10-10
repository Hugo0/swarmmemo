package board

// Trust module wiring (RFC0012 §4), owned by builder D: the paged input
// reader, the nightly runner, trust.get, the public run and evidence reads,
// the distribution for /stats and classifier v1. With TRUST off (the zero
// Features) nothing here runs and trust.get answers 503 as before.

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/trust"
)

// trustState is the trust runner the Store carries (Store.trust).
type trustState struct {
	mu     sync.Mutex // one run at a time in this process
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// endorsements reads the endorsement records (E's EndorsementPage);
	// replaced only by in-package tests.
	endorsements func(ctx context.Context, after int64, limit int) ([]EndorsementRecord, int64, error)
	// computed is called between computation and writes; tests only.
	computed func()
	// pages counts input pages read, for tests and the run log.
	pages atomic.Int64
	// Effective tier counts for /stats, cached per run (effectiveTierCounts).
	tierMu     sync.Mutex
	tierRun    int64
	tierAt     int64
	tierCounts map[string]int64
	// params caches the trust parameters for standing's active path.
	params standingCache
}

// TrustRunAfter is how long after 00:00 UTC the nightly run starts.
const TrustRunAfter = 30 * time.Minute

func (s *Store) openTrust() error { return nil }

// startTrust starts the nightly run while TRUST is shadow or allocation.
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
	return tx.Commit()
}

// trustParams reads the current trust parameter version from the params
// table (§2.7) and falls back to the compiled-in version 1 when there is
// none. A stored body that does not parse stops the run.
func trustParams(ctx context.Context, q allowance.Querier, now int64) (trust.Params, error) {
	if ok, err := tableExists(ctx, q, "params"); err != nil || !ok {
		return trust.DefaultParams(), err
	}
	var version int64
	var body string
	err := q.QueryRowContext(ctx, "SELECT version,body FROM params WHERE namespace=? AND effective_at<=? ORDER BY version DESC LIMIT 1", trust.ParamsNamespace, now).Scan(&version, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return trust.DefaultParams(), nil
	}
	if err != nil {
		return trust.Params{}, err
	}
	return trust.ParseParams(version, []byte(body))
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
	if err != nil || sum.State != "done" {
		return sum, err
	}
	// TODO(RFC0012 §4.5, TRUST_LIABILITY): forfeit the penalised fraction of
	// each penalised endorser's granted and earned balances. The ledger API
	// (§12.0.1) has no forfeit operation yet, so penalties act on allocation
	// weight only and TRUST_LIABILITY has no ledger effect until one exists.
	return sum, s.payTrustDividends(ctx, sum.ID, p, now)
}

// payTrustDividends mints a run's sponsor dividends as earned units from the
// day's tier-0 pool (§4.5) with TRUST_DIVIDENDS on, TRUST=allocation and the
// ledger on (shadow has no ledger effect, §4.6). Each dividend is its own
// short transaction; one the pool cannot fund stays unpaid, and so does the
// rest of the run's, until a later run tries again. One a sponsor refuses
// for its own reasons stays unpaid alone; the others are still paid.
func (s *Store) payTrustDividends(ctx context.Context, run int64, p trust.Params, now int64) error {
	f := s.config.Features
	if !f.TrustDividends || f.Trust != TrustAllocation || f.Ledger != LedgerOn {
		return nil
	}
	type dividend struct {
		sponsor, invitee string
		run, units       int64
	}
	var due []dividend
	if err := (trustDB{s}).Read(ctx, func(q allowance.Querier) error {
		rows, err := q.QueryContext(ctx, "SELECT run_id,sponsor,invitee,units FROM trust_dividends WHERE paid=0 AND units>0 AND run_id<=? ORDER BY run_id,sponsor,invitee LIMIT 500", run)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d dividend
			if err = rows.Scan(&d.run, &d.sponsor, &d.invitee, &d.units); err != nil {
				return err
			}
			due = append(due, d)
		}
		return rows.Err()
	}); err != nil {
		return err
	}
	for _, d := range due {
		err := (trustDB{s}).Write(ctx, func(q allowance.Querier) error {
			reason := "sponsor dividend for " + d.invitee + ", trust run " + strconv.FormatInt(d.run, 10)
			if err := s.ledger.led.Mint(ctx, q, d.sponsor, allowance.Resource(p.Sponsor.Resource), allowance.Earned, d.units, reason, now); err != nil {
				return err
			}
			_, err := q.ExecContext(ctx, "UPDATE trust_dividends SET paid=? WHERE run_id=? AND sponsor=? AND invitee=?", d.units, d.run, d.sponsor, d.invitee)
			return err
		})
		var refusal *allowance.Err
		if errors.As(err, &refusal) {
			if refusal.Code == "global_quota_exhausted" {
				return nil // the pool is dry today: the rest waits for a later run
			}
			// This sponsor cannot take it now (recipient_limit, say): it
			// waits for a later run, and the next sponsors are still paid.
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// WriteTrustInputs writes the snapshot a run as of today's 00:00 UTC would
// read, as JSONL: what a verifier needs to recompute it (§4.8).
func (s *Store) WriteTrustInputs(ctx context.Context, w io.Writer) error {
	now := s.now().Unix()
	asOf := now - now%86400
	db := trustDB{s}
	var p trust.Params
	if err := db.Read(ctx, func(q allowance.Querier) (err error) { p, err = trustParams(ctx, q, now); return err }); err != nil {
		return err
	}
	snap := trust.Snapshot{Params: p, Meta: trust.Meta{Schema: 1, AsOf: asOf}}
	if err := (trustInputs{s}).Read(ctx, asOf, p, snap.Add); err != nil {
		return err
	}
	snap.Meta.AsOf = asOf
	return snap.WriteJSONL(w)
}

// LiftTrustEvidence lifts evidence and its penalties with a public reason.
func (s *Store) LiftTrustEvidence(ctx context.Context, id, reason string) error {
	return trustDB{s}.Write(ctx, func(q allowance.Querier) error { return trust.Lift(ctx, q, id, reason, s.now().Unix()) })
}

// trustInputs is the paged input reader (trust.Inputs): every page is at most
// trust.PageRows rows in its own short read transaction.
type trustInputs struct{ s *Store }

func (in trustInputs) page(ctx context.Context, fn func(q allowance.Querier) error) error {
	in.s.trust.pages.Add(1)
	return trustDB{in.s}.Read(ctx, fn)
}

// Read emits the meta record, then posts, endorsements, proofs, accounts,
// breakers, transfers, claims, priors, penalties and sponsorships.
func (in trustInputs) Read(ctx context.Context, asOf int64, p trust.Params, emit func(trust.Record) error) error {
	s := in.s
	const rows = trust.PageRows
	windowStart := asOf - p.WindowDays*86400
	meta := trust.Record{Type: "meta", Schema: 1, AsOf: asOf}
	accounts := map[string]bool{}
	exists := map[string]bool{}
	if err := in.page(ctx, func(q allowance.Querier) error {
		for _, name := range []string{"account_breakers", "ledger_transfers", "allowance_claims", "ledger_entries", "domain_registrations",
			"service_calls", "x402_payments", "credit_topups", "standing_assessments"} {
			ok, err := tableExists(ctx, q, name)
			if err != nil {
				return err
			}
			exists[name] = ok
		}
		if err := q.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT id FROM trust_runs WHERE state='done' ORDER BY id DESC LIMIT ?)", p.OwnAvgRuns).Scan(&meta.PriorRuns); err != nil {
			return err
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
	oldest := asOf - max(p.WindowDays, p.Sponsor.WindowDays+p.Sponsor.DividendDays)*86400
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
				if e.CreatedAt >= asOf || e.CreatedAt < oldest {
					continue
				}
				rec := trust.Record{Type: "endorsement", Seq: e.Seq, Kind: e.Type, Value: int64(e.Value), CreatedAt: e.CreatedAt}
				if e.Type == "vote" && (e.Signature == nil || *e.Signature == "") {
					rec.Kind = "unsigned"
				}
				if e.Sponsor != nil {
					rec.Sponsor = *e.Sponsor
				}
				rec.Weight = int64(e.Weight)
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
	// out, so the published snapshot never names it (security review 1.20,
	// M11). A linked ed25519 key's account is named only when it is public too.
	// From parameter version 3 a domain proof carries its registrable
	// domain's registration time, when known: domain_registrations(domain,
	// registered_at, source, checked_at), read when the table exists.
	// TODO(C148): the RDAP lookup that creates and fills it (IANA bootstrap,
	// the safe fetcher, cached, rechecked rarely; a schema version of its
	// own). Until then every domain is priced by its link's age, and the
	// standing breakdown says so.
	registered := map[string]int64{}
	if p.Standing != nil && p.Standing.Rule >= 1 && exists["domain_registrations"] {
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
	// From parameter version 4 a wallet or github proof carries its root and
	// assessment (standing_assessments, standingroots.go), the assessment
	// only while it is no older than proof_fresh_days.
	assessedSQL := "'',0,0"
	if exists["standing_assessments"] {
		assessedSQL = "coalesce(sa.root,''),coalesce(sa.assessed,0),coalesce(sa.assessed_at,0)"
	}
	assessedJoin := ""
	if exists["standing_assessments"] {
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
 FROM identity_links l JOIN identities i ON i.id=l.agent`+assessedJoin+` WHERE l.rowid>? AND i.successor='' AND NOT (l.kind='x25519' AND l.state='lapsed') ORDER BY l.rowid LIMIT ?`, after, rows)
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
				if public && rec.CreatedAt < asOf {
					if rec.CheckedAt >= asOf {
						rec.CheckedAt = 0 // checked after the as-of time: not yet known then
					}
					if rec.Kind == "domain" && len(registered) > 0 {
						rec.RegisteredAt = registered[strings.TrimPrefix(trust.DomainRoot(rec.LinkValue, p.DomainSuffixes), "domain:")]
					}
					if p.Proofs[rec.Kind].Assess != "" {
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

	// Proof of work (version 4): one pow root per current public key, its
	// work as the assessment, checked at its last solution.
	// The run reads the total work so far; work after the as-of time also
	// moves the check past it, so such a root is not counted in that run.
	for after = 0; p.Proofs["pow"].Assess != "" && exists["standing_assessments"]; {
		var batch []trust.Record
		scanned := 0
		if err := in.page(ctx, func(q allowance.Querier) error {
			r, err := q.QueryContext(ctx, `SELECT sa.rowid,i.account,sa.root,sa.assessed,sa.created_at,sa.assessed_at,`+publicAccountSQL("i.account")+`
 FROM standing_assessments sa JOIN identities i ON i.id=sa.agent WHERE sa.rowid>? AND sa.kind='pow' AND i.successor='' ORDER BY sa.rowid LIMIT ?`, after, rows)
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

	// Accounts: first public appearance, 500 per page.
	list := make([]string, 0, len(accounts))
	for a := range accounts {
		list = append(list, a)
	}
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

	day := asOf / 86400
	funnelFrom := asOf - p.Detectors.FunnelDays*86400
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
					batch = append(batch, rec)
				}
				return r.Err()
			}); err != nil {
				return err
			}
			for _, rec := range batch {
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
		if err := paged("SELECT seq,account,started_at,trust_until FROM account_breakers WHERE seq>? AND trust_until>? AND started_at<? ORDER BY seq LIMIT ?", []any{asOf, asOf},
			func(r *sql.Rows) (trust.Record, int64, error) {
				rec := trust.Record{Type: "breaker"}
				var seq int64
				err := r.Scan(&seq, &rec.Account, &rec.StartedAt, &rec.TrustUntil)
				return rec, seq, err
			}); err != nil {
			return err
		}
	}
	// Transfers: the funnel detector's window, and from parameter version 3
	// standing's funded_days (a transfer or bounty reward links its two
	// accounts: spend between them is not seed).
	transfersFrom := funnelFrom
	if p.Standing != nil && p.Standing.Rule >= 1 {
		transfersFrom = min(transfersFrom, asOf-p.Standing.FundedDays*86400)
	}
	if exists["ledger_transfers"] {
		if err := paged("SELECT rowid,from_account,to_account,amount,done_at FROM ledger_transfers WHERE rowid>? AND state='done' AND done_at>=? AND done_at<? ORDER BY rowid LIMIT ?", []any{transfersFrom, asOf},
			func(r *sql.Rows) (trust.Record, int64, error) {
				rec := trust.Record{Type: "transfer"}
				var id int64
				err := r.Scan(&id, &rec.From, &rec.To, &rec.Amount, &rec.CreatedAt)
				return rec, id, err
			}); err != nil {
			return err
		}
	}
	// Claims and spends of post_bytes for the funnel detector; the detector
	// sums both per account over its window.
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
	var latest int64
	if err := in.page(ctx, func(q allowance.Querier) error {
		return q.QueryRowContext(ctx, "SELECT coalesce(max(id),0) FROM trust_runs WHERE state='done'").Scan(&latest)
	}); err != nil {
		return err
	}
	var cursor string
	for {
		var batch []trust.Record
		if err := in.page(ctx, func(q allowance.Querier) error {
			r, err := q.QueryContext(ctx, `SELECT account,sum(flow),max(run_id=? AND (flow>0 OR proof_collateral>0)) FROM trust_scores
 WHERE run_id IN (SELECT id FROM trust_runs WHERE state='done' ORDER BY id DESC LIMIT ?) AND account>? GROUP BY account ORDER BY account LIMIT ?`, latest, p.OwnAvgRuns, cursor, rows)
			if err != nil {
				return err
			}
			defer r.Close()
			for r.Next() {
				rec := trust.Record{Type: "prior"}
				if err = r.Scan(&rec.Account, &rec.FlowSum, &rec.Standing); err != nil {
					return err
				}
				cursor = rec.Account
				batch = append(batch, rec)
			}
			return r.Err()
		}); err != nil {
			return err
		}
		for _, rec := range batch {
			if err := emit(rec); err != nil {
				return err
			}
		}
		if len(batch) < rows {
			break
		}
	}
	if err := paged(`SELECT p.id,p.account,p.evidence_id,p.fraction_ppm,p.ends_at FROM trust_penalties p JOIN trust_evidence e ON e.id=p.evidence_id
 WHERE p.id>? AND e.lifted_at=0 AND p.ends_at>? AND p.starts_at<? ORDER BY p.id LIMIT ?`, []any{asOf, asOf},
		func(r *sql.Rows) (trust.Record, int64, error) {
			rec := trust.Record{Type: "penalty"}
			var id int64
			err := r.Scan(&id, &rec.Account, &rec.Evidence, &rec.FractionPPM, &rec.EndsAt)
			return rec, id, err
		}); err != nil {
		return err
	}
	if err := paged("SELECT rowid,invitee,sponsor,high_water FROM trust_sponsorships WHERE rowid>? ORDER BY rowid LIMIT ?", nil,
		func(r *sql.Rows) (trust.Record, int64, error) {
			rec := trust.Record{Type: "sponsorship"}
			var id int64
			err := r.Scan(&id, &rec.Invitee, &rec.SponsorOf, &rec.HighWater)
			return rec, id, err
		}); err != nil {
		return err
	}
	if p.Standing != nil {
		if err := in.readStanding(ctx, asOf, p, exists, paged, emit); err != nil {
			return err
		}
	}
	return emit(meta)
}

// readStanding emits the standing inputs of trust parameter version 2
// (RFC0015 §3): work.accept and verified witness edges between public
// accounts, and per public account and day the paid and earned credit spent
// (already public in the ledger journal). Credit held is never read.
// Accounts that are not public are never named, as for proofs.
func (in trustInputs) readStanding(ctx context.Context, asOf int64, p trust.Params, exists map[string]bool,
	paged func(string, []any, func(*sql.Rows) (trust.Record, int64, error)) error, emit func(trust.Record) error) error {
	windowStart := asOf - p.WindowDays*86400
	// work.accept: the accepting key's account → the worker, on public,
	// visible, non-simulated work items that stayed accepted.
	if err := paged(`SELECT t.rowid,t.work_id,ia.account,w.worker,t.accepted_at FROM work_transitions t JOIN works w ON w.id=t.work_id
 JOIN events e ON e.id=w.id JOIN rooms r ON r.name=e.room JOIN identities ia ON ia.id=t.author
 WHERE t.rowid>? AND t.operation='work.accept' AND w.state='accepted' AND w.worker<>'' AND r.visibility='public' AND e.hidden=0 AND e.kind<>'simulation'
 AND t.accepted_at>=? AND t.accepted_at<? AND `+publicAccountSQL("ia.account")+` AND `+publicAccountSQL("w.worker")+` ORDER BY t.rowid LIMIT ?`,
		[]any{windowStart, asOf}, func(r *sql.Rows) (trust.Record, int64, error) {
			rec := trust.Record{Type: "edge", Kind: "work_accept"}
			var id int64
			err := r.Scan(&id, &rec.ID, &rec.From, &rec.To, &rec.CreatedAt)
			return rec, id, err
		}); err != nil {
		return err
	}
	// identity.witness with verdict verified, current as of the run.
	if err := paged(`SELECT l.seq,iw.account,ia.account,l.created_at FROM link_witnesses l JOIN identities iw ON iw.id=l.witness JOIN identities ia ON ia.id=l.agent
 WHERE l.seq>? AND l.verdict='verified' AND (l.superseded_at=0 OR l.superseded_at>=?) AND l.created_at>=? AND l.created_at<?
 AND `+publicAccountSQL("iw.account")+` AND `+publicAccountSQL("ia.account")+` ORDER BY l.seq LIMIT ?`,
		[]any{asOf, windowStart, asOf}, func(r *sql.Rows) (trust.Record, int64, error) {
			rec := trust.Record{Type: "edge", Kind: "witness"}
			var seq int64
			err := r.Scan(&seq, &rec.From, &rec.To, &rec.CreatedAt)
			return rec, seq, err
		}); err != nil {
		return err
	}
	// Credit spent, per (account, day), paged by that key.
	day := asOf / 86400
	grouped := func(table, typ, query string, args ...any) error {
		if !exists[table] {
			return nil
		}
		account, after := "", int64(-1<<62)
		for {
			var batch []trust.Record
			if err := in.page(ctx, func(q allowance.Querier) error {
				bound := append(append([]any{}, args...), account, account, after, trust.PageRows)
				r, err := q.QueryContext(ctx, query, bound...)
				if err != nil {
					return err
				}
				defer r.Close()
				for r.Next() {
					rec := trust.Record{Type: typ}
					if err = r.Scan(&rec.Account, &rec.Day, &rec.Amount); err != nil {
						return err
					}
					account, after = rec.Account, rec.Day
					batch = append(batch, rec)
				}
				return r.Err()
			}); err != nil {
				return err
			}
			for _, rec := range batch {
				if err := emit(rec); err != nil {
					return err
				}
			}
			if len(batch) < trust.PageRows {
				return nil
			}
		}
	}
	oldest := day - min(3650, 8*p.Standing.HalfLifeDays)
	// From version 3 a paid x402 call is read on its own with its payee
	// (readPaidCalls); the aggregate leaves it out.
	payees := p.Standing.Rule >= 1 && exists["service_calls"] && exists["x402_payments"]
	notPaidCall := ""
	if payees {
		notPaidCall = " AND NOT (kind='commit' AND service='x402')"
	}
	if err := grouped("ledger_entries", "spend", `SELECT g.account,g.day,g.amount FROM (SELECT account,day,sum(abs(amount)) AS amount FROM ledger_entries
 WHERE kind IN ('spend','commit') AND resource='credit' AND bucket IN ('paid','earned') AND day>=? AND day<? AND account NOT LIKE 'anon:%'`+notPaidCall+`
 AND (account>? OR (account=? AND day>?)) GROUP BY account,day ORDER BY account,day) g WHERE g.amount>0 AND `+publicAccountSQL("g.account")+` ORDER BY g.account,g.day LIMIT ?`,
		oldest, day); err != nil {
		return err
	}
	if payees {
		return in.readPaidCalls(ctx, asOf-p.Standing.FundedDays*86400, asOf, oldest, day, exists, emit)
	}
	return nil
}

// readPaidCalls emits, per public account, day and payee, the paid and
// earned credit committed to x402 calls (parameter version 3), so standing
// can tell a payment from paying oneself. link_value is the resource's host
// (public in the catalogue), which standing compares with the spender's
// verified domains. to names the account that topped up credit from the
// address the call paid, only when naming it reveals nothing new: the
// spender itself, or an account a public transfer (or bounty reward) linked
// to the spender within funded_days. Which wallet funded which account is
// not published otherwise.
func (in trustInputs) readPaidCalls(ctx context.Context, fundedFrom, asOf, oldest, day int64, exists map[string]bool, emit func(trust.Record) error) error {
	type key struct {
		account, to, host string
		day               int64
	}
	sum := map[key]int64{}
	topups := exists["credit_topups"]
	linked := "0"
	if exists["ledger_transfers"] {
		linked = `EXISTS(SELECT 1 FROM ledger_transfers x WHERE x.state='done' AND x.done_at>=` + strconv.FormatInt(fundedFrom, 10) + ` AND x.done_at<` + strconv.FormatInt(asOf, 10) + `
 AND ((x.from_account=le.account AND x.to_account=t.account) OR (x.from_account=t.account AND x.to_account=le.account)))`
	}
	for after := int64(0); ; {
		n := 0
		if err := in.page(ctx, func(q allowance.Querier) error {
			payee := "''"
			if topups {
				payee = `coalesce((SELECT t.account FROM credit_topups t WHERE t.state='credited' AND lower(t.payer)=lower(p.pay_to)
 AND (t.account=le.account OR ` + linked + `) ORDER BY t.account=le.account DESC, t.account LIMIT 1),'')`
			}
			r, err := q.QueryContext(ctx, `SELECT le.seq,le.account,le.day,abs(le.amount),`+payee+`,
 coalesce((SELECT c.url FROM x402_catalogue c WHERE c.id=p.resource),(SELECT v.url FROM x402_vetted v WHERE v.id=p.resource),'')
 FROM ledger_entries le JOIN service_calls sc ON sc.hold_id=le.hold_id AND sc.service='x402' JOIN x402_payments p ON p.account=sc.account AND p.request_key=sc.request_key
 WHERE le.seq>? AND le.kind='commit' AND le.service='x402' AND le.resource='credit' AND le.bucket IN ('paid','earned') AND le.hold_id<>''
 AND le.day>=? AND le.day<? AND le.account NOT LIKE 'anon:%' AND `+publicAccountSQL("le.account")+` ORDER BY le.seq LIMIT ?`, after, oldest, day, trust.PageRows)
			if err != nil {
				return err
			}
			defer r.Close()
			for r.Next() {
				var k key
				var amount int64
				var resource string
				if err = r.Scan(&after, &k.account, &k.day, &amount, &k.to, &resource); err != nil {
					return err
				}
				n++
				if u, err := url.Parse(resource); err == nil {
					k.host = strings.ToLower(u.Hostname())
				}
				sum[k] += amount
			}
			return r.Err()
		}); err != nil {
			return err
		}
		if n < trust.PageRows {
			break
		}
	}
	keys := make([]key, 0, len(sum))
	for k := range sum {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.account != b.account {
			return a.account < b.account
		}
		if a.day != b.day {
			return a.day < b.day
		}
		if a.to != b.to {
			return a.to < b.to
		}
		return a.host < b.host
	})
	for _, k := range keys {
		if sum[k] <= 0 {
			continue
		}
		if err := emit(trust.Record{Type: "spend", Account: k.account, Day: k.day, Amount: sum[k], To: k.to, LinkValue: k.host}); err != nil {
			return err
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

// readTrust is trust.get (§4.7): what it would cost to rebuild an agent's
// standing, with its parts. Never a boolean.
func (s *Store) readTrust(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	mode := s.config.Features.Trust
	if mode == TrustOff {
		return Result{}, allowanceError("service_unavailable")
	}
	target := c.Target
	if target == "" && a.signed {
		target = a.id
	}
	if target == "" {
		return Result{}, problem(400, "invalid_agent", "Name the agent: target is its fingerprint or handle.")
	}
	public := `(EXISTS(SELECT 1 FROM events e JOIN rooms r ON r.name=e.room WHERE e.account=i.account AND r.visibility='public' AND e.hidden=0) OR EXISTS(SELECT 1 FROM audit au JOIN identities ai ON ai.id=au.actor WHERE ai.account=i.account AND au.operation IN ('agent.register','agent.profile.publish')))`
	var id, account string
	err := tx.QueryRowContext(ctx, "SELECT i.id,i.account FROM identities i WHERE (i.id=? OR i.handle=?) AND ("+public+" OR i.account=?)", target, strings.ToLower(target), a.account).Scan(&id, &account)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, problem(404, "not_found", "Agent not found.")
	}
	if err != nil {
		return Result{}, err
	}
	opt := trust.AnswerOptions{Agent: id, Mode: mode.String(), Now: now, ArchiveDelay: s.config.ArchiveDelaySeconds}
	if ok, err := tableExists(ctx, tx, "account_breakers"); err != nil {
		return Result{}, err
	} else if ok {
		if err = tx.QueryRowContext(ctx, "SELECT coalesce(max(trust_until),0) FROM account_breakers WHERE account=? AND trust_until>?", account, now).Scan(&opt.BreakerUntil); err != nil {
			return Result{}, err
		}
		opt.BreakerActive = opt.BreakerUntil > now
	}
	classifier := s.classifier() // classifier v1 under TRUST=allocation
	opt.EffectiveReason = "shadow: Design 0 rules allocate"
	if mode == TrustAllocation {
		opt.EffectiveReason = "allocation: the latest trust run allocates"
	}
	if st, err := classifier.Classify(ctx, tx, allowance.Subject{ID: account, KeyID: id, Signed: true}, now); err == nil {
		tier := int64(st.Tier)
		opt.Effective = &tier
	} else {
		opt.EffectiveReason = "the allocation classifier is not available"
	}
	report, err := trust.Answer(ctx, tx, account, opt)
	if err != nil {
		return Result{}, fromAllowance(err)
	}
	return Result{Data: report}, nil
}

// TrustDistribution is the public trust distribution for /stats (§11): the
// latest run's collateral histogram in log10 bins (bin i counts
// 10^i <= collateral < 10^(i+1); bin 0 also holds 0), and per tier how many
// of its accounts allocation places there now (tier_counts) and how many the
// run would place there (would_be_counts). In shadow the effective tiers come
// from the allocation classifier, counted once per run and cached; when that
// classifier is not available tier_counts is empty.
func (s *Store) TrustDistribution(ctx context.Context) (map[string]any, error) {
	mode := s.config.Features.Trust
	if mode == TrustOff {
		return nil, allowanceError("service_unavailable")
	}
	now := s.now().Unix()
	var h trust.Histogram
	if err := (trustDB{s}).Read(ctx, func(q allowance.Querier) (err error) { h, err = trust.Distribution(ctx, q, now); return err }); err != nil {
		return nil, err
	}
	wouldBe := map[string]int64{}
	for tier, n := range h.TierCounts {
		wouldBe[strconv.Itoa(int(tier))] = n
	}
	effective := wouldBe
	if mode == TrustShadow {
		var err error
		if effective, err = s.effectiveTierCounts(ctx, h.RunID, now); err != nil {
			return nil, err
		}
	}
	return map[string]any{"mode": mode.String(), "run": h.RunID, "as_of": h.AsOf, "stale": h.Stale, "collateral_log10_bins": h.CollateralLog10Bins,
		"tier_counts": effective, "would_be_counts": wouldBe}, nil
}

// trustTierCacheSeconds is how long effective tier counts are reused.
const trustTierCacheSeconds = 600

// effectiveTierCounts classifies the run's accounts with the allocation
// classifier, 500 per short read, and caches the counts per run.
func (s *Store) effectiveTierCounts(ctx context.Context, run, now int64) (map[string]int64, error) {
	t := &s.trust
	t.tierMu.Lock()
	defer t.tierMu.Unlock()
	if t.tierRun == run && now-t.tierAt < trustTierCacheSeconds && t.tierCounts != nil {
		return t.tierCounts, nil
	}
	counts := map[string]int64{}
	classifier := s.classifier()
	cursor := ""
	for run > 0 {
		var accounts []string
		err := (trustDB{s}).Read(ctx, func(q allowance.Querier) error {
			rows, err := q.QueryContext(ctx, "SELECT account FROM trust_scores WHERE run_id=? AND account>? ORDER BY account LIMIT 500", run, cursor)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var a string
				if err = rows.Scan(&a); err != nil {
					return err
				}
				accounts = append(accounts, a)
			}
			if err = rows.Err(); err != nil {
				return err
			}
			for _, a := range accounts {
				st, err := classifier.Classify(ctx, q, allowance.Subject{ID: a, Signed: true}, now)
				if err != nil {
					return errClassifierUnavailable
				}
				counts[strconv.Itoa(int(st.Tier))]++
			}
			return nil
		})
		if errors.Is(err, errClassifierUnavailable) {
			counts = map[string]int64{}
			break
		}
		if err != nil {
			return nil, err
		}
		if len(accounts) < 500 {
			break
		}
		cursor = accounts[len(accounts)-1]
	}
	t.tierRun, t.tierAt, t.tierCounts = run, now, counts
	return counts, nil
}

var errClassifierUnavailable = errors.New("trust: the allocation classifier is not available")

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

// TrustRun is one run with its inputs, parameters and capture bound.
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

// TrustEvidence lists evidence newest first (/api/trust/evidence).
func (s *Store) TrustEvidence(ctx context.Context, before int64, limit int) (evidence []map[string]any, next int64, err error) {
	if s.config.Features.Trust == TrustOff {
		return nil, 0, allowanceError("service_unavailable")
	}
	err = trustDB{s}.Read(ctx, func(q allowance.Querier) (err error) {
		evidence, next, err = trust.EvidenceList(ctx, q, before, limit)
		return err
	})
	return evidence, next, err
}

// trustClassifier is classifier v1 (§4.4): signed accounts are classified
// from their row in the latest run (no row: tier 3 at weight 1e6);
// anonymous subjects go to fallback (Design 0). It is used only with
// TRUST=allocation.
func (s *Store) trustClassifier(fallback allowance.Classifier) allowance.Classifier {
	return trustClassifier{fallback: fallback}
}

type trustClassifier struct{ fallback allowance.Classifier }

func (c trustClassifier) Classify(ctx context.Context, q allowance.Querier, subject allowance.Subject, now int64) (allowance.Standing, error) {
	if !subject.Signed {
		return c.fallback.Classify(ctx, q, subject, now)
	}
	score, ok, err := trust.Current(ctx, q, subject.ID)
	if err != nil {
		return allowance.Standing{}, err
	}
	if !ok {
		return allowance.Standing{Tier: allowance.TierSigned, WeightPPM: 1e6, Root: subject.ID, Source: "trust", Reason: "No row in the latest trust run: signed tier at one share."}, nil
	}
	run := strconv.FormatInt(score.RunID, 10)
	return allowance.Standing{Tier: score.Tier, WeightPPM: score.WeightPPM, Root: score.Root, Source: "trust:" + run,
		Reason: "Tier and weight from trust run " + run + "; parts at /api/agent/" + subject.ID + "/trust."}, nil
}
