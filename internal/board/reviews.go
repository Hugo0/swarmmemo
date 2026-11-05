package board

// Subjects and reviews (RFC 0015 §4-§6, phase 1B; RFC 0019, the general
// core). A review is a signed post in room reviews whose page is the
// subject's id, kind review, carrying data.review; comments are ordinary
// replies in its thread. The subjects, reviews and review_evidence tables
// index those posts, written in the posting transaction (a lab record's
// state after commit, reviews_records.go), and nothing here deletes a row.
// They are not rebuildable from the events: evidence states, ownership and
// standing were checked against the board as it stood when the review was
// posted. REVIEWS gates the write path and every read.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/judge"
	"swarmmemo/internal/reviews"
	"swarmmemo/internal/services"
	"swarmmemo/internal/trust"
)

// ReviewsRoom is the room every review is posted in.
const ReviewsRoom = "reviews"

// Review caps (RFC 0019 §4) and the default order's parameters (RFC 0015
// §5.4). The order is version 1 of the reviews parameters; its weights,
// half-life and prior are internal/judge's (RFC 0020, the one source).
const (
	ReviewsPerAccountDay = 20
	// ReviewsOperatorDay is the daily cap for the operator's importer and lab
	// keys (relationship outside_source or lab, each checked against its
	// parameters first): an import or a lab run rates a catalogue, not a few
	// things an agent used.
	ReviewsOperatorDay    = 200
	ReviewEvidenceDays    = 30
	ReviewPageDefault     = 20
	ReviewPageMax         = 50
	SubjectsPageDefault   = 20
	SubjectsPageMax       = 50
	SubjectQueryMaxBytes  = 128
	ReviewHalfLifeDays    = judge.HalfLifeDays
	ReviewBurstCap        = 3.0
	ReviewBurstDays       = 7
	ReviewPriorWeight     = judge.PriorWeight * 1.0
	ReviewPriorValue      = 1.0 * (judge.Neutral - judge.Min) / (judge.Max - judge.Min)
	ReviewOrderVersion    = 1
	reviewThreadCountMax  = 1000
	labRunsShown          = 10
	otherRatersShown      = 20
	reviewOffsetCursorMax = 10000
)

// ReviewEvidenceWeights are the default order's evidence classes
// (judge.EvidenceWeightPPM).
var ReviewEvidenceWeights = ppmFactors(judge.EvidenceWeightPPM)

// reviewClassRank orders the classes: a review counts its best verified one.
var reviewClassRank = []string{"opinion", "free_use", "observed", "work", "paid_use"}

// ReviewRelationshipFactors are the default order's relationship factors;
// lab and outside_source are shown apart and never in the community order.
// (judge.RelationshipFactorPPM).
var ReviewRelationshipFactors = ppmFactors(judge.RelationshipFactorPPM)

// ppmFactors is a ppm table as the factors the SQL order multiplies.
func ppmFactors(ppm map[string]int64) map[string]float64 {
	out := make(map[string]float64, len(ppm))
	for k, v := range ppm {
		out[k] = float64(v) / 1e6
	}
	return out
}

// reviewSchema (schema 30). reviews.event_id references events(id) and
// review_evidence.event_id references reviews(event_id), with foreign keys
// on: nothing deletes an event today (hides keep the row). Any erasure of
// events added later must delete the event's review_evidence rows, then its
// reviews row, first (TestReviewRowsBlockEventErasure).
const reviewSchema = `
CREATE TABLE IF NOT EXISTS subjects (
 id TEXT PRIMARY KEY, type TEXT NOT NULL, key TEXT NOT NULL, parent TEXT NOT NULL DEFAULT '',
 properties TEXT NOT NULL DEFAULT '{}', state TEXT NOT NULL DEFAULT 'active' CHECK(state IN ('active','stale')),
 first_seen INTEGER NOT NULL, last_seen INTEGER NOT NULL, listed_at INTEGER NOT NULL DEFAULT 0,
 UNIQUE(type,key));
CREATE INDEX IF NOT EXISTS subjects_listed ON subjects(listed_at) WHERE listed_at>0;
CREATE TABLE IF NOT EXISTS reviews (
 event_id TEXT PRIMARY KEY REFERENCES events(id), root_event TEXT NOT NULL, subject TEXT NOT NULL REFERENCES subjects(id),
 account TEXT NOT NULL, author TEXT NOT NULL, root TEXT NOT NULL,
 verdict TEXT NOT NULL CHECK(verdict IN ('good','mixed','bad','unusable','unverified')),
 relationship TEXT NOT NULL, relationship_raised TEXT NOT NULL DEFAULT '',
 works INTEGER NOT NULL DEFAULT -1, as_described INTEGER NOT NULL DEFAULT -1, safe INTEGER NOT NULL DEFAULT -1, good INTEGER NOT NULL DEFAULT -1,
 rubric TEXT NOT NULL DEFAULT '', rubric_version TEXT NOT NULL DEFAULT '', rubric_sha256 TEXT NOT NULL DEFAULT '',
 score INTEGER NOT NULL DEFAULT -1, band_lo INTEGER NOT NULL DEFAULT -1, band_hi INTEGER NOT NULL DEFAULT -1,
 grade TEXT NOT NULL DEFAULT '', flags TEXT NOT NULL DEFAULT '',
 evidence_class TEXT NOT NULL DEFAULT 'opinion', seasoned INTEGER NOT NULL DEFAULT 0 CHECK(seasoned IN (0,1)),
 review TEXT NOT NULL, created_at INTEGER NOT NULL, current INTEGER NOT NULL DEFAULT 1 CHECK(current IN (0,1)));
CREATE UNIQUE INDEX IF NOT EXISTS reviews_current ON reviews(subject,account) WHERE current=1;
CREATE INDEX IF NOT EXISTS reviews_subject ON reviews(subject,current,created_at);
CREATE INDEX IF NOT EXISTS reviews_account ON reviews(account,created_at);
CREATE TABLE IF NOT EXISTS review_evidence (
 event_id TEXT NOT NULL REFERENCES reviews(event_id), evidence TEXT NOT NULL, class TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('verified','failed','unknown')), checked_at INTEGER NOT NULL,
 detail TEXT NOT NULL DEFAULT '{}', PRIMARY KEY(event_id,evidence));
`

// ReviewsEnabled reports whether REVIEWS is on.
func (s *Store) ReviewsEnabled() bool { return s.config.Features.Reviews }

func reviewsUnavailable() error {
	return problem(404, "reviews_unavailable", "Subjects and reviews are not enabled on this board; /capabilities says what is.")
}

// subjectOptions are the board's own hosts: their account pages are never subjects.
func (s *Store) subjectOptions() reviews.Options { return subjectOptionsFor(s.config.ServiceID) }

// subjectOptionsFor is subjectOptions for a SERVICE_ID, before the Store exists.
func subjectOptionsFor(serviceID string) reviews.Options {
	id := strings.ToLower(serviceID)
	if id == "" {
		id = "swarmmemo.com"
	}
	return reviews.Options{OwnHosts: []string{id, "www." + id}}
}

func reviewError(err error) error {
	var se *reviews.Error
	if errors.As(err, &se) {
		if se.Code == "subject_refused" {
			return problem(422, "subject_refused", se.Message)
		}
		return problem(400, "invalid_subject", se.Message)
	}
	var pe *reviews.ParseError
	if errors.As(err, &pe) {
		return problem(400, "invalid_review", pe.Message)
	}
	return err
}

// pendingReview is a checked review waiting for its event id.
type pendingReview struct {
	review   reviews.Review
	raw      string
	account  string
	author   string
	previous string // the account's current review of the subject, if any
	// seasoned: the account had a visible public post VoterMinAge old when
	// it reviewed, so its review weighs at least a whole vote (the vote
	// floor, trust.Params.FlooredVoteWeight).
	seasoned bool
	// createdAt is when the review was posted, when that is not now: a
	// review whose tx: evidence verifies later is capped by its own day.
	createdAt int64
}

type reviewEvidenceRow struct {
	id, class, state string
	detail           map[string]any
	counterparty     string // a work item's other account, the one other reviewer who may cite it
}

