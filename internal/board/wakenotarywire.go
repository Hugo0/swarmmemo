package board

// Board wiring of the wakeup and notary services (RFC0012 §3): the read view
// the wakeup clock watches, the two services' refusals, and data.wakeups in
// updates.get. With neither service in SERVICES none of this runs: the view
// is only called by an enabled wakeup, and updates.get adds nothing.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
)

// Wake-up and notary limits, published beside the memory limits.
const (
	WakeupsPerAccount   = services.WakeupsPerAccount
	WakeupHorizonDays   = services.WakeupHorizon / 86400
	NotaryTextBytes     = services.NotaryTextBytes
	NotaryPerAccountDay = services.NotaryPerAccountDay
)

// serviceBoardView is services.BoardView over the store's tables.
type serviceBoardView struct{}

func (serviceBoardView) LatestSeq(ctx context.Context, q allowance.Querier) (int64, error) {
	var seq int64
	err := q.QueryRowContext(ctx, "SELECT coalesce(max(seq),0) FROM events").Scan(&seq)
	return seq, err
}

// mentionRE finds candidate @handle runs; mentionHandles keeps the ones that
// are whole tokens.
var mentionRE = regexp.MustCompile(`@([A-Za-z0-9][A-Za-z0-9_-]*)`)

// mentionHandles is the @handle tokens in text, in order: an @ at the start
// or after a character that cannot end a handle or address (so x@y.com is
// not one), followed by a whole handle of at most HandleMaxChars.
func mentionHandles(text string) []string {
	var out []string
	for _, m := range mentionRE.FindAllStringSubmatchIndex(text, -1) {
		if m[0] > 0 && strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-@.", rune(text[m[0]-1])) {
			continue
		}
		if handle := text[m[2]:m[3]]; len(handle) <= HandleMaxChars {
			out = append(out, handle)
		}
	}
	return out
}

// EventsAfter reads visible originals (no edits, nothing hidden) after a
// sequence. The author is empty for an anonymous message, so it never counts
// as the watcher's own.
func (serviceBoardView) EventsAfter(ctx context.Context, q allowance.Querier, after int64, limit int) ([]services.BoardEvent, error) {
	rows, err := q.QueryContext(ctx, `SELECT e.seq,e.id,e.room,CASE WHEN e.public_key='' THEN '' ELSE e.account END,
 coalesce((SELECT CASE WHEN p.public_key='' THEN '' ELSE p.account END FROM events p WHERE p.id=e.reply_to AND e.reply_to<>''),''),
 CASE WHEN e.recipient='' THEN '' ELSE coalesce((SELECT i.account FROM identities i WHERE i.id=e.recipient),e.recipient) END, e.text
 FROM events e WHERE e.seq>? AND e.hidden=0 AND e.supersedes='' ORDER BY e.seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	var out []services.BoardEvent
	var texts []string
	for rows.Next() {
		var ev services.BoardEvent
		var text string
		if err = rows.Scan(&ev.Seq, &ev.ID, &ev.Room, &ev.Author, &ev.ReplyToAuthor, &ev.Addressed, &text); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, ev)
		texts = append(texts, text)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Mentions, err = mentions(ctx, q, texts[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// mentions resolves up to MentionsMax distinct @handles in text to accounts.
func mentions(ctx context.Context, q allowance.Querier, text string) ([]string, error) {
	if !strings.Contains(text, "@") {
		return nil, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, h := range mentionHandles(text) {
		handle := strings.ToLower(h)
		if seen[handle] {
			continue
		}
		if len(seen) >= services.MentionsMax {
			break
		}
		seen[handle] = true
		var account string
		err := q.QueryRowContext(ctx, "SELECT account FROM identities WHERE handle=?", handle).Scan(&account)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, account)
	}
	return out, nil
}

func (serviceBoardView) CanRead(ctx context.Context, q allowance.Querier, account, room string) (bool, error) {
	var visibility string
	err := q.QueryRowContext(ctx, "SELECT visibility FROM rooms WHERE name=?", room).Scan(&visibility)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || visibility == "public" {
		return err == nil, err
	}
	var member int
	err = q.QueryRowContext(ctx, "SELECT count(*) FROM members WHERE room=? AND account=?", room, account).Scan(&member)
	return member > 0, err
}

// providerError maps the wakeup and notary refusals; nil for any other code.
func providerError(code string) error {
	switch code {
	case "wakeup_conflict":
		return problem(409, "wakeup_conflict", "An active wake-up already uses this key with other settings; cancel it first, or use another key.")
	case "wakeup_limit":
		return problem(409, "wakeup_limit", fmt.Sprintf("You have %d active wake-ups, the most allowed (or the service is full); cancel one or wait for one to fire.", WakeupsPerAccount))
	case "wakeup_not_found":
		return problem(404, "wakeup_not_found", "None of your wake-ups has that key or ID; service.read wakeup list shows yours.")
	case "wakeup_room_not_found":
		return problem(404, "not_found", "No room by that name is readable by you.")
	case "notary_not_found":
		return problem(404, "notary_not_found", "No receipt for that hash; stamp it with service.call notary stamp.")
	case "notary_limit":
		return &Error{Status: 429, Code: "notary_limit", Message: fmt.Sprintf("Your agent made %d receipts today, the most allowed; the count resets at 00:00 UTC.", NotaryPerAccountDay)}
	}
	return nil
}

// serviceNotices adds what enabled services contribute to an agent's
// updates.get (data.wakeups), given the incoming cursor (already validated
// by the read). It adds nothing while no enabled service contributes.
func (s *Store) serviceNotices(ctx context.Context, tx *sql.Tx, data map[string]any, agent, cursor string, a actor, now int64) error {
	e := s.services.engine
	if e == nil {
		return nil
	}
	var since int64
	if cursor != "" {
		var err error
		if since, err = s.parseCursor(cursor); err != nil {
			return err
		}
	}
	var account string
	err := tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", agent).Scan(&account)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	added, err := e.Notices(ctx, tx, services.NoticeQuery{Account: account, Caller: a.account, Since: since, Now: now})
	if err != nil {
		return err
	}
	for k, v := range added {
		data[k] = v
	}
	return nil
}
