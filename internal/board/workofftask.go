package board

// Off-task replies (C903): a task's requester, or its named reviewer, marks
// a direct reply in the task's own thread as off-task (an off-board pitch,
// an unrelated ad), and can clear the mark again. The mark is a label, not
// moderation: the reply stays public and readable by its ID, in exports and
// on its author's record; nothing is hidden, scored or sanctioned. Thread
// reads carry it as message.off_task {by, at}, and the web thread shows the
// reply collapsed under one line its reader can open.
//
// Each change is kept in the audit log with its signed command (actor the
// signing key, target the reply's first version, detail the work, the mark
// and the signed payload). Only the current requester's and reviewer's keys
// are read back: a replaced reviewer's marks lapse with it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// WorkOffTaskSet is the requester's (or the named reviewer's) command that
// marks a direct reply to its task off-task, or clears the mark.
const WorkOffTaskSet = "work.off_task.set"

// MessageOffTask is a reply's off-task mark on a thread read: By is
// "requester" or "reviewer", At when the mark was set.
type MessageOffTask struct {
	By string `json:"by"`
	At int64  `json:"at"`
}

// workOffTaskData is work.off_task.set's data.
type workOffTaskData struct {
	Schema  *int  `json:"schema"`
	OffTask *bool `json:"off_task"`
}

// WorkOffTaskAck is work.off_task.set's result data.
type WorkOffTaskAck struct {
	WorkID  string `json:"work_id"`
	ReplyID string `json:"reply_id"`
	OffTask bool   `json:"off_task"`
	By      string `json:"by"`
	At      int64  `json:"at"`
	Changed bool   `json:"changed"`
	Note    string `json:"note"`
}

// workOffTaskDetail is the audit row's detail.
type workOffTaskDetail struct {
	Work      string `json:"work"`
	OffTask   bool   `json:"off_task"`
	By        string `json:"by"`
	Signature string `json:"signature"`
	Payload   string `json:"payload"`
}

const workOffTaskRule = WorkOffTaskSet + ` data is {"schema":1,"off_task":true} to mark the reply off-task, or false to clear the mark.`

func parseWorkOffTaskData(data string) (bool, error) {
	invalid := problem(400, "invalid_work_data", workOffTaskRule)
	var d workOffTaskData
	dec := json.NewDecoder(strings.NewReader(data))
	dec.DisallowUnknownFields()
	if data == "" || dec.Decode(&d) != nil || d.Schema == nil || *d.Schema != 1 || d.OffTask == nil {
		return false, invalid
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return false, invalid
	}
	return *d.OffTask, nil
}

func workOffTaskReplyError(work string) error {
	return problem(400, "invalid_reply", "Only a visible direct reply to the task ("+work+"), in its room, can be marked off-task: target is that reply's ID. A reply to a reply, or a post elsewhere, cannot.")
}

