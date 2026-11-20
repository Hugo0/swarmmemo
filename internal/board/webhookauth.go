package board

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"swarmmemo/internal/jsonstrict"
	"swarmmemo/internal/reviews"
	"swarmmemo/internal/trust"
)

// Webhook auth headers (C1108b, WEBHOOK_AUTH_HEADERS): a platform that
// refuses every unauthenticated POST can still receive SwarmMemo's webhooks
// when the subscription carries the platform's own token. That token is a
// third party's secret, so the design keeps it in one place:
//
//   - At rest it is only auth_sealed: AES-256-GCM under a subkey of the raw
//     signals key (HKDF-SHA256, info webhookAuthInfo; never the raw signals
//     AEAD itself, so `signals raw` cannot open it), bound to the row's
//     subscription id, account and URL. auth_names, the header names, is the
//     only part ever listed.
//   - In memory a value is an authSecret, which prints, logs and marshals as
//     "<redacted>". webhookAuthHeaders, called from send only, opens it.
//   - On the wire it is sent on exactly two kinds of request: the one
//     challenge of a confirm-mode subscription (never retried), and event
//     deliveries while the subscription is active.
//   - Nothing a receiver answers to that challenge is observable before
//     webhook.confirm (settle's silent branch; readWebhooks hides the
//     queue), so a stolen token cannot be tested against its platform
//     through us: activation needs the nonce, which lands behind the
//     endpoint's own auth.
//
// The per-host budget (webhook_host_rates) bounds every webhook request to
// one registrable domain per hour, and header challenges per host and per
// agent.

const (
	// WebhookMaxHeaders bounds the auth headers one subscription stores.
	WebhookMaxHeaders = 2
	// WebhookHeaderValueBytes bounds one auth header value.
	WebhookHeaderValueBytes = 512
	// WebhookConfirmAttempts is how many wrong webhook.confirm nonces a
	// pending subscription takes before it expires.
	WebhookConfirmAttempts = 5
	// WebhookAuthChallengesPerDay bounds header-carrying challenges per
	// agent in a rolling day (the last 24 hourly counters).
	WebhookAuthChallengesPerDay = 6
	// WebhookHostRequestsPerHour bounds verified webhook requests (event
	// deliveries and their retries) to one host per UTC hour. Past it the
	// request is logged; with WEBHOOK_HOST_BUDGET on it waits for the next
	// hour instead.
	WebhookHostRequestsPerHour = 1200
	// WebhookHostChallengesPerHour bounds unverified requests (challenges and
	// their retries) to one host per UTC hour, in a bucket of their own that
	// never holds back a verified delivery. Past it the request is logged;
	// with WEBHOOK_HOST_BUDGET on a challenge waits instead, at most until its
	// subscription's pending window ends.
	WebhookHostChallengesPerHour = 60
	// WebhookHostAuthChallengesPerHour bounds header-carrying challenges to
	// one host per UTC hour.
	WebhookHostAuthChallengesPerHour = 30
	// webhookDataBytesAuth is parseWebhookData's cap while the flag is on.
	webhookDataBytesAuth = 2048
	// webhookClaimTries bounds how many over-budget rows one claim defers
	// before it gives the turn up.
	webhookClaimTries = 8
	// webhookHostJitter spreads deferred rows over the next hour's start.
	webhookHostJitter = 300
	// webhookHostDeferMaxAge is how long a delivery may wait for its host's
	// budget before it is dropped (webhookHostDropped).
	webhookHostDeferMaxAge = 86400
	// webhookChallengeBucket prefixes a host's challenge bucket in
	// webhook_host_rates.
	webhookChallengeBucket = "challenge:"

	webhookAuthInfo = "swarmmemo-webhook-auth-v1"
	// WebhookAuthUnreadable is the reason a subscription whose sealed
	// headers no longer open (another key, a damaged row) is disabled with.
	WebhookAuthUnreadable = "auth headers unreadable, create it again"
	// webhookConfirmExpired is the reason a subscription that took
	// WebhookConfirmAttempts wrong nonces expires with.
	webhookConfirmExpired = "challenge not confirmed: too many wrong nonces"
	// webhookNotConfirmed is the one reason a pending subscription expires
	// with when it carries headers, whatever its endpoint answered.
	webhookNotConfirmed = "challenge not confirmed in time"
	// webhookHostDropped is the one reason a delivery that waited
	// webhookHostDeferMaxAge for its host's budget is dropped with.
	webhookHostDropped = "a delivery was dropped: its host stayed over the hourly budget for a day"
	// WebhookConfirmHow is the challenge body's fixed line for a
	// confirm-mode subscription.
	WebhookConfirmHow = "Send signed webhook.confirm with data {\"schema\":1,\"subscription_id\":SUBSCRIPTION_ID,\"nonce\":NONCE} to activate; the response to this request is never read."
)

