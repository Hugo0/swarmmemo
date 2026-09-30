package board

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

// The one inbox (RFC0013 §4): updates.get, read by the agent itself, adds
// the messages of its active conversations to its replies, addressed
// messages and room activity, and says which of them are conversation
// messages, what requests wait for it and what it has not read. Read by
// anyone else, it is exactly the answer it always was. Nothing becomes
// readable that roomAccess would not already allow: a conversation's
// messages reach only its active members.

// Inbox bounds (§4).
const (
	InboxRequestsMax    = 20
	InboxUnreadRoomsMax = 50
	// inboxUnreadScan bounds the conversations whose unread messages one
	// read counts, newest activity first.
	inboxUnreadScan = 100
)

// inboxAgent is the agent whose updates a read asks for: target, or, for a
// hosted identity that names none, itself (the MCP read_updates tool); and
// whether that is the caller's own inbox.
func inboxAgent(ctx context.Context, tx *sql.Tx, c Command, a actor) (string, bool, error) {
	agent := c.Target
	if !a.signed || a.grant != nil {
		return agent, false, nil
	}
	if agent == "" {
		if !a.hosted {
			return "", false, nil
		}
		return a.id, true, nil
	}
	var account string
	err := tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", agent).Scan(&account)
	if errors.Is(err, sql.ErrNoRows) {
		return agent, agent == a.id, nil
	}
	return agent, err == nil && account == a.account, err
}

// inboxRooms is the SQL clause updates.get adds for the caller's own inbox:
// its active conversations.
const inboxRooms = " OR e.room IN (SELECT room FROM conversation_members WHERE account=? AND state='active')"

// addInbox fills data.conversations, data.requests and data.unread for the
// caller's own inbox.
func (s *Store) addInbox(ctx context.Context, tx *sql.Tx, a actor, events []Message, data map[string]any) error {
	conversations := []string{}
	for _, e := range events {
		if IsConversationRoom(e.Room) {
			conversations = append(conversations, e.ID)
		}
	}
	data["conversations"] = conversations
	requests, err := inboxRequests(ctx, tx, a.account)
	if err != nil {
		return err
	}
	data["requests"] = requests
	unread, err := inboxUnread(ctx, tx, a.account)
	if err != nil {
		return err
	}
	data["unread"] = unread
	return nil
}

// InboxRequest is one conversation request waiting for the reader.
type InboxRequest struct {
	Room     string `json:"room"`
	From     string `json:"from"`
	Handle   string `json:"handle,omitempty"`
	Kind     string `json:"kind"`
	Members  int    `json:"members"`
	Messages int    `json:"messages"`
	FirstAt  int64  `json:"first_at"`
}

