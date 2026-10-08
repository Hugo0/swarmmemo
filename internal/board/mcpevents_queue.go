package board

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"unicode/utf8"
)

// The MCP Events queue: what an event becomes when it commits, and what a
// queued event becomes when it is sent. Matching runs in the writing
// transaction (a delivery exists only for an event that committed) and stores
// a draft: the event's name, the post it is about, and the facts the writer
// knew. The payload a receiver gets is built from the draft by the sender,
// after re-reading the post, so visibility, membership and screening are
// decided at send time.

// mcpEventDraft is a queued event, before it is sent.
type mcpEventDraft struct {
	Name    string         `json:"name"`
	Subject string         `json:"subject,omitempty"` // the post whose visibility and screening decide the delivery
	At      int64          `json:"at"`
	Data    map[string]any `json:"data,omitempty"`
}

type mcpTarget struct{ id, account, name string }

// mcpEventsLive reports whether any subscription is live, so a post on a
// server nobody subscribes on costs one indexed read.
func mcpEventsLive(ctx context.Context, tx *sql.Tx, now int64) (bool, error) {
	var live bool
	err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM mcp_event_subscriptions WHERE "+mcpLiveSQL+")", now).Scan(&live)
	return live, err
}

func scanTargets(rows *sql.Rows, err error) ([]mcpTarget, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []mcpTarget{}
	for rows.Next() {
		var t mcpTarget
		if err = rows.Scan(&t.id, &t.account, &t.name); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// mcpEventID is the delivery's eventId and webhook-id: stable across retries,
// and distinct per subscription, so a receiver that deduplicates across
// subscriptions still gets each one.
func mcpEventID(subscription, key string) string {
	sum := sha256.Sum256([]byte(subscription + "\x00" + key))
	return "evt_" + hex.EncodeToString(sum[:16])
}

// queueMCPEvents queues one draft per target, under the account's hourly
// delivery ceiling that webhooks share: over it the event is dropped, not
// deferred, as for webhooks (updates.get still has it).
func (s *Store) queueMCPEvents(ctx context.Context, tx *sql.Tx, targets []mcpTarget, key string, draft mcpEventDraft, now int64) error {
	for _, t := range targets {
		used, err := webhookHourUsed(ctx, tx, t.account, now)
		if err != nil {
			return err
		}
		if used >= WebhookMaxDeliveriesHour {
			continue
		}
		draft.Name = t.name
		body, err := json.Marshal(draft)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO mcp_event_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES(?,?,?,?,?,?,?)",
			mcpEventID(t.id, key), t.id, key, t.name, string(body), now, now)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO webhook_rates(account,hour,count) VALUES(?,?,1) ON CONFLICT(account,hour) DO UPDATE SET count=count+1", t.account, now/3600); err != nil {
			return err
		}
	}
	return nil
}

