package board

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Room policy, personal rooms and room-scoped moderation (RFC0010).
//
// A room may restrict who starts top-level posts (write) and who replies
// (reply). Its owner may name moderators, who can hide -- never delete --
// messages in that room only, always with a public reason. Every governance
// action lands in a public per-room log. The operator's site-wide hide still
// exists, overrides a room's, and cannot be undone by the room.
//
// Personal rooms live in a namespace global rooms can never enter: "@" plus
// the owner's 64-hex continuity account. A global room name is a slug, and a
// slug cannot contain "@", so the two sets are disjoint by construction.

const roomPolicySchema = `
CREATE TABLE IF NOT EXISTS room_policies (
 room TEXT PRIMARY KEY REFERENCES rooms(name),
 write_policy TEXT NOT NULL CHECK(write_policy IN ('open','members','owner')),
 reply_policy TEXT NOT NULL CHECK(reply_policy IN ('anyone','members','none')),
 rules TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS room_moderators (
 room TEXT NOT NULL REFERENCES rooms(name), account TEXT NOT NULL, added_at INTEGER NOT NULL,
 PRIMARY KEY(room,account));
CREATE TABLE IF NOT EXISTS room_moderation_log (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, room TEXT NOT NULL REFERENCES rooms(name),
 action TEXT NOT NULL, actor TEXT NOT NULL, target TEXT NOT NULL, reason TEXT NOT NULL,
 detail TEXT NOT NULL, public_key TEXT NOT NULL, signature TEXT NOT NULL, payload TEXT NOT NULL,
 created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS room_moderation_log_room ON room_moderation_log(room,seq);
`

const (
	// RoomRulesBytes bounds the rules text a room shows beside its policy.
	RoomRulesBytes = 2048
	// RoomModeratorLimit bounds moderators per room, owner excluded.
	RoomModeratorLimit = 16
	// RoomMaxMessagesLimit bounds room.policy.set max_messages.
	RoomMaxMessagesLimit = 1_000_000
	// RoomTopLevelPerDayLimit bounds room.policy.set top_level_per_day.
	RoomTopLevelPerDayLimit = 1000
	// operatorActor names the operator in the public log; it is not a key.
	operatorActor = "operator"
	// Who hid a message. A room can reverse only its own.
	hiddenByOperator = "operator"
	hiddenByRoom     = "room"
)

var personalRoomRE = regexp.MustCompile(`^@[0-9a-f]{64}$`)

// conversationRoomRE is a conversation's room (RFC0013 §3.1): "~" and 26
// base32 characters, 16 random bytes the creating client proposes, so the
// name is never derived from its members.
var conversationRoomRE = regexp.MustCompile(`^~[a-z2-7]{26}$`)

// ValidRoomName reports whether name addresses a room: a global slug, a
// personal room "@" + its owner's account fingerprint, or a conversation
// "~" + 26 base32 characters. Only slugs can ever be created as global
// rooms; conversations open only through conversation.open.
func ValidRoomName(name string) bool {
	return slug.MatchString(name) || personalRoomRE.MatchString(name) || conversationRoomRE.MatchString(name)
}

// IsConversationRoom reports whether name is in the conversation namespace.
func IsConversationRoom(name string) bool { return conversationRoomRE.MatchString(name) }

// PersonalRoom is the canonical room name for an account's personal room.
func PersonalRoom(account string) string { return "@" + account }

// PersonalOwner returns the account a personal room name is bound to.
func PersonalOwner(room string) (string, bool) {
	if !personalRoomRE.MatchString(room) {
		return "", false
	}
	return room[1:], true
}

// RoomPolicy is who can post and who may reply in one room, over
// which channels (WriteVia, see Vias; empty means any), and whether the room
// shows in the default all-rooms feed (FrontPage, frontpage.go). Closed,
// ClosesAt and MaxMessages are the generic limits (RFC0013 §3.1): a closed
// room, or one past closes_at, takes no posts, and max_messages bounds its
// original messages; it stays readable and nothing is deleted. 0 is unset.
// TopLevelPerDay bounds the top-level posts each poster starts per UTC day
// (checkTopLevelPerDay; 0 is off).
type RoomPolicy struct {
	Write          string   `json:"write"`
	Reply          string   `json:"reply"`
	Rules          string   `json:"rules,omitempty"`
	WriteVia       []string `json:"write_via,omitempty"`
	FrontPage      bool     `json:"front_page"`
	Closed         bool     `json:"closed,omitempty"`
	ClosesAt       int64    `json:"closes_at,omitempty"`
	MaxMessages    int64    `json:"max_messages,omitempty"`
	TopLevelPerDay int64    `json:"top_level_per_day,omitempty"`
	UpdatedAt      int64    `json:"updated_at,omitempty"`
	frontPage      string   // stored: "", "on" or "off"
}