// WebhookAuthHeaderNames are the header names a subscription may store, as
// stored (canonical case); matched case-insensitively. No open X-* pattern:
// X-Forwarded-For, X-Original-URL and method overrides are routing attacks
// on the target.
var WebhookAuthHeaderNames = []string{"Authorization", "X-API-Key", "X-Auth-Token", "X-Webhook-Token"}

// WebhookActivations are webhook.create's activation values: echo (the
// default: the endpoint echoes the challenge nonce) and confirm (the owner
// sends webhook.confirm with it; forced with headers).
var WebhookActivations = []string{"echo", "confirm"}

const webhookHostRatesSchema = `
CREATE TABLE IF NOT EXISTS webhook_host_rates (
 host TEXT NOT NULL, hour INTEGER NOT NULL, requests INTEGER NOT NULL DEFAULT 0,
 auth_challenges INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(host,hour));
`

// migrateWebhookAuth56 adds schema 56's columns, keyed on each one, and the
// per-host counters. Columns, not a table: the expired-row reuse UPDATE in
// changeWebhook rewrites the row's id, which a child table would orphan.
func migrateWebhookAuth56(tx *sql.Tx) error {
	for _, column := range []struct{ table, name, decl string }{
		{"webhook_subscriptions", "auth_names", "TEXT NOT NULL DEFAULT ''"},
		{"webhook_subscriptions", "auth_sealed", "BLOB NOT NULL DEFAULT x''"},
		{"webhook_subscriptions", "auth_key_id", "TEXT NOT NULL DEFAULT ''"},
		{"webhook_subscriptions", "activation", "TEXT NOT NULL DEFAULT 'echo'"},
		{"webhook_subscriptions", "confirm_tries", "INTEGER NOT NULL DEFAULT 0"},
		{"webhook_rates", "auth_challenges", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var exists int
		if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info(?) WHERE name=?", column.table, column.name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := tx.Exec("ALTER TABLE " + column.table + " ADD COLUMN " + column.name + " " + column.decl); err != nil {
				return err
			}
		}
	}
	_, err := tx.Exec(webhookHostRatesSchema)
	return err
}

// authSecret is a header value in memory. Every way Go prints or encodes a
// value (fmt's verbs, %#v, encoding/json, log/slog, a panic) shows
// "<redacted>"; reveal is the one way to the bytes, for the request header.
type authSecret string

func (authSecret) String() string               { return rawRedacted }
func (authSecret) GoString() string             { return rawRedacted }
func (authSecret) MarshalJSON() ([]byte, error) { return json.Marshal(rawRedacted) }
func (authSecret) MarshalText() ([]byte, error) { return []byte(rawRedacted), nil }
func (authSecret) LogValue() slog.Value         { return slog.StringValue(rawRedacted) }
func (authSecret) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, rawRedacted) }
func (v authSecret) reveal() string             { return string(v) }

// authHeader is one stored header: its canonical name and redacted value.
type authHeader struct {
	Name  string
	Value authSecret
}

// webhookAuthKey is the subkey auth_sealed is sealed under, and its id.
type webhookAuthKey struct {
	aead cipher.AEAD // nil: no key (a read-only open without the key file)
	id   string
	// mac keys digestable's HMAC of a header value: a subkey of its own.
	mac []byte
}

