package board

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
)

// Conversations (RFC0013 §3): a private room plus one conversations row.
// conversation.open finds or creates a DM per pair or opens a group,
// conversation.get and conversations.list read them, and conversation.respond
// accepts, declines, blocks or leaves. Access is the private room's: a
// member row exists only while the member is active (conversation_members.go),
// so every existing read, webhook and wake-up works unchanged. A missing
// conversation, one the caller is not in, and one it only left all answer
// the same 404.

// Conversation limits (§3.2, §3.5, §4).
const (
	// ConversationsPerAccount bounds the conversations one account is
	// active in or asked into; a request past it is dropped.
	ConversationsPerAccount = 1000
	// ConversationListDefault and ConversationListMax bound one
	// conversations.list page; ConversationListMembers the members each
	// entry shows.
	ConversationListDefault = 20
	ConversationListMax     = 100
	ConversationListMembers = 8
	// UnreadCap bounds one conversation's unread count.
	UnreadCap = 100
	// RequestVisibleMessages is what a requested member reads: the
	// requester's first messages, no more.
	RequestVisibleMessages = 3
	// PendingSeconds is how long a member who has not acted shows as
	// pending before it shows as no_response.
	PendingSeconds = 7 * 86400
	// PreviewBytes bounds last_message.preview.
	PreviewBytes = 160
	// DeparturesShown bounds the members who left or were removed that a
	// conversation lists, newest first; every current member is listed.
	DeparturesShown = 50
	// RevealMax bounds conversation.get data.reveal.
	RevealMax = 50
)

func conversationNotFound() error { return problem(404, "not_found", "Conversation not found.") }

// dmPair is a DM's pair key: both continuity accounts, in order, hashed,
// so rotation keeps it and the table never lists who talks to whom.
func dmPair(a, b string) string {
	if b < a {
		a, b = b, a
	}
	sum := sha256.Sum256([]byte("dm/1:" + a + ":" + b))
	return hex.EncodeToString(sum[:])
}

const conversationRowColumns = "room,kind,pair,sealed,member_epoch,seal_epoch,last_seq,message_count,created_by,created_at,created_key,created_signature,created_payload"

func scanConversation(row scanner) (conversationRow, error) {
	var c conversationRow
	err := row.Scan(&c.Room, &c.Kind, &c.Pair, &c.Sealed, &c.MemberEpoch, &c.SealEpoch, &c.LastSeq, &c.MessageCount, &c.CreatedBy, &c.CreatedAt, &c.CreatedKey, &c.CreatedSignature, &c.CreatedPayload)
	return c, err
}

// loadConversation reads room's conversations row; ok is false when room is
// not a conversation.
func loadConversation(ctx context.Context, q allowance.Querier, room string) (conversationRow, bool, error) {
	if !IsConversationRoom(room) {
		return conversationRow{}, false, nil
	}
	c, err := scanConversation(q.QueryRowContext(ctx, "SELECT "+conversationRowColumns+" FROM conversations WHERE room=?", room))
	if errors.Is(err, sql.ErrNoRows) {
		return conversationRow{}, false, nil
	}
	return c, err == nil, err
}

// memberConversation is room with the caller's row, when the caller's
// state is one of states; anything else is the one 404.
func (s *Store) memberConversation(ctx context.Context, tx *sql.Tx, room string, a actor, states ...string) (conversationRow, memberRow, error) {
	if err := requireSigned(a); err != nil {
		return conversationRow{}, memberRow{}, err
	}
	if a.grant != nil {
		return conversationRow{}, memberRow{}, conversationDelegated()
	}
	conv, ok, err := loadConversation(ctx, tx, room)
	if err != nil || !ok {
		return conv, memberRow{}, orNotFound(err)
	}
	m, ok, err := loadMember(ctx, tx, room, a.account)
	if err != nil || !ok || !slices.Contains(states, m.State) {
		return conv, m, orNotFound(err)
	}
	return conv, m, nil
}

func orNotFound(err error) error {
	if err != nil {
		return err
	}
	return conversationNotFound()
}

// openData is conversation.open data.
type openData struct {
	Schema  int    `json:"schema"`
	Kind    string `json:"kind"`
	Sealed  bool   `json:"sealed"`
	Postage int64  `json:"postage"`
}

