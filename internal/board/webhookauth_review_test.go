package board

import (
	"fmt"
	"testing"
	"time"
)

// Tests for the C1108b adversarial review's fixes (2026-11-20).

// Fix 1: a name RegistrableDomain refuses (an underscore label) still folds
// to its root, so it opens no fresh bucket and passes no own-domain check;
// headers go only to a public DNS name; hosts are ASCII.
func TestWebhookHostKeyFoldsNamesSubjectsRefuse(t *testing.T) {
	for _, c := range [][2]string{
		{"https://a_1.platform.com/x", "platform.com"},
		{"https://a_2.platform.com/x", "platform.com"},
		{"https://x_y.swarmmemo.com/x", "swarmmemo.com"},
		{"https://deep.a_b.c.example.co.uk/x", "example.co.uk"},
		{"https://hooks.full.example/x", "full.example"},
	} {
		if got := webhookHost(c[0]); got != c[1] {
			t.Fatalf("webhookHost(%s) = %s, want %s", c[0], got, c[1])
		}
	}
	s, _ := authStore(t, func(authRequest) (int, string) { return 200, "" })
	if !s.ownWebhookHost("https://x_y.swarmmemo.com/hook") {
		t.Fatal("an underscore subdomain of our own domain is not ours")
	}
	key := keyFor(170)
	for _, target := range []string{"https://x_y.swarmmemo.com/hook", "https://a_1.platform.com/hook", "https://1.2.3.4/hook", "https://[2001:db8::1]/hook", "https://hooks.full.example/hook"} {
		fails(t, s, signed(key, Command{Operation: "webhook.create", Data: authCreateData(target, `{"Authorization":"x"}`)}), "invalid_webhook")
	}
	plain := openTest(t, Config{})
	plain.webhookInsecure = true
	for _, target := range []string{"https://ｅxample.com/h", "https://bücher.example.org/h", "https://b%C3%BCcher.example.org/h", "https://hooks.exаmple.org/h"} {
		if _, err := parseWebhookURL(target); err == nil {
			t.Fatalf("%s: a non-ASCII host was taken", target)
		}
		fails(t, plain, signed(key, Command{Operation: "webhook.create", Data: `{"schema":1,"url":"` + target + `"}`}), "invalid_webhook")
	}
	if _, err := parseWebhookURL("https://xn--bcher-kva.example.org/h"); err != nil {
		t.Fatalf("an xn-- name was refused: %v", err)
	}
}

// Fix 2: a claim that only defers rows still reports work, so the worker
// drains a long run of over-budget rows instead of sleeping a poll per
// eight; a row that waited a day is dropped with a constant reason.
func TestWebhookDeferredRowsKeepTheWorkerDraining(t *testing.T) {
	s, rec := authStore(t, func(authRequest) (int, string) { return 200, "" })
	s.config.Features.WebhookHostBudget = true
	hour := testTime / 3600
	full, _ := activeWebhook(t, s, "acct-full", "https://hooks.busy-host.org/x")
	open, _ := activeWebhook(t, s, "acct-open", "https://hooks.calm-host.org/x")
	if _, err := s.db.Exec("INSERT INTO webhook_host_rates(host,hour,requests) VALUES('busy-host.org',?,?)", hour, WebhookHostRequestsPerHour); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5*webhookClaimTries; i++ {
		if _, err := s.db.Exec("INSERT INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES(?,?,?,'event','{}',?,?)", randomID(), full, randomID(), testTime-100+int64(i), testTime); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec("INSERT INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES(?,?,?,'event','{}',?,?)", randomID(), open, randomID(), testTime, testTime); err != nil {
		t.Fatal(err)
	}
	worked, err := s.deliverOnce(testContext)
	if err != nil || !worked || len(rec.seen()) != 0 {
		t.Fatalf("a claim that deferred %d rows reported worked=%v (%v)", webhookClaimTries, worked, err)
	}
	// The worker's own loop: until a call does nothing, at most WebhookMaxFanout.
	for n := 0; n < WebhookMaxFanout; n++ {
		if worked, err = s.deliverOnce(testContext); err != nil || !worked {
			break
		}
	}
	if len(rec.seen()) != 1 {
		t.Fatalf("one tick of the worker sent %d; the row behind the busy host waited", len(rec.seen()))
	}
	// A day later, still over: dropped, with the one reason.
	later := testTime + webhookHostDeferMaxAge
	s.now = func() time.Time { return time.Unix(later, 0) }
	if _, err = s.db.Exec("INSERT INTO webhook_host_rates(host,hour,requests) VALUES('busy-host.org',?,?)", later/3600, WebhookHostRequestsPerHour); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE webhook_deliveries SET next_at=?", later); err != nil {
		t.Fatal(err)
	}
	drainAuth(t, s)
	if n := queueDepth(t, s); n != 0 {
		t.Fatalf("%d rows still queued after a day over budget", n)
	}
	if _, _, reason := subscriptionState(t, s, full); reason != webhookHostDropped {
		t.Fatalf("dropped with %q", reason)
	}
}

