package board

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// challengeEndpoint is a TLS receiver the sender reaches for
// https://hooks.example.org/..., so the subscription is made through
// webhook.create (which refuses loopback URLs) and still answered here.
// reply gets the attempt number (1-based) and the challenge nonce.
type challengeEndpoint struct {
	mu       sync.Mutex
	requests []string // the delivery id of each request
	dials    atomic.Int64
}

func (e *challengeEndpoint) seen() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string{}, e.requests...)
}

// challengeStore creates one subscription with webhook.create and points the
// sender at a test server answering with reply. blocked dials through the
// real SSRF dialer to the loopback server, which it must refuse.
func challengeStore(t *testing.T, blocked bool, reply func(attempt int, nonce string) (int, string)) (*Store, *challengeEndpoint, string, string) {
	t.Helper()
	s := openTest(t, Config{})
	s.webhookInsecure = true // skips DNS at creation; the URL is still checked
	e := &challengeEndpoint{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var body struct{ Nonce string }
		_ = json.Unmarshal(raw, &body)
		e.mu.Lock()
		e.requests = append(e.requests, r.Header.Get("X-SwarmMemo-Delivery"))
		attempt := len(e.requests)
		e.mu.Unlock()
		status, text := reply(attempt, body.Nonce)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, text)
	}))
	t.Cleanup(server.Close)
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	addr := server.Listener.Addr().String()
	s.webhookClient = &http.Client{
		Timeout:       webhookRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				e.dials.Add(1)
				if blocked {
					return s.webhookDial(ctx, network, addr)
				}
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
			TLSClientConfig:   &tls.Config{RootCAs: pool, ServerName: "example.com"},
			DisableKeepAlives: true,
		},
	}
	key := keyFor(131)
	result := run(t, s, signed(key, Command{Operation: "webhook.create", Data: `{"schema":1,"url":"https://hooks.example.org/challenge"}`}))
	id := result.Data["subscription_id"].(string)
	var account string
	if err := s.db.QueryRow("SELECT account FROM webhook_subscriptions WHERE id=?", id).Scan(&account); err != nil {
		t.Fatal(err)
	}
	if blocked {
		s.webhookInsecure = false // delivery dials through the SSRF filter
	}
	return s, e, id, account
}

// driveChallenge runs the sender at each queued row's due time until the
// queue is empty, and returns the times of the attempts it made.
func driveChallenge(t *testing.T, s *Store) []int64 {
	t.Helper()
	times := []int64{}
	for i := 0; i < 10 && queueDepth(t, s) > 0; i++ {
		var next int64
		if err := s.db.QueryRow("SELECT min(next_at) FROM webhook_deliveries").Scan(&next); err != nil {
			t.Fatal(err)
		}
		s.now = func() time.Time { return time.Unix(next, 0) }
		worked, err := s.deliverOnce(testContext)
		if err != nil {
			t.Fatal(err)
		}
		if worked {
			times = append(times, next)
		}
	}
	return times
}

func webhookRateCount(t *testing.T, s *Store, account string) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRow("SELECT coalesce(sum(count),0) FROM webhook_rates WHERE account=?", account).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A 503 is transient: the same challenge delivery is tried again after the
// delivery backoff, and an echo then activates the subscription.
func TestWebhookChallengeRetriedAfterTransientFailure(t *testing.T) {
	s, e, id, account := challengeStore(t, false, func(attempt int, nonce string) (int, string) {
		if attempt == 1 {
			return 503, "warming up"
		}
		return 200, `{"nonce":"` + nonce + `"}`
	})
	times := driveChallenge(t, s)
	if state, _, reason := subscriptionState(t, s, id); state != "active" || reason != "" {
		t.Fatalf("a retried, echoed challenge left the subscription %s (%q)", state, reason)
	}
	got := e.seen()
	if len(got) != 2 || got[0] != got[1] || got[0] == "" {
		t.Fatalf("want one challenge delivery tried twice, got %q", got)
	}
	if len(times) != 2 || times[1]-times[0] < webhookBackoff(1, func() float64 { return 0 }) {
		t.Fatalf("the retry ignored the delivery backoff: %v", times)
	}
	if n := webhookRateCount(t, s, account); n != 2 {
		t.Fatalf("challenge attempts counted %d against the hourly ceiling, want 2", n)
	}
}

