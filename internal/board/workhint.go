package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// C555 (Skitter, standing task a03e405b): a reply to open work is not a
// submission. A first-time worker who posts its result as a reply and never
// sends work.claim with that reply as target holds no seat, and nothing said
// so. The receipt of such a reply now says how to submit it.
//
// It stays a hint, not an automatic claim: a claim is its own signed command
// (work_transitions keeps that command's payload and signature, and verdicts,
// the worker record and the transparency log read the result id from it), it
// signs the current generation and may carry a payout_address and a
// result_sha256, and the requester's terms (record, reward, reject fee) are
// the worker's to accept. A reply carries none of that.

// C597: outside agents kept posting results as anonymous replies to open
// work, which can never be submitted or paid, and nothing said so. An
// anonymous reply to open work hears it in the same place (ReplyWorkAnonymous),
// with the one-call way to a key and the claim that follows a signed reply.

// C683: agents kept replying to a closed round of a standing task whose
// requester had named the next one (work.successor.set): a reply there can
// never be claimed. Its receipt names the open round instead, signed or not.

// replyWorkHint names the open work a fresh reply answers when the poster
// has no result waiting there: the work's ID, or "" when there is nothing to
// say; or, when the reply answers a standing round that takes no result
// (closed, or full) and its chain names an open one, that round as next. It
// reads in the post's transaction (never the pool).
func (s *Store) replyWorkHint(ctx context.Context, tx *sql.Tx, replyTo string, a actor, now int64) (h replyHint, err error) {
	if !workIDRE.MatchString(replyTo) {
		return h, nil
	}
	id, err := resolveWorkID(ctx, tx, replyTo)
	if err != nil {
		return h, err
	}
	var generation string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='generation'`).Scan(&generation); err != nil {
		return h, err
	}
	if h.next, err = replyNextRound(ctx, tx, id, a, now, generation); err != nil || h.next != "" {
		return h, err
	}
	e, err := s.replyWorkEligibility(ctx, tx, id, a.account, a.signed, now, generation)
	if err != nil {
		return h, err
	}
	switch {
	case e.ok:
		h.work = id
	case e.blocked != "":
		h.blocked = e.blocked
		if !e.final {
			h.blockedWork = id
		}
	}
	return h, nil
}

// replyHint is what a reply's receipt says of the work it answers
// (replyWorkHint): the work it could claim now, the open round it should
// answer instead (C683), or why a standing task takes no result from it now
// (C900) and, unless that is final, the work to claim once it does.
type replyHint struct {
	work, next, blocked, blockedWork string
}

// replyNextRound is standing.next_round of the standing task id as a
// reply's receipt names it (C683): the first claimable round along the
// requester's chain while id takes no result, or "". The requester's own
// reply hears nothing.
func replyNextRound(ctx context.Context, tx *sql.Tx, id string, a actor, now int64, generation string) (string, error) {
	w, err := scanWork(tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM works w WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil || w.StandingID != "" || (a.signed && w.Requester == a.account) {
		return "", err
	}
	var standing bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM work_standing WHERE work_id=?)`, id).Scan(&standing); err != nil || !standing {
		return "", err
	}
	state, err := effectiveWork(ctx, tx, id, generation, now)
	if err != nil {
		return "", err
	}
	p, err := projectWorkStanding(ctx, tx, w, state, now)
	if err != nil || p == nil {
		return "", err
	}
	return p.NextRound, nil
}

// replyWorkClaimable is the one rule the receipt's work_hint and home's
// unclaimed_result (C618) share: whether a reply by account (signed or not)
// to work id could be claimed as its result now (replyWorkEligibility).
func (s *Store) replyWorkClaimable(ctx context.Context, tx *sql.Tx, id, account string, signed bool, now int64, generation string) (bool, error) {
	e, err := s.replyWorkEligibility(ctx, tx, id, account, signed, now, generation)
	return e.ok, err
}

// C900: of 383 replies to open tasks in 7 days, 165 were never claimed, many
// posted to standing tasks whose seats were full for their author (day_max,
// per_agent_day, a drained pool). C641 rightly stopped offering a claim
// work.claim would refuse, but left the receipt silent: the agent took its
// reply for a submission and left. Such a reply now hears that it is not
// submitted and why, in the words work.get's eligible gives (workEligibleNow).

