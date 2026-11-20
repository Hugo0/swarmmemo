package board

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchema56Upgrade(t *testing.T) { schema29UpgradeFrom(t, 55) }

const testAuthValue = "Bearer canary-0123456789abcdef"

// authReceiver is a TLS endpoint the sender reaches for any https URL; it
// records every request's headers and body.
type authReceiver struct {
	mu       sync.Mutex
	requests []authRequest
}

type authRequest struct {
	header http.Header
	body   []byte
}

func (r *authReceiver) seen() []authRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]authRequest{}, r.requests...)
}

// authStore is a store with WEBHOOK_AUTH_HEADERS on whose sender dials a
// test server answering with reply, whatever the URL's host.
func authStore(t *testing.T, reply func(req authRequest) (int, string)) (*Store, *authReceiver) {
	t.Helper()
	s := openTest(t, Config{})
	s.config.Features.WebhookAuthHeaders = true
	return s, pointSender(t, s, reply)
}

func pointSender(t *testing.T, s *Store, reply func(req authRequest) (int, string)) *authReceiver {
	t.Helper()
	s.webhookInsecure = true // skips DNS at creation; the URL is still checked
	rec := &authReceiver{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		got := authRequest{header: r.Header.Clone(), body: raw}
		rec.mu.Lock()
		rec.requests = append(rec.requests, got)
		rec.mu.Unlock()
		status, text := reply(got)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, text)
	}))
	t.Cleanup(server.Close)
	s.webhookClient = testSenderClient(server)
	return rec
}

func testSenderClient(server *httptest.Server) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	addr := server.Listener.Addr().String()
	return &http.Client{
		Timeout:       webhookRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
			TLSClientConfig:   &tls.Config{RootCAs: pool, ServerName: "example.com"},
			DisableKeepAlives: true,
		},
	}
}

func authCreateData(url, headers string) string {
	return `{"schema":1,"url":"` + url + `","headers":` + headers + `}`
}

// drainAuth runs the sender until nothing is due now.
func drainAuth(t *testing.T, s *Store) int {
	t.Helper()
	n := 0
	for i := 0; i < 500; i++ {
		worked, err := s.deliverOnce(testContext)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			return n
		}
		n++
	}
	t.Fatal("the sender never ran dry")
	return n
}

func challengeNonce(t *testing.T, req authRequest) string {
	t.Helper()
	var body struct {
		Type, Nonce, Activation, How string
	}
	if err := json.Unmarshal(req.body, &body); err != nil || body.Type != "challenge" || body.Nonce == "" {
		t.Fatalf("not a challenge: %s", req.body)
	}
	return body.Nonce
}

// The value type never shows its bytes, whichever way Go prints it.
func TestWebhookAuthSecretRedacts(t *testing.T) {
	v := authSecret(testAuthValue)
	h := authHeader{Name: "Authorization", Value: v}
	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, nil))
	logger.Info("x", "value", v, "header", h, "headers", []authHeader{h})
	logger.Info("y", slog.Any("any", v), slog.Any("ptr", &h))
	raw, err := json.Marshal(map[string]any{"h": h, "v": v, "list": []authHeader{h}})
	if err != nil {
		t.Fatal(err)
	}
	text, _ := v.MarshalText()
	panicked := func() (out string) {
		defer func() { out = fmt.Sprint(recover()) }()
		panic(v)
	}()
	outputs := []string{
		fmt.Sprint(v), fmt.Sprintf("%v %s %q %x %X %+v %#v %10s %-3v", v, v, v, v, v, v, v, v, v),
		fmt.Sprintf("%v %+v %#v", h, h, h), fmt.Sprintf("%v %#v", &h, []authHeader{h}),
		fmt.Sprint(errors.New(fmt.Sprint(v))), logged.String(), string(raw), string(text), panicked,
	}
	for _, out := range outputs {
		if strings.Contains(out, "canary") || strings.Contains(out, "Bearer") || strings.Contains(out, fmt.Sprintf("%x", testAuthValue)) {
			t.Fatalf("a value leaked: %s", out)
		}
		if !strings.Contains(out, "redacted") {
			t.Fatalf("not redacted: %s", out)
		}
	}
	if v.reveal() != testAuthValue {
		t.Fatal("reveal lost the value")
	}
}

