package services

// Credit top-ups: SwarmMemo as an x402 server. An agent asks to top up N
// credits (1 credit = 1 micro-USDC); the board answers 402 with one x402 v2
// payment requirement (exact, EIP-3009, our network and asset, the
// operator's receiving address, amount N exactly, a short-lived quote bound
// to the agent's account); the agent retries with a signed
// transferWithAuthorization in PAYMENT-SIGNATURE (or X-PAYMENT), and the
// board credits paid credit only once the operator's facilitator has
// settled it on chain.
//
// This file is the pure, money-free part: the operator's config, the strict
// parser of a payment header, the check that a payment is exactly what we
// asked for, and the facilitator client (POST /verify, then /settle). It
// never holds a key: we only receive, and the facilitator broadcasts.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Top-up bounds and defaults, in credits (micro-USDC).
const (
	// TopupMinDefault is the smallest top-up by default: 0.10 USDC.
	TopupMinDefault = 100_000
	// TopupMaxDefault is the largest single top-up by default: 5 USDC.
	TopupMaxDefault = 5_000_000
	// TopupAccountDailyDefault is what one account may top up per UTC day
	// by default: 10 USDC.
	TopupAccountDailyDefault = 10_000_000
	// TopupBoardDailyDefault is what the whole board takes in top-ups per
	// UTC day, every account together, by default: 100 USDC.
	TopupBoardDailyDefault = 100_000_000
	// TopupCeiling bounds every limit the config sets (1,000 USDC): a typo
	// cannot open an unbounded inflow.
	TopupCeiling = 1_000_000_000
	// TopupQuoteSeconds is how long a payment requirement is good for.
	TopupQuoteSeconds = 300
	// TopupPaymentBytes bounds a payment header (base64), and the same
	// payment carried in a command's data.
	TopupPaymentBytes = 8 << 10
	// topupFacilitatorBytes bounds a facilitator answer we read.
	topupFacilitatorBytes = 16 << 10
	// topupFacilitatorTimeout bounds each facilitator call; settling waits
	// for the chain, so it is generous.
	topupFacilitatorTimeout = 30 * time.Second
	// topupValidBeforeMargin is how long an authorization must still be
	// valid when it reaches us, so it cannot lapse while it settles.
	topupValidBeforeMargin = 30
)

// TopupConfig is the loaded, validated top-up configuration
// (LoadTopupConfig). Amounts are credits, which are the asset's atomic
// units: the asset must be a 6-decimal USD token (USDC).
type TopupConfig struct {
	Network      string // CAIP-2, "eip155:8453"
	Asset        EVMAddress
	AssetName    string // its EIP-712 domain name, "USD Coin"
	AssetVersion string // its EIP-712 domain version, "2"
	// PayTo is the operator's receiving address. No key for it is ever
	// loaded: SwarmMemo only receives.
	PayTo          EVMAddress
	FacilitatorURL string
	// Min and Max bound one top-up; AccountDaily is what one account may
	// top up per UTC day, BoardDaily what every account together may.
	Min, Max, AccountDaily, BoardDaily int64
	facilitatorToken       string
	client                 *http.Client
}

type topupConfigFile struct {
	Schema               json.Number `json:"schema"`
	Enabled              bool        `json:"enabled"`
	Network              string      `json:"network"`
	Asset                string      `json:"asset"`
	AssetName            string      `json:"asset_name"`
	AssetVersion         string      `json:"asset_version"`
	PayTo                string      `json:"pay_to"`
	FacilitatorURL       string      `json:"facilitator_url"`
	FacilitatorTokenFile string      `json:"facilitator_token_file"`
	Limits               struct {
		Min          string `json:"min"`
		Max          string `json:"max"`
		AccountDaily string `json:"account_daily"`
		BoardDaily   string `json:"board_daily"`
	} `json:"limits"`
}

