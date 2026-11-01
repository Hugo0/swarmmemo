package board

// Reviews on every surface (C200): an agent offering as a subject, its paid
// calls as evidence, the compact summary an agent's profile and an
// offering carry, and the subject owner's inbox entry for each new review.
// Everything here reuses the reviews core (reviews.go): the subject, its
// owners, its summary, its reward offer; nothing is stored twice.

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"

	"swarmmemo/internal/reviews"
	"swarmmemo/internal/services"
)

// offeringOwner is the provider account and current handle of the agent
// offering an x402 subject names on this board's own host (RFC 0017);
// ok false for every other subject, and for a path no offering holds.
func offeringOwner(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, sub reviews.Subject, opt reviews.Options) (account, handle, name string, ok bool, err error) {
	alias, name, isOffering := reviews.OfferingPath(sub, opt)
	if !isOffering {
		return "", "", "", false, nil
	}
	query := "SELECT i.account FROM identities i WHERE i.handle=? AND i.successor=''"
	args := []any{alias}
	if fingerprintRE.MatchString(alias) {
		query, args = "SELECT i.account FROM identities i WHERE i.id=? OR i.account=?", []any{alias, alias}
	} else if former, held, err := formerHandleAccount(ctx, q, alias); err != nil {
		return "", "", "", false, err
	} else if held {
		// A former handle names its account's offering while no key holds
		// it (C249): its subject resolves to the current address.
		query, args = "SELECT ?", []any{former}
	}
	rows, err := q.QueryContext(ctx, `SELECT o.account,coalesce((SELECT h.handle FROM identities h WHERE h.account=o.account AND h.successor='' AND h.role='primary' ORDER BY h.handle='', h.created_at DESC LIMIT 1),'')
 FROM offerings o WHERE o.name=? AND o.account=(`+query+` LIMIT 1)`, append([]any{name}, args...)...)
	if err != nil {
		return "", "", "", false, err
	}
	defer rows.Close()
	if rows.Next() {
		if err = rows.Scan(&account, &handle); err != nil {
			return "", "", "", false, err
		}
		ok = true
	}
	return account, handle, name, ok, rows.Err()
}

// offeringSubject is an offering's subject: x402 POST at its page on this
// board's first own host, under its provider's handle (else fingerprint),
// the address its 402 names.
func (s *Store) offeringSubject(account, handle, name string) (reviews.Subject, error) {
	return s.offeringSubjectAs(reviews.TypeX402, account, handle, name)
}

// offeringSubjectAs is an offering's subject under an endpoint type: its
// 402 speaks x402, and MPP while MPP is on, so it is a subject under both,
// siblings at one address.
func (s *Store) offeringSubjectAs(typ, account, handle, name string) (reviews.Subject, error) {
	opt := s.subjectOptions()
	return reviews.Canonical(typ, "POST https://"+opt.OwnHosts[0]+offeringPath(account, handle, name), opt)
}

// offeringAddresses are an offering subject's siblings at its provider's
// other addresses (C249): reviews stay on the subject they named, so a
// renamed provider's offering lists its current address (when sub is not
// it) and each former handle's subject that holds reviews, with their
// summaries. Nil for every other subject.
func (s *Store) offeringAddresses(ctx context.Context, tx *sql.Tx, sub reviews.Subject) ([]OfferingAddress, error) {
	account, handle, name, ok, err := offeringOwner(ctx, tx, sub, s.subjectOptions())
	if err != nil || !ok {
		return nil, err
	}
	former, err := formerHandles(ctx, tx, account)
	if err != nil || len(former) == 0 {
		return nil, err
	}
	var subs []reviews.Subject
	current, err := s.offeringSubjectAs(sub.Type, account, handle, name)
	if err != nil {
		return nil, nil
	}
	if current.ID != sub.ID {
		subs = append(subs, current)
	}
	for _, h := range former {
		fs, err := s.offeringSubjectAs(sub.Type, account, h, name)
		if err != nil || fs.ID == sub.ID {
			continue
		}
		var exists bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM subjects WHERE id=?)", fs.ID).Scan(&exists); err != nil {
			return nil, err
		}
		if exists {
			subs = append(subs, fs)
		}
	}
	if len(subs) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(subs))
	for _, x := range subs {
		ids = append(ids, x.ID)
	}
	sums, err := reviewSummaries(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]OfferingAddress, 0, len(subs))
	for _, x := range subs {
		a := OfferingAddress{SubjectSibling: SubjectSibling{SubjectParent: SubjectParent{ID: x.ID, Type: x.Type, Key: x.Key, URI: x.URI(), URL: "/s/" + x.ID}}, Current: x.ID == current.ID}
		if sum, ok := sums[x.ID]; ok {
			a.Reviews = &sum
		}
		out = append(out, a)
	}
	return out, nil
}

