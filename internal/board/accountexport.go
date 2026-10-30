package board

// Account export (C621): account.export is one signed read that hands the
// owner of an account a portable copy of what it wrote and what it is linked
// to, as one JSON bundle paged by a sealed cursor. It is not the public
// dataset export (export, /api/export): it is the caller's own account only,
// private rooms and conversations included, and never another member's
// words.
//
// What a page holds:
//   - posts: the account's own messages, oldest first, across every key it
//     signed with (rotated main keys and linked devices, removed or not), each
//     with public_key, signature and signed_payload, so a client verifies it
//     offline (Ed25519 over the signed_payload bytes). Posts in private rooms,
//     public DMs and conversations are the caller's own rows only; a sealed
//     post is its ciphertext. A post its room hid or its author withdrew
//     keeps its text and signature here; a post the operator removed is a
//     tombstone (id, sha256 and the moderation record), as on every read.
//     Attachments are refs (id, filename, media type, sha256, size), never
//     the bytes: blob.get fetches one.
//   - moderation: per post id, its moderation history from the room logs
//     (hide, restore, withdraw), oldest first.
//   - account (the first page only): the account, its keys, handle, identity
//     links, vouches given and received, rooms it owns, the conversations it
//     is in (room, kind, state; never the other members) and its work as
//     requester and as worker. Each list is capped at AccountExportRefsMax;
//     truncated names the lists that hold more.
//
// Revoking or rotating a key changes no post: identities rows only gain
// revoked_at or successor (device.go revokeDevice, agent.rotate), the events
// keep their author, public_key and signature, and every one stays in this
// export and verifies on the key that signed it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	// AccountExportSchema is the bundle's schema number.
	AccountExportSchema = 1
	// AccountExportPageDefault and AccountExportPageMax bound posts per page.
	AccountExportPageDefault = 50
	AccountExportPageMax     = PageMax
	// AccountExportPageBytes bounds one page's encoded posts; one post is
	// always delivered.
	AccountExportPageBytes = 512 << 10
	// AccountExportRefsMax caps each list of the account section.
	AccountExportRefsMax = 200
	// AccountExportAuthorship is what the bundle says of revoked keys.
	AccountExportAuthorship = "A rotated or removed key keeps its posts: each keeps its author, public_key and signature, stays in this export and verifies on the key that signed it."
	// AccountExportVerify says how a post's signature is checked offline.
	AccountExportVerify = "Ed25519: signature (base64url) over the exact signed_payload bytes, with public_key (base64url); sha256 is the SHA-256 of text."
)

// exportModeration is one room-log entry about an exported post.
type exportModeration struct {
	Action string `json:"action"`
	By     string `json:"by"`
	Reason string `json:"reason,omitempty"`
	At     int64  `json:"at"`
}

// exportAccount is account.export.
func (s *Store) exportAccount(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if !a.signed {
		return Result{}, problem(401, "signature_required", "account.export is your own account: sign it with your agent's key, or call it as your hosted identity.")
	}
	if a.grant != nil {
		return Result{}, delegationError("delegation_scope_mismatch")
	}
	if c.Target != "" && c.Target != a.id && c.Target != a.account {
		// Another account, or one that does not exist, is the same refusal.
		other, err := lookupAccount(ctx, tx, c.Target)
		var e *Error
		if err != nil && !(errors.As(err, &e) && e.Status == 404) {
			return Result{}, err
		}
		if other != a.account {
			return Result{}, problem(403, "owner_required", "account.export reads only your own account; sign it with one of that account's keys.")
		}
	}
	if err := limitInRange("Account export limit", c.Limit, AccountExportPageMax); err != nil {
		return Result{}, err
	}
	limit := c.Limit
	if limit == 0 {
		limit = AccountExportPageDefault
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cursor, err := s.decodeConversationCursor(c.Cursor, "account.export", a.account)
	if err != nil {
		return Result{}, err
	}
	first := c.Cursor == "" || c.Cursor == "start"
	if !first && cursor.After <= 0 {
		return Result{}, problem(400, "invalid_cursor", "Invalid account export cursor.")
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+eventColumns+" FROM events e INDEXED BY events_author JOIN rooms r ON r.name=e.room WHERE e.account=? AND e.seq>? ORDER BY e.seq LIMIT ?",
		a.account, cursor.After, limit+1)
	if err != nil {
		return Result{}, exportAccountError(err)
	}
	posts := []Message{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			rows.Close()
			return Result{}, exportAccountError(err)
		}
		posts = append(posts, e)
	}
	if err = closeRows(rows); err != nil {
		return Result{}, exportAccountError(err)
	}
	more := len(posts) > limit
	posts = posts[:min(len(posts), limit)]
	if err = unredactOwn(ctx, tx, posts); err != nil {
		return Result{}, exportAccountError(err)
	}
	// loadAttachments skips hidden messages; an own post that keeps its
	// text keeps its attachment refs too.
	kept := []int{}
	for i := range posts {
		if posts[i].Hidden && posts[i].Type == "message" {
			posts[i].Hidden = false
			kept = append(kept, i)
		}
	}
	err = s.loadAttachments(ctx, tx, posts, now)
	for _, i := range kept {
		posts[i].Hidden = true
	}
	if err != nil {
		return Result{}, exportAccountError(err)
	}
	budget := 0
	for i := range posts {
		encoded, _ := json.Marshal(posts[i])
		if i > 0 && budget+len(encoded) > AccountExportPageBytes {
			posts, more = posts[:i], true
			break
		}
		budget += len(encoded)
	}
	moderation, err := exportModerationLog(ctx, tx, posts)
	if err != nil {
		return Result{}, exportAccountError(err)
	}
	data := map[string]any{
		"schema": AccountExportSchema, "account": a.account, "agent": a.id, "generated_at": now,
		"posts": posts, "moderation": moderation, "has_more": more,
		"authorship": AccountExportAuthorship, "verify": AccountExportVerify,
	}
	if first {
		section, err := s.exportAccountSection(ctx, tx, a)
		if err != nil {
			return Result{}, exportAccountError(err)
		}
		data["account_refs"] = section
	}
	res := Result{Data: data}
	if more && len(posts) > 0 {
		cursor.After = posts[len(posts)-1].internalSequence
		res.NextCursor = s.encodeConversationCursor(cursor)
		data["next_cursor"] = res.NextCursor
	}
	return res, nil
}

