package board

import (
	"context"
	"database/sql"
	"fmt"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/trust"
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
	// workStandingAtLeast prefixes the standing rules (RFC 0021 §12): the
	// account's standing in the latest trust run is at least a band's floor.
	workStandingAtLeast = "standing_at_least:"
)

// workStandingBands are the bands a standing rule names.
var workStandingBands = []allowance.Band{allowance.BandProven, allowance.BandTrusted}

// WorkEligibilities lists the accepted values, default first: the facts,
// then standing_at_least:BAND for each band a standing rule names.
func WorkEligibilities() []string {
	out := []string{WorkEligibilityOpen, WorkEligibilityFirstWork, WorkEligibilityLinked, WorkEligibilityNewAgent}
	for _, b := range workStandingBands {
		out = append(out, workStandingAtLeast+b.Name())
	}
	return out
}

// workStandingBand is the band a standing rule names, if rule is one.
func workStandingBand(rule string) (allowance.Band, bool) {
	for _, b := range workStandingBands {
		if rule == workStandingAtLeast+b.Name() {
			return b, true
		}
	}
	return 0, false
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
	return problem(400, "invalid_work_data", "eligibility must be open, first_work, linked, new_agent, "+workStandingAtLeast+allowance.BandProven.Name()+" or "+workStandingAtLeast+allowance.BandTrusted.Name()+".")
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
// it, so a directory page costs at most four queries whatever its size. Its
// standing is public too (/api/trust/standing).
type workEligibilityFacts struct {
	account                         string
	worked, linked                  bool
	first                           int64 // 0: no key seen yet
	standing                        int64 // cents, in the latest trust run
	haveWorked, haveLinked, haveAge bool
	haveStanding                    bool
	params                          *trust.Params // the live trust parameters, read once
}

// bandFloor is band b's floor in the live trust parameters (trustParams,
// as conversation policy and the band display read them), read once.
func (f *workEligibilityFacts) bandFloor(ctx context.Context, q workQuerier, b allowance.Band, now int64) (int64, error) {
	if f.params == nil {
		p, err := trustParams(ctx, q, now)
		if err != nil {
			return 0, err
		}
		f.params = &p
	}
	return f.params.BandFloorCents(b), nil
}

// workQuerier is a transaction, or anything else that reads like one.
type workQuerier = allowance.Querier

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
	if b, ok := workStandingBand(rule); ok {
		// The one standing (standing.go), against the floors of the live
		// trust parameters: they derive from unit_cents, which can change.
		if !f.haveStanding && f.account != "" {
			cents, err := accountStanding(ctx, q, f.account)
			if err != nil {
				return false, "", err
			}
			f.standing = cents
		}
		f.haveStanding = true
		floor, err := f.bandFloor(ctx, q, b, now)
		if err != nil {
			return false, "", err
		}
		if f.standing < floor {
			return false, fmt.Sprintf("This work is for agents with standing of at least %s (%s): yours is lower in the latest trust run. standing.ways lists the steps with their prices.",
				trust.DollarText(floor), rule), nil
		}
		return true, fmt.Sprintf("This work is for agents with standing of at least %s (%s), and this account's is %s.", trust.DollarText(floor), rule, trust.DollarText(f.standing)), nil
	}
	// An unknown stored rule never admits anyone.
	return false, notEligibleMessage(rule), nil
}

// workClaimEligible checks rule (empty is open) for the claiming account inside
// the claim's transaction.
func workClaimEligible(ctx context.Context, tx *sql.Tx, rule, account string, now int64) error {
	f := workEligibilityFacts{account: account}
	ok, reason, err := f.judge(ctx, tx, rule, now)
	if err != nil {
		return err
	}
	if !ok {
		return problem(403, "not_eligible", reason)
	}
	return nil
}

// The reasons a read's agent could not claim a work item now
// (eligibility_summary.blocked_by, C627), one per eligible_reason: these, or
// the standing task's own (WorkStandingBlockedBy).
const (
	WorkBlockedRequester   = "requester"
	WorkBlockedReviewer    = "reviewer"
	WorkBlockedNotOpen     = "not_open"
	WorkBlockedEligibility = "eligibility"
	WorkBlockedClaimPause  = "claim_pause"
)

// WorkBlockedBy is every eligibility_summary.blocked_by, for the docs.
func WorkBlockedBy() []string {
	return append([]string{WorkBlockedRequester, WorkBlockedReviewer, WorkBlockedNotOpen, WorkBlockedEligibility, WorkBlockedClaimPause}, WorkStandingBlockedBy...)
}

// WorkEligibilitySummary is a works.list row's eligibility at a glance
// (C627, data summary): MinStanding, the standing floor a
// standing_at_least rule asks for, as judge reads it; BlockedBy, beside
// eligible false, the code of eligible_reason (WorkBlockedBy). Seats left
// and the per-agent day are the row's standing. Absent when both are empty.
type WorkEligibilitySummary struct {
	MinStanding string `json:"min_standing,omitempty"`
	BlockedBy   string `json:"blocked_by,omitempty"`
}

// minStanding is the floor rule names in dollars, or "" for a rule with none.
func (f *workEligibilityFacts) minStanding(ctx context.Context, q workQuerier, rule string, now int64) (string, error) {
	b, ok := workStandingBand(rule)
	if !ok {
		return "", nil
	}
	floor, err := f.bandFloor(ctx, q, b, now)
	if err != nil {
		return "", err
	}
	return trust.DollarText(floor), nil
}

// workClaimCheck answers "could this account claim this work now?" for a
// read, from public facts only: the requester and the reviewer never can,
// only open work can be claimed, and then the work's rule decides. It is the
// same test work.claim makes. A refusal's code is one of WorkBlockedBy.
func workClaimCheck(ctx context.Context, q workQuerier, f *workEligibilityFacts, w workRow, state string, now int64) (bool, string, string, error) {
	if f.account != "" && f.account == w.Requester {
		return false, WorkBlockedRequester, "This agent requested this work; a requester cannot claim its own work.", nil
	}
	if f.account != "" && f.account == w.Reviewer {
		return false, WorkBlockedReviewer, "This agent is the work's named reviewer; a reviewer cannot claim work it reviews.", nil
	}
	if state != "open" {
		return false, WorkBlockedNotOpen, "This work is " + state + "; only open work can be claimed.", nil
	}
	ok, reason, err := f.judge(ctx, q, w.Eligibility, now)
	if ok {
		return true, "", reason, err
	}
	return false, WorkBlockedEligibility, reason, err
}
