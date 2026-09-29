package services

// x402 is a relay: an agent spends SwarmMemo credits on a pay-per-call API
// from the x402 Bazaar, and SwarmMemo pays that API in USDC from its own hot
// wallet. Most agents cannot hold a wallet; this lets them use x402 anyway.
//
// Safety line. Only resources in the operator's allowlist can be called: a
// fixed URL and method, a pinned recipient (payTo) and a maximum price per
// resource. The agent chooses an allowlisted id, query values for names the
// entry allows, and a JSON body where the entry allows one; never a URL, a
// path, a header or a method. Requests go out through the board's SSRF-safe
// dialer (the webhook dialer: public addresses only, checked after DNS),
// follow no redirect, use no proxy, and are bounded in time and size. The
// response is returned as data, never rendered.
//
// Money. Before any signature, one INSERT … SELECT reserves the amount
// against three caps (per call, per agent per UTC day, global per UTC day)
// or inserts nothing. Every authorization ever signed counts against the
// caps for good, whatever the upstream later says, because an upstream that
// holds a signed authorization can settle it until it expires. One call
// signs at most one authorization (UNIQUE(account, request_key)), and a
// payment the upstream rejects is never retried. The kill switch (config
// "paused", or the kill file existing) stops every new payment.
//
// Charging. The agent's quote is the resource's allowlisted maximum, priced
// with the "x402.call" parameter where the measured size is the amount in
// the asset's atomic units (micro-USDC). The agent is charged the price of
// what was actually paid, when it gets the response or when the paid
// response was too large and discarded (the agent chose its size); a
// rejected, failed or timed-out call is refunded in full and its USDC loss,
// if any, is ours, bounded by the caps. An account with X402DiscardsPerDay
// discarded answers today is refused before any request until 00:00 UTC.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/allowance"
)

// x402 bounds.
const (
	X402ArgsMax              = 4 << 10  // the published service_args_bytes
	X402ResponseBytesMax     = 12 << 10 // base64 of it and the wrapper fit X402StoredBodyBytes
	X402ResponseBytesDefault = 8 << 10
	// X402StoredBodyBytes bounds an x402 call record: the response as base64
	// (4/3 of X402ResponseBytesMax), the receipt and the wrapper.
	X402StoredBodyBytes = StoredBodyMaxBytes
	// X402DiscardsPerDay is how many paid calls a day an account may have
	// discarded as too large before its next call is refused up front,
	// before any payment.
	X402DiscardsPerDay  = 2
	X402TimeoutMax      = 20 * time.Second
	X402TimeoutDefault  = 10 * time.Second
	X402ResourcesMax    = 256
	X402QueryParamsMax  = 16
	X402QueryValueBytes = 512
	X402URLBytes        = 2048
	// Ceilings no config can exceed, in whole tokens: a typo in the caps
	// cannot turn a $2 budget into $2,000.
	X402GlobalDailyCeiling = 20
	X402PerCallCeiling     = 1
	// The authorization is valid from ten minutes ago (clock skew, as the
	// reference client does) until at most five minutes from now.
	x402ValidAfterSkew = 600
	x402WindowMin      = 60
	x402WindowMax      = 300
	x402MaxDuration    = 45 * time.Second
)

// X402Config is the provider's loaded, validated configuration
// (LoadX402Config). Amounts are in the asset's atomic units.
type X402Config struct {
	Network      string // CAIP-2, "eip155:8453"
	ChainID      int64
	Asset        EVMAddress
	AssetName    string // the token's EIP-712 domain name, "USD Coin"
	AssetVersion string // its EIP-712 domain version, "2"
	Decimals     int
	PerCall      int64
	AgentDaily   int64
	GlobalDaily  int64
	Paused       bool
	KillFile     string
	// AllowlistVersion and Resources are the operator's allowlist.
	AllowlistVersion int64
	Resources        []X402Resource
	Signer           X402Signer
	// rootCAs replaces the system roots (tests only).
	rootCAs *x509.CertPool
}

// X402Resource is one allowlisted pay-per-call endpoint.
type X402Resource struct {
	ID               string
	URL              string
	Method           string // GET or POST
	PayTo            EVMAddress
	MaxAmount        int64    // the most one call may pay, atomic units
	Query            []string // query parameter names the agent may set
	Body             bool     // POST: the agent may send a JSON object body
	MaxResponseBytes int
	Timeout          time.Duration
	Summary          string
	Source           string // where the entry came from, e.g. "bazaar 2026-09-29"
}

// x402 is the provider.
type x402 struct {
	cfg    *X402Config
	db     *sql.DB
	byID   map[string]*X402Resource
	client *http.Client
}

