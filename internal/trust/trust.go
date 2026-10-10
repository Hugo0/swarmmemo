// Package trust is the trust module (RFC0012 §4): it estimates the social
// collateral behind an account, what it would cost to acquire or rebuild it,
// from priced proofs and stake-bounded endorsement flow from two seed sets,
// with mechanical evidence, liability and sponsor dividends. It never answers
// "is this a human" and never as a boolean: every answer carries its parts,
// sources, as-of time and parameter version.
//
// Compute is a pure function of a Snapshot, so a third party can recompute a
// run from the public exports (§4.8). Run pages the inputs, computes without
// holding the database, and records the result in short transactions.
package trust

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"strings"

	"swarmmemo/internal/allowance"
)

// Schema is migration fragment D (RFC0012 §7), applied after services.Schema.
const Schema = `
CREATE TABLE IF NOT EXISTS trust_runs (
 id INTEGER PRIMARY KEY AUTOINCREMENT, as_of INTEGER NOT NULL, params_version INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('running','done','aborted')), inputs TEXT NOT NULL,
 nodes INTEGER NOT NULL DEFAULT 0, edges INTEGER NOT NULL DEFAULT 0, work INTEGER NOT NULL DEFAULT 0,
 capture_bound TEXT NOT NULL DEFAULT '', output_sha256 TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
 started_at INTEGER NOT NULL, finished_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS trust_scores (
 run_id INTEGER NOT NULL REFERENCES trust_runs(id), account TEXT NOT NULL, root TEXT NOT NULL,
 proof_collateral INTEGER NOT NULL, flow_a INTEGER NOT NULL, flow_b INTEGER NOT NULL, flow INTEGER NOT NULL,
 collateral INTEGER NOT NULL, tier INTEGER NOT NULL, weight_ppm INTEGER NOT NULL, parts TEXT NOT NULL,
 PRIMARY KEY(run_id,account));
CREATE INDEX IF NOT EXISTS trust_scores_account ON trust_scores(account,run_id);
CREATE TABLE IF NOT EXISTS trust_current (account TEXT PRIMARY KEY, run_id INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS trust_evidence (
 id TEXT PRIMARY KEY, run_id INTEGER NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('funnel','ring')),
 members TEXT NOT NULL, detail TEXT NOT NULL, detector_version INTEGER NOT NULL,
 created_at INTEGER NOT NULL, lifted_at INTEGER NOT NULL DEFAULT 0, lift_reason TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS trust_penalties (
 id INTEGER PRIMARY KEY AUTOINCREMENT, account TEXT NOT NULL, evidence_id TEXT NOT NULL REFERENCES trust_evidence(id),
 fraction_ppm INTEGER NOT NULL, starts_at INTEGER NOT NULL, ends_at INTEGER NOT NULL, UNIQUE(account,evidence_id));
CREATE INDEX IF NOT EXISTS trust_penalties_account ON trust_penalties(account,ends_at);
CREATE TABLE IF NOT EXISTS trust_sponsorships (
 invitee TEXT PRIMARY KEY, sponsor TEXT NOT NULL, record_seq INTEGER NOT NULL,
 created_at INTEGER NOT NULL, ends_at INTEGER NOT NULL, high_water INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS trust_dividends (
 run_id INTEGER NOT NULL, sponsor TEXT NOT NULL, invitee TEXT NOT NULL, units INTEGER NOT NULL,
 paid INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(run_id,sponsor,invitee));
CREATE INDEX IF NOT EXISTS trust_sponsorships_sponsor ON trust_sponsorships(sponsor);
CREATE INDEX IF NOT EXISTS trust_dividends_sponsor ON trust_dividends(sponsor,run_id);
CREATE TABLE IF NOT EXISTS trust_run_snapshots (
 run_id INTEGER PRIMARY KEY REFERENCES trust_runs(id), sha256 TEXT NOT NULL, bytes INTEGER NOT NULL, gz BLOB NOT NULL);
`