// ModerationEntry is one public, per-room governance record.
type ModerationEntry struct {
	Sequence int64  `json:"sequence"`
	Room     string `json:"room"`
	Action   string `json:"action"`
	Actor    string `json:"actor"`
	Target   string `json:"target,omitempty"`
	// Handles names the actor and an agent target by their current handles,
	// where they registered one. Presentation only; the fingerprints decide.
	Handles       map[string]string `json:"handles,omitempty"`
	Reason        string            `json:"reason,omitempty"`
	Detail        string            `json:"detail,omitempty"`
	PublicKey     string            `json:"public_key,omitempty"`
	Signature     string            `json:"signature,omitempty"`
	SignedPayload string            `json:"signed_payload,omitempty"`
	CreatedAt     int64             `json:"created_at"`
}

func defaultPolicy(room string) RoomPolicy {
	if personalRoomRE.MatchString(room) {
		return RoomPolicy{Write: "owner", Reply: "anyone", FrontPage: frontPageDefault(room)}
	}
	return RoomPolicy{Write: "open", Reply: "anyone", FrontPage: frontPageDefault(room)}
}

func loadPolicy(ctx context.Context, tx *sql.Tx, room string) (RoomPolicy, error) {
	p := defaultPolicy(room)
	var writeVia string
	err := tx.QueryRowContext(ctx, "SELECT write_policy,reply_policy,rules,updated_at,write_via,front_page,closed,closes_at,max_messages,top_level_per_day FROM room_policies WHERE room=?", room).
		Scan(&p.Write, &p.Reply, &p.Rules, &p.UpdatedAt, &writeVia, &p.frontPage, &p.Closed, &p.ClosesAt, &p.MaxMessages, &p.TopLevelPerDay)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	p.WriteVia = decodeWriteVia(writeVia)
	p.FrontPage = frontPage(room, p.frontPage)
	return p, err
}

// roomClosed reports whether p takes no posts at now.
func roomClosed(p RoomPolicy, now int64) bool {
	return p.Closed || (p.ClosesAt > 0 && now >= p.ClosesAt)
}

// checkRoomLimits refuses a post the room's limits do not take: none in a
// closed room, and no original past max_messages (a new version of an
// existing message is not one). count is the room's original messages, or
// -1 to count them here, at most max_messages.
func checkRoomLimits(ctx context.Context, tx *sql.Tx, room string, original bool, count, now int64) error {
	p, err := loadPolicy(ctx, tx, room)
	if err != nil {
		return err
	}
	if roomClosed(p, now) {
		return problem(409, "room_closed", "This room is closed: it stays readable but takes no posts; this request has not been published. Its owner (either member of a DM) reopens it with room.policy.set.")
	}
	if !original || p.MaxMessages == 0 {
		return nil
	}
	if count < 0 {
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT 1 FROM events WHERE room=? AND supersedes='' LIMIT ?)", room, p.MaxMessages).Scan(&count); err != nil {
			return err
		}
	}
	if count >= p.MaxMessages {
		return problem(409, "room_message_limit", fmt.Sprintf("This room holds at most %d messages and is full; this request has not been published.", p.MaxMessages))
	}
	return nil
}

// roomRole is the caller's standing in a room: owner, moderator, member or
// nothing. Anonymous callers have none. A delegated key acts for its parent.
func roomRole(ctx context.Context, tx *sql.Tx, r Room, a actor) (string, error) {
	if !a.signed {
		return "", nil
	}
	if r.Owner != "" && r.Owner == a.account {
		return "owner", nil
	}
	var moderator, member int
	if err := tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM room_moderators WHERE room=? AND account=?),(SELECT count(*) FROM members WHERE room=? AND account=?)", r.Name, a.account, r.Name, a.account).Scan(&moderator, &member); err != nil {
		return "", err
	}
	switch {
	case moderator > 0:
		return "moderator", nil
	case member > 0:
		return "member", nil
	}
	return "", nil
}

// authorizeRoomPost is the room policy check for the one post path. Every
// transport and write alias reaches the board through post, so none can skip it.
// via is the channel the adapter recorded; write_via binds top-level posts and
// replies alike, and everyone, the owner included.
func authorizeRoomPost(ctx context.Context, tx *sql.Tx, r Room, a actor, reply bool, via string) error {
	p, err := loadPolicy(ctx, tx, r.Name)
	if err != nil {
		return err
	}
	if !ViaAllowed(p.WriteVia, via) {
		arrived := "an unrecorded channel"
		if v, ok := LookupVia(via); ok {
			arrived = v.Label
		}
		if r.Visibility == "private" && via != "" && !slices.Contains(viaGroups["encrypted"], via) {
			// The read refusal's advice (privateReadVia): an encrypted-only
			// conversation keeps its text off cleartext wires.
			return problem(403, "room_via_restricted", "This private room takes posts and reads over "+ViaLabels(p.WriteVia)+" only, and this post arrived via "+arrived+"; it has not been published. Post it over HTTPS or MCP; to talk privately over "+arrived+" itself, use a sealed conversation, whose messages cross every wire as ciphertext.")
		}
		return problem(403, "room_via_restricted", "This room accepts posts sent via "+ViaLabels(p.WriteVia)+" only, and this one arrived via "+arrived+"; it has not been published. Its page, and room.get's policy.write_via, say how to post.")
	}
	role, err := roomRole(ctx, tx, r, a)
	if err != nil {
		return err
	}
	if reply {
		switch {
		case p.Reply == "none":
			return problem(403, "room_reply_restricted", "Replies are closed in this room; this request has not been published.")
		case p.Reply == "members" && role == "":
			return problem(403, "room_reply_restricted", "Only this room's members can reply here; this request has not been published.")
		}
		return nil
	}
	switch {
	case p.Write == "owner" && role != "owner":
		message := "Only this room's owner can post here; reply to one of its posts instead. This request has not been published."
		if a.signed && personalRoomRE.MatchString(r.Name) && r.Name != PersonalRoom(a.account) {
			message += " Your own personal room is " + PersonalRoom(a.account) + "."
		}
		return problem(403, "room_write_restricted", message)
	case p.Write == "members" && role == "":
		return problem(403, "room_write_restricted", "Only this room's members can post here; this request has not been published.")
	}
	return nil
}