// ErrTopupDisabled is LoadTopupConfig's answer for a config with "enabled"
// false: top-ups stay off.
var ErrTopupDisabled = errors.New("topup: the config says enabled: false")

// LoadTopupConfig reads the operator's TOPUP_CONFIG file and, when it names
// one, the facilitator's bearer token file (owner-only, like a key). Errors
// name the field, never a secret.
func LoadTopupConfig(path string) (*TopupConfig, error) {
	if path == "" {
		return nil, errors.New("topup: TOPUP_CONFIG is not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("topup: read config: %w", err)
	}
	return ParseTopupConfig(raw, readPrivateFile)
}

// ParseTopupConfig validates a config already read; readKey reads the
// facilitator token file (nil: the config must not name one).
func ParseTopupConfig(raw []byte, readKey func(string) ([]byte, error)) (*TopupConfig, error) {
	var f topupConfigFile
	if err := StrictObject(raw, &f); err != nil {
		return nil, errors.New("topup: config is not strict JSON with only the documented fields")
	}
	if !f.Enabled {
		return nil, ErrTopupDisabled
	}
	bad := func(field string) error { return fmt.Errorf("topup: config field %s is missing or invalid", field) }
	if f.Schema.String() != "1" {
		return nil, bad("schema (must be 1)")
	}
	c := &TopupConfig{AssetName: f.AssetName, AssetVersion: f.AssetVersion}
	if !x402NetworkRE.MatchString(f.Network) {
		return nil, bad(`network (CAIP-2 "eip155:CHAIN_ID")`)
	}
	c.Network = f.Network
	var ok bool
	if c.Asset, ok = ParseEVMAddress(f.Asset); !ok || c.Asset == (EVMAddress{}) {
		return nil, bad("asset")
	}
	if !x402TokenNameRE.MatchString(f.AssetName) {
		return nil, bad("asset_name")
	}
	if !x402TokenNameRE.MatchString(f.AssetVersion) {
		return nil, bad("asset_version")
	}
	if c.PayTo, ok = ParseEVMAddress(f.PayTo); !ok || c.PayTo == (EVMAddress{}) || c.PayTo == c.Asset {
		return nil, bad("pay_to (the operator's receiving address)")
	}
	if err := checkX402URL(f.FacilitatorURL); err != nil {
		return nil, bad("facilitator_url (" + err.Error() + ")")
	}
	c.FacilitatorURL = strings.TrimRight(f.FacilitatorURL, "/")
	if f.FacilitatorTokenFile != "" {
		if readKey == nil {
			return nil, bad("facilitator_token_file")
		}
		token, err := readKey(f.FacilitatorTokenFile)
		if err != nil {
			return nil, fmt.Errorf("topup: facilitator_token_file: %w", err)
		}
		c.facilitatorToken = strings.TrimSpace(string(token))
		if c.facilitatorToken == "" || len(c.facilitatorToken) > 4096 || strings.ContainsAny(c.facilitatorToken, "\r\n\t ") || !utf8.ValidString(c.facilitatorToken) {
			return nil, bad("facilitator_token_file (one token on one line)")
		}
	}
	limits := []struct {
		text, name string
		def        int64
		dst        *int64
	}{
		{f.Limits.Min, "limits.min", TopupMinDefault, &c.Min},
		{f.Limits.Max, "limits.max", TopupMaxDefault, &c.Max},
		{f.Limits.AccountDaily, "limits.account_daily", TopupAccountDailyDefault, &c.AccountDaily},
		{f.Limits.BoardDaily, "limits.board_daily", TopupBoardDailyDefault, &c.BoardDaily},
	}
	for _, l := range limits {
		*l.dst = l.def
		if l.text == "" {
			continue
		}
		n, ok := parseUnits(l.text, 6)
		if !ok || n <= 0 || n > TopupCeiling {
			return nil, bad(l.name + " (a positive USDC amount, at most " + formatUnits(TopupCeiling, 6) + ")")
		}
		*l.dst = n
	}
	if c.Min > c.Max || c.Max > c.AccountDaily || c.AccountDaily > c.BoardDaily {
		return nil, errors.New("topup: limits must satisfy min <= max <= account_daily <= board_daily")
	}
	return c, nil
}

// UseTestFacilitator sends every facilitator call to url through client:
// an httptest server stands in for the facilitator. Tests only; nothing in
// a config file or the environment sets it.
func (c *TopupConfig) UseTestFacilitator(client *http.Client, url string) {
	c.client, c.FacilitatorURL = client, strings.TrimRight(url, "/")
}

// httpClient is the facilitator client: through dial (the board's SSRF-safe
// dialer), no proxy, no redirect, bounded in time.
func (c *TopupConfig) httpClient(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Client {
	if c.client != nil {
		return c.client
	}
	t := &http.Transport{
		Proxy:                  nil,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  topupFacilitatorTimeout,
		MaxResponseHeaderBytes: 64 << 10,
		DisableKeepAlives:      true,
	}
	if dial != nil {
		t.DialContext = dial
	}
	return &http.Client{Transport: t, Timeout: topupFacilitatorTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// TopupRequirement is one payment requirement we issue: Amount credits to
// PayTo, good until Expires, with Quote binding it to one account.
type TopupRequirement struct {
	Amount  int64
	Quote   string
	Expires int64
}

type topupExtraWire struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Quote   string `json:"quote"`
}

type topupRequirementWire struct {
	Scheme            string         `json:"scheme"`
	Network           string         `json:"network"`
	Amount            string         `json:"amount"`
	Asset             string         `json:"asset"`
	PayTo             string         `json:"payTo"`
	MaxTimeoutSeconds int64          `json:"maxTimeoutSeconds"`
	Extra             topupExtraWire `json:"extra"`
}

// wire is r as an x402 v2 PaymentRequirements object.
func (c *TopupConfig) wire(r TopupRequirement) topupRequirementWire {
	return topupRequirementWire{Scheme: "exact", Network: c.Network, Amount: strconv.FormatInt(r.Amount, 10), Asset: c.Asset.String(),
		PayTo: c.PayTo.String(), MaxTimeoutSeconds: TopupQuoteSeconds, Extra: topupExtraWire{Name: c.AssetName, Version: c.AssetVersion, Quote: r.Quote}}
}

// PaymentRequired is the x402 v2 PaymentRequired object for r, and its
// base64 encoding for the PAYMENT-REQUIRED header.
func (c *TopupConfig) PaymentRequired(r TopupRequirement, resourceURL, description string) (map[string]any, string) {
	body := map[string]any{
		"x402Version": 2,
		"error":       "payment required",
		"resource":    map[string]any{"url": resourceURL, "description": description, "mimeType": "application/json"},
		"accepts":     []topupRequirementWire{c.wire(r)},
	}
	raw, _ := json.Marshal(body)
	return body, base64.StdEncoding.EncodeToString(raw)
}

// TopupPayment is a parsed x402 v2 payment payload for the exact EVM
// scheme: what the payer accepted, and its signed EIP-3009 authorization.
type TopupPayment struct {
	// Accepted is the requirement the payer says it accepted.
	Scheme, Network, Asset, PayTo, Amount string
	Name, Version, Quote                  string
	// The authorization.
	From, To                EVMAddress
	Value                   int64
	ValidAfter, ValidBefore int64
	Nonce                   string // "0x" + 64 lowercase hex
	Signature               string // "0x" + 130 lowercase hex
	resource                json.RawMessage
}

// TopupError is a refusal of a top-up payment: Code is the public error
// code, Definite says nothing can have settled (no money moved).
type TopupError struct {
	Code     string
	Reason   string // a sanitised facilitator reason, or ""
	Definite bool
}

func (e *TopupError) Error() string {
	if e.Reason != "" {
		return "topup: " + e.Code + " (" + e.Reason + ")"
	}
	return "topup: " + e.Code
}

// TopupFacilitatorUnavailable is the reason of a payment the facilitator
// could not verify (down, refusing our credentials, or answering nonsense):
// nothing moved, so the same payment may be presented again.
const TopupFacilitatorUnavailable = "facilitator_unavailable"

func topupRefusal(code string) error { return &TopupError{Code: code, Definite: true} }

var (
	evmNonceRE     = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)
	evmSignatureRE = regexp.MustCompile(`^0x[0-9a-fA-F]{130}$`)
	unixSecondsRE  = regexp.MustCompile(`^(0|[1-9][0-9]{0,11})$`)
	topupQuoteRE   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
)

type topupPayloadWire struct {
	Version    json.Number     `json:"x402Version"`
	Resource   json.RawMessage `json:"resource"`
	Accepted   json.RawMessage `json:"accepted"`
	Payload    json.RawMessage `json:"payload"`
	Extensions json.RawMessage `json:"extensions"`
}

type topupAcceptedWire struct {
	Scheme            string          `json:"scheme"`
	Network           string          `json:"network"`
	Amount            string          `json:"amount"`
	Asset             string          `json:"asset"`
	PayTo             string          `json:"payTo"`
	MaxTimeoutSeconds json.Number     `json:"maxTimeoutSeconds"`
	Extra             json.RawMessage `json:"extra"`
}

type topupAcceptedExtraWire struct {
	Name                string `json:"name"`
	Version             string `json:"version"`
	Quote               string `json:"quote"`
	AssetTransferMethod string `json:"assetTransferMethod"`
}

// ParseTopupPayment reads a PAYMENT-SIGNATURE (or X-PAYMENT) header value:
// base64 of one x402 v2 payment payload for the exact scheme, every field
// well formed. The payload and its authorization are strict (no unknown
// field); the envelope's resource and extensions are bounded and never
// trusted. Any malformation is payment_invalid.
func ParseTopupPayment(header string) (TopupPayment, error) {
	invalid := topupRefusal("payment_invalid")
	header = strings.TrimSpace(header)
	if header == "" || len(header) > TopupPaymentBytes {
		return TopupPayment{}, invalid
	}
	raw, ok := decodeBase64JSON(header)
	if !ok || len(raw) > TopupPaymentBytes {
		return TopupPayment{}, invalid
	}
	var w topupPayloadWire
	if StrictObject(raw, &w) != nil || w.Version.String() != "2" || !isObject(w.Accepted) || !isObject(w.Payload) {
		return TopupPayment{}, invalid
	}
	if (len(w.Resource) > 0 && string(w.Resource) != "null" && (!isObject(w.Resource) || len(w.Resource) > x402RawFieldBytes)) ||
		(len(w.Extensions) > 0 && string(w.Extensions) != "null" && (!isObject(w.Extensions) || len(w.Extensions) > x402RawFieldBytes)) {
		return TopupPayment{}, invalid
	}
	var acc topupAcceptedWire
	if StrictObject(w.Accepted, &acc) != nil || !isObject(acc.Extra) {
		return TopupPayment{}, invalid
	}
	var extra topupAcceptedExtraWire
	if StrictObject(acc.Extra, &extra) != nil || !topupQuoteRE.MatchString(extra.Quote) {
		return TopupPayment{}, invalid
	}
	var pl x402ExactPayload
	if StrictObject(w.Payload, &pl) != nil {
		return TopupPayment{}, invalid
	}
	a := pl.Authorization
	p := TopupPayment{Scheme: acc.Scheme, Network: acc.Network, Asset: acc.Asset, PayTo: acc.PayTo, Amount: acc.Amount,
		Name: extra.Name, Version: extra.Version, Quote: extra.Quote}
	if extra.AssetTransferMethod != "" && extra.AssetTransferMethod != "eip3009" {
		return TopupPayment{}, invalid
	}
	if p.From, ok = ParseEVMAddress(a.From); !ok || p.From == (EVMAddress{}) {
		return TopupPayment{}, invalid
	}
	if p.To, ok = ParseEVMAddress(a.To); !ok {
		return TopupPayment{}, invalid
	}
	if !atomicAmountRE.MatchString(a.Value) || !unixSecondsRE.MatchString(a.ValidAfter) || !unixSecondsRE.MatchString(a.ValidBefore) {
		return TopupPayment{}, invalid
	}
	p.Value, _ = strconv.ParseInt(a.Value, 10, 64)
	p.ValidAfter, _ = strconv.ParseInt(a.ValidAfter, 10, 64)
	p.ValidBefore, _ = strconv.ParseInt(a.ValidBefore, 10, 64)
	if !evmNonceRE.MatchString(a.Nonce) || !evmSignatureRE.MatchString(pl.Signature) {
		return TopupPayment{}, invalid
	}
	p.Nonce, p.Signature = strings.ToLower(a.Nonce), strings.ToLower(pl.Signature)
	if isObject(w.Resource) {
		p.resource = w.Resource
	}
	return p, nil
}

// Check holds p to exactly what r asked: the exact scheme on our network,
// our asset and token domain, our receiving address in both the accepted
// requirement and the authorization, the amount to the unit, the quote we
// issued, and an authorization valid now and for long enough to settle.
// The quote's binding to the account and its expiry are the caller's
// (it holds the key); r carries what it verified.
func (c *TopupConfig) Check(p TopupPayment, r TopupRequirement, now int64) error {
	mismatch := topupRefusal("payment_mismatch")
	if p.Scheme != "exact" || p.Network != c.Network || p.Quote != r.Quote {
		return mismatch
	}
	if asset, ok := ParseEVMAddress(p.Asset); !ok || asset != c.Asset {
		return mismatch
	}
	if payTo, ok := ParseEVMAddress(p.PayTo); !ok || payTo != c.PayTo || p.To != c.PayTo {
		return mismatch
	}
	if (p.Name != "" && p.Name != c.AssetName) || (p.Version != "" && p.Version != c.AssetVersion) {
		return mismatch
	}
	if p.Amount != strconv.FormatInt(r.Amount, 10) || p.Value != r.Amount {
		return mismatch
	}
	if r.Expires < now || p.ValidBefore < now+topupValidBeforeMargin || p.ValidAfter > now || p.ValidAfter >= p.ValidBefore {
		return topupRefusal("payment_expired")
	}
	return nil
}

// facilitatorBody is the /verify and /settle request: our own requirement,
// never the payer's echo of it, and the payload rebuilt from what we parsed.
func (c *TopupConfig) facilitatorBody(p TopupPayment, r TopupRequirement) ([]byte, error) {
	req := c.wire(r)
	payload := map[string]any{
		"x402Version": 2,
		"accepted":    req,
		"payload": x402ExactPayload{Signature: p.Signature, Authorization: x402AuthorizationWire{
			From: p.From.String(), To: p.To.String(), Value: strconv.FormatInt(p.Value, 10),
			ValidAfter: strconv.FormatInt(p.ValidAfter, 10), ValidBefore: strconv.FormatInt(p.ValidBefore, 10), Nonce: p.Nonce}},
	}
	if p.resource != nil {
		payload["resource"] = p.resource
	}
	return json.Marshal(map[string]any{"x402Version": 2, "paymentPayload": payload, "paymentRequirements": req})
}

// TopupSettlement is a settled payment.
type TopupSettlement struct {
	Transaction string // "0x" + 64 lowercase hex
	Payer       EVMAddress
}

// Settle has the facilitator verify p against r and then settle it. It
// holds no database connection: the caller records the outcome after.
// A *TopupError with Definite set means nothing settled (a refusal the
// facilitator gave, or a failure before settling was asked); without it
// the outcome is unknown (the facilitator was asked to settle and gave no
// usable answer), and no credit may follow until the operator resolves it.
func (c *TopupConfig) Settle(ctx context.Context, dial func(ctx context.Context, network, addr string) (net.Conn, error), p TopupPayment, r TopupRequirement) (TopupSettlement, error) {
	body, err := c.facilitatorBody(p, r)
	if err != nil {
		return TopupSettlement{}, &TopupError{Code: "payment_unsettled", Reason: "encode", Definite: true}
	}
	client := c.httpClient(dial)
	var v struct {
		IsValid       *bool  `json:"isValid"`
		InvalidReason string `json:"invalidReason"`
		Payer         string `json:"payer"`
	}
	status, err := c.post(ctx, client, "/verify", body, &v)
	switch {
	case err != nil || status >= 500 || v.IsValid == nil:
		// Verifying moves nothing: the payer may present it again.
		return TopupSettlement{}, &TopupError{Code: "payment_unsettled", Reason: TopupFacilitatorUnavailable, Definite: true}
	case !*v.IsValid:
		return TopupSettlement{}, &TopupError{Code: "payment_rejected", Reason: sanitizeReason(v.InvalidReason), Definite: true}
	case v.Payer != "" && !samePayer(v.Payer, p.From):
		return TopupSettlement{}, &TopupError{Code: "payment_rejected", Reason: "payer_mismatch", Definite: true}
	}
	var s struct {
		Success     *bool  `json:"success"`
		ErrorReason string `json:"errorReason"`
		Transaction string `json:"transaction"`
		Network     string `json:"network"`
		Payer       string `json:"payer"`
	}
	status, err = c.post(ctx, client, "/settle", body, &s)
	switch {
	case err != nil || status >= 500 || s.Success == nil:
		return TopupSettlement{}, &TopupError{Code: "payment_unsettled", Reason: "settle_unknown"}
	case !*s.Success:
		return TopupSettlement{}, &TopupError{Code: "payment_rejected", Reason: sanitizeReason(s.ErrorReason), Definite: true}
	case !txHashRE.MatchString(s.Transaction) || (s.Network != "" && s.Network != c.Network) || (s.Payer != "" && !samePayer(s.Payer, p.From)):
		// It says settled, but not in terms we can record: money may have
		// moved, so this is for the operator, never a credit.
		return TopupSettlement{}, &TopupError{Code: "payment_unsettled", Reason: "settle_malformed"}
	}
	return TopupSettlement{Transaction: strings.ToLower(s.Transaction), Payer: p.From}, nil
}

func samePayer(s string, want EVMAddress) bool {
	a, ok := ParseEVMAddress(s)
	if !ok {
		// Mixed case that is not a valid checksum: compare it lowered.
		a, ok = ParseEVMAddress(strings.ToLower(s))
	}
	return ok && a == want
}

func sanitizeReason(s string) string {
	if reasonRE.MatchString(s) {
		return s
	}
	return "unspecified"
}

// post sends body to the facilitator's path and decodes a bounded JSON
// answer into dst; it returns the HTTP status. A 4xx answer is decoded too:
// facilitators state refusals that way.
func (c *TopupConfig) post(ctx context.Context, client *http.Client, path string, body []byte, dst any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, topupFacilitatorTimeout)
	defer cancel()
	u, err := url.Parse(c.FacilitatorURL + path)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.facilitatorToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.facilitatorToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, topupFacilitatorBytes+1))
	if err != nil {
		return resp.StatusCode, err
	}
	if len(raw) > topupFacilitatorBytes || resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return resp.StatusCode, errors.New("topup: facilitator answer too large or redirected")
	}
	if !boundedJSON(raw, topupFacilitatorBytes, dst) {
		return resp.StatusCode, errors.New("topup: facilitator answer is not a JSON object")
	}
	return resp.StatusCode, nil
}

// FormatUSDC is credits as a USDC amount: 100000 is "0.1".
func FormatUSDC(credits int64) string { return formatUnits(credits, 6) }