// checkReview applies the review rules to a post before anything is
// charged, with point reads only: REVIEWS on, a signed root key (never a
// worker key), room reviews (the operator's public room), a top-level kind
// review whose page is the subject's id, the reserved relationships (lab for
// the operator's lab keys, outside_source for its importer keys), the daily
// cap and the lab record cap, one current review per account and subject (a
// second supersedes the first, or replaces one that can take no new
// version). The evidence is checked once the post is admitted and charged
// (indexReview). nil when the post is not a review.
func (s *Store) checkReview(ctx context.Context, tx *sql.Tx, c Command, a actor, d postData, now int64) (*pendingReview, error) {
	if d.Review == nil {
		// While REVIEWS is on, room reviews takes reviews and replies only.
		if s.ReviewsEnabled() && c.Room == ReviewsRoom && c.ReplyTo == "" {
			return nil, problem(400, "review_required", `A top-level post in room reviews is a review: kind review, page the subject's id, data {"schema":1,"review":{...}} (RFC 0019); comment on a review by replying to it.`)
		}
		return nil, nil
	}
	if !s.ReviewsEnabled() {
		return nil, reviewsUnavailable()
	}
	if a.grant != nil {
		return nil, problem(403, "review_worker_key", "A worker key cannot review: post the review signed with your own key.")
	}
	r, err := reviews.Parse(d.Review, s.subjectOptions())
	if err != nil {
		return nil, reviewError(err)
	}
	switch {
	case c.Room != ReviewsRoom:
		return nil, problem(400, "review_room", "A review is posted in room reviews.")
	case c.ReplyTo != "":
		return nil, problem(400, "review_room", "A review is a top-level post; comment on a review by replying to it without data.review.")
	case c.Kind != "review":
		return nil, problem(400, "review_room", "A review's kind is review.")
	case c.Page != r.Subject.ID:
		return nil, problem(400, "review_page", fmt.Sprintf("A review's page is its subject's id: %s for %s.", r.Subject.ID, r.Subject.URI()))
	case c.To != "":
		return nil, problem(400, "review_room", "A review is addressed to no one; it is public on its subject's page.")
	}
	switch r.Relationship {
	case "outside_source":
		if !s.allowedImporter(ctx, tx, a, now) {
			return nil, problem(403, "review_relationship_reserved", "relationship outside_source is posted only by the operator's importer keys; review as yourself with another relationship.")
		}
	case "lab":
		if !s.listedAccount(ctx, tx, a, LabsParamsNamespace, now) {
			return nil, problem(403, "review_relationship_reserved", "relationship lab is posted only by the operator's lab keys (the labs parameters); review as yourself with relationship tester, and cite your rubric's observations.")
		}
	}
	if err = s.checkOfferingSubject(ctx, tx, r.Subject); err != nil {
		return nil, err
	}
	if r.Subject.Type == reviews.TypeAgent {
		var known int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM identities WHERE account=?", r.Subject.Key).Scan(&known); err != nil {
			return nil, err
		}
		if known == 0 {
			return nil, problem(404, "subject_not_found", "No SwarmMemo agent has that account fingerprint; an agent subject is agent:ACCOUNT (agent.get shows it).")
		}
	}
	if svc, method, ok := r.Subject.ToolMethod(); ok && (s.services.engine == nil || !s.services.engine.Registry().IsTool(svc, method)) {
		return nil, problem(404, "subject_not_found", "This board has no tool "+r.Subject.Key+"; a tool subject is tool:swarmmemo:SERVICE.METHOD, an id tools search lists.")
	}
	midnight := now - now%86400
	// A rated accept's review (workRating, C604) is bound to its verdict:
	// one per accepted result, by the work's requester, citing that work. It
	// neither counts toward the daily cap nor is refused by it; every other
	// review is capped as before.
	if !workRating(ctx) {
		var today int
		if err = tx.QueryRowContext(ctx, reviewsCappedTodaySQL, a.account, midnight).Scan(&today); err != nil {
			return nil, err
		}
		limit := ReviewsPerAccountDay
		if r.Relationship == "outside_source" || r.Relationship == "lab" {
			limit = ReviewsOperatorDay
		}
		if today >= limit {
			return nil, (&Error{Status: 429, Code: "review_daily_limit", Message: fmt.Sprintf("An agent writes at most %d reviews (new versions included) per UTC day; a rating given with work.accept on your own task does not count. Nothing was published.", limit)}).until(allowance.NextDay(now), now)
		}
	}
	if hasRecordDoc(r) {
		// Recomputing a record is the costly part of a review: the account's
		// records of today that failed or still wait are capped.
		var open int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM reviews r JOIN review_evidence v ON v.event_id=r.event_id AND v.evidence>=? AND v.evidence<?
 WHERE r.account=? AND r.created_at>=? AND v.state IN ('unknown','failed')`, RecordEvidencePrefix, recordEvidenceEnd, a.account, midnight).Scan(&open); err != nil {
			return nil, err
		}
		if open >= ReviewRecordFailuresPerDay {
			return nil, &Error{Status: 429, Code: "review_record_limit", Message: fmt.Sprintf("At most %d of an agent's lab records a UTC day may fail or wait to be recomputed; check yours with swarmmemo rubric score before posting. Nothing was published.", ReviewRecordFailuresPerDay), RetryAfter: untilMidnight(now)}
		}
	}
	p := &pendingReview{review: r, raw: string(d.Review), account: a.account, author: a.id}
	var currentAuthor string
	err = tx.QueryRowContext(ctx, "SELECT event_id,author FROM reviews WHERE subject=? AND account=? AND current=1", r.Subject.ID, a.account).Scan(&p.previous, &currentAuthor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	switch {
	case p.previous == "" && d.Supersedes != "":
		var n int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM reviews WHERE event_id=?", d.Supersedes).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, problem(409, "review_supersede", "supersedes names a message that is not a review; a review supersedes only your current review of the same subject.")
		}
	case p.previous != "" && d.Supersedes != p.previous && currentAuthor == a.id && !workRating(ctx):
		// (A rated accept's review replaces the current one as a fresh
		// review, below: the newest paid work speaks, workreputation.go.)
		// A current review that can take no new version (hidden, or at
		// MaxVersions) is replaced by a fresh review, so no account is ever
		// locked out of a subject.
		stuck, err := reviewFrozen(ctx, tx, p.previous)
		if err != nil {
			return nil, err
		}
		if !stuck || d.Supersedes != "" {
			return nil, &Error{Status: 409, Code: "review_exists", Message: fmt.Sprintf("You already reviewed this subject (%s): post the new review with data.supersedes %q, so history is kept and the newest counts.", p.previous, p.previous), Details: map[string]any{"current_review": p.previous}}
		}
	}
	// A rotated key cannot supersede its predecessor's post; its new review
	// replaces the account's current one all the same (indexReview).
	//
	// The room is the operator's public room, opened by the first review
	// whatever NAME_GATE says (a review never opens a room of its author's);
	// room.create cannot claim the name (reservedRoomName). A row that is
	// anything else holds no review.
	var visibility, owner string
	err = tx.QueryRowContext(ctx, "SELECT visibility,owner FROM rooms WHERE name=?", ReviewsRoom).Scan(&visibility, &owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err = tx.ExecContext(ctx, "INSERT INTO rooms(name,visibility,owner,created_at) VALUES(?,'public','',?)", ReviewsRoom, now); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case visibility != "public" || owner != "":
		return nil, problem(409, "review_room_taken", "Room reviews on this board is not the operator's public room, so it holds no review; nothing was published. The operator frees it (RUNBOOK, Subjects and reviews).")
	}
	return p, nil
}

// reviewsCappedTodaySQL is the count the daily review cap reads (?1 the
// account, ?2 the UTC midnight): the account's reviews since midnight, less
// its rated accepts' reviews (rateWork, C604). Those are recognised from
// what the accept left behind, not from anything the client says: a
// relationship customer review citing work:ID of a work the account
// requested, written by the accepting key in the very second of that
// work's work.accept (the rating is posted in the accept's transaction).
// Counted once per work (DISTINCT), so each accepted result frees at most
// one review, and accepts are one per result.
const reviewsCappedTodaySQL = `SELECT (SELECT count(*) FROM reviews WHERE account=?1 AND created_at>=?2)
 - (SELECT count(DISTINCT w.id) FROM reviews r
 JOIN review_evidence v ON v.event_id=r.event_id AND v.evidence>='work:' AND v.evidence<'work;'
 JOIN works w ON w.id=substr(v.evidence,6) AND w.requester=r.account
 WHERE r.account=?1 AND r.created_at>=?2 AND r.relationship='customer'
 AND EXISTS(SELECT 1 FROM work_transitions t WHERE t.work_id=w.id AND t.operation='work.accept' AND t.author=r.author AND t.accepted_at=r.created_at))`

func hasRecordDoc(r reviews.Review) bool {
	return r.Relationship == "lab" && r.Record != nil && r.Record.Doc != ""
}

// reviewFrozen reports whether a review post can take no new version: a
// version of it is hidden, or it has MaxVersions (checkSupersession's rules).
func reviewFrozen(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var origin string
	if err := tx.QueryRowContext(ctx, "SELECT CASE WHEN origin<>'' THEN origin ELSE id END FROM events WHERE id=?", id).Scan(&origin); err != nil {
		return false, err
	}
	var versions int
	var hidden bool
	err := tx.QueryRowContext(ctx, "WITH "+messageVersions+" SELECT count(*),coalesce(max(hidden<>0),0) FROM v", origin).Scan(&versions, &hidden)
	return hidden || versions >= MaxVersions, err
}

// indexReview writes the derived rows once the review's event exists and
// the post is charged: the evidence checked against what this board holds,
// ownership, the subject (created by its first review), the review (the
// account's current one; its predecessor stays, not current) and its
// evidence states. A lab record waits for the recomputer (reviews_records.go).
// It returns the review's reward, for the receipt (reviewrewards.go).
func (s *Store) indexReview(ctx context.Context, tx *sql.Tx, p *pendingReview, eventID, origin string, now int64) (*ReviewReward, []FailedEvidence, error) {
	r := p.review
	sub := r.Subject
	evidence, err := s.verifyEvidence(ctx, tx, p.account, r, now)
	if err != nil {
		return nil, nil, err
	}
	owner, err := s.ownsSubject(ctx, tx, p.account, sub)
	if err != nil {
		return nil, nil, err
	}
	raised := ""
	if owner && r.Relationship != "owner" {
		raised = "owner"
	}
	parent := ""
	if ps, ok := sub.Parent(); ok && ps.ID != sub.ID {
		parent = ps.ID
	}
	// The review is about the subject's version now: an offering's current
	// version, 1 for every other subject (offering_versions.go).
	version, err := s.subjectVersion(ctx, tx, sub)
	if err != nil {
		return nil, nil, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO subjects(id,type,key,parent,first_seen,last_seen,version) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET last_seen=excluded.last_seen,version=excluded.version", sub.ID, sub.Type, sub.Key, parent, now, now, version); err != nil {
		return nil, nil, err
	}
	if p.previous != "" {
		if _, err = tx.ExecContext(ctx, "UPDATE reviews SET current=0 WHERE event_id=?", p.previous); err != nil {
			return nil, nil, err
		}
	}
	// Evidence cited by two reviews counts once: for the other account's
	// current review when that one already counts it, else for this one.
	// Only a work item can be two accounts' evidence (its requester's and
	// its worker's); calls, fetches and stamps bind to their own account.
	// Fixed when posted, so the order reads it from the stored class.
	for i := range evidence {
		e := &evidence[i]
		if e.state != "verified" || e.counterparty == "" {
			continue
		}
		var first string
		err = tx.QueryRowContext(ctx, `SELECT r.event_id FROM reviews r JOIN events ev ON ev.id=r.event_id JOIN review_evidence v ON v.event_id=r.event_id AND v.evidence=? AND v.state='verified'
 WHERE r.subject=? AND r.account=? AND r.current=1 AND ev.hidden=0 AND json_extract(v.detail,'$.counted_for') IS NULL`, e.id, sub.ID, e.counterparty).Scan(&first)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, err
		}
		if first != "" {
			e.detail["counted_for"] = first
		}
	}
	root := eventID
	if origin != "" {
		root = origin
	}
	dim := func(k string) int {
		if v, ok := r.Dimensions[k]; ok {
			return v
		}
		return -1
	}
	score, lo, hi := -1, -1, -1
	if r.Score != nil {
		score = *r.Score
	}
	if r.Band != nil {
		lo, hi = r.Band[0], r.Band[1]
	}
	var rubricID, rubricVersion, sum string
	if r.Rubric != nil {
		rubricID, rubricVersion, sum = r.Rubric.ID, r.Rubric.Version, r.Rubric.SHA256
	}
	class := "opinion"
	for _, e := range evidence {
		if e.state == "verified" && e.detail["counted_for"] == nil && slices.Index(reviewClassRank, e.class) > slices.Index(reviewClassRank, class) {
			class = e.class
		}
	}
	if p.seasoned, err = seasonedVoter(ctx, tx, p.account, now); err != nil {
		return nil, nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO reviews(event_id,root_event,subject,account,author,root,verdict,relationship,relationship_raised,works,as_described,safe,good,rubric,rubric_version,rubric_sha256,score,band_lo,band_hi,grade,flags,evidence_class,seasoned,review,created_at,current,version)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?)`, eventID, root, sub.ID, p.account, p.author, p.account, r.Verdict, r.Relationship, raised,
		dim("works"), dim("as_described"), dim("safe"), dim("good"), rubricID, rubricVersion, sum, score, lo, hi, r.Grade, strings.Join(r.Flags, ","), class, p.seasoned, p.raw, now, version); err != nil {
		return nil, nil, err
	}
	pending := false
	var onChain []string // tx: rows still to be read on Base
	for _, e := range evidence {
		detail, _ := json.Marshal(e.detail)
		if _, err = tx.ExecContext(ctx, "INSERT INTO review_evidence(event_id,evidence,class,state,checked_at,detail) VALUES(?,?,?,?,?,?)", eventID, e.id, e.class, e.state, now, string(detail)); err != nil {
			return nil, nil, err
		}
		pending = pending || e.state == "unknown" && (e.class == "record" || e.detail["reason"] == TxPending)
		if e.state == "unknown" && e.detail["reason"] == TxPending {
			onChain = append(onChain, e.id)
		}
	}
	// Listed (in subjects.list, Find and search engines) once two accounts
	// review it, its verified owner does (RFC 0015 §4.3), or an operator
	// importer key imports an outside source's rating of it (relationship
	// outside_source is refused to any other key); a declared owner
	// relationship lists nothing. Never unlisted. An account has one current
	// review of a subject, so two such rows are two accounts.
	if _, err = tx.ExecContext(ctx, `UPDATE subjects SET listed_at=?1 WHERE id=?2 AND listed_at=0 AND (?3 OR (SELECT count(*) FROM
 (SELECT 1 FROM reviews r JOIN events e ON e.id=r.event_id WHERE r.subject=?2 AND r.current=1 AND e.hidden=0 AND r.relationship<>'outside_source' LIMIT 2))>=2)`, now, sub.ID, owner || r.Relationship == "outside_source"); err != nil {
		return nil, nil, err
	}
	if pending {
		s.wakeReviewRecords()
	}
	if err = s.tellSubjectOwners(ctx, tx, p, eventID, origin, now); err != nil {
		return nil, nil, err
	}
	reward, err := s.offerReviewReward(ctx, tx, p, eventID, class, raised, now)
	if err != nil {
		return nil, nil, err
	}
	var failed []FailedEvidence
	for _, e := range evidence {
		if e.state == "failed" {
			failed = append(failed, failedEvidence(e.id, evidenceReason(e.detail)))
		}
	}
	// A tx: payment not read yet can still qualify: the reward is pending
	// the evidence check, not none (raiseReviewClass offers it once verified).
	if reward != nil && reward.State == "none" && reward.Reason == "evidence_class" && len(onChain) > 0 {
		reward = pendingEvidenceReward(onChain)
	} else if reward != nil && reward.State == "none" && reward.Reason == "evidence_class" && len(failed) > 0 {
		lines := make([]string, len(failed))
		for i, f := range failed {
			lines[i] = f.Line
		}
		reward.Note += " " + strings.Join(lines, "; ") + "."
	}
	return reward, failed, nil
}

// ownsSubject reports whether account owns s through its identity links
// (RFC 0015 §4.4): an agent owns its own agent subject; a verified domain
// link to a URL or domain subject's host or registrable domain owns it.
func (s *Store) ownsSubject(ctx context.Context, tx *sql.Tx, account string, sub reviews.Subject) (bool, error) {
	owners, err := s.subjectOwners(ctx, tx, sub)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(owners, func(o SubjectOwner) bool { return o.Account == account }), nil
}

// SubjectOwner is an account that owns a subject, and the link that says so.
type SubjectOwner struct {
	Account string `json:"account"`
	Handle  string `json:"handle,omitempty"`
	Link    string `json:"link"`
}

// A paid API listed through x402.submit (C299) is also its submitter's
// while it is listed, by either proof: a domain link, or a wallet (the
// API pays the submitter's verified wallet), link "submitted:PROOF"
// (services.Registry.X402Submitter). One of SwarmMemo's own tools (a tool
// subject, C301) is the operator's: the service accounts of the current
// trust parameters, link "operator" (toolOwners). Owner replies, owner
// pings, the reputation's sells, the trust run's owner records and the
// own-root exclusion (judge.OwnSubject) all follow from this one list.
func (s *Store) subjectOwners(ctx context.Context, q allowance.Querier, sub reviews.Subject) ([]SubjectOwner, error) {
	if sub.Type == reviews.TypeTool {
		return s.toolOwners(ctx, q)
	}
	out, err := domainOwners(ctx, q, sub, s.subjectOptions())
	if err != nil || sub.Type != reviews.TypeX402 || s.services.engine == nil {
		return out, err
	}
	account, proof := s.services.engine.Registry().X402Submitter(sub.ID)
	if account == "" || slices.ContainsFunc(out, func(o SubjectOwner) bool { return o.Account == account }) {
		return out, nil
	}
	var handle string
	if err = scanAll(ctx, q, "SELECT handle FROM identities WHERE account=? AND successor='' AND role='primary' LIMIT 1", []any{account}, func(r *sql.Rows) error { return r.Scan(&handle) }); err != nil {
		return nil, err
	}
	return append(out, SubjectOwner{Account: account, Handle: handle, Link: "submitted:" + proof}), nil
}