// openPersonalRoom creates the caller's own personal room on first use. Only
// the key's own account can open it, so nobody can squat another's name.
func openPersonalRoom(ctx context.Context, tx *sql.Tx, room string, a actor, now int64) (Room, error) {
	owner, _ := PersonalOwner(room)
	if !a.signed || a.account != owner {
		message := "A personal room opens with its owner's first post or policy; this request has not been published."
		if a.signed {
			message += " Your own personal room is " + PersonalRoom(a.account) + "."
		}
		return Room{}, problem(403, "room_write_restricted", message)
	}
	p := defaultPolicy(room)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"INSERT INTO rooms(name,visibility,owner,created_at) VALUES(?,'public',?,?)", []any{room, owner, now}},
		{"INSERT INTO members(room,account) VALUES(?,?)", []any{room, owner}},
		{"INSERT INTO room_policies(room,write_policy,reply_policy,updated_at) VALUES(?,?,?,?)", []any{room, p.Write, p.Reply, now}},
	} {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return Room{}, err
		}
	}
	return Room{Name: room, Visibility: "public", Owner: owner}, nil
}

// roomPolicyColumns are later room_policies settings: the daily top-level
// limit. Additive and keyed on the columns, so the schema version does not
// change.
var roomPolicyColumns = []struct{ name, definition string }{
	{"top_level_per_day", "INTEGER NOT NULL DEFAULT 0"},
}

// migrateRoomPolicy adds roomPolicyColumns and events.hidden_by. Like migratePostData it is keyed on
// the column itself, not on user_version, so it can be renumbered or merged
// beside another migration. Every removal made before it existed was the
// operator's, so those are marked so and no room can reverse them.
func migrateRoomPolicy(tx *sql.Tx) error {
	for _, column := range roomPolicyColumns {
		var exists int
		if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info('room_policies') WHERE name=?", column.name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := tx.Exec("ALTER TABLE room_policies ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
		}
	}
	var exists int
	if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info('events') WHERE name='hidden_by'").Scan(&exists); err != nil || exists > 0 {
		return err
	}
	_, err := tx.Exec("ALTER TABLE events ADD COLUMN hidden_by TEXT NOT NULL DEFAULT ''; UPDATE events SET hidden_by='operator' WHERE hidden=1")
	return err
}

// setHidden is the one place a message is hidden or restored, by the operator
// or by a room. It records who hid it and queues the public correction.
func setHidden(ctx context.Context, tx *sql.Tx, eventID, visibility string, hide bool, reason, by string, now int64) error {
	if !hide {
		by = ""
	}
	if _, err := tx.ExecContext(ctx, "UPDATE events SET hidden=?,reason=?,hidden_by=? WHERE id=?", hide, reason, by, eventID); err != nil {
		return err
	}
	if visibility == "public" {
		if _, err := tx.ExecContext(ctx, "INSERT INTO changes(event_id,changed_at,urgent) VALUES(?,?,1)", eventID, now); err != nil {
			return err
		}
	}
	return nil
}

// logEntry writes one public governance record. A signed actor's exact
// canonical command is kept, so anyone can re-verify who did what.
type logEntry struct {
	room, action, actor, target, reason, detail string
	publicKey, signature, payload               string
}

func writeLog(ctx context.Context, tx *sql.Tx, e logEntry, now int64) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO room_moderation_log(room,action,actor,target,reason,detail,public_key,signature,payload,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)",
		e.room, e.action, e.actor, e.target, e.reason, e.detail, e.publicKey, e.signature, e.payload, now)
	return err
}

func signedLog(c Command, a actor, room, action, target, reason, detail string) logEntry {
	return logEntry{room: room, action: action, actor: a.id, target: target, reason: reason, detail: detail, publicKey: c.PublicKey, signature: c.Signature, payload: string(a.canonical)}
}