func newX402(d Deps) Provider {
	x := &x402{}
	if d.X402 == nil || d.DB == nil || d.Dial == nil || d.X402.Signer == nil {
		return x // not configured: listed if enabled, but every call is service_unavailable
	}
	x.cfg, x.db = d.X402, d.DB
	x.byID = map[string]*X402Resource{}
	for i := range x.cfg.Resources {
		x.byID[x.cfg.Resources[i].ID] = &x.cfg.Resources[i]
	}
	x.client = &http.Client{
		// A redirect is a second address nobody allowlisted: never followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy:                  nil, // no proxy environment can become a bypass
			DialContext:            d.Dial,
			TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: x.cfg.rootCAs},
			TLSHandshakeTimeout:    5 * time.Second,
			ResponseHeaderTimeout:  X402TimeoutMax,
			MaxResponseHeaderBytes: 64 << 10,
			DisableKeepAlives:      true,
			MaxIdleConns:           0,
		},
	}
	return x
}

func (*x402) Schema() string {
	return `
CREATE TABLE IF NOT EXISTS x402_payments (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, request_key TEXT NOT NULL, resource TEXT NOT NULL,
 day INTEGER NOT NULL, amount INTEGER NOT NULL CHECK(amount > 0), network TEXT NOT NULL, asset TEXT NOT NULL,
 pay_to TEXT NOT NULL, nonce TEXT NOT NULL UNIQUE, valid_before INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('signed','paid','rejected','unknown','failed')),
 http_status INTEGER NOT NULL DEFAULT 0, response_bytes INTEGER NOT NULL DEFAULT 0,
 transaction_hash TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '',
 allowlist_version INTEGER NOT NULL, created_at INTEGER NOT NULL, finished_at INTEGER NOT NULL DEFAULT 0,
 UNIQUE(account,request_key));
CREATE INDEX IF NOT EXISTS x402_payments_day ON x402_payments(day,account);
`
}

func (*x402) Describe() Descriptor {
	return Descriptor{
		ID:      "x402",
		Summary: `Pay-per-call APIs from the x402 Bazaar without a wallet: SwarmMemo pays the API in USDC and charges you credits (the price is the USDC amount in micro-USDC, plus a margin). Only operator-allowlisted resources; service.read method "resources" lists them with their maximum prices and today's budget.`,
		Title:   "x402 relay", Topic: "Search and data",
		Line: "Call operator-allowlisted pay-per-call APIs from the x402 Bazaar without a wallet: SwarmMemo pays in USDC and charges you credit.",
		Limits: []Limit{
			{"x402_query_params", X402QueryParamsMax, "", "Query parameters of one call"},
			{"x402_query_value_bytes", X402QueryValueBytes, "bytes", "One query value"},
			{"x402_response_bytes", X402ResponseBytesMax, "bytes", "Response body returned"},
		},
		Mode: Remote,
		Methods: []Method{
			{Name: "call", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: X402ArgsMax, Price: Price{Base: 100, PerByte: 1, PerKiB: 100},
				Line:           "Call one allowlisted resource; charged when the paid response arrives, even one too large to keep.",
				PriceNote:      "base + per_byte × the API's price in micro-USDC + per_kib per 1,024 of it, in credit; the resources read lists each resource's max_cost",
				ExampleMaxCost: 5000,
				Args: []Arg{
					{"resource", "string", true, "an id from the resources read"},
					{"query", "object", false, "string values for the resource's query names"},
					{"body", "object", false, "a JSON body, for resources that take one"},
				},
				Example: json.RawMessage(`{"resource":"RESOURCE_ID","query":{"q":"agent message boards"}}`)},
			{Name: "resources", ArgsMax: 64, Line: "The allowlist: each resource with its query names, maximum price and today's budget."},
		},
		MaxDuration:   x402MaxDuration,
		StoredBodyMax: X402StoredBodyBytes,
	}
}

type x402Args struct {
	Resource string            `json:"resource"`
	Query    map[string]string `json:"query"`
	Body     json.RawMessage   `json:"body"`
}

// x402Plan is one validated call: the resource and the exact request.
type x402Plan struct {
	res  *X402Resource
	url  string
	body []byte // POST only
	max  int64  // atomic units: min(allowlisted maximum, per-call cap)
}

