package board

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"swarmmemo/internal/b64"
	"swarmmemo/internal/services"
)

// decryptWebPush is the browser's half of RFC 8291: what a user agent holding
// uaPrivate and authSecret reads from an aes128gcm body. The board never
// needs it; tests check the round trip with it.
func decryptWebPush(body []byte, uaPrivate *ecdh.PrivateKey, authSecret []byte) ([]byte, error) {
	if len(body) < 21 {
		return nil, errors.New("web push: body too short")
	}
	salt, idlen := body[:16], int(body[20])
	if binary.BigEndian.Uint32(body[16:20]) < 18 || len(body) < 21+idlen+17 {
		return nil, errors.New("web push: malformed header")
	}
	asPublic := body[21 : 21+idlen]
	as, err := ecdh.P256().NewPublicKey(asPublic)
	if err != nil {
		return nil, err
	}
	shared, err := uaPrivate.ECDH(as)
	if err != nil {
		return nil, err
	}
	cek, nonce, err := webPushKeys(shared, authSecret, salt, uaPrivate.PublicKey().Bytes(), asPublic)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		return nil, err
	}
	end := bytes.LastIndexByte(plain, 0x02)
	if end < 0 || len(bytes.Trim(plain[end+1:], "\x00")) != 0 {
		return nil, errors.New("web push: no last-record delimiter")
	}
	return plain[:end], nil
}

// verifyVAPID checks an Authorization header against the server's public key
// (base64url) and the audience it must name; tests use it as a push service
// would.
func verifyVAPID(header, public, audience string, now int64) error {
	rest, ok := strings.CutPrefix(header, "vapid t=")
	if !ok {
		return errors.New("not a vapid header")
	}
	token, k, ok := strings.Cut(rest, ", k=")
	if !ok || k != public {
		return errors.New("vapid: wrong or missing k")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("vapid: not a JWT")
	}
	raw, err := b64.URL(public)
	if err != nil {
		return err
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
	if err != nil {
		return err
	}
	sig, err := b64.URLSize(parts[2], 64)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return errors.New("vapid: bad signature")
	}
	claimsRaw, err := b64.URL(parts[1])
	if err != nil {
		return err
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	if err = json.Unmarshal(claimsRaw, &claims); err != nil {
		return err
	}
	if claims.Aud != audience || claims.Exp <= now || claims.Exp > now+24*3600 || claims.Sub == "" {
		return fmt.Errorf("vapid: claims %+v", claims)
	}
	return nil
}

// RFC 8291 Appendix A: the worked example, byte for byte.
func TestWebPushEncryptionMatchesRFC8291(t *testing.T) {
	u := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	curve := ecdh.P256()
	asPrivate, err := curve.NewPrivateKey(u("yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	uaPrivate, err := curve.NewPrivateKey(u("q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"))
	if err != nil {
		t.Fatal(err)
	}
	if got := base64.RawURLEncoding.EncodeToString(uaPrivate.PublicKey().Bytes()); got != "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4" {
		t.Fatalf("ua_public %s", got)
	}
	auth, salt := u("BTBZMqHH6r4Tts7J_aSIgg"), u("DGv6ra1nlYgDCS1FRnbzlw")
	plaintext := []byte("When I grow up, I want to be a watermelon")
	body, err := sealWebPush(plaintext, uaPrivate.PublicKey(), asPrivate, auth, salt)
	if err != nil {
		t.Fatal(err)
	}
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if got := base64.RawURLEncoding.EncodeToString(body); got != want {
		t.Fatalf("aes128gcm body\n got %s\nwant %s", got, want)
	}
	got, err := decryptWebPush(body, uaPrivate, auth)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("decrypt: %q %v", got, err)
	}
	// Any other auth secret or browser key reads nothing.
	if _, err = decryptWebPush(body, uaPrivate, make([]byte, 16)); err == nil {
		t.Fatal("decrypted with the wrong auth secret")
	}
	other, _ := curve.GenerateKey(rand.Reader)
	if _, err = decryptWebPush(body, other, auth); err == nil {
		t.Fatal("decrypted with the wrong browser key")
	}
}

// browser is a test user agent: its push keys and the subscription data it
// registers.
type browser struct {
	private *ecdh.PrivateKey
	auth    []byte
}

func newBrowser(t *testing.T) browser {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return browser{k, auth}
}

func (b browser) data(endpoint string, extra string) string {
	return fmt.Sprintf(`{"schema":1,"endpoint":%q,"p256dh":%q,"auth":%q%s}`, endpoint,
		base64.RawURLEncoding.EncodeToString(b.private.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(b.auth), extra)
}

// pushStore is a store with web push on: a generated VAPID key (tests only),
// inbox entries read, push.test an accepted push service, and a sender that
// may dial the loopback test server.
func pushStore(t testing.TB) *Store {
	t.Helper()
	s := openTest(t, Config{Features: Features{InboxEntries: InboxRead}})
	key, err := generateVAPIDKey()
	if err != nil {
		t.Fatal(err)
	}
	s.webPush.key = key
	s.webPush.hosts = append([]string{"push.test"}, PushServiceHosts...)
	s.webhookInsecure = true
	return s
}

// pushService is an httptest push service: it checks VAPID, decrypts each
// message as the browser would and answers status.
type pushService struct {
	mu     sync.Mutex
	status int
	got    []map[string]any
	header []http.Header
	errs   []error
}

func startPushService(t *testing.T, s *Store, b browser) (*httptest.Server, *pushService) {
	t.Helper()
	ps := &pushService{status: http.StatusCreated}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8192))
		ps.mu.Lock()
		defer ps.mu.Unlock()
		ps.header = append(ps.header, r.Header.Clone())
		if err := verifyVAPID(r.Header.Get("Authorization"), s.webPush.key.public, server.URL, s.now().Unix()); err != nil {
			ps.errs = append(ps.errs, err)
		}
		plain, err := decryptWebPush(body, b.private, b.auth)
		if err != nil {
			ps.errs = append(ps.errs, err)
		} else {
			var m map[string]any
			if err = json.Unmarshal(plain, &m); err != nil {
				ps.errs = append(ps.errs, err)
			}
			ps.got = append(ps.got, m)
		}
		w.WriteHeader(ps.status)
	}))
	t.Cleanup(server.Close)
	return server, ps
}