// toolOwners are the owners of every tool subject: SwarmMemo's own
// accounts (operatorAccountSet, the one list of them), so the operator's
// personas' reviews of its own tools weigh 0 and their replies are owner
// replies.
func (s *Store) toolOwners(ctx context.Context, q allowance.Querier) ([]SubjectOwner, error) {
	ops, err := operatorAccountSet(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]SubjectOwner, 0, len(ops))
	for _, a := range funnelSortedKeys(ops) {
		o := SubjectOwner{Account: a, Link: "operator"}
		if err = scanAll(ctx, q, "SELECT handle FROM identities WHERE account=? AND successor='' AND role='primary' LIMIT 1", []any{a}, func(r *sql.Rows) error { return r.Scan(&o.Handle) }); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// domainOwners is subjectOwners without the submitted listings: an
// offering's provider, an agent subject's agent, the verified domain links.
func domainOwners(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, sub reviews.Subject, opt reviews.Options) ([]SubjectOwner, error) {
	// An agent offering on this board is its provider's, never the board
	// domain's owners' (C200).
	if _, _, ok := reviews.OfferingPath(sub, opt); ok {
		account, handle, _, found, err := offeringOwner(ctx, q, sub, opt)
		if err != nil || !found {
			return nil, err
		}
		return []SubjectOwner{{Account: account, Handle: handle, Link: "offering"}}, nil
	}
	if sub.Type == reviews.TypeAgent {
		rows, err := q.QueryContext(ctx, "SELECT handle FROM identities WHERE account=? AND successor='' AND role='primary' LIMIT 1", sub.Key)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []SubjectOwner
		for rows.Next() {
			var h string
			if err = rows.Scan(&h); err != nil {
				return nil, err
			}
			out = append(out, SubjectOwner{Account: sub.Key, Handle: h, Link: "self"})
		}
		return out, rows.Err()
	}
	host := sub.Host()
	if host == "" {
		return nil, nil
	}
	names := []any{host, reviews.RegistrableDomain(host)}
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT i.account,coalesce((SELECT h.handle FROM identities h WHERE h.account=i.account AND h.successor='' AND h.role='primary' LIMIT 1),''),l.value
 FROM identity_links l JOIN identities i ON i.id=l.agent WHERE l.kind='domain' AND l.state='verified' AND l.value IN (?,?) ORDER BY i.account LIMIT 8`, names...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SubjectOwner
	for rows.Next() {
		var o SubjectOwner
		if err = rows.Scan(&o.Account, &o.Handle, &o.Link); err != nil {
			return nil, err
		}
		o.Link = "domain:" + o.Link
		out = append(out, o)
	}
	return out, rows.Err()
}

// verifyEvidence checks each cited evidence id against what this board
// holds, with point reads in the posting transaction (RFC 0015 §5.2). A
// failed check never refuses the review: the page shows the evidence as
// failed. Evidence binds to the account that created it, so another
// account's call, fetch, work or stamp fails here (without saying whether it
// exists). A lab record is added as a pending row for the recomputer.
func (s *Store) verifyEvidence(ctx context.Context, tx *sql.Tx, account string, r reviews.Review, now int64) ([]reviewEvidenceRow, error) {
	var out []reviewEvidenceRow
	for _, id := range r.Evidence {
		kind, value, _ := strings.Cut(id, ":")
		row := reviewEvidenceRow{id: id, state: "failed", detail: map[string]any{}}
		var err error
		switch kind {
		case "call":
			row.class = "free_use"
			err = s.verifyCallEvidence(ctx, tx, account, r.Subject, value, now, &row)
		case "fetch":
			row.class = "observed"
			err = s.verifyFetch(ctx, tx, account, r, value, now, &row)
		case "work":
			row.class = "work"
			err = verifyWork(ctx, tx, account, r.Subject, value, now, &row)
		case "stamp":
			row.class = "observed"
			err = verifyStamp(ctx, tx, account, value, &row)
		case "tx":
			// Read on chain after commit (reviews_tx.go): pending until then.
			err = s.checkTxEvidence(ctx, tx, r.Subject, value, &row)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	// A stamp proves only that the reviewer held some bytes. It is observed
	// evidence of this subject when it stamps the raw bytes of a cited fetch
	// that verified under the subject; any other stamp weighs as opinion.
	for i := range out {
		st := &out[i]
		if !strings.HasPrefix(st.id, "stamp:") || st.state != "verified" {
			continue
		}
		hash := strings.TrimPrefix(st.id, "stamp:")
		for _, f := range out {
			if strings.HasPrefix(f.id, "fetch:") && f.state == "verified" && f.detail["raw_sha256"] == hash {
				st.detail["fetch"] = f.id
			}
		}
		if st.detail["fetch"] == nil {
			st.class, st.detail["note"] = "opinion", "stamp_not_of_a_cited_fetch"
		}
	}
	if hasRecordDoc(r) {
		out = append(out, reviewEvidenceRow{id: RecordEvidencePrefix + r.Record.SHA256, class: "record", state: "unknown", detail: map[string]any{"doc": r.Record.Doc, "reason": RecordPending}})
	}
	return out, nil
}

// RecordEvidencePrefix names the evidence row of a lab record recomputed
// here; it is not cited evidence and never sets a review's class.
const RecordEvidencePrefix = "record:"

func (r *reviewEvidenceRow) fail(reason string) {
	r.state, r.detail = "failed", map[string]any{"reason": reason}
}

// ReviewEvidenceReasons say in plain words what each evidence check's reason
// code (detail reason, or note) means: the one table the review receipt,
// message reads, review cards and the web pages use. The codes stay as they
// are, for machines.
var ReviewEvidenceReasons = map[string]string{
	"not_your_call":                          "the call was not made by the reviewer's account: there is no such call here, its caller key belongs to another account, or it has no caller key and was not paid from a wallet linked to the reviewer's account",
	"not_a_finished_x402_call":               "it is not an x402 call that has finished (done or failed)",
	"not_a_finished_call":                    "it is not a call of the tool that has finished (done or failed)",
	"older_than_30_days":                     "it was made (or mined) more than 30 days before the review",
	"call_to_another_resource":               "the call went to another resource, offering or tool than the subject reviewed",
	"not_a_paid_offering_call":               "the offering call's payment has not settled to the provider (paid, answered, overdue or refunded)",
	"paid_from_the_providers_wallet":         "the call or payment was paid from one of the provider's own linked wallets, so no money reached another account",
	"not_your_fetch":                         "the fetch was not made by the reviewer's account, or there is no such fetch here",
	"not_a_finished_fetch":                   "it is not a fetch that finished",
	"fetch_of_another_site":                  "the fetched page is on another site than the subject (or the cited source)",
	"not_your_public_work":                   "there is no such public work here, or the reviewer's account is neither its requester nor its worker",
	"work_not_accepted_between_two_accounts": "the work is not accepted, or its requester and worker are the same account",
	"work_reward_not_paid":                   "no reward for this work was paid to the worker before the review",
	"request_does_not_name_the_subject":      "the work request does not name the subject reviewed (an agent subject is named by being the work's other party)",
	"not_your_stamp":                         "the stamp was not made by the reviewer's account, or there is no such stamp here",
	"stamp_not_of_a_cited_fetch":             "the stamp is not of a cited fetch's raw bytes, so it weighs as opinion",
	txUnchecked:                              "on-chain payments are not checked on this board, so it weighs as opinion",
	TxPending:                                "waiting to be read on chain",
	"tx_not_on_base":                         "the transaction is not on Base (eip155:8453), the one chain checked",
	"not_an_x402_or_mpp_subject":             "the subject is not an x402 or MPP endpoint, so it has no pay_to to pay",
	"subject_pay_to_unknown":                 "no catalogue, pinned resource or offering here names the subject's pay_to, so the payment cannot be matched",
	"tx_not_found":                           "no mined transaction has this hash on Base, a day after the review",
	"tx_failed":                              "the transaction failed on chain (status 0) and paid nothing",
	"tx_after_the_review":                    "the transaction was mined after the review",
	"tx_not_to_the_subjects_pay_to":          "the transaction has no Base USDC transfer to the subject's pay_to",
	"tx_not_from_your_wallet":                "the transfer to the subject's pay_to is not from a wallet linked to the reviewer's account",
	"tx_amount_zero":                         "the transfer from the reviewer's wallet to the subject's pay_to moved no USDC",
	"tx_cited_by_another_review":             "another account's review of this subject already counts this transaction",
	"not_your_doc_version":                   "there is no such doc version of the reviewer's",
	"record_unreadable":                      "the record could not be read",
	"not_observations_of_this_rubric":        "the record does not hold observations for this rubric",
	"observations_of_another_subject":        "the observations are of another subject",
	"as_of_after_the_review":                 "the observations are dated after the review",
	"as_of_older_than_30_days":               "the observations are dated more than 30 days before the review",
	"recomputed_result_differs":              "recomputed with the rubric, the result differs from the review's",
	RecordPending:                            "waiting to be recomputed",
}

// evidenceReason is an evidence row's reason code: its note when it has one
// (a verified stamp that weighs as opinion), else its reason.
func evidenceReason(detail map[string]any) string {
	if note, ok := detail["note"].(string); ok {
		return note
	}
	reason, _ := detail["reason"].(string)
	return reason
}

// FailedEvidence is a cited evidence id whose check failed, as a fresh
// review's receipt names it: the reason code, what it means, and one line
// that says both.
type FailedEvidence struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
	Why    string `json:"why,omitempty"`
	Line   string `json:"line"`
}

func failedEvidence(id, reason string) FailedEvidence {
	f := FailedEvidence{ID: id, Reason: reason, Why: ReviewEvidenceReasons[reason], Line: id + " failed: " + reason}
	if f.Why != "" {
		f.Line += " (" + f.Why + ")"
	}
	return f
}

// subjectMatchesURL reports whether a URL's method and address fall under
// sub: the same endpoint (x402 or mpp), the same origin for a site, the
// same registrable domain for a domain.
func subjectMatchesURL(sub reviews.Subject, method, raw string, opt reviews.Options) bool {
	switch sub.Type {
	case reviews.TypeX402, reviews.TypeMPP:
		got, err := reviews.Canonical(sub.Type, strings.ToUpper(method)+" "+raw, opt)
		return err == nil && got.ID == sub.ID
	case reviews.TypeSite:
		got, err := reviews.Canonical(reviews.TypeSite, raw, opt)
		return err == nil && got.ID == sub.ID
	case reviews.TypeMCP:
		got, err := reviews.Canonical(reviews.TypeMCP, raw, opt)
		return err == nil && got.ID == sub.ID
	case reviews.TypeDomain:
		got, err := reviews.Canonical(reviews.TypeDomain, raw, opt)
		return err == nil && got.ID == sub.ID
	}
	return false
}

// verifyCallEvidence checks call:ID, one check for every metered call the
// board records: an offering call (oc_ID); else a service call the
// reviewer made, named by its call id or by its receipt's hold_id (the same
// call). A relayed x402 call is evidence for the resource it called; a call
// of one of SwarmMemo's own tools (C301) for that tool's subject
// (reviews.TypeTool). Either is paid_use when its hold committed paid or
// earned credit (callPaid), else free_use.
func (s *Store) verifyCallEvidence(ctx context.Context, tx *sql.Tx, reviewer string, sub reviews.Subject, id string, now int64, row *reviewEvidenceRow) error {
	if strings.HasPrefix(id, "oc_") {
		return s.verifyOfferingCall(ctx, tx, reviewer, sub, id, now, row)
	}
	var account, service, method, state, public, hold string
	var created int64
	const callCols = "SELECT account,service,method,state,public,hold_id,created_at FROM service_calls"
	err := tx.QueryRowContext(ctx, callCols+" WHERE id=?", id).Scan(&account, &service, &method, &state, &public, &hold, &created)
	if errors.Is(err, sql.ErrNoRows) && id != "" {
		err = tx.QueryRowContext(ctx, callCols+" WHERE account=? AND hold_id=? LIMIT 1", reviewer, id).Scan(&account, &service, &method, &state, &public, &hold, &created)
	}
	if errors.Is(err, sql.ErrNoRows) || err == nil && account != reviewer {
		row.fail("not_your_call")
		return nil
	}
	if err != nil {
		return err
	}
	finished := state == "done" || state == "failed"
	toolService, toolMethod, tool := sub.ToolMethod()
	switch {
	case tool && !finished:
		row.fail("not_a_finished_call")
		return nil
	case tool && (service != toolService || method != toolMethod):
		row.fail("call_to_another_resource")
		return nil
	case !tool && (service != "x402" || !finished):
		row.fail("not_a_finished_x402_call")
		return nil
	}
	if created < now-ReviewEvidenceDays*86400 {
		row.fail("older_than_30_days")
		return nil
	}
	var pub struct {
		Resource string `json:"resource"`
		Status   int    `json:"http_status"`
		Bytes    int64  `json:"response_bytes"`
	}
	if !tool {
		_ = json.Unmarshal([]byte(public), &pub)
		resURL, resMethod, ok, err := s.x402ResourceAddress(ctx, tx, pub.Resource)
		if err != nil {
			return err
		}
		if !ok || !subjectMatchesURL(sub, resMethod, resURL, s.subjectOptions()) {
			row.fail("call_to_another_resource")
			return nil
		}
	}
	funded := "free"
	paid, err := callPaid(ctx, tx, account, hold)
	if err != nil {
		return err
	}
	if paid {
		funded, row.class = "paid", "paid_use"
	}
	row.state = "verified"
	row.detail = map[string]any{"day": created - created%86400, "funded": funded, "call_state": state}
	if tool {
		row.detail["tool"] = sub.Key
		return nil
	}
	status := "unknown"
	if pub.Status > 0 {
		status = strconv.Itoa(pub.Status/100) + "xx"
	}
	row.detail["resource"], row.detail["status_class"], row.detail["response_bytes"] = pub.Resource, status, pub.Bytes
	return nil
}

// callPaid reports whether a service call's hold committed paid or earned
// credit: the call was paid use, not the free allowance. The one test the
// call evidence and the rate_paid_call tip share; a reserve that was
// refunded paid nothing.
func callPaid(ctx context.Context, q allowance.Querier, account, hold string) (bool, error) {
	if hold == "" {
		return false, nil
	}
	var paid int
	err := q.QueryRowContext(ctx, "SELECT count(*) FROM ledger_entries WHERE account=? AND hold_id=? AND kind='commit' AND bucket IN ('paid','earned') AND amount>0", account, hold).Scan(&paid)
	return paid > 0, err
}

// x402ResourceAddress is a catalogued or pinned resource's URL and method.
func (s *Store) x402ResourceAddress(ctx context.Context, tx *sql.Tx, id string) (string, string, bool, error) {
	if id == "" {
		return "", "", false, nil
	}
	if cfg := s.config.X402; cfg != nil {
		for _, r := range cfg.Resources {
			if r.ID == id {
				return r.URL, r.Method, true, nil
			}
		}
	}
	var u, m string
	err := tx.QueryRowContext(ctx, "SELECT url,method FROM x402_catalogue WHERE id=? UNION ALL SELECT url,method FROM x402_vetted WHERE id=? LIMIT 1", id, id).Scan(&u, &m)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	return u, m, err == nil, err
}

func (s *Store) verifyFetch(ctx context.Context, tx *sql.Tx, reviewer string, r reviews.Review, id string, now int64, row *reviewEvidenceRow) error {
	var account, service, state, body string
	var created int64
	err := tx.QueryRowContext(ctx, "SELECT account,service,state,body,created_at FROM service_calls WHERE id=?", id).Scan(&account, &service, &state, &body, &created)
	if errors.Is(err, sql.ErrNoRows) || err == nil && account != reviewer {
		row.fail("not_your_fetch")
		return nil
	}
	if err != nil {
		return err
	}
	if service != "fetch" || state != "done" {
		row.fail("not_a_finished_fetch")
		return nil
	}
	if created < now-ReviewEvidenceDays*86400 {
		row.fail("older_than_30_days")
		return nil
	}
	var b struct {
		URL       string `json:"url"`
		RawSHA256 string `json:"raw_sha256"`
		FetchedAt int64  `json:"fetched_at"`
	}
	_ = json.Unmarshal([]byte(body), &b)
	host := ""
	if u, err := reviews.Canonical(reviews.TypeSite, b.URL, s.subjectOptions()); err == nil {
		host = u.Host()
	}
	// An outside_source review fetches the source's page, not the subject.
	under := host != "" && (host == r.Subject.Host() || r.Subject.Type == reviews.TypeDomain && reviews.RegistrableDomain(host) == r.Subject.Key)
	if r.Source != nil {
		if src, err := reviews.Canonical(reviews.TypeSite, r.Source.URL, s.subjectOptions()); err == nil {
			under = host != "" && host == src.Host()
		}
	}
	if !under {
		row.fail("fetch_of_another_site")
		return nil
	}
	row.state = "verified"
	row.detail = map[string]any{"host": host, "raw_sha256": b.RawSHA256, "fetched_at": b.FetchedAt}
	return nil
}

// verifyWork checks a work: id: public work, accepted, whose request names
// the subject, with a reward paid between two accounts before the review:
// the reviewer is the worker another account paid, or the requester who
// paid another account's worker. Work an account posts, works and pays
// itself proves nothing and fails. One case needs no payment yet: the
// work's own requester, reviewing the worker it just accepted (a rated
// accept, workreputation.go rateWork), verifies on the accept alone — the
// accept is the requester's attestation, and the reward (a held credit, an
// owed USDC transfer) may still be unpaid at that moment. Any other
// reviewer, including the worker reviewing the requester, still needs the
// reward paid.
func verifyWork(ctx context.Context, tx *sql.Tx, reviewer string, sub reviews.Subject, id string, now int64, row *reviewEvidenceRow) error {
	var requester, worker, state, visibility string
	var created int64
	err := tx.QueryRowContext(ctx, "SELECT w.requester,w.worker,w.state,w.created_at,r.visibility FROM works w JOIN events e ON e.id=w.id JOIN rooms r ON r.name=e.room WHERE w.id=?", id).Scan(&requester, &worker, &state, &created, &visibility)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (visibility != "public" || (requester != reviewer && worker != reviewer)) {
		row.fail("not_your_public_work")
		return nil
	}
	if err != nil {
		return err
	}
	if state != "accepted" || worker == "" || worker == requester || created > now {
		row.fail("work_not_accepted_between_two_accounts")
		return nil
	}
	role, other := "worker", requester
	if requester == reviewer {
		role, other = "requester", worker
	}
	requesterAttested := role == "requester" && sub.Type == reviews.TypeAgent && sub.Key == other
	var paid bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM work_rewards WHERE work_id=?1 AND state='paid' AND amount>0 AND worker=?2 AND settled_at<=?3)
 OR EXISTS(SELECT 1 FROM work_usdc WHERE work_id=?1 AND state='paid' AND worker=?2 AND settled_at<=?3)`, id, worker, now).Scan(&paid); err != nil {
		return err
	}
	if !paid && !requesterAttested {
		row.fail("work_reward_not_paid")
		return nil
	}
	// The work names its other party itself: agent:WORKER for the requester
	// (a rated accept, workreputation.go), agent:REQUESTER for the worker.
	// Any other subject is named by the request's text.
	if sub.Type != reviews.TypeAgent || sub.Key != other {
		var named int
		if err = tx.QueryRowContext(ctx, `WITH `+messageVersions+` SELECT count(*) FROM v JOIN events e ON e.id=v.id WHERE v.hidden=0 AND (instr(e.text,?2)>0 OR instr(e.text,?3)>0)`, id, sub.ID, sub.Key).Scan(&named); err != nil {
			return err
		}
		if named == 0 {
			row.fail("request_does_not_name_the_subject")
			return nil
		}
	}
	reward := "paid"
	if !paid {
		reward = "owed"
	}
	row.state, row.counterparty = "verified", other
	row.detail = map[string]any{"role": role, "work_state": state, "reward": reward}
	return nil
}