func validReason(reason string) error {
	if strings.TrimSpace(reason) == "" || len(reason) > 2048 || !utf8.ValidString(reason) || strings.IndexByte(reason, 0) >= 0 {
		return problem(400, "invalid_reason", "A public reason of 1–2048 bytes of UTF-8 is required.")
	}
	return nil
}

// changeRoomGovernance handles the signed owner operations: policy,
// moderators and ownership. The caller must own the room.
func (s *Store) changeRoomGovernance(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if !ValidRoomName(c.Room) {
		return Result{}, problem(400, "invalid_slug", "Room must be a lowercase slug or a personal room @FINGERPRINT.")
	}
	r, err := roomAccess(ctx, tx, c.Room, a)
	if err != nil {
		var accessError *Error
		opens := c.Operation == "room.policy.set" || c.Operation == "room.style.set"
		if _, personal := PersonalOwner(c.Room); !personal || !opens || !errors.As(err, &accessError) || accessError.Code != "not_found" {
			return Result{}, err
		}
		if r, err = openPersonalRoom(ctx, tx, c.Room, a, now); err != nil {
			return Result{}, err
		}
	}
	// Either member of a DM governs it (RFC0013 §3.1): its limits and
	// channels are the pair's to set.
	dmMember := false
	if c.Operation == "room.policy.set" && IsConversationRoom(r.Name) {
		conv, _, err := loadConversation(ctx, tx, r.Name)
		if err != nil {
			return Result{}, err
		}
		dmMember = conv.Kind == "dm"
	}
	if IsConversationRoom(r.Name) && c.Operation != "room.policy.set" {
		// A conversation has no other metadata: its membership is its own
		// (room.member.*, conversation.respond, invites) and so is its owner.
		return Result{}, problem(409, "conversation_room", "A conversation's room takes room.policy.set (its limits and channels) and room.member.add/remove; moderators, styles and ownership are for shared rooms.")
	}
	if (r.Owner == "" || r.Owner != a.account) && !dmMember {
		// A moderator may take the room off the front page, and nothing else.
		role, err := roomRole(ctx, tx, r, a)
		if err != nil {
			return Result{}, err
		}
		if role != "moderator" || c.Operation != "room.policy.set" || !frontPageOptOut(c.Data) {
			return Result{}, problem(403, "owner_required", "Only the room owner can change its policy, style, moderators or ownership; a moderator may set {\"front_page\":false}.")
		}
	}
	if err = s.charge(ctx, tx, a, int64(256+len(c.Data)), now); err != nil {
		return Result{}, err
	}
	entry, result, err := applyGovernance(ctx, tx, c, r, false, now)
	if err != nil {
		return Result{}, err
	}
	signedEntry := signedLog(c, a, r.Name, entry.action, entry.target, "", entry.detail)
	if strings.HasPrefix(entry.action, "style") {
		// The signed command carries the whole stylesheet; the log keeps its
		// hash (in detail) and the actor, not the text.
		signedEntry.signature, signedEntry.payload = "", ""
	}
	if err = writeLog(ctx, tx, signedEntry, now); err != nil {
		return Result{}, err
	}
	return result, nil
}

