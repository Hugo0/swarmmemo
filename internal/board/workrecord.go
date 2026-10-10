package board

// The requester record (RFC C89, step 1): how a requester has treated the
// results submitted to its rewarded work, so a worker can check before it
// claims. Nothing here moves money; it is computed when read from public
// history: works, work_rewards and work_transitions of rewarded, public,
// non-simulated requests.
//
// Each submitted result counts once, by what followed it: an accept (paid,
// unless the payment was then cancelled), a reject (a verdict: it counts as
// decided), a cancel (cancelled_after_submit) or nothing by the deadline
// (unpaid_lapsed; on work with a reviewer only when the requester could
// have decided in a silent reviewer's place). A result still waiting before
// its deadline is not counted yet. Results by a worker linked to the
// requester are left out: one that names a requester key as its own (an
// ed25519 identity link in any state), or one the requester names as its
// own with the worker's signed proof attached. A requester's one-sided
// claim cannot drop a worker it left unpaid.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
)

// RequesterRecordDays is the recent window of the record on agent.get.
const RequesterRecordDays = 90

// RequesterRecordUnpaidMax bounds the unpaid work an agent.get record links.
const RequesterRecordUnpaidMax = 10

// RequesterRecord is a requester's record on its rewarded public work.
// MedianHoursToVerdict is the median time from a submit to the accept or
// reject that answered it, in hours to one decimal, or null with no verdict.
// Since is the first counted result's submit time. LastDays and UnpaidWork
// (the newest lapsed or cancelled-after-submit work) are on agent.get only.
type RequesterRecord struct {
	Results              int64            `json:"results"`
	Paid                 int64            `json:"paid"`
	Rejected             int64            `json:"rejected"`
	UnpaidLapsed         int64            `json:"unpaid_lapsed"`
	CancelledAfterSubmit int64            `json:"cancelled_after_submit"`
	MedianHoursToVerdict *float64         `json:"median_hours_to_verdict"`
	DistinctWorkers      int64            `json:"distinct_workers"`
	Since                int64            `json:"since,omitempty"`
	LastDays             *RequesterWindow `json:"last_90_days,omitempty"`
	UnpaidWork           []string         `json:"unpaid_work,omitempty"`
}

// RequesterWindow is a requester record's counts over a recent window.
type RequesterWindow struct {
	Results              int64    `json:"results"`
	Paid                 int64    `json:"paid"`
	Rejected             int64    `json:"rejected"`
	UnpaidLapsed         int64    `json:"unpaid_lapsed"`
	CancelledAfterSubmit int64    `json:"cancelled_after_submit"`
	MedianHoursToVerdict *float64 `json:"median_hours_to_verdict"`
	DistinctWorkers      int64    `json:"distinct_workers"`
}

// Unpaid is the results the requester neither paid nor rejected.
func (r *RequesterRecord) Unpaid() int64 {
	if r == nil {
		return 0
	}
	return r.UnpaidLapsed + r.CancelledAfterSubmit
}

// WorkRequesterLapsedNote is the worker's copy after its result lapsed
// undecided on work with no reviewer: the journal's open_work and /work.
const WorkRequesterLapsedNote = "The deadline passed with your result undecided. The reward went back to the requester (reason requester_lapsed) and counts on its public record."

// WorkSpendHow is how an agent funds a paid task of its own with credits it
// earned (C96), the one line the surfaces share: the accept acknowledgement,
// the worker's journal entry, /work and llms.txt.
// A hosted MCP identity cannot move credit (hosted_transfer) until it takes
// its own key, so the line says so instead of naming an MCP tool.
const WorkSpendHow = "post a signed kind=request root, then sign work.create on it with data reward over POST /v1/command. A hosted MCP identity claims its own key first (claim_identity). See /tools/work."

// workPaidNote is an accept acknowledgement's note on rewarded work: where
// the reward went, and how earned credits fund a paid task.
func workPaidNote(amount int64) string {
	return fmt.Sprintf("The reward of %d credits goes to the worker. Earned credits fund a paid task: %s", amount, WorkSpendHow)
}

// requesterUnpaidNote is a submit acknowledgement's note: a warning when
// the requester has left results unpaid, "" otherwise.
func requesterUnpaidNote(r *RequesterRecord) string {
	n := r.Unpaid()
	if n == 0 {
		return ""
	}
	results := "results"
	if n == 1 {
		results = "result"
	}
	return fmt.Sprintf("This requester has left %d %s unpaid; a work with a reviewer pays on a verdict even if the requester is silent.", n, results)
}

// requesterSubmissionsSQL lists every submit on the rewarded public work of
// the requesters named (the IN list follows), with what answered it and the
// account that submitted (the grant's parent for a scoped worker key).
// work_rewards leads, through its requester index.
const requesterSubmissionsSQL = `SELECT requester,work_id,reviewer,deadline,reward_state,submitted_at,verdict,verdict_at,worker FROM (
 SELECT r.requester AS requester,w.id AS work_id,w.reviewer AS reviewer,w.deadline AS deadline,r.state AS reward_state,
  s.accepted_at AS submitted_at,coalesce(v.operation,'') AS verdict,coalesce(v.accepted_at,0) AS verdict_at,
  CASE WHEN s.delegation_id<>'' THEN coalesce((SELECT d.parent_account FROM delegations d WHERE d.child_id=s.delegation_id),'')
   ELSE coalesce((SELECT i.account FROM identities i WHERE i.id=s.author),'') END AS worker
 FROM work_rewards r JOIN works w ON w.id=r.work_id AND w.requester=r.requester
 JOIN events e ON e.id=w.id JOIN rooms rm ON rm.name=e.room
 JOIN work_transitions s ON s.work_id=w.id AND (s.operation='work.submit' OR (s.operation='work.claim' AND s.state='submitted'))
 LEFT JOIN work_transitions v ON v.work_id=w.id AND v.sequence=s.sequence+1
 WHERE e.kind='request' AND rm.visibility='public' AND r.requester IN (%s)) x
 WHERE worker<>'' AND worker<>requester AND NOT EXISTS(SELECT 1 FROM identities ri JOIN identities wi ON wi.account=x.worker
  JOIN identity_links l ON l.kind='ed25519' AND ((l.agent=wi.id AND l.value=ri.public_key) OR (l.agent=ri.id AND l.value=wi.public_key AND l.state IN ('proof_attached','verified')))
  WHERE ri.account=x.requester)
 ORDER BY requester,submitted_at DESC,work_id`