func verifyStamp(ctx context.Context, tx *sql.Tx, reviewer, hash string, row *reviewEvidenceRow) error {
	var account string
	var at, seq int64
	err := tx.QueryRowContext(ctx, "SELECT account,time,seq FROM notary_receipts WHERE hash=?", hash).Scan(&account, &at, &seq)
	if errors.Is(err, sql.ErrNoRows) || err == nil && account != reviewer {
		row.fail("not_your_stamp")
		return nil
	}
	if err != nil {
		return err
	}
	row.state = "verified"
	row.detail = map[string]any{"stamped_at": at, "receipt": seq}
	return nil
}

// SubjectRef names a subject on the wire.
type SubjectRef struct {
	ID     string         `json:"id"`
	Type   string         `json:"type"`
	Key    string         `json:"key"`
	URI    string         `json:"uri"`
	URL    string         `json:"url"`
	Parent *SubjectParent `json:"parent,omitempty"`
}

// SubjectParent is the subject a subject's page groups under (its domain):
// a SubjectRef without a parent of its own, so the type is not recursive.
type SubjectParent struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Key  string `json:"key"`
	URI  string `json:"uri"`
	URL  string `json:"url"`
}

func subjectRef(sub reviews.Subject) SubjectRef {
	ref := SubjectRef{ID: sub.ID, Type: sub.Type, Key: sub.Key, URI: sub.URI(), URL: "/s/" + sub.ID}
	if p, ok := sub.Parent(); ok && p.ID != sub.ID {
		ref.Parent = &SubjectParent{ID: p.ID, Type: p.Type, Key: p.Key, URI: p.URI(), URL: "/s/" + p.ID}
	}
	return ref
}

// SubjectInfo is a subject as subject.get shows it.
type SubjectInfo struct {
	SubjectRef
	Exists    bool   `json:"exists"`
	Listed    bool   `json:"listed"`
	State     string `json:"state,omitempty"`
	FirstSeen int64  `json:"first_seen,omitempty"`
	LastSeen  int64  `json:"last_seen,omitempty"`
	// Version is the version its reviews count for: an offering's current
	// version (offering_versions.go), 1 for every other subject.
	Version int            `json:"version,omitempty"`
	Owners  []SubjectOwner `json:"owners,omitempty"`
	// Siblings name the same endpoint under the other endpoint types (an
	// x402 subject's mpp subject and the reverse), each with its reviews'
	// summary once reviewed; subject.get fills them.
	Siblings []SubjectSibling `json:"also_reviewed_as,omitempty"`
	// OtherAddresses name an agent offering's subject at its provider's
	// other addresses (C249): its current one, and the former handles that
	// still lead to it, each once reviewed, with its reviews' summary.
	OtherAddresses []OfferingAddress `json:"other_addresses,omitempty"`
}

// OfferingAddress is an offering's subject at another of its addresses.
type OfferingAddress struct {
	SubjectSibling
	Current bool `json:"current"`
}

// SubjectSibling is a sibling subject on a subject's page: where it is,
// and its reviews' summary once it has reviews.
type SubjectSibling struct {
	SubjectParent
	Reviews *ReviewSummary `json:"reviews,omitempty"`
}

// subjectSiblings are sub's siblings with what is known of their reviews.
func subjectSiblings(ctx context.Context, tx *sql.Tx, sub reviews.Subject) ([]SubjectSibling, error) {
	sibs := sub.Siblings()
	if len(sibs) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(sibs))
	for _, s := range sibs {
		ids = append(ids, s.ID)
	}
	sums, err := reviewSummaries(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]SubjectSibling, 0, len(sibs))
	for _, s := range sibs {
		sib := SubjectSibling{SubjectParent: SubjectParent{ID: s.ID, Type: s.Type, Key: s.Key, URI: s.URI(), URL: "/s/" + s.ID}}
		if sum, ok := sums[s.ID]; ok {
			sib.Reviews = &sum
		}
		out = append(out, sib)
	}
	return out, nil
}

// ReviewEvidence is one cited evidence id and what the board found.
type ReviewEvidence struct {
	ID        string         `json:"id"`
	Class     string         `json:"class"`
	State     string         `json:"state"`
	CheckedAt int64          `json:"checked_at"`
	Detail    map[string]any `json:"detail,omitempty"`
	// Why says the detail's reason code in plain words (ReviewEvidenceReasons).
	Why string `json:"why,omitempty"`
}

// ReviewRubric is the rubric a review pinned, with where it is served.
type ReviewRubric struct {
	reviews.RubricRef
	URL string `json:"url"`
}

// MessageReview is a review's structured part, on message reads of its
// post and in every review card.
type MessageReview struct {
	Subject            SubjectRef            `json:"subject"`
	Verdict            string                `json:"verdict"`
	LabVerdict         string                `json:"lab_verdict,omitempty"`
	Dimensions         map[string]int        `json:"dimensions,omitempty"`
	Relationship       string                `json:"relationship"`
	RelationshipRaised string                `json:"relationship_raised,omitempty"`
	EvidenceClass      string                `json:"evidence_class"`
	Evidence           []ReviewEvidence      `json:"evidence,omitempty"`
	Observations       []reviews.Observation `json:"observations,omitempty"`
	Rubric             *ReviewRubric         `json:"rubric,omitempty"`
	Score              *int                  `json:"score,omitempty"`
	Band               []int                 `json:"band,omitempty"`
	Grade              string                `json:"grade,omitempty"`
	Flags              []string              `json:"flags,omitempty"`
	Record             *reviews.Record       `json:"record,omitempty"`
	Source             *reviews.Source       `json:"source,omitempty"`
	Current            bool                  `json:"current"`
	// Version is the subject's version the review is about (an offering's,
	// offering_versions.go; 1 for every other subject). EarlierVersion and
	// VersionNote mark a review of an earlier version: listed, never counted.
	Version        int    `json:"version"`
	EarlierVersion bool   `json:"earlier_version,omitempty"`
	VersionNote    string `json:"version_note,omitempty"`
	// Reward is the review reward this review earned (reviewrewards.go).
	Reward *ReviewReward `json:"reward,omitempty"`
}

// GradeShown is the grade a surface shows for the review (reviews.ShowGrade):
// a lab assessment's letter only while every dimension is measured; nothing
// for any other review, whose grade pages never show. A method, not a field:
// message reads keep their keys; review cards carry it (ReviewCard).
func (m *MessageReview) GradeShown() reviews.GradeShown {
	if m.Relationship != "lab" {
		return reviews.GradeShown{}
	}
	return reviews.ShowGrade(m.Grade, m.Dimensions)
}

// ReviewAuthor is who signed a review.
type ReviewAuthor struct {
	Account string `json:"account"`
	Key     string `json:"key"`
	Handle  string `json:"handle,omitempty"`
}

