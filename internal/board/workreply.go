package board

// Accepting a direct reply (C691). Some agents post a task's result as a
// plain reply and never send work.claim or work.submit, so the requester
// could not accept or pay it. work.accept with target, the reply's ID, does
// it in one signed step: in the accept's transaction the board runs the
// claim (and the submit) for the reply's author through the same
// transition code a signed claim runs (workStep), with every check it makes
// (eligibility, claim pause, reviewer, seats and per-agent limits,
// duplicate_result), then the requester's accept as usual.
//
// The record is the requester's signed accept alone: the board never signs
// for the worker, so no claim or submit transition is written in its name.
// The accept names no fence (amount): target names the attempt it accepts,
// and the transition carries the fence the claim gave it.
//
// Money is the accept's: credits to the reply's author. USDC only to the
// author's payout address on record (a signed one from its own claim, or its
// verified wallet link); without one no address is invented, the USDC
// promise is void (reason no_payout_address) and the result is paid in
// credits only, which the acknowledgement says. work.promise can owe it
// later, once the author links a wallet.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"swarmmemo/internal/services"
)

// WorkUSDCNoPayoutAddress is the void reason of a USDC reward an accepted
// direct reply could not be owed: its author has no payout address.
const WorkUSDCNoPayoutAddress = "no_payout_address"

// WorkAcceptReply is how the docs and hints name the step.
const WorkAcceptReply = "work.accept with target, a direct reply's ID, accepts that reply as the result: the board claims and submits it for its author in the same step, with the checks a claim makes; no fence (amount)."

// replyAuthor is the author of a reply an accept names.
type replyAuthor struct {
	id, account, publicKey string
}

func invalidReplyResult(to, room string) error {
	return problem(400, "invalid_work_result", "target must be a visible signed direct reply to the work's request ("+to+"), in its room ("+room+"), by another agent: that reply is the result you accept.")
}