// A sealed blob opens only for its own key, row, account and URL, and never
// with the raw signals AEAD it is derived from.
func TestWebhookAuthSealBindsRow(t *testing.T) {
	s := openTest(t, Config{})
	headers := []authHeader{{"Authorization", authSecret(testAuthValue)}, {"X-API-Key", "k-1"}}
	names, sealed, keyID, err := s.sealWebhookAuth(headers, "sub-1", "acct", "https://hooks.example.org/a")
	if err != nil || names != "Authorization,X-API-Key" || keyID != s.webhookAuth.id || keyID == s.rawSignals.id {
		t.Fatalf("seal: %q %q %v", names, keyID, err)
	}
	if bytes.Contains(sealed, []byte("canary")) {
		t.Fatal("the blob holds the value in the clear")
	}
	got, err := s.openWebhookAuth(names, sealed, keyID, "sub-1", "acct", "https://hooks.example.org/a")
	if err != nil || len(got) != 2 || got[0].Value.reveal() != testAuthValue || got[1].Name != "X-API-Key" {
		t.Fatalf("open: %v %v", got, err)
	}
	for _, c := range []struct{ names, key, id, account, url string }{
		{names, keyID, "sub-2", "acct", "https://hooks.example.org/a"},
		{names, keyID, "sub-1", "other", "https://hooks.example.org/a"},
		{names, keyID, "sub-1", "acct", "https://hooks.example.org/b"},
		{names, "0000000000000000", "sub-1", "acct", "https://hooks.example.org/a"},
		{"X-API-Key,Authorization", keyID, "sub-1", "acct", "https://hooks.example.org/a"},
		{"Authorization", keyID, "sub-1", "acct", "https://hooks.example.org/a"},
	} {
		if _, err := s.openWebhookAuth(c.names, sealed, c.key, c.id, c.account, c.url); !errors.Is(err, errWebhookAuthUnreadable) {
			t.Fatalf("opened under %+v", c)
		}
	}
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 1
	if _, err := s.openWebhookAuth(names, tampered, keyID, "sub-1", "acct", "https://hooks.example.org/a"); err == nil {
		t.Fatal("a tampered blob opened")
	}
	// The raw signals AEAD (what `signals raw` holds) opens nothing.
	aad := webhookAuthAAD(keyID, "sub-1", "acct", "https://hooks.example.org/a")
	if _, err := s.rawSignals.aead.Open(nil, nil, sealed, aad); err == nil {
		t.Fatal("the raw signals key opens a sealed header")
	}
	// Another store (another key) cannot open it.
	other := openTest(t, Config{})
	if _, err := other.openWebhookAuth(names, sealed, keyID, "sub-1", "acct", "https://hooks.example.org/a"); err == nil {
		t.Fatal("another key opened the blob")
	}
	// No key: nothing seals.
	other.webhookAuth = webhookAuthKey{}
	if _, _, _, err := other.sealWebhookAuth(headers, "s", "a", "u"); errCode(err) != "webhook_headers_unavailable" {
		t.Fatalf("sealed without a key: %v", err)
	}
}

// The parser takes only the allowlisted names, any case, stored canonical,
// at most two, values of visible ASCII with single spaces; every refusal
// names the rule and never the value.
func TestWebhookHeadersParse(t *testing.T) {
	ok := map[string][]string{
		`{"authorization":"Bearer a b"}`:                                          {"Authorization"},
		`{"X-API-KEY":"k","x-webhook-token":"t"}`:                                 {"X-API-Key", "X-Webhook-Token"},
		`{"X-Auth-Token":"` + strings.Repeat("a", WebhookHeaderValueBytes) + `"}`: {"X-Auth-Token"},
	}
	for raw, want := range ok {
		got, err := parseWebhookHeaders(json.RawMessage(raw))
		if err != nil || len(got) != len(want) {
			t.Fatalf("%s: %v %v", raw, got, err)
		}
		for i := range want {
			if got[i].Name != want[i] {
				t.Fatalf("%s: names %v", raw, got)
			}
		}
	}
	const secret = "s3cr3t-value"
	for _, raw := range []string{
		`{}`, `[]`, `"x"`, `{"Cookie":"` + secret + `"}`, `{"X-Forwarded-For":"` + secret + `"}`, `{"X-Original-URL":"` + secret + `"}`,
		`{"X-HTTP-Method-Override":"` + secret + `"}`, `{"Content-Type":"` + secret + `"}`, `{"X-SwarmMemo-Signature":"` + secret + `"}`,
		`{"Authorization":"` + secret + `","authorization":"` + secret + `"}`,
		`{"Authorization":"a","X-API-Key":"b","X-Auth-Token":"` + secret + `"}`,
		`{"Authorization":""}`, `{"Authorization":" ` + secret + `"}`, `{"Authorization":"` + secret + ` "}`, `{"Authorization":"a  ` + secret + `"}`,
		`{"Authorization":"a\t` + secret + `"}`, `{"Authorization":"a\n` + secret + `"}`, `{"Authorization":"a\r\nX: ` + secret + `"}`, `{"Authorization":"é` + secret + `"}`,
		`{"Authorization":"` + strings.Repeat("a", WebhookHeaderValueBytes+1) + `"}`, `{"Authorization":7}`, `{"Authorization":["` + secret + `"]}`,
	} {
		_, err := parseWebhookHeaders(json.RawMessage(raw))
		if errCode(err) != "invalid_webhook" {
			t.Fatalf("%s: %v", raw, err)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "aaaa") {
			t.Fatalf("%s: the refusal names the value: %v", raw, err)
		}
	}
}

