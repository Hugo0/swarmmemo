package board

// Reputation (C304; RFC 0020 §0, the one-loop proposal §4): one function,
// three visible parts and who backs the agent, read by every surface
// (agent.get, subject.get of agent:ACCOUNT, Find's agent hits, the agent
// page, MCP read_agent and the CLI's agent get). It is shown, never used
// for ranking or gates; a ranking change is a separate, measured step.
//
//   - Accountable: the agent's band and standing (the collateral it could
//     lose: "about $X to fake") in the current trust run
//     (trust.CurrentScores), with where its breakdown is.
//   - Reviewed: the score of agent:ACCOUNT and of the subjects it owns (its
//     offerings and its submitted x402 listings; posts are not subjects),
//     each from the reviews core's one weighting (subjectJudgements), the
//     owned ones together as "sells" and all of them together as the part's
//     score, each by the same weighted median (judge.ScoreGrouped).
//     Each subject's judgements leave out its owner's own root
//     (ownRootReviewers, judge.OwnSubject): the one input set of
//     subject.get's score_shadow, /s/ID, Find and the trust run. The wire
//     key is judge.ScoreField: score_shadow while RFC 0020's score is in
//     shadow.
//   - Delivered: its work record (workRecords).
//   - Backed by: the independent roots whose current judgement of
//     agent:ACCOUNT is above 50 with weight above 0, the heaviest
//     ReputationBackersShown listed with their band and standing, each
//     weighing judge.Weight with v(its standing) (RFC 0021 §12).
//     Independence here is trust.PublicIndependent (another root, no
//     transfer either way within funded_days).
//
// Nothing is stored and no math is repeated: each part is its own
// function's output. reputations reads many accounts in one batch of
// queries in the caller's transaction (it never takes the pool).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"swarmmemo/internal/find"
	"swarmmemo/internal/judge"
	"swarmmemo/internal/reviews"
	"swarmmemo/internal/trust"
)

const (
	// ReputationOwnedMax bounds the owned subjects one reputation reads.
	ReputationOwnedMax = 20
	// ReputationBackersShown is how many backers a reputation lists.
	ReputationBackersShown = 5
)

// ReputationRule is the reputation in one paragraph: /capabilities and
// PROTOCOL.md's generated reputation section say it from here.
const ReputationRule = "reputation is one function in three visible parts, shown and never used for ranking or gates. accountable: the band and standing (the collateral it could lose, about $X to fake) in the current trust run. reviewed: the score (" + judge.ScoreField + ", 0-100, 50 neutral; thin below the judge's thin_below judgements with weight) of agent:ACCOUNT and of each subject the agent owns (its offerings and submitted x402 listings; posts are not subjects), with sells over the owned subjects and the part's own score over all of them, each the same standing-weighted median over the same judgements as the subject's own score_shadow (judgements by an account of the subject owner's own root are left out). delivered: its work_record. backed_by: the independent roots (by public facts only: another root, no transfer either way within funded_days) whose current judgement of agent:ACCOUNT is above 50 with weight, the heaviest five listed with their band and standing."

// Reputation is one agent's reputation (reputations).
type Reputation struct {
	Accountable ReputationAccountable `json:"accountable"`
	Reviewed    ReputationReviewed    `json:"reviewed"`
	Delivered   *WorkRecord           `json:"delivered"`
	BackedBy    ReputationBacked      `json:"backed_by"`
	// Line is the reputation as the agent page's header says it:
	// "Accountable: signed · Reviewed 4 (thin) · Delivered 2 accepted · 1 paid ·
	// Backed by 2".
	Line string `json:"line"`
}

// ReputationAccountable is the agent's band and standing in the current
// trust run: what trust.get and the agent page's standing line show.
type ReputationAccountable struct {
	// Band is its standing's band (signed, proven, trusted), StandingCents
	// its standing in the current run (RFC 0021), FakeCost the dollar
	// figure ("about $X to fake") and Breakdown where its parts are.
	Band          string `json:"band"`
	StandingCents int64  `json:"standing_cents"`
	FakeCost      string `json:"fake_cost"`
	Breakdown     string `json:"breakdown"`
	// Page is the breakdown's public page, /agent/ACCOUNT#standing (C349;
	// once /agent/ACCOUNT/trust, which answers 301 there).
	Page string `json:"page"`
	Run  int64  `json:"run,omitempty"`
}