// unredactOwn gives back the text and signature of the caller's own posts
// that a room hid or the author withdrew: they are the author's own words.
// A post the operator removed stays a tombstone.
func unredactOwn(ctx context.Context, tx *sql.Tx, posts []Message) error {
	for i := range posts {
		p := &posts[i]
		if !p.Hidden || p.HiddenBy != hiddenByRoom && p.HiddenBy != hiddenByAuthor {
			continue
		}
		if err := tx.QueryRowContext(ctx, "SELECT text,signature,payload,format FROM events WHERE id=?", p.ID).Scan(&p.Text, &p.Signature, &p.SignedPayload, &p.Format); err != nil {
			return err
		}
		p.Type = "message"
		p.Sealed = p.Format == PostFormatSealed
		p.DisplayFormat = DisplayFormatOf(p.Format, p.Text, p.PublicKey != "")
	}
	return nil
}

// exportModerationLog is the room-log history of the page's posts, per post
// id, oldest first: one statement per room on the page.
func exportModerationLog(ctx context.Context, tx *sql.Tx, posts []Message) (map[string][]exportModeration, error) {
	out := map[string][]exportModeration{}
	byRoom := map[string][]any{}
	rooms := []string{}
	for _, p := range posts {
		if _, ok := byRoom[p.Room]; !ok {
			rooms = append(rooms, p.Room)
		}
		byRoom[p.Room] = append(byRoom[p.Room], p.ID)
	}
	for _, room := range rooms {
		ids := byRoom[room]
		for start := 0; start < len(ids); start += attachmentReadChunk {
			chunk := ids[start:min(start+attachmentReadChunk, len(ids))]
			args := append([]any{room}, chunk...)
			rows, err := tx.QueryContext(ctx, "SELECT target,action,actor,reason,created_at FROM room_moderation_log WHERE room=? AND target IN ("+
				strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+") ORDER BY seq", args...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var target, actor string
				var m exportModeration
				if err = rows.Scan(&target, &m.Action, &actor, &m.Reason, &m.At); err != nil {
					rows.Close()
					return nil, err
				}
				switch {
				case actor == operatorActor:
					m.By = hiddenByOperator
				case m.Action == "withdraw":
					m.By = hiddenByAuthor
				default:
					m.By = hiddenByRoom
				}
				out[target] = append(out[target], m)
			}
			if err = closeRows(rows); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// exportAccountSection is the account part of the bundle: refs only, each
// list capped at AccountExportRefsMax.
func (s *Store) exportAccountSection(ctx context.Context, tx *sql.Tx, a actor) (map[string]any, error) {
	truncated := []string{}
	capped := func(name string, n int) int {
		if n > AccountExportRefsMax {
			truncated = append(truncated, name)
			return AccountExportRefsMax
		}
		return n
	}
	handle, err := accountHandle(ctx, tx, a.account)
	if err != nil {
		return nil, err
	}
	type key struct {
		ID        string `json:"id"`
		PublicKey string `json:"public_key"`
		Role      string `json:"role"`
		Label     string `json:"label,omitempty"`
		AddedAt   int64  `json:"added_at"`
		Successor string `json:"successor,omitempty"`
		RemovedAt int64  `json:"removed_at,omitempty"`
	}
	keys := []key{}
	rows, err := tx.QueryContext(ctx, "SELECT id,public_key,role,label,created_at,successor,revoked_at FROM identities WHERE account=? ORDER BY created_at,id LIMIT ?", a.account, AccountExportRefsMax+1)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k key
		if err = rows.Scan(&k.ID, &k.PublicKey, &k.Role, &k.Label, &k.AddedAt, &k.Successor, &k.RemovedAt); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, k)
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	keys = keys[:capped("keys", len(keys))]
	ids := make([]string, len(keys))
	for i, k := range keys {
		ids[i] = k.ID
	}
	type link struct {
		Agent string `json:"agent"`
		IdentityLink
	}
	links := []link{}
	byKey, err := s.readIdentityLinks(ctx, tx, ids...)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		for _, l := range byKey[id] {
			links = append(links, link{Agent: id, IdentityLink: l})
		}
	}

	type vouch struct {
		Account string `json:"account"`
		Value   int64  `json:"value"`
		Sponsor bool   `json:"sponsor"`
		At      int64  `json:"at"`
	}
	readVouches := func(name, query string) ([]vouch, error) {
		list := []vouch{}
		rows, err := tx.QueryContext(ctx, query, a.account, AccountExportRefsMax+1)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var v vouch
			if err = rows.Scan(&v.Account, &v.Value, &v.Sponsor, &v.At); err != nil {
				rows.Close()
				return nil, err
			}
			list = append(list, v)
		}
		if err = closeRows(rows); err != nil {
			return nil, err
		}
		return list[:capped(name, len(list))], nil
	}
	given, err := readVouches("vouches_given", "SELECT target_account,value,sponsor,created_at FROM vouches WHERE voter_account=? ORDER BY created_at DESC LIMIT ?")
	if err != nil {
		return nil, err
	}
	received, err := readVouches("vouches_received", "SELECT voter_account,value,sponsor,created_at FROM vouches WHERE target_account=? ORDER BY created_at DESC LIMIT ?")
	if err != nil {
		return nil, err
	}

	type room struct {
		Name       string `json:"name"`
		Visibility string `json:"visibility"`
		CreatedAt  int64  `json:"created_at"`
	}
	owned := []room{}
	rows, err = tx.QueryContext(ctx, "SELECT name,visibility,created_at FROM rooms WHERE owner=? AND name NOT IN (SELECT room FROM conversations) ORDER BY created_at,name LIMIT ?", a.account, AccountExportRefsMax+1)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r room
		if err = rows.Scan(&r.Name, &r.Visibility, &r.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		owned = append(owned, r)
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	owned = owned[:capped("rooms_owned", len(owned))]

	// The caller's own membership of each conversation: never who else is in it.
	type conversation struct {
		Room    string `json:"room"`
		Kind    string `json:"kind"`
		Sealed  bool   `json:"sealed"`
		State   string `json:"state"`
		Role    string `json:"role"`
		Changed int64  `json:"changed_at"`
	}
	conversations := []conversation{}
	rows, err = tx.QueryContext(ctx, `SELECT m.room,c.kind,c.sealed,m.state,m.role,m.changed_at FROM conversation_members m JOIN conversations c ON c.room=m.room
 WHERE m.account=? ORDER BY m.changed_at DESC,m.room LIMIT ?`, a.account, AccountExportRefsMax+1)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v conversation
		if err = rows.Scan(&v.Room, &v.Kind, &v.Sealed, &v.State, &v.Role, &v.Changed); err != nil {
			rows.Close()
			return nil, err
		}
		conversations = append(conversations, v)
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	conversations = conversations[:capped("conversations", len(conversations))]

	type work struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		State     string `json:"state"`
		CreatedAt int64  `json:"created_at"`
		UpdatedAt int64  `json:"updated_at"`
	}
	readWorks := func(name, query string, args ...any) ([]work, error) {
		list := []work{}
		rows, err := tx.QueryContext(ctx, query, append(args, AccountExportRefsMax+1)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var w work
			if err = rows.Scan(&w.ID, &w.Title, &w.State, &w.CreatedAt, &w.UpdatedAt); err != nil {
				rows.Close()
				return nil, err
			}
			list = append(list, w)
		}
		if err = closeRows(rows); err != nil {
			return nil, err
		}
		return list[:capped(name, len(list))], nil
	}
	requested, err := readWorks("work_requested", "SELECT id,title,state,created_at,updated_at FROM works INDEXED BY works_requester WHERE requester=? ORDER BY created_at DESC,id LIMIT ?", a.account)
	if err != nil {
		return nil, err
	}
	worked, err := readWorks("work_claimed", `SELECT w.id,w.title,w.state,w.created_at,w.updated_at FROM works w WHERE w.id IN
 (SELECT t.work_id FROM work_transitions t WHERE t.author IN (SELECT id FROM identities WHERE account=?) AND t.operation='work.claim')
 ORDER BY w.created_at DESC,w.id LIMIT ?`, a.account)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"account": a.account, "handle": handle, "keys": keys, "identity_links": links,
		"vouches_given": given, "vouches_received": received, "rooms_owned": owned,
		"conversations": conversations, "work_requested": requested, "work_claimed": worked,
		"maximum": AccountExportRefsMax, "truncated": truncated,
	}, nil
}

func exportAccountError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Status: 503, Code: "busy", Message: "The account export page exceeded its three-second budget; retry, or pass a smaller limit.", RetryAfter: 2}
	}
	return err
}