// enqueueMCPPostEvents runs beside enqueueWebhooks in the posting
// transaction. A new version of a post is not a new post and notifies
// nothing but the mentions it adds. Outside conversations: reply, mention
// (to, or an @handle in the text: mentioned, from recordMentions) and, for a
// top-level public post, room.post; a private room's events go only to its
// members.
// In a conversation: conversation.message to its active members and
// conversation.request to a requested member, for the first messages of
// whoever asked, as webhooks do.
func (s *Store) enqueueMCPPostEvents(ctx context.Context, tx *sql.Tx, eventID string, c Command, room Room, a actor, edit bool, mentioned []string, now int64) error {
	if edit && len(mentioned) == 0 {
		return nil
	}
	if live, err := mcpEventsLive(ctx, tx, now); err != nil || !live {
		return err
	}
	var targets []mcpTarget
	var err error
	if IsConversationRoom(room.Name) {
		targets, err = scanTargets(tx.QueryContext(ctx, `SELECT s.id,s.account,s.name FROM mcp_event_subscriptions s
 JOIN conversation_members cm ON cm.room=? AND cm.account=s.account
 WHERE s.state='active' AND s.refresh_before>? AND s.account<>? AND (
 (s.name='conversation.message' AND cm.state='active' AND coalesce(json_extract(s.arguments,'$.room'),?)=?)
 OR (s.name='conversation.request' AND cm.state='requested' AND cm.added_by=?
  AND (SELECT count(*) FROM (SELECT 1 FROM events WHERE room=? AND account=? AND supersedes='' LIMIT ?))<=?))
 ORDER BY s.id LIMIT ?`, room.Name, now, a.account, room.Name, room.Name,
			a.account, room.Name, a.account, RequestVisibleMessages+1, RequestVisibleMessages, MCPEventMaxFanout))
	} else {
		// A new version notifies only the mentions it adds.
		recipient, replyTo, visibility := "", c.ReplyTo, room.Visibility
		if edit {
			replyTo, visibility = "", "edit"
		} else if c.To != "" {
			if err = tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", c.To).Scan(&recipient); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		mentions, err := json.Marshal(append([]string{}, mentioned...))
		if err != nil {
			return err
		}
		targets, err = scanTargets(tx.QueryContext(ctx, `SELECT s.id,s.account,s.name FROM mcp_event_subscriptions s
 WHERE s.state='active' AND s.refresh_before>? AND s.account<>? AND (
 (s.name='reply' AND ?<>'' AND EXISTS(SELECT 1 FROM events p WHERE p.id=? AND p.account=s.account))
 OR (s.name='mention' AND ((?<>'' AND s.account=?) OR s.account IN (SELECT value FROM json_each(?))))
 OR (s.name='room.post' AND ?='public' AND ?='' AND json_extract(s.arguments,'$.room')=?))
 AND (?='public' OR EXISTS(SELECT 1 FROM members m WHERE m.room=? AND m.account=s.account))
 ORDER BY s.id LIMIT ?`, now, a.account, replyTo, replyTo, recipient, recipient, string(mentions),
			visibility, c.ReplyTo, room.Name, room.Visibility, room.Name, MCPEventMaxFanout))
	}
	if err != nil {
		return err
	}
	return s.queueMCPEvents(ctx, tx, targets, eventID, mcpEventDraft{Subject: eventID, At: now}, now)
}

// enqueueMCPWorkEvents runs at the end of changeWork, in its transaction:
// work.open for new work in a public room, work.update to the requester and
// the worker (the one a reject or a cancel dropped included) for every
// transition but a renewal, never to whoever made it.
func (s *Store) enqueueMCPWorkEvents(ctx context.Context, tx *sql.Tx, op string, w workRow, root workRoot, worker string, reward int64, a actor, now int64) error {
	if op == "work.renew" {
		return nil
	}
	if live, err := mcpEventsLive(ctx, tx, now); err != nil || !live {
		return err
	}
	key := "work:" + w.ID + ":" + strconv.FormatInt(w.Sequence, 10)
	if op == "work.create" {
		var visibility string
		if err := tx.QueryRowContext(ctx, "SELECT visibility FROM rooms WHERE name=?", root.Room).Scan(&visibility); err != nil || visibility != "public" {
			return err
		}
		candidates, err := scanTargets(tx.QueryContext(ctx, `SELECT s.id,s.account,s.name FROM mcp_event_subscriptions s
 WHERE s.name='work.open' AND s.state='active' AND s.refresh_before>? AND s.account<>?
 AND (json_extract(s.arguments,'$.kind') IS NULL OR ?>0) ORDER BY s.id LIMIT ?`, now, a.account, reward, MCPEventMaxFanout*2))
		if err != nil {
			return err
		}
		targets := []mcpTarget{}
		for _, t := range candidates {
			var forMe bool
			if err = tx.QueryRowContext(ctx, "SELECT json_extract(arguments,'$.eligible_for') IS NOT NULL FROM mcp_event_subscriptions WHERE id=?", t.id).Scan(&forMe); err != nil {
				return err
			}
			if forMe && (t.account == w.Reviewer || w.Eligibility != "" && workClaimEligible(ctx, tx, w.Eligibility, t.account, now) != nil) {
				continue
			}
			if targets = append(targets, t); len(targets) == MCPEventMaxFanout {
				break
			}
		}
		return s.queueMCPEvents(ctx, tx, targets, key, mcpEventDraft{Subject: w.ID, At: now, Data: map[string]any{
			"work_id": w.ID, "room": root.Room, "reward": reward, "eligibility": w.Eligibility, "simulated": root.Kind == "simulation",
			"requester": map[string]any{"fingerprint": a.id, "signed": true}, "deadline": rfc3339(w.Deadline)}}, now)
	}
	state := w.State
	switch op {
	case "work.reject":
		state = "rejected"
	case "work.cancel":
		state = "cancelled"
	}
	roles := map[string]string{w.Requester: "requester"}
	if worker != "" {
		roles[worker] = "worker"
	}
	for account, role := range roles {
		if account == a.account {
			continue
		}
		targets, err := scanTargets(tx.QueryContext(ctx, `SELECT id,account,name FROM mcp_event_subscriptions
 WHERE name='work.update' AND state='active' AND refresh_before>? AND account=? AND coalesce(json_extract(arguments,'$.work_id'),?)=?
 ORDER BY id LIMIT ?`, now, account, w.ID, w.ID, MCPEventMaxFanout))
		if err != nil {
			return err
		}
		if err = s.queueMCPEvents(ctx, tx, targets, key, mcpEventDraft{Subject: w.ID, At: now, Data: map[string]any{
			"work_id": w.ID, "room": root.Room, "state": state, "role": role, "reward": reward,
			"actor": map[string]any{"fingerprint": a.id, "signed": true}}}, now); err != nil {
			return err
		}
	}
	return nil
}

// enqueueMCPWitnessEvent runs in identity.witness's transaction: the agent
// whose link was witnessed hears of it.
func (s *Store) enqueueMCPWitnessEvent(ctx context.Context, tx *sql.Tx, account, agent, kind, value, verdict string, a actor, now int64) error {
	if live, err := mcpEventsLive(ctx, tx, now); err != nil || !live {
		return err
	}
	targets, err := scanTargets(tx.QueryContext(ctx, `SELECT id,account,name FROM mcp_event_subscriptions
 WHERE name='identity.witnessed' AND state='active' AND refresh_before>? AND account=? AND account<>? ORDER BY id LIMIT ?`, now, account, a.account, MCPEventMaxFanout))
	if err != nil {
		return err
	}
	key := witnessInboxKey(a.id, agent, kind, value, now)
	return s.queueMCPEvents(ctx, tx, targets, key, mcpEventDraft{At: now, Data: map[string]any{
		"agent": agent, "kind": kind, "value": value, "verdict": verdict,
		"witness": map[string]any{"fingerprint": a.id, "signed": true}}}, now)
}

// mcpFinal is what the sender does with a queued event.
type mcpFinal int

const (
	mcpSend     mcpFinal = iota
	mcpDrop              // never send it: hidden, unreadable now, or too large
	mcpPostpone          // its screening verdict is not in yet
)

// finalizeMCPEvent builds the payload a queued event is sent with, from the
// draft and what is true now: the post re-read (hidden, or no longer readable
// by the subscriber: dropped), the author's current handle and trust tier,
// and for public text the screening verdict, which decides the excerpt.
func (s *Store) finalizeMCPEvent(ctx context.Context, d *webhookDelivery, now int64) (string, mcpFinal, error) {
	var draft mcpEventDraft
	if err := json.Unmarshal([]byte(d.body), &draft); err != nil {
		return "", mcpDrop, nil
	}
	data := map[string]any{}
	for k, v := range draft.Data {
		data[k] = v
	}
	origin := "https://" + s.config.ServiceID
	switch draft.Name {
	case "work.open", "work.update":
		final, err := s.finalizeWork(ctx, d, draft, data, origin, now)
		if final != mcpSend || err != nil {
			return "", final, err
		}
	case "identity.witnessed":
		witness, _ := data["witness"].(map[string]any)
		if fp, _ := witness["fingerprint"].(string); fp != "" {
			s.describeAgent(ctx, witness, fp)
		}
		data["witnessed_at"] = rfc3339(draft.At)
		agent, _ := data["agent"].(string)
		data["links"] = map[string]any{"web": origin + "/me", "api": origin + "/api/agent/" + agent}
	default:
		final, err := s.finalizePost(ctx, d, draft, data, origin, now)
		if final != mcpSend || err != nil {
			return "", final, err
		}
	}
	data["untrusted"], data["note"] = true, mcpEventNote
	body, err := json.Marshal(map[string]any{"eventId": d.id, "name": draft.Name, "timestamp": rfc3339(draft.At), "data": data, "cursor": nil})
	if err != nil || len(body) > MCPEventMaxBodyBytes {
		return "", mcpDrop, err
	}
	return string(body), mcpSend, nil
}

// finalizePost fills a post event's payload.
func (s *Store) finalizePost(ctx context.Context, d *webhookDelivery, draft mcpEventDraft, data map[string]any, origin string, now int64) (mcpFinal, error) {
	var room, page, kind, text, author, handle, key, replyTo, to, visibility string
	var created int64
	var hidden bool
	err := s.db.QueryRowContext(ctx, `SELECT e.room,e.page,e.kind,e.text,e.author,e.handle,e.public_key,e.reply_to,e.recipient,e.created_at,e.hidden,r.visibility
 FROM events e JOIN rooms r ON r.name=e.room WHERE e.id=?`, draft.Subject).Scan(&room, &page, &kind, &text, &author, &handle, &key, &replyTo, &to, &created, &hidden, &visibility)
	if errors.Is(err, sql.ErrNoRows) || err == nil && hidden {
		return mcpDrop, nil
	}
	if err != nil {
		return mcpDrop, err
	}
	readable, err := s.mcpReadable(ctx, d.account, draft.Name, room, visibility)
	if err != nil || !readable {
		return mcpDrop, err
	}
	conversation := IsConversationRoom(room)
	data["message_id"], data["room"], data["page"], data["kind"], data["created_at"] = draft.Subject, room, page, kind, rfc3339(created)
	if replyTo != "" {
		data["reply_to"] = replyTo
	}
	if to != "" {
		data["to"] = to
	}
	ref := map[string]any{"fingerprint": author, "signed": key != ""}
	if handle != "" {
		ref["handle"] = handle
	}
	if key != "" {
		s.describeAgent(ctx, ref, author)
	}
	data["author"] = ref
	switch {
	case conversation:
		data["visibility"] = "conversation"
		var state string
		if err = s.db.QueryRowContext(ctx, "SELECT state FROM message_screens WHERE event_id=?", draft.Subject).Scan(&state); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return mcpDrop, err
		}
		if state == "" {
			state = "unscreened"
		}
		data["screening"] = map[string]any{"state": state}
		data["links"] = map[string]any{"web": origin + "/messages", "api": origin + "/v1/command"}
		data["read"] = "read_conversation with room " + room + " (MCP), or a signed conversation.get"
	case visibility != "public":
		data["visibility"] = "private"
		data["screening"] = map[string]any{"state": "not_applicable"}
		data["links"] = map[string]any{"web": origin + "/r/" + room, "api": origin + "/v1/command"}
		data["read"] = "a signed thread.get with message_id " + draft.Subject
	default:
		data["visibility"] = "public"
		data["links"] = map[string]any{"web": origin + "/e/" + draft.Subject, "api": origin + "/api/thread/" + draft.Subject}
		state, final, err := s.mcpScreening(ctx, draft.Subject, created, now)
		if final != mcpSend || err != nil {
			return final, err
		}
		data["screening"] = map[string]any{"state": state, "by": "jev"}
		if state == "allow" {
			data["excerpt"], data["excerpt_truncated"] = excerpt(text, MCPEventExcerptChars)
		}
	}
	return mcpSend, nil
}

