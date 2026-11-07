package board

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Read acts (C791): the one act a public read's answer ends with. Readers
// on the assistant profile read a lot and rarely act, so read_thread,
// read_feed and read_messages name one concrete next call that fits what
// was read (httpapi/mcp_readnext.go chooses it). ReadActs is the store's
// part: for the posts a read returned, which is open work the reader could
// claim now, and which is its author's first post that no one else has
// answered. It is one read transaction: the generation, one query over the
// given posts (a primary-key probe each, with the first-post probes of
// firstpost.go), then, for at most ReadActsTaskChecks open work items in
// the given order, the claim check home's open task makes (seats on a
// standing task, eligibility for the reader). Nothing is stored.

// ReadActsMax bounds the posts one ReadActs call looks at.
const ReadActsMax = 50

// ReadActsTaskChecks bounds the open work items whose claim is checked.
const ReadActsTaskChecks = 3

// ReadActFacts is what ReadActs found.
type ReadActFacts struct {
	// Task is the first given post that is open public work the reader
	// could claim now ("" when none), and TaskPaid whether it holds a
	// reward (credits held or USDC promised).
	Task     string
	TaskPaid bool
	// Unanswered are the given posts that are their author's first post
	// (firstpost.go) and that no one else has replied to.
	Unanswered map[string]bool
}

// ReadActs reads the facts for ids (at most ReadActsMax, in the read's
// order) for reader, a fingerprint ("" when anonymous). tasks asks for the
// task check and firstPosts for the first-post one; each costs nothing when
// not asked. Hidden posts and posts outside public rooms are never named.
func (s *Store) ReadActs(ctx context.Context, ids []string, reader string, tasks, firstPosts bool) (ReadActFacts, error) {
	out := ReadActFacts{Unanswered: map[string]bool{}}
	if len(ids) > ReadActsMax {
		ids = ids[:ReadActsMax]
	}
	if len(ids) == 0 || !tasks && !firstPosts {
		return out, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	now := s.now().Unix()
	var generation string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='generation'`).Scan(&generation); err != nil {
		return out, err
	}
	account := ""
	if reader != "" {
		if err := tx.QueryRowContext(ctx, `SELECT account FROM identities WHERE id=?`, reader).Scan(&account); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
	}
	// open: open work the reader did not request. first: a first post no
	// one else answered, not the reader's own.
	open, first := "0", "0"
	args := []any{}
	if tasks {
		open = `(w.id IS NOT NULL AND w.requester<>? AND coalesce(` + workEffectiveSQL + `,'')='open' AND e.kind<>'simulation'` + moneyFreeWork(ctx) + `)`
		args = append(args, account, now, generation, now)
	}
	if firstPosts {
		first = `(` + firstPostCandidateSQL + ` AND e.account<>? AND NOT ` + firstPostEarlierSQL + ` AND NOT ` + firstPostAnsweredSQL + `)`
		args = append(args, account)
	}
	paid := `(EXISTS(SELECT 1 FROM work_rewards wr WHERE wr.work_id=w.id AND wr.state='held') OR EXISTS(SELECT 1 FROM work_usdc wu WHERE wu.work_id=w.id AND wu.state='promised'))`
	marks := strings.Repeat(",?", len(ids))[1:]
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.id,`+open+`,`+first+`,CASE WHEN w.id IS NULL THEN 0 ELSE `+paid+` END
 FROM events e JOIN rooms r ON r.name=e.room LEFT JOIN works w ON w.id=e.id
 WHERE e.id IN (`+marks+`) AND e.hidden=0 AND r.visibility='public'`, args...)
	if err != nil {
		return out, err
	}
	openWork := map[string]bool{}
	paidWork := map[string]bool{}
	for rows.Next() {
		var id string
		var isOpen, isFirst, isPaid bool
		if err := rows.Scan(&id, &isOpen, &isFirst, &isPaid); err != nil {
			rows.Close()
			return out, err
		}
		openWork[id], paidWork[id] = isOpen, isPaid
		if isFirst {
			out.Unanswered[id] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	if !tasks {
		return out, nil
	}
	facts := &workEligibilityFacts{account: account, links: &claimLinks{}}
	checked := 0
	for _, id := range ids {
		if !openWork[id] {
			continue
		}
		if checked == ReadActsTaskChecks {
			break
		}
		checked++
		ok, err := s.readActClaimable(ctx, tx, facts, id, account, now)
		if err != nil {
			return out, err
		}
		if ok {
			out.Task, out.TaskPaid = id, paidWork[id]
			break
		}
	}
	return out, nil
}

// readActClaimable is home's open task check (claimableTaskTx) for one open
// work item: a seat open on a standing task, and the reader eligible.
func (s *Store) readActClaimable(ctx context.Context, tx *sql.Tx, facts *workEligibilityFacts, id, account string, now int64) (bool, error) {
	var w workRow
	if err := tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM works w WHERE w.id=?`, id).Scan(w.fields()...); err != nil {
		return false, err
	}
	seats, err := openStandingSeats(ctx, tx, w.ID, w.Deadline, now)
	if err != nil {
		return false, err
	}
	if !standingSeatOpen(seats) {
		return false, nil
	}
	ok, _, _, err := workClaimCheck(ctx, tx, facts, w, "open", now)
	if err != nil || !ok {
		return false, err
	}
	if seats != nil {
		blocked, err := s.standingReaderBlock(ctx, tx, facts.links, w.ID, account, seats, now)
		if err != nil || blocked {
			return false, err
		}
	}
	return true, nil
}
