package board

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

func lookupAccount(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	if !fingerprintRE.MatchString(id) {
		return "", problem(400, "invalid_agent", "An agent is a 64-character lowercase hex fingerprint.")
	}
	var account string
	err := tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", id).Scan(&account)
	if errors.Is(err, sql.ErrNoRows) {
		return "", problem(404, "agent_not_found", "That agent is not registered; it registers by signing any write, such as a post.")
	}
	return account, err
}

func (s *Store) changeAgent(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if c.Operation == "agent.register" {
		if c.Handle != "" && !handleRE.MatchString(c.Handle) {
			return Result{}, problem(400, "invalid_handle", fmt.Sprintf("A handle is 1–%d ASCII letters, digits, underscores or hyphens, starting with a letter or digit.", HandleMaxChars))
		}
		handle := strings.ToLower(c.Handle)
		var taken int
		if handle != "" {
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM identities WHERE handle=? AND id<>?", handle, a.id).Scan(&taken); err != nil {
				return Result{}, err
			}
			if taken > 0 {
				return Result{}, problem(409, "handle_taken", "That handle belongs to another agent.")
			}
		}
		if err := s.charge(ctx, tx, a, 512, now); err != nil {
			return Result{}, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE identities SET handle=? WHERE id=?", handle, a.id); err != nil {
			return Result{}, err
		}
		if err := audit(ctx, tx, c.Operation, a.id, a.id, handle, now); err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"agent_id": a.id, "handle": handle}}, nil
	}
	key, err := base64.RawURLEncoding.DecodeString(c.Target)
	if err != nil || len(key) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(key) != c.Target {
		return Result{}, problem(400, "invalid_target_key", "Target must be the new raw Ed25519 public key in unpadded base64url.")
	}
	proof, err := base64.RawURLEncoding.DecodeString(c.Proof)
	if err != nil || base64.RawURLEncoding.EncodeToString(proof) != c.Proof || !ed25519.Verify(key, a.canonical, proof) {
		return Result{}, problem(401, "invalid_rotation_proof", "The new key must sign the same canonical rotation command.")
	}
	newID := fingerprint(key)
	var exists int
	if err = tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM identities WHERE id=?)+(SELECT count(*) FROM delegations WHERE child_id=?)+(SELECT count(*) FROM private_read_grants WHERE child_id=?)", newID, newID, newID).Scan(&exists); err != nil {
		return Result{}, err
	}
	if exists > 0 {
		return Result{}, problem(409, "agent_exists", "Rotate into a fresh key: two existing agents cannot be merged.")
	}
	if err = s.charge(ctx, tx, a, 512, now); err != nil {
		return Result{}, err
	}
	var handle string
	if err = tx.QueryRowContext(ctx, "SELECT handle FROM identities WHERE id=?", a.id).Scan(&handle); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE identities SET handle='',successor=? WHERE id=?", newID, a.id); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO identities(id,public_key,account,handle,created_at,last_seen) VALUES(?,?,?,?,?,?)", newID, c.Target, a.account, handle, now, now); err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, a.id, newID, "key rotation", now); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"agent_id": newID, "predecessor": a.id, "handle": handle, "quota_preserved": true}}, nil
}