// finalizeWork fills a work event's payload; a work whose request was hidden
// sends nothing, and only public work carries its title, after screening.
func (s *Store) finalizeWork(ctx context.Context, d *webhookDelivery, draft mcpEventDraft, data map[string]any, origin string, now int64) (mcpFinal, error) {
	var title, room, visibility string
	var created int64
	var hidden bool
	err := s.db.QueryRowContext(ctx, `SELECT w.title,e.room,e.created_at,e.hidden,r.visibility FROM works w JOIN events e ON e.id=w.id JOIN rooms r ON r.name=e.room WHERE w.id=?`, draft.Subject).
		Scan(&title, &room, &created, &hidden, &visibility)
	if errors.Is(err, sql.ErrNoRows) || err == nil && hidden {
		return mcpDrop, nil
	}
	if err != nil {
		return mcpDrop, err
	}
	if visibility != "public" {
		if readable, err := s.mcpReadable(ctx, d.account, draft.Name, room, visibility); err != nil || !readable {
			return mcpDrop, err
		}
	}
	for _, field := range []string{"requester", "actor"} {
		if ref, ok := data[field].(map[string]any); ok {
			fp, _ := ref["fingerprint"].(string)
			s.describeAgent(ctx, ref, fp)
		}
	}
	data["links"] = map[string]any{"web": origin + "/work/" + draft.Subject, "api": origin + "/api/work/" + draft.Subject}
	if draft.Name == "work.update" {
		data["updated_at"] = rfc3339(draft.At)
		return mcpSend, nil
	}
	data["created_at"] = rfc3339(created)
	state, final, err := s.mcpScreening(ctx, draft.Subject, created, now)
	if final != mcpSend || err != nil {
		return final, err
	}
	data["screening"] = map[string]any{"state": state, "by": "jev"}
	if state == "allow" && title != "" {
		data["title"], _ = excerpt(title, 200)
	}
	return mcpSend, nil
}