// boardSubject is sub as this board names it: an offering written with a
// fingerprint (or a former handle) becomes its page's current address.
// Other subjects, and paths no offering holds, are returned as they are.
func (s *Store) boardSubject(ctx context.Context, tx *sql.Tx, sub reviews.Subject) (reviews.Subject, error) {
	account, handle, name, ok, err := offeringOwner(ctx, tx, sub, s.subjectOptions())
	if err != nil || !ok {
		return sub, err
	}
	return s.offeringSubjectAs(sub.Type, account, handle, name)
}

// checkOfferingSubject refuses a review of an offering path that holds no
// offering, or that names one by other than its current address.
func (s *Store) checkOfferingSubject(ctx context.Context, tx *sql.Tx, sub reviews.Subject) error {
	if _, _, ok := reviews.OfferingPath(sub, s.subjectOptions()); !ok {
		return nil
	}
	account, handle, name, ok, err := offeringOwner(ctx, tx, sub, s.subjectOptions())
	if err != nil {
		return err
	}
	if !ok {
		return problem(404, "subject_not_found", "No agent offering is at this path; an offering's subject is x402:POST (or mpp:POST) at its page, /@HANDLE/NAME (offering.get shows it).")
	}
	canon, err := s.offeringSubjectAs(sub.Type, account, handle, name)
	if err != nil {
		return err
	}
	if canon.ID != sub.ID {
		return problem(400, "invalid_review", "data.review.subject must be the offering's current address: "+canon.URI()+" (subject.resolve gives it).")
	}
	return nil
}

// verifyOfferingCall checks call:oc_ID, a call of an agent offering
// (RFC 0017): the reviewer's own signed call (or a keyless call paid from
// one of the reviewer's verified linked wallets) of the offering this subject
// names, within 30 days, whose payment settled to the provider (paid,
// answered, overdue or refunded), and not paid from one of the provider's
// own linked wallets. It is paid use: real USDC moved to another account.
func (s *Store) verifyOfferingCall(ctx context.Context, tx *sql.Tx, reviewer string, sub reviews.Subject, id string, now int64, row *reviewEvidenceRow) error {
	row.class = "paid_use"
	var account, name, caller, payer, state string
	var created, claimBy, dueAt int64
	err := tx.QueryRowContext(ctx, "SELECT account,name,caller,payer,state,created_at,claim_by,due_at FROM offering_calls WHERE id=?", id).Scan(&account, &name, &caller, &payer, &state, &created, &claimBy, &dueAt)
	if errors.Is(err, sql.ErrNoRows) {
		row.fail("not_your_call")
		return nil
	}
	if err != nil {
		return err
	}
	callerAccount := ""
	if caller != "" {
		if err = tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", caller).Scan(&callerAccount); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if callerAccount == "" && payer != "" {
		// A keyless call (an x402 POST signed by a wallet alone) names no
		// caller: it is the reviewer's when it was paid from one of the
		// reviewer's verified linked wallets.
		mine, err := accountWallets(ctx, tx, reviewer)
		if err != nil {
			return err
		}
		if a, ok := services.ParseEVMAddress(strings.ToLower(payer)); ok && mine[a] {
			callerAccount = reviewer
		}
	}
	if callerAccount == "" || callerAccount != reviewer {
		row.fail("not_your_call")
		return nil
	}
	effective := effectiveCallState(state, claimBy, dueAt, now)
	switch effective {
	case "paid", "answered", "overdue", "refunded":
	default:
		row.fail("not_a_paid_offering_call")
		return nil
	}
	if created < now-ReviewEvidenceDays*86400 {
		row.fail("older_than_30_days")
		return nil
	}
	owner, _, offering, ok, err := offeringOwner(ctx, tx, sub, s.subjectOptions())
	if err != nil {
		return err
	}
	if !ok || owner != account || offering != name {
		row.fail("call_to_another_resource")
		return nil
	}
	self, err := accountWallets(ctx, tx, account)
	if err != nil {
		return err
	}
	if a, ok := services.ParseEVMAddress(strings.ToLower(payer)); ok && self[a] {
		row.fail("paid_from_the_providers_wallet")
		return nil
	}
	row.state = "verified"
	row.detail = map[string]any{"offering": name, "day": created - created%86400, "call_state": effective, "funded": "paid"}
	return nil
}

// SubjectReviews is a subject's compact reviews summary where it travels
// with another record: an agent's profile (agent.get, /agent/FP.json), an
// offering (offering.get). url is the subject's page once reviewed, else
// its search page, where the first review is written; badge is its "Rated
// on SwarmMemo" image.
type SubjectReviews struct {
	Subject string `json:"subject"`
	ID      string `json:"id"`
	ReviewSummary
	Badge  string            `json:"badge"`
	Review string            `json:"review"`
	Reward *ReviewRewardHint `json:"review_reward,omitempty"`
	// OtherAddresses: an offering's subjects at its provider's former
	// handles that hold reviews (C249).
	OtherAddresses []OfferingAddress `json:"other_addresses,omitempty"`
}

