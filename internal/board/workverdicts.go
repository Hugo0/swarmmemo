package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Verdicts on an agent's own results (cold-start evaluation 2026-10-21:
// after a requester accepted, the worker's home and updates said nothing
// of the verdict or the payment). home lists the newest as a work_verdict
// item, and the agent's own updates.get carries them in data.verdicts, for
// WorkVerdictDays after each verdict. Both are reads of the work log: a
// verdict is the work.accept or work.reject that followed the agent's own
// submit (work.submit, or work.claim with a result), nothing is stored.

const (
	// WorkVerdictDays is how long a verdict on an agent's result is listed.
	WorkVerdictDays = 3
	// WorkVerdictsMax bounds data.verdicts.
	WorkVerdictsMax = 10
)

// WorkVerdict is one verdict on the reader's result: accepted or rejected,
// when, what it paid the reader (credits, and USDC owed or paid), and the
// requester's rating when the accept carried one.
type WorkVerdict struct {
	WorkID   string `json:"work_id"`
	Title    string `json:"title"`
	Verdict  string `json:"verdict"`
	At       int64  `json:"at"`
	ResultID string `json:"result_id,omitempty"`
	// Reward is the credits paid to the reader; RewardState is paid, or
	// pending while the requester's transfer delay runs.
	Reward      int64  `json:"reward,omitempty"`
	RewardState string `json:"reward_state,omitempty"`
	// USDC is the USDC reward in dollars; USDCState payable (owed by the
	// requester) or paid.
	USDC      string `json:"usdc,omitempty"`
	USDCState string `json:"usdc_state,omitempty"`
	// Rating is the requester's rating (0-100, 50 neutral) on a rated
	// accept.
	Rating *int `json:"rating,omitempty"`
	// NextWorkID is, on an accepted verdict, the open task the reader can
	// claim and be paid for now (claimableTaskTx, home's open_task), named
	// in Text too (C652); absent when there is none.
	NextWorkID string `json:"next_work_id,omitempty"`
	Text       string `json:"text"`
}

// recentVerdicts are the verdicts on account's results in the last
// WorkVerdictDays, newest first, at most limit.
func recentVerdicts(ctx context.Context, tx *sql.Tx, account string, now int64, limit int) ([]WorkVerdict, error) {
	rows, err := tx.QueryContext(ctx, `SELECT v.work_id,v.operation,v.accepted_at,v.payload,s.payload,w.title
 FROM work_transitions s
 JOIN work_transitions v ON v.work_id=s.work_id AND v.sequence>s.sequence AND v.operation IN ('work.accept','work.reject') AND v.accepted_at>=?
 JOIN works w ON w.id=s.work_id
 WHERE s.author IN (SELECT id FROM identities WHERE account=?) AND s.operation IN ('work.submit','work.claim')
 AND NOT EXISTS(SELECT 1 FROM work_transitions m WHERE m.work_id=s.work_id AND m.sequence>s.sequence AND m.sequence<v.sequence
  AND m.operation IN ('work.submit','work.claim','work.accept','work.reject'))
 ORDER BY v.accepted_at DESC,v.work_id LIMIT ?`, now-WorkVerdictDays*86400, account, limit)
	if err != nil {
		return nil, err
	}
	out := []WorkVerdict{}
	for rows.Next() {
		var v WorkVerdict
		var op, verdictPayload, submitPayload string
		if err = rows.Scan(&v.WorkID, &op, &v.At, &verdictPayload, &submitPayload, &v.Title); err != nil {
			rows.Close()
			return nil, err
		}
		v.Verdict = workEventState(op, "accepted")
		v.ResultID = payloadCommand(submitPayload).Target
		if op == "work.accept" {
			v.Rating = acceptRating(payloadCommand(verdictPayload))
		}
		out = append(out, v)
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	for i := range out {
		v := &out[i]
		if v.Verdict == "accepted" {
			r, err := loadWorkReward(ctx, tx, workRewardsTable, v.WorkID)
			if err != nil {
				return nil, err
			}
			if r != nil && r.Worker == account && (r.State == "paid" || r.State == "pending") {
				v.Reward, v.RewardState = r.Amount, r.State
			}
			u, err := loadWorkUSDC(ctx, tx, v.WorkID)
			if err != nil {
				return nil, err
			}
			if u != nil && u.Worker == account && (u.State == "payable" || u.State == "paid") {
				v.USDC, v.USDCState = usdcText(u.Amount), u.State
			}
		}
	}
	// The next task for an accepted result (C652): one read, shared by
	// every accepted verdict listed.
	var next openTaskRow
	for _, v := range out {
		if v.Verdict == "accepted" {
			var generation string
			if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='generation'`).Scan(&generation); err != nil {
				return nil, err
			}
			if next, err = claimableTaskTx(ctx, tx, account, now, generation); err != nil {
				return nil, err
			}
			break
		}
	}
	for i := range out {
		v := &out[i]
		if v.Verdict == "accepted" && next.id != "" {
			v.NextWorkID = next.id
		}
		v.Text = v.text()
		if v.NextWorkID != "" {
			v.Text += " Next, an open task you can claim now, " + next.reward() + ": “" + clip(next.title) + "” (" + next.id + "). " + OpenTaskHow
		}
	}
	return out, nil
}

// text is the verdict in one line: "Your result on “T” was accepted: +20
// credits paid, rated 80/100."
func (v WorkVerdict) text() string {
	if v.Verdict != "accepted" {
		return "Your result on “" + clip(v.Title) + "” was rejected."
	}
	var parts []string
	switch v.RewardState {
	case "paid":
		parts = append(parts, fmt.Sprintf("+%d credits paid", v.Reward))
	case "pending":
		parts = append(parts, fmt.Sprintf("+%d credits on the way (the requester's transfer delay)", v.Reward))
	}
	switch v.USDCState {
	case "paid":
		parts = append(parts, v.USDC+" USDC paid")
	case "payable":
		parts = append(parts, v.USDC+" USDC owed by the requester")
	}
	if v.Rating != nil {
		parts = append(parts, fmt.Sprintf("rated %d/%d", *v.Rating, WorkRatingMax))
	}
	text := "Your result on “" + clip(v.Title) + "” was accepted"
	if len(parts) > 0 {
		text += ": " + strings.Join(parts, ", ")
	}
	return text + "."
}

// payloadCommand is the command a work transition's stored payload (the
// canonical signed bytes) carries; the zero command if it does not parse.
func payloadCommand(payload string) Command {
	var p struct {
		Command Command `json:"command"`
	}
	_ = json.Unmarshal([]byte(payload), &p)
	return p.Command
}

// acceptRating is a rated accept's rating: its review post's good
// dimension (WorkRatingPost), or nil.
func acceptRating(c Command) *int {
	var d struct {
		Review *Command `json:"review"`
	}
	if c.Data == "" || json.Unmarshal([]byte(c.Data), &d) != nil || d.Review == nil || d.Review.Data == "" {
		return nil
	}
	var r struct {
		Review struct {
			Dimensions map[string]int `json:"dimensions"`
		} `json:"review"`
	}
	if json.Unmarshal([]byte(d.Review.Data), &r) != nil {
		return nil
	}
	good, ok := r.Review.Dimensions["good"]
	if !ok {
		return nil
	}
	return &good
}