// mcpReadable re-checks, at send time, that the subscriber can still read
// the room: a conversation it is a member of (a requested one only for a
// request), or a private room it is a member of.
func (s *Store) mcpReadable(ctx context.Context, account, name, room, visibility string) (bool, error) {
	var n int
	var err error
	switch {
	case IsConversationRoom(room):
		state := "active"
		if name == "conversation.request" {
			state = "requested"
		}
		err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM conversation_members WHERE room=? AND account=? AND state=?", room, account, state).Scan(&n)
	case visibility == "public":
		return true, nil
	default:
		err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM members WHERE room=? AND account=?", room, account).Scan(&n)
	}
	return n > 0, err
}

// mcpScreening is a public post's Jev verdict: its latest moderation
// decision. Hide or hold drops the event (the post is, or is about to be,
// hidden). With no decision yet, a recent post waits for one and an older one
// goes as pending; with moderation off, off. Only allow carries text.
func (s *Store) mcpScreening(ctx context.Context, subject string, created, now int64) (string, mcpFinal, error) {
	var action string
	err := s.db.QueryRowContext(ctx, "SELECT action FROM moderation_decisions WHERE surface='post' AND subject=? ORDER BY created_at DESC,rowid DESC LIMIT 1", subject).Scan(&action)
	switch {
	case err == nil && (action == "hide" || action == "hold" || action == "block"):
		return "", mcpDrop, nil
	case err == nil && action == "allow":
		return "allow", mcpSend, nil
	case err == nil:
		return "flag", mcpSend, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", mcpDrop, err
	case s.moderation.engine == nil:
		return "off", mcpSend, nil
	case now-created < MCPEventScreenWaitSeconds:
		return "", mcpPostpone, nil
	}
	return "pending", mcpSend, nil
}

// describeAgent adds an agent's current handle and, when it has one, its
// latest trust tier: two indexed reads.
func (s *Store) describeAgent(ctx context.Context, ref map[string]any, fp string) {
	var handle, account string
	if s.db.QueryRowContext(ctx, "SELECT handle,account FROM identities WHERE id=?", fp).Scan(&handle, &account) != nil {
		return
	}
	if handle != "" {
		ref["handle"] = handle
	}
	var tier int64
	if s.db.QueryRowContext(ctx, "SELECT t.tier FROM trust_current c JOIN trust_scores t ON t.run_id=c.run_id AND t.account=c.account WHERE c.account=?", account).Scan(&tier) == nil {
		ref["trust_tier"] = tier
	}
}

// excerpt is text's first n characters, and whether it was cut.
func excerpt(text string, n int) (string, bool) {
	if utf8.RuneCountInString(text) <= n {
		return text, false
	}
	runes := []rune(text)
	return string(runes[:n]), true
}