// Fixes 3 and 6: challenges count in their own bucket and never hold back a
// verified delivery; a deferred challenge waits at most until its pending
// window ends; verified deliveries past their bucket are only logged while
// WEBHOOK_HOST_BUDGET is off.
func TestWebhookChallengesHaveTheirOwnBucket(t *testing.T) {
	s, rec := authStore(t, func(authRequest) (int, string) { return 200, "" })
	s.config.Features.WebhookHostBudget = true // the challenge bucket enforced
	hour := testTime / 3600
	active, _ := activeWebhook(t, s, "acct-victim", "https://hooks.victim-host.org/x")
	if _, err := s.db.Exec("INSERT INTO webhook_host_rates(host,hour,requests) VALUES(?,?,?)", webhookChallengeBucket+"victim-host.org", hour, WebhookHostChallengesPerHour); err != nil {
		t.Fatal(err)
	}
	// Early in the hour: the next hour's start plus its jitter may fall past
	// the pending window, which caps the wait.
	now := hour*3600 + 10
	s.now = func() time.Time { return time.Unix(now, 0) }
	created := run(t, s, signed(keyFor(171), Command{Operation: "webhook.create", Data: `{"schema":1,"url":"https://other.victim-host.org/hook"}`, Timestamp: now}))
	pending := created.Data["subscription_id"].(string)
	if _, err := s.db.Exec("INSERT INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES(?,?,?,'event','{}',?,?)", randomID(), active, randomID(), now, now); err != nil {
		t.Fatal(err)
	}
	drainAuth(t, s)
	got := rec.seen()
	if len(got) != 1 || string(got[0].body) != "{}" {
		t.Fatalf("with the challenge bucket full the verified delivery was held: %d requests", len(got))
	}
	var next int64
	if err := s.db.QueryRow("SELECT next_at FROM webhook_deliveries WHERE subscription=?", pending).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next > now+WebhookPendingTTL-1 || next <= now {
		t.Fatalf("the challenge waits until %d, past its window's end %d", next, now+WebhookPendingTTL-1)
	}
	// Verified bucket over its cap: logged and sent while the flag is off.
	s.config.Features.WebhookHostBudget = false
	if _, err := s.db.Exec("INSERT INTO webhook_host_rates(host,hour,requests) VALUES('victim-host.org',?,?) ON CONFLICT(host,hour) DO UPDATE SET requests=excluded.requests", hour, WebhookHostRequestsPerHour+5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES(?,?,?,'event','{}',?,?)", randomID(), active, randomID(), now, now); err != nil {
		t.Fatal(err)
	}
	drainAuth(t, s)
	if len(rec.seen()) != 2 {
		t.Fatal("with WEBHOOK_HOST_BUDGET off a delivery over the host budget was held")
	}
	s.config.Features.WebhookHostBudget = true
	if _, err := s.db.Exec("INSERT INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES(?,?,?,'event','{}',?,?)", randomID(), active, randomID(), now, now); err != nil {
		t.Fatal(err)
	}
	drainAuth(t, s)
	if len(rec.seen()) != 2 {
		t.Fatal("with WEBHOOK_HOST_BUDGET on a delivery over the host budget was sent")
	}
}

// Fix 4: a header challenge's row is marked before it is sent, so a second
// lease (the worker died, or settle failed) never sends it again.
func TestWebhookHeaderChallengeNeverResentAfterARelease(t *testing.T) {
	s, rec := authStore(t, func(authRequest) (int, string) { return 200, "" })
	owner := keyFor(172)
	run(t, s, signed(owner, Command{Operation: "webhook.create", Data: authCreateData("https://hooks.example.org/once", `{"Authorization":"`+testAuthValue+`"}`)}))
	// A claim whose worker dies before the send.
	d, err := s.claimDelivery(testContext, testTime)
	if err != nil || d == nil || d.attempts != 0 {
		t.Fatalf("claim: %v %v", d, err)
	}
	s.now = func() time.Time { return time.Unix(testTime+webhookLeaseSecond+1, 0) }
	drainAuth(t, s)
	if len(rec.seen()) != 0 || queueDepth(t, s) != 0 {
		t.Fatalf("after a lost lease the header challenge was sent %d times, %d queued", len(rec.seen()), queueDepth(t, s))
	}
	// A send whose settle fails: the second lease sends nothing.
	run(t, s, signed(owner, Command{Operation: "webhook.create", Data: authCreateData("https://hooks.example.org/twice", `{"Authorization":"`+testAuthValue+`"}`), Timestamp: s.now().Unix()}))
	d, err = s.claimDelivery(testContext, s.now().Unix())
	if err != nil || d == nil {
		t.Fatalf("claim: %v", err)
	}
	if ok, _, _, _ := s.send(testContext, d, s.now().Unix()); !ok || len(rec.seen()) != 1 {
		t.Fatal("the first send did not go")
	}
	s.now = func() time.Time { return time.Unix(testTime+2*(webhookLeaseSecond+1), 0) }
	drainAuth(t, s)
	if len(rec.seen()) != 1 || queueDepth(t, s) != 0 {
		t.Fatalf("after a failed settle the header challenge went %d times, %d queued", len(rec.seen()), queueDepth(t, s))
	}
}

