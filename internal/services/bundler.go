package services

// Bundlers. The x402 service is an aggregator: one catalogue of pay-per-call
// APIs, one service.call, one credit bill, whichever upstream sells the API.
// A Bundler is one such upstream, a rail that pays for a call on SwarmMemo's
// behalf: x402 pays each call in USDC from the relay wallet (x402Rail), and a
// key-based marketplace bills an account SwarmMemo holds with it. The service
// does everything else the same way for every bundler: the catalogue and the
// arguments (only a listed resource, never a URL, header or method), the
// credit (the engine holds the resource's maximum and settles what was
// paid), the caps and the kill switch (every upstream dollar is a row in
// x402_payments, reserved before it is committed), and the answer (bounded,
// untrusted data, never rendered).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"
)

// A Bundler pays one upstream for one call.
type Bundler interface {
	// Name is the bundler's id: a resource's "bundler", and the rail its
	// payments are recorded under (x402_payments.network for key-based
	// bundlers, the CAIP-2 chain for x402).
	Name() string
	// Ready reports whether a call can be made now: configured, its key
	// present. A bundler that is not ready lists nothing and every call to
	// its resources is service_unavailable.
	Ready() bool
	// Exchange makes the call p describes. It must reserve through pay,
	// exactly once, before it commits any money (the reservation may be
	// refused by a cap: return that error), and after reserving it records
	// every outcome but success through pay.finish. It returns the
	// upstream's bounded response and, when it paid, the receipt; the
	// service records the success.
	Exchange(ctx context.Context, p x402Plan, pay *payment) (x402Response, *x402Receipt, error)
}

// A toolBundler is a key-based bundler that serves every resource at one
// endpoint and names each by its tool id (X402Resource.Tool); the agent's
// JSON body is the tool's arguments.
type toolBundler interface {
	Bundler
	Endpoint() string
}

// payment is one call's access to the shared payments ledger and the
// SSRF-safe client.
type payment struct {
	x      *x402
	c      Call
	id     string      // the x402_payments row, once reserved
	r      reservation // what was reserved
	state  string      // the state finish recorded
	status int         // the upstream's HTTP status finish recorded
}

// fetch sends the plan's request with extra headers, reading at most limit
// bytes of the answer.
func (pay *payment) fetch(ctx context.Context, p x402Plan, header http.Header, limit int) (x402Response, error) {
	return pay.x.fetch(ctx, p, header, limit)
}

// reserve records an amount before it is committed, in one statement that
// inserts nothing when it would pass a cap (x402.reserve).
func (pay *payment) reserve(ctx context.Context, p x402Plan, r reservation) error {
	if pay.id != "" {
		return refusal("upstream_failed") // one payment per call
	}
	r.id = newCallID()
	if err := pay.x.reserve(ctx, pay.c, p, r); err != nil {
		return err
	}
	pay.id, pay.r = r.id, r
	return nil
}

// finish records what happened after the reservation; a no-op before it.
func (pay *payment) finish(state string, status, bytes int, tx, reason string) {
	if pay.id != "" {
		pay.state, pay.status = state, status
		pay.x.finishPayment(pay.id, state, status, bytes, tx, reason)
	}
}

// sent reports whether the payment left our hands without an answer: the
// upstream holds it and may settle it (unknown), or refused it after
// receiving it (rejected).
func (pay *payment) sent() bool { return pay.state == "unknown" || pay.state == "rejected" }

// settle lowers the reserved amount to what the upstream says it charged,
// when that is less (a key-based bundler that bills after the call).
func (pay *payment) settle(amount int64) {
	if pay.id == "" || amount <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = pay.x.db.ExecContext(ctx, "UPDATE x402_payments SET amount=? WHERE id=? AND state='signed' AND amount>=?", amount, pay.id, amount)
}

// reservation is one row of x402_payments before it is committed. Network
// and Asset name the rail: a CAIP-2 chain and a token for x402, the
// bundler's name and "USD" for a key-based bundler.
type reservation struct {
	id          string
	amount      int64 // micro-USD (atomic USDC)
	network     string
	asset       string
	payTo       string
	nonce       string // unique per payment
	validBefore int64
}

// randomNonce is 32 random bytes as 0x-hex.
func randomNonce() (string, [32]byte, error) {
	var n [32]byte
	if _, err := rand.Read(n[:]); err != nil {
		return "", n, err
	}
	return "0x" + hex.EncodeToString(n[:]), n, nil
}