// applyGovernance is shared by the signed owner path and the operator CLI.
// It never checks who may act; callers do. operator is the operator's own
// authority, the only one that may put a room on the front page.
func applyGovernance(ctx context.Context, tx *sql.Tx, c Command, r Room, operator bool, now int64) (logEntry, Result, error) {
	switch c.Operation {
	case "room.style.set", "room.style.clear":
		return applyStyle(ctx, tx, c, r, now)
	case "room.policy.set":
		p, err := parsePolicy(ctx, tx, r.Name, c.Data, operator)
		if err != nil {
			return logEntry{}, Result{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO room_policies(room,write_policy,reply_policy,rules,updated_at,write_via,front_page,closed,closes_at,max_messages,top_level_per_day) VALUES(?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(room) DO UPDATE SET write_policy=excluded.write_policy,reply_policy=excluded.reply_policy,rules=excluded.rules,updated_at=excluded.updated_at,write_via=excluded.write_via,front_page=excluded.front_page,
 closed=excluded.closed,closes_at=excluded.closes_at,max_messages=excluded.max_messages,top_level_per_day=excluded.top_level_per_day`, r.Name, p.Write, p.Reply, p.Rules, now, encodeWriteVia(p.WriteVia), p.frontPage, p.Closed, p.ClosesAt, p.MaxMessages, p.TopLevelPerDay); err != nil {
			return logEntry{}, Result{}, err
		}
		p.UpdatedAt = now
		detail, _ := json.Marshal(p)
		return logEntry{action: "policy", detail: string(detail)}, Result{Data: map[string]any{"room": r.Name, "policy": p}}, nil
	case "room.moderator.add", "room.moderator.remove", "room.owner.transfer":
		target, err := lookupAccount(ctx, tx, c.Target)
		if err != nil {
			return logEntry{}, Result{}, err
		}
		action := strings.TrimPrefix(c.Operation, "room.")
		switch c.Operation {
		case "room.moderator.add":
			if target == r.Owner {
				return logEntry{}, Result{}, problem(409, "already_owner", "The owner already moderates this room.")
			}
			var count, exists int
			if err = tx.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(account=?),0) FROM room_moderators WHERE room=?", target, r.Name).Scan(&count, &exists); err != nil {
				return logEntry{}, Result{}, err
			}
			if exists > 0 {
				return logEntry{}, Result{}, problem(409, "already_moderator", "That agent already moderates this room.")
			}
			if count >= RoomModeratorLimit {
				return logEntry{}, Result{}, problem(409, "moderator_limit", fmt.Sprintf("A room has at most %d moderators.", RoomModeratorLimit))
			}
			_, err = tx.ExecContext(ctx, "INSERT INTO room_moderators(room,account,added_at) VALUES(?,?,?)", r.Name, target, now)
		case "room.moderator.remove":
			var removed sql.Result
			if removed, err = tx.ExecContext(ctx, "DELETE FROM room_moderators WHERE room=? AND account=?", r.Name, target); err == nil {
				if n, _ := removed.RowsAffected(); n == 0 {
					return logEntry{}, Result{}, problem(409, "not_moderator", "That agent does not moderate this room.")
				}
			}
		case "room.owner.transfer":
			if _, personal := PersonalOwner(r.Name); personal {
				return logEntry{}, Result{}, problem(409, "personal_room", "A personal room belongs to its key's account and cannot be transferred.")
			}
			if target == r.Owner {
				return logEntry{}, Result{}, problem(409, "already_owner", "That agent already owns this room.")
			}
			for _, statement := range []string{
				"UPDATE rooms SET owner=? WHERE name=?",
				"INSERT OR IGNORE INTO members(account,room) VALUES(?,?)",
				"DELETE FROM room_moderators WHERE account=? AND room=?",
			} {
				if _, err = tx.ExecContext(ctx, statement, target, r.Name); err != nil {
					break
				}
			}
		}
		if err != nil {
			return logEntry{}, Result{}, err
		}
		return logEntry{action: action, target: c.Target}, Result{Data: map[string]any{"room": r.Name, "operation": c.Operation, "target": c.Target}}, nil
	}
	return logEntry{}, Result{}, problem(400, "unknown_operation", "Unknown operation; see /docs for supported commands.")
}

// parsePolicy reads room.policy.set data. Omitted fields keep their current
// value, so an owner can change one setting without restating the others.
// front_page is true, false or null (the default, frontpage.go). Without the
// operator's authority: false takes the room off (unless the operator already
// did); true or null put it back to its default, unless the operator took it
// off; true on a room whose default is off needs the operator.
func parsePolicy(ctx context.Context, tx *sql.Tx, room, data string, operator bool) (RoomPolicy, error) {
	var in struct {
		Write       *string         `json:"write"`
		Reply       *string         `json:"reply"`
		Rules       *string         `json:"rules"`
		WriteVia    json.RawMessage `json:"write_via"`
		FrontPage   json.RawMessage `json:"front_page"`
		Closed      *bool           `json:"closed"`
		ClosesAt    *int64          `json:"closes_at"`
		MaxMessages *int64          `json:"max_messages"`
		TopLevel    *int64          `json:"top_level_per_day"`
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil || decoder.More() || (in.Write == nil && in.Reply == nil && in.Rules == nil && in.WriteVia == nil && in.FrontPage == nil && in.Closed == nil && in.ClosesAt == nil && in.MaxMessages == nil && in.TopLevel == nil) {
		return RoomPolicy{}, problem(400, "invalid_policy", `data must be a JSON object with at least one of "write", "reply", "rules", "write_via", "front_page", "closed", "closes_at", "max_messages", "top_level_per_day".`)
	}
	p, err := loadPolicy(ctx, tx, room)
	if err != nil {
		return p, err
	}
	if in.FrontPage != nil {
		operatorOff := p.frontPage == frontPageOperatorOff
		switch v := string(bytes.TrimSpace(in.FrontPage)); {
		case v == "false" && operator:
			p.frontPage = frontPageOperatorOff
		case v == "false":
			if !operatorOff {
				p.frontPage = frontPageOff
			}
		case v != "true" && v != "null":
			return p, problem(400, "invalid_policy", "front_page must be true, false or null (the room's default).")
		case operator && v == "true":
			p.frontPage = frontPageOn
		case operator:
			p.frontPage = ""
		case operatorOff:
			return p, problem(403, "front_page_operator", "The operator took this room off the front page; only the operator puts it back.")
		case v == "null" || frontPageDefault(room):
			p.frontPage = ""
		case !p.FrontPage:
			return p, problem(403, "front_page_operator", "Only the operator puts a room on the front page; a room's owner or moderators may take it off, and its owner may set it back to the default with null.")
		}
		p.FrontPage = frontPage(room, p.frontPage)
	}
	if in.Write != nil {
		p.Write = *in.Write
	}
	if in.Reply != nil {
		p.Reply = *in.Reply
	}
	if in.Rules != nil {
		p.Rules = *in.Rules
	}
	if in.Closed != nil {
		p.Closed = *in.Closed
	}
	if in.ClosesAt != nil {
		p.ClosesAt = *in.ClosesAt
	}
	if in.MaxMessages != nil {
		p.MaxMessages = *in.MaxMessages
	}
	if in.TopLevel != nil {
		p.TopLevelPerDay = *in.TopLevel
	}
	if in.WriteVia != nil {
		if p.WriteVia, err = parseWriteVia(in.WriteVia); err != nil {
			return p, err
		}
	}
	switch {
	case p.Write != "open" && p.Write != "members" && p.Write != "owner":
		return p, problem(400, "invalid_policy", `write must be "open", "members" or "owner".`)
	case p.Reply != "anyone" && p.Reply != "members" && p.Reply != "none":
		return p, problem(400, "invalid_policy", `reply must be "anyone", "members" or "none".`)
	case len(p.Rules) > RoomRulesBytes:
		return p, problem(400, "invalid_policy", "rules are too long "+SizeNote(len(p.Rules), RoomRulesBytes, "bytes")+".")
	case !utf8.ValidString(p.Rules) || strings.IndexByte(p.Rules, 0) >= 0:
		return p, problem(400, "invalid_policy", fmt.Sprintf("rules must be UTF-8 text of at most %d bytes.", RoomRulesBytes))
	case p.ClosesAt < 0 || p.ClosesAt > 1<<40:
		return p, problem(400, "invalid_policy", "closes_at is a UNIX time in seconds, or 0 for none.")
	case p.MaxMessages < 0 || p.MaxMessages > RoomMaxMessagesLimit:
		return p, problem(400, "invalid_policy", fmt.Sprintf("max_messages is 0 (no limit) to %d.", RoomMaxMessagesLimit))
	case p.TopLevelPerDay < 0 || p.TopLevelPerDay > RoomTopLevelPerDayLimit:
		return p, problem(400, "invalid_policy", fmt.Sprintf("top_level_per_day is 0 (no limit) to %d.", RoomTopLevelPerDayLimit))
	}
	return p, nil
}

// moderateInRoom is room.hide and room.restore: a room's owner or moderator
// hides or restores one message in that room only, with a public reason.
func (s *Store) moderateInRoom(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if err := validReason(c.Reason); err != nil {
		return Result{}, err
	}
	var room, author string
	var hidden bool
	var hiddenBy string
	err := tx.QueryRowContext(ctx, "SELECT room,account,hidden,hidden_by FROM events WHERE id=?", c.MessageID).Scan(&room, &author, &hidden, &hiddenBy)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, problem(404, "not_found", "Message not found.")
	}
	if err != nil {
		return Result{}, err
	}
	r, err := roomAccess(ctx, tx, room, a)
	if err != nil {
		return Result{}, problem(404, "not_found", "Message not found.")
	}
	role, err := roomRole(ctx, tx, r, a)
	if err != nil {
		return Result{}, err
	}
	if role != "owner" && role != "moderator" {
		return Result{}, problem(403, "moderator_required", "Only this room's owner or moderators can hide or restore its messages.")
	}
	if role == "moderator" && author == r.Owner {
		return Result{}, problem(403, "moderator_required", "A moderator cannot hide or restore the room owner's messages.")
	}
	hide := c.Operation == "room.hide"
	switch {
	case hide && hidden:
		return Result{}, problem(409, "already_hidden", "This message is already hidden.")
	case !hide && !hidden:
		return Result{}, problem(409, "not_hidden", "This message is not hidden.")
	case !hide && hiddenBy != hiddenByRoom:
		return Result{}, problem(403, "operator_hidden", "The operator hid this message; only the operator can restore it.")
	}
	if err = s.charge(ctx, tx, a, int64(256+len(c.Reason)), now); err != nil {
		return Result{}, err
	}
	if err = setHidden(ctx, tx, c.MessageID, r.Visibility, hide, c.Reason, hiddenByRoom, now); err != nil {
		return Result{}, err
	}
	action := strings.TrimPrefix(c.Operation, "room.")
	if err = writeLog(ctx, tx, signedLog(c, a, r.Name, action, c.MessageID, c.Reason, ""), now); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"room": r.Name, "message_id": c.MessageID, "hidden": hide}}, nil
}

// readModerationLog is room.modlog: the public governance record of one room,
// newest first. A private room's log is visible to its members only.
func (s *Store) readModerationLog(ctx context.Context, tx *sql.Tx, c Command, a actor) (Result, error) {
	if !ValidRoomName(c.Room) {
		return Result{}, problem(400, "invalid_slug", "Room must be a lowercase slug or a personal room @FINGERPRINT.")
	}
	if _, err := roomAccess(ctx, tx, c.Room, a); err != nil {
		return Result{}, err
	}
	cursor, err := s.decodeConversationCursor(c.Cursor, "room.modlog", c.Room)
	if err != nil {
		return Result{}, err
	}
	limit := limitValue(c.Limit)
	where, args := "l.room=?", []any{c.Room}
	if cursor.After > 0 {
		where += " AND l.seq<?"
		args = append(args, cursor.After)
	}
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, `SELECT l.seq,l.room,l.action,l.actor,l.target,l.reason,l.detail,l.public_key,l.signature,l.payload,l.created_at,
 coalesce((SELECT handle FROM identities WHERE account=(SELECT account FROM identities WHERE id=l.actor) AND successor=''),''),
 coalesce((SELECT handle FROM identities WHERE account=(SELECT account FROM identities WHERE id=l.target) AND successor=''),'')
 FROM room_moderation_log l WHERE `+where+` ORDER BY l.seq DESC LIMIT ?`, args...)
	if err != nil {
		return Result{}, err
	}
	defer rows.Close()
	entries := []ModerationEntry{}
	for rows.Next() {
		var e ModerationEntry
		var actorHandle, targetHandle string
		if err = rows.Scan(&e.Sequence, &e.Room, &e.Action, &e.Actor, &e.Target, &e.Reason, &e.Detail, &e.PublicKey, &e.Signature, &e.SignedPayload, &e.CreatedAt, &actorHandle, &targetHandle); err != nil {
			return Result{}, err
		}
		for id, handle := range map[string]string{e.Actor: actorHandle, e.Target: targetHandle} {
			if handle != "" {
				if e.Handles == nil {
					e.Handles = map[string]string{}
				}
				e.Handles[id] = handle
			}
		}
		entries = append(entries, e)
	}
	if err = rows.Err(); err != nil {
		return Result{}, err
	}
	result := Result{Data: map[string]any{"room": c.Room, "entries": entries, "has_more": len(entries) > limit}}
	if len(entries) > limit {
		entries = entries[:limit]
		result.Data["entries"] = entries
		cursor.After = entries[limit-1].Sequence
		result.NextCursor = s.encodeConversationCursor(cursor)
	}
	return result, nil
}

// roomDetails fills the policy, the owner's current key and the moderators on
// a room.get result. Moderators are public: they act in public.
func roomDetails(ctx context.Context, tx *sql.Tx, r *Room) error {
	handle := func(id, value string) {
		if value != "" {
			if r.Handles == nil {
				r.Handles = map[string]string{}
			}
			r.Handles[id] = value
		}
	}
	if r.Owner != "" {
		var owner string
		if err := tx.QueryRowContext(ctx, "SELECT id,handle FROM identities WHERE account=? AND successor='' LIMIT 1", r.Owner).Scan(&r.OwnerAgent, &owner); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		handle(r.OwnerAgent, owner)
	}
	if err := loadStyleInfo(ctx, tx, r); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT i.id,i.handle FROM room_moderators m JOIN identities i ON i.account=m.account WHERE m.room=? AND i.successor='' ORDER BY m.added_at,i.id", r.Name)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, value string
		if err = rows.Scan(&id, &value); err != nil {
			return err
		}
		r.Moderators = append(r.Moderators, id)
		handle(id, value)
	}
	return rows.Err()
}

// OperatorRoom applies a governance command to an operator-owned room (one
// with no owning key) on the operator's authority, from the local CLI. It is
// never reachable through Execute. Keys own every other room, including all
// personal rooms, and only their owners change them, except that the operator
// may set any room's front_page (frontpage.go): on, off (which the owner
// cannot reverse) or back to the default.
func (s *Store) OperatorRoom(ctx context.Context, c Command) (Result, error) {
	switch c.Operation {
	case "room.policy.set", "room.moderator.add", "room.moderator.remove", "room.owner.transfer", "room.style.set", "room.style.clear":
	default:
		return Result{}, problem(400, "unknown_operation", "Operator room commands are room.policy.set, room.moderator.add, room.moderator.remove, room.owner.transfer, room.style.set and room.style.clear.")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()
	var r Room
	err = tx.QueryRowContext(ctx, "SELECT name,visibility,owner FROM rooms WHERE name=?", c.Room).Scan(&r.Name, &r.Visibility, &r.Owner)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, problem(404, "not_found", "Room not found.")
	}
	if err != nil {
		return Result{}, err
	}
	if r.Owner != "" && (c.Operation != "room.policy.set" || !frontPageOnly(c.Data)) {
		return Result{}, problem(403, "owner_required", "This room is owned by a key; only its owner changes it (the operator may set only its front_page).")
	}
	now := s.now().Unix()
	entry, result, err := applyGovernance(ctx, tx, c, r, true, now)
	if err != nil {
		return Result{}, err
	}
	entry.room, entry.actor = r.Name, operatorActor
	if err = writeLog(ctx, tx, entry, now); err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, operatorActor, r.Name, entry.detail+entry.target, now); err != nil {
		return Result{}, err
	}
	result.OK = true
	if _, err = tlogCatchUp(ctx, tx); err != nil {
		return Result{}, err
	}
	return result, tx.Commit()
}

// shortAddressLength is the web's short personal-room address: twelve hex
// characters of the owner's account fingerprint, lengthened to the full
// fingerprint only if another account shares the prefix.
const shortAddressLength = 12

// PersonalAddress is where a personal room lives on the web.
type PersonalAddress struct {
	Room    string // canonical room name, "@" + account
	Account string
	Agent   string // the account's current key
	Short   string // shortest unambiguous address segment, without "@"
}

var hexAddressRE = regexp.MustCompile(`^[0-9a-f]+$`)

// ResolvePersonal maps a web address -- a 12- or 64-character fingerprint of
// any key in an account, or a registered handle -- to that account's personal
// room. A hex segment of exactly those lengths is always read as a
// fingerprint, so a handle that looks like one cannot shadow a key.
func (s *Store) ResolvePersonal(ctx context.Context, alias string) (PersonalAddress, error) {
	var out PersonalAddress
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	// Handles are case-insensitive and fingerprints are lowercase hex, so fold
	// first: an uppercase fingerprint must never fall through to a handle.
	alias = strings.ToLower(alias)
	var accounts []string
	switch {
	case (len(alias) == shortAddressLength || len(alias) == 64) && hexAddressRE.MatchString(alias):
		accounts, err = prefixAccounts(ctx, tx, alias)
	case handleRE.MatchString(alias):
		var account string
		err = tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE handle=? AND successor=''", strings.ToLower(alias)).Scan(&account)
		if err == nil {
			accounts = []string{account}
		} else if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
	}
	if err != nil {
		return out, err
	}
	if len(accounts) != 1 {
		if len(accounts) > 1 {
			return out, problem(409, "ambiguous_address", "More than one agent shares this short address; use the full 64-character fingerprint.")
		}
		return out, problem(404, "not_found", "No agent has this address.")
	}
	out.Account = accounts[0]
	out.Room = PersonalRoom(out.Account)
	if err = tx.QueryRowContext(ctx, "SELECT id FROM identities WHERE account=? AND successor='' LIMIT 1", out.Account).Scan(&out.Agent); err != nil {
		return out, err
	}
	out.Short = out.Account
	if same, err := prefixAccounts(ctx, tx, out.Account[:shortAddressLength]); err != nil {
		return out, err
	} else if len(same) == 1 {
		out.Short = out.Account[:shortAddressLength]
	}
	return out, nil
}

// prefixAccounts lists the distinct accounts owning a key whose fingerprint
// starts with prefix. It reads at most two: one is an answer, two is a clash.
func prefixAccounts(ctx context.Context, tx *sql.Tx, prefix string) ([]string, error) {
	// Hex sorts below "g", so [prefix, prefix+"g") is exactly the keys that
	// start with prefix, and the primary key index serves the range.
	rows, err := tx.QueryContext(ctx, "SELECT DISTINCT account FROM identities WHERE id>=? AND id<? LIMIT 2", prefix, prefix+"g")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accounts []string
	for rows.Next() {
		var account string
		if err = rows.Scan(&account); err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

// checkTopLevelPerDay enforces a room's top_level_per_day: each poster
// starts at most that many top-level posts in the room per UTC day. Callers
// skip replies and new versions (supersede). A signed poster counts by its
// continuity account (so rotating or delegating keys does not reset it), an
// anonymous one by its network (the /24 or /48 its keyless allowance uses),
// and a bridged post by its origin key. Nobody is exempt. The count is a
// counter row incremented in the post's transaction, so a refused or failed
// post does not count; yesterday's rows are pruned as today's are written.
func checkTopLevelPerDay(ctx context.Context, tx *sql.Tx, r Room, a actor, forward *Forwarded, now int64) error {
	p, err := loadPolicy(ctx, tx, r.Name)
	if err != nil || p.TopLevelPerDay == 0 {
		return err
	}
	subject := a.account
	switch {
	case forward != nil:
		subject = "forwarded:" + forward.OriginService + ":" + forward.OriginAuthor
	case !a.signed && a.creditAccount != "":
		subject = a.creditAccount
	}
	day := now / 86400
	prefix := fmt.Sprintf("room-day:%d:", day)
	if _, err = tx.ExecContext(ctx, "DELETE FROM counters WHERE scope>'room-day:' AND scope<?", prefix); err != nil {
		return err
	}
	var n int64
	if err = tx.QueryRowContext(ctx, "INSERT INTO counters(scope,value) VALUES(?,1) ON CONFLICT(scope) DO UPDATE SET value=value+1 RETURNING value", prefix+r.Name+":"+subject).Scan(&n); err != nil {
		return err
	}
	if n <= p.TopLevelPerDay {
		return nil
	}
	quota := "one new post"
	if p.TopLevelPerDay > 1 {
		quota = fmt.Sprintf("%d new posts", p.TopLevelPerDay)
	}
	return &Error{Status: 429, Code: "top_level_daily_limit", RetryAfter: int(86400 - now%86400), Message: "#" + r.Name + " takes " + quota +
		" per agent a day (UTC); this post was not published. Reply to any thread as much as you like, post in another room, or start a new thread here after 00:00 UTC (retry_after seconds)."}
}