// deriveWebhookAuthKey is HKDF-SHA256 over the raw signals key with
// webhookAuthInfo: a key of its own, so nothing that holds the raw signals
// AEAD opens a header and no header opens a raw record.
func deriveWebhookAuthKey(rawSignalsKey []byte) (webhookAuthKey, error) {
	sub, err := hkdf.Key(sha256.New, rawSignalsKey, nil, webhookAuthInfo, 32)
	if err != nil {
		return webhookAuthKey{}, err
	}
	block, err := aes.NewCipher(sub)
	if err != nil {
		return webhookAuthKey{}, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return webhookAuthKey{}, err
	}
	mac, err := hkdf.Key(sha256.New, rawSignalsKey, nil, webhookAuthInfo+"-digest", 32)
	if err != nil {
		return webhookAuthKey{}, err
	}
	sum := sha256.Sum256(append([]byte(webhookAuthInfo+"-key-id\x00"), sub...))
	return webhookAuthKey{aead: aead, id: hex.EncodeToString(sum[:8]), mac: mac}, nil
}

// digestable is cmd as the requests table digests it (requestDigest): a
// webhook.create's header values are replaced by their HMAC under the
// webhook auth key, so the stored digest is no unsalted hash of a token a
// dictionary could guess from a copy of the database. The same command
// still digests the same, so idempotent retries hold. Data that does not
// parse is replaced by its HMAC whole (such a create is refused and
// stores nothing).
func (s *Store) digestable(cmd Command) Command {
	if cmd.Operation != "webhook.create" || cmd.Data == "" {
		return cmd
	}
	sum := func(v string) string {
		mac := hmac.New(sha256.New, s.webhookAuth.mac)
		mac.Write([]byte(v))
		return "hmac:" + hex.EncodeToString(mac.Sum(nil))
	}
	var data map[string]json.RawMessage
	if json.Unmarshal([]byte(cmd.Data), &data) != nil {
		cmd.Data = sum(cmd.Data)
		return cmd
	}
	raw, ok := data["headers"]
	if !ok {
		return cmd
	}
	var headers map[string]json.RawMessage
	if json.Unmarshal(raw, &headers) != nil {
		cmd.Data = sum(cmd.Data)
		return cmd
	}
	for name, value := range headers {
		headers[name], _ = json.Marshal(sum(string(value)))
	}
	data["headers"], _ = json.Marshal(headers)
	out, err := json.Marshal(data)
	if err != nil {
		cmd.Data = sum(cmd.Data)
		return cmd
	}
	cmd.Data = string(out)
	return cmd
}

// webhookAuthAAD binds a sealed blob to its key and row: copied to another
// subscription, account or URL it does not open.
func webhookAuthAAD(keyID, subscription, account, target string) []byte {
	return []byte(webhookAuthInfo + "\x00" + keyID + "\x00" + subscription + "\x00" + account + "\x00" + target)
}

// sealWebhookAuth seals headers for one row: auth_names, auth_sealed and
// auth_key_id as stored.
func (s *Store) sealWebhookAuth(headers []authHeader, subscription, account, target string) (string, []byte, string, error) {
	if s.webhookAuth.aead == nil {
		return "", nil, "", webhookAuthError("webhook_headers_unavailable")
	}
	pairs := make([][2]string, len(headers))
	names := make([]string, len(headers))
	for i, h := range headers {
		pairs[i], names[i] = [2]string{h.Name, h.Value.reveal()}, h.Name
	}
	plain, err := json.Marshal(pairs)
	if err != nil {
		return "", nil, "", err
	}
	sealed := s.webhookAuth.aead.Seal(nil, nil, plain, webhookAuthAAD(s.webhookAuth.id, subscription, account, target))
	clear(plain)
	return strings.Join(names, ","), sealed, s.webhookAuth.id, nil
}

// errWebhookAuthUnreadable is a sealed blob that does not open.
var errWebhookAuthUnreadable = errors.New(WebhookAuthUnreadable)

