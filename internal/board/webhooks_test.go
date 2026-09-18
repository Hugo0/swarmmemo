package board

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
)

const testWebhookData = `{"schema":1,"url":"https://hooks.example.org/swarmmemo"}`

// activeWebhook installs a confirmed subscription directly. Delivery tests point
// it at a loopback test server, which webhook.create would correctly refuse, so
// the storage row is written here rather than through the public operation.
func activeWebhook(t *testing.T, s *Store, account, url string) (string, string) {
	t.Helper()
	id, secret := randomID(), webhookSecret()
	_, err := s.db.Exec("INSERT INTO webhook_subscriptions(id,account,created_by,url,secret,state,created_at,confirmed_at) VALUES(?,?,?,?,?,'active',?,?)",
		id, account, account, url, secret, testTime, testTime)
	if err != nil {
		t.Fatal(err)
	}
	return id, secret
}

func webhookRows(t *testing.T, s *Store) []map[string]string {
	t.Helper()
	rows, err := s.db.Query("SELECT id,subscription,event_id,kind,body,attempts,next_at FROM webhook_deliveries ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var id, subscription, event, kind, body string
		var attempts, next int64
		if err = rows.Scan(&id, &subscription, &event, &kind, &body, &attempts, &next); err != nil {
			t.Fatal(err)
		}
		out = append(out, map[string]string{"id": id, "subscription": subscription, "event": event, "kind": kind, "body": body})
	}
	return out
}

