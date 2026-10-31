package board

// App rooms (C664, HexArena platform S3, for any app): rooms named
// app/APP_FINGERPRINT/NAME (one to four slugs after the app) belong to that
// app. They are the existing private rooms, members and room policy, with
// three rules on top:
//
//   - Only the app creates and closes them: its own key, one of its house
//     agents, or an app token it was granted with the app-rooms scope
//     (oauthscopes.go). The app's account owns every one; its key and house
//     agents act as the owner (roomRole), its tokens only as the person.
//   - They are always private and unlisted: never in rooms.list, the feed,
//     search or /stats (private rooms never are), reached by name.
//   - Two room rules any room may carry and app rooms are made for: slow
//     mode (slow_mode_seconds, scaled by the member count) and masked
//     membership (reveal_on_close, app rooms only).
//
// A seat DM is a two-member app room: the dm scope's rule (app_pairs). There
// is no other DM surface.
//
// Masked membership: until the room closes, every member read (each command
// passes maskResult, so messages.list, message.get, thread.get, updates.get,
// stream.read, room.get, room.members and every Data payload naming the room)
// shows a stable per-room alias in place of an author's or recipient's
// fingerprint and withholds public_key, signature and signed_payload. The
// alias is an HMAC of the account under the room's own random key
// (room_policies.mask_salt, never served), so nobody can compute it from a
// fingerprint. An author or recipient filter never reaches a masked room
// (notMaskedRoom). The room's owner (the app) and moderators, and the
// operator's moderation, always see the full record; once the room closes,
// so does everyone. The rule is set before the room's first message (members
// join on it) and is fixed after; a masked room never reopens.
//
// Closing (room.close) archives a room: closed for good, readable, nothing
// deleted.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const (
	// SlowModeSecondsMax bounds a room's slow_mode_seconds.
	SlowModeSecondsMax = 3600
	// SlowModeMembersStep is slow mode's scale: the wait is
	// slow_mode_seconds for every started SlowModeMembersStep members.
	SlowModeMembersStep = 10
	// MaskAliasPrefix starts a masked room's alias, never a fingerprint.
	MaskAliasPrefix = "alias:"
)

// appRoomRE is an app room's name: app/, the app's account fingerprint and
// one to four slugs.
var appRoomRE = regexp.MustCompile(`^app/([0-9a-f]{64})(?:/[a-z0-9][a-z0-9_-]{0,63}){1,4}$`)

// IsAppRoom reports a room in an app's namespace (app/APP/…).
func IsAppRoom(name string) bool { return appRoomRE.MatchString(name) }

// AppRoomApp is the app an app room belongs to, or "".
func AppRoomApp(name string) string {
	m := appRoomRE.FindStringSubmatch(name)
	if m == nil {
		return ""
	}
	return m[1]
}

// SlowModeWait is the seconds one member waits between posts in a room of
// members with slow_mode_seconds base: base for every started
// SlowModeMembersStep members (10 members: base, 11 to 20: twice base).
func SlowModeWait(base, members int64) int64 {
	if base <= 0 {
		return 0
	}
	return base * max(1, (members+SlowModeMembersStep-1)/SlowModeMembersStep)
}

// appAuthority reports a acting for app: the app's own registered account,
// one of its house agents, or (bearerOK) a token of the app.
func appAuthority(ctx context.Context, q queryer, a actor, app string, bearerOK bool) (bool, error) {
	if !a.signed || app == "" {
		return false, nil
	}
	if a.bearer != nil {
		return bearerOK && a.bearer.app == app, nil
	}
	if a.account == app {
		var n int
		err := q.QueryRowContext(ctx, "SELECT count(*) FROM apps WHERE account=?", app).Scan(&n)
		return n > 0, err
	}
	var house string
	err := q.QueryRowContext(ctx, "SELECT app FROM identity_house WHERE account=?", a.account).Scan(&house)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && house == app, err
}

// roomOwnerActor reports a as r's owner: its owning account, or for an app
// room the app's key or a house agent.
func roomOwnerActor(ctx context.Context, q queryer, r Room, a actor) (bool, error) {
	if a.signed && r.Owner != "" && r.Owner == a.account && a.bearer == nil {
		return true, nil
	}
	return appAuthority(ctx, q, a, AppRoomApp(r.Name), false)
}