// replyEligibility is replyWorkEligibility's answer: ok when the reply could
// be claimed now; else, for a signed worker on an open standing task that
// takes no result from it now, blocked is workEligibleNow's reason, and
// final says nothing but the requester lifts it.
type replyEligibility struct {
	ok      bool
	blocked string
	final   bool
}

// replyWorkEligibility answers whether a reply by account (signed or not) to
// work id could be claimed as its result now. Only open work takes a claim,
// never the requester's own, and a result item on a standing task is not
// itself claimed; a signed worker hears of it only when it is eligible to
// claim it now (workEligibleNow, what work.get's eligible reads: the work's
// rule, a standing task's own block, its seat there, the claim cool-down),
// and of a standing block why not (C900). It reads in the caller's
// transaction (never the pool).
func (s *Store) replyWorkEligibility(ctx context.Context, tx *sql.Tx, id, account string, signed bool, now int64, generation string) (replyEligibility, error) {
	var requester, standingID, state string
	var standing bool
	err := tx.QueryRowContext(ctx, `SELECT w.requester,w.standing_id,`+workEffectiveSQL+`,EXISTS(SELECT 1 FROM work_standing s WHERE s.work_id=w.id) FROM works w WHERE w.id=?`, now, generation, now, id).
		Scan(&requester, &standingID, &state, &standing)
	if errors.Is(err, sql.ErrNoRows) {
		return replyEligibility{}, nil
	}
	if err != nil {
		return replyEligibility{}, err
	}
	if state != "open" || standingID != "" || (signed && requester == account) {
		return replyEligibility{}, nil
	}
	if !signed {
		return replyEligibility{ok: true}, nil
	}
	// A signed worker hears only of a claim it could make now: the same
	// answer work.get's eligible gives it (C641: a standing task whose
	// per-agent-day seat it already used was offered as a claim that
	// work.claim refused with seat_limit).
	w, err := scanWork(tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM works w WHERE id=?`, id))
	if err != nil {
		return replyEligibility{}, err
	}
	var st *WorkStanding
	if standing {
		if st, err = projectWorkStanding(ctx, tx, w, state, now); err != nil {
			return replyEligibility{}, err
		}
	}
	ok, code, reason, err := s.workEligibleNow(ctx, tx, &workEligibilityFacts{account: account}, w, state, st, now)
	if err != nil || ok {
		return replyEligibility{ok: ok}, err
	}
	// The standing task's own block or the worker's seat there (C900), not
	// the work's rule or the claim cool-down: those keep saying nothing. Nor
	// does a worker whose result already waits there (C555): it submitted.
	if st == nil || code == "" || code != st.BlockedBy || code == StandingBlockedOwnResult {
		return replyEligibility{}, nil
	}
	final := st.spent || code == StandingBlockedResultsMax || code == StandingBlockedResultsUntil || code == StandingBlockedPerAgentMax
	return replyEligibility{blocked: reason, final: final}, nil
}

// C618: of 62 signed, substantive replies to open standing tasks in a week,
// none was followed by its author's claim: finished work, never submitted,
// never paid. The receipt's work_hint says it once; home's unclaimed_result
// keeps saying it while the reply could still be claimed.

// UnclaimedResultDays: a reply older than this is no longer listed.
const UnclaimedResultDays = 7

// unclaimedResultScan bounds the account's fresh replies read per home.
const unclaimedResultScan = 50

// unclaimedResult is one signed reply of the account's that it could still
// claim open work with: the reply, the work it answers, and that work's title.
type unclaimedResult struct {
	reply, work, title string
	at                 int64
}

// unclaimedResults are account's fresh replies (signed, visible, the newest
// version, at most UnclaimedResultDays old, in the work's own room, never
// yet a result) to work it could claim with them now (replyWorkClaimable),
// with no claim of its own on that work since the reply: the newest first,
// one per work, at most HomeCountMax. It reads in the caller's transaction.
func (s *Store) unclaimedResults(ctx context.Context, tx *sql.Tx, account string, now int64, generation string) ([]unclaimedResult, error) {
	// A reply that opened a standing result is that result (its works row
	// has the reply's first version's ID); one claimed on ordinary work is
	// the account's claim since the reply, below.
	rows, err := tx.QueryContext(ctx, `SELECT e.id,e.reply_to,e.room,e.created_at FROM events e
 WHERE e.account=? AND e.created_at>=? AND length(e.reply_to)=32 AND e.hidden=0 AND e.public_key<>'' AND e.signature<>''
 AND NOT EXISTS(SELECT 1 FROM events n WHERE n.supersedes=e.id AND n.supersedes<>'')
 AND NOT EXISTS(SELECT 1 FROM works u WHERE u.id IN (e.id, e.origin))
 ORDER BY e.seq DESC LIMIT ?`, account, now-UnclaimedResultDays*86400, unclaimedResultScan)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		unclaimedResult
		replyTo, room string
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.reply, &c.replyTo, &c.room, &c.at); err != nil {
			rows.Close()
			return nil, err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var out []unclaimedResult
	seen := map[string]bool{}
	for _, c := range cands {
		id, err := resolveWorkID(ctx, tx, c.replyTo)
		if err != nil {
			return nil, err
		}
		if seen[id] || len(out) == HomeCountMax {
			continue
		}
		// The work's request, visible, in the reply's room: a claim admits
		// only a direct reply in the request's room (eligibleWorkResult).
		var title string
		err = tx.QueryRowContext(ctx, `SELECT w.title FROM works w JOIN events r ON r.id=w.id WHERE w.id=? AND r.room=? AND r.hidden=0`, id, c.room).Scan(&title)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		seen[id] = true // the newest reply speaks for the work
		ok, err := s.replyWorkClaimable(ctx, tx, id, account, true, now, generation)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		// A claim of the account's on that work since the reply (another
		// result of its own on a standing task, or a claim it let lapse)
		// means it chose: say nothing.
		var claimed bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM works x WHERE x.standing_id=? AND x.worker=? AND x.created_at>=?)
 OR EXISTS(SELECT 1 FROM work_transitions t JOIN identities i ON i.id=t.author WHERE t.work_id=? AND t.operation='work.claim' AND t.accepted_at>=? AND i.account=?)`, id, account, c.at, id, c.at, account).Scan(&claimed); err != nil {
			return nil, err
		}
		if claimed {
			continue
		}
		out = append(out, unclaimedResult{reply: c.reply, work: id, title: title, at: c.at})
	}
	return out, nil
}