// Off, the fields and webhook.confirm answer webhook_headers_unavailable and
// a plain create is exactly as before.
func TestWebhookAuthHeadersFlagOff(t *testing.T) {
	s := insecureStore(t)
	key := keyFor(141)
	for _, data := range []string{authCreateData("https://hooks.example.org/a", `{"Authorization":"x"}`), `{"schema":1,"url":"https://hooks.example.org/a","activation":"confirm"}`} {
		fails(t, s, signed(key, Command{Operation: "webhook.create", Data: data}), "webhook_headers_unavailable")
	}
	fails(t, s, signed(key, Command{Operation: "webhook.confirm", Data: `{"schema":1,"subscription_id":"` + randomID() + `","nonce":"n"}`}), "webhook_headers_unavailable")
	created := run(t, s, signed(key, Command{Operation: "webhook.create", Data: testWebhookData}))
	for _, field := range []string{"headers", "activation", "headers_listed"} {
		if _, ok := created.Data[field]; ok {
			t.Fatalf("flag off, create answers %s", field)
		}
	}
	listed := run(t, s, signed(key, Command{Operation: "webhook.list"}))
	if _, ok := listed.Data["headers_listed"]; ok {
		t.Fatal("flag off, list answers headers_listed")
	}
	// Over the old 1024-byte bound is still refused.
	fails(t, s, signed(key, Command{Operation: "webhook.create", Data: `{"schema":1,"url":"https://hooks.example.org/` + strings.Repeat("a", 1100) + `"}`}), "invalid_webhook")
}

// Refused with headers: a URL wire, our own domain, no key, echo activation.
func TestWebhookAuthHeadersRefusals(t *testing.T) {
	s, _ := authStore(t, func(authRequest) (int, string) { return 200, "" })
	key := keyFor(142)
	data := authCreateData("https://hooks.example.org/a", `{"Authorization":"`+testAuthValue+`"}`)
	for _, via := range []string{"c64", "get", "x-text", "mkcol", "post", "put", "mcp", "dns", "email", "a2a"} {
		_, err := s.Execute(WithVia(testContext, via), signed(key, Command{Operation: "webhook.create", Data: data}), "test-origin")
		if code := errCode(err); code != "webhook_headers_post_only" && code != "unsupported_operation" {
			t.Fatalf("via %s: %v", via, err)
		}
	}
	for _, via := range []string{"command", "ui"} {
		_, err := s.Execute(WithVia(testContext, via), signed(key, Command{Operation: "webhook.create", Data: authCreateData("https://hooks.example.org/"+via, `{"Authorization":"x"}`)}), "test-origin")
		if err != nil {
			t.Fatalf("via %s refused: %v", via, err)
		}
	}
	for _, own := range []string{"https://swarmmemo.com/hook", "https://api.swarmmemo.com/hook", "https://SwarmMemo.com./x"} {
		fails(t, s, signed(key, Command{Operation: "webhook.create", Data: authCreateData(own, `{"Authorization":"x"}`)}), "invalid_webhook")
	}
	fails(t, s, signed(key, Command{Operation: "webhook.create", Data: `{"schema":1,"url":"https://hooks.example.org/e","activation":"echo","headers":{"Authorization":"x"}}`}), "invalid_webhook")
	fails(t, s, signed(key, Command{Operation: "webhook.create", Data: `{"schema":1,"url":"https://hooks.example.org/e","activation":"2xx"}`}), "invalid_webhook")
	saved := s.webhookAuth
	s.webhookAuth = webhookAuthKey{}
	fails(t, s, signed(key, Command{Operation: "webhook.create", Data: data}), "webhook_headers_unavailable")
	s.webhookAuth = saved
	// The flag's larger bound: a 2 KiB create with headers is read.
	long := authCreateData("https://hooks.example.org/long", `{"Authorization":"`+strings.Repeat("a", WebhookHeaderValueBytes)+`","X-API-Key":"`+strings.Repeat("b", WebhookHeaderValueBytes)+`"}`)
	if len(long) <= 1024 || len(long) > webhookDataBytesAuth {
		t.Fatalf("sample is %d bytes", len(long))
	}
	run(t, s, signed(key, Command{Operation: "webhook.create", Data: long}))
}