// Every address family and special-use range the sender must refuse. This runs
// against the same predicate the dialer uses, so a pass here is a pass on the
// connection path, not only on the parser.
func TestWebhookRefusesNonPublicAddressRanges(t *testing.T) {
	blocked := []string{
		"0.0.0.0", "0.0.0.1", "127.0.0.1", "127.1.2.3", "10.0.0.7", "10.255.255.255",
		"172.16.0.1", "172.31.255.254", "192.168.1.1", "169.254.169.254", "169.254.0.1",
		"100.64.0.1", "100.127.255.255", "192.0.0.1", "192.0.2.5", "198.51.100.5", "203.0.113.5",
		"192.88.99.1", "198.18.0.1", "198.19.255.255", "224.0.0.1", "239.255.255.255",
		"240.0.0.1", "255.255.255.255",
		"::", "::1", "fc00::1", "fd00:1234::1", "fe80::1", "ff02::1", "ff01::1",
		"2001:db8::1", "2002:7f00:1::1", "2001::1", "64:ff9b::7f00:1", "100::1",
		// IPv4-mapped forms of blocked v4 addresses must not survive the unwrap.
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", "::ffff:192.168.0.1",
	}
	for _, raw := range blocked {
		ip := net.ParseIP(raw)
		if ip == nil {
			t.Fatalf("test address does not parse: %s", raw)
		}
		if err := publicWebhookIP(ip); err == nil {
			t.Fatalf("address accepted but must be refused: %s", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700::1111", "2a00:1450:4001::200e"} {
		if err := publicWebhookIP(net.ParseIP(raw)); err != nil {
			t.Fatalf("public address refused: %s: %v", raw, err)
		}
	}
	if err := publicWebhookIP(nil); err == nil {
		t.Fatal("a missing address must not be treated as public")
	}
}

func TestWebhookURLShapeIsRestrictive(t *testing.T) {
	for _, raw := range []string{
		"", "example.org", "http://example.org/hook", "ftp://example.org",
		"https://user:pass@example.org/hook", "https://example.org/hook#fragment",
		"https://example.org:8443/hook", "https://example.org:80/hook", "https:///hook",
		"https://example.org/hook\nX-Injected: 1", "https://example.org/ hook",
		"https://" + strings.Repeat("a", WebhookMaxURLBytes) + ".example.org/hook",
		"file:///etc/passwd", "gopher://example.org/",
	} {
		if _, err := parseWebhookURL(raw); err == nil {
			t.Fatalf("URL accepted but must be refused: %q", raw)
		}
	}
	for _, raw := range []string{"https://example.org", "https://example.org/hook?x=1", "https://example.org:443/hook"} {
		if _, err := parseWebhookURL(raw); err != nil {
			t.Fatalf("plain public https URL refused: %q: %v", raw, err)
		}
	}
}

func TestWebhookDialerRefusesLoopbackAtConnectionTime(t *testing.T) {
	s := openTest(t, Config{})
	if _, err := s.webhookDial(testContext, "tcp", "127.0.0.1:443"); err == nil {
		t.Fatal("the dialer connected to loopback")
	}
	// The filter lives on the dial, not on the URL, so an address that only turns
	// private after creation is still refused at delivery time.
	if _, err := s.webhookDial(testContext, "tcp", "[::1]:443"); err == nil {
		t.Fatal("the dialer connected to IPv6 loopback")
	}
}

func TestWebhookOperationsRequireASignedKeyAndExactFields(t *testing.T) {
	s := openTest(t, Config{})
	s.webhookInsecure = true
	key := keyFor(91)
	for _, anonymous := range []Command{
		{Operation: "webhook.create", Data: testWebhookData},
		{Operation: "webhook.delete", Target: randomID()},
		{Operation: "webhook.list"},
	} {
		fails(t, s, anonymous, "signature_required")
	}
	fails(t, s, signed(key, Command{Operation: "webhook.create", Data: testWebhookData, Room: "lobby"}), "unexpected_field")
	fails(t, s, signed(key, Command{Operation: "webhook.delete", Target: randomID(), Data: testWebhookData}), "unexpected_field")
	for _, raw := range []string{
		`null`, `{}`, `[]`, testWebhookData + `{}`,
		`{"schema":2,"url":"https://hooks.example.org/x"}`,
		`{"schema":1,"url":"https://hooks.example.org/x","secret":"mine"}`,
		`{"schema":1,"url":"https://hooks.example.org/x","url":"https://hooks.example.org/y"}`,
		`{"schema":1}`, `{"url":"https://hooks.example.org/x"}`,
		`{"schema":1,"url":null}`,
	} {
		fails(t, s, signed(key, Command{Operation: "webhook.create", Data: raw}), "invalid_webhook")
	}
	fails(t, s, signed(key, Command{Operation: "webhook.create", Data: `{"schema":1,"url":"http://hooks.example.org/x"}`}), "invalid_webhook")
	for _, raw := range []string{
		`{"schema":1,"url":"https://10.0.0.1/x"}`,
		`{"schema":1,"url":"https://127.0.0.1/x"}`,
		`{"schema":1,"url":"https://[::1]/x"}`,
		`{"schema":1,"url":"https://169.254.169.254/latest/meta-data/"}`,
	} {
		fails(t, s, signed(key, Command{Operation: "webhook.create", Data: raw}), "webhook_address_blocked")
	}
	fails(t, s, signed(key, Command{Operation: "webhook.delete", Target: "not-an-id"}), "webhook_not_found")
	fails(t, s, signed(key, Command{Operation: "webhook.delete", Target: randomID()}), "webhook_not_found")
}

func TestWebhookCapsSubscriptionsAndNeverListsTheSecret(t *testing.T) {
	s := openTest(t, Config{})
	s.webhookInsecure = true
	key := keyFor(92)
	created := []string{}
	for i := 0; i < WebhookMaxPerAccount; i++ {
		data := `{"schema":1,"url":"https://hooks.example.org/` + string(rune('a'+i)) + `"}`
		result := run(t, s, signed(key, Command{Operation: "webhook.create", Data: data}))
		secret, _ := result.Data["secret"].(string)
		if len(secret) < 32 {
			t.Fatal("create must return a usable secret exactly once")
		}
		if result.Data["state"] != "pending" {
			t.Fatal("a new subscription must start pending")
		}
		created = append(created, result.Data["subscription_id"].(string))
	}
	fails(t, s, signed(key, Command{Operation: "webhook.create", Data: `{"schema":1,"url":"https://hooks.example.org/z"}`}), "webhook_limit")
	fails(t, s, signed(key, Command{Operation: "webhook.create", Data: `{"schema":1,"url":"https://hooks.example.org/a"}`}), "webhook_limit")

	listed := run(t, s, signed(key, Command{Operation: "webhook.list"}))
	encoded, err := json.Marshal(listed.Data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret\":\"") {
		t.Fatalf("webhook.list disclosed a secret: %s", encoded)
	}
	subscriptions, _ := listed.Data["subscriptions"].([]map[string]any)
	if len(subscriptions) != WebhookMaxPerAccount {
		t.Fatalf("listed %d subscriptions", len(subscriptions))
	}
	// Each creation queued exactly one challenge and nothing else.
	queued := webhookRows(t, s)
	if len(queued) != WebhookMaxPerAccount {
		t.Fatalf("queued %d deliveries for %d creations", len(queued), WebhookMaxPerAccount)
	}
	for _, row := range queued {
		if row["kind"] != "challenge" {
			t.Fatalf("a pending subscription queued a %s delivery", row["kind"])
		}
	}
	run(t, s, signed(key, Command{Operation: "webhook.delete", Target: created[0]}))
	fails(t, s, signed(key, Command{Operation: "webhook.delete", Target: created[0]}), "webhook_not_found")
	if rows := webhookRows(t, s); len(rows) != WebhookMaxPerAccount-1 {
		t.Fatal("deleting a subscription must drop what was queued for it")
	}
	// Another key cannot delete this account's subscription.
	other := keyFor(93)
	register(t, s, other)
	fails(t, s, signed(other, Command{Operation: "webhook.delete", Target: created[1]}), "webhook_not_found")
	run(t, s, signed(key, Command{Operation: "webhook.create", Data: `{"schema":1,"url":"https://hooks.example.org/z"}`}))
}

func TestWebhookCreationSpendsAllowance(t *testing.T) {
	s := openTest(t, Config{DailyBytes: 1024})
	s.webhookInsecure = true
	key := keyFor(94)
	run(t, s, signed(key, Command{Operation: "webhook.create", Data: testWebhookData}))
	fails(t, s, signed(key, Command{Operation: "webhook.create", Data: `{"schema":1,"url":"https://hooks.example.org/b"}`}), "quota_exhausted")
}

// The three reasons must match what updates.get would have returned to the same
// key, and the body must never carry text.
func TestWebhookEnqueueFollowsUpdatesScopeAndCarriesNoText(t *testing.T) {
	s := openTest(t, Config{})
	subscriber, poster := keyFor(95), keyFor(96)
	register(t, s, subscriber)
	register(t, s, poster)
	mine := run(t, s, signed(subscriber, Command{Operation: "post", Room: "lobby", Page: "main", Text: "seed"})).Receipt.ID
	id, _ := activeWebhook(t, s, keyID(subscriber), "https://hooks.example.org/x")

	run(t, s, signed(poster, Command{Operation: "post", Room: "lobby", Page: "main", Text: "a reply", ReplyTo: mine}))
	run(t, s, signed(poster, Command{Operation: "post", Room: "lobby", Page: "main", Text: "mail for you", To: keyID(subscriber)}))
	run(t, s, signed(poster, Command{Operation: "post", Room: "lobby", Page: "main", Text: "just talking"}))
	// The subscriber's own post is not news to its author.
	run(t, s, signed(subscriber, Command{Operation: "post", Room: "lobby", Page: "main", Text: "mine again"}))
	// A room the subscriber has never posted in concerns nobody here.
	run(t, s, signed(poster, Command{Operation: "post", Room: "elsewhere", Page: "main", Text: "unrelated"}))

	rows := webhookRows(t, s)
	if len(rows) != 3 {
		t.Fatalf("queued %d deliveries, want 3", len(rows))
	}
	reasons := []string{}
	for _, row := range rows {
		if row["subscription"] != id {
			t.Fatal("delivery queued for the wrong subscription")
		}
		var body struct {
			Schema   int    `json:"schema"`
			Delivery string `json:"delivery_id"`
			Type     string `json:"type"`
			Reason   string `json:"reason"`
			Event    struct {
				ID, Room, Visibility string
			} `json:"event"`
		}
		if err := json.Unmarshal([]byte(row["body"]), &body); err != nil {
			t.Fatal(err)
		}
		if body.Schema != 1 || body.Type != "event" || body.Delivery != row["id"] {
			t.Fatalf("delivery body does not match its row: %s", row["body"])
		}
		for _, leaked := range []string{"a reply", "mail for you", "just talking", "text"} {
			if strings.Contains(row["body"], leaked) {
				t.Fatalf("delivery body carries message content: %s", row["body"])
			}
		}
		reasons = append(reasons, body.Reason)
	}
	want := map[string]bool{"reply": true, "addressed": true, "room_activity": true}
	for _, reason := range reasons {
		if !want[reason] {
			t.Fatalf("unexpected or repeated reason %q in %v", reason, reasons)
		}
		delete(want, reason)
	}
	if len(want) != 0 {
		t.Fatalf("missing reasons %v (got %v)", want, reasons)
	}
}

func TestWebhookPrivateRoomNeedsCurrentMembership(t *testing.T) {
	s := openTest(t, Config{})
	owner, member := keyFor(97), keyFor(98)
	register(t, s, owner)
	register(t, s, member)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "vault", Visibility: "private", Members: []string{keyID(member)}}))
	run(t, s, signed(member, Command{Operation: "post", Room: "vault", Page: "main", Text: "member seed"}))
	activeWebhook(t, s, keyID(member), "https://hooks.example.org/x")

	run(t, s, signed(owner, Command{Operation: "post", Room: "vault", Page: "main", Text: "while a member"}))
	if len(webhookRows(t, s)) != 1 {
		t.Fatal("a current member must be notified of private room activity")
	}
	run(t, s, signed(owner, Command{Operation: "room.member.remove", Room: "vault", Target: keyID(member)}))
	run(t, s, signed(owner, Command{Operation: "post", Room: "vault", Page: "main", Text: "after removal"}))
	// The removed member still has old posts in the room; they must not keep
	// producing notifications about content it can no longer read.
	if rows := webhookRows(t, s); len(rows) != 1 {
		t.Fatalf("a removed member was notified: %d deliveries", len(rows))
	}
}