func (x *x402) plan(c Call) (x402Plan, error) {
	if x.cfg == nil {
		return x402Plan{}, refusal("service_unavailable")
	}
	var a x402Args
	if err := StrictObject(c.Args, &a); err != nil {
		return x402Plan{}, err
	}
	res, ok := x.byID[a.Resource]
	if !ok {
		return x402Plan{}, refusal("x402_unknown_resource")
	}
	u, err := url.Parse(res.URL)
	if err != nil {
		return x402Plan{}, refusal("service_unavailable")
	}
	q := u.Query()
	if len(a.Query) > X402QueryParamsMax {
		return x402Plan{}, refusal("invalid_service_data")
	}
	for k, v := range a.Query {
		if !slices.Contains(res.Query, k) || q.Has(k) || len(v) > X402QueryValueBytes || !utf8.ValidString(v) {
			return x402Plan{}, refusal("invalid_service_data")
		}
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	p := x402Plan{res: res, url: u.String(), max: min(res.MaxAmount, x.cfg.PerCall)}
	if len(p.url) > X402URLBytes {
		return x402Plan{}, refusal("invalid_service_data")
	}
	switch {
	case len(a.Body) > 0 && (!res.Body || !isObject(a.Body)):
		return x402Plan{}, refusal("invalid_service_data")
	case res.Method == http.MethodPost:
		var b bytes.Buffer
		if len(a.Body) == 0 {
			b.WriteString("{}")
		} else if err := json.Compact(&b, a.Body); err != nil {
			return x402Plan{}, refusal("invalid_service_data")
		}
		p.body = b.Bytes()
	}
	return p, nil
}

func (x *x402) Quote(c Call) (Quote, error) {
	p, err := x.plan(c)
	if err != nil {
		return Quote{}, err
	}
	if x.cfg.Paused {
		return Quote{}, refusal("service_unavailable")
	}
	return Quote{Resource: allowance.Credit, Max: c.Price.For(p.max)}, nil
}

// paused is the kill switch: the config flag, or the kill file existing. An
// error other than "does not exist" also stops payments (fail closed).
func (x *x402) paused() bool {
	if x.cfg == nil || x.cfg.Paused {
		return true
	}
	if x.cfg.KillFile == "" {
		return false
	}
	_, err := os.Stat(x.cfg.KillFile)
	return !errors.Is(err, fs.ErrNotExist)
}

// x402Response is one bounded upstream response.
type x402Response struct {
	status      int
	header      http.Header
	body        []byte
	over        bool // the body was longer than the limit
	contentType string
}

func (x *x402) fetch(ctx context.Context, p x402Plan, payName, payValue string, limit int) (x402Response, error) {
	ctx, cancel := context.WithTimeout(ctx, p.res.Timeout)
	defer cancel()
	var body io.Reader
	if p.body != nil {
		body = bytes.NewReader(p.body)
	}
	req, err := http.NewRequestWithContext(ctx, p.res.Method, p.url, body)
	if err != nil {
		return x402Response{}, err
	}
	req.Header.Set("User-Agent", "SwarmMemo-x402/1 (+https://swarmmemo.com/protocol.md)")
	req.Header.Set("Accept", "application/json, text/plain;q=0.9, */*;q=0.1")
	if p.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if payName != "" {
		req.Header.Set(payName, payValue)
	}
	resp, err := x.client.Do(req)
	if err != nil {
		return x402Response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return x402Response{}, err
	}
	r := x402Response{status: resp.StatusCode, header: resp.Header, body: raw, contentType: resp.Header.Get("Content-Type")}
	if len(raw) > limit {
		r.body, r.over = nil, true
	}
	return r, nil
}

func (x *x402) Run(ctx context.Context, tx *sql.Tx, c Call) (Result, error) {
	if tx != nil {
		return Result{}, errors.New("services: x402 runs after commit")
	}
	p, err := x.plan(c)
	if err != nil {
		return Result{}, err
	}
	if x.paused() {
		return Result{}, refusal("service_unavailable")
	}
	// Refused up front, before any request: an account whose paid answers
	// keep coming back too large (it controls their size through the query).
	var discards int
	if err = x.db.QueryRowContext(ctx, "SELECT count(*) FROM x402_payments WHERE day=? AND account=? AND reason='response_too_large'", c.Now/86400, c.Subject.ID).Scan(&discards); err != nil {
		return Result{}, refusal("upstream_failed")
	}
	if discards >= X402DiscardsPerDay {
		return Result{}, &allowance.Err{Code: "x402_response_too_large", RetryAfter: int(86400 - c.Now%86400)}
	}
	first, err := x.fetch(ctx, p, "", "", max(x402RequiredBodyBytes, p.res.MaxResponseBytes))
	if err != nil {
		return Result{}, refusal("upstream_failed")
	}
	switch {
	case first.status >= 200 && first.status < 300: // free today: nothing to pay
		if first.over || len(first.body) > p.res.MaxResponseBytes {
			return Result{}, refusal("x402_response_too_large")
		}
		return x.result(c, p, first, nil)
	case first.status != http.StatusPaymentRequired:
		return Result{}, refusal("upstream_failed")
	}
	required, ok := parseX402Required(first.header.Get(x402V2Required), first.body)
	if !ok {
		return Result{}, refusal("x402_not_payable")
	}
	choice, err := chooseX402(required, x402Want{Network: x.cfg.Network, Asset: x.cfg.Asset, AssetName: x.cfg.AssetName, AssetVersion: x.cfg.AssetVersion, PayTo: p.res.PayTo, Max: p.max})
	if err != nil {
		return Result{}, err
	}
	if x.paused() {
		return Result{}, refusal("service_unavailable")
	}
	now := time.Now().Unix() // the chain's clock, not the store's
	window := min(max(choice.TimeoutSecs, x402WindowMin), x402WindowMax)
	auth := TransferAuthorization{From: x.cfg.Signer.Address(), To: choice.PayTo, Value: choice.Amount, ValidAfter: now - x402ValidAfterSkew, ValidBefore: now + window}
	if _, err = rand.Read(auth.Nonce[:]); err != nil {
		return Result{}, refusal("upstream_failed")
	}
	payID := newCallID()
	if err = x.reserve(ctx, c, p, payID, auth); err != nil {
		return Result{}, err
	}
	domain := EIP712Domain{Name: x.cfg.AssetName, Version: x.cfg.AssetVersion, ChainID: x.cfg.ChainID, VerifyingContract: x.cfg.Asset}
	sig, err := x.cfg.Signer.SignAuthorization(domain, auth)
	var name, value string
	if err == nil {
		name, value, err = x402PaymentHeader(required, choice, auth, sig)
	}
	if err != nil {
		x.finishPayment(payID, "failed", 0, 0, "", "signing")
		return Result{}, refusal("upstream_failed")
	}
	paid, err := x.fetch(ctx, p, name, value, p.res.MaxResponseBytes)
	if err != nil {
		// The upstream holds a valid authorization and may have settled it:
		// the outcome is unknown, the caps keep counting it, the agent is
		// refunded.
		x.finishPayment(payID, "unknown", 0, 0, "", "no_response")
		return Result{}, refusal("upstream_failed")
	}
	settled := parseX402Settlement(paid.header.Get(x402V2Response), paid.header.Get(x402V1Response))
	switch {
	case paid.status == http.StatusPaymentRequired:
		x.finishPayment(payID, "rejected", paid.status, len(paid.body), settled.Transaction, settled.Reason)
		return Result{}, refusal("x402_payment_rejected")
	case paid.status < 200 || paid.status >= 300:
		x.finishPayment(payID, "unknown", paid.status, len(paid.body), settled.Transaction, "http_status")
		return Result{}, refusal("upstream_failed")
	}
	receipt := &x402Receipt{
		Amount: strconv.FormatInt(choice.Amount, 10), Price: formatUnits(choice.Amount, x.cfg.Decimals), Asset: x.cfg.Asset.String(),
		Network: x.cfg.Network, PayTo: choice.PayTo.String(), Payer: auth.From.String(), Nonce: "0x" + hex.EncodeToString(auth.Nonce[:]),
		Transaction: settled.Transaction, X402Version: choice.Version,
	}
	if paid.over {
		// We paid for it: the answer is discarded, but the call is charged,
		// never refunded (security review 1.20, M1), and the account's
		// discards today count against it before its next payment.
		x.finishPayment(payID, "paid", paid.status, p.res.MaxResponseBytes+1, settled.Transaction, "response_too_large")
		return x.discarded(c, p, paid.status, p.res.MaxResponseBytes+1, receipt), nil
	}
	res, err := x.result(c, p, paid, receipt)
	if err != nil {
		x.finishPayment(payID, "paid", paid.status, len(paid.body), settled.Transaction, "response_too_large")
		return x.discarded(c, p, paid.status, len(paid.body), receipt), nil
	}
	x.finishPayment(payID, "paid", paid.status, len(paid.body), settled.Transaction, "")
	return res, nil
}

// reserve records the authorization before it is signed, in one statement
// that inserts nothing when the amount would exceed the agent's or the
// global daily cap. A second payment for the same call is refused by
// UNIQUE(account, request_key).
func (x *x402) reserve(ctx context.Context, c Call, p x402Plan, id string, a TransferAuthorization) error {
	if a.Value <= 0 || a.Value > x.cfg.PerCall || a.Value > p.res.MaxAmount {
		return refusal("x402_price_changed")
	}
	day := c.Now / 86400
	res, err := x.db.ExecContext(ctx, `INSERT INTO x402_payments(id,account,request_key,resource,day,amount,network,asset,pay_to,nonce,valid_before,state,allowlist_version,created_at)
SELECT ?,?,?,?,?,?,?,?,?,?,?,'signed',?,?
WHERE (SELECT COALESCE(SUM(amount),0) FROM x402_payments WHERE day=?) + ? <= ?
  AND (SELECT COALESCE(SUM(amount),0) FROM x402_payments WHERE day=? AND account=?) + ? <= ?
  AND NOT EXISTS (SELECT 1 FROM x402_payments WHERE account=? AND request_key=?)`,
		id, c.Subject.ID, c.RequestKey, p.res.ID, day, a.Value, x.cfg.Network, x.cfg.Asset.String(), a.To.String(), "0x"+hex.EncodeToString(a.Nonce[:]), a.ValidBefore, x.cfg.AllowlistVersion, c.Now,
		day, a.Value, x.cfg.GlobalDaily, day, c.Subject.ID, a.Value, x.cfg.AgentDaily, c.Subject.ID, c.RequestKey)
	if err != nil {
		return refusal("upstream_failed") // fail closed: no row, no signature
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		var exists int
		if x.db.QueryRowContext(ctx, "SELECT count(*) FROM x402_payments WHERE account=? AND request_key=?", c.Subject.ID, c.RequestKey).Scan(&exists) == nil && exists > 0 {
			return refusal("upstream_failed") // never a second payment for one call
		}
		return &allowance.Err{Code: "x402_cap_reached", RetryAfter: int(86400 - c.Now%86400)}
	}
	return nil
}

// finishPayment records what happened after signing. It runs even when the
// call's context has ended, briefly; a failure leaves the row "signed", which
// the caps count exactly the same.
func (x *x402) finishPayment(id, state string, status, bytes int, tx, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = x.db.ExecContext(ctx, "UPDATE x402_payments SET state=?, http_status=?, response_bytes=?, transaction_hash=?, reason=?, finished_at=? WHERE id=? AND state='signed'",
		state, status, bytes, tx, reason, time.Now().Unix(), id)
}

type x402Receipt struct {
	Amount      string `json:"amount"` // atomic units
	Price       string `json:"price"`  // the same, in whole tokens
	Asset       string `json:"asset"`
	Network     string `json:"network"`
	PayTo       string `json:"pay_to"`
	Payer       string `json:"payer"`
	Nonce       string `json:"nonce"`
	Transaction string `json:"transaction,omitempty"`
	X402Version int    `json:"x402_version"`
}

type x402Out struct {
	Resource         string          `json:"resource"`
	AllowlistVersion int64           `json:"allowlist_version"`
	Status           int             `json:"status"`
	ContentType      string          `json:"content_type,omitempty"`
	Encoding         string          `json:"encoding"` // json, text or base64
	Body             json.RawMessage `json:"body"`
	Bytes            int             `json:"bytes"`
	Payment          *x402Receipt    `json:"payment,omitempty"`
}

var mimeTypeRE = regexp.MustCompile(`^[a-z0-9!#$&^_.+-]{1,64}/[a-z0-9!#$&^_.+-]{1,64}$`)

// result is the agent's answer: the upstream's bytes as data (JSON inline
// when it is JSON, text when it is UTF-8 text, else base64), never rendered.
func (x *x402) result(c Call, p x402Plan, r x402Response, receipt *x402Receipt) (Result, error) {
	out := x402Out{Resource: p.res.ID, AllowlistVersion: x.cfg.AllowlistVersion, Status: r.status, Bytes: len(r.body), Payment: receipt}
	mt, _, _ := mime.ParseMediaType(r.contentType)
	mt = strings.ToLower(mt)
	if mimeTypeRE.MatchString(mt) {
		out.ContentType = mt
	}
	// No HTML escaping (security review 1.20, M2): '<', '>' and '&' stay one
	// byte, so a JSON answer never grows here, and text that escaping would
	// grow past base64 goes as base64. The body is then at most 4/3 of the
	// response limit plus the wrapper, inside X402StoredBodyBytes.
	var compact bytes.Buffer
	switch {
	case len(bytes.TrimSpace(r.body)) > 0 && json.Valid(r.body) && utf8.Valid(r.body) && json.Compact(&compact, r.body) == nil:
		out.Encoding, out.Body = "json", compact.Bytes()
	case utf8.Valid(r.body) && (strings.HasPrefix(mt, "text/") || mt == ""):
		out.Encoding, out.Body = "text", marshalNoEscape(string(r.body))
		if len(out.Body) > base64.StdEncoding.EncodedLen(len(r.body))+2 {
			out.Encoding, out.Body = "base64", marshalNoEscape(base64.StdEncoding.EncodeToString(r.body))
		}
	default:
		out.Encoding, out.Body = "base64", marshalNoEscape(base64.StdEncoding.EncodeToString(r.body))
	}
	body := marshalNoEscape(out)
	if body == nil || len(body) > X402StoredBodyBytes {
		return Result{}, refusal("x402_response_too_large")
	}
	amount := int64(0)
	public := map[string]any{"resource": p.res.ID, "http_status": r.status, "response_bytes": len(r.body)}
	if receipt != nil {
		amount, _ = strconv.ParseInt(receipt.Amount, 10, 64)
		public["amount"] = receipt.Amount // not the transaction: it would tie the agent to our wallet's history
	}
	pub, _ := json.Marshal(public)
	return Result{Body: body, Used: c.Price.For(amount), Public: pub}, nil
}

// marshalNoEscape is json.Marshal without HTML escaping; nil on error.
func marshalNoEscape(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil
	}
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'})
}