// DB runs short transactions. The board implements it on its one
// connection; the trust module never holds a transaction while computing.
type DB interface {
	Read(ctx context.Context, fn func(q allowance.Querier) error) error
	Write(ctx context.Context, fn func(q allowance.Querier) error) error
}

// RunSummary is one finished (or aborted) run.
type RunSummary struct {
	ID            int64
	AsOf          int64
	ParamsVersion int64
	State         string // "done", "aborted"
	Nodes, Edges  int
	Work          int64
	OutputSHA256  string
	Error         string
	// SnapshotSHA256 and SnapshotBytes describe the run's input snapshot
	// (JSONL), published at /api/trust/runs/ID/snapshot when it fits
	// SnapshotBytesMax.
	SnapshotSHA256 string
	SnapshotBytes  int64
}

// Score is one account's current trust_scores row.
type Score struct {
	RunID           int64
	Account         string
	Root            string
	ProofCollateral int64
	FlowA, FlowB    int64 // FlowB is -1 when seed set B had too few members
	Flow            int64
	Collateral      int64
	Tier            allowance.Tier
	WeightPPM       int64
	Parts           Parts
}

// Report is the trust.get answer (§4.7), marshalled as its data object.
type Report map[string]any

// Histogram is the trust distribution for /stats (§11).
type Histogram struct {
	RunID               int64
	AsOf                int64
	Stale               bool
	CollateralLog10Bins []int64                  // bin i: 10^i ≤ collateral < 10^(i+1); bin 0 also holds collateral 0
	TierCounts          map[allowance.Tier]int64 // would-be tiers of the run
}

// ErrBusy is returned when a run is already in progress.
var ErrBusy = errors.New("trust: a run is already in progress")

