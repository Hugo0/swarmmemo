package services

// frames is a key-based bundler: the Frames API (api.frames.ag), which pays
// data providers itself and bills an account by plan, in credits of $0.001.
// An allowlist entry with "bundler":"frames" names a Frames tool by its id;
// a call becomes one POST /v1/tools/invoke with our bearer key, the agent's
// JSON body as the tool's arguments, max_usd at the entry's maximum and an
// idempotency key per call, so a retry never pays twice. The agent is
// charged what Frames reports it charged (billing.charged_credits), or the
// maximum when it reports nothing; our account's billing block is removed
// from the answer. The key is read once from key_file and never printed;
// without it the bundler is not ready and its resources are unavailable.
//
// With "open": true the whole Frames catalogue is callable as well, without
// allowlist entries: frames_open.go.

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

// FramesBaseURL is the Frames API's documented base.
const FramesBaseURL = "https://api.frames.ag/v1"

// framesCreditMicroUSD is one Frames credit, $0.001, in micro-USD.
const framesCreditMicroUSD = 1000

type x402BundlersFile struct {
	Frames *x402FramesFile `json:"frames"`
}

type x402FramesFile struct {
	KeyFile string `json:"key_file"`
	BaseURL string `json:"base_url"`
	// The open Frames catalogue (frames_open.go); absent or false keeps
	// Frames to the allowlist's entries.
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
	if f == nil || f.Frames == nil {
		return nil
	}
	base := strings.TrimSuffix(f.Frames.BaseURL, "/")
	if base == "" {
		base = FramesBaseURL
	}
	if err := checkX402URL(base); err != nil {
		return fmt.Errorf("x402: config field bundlers.frames.base_url is invalid (%s)", err)
	}
	if f.Frames.KeyFile == "" {
		return fmt.Errorf("x402: config field bundlers.frames.key_file is required")
	}
	b := &frames{base: base, endpoint: base + "/tools/invoke"}
	open, err := c.parseFramesOpen(f.Frames)
	if err != nil {
		return err
	}
	b.open = open
	raw, err := readKey(f.Frames.KeyFile)
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

type frames struct {
	base     string // the API's base URL, ".../v1"
	endpoint string
	key      string      // never printed: String hides it
	open     *FramesOpen // nil: only allowlisted Frames tools
}

func (f *frames) Name() string     { return "frames" }
func (f *frames) Ready() bool      { return f.key != "" }
func (f *frames) Endpoint() string { return f.endpoint }

// String and GoString keep the key out of every formatted config.
func (f *frames) String() string   { return "frames(" + f.endpoint + ")" }
func (f *frames) GoString() string { return f.String() }

// framesCharged is what Frames reports it charged, in micro-USD, capped at
// max: a value at or over the cap, negative or not a number is the cap, so
// no float conversion can overflow into a free call (security review of the
// aggregator, L1).
func framesCharged(credits float64, max int64) int64 {
	if math.IsNaN(credits) || credits < 0 || credits*framesCreditMicroUSD >= float64(max) {
		return max
	}
	return min(max, int64(credits*framesCreditMicroUSD+0.5))
}

func (f *frames) Exchange(ctx context.Context, p x402Plan, pay *payment) (x402Response, *x402Receipt, error) {
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
	if p.res.framesSearch != "" {
		invoke["search_ids"] = []string{p.res.framesSearch}
	}
	body, err := json.Marshal(invoke)
	if err != nil {
		return x402Response{}, nil, refusal("upstream_failed")
	}
	req := p
	req.body = body
	if err = pay.reserve(ctx, p, reservation{amount: p.max, network: f.Name(), asset: "USD", payTo: "frames:" + p.res.Tool, nonce: idem, validBefore: time.Now().Unix() + int64(p.res.Timeout/time.Second)}); err != nil {
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
		// Frames charges nothing for a call it refuses: a definite
		// no-charge, which counts against no cap (M1).
		pay.finish("failed", resp.status, len(resp.body), "", "http_status")
		return x402Response{}, nil, refusal("upstream_failed")
	case resp.status < 200 || resp.status >= 300:
		// A server error may or may not have been billed: it keeps counting.
		pay.finish("unknown", resp.status, len(resp.body), "", "http_status")
		return x402Response{}, nil, refusal("upstream_failed")
	}
	charged := p.max // what we assume when Frames reports nothing, or we could not read it
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
					charged = framesCharged(credits, p.max)
				}
			}
			delete(out, "billing") // our account's balance is not the agent's business
			if b := marshalNoEscape(out); b != nil {
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
		PayTo: FramesPrefix + p.res.Tool, Payer: "swarmmemo", Nonce: idem}, nil
}