// discarded is the answer to a paid call whose response went over the
// resource's limit: no body, the payment receipt, and the call's price.
func (x *x402) discarded(c Call, p x402Plan, status, n int, receipt *x402Receipt) Result {
	out := x402Out{Resource: p.res.ID, AllowlistVersion: x.cfg.AllowlistVersion, Status: status, Encoding: "discarded", Body: json.RawMessage("null"), Bytes: n, Payment: receipt}
	amount, _ := strconv.ParseInt(receipt.Amount, 10, 64)
	pub, _ := json.Marshal(map[string]any{"resource": p.res.ID, "http_status": status, "response_bytes": n, "amount": receipt.Amount, "discarded": true})
	return Result{Body: marshalNoEscape(out), Used: c.Price.For(amount), Public: pub}
}

// Read serves "resources": the allowlist with maximum prices in USDC and in
// credits, the caps, and what is left of today's budget.
func (x *x402) Read(ctx context.Context, q allowance.Querier, c Call) (json.RawMessage, error) {
	if x.cfg == nil {
		return nil, refusal("service_unavailable")
	}
	if err := StrictObject(c.Args, &struct{}{}); err != nil {
		return nil, err
	}
	price := DefaultPrices()["x402.call"]
	if c.Prices != nil {
		price = c.Prices["x402.call"]
	}
	day := c.Now / 86400
	var globalSpent, yours int64
	if err := q.QueryRowContext(ctx, "SELECT COALESCE(SUM(amount),0), COALESCE(SUM(CASE WHEN account=? THEN amount ELSE 0 END),0) FROM x402_payments WHERE day=?", c.Subject.ID, day).Scan(&globalSpent, &yours); err != nil {
		return nil, err
	}
	resources := []any{}
	for _, r := range x.cfg.Resources {
		m := min(r.MaxAmount, x.cfg.PerCall)
		query := r.Query
		if query == nil {
			query = []string{}
		}
		resources = append(resources, map[string]any{
			"id": r.ID, "summary": r.Summary, "method": r.Method, "query": query, "body": r.Body,
			"max_price": formatUnits(m, x.cfg.Decimals), "max_cost": price.For(m), "max_response_bytes": r.MaxResponseBytes,
		})
	}
	today := map[string]any{"global_spent": formatUnits(globalSpent, x.cfg.Decimals), "global_remaining": formatUnits(max(0, x.cfg.GlobalDaily-globalSpent), x.cfg.Decimals)}
	if c.Subject.Signed {
		today["your_spent"], today["your_remaining"] = formatUnits(yours, x.cfg.Decimals), formatUnits(max(0, x.cfg.AgentDaily-yours), x.cfg.Decimals)
	}
	out, err := json.Marshal(map[string]any{
		"allowlist_version": x.cfg.AllowlistVersion, "network": x.cfg.Network, "asset": x.cfg.Asset.String(), "asset_name": x.cfg.AssetName,
		"paused": x.paused(), "price": price, "resources": resources, "today": today,
		"caps": map[string]any{"per_call": formatUnits(x.cfg.PerCall, x.cfg.Decimals), "agent_daily": formatUnits(x.cfg.AgentDaily, x.cfg.Decimals), "global_daily": formatUnits(x.cfg.GlobalDaily, x.cfg.Decimals)},
	})
	return out, err
}

