package board

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Webhooks push the same three things /api/updates already returns to a key —
// replies to its messages, messages addressed to it, activity in rooms it posted
// in — to an HTTPS endpoint the key owns. Push is a transport for an existing
// read: nothing new becomes visible, and a delivery carries identifiers only, so
// the receiver still has to fetch the message under its own authority.
//
// The whole subsystem is outbound HTTP to an attacker-chosen URL, so the bounds
// live here rather than in the caller: address filtering at creation and again on
// the dial, a per-account subscription cap, an hourly delivery ceiling, a
// persistent queue instead of goroutine fan-out, and a challenge the endpoint must
// echo before it receives anything real. See docs/rfcs/0006-webhooks.md.

const webhookSchema = `
CREATE TABLE IF NOT EXISTS webhook_subscriptions (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, created_by TEXT NOT NULL,
 url TEXT NOT NULL, secret TEXT NOT NULL, state TEXT NOT NULL CHECK(state IN ('pending','active','disabled')),
 challenge TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
 confirmed_at INTEGER NOT NULL DEFAULT 0, disabled_at INTEGER NOT NULL DEFAULT 0,
 failures INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
 UNIQUE(account,url));
CREATE INDEX IF NOT EXISTS webhook_account ON webhook_subscriptions(account,id);
CREATE INDEX IF NOT EXISTS webhook_active ON webhook_subscriptions(state,account);
CREATE TABLE IF NOT EXISTS webhook_deliveries (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL,
 subscription TEXT NOT NULL REFERENCES webhook_subscriptions(id) ON DELETE CASCADE,
 event_id TEXT NOT NULL, kind TEXT NOT NULL, body TEXT NOT NULL,
 attempts INTEGER NOT NULL DEFAULT 0, next_at INTEGER NOT NULL,
 leased_until INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL,
 UNIQUE(subscription,event_id));
CREATE INDEX IF NOT EXISTS webhook_queue ON webhook_deliveries(next_at,seq);
CREATE TABLE IF NOT EXISTS webhook_rates (
 account TEXT NOT NULL, hour INTEGER NOT NULL, count INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(account,hour));
`

const (
	// Caps are deliberately small: this is a request multiplier pointed at third
	// parties, and nobody has asked for push yet.
	WebhookMaxPerAccount     = 4
	WebhookMaxDeliveriesHour = 240
	WebhookMaxFanout         = 32
	WebhookMaxAttempts       = 6
	WebhookDisableFailures   = 5
	WebhookPendingTTL        = 3600
	WebhookMaxURLBytes       = 512

	webhookBaseBackoff = 30
	webhookMaxBackoff  = 3600
	webhookLeaseSecond = 120
)

func webhookError(code string) error {
	switch code {
	case "webhook_not_found":
		return problem(404, "webhook_not_found", "No such subscription for this account.")
	case "webhook_limit":
		return problem(409, "webhook_limit", "This account already holds the maximum number of webhook subscriptions; delete one first.")
	case "webhook_exists":
		return problem(409, "webhook_exists", "This account already subscribes that URL.")
	case "webhook_delegated":
		return problem(403, "webhook_delegated", "A scoped child grant cannot manage its parent's webhook subscriptions.")
	case "webhook_address_blocked":
		return problem(400, "webhook_address_blocked", "The callback host resolves to a private, loopback, link-local, multicast, carrier-NAT or otherwise non-public address.")
	case "webhook_unresolved":
		return problem(400, "webhook_unresolved", "The callback host does not resolve to any usable public address.")
	}
	return problem(400, "invalid_webhook", "Data must be a strict schema-1 JSON object with an https URL of up to 512 bytes, no credentials, no fragment and no port other than 443.")
}

// webhookBlocked holds the ranges IsGlobalUnicast and friends do not already
// cover, or cover inconsistently between families. The predicate checks stay
// alongside it; both run on every address.
var webhookBlocked = func() []*net.IPNet {
	nets := []*net.IPNet{}
	for _, cidr := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8",
		"2001::/32", "2002::/16", "64:ff9b::/96", "100::/64", "2001:db8::/32",
	} {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(err)
		}
		nets = append(nets, n)
	}
	return nets
}()

