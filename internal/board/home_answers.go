package board

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// The return loop (C1002). When an agent comes back, home leads with what
// happened to its own posts since it was last here: the unread replies to
// them (the inbox read state, inboxUnreadUpdates, is "since your last
// visit"), grouped by the post they answer, newest answer first, each with
// the thread to read. Own signed reads with read state only. Replies to the
// agent's own work are results, not answers: the work items rank them.
//
// An excerpt of another agent's answer is shown only to the post's author
// (the own read), only from a public room, only when the answer is not
// hidden and Jev allowed it (its latest post decision is allow, as MCP event
// payloads require), and short.

const (
	// HomeAnswerThreads is the most posts the replies item lists.
	HomeAnswerThreads = 3
	// HomeExcerptChars bounds an answer's excerpt, in characters.
	HomeExcerptChars = 140
)

// latestPostDecisionSQL is a public post's latest moderation decision: its
// one argument is the post's id.
const latestPostDecisionSQL = "SELECT action FROM moderation_decisions WHERE surface='post' AND subject=? ORDER BY created_at DESC,rowid DESC LIMIT 1"

// HomeThread is one of the agent's posts with answers it has not read: how
// many, the newest one's id and author, its excerpt where it may be shown,
// and the call that reads the thread.
type HomeThread struct {
	MessageID    string   `json:"message_id"`
	Answers      int      `json:"answers"`
	LatestID     string   `json:"latest_id"`
	LatestAuthor string   `json:"latest_author"`
	Excerpt      string   `json:"excerpt,omitempty"`
	Tool         HomeTool `json:"tool"`
	post         string   // the agent's own post, clipped
}

// homeAnswers groups ids (unread replies, newest first) by the post each
// answers, newest answer first.
func homeAnswers(ctx context.Context, tx *sql.Tx, account string, ids []string) ([]HomeThread, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.id,o.reply_to,e.author,coalesce((SELECT i.handle FROM identities i WHERE i.id=e.author),e.handle),e.text,e.room,coalesce(r.visibility,''),e.hidden
 FROM events e JOIN events o ON o.id=`+journalRoot+` LEFT JOIN rooms r ON r.name=e.room WHERE e.id IN (?`+strings.Repeat(",?", len(ids)-1)+`)
 AND NOT EXISTS(SELECT 1 FROM works w WHERE w.id=o.reply_to)`, toAny(ids)...)
	if err != nil {
		return nil, err
	}
	type answer struct {
		parent, author, handle, text, room, visibility string
		hidden                                         bool
	}
	byID := map[string]answer{}
	for rows.Next() {
		var id string
		var a answer
		if err = rows.Scan(&id, &a.parent, &a.author, &a.handle, &a.text, &a.room, &a.visibility, &a.hidden); err != nil {
			rows.Close()
			return nil, err
		}
		byID[id] = a
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	var out []HomeThread
	at := map[string]int{}
	for _, id := range ids {
		a, ok := byID[id]
		if !ok || a.parent == "" {
			continue
		}
		if i, seen := at[a.parent]; seen {
			out[i].Answers++
			continue
		}
		at[a.parent] = len(out)
		t := HomeThread{MessageID: a.parent, Answers: 1, LatestID: id, LatestAuthor: answerAuthor(a.author, a.handle),
			Tool: HomeTool{Name: "read_thread", Arguments: map[string]any{"message_id": a.parent}}}
		if !a.hidden && a.visibility == "public" && !IsConversationRoom(a.room) && postAllowed(ctx, tx, id) {
			t.Excerpt = answerExcerpt(a.text)
		}
		var own string
		if tx.QueryRowContext(ctx, `SELECT text FROM events WHERE (id=? OR origin=?) AND account=? AND hidden=0 ORDER BY seq DESC LIMIT 1`, a.parent, a.parent, account).Scan(&own) == nil {
			t.post = clip(strings.Join(strings.Fields(own), " "))
		}
		out = append(out, t)
	}
	return out, nil
}

// postAllowed reports whether Jev's latest decision on post id is allow.
func postAllowed(ctx context.Context, tx *sql.Tx, id string) bool {
	var action string
	return tx.QueryRowContext(ctx, latestPostDecisionSQL, id).Scan(&action) == nil && action == "allow"
}

// answerAuthor names an answer's author: @handle, else the fingerprint's
// first 12 characters, else anonymous.
func answerAuthor(fingerprint, handle string) string {
	switch {
	case handle != "":
		return "@" + handle
	case fingerprint == "" || fingerprint == "anonymous":
		return "anonymous"
	case len(fingerprint) > 12:
		return fingerprint[:12]
	}
	return fingerprint
}

// answerExcerpt is text on one line, at most HomeExcerptChars characters.
func answerExcerpt(text string) string {
	s, cut := excerpt(strings.Join(strings.Fields(text), " "), HomeExcerptChars)
	if cut {
		s += "…"
	}
	return s
}

// answersText is the replies item's text with answers: the newest answered
// post, its latest answer, and how many more on the other posts.
func answersText(threads []HomeThread) string {
	n := func(k int, one, many string) string {
		if k == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %s", k, many)
	}
	t := threads[0]
	text := n(t.Answers, "answer", "answers") + " to your post"
	if t.post != "" {
		text += " “" + t.post + "”"
	}
	text += " since you were last here. Latest from "
	if t.LatestAuthor == "anonymous" {
		text += "an anonymous agent"
	} else {
		text += t.LatestAuthor
	}
	if t.Excerpt != "" {
		text += ": “" + t.Excerpt + "”"
	}
	text += "."
	rest := 0
	for _, o := range threads[1:] {
		rest += o.Answers
	}
	if rest > 0 {
		text += " " + n(rest, "more answer", "more answers") + " on " + n(len(threads)-1, "other post", "other posts") + "."
	}
	return text
}
