package board

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type received struct {
	body      []byte
	delivery  string
	timestamp string
	signature string
}

// insecureStore is a store whose sender may dial the loopback test server. The
// hook is unexported and set only here; no configuration reaches it.
func insecureStore(t *testing.T) *Store {
	t.Helper()
	s := openTest(t, Config{})
	s.webhookInsecure = true
	s.webhookPoll = 5 * time.Millisecond
	return s
}

func endpoint(t *testing.T, handler func(*received) (int, string)) (*httptest.Server, func() []received) {
	t.Helper()
	var mu sync.Mutex
	seen := []received{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		got := received{body: body, delivery: r.Header.Get("X-SwarmMemo-Delivery"), timestamp: r.Header.Get("X-SwarmMemo-Timestamp"), signature: r.Header.Get("X-SwarmMemo-Signature")}
		mu.Lock()
		seen = append(seen, got)
		mu.Unlock()
		status, reply := handler(&got)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(server.Close)
	return server, func() []received {
		mu.Lock()
		defer mu.Unlock()
		return append([]received{}, seen...)
	}
}

func subscriptionState(t *testing.T, s *Store, id string) (string, int64, string) {
	t.Helper()
	var state, reason string
	var failures int64
	if err := s.db.QueryRow("SELECT state,failures,last_error FROM webhook_subscriptions WHERE id=?", id).Scan(&state, &failures, &reason); err != nil {
		t.Fatal(err)
	}
	return state, failures, reason
}

func queueDepth(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM webhook_deliveries").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Every delivery must be verifiable from the exact bytes the endpoint received,
// and must carry a stable id so a receiver can dedupe a retried delivery.
func TestWebhookDeliveryIsSignedOverExactBytesAndCarriesAnID(t *testing.T) {
	s := insecureStore(t)
	server, seen := endpoint(t, func(*received) (int, string) { return 200, "thanks" })
	id, secret := activeWebhook(t, s, "account-a", server.URL+"/hook")
	_, err := s.db.Exec("INSERT INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES(?,?,?,?,?,?,?)",
		"delivery-1", id, "event-1", "event", `{"schema":1,"delivery_id":"delivery-1","type":"event"}`, testTime, testTime)
	if err != nil {
		t.Fatal(err)
	}
	worked, err := s.deliverOnce(testContext)
	if err != nil || !worked {
		t.Fatalf("delivery did not run: %v", err)
	}
	got := seen()
	if len(got) != 1 {
		t.Fatalf("endpoint saw %d requests", len(got))
	}
	stamp, err := strconv.ParseInt(got[0].timestamp, 10, 64)
	if err != nil {
		t.Fatalf("missing or malformed timestamp header: %q", got[0].timestamp)
	}
	if want := SignWebhook(secret, stamp, got[0].body); got[0].signature != want {
		t.Fatalf("signature %q does not verify over the received bytes (want %q)", got[0].signature, want)
	}
	// A different timestamp, body or secret must not verify: that is what stops a
	// captured delivery being replayed at another time or against another
	// subscription.
	if SignWebhook(secret, stamp+1, got[0].body) == got[0].signature {
		t.Fatal("the timestamp does not participate in the signature")
	}
	if SignWebhook(secret, stamp, append(got[0].body, '!')) == got[0].signature {
		t.Fatal("the body does not participate in the signature")
	}
	if SignWebhook(webhookSecret(), stamp, got[0].body) == got[0].signature {
		t.Fatal("another subscription's secret produced the same signature")
	}
	if got[0].delivery != "delivery-1" {
		t.Fatalf("delivery id header %q", got[0].delivery)
	}
	if queueDepth(t, s) != 0 {
		t.Fatal("a delivered notification stayed queued")
	}
}

func TestWebhookRetriedDeliveryKeepsItsIDAndBacksOff(t *testing.T) {
	s := insecureStore(t)
	var fail atomic.Bool
	fail.Store(true)
	server, seen := endpoint(t, func(*received) (int, string) {
		if fail.Load() {
			return 500, "later"
		}
		return 200, "ok"
	})
	id, _ := activeWebhook(t, s, "account-b", server.URL+"/hook")
	if _, err := s.db.Exec("INSERT INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES('d2',?,'e2','event','{}',?,?)", id, testTime, testTime); err != nil {
		t.Fatal(err)
	}
	if _, err := s.deliverOnce(testContext); err != nil {
		t.Fatal(err)
	}
	var attempts, next int64
	if err := s.db.QueryRow("SELECT attempts,next_at FROM webhook_deliveries WHERE id='d2'").Scan(&attempts, &next); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || next <= testTime {
		t.Fatalf("a failed delivery was not rescheduled: attempts=%d next=%d", attempts, next)
	}
	// It is not due yet, so a second pass must not hammer the endpoint.
	if worked, err := s.deliverOnce(testContext); err != nil || worked {
		t.Fatalf("backoff was ignored: worked=%v err=%v", worked, err)
	}
	s.now = func() time.Time { return time.Unix(next, 0) }
	fail.Store(false)
	if _, err := s.deliverOnce(testContext); err != nil {
		t.Fatal(err)
	}
	got := seen()
	if len(got) != 2 {
		t.Fatalf("endpoint saw %d requests", len(got))
	}
	if got[0].delivery != got[1].delivery || got[0].delivery != "d2" {
		t.Fatalf("the delivery id changed across a retry: %q then %q", got[0].delivery, got[1].delivery)
	}
	if got[0].signature == got[1].signature {
		t.Fatal("a retry reused its timestamp and signature, so a replay is indistinguishable")
	}
	if queueDepth(t, s) != 0 {
		t.Fatal("a delivery that eventually succeeded stayed queued")
	}
	if _, failures, _ := subscriptionState(t, s, id); failures != 0 {
		t.Fatalf("success did not reset the failure counter: %d", failures)
	}
}

func TestWebhookBackoffGrowsAndIsBounded(t *testing.T) {
	for _, attempt := range []int64{1, 2, 3, 4, 5, 6, 20} {
		low, high := webhookBackoff(attempt, func() float64 { return 0 }), webhookBackoff(attempt, func() float64 { return 1 })
		if low >= high {
			t.Fatalf("attempt %d has no jitter spread: %d..%d", attempt, low, high)
		}
		if high > webhookMaxBackoff {
			t.Fatalf("attempt %d exceeds the cap: %d", attempt, high)
		}
	}
	if webhookBackoff(1, func() float64 { return 0.5 }) >= webhookBackoff(4, func() float64 { return 0.5 }) {
		t.Fatal("backoff does not grow with attempts")
	}
	if webhookBackoff(20, func() float64 { return 0.5 }) != webhookBackoff(30, func() float64 { return 0.5 }) {
		t.Fatal("backoff is not capped")
	}
}

func TestWebhookPermanentRejectionIsNotRetriedAndDisablesEventually(t *testing.T) {
	s := insecureStore(t)
	server, seen := endpoint(t, func(*received) (int, string) { return 410, "gone" })
	id, _ := activeWebhook(t, s, "account-c", server.URL+"/hook")
	for i := 0; i < WebhookDisableFailures; i++ {
		if _, err := s.db.Exec("INSERT INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES(?,?,?,'event','{}',?,?)",
			"d"+strconv.Itoa(i), id, "e"+strconv.Itoa(i), testTime, testTime); err != nil {
			t.Fatal(err)
		}
		if _, err := s.deliverOnce(testContext); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen()) != WebhookDisableFailures {
		t.Fatalf("a permanent rejection was retried: %d requests for %d deliveries", len(seen()), WebhookDisableFailures)
	}
	state, failures, reason := subscriptionState(t, s, id)
	if state != "disabled" {
		t.Fatalf("sustained failure did not disable the subscription: %s (%d failures)", state, failures)
	}
	if reason == "" {
		t.Fatal("disabling recorded no reason")
	}
	// A disabled subscription is never dialed again; its queued work is dropped.
	if _, err := s.db.Exec("INSERT INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES('after',?,'after','event','{}',?,?)", id, testTime, testTime); err != nil {
		t.Fatal(err)
	}
	if _, err := s.deliverOnce(testContext); err != nil {
		t.Fatal(err)
	}
	if len(seen()) != WebhookDisableFailures {
		t.Fatal("a disabled subscription received another delivery")
	}
	if queueDepth(t, s) != 0 {
		t.Fatal("work for a disabled subscription stayed queued")
	}
}

func TestWebhookGivesUpAfterBoundedAttempts(t *testing.T) {
	s := insecureStore(t)
	server, seen := endpoint(t, func(*received) (int, string) { return 503, "busy" })
	id, _ := activeWebhook(t, s, "account-d", server.URL+"/hook")
	if _, err := s.db.Exec("INSERT INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES('d',?,'e','event','{}',?,?)", id, testTime, testTime); err != nil {
		t.Fatal(err)
	}
	now := testTime
	for i := 0; i < WebhookMaxAttempts+3 && queueDepth(t, s) > 0; i++ {
		s.now = func() time.Time { return time.Unix(now, 0) }
		if _, err := s.deliverOnce(testContext); err != nil {
			t.Fatal(err)
		}
		now += webhookMaxBackoff + 1
	}
	if n := len(seen()); n != WebhookMaxAttempts {
		t.Fatalf("a failing endpoint received %d attempts, want %d", n, WebhookMaxAttempts)
	}
	if queueDepth(t, s) != 0 {
		t.Fatal("an exhausted delivery stayed queued")
	}
}

func TestWebhookDoesNotFollowRedirects(t *testing.T) {
	s := insecureStore(t)
	target, targetSeen := endpoint(t, func(*received) (int, string) { return 200, "ok" })
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/moved", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)
	id, _ := activeWebhook(t, s, "account-e", redirector.URL+"/hook")
	if _, err := s.db.Exec("INSERT INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES('d',?,'e','event','{}',?,?)", id, testTime, testTime); err != nil {
		t.Fatal(err)
	}
	if _, err := s.deliverOnce(testContext); err != nil {
		t.Fatal(err)
	}
	if len(targetSeen()) != 0 {
		t.Fatal("the sender followed a redirect to a second address")
	}
	if _, _, reason := subscriptionState(t, s, id); reason == "" {
		t.Fatal("a redirect was treated as success")
	}
}

func TestWebhookChallengeActivatesOnlyOnEcho(t *testing.T) {
	for _, echo := range []bool{false, true} {
		s := insecureStore(t)
		var nonce atomic.Value
		nonce.Store("")
		server, seen := endpoint(t, func(r *received) (int, string) {
			var body struct{ Nonce string }
			_ = json.Unmarshal(r.body, &body)
			nonce.Store(body.Nonce)
			if echo {
				return 200, `{"echo":"` + body.Nonce + `"}`
			}
			return 200, `{"ok":true}`
		})
		id := randomID()
		challenge := randomID()
		if _, err := s.db.Exec("INSERT INTO webhook_subscriptions(id,account,created_by,url,secret,state,challenge,created_at) VALUES(?,?,?,?,?,'pending',?,?)",
			id, "account-f", "account-f", server.URL+"/hook", webhookSecret(), challenge, testTime); err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]any{"schema": 1, "delivery_id": "c1", "subscription_id": id, "type": "challenge", "nonce": challenge})
		if _, err := s.db.Exec("INSERT INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES('c1',?,?,'challenge',?,?,?)",
			id, "challenge:"+id, string(body), testTime, testTime); err != nil {
			t.Fatal(err)
		}
		// An event queued for a pending subscription must not be sent.
		if _, err := s.db.Exec("INSERT INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES('e1',?,'e1','event','{}',?,?)", id, testTime, testTime); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			if _, err := s.deliverOnce(testContext); err != nil {
				t.Fatal(err)
			}
		}
		state, _, _ := subscriptionState(t, s, id)
		if echo && state != "active" {
			t.Fatalf("an echoed nonce did not activate the subscription: %s", state)
		}
		if !echo && state != "pending" {
			t.Fatalf("an unechoed challenge changed the state to %s", state)
		}
		got := seen()
		if len(got) == 0 || got[0].delivery != "c1" {
			t.Fatal("the first request to a new subscription must be its challenge")
		}
		if !echo && len(got) != 1 {
			t.Fatalf("an unconfirmed subscription received %d requests", len(got))
		}
		if echo && len(got) != 2 {
			t.Fatalf("a confirmed subscription received %d requests, want the challenge and the queued event", len(got))
		}
	}
}