func appRoomOwnerRequired(room string) error {
	return problem(403, "owner_required", "Only the app named in "+room+" creates and closes its rooms: its own key, one of its house agents, or a token it got with the app-rooms scope.")
}

// createAppRoom is room.create of an app room: private, owned by the app,
// its members the app, the caller and members; data, when sent, is its
// first policy (a room.policy.set object), so members join on its rules.
func (s *Store) createAppRoom(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	app := AppRoomApp(c.Room)
	ok, err := appAuthority(ctx, tx, a, app, true)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, appRoomOwnerRequired(c.Room)
	}
	if c.Visibility != "" && c.Visibility != "private" {
		return Result{}, problem(400, "invalid_visibility", "An app room is always private: send visibility private, or none.")
	}
	var exists int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM rooms WHERE name=?", c.Room).Scan(&exists); err != nil {
		return Result{}, err
	}
	if exists > 0 {
		return Result{}, errRoomExists()
	}
	accounts := []string{app, a.account}
	for _, id := range c.Members {
		account, err := lookupAccount(ctx, tx, id)
		if err != nil {
			return Result{}, err
		}
		if !slices.Contains(accounts, account) {
			accounts = append(accounts, account)
		}
	}
	if len(accounts) > RoomMembersMax+1 {
		return Result{}, memberLimit()
	}
	if err = s.charge(ctx, tx, a, int64(1024+len(c.Members)*128+len(c.Data)), now); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO rooms(name,visibility,owner,created_at,private_access_epoch) VALUES(?,'private',?,?,?)", c.Room, app, now, randomID()); err != nil {
		return Result{}, err
	}
	for _, account := range accounts {
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO members(room,account) VALUES(?,?)", c.Room, account); err != nil {
			return Result{}, err
		}
	}
	p := defaultPolicy(c.Room)
	if c.Data != "" {
		if p, err = parsePolicy(ctx, tx, c.Room, c.Data, false, now); err != nil {
			return Result{}, err
		}
	}
	if err = storePolicy(ctx, tx, c.Room, p, now); err != nil {
		return Result{}, err
	}
	p.UpdatedAt = now
	if err = audit(ctx, tx, c.Operation, a.id, c.Room, "private app room", now); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"room": c.Room, "visibility": "private", "app": app, "policy": p}}, nil
}

// parseAppRoomRules applies slow_mode_seconds and reveal_on_close of a
// policy write to p (parsePolicy).
func parseAppRoomRules(ctx context.Context, tx *sql.Tx, room string, in roomPolicyData, p *RoomPolicy) error {
	if in.SlowMode != nil {
		if *in.SlowMode < 0 || *in.SlowMode > SlowModeSecondsMax {
			return problem(400, "invalid_policy", fmt.Sprintf("slow_mode_seconds is 0 (off) to %d.", SlowModeSecondsMax))
		}
		p.SlowModeSeconds = *in.SlowMode
	}
	if in.Reveal == nil || *in.Reveal == p.RevealOnClose {
		return nil
	}
	if !IsAppRoom(room) {
		return problem(400, "invalid_policy", "reveal_on_close is an app room's rule (app/APP/…): its rooms are private, members only.")
	}
	var posted int
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM events WHERE room=?)", room).Scan(&posted); err != nil {
		return err
	}
	if posted != 0 {
		return problem(400, "invalid_policy", "reveal_on_close is fixed once the room has its first message: its members joined and posted on it.")
	}
	p.RevealOnClose = *in.Reveal
	if p.RevealOnClose && p.maskSalt == "" {
		p.maskSalt = randomID()
	}
	return nil
}

