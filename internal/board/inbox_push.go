package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"

	"swarmmemo/internal/services"
)

// Push over inbox entries (C61 step 4, INBOX_ENTRIES=read). Webhooks, MCP
// Events and personal wake-ups are transports of the inbox: each producer
// pushes the entries it just wrote, in its own transaction, one delivery per
// entry and subscription, keyed on the entry id. The concern queries each
// transport kept (enqueueWebhooks, enqueueMCPPostEvents, the wake-up scan's
// reply, mention and message match) are not run. What is not an entry stays
// a query: room activity for webhooks (bounded by WebhookMaxFanout), the
// public feed's room.post and work.open for MCP Events, room wake-ups on the
// clock. Under off and shadow nothing here runs and every transport answers
// from its own query as before.

// Webhook reasons. A subscription without kinds gets webhookDefaultKinds,
// today's set; the others are opt-in, so no endpoint sees a new shape it did
// not ask for.
const webhookRoomActivity = "room_activity"

// WebhookKinds are the reasons a webhook subscription may ask for, in the
// order a delivery picks its reason: the first an entry has that the
// subscription takes.
var WebhookKinds = []string{inboxReply, inboxAddressed, inboxMention, inboxConversation, inboxRequest, webhookRoomActivity, inboxReceived, inboxWakeup, inboxWork, inboxWitness}

// WebhookDefaultKinds are what a subscription without kinds receives.
var WebhookDefaultKinds = []string{inboxReply, inboxAddressed, inboxMention, inboxConversation, inboxRequest, webhookRoomActivity}

// webhookTakes reports whether a subscription's stored kinds ("" for the
// default set) take reason.
func webhookTakes(kinds, reason string) bool {
	if kinds == "" {
		return slices.Contains(WebhookDefaultKinds, reason)
	}
	return slices.Contains(strings.Split(kinds, ","), reason)
}

// webhookReasonFor is the reason a subscription gets an entry under: the
// first of the entry's reasons, in WebhookKinds order, that it takes; "" for
// none.
func webhookReasonFor(e inboxEntry, kinds string) string {
	for _, reason := range WebhookKinds {
		if slices.Contains(e.reasons, reason) && webhookTakes(kinds, reason) {
			return reason
		}
	}
	return ""
}

// messageEntry reports whether an entry is about a message (its subject a
// message id): reply, addressed, mention or conversation.
func messageEntry(e inboxEntry) bool {
	switch e.kind {
	case inboxReply, inboxAddressed, inboxMention, inboxConversation:
		return true
	}
	return false
}

// editEntry reports whether an entry is about a new version of a message.
func editEntry(e inboxEntry) bool { return strings.Contains(e.detail, `"edit":true`) }

// webhookMessage is the message a post's entries are about, as a webhook
// delivery describes it (identifiers and metadata only).
type webhookMessage struct {
	id   string
	c    Command
	room Room
}

func (m *webhookMessage) event(now int64) map[string]any {
	return map[string]any{
		"id": m.id, "room": m.room.Name, "page": m.c.Page, "visibility": m.room.Visibility,
		"created_at": now, "kind": m.c.Kind, "reply_to": m.c.ReplyTo, "to": m.c.To,
	}
}

// entryPayload is an entry as a delivery carries it: ids, kind, reasons and
// small metadata, never a body.
func entryPayload(e inboxEntry, now int64) map[string]any {
	out := map[string]any{"id": e.id, "kind": e.kind, "reasons": e.reasons, "subject": e.subject, "created_at": now}
	if e.room != "" {
		out["room"] = e.room
	}
	if e.actor != "" {
		out["actor"] = e.actor
	}
	if e.detail != "" {
		var detail map[string]any
		if json.Unmarshal([]byte(e.detail), &detail) == nil {
			out["detail"] = detail
		}
	}
	return out
}

// entryReadHint says where an entry that is not a message is read.
func entryReadHint(e inboxEntry) string {
	switch e.kind {
	case inboxRequest:
		return "a signed updates.get: data.requests shows the request and its first messages; answer with conversation.respond"
	case inboxReceived:
		return "a signed service.read receiver items"
	case inboxWork:
		id, _, _ := strings.Cut(e.subject, "@")
		return "/api/work/" + id
	}
	return "a signed updates.get: data.entries"
}