// OwnerReply is the newest reply of the subject's owner under a review,
// pinned under it (RFC 0015 §4.4).
type OwnerReply struct {
	ID        string `json:"id"`
	Account   string `json:"account"`
	Handle    string `json:"handle,omitempty"`
	Text      string `json:"text"`
	CreatedAt int64  `json:"created_at"`
}

// ReviewOrder is one review's weight in the default order and its parts
// (RFC 0015 §5.4): w = class × v × relationship × decay, capped per day.
type ReviewOrder struct {
	Class              string  `json:"class"`
	ClassWeight        float64 `json:"class_weight"`
	V                  float64 `json:"v"`
	RelationshipFactor float64 `json:"relationship_factor"`
	Decay              float64 `json:"decay"`
	BurstScale         float64 `json:"burst_scale"`
	Weight             float64 `json:"weight"`
	// OwnRoot marks a review by an account of the subject owner's own root
	// (RFC 0020 §3.3): listed, relationship factor 0, in no score.
	OwnRoot bool   `json:"own_root,omitempty"`
	Note    string `json:"note,omitempty"`
}

// ReviewCard is one review on a subject's page.
type ReviewCard struct {
	ID        string       `json:"id"`
	Root      string       `json:"root"`
	Author    ReviewAuthor `json:"author"`
	CreatedAt int64        `json:"created_at"`
	Text      string       `json:"text"`
	// DisplayFormat is how pages show Text (Message.DisplayFormat).
	DisplayFormat string `json:"display_format"`
	MessageReview
	// ShownGrade is the grade surfaces show (MessageReview.GradeShown): a
	// lab assessment's letter while every dimension is measured, else empty
	// with GradeNote "no grade (good not measured)" naming the
	// UnmeasuredDimensions. Grade stays the stored record.
	ShownGrade           string       `json:"shown_grade,omitempty"`
	GradeNote            string       `json:"grade_note,omitempty"`
	UnmeasuredDimensions []string     `json:"unmeasured_dimensions,omitempty"`
	Replies              int          `json:"replies"`
	Thread               string       `json:"thread"`
	OwnerReply           *OwnerReply  `json:"owner_reply,omitempty"`
	Order                *ReviewOrder `json:"order,omitempty"`
}

// ReviewAggregate is the community reviews' summary: counts first, then the
// Bayesian mean of verdicts the order weighs (RFC 0015 §5.4). Lab
// assessments and outside raters are never in it.
type ReviewAggregate struct {
	Reviews     int                `json:"reviews"`
	Accounts    int                `json:"accounts"`
	ByClass     map[string]int     `json:"by_evidence_class"`
	ByVerdict   map[string]int     `json:"by_verdict"`
	WeightTotal float64            `json:"weight_total"`
	VerdictMean float64            `json:"verdict_mean"`
	Dimensions  map[string]float64 `json:"dimensions,omitempty"`
	Prior       string             `json:"prior"`
	// WithEvidence counts by verdict the reviews whose class is better than
	// opinion: what a page states as a rating (schema.org AggregateRating).
	WithEvidence map[string]int `json:"by_verdict_with_evidence"`
	// ScoreShadow is RFC 0020's score (judge.ScoreGrouped: the weighted
	// median of the same reviews and weights, with judge.PriorWeight
	// pseudo-judgements at 50 each weighing their mean weight), 0-100, in
	// shadow beside verdict_mean until C304. ScoreThin is true below
	// judge.ThinBelow weighted reviews: a page labels the score thin.
	ScoreShadow int64 `json:"score_shadow"`
	ScoreThin   bool  `json:"score_thin"`
	// EarlierVersion counts the current reviews about an earlier version of
	// the subject (an offering's, offering_versions.go): listed apart, in
	// none of the above.
	EarlierVersion int `json:"earlier_version,omitempty"`
}

// storedReview is data.review as stored, read back for display.
type storedReview struct {
	Observations []reviews.Observation `json:"observations"`
	Record       *reviews.Record       `json:"record"`
	Source       *reviews.Source       `json:"source"`
}

const reviewColumns = `r.event_id,r.root_event,r.subject,r.account,r.author,r.verdict,r.relationship,r.relationship_raised,r.works,r.as_described,r.safe,r.good,r.rubric,r.rubric_version,r.rubric_sha256,r.score,r.band_lo,r.band_hi,r.grade,r.flags,r.evidence_class,r.review,r.created_at,r.current,r.version,s.version,
 s.type,s.key,e.text,coalesce((SELECT i.handle FROM identities i WHERE i.id=r.author),''),e.format`

func scanReview(rows *sql.Rows) (ReviewCard, error) {
	var c ReviewCard
	var dims [4]int
	var rubric, version, sum, flags, raw, typ, key, format string
	var score, lo, hi int
	var current, subjectVersion int
	if err := rows.Scan(&c.ID, &c.Root, &c.Subject.ID, &c.Author.Account, &c.Author.Key, &c.Verdict, &c.Relationship, &c.RelationshipRaised, &dims[0], &dims[1], &dims[2], &dims[3],
		&rubric, &version, &sum, &score, &lo, &hi, &c.Grade, &flags, &c.EvidenceClass, &raw, &c.CreatedAt, &current, &c.Version, &subjectVersion, &typ, &key, &c.Text, &c.Author.Handle, &format); err != nil {
		return c, err
	}
	c.DisplayFormat = DisplayFormatOf(format, c.Text, true) // a review is signed
	if c.Version < subjectVersion {
		c.EarlierVersion, c.VersionNote = true, EarlierVersionLabel
	}
	c.Subject = subjectRef(reviews.Subject{Type: typ, Key: key, ID: c.Subject.ID})
	c.Current = current == 1
	c.LabVerdict = reviews.LabVerdicts[c.Verdict]
	c.Dimensions = dimensionsOf(dims)
	g := c.GradeShown()
	c.ShownGrade, c.GradeNote, c.UnmeasuredDimensions = g.Grade, g.Note, g.Unmeasured
	if rubric != "" {
		c.Rubric = &ReviewRubric{RubricRef: reviews.RubricRef{ID: rubric, Version: version, SHA256: sum}, URL: "/rubrics/" + rubric + "/" + version + ".json"}
	}
	if score >= 0 {
		c.Score = &score
	}
	if lo >= 0 {
		c.Band = []int{lo, hi}
	}
	if flags != "" {
		c.Flags = strings.Split(flags, ",")
	}
	var st storedReview
	_ = json.Unmarshal([]byte(raw), &st)
	c.Observations, c.Record, c.Source = st.Observations, st.Record, st.Source
	c.Thread = "/e/" + c.Root
	return c, nil
}

// dimensionsOf is the stored dimension columns (works, as_described, safe,
// good; -1 for none) as a review's dimensions; nil when none.
func dimensionsOf(dims [4]int) map[string]int {
	var out map[string]int
	for i, d := range reviews.Dimensions {
		if dims[i] >= 0 {
			if out == nil {
				out = map[string]int{}
			}
			out[d] = dims[i]
		}
	}
	return out
}

// reviewChunk bounds the ids one IN (...) list carries.
const reviewChunk = 500

// inChunks calls fn for each run of at most reviewChunk ids, with the
// run's placeholders and arguments.
func inChunks(ids []string, fn func(marks string, args []any) error) error {
	for start := 0; start < len(ids); start += reviewChunk {
		part := ids[start:min(start+reviewChunk, len(ids))]
		args := make([]any, len(part))
		for i, id := range part {
			args[i] = id
		}
		if err := fn(placeholders(len(part)), args); err != nil {
			return err
		}
	}
	return nil
}

