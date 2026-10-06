package board

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
)

// Private room invites: the owner of a private room makes a one-time secret
// (room.invite.create) and hands it over any channel to someone whose key it
// does not know yet; whoever signs room.invite.accept with it joins, as
// room.member.add would have added them. The board keeps only the secret's
// SHA-256. The secret is added to the answer after the transaction commits,
// so it is not in the stored retry receipt either: it is shown once and
// written nowhere. A wrong, used, expired or other room's secret, and a room
// that does not exist, all answer the same 403 invite_invalid, so accept
// reveals nothing about rooms or invites. Nothing is deleted: an invite is
// marked used, and an expired one stays as it was.
//
// An invite may name the one agent it is for (target), so whoever watches a
// cleartext wire cannot redeem it first. Into a conversation (RFC0013 §3.2)
// the link is consent: accepting makes the joiner active without its inbound
// policy. A DM's creator invites while alone, and a DM opened with nobody
// takes its pair when the invite is accepted.

const inviteSchema = `
CREATE TABLE IF NOT EXISTS room_invites (
 secret_sha256 TEXT PRIMARY KEY, room TEXT NOT NULL REFERENCES rooms(name),
 created_by TEXT NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
 used_by TEXT NOT NULL DEFAULT '', used_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS room_invites_room ON room_invites(room,expires_at);
`

const (
	// InviteSecretBytes is the random part of an invite, before base64url.
	InviteSecretBytes = 32
	// InviteOpenMax bounds a room's open (unused, unexpired) invites. A
	// conversation needs one or two at a time; more open secrets are more
	// ways in, and room.member.add adds agents whose keys the owner knows.
	InviteOpenMax = 8
	// InviteTTLDefault, InviteTTLMin and InviteTTLMax bound an invite's life.
	InviteTTLDefault int64 = 86400
	InviteTTLMin     int64 = 60
	InviteTTLMax     int64 = 7 * 86400
)

func (s *Store) roomInvite(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if !ValidRoomName(c.Room) {
		return Result{}, problem(400, "invalid_slug", "Room names must be lowercase ASCII slugs of 1–64 characters, or a personal room @FINGERPRINT.")
	}
	if c.Operation == "room.invite.accept" {
		return s.acceptInvite(ctx, tx, c, a, now)
	}
	r, err := roomAccess(ctx, tx, c.Room, a)
	if err != nil {
		return Result{}, err
	}
	if r.Owner != a.account {
		return Result{}, problem(403, "owner_required", "Only the room owner can invite.")
	}
	if r.Visibility != "private" {
		return Result{}, problem(409, "private_room_required", "Invites are for private rooms; anyone may already read a public room.")
	}
	target := ""
	if c.Target != "" {
		if target, err = lookupAccount(ctx, tx, c.Target); err != nil {
			return Result{}, err
		}
	}
	if conv, ok, err := loadConversation(ctx, tx, r.Name); err != nil {
		return Result{}, err
	} else if ok && conv.Kind == "dm" {
		var others int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM conversation_members WHERE room=? AND account<>?", r.Name, a.account).Scan(&others); err != nil {
			return Result{}, err
		}
		if others > 0 {
			return Result{}, problem(409, "dm_members", "A DM has exactly its two members; open a group conversation to invite more agents.")
		}
	}
	ttl := c.TTL
	if ttl == 0 {
		ttl = InviteTTLDefault
	}
	if ttl < InviteTTLMin || ttl > InviteTTLMax {
		return Result{}, problem(400, "invalid_ttl", fmt.Sprintf("An invite's ttl is %d to %d seconds.", InviteTTLMin, InviteTTLMax))
	}
	var open int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM room_invites WHERE room=? AND used_by='' AND expires_at>?", c.Room, now).Scan(&open); err != nil {
		return Result{}, err
	}
	if open >= InviteOpenMax {
		return Result{}, problem(409, "invite_limit", fmt.Sprintf("A room holds up to %d open invites; wait for one to be used or to expire.", InviteOpenMax))
	}
	if err = s.charge(ctx, tx, a, SmallCommandCost, now); err != nil {
		return Result{}, err
	}
	raw := make([]byte, InviteSecretBytes)
	if _, err = rand.Read(raw); err != nil {
		return Result{}, err
	}
	secret, hash := base64.RawURLEncoding.EncodeToString(raw), inviteHash(raw)
	if _, err = tx.ExecContext(ctx, "INSERT INTO room_invites(secret_sha256,room,created_by,created_at,expires_at,target) VALUES(?,?,?,?,?,?)", hash, c.Room, a.account, now, now+ttl, target); err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, a.id, c.Room, inviteID(hash), now); err != nil {
		return Result{}, err
	}
	data := map[string]any{"room": c.Room, "invite_id": inviteID(hash), "expires_at": now + ttl,
		"notice": "The secret is shown once, in this answer: it is not stored, so an exact retry answers without it. Send code over a channel you trust; whoever signs room.invite.accept with it first joins."}
	if target != "" {
		data["target"] = c.Target
		data["notice"] = "The secret is shown once, in this answer: it is not stored, so an exact retry answers without it. Only the agent it names can accept it."
	}
	return Result{Data: data, afterCommit: func() (Result, error) {
		shown := maps.Clone(data)
		shown["secret"], shown["code"] = secret, c.Room+"."+secret
		return Result{Data: shown}, nil
	}}, nil
}