// subscribeBrowser registers b for key and points its stored endpoint at
// the test server (the endpoint a browser gives is https at a push service).
func subscribeBrowser(t *testing.T, s *Store, key ed25519.PrivateKey, b browser, server string) string {
	t.Helper()
	r := run(t, s, signed(key, Command{Operation: "push.subscribe", RequestID: randomID(), Data: b.data("https://push.test/send/"+randomID(), `,"label":"Test browser"`)}))
	id := r.Data["subscription_id"].(string)
	if _, err := s.db.Exec("UPDATE webhook_subscriptions SET url=? WHERE id=?", server+"/send/"+id, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// pushRow is a browser subscription's state, the deliveries queued for it
// and its last error.
func pushRow(t *testing.T, s *Store, id string) (state string, queued int64, lastError string) {
	t.Helper()
	if err := s.db.QueryRow("SELECT state,(SELECT count(*) FROM webhook_deliveries WHERE subscription=s.id),last_error FROM webhook_subscriptions s WHERE id=?", id).Scan(&state, &queued, &lastError); err != nil {
		t.Fatal(err)
	}
	return
}

// A reply to an opted-in agent reaches its browser's push service through the
// webhook queue and sender, VAPID signed for that service's origin and
// encrypted so only the browser reads it; the payload is the webhook event's
// ids and the unread count, never the reply's text.
func TestWebPushDeliversEncryptedCountsAndIDs(t *testing.T) {
	s := pushStore(t)
	alice, bob := keyFor(231), keyFor(232)
	register(t, s, alice)
	register(t, s, bob)
	b := newBrowser(t)
	server, ps := startPushService(t, s, b)
	id := subscribeBrowser(t, s, alice, b, server.URL)

	root := run(t, s, signed(alice, Command{Operation: "post", Room: "lobby", Text: "root by alice", RequestID: randomID()})).Receipt.ID
	reply := run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", ReplyTo: root, Text: "the secret words of the reply", RequestID: randomID()})).Receipt.ID
	if _, queued, _ := pushRow(t, s, id); queued != 1 {
		t.Fatalf("the reply's entry queued %d deliveries for the browser", queued)
	}
	worked, err := s.deliverOnce(testContext)
	if err != nil || !worked {
		t.Fatalf("no push sent: %v", err)
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if len(ps.errs) > 0 || len(ps.got) != 1 {
		t.Fatalf("push service: %d messages, errors %v", len(ps.got), ps.errs)
	}
	m := ps.got[0]
	entry, _ := m["entry"].(map[string]any)
	event, _ := m["event"].(map[string]any)
	if m["reason"] != "reply" || entry["subject"] != reply || event["id"] != reply || m["unread"] != float64(1) || m["subscription_id"] != id {
		t.Fatalf("payload %v", m)
	}
	raw, _ := json.Marshal(m)
	if strings.Contains(string(raw), "secret words") || strings.Contains(string(raw), "root by alice") {
		t.Fatalf("payload carries message text: %s", raw)
	}
	h := ps.header[0]
	if h.Get("Content-Encoding") != "aes128gcm" || h.Get("TTL") != "86400" || h.Get("Topic") == "" || h.Get("Urgency") != "normal" || h.Get("X-SwarmMemo-Signature") != "" {
		t.Fatalf("headers %v", h)
	}
	if state, queued, _ := pushRow(t, s, id); state != "active" || queued != 0 {
		t.Fatalf("after delivery: %s, %d queued", state, queued)
	}
	if worked, _ = s.deliverOnce(testContext); worked {
		t.Fatal("a second push was sent for one entry")
	}
	// The browser is not a webhook: webhook.list and webhook.delete leave it
	// alone, and room activity is never queued for it.
	listed := run(t, s, signed(alice, Command{Operation: "webhook.list"})).Data["subscriptions"].([]map[string]any)
	if len(listed) != 0 {
		t.Fatalf("webhook.list shows the browser: %v", listed)
	}
	fails(t, s, signed(alice, Command{Operation: "webhook.delete", RequestID: randomID(), Target: id}), "webhook_not_found")
	carol := keyFor(242)
	register(t, s, carol)
	run(t, s, signed(carol, Command{Operation: "post", Room: "lobby", Text: "unrelated", RequestID: randomID()}))
	if _, queued, _ := pushRow(t, s, id); queued != 0 {
		t.Fatal("room activity was queued for a browser")
	}
}

// A push service answering 404 or 410 means the browser dropped the
// subscription: it is disabled at once (gone, kept, never deleted) and sent
// nothing more, until the browser subscribes again.
func TestWebPushUnsubscribesOnGone(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s := pushStore(t)
			alice, bob := keyFor(233), keyFor(234)
			register(t, s, alice)
			register(t, s, bob)
			b := newBrowser(t)
			server, ps := startPushService(t, s, b)
			ps.status = status
			id := subscribeBrowser(t, s, alice, b, server.URL)
			root := run(t, s, signed(alice, Command{Operation: "post", Room: "lobby", Text: "root", RequestID: randomID()})).Receipt.ID
			run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", ReplyTo: root, Text: "reply", RequestID: randomID()}))
			if worked, err := s.deliverOnce(testContext); err != nil || !worked {
				t.Fatalf("no push sent: %v", err)
			}
			state, queued, lastError := pushRow(t, s, id)
			if state != "disabled" || queued != 0 || !strings.Contains(lastError, fmt.Sprint(status)) {
				t.Fatalf("after %d: state %s, %d queued, error %q", status, state, queued, lastError)
			}
			// Later news is not queued for it, and push.list says it is gone.
			run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", ReplyTo: root, Text: "more", RequestID: randomID()}))
			if _, queued, _ = pushRow(t, s, id); queued != 0 {
				t.Fatal("queued for a gone browser")
			}
			list := run(t, s, signed(alice, Command{Operation: "push.list"})).Data["subscriptions"].([]map[string]any)
			if len(list) != 1 || list[0]["state"] != "gone" || !strings.Contains(fmt.Sprint(list[0]["gone_reason"]), "unsubscribed") {
				t.Fatalf("push.list %v", list)
			}
			// The browser subscribing again revives the same row.
			if _, err := s.db.Exec("UPDATE webhook_subscriptions SET url='https://push.test/again' WHERE id=?", id); err != nil {
				t.Fatal(err)
			}
			again := run(t, s, signed(alice, Command{Operation: "push.subscribe", RequestID: randomID(), Data: b.data("https://push.test/again", "")})).Data
			if again["subscription_id"] != id {
				t.Fatalf("re-subscribing a gone browser made a new row: %v", again)
			}
			if state, _, lastError := pushRow(t, s, id); state != "active" || lastError != "" {
				t.Fatalf("revived row: %s %q", state, lastError)
			}
		})
	}
}

