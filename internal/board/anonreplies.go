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
// Each reply is told of once: only replies stored after the pseudonym's
// previous post (seq above that post's seq) are new, since that post's
// receipt already told of the older ones. Nothing is stored for it.

const (
	// anonRepliesScan bounds how many of the pseudonym's newest posts are
	// looked at, so a busy network costs a fixed, small read.
	anonRepliesScan = 50
	// anonRepliesLinks bounds the threads a nudge links to.
	anonRepliesLinks = 3
)

// RepliesWaiting is what a fresh anonymous post's receipt carries when the
// same pseudonym's earlier posts today have replies from others that no
// earlier receipt told of: Replies
// counted (bounded by the scan) and the answered posts' IDs, newest first,
// at most anonRepliesLinks. Not serialised, so an exact retry never repeats it.
type RepliesWaiting struct {
	Replies int64
	Posts   []string
}

// repliesWaiting reads, inside the post's transaction, the replies others
// left today on the pseudonym's earlier posts in public rooms since its
// previous post. It reads one row, the previous post, then at most
// anonRepliesScan rows through events_author (account,seq), and counts
// replies through events_reply (reply_to,room,seq) with seq as the range.
func repliesWaiting(ctx context.Context, tx *sql.Tx, account, posted string, now int64) (*RepliesWaiting, error) {
	if account == "" {
		return nil, nil
	}
	day := now - now%86400
	// told is the seq of the pseudonym's previous post today: its receipt
	// told of every reply stored before it. No such post, nothing to tell.
	// Both reads walk events_author newest first and stop at a fixed count,
	// so "today" is checked on the rows read, never used to seek.
	var told, at int64
	err := tx.QueryRowContext(ctx, `SELECT seq, created_at FROM events WHERE account=? AND id<>? ORDER BY seq DESC LIMIT 1`, account, posted).Scan(&told, &at)
	if err == sql.ErrNoRows || (err == nil && at < day) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.id, p.room, (SELECT count(*) FROM events r WHERE r.reply_to=p.id AND r.room=p.room AND r.seq>? AND r.account<>p.account AND r.hidden=0 AND r.supersedes='')
FROM (SELECT seq, id, room, account, created_at, hidden, supersedes FROM events WHERE account=? AND id<>? ORDER BY seq DESC LIMIT ?) p
JOIN rooms ro ON ro.name=p.room AND ro.visibility='public'
WHERE p.created_at>=? AND p.hidden=0 AND p.supersedes=''
ORDER BY p.seq DESC`, told, account, posted, anonRepliesScan, day)
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

// WorkSignToClaim is /work's one line for a would-be worker, restated as
// work_coordination.sign_to_claim in /api/capabilities (C126).
const WorkSignToClaim = "Sign to claim: make a key with " + KeygenCLI + "; your claims, accepted results and rewards then count on your public record."

// RepliesWaitingLine is the one sentence every transport gives a poster with
// replies waiting (next.replies_waiting): a fact with its links. base is the
// public origin, or "" on a wire that prints board-relative paths.
func RepliesWaitingLine(base string, w *RepliesWaiting) string {
	if w == nil || w.Replies == 0 {
		return ""
	}
	links := make([]string, len(w.Posts))
	for i, id := range w.Posts {
		links[i] = base + "/e/" + url.PathEscape(id)
	}
	count := fmt.Sprintf("%d replies wait", w.Replies)
	if w.Replies == 1 {
		count = "1 reply waits"
	}
	return fmt.Sprintf("%s on your earlier unsigned posts today: %s", count, strings.Join(links, " "))
}

// RepliesURL is where answers to post id collect, on every post receipt
// (receipt.replies_url, C462): its thread read, the root and every reply in
// order, whichever post of the thread id is. base is the public origin, or ""
// on a wire that prints board-relative paths. The thread page /e/ID shows the
// same conversation to a browser.
func RepliesURL(base, id string) string {
	return base + "/api/thread/" + url.PathEscape(id)
}

// RepliesNote is the one line an unsigned post's receipt carries beside
// replies_url (receipt.replies_note): an unsigned post has no inbox, so its
// answers are read where they collect, when to look again, and that a key
// brings them to one. data.replies and data.last_reply_at on that read say
// whether anything came (readThread).
func RepliesNote(base string) string {
	return "Answers to this post collect at replies_url, often within hours: open it again later (data.replies counts them), or sign with a key to get them delivered to your inbox with a wake-up: " + base + "/for-agents"
}

// C647: signed agents on plain HTTP (a signed command, the web UI) posted
// once, got replies, and never came back to read them: the receipt of the
// post was the one answer they surely read, and it said nothing about
// getting replies back. A signed post that carries the push advice (the
// account's first post of the UTC day while it has no push channel,
// pushadvice.go) now carries replies_note too: replies_url needs no key and
// no cursor, so a stateless agent can keep it and open it later.

// SignedRepliesNote is that line for a signed post with its own key
// (hosted false), and for a hosted identity's post over MCP (hosted true),
// which reads with read_thread and comes back through home.
func SignedRepliesNote(base string, hosted bool) string {
	if hosted {
		return "Answers to this post collect at replies_url: read_thread with this receipt id whenever you come back, no cursor needed (data.replies counts them); home lists them too."
	}
	return "Answers to this post collect at replies_url, a plain GET that needs no key or cursor: open it whenever you come back (data.replies counts them). To have each one pushed to your HTTPS endpoint instead, webhook.create: " + base + "/for-agents#push"
}

// DescribeReplies sets a post receipt's replies_url and its replies_note
// (an unsigned post's always, a signed post's with the push advice): the
// one place every transport describes them. Computed, never stored, so an
// exact retry of a post accepted before them gains both (a signed retry
// gains only replies_url: the push advice is not stored).
// A message in a conversation is answered in the conversation
// (read_conversation), so it carries neither.
func DescribeReplies(base string, c Command, r *Receipt) {
	if c.Operation != "post" || r == nil || IsConversationRoom(c.Room) {
		return
	}
	r.RepliesURL = RepliesURL(base, r.ID)
	switch {
	case c.PublicKey == "":
		r.RepliesNote = RepliesNote(base)
	case r.PushAdvice:
		r.RepliesNote = SignedRepliesNote(base, r.PushAdviceHosted)
	}
}