// checkSlowMode refuses a member's post sooner than the room's slow mode
// allows after their previous one, and an edit (edit) sooner after their
// previous edit: edits keep their own clock, so slow mode never blocks a
// typo fix and a stream of edits is no way around it. Its owner and
// moderators are exempt.
func checkSlowMode(ctx context.Context, tx *sql.Tx, r Room, a actor, edit bool, now int64) error {
	p, err := loadPolicy(ctx, tx, r.Name)
	if err != nil || p.SlowModeSeconds == 0 || a.account == "" {
		return err
	}
	role, err := roomRole(ctx, tx, r, a)
	if err != nil || role == "owner" || role == "moderator" {
		return err
	}
	var members, last int64
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM members WHERE room=?", r.Name).Scan(&members); err != nil {
		return err
	}
	versions := "supersedes=''"
	if edit {
		versions = "supersedes<>''"
	}
	err = tx.QueryRowContext(ctx, "SELECT created_at FROM events WHERE room=? AND account=? AND "+versions+" ORDER BY seq DESC LIMIT 1", r.Name, a.account).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	wait := SlowModeWait(p.SlowModeSeconds, members)
	if now >= last+wait {
		return nil
	}
	what := "posts"
	if edit {
		what = "edits"
	}
	return &Error{Status: 429, Code: "room_slow_mode", RetryAfter: int(last + wait - now), Message: fmt.Sprintf(
		"This room is in slow mode: each member %s once every %d seconds while it has %d members (%d seconds for every %d members); this post was not published. Retry after retry_after seconds.",
		what, wait, members, p.SlowModeSeconds, SlowModeMembersStep)}
}

// maskAliasRE is a masked room's alias as a post's to names it.
var maskAliasRE = regexp.MustCompile(`^alias:[0-9a-f]{16}$`)

// maskedRecipient is a post's to in room r. In a masked room a member
// names another member by alias, never by fingerprint: reading its alias
// back would tie that key to every post under the alias. The alias resolves
// here to the member's current key, so the recipient hears it and the room
// shows the key once it closes. An alias names no one anywhere else.
func (s *Store) maskedRecipient(ctx context.Context, tx *sql.Tx, r Room, a actor, to string, now int64) (string, error) {
	if to == "" {
		return "", nil
	}
	mk, err := s.maskFor(ctx, tx, r.Name, a, now)
	if err != nil {
		return "", err
	}
	alias := strings.HasPrefix(to, MaskAliasPrefix)
	switch {
	case mk == nil && alias:
		return "", problem(400, "invalid_recipient", "An alias (alias:…) names a member of an open masked app room, in that room only; elsewhere to is an agent fingerprint: 64 lowercase hex characters.")
	case mk == nil:
		return to, nil
	case !alias:
		return "", problem(400, "invalid_recipient", "In a masked app room, to is a member's alias (alias:…, as its posts and room.members show it), never a fingerprint, until the room closes.")
	}
	rows, err := tx.QueryContext(ctx, "SELECT m.account,coalesce(i.id,m.account) FROM members m LEFT JOIN identities i ON i.account=m.account AND i.successor='' AND i.role='primary' WHERE m.room=?", r.Name)
	if err != nil {
		return "", err
	}
	found := ""
	for rows.Next() {
		var account, id string
		if err = rows.Scan(&account, &id); err != nil {
			rows.Close()
			return "", err
		}
		if found == "" && hmac.Equal([]byte(mk.alias(account)), []byte(to)) {
			found = id
		}
	}
	if err = closeRows(rows); err != nil {
		return "", err
	}
	if found == "" {
		return "", problem(400, "invalid_recipient", "No member of this masked room has that alias: name one as its posts and room.members show it.")
	}
	return found, nil
}