// An unconfirmed subscription is marked expired, not deleted: the row stays, it
// is never contacted, and it frees its live slot.
func TestWebhookPendingSubscriptionsExpire(t *testing.T) {
	s := insecureStore(t)
	key := keyFor(94)
	url := func(i int) string { return `{"schema":1,"url":"https://hooks.example.org/` + string(rune('a'+i)) + `"}` }
	for i := 0; i < WebhookMaxPerAccount; i++ {
		run(t, s, signed(key, Command{Operation: "webhook.create", Data: url(i)}))
	}
	later := int64(testTime + WebhookPendingTTL + 1)
	s.now = func() time.Time { return time.Unix(later, 0) }
	s.expireWebhooks(testContext)
	var rows, expired int
	if err := s.db.QueryRow("SELECT count(*),coalesce(sum(CASE WHEN "+webhookExpiredSQL+" THEN 1 ELSE 0 END),0) FROM webhook_subscriptions").Scan(&rows, &expired); err != nil {
		t.Fatal(err)
	}
	if rows != WebhookMaxPerAccount || expired != WebhookMaxPerAccount {
		t.Fatalf("after expiry: %d rows, %d expired; want all kept and marked", rows, expired)
	}
	if queued := webhookRows(t, s); len(queued) != 0 {
		t.Fatal("an expired subscription kept queued challenges")
	}
	listed := run(t, s, signed(key, Command{Operation: "webhook.list", Timestamp: later}))
	for _, item := range listed.Data["subscriptions"].([]map[string]any) {
		if item["state"] != "expired" || item["pending_expires"] != nil {
			t.Fatalf("listed %v, want state expired", item)
		}
	}
	// Expired rows do not hold live slots, and re-subscribing an expired URL
	// reuses its row instead of colliding with it.
	for i := 0; i < WebhookMaxPerAccount; i++ {
		result := run(t, s, signed(key, Command{Operation: "webhook.create", Data: url(i + 1), Timestamp: later}))
		if result.Data["state"] != "pending" {
			t.Fatal("re-subscription must start pending")
		}
	}
	fails(t, s, signed(key, Command{Operation: "webhook.create", Data: url(9), Timestamp: later}), "webhook_limit")
	if err := s.db.QueryRow("SELECT count(*),coalesce(sum(CASE WHEN "+webhookExpiredSQL+" THEN 1 ELSE 0 END),0) FROM webhook_subscriptions").Scan(&rows, &expired); err != nil {
		t.Fatal(err)
	}
	if rows != WebhookMaxPerAccount+1 || expired != 1 {
		t.Fatalf("after re-subscribing: %d rows, %d expired; want %d and 1", rows, expired, WebhookMaxPerAccount+1)
	}
	// Retained expired rows are bounded too.
	var account, live string
	if err := s.db.QueryRow("SELECT account,id FROM webhook_subscriptions WHERE state='pending' LIMIT 1").Scan(&account, &live); err != nil {
		t.Fatal(err)
	}
	run(t, s, signed(key, Command{Operation: "webhook.delete", Target: live, Timestamp: later}))
	for i := 0; i < WebhookMaxExpiredPerAccount-1; i++ {
		if _, err := s.db.Exec("INSERT INTO webhook_subscriptions(id,account,created_by,url,secret,state,created_at,disabled_at) VALUES(?,?,?,?,?,'disabled',?,?)",
			randomID(), account, account, "https://hooks.example.org/old"+string(rune('a'+i)), webhookSecret(), testTime, later); err != nil {
			t.Fatal(err)
		}
	}
	fails(t, s, signed(key, Command{Operation: "webhook.create", Data: url(9), Timestamp: later}), "webhook_limit")
}