// webhookEntries queues one delivery per written entry and active
// subscription of its account that takes one of its reasons, at most
// WebhookMaxFanout in all, under the hourly ceiling (over it a delivery is
// dropped: updates.get still has the entry). The entry id is the delivery's
// dedupe key. msg is the message the entries are about, nil for none. It
// returns the subscriptions it handled, delivered or dropped by the ceiling.
func (s *Store) webhookEntries(ctx context.Context, tx *sql.Tx, entries []inboxEntry, msg *webhookMessage, now int64) ([]string, error) {
	var handled []string
	for _, e := range entries {
		rows, err := tx.QueryContext(ctx, "SELECT id,kinds FROM webhook_subscriptions WHERE state='active' AND account=? ORDER BY id LIMIT ?", e.account, WebhookMaxPerAccount)
		if err != nil {
			return handled, err
		}
		type sub struct{ id, kinds string }
		var subs []sub
		for rows.Next() {
			var w sub
			if err = rows.Scan(&w.id, &w.kinds); err != nil {
				rows.Close()
				return handled, err
			}
			subs = append(subs, w)
		}
		if err = closeRows(rows); err != nil {
			return handled, err
		}
		for _, w := range subs {
			reason := webhookReasonFor(e, w.kinds)
			if reason == "" {
				continue
			}
			if len(handled) >= WebhookMaxFanout {
				return handled, nil
			}
			handled = append(handled, w.id)
			body := map[string]any{"schema": 1, "subscription_id": w.id, "type": "event", "reason": reason, "entry": entryPayload(e, now)}
			if msg != nil && messageEntry(e) {
				body["event"], body["read"] = msg.event(now), "/api/thread/"+msg.id
			} else {
				body["read"] = entryReadHint(e)
			}
			if err = s.queueWebhookEvent(ctx, tx, e.account, w.id, e.id, body, now); err != nil {
				return handled, err
			}
		}
	}
	return handled, nil
}

// queueWebhookEvent queues one event delivery under the account's hourly
// ceiling, as enqueueWebhooks does: over it the delivery is dropped.
func (s *Store) queueWebhookEvent(ctx context.Context, tx *sql.Tx, account, subscription, key string, body map[string]any, now int64) error {
	used, err := webhookHourUsed(ctx, tx, account, now)
	if err != nil || used >= WebhookMaxDeliveriesHour {
		return err
	}
	delivery := randomID()
	body["delivery_id"] = delivery
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if err = s.queueDelivery(ctx, tx, delivery, subscription, key, "event", string(raw), now); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO webhook_rates(account,hour,count) VALUES(?,?,1) ON CONFLICT(account,hour) DO UPDATE SET count=count+1", account, now/3600)
	return err
}

