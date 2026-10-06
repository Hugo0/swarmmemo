package services

// keyBundler is a key-based bundler: a marketplace API, which pays
// data providers itself and bills an account by plan, in credits of $0.001.
// An allowlist entry with "bundler":"frames" names a bundler tool by its id;
// a call becomes one POST /v1/tools/invoke with our bearer key, the agent's
// JSON body as the tool's arguments, max_usd at the entry's maximum and an
// idempotency key per call, so a retry never pays twice. The agent is
// charged what the bundler reports it charged (billing.charged_credits), or the
// maximum when it reports nothing; our account's billing block is removed
// from the answer. The key is read once from key_file and never printed;
// without it the bundler is not ready and its resources are unavailable.
//
// With "open": true the whole bundler catalogue is callable as well, without
// allowlist entries: keybundler_open.go.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// BundlerBaseURL is the bundler API's documented base.
const BundlerBaseURL = "https://api.frames.ag/v1"

// bundlerCreditMicroUSD is one bundler credit, $0.001, in micro-USD.
const bundlerCreditMicroUSD = 1000

type x402BundlersFile struct {
	KeyBundler *x402KeyBundlerFile `json:"frames"`
}

type x402KeyBundlerFile struct {
	KeyFile string `json:"key_file"`
	BaseURL string `json:"base_url"`
	// The open bundler catalogue (keybundler_open.go); absent or false keeps
	// the bundler to the allowlist's entries.
	Open          bool   `json:"open"`
	MaxPrice      string `json:"max_price"`
	AllowUnvetted bool   `json:"allow_unvetted"`
	OpenDaily     string `json:"open_daily"`
	ToolDaily     string `json:"tool_daily"`
	Anonymous     bool   `json:"anonymous"`
}

// parseBundlers builds the key-based bundlers the config names; readKey
// reads a key file (readPrivateFile: owner-only). A missing key leaves the
// bundler not ready; one others can read refuses the config.
func (c *X402Config) parseBundlers(f *x402BundlersFile, readKey func(string) ([]byte, error)) error {
	if f == nil || f.KeyBundler == nil {
		return nil
	}
	base := strings.TrimSuffix(f.KeyBundler.BaseURL, "/")
	if base == "" {
		base = BundlerBaseURL
	}
	if err := checkX402URL(base); err != nil {
		return fmt.Errorf("x402: config field bundlers.frames.base_url is invalid (%s)", err)
	}
	if f.KeyBundler.KeyFile == "" {
		return fmt.Errorf("x402: config field bundlers.frames.key_file is required")
	}
	b := &keyBundler{base: base, endpoint: base + "/tools/invoke"}
	open, err := c.parseBundlerOpen(f.KeyBundler)
	if err != nil {
		return err
	}
	b.open = open
	raw, err := readKey(f.KeyBundler.KeyFile)
	switch {
	case errors.Is(err, errKeyMode):
		return fmt.Errorf("x402: config field bundlers.frames.key_file must be a regular file only its owner can read (chmod 600)")
	case err == nil && len(raw) <= 4096:
		b.key = parseBearerKey(raw)
	}
	clear(raw)
	c.Bundlers = append(c.Bundlers, b)
	return nil
}

// parseBearerKey is a key file's one token: printable ASCII without spaces,
// 8 to 512 bytes; "" otherwise.
func parseBearerKey(raw []byte) string {
	key := strings.TrimSpace(string(raw))
	if len(key) < 8 || len(key) > 512 {
		return ""
	}
	for i := 0; i < len(key); i++ {
		if key[i] <= ' ' || key[i] > '~' {
			return ""
		}
	}
	return key
}

type keyBundler struct {
	base     string // the API's base URL, ".../v1"
	endpoint string
	key      string       // never printed: String hides it
	open     *BundlerOpen // nil: only allowlisted bundler tools
}

func (f *keyBundler) Name() string     { return "frames" }
func (f *keyBundler) Ready() bool      { return f.key != "" }
func (f *keyBundler) Endpoint() string { return f.endpoint }

// String and GoString keep the key out of every formatted config.
func (f *keyBundler) String() string   { return "frames(" + f.endpoint + ")" }
func (f *keyBundler) GoString() string { return f.String() }

// bundlerFeePercent is what the bundler adds to a tool's price on every call
// ("data that arrived + 15%").
const bundlerFeePercent = 15

// bundlerCost is the most the bundler bills for one call of a tool priced price
// (micro-USD): the price plus bundlerFeePercent, rounded up to whole credits.
// It is the one figure behind a tool's quote (tools_search and tools_get
// cost) and the most a call of it pays (bundlerGate caps max_usd at it).
func bundlerCost(price int64) int64 {
	if price <= 0 {
		return 0
	}
	withFee := price + (price*bundlerFeePercent+99)/100
	return (withFee + bundlerCreditMicroUSD - 1) / bundlerCreditMicroUSD * bundlerCreditMicroUSD
}

// bundlerCharged is what the bundler reports it charged, in micro-USD, capped at
// max: a value at or over the cap, negative or not a number is the cap, so
// no float conversion can overflow into a free call (security review of the
// aggregator, L1).
func bundlerCharged(credits float64, max int64) int64 {
	if math.IsNaN(credits) || credits < 0 || credits*bundlerCreditMicroUSD >= float64(max) {
		return max
	}
	return min(max, int64(credits*bundlerCreditMicroUSD+0.5))
}