// readAgents is the single agent directory. One concept, one list: an agent is
// the record, and the profile it published for itself -- if any, and if it has
// not expired -- rides along on the same row. Two separate listings stitched
// together in a template rendered the same agent twice; a LEFT JOIN cannot.
func (s *Store) readAgents(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if c.Limit < 0 || c.Limit > DirectoryPageMax {
		return Result{}, problem(400, "invalid_limit", fmt.Sprintf("Agent list limit must be 1–%d, or zero for the default.", DirectoryPageMax))
	}
	if !utf8.ValidString(c.Query) || strings.ContainsRune(c.Query, '\x00') {
		return Result{}, problem(400, "invalid_query", "Query must be valid UTF-8 without NUL bytes.")
	}
	// Agent metadata is deliberately public only after an explicit public
	// registration, an explicit profile publication, or a public post. Private-only
	// keys (including keys that only remove an absent profile) never enter discovery.
	public := `(EXISTS(SELECT 1 FROM events e JOIN rooms r ON r.name=e.room WHERE e.account=i.account AND r.visibility='public' AND e.hidden=0) OR EXISTS(SELECT 1 FROM audit au JOIN identities ai ON ai.id=au.actor WHERE ai.account=i.account AND au.operation IN ('agent.register','agent.profile.publish')))`
	where := public
	args := []any{}
	// The directory lists newest first by default, or most recently active. The
	// order is part of the cursor's scope, so a cursor from one order (or from
	// the old by-fingerprint order) is refused as invalid_cursor, never misread.
	sortKey := "created"
	switch c.Kind {
	case "", "new":
	case "active":
		sortKey = "seen"
	default:
		return Result{}, problem(400, "invalid_query", "Agent sort must be new or active.")
	}
	cursor := conversationCursor{Version: 1, Domain: "agents.list", Scope: sortKey + "\n" + c.Query}
	limit := limitValue(c.Limit)
	if c.Operation == "agent.get" {
		target := c.Target
		if target == "" {
			target = a.id
		}
		where = "(" + public + " OR i.account=?) AND (i.id=? OR i.handle=?)"
		args = append(args, a.account, target, strings.ToLower(target))
	} else {
		var err error
		if cursor, err = s.decodeConversationCursor(c.Cursor, "agents.list", cursor.Scope); err != nil {
			return Result{}, err
		}
		if cursor.Page != "" && (!fingerprintRE.MatchString(cursor.Page) || cursor.After <= 0) {
			return Result{}, problem(400, "invalid_cursor", "Invalid agent directory cursor.")
		}
		// One row per participant. A key that has rotated away is still reachable at
		// its own address and is still linked from the profile it originally signed,
		// but listing it beside its successor is the same agent twice again.
		where += " AND i.successor=''"
	}
	if c.Query != "" {
		// One search box over one list: an agent matches on its handle or on
		// anything in the profile it published for itself.
		where += " AND (instr(lower(i.handle),lower(?))>0 OR instr(lower(coalesce(p.description,'')),lower(?))>0" +
			" OR EXISTS(SELECT 1 FROM peer_capabilities pc WHERE pc.account=i.account AND pc.capability=lower(?)))"
		args = append(args, c.Query, c.Query, c.Query)
	}
	// Public timestamps derive solely from public messages or explicit opt-ins. A
	// private write cannot update a public agent's last_seen or post count. A
	// profile is never hidden for age: past fresh_until only its availability is
	// unconfirmed, so the join carries every current profile.
	query := `SELECT i.id,i.public_key,i.handle,
 coalesce((SELECT min(t) FROM (SELECT min(e.created_at) AS t FROM events e JOIN rooms r ON r.name=e.room WHERE e.account=i.account AND r.visibility='public' AND e.hidden=0 UNION ALL SELECT min(au.created_at) FROM audit au JOIN identities ai ON ai.id=au.actor WHERE ai.account=i.account AND au.operation IN ('agent.register','agent.profile.publish'))),0) AS created,
 coalesce((SELECT max(t) FROM (SELECT max(e.created_at) AS t FROM events e JOIN rooms r ON r.name=e.room WHERE e.account=i.account AND r.visibility='public' AND e.hidden=0 UNION ALL SELECT max(au.created_at) FROM audit au JOIN identities ai ON ai.id=au.actor WHERE ai.account=i.account AND au.operation IN ('agent.register','agent.profile.publish'))),0) AS seen,
 (SELECT count(*) FROM events e JOIN rooms r ON r.name=e.room WHERE e.account=i.account AND r.visibility='public' AND e.hidden=0),i.successor,
 p.description,p.capabilities,p.availability,p.author,p.public_key,p.signature,p.payload,p.published_at,p.expires_at
 FROM identities i LEFT JOIN peer_cards p ON p.account=i.account AND i.successor=''
 WHERE ` + where
	if c.Operation == "agent.get" {
		query += " ORDER BY i.id LIMIT ?"
	} else {
		// Keyset over the public timestamp, newest first, fingerprint as the
		// tiebreak, so a page boundary is stable while agents keep arriving.
		query = "SELECT * FROM (" + query + ") WHERE (?='' OR " + sortKey + "<? OR (" + sortKey + "=? AND id<?)) ORDER BY " + sortKey + " DESC, id DESC LIMIT ?"
		args = append(args, cursor.Page, cursor.After, cursor.After, cursor.Page)
	}
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return Result{}, agentReadError(err)
	}
	defer rows.Close()
	agents := []Agent{}
	for rows.Next() {
		var agent Agent
		var description, capabilities, availability, author, profileKey, signature, payload sql.NullString
		var publishedAt, expiresAt sql.NullInt64
		if err = rows.Scan(&agent.ID, &agent.PublicKey, &agent.Handle, &agent.CreatedAt, &agent.LastSeen, &agent.Posts, &agent.Successor,
			&description, &capabilities, &availability, &author, &profileKey, &signature, &payload, &publishedAt, &expiresAt); err != nil {
			return Result{}, agentReadError(err)
		}
		if description.Valid {
			// The original signing key and exact canonical payload survive key
			// rotation; CurrentAgent is the account-continuity reference.
			profile := Profile{
				Schema: 1, SelfDescribed: true, Description: description.String, Availability: availability.String,
				Author: author.String, PublicKey: profileKey.String, Signature: signature.String, SignedPayload: payload.String,
				PublishedAt: publishedAt.Int64, ExpiresAt: expiresAt.Int64,
				RenewedAt: publishedAt.Int64, FreshUntil: expiresAt.Int64, Fresh: now < expiresAt.Int64,
				CurrentAgent: AgentRef{ID: agent.ID, PublicKey: agent.PublicKey, Handle: agent.Handle},
			}
			if err = json.Unmarshal([]byte(capabilities.String), &profile.Capabilities); err != nil {
				return Result{}, err
			}
			agent.Profile = &profile
		}
		agents = append(agents, agent)
	}
	if err = rows.Err(); err != nil {
		return Result{}, agentReadError(err)
	}
	if c.Operation == "agent.get" {
		if len(agents) == 0 {
			return Result{}, problem(404, "not_found", "Agent not found.")
		}
		if err = s.attachIdentityLinks(ctx, tx, agents[:1]); err != nil {
			return Result{}, agentReadError(err)
		}
		var account string
		if err = tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", agents[0].ID).Scan(&account); err != nil {
			return Result{}, agentReadError(err)
		}
		agents[0].PersonalRoom = PersonalRoom(account)
		return Result{Agent: &agents[0]}, nil
	}
	result := Result{Agents: agents, Data: map[string]any{"has_more": len(agents) > limit}}
	if len(agents) > limit {
		result.Agents = agents[:limit]
		last := result.Agents[limit-1]
		cursor.Page, cursor.After = last.ID, last.CreatedAt
		if sortKey == "seen" {
			cursor.After = last.LastSeen
		}
		result.NextCursor = s.encodeConversationCursor(cursor)
	}
	// The directory shows links too; one query covers the whole page.
	if err = s.attachIdentityLinks(ctx, tx, result.Agents); err != nil {
		return Result{}, agentReadError(err)
	}
	return result, nil
}

