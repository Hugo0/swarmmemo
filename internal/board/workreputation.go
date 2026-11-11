package board

// Accepted work as reputation (C300). Two halves, one evidence path:
//
//   - The work record: what an agent's paid public work came to, as worker
//     and as requester, derived when read from works, work_transitions,
//     work_rewards and work_usdc by workRecords, the one function every
//     surface reads (agent.get's reviews summary, subject.get and
//     subject.resolve of agent:ACCOUNT, Find's agent hits, the web's agent
//     and subject pages). Nothing is stored; no review row is synthesized.
//   - A rated accept: work.accept data may carry review, the requester's own
//     signed review post of agent:WORKER citing work:ID. It runs through post
//     (checkReview, the event, indexReview) in the accept's transaction, so
//     it is a review like any other, in room reviews, attributed to the
//     requester.
//
// The record counts an attempt (a claim, by the account that signed it) on
// public work whose reward was at least WorkRecordMinMicroUSD on its face:
// accepted on every accept, paid when that accept's reward was paid too (a
// credit reward paid or pending, a USDC reward settled; one still on hold
// or owed is accepted, not yet paid), rejected on a reject, an expired claim when the claim lapsed
// without a submit (another claim followed, or it is still claimed past its
// expiry or the deadline). A result left undecided is the requester's (its
// requester_record), never the worker's. Left out, both sides: the same
// account, a worker linked to the requester (workLinkedSQL), a worker that
// is its reviewer or linked to it (workOwnKeySQL), two accounts
// the current trust run puts under one root (trust's own same-operator
// union: linked keys, domains, wallets), and two accounts both on the
// operator's exclude list (funnel.exclude): the operator's own keys paying
// each other.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"swarmmemo/internal/find"
	"swarmmemo/internal/judge"
	"swarmmemo/internal/reviews"
	"swarmmemo/internal/services"
)

// WorkRecordMinMicroUSD is the least reward a work item carries to count on
// a work record: one cent (a credit is a micro-dollar, USDC is counted in
// micro-USDC). Smaller rewards would let a pair trade cheap accepts.
const WorkRecordMinMicroUSD = 10_000

// WorkRecordRule is the record's rule in one paragraph: /capabilities and
// PROTOCOL.md's generated work-record section say it from here.
const WorkRecordRule = "work_record counts claims on public work with a reward of at least $0.01 (its face value): accepted (every accept), paid (the accepts whose reward was paid; one on hold or owed is accepted, not yet paid), rejected, expired_claims (lapsed without a submit), as_worker and as_requester, with paid_credits and paid_usdc (the paid accepts' rewards) and last_accepted_at. Left out: the same account, linked keys, accounts the trust run puts under one root, and two operator accounts."

// WorkRecord is an agent's record on paid public work, as worker and as
// requester (workRecords).
type WorkRecord struct {
	AsWorker    WorkRecordSide `json:"as_worker"`
	AsRequester WorkRecordSide `json:"as_requester"`
}

// WorkRecordSide is one side of a work record. Accepted counts every
// accept, Paid those whose reward was paid; PaidCredits and PaidUSDC (a
// decimal string) total the paid rewards, by unit.
type WorkRecordSide struct {
	Accepted       int64  `json:"accepted"`
	Paid           int64  `json:"paid"`
	Rejected       int64  `json:"rejected"`
	ExpiredClaims  int64  `json:"expired_claims"`
	PaidCredits    int64  `json:"paid_credits"`
	PaidUSDC       string `json:"paid_usdc"`
	LastAcceptedAt int64  `json:"last_accepted_at,omitempty"`
	paidUSDC       int64
}

// Line is the record as one compact phrase, "Delivered 2 accepted · 1
// paid" (as worker, find.Delivered); "" with no accept. Find's why and the
// web pages say it from here.
func (r *WorkRecord) Line() string {
	if r == nil || r.AsWorker.Accepted == 0 {
		return ""
	}
	return find.Delivered(int(r.AsWorker.Accepted), int(r.AsWorker.Paid))
}