// A server error is retried with the webhook backoff; the delivery stays
// queued and the browser active.
func TestWebPushRetriesServerErrors(t *testing.T) {
	s := pushStore(t)
	alice, bob := keyFor(235), keyFor(236)
	register(t, s, alice)
	register(t, s, bob)
	b := newBrowser(t)
	server, ps := startPushService(t, s, b)
	ps.status = http.StatusServiceUnavailable
	id := subscribeBrowser(t, s, alice, b, server.URL)
	root := run(t, s, signed(alice, Command{Operation: "post", Room: "lobby", Text: "root", RequestID: randomID()})).Receipt.ID
	run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", ReplyTo: root, Text: "reply", RequestID: randomID()}))
	if worked, err := s.deliverOnce(testContext); err != nil || !worked {
		t.Fatalf("no push sent: %v", err)
	}
	state, queued, lastError := pushRow(t, s, id)
	if state != "active" || queued != 1 || !strings.Contains(lastError, "503") {
		t.Fatalf("after 503: %s, %d queued, %q", state, queued, lastError)
	}
	if worked, _ := s.deliverOnce(testContext); worked {
		t.Fatal("retried before the backoff")
	}
	ps.mu.Lock()
	ps.status = http.StatusCreated
	ps.mu.Unlock()
	s.now = func() time.Time { return time.Unix(testTime+webhookMaxBackoff, 0) }
	if worked, err := s.deliverOnce(testContext); err != nil || !worked {
		t.Fatalf("no retry: %v", err)
	}
	if _, queued, _ = pushRow(t, s, id); queued != 0 {
		t.Fatal("the retry did not settle")
	}
}

