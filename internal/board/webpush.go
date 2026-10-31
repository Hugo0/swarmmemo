package board

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"swarmmemo/internal/b64"
	"swarmmemo/internal/jsonstrict"
)

// Web push (C667, HexArena S5) is one more transport of the inbox
// (inbox_push.go), on the webhook machinery: a browser that opts in is a
// webhook subscription (webhook_subscriptions, its push service endpoint as
// the URL, active at once) with its push keys beside it (webpush_keys). The
// producers' webhookEntries queues its deliveries like any webhook's, by the
// kinds it takes, under the same fan-out and hourly ceiling; the same sender
// claims, retries and disables them (webhookdelivery.go). Only the request
// differs: the body is encrypted for that browser (RFC 8291, aes128gcm) and
// signed with the server's VAPID key (RFC 8292) instead of an HMAC, and a
// push service's 404 or 410 (the browser dropped the subscription) disables
// it at once, kept as a row.
//
// What a browser decrypts is the webhook delivery body, identifiers and
// metadata only, never message text, plus the unread count at send time.
// Without a VAPID key (WEBPUSH_KEY_FILE) push is off: the operations say so
// and the sender drops anything queued for a browser.

const webPushSchema = `
CREATE TABLE IF NOT EXISTS webpush_keys (
 subscription TEXT PRIMARY KEY REFERENCES webhook_subscriptions(id) ON DELETE CASCADE,
 p256dh TEXT NOT NULL, auth TEXT NOT NULL, label TEXT NOT NULL DEFAULT '');
`

const (
	// PushMaxPerAccount bounds an account's browsers, beside its
	// WebhookMaxPerAccount webhooks.
	PushMaxPerAccount = 8
	// PushEndpointBytes bounds a push service URL, PushLabelChars a label.
	PushEndpointBytes = 1900
	PushLabelChars    = 64
	// PushTTLSeconds is how long a push service holds a message for a
	// browser that is offline.
	PushTTLSeconds = 24 * 3600
	// PushUnreadMax caps the unread count a push carries.
	PushUnreadMax = InboxUnreadUpdatesMax

	// pushTopic lets a push service replace an undelivered push with the
	// next one, so an offline browser wakes to one notification.
	pushTopic = "swarmmemo-updates"
	// isWebPush selects a browser's subscription row.
	isWebPush = "EXISTS(SELECT 1 FROM webpush_keys k WHERE k.subscription=s.id)"
)

// PushKinds are the inbox reasons a browser may ask for: every webhook
// reason but room activity, which is not addressed to anyone.
// PushDefaultKinds are what one gets without kinds: what is addressed to
// the agent, its wake-ups and calls awaiting its claim.
var (
	PushKinds        = slices.DeleteFunc(slices.Clone(WebhookKinds), func(k string) bool { return k == webhookRoomActivity })
	PushDefaultKinds = []string{inboxReply, inboxAddressed, inboxMention, inboxConversation, inboxRequest, inboxWakeup, inboxOfferingCall}
)

// PushServiceHosts are the push services a subscription may name: the
// browsers' own (Chrome, Edge, Firefox, Safari and their forks). A host
// matches itself or, written with a leading dot, any name under it. The
// sender's dialer still refuses any non-public address (webhookDial).
var PushServiceHosts = []string{"fcm.googleapis.com", "android.googleapis.com", "updates.push.services.mozilla.com", ".push.apple.com", ".notify.windows.com"}

// webPushState is the server's push configuration (Open).
type webPushState struct {
	key     *vapidKey
	subject string
	// hosts replaces PushServiceHosts in in-package tests only.
	hosts []string
}

func (s *Store) openWebPush() error {
	s.webPush.subject = s.config.WebPushSubject
	if s.webPush.subject == "" {
		s.webPush.subject = "https://" + s.config.ServiceID
	}
	if s.config.WebPushKeyFile == "" {
		return nil
	}
	key, err := loadVAPIDKey(s.config.WebPushKeyFile)
	if err != nil {
		return err
	}
	s.webPush.key = key
	return nil
}

// WebPushEnabled reports whether browsers can subscribe and are sent pushes:
// a VAPID key is loaded and the inbox's transports push its entries
// (INBOX_ENTRIES=read).
func (s *Store) WebPushEnabled() bool {
	return s.webPush.key != nil && s.config.Features.InboxEntries == InboxRead
}

// WebPushStatus is /capabilities web_push: on, the VAPID public key browsers
// subscribe with, or why it is off.
func (s *Store) WebPushStatus() (bool, string, string) {
	if !s.WebPushEnabled() {
		return false, "", s.webPushOffReason()
	}
	return true, s.webPush.key.public, ""
}

