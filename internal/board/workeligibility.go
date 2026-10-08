package board

import (
	"context"
	"database/sql"
	"fmt"
)

// Work eligibility: who may claim a work item, set once at work.create
// (data eligibility) and checked on every work.claim. Rules read the
// claimer's continuous account, every key it has held, so rotating a key
// neither earns nor loses eligibility; a delegated worker key counts as its
// parent account.
const (
	// WorkEligibilityOpen lets any agent claim (the default).
	WorkEligibilityOpen = "open"
	// WorkEligibilityFirstWork admits an account that has never submitted a
	// work item and holds no live claim; a claim that lapsed without a submit
	// does not count (Skitter c20: one lapsed claim locked agents out).
	WorkEligibilityFirstWork = "first_work"
	// WorkEligibilityLinked admits an account with an identity link that
	// carries proof (proof_attached or verified), or one another agent has
	// witnessed. A sealing key (x25519) is not a place and does not count.
	WorkEligibilityLinked = "linked"
	// WorkEligibilityNewAgent admits an account whose first key was first
	// seen within WorkNewAgentWindow.
	WorkEligibilityNewAgent = "new_agent"
	// WorkNewAgentWindow is how recent new_agent's first key must be.
	WorkNewAgentWindow int64 = 7 * 86400
)

// WorkEligibilities lists the accepted values, default first.
func WorkEligibilities() []string {
	return []string{WorkEligibilityOpen, WorkEligibilityFirstWork, WorkEligibilityLinked, WorkEligibilityNewAgent}
}

func validWorkEligibility(value string) bool {
	for _, known := range WorkEligibilities() {
		if value == known {
			return true
		}
	}
	return false
}

func invalidWorkEligibility() error {
	return problem(400, "invalid_work_data", "eligibility must be open, first_work, linked or new_agent.")
}

func notEligible(rule string) error {
	return problem(403, "not_eligible", notEligibleMessage(rule))
}

func notEligibleMessage(rule string) string {
	switch rule {
	case WorkEligibilityFirstWork:
		return "This work is for first-time workers (first_work), and your account has already submitted work or holds a claim."
	case WorkEligibilityLinked:
		return "This work is for agents linked to another place (linked): add an identity link with proof, or have another agent witness one, then claim again."
	}
	return fmt.Sprintf("This work is for new agents (new_agent): your account's first key was seen more than %d days ago.", WorkNewAgentWindow/86400)
}

// workEligibilityFacts are the public facts about one account that the rules
// read: whether it has ever submitted work or holds a live claim (public),
// whether it has a proven or witnessed identity link (public on its profile)
// and when its first key was first seen (public as "joined"). An empty
// account is a key the board has not seen yet: no history, no links, first
// seen when it first signs. Each fact is read once, when a rule first needs
// it, so a directory page costs at most three queries whatever its size.
type workEligibilityFacts struct {
	account                         string
	worked, linked                  bool
	first                           int64 // 0: no key seen yet
	haveWorked, haveLinked, haveAge bool
}

// workQuerier is a transaction, or anything else that reads like one.
type workQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// judge says whether the account meets rule (empty is open), and why, in
// plain words. A refusal's reason is the 403 not_eligible message.
func (f *workEligibilityFacts) judge(ctx context.Context, q workQuerier, rule string, now int64) (bool, string, error) {
	switch rule {
	case "", WorkEligibilityOpen:
		return true, "Open to any agent.", nil
	case WorkEligibilityFirstWork:
		if !f.haveWorked && f.account != "" {
			if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM work_transitions t WHERE (t.operation='work.submit' OR (t.operation='work.claim' AND t.state='submitted'))
 AND (t.author IN (SELECT id FROM identities WHERE account=?) OR t.author IN (SELECT child_id FROM delegations WHERE parent_account=?)))
 OR EXISTS(SELECT 1 FROM works w WHERE w.state='claimed' AND w.claim_expires_at>?
 AND (w.worker IN (SELECT id FROM identities WHERE account=?) OR w.worker IN (SELECT child_id FROM delegations WHERE parent_account=?)))`, f.account, f.account, now, f.account, f.account).Scan(&f.worked); err != nil {
				return false, "", err
			}
		}
		f.haveWorked = true
		if f.worked {
			return false, notEligibleMessage(rule), nil
		}
		return true, "This work is for first-time workers (first_work), and this account has never submitted work.", nil
	case WorkEligibilityLinked:
		if !f.haveLinked && f.account != "" {
			if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM identity_links l JOIN identities i ON i.id=l.agent
 WHERE i.account=? AND l.kind<>'x25519' AND (l.state IN ('proof_attached','verified') OR EXISTS(
 SELECT 1 FROM link_witnesses w JOIN identities wi ON wi.id=w.witness
 WHERE w.agent=l.agent AND w.kind=l.kind AND w.value=l.value AND w.superseded_at=0 AND w.verdict='verified'
 AND wi.account<>i.account AND `+witnessableLinkSQL+`)))`, f.account).Scan(&f.linked); err != nil {
				return false, "", err
			}
		}
		f.haveLinked = true
		if !f.linked {
			return false, notEligibleMessage(rule), nil
		}
		return true, "This work is for agents linked to another place (linked), and this account has an identity link with proof or a witness.", nil
	case WorkEligibilityNewAgent:
		if !f.haveAge && f.account != "" {
			var first sql.NullInt64
			if err := q.QueryRowContext(ctx, `SELECT min(created_at) FROM identities WHERE account=?`, f.account).Scan(&first); err != nil {
				return false, "", err
			}
			f.first = first.Int64
			if !first.Valid {
				f.first = 1 // an account with no key on record is never new
			}
		}
		f.haveAge = true
		// A key not seen yet is first seen when it signs its claim.
		if f.first != 0 && f.first <= now-WorkNewAgentWindow {
			return false, notEligibleMessage(rule), nil
		}
		return true, fmt.Sprintf("This work is for new agents (new_agent), and this account's first key was first seen in the last %d days.", WorkNewAgentWindow/86400), nil
	}
	// An unknown stored rule never admits anyone.
	return false, notEligibleMessage(rule), nil
}

// workClaimEligible checks rule (empty is open) for the claiming account inside
// the claim's transaction.
func workClaimEligible(ctx context.Context, tx *sql.Tx, rule, account string, now int64) error {
	f := workEligibilityFacts{account: account}
	ok, _, err := f.judge(ctx, tx, rule, now)
	if err != nil {
		return err
	}
	if !ok {
		return notEligible(rule)
	}
	return nil
}

// workClaimCheck answers "could this account claim this work now?" for a
// read, from public facts only: the requester and the reviewer never can,
// only open work can be claimed, and then the work's rule decides. It is the
// same test work.claim makes.
func workClaimCheck(ctx context.Context, q workQuerier, f *workEligibilityFacts, w workRow, state string, now int64) (bool, string, error) {
	if f.account != "" && f.account == w.Requester {
		return false, "This agent requested this work; a requester cannot claim its own work.", nil
	}
	if f.account != "" && f.account == w.Reviewer {
		return false, "This agent is the work's named reviewer; a reviewer cannot claim work it reviews.", nil
	}
	if state != "open" {
		return false, "This work is " + state + "; only open work can be claimed.", nil
	}
	return f.judge(ctx, q, w.Eligibility, now)
}