// The whole lifecycle: one challenge with the headers, never retried and
// never observable; webhook.confirm by the owner only; events carry the
// headers with the contract's own headers intact; after activation a 401 is
// the owner's to see; delete removes the blob.
func TestWebhookAuthHeadersLifecycle(t *testing.T) {
	var status atomic.Int64
	status.Store(503)
	s, rec := authStore(t, func(req authRequest) (int, string) {
		return int(status.Load()), `{"nonce":"echoed"}`
	})
	owner, other := keyFor(143), keyFor(144)
	created := run(t, s, signed(owner, Command{Operation: "webhook.create", Data: authCreateData("https://hooks.example.org/life", `{"authorization":"`+testAuthValue+`","X-Webhook-Token":"tok-2"}`)}))
	id := created.Data["subscription_id"].(string)
	if created.Data["activation"] != "confirm" || fmt.Sprint(created.Data["headers"]) != "[Authorization X-Webhook-Token]" || created.Data["headers_listed"] != false {
		t.Fatalf("create: %v", created.Data)
	}
	if strings.Contains(fmt.Sprint(created.Data), "canary") {
		t.Fatal("the create result holds the value")
	}
	// A 503 on the challenge is not retried: exactly one request, and
	// nothing left queued to retry.
	if n := drainAuth(t, s); n != 1 || queueDepth(t, s) != 0 {
		t.Fatalf("sender ran %d times, %d queued", n, queueDepth(t, s))
	}
	got := rec.seen()
	if len(got) != 1 {
		t.Fatalf("the header challenge was sent %d times", len(got))
	}
	ch := got[0]
	if ch.header.Get("Authorization") != testAuthValue || ch.header.Get("X-Webhook-Token") != "tok-2" || ch.header.Get("X-SwarmMemo-Signature") == "" || ch.header.Get("Content-Type") != "application/json" || ch.header.Get("User-Agent") != "SwarmMemo-Webhook/1" {
		t.Fatalf("challenge headers: %v", ch.header)
	}
	var body map[string]any
	_ = json.Unmarshal(ch.body, &body)
	if body["activation"] != "confirm" || body["how"] != WebhookConfirmHow || strings.Contains(string(ch.body), "canary") {
		t.Fatalf("challenge body: %s", ch.body)
	}
	nonce := challengeNonce(t, ch)
	state, failures, reason := subscriptionState(t, s, id)
	if state != "pending" || failures != 0 || reason != "" {
		t.Fatalf("the challenge's answer changed the row: %s %d %q", state, failures, reason)
	}
	listed := run(t, s, signed(owner, Command{Operation: "webhook.list"}))
	item := listed.Data["subscriptions"].([]map[string]any)[0]
	for _, hidden := range []string{"last_error", "pending_deliveries", "oldest_pending_attempts", "oldest_pending_next_attempt_at"} {
		if _, ok := item[hidden]; ok {
			t.Fatalf("a header subscription lists %s before confirm: %v", hidden, item)
		}
	}
	if item["state"] != "pending" || fmt.Sprint(item["headers"]) != "[Authorization X-Webhook-Token]" || listed.Data["headers_listed"] != false || listed.Data["queued_deliveries"] != int64(0) {
		t.Fatalf("list: %v", listed.Data)
	}
	// Another account cannot confirm it, even with the nonce.
	fails(t, s, signed(other, Command{Operation: "webhook.confirm", Data: `{"schema":1,"subscription_id":"` + id + `","nonce":"` + nonce + `"}`}), "webhook_not_found")
	fails(t, s, signed(owner, Command{Operation: "webhook.confirm", Data: `{"schema":1,"subscription_id":"` + id + `","nonce":"wrong"}`}), "webhook_nonce_mismatch")
	var tries int
	if err := s.db.QueryRow("SELECT confirm_tries FROM webhook_subscriptions WHERE id=?", id).Scan(&tries); err != nil || tries != 1 {
		t.Fatalf("a wrong nonce counted %d (%v)", tries, err)
	}
	confirmed := run(t, s, signed(owner, Command{Operation: "webhook.confirm", Data: `{"schema":1,"subscription_id":"` + id + `","nonce":"` + nonce + `"}`}))
	if confirmed.Data["state"] != "active" {
		t.Fatalf("confirm: %v", confirmed.Data)
	}
	fails(t, s, signed(owner, Command{Operation: "webhook.confirm", Data: `{"schema":1,"subscription_id":"` + id + `","nonce":"` + nonce + `"}`}), "webhook_not_found")
	// An event: carries the headers, and the contract's headers.
	status.Store(200)
	register(t, s, other)
	run(t, s, signed(other, Command{Operation: "post", Room: "lobby", Text: "hello owner", To: keyID(owner), RequestID: "to-owner-1"}))
	drainAuth(t, s)
	got = rec.seen()
	if len(got) != 2 {
		t.Fatalf("after confirm the receiver saw %d requests", len(got))
	}
	ev := got[1]
	if ev.header.Get("Authorization") != testAuthValue || !strings.Contains(string(ev.body), `"type":"event"`) || ev.header.Get("X-SwarmMemo-Signature") != SignWebhook(created.Data["secret"].(string), testTime, ev.body) {
		t.Fatalf("event: %v %s", ev.header, ev.body)
	}
	// A 401 after activation is the owner's to see.
	status.Store(401)
	run(t, s, signed(other, Command{Operation: "post", Room: "lobby", Text: "hello again", To: keyID(owner), RequestID: "to-owner-2"}))
	drainAuth(t, s)
	if _, _, reason = subscriptionState(t, s, id); reason != "status 401" {
		t.Fatalf("after activation a 401 reads %q", reason)
	}
	run(t, s, signed(owner, Command{Operation: "webhook.delete", Target: id}))
	var rows int
	if err := s.db.QueryRow("SELECT count(*) FROM webhook_subscriptions WHERE auth_sealed<>x''").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("a deleted subscription kept its blob: %d %v", rows, err)
	}
}