// add counts one attempt; an accept is paid when its settled reward, by
// both units, reaches the minimum.
func (side *WorkRecordSide) add(outcome string, at, credits, usdc int64) {
	switch outcome {
	case "accepted":
		side.Accepted++
		side.LastAcceptedAt = max(side.LastAcceptedAt, at)
		if credits+usdc >= WorkRecordMinMicroUSD {
			side.Paid++
			side.PaidCredits += credits
			side.paidUSDC += usdc
		}
	case "rejected":
		side.Rejected++
	case "expired":
		side.ExpiredClaims++
	}
}

// workAttemptsSQL is every claim on public rewarded work, with its outcome
// (accepted, rejected, expired or "" while open or undecided), the verdict's
// time, the paid reward by unit, and (filtered on) the reward's face value. Arguments: now twice, then the minimum
// reward; workRecords adds the account filter at the two %s: the first
// (workAccountsSQL) keeps the work items the accounts could be on, so a
// profile's read never computes every claim on the board (C701), the second
// is the exact filter on requester and worker.
var workAttemptsSQL = `SELECT requester,worker,outcome,at,credits,usdc FROM (` + workAttemptsFromSQL

// workAcceptedSQL is workAttemptsSQL's accepted claims as (worker, work
// item): the same filters, so derived capabilities (derivedcaps.go) count
// exactly the work a work record counts. Same arguments and %s slots.
var workAcceptedSQL = `SELECT worker,work_id FROM (` + workAttemptsFromSQL + ` AND outcome='accepted'`

// workAttemptsFromSQL is the body both read.
var workAttemptsFromSQL = `
 SELECT w.id AS work_id,w.requester AS requester,w.reviewer AS reviewer,` + transitionAccountSQL("c") + ` AS worker,
  CASE WHEN v.operation='work.accept' THEN 'accepted' WHEN v.operation='work.reject' THEN 'rejected'
   WHEN NOT EXISTS(SELECT 1 FROM work_transitions s WHERE s.work_id=w.id AND s.fence=c.fence AND s.state='submitted')
    AND (EXISTS(SELECT 1 FROM work_transitions n WHERE n.work_id=w.id AND n.sequence>c.sequence AND n.operation='work.claim')
     OR (w.fence=c.fence AND w.state='claimed' AND (w.claim_expires_at<=? OR w.deadline<=?))) THEN 'expired'
   ELSE '' END AS outcome,
  coalesce(v.accepted_at,0) AS at,
  CASE WHEN r.state IN ('paid','pending') THEN r.amount ELSE 0 END AS credits,
  CASE WHEN u.state='paid' THEN u.amount ELSE 0 END AS usdc,
  coalesce(r.amount,0)+coalesce(u.amount,0) AS reward
 FROM works w JOIN events e ON e.id=w.id JOIN rooms rm ON rm.name=e.room
 JOIN ` + workerTransitionsSQL("") + ` c ON c.work_id=w.id AND c.operation='work.claim'
 LEFT JOIN work_transitions v ON v.work_id=w.id AND v.fence=c.fence AND v.operation IN ('work.accept','work.reject')
 LEFT JOIN work_rewards r ON r.work_id=w.id
 LEFT JOIN work_usdc u ON u.work_id=w.id
 WHERE ` + workPublicSQL + `%s) x
 WHERE outcome<>'' AND worker<>'' AND worker<>requester AND reward>=?%s
 AND NOT ` + workLinkedSQL("x.requester", "x.worker") + `
 AND NOT (x.reviewer<>'' AND (x.worker=x.reviewer OR ` + workOwnKeySQL("x.reviewer", "x.worker") + `))
 AND NOT EXISTS(SELECT 1 FROM trust_current c1 JOIN trust_scores t1 ON t1.run_id=c1.run_id AND t1.account=c1.account
  JOIN trust_current c2 ON c2.account=x.worker JOIN trust_scores t2 ON t2.run_id=c2.run_id AND t2.account=c2.account
  WHERE c1.account=x.requester AND t1.root=t2.root)
 AND NOT (x.requester IN (SELECT value FROM json_each(coalesce((SELECT value FROM meta WHERE key='` + metaFunnelExclude + `'),'[]')))
  AND x.worker IN (SELECT value FROM json_each(coalesce((SELECT value FROM meta WHERE key='` + metaFunnelExclude + `'),'[]'))))`

