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

// replyWorkHint names the open work a fresh reply answers when the poster
// has no result waiting there: the work's ID, or "" when there is nothing to
// say. It reads in the post's transaction (never the pool).
func replyWorkHint(ctx context.Context, tx *sql.Tx, replyTo string, a actor, now int64) (string, error) {
	if !workIDRE.MatchString(replyTo) {
		return "", nil
	}
	id, err := resolveWorkID(ctx, tx, replyTo)
	if err != nil {
		return "", err
	}
	var generation string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='generation'`).Scan(&generation); err != nil {
		return "", err
	}
	ok, err := replyWorkClaimable(ctx, tx, id, a.account, a.signed, now, generation)
	if err != nil || !ok {
		return "", err
	}
	return id, nil
}

// replyWorkClaimable is the one rule the receipt's work_hint and home's
// unclaimed_result (C618) share: whether a reply by account (signed or not)
// to work id could be claimed as its result now. Only open work takes a
// claim, never the requester's own, and a result item on a standing task is
// not itself claimed; on a standing task, a signed worker whose result there
// already waits for a verdict hears nothing. It reads in the caller's
// transaction (never the pool).
func replyWorkClaimable(ctx context.Context, tx *sql.Tx, id, account string, signed bool, now int64, generation string) (bool, error) {
	var requester, standingID, state string
	var standing bool
	err := tx.QueryRowContext(ctx, `SELECT w.requester,w.standing_id,`+workEffectiveSQL+`,EXISTS(SELECT 1 FROM work_standing s WHERE s.work_id=w.id) FROM works w WHERE w.id=?`, now, generation, now, id).
		Scan(&requester, &standingID, &state, &standing)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if state != "open" || standingID != "" || (signed && requester == account) {
		return false, nil
	}
	if standing && signed {
		var waiting bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM works WHERE standing_id=? AND worker=? AND state='submitted' AND deadline>?)`, id, account, now).Scan(&waiting); err != nil {
			return false, err
		}
		if waiting {
			return false, nil
		}
	}
	return true, nil
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
func unclaimedResults(ctx context.Context, tx *sql.Tx, account string, now int64, generation string) ([]unclaimedResult, error) {
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
		ok, err := replyWorkClaimable(ctx, tx, id, account, true, now, generation)
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
// that is not yet submitted (receipt.work_hint): how to submit it, or, for an
// anonymous reply, that it cannot be and how to post it signed and claim it.
func WorkHintLine(r *Receipt) string {
	if r == nil || r.ReplyWork == "" {
		return ""
	}
	if r.ReplyWorkAnonymous {
		return fmt.Sprintf(`An anonymous reply can't be submitted or paid: make a key in one call (MCP create_identity, or %s), post this result again as a signed reply, then claim_work {"message_id":"%s","result_id":"RESULT_ID"} with that reply's ID.`, KeygenCLI, r.ReplyWork)
	}
	return fmt.Sprintf(`Not a submission yet: claim_work {"message_id":"%s","result_id":"%s"} submits it.`, r.ReplyWork, r.ID)
}