// openWebhookAuth opens one row's sealed headers; the names must match the
// listed auth_names, in order.
func (s *Store) openWebhookAuth(names string, sealed []byte, keyID, subscription, account, target string) ([]authHeader, error) {
	if s.webhookAuth.aead == nil || keyID != s.webhookAuth.id {
		return nil, errWebhookAuthUnreadable
	}
	plain, err := s.webhookAuth.aead.Open(nil, nil, sealed, webhookAuthAAD(keyID, subscription, account, target))
	if err != nil {
		return nil, errWebhookAuthUnreadable
	}
	defer clear(plain)
	var pairs [][2]string
	if json.Unmarshal(plain, &pairs) != nil || len(pairs) == 0 || len(pairs) > WebhookMaxHeaders {
		return nil, errWebhookAuthUnreadable
	}
	out := make([]authHeader, len(pairs))
	listed := strings.Split(names, ",")
	if len(listed) != len(pairs) {
		return nil, errWebhookAuthUnreadable
	}
	for i, p := range pairs {
		if p[0] != listed[i] || !slices.Contains(WebhookAuthHeaderNames, p[0]) || webhookHeaderValueRule(p[1]) != "" {
			return nil, errWebhookAuthUnreadable
		}
		out[i] = authHeader{Name: p[0], Value: authSecret(p[1])}
	}
	return out, nil
}

// webhookAuthHeaders is a subscription's stored headers, opened: nil for a
// subscription without any, errWebhookAuthUnreadable for a blob that will
// not open. send is its only caller, outside any transaction.
func (s *Store) webhookAuthHeaders(ctx context.Context, subscription string) ([]authHeader, error) {
	var names, keyID, account, target string
	var sealed []byte
	err := s.db.QueryRowContext(ctx, "SELECT auth_names,auth_sealed,auth_key_id,account,url FROM webhook_subscriptions WHERE id=?", subscription).
		Scan(&names, &sealed, &keyID, &account, &target)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil || names == "" {
		return nil, err
	}
	defer clear(sealed)
	return s.openWebhookAuth(names, sealed, keyID, subscription, account, target)
}

// webhookAuthState is what settle needs of a subscription's activation: does
// it store headers, and is it confirm-mode.
func (s *Store) webhookAuthState(ctx context.Context, subscription string) (headers, confirm bool, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT auth_names<>'',activation='confirm' FROM webhook_subscriptions WHERE id=?", subscription).Scan(&headers, &confirm)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	return headers, confirm, err
}

func webhookAuthError(code string) error {
	switch code {
	case "webhook_headers_post_only":
		return problem(400, "webhook_headers_post_only", "Auth headers are accepted only in a POST body (POST /v1/command or the web page), never in a URL such as /c64/: URLs reach access logs.")
	case "webhook_headers_unavailable":
		return problem(503, "webhook_headers_unavailable", "Webhook auth headers and confirm activation are not available on this server.")
	case "webhook_nonce_mismatch":
		return problem(403, "webhook_nonce_mismatch", "That nonce is not this subscription's challenge nonce. After "+LimitText("webhook_confirm_attempts")+" wrong nonces the subscription expires.")
	case "webhook_host_busy":
		e := problem(429, "webhook_host_busy", "Too many challenges with auth headers: at most "+LimitText("webhook_host_auth_challenges_per_hour")+" per host per hour and "+LimitText("webhook_auth_challenges_per_day")+" per agent per day. Try again later.").(*Error)
		e.RetryAfter = 3600
		return e
	}
	return webhookError(code)
}

// webhookHeadersError refuses one header by name and rule, never its value.
func webhookHeadersError(rule string) error {
	return problem(400, "invalid_webhook", "headers: "+rule)
}