// publicWebhookIP is the single address decision. An IPv4-mapped IPv6 literal is
// unwrapped first, so ::ffff:127.0.0.1 cannot slip past the v4 ranges.
func publicWebhookIP(ip net.IP) error {
	if ip == nil {
		return webhookError("webhook_unresolved")
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return webhookError("webhook_address_blocked")
	}
	for _, n := range webhookBlocked {
		if n.Contains(ip) {
			return webhookError("webhook_address_blocked")
		}
	}
	return nil
}

// parseWebhookURL rejects everything about a URL that is not a plain public
// HTTPS endpoint. The port is pinned to 443 so a subscription cannot be used to
// walk another host's ports; address filtering already covers our own network.
func parseWebhookURL(raw string) (*url.URL, error) {
	if raw == "" || len(raw) > WebhookMaxURLBytes || !utf8.ValidString(raw) || strings.ContainsAny(raw, " \t\r\n") {
		return nil, webhookError("invalid_webhook")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Host == "" || u.Hostname() == "" {
		return nil, webhookError("invalid_webhook")
	}
	if port := u.Port(); port != "" && port != "443" {
		return nil, webhookError("invalid_webhook")
	}
	return u, nil
}

// checkWebhookHost is the creation-time half of the SSRF control. It is an early
// honest error, not the security boundary: the dialer re-checks the address that
// is actually connected to, which is what a rebinding attack has to defeat.
func (s *Store) checkWebhookHost(ctx context.Context, u *url.URL) error {
	// A literal address needs no resolver, so it is always checked; the test hook
	// only skips the DNS half, which in-package tests cannot depend on.
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		return publicWebhookIP(ip)
	}
	if s.webhookInsecure {
		return nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", u.Hostname())
	if err != nil || len(ips) == 0 {
		return webhookError("webhook_unresolved")
	}
	for _, ip := range ips {
		if err = publicWebhookIP(ip); err != nil {
			return err
		}
	}
	return nil
}

type webhookSubscription struct {
	ID, Account, CreatedBy, URL, Secret, State, Challenge, LastError string
	Created, Confirmed, Disabled, Failures                           int64
}

const webhookColumns = `id,account,created_by,url,secret,state,challenge,created_at,confirmed_at,disabled_at,failures,last_error`

func scanWebhook(row scanner) (webhookSubscription, error) {
	var s webhookSubscription
	err := row.Scan(&s.ID, &s.Account, &s.CreatedBy, &s.URL, &s.Secret, &s.State, &s.Challenge, &s.Created, &s.Confirmed, &s.Disabled, &s.Failures, &s.LastError)
	return s, err
}

// parseWebhookData accepts only {"schema":1,"url":"https://..."}; an unknown or
// repeated field is an error rather than something silently ignored, matching
// how peer cards and delegation contexts are parsed.
func parseWebhookData(raw string) (string, error) {
	if len(raw) > 1024 || !utf8.ValidString(raw) {
		return "", webhookError("invalid_webhook")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", webhookError("invalid_webhook")
	}
	seen := map[string]bool{}
	schema, target := 0, ""
	for decoder.More() {
		token, err = decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return "", webhookError("invalid_webhook")
		}
		seen[name] = true
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil || string(value) == "null" {
			return "", webhookError("invalid_webhook")
		}
		switch name {
		case "schema":
			err = json.Unmarshal(value, &schema)
		case "url":
			err = json.Unmarshal(value, &target)
		default:
			return "", webhookError("invalid_webhook")
		}
		if err != nil {
			return "", webhookError("invalid_webhook")
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return "", webhookError("invalid_webhook")
	}
	if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
		return "", webhookError("invalid_webhook")
	}
	if len(seen) != 2 || schema != 1 {
		return "", webhookError("invalid_webhook")
	}
	return target, nil
}

func webhookSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Store) changeWebhook(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if a.grant != nil {
		return Result{}, webhookError("webhook_delegated")
	}
	if c.Operation == "webhook.delete" {
		if !workIDRE.MatchString(c.Target) {
			return Result{}, webhookError("webhook_not_found")
		}
		if err := s.charge(ctx, tx, a, 256, now); err != nil {
			return Result{}, err
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM webhook_subscriptions WHERE id=? AND account=?", c.Target, a.account)
		if err != nil {
			return Result{}, err
		}
		removed, err := res.RowsAffected()
		if err != nil {
			return Result{}, err
		}
		if removed == 0 {
			return Result{}, webhookError("webhook_not_found")
		}
		// Queued deliveries for a deleted subscription are pointless work against
		// an endpoint that just unsubscribed; drop them in the same transaction.
		if _, err = tx.ExecContext(ctx, "DELETE FROM webhook_deliveries WHERE subscription=?", c.Target); err != nil {
			return Result{}, err
		}
		if err = audit(ctx, tx, c.Operation, a.id, c.Target, "webhook subscription deleted", now); err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"deleted": true, "subscription_id": c.Target}}, nil
	}
	target, err := parseWebhookData(c.Data)
	if err != nil {
		return Result{}, err
	}
	u, err := parseWebhookURL(target)
	if err != nil {
		return Result{}, err
	}
	if err = s.checkWebhookHost(ctx, u); err != nil {
		return Result{}, err
	}
	var held int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM webhook_subscriptions WHERE account=?", a.account).Scan(&held); err != nil {
		return Result{}, err
	}
	if held >= WebhookMaxPerAccount {
		return Result{}, webhookError("webhook_limit")
	}
	if err = s.charge(ctx, tx, a, int64(len(a.canonical))+512, now); err != nil {
		return Result{}, err
	}
	id, secret, nonce := randomID(), webhookSecret(), randomID()
	_, err = tx.ExecContext(ctx, "INSERT INTO webhook_subscriptions(id,account,created_by,url,secret,state,challenge,created_at) VALUES(?,?,?,?,?,'pending',?,?)",
		id, a.account, a.id, u.String(), secret, nonce, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Result{}, webhookError("webhook_exists")
		}
		return Result{}, err
	}
	delivery := randomID()
	body, err := json.Marshal(map[string]any{"schema": 1, "delivery_id": delivery, "subscription_id": id, "type": "challenge", "nonce": nonce})
	if err != nil {
		return Result{}, err
	}
	if err = s.queueDelivery(ctx, tx, delivery, id, "challenge:"+id, "challenge", string(body), now); err != nil {
		return Result{}, err
	}
	// The URL is deliberately not recorded in the audit detail: the audit table is
	// read by operator tooling and a callback URL can itself carry a token.
	if err = audit(ctx, tx, c.Operation, a.id, id, "webhook subscription created, pending endpoint echo", now); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{
		"subscription_id": id, "url": u.String(), "state": "pending", "secret": secret,
		"signature":       "hex HMAC-SHA256 over X-SwarmMemo-Timestamp + \".\" + the exact body bytes, sent as X-SwarmMemo-Signature: v1=...",
		"challenge":       "A challenge POST is queued; echo its nonce in a 2xx response body to activate this subscription.",
		"pending_expires": now + WebhookPendingTTL,
		"notice":          "This secret is shown once. Deliveries carry identifiers only, never message text; fetch the message with your own key.",
	}}, nil
}

// queueDelivery is INSERT OR IGNORE against UNIQUE(subscription,event_id): one
// event produces at most one delivery per subscription, whatever the caller does.
func (s *Store) queueDelivery(ctx context.Context, tx *sql.Tx, delivery, subscription, event, kind, body string, now int64) error {
	_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES(?,?,?,?,?,?,?)",
		delivery, subscription, event, kind, body, now, now)
	return err
}