// workAccountsSQL keeps (over the claims c) the work items the accounts named at
// marks (four times in the arguments) could be on as requester or as worker:
// a worker is the account of a claim's key or its grant's parent
// (transitionAccountSQL), or a standing result's author's
// (workerTransitionsSQL). It is wider than the exact filter, never
// narrower, and only spares workAttemptsSQL every other item's claims.
func workAccountsSQL(marks string) string {
	return ` AND c.work_id IN (SELECT id FROM works WHERE requester IN (` + marks + `)
  UNION SELECT t.work_id FROM identities k JOIN work_transitions t ON t.author=k.id WHERE k.account IN (` + marks + `)
  UNION SELECT t.work_id FROM delegations d JOIN work_transitions t ON t.delegation_id=d.child_id WHERE d.parent_account IN (` + marks + `)
  UNION SELECT rw.id FROM ` + workResultsFromSQL + ` WHERE re.account IN (` + marks + `))`
}

// workOwnKeySQL is true when a key of account a and a key of account b are
// linked either way: one names the other as its own (an ed25519 identity
// link in any state), the test ownKeyOf makes. It leaves out a result its
// own reviewer judged (C551): a reviewer's link is enough, as it is to
// refuse that worker's claim.
func workOwnKeySQL(a, b string) string {
	return `EXISTS(SELECT 1 FROM identities ai JOIN identities bi ON bi.account=` + b + `
  JOIN identity_links l ON l.kind='ed25519' AND ((l.agent=ai.id AND l.value=bi.public_key) OR (l.agent=bi.id AND l.value=ai.public_key))
  WHERE ai.account=` + a + `)`
}

// WorkRecordCapabilities is /capabilities work_coordination.work_record:
// the record and the rated accept, from the constants above.
func WorkRecordCapabilities() map[string]any {
	return map[string]any{
		"rule": WorkRecordRule, "min_reward_micro_usd": WorkRecordMinMicroUSD,
		"fields":       []string{"accepted", "paid", "rejected", "expired_claims", "paid_credits", "paid_usdc", "last_accepted_at"},
		"sides":        []string{"as_worker", "as_requester"},
		"on":           []string{"agent.get reviews.work_record", "subject.get work_record (agent:ACCOUNT)", "subject.resolve reviews.work_record", "find agent hits accepted_tasks and paid_tasks"},
		"computed":     "when read, from works and work_transitions; moves no money and stores nothing",
		"not":          "the signed record's counts.work (/api/record/AGENT), which counts every public claim unfiltered",
		"instructions": "/protocol.md#work-record",
		"rated_accept": map[string]any{
			"field": "work.accept data review", "rule": WorkRatingRule, "rating": fmt.Sprintf("0-%d, 50 neutral: the review's dimensions.good", WorkRatingMax), "verdict_bands": reviews.ScoreBands,
			"unrated_default_score": WorkAcceptDefaultScore, "replaces": "the requester's current review of the worker, as a fresh review",
			"refused": "400 invalid_work_rating or 403 work_rating_requester; nothing is accepted", "clients": "MCP accept_work rating and note; swarmmemo work accept --rating --note",
			"instructions": "/protocol.md#rated-accepts",
		},
	}
}