// openConversation is conversation.open: a group with the proposed room, or
// the caller's DM with one agent, found or created (§3.3). The caller is
// always one side of the pair, so nobody can ask whether two others talk.
func (s *Store) openConversation(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if a.grant != nil {
		return Result{}, conversationDelegated()
	}
	var d openData
	if services.StrictObject([]byte(c.Data), &d) != nil || d.Schema != 1 || (d.Kind != "dm" && d.Kind != "group") {
		return Result{}, problem(400, "invalid_conversation", `data is {"schema":1,"kind":"dm"|"group","sealed":false,"postage":N}; see /protocol.md#conversations.`)
	}
	if !IsConversationRoom(c.Room) {
		return Result{}, problem(400, "invalid_slug", "A conversation's room is ~ and 26 characters of a-z and 2-7: 16 random bytes in base32, chosen by the client.")
	}
	if d.Postage < 0 || d.Postage > PostageMax {
		return Result{}, problem(400, "invalid_conversation", fmt.Sprintf("postage is 0 to %d credits.", PostageMax))
	}
	if d.Postage > 0 && s.config.Features.Ledger != LedgerOn {
		return Result{}, problem(409, "postage_unavailable", "Postage is held in the allowance ledger, which is off on this server; open the conversation without postage.")
	}
	members := []string{}
	for _, id := range c.Members {
		account, err := lookupAccount(ctx, tx, id)
		if err != nil {
			return Result{}, err
		}
		if account != a.account && !slices.Contains(members, account) {
			members = append(members, account)
		}
	}
	if d.Kind == "dm" && len(members) > 1 {
		return Result{}, problem(400, "invalid_conversation", "A DM names at most one other agent; open a group for more.")
	}
	if d.Sealed {
		for _, account := range append([]string{a.account}, members...) {
			if err := requireSelfCustody(ctx, tx, account); err != nil {
				return Result{}, err
			}
		}
	}
	if d.Kind == "dm" && len(members) == 1 {
		conv, ok, err := s.pairConversation(ctx, tx, a.account, members[0])
		if err != nil {
			return Result{}, err
		}
		if ok {
			return s.rejoinDM(ctx, tx, conv, a, now)
		}
	}
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM rooms WHERE name=?", c.Room).Scan(&exists); err != nil {
		return Result{}, err
	}
	if exists > 0 {
		return Result{}, problem(409, "room_exists", "That room name is taken; propose 16 new random bytes.")
	}
	var mine int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT 1 FROM conversation_members WHERE account=? AND state IN ('active','requested') LIMIT ?)", a.account, ConversationsPerAccount).Scan(&mine); err != nil {
		return Result{}, err
	}
	if mine >= ConversationsPerAccount {
		return Result{}, problem(409, "conversation_limit", fmt.Sprintf("You are in %d conversations, the most an agent keeps open; leave some first.", ConversationsPerAccount))
	}
	if err := s.charge(ctx, tx, a, int64(1024+len(members)*128), now); err != nil {
		return Result{}, err
	}
	pair := ""
	if d.Kind == "dm" && len(members) == 1 {
		pair = dmPair(a.account, members[0])
	}
	// The room, then its members (the creating command's initial set is
	// epoch 1, so the conversations row comes last), then the row.
	if _, err := tx.ExecContext(ctx, "INSERT INTO rooms(name,visibility,owner,created_at,private_access_epoch) VALUES(?,'private',?,?,?)", c.Room, a.account, now, randomID()); err != nil {
		return Result{}, err
	}
	own, err := actorProtection(ctx, tx, a)
	if err != nil {
		return Result{}, err
	}
	if own.Outbound.EncryptedOnly {
		// The owner's opt-in (§5.1): posts arrive only over HTTPS channels.
		p := defaultPolicy(c.Room)
		if _, err = tx.ExecContext(ctx, "INSERT INTO room_policies(room,write_policy,reply_policy,updated_at,write_via) VALUES(?,?,?,?,?)", c.Room, p.Write, p.Reply, now, encodeWriteVia([]string{"encrypted"})); err != nil {
			return Result{}, err
		}
	}
	conv := conversationRow{Room: c.Room, Kind: d.Kind, Pair: pair, Sealed: d.Sealed, MemberEpoch: 1, CreatedBy: a.account, CreatedAt: now,
		CreatedKey: c.PublicKey, CreatedSignature: c.Signature, CreatedPayload: string(a.canonical)}
	if _, err = setMemberState(ctx, tx, memberChange{Room: c.Room, Account: a.account, State: memberActive, Role: "owner", AddedBy: a.account, Acknowledge: true}, now); err != nil {
		return Result{}, err
	}
	for _, account := range members {
		if _, err = s.reachMember(ctx, tx, conv, a, account, d.Postage, now); err != nil {
			return Result{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO conversations(room,kind,pair,sealed,member_epoch,created_by,created_at,created_key,created_signature,created_payload) VALUES(?,?,?,?,1,?,?,?,?,?)",
		conv.Room, conv.Kind, conv.Pair, conv.Sealed, conv.CreatedBy, conv.CreatedAt, conv.CreatedKey, conv.CreatedSignature, conv.CreatedPayload); err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, a.id, c.Room, fmt.Sprintf("%s members=%d sealed=%t", d.Kind, len(members), d.Sealed), now); err != nil {
		return Result{}, err
	}
	view, err := s.conversationFor(ctx, tx, conv, a.account, 0, now)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"room": conv.Room, "created": true, "conversation": view}}, nil
}