// Five wrong nonces expire the subscription; its blob goes, its names stay.
func TestWebhookConfirmAttemptsExpire(t *testing.T) {
	s, _ := authStore(t, func(authRequest) (int, string) { return 200, "" })
	owner := keyFor(145)
	id := run(t, s, signed(owner, Command{Operation: "webhook.create", Data: authCreateData("https://hooks.example.org/tries", `{"X-API-Key":"`+testAuthValue+`"}`)})).Data["subscription_id"].(string)
	for i := 0; i < WebhookConfirmAttempts; i++ {
		fails(t, s, signed(owner, Command{Operation: "webhook.confirm", Data: `{"schema":1,"subscription_id":"` + id + `","nonce":"bad"}`}), "webhook_nonce_mismatch")
	}
	fails(t, s, signed(owner, Command{Operation: "webhook.confirm", Data: `{"schema":1,"subscription_id":"` + id + `","nonce":"bad"}`}), "webhook_not_found")
	var state, names, reason, challenge string
	var sealed []byte
	if err := s.db.QueryRow("SELECT state,auth_names,auth_sealed,last_error,challenge FROM webhook_subscriptions WHERE id=?", id).Scan(&state, &names, &sealed, &reason, &challenge); err != nil {
		t.Fatal(err)
	}
	if state != "disabled" || names != "X-API-Key" || len(sealed) != 0 || reason != webhookConfirmExpired || challenge != "" || queueDepth(t, s) != 0 {
		t.Fatalf("after %d wrong nonces: %s %q %d %q %q", WebhookConfirmAttempts, state, names, len(sealed), reason, challenge)
	}
	item := run(t, s, signed(owner, Command{Operation: "webhook.list"})).Data["subscriptions"].([]map[string]any)[0]
	if item["state"] != "expired" || item["disabled_reason"] != webhookConfirmExpired {
		t.Fatalf("listed %v", item)
	}
	// Re-creating the URL writes every auth column afresh.
	again := run(t, s, signed(owner, Command{Operation: "webhook.create", Data: `{"schema":1,"url":"https://hooks.example.org/tries"}`}))
	if again.Data["activation"] != "echo" || fmt.Sprint(again.Data["headers"]) != "[]" {
		t.Fatalf("re-created: %v", again.Data)
	}
	var tries int
	if err := s.db.QueryRow("SELECT auth_names,auth_sealed,activation,confirm_tries FROM webhook_subscriptions WHERE url='https://hooks.example.org/tries'").Scan(&names, &sealed, &state, &tries); err != nil {
		t.Fatal(err)
	}
	if names != "" || len(sealed) != 0 || state != "echo" || tries != 0 {
		t.Fatalf("reused row: %q %d %q %d", names, len(sealed), state, tries)
	}
}