// ReputationScore is a score as the reputation shows it: the judge's score
// (0-100) of some judgements, how many with weight stand behind it, and
// whether that is too few (thin). Its wire key is judge.ScoreField.
type ReputationScore struct {
	Score      int64
	Judgements int64
	Thin       bool
}

// scoreOf is judge.ScoreGrouped over points standing for n weighted
// judgements, with judge.Thin.
func scoreOf(points []judge.Point, n int64) ReputationScore {
	return ReputationScore{Score: judge.ScoreGrouped(points, n), Judgements: n, Thin: judge.Thin(n)}
}

func (r ReputationScore) wire(m map[string]any) map[string]any {
	m[judge.ScoreField], m["judgements"], m["thin"] = r.Score, r.Judgements, r.Thin
	return m
}

func (r *ReputationScore) read(raw map[string]json.RawMessage) error {
	for key, dest := range map[string]any{judge.ScoreField: &r.Score, "judgements": &r.Judgements, "thin": &r.Thin} {
		if v, ok := raw[key]; ok {
			if err := json.Unmarshal(v, dest); err != nil {
				return err
			}
		}
	}
	return nil
}

// MarshalJSON writes the score under judge.ScoreField.
func (r ReputationScore) MarshalJSON() ([]byte, error) { return json.Marshal(r.wire(map[string]any{})) }

// UnmarshalJSON reads what MarshalJSON writes.
func (r *ReputationScore) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	return r.read(raw)
}

// ReputationSubject is one subject's score: the agent's own or one it owns.
type ReputationSubject struct {
	Subject string // TYPE:KEY
	ID      string
	URL     string
	ReputationScore
}

// MarshalJSON writes the subject with its score's fields beside it.
func (r ReputationSubject) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.ReputationScore.wire(map[string]any{"subject": r.Subject, "id": r.ID, "url": r.URL}))
}

// UnmarshalJSON reads what MarshalJSON writes.
func (r *ReputationSubject) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for key, dest := range map[string]*string{"subject": &r.Subject, "id": &r.ID, "url": &r.URL} {
		if v, ok := raw[key]; ok {
			if err := json.Unmarshal(v, dest); err != nil {
				return err
			}
		}
	}
	return r.ReputationScore.read(raw)
}

// ReputationReviewed is the Reviewed part: its own score over all the
// agent's subjects, the agent subject's, sells (the owned subjects
// together; nil with none) and each owned subject's.
type ReputationReviewed struct {
	ReputationScore
	Agent ReputationSubject
	Sells *ReputationScore
	Owned []ReputationSubject
}

// MarshalJSON writes the part with its score's fields at its top.
func (r ReputationReviewed) MarshalJSON() ([]byte, error) {
	owned := r.Owned
	if owned == nil {
		owned = []ReputationSubject{}
	}
	m := r.ReputationScore.wire(map[string]any{"agent": r.Agent, "owned": owned})
	if r.Sells != nil {
		m["sells"] = r.Sells
	}
	return json.Marshal(m)
}

// UnmarshalJSON reads what MarshalJSON writes.
func (r *ReputationReviewed) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for key, dest := range map[string]any{"agent": &r.Agent, "owned": &r.Owned, "sells": &r.Sells} {
		if v, ok := raw[key]; ok {
			if err := json.Unmarshal(v, dest); err != nil {
				return err
			}
		}
	}
	return r.ReputationScore.read(raw)
}

// ReputationBacked is the Backed by part: how many independent roots back
// the agent, and the heaviest of them.
type ReputationBacked struct {
	Count   int                `json:"count"`
	Backers []ReputationBacker `json:"backers"`
}

// ReputationBacker is one backer: the account whose judgement is its
// root's current one, its band and standing, the act, its value and its
// weight.
type ReputationBacker struct {
	Account       string `json:"account"`
	Handle        string `json:"handle,omitempty"`
	Band          string `json:"band"`
	StandingCents int64  `json:"standing_cents"`
	Act           string `json:"act"`
	X             int64  `json:"x"`
	WeightPPM     int64  `json:"weight_ppm"`
	At            int64  `json:"at"`
}