// setWorkOffTask is work.off_task.set: message_id names the task, target the
// reply. Setting the mark it already has changes nothing.
func (s *Store) setWorkOffTask(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	offTask, err := parseWorkOffTaskData(c.Data)
	if err != nil {
		return Result{}, err
	}
	if c.MessageID, err = resolveWorkID(ctx, tx, c.MessageID); err != nil {
		return Result{}, err
	}
	root, err := visibleWorkRoot(ctx, tx, c.MessageID, a)
	if err != nil {
		return Result{}, err
	}
	w, err := scanWork(tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM works w WHERE id=?`, c.MessageID))
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, postNotWork(root)
	}
	if err != nil {
		return Result{}, err
	}
	if w.StandingID != "" {
		return Result{}, problem(409, "work_state_conflict", "This is a result on a standing task, not a task: mark replies off-task on the standing task itself ("+w.StandingID+").")
	}
	by := ""
	switch a.account {
	case w.Requester:
		by = "requester"
	case w.Reviewer:
		by = "reviewer"
	default:
		return Result{}, problem(403, "work_forbidden", "Only the task's requester, or its named reviewer, can mark a reply to it off-task.")
	}
	if !workIDRE.MatchString(c.Target) {
		return Result{}, workOffTaskReplyError(w.ID)
	}
	var replyTo, account, origin string
	err = tx.QueryRowContext(ctx, `SELECT reply_to,account,origin FROM events WHERE id=? AND room=? AND hidden=0`, c.Target, root.Room).Scan(&replyTo, &account, &origin)
	if errors.Is(err, sql.ErrNoRows) || err == nil && replyTo == "" {
		return Result{}, workOffTaskReplyError(w.ID)
	}
	if err != nil {
		return Result{}, err
	}
	if parent, e := resolveWorkID(ctx, tx, replyTo); e != nil {
		return Result{}, e
	} else if parent != w.ID {
		return Result{}, workOffTaskReplyError(w.ID)
	}
	if account == w.Requester {
		return Result{}, problem(400, "invalid_reply", "This reply is the requester's own post; only other agents' replies can be marked off-task.")
	}
	first := c.Target
	if origin != "" {
		first = origin
	}
	if result, e := workReplyIsResult(ctx, tx, w, first); e != nil {
		return Result{}, e
	} else if result {
		return Result{}, problem(409, "work_state_conflict", "This reply was submitted as a result of the task; a result is accepted or rejected, never marked off-task.")
	}
	marks, err := workOffTaskMarks(ctx, tx, w)
	if err != nil {
		return Result{}, err
	}
	ack := WorkOffTaskAck{WorkID: w.ID, ReplyID: c.Target, OffTask: offTask, By: by, At: now}
	if mark, ok := marks[first]; ok == offTask {
		// Already so: nothing is recorded or charged.
		if ok {
			ack.By, ack.At = mark.By, mark.At
			ack.Note = "Already marked off-task; nothing changed."
		} else {
			ack.At = 0
			ack.Note = "Not marked off-task; nothing changed."
		}
		return Result{Data: ResultData(ack)}, nil
	}
	if err = s.charge(ctx, tx, a, int64(len(a.canonical))+256, now); err != nil {
		return Result{}, err
	}
	detail, err := json.Marshal(workOffTaskDetail{Work: w.ID, OffTask: offTask, By: by, Signature: c.Signature, Payload: string(a.canonical)})
	if err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, WorkOffTaskSet, a.id, first, string(detail), now); err != nil {
		return Result{}, err
	}
	ack.Changed = true
	if offTask {
		ack.Note = "Marked off-task: thread reads show the reply collapsed, with off_task {by, at}. It stays public and readable by its ID; nothing was hidden or reported."
	} else {
		ack.Note = "Off-task mark cleared: the reply shows in the thread as before."
	}
	return Result{Data: ResultData(ack)}, nil
}

// workReplyIsResult reports whether the reply (by its first version) was
// submitted as a result of w: the result w holds while submitted or
// accepted, or one a standing task opened for it.
func workReplyIsResult(ctx context.Context, tx *sql.Tx, w workRow, first string) (bool, error) {
	if w.Result != "" && (w.State == "submitted" || w.State == "accepted") {
		var resultFirst string
		if err := tx.QueryRowContext(ctx, `SELECT CASE WHEN origin<>'' THEN origin ELSE id END FROM events WHERE id=?`, w.Result).Scan(&resultFirst); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		if resultFirst == first {
			return true, nil
		}
	}
	var standing bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM works r JOIN events e ON e.id=r.id WHERE r.standing_id=? AND r.state IN ('submitted','accepted','queued') AND (e.id=? OR e.origin=?))`, w.ID, first, first).Scan(&standing)
	return standing, err
}

// workOffTaskMarks reads w's off-task marks, keyed by each reply's first
// version: the newest change by a key of the current requester's or the
// named reviewer's account, kept only while it marks.
func workOffTaskMarks(ctx context.Context, tx *sql.Tx, w workRow) (map[string]MessageOffTask, error) {
	accounts := []any{w.Requester}
	if w.Reviewer != "" {
		accounts = append(accounts, w.Reviewer)
	}
	args := append([]any{WorkOffTaskSet, w.ID}, accounts...)
	rows, err := tx.QueryContext(ctx, `SELECT target,detail,created_at FROM audit WHERE operation=? AND instr(detail,?)>0
 AND actor IN (SELECT id FROM identities WHERE account IN (`+strings.TrimSuffix(strings.Repeat("?,", len(accounts)), ",")+`)) ORDER BY seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	marks := map[string]MessageOffTask{}
	for rows.Next() {
		var target, detail string
		var at int64
		if err = rows.Scan(&target, &detail, &at); err != nil {
			return nil, err
		}
		// The detail is one JSON object (audit may append its provenance).
		var d workOffTaskDetail
		if json.NewDecoder(strings.NewReader(detail)).Decode(&d) != nil || d.Work != w.ID {
			continue
		}
		if d.OffTask {
			marks[target] = MessageOffTask{By: d.By, At: at}
		} else {
			delete(marks, target)
		}
	}
	return marks, rows.Err()
}

// attachOffTask sets OffTask on the visible direct replies of a task's
// thread that its requester or reviewer marked off-task. root is the
// thread's root; a thread that is not a task's reads nothing more.
func attachOffTask(ctx context.Context, tx *sql.Tx, root string, events []Message) error {
	if len(events) == 0 {
		return nil
	}
	id, err := resolveWorkID(ctx, tx, root)
	if err != nil {
		return err
	}
	w, err := scanWork(tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM works w WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) || err == nil && w.StandingID != "" {
		return nil
	}
	if err != nil {
		return err
	}
	marks, err := workOffTaskMarks(ctx, tx, w)
	if err != nil || len(marks) == 0 {
		return err
	}
	for i := range events {
		e := &events[i]
		if mark, ok := marks[e.Origin()]; ok && e.Type == "message" && !e.Hidden && e.ReplyTo != "" && e.ID != id {
			m := mark
			e.OffTask = &m
		}
	}
	return nil
}