// Expiry and auto-disable keep the names only; a blob that will not open
// disables the subscription without a request.
func TestWebhookAuthClearedOnExpiryDisableAndUnreadable(t *testing.T) {
	s, rec := authStore(t, func(authRequest) (int, string) { return 401, "" })
	owner := keyFor(146)
	create := func(path string) string {
		return run(t, s, signed(owner, Command{Operation: "webhook.create", Data: authCreateData("https://hooks.example.org/"+path, `{"Authorization":"`+testAuthValue+`"}`)})).Data["subscription_id"].(string)
	}
	expiring, broken, live := create("expire"), create("broken"), create("live")
	if _, err := s.db.Exec("UPDATE webhook_subscriptions SET auth_sealed=x'00' WHERE id=?", broken); err != nil {
		t.Fatal(err)
	}
	// live is activated by hand; its deliveries then fail 401 five times.
	if _, err := s.db.Exec("UPDATE webhook_subscriptions SET state='active',confirmed_at=?,challenge='' WHERE id=?", testTime, live); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DELETE FROM webhook_deliveries WHERE subscription IN (?,?)", live, expiring); err != nil {
		t.Fatal(err)
	}
	drainAuth(t, s)
	if n := len(rec.seen()); n != 0 {
		t.Fatalf("an unreadable blob was sent (%d requests)", n)
	}
	var state, reason string
	var sealed []byte
	if err := s.db.QueryRow("SELECT state,last_error,auth_sealed FROM webhook_subscriptions WHERE id=?", broken).Scan(&state, &reason, &sealed); err != nil || state != "disabled" || reason != WebhookAuthUnreadable || len(sealed) != 0 {
		t.Fatalf("unreadable: %s %q %d %v", state, reason, len(sealed), err)
	}
	for i := 0; i < WebhookDisableFailures; i++ {
		if _, err := s.db.Exec("INSERT INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES(?,?,?,'event','{}',?,?)", randomID(), live, randomID(), testTime, testTime); err != nil {
			t.Fatal(err)
		}
	}
	drainAuth(t, s)
	if err := s.db.QueryRow("SELECT state,auth_sealed FROM webhook_subscriptions WHERE id=?", live).Scan(&state, &sealed); err != nil || state != "disabled" || len(sealed) != 0 {
		t.Fatalf("auto-disabled: %s %d %v", state, len(sealed), err)
	}
	for _, r := range rec.seen() {
		if r.header.Get("Authorization") != testAuthValue {
			t.Fatal("an active event went without its header")
		}
	}
	s.now = func() time.Time { return time.Unix(testTime+WebhookPendingTTL+1, 0) }
	s.expireWebhooks(testContext)
	var names string
	if err := s.db.QueryRow("SELECT state,last_error,auth_sealed,auth_names FROM webhook_subscriptions WHERE id=?", expiring).Scan(&state, &reason, &sealed, &names); err != nil || state != "disabled" || reason != webhookNotConfirmed || len(sealed) != 0 || names != "Authorization" {
		t.Fatalf("expired: %s %q %d %q %v", state, reason, len(sealed), names, err)
	}
}

// Off after headers were stored: the rows wait in the queue, never sent bare.
func TestWebhookAuthFlagOffNeverSendsBare(t *testing.T) {
	s, rec := authStore(t, func(authRequest) (int, string) { return 200, "" })
	owner := keyFor(147)
	run(t, s, signed(owner, Command{Operation: "webhook.create", Data: authCreateData("https://hooks.example.org/off", `{"Authorization":"`+testAuthValue+`"}`)}))
	s.config.Features.WebhookAuthHeaders = false
	if n := drainAuth(t, s); n != 0 || len(rec.seen()) != 0 || queueDepth(t, s) != 1 {
		t.Fatalf("flag off: %d sends, %d requests, %d queued", n, len(rec.seen()), queueDepth(t, s))
	}
	// Even a delivery send is handed directly holds the bare request back.
	d := &webhookDelivery{id: "x", subscription: "", kind: "challenge", state: "pending"}
	if err := s.db.QueryRow("SELECT id,url,secret FROM webhook_subscriptions").Scan(&d.subscription, &d.url, &d.secret); err != nil {
		t.Fatal(err)
	}
	if ok, _, _, reason := s.send(testContext, d, testTime); ok || reason != webhookAuthHeld || len(rec.seen()) != 0 {
		t.Fatalf("send with the flag off: %v %q", ok, reason)
	}
	s.config.Features.WebhookAuthHeaders = true
	if n := drainAuth(t, s); n != 1 || len(rec.seen()) != 1 || rec.seen()[0].header.Get("Authorization") != testAuthValue {
		t.Fatalf("flag on again: %d sends", n)
	}
}