func TestWebhookHourlyDeliveryCeiling(t *testing.T) {
	s := openTest(t, Config{})
	subscriber, poster := keyFor(99), keyFor(100)
	register(t, s, subscriber)
	register(t, s, poster)
	run(t, s, signed(subscriber, Command{Operation: "post", Room: "lobby", Page: "main", Text: "seed"}))
	activeWebhook(t, s, keyID(subscriber), "https://hooks.example.org/x")
	if _, err := s.db.Exec("INSERT INTO webhook_rates(account,hour,count) VALUES(?,?,?)", keyID(subscriber), testTime/3600, int64(WebhookMaxDeliveriesHour)); err != nil {
		t.Fatal(err)
	}
	run(t, s, signed(poster, Command{Operation: "post", Room: "lobby", Page: "main", Text: "over the ceiling"}))
	if rows := webhookRows(t, s); len(rows) != 0 {
		t.Fatalf("the hourly ceiling did not stop enqueue: %d deliveries", len(rows))
	}
	// The post itself still succeeded, and the event is still in updates.get.
	updates := run(t, s, Command{Operation: "updates.get", Target: keyID(subscriber)})
	if len(updates.Messages) == 0 {
		t.Fatal("a dropped notification must not remove the event from the return read")
	}
}

func TestWebhookOnlyOneDeliveryPerEventPerSubscription(t *testing.T) {
	s := openTest(t, Config{})
	subscriber, poster := keyFor(101), keyFor(102)
	register(t, s, subscriber)
	register(t, s, poster)
	mine := run(t, s, signed(subscriber, Command{Operation: "post", Room: "lobby", Page: "main", Text: "seed"})).Receipt.ID
	activeWebhook(t, s, keyID(subscriber), "https://hooks.example.org/x")
	// A reply, addressed to the same account, in a room it posts in: three reasons,
	// one event, and still exactly one delivery.
	run(t, s, signed(poster, Command{Operation: "post", Room: "lobby", Page: "main", Text: "all three", ReplyTo: mine, To: keyID(subscriber)}))
	if rows := webhookRows(t, s); len(rows) != 1 {
		t.Fatalf("one event produced %d deliveries", len(rows))
	}
}

