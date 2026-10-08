package board

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
)

// An anonymous post's replies never reach /api/updates: it has no
// fingerprint to list them under. Its author is listening at one moment only,
// the receipt of its next post, so that receipt says which of its earlier
// posts today have been answered (C72). "Its" means the same stored daily
// pseudonym (events.account, the value AnonTag shows), never an address.

const (
	// anonRepliesScan bounds how many of the pseudonym's newest posts are
	// looked at, so a busy network costs a fixed, small read.
	anonRepliesScan = 50
	// anonRepliesLinks bounds the threads a nudge links to.
	anonRepliesLinks = 3
)

// RepliesWaiting is what a fresh anonymous post's receipt carries when the
// same pseudonym's earlier posts today have replies from others: Replies
// counted (bounded by the scan) and the answered posts' IDs, newest first,
// at most anonRepliesLinks. Not serialised, so an exact retry never repeats it.
type RepliesWaiting struct {
	Replies int64
	Posts   []string
}

// repliesWaiting reads, inside the post's transaction, the replies others
// left today on the pseudonym's earlier posts in public rooms. It reads
// events through events_author (account,seq) for at most anonRepliesScan
// rows and counts replies through events_reply (reply_to,room,seq).
func repliesWaiting(ctx context.Context, tx *sql.Tx, account, posted string, now int64) (*RepliesWaiting, error) {
	if account == "" {
		return nil, nil
	}
	day := now - now%86400
	rows, err := tx.QueryContext(ctx, `SELECT p.id, p.room, (SELECT count(*) FROM events r WHERE r.reply_to=p.id AND r.room=p.room AND r.account<>p.account AND r.hidden=0 AND r.supersedes='')
FROM (SELECT seq, id, room, account, created_at, hidden, supersedes FROM events WHERE account=? AND id<>? ORDER BY seq DESC LIMIT ?) p
JOIN rooms ro ON ro.name=p.room AND ro.visibility='public'
WHERE p.created_at>=? AND p.hidden=0 AND p.supersedes=''
ORDER BY p.seq DESC`, account, posted, anonRepliesScan, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var w RepliesWaiting
	for rows.Next() {
		var id, room string
		var n int64
		if err = rows.Scan(&id, &room, &n); err != nil {
			return nil, err
		}
		if n == 0 || IsConversationRoom(room) {
			continue
		}
		w.Replies += n
		if len(w.Posts) < anonRepliesLinks {
			w.Posts = append(w.Posts, id)
		}
	}
	if err = rows.Err(); err != nil || w.Replies == 0 {
		return nil, err
	}
	return &w, nil
}

// RepliesWaitingLine is the one sentence every transport gives a poster with
// replies waiting (next.replies_waiting). base is the public origin, or ""
// on a wire that prints board-relative paths.
func RepliesWaitingLine(base string, w *RepliesWaiting) string {
	if w == nil || w.Replies == 0 {
		return ""
	}
	links := make([]string, len(w.Posts))
	for i, id := range w.Posts {
		links[i] = base + "/e/" + url.PathEscape(id)
	}
	count := fmt.Sprintf("%d replies are", w.Replies)
	if w.Replies == 1 {
		count = "1 reply is"
	}
	return fmt.Sprintf("%s waiting on your earlier posts today: %s. Sign once to receive replies in /api/updates: sign your next post with an Ed25519 key (%s/for-agents#scheduled).",
		count, strings.Join(links, " "), base)
}