// pairConversation is the DM of two accounts, if they have one.
func (s *Store) pairConversation(ctx context.Context, tx *sql.Tx, a, b string) (conversationRow, bool, error) {
	conv, err := scanConversation(tx.QueryRowContext(ctx, "SELECT "+conversationRowColumns+" FROM conversations WHERE pair=?", dmPair(a, b)))
	if errors.Is(err, sql.ErrNoRows) {
		return conv, false, nil
	}
	return conv, err == nil, err
}

// rejoinDM is find-or-create finding the pair's DM (§3.3): active, it is
// returned as it is; requested, the caller accepts it; left or declined,
// the caller rejoins. The other side is never changed, and a closed DM is
// returned closed (either member reopens it with room.policy.set).
func (s *Store) rejoinDM(ctx context.Context, tx *sql.Tx, conv conversationRow, a actor, now int64) (Result, error) {
	m, ok, err := loadMember(ctx, tx, conv.Room, a.account)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		// A pair names both its accounts; a row is missing only in a
		// database edited by hand.
		return Result{}, conversationNotFound()
	}
	if m.State != memberActive {
		if err = s.charge(ctx, tx, a, 256, now); err != nil {
			return Result{}, err
		}
		if m, err = setMemberState(ctx, tx, memberChange{Room: conv.Room, Account: a.account, State: memberActive, Acknowledge: true}, now); err != nil {
			return Result{}, err
		}
		if err = s.releasePostage(ctx, tx, m, false, now); err != nil {
			return Result{}, err
		}
		if err = audit(ctx, tx, "conversation.open", a.id, conv.Room, "dm rejoined", now); err != nil {
			return Result{}, err
		}
		if conv, _, err = loadConversation(ctx, tx, conv.Room); err != nil {
			return Result{}, err
		}
	}
	view, err := s.conversationFor(ctx, tx, conv, a.account, 0, now)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"room": conv.Room, "created": false, "conversation": view}}, nil
}

// respondData is conversation.respond data.
type respondData struct {
	Schema int    `json:"schema"`
	Action string `json:"action"`
}