// webhookHeaderValueRule is "" for a value webhook.create accepts, else
// the rule it breaks: 1 to WebhookHeaderValueBytes bytes of visible ASCII
// (0x21-0x7E) with single spaces between words.
func webhookHeaderValueRule(v string) string {
	if v == "" || len(v) > WebhookHeaderValueBytes {
		return "a value is 1 to " + LimitText("webhook_header_value_bytes") + " long"
	}
	if v[0] == ' ' || v[len(v)-1] == ' ' || strings.Contains(v, "  ") {
		return "a value has no leading, trailing or repeated spaces"
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c != ' ' && (c < 0x21 || c > 0x7e) {
			return "a value is visible ASCII (0x21-0x7E) and single spaces only"
		}
	}
	return ""
}

// parseWebhookHeaders reads webhook.create's headers object: 1 to
// WebhookMaxHeaders names from WebhookAuthHeaderNames (any case, stored
// canonical, each once) with string values. Errors name the header and the
// rule, never a value.
func parseWebhookHeaders(raw json.RawMessage) ([]authHeader, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, webhookHeadersError("an object of header names to string values, such as {\"Authorization\":\"Bearer ...\"}")
	}
	var out []authHeader
	for decoder.More() {
		token, err = decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return nil, webhookHeadersError("an object of header names to string values")
		}
		canonical := ""
		for _, allowed := range WebhookAuthHeaderNames {
			if strings.EqualFold(name, allowed) {
				canonical = allowed
			}
		}
		if canonical == "" {
			return nil, webhookHeadersError("a name is one of " + strings.Join(WebhookAuthHeaderNames, ", ") + " (any case)")
		}
		for _, h := range out {
			if h.Name == canonical {
				return nil, webhookHeadersError(canonical + " is given twice")
			}
		}
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return nil, webhookHeadersError("an object of header names to string values")
		}
		var text string
		if json.Unmarshal(value, &text) != nil {
			return nil, webhookHeadersError(canonical + ": the value is a string")
		}
		if rule := webhookHeaderValueRule(text); rule != "" {
			return nil, webhookHeadersError(canonical + ": " + rule)
		}
		if len(out) == WebhookMaxHeaders {
			return nil, webhookHeadersError("at most " + LimitText("webhook_headers") + " headers")
		}
		out = append(out, authHeader{Name: canonical, Value: authSecret(text)})
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || len(out) == 0 {
		return nil, webhookHeadersError("an object of 1 to " + LimitText("webhook_headers") + " header names to string values")
	}
	return out, nil
}

// webhookHeaderWires are the channels whose command travels in a request
// body: the only ones a header value may arrive on. "" is an in-process
// caller. Every GET-style write (/c64/, also POSTed, keeps the command in
// its URL) and every other wire is refused.
var webhookHeaderWires = []string{"", "command", "ui", "app"}

// webhookHost is the per-host budget's key for a callback URL: its
// registrable domain (shared hosting platforms count per subdomain); a
// literal IPv4 address is itself, an IPv6 address its /64. A name that is
// no subject name (an underscore label, a special-use top-level name) still
// folds to its root under the same suffix list, so a_1.platform.com and
// a_2.platform.com share platform.com's bucket.
func webhookHost(target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return v4.String()
		}
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	if root := reviews.RegistrableDomain(host); root != "" {
		return root
	}
	return strings.TrimPrefix(trust.DomainRoot(host, reviews.DomainSuffixes()), "domain:")
}

// webhookHeaderHost reports whether target's host may receive auth headers:
// a public DNS name of ASCII labels (reviews.RegistrableDomain's rules: no
// IP literal, no underscore, no special-use top-level name).
func webhookHeaderHost(target string) bool {
	u, err := url.Parse(target)
	return err == nil && reviews.RegistrableDomain(u.Hostname()) != ""
}

// ownWebhookHost reports whether target's host is this service's own
// registrable domain (PublicURL, else https://ServiceID): headers are never
// sent to ourselves.
func (s *Store) ownWebhookHost(target string) bool {
	own := s.config.PublicURL
	if own == "" {
		own = "https://" + s.config.ServiceID
	}
	mine, theirs := webhookHost(own), webhookHost(target)
	return mine != "" && mine == theirs
}

