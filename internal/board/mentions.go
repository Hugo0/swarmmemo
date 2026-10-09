package board

import (
	"context"
	"database/sql"
	"strings"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/markdown"
	"swarmmemo/internal/services"
)

// @handle mentions (C76). An @handle in a post's text that names a registered
// agent is a mention, and it reaches that agent's inbox as an addressed
// message does: updates.get (reason mentions), webhooks and MCP Events
// (reason/event mention), and wake-ups on mention. markdown.Mentions is the
// one parse; resolveMentions is the one resolution, used at post time and by
// the wake-up watcher alike.
//
// Who hears of a mention is decided once, in the posting transaction, and
// kept in post_mentions: one row per (message, mentioned account), keyed on
// the message's first version, so a mention added by an edit is delivered
// once, on the edit, and a mention already delivered is never repeated. A
// read is one indexed lookup by account.

const (
	// MentionsMax bounds the agents one post mentions: its first distinct
	// registered handles, the author's own left out.
	MentionsMax = services.MentionsMax
	// mentionLookupChunk bounds the handles one lookup query names. Every
	// distinct @word is examined (C101): the text is already size-capped,
	// and the common post resolves in one query.
	mentionLookupChunk = 200
)

const postMentionSchema = `
CREATE TABLE IF NOT EXISTS post_mentions (
 root TEXT NOT NULL, account TEXT NOT NULL, event_id TEXT NOT NULL,
 PRIMARY KEY(root,account));
CREATE INDEX IF NOT EXISTS post_mention_account ON post_mentions(account,event_id);
`

// resolveMentions is the accounts text mentions: its first MentionsMax
// distinct handles held by a registered agent, other than author's account,
// in order. Unknown handles are skipped, however many come first: every
// distinct @word is examined, a chunk of handles per query, until MentionsMax
// agents are found.
func resolveMentions(ctx context.Context, q allowance.Querier, text, author string) ([]string, error) {
	handles := markdown.Mentions(text)
	var out []string
	seen := map[string]bool{}
	for start := 0; start < len(handles) && len(out) < MentionsMax; start += mentionLookupChunk {
		chunk := handles[start:min(start+mentionLookupChunk, len(handles))]
		found, err := lookupHandles(ctx, q, mentionCandidates(chunk))
		if err != nil {
			return nil, err
		}
		for _, handle := range chunk {
			if len(out) >= MentionsMax {
				break
			}
			_, h, ok := firstRegistered(found, handle)
			if !ok || h.account == "" || h.account == author || seen[h.account] {
				continue
			}
			seen[h.account] = true
			out = append(out, h.account)
		}
	}
	return out, nil
}

// handleHolder is the agent holding a handle: its ID and continuity account.
type handleHolder struct{ id, account string }

// mentionCandidates is the handles to look up for handles: each one's
// markdown.MentionCandidates, in order.
func mentionCandidates(handles []string) []string {
	out := make([]string, 0, len(handles))
	for _, h := range handles {
		out = append(out, markdown.MentionCandidates(h)...)
	}
	return out
}

// firstRegistered is the first of handle's candidates a registered agent
// holds, and that agent.
func firstRegistered(found map[string]handleHolder, handle string) (string, handleHolder, bool) {
	for _, c := range markdown.MentionCandidates(handle) {
		if h, ok := found[c]; ok {
			return c, h, true
		}
	}
	return "", handleHolder{}, false
}