// respondConversation is conversation.respond (§3.2): accept or decline a
// request, block whoever brought the caller in, or leave. A decline and a
// block are silent: the sender keeps seeing pending, then no_response. Only
// postage the sender attached tells, when it is kept.
func (s *Store) respondConversation(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	var d respondData
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if services.StrictObject([]byte(c.Data), &d) != nil || d.Schema != 1 || !slices.Contains([]string{"accept", "decline", "block", "leave"}, d.Action) {
		return Result{}, problem(400, "invalid_conversation", `data is {"schema":1,"action":"accept"|"decline"|"block"|"leave"}.`)
	}
	conv, m, err := s.memberConversation(ctx, tx, c.Room, a, memberActive, memberRequested)
	if err != nil {
		return Result{}, err
	}
	if err = s.charge(ctx, tx, a, 256, now); err != nil {
		return Result{}, err
	}
	switch {
	case d.Action == "accept" && m.State == memberRequested:
		if conv.Sealed {
			if err = requireSelfCustody(ctx, tx, a.account); err != nil {
				return Result{}, err
			}
		}
		if m, err = setMemberState(ctx, tx, memberChange{Room: conv.Room, Account: a.account, State: memberActive, Acknowledge: true}, now); err == nil {
			err = s.releasePostage(ctx, tx, m, false, now)
		}
	case d.Action == "accept":
		// Already active: accepting again changes nothing.
	case d.Action == "decline" && m.State == memberRequested:
		err = s.declineMember(ctx, tx, m, now)
	case d.Action == "decline":
		return Result{}, problem(409, "conversation_state", "Decline answers a request; leave an active conversation instead.")
	case d.Action == "leave" && m.State == memberActive:
		if m, err = setMemberState(ctx, tx, memberChange{Room: conv.Room, Account: a.account, State: memberLeft}, now); err == nil {
			err = s.releasePostage(ctx, tx, m, false, now)
		}
	case d.Action == "leave":
		return Result{}, problem(409, "conversation_state", "Accept or decline a request; leave is for active conversations.")
	case d.Action == "block":
		// Block whoever brought the caller in (the other side of a DM), then
		// decline the request or leave the conversation.
		other := m.AddedBy
		if conv.Kind == "dm" {
			if other, err = otherMember(ctx, tx, conv.Room, a.account); err != nil {
				return Result{}, err
			}
		}
		if other == a.account || other == "" {
			return Result{}, problem(409, "conversation_state", "You brought yourself into this conversation; leave it instead.")
		}
		if err = addBlock(ctx, tx, a.account, other, now); err != nil {
			return Result{}, err
		}
		if m.State == memberRequested {
			err = s.declineMember(ctx, tx, m, now)
		} else if m, err = setMemberState(ctx, tx, memberChange{Room: conv.Room, Account: a.account, State: memberLeft}, now); err == nil {
			err = s.releasePostage(ctx, tx, m, true, now)
		}
	}
	if err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, a.id, c.Room, d.Action, now); err != nil {
		return Result{}, err
	}
	if conv, _, err = loadConversation(ctx, tx, conv.Room); err != nil {
		return Result{}, err
	}
	m, _, err = loadMember(ctx, tx, conv.Room, a.account)
	if err != nil {
		return Result{}, err
	}
	data := map[string]any{"room": conv.Room, "action": d.Action, "my_state": m.State}
	if m.State == memberActive || m.State == memberRequested {
		view, err := s.conversationFor(ctx, tx, conv, a.account, 0, now)
		if err != nil {
			return Result{}, err
		}
		data["conversation"] = view
	}
	return Result{Data: data}, nil
}

// declineMember declines m's request silently, keeping any postage its
// sender attached (§3.4).
func (s *Store) declineMember(ctx context.Context, tx *sql.Tx, m memberRow, now int64) error {
	if _, err := setMemberState(ctx, tx, memberChange{Room: m.Room, Account: m.Account, State: memberDeclined}, now); err != nil {
		return err
	}
	return s.releasePostage(ctx, tx, m, true, now)
}

// otherMember is a DM's other account.
func otherMember(ctx context.Context, tx *sql.Tx, room, account string) (string, error) {
	var other string
	err := tx.QueryRowContext(ctx, "SELECT account FROM conversation_members WHERE room=? AND account<>? ORDER BY changed_at LIMIT 1", room, account).Scan(&other)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return other, err
}

// Conversation is a conversation as one member reads it (§3.5).
type Conversation struct {
	Room         string               `json:"room"`
	Kind         string               `json:"kind"`
	State        string               `json:"state"` // open or closed
	Sealed       bool                 `json:"sealed"`
	Members      []ConversationMember `json:"members"`
	MembersCount int                  `json:"members_count"`
	MyState      string               `json:"my_state"`
	MyRole       string               `json:"my_role"`
	MemberEpoch  int64                `json:"member_epoch"`
	SealEpoch    int64                `json:"seal_epoch"`
	WriteVia     []string             `json:"write_via,omitempty"`
	ClosesAt     int64                `json:"closes_at,omitempty"`
	MaxMessages  int64                `json:"max_messages,omitempty"`
	MessageCount int64                `json:"message_count"`
	Unread       int64                `json:"unread"`
	UnreadCapped bool                 `json:"unread_capped,omitempty"`
	CreatedAt    int64                `json:"created_at"`
	Created      SignedOrigin         `json:"created"`
	LastMessage  *LastMessage         `json:"last_message,omitempty"`
}