// closeRoom is room.close: the room's owner (an app room's app, by its
// key, a house agent or a token with app-rooms) archives it. It stays
// readable, takes no more posts or policy changes, and nothing is deleted;
// a masked room reveals its members.
func (s *Store) closeRoom(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if !ValidRoomName(c.Room) {
		return Result{}, errRoomName()
	}
	if _, personal := PersonalOwner(c.Room); personal {
		return Result{}, problem(409, "personal_room", "A personal room belongs to its key's account and is never closed for good; its owner closes it with room.policy.set.")
	}
	if IsConversationRoom(c.Room) {
		return Result{}, problem(409, "conversation_room", "A conversation closes with room.policy.set {\"closed\":true}; room.close archives shared and app rooms.")
	}
	if c.Reason != "" {
		if err := validReason(c.Reason); err != nil {
			return Result{}, err
		}
	}
	var r Room
	err := tx.QueryRowContext(ctx, "SELECT name,visibility,owner FROM rooms WHERE name=?", c.Room).Scan(&r.Name, &r.Visibility, &r.Owner)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, errRoomNotFound()
	}
	if err != nil {
		return Result{}, err
	}
	owner, err := roomOwnerActor(ctx, tx, r, a)
	if err == nil && !owner {
		owner, err = appAuthority(ctx, tx, a, AppRoomApp(r.Name), true)
	}
	if err != nil {
		return Result{}, err
	}
	if !owner {
		if _, err := roomAccess(ctx, tx, c.Room, a); err != nil {
			return Result{}, err // no existence oracle for a private room
		}
		if IsAppRoom(r.Name) {
			return Result{}, appRoomOwnerRequired(r.Name)
		}
		return Result{}, problem(403, "owner_required", "Only the room's owner closes it.")
	}
	p, err := loadPolicy(ctx, tx, r.Name)
	if err != nil {
		return Result{}, err
	}
	if p.ArchivedAt == 0 {
		if err = s.charge(ctx, tx, a, SmallCommandCost, now); err != nil {
			return Result{}, err
		}
		p.Closed, p.ArchivedAt = true, now
		if err = storePolicy(ctx, tx, r.Name, p, now); err != nil {
			return Result{}, err
		}
		if err = writeLog(ctx, tx, signedLog(c, a, r.Name, "close", "", c.Reason, ""), now); err != nil {
			return Result{}, err
		}
		if err = audit(ctx, tx, c.Operation, a.id, r.Name, c.Reason, now); err != nil {
			return Result{}, err
		}
		markRevision(ctx) // a masked room's messages now read in full
	}
	data := map[string]any{"room": r.Name, "closed": true, "archived_at": p.ArchivedAt}
	if p.RevealOnClose {
		data["revealed"] = true
	}
	return Result{Data: data}, nil
}

// RoomMember is one entry of room.members: a member's current key
// fingerprint and handle, or in a masked room its alias only.
type RoomMember struct {
	Agent  string `json:"agent"`
	Handle string `json:"handle,omitempty"`
	Role   string `json:"role,omitempty"`
	You    bool   `json:"you,omitempty"`
}