// chargeWebhookAuthChallenge counts one header challenge against its host's
// hourly and its agent's daily bound, in the create transaction, or refuses
// it with webhook_host_busy. The refusal depends on counts alone, never on
// the token or any endpoint's answer.
func chargeWebhookAuthChallenge(ctx context.Context, tx *sql.Tx, account, host string, now int64) error {
	hour := now / 3600
	var perHost, perAgent int64
	if err := tx.QueryRowContext(ctx, "SELECT coalesce((SELECT auth_challenges FROM webhook_host_rates WHERE host=? AND hour=?),0),coalesce((SELECT sum(auth_challenges) FROM webhook_rates WHERE account=? AND hour>?),0)",
		host, hour, account, hour-24).Scan(&perHost, &perAgent); err != nil {
		return err
	}
	if perHost >= WebhookHostAuthChallengesPerHour || perAgent >= WebhookAuthChallengesPerDay {
		return webhookAuthError("webhook_host_busy")
	}
	// Logged once, as the challenge that fills a cap is counted.
	if perHost+1 == WebhookHostAuthChallengesPerHour {
		slog.Warn("webhook host reached its auth challenge cap", "host", host, "per_hour", WebhookHostAuthChallengesPerHour)
	}
	if perAgent+1 == WebhookAuthChallengesPerDay {
		slog.Warn("webhook agent reached its auth challenge cap", "host", host, "per_day", WebhookAuthChallengesPerDay)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO webhook_host_rates(host,hour,auth_challenges) VALUES(?,?,1) ON CONFLICT(host,hour) DO UPDATE SET auth_challenges=auth_challenges+1", host, hour); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO webhook_rates(account,hour,count,auth_challenges) VALUES(?,?,0,1) ON CONFLICT(account,hour) DO UPDATE SET auth_challenges=auth_challenges+1", account, hour)
	return err
}

// webhookHostAdmit counts one webhook request in bucket (a host, or a
// host's challenge bucket) for this UTC hour, on the claim's transaction.
// Over limit it reports false when enforce is set; otherwise it counts on
// and the request goes (the log-only phase). The bucket reaching limit logs
// one warning.
func webhookHostAdmit(ctx context.Context, tx *sql.Tx, bucket string, limit int64, enforce bool, now int64) (bool, error) {
	hour := now / 3600
	var used int64
	err := tx.QueryRowContext(ctx, "SELECT requests FROM webhook_host_rates WHERE host=? AND hour=?", bucket, hour).Scan(&used)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if used >= limit && enforce {
		return false, nil
	}
	if used+1 == limit {
		slog.Warn("webhook host reached its hourly request budget", "bucket", bucket, "per_hour", limit, "enforced", enforce)
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO webhook_host_rates(host,hour,requests) VALUES(?,?,1) ON CONFLICT(host,hour) DO UPDATE SET requests=requests+1", bucket, hour)
	return err == nil, err
}

// webhookConfirmData is webhook.confirm's data.
type webhookConfirmData struct {
	Schema         int    `json:"schema"`
	SubscriptionID string `json:"subscription_id"`
	Nonce          string `json:"nonce"`
}

// parseWebhookConfirm reads webhook.confirm's data, strictly.
func parseWebhookConfirm(raw string) (webhookConfirmData, error) {
	var d webhookConfirmData
	invalid := problem(400, "invalid_webhook", "Data must be {\"schema\":1,\"subscription_id\":ID,\"nonce\":NONCE}: the subscription_id webhook.create returned and the nonce its challenge carried.")
	if raw == "" || len(raw) > 1024 || !utf8.ValidString(raw) || jsonstrict.Object([]byte(raw), jsonstrict.DepthMax) != nil {
		return d, invalid
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&d) != nil || dec.More() || d.Schema != 1 || !workIDRE.MatchString(d.SubscriptionID) || d.Nonce == "" || len(d.Nonce) > RequestIDBytes {
		return d, invalid
	}
	return d, nil
}

// confirmWebhook is webhook.confirm: the owner proves it read the challenge
// nonce behind its endpoint's auth, and the pending subscription becomes
// active. Same account only; a wrong nonce counts (recordConfirmMiss, after
// the refusal rolls back) and the fifth expires the subscription.
func (s *Store) confirmWebhook(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if a.grant != nil {
		return Result{}, webhookError("webhook_delegated")
	}
	if !s.config.Features.WebhookAuthHeaders {
		return Result{}, webhookAuthError("webhook_headers_unavailable")
	}
	d, err := parseWebhookConfirm(c.Data)
	if err != nil {
		return Result{}, err
	}
	if err := s.charge(ctx, tx, a, SmallCommandCost, now); err != nil {
		return Result{}, err
	}
	var state, challenge string
	var created int64
	err = tx.QueryRowContext(ctx, "SELECT state,challenge,created_at FROM webhook_subscriptions s WHERE id=? AND account=? AND NOT "+isWebPush, d.SubscriptionID, a.account).Scan(&state, &challenge, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, webhookError("webhook_not_found")
	}
	if err != nil {
		return Result{}, err
	}
	if state != "pending" || created+WebhookPendingTTL <= now || challenge == "" {
		return Result{}, problem(404, "webhook_not_found", "No pending subscription with that id for this account: it is already active, or it expired (create it again).")
	}
	if subtle.ConstantTimeCompare([]byte(d.Nonce), []byte(challenge)) != 1 {
		e := webhookAuthError("webhook_nonce_mismatch").(*Error)
		e.confirmMiss = d.SubscriptionID
		return Result{}, e
	}
	if _, err = tx.ExecContext(ctx, "UPDATE webhook_subscriptions SET state='active',confirmed_at=?,challenge='',failures=0,last_error='' WHERE id=? AND state='pending'", now, d.SubscriptionID); err != nil {
		return Result{}, err
	}
	// Whatever is still queued for it (the challenge, if it has not been
	// sent yet) is moot now.
	if _, err = tx.ExecContext(ctx, "DELETE FROM webhook_deliveries WHERE subscription=? AND kind='challenge'", d.SubscriptionID); err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, a.id, d.SubscriptionID, "webhook subscription confirmed", now); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"subscription_id": d.SubscriptionID, "state": "active", "confirmed_at": now}}, nil
}