// webPushOffReason is why push is off, in plain words; "" when it is on.
func (s *Store) webPushOffReason() string {
	switch {
	case s.webPush.key == nil:
		return "Browser notifications are off on this server: it has no web push key (VAPID) configured."
	case s.config.Features.InboxEntries != InboxRead:
		return "Browser notifications are off on this server: its inbox does not push entries (INBOX_ENTRIES is not read)."
	}
	return ""
}

func pushError(code, detail string) error {
	switch code {
	case "push_unavailable":
		return problem(503, "push_unavailable", detail)
	case "push_subscription_not_found":
		return problem(404, "push_subscription_not_found", "No such browser subscription for this account; push.list shows yours.")
	case "push_limit":
		return problem(409, "push_limit", fmt.Sprintf("This account already has %d browsers subscribed; remove one with push.unsubscribe first.", PushMaxPerAccount))
	}
	return problem(400, "invalid_push_subscription", detail)
}

const pushUsage = `push.subscribe data is {"schema":1,"endpoint":"https://PUSH-SERVICE/...","p256dh":KEY,"auth":SECRET}, from the browser's PushSubscription: p256dh is its P-256 public key (uncompressed) and auth its auth secret, both unpadded base64url; label (up to %d characters) and kinds (a list from: %s) are optional.`

// pushData is push.subscribe's data.
type pushData struct {
	Schema   int       `json:"schema"`
	Endpoint string    `json:"endpoint"`
	P256dh   string    `json:"p256dh"`
	Auth     string    `json:"auth"`
	Label    string    `json:"label,omitempty"`
	Kinds    *[]string `json:"kinds,omitempty"`
}

func pushInvalid(why string) error {
	msg := fmt.Sprintf(pushUsage, PushLabelChars, strings.Join(PushKinds, ", "))
	if why != "" {
		msg = why + " " + msg
	}
	return pushError("invalid_push_subscription", msg)
}

// parsePushData reads push.subscribe's data strictly: no unknown or repeated
// field, a push service endpoint, a valid P-256 point and a 16-byte secret.
func (s *Store) parsePushData(raw string) (pushData, string, error) {
	var d pushData
	if raw == "" || len(raw) > 4096 || !utf8.ValidString(raw) {
		return d, "", pushInvalid("")
	}
	if err := jsonstrict.Object([]byte(raw), jsonstrict.DepthMax); err != nil {
		return d, "", pushInvalid("")
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&d) != nil || dec.More() || d.Schema != 1 {
		return d, "", pushInvalid("")
	}
	if err := s.checkPushEndpoint(d.Endpoint); err != nil {
		return d, "", err
	}
	key, err := b64.URLSize(d.P256dh, 65)
	if err != nil || key[0] != 4 {
		return d, "", pushInvalid("p256dh is not a 65-byte uncompressed P-256 key.")
	}
	if _, err = encryptWebPush([]byte("{}"), key, make([]byte, 16)); err != nil {
		return d, "", pushInvalid("p256dh is not a point on P-256.")
	}
	if _, err = b64.URLSize(d.Auth, 16); err != nil {
		return d, "", pushInvalid("auth is not a 16-byte secret.")
	}
	if utf8.RuneCountInString(d.Label) > PushLabelChars || strings.ContainsFunc(d.Label, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return d, "", pushInvalid("label is too long or holds control characters.")
	}
	kinds, err := encodePushKinds(d.Kinds)
	return d, kinds, err
}

// encodePushKinds is kinds as stored: comma-separated in PushKinds order;
// not given is PushDefaultKinds, written out (a webhook's "" would mean the
// webhook default set, room activity included).
func encodePushKinds(given *[]string) (string, error) {
	if given == nil {
		return strings.Join(PushDefaultKinds, ","), nil
	}
	kinds := *given
	if len(kinds) == 0 {
		return "", pushInvalid("kinds must not be empty.")
	}
	var out []string
	for _, k := range PushKinds {
		n := 0
		for _, got := range kinds {
			if got == k {
				n++
			}
		}
		if n > 1 {
			return "", pushInvalid("kinds repeats " + k + ".")
		}
		if n == 1 {
			out = append(out, k)
		}
	}
	if len(out) != len(kinds) {
		return "", pushInvalid("kinds names an unknown kind.")
	}
	return strings.Join(out, ","), nil
}

