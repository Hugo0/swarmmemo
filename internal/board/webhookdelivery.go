package board

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The sender is a bounded worker pool over a table, not a goroutine per event.
// A burst of posts becomes rows; workers claim one row at a time under a short
// lease, so a crash strands nothing and a restart resumes the same queue with the
// same backoff. Nothing here runs unless StartWebhookDelivery is called.

const (
	webhookRequestTimeout = 8 * time.Second
	webhookDialTimeout    = 3 * time.Second
	webhookResponseBytes  = 8 << 10
	webhookDefaultWorkers = 2
	webhookDefaultPoll    = 2 * time.Second
	webhookMaintainEvery  = 60 * time.Second
)

// webhookDial is the SSRF boundary that actually matters: it filters the address
// being connected to, after resolution, so a hostname that answered with a public
// address at creation and a private one at delivery time is refused here.
func (s *Store) webhookDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: webhookDialTimeout}
	if s.webhookInsecure {
		return dialer.DialContext(ctx, network, addr)
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, webhookError("webhook_unresolved")
	}
	var last error = webhookError("webhook_unresolved")
	for _, ip := range ips {
		if err = publicWebhookIP(ip); err != nil {
			// One blocked address disqualifies the host: a resolver that returns a
			// public and a private answer together is exactly the rebinding shape.
			return nil, err
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

func (s *Store) webhookHTTP() *http.Client {
	s.webhookOnce.Do(func() {
		if s.webhookClient != nil {
			return
		}
		s.webhookClient = &http.Client{
			Timeout: webhookRequestTimeout,
			// A redirect is a second attacker-chosen address, so it is never
			// followed; the 3xx is returned and counts as a failed delivery.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				Proxy:                 nil, // No proxy environment can become a bypass.
				DialContext:           s.webhookDial,
				TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
				TLSHandshakeTimeout:   4 * time.Second,
				ResponseHeaderTimeout: 5 * time.Second,
				DisableKeepAlives:     true,
				MaxIdleConns:          0,
			},
		}
	})
	return s.webhookClient
}

// SignWebhook is the exact contract a receiver implements: hex HMAC-SHA256 over
// the timestamp, a dot, and the exact body bytes. Exported so the docs and the
// tests verify one implementation rather than two descriptions of one.
func SignWebhook(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

type webhookDelivery struct {
	seq                           int64
	id, subscription, kind, body  string
	attempts                      int64
	url, secret, state, challenge string
	account                       string
}

// claimDelivery leases one due row. The lease means a crashed or stopped worker
// releases its row by expiry instead of stranding it.
func (s *Store) claimDelivery(ctx context.Context, now int64) (*webhookDelivery, error) {
	var d webhookDelivery
	err := s.db.QueryRowContext(ctx, `UPDATE webhook_deliveries SET leased_until=? WHERE seq=(
 SELECT seq FROM webhook_deliveries WHERE next_at<=? AND leased_until<=? ORDER BY next_at,seq LIMIT 1)
 RETURNING seq,id,subscription,kind,body,attempts`, now+webhookLeaseSecond, now, now).
		Scan(&d.seq, &d.id, &d.subscription, &d.kind, &d.body, &d.attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	err = s.db.QueryRowContext(ctx, "SELECT url,secret,state,challenge,account FROM webhook_subscriptions WHERE id=?", d.subscription).
		Scan(&d.url, &d.secret, &d.state, &d.challenge, &d.account)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && d.state == "disabled") {
		_, err = s.db.ExecContext(ctx, "DELETE FROM webhook_deliveries WHERE seq=?", d.seq)
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	// A pending subscription receives its challenge and nothing else.
	if d.state == "pending" && d.kind != "challenge" {
		_, err = s.db.ExecContext(ctx, "UPDATE webhook_deliveries SET leased_until=0,next_at=? WHERE seq=?", now+webhookBaseBackoff, d.seq)
		return nil, err
	}
	return &d, nil
}

// send performs one attempt. ok reports a 2xx; permanent reports a status the
// receiver will keep rejecting, so retrying it is only load on someone else.
func (s *Store) send(ctx context.Context, d *webhookDelivery, now int64) (ok, permanent bool, echo string, reason string) {
	body := []byte(d.body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return false, true, "", "invalid callback URL"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "SwarmMemo-Webhook/1")
	req.Header.Set("X-SwarmMemo-Delivery", d.id)
	req.Header.Set("X-SwarmMemo-Timestamp", strconv.FormatInt(now, 10))
	req.Header.Set("X-SwarmMemo-Signature", SignWebhook(d.secret, now, body))
	req.ContentLength = int64(len(body))
	resp, err := s.webhookHTTP().Do(req)
	if err != nil {
		return false, false, "", "request failed: " + webhookReason(err)
	}
	defer resp.Body.Close()
	// Bounded read: the response is only interesting for the challenge echo, and
	// an endpoint must not be able to spend our memory by streaming forever.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, webhookResponseBytes))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, false, string(raw), ""
	}
	status := "status " + strconv.Itoa(resp.StatusCode)
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return false, true, "", status + " (redirects are never followed)"
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408 && resp.StatusCode != 429 {
		return false, true, "", status
	}
	return false, false, "", status
}

// webhookReason keeps a failure description short and free of anything the
// endpoint controls, so last_error cannot become a storage or injection channel.
func webhookReason(err error) string {
	var blocked *Error
	if errors.As(err, &blocked) {
		return blocked.Code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "connection failed"
}

func webhookBackoff(attempts int64, jitter func() float64) int64 {
	delay := int64(webhookBaseBackoff)
	for i := int64(1); i < attempts && delay < webhookMaxBackoff; i++ {
		delay *= 2
	}
	if delay > webhookMaxBackoff {
		delay = webhookMaxBackoff
	}
	// ±25%, so endpoints that failed together do not retry together. The cap
	// applies after jitter, not before, or the spread would exceed it.
	scaled := int64(float64(delay) * (0.75 + 0.5*jitter()))
	if scaled < 1 {
		scaled = 1
	}
	if scaled > webhookMaxBackoff {
		scaled = webhookMaxBackoff
	}
	return scaled
}

func (s *Store) settle(ctx context.Context, d *webhookDelivery, ok, permanent bool, echo, reason string, now int64) error {
	if ok && d.kind == "challenge" {
		if !strings.Contains(echo, d.challenge) {
			// Consent was not proven. The subscription stays pending and expires;
			// no further request is made to an endpoint that did not answer us.
			if _, err := s.db.ExecContext(ctx, "UPDATE webhook_subscriptions SET last_error='challenge nonce not echoed' WHERE id=?", d.subscription); err != nil {
				return err
			}
			_, err := s.db.ExecContext(ctx, "DELETE FROM webhook_deliveries WHERE seq=?", d.seq)
			return err
		}
		if _, err := s.db.ExecContext(ctx, "UPDATE webhook_subscriptions SET state='active',confirmed_at=?,challenge='',failures=0,last_error='' WHERE id=?", now, d.subscription); err != nil {
			return err
		}
		_, err := s.db.ExecContext(ctx, "DELETE FROM webhook_deliveries WHERE seq=?", d.seq)
		return err
	}
	if ok {
		if _, err := s.db.ExecContext(ctx, "UPDATE webhook_subscriptions SET failures=0,last_error='' WHERE id=?", d.subscription); err != nil {
			return err
		}
		_, err := s.db.ExecContext(ctx, "DELETE FROM webhook_deliveries WHERE seq=?", d.seq)
		return err
	}
	attempts := d.attempts + 1
	exhausted := permanent || attempts >= WebhookMaxAttempts || d.kind == "challenge"
	if !exhausted {
		_, err := s.db.ExecContext(ctx, "UPDATE webhook_deliveries SET attempts=?,next_at=?,leased_until=0 WHERE seq=?",
			attempts, now+webhookBackoff(attempts, rand.Float64), d.seq)
		return err
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM webhook_deliveries WHERE seq=?", d.seq); err != nil {
		return err
	}
	var failures int64
	if err := s.db.QueryRowContext(ctx, "UPDATE webhook_subscriptions SET failures=failures+1,last_error=? WHERE id=? RETURNING failures", reason, d.subscription).Scan(&failures); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if failures < WebhookDisableFailures {
		return nil
	}
	// Auto-disable records why and stops dialing. Deletion is left to the owner,
	// who needs to be able to read the reason.
	_, err := s.db.ExecContext(ctx, "UPDATE webhook_subscriptions SET state='disabled',disabled_at=? WHERE id=? AND state<>'disabled'", now, d.subscription)
	return err
}

// deliverOnce claims and completes at most one delivery. It reports whether it
// did work, so a worker can drain a backlog without waiting for the next tick.
func (s *Store) deliverOnce(ctx context.Context) (bool, error) {
	now := s.now().Unix()
	d, err := s.claimDelivery(ctx, now)
	if err != nil || d == nil {
		return d != nil, err
	}
	// The attempt and its outcome both survive shutdown: cancelling mid-request
	// would abort a delivery the endpoint may already have processed, and the
	// retry would then be a duplicate. StopWebhookDelivery waits for this instead.
	attempt, release := context.WithTimeout(context.WithoutCancel(ctx), webhookRequestTimeout)
	ok, permanent, echo, reason := s.send(attempt, d, s.now().Unix())
	release()
	settle, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return true, s.settle(settle, d, ok, permanent, echo, reason, s.now().Unix())
}

// StartWebhookDelivery runs the sender until ctx is cancelled. Callers pair it
// with StopWebhookDelivery after the HTTP server has stopped, the way
// FlushReaderCounts is paired with shutdown.
func (s *Store) StartWebhookDelivery(ctx context.Context, workers int) {
	if workers <= 0 {
		workers = webhookDefaultWorkers
	}
	poll := s.webhookPoll
	if poll <= 0 {
		poll = webhookDefaultPoll
	}
	for i := 0; i < workers; i++ {
		s.webhookWG.Add(1)
		go func() {
			defer s.webhookWG.Done()
			ticker := time.NewTicker(poll)
			defer ticker.Stop()
			for {
				for n := 0; n < WebhookMaxFanout; n++ {
					worked, err := survive(func() (bool, error) { return s.deliverOnce(ctx) })
					if err != nil || !worked || ctx.Err() != nil {
						break
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	s.webhookWG.Add(1)
	go func() {
		defer s.webhookWG.Done()
		ticker := time.NewTicker(webhookMaintainEvery)
		defer ticker.Stop()
		for {
			_, _ = survive(func() (bool, error) { s.expireWebhooks(ctx); return true, nil })
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// StopWebhookDelivery waits for in-flight attempts. Queued rows survive: the
// queue is a table, so stopping loses no notification and repeats none that the
// endpoint already acknowledged.
func (s *Store) StopWebhookDelivery() { s.webhookWG.Wait() }

// expireWebhooks removes subscriptions that were never confirmed and hourly
// counters nobody will read again. Errors are ignored: this is maintenance, and
// every bound it tidies is also enforced on the request path.
func (s *Store) expireWebhooks(ctx context.Context) {
	now := s.now().Unix()
	_, _ = s.db.ExecContext(ctx, "DELETE FROM webhook_deliveries WHERE subscription IN (SELECT id FROM webhook_subscriptions WHERE state='pending' AND created_at<?)", now-WebhookPendingTTL)
	_, _ = s.db.ExecContext(ctx, "DELETE FROM webhook_subscriptions WHERE state='pending' AND created_at<?", now-WebhookPendingTTL)
	_, _ = s.db.ExecContext(ctx, "DELETE FROM webhook_rates WHERE hour<?", now/3600-24)
}