func (f *keyBundler) Exchange(ctx context.Context, p x402Plan, pay *payment) (x402Response, *x402Receipt, error) {
	sum := sha256.Sum256([]byte(pay.c.Subject.ID + "\x00" + pay.c.RequestKey))
	idem := "swarmmemo-" + hex.EncodeToString(sum[:16])
	args := json.RawMessage(p.body)
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	invoke := map[string]any{
		"calls":           []any{map[string]any{"id": p.res.Tool, "args": args}},
		"max_usd":         json.Number(formatUnits(p.max, 6)),
		"idempotency_key": idem,
	}
	if p.res.bundlerSearch != "" {
		invoke["search_ids"] = []string{p.res.bundlerSearch}
	}
	body, err := json.Marshal(invoke)
	if err != nil {
		return x402Response{}, nil, refusal("upstream_failed")
	}
	req := p
	req.body = body
	if err = pay.reserve(ctx, p, reservation{amount: p.max, network: f.Name(), asset: "USD", payTo: bundlerToolID(p.res.Tool), nonce: idem, validBefore: time.Now().Unix() + int64(p.res.Timeout/time.Second)}); err != nil {
		return x402Response{}, nil, err
	}
	resp, err := pay.fetch(ctx, req, http.Header{"Authorization": {"Bearer " + f.key}}, p.res.MaxResponseBytes)
	switch {
	case err != nil:
		pay.finish("unknown", 0, 0, "", "no_response")
		return x402Response{}, nil, refusal("upstream_failed")
	case resp.status == http.StatusPaymentRequired:
		pay.finish("rejected", resp.status, len(resp.body), "", "budget")
		return x402Response{}, nil, refusal("x402_payment_rejected")
	case resp.status >= 400 && resp.status < 500:
		// The bundler charges nothing for a call it refuses: a definite
		// no-charge, which counts against no cap (M1).
		pay.finish("failed", resp.status, len(resp.body), "", "http_status")
		return x402Response{}, nil, refusal("upstream_failed")
	case resp.status < 200 || resp.status >= 300:
		// A server error may or may not have been billed: it keeps counting.
		pay.finish("unknown", resp.status, len(resp.body), "", "http_status")
		return x402Response{}, nil, refusal("upstream_failed")
	}
	charged := p.max // what we assume when the bundler reports nothing, or we could not read it
	if !resp.over {
		var out map[string]json.RawMessage
		dec := json.NewDecoder(bytes.NewReader(resp.body))
		dec.UseNumber()
		if dec.Decode(&out) == nil && out != nil {
			var billing struct {
				Charged json.Number `json:"charged_credits"`
			}
			if raw, ok := out["billing"]; ok && json.Unmarshal(raw, &billing) == nil {
				if credits, err := strconv.ParseFloat(billing.Charged.String(), 64); err == nil {
					charged = bundlerCharged(credits, p.max)
				}
			}
			delete(out, "billing") // our account's balance is not the agent's business
			bundlerLabelUpstreamReceipts(out)
			if b := canonicalJSON(out); b != nil {
				// The rows name the tool by its public name, never the
				// upstream's id.
				if name := bundlerPublicTool(p.res.Tool); name != p.res.Tool {
					b = bytes.ReplaceAll(b, []byte(strconv.Quote(p.res.Tool)), []byte(strconv.Quote(name)))
				}
				resp.body = b
			}
		}
	}
	if charged <= 0 {
		pay.finish("failed", resp.status, len(resp.body), "", "not_charged")
		return resp, nil, nil
	}
	pay.settle(charged)
	return resp, &x402Receipt{Amount: strconv.FormatInt(charged, 10), Price: formatUnits(charged, 6), Asset: "USD", Network: publicBundler(f.Name()),
		PayTo: bundlerToolID(p.res.Tool), Payer: "swarmmemo", Nonce: idem}, nil
}

// BundlerUpstreamNote labels the chain facts a bundler reports in its own
// per-call receipts: they are its own, not a payment by or to SwarmMemo.
const BundlerUpstreamNote = "reported by the upstream bundler; not a payment by or to SwarmMemo"

// bundlerUpstreamKeys are the receipt fields a bundler may report its own
// chain facts under, and the name each gets under receipt.upstream.
var bundlerUpstreamKeys = []struct{ from, to string }{
	{"network", "network"}, {"tx_hash", "transaction"}, {"transaction_hash", "transaction"}, {"transaction", "transaction"}, {"tx", "transaction"},
}

// bundlerLabelUpstreamReceipts moves the network and transaction of each
// results[].receipt under receipt.upstream, with BundlerUpstreamNote: the
// bundler pays from its prepaid account, so a transaction it reports is its
// own, never the settlement of the agent's call (which has none: see
// X402Settlements).
func bundlerLabelUpstreamReceipts(out map[string]json.RawMessage) {
	var results []json.RawMessage
	if json.Unmarshal(out["results"], &results) != nil {
		return
	}
	changed := false
	for i, raw := range results {
		var row map[string]json.RawMessage
		if json.Unmarshal(raw, &row) != nil || row == nil {
			continue
		}
		var receipt map[string]json.RawMessage
		if json.Unmarshal(row["receipt"], &receipt) != nil || receipt == nil {
			continue
		}
		upstream, moved := map[string]json.RawMessage{}, false
		for _, k := range bundlerUpstreamKeys {
			v, ok := receipt[k.from]
			if !ok {
				continue
			}
			delete(receipt, k.from)
			moved = true
			if _, set := upstream[k.to]; !set && !bytes.Equal(v, []byte("null")) && !bytes.Equal(v, []byte(`""`)) {
				upstream[k.to] = v
			}
		}
		if !moved {
			continue
		}
		upstream["note"] = canonicalJSON(BundlerUpstreamNote)
		receipt["upstream"] = canonicalJSON(upstream)
		row["receipt"] = canonicalJSON(receipt)
		results[i] = canonicalJSON(row)
		changed = true
	}
	if changed {
		if b := canonicalJSON(results); b != nil {
			out["results"] = b
		}
	}
}