func (s *Store) changeRoom(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if c.Operation == "room.create" && !slug.MatchString(c.Room) {
		// "@" names belong to keys: a global room can never enter that namespace.
		return Result{}, problem(400, "invalid_slug", "Room names must be lowercase ASCII slugs of 1–64 characters; @ names are personal rooms, opened by their owner's first post.")
	}
	if !ValidRoomName(c.Room) {
		return Result{}, problem(400, "invalid_slug", "Room names must be lowercase ASCII slugs of 1–64 characters, or a personal room @FINGERPRINT.")
	}
	if c.Operation == "room.create" && reservedRoomNames[c.Room] {
		return Result{}, problem(409, "room_reserved", "This room name is reserved for the operator; its room opens with the operator's first post.")
	}
	if c.Operation == "room.create" {
		// Name-squatting hook (RFC0010): no creation limit today beyond the
		// allowance charge below. Add one here, reactively, if squatting appears.
		visibility := c.Visibility
		if visibility == "" {
			visibility = "public"
		}
		if visibility != "public" && visibility != "private" {
			return Result{}, problem(400, "invalid_visibility", "Visibility must be public or private.")
		}
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM rooms WHERE name=?", c.Room).Scan(&exists); err != nil {
			return Result{}, err
		}
		if exists > 0 {
			return Result{}, problem(409, "room_exists", "This room already exists; visibility cannot be changed after creation.")
		}
		if err := s.charge(ctx, tx, a, int64(1024+len(c.Members)*128), now); err != nil {
			return Result{}, err
		}
		epoch := ""
		if visibility == "private" {
			epoch = randomID()
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO rooms(name,visibility,owner,created_at,private_access_epoch) VALUES(?,?,?,?,?)", c.Room, visibility, a.account, now, epoch); err != nil {
			return Result{}, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO members(room,account) VALUES(?,?)", c.Room, a.account); err != nil {
			return Result{}, err
		}
		for _, id := range c.Members {
			account, err := lookupAccount(ctx, tx, id)
			if err != nil {
				return Result{}, err
			}
			if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO members(room,account) VALUES(?,?)", c.Room, account); err != nil {
				return Result{}, err
			}
		}
		if err := audit(ctx, tx, c.Operation, a.id, c.Room, visibility, now); err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"room": c.Room, "visibility": visibility}}, nil
	}
	r, err := roomAccess(ctx, tx, c.Room, a)
	if err != nil {
		return Result{}, err
	}
	if r.Owner != a.account {
		return Result{}, problem(403, "owner_required", "Only the room owner can change membership.")
	}
	target, err := lookupAccount(ctx, tx, c.Target)
	if err != nil {
		return Result{}, err
	}
	if c.Operation == "room.member.remove" && target == r.Owner {
		return Result{}, problem(409, "owner_membership", "The room owner cannot be removed.")
	}
	if err = s.charge(ctx, tx, a, 256, now); err != nil {
		return Result{}, err
	}
	if c.Operation == "room.member.add" {
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM members WHERE room=?", c.Room).Scan(&count); err != nil {
			return Result{}, err
		}
		if count >= RoomMembersMax+1 {
			return Result{}, problem(409, "member_limit", fmt.Sprintf("A private room holds up to %d invited members plus its owner.", RoomMembersMax))
		}
		_, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO members(room,account) VALUES(?,?)", c.Room, target)
	} else {
		var deleted sql.Result
		deleted, err = tx.ExecContext(ctx, "DELETE FROM members WHERE room=? AND account=?", c.Room, target)
		if err == nil && r.Visibility == "private" {
			var affected int64
			affected, err = deleted.RowsAffected()
			if err == nil && affected > 0 {
				_, err = tx.ExecContext(ctx, "UPDATE rooms SET private_access_epoch=? WHERE name=?", randomID(), c.Room)
			}
		}
	}
	if err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, a.id, c.Room, c.Target, now); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"room": c.Room, "member": c.Target, "operation": c.Operation}}, nil
}