// Three transient failures end the challenge: no fourth attempt, the
// subscription stays pending inside its window, and expiry still applies.
func TestWebhookChallengeStopsAfterBoundedAttempts(t *testing.T) {
	s, e, id, account := challengeStore(t, false, func(int, string) (int, string) { return 503, "down" })
	times := driveChallenge(t, s)
	if n := len(e.seen()); n != WebhookChallengeAttempts {
		t.Fatalf("a failing endpoint received %d challenges, want %d", n, WebhookChallengeAttempts)
	}
	for _, at := range times {
		if at >= testTime+WebhookPendingTTL {
			t.Fatalf("a challenge attempt at %d fell outside the pending window", at)
		}
	}
	if queueDepth(t, s) != 0 {
		t.Fatal("an exhausted challenge stayed queued")
	}
	state, _, reason := subscriptionState(t, s, id)
	if state != "pending" || reason != "status 503" {
		t.Fatalf("after the last attempt: %s %q", state, reason)
	}
	if n := webhookRateCount(t, s, account); n != WebhookChallengeAttempts {
		t.Fatalf("challenge attempts counted %d, want %d", n, WebhookChallengeAttempts)
	}
	s.now = func() time.Time { return time.Unix(testTime+WebhookPendingTTL+1, 0) }
	s.expireWebhooks(testContext)
	if state, _, _ := subscriptionState(t, s, id); state != "disabled" {
		t.Fatalf("an unconfirmed subscription did not expire: %s", state)
	}
}

// Answers that are not transient end the challenge after one attempt: the
// host asked us to stop (429), refused (4xx), redirected (3xx) or answered
// without consenting (2xx with no nonce).
func TestWebhookChallengeNotRetriedOnFinalAnswers(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
	}{{429, "slow down"}, {404, "no"}, {401, "no"}, {301, ""}, {200, `{"ok":true}`}} {
		s, e, id, _ := challengeStore(t, false, func(int, string) (int, string) { return c.status, c.body })
		driveChallenge(t, s)
		if n := len(e.seen()); n != 1 {
			t.Fatalf("status %d: %d challenge attempts, want 1", c.status, n)
		}
		if queueDepth(t, s) != 0 {
			t.Fatalf("status %d: the challenge stayed queued", c.status)
		}
		if state, _, reason := subscriptionState(t, s, id); state != "pending" || reason == "" || strings.Contains(reason, "will retry") {
			t.Fatalf("status %d: %s %q", c.status, state, reason)
		}
	}
}

// A dialer refusal is permanent for a challenge: the SSRF filter is not asked
// again, and the endpoint is never reached.
func TestWebhookChallengeNotRetriedWhenTheAddressIsBlocked(t *testing.T) {
	s, e, id, _ := challengeStore(t, true, func(int, string) (int, string) { return 200, "" })
	driveChallenge(t, s)
	if len(e.seen()) != 0 {
		t.Fatal("a blocked address was reached")
	}
	if n := e.dials.Load(); n != 1 {
		t.Fatalf("a blocked address was dialed %d times, want 1", n)
	}
	if queueDepth(t, s) != 0 {
		t.Fatal("a blocked challenge stayed queued")
	}
	if _, _, reason := subscriptionState(t, s, id); !strings.Contains(reason, "webhook_address_blocked") || strings.Contains(reason, "will retry") {
		t.Fatalf("blocked challenge reason %q", reason)
	}
}

// Retries are counted against, and stopped by, the hourly delivery ceiling.
func TestWebhookChallengeRetryRespectsTheHourlyCeiling(t *testing.T) {
	s, e, _, account := challengeStore(t, false, func(int, string) (int, string) { return 503, "down" })
	if _, err := s.db.Exec("UPDATE webhook_rates SET count=? WHERE account=?", int64(WebhookMaxDeliveriesHour), account); err != nil {
		t.Fatal(err)
	}
	driveChallenge(t, s)
	if n := len(e.seen()); n != 1 {
		t.Fatalf("a challenge over the hourly ceiling was retried: %d attempts", n)
	}
}

func TestWebhookChallengeTransientReasons(t *testing.T) {
	for reason, want := range map[string]bool{
		"request failed: timeout": true, "request failed: connection failed": true,
		"status 500": true, "status 503": true, "status 599": true, "status 408": true,
		"status 429": false, "status 404": false, "status 400": false, "status 301 (redirects are never followed)": false,
		"request failed: webhook_address_blocked": false, "request failed: webhook_unresolved": false,
		"invalid callback URL": false, "": false,
	} {
		if got := challengeTransient(reason); got != want {
			t.Errorf("challengeTransient(%q) = %v, want %v", reason, got, want)
		}
	}
}