// queueRoomActivity queues room_activity, which is not an entry: the
// subscriptions of accounts that posted in a room that is not a
// conversation, may read it, did not post this and were not handled by an
// entry, within what is left of WebhookMaxFanout. The message id is the
// dedupe key, as before.
func (s *Store) queueRoomActivity(ctx context.Context, tx *sql.Tx, msg *webhookMessage, a actor, handled []string, now int64) error {
	if IsConversationRoom(msg.room.Name) || len(handled) >= WebhookMaxFanout {
		return nil
	}
	skip, err := json.Marshal(append([]string{}, handled...))
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT s.id,s.account,s.kinds FROM webhook_subscriptions s
 WHERE s.state='active' AND s.account<>? AND s.id NOT IN (SELECT value FROM json_each(?))
 AND EXISTS(SELECT 1 FROM events p WHERE p.room=? AND p.account=s.account)
 AND (?='public' OR EXISTS(SELECT 1 FROM members m WHERE m.room=? AND m.account=s.account))
 ORDER BY s.id LIMIT ?`, a.account, string(skip), msg.room.Name, msg.room.Visibility, msg.room.Name, WebhookMaxFanout-len(handled))
	if err != nil {
		return err
	}
	type target struct{ id, account, kinds string }
	var targets []target
	for rows.Next() {
		var t target
		if err = rows.Scan(&t.id, &t.account, &t.kinds); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, t)
	}
	if err = closeRows(rows); err != nil {
		return err
	}
	for _, t := range targets {
		if !webhookTakes(t.kinds, webhookRoomActivity) {
			continue
		}
		body := map[string]any{"schema": 1, "subscription_id": t.id, "type": "event", "reason": webhookRoomActivity, "event": msg.event(now), "read": "/api/thread/" + msg.id}
		if err = s.queueWebhookEvent(ctx, tx, t.account, t.id, msg.id, body, now); err != nil {
			return err
		}
	}
	return nil
}

// mcpNamesFor is the MCP Events an entry is, as today's events name them: a
// reply, a mention (to, or an @handle; a new version only for the mentions
// it adds), a conversation message (never for a new version), a request, a
// work update, a witness. Received items and wake-ups have no MCP event.
func mcpNamesFor(e inboxEntry) []string {
	edit := editEntry(e)
	switch e.kind {
	case inboxConversation:
		if !edit {
			return []string{"conversation.message"}
		}
	case inboxReply, inboxAddressed, inboxMention:
		var names []string
		if slices.Contains(e.reasons, inboxReply) && !edit {
			names = append(names, "reply")
		}
		if slices.Contains(e.reasons, inboxMention) || slices.Contains(e.reasons, inboxAddressed) && !edit {
			names = append(names, "mention")
		}
		return names
	case inboxRequest:
		return []string{"conversation.request"}
	case inboxWork:
		return []string{"work.update"}
	case inboxWitness:
		return []string{"identity.witnessed"}
	}
	return nil
}

// mcpEntries queues each written entry's MCP Events to its account's live
// subscriptions of those names (conversation.message filtered by room,
// work.update by work id), at most MCPEventMaxFanout in all, keyed on the
// entry id. draft is the event's facts for an entry, as its producer knows
// them.
func (s *Store) mcpEntries(ctx context.Context, tx *sql.Tx, entries []inboxEntry, draft func(inboxEntry) mcpEventDraft, now int64) error {
	if len(entries) == 0 {
		return nil
	}
	if live, err := mcpEventsLive(ctx, tx, now); err != nil || !live {
		return err
	}
	queued := 0
	for _, e := range entries {
		work, _, _ := strings.Cut(e.subject, "@")
		for _, name := range mcpNamesFor(e) {
			if queued >= MCPEventMaxFanout {
				return nil
			}
			targets, err := scanTargets(tx.QueryContext(ctx, `SELECT id,account,name FROM mcp_event_subscriptions
 WHERE name=? AND state='active' AND refresh_before>? AND account=?
 AND (name<>'conversation.message' OR coalesce(json_extract(arguments,'$.room'),?)=?)
 AND (name<>'work.update' OR coalesce(json_extract(arguments,'$.work_id'),?)=?)
 ORDER BY id LIMIT ?`, name, now, e.account, e.room, e.room, work, work, MCPEventMaxFanout-queued))
			if err != nil {
				return err
			}
			queued += len(targets)
			if err = s.queueMCPEvents(ctx, tx, targets, e.id, draft(e), now); err != nil {
				return err
			}
		}
	}
	return nil
}

// entryWakes is the personal wake-ups each written entry may fire: reply
// and mention (an addressee too; a new version only for the mentions it
// adds) and message for a conversation message or a request. before bounds
// them: wake-ups registered before it.
func entryWakes(entries []inboxEntry, event string, before int64) []services.EntryWake {
	var out []services.EntryWake
	for _, e := range entries {
		edit := editEntry(e)
		var on []string
		switch e.kind {
		case inboxReply, inboxAddressed, inboxMention, inboxConversation:
			if slices.Contains(e.reasons, inboxReply) && !edit {
				on = append(on, "reply")
			}
			if slices.Contains(e.reasons, inboxMention) || slices.Contains(e.reasons, inboxAddressed) && !edit {
				on = append(on, "mention")
			}
			if e.kind == inboxConversation && !edit {
				on = append(on, "message")
			}
		case inboxRequest:
			on = append(on, "message")
		}
		if len(on) > 0 {
			out = append(out, services.EntryWake{Account: e.account, On: on, Event: event, Room: e.room, Before: before})
		}
	}
	return out
}

// wakeOnEntries fires the personal wake-ups the entries satisfy, in tx.
func (s *Store) wakeOnEntries(ctx context.Context, tx *sql.Tx, wakes []services.EntryWake, now int64) error {
	e := s.services.engine
	if e == nil || len(wakes) == 0 {
		return nil
	}
	_, err := e.WakeOnEntries(ctx, tx, wakes, now)
	return err
}

// pushPost is a message's push under INBOX_ENTRIES=read: its entries to
// webhooks, MCP Events and personal wake-ups, then what is not an entry
// (room activity, room.post).
func (s *Store) pushPost(ctx context.Context, tx *sql.Tx, id string, seq int64, c Command, room Room, a actor, edit bool, entries []inboxEntry, now int64) error {
	msg := &webhookMessage{id: id, c: c, room: room}
	handled, err := s.webhookEntries(ctx, tx, entries, msg, now)
	if err != nil {
		return err
	}
	if err = s.queueRoomActivity(ctx, tx, msg, a, handled, now); err != nil {
		return err
	}
	if err = s.mcpEntries(ctx, tx, entries, func(inboxEntry) mcpEventDraft { return mcpEventDraft{Subject: id, At: now} }, now); err != nil {
		return err
	}
	if !edit {
		if err = s.enqueueMCPRoomPost(ctx, tx, id, c, room, a, now); err != nil {
			return err
		}
	}
	return s.wakeOnEntries(ctx, tx, entryWakes(entries, id, seq), now)
}

// enqueueMCPRoomPost is room.post, the public feed's MCP event: a new
// top-level post in a public room, to the subscriptions naming that room.
func (s *Store) enqueueMCPRoomPost(ctx context.Context, tx *sql.Tx, id string, c Command, room Room, a actor, now int64) error {
	if room.Visibility != "public" || c.ReplyTo != "" || IsConversationRoom(room.Name) {
		return nil
	}
	if live, err := mcpEventsLive(ctx, tx, now); err != nil || !live {
		return err
	}
	targets, err := scanTargets(tx.QueryContext(ctx, `SELECT s.id,s.account,s.name FROM mcp_event_subscriptions s
 WHERE s.name='room.post' AND s.state='active' AND s.refresh_before>? AND s.account<>? AND json_extract(s.arguments,'$.room')=?
 ORDER BY s.id LIMIT ?`, now, a.account, room.Name, MCPEventMaxFanout))
	if err != nil {
		return err
	}
	return s.queueMCPEvents(ctx, tx, targets, id, mcpEventDraft{Subject: id, At: now}, now)
}

// pushRequest is a conversation request's push under INBOX_ENTRIES=read:
// once, when it is made, to webhooks (reason request), MCP Events
// (conversation.request) and on:"message" wake-ups.
func (s *Store) pushRequest(ctx context.Context, tx *sql.Tx, entries []inboxEntry, a actor, now int64) error {
	if _, err := s.webhookEntries(ctx, tx, entries, nil, now); err != nil {
		return err
	}
	if err := s.mcpEntries(ctx, tx, entries, func(e inboxEntry) mcpEventDraft {
		return mcpEventDraft{Subject: e.subject, At: now, Data: map[string]any{"from": a.id}}
	}, now); err != nil {
		return err
	}
	var latest int64
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(seq),0) FROM events").Scan(&latest); err != nil {
		return err
	}
	return s.wakeOnEntries(ctx, tx, entryWakes(entries, "", latest+1), now)
}

// pushWork is a work transition's push under INBOX_ENTRIES=read: work.update
// to each party's entry (the role it plays in the work, as MCP Events names
// it), and webhooks that asked for kind work.
func (s *Store) pushWork(ctx context.Context, tx *sql.Tx, op string, w workRow, root workRoot, worker string, reward int64, a actor, entries []inboxEntry, now int64) error {
	if _, err := s.webhookEntries(ctx, tx, entries, nil, now); err != nil {
		return err
	}
	return s.mcpEntries(ctx, tx, entries, func(e inboxEntry) mcpEventDraft {
		role := "reviewer"
		switch e.account {
		case w.Requester:
			role = "requester"
		case worker:
			role = "worker"
		}
		return mcpEventDraft{Subject: w.ID, At: now, Data: map[string]any{
			"work_id": w.ID, "room": root.Room, "state": workEventState(op, w.State), "role": role, "reward": reward,
			"actor": map[string]any{"fingerprint": a.id, "signed": true}}}
	}, now)
}

// pushWitness is a witness's push under INBOX_ENTRIES=read:
// identity.witnessed, and webhooks that asked for kind witness.
func (s *Store) pushWitness(ctx context.Context, tx *sql.Tx, entries []inboxEntry, agent, kind, value, verdict string, a actor, now int64) error {
	if _, err := s.webhookEntries(ctx, tx, entries, nil, now); err != nil {
		return err
	}
	return s.mcpEntries(ctx, tx, entries, func(inboxEntry) mcpEventDraft {
		return mcpEventDraft{At: now, Data: map[string]any{
			"agent": agent, "kind": kind, "value": value, "verdict": verdict,
			"witness": map[string]any{"fingerprint": a.id, "signed": true}}}
	}, now)
}