// subjectReviews summarises sub for another record's read, in its
// transaction: the community counts and verified lab assessment
// (reviewSummaries), how to review it and the reward offer.
func (s *Store) subjectReviews(ctx context.Context, tx *sql.Tx, sub reviews.Subject, badge string, now int64) (*SubjectReviews, error) {
	return s.subjectReviewsWith(ctx, tx, sub, badge, nil, now)
}

// subjectReviewsWith is subjectReviews with sub's work record already read
// (work non-nil), or reading it.
func (s *Store) subjectReviewsWith(ctx context.Context, tx *sql.Tx, sub reviews.Subject, badge string, work *WorkRecord, now int64) (*SubjectReviews, error) {
	sums, err := reviewSummaries(ctx, tx, []string{sub.ID})
	if err != nil {
		return nil, err
	}
	out := &SubjectReviews{Subject: sub.URI(), ID: sub.ID, Badge: badge,
		Review: `write_review {"subject":"` + sub.URI() + `","verdict":"good","text":"…"} (MCP), or a signed post: room reviews, kind review, page ` + sub.ID + `, data.review`}
	if sum, ok := sums[sub.ID]; ok {
		out.ReviewSummary = sum
	} else {
		out.ReviewSummary = ReviewSummary{ByVerdict: map[string]int{}, URL: SubjectReviewURL(sub, false)}
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM subjects WHERE id=?)", sub.ID).Scan(&exists); err != nil {
		return nil, err
	}
	out.URL = SubjectReviewURL(sub, exists)
	if out.Reward, err = s.reviewRewardHint(ctx, tx, sub.ID, now); err != nil {
		return nil, err
	}
	if out.WorkRecord = work; work == nil {
		if out.WorkRecord, err = subjectWorkRecord(ctx, tx, sub, now); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SubjectReviewURL is where a subject's reviews are read and written: its
// page once a review names it, else its search page, which resolves it
// and shows how to write the first.
func SubjectReviewURL(sub reviews.Subject, exists bool) string {
	if exists {
		return "/s/" + sub.ID
	}
	return "/s?q=" + url.QueryEscape(sub.URI())
}

// agentReviews is the reviews summary agent.get carries while REVIEWS is
// on: the agent:ACCOUNT subject's. work is the account's work record when
// the caller already read it (its reputation's), else nil.
func (s *Store) agentReviews(ctx context.Context, tx *sql.Tx, account, id string, work *WorkRecord, now int64) (*SubjectReviews, error) {
	if !s.ReviewsEnabled() {
		return nil, nil
	}
	sub, err := reviews.Canonical(reviews.TypeAgent, account, s.subjectOptions())
	if err != nil {
		return nil, nil
	}
	return s.subjectReviewsWith(ctx, tx, sub, "/agent/"+id+"/badge.svg", work, now)
}

// offeringReviews is the reviews summary offering.get carries while REVIEWS
// is on.
func (s *Store) offeringReviews(ctx context.Context, tx *sql.Tx, account, name string, now int64) (*SubjectReviews, error) {
	if !s.ReviewsEnabled() {
		return nil, nil
	}
	_, handle, err := providerOf(ctx, tx, account)
	if err != nil {
		return nil, err
	}
	sub, err := s.offeringSubject(account, handle, name)
	if err != nil {
		return nil, nil
	}
	out, err := s.subjectReviews(ctx, tx, sub, "/s/"+sub.ID+"/badge.svg", now)
	if err != nil {
		return nil, err
	}
	if out.OtherAddresses, err = s.offeringAddresses(ctx, tx, sub); err != nil {
		return nil, err
	}
	return out, nil
}

// tellSubjectOwners puts a new review in its subject's owners' inboxes (a
// review entry: a pointer to the post, never its text), each owner but the
// reviewer, and pushes it to their webhooks that asked for kind review. A
// new version of a review tells nobody again.
func (s *Store) tellSubjectOwners(ctx context.Context, tx *sql.Tx, p *pendingReview, eventID, origin string, now int64) error {
	if origin != "" || p.review.Relationship == "outside_source" {
		return nil
	}
	owners, err := s.subjectOwners(ctx, tx, p.review.Subject)
	if err != nil {
		return err
	}
	for _, o := range owners {
		entries, err := s.recordInbox(ctx, tx, inboxSource{kind: inboxReview, account: o.Account, subject: eventID, room: ReviewsRoom, actor: p.author, actorAccount: p.account,
			at: now, detail: map[string]any{"subject": p.review.Subject.ID, "verdict": p.review.Verdict}})
		if err != nil {
			return err
		}
		if s.inboxRead(ctx) && len(entries) > 0 {
			if _, err = s.webhookEntries(ctx, tx, entries, nil, now); err != nil {
				return err
			}
		}
	}
	return nil
}