// Confirm-mode without headers: a 2xx never activates; a 5xx is retried and
// shown as for echo; webhook.confirm activates.
func TestWebhookConfirmModeWithoutHeaders(t *testing.T) {
	var calls atomic.Int64
	s, rec := authStore(t, func(authRequest) (int, string) {
		if calls.Add(1) == 1 {
			return 503, ""
		}
		return 200, `{"nonce":"anything"}`
	})
	owner := keyFor(148)
	created := run(t, s, signed(owner, Command{Operation: "webhook.create", Data: `{"schema":1,"url":"https://hooks.example.org/2xx","activation":"confirm"}`}))
	id := created.Data["subscription_id"].(string)
	if created.Data["activation"] != "confirm" || fmt.Sprint(created.Data["headers"]) != "[]" {
		t.Fatalf("create: %v", created.Data)
	}
	drainAuth(t, s)
	if _, _, reason := subscriptionState(t, s, id); !strings.Contains(reason, "challenge attempt 1 failed, will retry: status 503") {
		t.Fatalf("a headerless confirm challenge hides its failure: %q", reason)
	}
	driveChallenge(t, s)
	state, _, reason := subscriptionState(t, s, id)
	if state != "pending" || reason != "" || len(rec.seen()) != 2 {
		t.Fatalf("a 2xx activated or left an error: %s %q %d", state, reason, len(rec.seen()))
	}
	nonce := challengeNonce(t, rec.seen()[1])
	run(t, s, signed(owner, Command{Operation: "webhook.confirm", Data: `{"schema":1,"subscription_id":"` + id + `","nonce":"` + nonce + `"}`, Timestamp: s.now().Unix()}))
	if state, _, _ = subscriptionState(t, s, id); state != "active" {
		t.Fatalf("confirm left %s", state)
	}
}