// formatUnits writes atomic units as a decimal in whole tokens: 1500 with 6
// decimals is "0.0015".
func formatUnits(n int64, decimals int) string {
	if n < 0 {
		return "-" + formatUnits(-n, decimals)
	}
	s := strconv.FormatInt(n, 10)
	if decimals == 0 {
		return s
	}
	if len(s) <= decimals {
		s = strings.Repeat("0", decimals-len(s)+1) + s
	}
	whole, frac := s[:len(s)-decimals], strings.TrimRight(s[len(s)-decimals:], "0")
	if frac == "" {
		return whole
	}
	return whole + "." + frac
}

var decimalRE = regexp.MustCompile(`^(0|[1-9][0-9]{0,6})(\.[0-9]{1,18})?$`)

// parseUnits reads a decimal amount of whole tokens ("0.05") into atomic
// units, refusing more fraction digits than the asset has.
func parseUnits(s string, decimals int) (int64, bool) {
	if !decimalRE.MatchString(s) {
		return 0, false
	}
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > decimals {
		return 0, false
	}
	n, err := strconv.ParseInt(whole+frac+strings.Repeat("0", decimals-len(frac)), 10, 64)
	return n, err == nil
}

// Operator files.

type x402ConfigFile struct {
	Schema        json.Number `json:"schema"`
	Enabled       bool        `json:"enabled"`
	Paused        bool        `json:"paused"`
	KillFile      string      `json:"kill_file"`
	WalletKeyFile string      `json:"wallet_key_file"`
	Network       string      `json:"network"`
	Asset         string      `json:"asset"`
	AssetName     string      `json:"asset_name"`
	AssetVersion  string      `json:"asset_version"`
	Decimals      json.Number `json:"decimals"`
	Caps          struct {
		GlobalDaily string `json:"global_daily"`
		AgentDaily  string `json:"agent_daily"`
		PerCall     string `json:"per_call"`
	} `json:"caps"`
	AllowlistFile string `json:"allowlist_file"`
}

