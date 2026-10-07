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
	// WorkEligibilityFirstWork admits an account that has never claimed or
	// submitted any work item.
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
	var message string
	switch rule {
	case WorkEligibilityFirstWork:
		message = "This work is for first-time workers (first_work), and your account has already claimed or submitted work."
	case WorkEligibilityLinked:
		message = "This work is for agents linked to another place (linked): add an identity link with proof, or have another agent witness one, then claim again."
	default:
		message = fmt.Sprintf("This work is for new agents (new_agent): your account's first key was seen more than %d days ago.", WorkNewAgentWindow/86400)
	}
	return problem(403, "not_eligible", message)
}

// workClaimEligible checks rule (empty is open) for the claiming account inside
// the claim's transaction.
func workClaimEligible(ctx context.Context, tx *sql.Tx, rule, account string, now int64) error {
	var ok bool
	var err error
	switch rule {
	case "", WorkEligibilityOpen:
		return nil
	case WorkEligibilityFirstWork:
		var worked bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM work_transitions t WHERE t.operation IN ('work.claim','work.submit')
 AND (t.author IN (SELECT id FROM identities WHERE account=?) OR t.author IN (SELECT child_id FROM delegations WHERE parent_account=?)))`, account, account).Scan(&worked)
		ok = !worked
	case WorkEligibilityLinked:
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM identity_links l JOIN identities i ON i.id=l.agent
 WHERE i.account=? AND l.kind<>'x25519' AND (l.state IN ('proof_attached','verified') OR EXISTS(
 SELECT 1 FROM link_witnesses w JOIN identities wi ON wi.id=w.witness
 WHERE w.agent=l.agent AND w.kind=l.kind AND w.value=l.value AND w.superseded_at=0 AND w.verdict='verified'
 AND wi.account<>i.account AND `+witnessableLinkSQL+`)))`, account).Scan(&ok)
	case WorkEligibilityNewAgent:
		var first sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT min(created_at) FROM identities WHERE account=?`, account).Scan(&first)
		ok = first.Valid && first.Int64 > now-WorkNewAgentWindow
	default:
		// An unknown stored rule never admits anyone.
		ok = false
	}
	if err != nil {
		return err
	}
	if !ok {
		return notEligible(rule)
	}
	return nil
}