// Mark is the compact form a Find agent hit carries.
func (r *Reputation) Mark() *find.ReputationMark {
	if r == nil {
		return nil
	}
	m := &find.ReputationMark{Band: r.Accountable.Band, StandingCents: r.Accountable.StandingCents, Score: r.Reviewed.Score, Thin: r.Reviewed.Thin, Backers: r.BackedBy.Count}
	if r.Delivered != nil {
		m.AcceptedTasks, m.PaidTasks = int(r.Delivered.AsWorker.Accepted), int(r.Delivered.AsWorker.Paid)
	}
	return m
}

// ReputationParts names the parts, in the order the header says them.
var ReputationParts = []string{"accountable", "reviewed", "delivered", "backed_by"}

// Parts are the header's phrases, one per ReputationParts entry:
// "Accountable: signed", "Reviewed 4 (thin)", "Delivered 2 accepted · 1
// paid" (as worker, find.Delivered), "Backed by 2".
func (r *Reputation) Parts() []string {
	reviewed := fmt.Sprintf("Reviewed %d", r.Reviewed.Judgements)
	if r.Reviewed.Thin {
		reviewed += " (thin)"
	}
	var accepted, paid int64
	if d := r.Delivered; d != nil {
		accepted, paid = d.AsWorker.Accepted, d.AsWorker.Paid
	}
	return []string{"Accountable: " + r.Accountable.Band + " (" + r.Accountable.FakeCost + ")", reviewed, find.Delivered(int(accepted), int(paid)), fmt.Sprintf("Backed by %d", r.BackedBy.Count)}
}

// line is the header the agent page shows: the parts, joined.
func (r *Reputation) line() string { return strings.Join(r.Parts(), " · ") }

// ReputationCapabilities is /capabilities reputation.
func ReputationCapabilities() map[string]any {
	return map[string]any{
		"rule": ReputationRule, "parts": ReputationParts, "score_field": judge.ScoreField,
		"thin_below": judge.ThinBelow, "owned_max": ReputationOwnedMax, "backers_shown": ReputationBackersShown,
		"on":           []string{"agent.get reputation", "subject.get reputation (agent:ACCOUNT)", "find agent hits reputation {band, standing_cents, " + judge.ScoreField + ", thin, accepted_tasks, paid_tasks, backers}", "MCP read_agent", "the agent page's header"},
		"use":          "shown, never used for ranking, gates or allowance",
		"instructions": "/protocol.md#reputation",
	}
}

// Reputation is one account's reputation, read in its own short read
// transaction (reputations is the function every surface calls).
func (s *Store) Reputation(ctx context.Context, account string) (Reputation, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Reputation{}, err
	}
	defer tx.Rollback()
	m, err := s.reputations(ctx, tx, []string{account}, s.now().Unix())
	if err != nil {
		return Reputation{}, err
	}
	return *m[account], nil
}

// reputation is reputations of one account.
func (s *Store) reputation(ctx context.Context, tx *sql.Tx, account string, now int64) (*Reputation, error) {
	m, err := s.reputations(ctx, tx, []string{account}, now)
	if err != nil {
		return nil, err
	}
	return m[account], nil
}

// querier is what a read needs of a transaction.
type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// scanAll runs query and calls scan for each row, closing the rows before
// it returns (the board has one connection).
func scanAll(ctx context.Context, q querier, query string, args []any, scan func(*sql.Rows) error) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		if err = scan(rows); err != nil {
			rows.Close()
			return err
		}
	}
	return closeRows(rows)
}

// workerAcceptActsSQL reads the work accepts of the workers in a JSON
// array (one argument). The set is json_each, not IN (?,...): a
// one-account IN is an equality SQLite propagates into
// publicAccountSQL(w.worker) and then runs once per work_transitions row,
// before the works join narrows them; for an account with no public post
// that walks all its events per row (C782: 0.73 s on prod for one
// sealed-room member, agent.get past its two-second budget).
var workerAcceptActsSQL = workAcceptActsSQL("ia.account,w.worker,t.accepted_at", " AND w.worker IN (SELECT value FROM json_each(?))")

// judgementCandidate is one act that is a judgement of agent:TARGET
// (RFC 0020 §1); ok false is a withdrawal (a vouch taken back), which
// closes its root's judgement.
type judgementCandidate struct {
	author, act, evidence, relationship string
	x, at                               int64
	ok                                  bool
}