// Started for web push alone (webhook delivery off), the sender sends to
// browsers and leaves webhooks' deliveries queued.
func TestWebPushOnlySenderLeavesWebhooks(t *testing.T) {
	s := pushStore(t)
	alice, bob := keyFor(243), keyFor(244)
	register(t, s, alice)
	register(t, s, bob)
	b := newBrowser(t)
	server, ps := startPushService(t, s, b)
	id := subscribeBrowser(t, s, alice, b, server.URL)
	hook, _ := activeWebhook(t, s, keyID(alice), server.URL+"/hook")
	s.webPushOnly = true
	root := run(t, s, signed(alice, Command{Operation: "post", Room: "lobby", Text: "root", RequestID: randomID()})).Receipt.ID
	run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", ReplyTo: root, Text: "reply", RequestID: randomID()}))
	for i := 0; i < 3; i++ {
		_, _ = s.deliverOnce(testContext)
	}
	if _, queued, _ := pushRow(t, s, id); queued != 0 {
		t.Fatal("the browser's push was not sent")
	}
	if _, queued, _ := pushRow(t, s, hook); queued != 1 {
		t.Fatalf("the webhook's delivery was sent or dropped by a push-only sender (%d queued)", queued)
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if len(ps.got) != 1 {
		t.Fatalf("push service saw %d", len(ps.got))
	}
}

// Without a VAPID key web push is off, said plainly; nothing is queued.
func TestWebPushOffWithoutKey(t *testing.T) {
	s := openTest(t, Config{Features: Features{InboxEntries: InboxRead}})
	alice := keyFor(237)
	register(t, s, alice)
	fails(t, s, signed(alice, Command{Operation: "push.subscribe", RequestID: randomID(), Data: newBrowser(t).data("https://fcm.googleapis.com/fcm/send/x", "")}), "push_unavailable")
	data := run(t, s, signed(alice, Command{Operation: "push.list"})).Data
	if data["enabled"] != false || !strings.Contains(data["reason"].(string), "no web push key") || data["public_key"] != nil {
		t.Fatalf("push.list while off: %v", data)
	}
	if s.WebPushEnabled() {
		t.Fatal("enabled without a key")
	}
}

func TestWebPushOperationsCheckTheirInput(t *testing.T) {
	s := pushStore(t)
	alice, bob := keyFor(238), keyFor(239)
	register(t, s, alice)
	register(t, s, bob)
	b := newBrowser(t)
	for _, anonymous := range []Command{
		{Operation: "push.subscribe", Data: b.data("https://fcm.googleapis.com/fcm/send/x", "")},
		{Operation: "push.unsubscribe", Target: randomID()},
		{Operation: "push.list"},
	} {
		fails(t, s, anonymous, "signature_required")
	}
	good := "https://fcm.googleapis.com/fcm/send/abc"
	for _, raw := range []string{
		``, `null`, `{}`, `[]`,
		b.data("http://fcm.googleapis.com/fcm/send/x", ""),
		b.data("https://evil.example/fcm/send/x", ""),
		b.data("https://fcm.googleapis.com.evil.example/x", ""),
		b.data("https://fcm.googleapis.com:8443/x", ""),
		b.data("https://user@fcm.googleapis.com/x", ""),
		b.data("https://127.0.0.1/x", ""),
		b.data("https://"+strings.Repeat("a", PushEndpointBytes)+".push.apple.com/x", ""),
		b.data(good, `,"extra":1`),
		b.data(good, `,"label":"`+strings.Repeat("x", PushLabelChars+1)+`"`),
		b.data(good, `,"kinds":[]`),
		b.data(good, `,"kinds":["reply","reply"]`),
		b.data(good, `,"kinds":["room_activity"]`),
		strings.Replace(b.data(good, ""), `"schema":1`, `"schema":2`, 1),
		fmt.Sprintf(`{"schema":1,"endpoint":%q,"p256dh":%q,"auth":%q}`, good, base64.RawURLEncoding.EncodeToString(append([]byte{4}, make([]byte, 64)...)), base64.RawURLEncoding.EncodeToString(b.auth)),
		fmt.Sprintf(`{"schema":1,"endpoint":%q,"p256dh":%q,"auth":"short"}`, good, base64.RawURLEncoding.EncodeToString(b.private.PublicKey().Bytes())),
	} {
		fails(t, s, signed(alice, Command{Operation: "push.subscribe", RequestID: randomID(), Data: raw}), "invalid_push_subscription")
	}
	for _, host := range []string{"fcm.googleapis.com", "updates.push.services.mozilla.com", "web.push.apple.com", "wns2-par02p.notify.windows.com"} {
		r := run(t, s, signed(alice, Command{Operation: "push.subscribe", RequestID: randomID(), Data: b.data("https://"+host+"/x/"+randomID(), `,"kinds":["wakeup","reply"]`)}))
		if k := r.Data["kinds"].([]string); len(k) != 2 || k[0] != "reply" {
			t.Fatalf("%s kinds %v", host, k)
		}
	}
	// The same browser again is the same subscription.
	first := run(t, s, signed(alice, Command{Operation: "push.subscribe", RequestID: randomID(), Data: b.data(good, "")})).Data["subscription_id"]
	again := run(t, s, signed(alice, Command{Operation: "push.subscribe", RequestID: randomID(), Data: b.data(good, `,"label":"renamed"`)})).Data["subscription_id"]
	if first != again {
		t.Fatal("re-subscribing one endpoint made a second subscription")
	}
	for i := 5; i < PushMaxPerAccount; i++ {
		run(t, s, signed(alice, Command{Operation: "push.subscribe", RequestID: randomID(), Data: b.data(good+fmt.Sprint(i), "")}))
	}
	fails(t, s, signed(alice, Command{Operation: "push.subscribe", RequestID: randomID(), Data: b.data(good+"over", "")}), "push_limit")
	// push.list never shows an endpoint; it gives the key browsers need.
	data := run(t, s, signed(alice, Command{Operation: "push.list"})).Data
	raw, _ := json.Marshal(data)
	if strings.Contains(string(raw), "/fcm/send/") || data["public_key"] != s.webPush.key.public || data["enabled"] != true {
		t.Fatalf("push.list %s", raw)
	}
	// Another account cannot remove alice's; alice can.
	fails(t, s, signed(bob, Command{Operation: "push.unsubscribe", RequestID: randomID(), Target: first.(string)}), "push_subscription_not_found")
	fails(t, s, signed(alice, Command{Operation: "push.unsubscribe", RequestID: randomID(), Target: "nope"}), "push_subscription_not_found")
	run(t, s, signed(alice, Command{Operation: "push.unsubscribe", RequestID: randomID(), Target: first.(string)}))
	if n := sqlCount(t, s, "SELECT count(*) FROM webhook_subscriptions WHERE id='"+first.(string)+"'") + sqlCount(t, s, "SELECT count(*) FROM webpush_keys WHERE subscription='"+first.(string)+"'"); n != 0 {
		t.Fatal("unsubscribe kept the row")
	}
	run(t, s, signed(alice, Command{Operation: "push.subscribe", RequestID: randomID(), Data: b.data(good+"over", "")}))
}

// A browser gets only the kinds it asked for: a wakeup-only browser is not
// sent a reply, and is sent a wake-up firing (urgency high).
func TestWebPushFollowsKinds(t *testing.T) {
	s := pushStore(t)
	alice, bob := keyFor(240), keyFor(241)
	register(t, s, alice)
	register(t, s, bob)
	b := newBrowser(t)
	server, ps := startPushService(t, s, b)
	id := subscribeBrowser(t, s, alice, b, server.URL)
	if _, err := s.db.Exec("UPDATE webhook_subscriptions SET kinds='wakeup' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	root := run(t, s, signed(alice, Command{Operation: "post", Room: "lobby", Text: "root", RequestID: randomID()})).Receipt.ID
	run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", ReplyTo: root, Text: "reply", RequestID: randomID()}))
	if _, queued, _ := pushRow(t, s, id); queued != 0 {
		t.Fatal("a wakeup-only browser was queued a reply")
	}
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	wake := services.WakeInboxEntry(randomID(), keyID(alice), "time", "", "", 1, testTime, false)
	if err = (serviceBoardView{s}).AddInboxEntry(testContext, tx, wake); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if worked, err := s.deliverOnce(testContext); err != nil || !worked {
		t.Fatalf("the wake-up was not pushed: %v", err)
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if len(ps.got) != 1 || ps.got[0]["reason"] != "wakeup" || ps.header[0].Get("Urgency") != "high" {
		t.Fatalf("wake-up push: %v %v", ps.got, ps.errs)
	}
}

func TestLoadVAPIDKeyNeedsAPrivateFile(t *testing.T) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	scalar, _ := k.Bytes()
	dir := t.TempDir()
	path := filepath.Join(dir, "vapid.key")
	if err = os.WriteFile(path, []byte("# vapid\n"+base64.RawURLEncoding.EncodeToString(scalar)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = loadVAPIDKey(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("a world-readable key loaded: %v", err)
	}
	if err = os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := loadVAPIDKey(path)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := k.PublicKey.Bytes()
	if key.public != base64.RawURLEncoding.EncodeToString(pub) {
		t.Fatal("public key does not match the private scalar")
	}
	if err = os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = loadVAPIDKey(path); err == nil || strings.Contains(err.Error(), "not a key") {
		t.Fatalf("a bad key loaded, or the error echoes the file: %v", err)
	}
	// Open refuses to start with a bad key rather than run with push off.
	if _, err = Open(filepath.Join(dir, "board.sqlite"), Config{WebPushKeyFile: path}); err == nil {
		t.Fatal("Open took a malformed WEBPUSH_KEY_FILE")
	}
}

var (
	_ = aes.NewCipher
	_ = cipher.NewGCM
	_ = binary.BigEndian
	_ = errors.New
	_ = sha256.Sum256
	_ = big.NewInt
	_ = b64.URL
)

// push.subscribe's parser takes any bytes without panicking, and what it
// accepts is a push service URL with a valid key and secret.
func FuzzPushData(f *testing.F) {
	s := pushStore(f)
	b := browser{}
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	b.private, b.auth = k, make([]byte, 16)
	f.Add(b.data("https://fcm.googleapis.com/fcm/send/x", ""))
	f.Add(b.data("https://web.push.apple.com/x", `,"kinds":["wakeup"],"label":"x"`))
	f.Add(`{"schema":1,"endpoint":"https://push.test/x","p256dh":"","auth":""}`)
	f.Add(`{"schema":1,"schema":1}`)
	f.Fuzz(func(t *testing.T, raw string) {
		d, kinds, err := s.parsePushData(raw)
		if err != nil {
			return
		}
		if s.checkPushEndpoint(d.Endpoint) != nil {
			t.Fatalf("accepted endpoint %q", d.Endpoint)
		}
		if key, e := b64.URLSize(d.P256dh, 65); e != nil {
			t.Fatalf("accepted p256dh %q", d.P256dh)
		} else if _, e = encryptWebPush([]byte("{}"), key, make([]byte, 16)); e != nil {
			t.Fatalf("accepted a key it cannot encrypt to: %v", e)
		}
		for _, k := range webhookKindsList(kinds) {
			if !slicesContains(PushKinds, k) {
				t.Fatalf("accepted kind %q", k)
			}
		}
	})
}

func slicesContains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// samplePushKey is a valid browser p256dh key, for the data spec's probe.
var samplePushKey = func() string {
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes())
}()