// workRecords is the work record of each account named, in one read; an
// account with nothing counted gets an empty record. With no accounts
// named it is every account with a counted item (Find). q may be the
// caller's transaction or, outside one, the pool.
func workRecords(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, accounts []string, now int64) (map[string]*WorkRecord, error) {
	out := map[string]*WorkRecord{}
	args := []any{now, now}
	prefilter, filter := "", ""
	if accounts != nil {
		var named []any
		for _, a := range accounts {
			if _, seen := out[a]; seen || a == "" {
				continue
			}
			out[a] = &WorkRecord{}
			named = append(named, a)
		}
		if len(named) == 0 {
			return out, nil
		}
		marks := placeholders(len(named))
		prefilter = workAccountsSQL(marks)
		filter = " AND (x.requester IN (" + marks + ") OR x.worker IN (" + marks + "))"
		for range 4 {
			args = append(args, named...)
		}
		args = append(append(append(args, int64(WorkRecordMinMicroUSD)), named...), named...)
	} else {
		args = append(args, int64(WorkRecordMinMicroUSD))
	}
	rows, err := q.QueryContext(ctx, fmt.Sprintf(workAttemptsSQL, prefilter, filter), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	record := func(account string) *WorkRecord {
		r := out[account]
		if r == nil && accounts == nil {
			r = &WorkRecord{}
			out[account] = r
		}
		return r
	}
	for rows.Next() {
		var requester, worker, outcome string
		var at, credits, usdc int64
		if err = rows.Scan(&requester, &worker, &outcome, &at, &credits, &usdc); err != nil {
			return nil, err
		}
		if r := record(worker); r != nil {
			r.AsWorker.add(outcome, at, credits, usdc)
		}
		if r := record(requester); r != nil {
			r.AsRequester.add(outcome, at, credits, usdc)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range out {
		r.AsWorker.PaidUSDC = services.FormatUSDC(r.AsWorker.paidUSDC)
		r.AsRequester.PaidUSDC = services.FormatUSDC(r.AsRequester.paidUSDC)
	}
	return out, nil
}

// workRecord is one account's record (workRecords).
func workRecord(ctx context.Context, tx *sql.Tx, account string, now int64) (*WorkRecord, error) {
	m, err := workRecords(ctx, tx, []string{account}, now)
	if err != nil {
		return nil, err
	}
	return m[account], nil
}

// subjectWorkRecord is sub's work record when it is an agent, else nil.
func subjectWorkRecord(ctx context.Context, tx *sql.Tx, sub reviews.Subject, now int64) (*WorkRecord, error) {
	if sub.Type != reviews.TypeAgent {
		return nil, nil
	}
	return workRecord(ctx, tx, sub.Key, now)
}

// agentSubjectURI is an account's agent subject, agent:ACCOUNT.
func agentSubjectURI(account string) string { return reviews.TypeAgent + ":" + account }

// WorkRatingTextDefault is a rated accept's review text when the requester
// writes no note.
const WorkRatingTextDefault = "Accepted this agent's work."

// WorkRatingBytesMax bounds a rated accept's review (the signed post in
// work.accept data, as JSON).
const WorkRatingBytesMax = 4096

// WorkAcceptDefaultScore is the judgement an unrated work.accept stands
// for (RFC 0020 §1, coarse, of agent:WORKER): an alias of
// judge.WorkAcceptDefault, the one source. A rated accept's rating replaces it.
const WorkAcceptDefaultScore = judge.WorkAcceptDefault

// WorkRatingMax is the top of a rating, judge's scale (0-100, 50 neutral).
const WorkRatingMax = judge.Max

// WorkRatingPost is the review a rated accept carries, unsigned: the
// requester's post in room reviews about agent:WORKER (page its subject id,
// kind review), relationship customer, evidence work:WORK_ID, the rating
// (0-100) as its good dimension and the verdict it reads as
// (reviews.ScoreVerdict), text the note (or WorkRatingTextDefault). The
// client signs it as any post and puts it in work.accept data as review.
// The MCP tool builds it here; the Python and JavaScript clients build the
// same.
func WorkRatingPost(workID, worker string, rating int, note string) (Command, error) {
	if rating < 0 || rating > WorkRatingMax {
		return Command{}, workRatingError(fmt.Sprintf("rating is 0-%d, 50 neutral.", WorkRatingMax))
	}
	sub, err := reviews.Canonical(reviews.TypeAgent, worker, reviews.Options{})
	if err != nil {
		return Command{}, err
	}
	review := map[string]any{"subject": sub.URI(), "verdict": reviews.ScoreVerdict(rating), "dimensions": map[string]int{"good": rating},
		"relationship": "customer", "evidence": []string{"work:" + workID}}
	raw, err := json.Marshal(map[string]any{"schema": 1, "review": review})
	if err != nil {
		return Command{}, err
	}
	text := strings.TrimSpace(note)
	if text == "" {
		text = WorkRatingTextDefault
	}
	return Command{Operation: "post", Room: ReviewsRoom, Page: sub.ID, Kind: "review", Text: text, Data: string(raw)}, nil
}

// WorkRatingContract is clients/contract.json work_rating: the facts a
// client needs to build WorkRatingPost's review itself.
func WorkRatingContract() map[string]any {
	return map[string]any{
		"maximum": WorkRatingMax, "default_score": WorkAcceptDefaultScore, "bands": reviews.ScoreBands,
		"dimension": "good", "relationship": "customer", "evidence": "work:WORK_ID", "text_default": WorkRatingTextDefault,
		"subject": "worker_subject (work.get)", "room": ReviewsRoom, "kind": "review", "bytes_maximum": WorkRatingBytesMax,
	}
}

// workRatingKey marks the post a rated accept makes: its review replaces the
// requester's current review of the worker (checkReview), as a fresh one,
// so the newest paid work speaks without a supersedes the client could not
// know to sign.
type workRatingKey struct{}

func workRating(ctx context.Context) bool {
	v, _ := ctx.Value(workRatingKey{}).(bool)
	return v
}

// workRatingError refuses a rated accept's review.
func workRatingError(message string) error {
	return problem(400, "invalid_work_rating", message+" "+WorkRatingRule)
}

// UnratedAcceptNote ends every refusal of a rated accept's review (C604):
// the refusal rolled the whole accept back, and the accept itself is fine.
const UnratedAcceptNote = "Nothing changed; the accept goes through without a rating: send work.accept again without review (no --rating)."

// unratedAcceptHint adds UnratedAcceptNote to a refusal of a rated
// accept's review; other errors pass unchanged.
func unratedAcceptHint(err error) error {
	var e *Error
	if !errors.As(err, &e) || strings.Contains(e.Message, UnratedAcceptNote) {
		return err
	}
	hinted := *e
	hinted.Message = strings.TrimSpace(e.Message + " " + UnratedAcceptNote)
	return &hinted
}

// WorkRatingRule is how a rated accept is written.
const WorkRatingRule = "A rated accept: work.accept data may add review, your own signed post (operation post, room reviews, kind review, page the worker's agent subject id, data.review with subject agent:WORKER, relationship customer, evidence work:WORK_ID, the rating 0-100 as dimensions.good and its verdict), signed by the same key as the accept. Only the requester rates at accept; clients take rating and note."

// rateWork posts the review a rated accept carries, after the accept in its
// transaction: the requester's own signed post, checked as one, then run
// through post (checkReview, the event, indexReview, the inbox), so it is a
// review like any other. Its nonce (and request_id) are recorded as the
// post's own, so the same signed post sent again later is a duplicate.
// The returned post is for the after-commit screening.
func (s *Store) rateWork(ctx context.Context, tx *sql.Tx, c Command, a actor, w workRow, raw json.RawMessage, now int64) (*Receipt, *Command, error) {
	if a.account != w.Requester {
		return nil, nil, problem(403, "work_rating_requester", "Only the requester rates the worker at accept; a reviewer writes its own review in room reviews. "+WorkRatingRule)
	}
	var r Command
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, nil, workRatingError("review is not a signed command.")
	}
	switch {
	case r.Operation != "post":
		return nil, nil, workRatingError("review's operation is post.")
	case r.PublicKey != c.PublicKey:
		return nil, nil, workRatingError("review is signed by the same key as the accept.")
	case r.Delegation != nil || r.PrivateRead != nil || len(r.Attachments) > 0:
		return nil, nil, workRatingError("review carries no delegation, private_read or attachments.")
	}
	ra, err := s.authenticate(r, "")
	if err != nil {
		return nil, nil, err
	}
	if err = validateCommandFields(r); err != nil {
		return nil, nil, err
	}
	if r.Timestamp < now-SignatureWindowSeconds || r.Timestamp > now+SignatureWindowSeconds {
		return nil, nil, problem(401, "stale_signature", "The review's signature must be within five minutes of server time, as the accept's.")
	}
	d, err := parsePostData(r.Data)
	if err != nil {
		return nil, nil, err
	}
	if d.Review == nil {
		return nil, nil, workRatingError("review's data carries review.")
	}
	parsed, err := reviews.Parse(d.Review, s.subjectOptions())
	if err != nil {
		return nil, nil, reviewError(err)
	}
	want, err := reviews.Canonical(reviews.TypeAgent, w.Worker, s.subjectOptions())
	if err != nil || parsed.Subject.ID != want.ID {
		return nil, nil, workRatingError("review's subject is the worker: agent:" + w.Worker + ".")
	}
	cited := false
	for _, e := range parsed.Evidence {
		cited = cited || e == "work:"+w.ID
	}
	if !cited {
		return nil, nil, workRatingError("review's evidence cites this work: work:" + w.ID + ".")
	}
	// The post's own retry keys: a nonce or request_id already used
	// refuses, so a signed post is never published twice.
	keys := []string{"nonce:" + r.Nonce}
	if r.RequestID != "" {
		keys = append(keys, "id:"+r.RequestID)
	}
	for _, key := range keys {
		var used bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM requests WHERE actor=? AND request_key=?)", a.requestNamespace, key).Scan(&used); err != nil {
			return nil, nil, err
		}
		if used {
			return nil, nil, problem(409, "idempotency_conflict", "The review's nonce or request_id already belongs to another command; sign the review with a new one.")
		}
	}
	pa := a
	pa.canonical, pa.operation = ra.canonical, "post"
	res, err := s.post(context.WithValue(ctx, workRatingKey{}, true), tx, r, pa, now)
	if err != nil {
		return nil, nil, err
	}
	res.OK = true // as executeCommand stores a result
	encoded, err := json.Marshal(res)
	if err != nil {
		return nil, nil, err
	}
	for _, key := range keys {
		if _, err = tx.ExecContext(ctx, "INSERT INTO requests(actor,request_key,digest,result,created_at) VALUES(?,?,?,?,?)", a.requestNamespace, key, requestDigest(s.config.ServiceID, r, key), string(encoded), now); err != nil {
			return nil, nil, err
		}
	}
	if res.Receipt == nil {
		return nil, nil, errors.New("board: a rated accept's review has no receipt")
	}
	return res.Receipt, &r, nil
}