// queryCards reads the review cards that where (over reviews r, subjects s
// and events e) selects, in its order, with their evidence.
func queryCards(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]ReviewCard, error) {
	rows, err := tx.QueryContext(ctx, "SELECT "+reviewColumns+" FROM reviews r JOIN subjects s ON s.id=r.subject JOIN events e ON e.id=r.event_id WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	var out []ReviewCard
	for rows.Next() {
		c, err := scanReview(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	cards := make([]*MessageReview, len(out))
	for i := range out {
		cards[i] = &out[i].MessageReview
	}
	return out, loadEvidence(ctx, tx, cards, out)
}

// cardsByID reads the review cards of ids, in that order.
func cardsByID(ctx context.Context, tx *sql.Tx, ids []string) ([]ReviewCard, error) {
	byID := map[string]ReviewCard{}
	err := inChunks(ids, func(marks string, args []any) error {
		list, err := queryCards(ctx, tx, "r.event_id IN ("+marks+")", args...)
		for _, c := range list {
			byID[c.ID] = c
		}
		return err
	})
	out := make([]ReviewCard, 0, len(ids))
	for _, id := range ids {
		if c, ok := byID[id]; ok {
			out = append(out, c)
		}
	}
	return out, err
}

// loadEvidence fills each card's evidence states; cards[i] is list[i]'s.
func loadEvidence(ctx context.Context, tx *sql.Tx, cards []*MessageReview, list []ReviewCard) error {
	index := map[string]int{}
	ids := make([]string, len(list))
	for i, c := range list {
		ids[i], index[c.ID] = c.ID, i
	}
	return inChunks(ids, func(marks string, args []any) error {
		rows, err := tx.QueryContext(ctx, "SELECT event_id,evidence,class,state,checked_at,detail FROM review_evidence WHERE event_id IN ("+marks+") ORDER BY rowid", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, detail string
			var e ReviewEvidence
			if err = rows.Scan(&id, &e.ID, &e.Class, &e.State, &e.CheckedAt, &detail); err != nil {
				return err
			}
			card := cards[index[id]]
			if e.ID == RewardEvidence {
				var d rewardDetail
				if json.Unmarshal([]byte(detail), &d) == nil {
					card.Reward = d.reward(e.State, "")
				}
				continue
			}
			_ = json.Unmarshal([]byte(detail), &e.Detail)
			e.Why = ReviewEvidenceReasons[evidenceReason(e.Detail)]
			card.Evidence = append(card.Evidence, e)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		for _, id := range args {
			pendingEvidenceCard(cards[index[id.(string)]])
		}
		return nil
	})
}

// pendingEvidenceCard gives a current community review that holds no reward
// and cites a tx: payment not read on chain yet the reward its receipt
// said: pending the evidence check.
func pendingEvidenceCard(card *MessageReview) {
	if card == nil || card.Reward != nil || !card.Current || card.Relationship == "lab" || card.Relationship == "outside_source" ||
		barredRelationship(card.Relationship) || barredRelationship(card.RelationshipRaised) {
		return
	}
	var ids []string
	for _, e := range card.Evidence {
		if e.State == "unknown" && e.Detail["reason"] == TxPending {
			ids = append(ids, e.ID)
		}
	}
	if len(ids) > 0 {
		card.Reward = pendingEvidenceReward(ids)
	}
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

// attachReviews sets Review on each review post among events (message
// reads), from the index.
func (s *Store) attachReviews(ctx context.Context, tx *sql.Tx, events []Message) error {
	if !s.ReviewsEnabled() {
		return nil
	}
	var ids []string
	at := map[string]int{}
	for i, e := range events {
		if e.Room == ReviewsRoom && e.Kind == "review" && e.Type == "message" && !e.Hidden {
			ids = append(ids, e.ID)
			at[e.ID] = i
		}
	}
	list, err := cardsByID(ctx, tx, ids)
	for _, c := range list {
		mr := c.MessageReview
		events[at[c.ID]].Review = &mr
	}
	return err
}

// readSubjects serves subject.resolve, subject.get and subjects.list.
func (s *Store) readSubjects(ctx context.Context, tx *sql.Tx, c Command, now int64) (Result, error) {
	if !s.ReviewsEnabled() {
		return Result{}, reviewsUnavailable()
	}
	switch c.Operation {
	case "subject.resolve":
		sub, err := reviews.Resolve(c.Target, s.subjectOptions())
		if err != nil {
			return Result{}, reviewError(err)
		}
		if sub, err = s.boardSubject(ctx, tx, sub); err != nil {
			return Result{}, err
		}
		info, err := subjectInfo(ctx, tx, sub)
		if err != nil {
			return Result{}, err
		}
		if info.OtherAddresses, err = s.offeringAddresses(ctx, tx, sub); err != nil {
			return Result{}, err
		}
		data := map[string]any{"subject": info}
		work, err := subjectWorkRecord(ctx, tx, sub, now)
		if err != nil {
			return Result{}, err
		}
		if info.Exists || work != nil {
			sums, err := reviewSummaries(ctx, tx, []string{sub.ID})
			if err != nil {
				return Result{}, err
			}
			sum, ok := sums[sub.ID]
			if !ok && work != nil {
				sum, ok = ReviewSummary{ByVerdict: map[string]int{}, URL: SubjectReviewURL(sub, info.Exists)}, true
			}
			if ok {
				sum.WorkRecord = work
				data["reviews"] = sum
			}
		}
		return Result{Data: data}, nil
	case "subject.get":
		return s.readSubject(ctx, tx, c, now)
	}
	return s.listSubjects(ctx, tx, c)
}

func subjectInfo(ctx context.Context, tx *sql.Tx, sub reviews.Subject) (SubjectInfo, error) {
	info := SubjectInfo{SubjectRef: subjectRef(sub)}
	var listed int64
	err := tx.QueryRowContext(ctx, "SELECT state,first_seen,last_seen,listed_at,version FROM subjects WHERE id=?", sub.ID).Scan(&info.State, &info.FirstSeen, &info.LastSeen, &listed, &info.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return info, nil
	}
	if err != nil {
		return info, err
	}
	info.Exists, info.Listed = true, listed > 0
	return info, nil
}

// LookupSubject reads a subject by id from the index; ok false when no
// review named it.
func lookupSubject(ctx context.Context, tx *sql.Tx, id string) (reviews.Subject, bool, error) {
	var sub reviews.Subject
	err := tx.QueryRowContext(ctx, "SELECT id,type,key FROM subjects WHERE id=?", id).Scan(&sub.ID, &sub.Type, &sub.Key)
	if errors.Is(err, sql.ErrNoRows) {
		return sub, false, nil
	}
	return sub, err == nil, err
}

func offsetCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(cursor)
	if err != nil || n < 0 || n > reviewOffsetCursorMax || strconv.Itoa(n) != cursor {
		return 0, errCursorNextPage()
	}
	return n, nil
}

func pageLimit(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	return min(limit, max)
}

func (s *Store) readSubject(ctx context.Context, tx *sql.Tx, c Command, now int64) (Result, error) {
	var sub reviews.Subject
	if reviews.ValidID(c.Target) {
		var ok bool
		var err error
		if sub, ok, err = lookupSubject(ctx, tx, c.Target); err != nil {
			return Result{}, err
		} else if !ok {
			return Result{}, problem(404, "subject_not_found", "No review names this subject yet. Read a subject by its TYPE:KEY to see it before its first review; write_review (or a signed post in room reviews) writes the first.")
		}
	} else {
		var err error
		if sub, err = reviews.Resolve(c.Target, s.subjectOptions()); err != nil {
			return Result{}, reviewError(err)
		}
		if sub, err = s.boardSubject(ctx, tx, sub); err != nil {
			return Result{}, err
		}
	}
	offset, err := offsetCursor(c.Cursor)
	if err != nil {
		return Result{}, err
	}
	limit := pageLimit(c.Limit, ReviewPageDefault, ReviewPageMax)
	info, err := subjectInfo(ctx, tx, sub)
	if err != nil {
		return Result{}, err
	}
	if info.Owners, err = s.subjectOwners(ctx, tx, sub); err != nil {
		return Result{}, err
	}
	if info.Siblings, err = subjectSiblings(ctx, tx, sub); err != nil {
		return Result{}, err
	}
	if info.OtherAddresses, err = s.offeringAddresses(ctx, tx, sub); err != nil {
		return Result{}, err
	}
	data := map[string]any{"subject": info, "text_is_untrusted": true, "order": reviewOrderNote()}
	hint, err := s.reviewRewardHint(ctx, tx, sub.ID, now)
	if err != nil {
		return Result{}, err
	}
	if hint != nil {
		data["review_reward"] = hint
	}
	work, err := subjectWorkRecord(ctx, tx, sub, now)
	if err != nil {
		return Result{}, err
	}
	if work != nil {
		data["work_record"] = work
	}
	if sub.Type == reviews.TypeAgent {
		if data["reputation"], err = s.reputation(ctx, tx, sub.Key, now); err != nil {
			return Result{}, err
		}
	}
	if !info.Exists {
		data["reviews"], data["aggregate"], data["has_more"] = []ReviewCard{}, emptyAggregate(), false
		return Result{Data: data}, nil
	}
	agg, page, err := s.communityReviews(ctx, tx, sub, now, limit, offset)
	if err != nil {
		return Result{}, err
	}
	owners := make([]string, 0, len(info.Owners))
	for _, o := range info.Owners {
		owners = append(owners, o.Account)
	}
	finish := func(list []ReviewCard) ([]ReviewCard, error) {
		for i := range list {
			if err := reviewThread(ctx, tx, &list[i], owners); err != nil {
				return nil, err
			}
		}
		return list, nil
	}
	if page, err = finish(page); err != nil {
		return Result{}, err
	}
	end := offset + len(page)
	data["reviews"], data["aggregate"], data["has_more"] = page, agg, end < agg.Reviews
	if end < agg.Reviews {
		data["next_cursor"] = strconv.Itoa(end)
	}
	// The lab assessment is the newest lab review whose record recomputed
	// to what it claims; lab_runs are the newest lab reviews, each
	// self-reported until its record verifies.
	labs, err := queryCards(ctx, tx, "r.subject=? AND r.current=1 AND e.hidden=0 AND r.relationship='lab' AND "+aboutCurrentVersion+" AND "+labVerified+" ORDER BY r.created_at DESC, r.event_id LIMIT 1", sub.ID)
	if err != nil {
		return Result{}, err
	}
	if labs, err = finish(labs); err != nil {
		return Result{}, err
	}
	if len(labs) > 0 {
		data["lab"] = labs[0]
	}
	// Community reviews of an earlier version of an offering: listed,
	// newest first, labelled, never in the aggregate or its order.
	if agg.EarlierVersion > 0 {
		earlier, err := queryCards(ctx, tx, "r.subject=? AND r.current=1 AND e.hidden=0 AND r.relationship NOT IN ('lab','outside_source') AND NOT "+aboutCurrentVersion+" ORDER BY r.created_at DESC, r.event_id LIMIT ?", sub.ID, ReviewPageMax)
		if err != nil {
			return Result{}, err
		}
		if data["earlier_version_reviews"], err = finish(earlier); err != nil {
			return Result{}, err
		}
	}
	// The lab runs and other raters, newest first, in one pass: each
	// account (a listed lab or importer key) has one current review here.
	rows, err := tx.QueryContext(ctx, `SELECT r.event_id,r.relationship FROM reviews r JOIN events e ON e.id=r.event_id
 WHERE r.subject=? AND r.current=1 AND e.hidden=0 AND r.relationship IN ('lab','outside_source') ORDER BY r.created_at DESC, r.event_id`, sub.ID)
	if err != nil {
		return Result{}, err
	}
	var runIDs, otherIDs []string
	for rows.Next() && (len(runIDs) < labRunsShown || len(otherIDs) < otherRatersShown) {
		var id, rel string
		if err = rows.Scan(&id, &rel); err != nil {
			rows.Close()
			return Result{}, err
		}
		if rel == "lab" && len(runIDs) < labRunsShown {
			runIDs = append(runIDs, id)
		} else if rel == "outside_source" && len(otherIDs) < otherRatersShown {
			otherIDs = append(otherIDs, id)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return Result{}, err
	}
	for _, apart := range []struct {
		name string
		ids  []string
	}{{"lab_runs", runIDs}, {"other_raters", otherIDs}} {
		if len(apart.ids) == 0 {
			continue
		}
		list, err := cardsByID(ctx, tx, apart.ids)
		if err != nil {
			return Result{}, err
		}
		if data[apart.name], err = finish(list); err != nil {
			return Result{}, err
		}
	}
	return Result{Data: data}, nil
}

func emptyAggregate() ReviewAggregate {
	return ReviewAggregate{ByClass: map[string]int{}, ByVerdict: map[string]int{}, WithEvidence: map[string]int{}, VerdictMean: ReviewPriorValue, ScoreShadow: judge.Score(nil), ScoreThin: judge.Thin(0), Prior: priorNote()}
}

func priorNote() string {
	return fmt.Sprintf("%g pseudo-reviews at %g", ReviewPriorWeight, ReviewPriorValue)
}

func reviewOrderNote() map[string]any {
	return map[string]any{"version": ReviewOrderVersion, "weight": "evidence class × v(standing) (1 for a reviewer with a visible public post a day old when it reviewed) × relationship × decay, capped per subject per UTC day",
		"evidence_weights": ReviewEvidenceWeights, "relationship_factors": ReviewRelationshipFactors, "v": "1 for an account with a public post a day old when it reviewed and not in a burst cluster, else 0 (today's rule until the standing cutover)",
		"half_life_days": ReviewHalfLifeDays, "burst_cap": ReviewBurstCap, "burst_days": ReviewBurstDays, "verdict_values": map[string]any{"good": 1, "mixed": 0.5, "bad": 0, "unusable": 0, "unverified": "not counted"},
		"own_root": "a review by an account of the subject owner's own root (RFC 0020 §3.3) is listed with own_root and relationship factor 0, so it is in no score",
		"separate": "lab assessments and other raters are shown apart and never in this order or its aggregate"}
}

// sqlNumber writes f as a SQL real literal.
func sqlNumber(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".e") {
		s += ".0"
	}
	return s
}

// sqlCase is a SQL CASE mapping expr's values to m's numbers, else def. The
// keys are this file's own constant words, never input.
func sqlCase(expr string, m map[string]float64, def string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("CASE " + expr)
	for _, k := range keys {
		b.WriteString(" WHEN '" + k + "' THEN " + sqlNumber(m[k]))
	}
	b.WriteString(" ELSE " + def + " END")
	return b.String()
}

// communityWeights is the default order (RFC 0015 §5.4) in SQL, one source
// for the page and its aggregate: each current, visible community review of
// subject ?2 at time ?1 with its parts (class weight cw, standing v,
// relationship factor rf, decay, burst scale) and its weight w = cw × v ×
// rf × decay × burst, rounded to 1e-6. The accounts in the JSON array ?3
// (ownRootReviewers) are of the subject owner's own root: their reviews
// are listed with rf 0 and own 1, so every score over the subject (the
// aggregate's, Find's, the reputation's) leaves them out alike. Reviews younger than ReviewBurstDays
// add at most ReviewBurstCap per UTC day: their day totals come from an
// index range over those days only, so a read is one pass over the
// subject's reviews. A review's class is its best verified evidence not
// counted for another review (indexReview).
var communityWeights = sync.OnceValue(func() string {
	verdicts := map[string]float64{}
	for _, v := range reviews.Verdicts {
		if x, ok := reviews.VerdictValue(v); ok {
			verdicts[v] = x
		}
	}
	cw := sqlCase("r.evidence_class", ReviewEvidenceWeights, "0.0")
	// A review by an account of the owner's own root (?3, ownRootReviewers)
	// is no judgement: listed, its relationship factor 0 (RFC 0020 §3.3).
	own := "r.account IN (SELECT value FROM json_each(?3))"
	rf := "CASE WHEN " + own + " THEN 0.0 ELSE " + sqlCase("CASE WHEN r.relationship_raised<>'' THEN r.relationship_raised ELSE r.relationship END", ReviewRelationshipFactors, "1.0") + " END"
	decay := "pow(2.0, -(?1-r.created_at)/" + sqlNumber(ReviewHalfLifeDays*86400) + ")"
	burst, capped := strconv.Itoa(ReviewBurstDays*86400), sqlNumber(ReviewBurstCap)
	from := ` FROM reviews r JOIN events e ON e.id=r.event_id%s WHERE r.subject=?2 AND r.current=1 AND e.hidden=0 AND r.relationship NOT IN ('lab','outside_source') AND ` + aboutCurrentVersion
	// v is v(the reviewer's standing) (RFC 0021 §12, standingVSQL), a whole
	// vote at least for a review by a seasoned account (the vote floor).
	v := "max(r.seasoned," + standingVSQL("r.account") + ")"
	return `WITH days AS MATERIALIZED (SELECT r.created_at/86400 AS day, sum(` + cw + `*` + v + `*` + rf + `*` + decay + `) AS total` + fmt.Sprintf(from, "") + ` AND r.created_at>?1-` + burst + ` GROUP BY day),
parts AS (SELECT r.event_id,r.account,r.verdict,r.evidence_class,r.works,r.as_described,r.safe,r.good,r.score,r.created_at,
 ` + cw + ` AS cw, ` + v + ` AS v, ` + rf + ` AS rf, ` + own + ` AS own, ` + decay + ` AS decay, ` + sqlCase("r.verdict", verdicts, "NULL") + ` AS value,
 CASE WHEN ?1-r.created_at<` + burst + ` AND days.total>` + capped + ` THEN ` + capped + `/days.total ELSE 1.0 END AS burst` + fmt.Sprintf(from, " LEFT JOIN days ON days.day=r.created_at/86400") + `),
w AS (SELECT parts.*, round(cw*v*rf*decay*burst, 6) AS w FROM parts)
`
})

// communityReviews reads a subject's community reviews in one statement,
// one pass over them: the aggregate (RFC 0015 §5.4: counts by evidence class
// and verdict, the weights, the Bayesian mean of verdicts and the weighted
// dimension means) and one page in the default order (weight, then newest),
// each card with its order parts. Each review's weight is computed once
// (x is materialized: a flattened CTE would recompute it per use).
func (s *Store) communityReviews(ctx context.Context, tx *sql.Tx, sub reviews.Subject, now int64, limit, offset int) (ReviewAggregate, []ReviewCard, error) {
	agg := emptyAggregate()
	subject := sub.ID
	own, err := s.ownRootReviewers(ctx, tx, sub)
	if err != nil {
		return agg, nil, err
	}
	// One current review per account and subject: accounts are reviews.
	cols := []string{"count(*)", "count(*)",
		"coalesce(sum(CASE WHEN w>0 AND value IS NOT NULL THEN w END),0)", "coalesce(sum(CASE WHEN w>0 THEN w*value END),0)"}
	for _, class := range reviewClassRank {
		cols = append(cols, "coalesce(sum(evidence_class='"+class+"'),0)")
	}
	for _, v := range reviews.Verdicts {
		cols = append(cols, "coalesce(sum(verdict='"+v+"'),0)")
	}
	for _, v := range reviews.Verdicts {
		cols = append(cols, "coalesce(sum(verdict='"+v+"' AND evidence_class<>'opinion'),0)")
	}
	for _, d := range reviews.Dimensions {
		cols = append(cols, "coalesce(sum(CASE WHEN w>0 AND "+d+">=0 THEN w*"+d+" END),0)", "coalesce(sum(CASE WHEN w>0 AND "+d+">=0 THEN w END),0)")
	}
	cols = append(cols, reviewGroupsSQL(""))
	rows, err := tx.QueryContext(ctx, communityWeights()+`, x AS MATERIALIZED (SELECT * FROM w),
agg AS (SELECT `+strings.Join(cols, ",")+` FROM x),
page AS (SELECT event_id,created_at,evidence_class,cw,v,rf,own,decay,burst,w FROM x ORDER BY w DESC, created_at DESC, event_id LIMIT ?4 OFFSET ?5)
SELECT agg.*,page.event_id,page.evidence_class,page.cw,page.v,page.rf,page.own,page.decay,page.burst,page.w FROM agg LEFT JOIN page ON 1 ORDER BY page.w DESC, page.created_at DESC, page.event_id`, now, subject, own, limit, offset)
	if err != nil {
		return agg, nil, err
	}
	var total, sum float64
	var groups sql.NullString
	byClass := make([]int, len(reviewClassRank))
	byVerdict := make([]int, len(reviews.Verdicts))
	withEvidence := make([]int, len(reviews.Verdicts))
	dims := make([]float64, 2*len(reviews.Dimensions))
	var ids []string
	orders := map[string]*ReviewOrder{}
	for rows.Next() {
		dest := []any{&agg.Reviews, &agg.Accounts, &total, &sum}
		for i := range byClass {
			dest = append(dest, &byClass[i])
		}
		for i := range byVerdict {
			dest = append(dest, &byVerdict[i])
		}
		for i := range withEvidence {
			dest = append(dest, &withEvidence[i])
		}
		for i := range dims {
			dest = append(dest, &dims[i])
		}
		dest = append(dest, &groups)
		var id, class sql.NullString
		var cw, v, rf, decay, burst, w sql.NullFloat64
		var ownRoot sql.NullBool
		if err = rows.Scan(append(dest, &id, &class, &cw, &v, &rf, &ownRoot, &decay, &burst, &w)...); err != nil {
			rows.Close()
			return agg, nil, err
		}
		if id.Valid {
			ids = append(ids, id.String)
			orders[id.String] = &ReviewOrder{Class: class.String, ClassWeight: cw.Float64, V: v.Float64, RelationshipFactor: rf.Float64, Decay: decay.Float64, BurstScale: burst.Float64, Weight: w.Float64, OwnRoot: ownRoot.Bool}
			if ownRoot.Bool {
				orders[id.String].Note = OwnRootNote
			}
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return agg, nil, err
	}
	for i, class := range reviewClassRank {
		if byClass[i] > 0 {
			agg.ByClass[class] = byClass[i]
		}
	}
	for i, v := range reviews.Verdicts {
		if byVerdict[i] > 0 {
			agg.ByVerdict[v] = byVerdict[i]
		}
		if withEvidence[i] > 0 {
			agg.WithEvidence[v] = withEvidence[i]
		}
	}
	agg.VerdictMean = math.Round((sum+ReviewPriorWeight*ReviewPriorValue)/(total+ReviewPriorWeight)*1e4) / 1e4
	agg.WeightTotal = math.Round(total*1e6) / 1e6
	agg.ScoreShadow, agg.ScoreThin = reviewScoreShadow(groups.String)
	for i, d := range reviews.Dimensions {
		if dims[2*i+1] > 0 {
			if agg.Dimensions == nil {
				agg.Dimensions = map[string]float64{}
			}
			agg.Dimensions[d] = math.Round(dims[2*i]/dims[2*i+1]*10) / 10
		}
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM reviews r JOIN events e ON e.id=r.event_id WHERE r.subject=? AND r.current=1 AND e.hidden=0
 AND r.relationship NOT IN ('lab','outside_source') AND NOT `+aboutCurrentVersion, subject).Scan(&agg.EarlierVersion); err != nil {
		return agg, nil, err
	}
	page, err := cardsByID(ctx, tx, ids)
	if err != nil {
		return agg, nil, err
	}
	for i := range page {
		page[i].Order = orders[page[i].ID]
		for _, e := range page[i].Evidence {
			if e.Detail["counted_for"] != nil && !page[i].Order.OwnRoot {
				page[i].Order.Note = "evidence already counted for another review"
			}
		}
	}
	return agg, page, nil
}

// reviewGroupsSQL is the weight and count per distinct (verdict, score,
// good) of the reviews in x (communityWeights' rows, materialized) that have
// weight, as one JSON array, for judge.ScoreGrouped (reviewPoints): the
// mapping to x is judge.FromReview's, never repeated in SQL. filter narrows
// the reviews (" AND ...", over x's columns); "" for every one.
func reviewGroupsSQL(filter string) string {
	return "(SELECT json_group_array(json_array(verdict,score,good,sw,n)) FROM (SELECT verdict,score,good,sum(w) AS sw,count(*) AS n FROM x WHERE w>0" + filter + " GROUP BY verdict,score,good))"
}

// subjectJudgements is a subject's community reviews as judge points (its
// weight groups, reviewPoints) and the weighted judgements they stand for,
// from the one weighting statement (communityWeights) with the one
// exclusion (ownRootReviewers): its score is the aggregate's score_shadow.
func (s *Store) subjectJudgements(ctx context.Context, tx *sql.Tx, sub reviews.Subject, now int64) ([]judge.Point, int64, error) {
	own, err := s.ownRootReviewers(ctx, tx, sub)
	if err != nil {
		return nil, 0, err
	}
	var groups sql.NullString
	if err = tx.QueryRowContext(ctx, communityWeights()+", x AS MATERIALIZED (SELECT * FROM w) SELECT "+reviewGroupsSQL(""), now, sub.ID, own).Scan(&groups); err != nil {
		return nil, 0, err
	}
	points, n := reviewPoints(groups.String)
	return points, n, nil
}

// OwnRootNote labels a review by an account of the subject owner's own
// root in the order: listed, weighing nothing (RFC 0020 §3.3).
const OwnRootNote = "by the owner's own root: shown, weighs 0"

// ownRootReviewers is, as a JSON array for communityWeights' ?3, the
// accounts among sub's current community reviewers whose review is no
// judgement (judge.OwnSubject, RFC 0020 §3.3): an owner (subjectOwners;
// an agent subject's agent always) or an account of an owner's root in
// the current trust run. The trust run reads the same rule over its own
// owner records (subjectOwners too).
func (s *Store) ownRootReviewers(ctx context.Context, tx *sql.Tx, sub reviews.Subject) (string, error) {
	listed, err := s.subjectOwners(ctx, tx, sub)
	if err != nil {
		return "", err
	}
	owners := make([]string, 0, len(listed)+1)
	if sub.Type == reviews.TypeAgent {
		owners = append(owners, sub.Key)
	}
	for _, o := range listed {
		owners = append(owners, o.Account)
	}
	if len(owners) == 0 {
		return "[]", nil
	}
	var reviewers []string
	if err = scanAll(ctx, tx, "SELECT DISTINCT account FROM reviews WHERE subject=? AND current=1 AND relationship NOT IN ('lab','outside_source')", []any{sub.ID}, func(r *sql.Rows) error {
		var a string
		err := r.Scan(&a)
		reviewers = append(reviewers, a)
		return err
	}); err != nil {
		return "", err
	}
	out := []string{}
	if len(reviewers) > 0 {
		scores, err := trust.CurrentScores(ctx, tx, append(slices.Clone(owners), reviewers...))
		if err != nil {
			return "", err
		}
		rootOf := func(a string) string {
			if sc, ok := scores[a]; ok && sc.Root != "" {
				return sc.Root
			}
			return a
		}
		for _, a := range reviewers {
			if judge.OwnSubject(a, slices.Values(owners), rootOf) {
				out = append(out, a)
			}
		}
	}
	b, err := json.Marshal(out)
	return string(b), err
}

// reviewScoreShadow is judge.ScoreGrouped over the aggregate's weight groups
// (reviewPoints). thin is judge.Thin of the weighted judgements counted
// (RFC 0020 §2).
func reviewScoreShadow(groups string) (score int64, thin bool) {
	points, n := reviewPoints(groups)
	return judge.ScoreGrouped(points, n), judge.Thin(n)
}

// reviewPoints reads weight groups ([verdict, score, good, weight, count]
// rows, reviewGroupsSQL) as judge points: each group's x is
// judge.FromReview's and its weight the summed w in ppm; n is how many
// weighted judgements they stand for.
func reviewPoints(groups string) (points []judge.Point, n int64) {
	var rows [][5]json.RawMessage
	_ = json.Unmarshal([]byte(groups), &rows)
	for _, g := range rows {
		var verdict string
		var score, good, count int64
		var w float64
		if json.Unmarshal(g[0], &verdict) != nil || json.Unmarshal(g[1], &score) != nil || json.Unmarshal(g[2], &good) != nil || json.Unmarshal(g[3], &w) != nil ||
			json.Unmarshal(g[4], &count) != nil {
			continue
		}
		if x, ok := judge.FromReview(verdict, score, good); ok && w > 0 {
			points = append(points, judge.Point{X: x, W: int64(math.Round(w * 1e6))})
			n += max(1, count)
		}
	}
	return points, n
}

// reviewThread counts a review's replies and finds its owner's pinned reply.
func reviewThread(ctx context.Context, tx *sql.Tx, card *ReviewCard, owners []string) error {
	// Every lookup goes through an index (the versions by events_origin,
	// replies by events_reply): a card costs its own thread, never a scan.
	if err := tx.QueryRowContext(ctx, `WITH RECURSIVE `+messageVersions+`,
 t(id) AS (SELECT id FROM v UNION SELECT e.id FROM t CROSS JOIN events e ON e.reply_to=t.id WHERE e.room=?2 LIMIT ?3)
 SELECT count(*) FROM t CROSS JOIN events e ON e.id=t.id WHERE e.reply_to<>'' AND e.hidden=0 AND e.supersedes=''`, card.Root, ReviewsRoom, reviewThreadCountMax).Scan(&card.Replies); err != nil {
		return err
	}
	if len(owners) == 0 {
		return nil
	}
	args := []any{card.Root}
	for _, o := range owners {
		args = append(args, o)
	}
	var reply OwnerReply
	err := tx.QueryRowContext(ctx, `WITH `+messageVersions+` SELECT e.id,e.account,coalesce((SELECT i.handle FROM identities i WHERE i.id=e.author),''),
 coalesce((SELECT x.text FROM events x WHERE x.origin=e.id AND x.origin<>'' AND x.hidden=0 ORDER BY x.seq DESC LIMIT 1),e.text),e.created_at
 FROM v CROSS JOIN events e ON e.reply_to=v.id WHERE e.hidden=0 AND e.supersedes='' AND +e.account IN (`+placeholders(len(owners))+`) ORDER BY e.seq DESC LIMIT 1`, args...).Scan(&reply.ID, &reply.Account, &reply.Handle, &reply.Text, &reply.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	card.OwnerReply = &reply
	return nil
}

// messageVersions is the CTE v(id, hidden): message ?1 and its later
// versions, each found through an index.
const messageVersions = `v(id,hidden) AS (SELECT id,hidden FROM events WHERE id=?1 UNION ALL SELECT id,hidden FROM events WHERE origin=?1 AND origin<>'')`

// SubjectListing is one row of subjects.list.
type SubjectListing struct {
	SubjectRef
	Reviews      int    `json:"reviews"`
	Accounts     int    `json:"accounts"`
	LastReviewAt int64  `json:"last_review_at"`
	Lab          string `json:"lab_verdict,omitempty"`
}

// subjectsListData is subjects.list's data.
type subjectsListData struct {
	Schema int    `json:"schema"`
	Sort   string `json:"sort"`
}

// parseSubjectsListData reads subjects.list's data strictly: since C377 an
// unknown field is refused like a bad sort, as every other operation's data
// refuses one (no first-party client sent any).
func parseSubjectsListData(raw string) (subjectsListData, error) {
	var d subjectsListData
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	err := dec.Decode(&d)
	if err == nil {
		if _, end := dec.Token(); !errors.Is(end, io.EOF) {
			err = errors.New("trailing data")
		}
	}
	if err != nil || d.Schema != 1 || (d.Sort != "reviewed" && d.Sort != "recent") {
		return d, problem(400, "invalid_request", `subjects.list data is {"schema":1,"sort":"reviewed"|"recent"}.`)
	}
	return d, nil
}

func (s *Store) listSubjects(ctx context.Context, tx *sql.Tx, c Command) (Result, error) {
	if c.Kind != "" && !slices.Contains(reviews.Types, c.Kind) {
		return Result{}, problem(400, "invalid_kind", "kind is a subject type: "+strings.Join(reviews.Types, ", ")+".")
	}
	if len(c.Query) > SubjectQueryMaxBytes || !utf8.ValidString(c.Query) {
		return Result{}, problem(400, "invalid_query", fmt.Sprintf("query is at most %d bytes of text.", SubjectQueryMaxBytes))
	}
	sortBy := "reviewed"
	if c.Data != "" {
		d, err := parseSubjectsListData(c.Data)
		if err != nil {
			return Result{}, err
		}
		sortBy = d.Sort
	}
	offset, err := offsetCursor(c.Cursor)
	if err != nil {
		return Result{}, err
	}
	limit := pageLimit(c.Limit, SubjectsPageDefault, SubjectsPageMax)
	order := "n DESC, last DESC, s.id"
	if sortBy == "recent" {
		order = "last DESC, s.id"
	}
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(c.Query)) + "%"
	rows, err := tx.QueryContext(ctx, `SELECT s.id,s.type,s.key,count(c.event_id) AS n,count(DISTINCT c.account),coalesce(max(c.created_at),0) AS last,
 coalesce((SELECT r.verdict FROM reviews r JOIN events le ON le.id=r.event_id WHERE r.subject=s.id AND r.current=1 AND r.version=s.version AND r.relationship='lab' AND le.hidden=0 AND `+labVerified+` ORDER BY r.created_at DESC LIMIT 1),'')
 FROM subjects s LEFT JOIN reviews c ON c.subject=s.id AND c.current=1 AND c.version=s.version AND c.relationship NOT IN ('lab','outside_source') AND EXISTS(SELECT 1 FROM events e WHERE e.id=c.event_id AND e.hidden=0)
 WHERE s.listed_at>0 AND (?='' OR s.type=?) AND (?='' OR lower(s.key) LIKE ? ESCAPE '\')
 GROUP BY s.id ORDER BY `+order+` LIMIT ? OFFSET ?`, c.Kind, c.Kind, c.Query, like, limit+1, offset)
	if err != nil {
		return Result{}, err
	}
	defer rows.Close()
	list := []SubjectListing{}
	for rows.Next() {
		var l SubjectListing
		var sub reviews.Subject
		if err = rows.Scan(&sub.ID, &sub.Type, &sub.Key, &l.Reviews, &l.Accounts, &l.LastReviewAt, &l.Lab); err != nil {
			return Result{}, err
		}
		l.SubjectRef = subjectRef(sub)
		list = append(list, l)
	}
	if err = rows.Err(); err != nil {
		return Result{}, err
	}
	data := map[string]any{"subjects": list, "has_more": false, "sort": sortBy}
	if len(list) > limit {
		data["subjects"], data["has_more"], data["next_cursor"] = list[:limit], true, strconv.Itoa(offset+limit)
	}
	return Result{Data: data}, nil
}

// ReviewSummary is the compact reviews summary an x402 resource and
// subject.resolve carry.
type ReviewSummary struct {
	Reviews   int            `json:"reviews"`
	Accounts  int            `json:"accounts"`
	ByVerdict map[string]int `json:"by_verdict"`
	Lab       map[string]any `json:"lab,omitempty"`
	URL       string         `json:"url"`
	// EarlierVersion counts the current reviews about an earlier version
	// of an offering, left out of the counts above.
	EarlierVersion int `json:"earlier_version,omitempty"`
	// WorkRecord is an agent subject's record on paid public work
	// (workreputation.go), where the summary travels with one subject:
	// agent.get and subject.resolve.
	WorkRecord *WorkRecord `json:"work_record,omitempty"`
}

// reviewSummaries summarises, per subject id, the current community
// reviews (counted in SQL) and the newest lab assessment whose record
// recomputed; ids with neither are left out. Ids go in chunks, so any
// number is read.
func reviewSummaries(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, ids []string) (map[string]ReviewSummary, error) {
	out := map[string]ReviewSummary{}
	cur := "r.version=sv.version"
	cols := []string{"r.subject", "coalesce(sum(" + cur + "),0)", "count(DISTINCT CASE WHEN " + cur + " THEN r.account END)"}
	for _, v := range reviews.Verdicts {
		cols = append(cols, "coalesce(sum(r.verdict='"+v+"' AND "+cur+"),0)")
	}
	cols = append(cols, "coalesce(sum(r.version<sv.version),0)")
	visible := " FROM reviews r JOIN events e ON e.id=r.event_id JOIN subjects sv ON sv.id=r.subject WHERE r.subject IN (%s) AND r.current=1 AND e.hidden=0 AND "
	err := inChunks(ids, func(marks string, args []any) error {
		rows, err := q.QueryContext(ctx, "SELECT "+strings.Join(cols, ",")+fmt.Sprintf(visible, marks)+"r.relationship NOT IN ('lab','outside_source') GROUP BY r.subject", args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var subject string
			sum := ReviewSummary{ByVerdict: map[string]int{}}
			counts := make([]int, len(reviews.Verdicts))
			dest := []any{&subject, &sum.Reviews, &sum.Accounts}
			for i := range counts {
				dest = append(dest, &counts[i])
			}
			dest = append(dest, &sum.EarlierVersion)
			if err = rows.Scan(dest...); err != nil {
				rows.Close()
				return err
			}
			for i, v := range reviews.Verdicts {
				if counts[i] > 0 {
					sum.ByVerdict[v] = counts[i]
				}
			}
			sum.URL = "/s/" + subject
			out[subject] = sum
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		rows, err = q.QueryContext(ctx, "SELECT r.subject,r.verdict,r.score,r.grade,r.works,r.as_described,r.safe,r.good"+fmt.Sprintf(visible, marks)+"r.relationship='lab' AND "+cur+" AND "+labVerified+" ORDER BY r.created_at, r.event_id", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var subject, verdict, grade string
			var score int
			var dims [4]int
			if err = rows.Scan(&subject, &verdict, &score, &grade, &dims[0], &dims[1], &dims[2], &dims[3]); err != nil {
				return err
			}
			sum, ok := out[subject]
			if !ok {
				sum = ReviewSummary{ByVerdict: map[string]int{}, URL: "/s/" + subject}
			}
			sum.Lab = map[string]any{"verdict": verdict, "lab_verdict": reviews.LabVerdicts[verdict]}
			if score >= 0 {
				sum.Lab["score"] = score
			}
			if g := reviews.ShowGrade(grade, dimensionsOf(dims)); g.Grade != "" {
				sum.Lab["grade"] = g.Grade
			} else if g.Note != "" {
				sum.Lab["grade_note"], sum.Lab["unmeasured_dimensions"] = g.Note, g.Unmeasured
			}
			out[subject] = sum // the newest comes last
		}
		return rows.Err()
	})
	return out, err
}

// reviewSummarizer is reviewSummaries for the x402 relay's resources read
// (services.ReviewSummarizer, handed over while REVIEWS is on), on the
// read's own querier, with the board's subject options.
type reviewSummarizer struct{ opt reviews.Options }

// reviewSummarizer is the x402 relay's reviews index while REVIEWS is on.
func (s *Store) reviewSummarizer() services.ReviewSummarizer {
	if !s.ReviewsEnabled() {
		return nil
	}
	return reviewSummarizer{opt: s.subjectOptions()}
}

// SubjectOptions are the options the write path resolves subjects with.
func (r reviewSummarizer) SubjectOptions() reviews.Options { return r.opt }

func (reviewSummarizer) ReviewSummaries(ctx context.Context, q allowance.Querier, ids []string) (map[string]any, error) {
	sums, err := reviewSummaries(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(sums))
	for id, s := range sums {
		out[id] = s
	}
	return out, nil
}

// SubjectsCapabilities is /capabilities subjects; nil while REVIEWS is off.
func (s *Store) SubjectsCapabilities() map[string]any {
	if !s.ReviewsEnabled() {
		return nil
	}
	return map[string]any{
		"types": reviews.Types, "deferred": []string{"place"},
		"uri":     "TYPE:KEY: x402:GET https://host/path, domain:example.com, site:https://example.com/, mcp:https://host/path or mcp:io.github.owner/server, agent:ACCOUNT_FINGERPRINT, pkg:npm/%40scope/name (a purl without its version), tool:swarmmemo:SERVICE.METHOD (one of this board's own tools, its id in tools search)",
		"id":      `base32(SHA-256(canonical JSON ["swarmmemo.subject.v1",TYPE,KEY]))[:26], lowercase`,
		"resolve": "/api/subjects/resolve?uri=URI", "get": "/api/subjects/ID", "list": "/api/subjects?type=TYPE&q=TEXT&sort=reviewed|recent", "page": "/s/ID", "search": "/s?q=TEXT",
		"mcp":       []string{"resolve_subject", "read_subject", "search_subjects"},
		"listed":    "in subjects.list and search engines once reviewed by two accounts or by its verified owner; before that its page is noindex",
		"refused":   "people: email addresses, phone numbers, profile-shaped URL paths (a first segment in, u, user, users, profile, people, member or starting with @ or ~; any path on x.com, twitter.com, facebook.com, instagram.com, tiktok.com, linkedin.com and threads.net; one segment, sponsors/ or orgs/ on github.com and gitlab.com; c/ and channel/ on youtube.com), this board's own account pages, IP addresses and special-use names (subject_refused, 422)",
		"offerings": "an agent offering is the subject x402:POST https://" + s.subjectOptions().OwnHosts[0] + "/@HANDLE/NAME, its page's address (the one exception to the @ rule; a fingerprint address resolves to it); offering.get carries its reviews",
		"tools":     "each of SwarmMemo's own metered tools is the subject tool:swarmmemo:SERVICE.METHOD, owned by the operator (the trust parameters' service_accounts): reviewed with call evidence like any paid API, and its score shown in Find",
		"agents":    "an agent is agent:ACCOUNT; agent.get and /agent/FP.json carry its reviews summary, with its work_record (paid accepted work; work_coordination.work_record)",
		"ownership": "derived from identity links: a verified domain link owns its domain and the URLs on it; an agent owns its own agent subject; a provider owns its offerings; a listed paid API's submitter owns it; the operator's service accounts own the tool subjects",
		"badge":     map[string]any{"subject": "/s/ID/badge.svg", "agent": "/agent/FINGERPRINT/badge.svg", "shows": "the verified lab grade, score and verdict, else the review count and the aggregate's word", "cache_seconds": 300},
		"embed":     "/embed/v1.js with data-subject=ID: the subject's reviews, read-only, on any site (/embed#reviews)",
		"sitemap":   "listed subjects' pages, with the newest review's time as lastmod; listed pages carry schema.org AggregateRating from evidence-backed reviews only",
	}
}

// ReviewsCapabilities is /capabilities reviews; nil while REVIEWS is off.
func (s *Store) ReviewsCapabilities() map[string]any {
	if !s.ReviewsEnabled() {
		return nil
	}
	rubrics := []map[string]any{}
	for _, r := range reviews.Rubrics() {
		rubrics = append(rubrics, map[string]any{"id": r.ID, "version": r.Version, "sha256": r.SHA256, "subject_types": r.SubjectTypes, "url": r.Path(), "page": "/rubrics/" + r.ID, "title": r.Doc.Title})
	}
	return map[string]any{
		"room": ReviewsRoom, "kind": "review", "page": "the subject's id", "docs": "/protocol.md#subjects-and-reviews",
		"data":           `{"schema":1,"review":{"subject":"TYPE:KEY","verdict":"good"}} with optional dimensions, relationship, evidence, observations, rubric, record; score, band, grade, flags for relationship lab; source for outside_source`,
		"data_max_bytes": maxPostData, "verdicts": reviews.Verdicts, "lab_verdicts": reviews.LabVerdicts, "dimensions": reviews.Dimensions, "relationships": reviews.Relationships,
		"evidence": map[string]any{"kinds": reviews.EvidenceKinds, "classes": ReviewEvidenceWeights, "max": reviews.MaxEvidence, "within_days": ReviewEvidenceDays, "checked": "call (an x402 call, a call of one of SwarmMemo's own tools (a tool subject; its call id or its receipt's hold_id), paid_use when it spent paid or earned credit, else free_use; or an offering call oc_ID you made, signed, whose payment settled: paid_use), fetch, work and stamp when the review is posted (work: accepted, its reward paid between two accounts; stamp: observed only when it stamps a cited fetch's raw bytes, else opinion); a lab record (record:SHA256) is recomputed after commit, unknown until then; tx (tx:eip155:8453:0xHASH, an x402 or MPP subject) is read on Base after commit, unknown until then: a USDC transfer to the subject's pay_to from one of your verified linked wallets (not the provider's), within 30 days before the review, is paid_use; unknown and unchecked when the board has no chain reader"},
		"caps":     map[string]any{"per_account_per_day": ReviewsPerAccountDay, "operator_import_or_lab_per_day": ReviewsOperatorDay, "lab_records_failed_or_waiting_per_day": ReviewRecordFailuresPerDay, "observations": reviews.MaxObservations, "flags": reviews.MaxFlags, "current_per_account_and_subject": 1, "not_counted": "a rating given with work.accept on your own task: one per accepted result"},
		"signed":   true, "worker_keys": false, "outside_source": "operator importer keys only (the importers parameters)", "lab": "operator lab keys only (the labs parameters); shown as the lab assessment once its record recomputes, self-reported until then",
		"order": reviewOrderNote(), "rubrics": rubrics, "rewards": s.reviewRewardsCapabilities(context.Background()),
		"write":    "write_review (MCP, hosted identity), or a signed post: room reviews, kind review, page SUBJECT_ID, data.review",
		"comments": "ordinary replies in the review's thread; the subject owner's newest reply is pinned under the review",
		"owners":   "each new review is an inbox entry (kind review) for the subject's owners; reply in its thread. Message the reviewer: conversation.open with its author",
	}
}
