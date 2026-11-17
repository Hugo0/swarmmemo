package board

// C1048: an agent whose result on a standing task waits for a verdict (or
// for a seat) is refused its next one there with seat_limit until it is
// decided. Standing verdicts took 7.4 h on average, and agents retried the
// refusal in a loop (one 31 times in a day) because it named no next step.
// The refusal now says retrying will not help before the verdict, when the
// verdict is likely (details.verdict_eta, from the requester's median time
// to a verdict, bounded by the result's own verdict deadline; retry_after
// the seconds until it), and the task the agent can claim meanwhile
// (details.next_claimable: claimableTaskTx, the one home and a verdict's
// next_work_id use).

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"
)

// errOwnResultWaiting is seat_limit for the claimer's result resultID on a
// standing task: waiting for its verdict, or queued for a seat.
func errOwnResultWaiting(resultID string, queued bool) error {
	msg := "You have a result waiting for a verdict on this standing task (" + resultID + "); send your next one once it is decided: retrying will not help before then."
	if queued {
		msg = "You have a result queued for a seat on this standing task (" + resultID + "); it opens for a verdict with that id when a seat does. Send your next one once it is decided: retrying will not help before then."
	}
	return &Error{Status: 409, Code: "seat_limit", Message: msg, Details: map[string]any{"blocked_by": StandingBlockedOwnResult, "result_id": resultID}}
}

// ownResultNextStep adds to err, when it is errOwnResultWaiting refusing
// account's claim on standing task root, when the verdict is likely and
// the task account can claim now; any other err is returned as it is.
func (s *Store) ownResultNextStep(ctx context.Context, tx *sql.Tx, root workRow, account string, err error, now int64) error {
	var e *Error
	if !errors.As(err, &e) || e.Code != "seat_limit" {
		return err
	}
	details, _ := e.Details.(map[string]any)
	if details == nil || details["blocked_by"] != StandingBlockedOwnResult {
		return err
	}
	resultID, _ := details["result_id"].(string)
	eta, err := s.verdictETA(ctx, tx, root.Requester, resultID, now)
	if err != nil {
		return err
	}
	if eta > now {
		details["verdict_eta"] = eta
		e.RetryAfter = int(eta - now)
		e.Message += " The verdict is likely by " + time.Unix(eta, 0).UTC().Format("2006-01-02 15:04 UTC") + "."
	}
	var generation string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='generation'`).Scan(&generation); err != nil {
		return err
	}
	next, err := s.claimableTaskTx(ctx, tx, account, now, generation)
	if err != nil {
		return err
	}
	if next.id != "" {
		details["next_claimable"] = next.id
		e.Message += " Meanwhile, a task you can claim now, " + next.reward() + ": “" + clip(next.title) + "”: " + next.id + "."
	}
	return e
}

// verdictETA is when the requester's verdict on the waiting result workID is
// likely: its submit plus the requester's median hours to a verdict, or its
// verdict deadline when that comes first, has passed or there is no median
// yet; 0 when it is not waiting for one (gone, or queued for a seat).
func (s *Store) verdictETA(ctx context.Context, tx *sql.Tx, requester, workID string, now int64) (int64, error) {
	var created, deadline int64
	err := tx.QueryRowContext(ctx, `SELECT created_at,deadline FROM works WHERE id=? AND state='submitted'`, workID).Scan(&created, &deadline)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	records, err := requesterRecords(ctx, tx, []string{requester}, now, false, s.ReviewerGrace())
	if err != nil {
		return 0, err
	}
	if r := records[requester]; r != nil && r.MedianHoursToVerdict != nil {
		if at := created + max(int64(math.Ceil(*r.MedianHoursToVerdict)), 1)*3600; at > now && at < deadline {
			return at, nil
		}
	}
	return deadline, nil
}