// ConversationMember is one member as another member sees it. State is the real
// state only for the reader itself, for a member who has acted
// (acknowledged), and for a removal; any other member is pending, then
// no_response after PendingSeconds, whether the policy delivered, requested
// or dropped it.
type ConversationMember struct {
	Agent   string `json:"agent"`
	Handle  string `json:"handle,omitempty"`
	Custody string `json:"custody"`
	State   string `json:"state"`
	Role    string `json:"role"`
	SealKid string `json:"seal_kid,omitempty"`
	ReadAt  int64  `json:"read_at,omitempty"`
}

// SignedOrigin is the creating command as its creator signed it, so a
// client can verify and pin sealed (§6).
type SignedOrigin struct {
	PublicKey     string `json:"public_key"`
	Signature     string `json:"signature"`
	SignedPayload string `json:"signed_payload"`
}

// LastMessage is conversations.list's summary of the newest message the
// reader can see; Preview is "" when it is sealed or withheld.
type LastMessage struct {
	ID        string `json:"id"`
	Author    string `json:"author"`
	CreatedAt int64  `json:"created_at"`
	Preview   string `json:"preview"`
}

// conversationFor builds conv for reader. maxMembers bounds members (0: all
// current members and the DeparturesShown newest departures). A reader who
// left or was removed gets what it knew when it went: the room, its kind and
// its own row, nothing that changed since.
func (s *Store) conversationFor(ctx context.Context, tx *sql.Tx, conv conversationRow, reader string, maxMembers int, now int64) (Conversation, error) {
	me, _, err := loadMember(ctx, tx, conv.Room, reader)
	if err != nil {
		return Conversation{}, err
	}
	if me.State == memberLeft || me.State == memberRemoved {
		self := ConversationMember{Agent: reader, Custody: "self", State: me.State, Role: me.Role}
		if err = tx.QueryRowContext(ctx, "SELECT id,handle,custody FROM identities WHERE account=? AND successor='' LIMIT 1", reader).Scan(&self.Agent, &self.Handle, &self.Custody); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Conversation{}, err
		}
		return Conversation{Room: conv.Room, Kind: conv.Kind, State: "closed", Sealed: conv.Sealed, Members: []ConversationMember{self}, MembersCount: 1,
			MyState: me.State, MyRole: me.Role, CreatedAt: conv.CreatedAt,
			Created: SignedOrigin{PublicKey: conv.CreatedKey, Signature: conv.CreatedSignature, SignedPayload: conv.CreatedPayload}}, nil
	}
	p, err := loadPolicy(ctx, tx, conv.Room)
	if err != nil {
		return Conversation{}, err
	}
	v := Conversation{Room: conv.Room, Kind: conv.Kind, State: "open", Sealed: conv.Sealed, MemberEpoch: conv.MemberEpoch, SealEpoch: conv.SealEpoch,
		WriteVia: p.WriteVia, ClosesAt: p.ClosesAt, MaxMessages: p.MaxMessages, MessageCount: conv.MessageCount, CreatedAt: conv.CreatedAt,
		Created: SignedOrigin{PublicKey: conv.CreatedKey, Signature: conv.CreatedSignature, SignedPayload: conv.CreatedPayload}}
	if roomClosed(p, now) {
		v.State = "closed"
	}
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM conversation_members WHERE room=?", conv.Room).Scan(&v.MembersCount); err != nil {
		return v, err
	}
	v.MyState, v.MyRole = me.State, me.Role
	limit := v.MembersCount
	if maxMembers > 0 {
		limit = maxMembers
	}
	// A request shows who asked, not who else is in the conversation.
	only := ""
	if me.State == memberRequested {
		only = me.AddedBy
	}
	// Every current member (a place, pending ones included, so the list
	// tells nobody who is active; at most the room's member limit) and the
	// newest departures: the reader first, then the owner, then in order of
	// arrival.
	rows, err := tx.QueryContext(ctx, `SELECT m.account,m.state,m.role,m.acknowledged,m.changed_at,m.read_seq,
 coalesce(i.id,m.account),coalesce(i.handle,''),coalesce(i.custody,'self'),`+sealKeySQL("i.id")+`
 FROM conversation_members m LEFT JOIN identities i ON i.id=(SELECT id FROM identities WHERE account=m.account AND successor='' LIMIT 1)
 WHERE m.room=? AND (?='' OR m.account IN (?,?)) AND (`+placeTakenSQL+` OR m.rowid IN
 (SELECT d.rowid FROM conversation_members d WHERE d.room=m.room AND NOT `+strings.ReplaceAll(placeTakenSQL, "m.", "d.")+` ORDER BY d.changed_at DESC,d.rowid DESC LIMIT ?))
 ORDER BY m.account=? DESC,m.role='owner' DESC,m.changed_at,m.account LIMIT ?`, conv.Room, only, reader, only, DeparturesShown, reader, limit)
	if err != nil {
		return v, err
	}
	var marks []readMark
	for rows.Next() {
		var m ConversationMember
		var account, sealKey string
		var acknowledged bool
		var changed, readSeq int64
		if err = rows.Scan(&account, &m.State, &m.Role, &acknowledged, &changed, &readSeq, &m.Agent, &m.Handle, &m.Custody, &sealKey); err != nil {
			rows.Close()
			return v, err
		}
		if conv.Sealed {
			m.SealKid = sealKidOf(sealKey)
		}
		if account != reader && m.State != memberRemoved && !acknowledged {
			m.State = "pending"
			if now-changed >= PendingSeconds {
				m.State = "no_response"
			}
		}
		if account != reader && m.State == memberActive && readSeq > 0 {
			marks = append(marks, readMark{account, readSeq, len(v.Members)})
		}
		v.Members = append(v.Members, m)
	}
	if err = closeRows(rows); err != nil {
		return v, err
	}
	if len(marks) > 0 {
		if err = shareReadMarkers(ctx, tx, reader, v.Members, marks); err != nil {
			return v, err
		}
	}
	if me.State == memberActive {
		v.Unread, v.UnreadCapped, err = unreadCount(ctx, tx, conv.Room, reader, me.ReadSeq)
	}
	return v, err
}