// Stopping must not lose a queued notification, and must not repeat one the
// endpoint already acknowledged, the way FlushReaderCounts is paired with
// shutdown in cmd/swarmmemo.
func TestWebhookShutdownDrainsWithoutLosingOrRepeating(t *testing.T) {
	s := insecureStore(t)
	var delivered sync.Map
	var duplicates atomic.Int64
	release := make(chan struct{})
	server, seen := endpoint(t, func(r *received) (int, string) {
		if _, loaded := delivered.LoadOrStore(r.delivery, true); loaded {
			duplicates.Add(1)
		}
		<-release
		return 200, "ok"
	})
	id, _ := activeWebhook(t, s, "account-h", server.URL+"/hook")
	const queued = 6
	for i := 0; i < queued; i++ {
		if _, err := s.db.Exec("INSERT INTO webhook_deliveries(id,subscription,event_id,kind,body,next_at,created_at) VALUES(?,?,?,'event','{}',?,?)",
			"q"+strconv.Itoa(i), id, "e"+strconv.Itoa(i), testTime, testTime); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.StartWebhookDelivery(ctx, 2)
	// Let both workers block inside an attempt, then stop the service under them.
	deadline := time.Now().Add(5 * time.Second)
	for len(seen()) < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	inflight := len(seen())
	if inflight == 0 {
		t.Fatal("no delivery started")
	}
	cancel()
	close(release)
	done := make(chan struct{})
	go func() { s.StopWebhookDelivery(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("StopWebhookDelivery did not return")
	}
	if duplicates.Load() != 0 {
		t.Fatalf("%d deliveries were repeated across shutdown", duplicates.Load())
	}
	// Whatever was in flight was recorded, so it is gone from the queue; whatever
	// was not started is still there, waiting for the next start.
	remaining := queueDepth(t, s)
	if remaining+len(seen()) != queued {
		t.Fatalf("queue lost or duplicated work: %d queued + %d sent, want %d", remaining, len(seen()), queued)
	}
	if remaining == 0 {
		t.Fatal("shutdown drained the whole backlog instead of leaving it durable")
	}
	var leased int
	if err := s.db.QueryRow("SELECT count(*) FROM webhook_deliveries WHERE leased_until>?", testTime).Scan(&leased); err != nil {
		t.Fatal(err)
	}
	if leased != 0 {
		t.Fatalf("%d deliveries were left leased by a stopped worker", leased)
	}
}