// Run computes one run as of asOf from in and records it. The snapshot is
// read page by page (each page its own transaction), the computation holds
// no transaction, and the result is written in short transactions of at
// most 500 rows. On ErrWorkExceeded the run is recorded as aborted and the
// previous run stays current. computed, when set, is called between the
// computation and the writes (tests use it).
func Run(ctx context.Context, db DB, in Inputs, p Params, asOf, now int64, computed func()) (RunSummary, error) {
	var id int64
	err := db.Write(ctx, func(q allowance.Querier) error {
		// A run left running for an hour was interrupted: mark it, never delete.
		if _, err := q.ExecContext(ctx, "UPDATE trust_runs SET state='aborted',error='interrupted',finished_at=? WHERE state='running' AND started_at<=?", now, now-3600); err != nil {
			return err
		}
		var running int
		if err := q.QueryRowContext(ctx, "SELECT count(*) FROM trust_runs WHERE state='running' AND started_at>?", now-3600).Scan(&running); err != nil {
			return err
		}
		if running > 0 {
			return ErrBusy
		}
		res, err := q.ExecContext(ctx, "INSERT INTO trust_runs(as_of,params_version,state,inputs,started_at) VALUES(?,?,'running','{}',?)", asOf, p.Version, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return RunSummary{}, err
	}
	sum := RunSummary{ID: id, AsOf: asOf, ParamsVersion: p.Version, State: "aborted"}
	fail := func(cause error) (RunSummary, error) {
		sum.Error = cause.Error()
		if len(sum.Error) > 500 {
			sum.Error = sum.Error[:500]
		}
		keep := context.WithoutCancel(ctx)
		_ = db.Write(keep, func(q allowance.Querier) error {
			_, err := q.ExecContext(keep, "UPDATE trust_runs SET state='aborted',error=?,work=?,finished_at=? WHERE id=?", sum.Error, sum.Work, now, id)
			return err
		})
		return sum, cause
	}
	snap := Snapshot{Params: p, Meta: Meta{Schema: 1, AsOf: asOf}}
	if err = in.Read(ctx, asOf, p, func(r Record) error {
		if r.Type == "params" {
			return fmt.Errorf("trust inputs: the reader may not replace the parameters")
		}
		return snap.Add(r)
	}); err != nil {
		return fail(err)
	}
	snap.Meta.AsOf = asOf
	out, err := Compute(ctx, snap)
	sum.Work = out.Work
	if err != nil {
		return fail(err)
	}
	if computed != nil {
		computed()
	}
	canonical := out.Canonical()
	sum.OutputSHA256 = sha256Hex(canonical)
	// The inputs exactly as computed, published so anyone can recompute the
	// run (recompute.py run). Kept only when within SnapshotBytesMax.
	snapshot, snapErr := packSnapshot(&snap)
	sum.SnapshotSHA256, sum.SnapshotBytes = snapshot.sha256, snapshot.bytes
	sum.Nodes, sum.Edges = int(out.Nodes), int(out.Edges)
	for start := 0; start < len(out.Scores); start += 500 {
		batch := out.Scores[start:min(start+500, len(out.Scores))]
		if err = db.Write(ctx, func(q allowance.Querier) error {
			for _, sc := range batch {
				flowB := int64(-1)
				if sc.FlowB != nil {
					flowB = *sc.FlowB
				}
				if _, err := q.ExecContext(ctx, "INSERT INTO trust_scores(run_id,account,root,proof_collateral,flow_a,flow_b,flow,collateral,tier,weight_ppm,parts) VALUES(?,?,?,?,?,?,?,?,?,?,?)",
					id, sc.Account, sc.Root, sc.ProofCollateral, sc.FlowA, flowB, sc.Flow, sc.Collateral, sc.Tier, sc.WeightPPM, string(Canonical(sc.Parts))); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return fail(err)
		}
	}
	inputs := Canonical(map[string]any{"summary": out.Inputs, "params_version": p.Version, "params_sha256": out.ParamsSHA256, "params": json.RawMessage(p.Body()),
		"seeds_a": out.SeedsA, "seeds_b": out.SeedsB, "seeds_b_used": out.SeedsBUsed, "pool_units": out.PoolUnits, "active_accounts": out.Active,
		"snapshot_sha256": snapshot.sha256, "snapshot_bytes": snapshot.bytes, "standing": out.Standing})
	err = db.Write(ctx, func(q allowance.Querier) error {
		for _, ev := range out.Evidence {
			if _, err := q.ExecContext(ctx, "INSERT OR IGNORE INTO trust_evidence(id,run_id,kind,members,detail,detector_version,created_at) VALUES(?,?,?,?,?,?,?)",
				ev.ID, id, ev.Kind, string(Canonical(ev.Members)), ev.Detail, ev.DetectorVersion, asOf); err != nil {
				return err
			}
		}
		for _, pen := range out.Penalties {
			if _, err := q.ExecContext(ctx, "INSERT OR IGNORE INTO trust_penalties(account,evidence_id,fraction_ppm,starts_at,ends_at) VALUES(?,?,?,?,?)",
				pen.Account, pen.Evidence, pen.FractionPPM, pen.StartsAt, pen.EndsAt); err != nil {
				return err
			}
		}
		for _, sp := range out.Sponsorships {
			if _, err := q.ExecContext(ctx, `INSERT INTO trust_sponsorships(invitee,sponsor,record_seq,created_at,ends_at,high_water) VALUES(?,?,?,?,?,?)
 ON CONFLICT(invitee) DO UPDATE SET sponsor=excluded.sponsor,record_seq=excluded.record_seq,created_at=excluded.created_at,ends_at=excluded.ends_at,high_water=excluded.high_water`,
				sp.Invitee, sp.Sponsor, sp.RecordSeq, sp.CreatedAt, sp.EndsAt, sp.HighWater); err != nil {
				return err
			}
		}
		for _, dv := range out.Dividends {
			if _, err := q.ExecContext(ctx, "INSERT INTO trust_dividends(run_id,sponsor,invitee,units) VALUES(?,?,?,?)", id, dv.Sponsor, dv.Invitee, dv.Units); err != nil {
				return err
			}
		}
		if snapErr == nil {
			if _, err := q.ExecContext(ctx, "INSERT INTO trust_run_snapshots(run_id,sha256,bytes,gz) VALUES(?,?,?,?)", id, snapshot.sha256, snapshot.bytes, snapshot.gz); err != nil {
				return err
			}
		}
		if _, err := q.ExecContext(ctx, "INSERT INTO trust_current(account,run_id) SELECT account,run_id FROM trust_scores WHERE run_id=? ON CONFLICT(account) DO UPDATE SET run_id=excluded.run_id", id); err != nil {
			return err
		}
		_, err := q.ExecContext(ctx, "UPDATE trust_runs SET state='done',inputs=?,nodes=?,edges=?,work=?,capture_bound=?,output_sha256=?,finished_at=? WHERE id=?",
			string(inputs), out.Nodes, out.Edges, out.Work, string(Canonical(out.CaptureBound)), sum.OutputSHA256, now, id)
		return err
	})
	if err != nil {
		return fail(err)
	}
	sum.State = "done"
	return sum, nil
}

// Current reads an account's score in the latest done run; ok is false when
// that run has no row for it.
func Current(ctx context.Context, q allowance.Querier, account string) (score Score, ok bool, err error) {
	var parts string
	var tier int64
	err = q.QueryRowContext(ctx, `SELECT s.run_id,s.account,s.root,s.proof_collateral,s.flow_a,s.flow_b,s.flow,s.collateral,s.tier,s.weight_ppm,s.parts
 FROM trust_current c JOIN trust_scores s ON s.run_id=c.run_id AND s.account=c.account
 WHERE c.account=? AND c.run_id=(SELECT max(id) FROM trust_runs WHERE state='done')`, account).
		Scan(&score.RunID, &score.Account, &score.Root, &score.ProofCollateral, &score.FlowA, &score.FlowB, &score.Flow, &score.Collateral, &tier, &score.WeightPPM, &parts)
	if errors.Is(err, sql.ErrNoRows) {
		return Score{}, false, nil
	}
	if err != nil {
		return Score{}, false, err
	}
	score.Tier = allowance.Tier(tier)
	if err = json.Unmarshal([]byte(parts), &score.Parts); err != nil {
		return Score{}, false, err
	}
	return score, true, nil
}

// latestRuns reads the latest done run and the latest run of any state.
func latestRuns(ctx context.Context, q allowance.Querier) (done map[string]any, lastState string, err error) {
	var id, asOf, version, finished int64
	var inputs string
	err = q.QueryRowContext(ctx, "SELECT id,as_of,params_version,finished_at,inputs FROM trust_runs WHERE state='done' ORDER BY id DESC LIMIT 1").Scan(&id, &asOf, &version, &finished, &inputs)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, "", err
	}
	if err == nil {
		var in struct {
			Summary InputsSummary `json:"summary"`
		}
		_ = json.Unmarshal([]byte(inputs), &in)
		done = map[string]any{"id": id, "as_of": asOf, "params_version": version, "computed_at": finished, "summary": in.Summary}
	}
	err = q.QueryRowContext(ctx, "SELECT state FROM trust_runs WHERE state<>'running' ORDER BY id DESC LIMIT 1").Scan(&lastState)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return done, lastState, err
}

// AnswerOptions is what the board knows that the trust tables do not.
type AnswerOptions struct {
	Agent           string // the key the caller named
	Mode            string // "shadow" or "allocation"
	Effective       *int64 // the tier allocation uses now; nil when unknown
	EffectiveReason string
	BreakerActive   bool
	BreakerUntil    int64
	Now             int64
	ArchiveDelay    int64
}

// StaleAfter is how long a run stays fresh.
const StaleAfter = 2 * day

// Answer is the public trust.get answer for an account (§4.7). It is never
// a boolean: collateral with its parts, flows from both seed sets, liability,
// sponsorship, the breaker and both tiers, with as-of time, run and version.
func Answer(ctx context.Context, q allowance.Querier, account string, opt AnswerOptions) (Report, error) {
	run, lastState, err := latestRuns(ctx, q)
	if err != nil {
		return nil, err
	}
	caveats := []string{}
	report := Report{"schema": 1, "agent": opt.Agent, "account": account, "mode": opt.Mode, "boolean": false}
	if run == nil {
		caveats = append(caveats, "no trust run has finished yet")
		report["run"], report["as_of"], report["computed_at"], report["params_version"], report["stale"] = nil, nil, nil, nil, true
		report["inputs_through"] = nil
	} else {
		asOf := run["as_of"].(int64)
		summary := run["summary"].(InputsSummary)
		stale := lastState == "aborted" || opt.Now-asOf > StaleAfter
		if lastState == "aborted" {
			caveats = append(caveats, "the latest run was aborted at its work bound; this answer is from the previous run")
		}
		report["run"], report["as_of"], report["computed_at"], report["params_version"], report["stale"] = run["id"], asOf, run["computed_at"], run["params_version"], stale
		report["inputs_through"] = map[string]any{"events_seq": summary.EventsSeq, "endorsements_seq": summary.EndorsementsSeq, "ledger_seq": summary.LedgerSeq}
		report["recomputable_from"] = asOf + opt.ArchiveDelay
	}
	score, ok, err := Current(ctx, q, account)
	if err != nil {
		return nil, err
	}
	if !ok {
		score = Score{Account: account, Root: account, FlowB: -1, Tier: allowance.TierSigned, WeightPPM: 1e6, Parts: Parts{Proofs: []ProofPart{}, Endorsers: []EndorserPart{}}}
		if run != nil {
			caveats = append(caveats, "not scored in the latest run: no public signed activity, endorsement or proof in its window")
		}
	}
	var log10 float64
	if score.Collateral > 0 {
		log10 = math.Round(math.Log10(float64(score.Collateral+1))*100) / 100
	}
	report["collateral"] = map[string]any{"total": score.Collateral, "proof": score.ProofCollateral, "endorsement": score.Collateral - score.ProofCollateral, "log10": log10, "unit": "see /api/params/trust (collateral_unit)"}
	report["root"] = score.Root
	proofs := score.Parts.Proofs
	if proofs == nil {
		proofs = []ProofPart{}
	}
	report["proofs"] = proofs
	var flowB any
	if score.FlowB >= 0 {
		flowB = score.FlowB
	} else {
		caveats = append(caveats, "seed set B had fewer members than its minimum; flow is from seed set A only")
	}
	endorsers := score.Parts.Endorsers
	if endorsers == nil {
		endorsers = []EndorserPart{}
	}
	report["endorsements"] = map[string]any{
		"flow":            map[string]any{"a": score.FlowA, "b": flowB, "effective": score.Flow, "unit": "1/unit_per_share of a fair share"},
		"endorsers":       endorsers,
		"endorsers_total": score.Parts.EndorsersTotal,
		"down_votes":      score.Parts.DownVotes,
		"own_avg":         score.Parts.OwnAvg,
		"transit":         score.Parts.Transit,
		"seed":            score.Parts.Seed,
	}
	liability, err := accountLiability(ctx, q, account, opt.Now)
	if err != nil {
		return nil, err
	}
	liability["penalty_ppm"] = score.Parts.PenaltyPPM
	report["liability"] = liability
	sponsor, err := accountSponsor(ctx, q, account)
	if err != nil {
		return nil, err
	}
	report["sponsor"] = sponsor
	report["breaker"] = map[string]any{"active": opt.BreakerActive, "trust_until": opt.BreakerUntil, "reset_in_run": score.Parts.Reset}
	tier := map[string]any{"would_be": int64(score.Tier), "effective": nil, "reason": opt.EffectiveReason}
	if opt.Effective != nil {
		tier["effective"] = *opt.Effective
	}
	report["tier"] = tier
	report["weight_ppm"] = score.WeightPPM
	// RFC0015 standing (trust parameter version 2): null before.
	standing, err := CurrentStanding(ctx, q, account)
	if err != nil {
		return nil, err
	}
	report["standing"] = standing
	for _, part := range proofs {
		if part.Kind == "history" {
			caveats = append(caveats, "history is lagged one run")
			break
		}
	}
	for _, part := range proofs {
		if part.Kind == "domain" && part.WeightPPM > 0 {
			caveats = append(caveats, "domain verified age is unknown: counted at most half its ramp")
			break
		}
	}
	caveats = append(caveats, "proof states are this service's attestation (RFC0009); endorsements and replies are recomputable from /v1/export")
	report["caveats"] = caveats
	return report, nil
}

func accountLiability(ctx context.Context, q allowance.Querier, account string, now int64) (map[string]any, error) {
	rows, err := q.QueryContext(ctx, `SELECT p.evidence_id,p.fraction_ppm,p.starts_at,p.ends_at,e.kind,e.lifted_at,e.lift_reason FROM trust_penalties p JOIN trust_evidence e ON e.id=p.evidence_id
 WHERE p.account=? AND p.ends_at>? ORDER BY p.starts_at DESC,p.evidence_id LIMIT 20`, account, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	penalties := []map[string]any{}
	evidence := []string{}
	for rows.Next() {
		var id, kind, reason string
		var fraction, starts, ends, lifted int64
		if err = rows.Scan(&id, &fraction, &starts, &ends, &kind, &lifted, &reason); err != nil {
			return nil, err
		}
		pen := map[string]any{"evidence": id, "kind": kind, "fraction_ppm": fraction, "starts_at": starts, "ends_at": ends, "lifted": lifted > 0}
		if lifted > 0 {
			pen["lifted_at"], pen["lift_reason"] = lifted, reason
		}
		penalties = append(penalties, pen)
		evidence = append(evidence, id)
	}
	return map[string]any{"penalties": penalties, "evidence": evidence}, rows.Err()
}

func accountSponsor(ctx context.Context, q allowance.Querier, account string) (map[string]any, error) {
	var sponsoredBy any
	var by string
	err := q.QueryRowContext(ctx, "SELECT sponsor FROM trust_sponsorships WHERE invitee=?", account).Scan(&by)
	if err == nil {
		sponsoredBy = by
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var sponsoring, units int64
	if err = q.QueryRowContext(ctx, "SELECT count(*) FROM trust_sponsorships WHERE sponsor=?", account).Scan(&sponsoring); err != nil {
		return nil, err
	}
	if err = q.QueryRowContext(ctx, "SELECT coalesce(sum(units),0) FROM trust_dividends WHERE sponsor=?", account).Scan(&units); err != nil {
		return nil, err
	}
	return map[string]any{"sponsored_by": sponsoredBy, "sponsoring": sponsoring, "dividends": map[string]any{"units": units, "paid": false}}, nil
}

// Distribution is the public trust distribution of the latest done run;
// stale as in Answer.
func Distribution(ctx context.Context, q allowance.Querier, now int64) (Histogram, error) {
	h := Histogram{CollateralLog10Bins: make([]int64, 12), TierCounts: map[allowance.Tier]int64{}, Stale: true}
	run, lastState, err := latestRuns(ctx, q)
	if err != nil || run == nil {
		return h, err
	}
	h.RunID, h.AsOf = run["id"].(int64), run["as_of"].(int64)
	h.Stale = lastState == "aborted" || now-h.AsOf > StaleAfter
	rows, err := q.QueryContext(ctx, "SELECT max(0,length(CAST(collateral AS TEXT))-1)*(collateral>0),tier,count(*) FROM trust_scores WHERE run_id=? GROUP BY 1,2", h.RunID)
	if err != nil {
		return h, err
	}
	defer rows.Close()
	for rows.Next() {
		var bin, tier, n int64
		if err = rows.Scan(&bin, &tier, &n); err != nil {
			return h, err
		}
		h.CollateralLog10Bins[min(bin, int64(len(h.CollateralLog10Bins)-1))] += n
		h.TierCounts[allowance.Tier(tier)] += n
	}
	return h, rows.Err()
}

// Runs lists runs newest first, before the given id (0: from the newest).
func Runs(ctx context.Context, q allowance.Querier, before int64, limit int) ([]map[string]any, error) {
	if before <= 0 {
		before = math.MaxInt64
	}
	rows, err := q.QueryContext(ctx, "SELECT id,as_of,params_version,state,nodes,edges,work,output_sha256,started_at,finished_at FROM trust_runs WHERE id<? ORDER BY id DESC LIMIT ?", before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, asOf, version, nodes, edges, work, started, finished int64
		var state, hash string
		if err = rows.Scan(&id, &asOf, &version, &state, &nodes, &edges, &work, &hash, &started, &finished); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "as_of": asOf, "params_version": version, "state": state, "nodes": nodes, "edges": edges, "work": work,
			"output_sha256": hash, "started_at": started, "finished_at": finished})
	}
	return out, rows.Err()
}

// RunDetail is one run with its inputs, parameters and capture bound.
func RunDetail(ctx context.Context, q allowance.Querier, id int64) (map[string]any, bool, error) {
	var asOf, version, nodes, edges, work, started, finished int64
	var state, inputs, bound, hash, failure string
	err := q.QueryRowContext(ctx, "SELECT as_of,params_version,state,inputs,nodes,edges,work,capture_bound,output_sha256,error,started_at,finished_at FROM trust_runs WHERE id=?", id).
		Scan(&asOf, &version, &state, &inputs, &nodes, &edges, &work, &bound, &hash, &failure, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	out := map[string]any{"id": id, "as_of": asOf, "params_version": version, "state": state, "inputs": json.RawMessage(inputs), "nodes": nodes, "edges": edges,
		"work": work, "capture_bound": nil, "output_sha256": hash, "error": failure, "started_at": started, "finished_at": finished, "snapshot": nil,
		"recompute": "Rebuild the inputs from /v1/export, /v1/export?stream=endorsements, /api/ledger and the previous runs, apply the published algorithm, and compare the sha256 of the canonical output with output_sha256."}
	var snapSHA string
	var snapBytes int64
	err = q.QueryRowContext(ctx, "SELECT sha256,bytes FROM trust_run_snapshots WHERE run_id=?", id).Scan(&snapSHA, &snapBytes)
	switch {
	case err == nil:
		out["snapshot"] = map[string]any{"url": fmt.Sprintf("/api/trust/runs/%d/snapshot", id), "sha256": snapSHA, "bytes": snapBytes}
		out["recompute"] = fmt.Sprintf("Download /api/trust/runs/%d/snapshot (its sha256 is snapshot.sha256), run scripts/trust/recompute.py run on it, and compare the sha256 of its output with output_sha256.", id)
	case !errors.Is(err, sql.ErrNoRows):
		return nil, false, err
	}
	if bound != "" {
		out["capture_bound"] = json.RawMessage(bound)
	}
	var evidence int64
	if err = q.QueryRowContext(ctx, "SELECT count(*) FROM trust_evidence WHERE run_id=?", id).Scan(&evidence); err != nil {
		return nil, false, err
	}
	out["evidence"] = evidence
	return out, true, nil
}

// EvidenceList lists evidence newest first with its penalties, before the
// given rowid (0: from the newest).
func EvidenceList(ctx context.Context, q allowance.Querier, before int64, limit int) ([]map[string]any, int64, error) {
	if before <= 0 {
		before = math.MaxInt64
	}
	rows, err := q.QueryContext(ctx, `SELECT rowid,id,run_id,kind,members,detail,detector_version,created_at,lifted_at,lift_reason,
 (SELECT count(*) FROM trust_penalties p WHERE p.evidence_id=trust_evidence.id) FROM trust_evidence WHERE rowid<? ORDER BY rowid DESC LIMIT ?`, before, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []map[string]any{}
	var last int64
	for rows.Next() {
		var rowid, run, version, created, lifted, penalties int64
		var id, kind, members, detail, reason string
		if err = rows.Scan(&rowid, &id, &run, &kind, &members, &detail, &version, &created, &lifted, &reason, &penalties); err != nil {
			return nil, 0, err
		}
		last = rowid
		ev := map[string]any{"id": id, "run": run, "kind": kind, "members": json.RawMessage(members), "detail": json.RawMessage(detail), "detector_version": version,
			"created_at": created, "penalties": penalties, "lifted": lifted > 0}
		if lifted > 0 {
			ev["lifted_at"], ev["lift_reason"] = lifted, reason
		}
		out = append(out, ev)
	}
	if len(out) < limit {
		last = 0
	}
	return out, last, rows.Err()
}

// Lift lifts evidence and every penalty resting on it, with a public
// reason. It is the only human action on evidence: a human can lift a
// penalty, never impose one.
func Lift(ctx context.Context, q allowance.Querier, id, reason string, now int64) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 500 {
		return fmt.Errorf("a public reason of 1–500 bytes is required")
	}
	res, err := q.ExecContext(ctx, "UPDATE trust_evidence SET lifted_at=?,lift_reason=? WHERE id=? AND lifted_at=0", now, reason, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no unlifted evidence %q", id)
	}
	return nil
}

// SnapshotBytesMax bounds one run's published input snapshot (JSONL,
// uncompressed). A larger snapshot is not kept; the run still records its
// sha256 and size.
const SnapshotBytesMax = 64 << 20

var errSnapshotTooLarge = errors.New("trust: the input snapshot is larger than SnapshotBytesMax")

type packedSnapshot struct {
	sha256 string
	bytes  int64
	gz     []byte
}

// packSnapshot writes the snapshot's JSONL form, gzipped, with its sha256 and
// uncompressed size. Past SnapshotBytesMax it stops keeping the bytes but
// finishes the hash and the count.
func packSnapshot(s *Snapshot) (packedSnapshot, error) {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	h := sha256.New()
	w := &snapshotWriter{zw: zw, h: h}
	if err := s.WriteJSONL(w); err != nil {
		return packedSnapshot{}, err
	}
	if err := zw.Close(); err != nil {
		return packedSnapshot{}, err
	}
	p := packedSnapshot{sha256: hex.EncodeToString(h.Sum(nil)), bytes: w.n}
	if w.over {
		return p, errSnapshotTooLarge
	}
	p.gz = buf.Bytes()
	return p, nil
}

type snapshotWriter struct {
	zw   *gzip.Writer
	h    hash.Hash
	n    int64
	over bool
}

func (w *snapshotWriter) Write(b []byte) (int, error) {
	w.h.Write(b)
	w.n += int64(len(b))
	if w.n > SnapshotBytesMax {
		w.over = true
	}
	if !w.over {
		if _, err := w.zw.Write(b); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

// OpenSnapshot opens run id's input snapshot as JSONL (decompressed as it is
// read), with its sha256 and size; ok is false when the run kept none.
func OpenSnapshot(ctx context.Context, q allowance.Querier, id int64) (r io.Reader, sha string, n int64, ok bool, err error) {
	var gz []byte
	err = q.QueryRowContext(ctx, "SELECT sha256,bytes,gz FROM trust_run_snapshots WHERE run_id=?", id).Scan(&sha, &n, &gz)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", 0, false, nil
	}
	if err != nil {
		return nil, "", 0, false, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, "", 0, false, err
	}
	return io.LimitReader(zr, SnapshotBytesMax), sha, n, true, nil
}