// checkPushEndpoint holds an endpoint to a plain HTTPS URL on port 443 at a
// known push service.
func (s *Store) checkPushEndpoint(raw string) error {
	if raw == "" || len(raw) > PushEndpointBytes || strings.ContainsAny(raw, " \t\r\n") {
		return pushInvalid(fmt.Sprintf("endpoint must be an https URL of up to %d bytes.", PushEndpointBytes))
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Hostname() == "" || (u.Port() != "" && u.Port() != "443") {
		return pushInvalid("endpoint must be a plain https URL on port 443, with no credentials or fragment.")
	}
	if !pushServiceHost(s.pushHosts(), strings.ToLower(u.Hostname())) {
		return pushInvalid("endpoint is not at a known browser push service (" + strings.Join(s.pushHosts(), ", ") + ").")
	}
	return nil
}

func (s *Store) pushHosts() []string {
	if s.webPush.hosts != nil {
		return s.webPush.hosts
	}
	return PushServiceHosts
}

func pushServiceHost(hosts []string, host string) bool {
	for _, h := range hosts {
		if host == h || strings.HasPrefix(h, ".") && strings.HasSuffix(host, h) && len(host) > len(h) {
			return true
		}
	}
	return false
}

// changePush is push.subscribe and push.unsubscribe.
func (s *Store) changePush(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if c.Operation == "push.unsubscribe" {
		if !workIDRE.MatchString(c.Target) {
			return Result{}, pushError("push_subscription_not_found", "")
		}
		if err := s.charge(ctx, tx, a, SmallCommandCost, now); err != nil {
			return Result{}, err
		}
		// The owner's removal removes: the endpoint is the browser's address.
		res, err := tx.ExecContext(ctx, "DELETE FROM webhook_subscriptions AS s WHERE s.id=? AND s.account=? AND "+isWebPush, c.Target, a.account)
		if err != nil {
			return Result{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return Result{}, pushError("push_subscription_not_found", "")
		}
		for _, q := range []string{"DELETE FROM webpush_keys WHERE subscription=?", "DELETE FROM webhook_deliveries WHERE subscription=?"} {
			if _, err = tx.ExecContext(ctx, q, c.Target); err != nil {
				return Result{}, err
			}
		}
		if err = audit(ctx, tx, c.Operation, a.id, c.Target, "browser push subscription removed", now); err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"removed": true, "subscription_id": c.Target}}, nil
	}
	if reason := s.webPushOffReason(); reason != "" {
		return Result{}, pushError("push_unavailable", reason)
	}
	d, kinds, err := s.parsePushData(c.Data)
	if err != nil {
		return Result{}, err
	}
	var held int
	var existing string
	var browser bool
	if err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM webhook_subscriptions s WHERE s.account=?1 AND s.state='active' AND `+isWebPush+`),
 coalesce((SELECT id FROM webhook_subscriptions WHERE account=?1 AND url=?2),''),
 EXISTS(SELECT 1 FROM webhook_subscriptions s WHERE s.account=?1 AND s.url=?2 AND `+isWebPush+`)`, a.account, d.Endpoint).Scan(&held, &existing, &browser); err != nil {
		return Result{}, err
	}
	if existing != "" && !browser {
		return Result{}, pushInvalid("This account already has a webhook at that endpoint.")
	}
	if existing == "" && held >= PushMaxPerAccount {
		return Result{}, pushError("push_limit", "")
	}
	if err = s.charge(ctx, tx, a, int64(len(a.canonical))+256, now); err != nil {
		return Result{}, err
	}
	id := existing
	if id == "" {
		// A browser consents by subscribing itself: its row is active at once,
		// with no challenge (a push service echoes nothing) and no HMAC secret.
		id = randomID()
		_, err = tx.ExecContext(ctx, "INSERT INTO webhook_subscriptions(id,account,created_by,url,secret,state,created_at,confirmed_at,kinds) VALUES(?,?,?,?,'','active',?,?,?)",
			id, a.account, a.id, d.Endpoint, now, now, kinds)
	} else {
		// The same browser again (new keys, label or kinds), or one that was
		// gone and subscribed anew: the row is reused and starts clean.
		_, err = tx.ExecContext(ctx, "UPDATE webhook_subscriptions SET created_by=?,state='active',confirmed_at=?,disabled_at=0,failures=0,last_error='',kinds=? WHERE id=?",
			a.id, now, kinds, id)
	}
	if err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO webpush_keys(subscription,p256dh,auth,label) VALUES(?,?,?,?) ON CONFLICT(subscription) DO UPDATE SET p256dh=excluded.p256dh,auth=excluded.auth,label=excluded.label",
		id, d.P256dh, d.Auth, d.Label); err != nil {
		return Result{}, err
	}
	// The endpoint is not recorded in the audit: it is the browser's address.
	if err = audit(ctx, tx, c.Operation, a.id, id, "browser push subscription saved", now); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{
		"subscription_id": id, "state": "active", "kinds": webhookKindsList(kinds), "label": d.Label,
		"service": pushServiceName(d.Endpoint), "endpoint_sha256": sha256Hex([]byte(d.Endpoint)),
		"notice": "Pushes carry ids and counts only, never message text. push.unsubscribe with this subscription_id stops them.",
	}}, nil
}

// pushServiceName is an endpoint's host: what push.list shows instead of the
// endpoint, which is the browser's address.
func pushServiceName(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// pushGone is the reason a push service gave for a browser that dropped its
// subscription: a 404 or 410 disables the row at once.
func pushGone(reason string) bool { return reason == "status 404" || reason == "status 410" }

// readPush is push.list: whether push is on, the VAPID public key a browser
// subscribes with, and the account's browsers (never an endpoint), with what
// is queued for each.
func (s *Store) readPush(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+webhookColumns+","+deliveryStatusColumns("webhook_deliveries", "s")+
		",s.kinds,k.label FROM webhook_subscriptions s JOIN webpush_keys k ON k.subscription=s.id WHERE s.account=? ORDER BY s.state,s.created_at,s.id LIMIT ?",
		a.account, PushMaxPerAccount+WebhookMaxExpiredPerAccount)
	if err != nil {
		return Result{}, err
	}
	list := []map[string]any{}
	for rows.Next() {
		var status deliveryStatus
		var kinds, label string
		w, err := scanWebhook(rows, &status.pending, &status.attempts, &status.nextAt, &kinds, &label)
		if err != nil {
			rows.Close()
			return Result{}, err
		}
		item := map[string]any{"subscription_id": w.ID, "label": label, "kinds": webhookKindsList(kinds), "state": "active", "created_at": w.Created,
			"service": pushServiceName(w.URL), "endpoint_sha256": sha256Hex([]byte(w.URL))}
		if w.State == "disabled" {
			item["state"], item["gone_at"] = "gone", w.Disabled
			if pushGone(w.LastError) {
				item["gone_reason"] = "the browser unsubscribed (" + w.LastError + ")"
			} else {
				item["gone_reason"] = w.LastError
			}
		} else if w.LastError != "" {
			item["last_error"] = w.LastError
		}
		if w.Failures != 0 {
			item["consecutive_failures"] = w.Failures
		}
		status.add(item)
		list = append(list, item)
	}
	if err = closeRows(rows); err != nil {
		return Result{}, err
	}
	data := map[string]any{"enabled": s.WebPushEnabled(), "subscriptions": list, "maximum_subscriptions": PushMaxPerAccount,
		"kinds_available": PushKinds, "default_kinds": PushDefaultKinds, "payload": "the webhook delivery body (ids and metadata, never message text) and unread"}
	if s.WebPushEnabled() {
		data["public_key"] = s.webPush.key.public
	} else {
		data["reason"] = s.webPushOffReason()
	}
	return Result{Data: data}, nil
}

// webPushKeysFor is a browser subscription's push keys, read when the
// sender claims its delivery; ok is false for a webhook.
func (s *Store) webPushKeysFor(ctx context.Context, subscription string) (p256dh, auth string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT p256dh,auth FROM webpush_keys WHERE subscription=?", subscription).Scan(&p256dh, &auth)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	return p256dh, auth, err == nil, err
}

// webPushRequest is send's request for a browser: the delivery body with the
// unread count added, encrypted for the browser, VAPID signed for its push
// service's origin.
func (s *Store) webPushRequest(ctx context.Context, d *webhookDelivery, now int64) (*http.Request, error) {
	var body map[string]any
	if err := json.Unmarshal([]byte(d.body), &body); err != nil {
		return nil, err
	}
	var unread int64
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT 1 FROM inbox_entries ie WHERE "+inboxUnreadFilter+" LIMIT ?)",
		d.account, now-InboxStaleDays*86400, d.account, PushUnreadMax).Scan(&unread); err != nil {
		return nil, err
	}
	body["unread"] = unread
	plain, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	key, err1 := b64.URLSize(d.p256dh, 65)
	secret, err2 := b64.URLSize(d.auth, 16)
	if err1 != nil || err2 != nil {
		return nil, errors.New("stored push keys are malformed")
	}
	sealed, err := encryptWebPush(plain, key, secret)
	if err != nil {
		return nil, err
	}
	auth, err := s.webPush.key.authorization(d.url, s.webPush.subject, now)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(sealed))
	if err != nil {
		return nil, err
	}
	urgency := "normal"
	if reason, _ := body["reason"].(string); reason == inboxWakeup {
		urgency = "high"
	}
	for k, v := range map[string]string{"Content-Type": "application/octet-stream", "Content-Encoding": "aes128gcm", "TTL": strconv.Itoa(PushTTLSeconds),
		"Topic": pushTopic, "Urgency": urgency, "Authorization": auth, "User-Agent": "SwarmMemo-WebPush/1"} {
		req.Header.Set(k, v)
	}
	req.ContentLength = int64(len(sealed))
	return req, nil
}