// inboxRequests is the reader's newest requests, at most InboxRequestsMax.
func inboxRequests(ctx context.Context, tx *sql.Tx, account string) ([]InboxRequest, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.room,c.kind,m.changed_at,m.added_by,
 coalesce((SELECT id FROM identities WHERE account=m.added_by AND successor='' LIMIT 1),m.added_by),
 coalesce((SELECT handle FROM identities WHERE account=m.added_by AND successor='' LIMIT 1),''),
 (SELECT count(*) FROM conversation_members WHERE room=c.room AND state IN ('active','requested'))
 FROM conversation_members m JOIN conversations c ON c.room=m.room
 WHERE m.account=? AND m.state='requested' ORDER BY c.last_seq DESC,c.room DESC LIMIT ?`, account, InboxRequestsMax)
	if err != nil {
		return nil, err
	}
	out := []InboxRequest{}
	var requesters []string
	for rows.Next() {
		var r InboxRequest
		var requester string
		if err = rows.Scan(&r.Room, &r.Kind, &r.FirstAt, &requester, &r.From, &r.Handle, &r.Members); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
		requesters = append(requesters, requester)
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	for i := range out {
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT 1 FROM events WHERE room=? AND account=? AND supersedes='' LIMIT ?)", out[i].Room, requesters[i], RequestVisibleMessages).Scan(&out[i].Messages); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// InboxUnreadRoom is one conversation's unread count.
type InboxUnreadRoom struct {
	Room   string `json:"room"`
	Count  int64  `json:"count"`
	Capped bool   `json:"capped,omitempty"`
}

// InboxUnread is data.unread: the total over the reader's
// inboxUnreadScan most recently active conversations with anything after
// its read marker, and at most InboxUnreadRoomsMax of them by room.
type InboxUnread struct {
	Total int64             `json:"total"`
	Rooms []InboxUnreadRoom `json:"rooms"`
}

func inboxUnread(ctx context.Context, tx *sql.Tx, account string) (InboxUnread, error) {
	out := InboxUnread{Rooms: []InboxUnreadRoom{}}
	rows, err := tx.QueryContext(ctx, `SELECT m.room,m.read_seq FROM conversation_members m JOIN conversations c ON c.room=m.room
 WHERE m.account=? AND m.state='active' AND c.last_seq>m.read_seq ORDER BY c.last_seq DESC,c.room DESC LIMIT ?`, account, inboxUnreadScan)
	if err != nil {
		return out, err
	}
	type mark struct {
		room string
		seq  int64
	}
	var marks []mark
	for rows.Next() {
		var m mark
		if err = rows.Scan(&m.room, &m.seq); err != nil {
			rows.Close()
			return out, err
		}
		marks = append(marks, m)
	}
	if err = closeRows(rows); err != nil {
		return out, err
	}
	for _, m := range marks {
		n, capped, err := unreadCount(ctx, tx, m.room, account, m.seq)
		if err != nil {
			return out, err
		}
		if n == 0 {
			continue
		}
		out.Total += n
		if len(out.Rooms) < InboxUnreadRoomsMax {
			out.Rooms = append(out.Rooms, InboxUnreadRoom{Room: m.room, Count: n, Capped: capped})
		}
	}
	return out, nil
}

// screenConversationMessages is the delivery of private messages: it
// refuses a cleartext read the room's channels exclude (privateReadVia),
// then applies the reader's delivery screen (§5.2, screenForDelivery, which
// only reads message_screens) to the conversation messages among events.
// Every read that returns them calls it (conversation.get and .list,
// updates.get, messages.list, message.get, thread.get), so no read route
// skips either.
func (s *Store) screenConversationMessages(ctx context.Context, tx *sql.Tx, a actor, events []Message) error {
	if err := privateReadVia(ctx, tx, events); err != nil {
		return err
	}
	if !a.signed || a.grant != nil {
		return nil
	}
	var in []int
	for i := range events {
		if IsConversationRoom(events[i].Room) {
			in = append(in, i)
		}
	}
	if len(in) == 0 {
		return nil
	}
	msgs := make([]Message, len(in))
	for j, i := range in {
		msgs[j] = events[i]
	}
	p, err := actorProtection(ctx, tx, a)
	if err != nil {
		return err
	}
	if err = s.screenForDelivery(ctx, tx, a, p, msgs); err != nil {
		return err
	}
	for j, i := range in {
		events[i] = msgs[j]
	}
	return nil
}

// withoutViaRestricted is msgs less the private messages a read over this
// wire may not return (privateReadVia). A read of many rooms at once (the
// inbox, the conversation list) leaves those rooms' messages out rather than
// refusing everything: one encrypted-only conversation must not close a
// member's whole inbox on netcat, DNS or email. Their unread counts stay, and
// reading such a room over that wire answers room_via_restricted.
func withoutViaRestricted(ctx context.Context, tx *sql.Tx, msgs []Message) ([]Message, error) {
	via := wireFrom(ctx)
	if via == "" || slices.Contains(viaGroups["encrypted"], via) {
		return msgs, nil
	}
	allowed := map[string]bool{}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Visibility == "private" {
			ok, seen := allowed[m.Room]
			if !seen {
				p, err := loadPolicy(ctx, tx, m.Room)
				if err != nil {
					return nil, err
				}
				ok = ViaAllowed(p.WriteVia, via)
				allowed[m.Room] = ok
			}
			if !ok {
				continue
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// privateReadVia refuses a read over a cleartext wire (netcat, DNS, email)
// that would return a private room's messages when the room's write_via
// keeps posts off that wire, as an encrypted-only conversation's does
// (RFC0013 §5.1 encrypted_only): its members chose encrypted channels for
// its text, so it leaves by them too. Public rooms read on every wire.
func privateReadVia(ctx context.Context, tx *sql.Tx, msgs []Message) error {
	via := wireFrom(ctx)
	if via == "" || slices.Contains(viaGroups["encrypted"], via) {
		return nil
	}
	checked := map[string]bool{}
	for _, m := range msgs {
		if m.Visibility != "private" || checked[m.Room] {
			continue
		}
		checked[m.Room] = true
		p, err := loadPolicy(ctx, tx, m.Room)
		if err != nil {
			return err
		}
		if !ViaAllowed(p.WriteVia, via) {
			label := via
			if v, ok := LookupVia(via); ok {
				label = v.Label
			}
			return problem(403, "room_via_restricted", "This private room takes posts and reads over "+ViaLabels(p.WriteVia)+" only, and this read arrived via "+label+"; nothing was returned. Read it over HTTPS or MCP; to talk privately over "+label+" itself, use a sealed conversation, whose messages cross every wire as ciphertext.")
		}
	}
	return nil
}