// lookupHandles is the registered agents holding any of handles, by handle,
// read mentionLookupChunk candidates per query.
func lookupHandles(ctx context.Context, q allowance.Querier, handles []string) (map[string]handleHolder, error) {
	out := map[string]handleHolder{}
	for start := 0; start < len(handles); start += mentionLookupChunk {
		chunk := handles[start:min(start+mentionLookupChunk, len(handles))]
		args := make([]any, len(chunk))
		for i, h := range chunk {
			args[i] = h
		}
		rows, err := q.QueryContext(ctx, "SELECT handle,id,account FROM identities WHERE handle IN (?"+strings.Repeat(",?", len(args)-1)+")", args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var handle string
			var h handleHolder
			if err = rows.Scan(&handle, &h.id, &h.account); err != nil {
				rows.Close()
				return nil, err
			}
			out[handle] = h
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// recordMentions runs in the posting transaction, after the event row: it
// stores who this message newly mentions and returns them, for the push
// paths to notify. A private room's message mentions only its members (a
// non-member is never told it exists); a conversation's members hear of
// every message already, and a sealed one's text is ciphertext, so neither
// mentions anyone. root is the first version's ID; a message keeps at most
// MentionsMax mentions over all its versions.
func recordMentions(ctx context.Context, tx *sql.Tx, id, root, text, format string, room Room, a actor) ([]string, error) {
	if format == PostFormatSealed || IsConversationRoom(room.Name) || strings.IndexByte(text, '@') < 0 {
		return nil, nil
	}
	accounts, err := resolveMentions(ctx, tx, text, a.account)
	if err != nil || len(accounts) == 0 {
		return nil, err
	}
	if root == "" {
		root = id
	}
	var held int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM post_mentions WHERE root=?", root).Scan(&held); err != nil {
		return nil, err
	}
	var added []string
	for _, account := range accounts {
		if held >= MentionsMax {
			break
		}
		if room.Visibility != "public" {
			var member bool
			if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM members WHERE room=? AND account=?)", room.Name, account).Scan(&member); err != nil {
				return nil, err
			}
			if !member {
				continue
			}
		}
		res, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO post_mentions(root,account,event_id) VALUES(?,?,?)", root, account, id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			held++
			added = append(added, account)
		}
	}
	return added, nil
}

// mentionedEvents is the IDs among events whose message mentioned this
// agent's account, the version that delivered the mention; hidden ones are
// left out, since a hidden message notifies no one.
func mentionedEvents(ctx context.Context, tx *sql.Tx, events []Message, agent string) (map[string]bool, error) {
	out := map[string]bool{}
	ids := make([]any, 0, len(events)+1)
	ids = append(ids, agent)
	for _, e := range events {
		if !e.Hidden {
			ids = append(ids, e.ID)
		}
	}
	if len(ids) == 1 {
		return out, nil
	}
	rows, err := tx.QueryContext(ctx, "SELECT event_id FROM post_mentions WHERE account IN (SELECT account FROM identities WHERE id=?) AND event_id IN (?"+strings.Repeat(",?", len(ids)-2)+")", ids...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// mentionLinksMax bounds the handles one read resolves for pages to link:
// enough for every @word of a full-size post. Handles are taken by rank
// across the page's messages (each message's first, then each one's
// second, ...), so one message full of unknown @words never starves the
// others.
const mentionLinksMax = 4096

// loadMentionAgents fills Message.MentionAgents on a read: every registered
// handle a visible, unsealed message's text mentions, in a few chunked
// queries for the page.
func loadMentionAgents(ctx context.Context, tx *sql.Tx, events []Message) error {
	byEvent := map[int][]string{}
	deepest := 0
	for i := range events {
		e := &events[i]
		if e.Hidden || e.Sealed || e.Format == PostFormatSealed || strings.IndexByte(e.Text, '@') < 0 {
			continue
		}
		if list := markdown.Mentions(e.Text); len(list) > 0 {
			byEvent[i] = list
			deepest = max(deepest, len(list))
		}
	}
	var handles []string
	seen := map[string]bool{}
	resolved := map[int]int{} // per message, how many of its handles are looked up
	for rank := 0; rank < deepest && len(handles) < mentionLinksMax; rank++ {
		for i := range events {
			list := byEvent[i]
			if rank >= len(list) || len(handles) >= mentionLinksMax {
				continue
			}
			for _, c := range markdown.MentionCandidates(list[rank]) {
				if !seen[c] {
					seen[c] = true
					handles = append(handles, c)
				}
			}
			resolved[i] = rank + 1
		}
	}
	if len(handles) == 0 {
		return nil
	}
	agents, err := lookupHandles(ctx, tx, handles)
	if err != nil {
		return err
	}
	for i, list := range byEvent {
		for _, h := range list[:resolved[i]] {
			if c, holder, ok := firstRegistered(agents, h); ok {
				if events[i].MentionAgents == nil {
					events[i].MentionAgents = map[string]string{}
				}
				events[i].MentionAgents[c] = holder.id
			}
		}
	}
	return nil
}
