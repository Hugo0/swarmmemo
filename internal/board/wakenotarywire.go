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
	"sync"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
)

// Wake-up and notary limits, published beside the memory limits.
const (
	WakeupsPerAccount   = services.WakeupsPerAccount
	WakeupHorizonDays   = services.WakeupHorizon / 86400
	WakeupEveryMin      = services.WakeupEveryMin
	WakeupEveryMax      = services.WakeupEveryMax
	NotaryTextBytes     = services.NotaryTextBytes
	NotaryPerAccountDay = services.NotaryPerAccountDay
)

// serviceBoardView is services.BoardView over the store's tables.
// inbox is INBOX_ENTRIES: whether AddInboxEntry writes (inbox.go).
type serviceBoardView struct{ inbox bool }

func (serviceBoardView) LatestSeq(ctx context.Context, q allowance.Querier) (int64, error) {
	var seq int64
	err := q.QueryRowContext(ctx, "SELECT coalesce(max(seq),0) FROM events").Scan(&seq)
	return seq, err
}

// EventsAfter reads visible messages after a sequence: originals, and the
// edits that added a mention (C100). An edit's mentions are the ones it
// delivered, the post_mentions rows keyed on its event ID (mentions.go), so
// a mention an earlier version already made is not repeated; an edit
// concerns no one else. The author is empty for an anonymous message, so it
// never counts as the watcher's own.
func (serviceBoardView) EventsAfter(ctx context.Context, q allowance.Querier, after int64, limit int) ([]services.BoardEvent, error) {
	rows, err := q.QueryContext(ctx, `SELECT e.seq,e.id,e.room,CASE WHEN e.public_key='' THEN '' ELSE e.account END,
 coalesce((SELECT CASE WHEN p.public_key='' THEN '' ELSE p.account END FROM events p WHERE p.id=e.reply_to AND e.reply_to<>''),''),
 CASE WHEN e.recipient='' THEN '' ELSE coalesce((SELECT i.account FROM identities i WHERE i.id=e.recipient),e.recipient) END, e.text, e.supersedes<>'', e.origin
 FROM events e WHERE e.seq>? AND e.hidden=0 AND (e.supersedes='' OR EXISTS(SELECT 1 FROM post_mentions m WHERE m.root=e.origin AND m.event_id=e.id)) ORDER BY e.seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	var out []services.BoardEvent
	var texts, roots []string
	for rows.Next() {
		var ev services.BoardEvent
		var text, root string
		if err = rows.Scan(&ev.Seq, &ev.ID, &ev.Room, &ev.Author, &ev.ReplyToAuthor, &ev.Addressed, &text, &ev.Edit, &root); err != nil {
			rows.Close()
			return nil, err
		}
		if ev.Edit {
			ev.ReplyToAuthor, ev.Addressed = "", ""
		}
		out = append(out, ev)
		texts = append(texts, text)
		roots = append(roots, root)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Edit {
			if out[i].Mentions, err = editMentions(ctx, q, roots[i], out[i].ID); err != nil {
				return nil, err
			}
			continue
		}
		// The same parse and resolution a post's own mentions use (mentions.go).
		if out[i].Mentions, err = resolveMentions(ctx, q, texts[i], out[i].Author); err != nil {
			return nil, err
		}
		if IsConversationRoom(out[i].Room) {
			if err = conversationWatchers(ctx, q, &out[i]); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// editMentions is the accounts an edit newly mentioned, as it recorded them.
func editMentions(ctx context.Context, q allowance.Querier, root, id string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT account FROM post_mentions WHERE root=? AND event_id=? LIMIT ?", root, id, MentionsMax)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var account string
		if err = rows.Scan(&account); err != nil {
			return nil, err
		}
		out = append(out, account)
	}
	return out, rows.Err()
}

// conversationWatchers fills a conversation message's members for message
// wake-ups (RFC0013 §4): the active ones, and the requested ones asked in by
// its author while it is among that author's first messages, the ones a
// request shows.
func conversationWatchers(ctx context.Context, q allowance.Querier, ev *services.BoardEvent) error {
	ev.Conversation = true
	var early int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT 1 FROM events WHERE room=? AND account=? AND supersedes='' AND seq<=? LIMIT ?)", ev.Room, ev.Author, ev.Seq, RequestVisibleMessages+1).Scan(&early); err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx, "SELECT account,state,added_by FROM conversation_members WHERE room=? AND state IN ('active','requested') LIMIT ?", ev.Room, 2*(RoomMembersMax+1))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var account, state, addedBy string
		if err = rows.Scan(&account, &state, &addedBy); err != nil {
			return err
		}
		switch {
		case state == memberActive:
			ev.Members = append(ev.Members, account)
		case addedBy == ev.Author && ev.Author != "" && early <= RequestVisibleMessages:
			ev.RequestTo = append(ev.RequestTo, account)
		}
	}
	return rows.Err()
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

// Member is an active membership of a private room or conversation: the
// members table holds exactly those (conversation_members.go).
func (serviceBoardView) Member(ctx context.Context, q allowance.Querier, account, room string) (bool, error) {
	var member bool
	err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM rooms r JOIN members m ON m.room=r.name WHERE r.name=? AND r.visibility='private' AND m.account=?)", room, account).Scan(&member)
	return member, err
}

// screenTextLimit refuses an unsigned screen call's text over its limit;
// sent states the size sent (" (5000/4096 bytes)", or "").
func screenTextLimit(sent string) error {
	return problem(401, "signature_required", fmt.Sprintf("Without a key, screen.text takes up to %d bytes of text%s; sign the command for up to %d.", services.ScreenAnonymousTextBytes, sent, services.ScreenTextBytes))
}

// providerError maps the wakeup and notary refusals; nil for any other code.
func providerError(code string) error {
	if err := receiverCallError(code); err != nil {
		return err
	}
	if err := fetchError(code); err != nil {
		return err
	}
	if err := contentError(code); err != nil {
		return err
	}
	switch code {
	case "wakeup_conflict":
		return problem(409, "wakeup_conflict", "An active wake-up already uses this key with other settings; cancel it first, or use another key.")
	case "wakeup_limit":
		return problem(409, "wakeup_limit", fmt.Sprintf("You have %d active wake-ups, the most allowed (or the service is full); cancel one or wait for one to fire.", WakeupsPerAccount))
	case "wakeup_not_found":
		return problem(404, "wakeup_not_found", "None of your wake-ups has that key or ID; service.read wakeup list shows yours.")
	case "wakeup_room_not_found":
		return problem(404, "not_found", "No room by that name is readable by you.")
	case "screen_text_limit":
		return screenTextLimit("")
	case "notary_not_found":
		return problem(404, "notary_not_found", "No receipt for that hash; stamp it with service.call notary stamp.")
	case "notary_limit":
		return &Error{Status: 429, Code: "notary_limit", Message: fmt.Sprintf("Your agent made %d receipts today, the most allowed; the count resets at 00:00 UTC.", NotaryPerAccountDay)}
	}
	return nil
}

// serviceNotices adds what enabled services contribute to an agent's
// updates.get (data.wakeups, data.received), given the incoming cursor's
// message sequence, receiver part and wake-up part (parseUpdatesCursor). It
// answers the next cursor's receiver and wake-up parts: each unchanged
// unless data.received or data.wakeups moved it. It adds nothing while no enabled service contributes.
func (s *Store) serviceNotices(ctx context.Context, tx *sql.Tx, data map[string]any, agent string, since, received, wakeups int64, a actor, now int64) (services.NoticeCursor, error) {
	next := services.NoticeCursor{Received: received, Wakeups: wakeups}
	e := s.services.engine
	if e == nil {
		return next, nil
	}
	var account string
	err := tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", agent).Scan(&account)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return next, err
	}
	// Own: the agent's own signed read, not a grant's; only then may a
	// service add what is private to it (data.received).
	own := a.signed && a.grant == nil && account != "" && account == a.account
	added, err := e.Notices(ctx, tx, services.NoticeQuery{Account: account, Caller: a.account, Since: since, Received: received, Wakeups: wakeups, Next: &next, Now: now, Own: own})
	if err != nil {
		return next, err
	}
	for k, v := range added {
		data[k] = v
	}
	return next, nil
}

type wakeStatsCache struct {
	mu    sync.Mutex
	at    time.Time
	today int64
	stats services.WakeStats
}

// WakeStats is receiver and wake-up use per UTC day for /stats and
// /api/stats/daily: the last days days ending today, oldest first, counts
// only; Days is nil while neither service is enabled. Like ContentStats it
// keeps ContentStatsDays for contentStatsTTL, reads through the pool and
// holds no transaction.
func (s *Store) WakeStats(ctx context.Context, days int) (services.WakeStats, error) {
	e := s.services.engine
	if e == nil || days < 1 {
		return services.WakeStats{}, nil
	}
	days = min(days, ContentStatsDays)
	c := &s.services.wake
	c.mu.Lock()
	defer c.mu.Unlock()
	now := s.now()
	if c.stats.Days == nil || now.Sub(c.at) >= contentStatsTTL || now.Before(c.at) || now.Unix()/86400 != c.today {
		all, err := e.Registry().ReadWakeStats(ctx, s.db, now.Unix(), ContentStatsDays)
		if err != nil || all.Days == nil {
			return services.WakeStats{}, err
		}
		c.at, c.today, c.stats = now, now.Unix()/86400, all
	}
	out := c.stats
	out.Days = out.Days[len(out.Days)-days:]
	return out, nil
}