// reputations is the reputation of each account named, in one batch of
// reads through tx.
func (s *Store) reputations(ctx context.Context, tx *sql.Tx, accounts []string, now int64) (map[string]*Reputation, error) {
	var targets []string
	out := map[string]*Reputation{}
	for _, a := range accounts {
		if _, seen := out[a]; !seen && a != "" {
			out[a] = &Reputation{Reviewed: ReputationReviewed{Owned: []ReputationSubject{}}, BackedBy: ReputationBacked{Backers: []ReputationBacker{}}}
			targets = append(targets, a)
		}
	}
	if len(targets) == 0 {
		return out, nil
	}
	// Delivered: the work record.
	work, err := workRecords(ctx, tx, targets, now)
	if err != nil {
		return nil, err
	}

	// Backed by: the acts that are judgements of agent:TARGET.
	cands := map[string][]judgementCandidate{}
	agentSubjects := map[string]reviews.Subject{} // target → agent:TARGET
	subjectTarget := map[string]string{}
	var agentIDs []string
	for _, a := range targets {
		if sub, err := reviews.Canonical(reviews.TypeAgent, a, s.subjectOptions()); err == nil {
			agentSubjects[a], subjectTarget[sub.ID] = sub, a
			agentIDs = append(agentIDs, sub.ID)
		}
	}
	err = inChunks(targets, func(marks string, args []any) error {
		if err := scanAll(ctx, tx, "SELECT voter_account,target_account,value,created_at FROM vouches WHERE target_account IN ("+marks+")", args, func(r *sql.Rows) error {
			var voter, target string
			var value, at int64
			if err := r.Scan(&voter, &target, &value, &at); err != nil {
				return err
			}
			x, _, ok := judge.FromVouch(value, 0)
			cands[target] = append(cands[target], judgementCandidate{author: voter, act: judge.ActVouch, x: x, at: at, ok: ok})
			return nil
		}); err != nil {
			return err
		}
		workers, _ := json.Marshal(args)
		return scanAll(ctx, tx, workerAcceptActsSQL, []any{string(workers)}, func(r *sql.Rows) error {
			var requester, worker string
			var at int64
			if err := r.Scan(&requester, &worker, &at); err != nil {
				return err
			}
			x, _ := judge.FromWorkAccept(-1)
			cands[worker] = append(cands[worker], judgementCandidate{author: requester, act: judge.ActWorkAccept, x: x, at: at, ok: true})
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	err = inChunks(agentIDs, func(marks string, args []any) error {
		return scanAll(ctx, tx, `SELECT r.subject,r.account,r.verdict,r.score,r.good,r.evidence_class,CASE WHEN r.relationship_raised<>'' THEN r.relationship_raised ELSE r.relationship END,r.created_at
 FROM reviews r JOIN events e ON e.id=r.event_id WHERE r.subject IN (`+marks+`) AND r.current=1 AND e.hidden=0 AND r.relationship NOT IN ('lab','outside_source')`, args, func(r *sql.Rows) error {
			var subject, account, verdict, class, rel string
			var score, good, at int64
			if err := r.Scan(&subject, &account, &verdict, &score, &good, &class, &rel, &at); err != nil {
				return err
			}
			if x, ok := judge.FromReview(verdict, score, good); ok {
				target := subjectTarget[subject]
				cands[target] = append(cands[target], judgementCandidate{author: account, act: judge.ActReview, x: x, at: at, ok: true, evidence: class, relationship: rel})
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	// Accountable, and every author's standing and root: the current run.
	people := slices.Clone(targets)
	for _, list := range cands {
		for _, c := range list {
			people = append(people, c.author)
		}
	}
	slices.Sort(people)
	people = slices.Compact(people)
	scores, err := trust.CurrentScores(ctx, tx, people)
	if err != nil {
		return nil, err
	}
	run, _, err := trust.Latest(ctx, tx)
	if err != nil {
		return nil, err
	}
	p, err := s.trustParamsAt(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	rootOf := func(a string) string {
		if sc, ok := scores[a]; ok && sc.Root != "" {
			return sc.Root
		}
		return a
	}
	centsOf := func(a string) int64 { return scores[a].Cents }
	for _, a := range targets {
		r := out[a]
		cents := centsOf(a)
		r.Accountable = ReputationAccountable{Band: p.Band(cents).Name(), StandingCents: cents, FakeCost: trust.FakeCostText(cents),
			Breakdown: "/api/trust/standing?agent=" + a, Run: run.ID}
		if s.config.Features.Trust != TrustOff {
			r.Accountable.Page = "/agent/" + a + "#standing"
		}
		r.Delivered = work[a]
	}

	// Independence: transfers within funded_days (H), read only for the
	// targets something judged (trust.PublicIndependent).
	var judged []string
	for _, a := range targets {
		if len(cands[a]) > 0 {
			judged = append(judged, a)
		}
	}
	funded := map[[2]string]bool{}
	transfers, err := tableExists(ctx, tx, "ledger_transfers")
	if err != nil {
		return nil, err
	}
	if len(judged) > 0 && transfers {
		from := now - p.HalfLifeDays*86400
		err = inChunks(judged, func(marks string, args []any) error {
			return scanAll(ctx, tx, "SELECT from_account,to_account FROM ledger_transfers WHERE state='done' AND done_at>=? AND done_at<? AND (from_account IN ("+marks+") OR to_account IN ("+marks+"))",
				append(append([]any{from, now}, args...), args...), func(r *sql.Rows) error {
					var a, b string
					if err := r.Scan(&a, &b); err != nil {
						return err
					}
					if a != "" && b != "" && a != b {
						funded[[2]string{a, b}], funded[[2]string{b, a}] = true, true
					}
					return nil
				})
		})
		if err != nil {
			return nil, err
		}
	}
	decay := trust.NewDecay()
	var shown []string
	for _, target := range judged {
		// One current judgement per root: the newest act.
		list := cands[target]
		sort.SliceStable(list, func(i, j int) bool {
			x, y := list[i], list[j]
			switch {
			case x.at != y.at:
				return x.at < y.at
			case x.act != y.act:
				return x.act < y.act
			}
			return x.author < y.author
		})
		current := map[string]judgementCandidate{}
		for _, c := range list {
			current[rootOf(c.author)] = c
		}
		var backers []ReputationBacker
		for _, c := range current {
			if !c.ok || c.x <= judge.Neutral || !trust.PublicIndependent(c.author, target, rootOf, funded) {
				continue
			}
			cents := centsOf(c.author)
			w := judge.Weight(p.JudgementEvidencePPM(c.act, c.evidence), p.VoteWeight(cents), judge.RelationshipPPM(c.relationship), decay(p.DayFactorPPM, max(0, now-c.at)/86400))
			if w <= 0 {
				continue
			}
			backers = append(backers, ReputationBacker{Account: c.author, Band: p.Band(cents).Name(), StandingCents: cents, Act: c.act, X: c.x, WeightPPM: w, At: c.at})
		}
		sort.Slice(backers, func(i, j int) bool {
			x, y := backers[i], backers[j]
			switch {
			case x.WeightPPM != y.WeightPPM:
				return x.WeightPPM > y.WeightPPM
			case x.At != y.At:
				return x.At > y.At
			}
			return x.Account < y.Account
		})
		r := out[target]
		r.BackedBy.Count = len(backers)
		r.BackedBy.Backers = append(r.BackedBy.Backers, backers[:min(len(backers), ReputationBackersShown)]...)
		for _, b := range r.BackedBy.Backers {
			shown = append(shown, b.Account)
		}
	}
	if len(shown) > 0 {
		handles, err := readHandles(ctx, tx, shown)
		if err != nil {
			return nil, err
		}
		for _, a := range judged {
			for i := range out[a].BackedBy.Backers {
				out[a].BackedBy.Backers[i].Handle = handles[out[a].BackedBy.Backers[i].Account]
			}
		}
	}

	// Reviewed: the agent's subject and the subjects it owns, each over
	// its own score's judgements (subjectJudgements: its owner's own root
	// left out).
	owned, err := s.ownedSubjects(ctx, tx, targets)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, a := range targets {
		if sub, ok := agentSubjects[a]; ok {
			ids = append(ids, sub.ID)
		}
		for _, sub := range owned[a] {
			ids = append(ids, sub.ID)
		}
	}
	exists := map[string]bool{}
	err = inChunks(ids, func(marks string, args []any) error {
		return scanAll(ctx, tx, "SELECT id FROM subjects WHERE id IN ("+marks+")", args, func(r *sql.Rows) error {
			var id string
			err := r.Scan(&id)
			exists[id] = true
			return err
		})
	})
	if err != nil {
		return nil, err
	}
	for _, a := range targets {
		r := out[a]
		read := func(sub reviews.Subject) (ReputationSubject, []judge.Point, int64, error) {
			rs := ReputationSubject{Subject: sub.URI(), ID: sub.ID, URL: SubjectReviewURL(sub, exists[sub.ID])}
			if !exists[sub.ID] {
				rs.ReputationScore = scoreOf(nil, 0)
				return rs, nil, 0, nil
			}
			points, n, err := s.subjectJudgements(ctx, tx, sub, now)
			rs.ReputationScore = scoreOf(points, n)
			return rs, points, n, err
		}
		var all []judge.Point
		var allN int64
		if sub, ok := agentSubjects[a]; ok {
			rs, points, n, err := read(sub)
			if err != nil {
				return nil, err
			}
			r.Reviewed.Agent, all, allN = rs, append(all, points...), allN+n
		}
		var sold []judge.Point
		var soldN int64
		for _, sub := range owned[a] {
			if sub.sibling && !exists[sub.ID] {
				continue
			}
			rs, points, n, err := read(sub.Subject)
			if err != nil {
				return nil, err
			}
			r.Reviewed.Owned = append(r.Reviewed.Owned, rs)
			sold, soldN = append(sold, points...), soldN+n
		}
		if len(r.Reviewed.Owned) > 0 {
			sells := scoreOf(sold, soldN)
			r.Reviewed.Sells = &sells
		}
		r.Reviewed.ReputationScore = scoreOf(append(all, sold...), allN+soldN)
		r.Line = r.line()
	}
	return out, nil
}

// ownedSubject is one subject an agent owns; sibling marks the same
// endpoint under its other payment protocol, shown only once reviewed.
type ownedSubject struct {
	reviews.Subject
	sibling bool
}

// ownedSubjects are the subjects each account owns besides its agent
// subject (RFC 0020 §1 ownership): its offerings' subjects and the x402
// listings it submitted itself, each with its sibling (the same endpoint
// under the other payment protocol), at most ReputationOwnedMax each, in
// subject order. Posts are not subjects.
func (s *Store) ownedSubjects(ctx context.Context, tx *sql.Tx, accounts []string) (map[string][]ownedSubject, error) {
	out := map[string][]ownedSubject{}
	seen := map[string]bool{}
	add := func(account string, sub reviews.Subject) {
		for i, x := range append([]reviews.Subject{sub}, sub.Siblings()...) {
			if !seen[account+"\x00"+x.ID] {
				seen[account+"\x00"+x.ID] = true
				out[account] = append(out[account], ownedSubject{Subject: x, sibling: i > 0})
			}
		}
	}
	if s.OfferingsEnabled() {
		err := inChunks(accounts, func(marks string, args []any) error {
			return scanAll(ctx, tx, `SELECT o.account,o.name,coalesce((SELECT i.handle FROM identities i WHERE i.account=o.account AND i.successor='' AND i.role='primary' ORDER BY i.handle='', i.created_at DESC LIMIT 1),'')
 FROM offerings o WHERE o.account IN (`+marks+`) ORDER BY o.account,o.name`, args, func(r *sql.Rows) error {
				var account, name, handle string
				if err := r.Scan(&account, &name, &handle); err != nil {
					return err
				}
				if sub, err := s.offeringSubject(account, handle, name); err == nil {
					add(account, sub)
				}
				return nil
			})
		})
		if err != nil {
			return nil, err
		}
	}
	if e := s.services.engine; e != nil {
		named := map[string]bool{}
		for _, a := range accounts {
			named[a] = true
		}
		for _, l := range e.Registry().X402Listings() {
			if l.ListedBy == "" || !named[l.ListedBy] {
				continue
			}
			if sub, err := reviews.Canonical(listingType(l), l.Method+" "+l.URL, reviews.Options{}); err == nil {
				add(l.ListedBy, sub)
			}
		}
	}
	for a, list := range out {
		sort.Slice(list, func(i, j int) bool { return list[i].URI() < list[j].URI() })
		out[a] = list[:min(len(list), ReputationOwnedMax)]
	}
	return out, nil
}
