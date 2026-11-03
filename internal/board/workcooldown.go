package board

import (
	"context"
	"fmt"
	"time"
)

// The claim cool-down (C580): a worker whose results on one standing task
// (or on one work item) were rejected WorkClaimCooldownRejects times within
// WorkClaimCooldownWindow cannot claim it again until WorkClaimCooldown
// after the last of those rejections. It stops a script resubmitting a
// template result every half hour, so the requester does not reject each by
// hand. Rejections count per account and across the accounts the existing
// link signals tie to it: one burst cluster (funnel_burst.go) or one shared
// network (sharedNetwork, the invite rewards' rule), so sibling keys share
// one count. Derived from the work history, nothing is stored; an accept
// never counts, and the requester's own transitions are not touched.
const (
	WorkClaimCooldownRejects       = 2
	WorkClaimCooldownWindow  int64 = 7 * 86400
	WorkClaimCooldown        int64 = 86400
)

// workClaimCooldownSQL is every rejection since ?2 with the account whose
// claim it judged (the claim of the same fence), on the work items %s picks.
const workClaimCooldownSQL = `SELECT %s, r.accepted_at FROM work_transitions r
 JOIN work_transitions c ON c.work_id=r.work_id AND c.fence=r.fence AND c.operation='work.claim'
 WHERE r.operation='work.reject' AND r.accepted_at>=?2 AND r.work_id IN (%s)`

// workClaimCooldownUntil is when account may claim w again, or 0 when it
// may now. On a standing task w is the task itself: its rejected results
// are the items it opened (standing_id, state cancelled). links holds the
// link signals a page has read for account already (nil reads them for
// this call alone).
func (s *Store) workClaimCooldownUntil(ctx context.Context, q workQuerier, links *claimLinks, w workRow, standing bool, account string, now int64) (int64, error) {
	from := now - WorkClaimCooldownWindow
	items := "?1"
	if standing {
		items = "SELECT id FROM works WHERE standing_id=?1 AND standing_id<>'' AND state='cancelled' AND updated_at>=?2"
	}
	rows, err := q.QueryContext(ctx, fmt.Sprintf(workClaimCooldownSQL, transitionAccountSQL("c"), items), w.ID, from)
	if err != nil {
		return 0, err
	}
	type reject struct {
		account string
		at      int64
	}
	var rejects []reject
	for rows.Next() {
		var r reject
		if err = rows.Scan(&r.account, &r.at); err != nil {
			rows.Close()
			return 0, err
		}
		rejects = append(rejects, r)
	}
	if err = closeRows(rows); err != nil || len(rejects) < WorkClaimCooldownRejects {
		return 0, err
	}
	if links == nil {
		links = &claimLinks{}
	}
	var count int
	var last int64
	for _, r := range rejects {
		if r.account == "" {
			continue
		}
		linked := r.account == account
		if !linked {
			if linked, err = links.siblings(ctx, s, q, account, r.account, from); err != nil {
				return 0, err
			}
		}
		if linked {
			count++
			last = max(last, r.at)
		}
	}
	if count < WorkClaimCooldownRejects || now >= last+WorkClaimCooldown {
		return 0, nil
	}
	return last + WorkClaimCooldown, nil
}

// claimLinks are the claim cool-down's link signals (siblings) one
// read has loaded: the stored burst clusters, parsed once, and each
// account's network peers since a time. A page of work rows judged for one
// caller shares them (workEligibilityFacts), so the same caller's links
// cost the same queries for one row or a whole page; nothing changes them
// inside one transaction.
type claimLinks struct {
	burst *BurstResult // nil: not read yet
	peers map[claimPeers]map[string]bool
}

type claimPeers struct {
	account string
	from    int64
}

// siblings reports whether the existing link signals tie a and b: one
// burst cluster, or one network both wrote from since from (sharedNetwork).
func (l *claimLinks) siblings(ctx context.Context, s *Store, q workQuerier, a, b string, from int64) (bool, error) {
	if l.burst == nil {
		res, err := storedBurst(ctx, q)
		if err != nil {
			return false, err
		}
		l.burst = res
	}
	if l.burst.sameCluster(a, b) {
		return true, nil
	}
	key := claimPeers{a, from}
	peers, ok := l.peers[key]
	if !ok {
		var err error
		if peers, err = s.networkPeers(ctx, q, a, from); err != nil {
			return false, err
		}
		if l.peers == nil {
			l.peers = map[claimPeers]map[string]bool{}
		}
		l.peers[key] = peers
	}
	return peers[b], nil
}

// workClaimCooldownText is the refusal's plain message.
func workClaimCooldownText(standing bool, until int64) string {
	what := "this task"
	if standing {
		what = "this standing task"
	}
	return fmt.Sprintf("Claims on %s are paused for you until %s: %d or more results on it from your account (or keys linked to it) were rejected in the last %d days, so each new claim waits %d hours after the latest rejection. Read the rejection reasons in work.history and change your result before you try again.",
		what, time.Unix(until, 0).UTC().Format(time.RFC3339), WorkClaimCooldownRejects, WorkClaimCooldownWindow/86400, WorkClaimCooldown/3600)
}

// workClaimCooldownReplyText is the same refusal worded for the requester
// (or reviewer) whose accept of a direct reply (C691) ran the claim for the
// reply's author: the pause is the author's, not the decider's (C706). It
// still holds there: the defence against an account and a linked one
// recycling rejected results must not depend on who sends the claim, and a
// reply has no verdict without one, so the way on is the same accept once
// the pause ends.
func workClaimCooldownReplyText(standing bool, until int64) string {
	what := "this task"
	if standing {
		what = "this standing task"
	}
	at := time.Unix(until, 0).UTC().Format(time.RFC3339)
	return fmt.Sprintf("The reply's author is paused on %s until %s: %d or more of its results on it (from its account or keys linked to it) were rejected in the last %d days, so the board cannot claim this reply for it until %d hours after the latest rejection. Your account is not paused. The reply stays as it is: send the same accept after %s.",
		what, at, WorkClaimCooldownRejects, WorkClaimCooldownWindow/86400, WorkClaimCooldown/3600, at)
}

// refuseClaimCooldown is work.claim's 403 not_eligible while the cool-down
// holds. forReply is the claim an accept of a direct reply runs for the
// reply's author: the same code, worded for the requester.
func (s *Store) refuseClaimCooldown(ctx context.Context, q workQuerier, w workRow, standing bool, account string, now int64, forReply bool) error {
	until, err := s.workClaimCooldownUntil(ctx, q, nil, w, standing, account, now)
	if err != nil || until == 0 {
		return err
	}
	if forReply {
		return problem(403, "not_eligible", workClaimCooldownReplyText(standing, until))
	}
	return problem(403, "not_eligible", workClaimCooldownText(standing, until))
}