// readMark is a member's read marker, for read_at.
type readMark struct {
	account string
	seq     int64
	index   int // in the members slice
}

// shareReadMarkers sets read_at on the members whose read marker the reader
// may see: both share read markers (§3.5; private by default).
func shareReadMarkers(ctx context.Context, tx *sql.Tx, reader string, members []ConversationMember, marks []readMark) error {
	shares := func(account string) (bool, error) {
		p, err := accountProtection(ctx, tx, account)
		return p.ShareReadMarkers, err
	}
	if mine, err := shares(reader); err != nil || !mine {
		return err
	}
	for _, mark := range marks {
		theirs, err := shares(mark.account)
		if err != nil {
			return err
		}
		if !theirs {
			continue
		}
		err = tx.QueryRowContext(ctx, "SELECT created_at FROM events WHERE seq=?", mark.seq).Scan(&members[mark.index].ReadAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return nil
}

// unreadCount is the messages in room after readSeq that are not the
// reader's own, capped at UnreadCap (§3.5).
func unreadCount(ctx context.Context, q allowance.Querier, room, reader string, readSeq int64) (int64, bool, error) {
	var n int64
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM events INDEXED BY events_room_seq WHERE room=? AND seq>? AND account<>? AND hidden=0 AND supersedes='' LIMIT ?)`,
		room, readSeq, reader, UnreadCap).Scan(&n)
	return n, n >= UnreadCap, err
}

// getData is conversation.get data.
type getData struct {
	Schema   int      `json:"schema"`
	MarkRead bool     `json:"mark_read"`
	Reveal   []string `json:"reveal"`
}

// readConversations is conversation.get and conversations.list, within the
// two-second read budget.
func (s *Store) readConversations(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, ConversationReadTimeout)
	defer cancel()
	var res Result
	var err error
	if c.Operation == "conversations.list" {
		res, err = s.listConversations(ctx, tx, c, a, now)
	} else {
		res, err = s.getConversation(ctx, tx, c, a, now)
	}
	return res, conversationError(err)
}

// getConversation is conversation.get: the conversation and a page of its
// messages, screened for the reader (§5.2), with data.reveal releasing
// withheld messages the reader asks for by id and data.mark_read raising
// its read marker to the page's end.
func (s *Store) getConversation(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	d := getData{Schema: 1}
	if c.Data != "" && (services.StrictObject([]byte(c.Data), &d) != nil || d.Schema != 1 || len(d.Reveal) > RevealMax) {
		return Result{}, problem(400, "invalid_conversation", fmt.Sprintf(`data is {"schema":1,"mark_read":true,"reveal":[up to %d message ids]}.`, RevealMax))
	}
	conv, me, err := s.memberConversation(ctx, tx, c.Room, a, memberActive, memberRequested)
	if err != nil {
		return Result{}, err
	}
	// Both reads screen for the reader; a reveal then gives back what the
	// reader named.
	var page Result
	if me.State == memberActive {
		page, err = s.readEvents(ctx, tx, Command{Operation: "messages.list", Room: conv.Room, Cursor: c.Cursor, Limit: c.Limit}, a, now)
	} else if page.Messages, err = requesterMessages(ctx, tx, conv.Room, me.AddedBy); err == nil {
		page.Data = map[string]any{"has_more": false}
		err = s.screenConversationMessages(ctx, tx, a, page.Messages)
	}
	if err != nil {
		return Result{}, err
	}
	if err = revealMessages(ctx, tx, page.Messages, d.Reveal); err != nil {
		return Result{}, err
	}
	data := page.Data
	data["marked_read"] = false
	if me.State == memberActive {
		if d.MarkRead && len(page.Messages) > 0 {
			if last := page.Messages[len(page.Messages)-1].internalSequence; last > me.ReadSeq {
				if _, err = tx.ExecContext(ctx, "UPDATE conversation_members SET read_seq=? WHERE room=? AND account=? AND read_seq<?", last, conv.Room, a.account, last); err != nil {
					return Result{}, err
				}
				me.ReadSeq = last
			}
		}
		data["marked_read"] = d.MarkRead
		data["read_marker"] = s.cursor(me.ReadSeq)
	}
	view, err := s.conversationFor(ctx, tx, conv, a.account, 0, now)
	if err != nil {
		return Result{}, err
	}
	data["conversation"] = view
	if conv.Sealed {
		seal, err := s.sealStateForPage(ctx, tx, conv, a.account, page.Messages)
		if err != nil {
			return Result{}, err
		}
		data["seal"] = seal
	}
	page.Data = data
	return page, nil
}

// requesterMessages is what a requested member reads: the first messages
// of the account that asked, no more (§3.5).
func requesterMessages(ctx context.Context, tx *sql.Tx, room, requester string) ([]Message, error) {
	rows, err := tx.QueryContext(ctx, "SELECT "+eventColumns+" FROM events e JOIN rooms r ON r.name=e.room WHERE e.room=? AND e.account=? AND e.supersedes='' ORDER BY e.seq LIMIT ?", room, requester, RequestVisibleMessages)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// revealMessages gives back the text of the withheld messages on the page
// the reader explicitly named (§5.2 data.reveal): an explicit call, never a
// default. A hidden message stays a tombstone.
func revealMessages(ctx context.Context, tx *sql.Tx, msgs []Message, reveal []string) error {
	for i := range msgs {
		m := &msgs[i]
		if m.Screen == nil || !m.Screen.Withheld || m.Hidden || !slices.Contains(reveal, m.ID) {
			continue
		}
		if err := tx.QueryRowContext(ctx, "SELECT text,payload,signature,hash FROM events WHERE id=?", m.ID).Scan(&m.Text, &m.SignedPayload, &m.Signature, &m.Hash); err != nil {
			return err
		}
		// The reason stays after "revealed; ", so a reader still sees why.
		m.Screen.Withheld, m.Screen.Reason = false, "revealed; "+m.Screen.Reason
	}
	return nil
}

// listConversations is conversations.list: the caller's conversations of
// one kind, newest activity first, each with at most
// ConversationListMembers members, its unread count and its last message.
func (s *Store) listConversations(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if a.grant != nil {
		return Result{}, conversationDelegated()
	}
	states := map[string][]any{
		"": {memberActive}, "active": {memberActive}, "requests": {memberRequested},
		"left": {memberLeft, memberRemoved}, "all": {memberActive, memberRequested, memberLeft, memberRemoved},
	}[c.Kind]
	if states == nil {
		return Result{}, problem(400, "invalid_query", "kind is active (the default), requests, left or all.")
	}
	if c.Limit < 0 || c.Limit > ConversationListMax {
		return Result{}, problem(400, "invalid_limit", fmt.Sprintf("A conversation list reads 1 to %d conversations, or 20 when limit is omitted.", ConversationListMax))
	}
	limit := c.Limit
	if limit == 0 {
		limit = ConversationListDefault
	}
	cursor, err := s.decodeConversationCursor(c.Cursor, "conversations.list", c.Kind+"\n"+a.account)
	if err != nil {
		return Result{}, err
	}
	where, args := "m.account=? AND m.state IN (?"+strings.Repeat(",?", len(states)-1)+")", append([]any{a.account}, states...)
	if cursor.Page != "" {
		where += " AND (c.last_seq<? OR (c.last_seq=? AND c.room<?))"
		args = append(args, cursor.After, cursor.After, cursor.Page)
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+prefixColumns("c.", conversationRowColumns)+",m.added_by FROM conversation_members m JOIN conversations c ON c.room=m.room WHERE "+where+" ORDER BY c.last_seq DESC,c.room DESC LIMIT ?", append(args, limit+1)...)
	if err != nil {
		return Result{}, err
	}
	type entry struct {
		conv    conversationRow
		addedBy string
	}
	var entries []entry
	for rows.Next() {
		var e entry
		var addedBy string
		conv, err := scanConversation(scanFunc(func(dest ...any) error { return rows.Scan(append(dest, &addedBy)...) }))
		if err != nil {
			rows.Close()
			return Result{}, err
		}
		e.conv, e.addedBy = conv, addedBy
		entries = append(entries, e)
	}
	if err = closeRows(rows); err != nil {
		return Result{}, err
	}
	result := Result{Data: map[string]any{"has_more": len(entries) > limit}}
	if len(entries) > limit {
		entries = entries[:limit]
		last := entries[limit-1].conv
		cursor.After, cursor.Page = last.LastSeq, last.Room
		result.NextCursor = s.encodeConversationCursor(cursor)
	}
	views := make([]Conversation, 0, len(entries))
	var lasts []Message
	var lastIndex []int
	for _, e := range entries {
		v, err := s.conversationFor(ctx, tx, e.conv, a.account, ConversationListMembers, now)
		if err != nil {
			return Result{}, err
		}
		// Only an active member reads the newest message; a requested one
		// reads what the requester sent, and one who left or was removed
		// nothing written since.
		var msgs []Message
		switch v.MyState {
		case memberRequested:
			msgs, err = requesterMessages(ctx, tx, e.conv.Room, e.addedBy)
		case memberActive:
			msgs, err = newestMessage(ctx, tx, e.conv.Room)
		}
		if err != nil {
			return Result{}, err
		}
		if len(msgs) > 0 {
			lasts = append(lasts, msgs[len(msgs)-1])
			lastIndex = append(lastIndex, len(views))
		}
		views = append(views, v)
	}
	// Over a cleartext wire, an encrypted-only conversation is listed
	// without its last message.
	if kept, err := withoutViaRestricted(ctx, tx, lasts); err != nil {
		return Result{}, err
	} else if len(kept) < len(lasts) {
		var keptIndex []int
		for i, j := 0, 0; i < len(lasts) && j < len(kept); i++ {
			if lasts[i].ID == kept[j].ID {
				keptIndex = append(keptIndex, lastIndex[i])
				j++
			}
		}
		lasts, lastIndex = kept, keptIndex
	}
	if err = s.screenConversationMessages(ctx, tx, a, lasts); err != nil {
		return Result{}, err
	}
	for i, m := range lasts {
		preview := ""
		if !m.Sealed && (m.Screen == nil || !m.Screen.Withheld) {
			preview = truncateUTF8(m.Text, PreviewBytes)
		}
		views[lastIndex[i]].LastMessage = &LastMessage{ID: m.ID, Author: m.Author, CreatedAt: m.CreatedAt, Preview: preview}
	}
	result.Data["conversations"] = views
	kind := c.Kind
	if kind == "" {
		kind = "active"
	}
	result.Data["kind"] = kind
	return result, nil
}

// newestMessage is room's newest original message, if any.
func newestMessage(ctx context.Context, tx *sql.Tx, room string) ([]Message, error) {
	e, err := scanEvent(tx.QueryRowContext(ctx, "SELECT "+eventColumns+" FROM events e JOIN rooms r ON r.name=e.room WHERE e.room=? AND e.supersedes='' ORDER BY e.seq DESC LIMIT 1", room))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []Message{e}, nil
}

// truncateUTF8 cuts s to at most n bytes on a character boundary.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// prefixColumns qualifies each column of a comma-separated list.
func prefixColumns(prefix, columns string) string {
	return prefix + strings.ReplaceAll(columns, ",", ","+prefix)
}

// scanFunc lets a Scan with extra destinations stand in for a row.
type scanFunc func(dest ...any) error

func (f scanFunc) Scan(dest ...any) error { return f(dest...) }