// The per-host budget: header challenges are bounded per host per hour and
// per agent per day at create (429 webhook_host_busy, nothing charged); every
// request is bounded per host per hour at claim, where an over-budget row
// waits for the next hour and the claim moves on.
func TestWebhookHostBudget(t *testing.T) {
	s, rec := authStore(t, func(authRequest) (int, string) { return 200, "" })
	owner := keyFor(149)
	hour := testTime / 3600
	createAuth := func(key ed25519.PrivateKey, url string) error {
		_, err := s.Execute(testContext, signed(key, Command{Operation: "webhook.create", Data: authCreateData(url, `{"Authorization":"x"}`)}), "test-origin")
		return err
	}
	// Per agent: 6 a day, creates and deletes in between.
	for i := 0; i < WebhookAuthChallengesPerDay; i++ {
		if err := createAuth(owner, fmt.Sprintf("https://h%d.example.org/a", i)); err != nil {
			t.Fatal(err)
		}
		var id string
		_ = s.db.QueryRow("SELECT id FROM webhook_subscriptions WHERE state='pending' LIMIT 1").Scan(&id)
		run(t, s, signed(owner, Command{Operation: "webhook.delete", Target: id}))
	}
	if err := createAuth(owner, "https://h9.example.org/a"); errCode(err) != "webhook_host_busy" {
		t.Fatalf("seventh header challenge in a day: %v", err)
	}
	var e *Error
	if err := createAuth(owner, "https://h9.example.org/a"); !errors.As(err, &e) || e.Status != 429 {
		t.Fatalf("busy status: %v", err)
	}
	// A plain create is not counted.
	run(t, s, signed(owner, Command{Operation: "webhook.create", Data: `{"schema":1,"url":"https://h9.example.org/plain"}`}))
	// Per host: 30 an hour, whoever creates; subdomains are one host.
	if _, err := s.db.Exec("INSERT INTO webhook_host_rates(host,hour,auth_challenges) VALUES('example.net',?,?)", hour, WebhookHostAuthChallengesPerHour-1); err != nil {
		t.Fatal(err)
	}
	fresh := keyFor(150)
	if err := createAuth(fresh, "https://a.example.net/x"); err != nil {
		t.Fatal(err)
	}
	if err := createAuth(fresh, "https://b.example.net/x"); errCode(err) != "webhook_host_busy" {
		t.Fatalf("31st header challenge to a host: %v", err)
	}
	for _, c := range [][2]string{{"https://a.b.example.co.uk/x", "example.co.uk"}, {"https://x.vercel.app/y", "x.vercel.app"}, {"https://1.2.3.4/y", "1.2.3.4"}, {"https://[2001:db8:1:2:3:4:5:6]/y", "2001:db8:1:2::/64"}, {"https://HOOKS.Example.ORG./z", "example.org"}} {
		if got := webhookHost(c[0]); got != c[1] {
			t.Fatalf("webhookHost(%s) = %s, want %s", c[0], got, c[1])
		}
	}
	// Claim, with WEBHOOK_HOST_BUDGET on: a host over 1200 an hour waits;
	// the next host's row is sent.
	s.config.Features.WebhookHostBudget = true
	if _, err := s.db.Exec("DELETE FROM webhook_deliveries"); err != nil {
		t.Fatal(err)
	}
	full, _ := activeWebhook(t, s, "acct-full", "https://hooks.full-host.org/x")
	open, _ := activeWebhook(t, s, "acct-open", "https://hooks.open-host.org/x")
	if _, err := s.db.Exec("INSERT INTO webhook_host_rates(host,hour,requests) VALUES('full-host.org',?,?)", hour, WebhookHostRequestsPerHour); err != nil {
		t.Fatal(err)
	}
	for i, sub := range []string{full, full, open} {
		if _, err := s.db.Exec("INSERT INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at,attempts) VALUES(?,?,?,'event','{}',?,?,2)", randomID(), sub, randomID(), testTime+int64(i), testTime); err != nil {
			t.Fatal(err)
		}
	}
	s.now = func() time.Time { return time.Unix(testTime+10, 0) }
	before := len(rec.seen())
	if n := drainAuth(t, s); n != 1 || len(rec.seen()) != before+1 {
		t.Fatalf("claim sent %d", n)
	}
	rows, err := s.db.Query("SELECT next_at,attempts,leased_until FROM webhook_deliveries WHERE subscription=?", full)
	if err != nil {
		t.Fatal(err)
	}
	deferred := 0
	for rows.Next() {
		var next, attempts, leased int64
		if err = rows.Scan(&next, &attempts, &leased); err != nil {
			t.Fatal(err)
		}
		if next < (hour+1)*3600 || next > (hour+1)*3600+webhookHostJitter || attempts != 2 || leased != 0 {
			t.Fatalf("deferred row: next %d (hour starts %d) attempts %d leased %d", next, (hour+1)*3600, attempts, leased)
		}
		deferred++
	}
	rows.Close()
	var counted int64
	if err = s.db.QueryRow("SELECT requests FROM webhook_host_rates WHERE host='open-host.org' AND hour=?", hour).Scan(&counted); err != nil || deferred != 2 || counted != 1 {
		t.Fatalf("deferred %d, open-host.org counted %d (%v)", deferred, counted, err)
	}
	// The next hour sends them, counted afresh.
	s.now = func() time.Time { return time.Unix((hour+1)*3600+webhookHostJitter, 0) }
	if n := drainAuth(t, s); n != 2 {
		t.Fatalf("next hour sent %d", n)
	}
	// More than webhookClaimTries over-budget rows ahead of a sendable one:
	// the claim gives its turn up, and a later one gets there.
	if _, err = s.db.Exec("UPDATE webhook_host_rates SET requests=? WHERE host='full-host.org'", WebhookHostRequestsPerHour); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO webhook_host_rates(host,hour,requests) VALUES('full-host.org',?,?) ON CONFLICT(host,hour) DO UPDATE SET requests=excluded.requests", hour+1, WebhookHostRequestsPerHour); err != nil {
		t.Fatal(err)
	}
	now := s.now().Unix()
	for i := 0; i < webhookClaimTries+2; i++ {
		if _, err = s.db.Exec("INSERT INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES(?,?,?,'event','{}',?,?)", randomID(), full, randomID(), now-100+int64(i), now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.Exec("INSERT INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES(?,?,?,'event','{}',?,?)", randomID(), open, randomID(), now, now); err != nil {
		t.Fatal(err)
	}
	before = len(rec.seen())
	for i := 0; i < 3 && len(rec.seen()) == before; i++ {
		if _, err = s.deliverOnce(testContext); err != nil {
			t.Fatal(err)
		}
	}
	if len(rec.seen()) != before+1 {
		t.Fatal("a busy host blocked the queue")
	}
}