// Fix 5: the requests digest of a create with headers is keyed, not a plain
// hash a dictionary could match; the same command still finds its receipt
// and another value under its request_id still conflicts.
func TestWebhookCreateDigestIsKeyed(t *testing.T) {
	s, _ := authStore(t, func(authRequest) (int, string) { return 200, "" })
	owner := keyFor(173)
	data := authCreateData("https://hooks.example.org/digest", `{"X-API-Key":"hunter2"}`)
	c := signed(owner, Command{Operation: "webhook.create", Data: data, RequestID: "digest-1"})
	first := run(t, s, c)
	rows, err := s.db.Query("SELECT digest FROM requests")
	if err != nil {
		t.Fatal(err)
	}
	var digests []string
	for rows.Next() {
		var d string
		_ = rows.Scan(&d)
		digests = append(digests, d)
	}
	rows.Close()
	plain := map[string]bool{requestDigest(s.config.ServiceID, c, "id:digest-1"): true, requestDigest(s.config.ServiceID, c, "nonce:"+c.Nonce): true, sha256Hex(Canonical(s.config.ServiceID, c)): true}
	if len(digests) == 0 {
		t.Fatal("no requests rows")
	}
	for _, d := range digests {
		if plain[d] {
			t.Fatalf("a requests digest is the unkeyed hash of the command: %s", d)
		}
	}
	again := run(t, s, signed(owner, Command{Operation: "webhook.create", Data: data, RequestID: "digest-1", Nonce: "another-nonce"}))
	if again.Data["subscription_id"] != first.Data["subscription_id"] {
		t.Fatal("an exact retry under its request_id was not answered from its receipt")
	}
	fails(t, s, signed(owner, Command{Operation: "webhook.create", Data: authCreateData("https://hooks.example.org/digest", `{"X-API-Key":"hunter3"}`), RequestID: "digest-1"}), "idempotency_conflict")
	if fmt.Sprint(s.digestable(c).Data) == data {
		t.Fatal("digestable left the value")
	}
}

// Re-review of fix 3: with WEBHOOK_HOST_BUDGET off the challenge bucket only
// counts and logs, so a create/delete loop on a domain cannot hold back
// other agents' activations there; with it on, the bucket's 61st waits.
func TestWebhookChallengeBucketLogsUntilEnforced(t *testing.T) {
	for _, enforce := range []bool{false, true} {
		s, rec := authStore(t, func(authRequest) (int, string) { return 503, "" })
		s.config.Features.WebhookHostBudget = enforce
		total := WebhookHostChallengesPerHour + 3
		for i := 0; i < total; i++ {
			sub := randomID()
			if _, err := s.db.Exec("INSERT INTO webhook_subscriptions(id,account,created_by,url,secret,state,challenge,created_at) VALUES(?,?,?,?,?,'pending',?,?)",
				sub, fmt.Sprintf("acct-%d", i), "x", fmt.Sprintf("https://h%d.shared-domain.org/hook", i), webhookSecret(), randomID(), testTime); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("INSERT INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES(?,?,?,'challenge','{}',?,?)", randomID(), sub, "challenge:"+sub, testTime, testTime); err != nil {
				t.Fatal(err)
			}
		}
		drainAuth(t, s)
		want := total
		if enforce {
			want = WebhookHostChallengesPerHour
		}
		if got := len(rec.seen()); got != want {
			t.Fatalf("WEBHOOK_HOST_BUDGET=%v: %d of %d challenges to one domain sent in the hour, want %d", enforce, got, total, want)
		}
		var counted int64
		if err := s.db.QueryRow("SELECT requests FROM webhook_host_rates WHERE host=?", webhookChallengeBucket+"shared-domain.org").Scan(&counted); err != nil || counted != int64(want) {
			t.Fatalf("WEBHOOK_HOST_BUDGET=%v: the challenge bucket counted %d (%v)", enforce, counted, err)
		}
	}
}