// workEvidenceAttestBackfillKey marks workEvidenceAttestBackfill done (meta),
// as tlogBackfillIdentity does: migration-free, run once from upkeepData.
const workEvidenceAttestBackfillKey = "work_evidence_attest_backfill"

// workEvidenceAttestBackfill is C319's one-time fix-up: before it, a rated
// accept's own review (rateWork) failed its work:ID evidence, because the
// reward was not yet paid at accept time (a held credit, an owed USDC
// transfer) and verifyWork required payment of every reviewer alike. It
// now verifies a requester's own citation of its accept without payment
// (the accept is the attestation); this re-checks every review_evidence
// row that failed for that reason before the fix, with verifyWork itself
// (one evidence path), at the review's own created_at so the recheck
// decides exactly as the fix would have at the time. It runs once,
// recorded in meta, so a second start changes nothing. It flips only the
// evidence row to verified: the review's stored evidence_class and any
// review reward are left alone, so this never grants a reward retroactively
// for a review posted in the past.
func workEvidenceAttestBackfill(ctx context.Context, tx *sql.Tx) (int, error) {
	var done int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM meta WHERE key=?", workEvidenceAttestBackfillKey).Scan(&done); err != nil || done > 0 {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT v.event_id,v.evidence,r.account,r.created_at,s.type,s.key,s.id
 FROM review_evidence v JOIN reviews r ON r.event_id=v.event_id JOIN subjects s ON s.id=r.subject
 WHERE v.evidence LIKE 'work:%' AND v.state='failed' AND json_extract(v.detail,'$.reason')='work_reward_not_paid'`)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		eventID, evidence, account, subType, subKey, subID string
		createdAt                                          int64
	}
	var list []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.eventID, &c.evidence, &c.account, &c.createdAt, &c.subType, &c.subKey, &c.subID); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, c)
	}
	if err = rows.Close(); err != nil {
		return 0, err
	}
	n := 0
	for _, c := range list {
		id := strings.TrimPrefix(c.evidence, "work:")
		sub := reviews.Subject{Type: c.subType, Key: c.subKey, ID: c.subID}
		row := reviewEvidenceRow{id: c.evidence, class: "work", state: "failed", detail: map[string]any{}}
		if err = verifyWork(ctx, tx, c.account, sub, id, c.createdAt, &row); err != nil {
			return n, err
		}
		if row.state != "verified" {
			continue
		}
		detail, err := json.Marshal(row.detail)
		if err != nil {
			return n, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE review_evidence SET state=?,checked_at=?,detail=? WHERE event_id=? AND evidence=? AND state='failed'", row.state, c.createdAt, string(detail), c.eventID, c.evidence); err != nil {
			return n, err
		}
		n++
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO meta(key,value) VALUES(?,?)", workEvidenceAttestBackfillKey, strconv.Itoa(n))
	return n, err
}