func (s *Store) readWebhooks(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if a.grant != nil {
		return Result{}, webhookError("webhook_delegated")
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+webhookColumns+" FROM webhook_subscriptions WHERE account=? ORDER BY created_at,id LIMIT ?", a.account, limitValue(c.Limit))
	if err != nil {
		return Result{}, err
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return Result{}, err
		}
		item := map[string]any{"subscription_id": w.ID, "url": w.URL, "state": w.State, "created_at": w.Created, "consecutive_failures": w.Failures}
		if w.Confirmed != 0 {
			item["confirmed_at"] = w.Confirmed
		}
		if w.Disabled != 0 {
			item["disabled_at"], item["disabled_reason"] = w.Disabled, w.LastError
		} else if w.LastError != "" {
			item["last_error"] = w.LastError
		}
		if w.State == "pending" {
			item["pending_expires"] = w.Created + WebhookPendingTTL
		}
		list = append(list, item)
	}
	if err = rows.Err(); err != nil {
		return Result{}, err
	}
	var queued int64
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM webhook_deliveries d JOIN webhook_subscriptions s ON s.id=d.subscription WHERE s.account=?", a.account).Scan(&queued); err != nil {
		return Result{}, err
	}
	used, err := webhookHourUsed(ctx, tx, a.account, now)
	if err != nil {
		return Result{}, err
	}
	// Secrets are never listed. A caller that lost one deletes and recreates.
	return Result{Data: map[string]any{
		"subscriptions": list, "queued_deliveries": queued,
		"maximum_subscriptions": WebhookMaxPerAccount, "deliveries_this_hour": used,
		"maximum_deliveries_per_hour": WebhookMaxDeliveriesHour, "secret_listed": false,
	}}, nil
}

func webhookHourUsed(ctx context.Context, tx *sql.Tx, account string, now int64) (int64, error) {
	var used int64
	err := tx.QueryRowContext(ctx, "SELECT count FROM webhook_rates WHERE account=? AND hour=?", account, now/3600).Scan(&used)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return used, err
}

// enqueueWebhooks runs inside the posting transaction: a delivery exists only if
// the event it describes committed. It reuses the /api/updates concern rules, and
// re-checks private-room membership so a removed member's old posts in a room
// cannot keep producing notifications.
func (s *Store) enqueueWebhooks(ctx context.Context, tx *sql.Tx, eventID string, c Command, room Room, a actor, now int64) error {
	recipient := ""
	if c.To != "" {
		if err := tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", c.To).Scan(&recipient); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	const match = `(? <> '' AND EXISTS(SELECT 1 FROM events p WHERE p.id=? AND p.account=s.account))`
	query := `SELECT s.id,s.account,CASE
 WHEN ` + match + ` THEN 'reply'
 WHEN ? <> '' AND s.account=? THEN 'addressed'
 ELSE 'room_activity' END
 FROM webhook_subscriptions s WHERE s.state='active' AND s.account<>?
 AND (` + match + ` OR (? <> '' AND s.account=?) OR EXISTS(SELECT 1 FROM events p WHERE p.room=? AND p.account=s.account))
 AND (?='public' OR EXISTS(SELECT 1 FROM members m WHERE m.room=? AND m.account=s.account))
 ORDER BY s.id LIMIT ?`
	rows, err := tx.QueryContext(ctx, query,
		c.ReplyTo, c.ReplyTo, recipient, recipient, a.account,
		c.ReplyTo, c.ReplyTo, recipient, recipient, room.Name,
		room.Visibility, room.Name, WebhookMaxFanout)
	if err != nil {
		return err
	}
	type target struct{ id, account, reason string }
	targets := []target{}
	for rows.Next() {
		var t target
		if err = rows.Scan(&t.id, &t.account, &t.reason); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, t := range targets {
		used, err := webhookHourUsed(ctx, tx, t.account, now)
		if err != nil {
			return err
		}
		// Over the hourly ceiling the notification is dropped, not deferred: a
		// delayed flood is still a flood, and /api/updates still has the event.
		if used >= WebhookMaxDeliveriesHour {
			continue
		}
		delivery := randomID()
		body, err := json.Marshal(map[string]any{
			"schema": 1, "delivery_id": delivery, "subscription_id": t.id, "type": "event", "reason": t.reason,
			"event": map[string]any{
				"id": eventID, "room": room.Name, "page": c.Page, "visibility": room.Visibility,
				"created_at": now, "kind": c.Kind, "reply_to": c.ReplyTo, "to": c.To,
			},
			"read": "/api/thread/" + eventID,
		})
		if err != nil {
			return err
		}
		if err = s.queueDelivery(ctx, tx, delivery, t.id, eventID, "event", string(body), now); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO webhook_rates(account,hour,count) VALUES(?,?,1) ON CONFLICT(account,hour) DO UPDATE SET count=count+1", t.account, now/3600); err != nil {
			return err
		}
	}
	return nil
}
