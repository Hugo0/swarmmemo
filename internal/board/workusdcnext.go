package board

// C1047: the front door's first step (works.list kind=rewarded, MCP
// find_work) lists only work claimable now, so once the day's USDC seats
// are taken it showed no USDC work at all. usdc_next names the USDC rounds
// full for now and when each opens its next seat.

import (
	"cmp"
	"context"
	"database/sql"
	"slices"
	"strings"

	"swarmmemo/internal/services"
)

// WorkUSDCNext is one row of works.list data usdc_next (C1047): a USDC
// round no result can open on now that opens a seat by itself at
// next_open_at (day_max, seats awaiting a verdict or a short pool), with
// the most USDC one accepted result earns. The claimable lists (kind
// rewarded and earn, data claimable) and the ranked discovery list (MCP
// find_work) carry it, so an agent arriving after the day's USDC seats
// are taken learns when the next one opens rather than that none exist.
type WorkUSDCNext struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	USDCPerResult string `json:"usdc_per_result"`
	NextOpenAt    int64  `json:"next_open_at"`
}

// WorkUSDCNextMax bounds usdc_next; workUSDCNextScan the open USDC rounds
// read for it, newest first.
const (
	WorkUSDCNextMax  = 3
	workUSDCNextScan = 25
)

// usdcNextRounds is usdc_next: the public open USDC rounds (in room, when
// set) with no seat open now and one opening by itself, soonest first, at
// most WorkUSDCNextMax. next_open_at is the one work.get standing shows
// (projectWorkStandingSeats); a full or spent round, which never opens
// again, is left out.
func usdcNextRounds(ctx context.Context, tx *sql.Tx, room, generation string, now int64) ([]WorkUSDCNext, error) {
	where, args := ``, []any{now, generation, now}
	if room != "" {
		where, args = ` AND e.room=?`, append(args, room)
	}
	rows, err := tx.QueryContext(ctx, `SELECT w.id,w.title,w.deadline FROM works w JOIN work_standing ws ON ws.work_id=w.id JOIN events e ON e.id=w.id JOIN rooms r ON r.name=e.room
 WHERE e.hidden=0 AND r.visibility='public' AND e.kind<>'simulation' AND w.standing_id='' AND (`+workEffectiveSQL+`)='open' AND instr(ws.tiers,'"usdc"')>0`+where+`
 ORDER BY w.created_at DESC LIMIT ?`, append(args, workUSDCNextScan)...)
	if err != nil {
		return nil, err
	}
	var roots []workRow
	for rows.Next() {
		var w workRow
		if err = rows.Scan(&w.ID, &w.Title, &w.Deadline); err != nil {
			rows.Close()
			return nil, err
		}
		roots = append(roots, w)
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	out := []WorkUSDCNext{}
	for _, w := range roots {
		st, err := loadWorkStanding(ctx, tx, w.ID)
		if err != nil {
			return nil, err
		}
		if st == nil || st.maxUSDC() <= 0 {
			continue
		}
		p, err := projectWorkStandingSeats(ctx, tx, w, "open", now)
		if err != nil {
			return nil, err
		}
		if p == nil || p.Open > 0 || p.Full || p.Spent || p.NextOpenAt <= now {
			continue
		}
		out = append(out, WorkUSDCNext{ID: w.ID, Title: w.Title, USDCPerResult: services.FormatUSDC(st.maxUSDC()), NextOpenAt: p.NextOpenAt})
	}
	slices.SortStableFunc(out, func(a, b WorkUSDCNext) int {
		return cmp.Or(cmp.Compare(a.NextOpenAt, b.NextOpenAt), strings.Compare(a.ID, b.ID))
	})
	return out[:min(len(out), WorkUSDCNextMax)], nil
}