// acceptReply is work.accept with target (C691): see the file comment.
func (s *Store) acceptReply(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	d, err := parseWorkData(c.Data, c.Operation)
	if err != nil {
		return Result{}, err
	}
	if c.Amount != 0 {
		return Result{}, problem(409, "work_fence_mismatch", "An accept that names a reply (target) takes no fence: leave amount out; the reply is the attempt it accepts. To accept a result already submitted, send amount, its fence, without target.")
	}
	id, err := resolveWorkID(ctx, tx, c.MessageID)
	if err != nil {
		return Result{}, err
	}
	root, err := visibleWorkRoot(ctx, tx, id, a)
	if err != nil {
		return Result{}, err
	}
	w, err := scanWork(tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM works w WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, workNotFound()
	}
	if err != nil {
		return Result{}, err
	}
	if w.StandingID != "" {
		return Result{}, problem(400, "invalid_work_result", "target names a reply to the task itself: send this accept to the standing task ("+w.StandingID+"), or accept this result without target, with its fence.")
	}
	// The decider, as for any accept: the named reviewer, else the requester.
	if w.Reviewer != "" && a.account != w.Reviewer {
		return Result{}, reviewerError("not_the_reviewer")
	}
	if w.Reviewer == "" && a.account != w.Requester {
		return Result{}, errRequesterOnly()
	}
	var generation string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='generation'`).Scan(&generation); err != nil {
		return Result{}, err
	}
	if d.Generation != generation {
		return Result{}, errWorkGenerationStale()
	}
	author, err := directReplyAuthor(ctx, tx, c.Target, w, root)
	if err != nil {
		return Result{}, err
	}
	standing, err := loadWorkStanding(ctx, tx, w.ID)
	if err != nil {
		return Result{}, err
	}
	state, err := effectiveWork(ctx, tx, w.ID, generation, now)
	if err != nil {
		return Result{}, err
	}
	worker := actor{id: author.id, account: author.account, publicKey: author.publicKey, signed: true}
	step := func(op string, fence int64) (WorkAck, error) {
		worker.operation = op
		r, e := s.workStep(ctx, tx, Command{Operation: op, MessageID: w.ID, Target: c.Target, Amount: fence, Data: workGenerationData(generation)}, worker, now, true)
		if e != nil {
			return WorkAck{}, e
		}
		return r.Data["ack"].(WorkAck), nil
	}
	var item WorkAck
	how := "the board claimed and submitted it for its author in this step"
	switch {
	case standing != nil || state == "open":
		item, err = step("work.claim", 0)
	case state == "claimed" && w.Worker == author.account:
		// The author claimed and replied, but never submitted.
		item, err = step("work.submit", w.Fence)
		how = "the board submitted it for its author, who held the claim, in this step"
	case state == "submitted" && w.Worker == author.account && sameResult(ctx, tx, c.Target, w.Result):
		// Already submitted: the accept is the ordinary one.
		item, how = WorkAck{WorkID: w.ID, State: w.State, Fence: w.Fence}, "it was already submitted"
	case state == "claimed" || state == "submitted":
		return Result{}, problem(409, "work_state_conflict", "This work is held by another attempt ("+state+"): accept or reject that one; a reply is accepted while the work is open, or by the claim holder's own reply.")
	default:
		return Result{}, errWorkStateNotAllowed()
	}
	if err != nil {
		return Result{}, err
	}
	// The requester's own signed accept, as sent: the fence is the one the
	// claim gave the result, and its transition is the step's record.
	accept := c
	accept.Amount = item.Fence
	if item.WorkID != w.ID {
		accept.MessageID = item.WorkID
	}
	res, err := s.workStep(ctx, tx, accept, a, now, false)
	if err != nil {
		return Result{}, err
	}
	ack := res.Data["ack"].(WorkAck)
	who := author.id
	if identity, e := currentWorkIdentity(ctx, tx, author.account); e == nil && identity.Handle != "" {
		who = "@" + identity.Handle
	}
	note := "Accepted reply " + c.Target + " by " + who + " as the result; " + how + "."
	u, err := loadWorkUSDC(ctx, tx, item.WorkID)
	if err != nil {
		return Result{}, err
	}
	if u != nil && u.State == "void" && u.Reason == WorkUSDCNoPayoutAddress {
		note += fmt.Sprintf(" Paid in credits only: the author has no payout address (it signed none and links no wallet), so the %s USDC is not owed; once it links one, work.promise can owe it.", services.FormatUSDC(u.Amount))
	}
	ack.Note = joinNote(note, ack.Note)
	res.Data["ack"] = ack
	return res, nil
}

// directReplyAuthor admits a reply an accept names: visible, signed, a
// direct reply to the work's request (or an edited version of it), in its
// room, by an agent other than the requester.
func directReplyAuthor(ctx context.Context, tx *sql.Tx, target string, w workRow, root workRoot) (replyAuthor, error) {
	var r replyAuthor
	if !workIDRE.MatchString(target) {
		return r, invalidReplyResult(w.ID, root.Room)
	}
	var room, replyTo, signature string
	err := tx.QueryRowContext(ctx, `SELECT author,account,public_key,signature,room,reply_to FROM events WHERE id=? AND hidden=0`, target).Scan(&r.id, &r.account, &r.publicKey, &signature, &room, &replyTo)
	if errors.Is(err, sql.ErrNoRows) {
		return r, invalidReplyResult(w.ID, root.Room)
	}
	if err != nil {
		return r, err
	}
	if r.publicKey == "" || signature == "" || r.account == "" || room != root.Room || replyTo == "" {
		return r, invalidReplyResult(w.ID, root.Room)
	}
	if replyTo != w.ID {
		if replyTo, err = resolveWorkID(ctx, tx, replyTo); err != nil {
			return r, err
		}
		if replyTo != w.ID {
			return r, invalidReplyResult(w.ID, root.Room)
		}
	}
	if r.account == w.Requester {
		return r, problem(403, "work_forbidden", "That reply is the requester's own: a result comes from another agent.")
	}
	return r, nil
}

// sameResult says whether reply is the submitted result, or an earlier
// version of it.
func sameResult(ctx context.Context, tx *sql.Tx, reply, result string) bool {
	if reply == result {
		return true
	}
	newest, _, err := resultVersion(ctx, tx, reply)
	return err == nil && newest == result
}

// workGenerationData is the minimal work data: schema and generation.
func workGenerationData(generation string) string {
	return `{"schema":1,"generation":"` + generation + `"}`
}

func joinNote(a, b string) string {
	if b == "" {
		return a
	}
	return a + " " + b
}

// workResultsFromSQL joins results re to their work rw from the result's
// side, for a read narrowed to one author's events (events_author): the
// result's work is a standing result's own (its first version's ID), else
// the request it replies to (or that request's root, when it names an
// edited version), found by the primary key, never a scan.
const workResultsFromSQL = `events re INDEXED BY events_author JOIN works rw ON rw.id IN (re.id,re.origin,re.reply_to,(SELECT o.origin FROM events o WHERE o.id=re.reply_to)) AND rw.result_id=re.id`

// workerTransitionsSQL is work_transitions as a worker's side reads it: the
// signed transitions, plus, for each result accepted as a direct reply
// (C691), the attempt its author never signed (C692). That row stands where
// the author's own submit would: its key (the reply's), the claim's fence,
// state submitted, the accept's time, and sequence one below the accept, so
// a read that finds a submit's verdict at sequence+1 finds the accept. Its
// operation is work.claim when the board claimed the reply too, and
// work.submit when the author held a signed claim; it carries no signature
// (the accept's payload is the signed record). An accept of a result its
// author submitted itself adds nothing.
//
// Every worker count reads it (the record's counts.work and accepted
// proofs, works.list worker, reputation, the funnel's acts, the account
// export), so a reply-accepted result counts for its author exactly like a
// claimed and accepted one. account, an SQL expression (a parameter), names
// the one account a read is for: its replies are found by events_author and
// their work by the primary key, never a scan; "" is every account's.
func workerTransitionsSQL(account string) string {
	from := `works rw JOIN events re ON re.id=rw.result_id`
	if account != "" {
		from = workResultsFromSQL + ` AND re.account=` + account
	}
	return `(SELECT work_id,sequence,operation,author,public_key,signature,payload,accepted_at,fence,generation,state,delegation_id FROM work_transitions
 UNION ALL
 SELECT a.work_id,a.sequence-1,
  CASE WHEN EXISTS(SELECT 1 FROM work_transitions c WHERE c.work_id=a.work_id AND c.fence=a.fence AND c.operation='work.claim') THEN 'work.submit' ELSE 'work.claim' END,
  re.author,re.public_key,'',a.payload,a.accepted_at,a.fence,a.generation,'submitted',''
 FROM ` + from + ` JOIN work_transitions a ON a.work_id=rw.id AND a.operation='work.accept'
 WHERE rw.state='accepted' AND coalesce(json_extract(a.payload,'$.command.target'),'')<>''
 AND NOT EXISTS(SELECT 1 FROM work_transitions p WHERE p.work_id=a.work_id AND p.fence=a.fence AND p.state='submitted'))`
}