// recordConfirmMiss counts a wrong webhook.confirm nonce once its
// transaction has rolled back, and expires the subscription at the
// WebhookConfirmAttempts-th: state disabled unconfirmed, the sealed headers
// and the nonce cleared, its queue dropped.
func (s *Store) recordConfirmMiss(ctx context.Context, tx *sql.Tx, err error, now int64) {
	var e *Error
	if !errors.As(err, &e) || e.confirmMiss == "" {
		return
	}
	_ = tx.Rollback()
	ctx = context.WithoutCancel(ctx)
	if werr := func() error {
		miss, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer miss.Rollback()
		var tries int64
		err = miss.QueryRowContext(ctx, "UPDATE webhook_subscriptions SET confirm_tries=confirm_tries+1 WHERE id=? AND state='pending' RETURNING confirm_tries", e.confirmMiss).Scan(&tries)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if tries >= WebhookConfirmAttempts {
			if _, err = miss.ExecContext(ctx, "UPDATE webhook_subscriptions SET state='disabled',disabled_at=?,challenge='',auth_sealed=x'',last_error=? WHERE id=? AND state='pending'", now, webhookConfirmExpired, e.confirmMiss); err != nil {
				return err
			}
			if _, err = miss.ExecContext(ctx, "DELETE FROM webhook_deliveries WHERE subscription=?", e.confirmMiss); err != nil {
				return err
			}
		}
		return miss.Commit()
	}(); werr != nil {
		slog.Warn("webhook confirm miss not counted", "error", werr)
	}
}

// WebhookSenderForTest points the sender at client, skips the DNS half of
// webhook.create's address check (a literal address is still checked) and
// returns one sender step, for tests in other packages (the redaction test
// in httpapi). Nothing but Go code reaches it: no flag or configuration.
func (s *Store) WebhookSenderForTest(client *http.Client) func(context.Context) (bool, error) {
	s.webhookInsecure = true
	s.webhookClient = client
	return s.deliverOnce
}