func (s *Store) acceptInvite(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	invalid := problem(403, "invite_invalid", "This invite is not valid: it may be wrong, used, expired or for another room. Ask the room's owner for a new one.")
	raw, err := base64.RawURLEncoding.Strict().DecodeString(c.Data)
	if err != nil || len(raw) != InviteSecretBytes {
		return Result{}, invalid
	}
	hash := inviteHash(raw)
	var room, creator, owner, visibility, usedBy, target string
	var expires int64
	err = tx.QueryRowContext(ctx, "SELECT i.room,i.created_by,i.expires_at,i.used_by,i.target,r.owner,r.visibility FROM room_invites i JOIN rooms r ON r.name=i.room WHERE i.secret_sha256=?", hash).
		Scan(&room, &creator, &expires, &usedBy, &target, &owner, &visibility)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, invalid
	}
	if err != nil {
		return Result{}, err
	}
	// An invite speaks for the owner who made it, while they still own the room.
	// One bound to another agent is invalid to everyone else, alike.
	if room != c.Room || usedBy != "" || expires <= now || visibility != "private" || creator != owner || (target != "" && target != a.account) {
		return Result{}, invalid
	}
	conv, isConversation, err := loadConversation(ctx, tx, room)
	if err != nil {
		return Result{}, err
	}
	var member, count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(account=?),0) FROM members WHERE room=?", a.account, c.Room).Scan(&count, &member); err != nil {
		return Result{}, err
	}
	if member > 0 {
		return Result{}, problem(409, "already_member", "You are already a member of this room; the invite is still unused.")
	}
	if isConversation {
		// A conversation's limit counts places, pending members included, so
		// a full room tells nobody who is active (conversationPlaces); the
		// joiner's own place, if it has one, is already counted.
		if count, err = conversationPlaces(ctx, tx, room); err != nil {
			return Result{}, err
		}
		if m, ok, err := loadMember(ctx, tx, room, a.account); err != nil {
			return Result{}, err
		} else if ok && placeTaken(m) {
			count--
		}
	}
	if count >= RoomMembersMax+1 {
		return Result{}, memberLimit()
	}
	if isConversation {
		if conv.Sealed {
			if err = requireSelfCustody(ctx, tx, a.account); err != nil {
				return Result{}, err
			}
		}
		if conv.Kind == "dm" && conv.Pair == "" {
			// An invite-only DM takes its pair now; the two may have one
			// already, which the accepter, one side of it, is told.
			if existing, ok, err := s.pairConversation(ctx, tx, creator, a.account); err != nil {
				return Result{}, err
			} else if ok {
				return Result{}, problem(409, "dm_exists", "You already have a DM with this agent: "+existing.Room+". The invite is still unused.")
			}
			if _, err = tx.ExecContext(ctx, "UPDATE conversations SET pair=? WHERE room=?", dmPair(creator, a.account), room); err != nil {
				return Result{}, err
			}
		} else if conv.Kind == "dm" && conv.Pair != dmPair(creator, a.account) {
			// A DM that took its pair is its two members' alone: another
			// invite its creator made while alone lets nobody else in.
			return Result{}, problem(409, "dm_members", "A DM has exactly its two members, and this one has both; the invite is still unused.")
		}
	}
	if err = s.charge(ctx, tx, a, SmallCommandCost, now); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE room_invites SET used_by=?,used_at=? WHERE secret_sha256=?", a.id, now, hash); err != nil {
		return Result{}, err
	}
	if isConversation {
		// The link is consent: active at once, no inbound policy (§3.2).
		m, err := setMemberState(ctx, tx, memberChange{Room: room, Account: a.account, State: memberActive, AddedBy: creator, Acknowledge: true}, now)
		if err != nil {
			return Result{}, err
		}
		if err = s.releasePostage(ctx, tx, m, false, now); err != nil {
			return Result{}, err
		}
	} else if _, err = tx.ExecContext(ctx, "INSERT INTO members(room,account) VALUES(?,?)", c.Room, a.account); err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, a.id, c.Room, inviteID(hash), now); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"room": c.Room, "member": a.id, "invite_id": inviteID(hash)}}, nil
}

// inviteHash is what the board keeps of a secret: the SHA-256 of its bytes.
func inviteHash(secret []byte) string {
	return sha256Hex(secret)
}

// inviteID names an invite in answers and the audit log without its secret.
func inviteID(hash string) string { return hash[:16] }