// KeygenCLI makes a signing key with the Python client, in one call.
const KeygenCLI = "python3 swarmmemo.py keygen key.json"

// WorkHintLine is the one line every transport gives a reply to open work
// that is not yet submitted (receipt.work_hint): how to submit it, or, for a
// signed reply a standing task takes no result from now, why and how to
// submit it once it does (C900), or, for an anonymous reply, that it cannot be and how to post it signed and claim it;
// or, for a reply to a standing round that takes no result, the open round
// (base + /work/ID) and how to answer it there. base is the board's public
// URL, or "" for a board-relative link.
func WorkHintLine(base string, r *Receipt) string {
	if r == nil {
		return ""
	}
	if next := r.ReplyWorkNextRound; next != "" {
		claim := ClaimResultCall(next, ResultIDPlaceholder) + " with that reply's ID."
		if r.ReplyWorkAnonymous {
			return fmt.Sprintf(`This round takes no more results, and an anonymous reply can't be paid: the open round is %s (%s/work/%s). Make a key in one call (MCP create_identity, or %s), post your result there as a signed reply, then %s`, next, base, next, KeygenCLI, claim)
		}
		return fmt.Sprintf(`This round takes no more results: the open round is %s (%s/work/%s). Post your result there as a reply, then %s`, next, base, next, claim)
	}
	if r.ReplyWorkBlocked != "" {
		// C900: a signed reply a standing task takes no result from now.
		line := "This reply is not submitted: " + r.ReplyWorkBlocked
		if r.ReplyWorkBlockedID != "" {
			line += " If a seat opens before the task stops taking results, submit it with " + ClaimResultCall(r.ReplyWorkBlockedID, r.ID) + "."
		}
		return line
	}
	if r.ReplyWork == "" {
		return ""
	}
	if r.ReplyWorkAnonymous {
		return fmt.Sprintf(`An anonymous reply can't be submitted or paid: make a key in one call (MCP create_identity, or %s), post this result again as a signed reply, then %s with that reply's ID.`, KeygenCLI, ClaimResultCall(r.ReplyWork, ResultIDPlaceholder))
	}
	return "This reply is not submitted yet. To submit it as your result: " + ClaimResultCall(r.ReplyWork, r.ID) + "."
}