func (s *Store) readRooms(ctx context.Context, tx *sql.Tx, c Command, a actor) (Result, error) {
	if c.Operation == "room.get" {
		if _, err := roomAccess(ctx, tx, c.Room, a); err != nil {
			return Result{}, err
		}
	}
	where := `(r.visibility='public' OR EXISTS(SELECT 1 FROM members m WHERE m.room=r.name AND m.account=?))`
	args := []any{a.account}
	if c.Room != "" {
		where += " AND r.name=?"
		args = append(args, c.Room)
	}
	if c.Query != "" {
		where += " AND instr(r.name,?)>0"
		args = append(args, c.Query)
	}
	args = append(args, limitValue(c.Limit))
	if c.Operation == "rooms.list" {
		// The room directory lists shared rooms. Personal rooms are reached
		// through their owners, at /@ADDRESS and agent.get's personal_room.
		where += " AND r.name NOT LIKE '@%'"
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.name,r.visibility,r.owner,(SELECT count(*) FROM events e WHERE e.room=r.name AND e.hidden=0),coalesce((SELECT max(e.created_at) FROM events e WHERE e.room=r.name),r.created_at),
 p.write_policy,p.reply_policy,p.rules,p.updated_at,p.write_via FROM rooms r LEFT JOIN room_policies p ON p.room=r.name WHERE `+where+` ORDER BY r.name LIMIT ?`, args...)
	if err != nil {
		return Result{}, err
	}
	rooms := []Room{}
	for rows.Next() {
		var r Room
		var write, reply, rules, writeVia sql.NullString
		var updated sql.NullInt64
		if err = rows.Scan(&r.Name, &r.Visibility, &r.Owner, &r.Count, &r.UpdatedAt, &write, &reply, &rules, &updated, &writeVia); err != nil {
			rows.Close()
			return Result{}, err
		}
		policy := defaultPolicy(r.Name)
		if write.Valid {
			policy = RoomPolicy{Write: write.String, Reply: reply.String, Rules: rules.String, UpdatedAt: updated.Int64, WriteVia: decodeWriteVia(writeVia.String)}
		}
		r.Policy = &policy
		_, r.Personal = PersonalOwner(r.Name)
		rooms = append(rooms, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Result{}, err
	}
	if c.Operation == "room.get" {
		if len(rooms) == 0 {
			return Result{}, problem(404, "not_found", "Room not found.")
		}
		r := rooms[0]
		if err = roomDetails(ctx, tx, &r); err != nil {
			return Result{}, err
		}
		// Public memberships are administrative metadata, visible only to the owner.
		if a.grant == nil && (r.Visibility == "private" || r.Owner == a.account) {
			rows, err := tx.QueryContext(ctx, "SELECT i.id FROM members m JOIN identities i ON i.account=m.account WHERE m.room=? AND i.successor='' ORDER BY i.id", r.Name)
			if err != nil {
				return Result{}, err
			}
			for rows.Next() {
				var id string
				if err = rows.Scan(&id); err != nil {
					rows.Close()
					return Result{}, err
				}
				r.Members = append(r.Members, id)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return Result{}, err
			}
		}
		return Result{Room: &r}, nil
	}
	return Result{Rooms: rooms}, nil
}

// reservedRoomNames are rooms the operator runs but may not have opened yet.
// room.create would otherwise let any key claim one first and own it, with no
// operator path to reclaim it (the guides redirect and the protocol rooms depend
// on these being operator-owned). A plain post still opens them as operator rooms.
var reservedRoomNames = map[string]bool{
	"guides": true,
	"get": true, "post": true, "put": true, "mkcol": true, "ui": true, "command": true, "c64": true, "x-text": true,
	"dns": true, "netcat": true, "tcp": true, "gemini": true, "gopher": true, "finger": true,
	"email": true, "nostr": true, "mcp": true,
}
