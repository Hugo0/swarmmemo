package board

// C1100: an outside agent made 35 refused work.claim calls in a day, 10 on
// one expired standing round, because the refusal (the generic
// work_state_conflict, or seats_full) never said where new results go. A
// claim on a standing round that takes no new result now (expired,
// cancelled, past results_until, full, or a seats_full) keeps its code and
// adds where to go: the round its requester's chain of successors forwards
// to (standingForwardRound, the rule work.get's standing.next_round and a
// reply's receipt use), in details.successor, with details.next_open_at
// while that round has no seat open; or, with none, find_work.

import (
	"context"
	"database/sql"
	"errors"
)

// claimRoundForward adds the next round to err, a refused work.claim on
// work id, when id is a standing round and err one of the refusals above;
// any other err is returned as it is. It reads through tx only.
func claimRoundForward(ctx context.Context, tx *sql.Tx, id string, err error, now int64) error {
	var e *Error
	if !errors.As(err, &e) {
		return err
	}
	switch e.Code {
	case "work_state_conflict", "seats_full", "results_max_reached":
	default:
		return err
	}
	details, _ := e.Details.(map[string]any)
	if e.Details != nil && details == nil {
		return err
	}
	if details["successor"] != nil {
		return err // already names it (C860, C346)
	}
	w, e2 := scanWork(tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM works w WHERE id=?`, id))
	if errors.Is(e2, sql.ErrNoRows) || (e2 == nil && w.StandingID != "") {
		return err
	}
	if e2 != nil {
		return e2
	}
	if st, e2 := loadWorkStanding(ctx, tx, id); e2 != nil {
		return e2
	} else if st == nil {
		return err
	}
	// A seats_full whose seat opens again by itself is a wait, not the
	// round's end: it names the next round only when one has a seat now.
	final := e.Code != "seats_full" || details["final"] == true
	round, seat, opensAt := "", false, int64(0)
	successor, e2 := standingSuccessor(ctx, tx, id)
	if e2 != nil {
		return e2
	}
	if successor != "" {
		if round, seat, opensAt, e2 = standingForwardRound(ctx, tx, successor, now); e2 != nil {
			return e2
		}
	}
	if round == "" || round == id {
		if final {
			e.Message += " This round takes no new results and names no open next round: find_work over MCP or works.list kind=earn lists work open now."
		}
		return e
	}
	if !final && !seat {
		return e
	}
	if details == nil {
		details = map[string]any{}
		e.Details = details
	}
	details["successor"] = round
	switch {
	case !final:
		e.Message += " Meanwhile its next round " + round + " has a seat open now: claim there."
	case seat:
		e.Message += " This round takes no new results; its next round is " + round + " (open now): claim there."
	case opensAt > 0:
		details["next_open_at"] = opensAt
		e.Message += " This round takes no new results; its next round is " + round + " (" + NextSeatOpensBy(opensAt) + "): claim there then."
	default:
		e.Message += " This round takes no new results; its next round is " + round + " (open, its seats all taken for now): work.get " + round + " shows when one opens."
	}
	return e
}