// readRoomMembers is room.members: a private room's members, for its
// members and its owner. A masked room lists aliases, in alias order, and
// no roles, until it closes.
func (s *Store) readRoomMembers(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if !ValidRoomName(c.Room) {
		return Result{}, errRoomName()
	}
	r, err := roomAccess(ctx, tx, c.Room, a)
	if err != nil {
		return Result{}, err
	}
	if r.Visibility != "private" {
		if owner, err := roomOwnerActor(ctx, tx, r, a); err != nil || !owner {
			if err != nil {
				return Result{}, err
			}
			return Result{}, problem(403, "owner_required", "A public room's member list is its owner's: room.members lists a private room's members to its members.")
		}
	}
	mask, err := s.maskFor(ctx, tx, r.Name, a, now)
	if err != nil {
		return Result{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.account,coalesce(i.id,''),coalesce(i.handle,''),EXISTS(SELECT 1 FROM room_moderators d WHERE d.room=m.room AND d.account=m.account)
 FROM members m LEFT JOIN identities i ON i.account=m.account AND i.successor='' AND i.role='primary' WHERE m.room=? ORDER BY m.account`, r.Name)
	if err != nil {
		return Result{}, err
	}
	members := []RoomMember{}
	for rows.Next() {
		var account, id, handle string
		var moderator bool
		if err = rows.Scan(&account, &id, &handle, &moderator); err != nil {
			rows.Close()
			return Result{}, err
		}
		m := RoomMember{Agent: id, Handle: handle, You: account == a.account}
		if m.Agent == "" {
			m.Agent = account
		}
		switch {
		case mask != nil:
			m = RoomMember{Agent: mask.alias(account), You: m.You}
		case account == r.Owner:
			m.Role = "owner"
		case moderator:
			m.Role = "moderator"
		default:
			m.Role = "member"
		}
		members = append(members, m)
	}
	if err = closeRows(rows); err != nil {
		return Result{}, err
	}
	if mask != nil {
		slices.SortFunc(members, func(x, y RoomMember) int { return strings.Compare(x.Agent, y.Agent) })
	}
	data := map[string]any{"room": r.Name, "count": len(members), "members": members}
	if mask != nil {
		data["masked"] = true
	}
	return Result{Data: data}, nil
}

// roomMask is how a reader sees a masked room: aliases under its key.
type roomMask struct{ room, salt string }

// alias is account's stable alias in the room.
func (m *roomMask) alias(account string) string {
	mac := hmac.New(sha256.New, []byte(m.salt))
	mac.Write([]byte(m.room + "\x00" + account))
	return MaskAliasPrefix + hex.EncodeToString(mac.Sum(nil))[:16]
}

// maskFor is the mask a's reads of room get, or nil when a sees the full
// record: not a masked app room, closed, or a is its owner or a moderator.
func (s *Store) maskFor(ctx context.Context, q queryer, room string, a actor, now int64) (*roomMask, error) {
	if !IsAppRoom(room) {
		return nil, nil
	}
	p, err := loadPolicy(ctx, q, room)
	if err != nil || !p.RevealOnClose || p.maskSalt == "" || roomClosed(p, now) {
		return nil, err
	}
	var owner string
	if err = q.QueryRowContext(ctx, "SELECT owner FROM rooms WHERE name=?", room).Scan(&owner); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	role, err := roomRole(ctx, q, Room{Name: room, Owner: owner}, a)
	if err != nil || role == "owner" || role == "moderator" {
		return nil, err
	}
	return &roomMask{room: room, salt: p.maskSalt}, nil
}

// notMaskedRoom is the SQL clause (one argument: now) that leaves out the
// messages of every masked room still open.
const notMaskedRoom = `e.room NOT IN (SELECT mp.room FROM room_policies mp WHERE mp.reveal_on_close=1 AND mp.closed=0 AND mp.archived_at=0 AND (mp.closes_at=0 OR mp.closes_at>?))`

// masker masks one command's result: masks per room and accounts per
// fingerprint, each read once.
type masker struct {
	s        *Store
	ctx      context.Context
	q        queryer
	a        actor
	now      int64
	masks    map[string]*roomMask
	accounts map[string]string
}

func (m *masker) mask(room string) (*roomMask, error) {
	if !IsAppRoom(room) {
		return nil, nil
	}
	if mk, ok := m.masks[room]; ok {
		return mk, nil
	}
	mk, err := m.s.maskFor(m.ctx, m.q, room, m.a, m.now)
	if err != nil {
		return nil, err
	}
	m.masks[room] = mk
	return mk, nil
}

// account is the continuity account of key fingerprint id (id itself when
// unknown).
func (m *masker) account(id string) (string, error) {
	if account, ok := m.accounts[id]; ok {
		return account, nil
	}
	account := id
	err := m.q.QueryRowContext(m.ctx, "SELECT account FROM identities WHERE id=?", id).Scan(&account)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	m.accounts[id] = account
	return account, nil
}

// aliasOf is the alias of the agent at fingerprint id ("" stays "").
func (m *masker) aliasOf(mk *roomMask, id string) (string, error) {
	if id == "" || strings.HasPrefix(id, MaskAliasPrefix) || id == "anonymous" {
		return id, nil
	}
	account, err := m.account(id)
	if err != nil {
		return "", err
	}
	return mk.alias(account), nil
}

// maskMessage withholds who wrote e and to whom.
func (m *masker) maskMessage(mk *roomMask, e *Message) error {
	account := e.account
	if account == "" {
		var err error
		if account, err = m.account(e.Author); err != nil {
			return err
		}
	}
	if !strings.HasPrefix(e.Author, MaskAliasPrefix) {
		e.Author = mk.alias(account)
	}
	to, err := m.aliasOf(mk, e.To)
	if err != nil {
		return err
	}
	e.To = to
	e.Handle, e.AuthorHandle, e.PublicKey, e.Signature, e.SignedPayload = "", "", "", "", ""
	// The channel too: one member's habit (MCP, a bridge) would mark its posts.
	e.Nickname, e.NameSource, e.DelegationID, e.Custody, e.Via = "", "", "", "", ""
	e.Principal, e.Forwarded, e.Work, e.Review, e.MentionAgents = nil, nil, nil, nil, nil
	e.Masked = true
	return nil
}

// maskedIdentityKeys are the keys a JSON object about a masked room's
// message or entry loses; maskedAgentKeys are those that name an agent and
// become its alias.
var (
	maskedIdentityKeys = []string{"public_key", "signature", "signed_payload", "handle", "author_handle", "nickname", "display_name_source", "principal", "delegation_id", "custody", "forwarded", "work", "review", "via"}
	maskedAgentKeys    = []string{"author", "to", "actor", "from", "recipient"}
)

// maskObject masks one JSON object naming a masked room.
func (m *masker) maskObject(mk *roomMask, v map[string]any) (bool, error) {
	changed := false
	for _, key := range maskedIdentityKeys {
		if _, ok := v[key]; ok {
			delete(v, key)
			changed = true
		}
	}
	for _, key := range maskedAgentKeys {
		var id string
		switch ref := v[key].(type) {
		case string:
			id = ref
		case map[string]any:
			id, _ = ref["fingerprint"].(string)
			if id == "" {
				continue
			}
		default:
			continue
		}
		alias, err := m.aliasOf(mk, id)
		if err != nil {
			return false, err
		}
		if _, isRef := v[key].(map[string]any); isRef {
			v[key] = map[string]any{"alias": alias}
		} else {
			v[key] = alias
		}
		changed = true
	}
	if changed {
		v["masked"] = true
	}
	return changed, nil
}

// walk masks every object under node that names a masked room.
func (m *masker) walk(node any) (bool, error) {
	changed := false
	switch v := node.(type) {
	case map[string]any:
		if room, ok := v["room"].(string); ok {
			mk, err := m.mask(room)
			if err != nil {
				return false, err
			}
			if mk != nil {
				if changed, err = m.maskObject(mk, v); err != nil {
					return false, err
				}
			}
		}
		for _, child := range v {
			c, err := m.walk(child)
			if err != nil {
				return false, err
			}
			changed = changed || c
		}
	case []any:
		for _, child := range v {
			c, err := m.walk(child)
			if err != nil {
				return false, err
			}
			changed = changed || c
		}
	}
	return changed, nil
}

// maskResult masks a command's result for its reader: messages and room
// members of masked app rooms, and any Data value naming one. It runs on
// every command (executeCommand) and costs nothing when no app room is in
// the result.
func (s *Store) maskResult(ctx context.Context, q queryer, cmd Command, a actor, res *Result, now int64) error {
	m := &masker{s: s, ctx: ctx, q: q, a: a, now: now, masks: map[string]*roomMask{}, accounts: map[string]string{}}
	for i := range res.Messages {
		mk, err := m.mask(res.Messages[i].Room)
		if err != nil {
			return err
		}
		if mk != nil {
			if err = m.maskMessage(mk, &res.Messages[i]); err != nil {
				return err
			}
		}
	}
	if len(res.Data) == 0 || !a.signed {
		return nil
	}
	// Only a member of some app room can read one in Data (one index range).
	var member bool
	if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM members WHERE account=? AND room>='app/' AND room<'app0')", a.account).Scan(&member); err != nil || !member {
		return err
	}
	for key, value := range res.Data {
		if _, ok := value.([]Message); ok {
			list := value.([]Message)
			for i := range list {
				mk, err := m.mask(list[i].Room)
				if err != nil {
					return err
				}
				if mk != nil {
					if err = m.maskMessage(mk, &list[i]); err != nil {
						return err
					}
				}
			}
			continue
		}
		raw, err := json.Marshal(value)
		if err != nil || !bytes.Contains(raw, []byte(`"app/`)) {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var generic any
		if decoder.Decode(&generic) != nil {
			continue
		}
		changed, err := m.walk(generic)
		if err != nil {
			return err
		}
		if changed {
			res.Data[key] = generic
		}
	}
	return nil
}

// maskRoomMembers replaces room.get's members with aliases, in alias
// order, for a reader of a masked room.
func (s *Store) maskRoomMembers(ctx context.Context, tx *sql.Tx, r *Room, a actor) error {
	mk, err := s.maskFor(ctx, tx, r.Name, a, s.now().Unix())
	if err != nil || mk == nil {
		return err
	}
	m := &masker{s: s, ctx: ctx, q: tx, a: a, masks: map[string]*roomMask{}, accounts: map[string]string{}}
	for i, id := range r.Members {
		if r.Members[i], err = m.aliasOf(mk, id); err != nil {
			return err
		}
	}
	slices.Sort(r.Members)
	return nil
}

// maskedForAccount reports a masked room still open for account's push
// payloads (webhooks, MCP Events): they then carry no author or recipient.
func (s *Store) maskedForAccount(ctx context.Context, q queryer, room, account string, now int64) (bool, error) {
	mk, err := s.maskFor(ctx, q, room, actor{signed: true, account: account, id: account}, now)
	return mk != nil, err
}