type x402AllowlistFile struct {
	Schema    json.Number `json:"schema"`
	Version   json.Number `json:"version"`
	Resources []struct {
		ID               string      `json:"id"`
		URL              string      `json:"url"`
		Method           string      `json:"method"`
		PayTo            string      `json:"pay_to"`
		MaxPrice         string      `json:"max_price"`
		Query            []string    `json:"query"`
		Body             bool        `json:"body"`
		MaxResponseBytes json.Number `json:"max_response_bytes"`
		TimeoutSeconds   json.Number `json:"timeout_seconds"`
		Summary          string      `json:"summary"`
		Source           string      `json:"source"`
	} `json:"resources"`
}

var (
	x402IDRE        = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	x402ParamRE     = regexp.MustCompile(`^[A-Za-z0-9_.\-\[\]]{1,64}$`)
	x402NetworkRE   = regexp.MustCompile(`^eip155:([1-9][0-9]{0,11})$`)
	x402TokenNameRE = regexp.MustCompile(`^[A-Za-z0-9 ._()-]{1,64}$`)
)

// ErrX402Disabled is LoadX402Config's answer for a config with "enabled"
// false: x402 stays off.
var ErrX402Disabled = errors.New("x402: the config says enabled: false")

// LoadX402Config reads the operator's config file, the allowlist it names and
// the wallet key it names, and validates all three. Errors name the field,
// never a secret.
func LoadX402Config(path string) (*X402Config, error) {
	if path == "" {
		return nil, errors.New("x402: X402_CONFIG is not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("x402: read config: %w", err)
	}
	var f x402ConfigFile
	if err := StrictObject(raw, &f); err != nil {
		return nil, errors.New("x402: config is not strict JSON with only the documented fields")
	}
	if !f.Enabled {
		return nil, ErrX402Disabled
	}
	list, err := os.ReadFile(f.AllowlistFile)
	if err != nil {
		return nil, fmt.Errorf("x402: read allowlist_file: %w", err)
	}
	if f.WalletKeyFile == "" {
		return nil, errors.New("x402: wallet_key_file is required")
	}
	signer, err := LoadX402Signer(f.WalletKeyFile)
	if err != nil {
		return nil, err
	}
	return parseX402Config(f, list, signer)
}

// ParseX402Config validates a config and allowlist already read (tests and
// the operator's check command).
func ParseX402Config(config, allowlist []byte, signer X402Signer) (*X402Config, error) {
	var f x402ConfigFile
	if err := StrictObject(config, &f); err != nil {
		return nil, errors.New("x402: config is not strict JSON with only the documented fields")
	}
	return parseX402Config(f, allowlist, signer)
}

func parseX402Config(f x402ConfigFile, allowlist []byte, signer X402Signer) (*X402Config, error) {
	bad := func(field string) error { return fmt.Errorf("x402: config field %s is missing or invalid", field) }
	if f.Schema.String() != "1" {
		return nil, bad("schema (must be 1)")
	}
	c := &X402Config{Paused: f.Paused, KillFile: f.KillFile, Signer: signer, AssetName: f.AssetName, AssetVersion: f.AssetVersion}
	m := x402NetworkRE.FindStringSubmatch(f.Network)
	if m == nil {
		return nil, bad(`network (CAIP-2 "eip155:CHAIN_ID")`)
	}
	c.Network = f.Network
	c.ChainID, _ = strconv.ParseInt(m[1], 10, 64)
	var ok bool
	if c.Asset, ok = ParseEVMAddress(f.Asset); !ok {
		return nil, bad("asset")
	}
	if !x402TokenNameRE.MatchString(f.AssetName) {
		return nil, bad("asset_name")
	}
	if !x402TokenNameRE.MatchString(f.AssetVersion) {
		return nil, bad("asset_version")
	}
	d, err := strconv.Atoi(f.Decimals.String())
	if err != nil || d < 0 || d > 12 {
		return nil, bad("decimals (0 to 12)")
	}
	// USDC has 6 decimals on every chain: a typo would scale every compiled
	// ceiling by a power of ten (security review 1.20, L1).
	if f.AssetName == "USD Coin" && d != 6 {
		return nil, bad("decimals (USD Coin has 6)")
	}
	c.Decimals = d
	unit := int64(1)
	for range d {
		unit *= 10
	}
	caps := []struct {
		text, name, def string
		dst             *int64
	}{
		{f.Caps.GlobalDaily, "caps.global_daily", "2", &c.GlobalDaily},
		{f.Caps.AgentDaily, "caps.agent_daily", "0.25", &c.AgentDaily},
		{f.Caps.PerCall, "caps.per_call", "0.05", &c.PerCall},
	}
	for _, cp := range caps {
		text := cp.text
		if text == "" {
			text = cp.def
		}
		n, ok := parseUnits(text, d)
		if !ok || n <= 0 {
			return nil, bad(cp.name)
		}
		*cp.dst = n
	}
	if c.GlobalDaily > X402GlobalDailyCeiling*unit {
		return nil, fmt.Errorf("x402: caps.global_daily is above the compiled ceiling of %d", X402GlobalDailyCeiling)
	}
	if c.PerCall > X402PerCallCeiling*unit {
		return nil, fmt.Errorf("x402: caps.per_call is above the compiled ceiling of %d", X402PerCallCeiling)
	}
	if c.AgentDaily > c.GlobalDaily || c.PerCall > c.AgentDaily {
		return nil, errors.New("x402: caps must satisfy per_call <= agent_daily <= global_daily")
	}
	if signer == nil {
		return nil, bad("wallet_key_file")
	}
	if err := c.parseAllowlist(allowlist); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *X402Config) parseAllowlist(raw []byte) error {
	var f x402AllowlistFile
	if err := StrictObject(raw, &f); err != nil {
		return errors.New("x402: allowlist is not strict JSON with only the documented fields")
	}
	if f.Schema.String() != "1" {
		return errors.New("x402: allowlist schema must be 1")
	}
	v, err := strconv.ParseInt(f.Version.String(), 10, 64)
	if err != nil || v < 1 {
		return errors.New("x402: allowlist version must be a positive integer; raise it on every change")
	}
	c.AllowlistVersion = v
	if len(f.Resources) > X402ResourcesMax {
		return fmt.Errorf("x402: at most %d allowlisted resources", X402ResourcesMax)
	}
	seen := map[string]bool{}
	for i, r := range f.Resources {
		bad := func(field string) error {
			return fmt.Errorf("x402: allowlist resource %d (%q): %s is missing or invalid", i, r.ID, field)
		}
		if !x402IDRE.MatchString(r.ID) || seen[r.ID] {
			return bad("id (unique, lowercase letters, digits, . _ -)")
		}
		seen[r.ID] = true
		if err := checkX402URL(r.URL); err != nil {
			return bad("url (" + err.Error() + ")")
		}
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			return bad("method (GET or POST)")
		}
		if r.Body && r.Method != http.MethodPost {
			return bad("body (only for POST)")
		}
		payTo, ok := ParseEVMAddress(r.PayTo)
		if !ok || payTo == (EVMAddress{}) {
			return bad("pay_to")
		}
		amount, ok := parseUnits(r.MaxPrice, c.Decimals)
		if !ok || amount <= 0 || amount > c.PerCall {
			return bad("max_price (positive, at most caps.per_call)")
		}
		if len(r.Query) > X402QueryParamsMax {
			return bad("query (too many names)")
		}
		for _, name := range r.Query {
			if !x402ParamRE.MatchString(name) {
				return bad("query")
			}
		}
		res := X402Resource{ID: r.ID, URL: r.URL, Method: r.Method, PayTo: payTo, MaxAmount: amount, Query: slices.Clone(r.Query), Body: r.Body,
			MaxResponseBytes: X402ResponseBytesDefault, Timeout: X402TimeoutDefault, Summary: r.Summary, Source: r.Source}
		if r.MaxResponseBytes != "" {
			n, err := strconv.Atoi(r.MaxResponseBytes.String())
			if err != nil || n < 1 || n > X402ResponseBytesMax {
				return bad(fmt.Sprintf("max_response_bytes (1 to %d)", X402ResponseBytesMax))
			}
			res.MaxResponseBytes = n
		}
		if r.TimeoutSeconds != "" {
			n, err := strconv.Atoi(r.TimeoutSeconds.String())
			if err != nil || n < 1 || time.Duration(n)*time.Second > X402TimeoutMax {
				return bad("timeout_seconds (1 to 20)")
			}
			res.Timeout = time.Duration(n) * time.Second
		}
		if len(r.Summary) > 280 || !utf8.ValidString(r.Summary) || len(r.Source) > 200 || !utf8.ValidString(r.Source) {
			return bad("summary or source (too long)")
		}
		c.Resources = append(c.Resources, res)
	}
	return nil
}

// checkX402URL admits only a plain public HTTPS endpoint on port 443: no
// credentials, no fragment, no IP literal (the dialer still checks every
// address it connects to).
func checkX402URL(raw string) error {
	if raw == "" || len(raw) > X402URLBytes/2 || !utf8.ValidString(raw) || strings.ContainsAny(raw, " \t\r\n") {
		return errors.New("empty, long or with spaces")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Opaque != "" || u.Hostname() == "" {
		return errors.New("must be https://host/path")
	}
	if port := u.Port(); port != "" && port != "443" {
		return errors.New("port must be 443")
	}
	if net.ParseIP(u.Hostname()) != nil || !strings.Contains(u.Hostname(), ".") {
		return errors.New("host must be a public DNS name")
	}
	return nil
}