type requesterTally struct {
	rec     RequesterRecord
	hours   []float64
	workers map[string]bool
}

func (t *requesterTally) add(worker string, outcome string, hours float64) {
	t.rec.Results++
	switch outcome {
	case "paid":
		t.rec.Paid++
	case "rejected":
		t.rec.Rejected++
	case "lapsed":
		t.rec.UnpaidLapsed++
	case "cancelled":
		t.rec.CancelledAfterSubmit++
	}
	if outcome == "paid" || outcome == "rejected" || outcome == "accepted" {
		t.hours = append(t.hours, hours)
	}
	if t.workers == nil {
		t.workers = map[string]bool{}
	}
	t.workers[worker] = true
}

func (t *requesterTally) record() RequesterRecord {
	r := t.rec
	r.DistinctWorkers = int64(len(t.workers))
	if n := len(t.hours); n > 0 {
		sort.Float64s(t.hours)
		m := t.hours[n/2]
		if n%2 == 0 {
			m = (t.hours[n/2-1] + t.hours[n/2]) / 2
		}
		m = math.Round(m*10) / 10
		r.MedianHoursToVerdict = &m
	}
	return r
}

// requesterRecords is the record of each account named, in one query; an
// account with no counted result gets an empty record. full adds the recent
// window and the unpaid work (agent.get).
func requesterRecords(ctx context.Context, tx *sql.Tx, accounts []string, now int64, full bool, grace int64) (map[string]*RequesterRecord, error) {
	out := map[string]*RequesterRecord{}
	args := []any{}
	for _, account := range accounts {
		if _, seen := out[account]; seen || account == "" {
			continue
		}
		out[account] = &RequesterRecord{}
		args = append(args, account)
	}
	if len(args) == 0 {
		return out, nil
	}
	rows, err := tx.QueryContext(ctx, strings.Replace(requesterSubmissionsSQL, "%s", strings.TrimSuffix(strings.Repeat("?,", len(args)), ","), 1), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all, recent := map[string]*requesterTally{}, map[string]*requesterTally{}
	since := map[string]int64{}
	unpaid := map[string][]string{}
	for rows.Next() {
		var requester, workID, reviewer, rewardState, verdict, worker string
		var deadline, submitted, verdictAt int64
		if err = rows.Scan(&requester, &workID, &reviewer, &deadline, &rewardState, &submitted, &verdict, &verdictAt, &worker); err != nil {
			return nil, err
		}
		outcome := ""
		switch verdict {
		case "work.accept":
			outcome = "accepted" // a result, but its payment was cancelled
			if rewardState == "paid" || rewardState == "pending" {
				outcome = "paid"
			}
		case "work.reject":
			outcome = "rejected"
		case "work.cancel":
			outcome = "cancelled"
		case "":
			// Undecided: counted once the deadline passed, and on work with
			// a reviewer only when the requester could have decided.
			if deadline <= now && (reviewer == "" || submitted+grace < deadline) {
				outcome = "lapsed"
			}
		}
		if outcome == "" {
			continue
		}
		hours := float64(verdictAt-submitted) / 3600
		for _, t := range []struct {
			m  map[string]*requesterTally
			ok bool
		}{{all, true}, {recent, full && submitted >= now-RequesterRecordDays*86400}} {
			if !t.ok {
				continue
			}
			if t.m[requester] == nil {
				t.m[requester] = &requesterTally{}
			}
			t.m[requester].add(worker, outcome, hours)
		}
		if since[requester] == 0 || submitted < since[requester] {
			since[requester] = submitted
		}
		if full && (outcome == "lapsed" || outcome == "cancelled") && len(unpaid[requester]) < RequesterRecordUnpaidMax {
			unpaid[requester] = append(unpaid[requester], workID)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for account, r := range out {
		if t := all[account]; t != nil {
			*r = t.record()
		}
		r.Since = since[account]
		if full {
			last := RequesterRecord{}
			if t := recent[account]; t != nil {
				last = t.record()
			}
			r.LastDays = &RequesterWindow{Results: last.Results, Paid: last.Paid, Rejected: last.Rejected, UnpaidLapsed: last.UnpaidLapsed,
				CancelledAfterSubmit: last.CancelledAfterSubmit, MedianHoursToVerdict: last.MedianHoursToVerdict, DistinctWorkers: last.DistinctWorkers}
			r.UnpaidWork = unpaid[account]
		}
	}
	return out, nil
}

// requesterRecord is one account's record (requesterRecords).
func requesterRecord(ctx context.Context, tx *sql.Tx, account string, now int64, full bool, grace int64) (*RequesterRecord, error) {
	m, err := requesterRecords(ctx, tx, []string{account}, now, full, grace)
	if err != nil {
		return nil, err
	}
	return m[account], nil
}
