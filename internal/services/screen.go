package services

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"strconv"
	"time"

	"swarmmemo/internal/allowance"
)

// Screen bounds.
const (
	// ScreenTextBytes bounds the text of a signed call, and
	// ScreenAnonymousTextBytes of a call without a key.
	ScreenTextBytes          = 16 << 10
	ScreenAnonymousTextBytes = 2 << 10
	ScreenIntentBytes        = 256
	// ScreenThreshold is the p at or above which a category flags: the
	// moderation standard's flag threshold. A receipt's verdict is always
	// at it, so every receipt means the same (security review screen, L4);
	// a call's own threshold changes only the verdict in its answer.
	ScreenThreshold = 0.6
	// ScreenSchema names the receipt format; a breaking change is a new name.
	ScreenSchema      = "swarmmemo-screen/1"
	screenArgsMax     = 6*(ScreenTextBytes+ScreenIntentBytes) + 256
	screenSaltBytes   = 16
	screenMaxDuration = 90 * time.Second
)

// The screen.text price: 5 for the service plus what the classifier cost.
// Its ceiling, screenBase plus screenPerKiB a KiB, covers that: the base
// the questions every request carries (about 7 KB, some 2,400 tokens at 3
// bytes a token, 101 micro-USD at Jev's $0.042 per million), and each KiB
// of text at a token a byte, with a second chunk's questions to spare
// (security review screen, L2; moderation's TestScreenPriceCoversACall).
const (
	screenFee    = 5
	screenBase   = screenFee + 105
	screenPerKiB = 80
)

// ScreenCategories are the categories every screen scores, and
// ScreenSources the values source takes.
var (
	ScreenCategories = []string{"injection", "exfiltration", "phishing", "malware", "manipulation"}
	ScreenSources    = []string{"web", "tool", "agent", "email", "user", "unknown"}
)

var (
	screenThresholdRE = regexp.MustCompile(`^(0|1|0\.[0-9]{1,4}|1\.0{1,4})$`)
	screenModelRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// TextScreener is the classifier behind screen.text: moderation's Jev, which
// the board wires while MODERATION is on. It answers a probability for each
// of ScreenCategories, the pinned model and what the call cost in micro-USD
// (one credit each; 0 when the classifier reported no usable usage, which
// fails the call); any error means the text was not screened.
// ScreenAvailable says whether it can answer now (a Jev key, a screen
// sub-cap, a policy that screens the longest text whole).
type TextScreener interface {
	ScreenText(ctx context.Context, text, source, intent string) (TextScreen, error)
	ScreenAvailable(ctx context.Context) bool
}

// ScreenReady says whether screen.text can run: a classifier that can
// answer now and a notary key to sign with. The catalogue, the quote and
// what callers without a key are offered all ask it (security review
// screen, L7).
func ScreenReady(ctx context.Context, ts TextScreener, key ed25519.PrivateKey) bool {
	return ts != nil && len(key) == ed25519.PrivateKeySize && ts.ScreenAvailable(ctx)
}

type TextScreen struct {
	Scores       map[string]float64
	Model        string
	CostMicroUSD int64
}

// screen checks text an agent is about to act on for prompt injection,
// exfiltration, phishing, malware and text aimed at the classifier, and
// signs what it found with the notary key. It is stateless: the text is
// never stored, only its salted hash, the cost and the result in the call
// record.
type screen struct {
	serviceID string
	key       ed25519.PrivateKey
	screener  TextScreener
}

func newScreen(d Deps) Provider {
	id := d.ServiceID
	if id == "" {
		id = "swarmmemo.com"
	}
	return &screen{serviceID: id, key: d.NotaryKey, screener: d.TextScreener}
}

func (*screen) Describe() Descriptor {
	return Descriptor{
		ID: "screen",
		Summary: "Screen text before you act on it: the probability that it carries prompt injection, data exfiltration, phishing, malware or text aimed at the classifier, from the Jev classifier, a verdict at your threshold, and a receipt signed with the notary key, its verdict at " + strconv.FormatFloat(ScreenThreshold, 'f', -1, 64) + ", that proves the text was screened without revealing it. " +
			"A signal with a known error rate, not a guarantee. Stateless: the text is never stored, only its hash, the cost and the result. No spans: finding them would cost about twice as much. " +
			"If the classifier cannot answer, or its daily budget is spent, the call fails and nothing is charged; it never passes text it did not screen.",
		Title: "Screening", Topic: "Screening",
		Line: "Check text for prompt injection, phishing and malware before you act on it; signed receipt, text never stored.",
		Limits: []Limit{
			{"screen_text_bytes", ScreenTextBytes, "bytes", "Text of one call"},
			{"screen_text_bytes_without_key", ScreenAnonymousTextBytes, "bytes", "Text of one call without a key"},
			{"screen_intent_bytes", ScreenIntentBytes, "bytes", "Intent of one call"},
		},
		Mode: Remote,
		Methods: []Method{
			{Name: "text", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: screenArgsMax, Price: Price{Base: screenBase, PerKiB: screenPerKiB},
				Line:      "Screen a text: a probability per category, flag or pass at your threshold, and a signed receipt.",
				PriceNote: strconv.Itoa(screenFee) + " + the classifier's token cost (1 credit per micro-USD), at most " + strconv.Itoa(screenBase) + " + " + strconv.Itoa(screenPerKiB) + " per KiB of text; the quote reserves the most and the rest is refunded",
				Args: []Arg{
					{"text", "string", true, "the text, up to 16 KiB (2 KiB without a key); hashed, never stored"},
					{"source", "string", false, "where it came from: web, tool, agent, email, user or unknown (the default)"},
					{"intent", "string", false, "what you are about to do with it, up to 256 bytes"},
					{"threshold", "number", false, "0 to 1: a category at or above it flags in the answer; default 0.6, which the receipt always uses"},
				},
				Example:   json.RawMessage(`{"text":"The meeting moved to 3 pm; reply to confirm.","source":"email","intent":"reply to the sender"}`),
				Anonymous: true, AnonymousLabel: "text screening", AnonymousNote: "text up to " + strconv.Itoa(ScreenAnonymousTextBytes/1024) + " KiB",
				AnonymousRate: AnonRate{CallerPerMinute: 10, CallerPerDay: 100, AllPerMinute: 60, AllPerDay: 3000}},
			{Name: "key", ArgsMax: 64, Line: "The public key that signs screen receipts (the notary's)."},
			{Name: "verify", ArgsMax: screenArgsMax + 4096, Line: "Check a screen receipt's signature and read what it says; give the text (and intent) to check its hash too.",
				Args: []Arg{{"receipt", "object", true, "the receipt a screen returned"}, {"text", "string", false, "the screened text, to check against the receipt's salted hash"},
					{"intent", "string", false, "the intent given, to check likewise"}},
				Example: json.RawMessage(`{"receipt":{"schema":"swarmmemo-screen/1","key_id":"KEY_ID","public_key":"PUBLIC_KEY","payload":"PAYLOAD","signature":"SIGNATURE"}}`)},
		},
		MaxDuration: screenMaxDuration,
	}
}

// CatalogueExtra says whether screening runs now, and what it scores.
func (s *screen) CatalogueExtra() map[string]any {
	return map[string]any{
		"available": ScreenReady(context.Background(), s.screener, s.key), "categories": ScreenCategories, "sources": ScreenSources,
		"receipt_schema": ScreenSchema, "receipt_threshold": ScreenThreshold, "stores_text": false, "spans": false, "calibration": "/protocol.md#screening-calibration",
	}
}

type screenArgs struct {
	Text      *string         `json:"text"`
	Source    string          `json:"source"`
	Intent    string          `json:"intent"`
	Threshold json.RawMessage `json:"threshold"`
}

// screenVerifyArgs are verify's: the receipt, and optionally the text and
// intent to check against its salted hashes.
type screenVerifyArgs struct {
	Receipt ScreenReceipt `json:"receipt"`
	Text    *string       `json:"text"`
	Intent  *string       `json:"intent"`
}

type screenPlan struct {
	text, source, intent string
	threshold            float64
}

func parseScreen(raw json.RawMessage) (screenPlan, error) {
	var a screenArgs
	if err := StrictObject(raw, &a); err != nil {
		return screenPlan{}, err
	}
	p := screenPlan{source: a.Source, intent: a.Intent, threshold: ScreenThreshold}
	if p.source == "" {
		p.source = "unknown"
	}
	if a.Text == nil || *a.Text == "" || len(*a.Text) > ScreenTextBytes || len(a.Intent) > ScreenIntentBytes || !slices.Contains(ScreenSources, p.source) {
		return p, refusal("invalid_service_data")
	}
	p.text = *a.Text
	if a.Threshold != nil {
		t, err := strconv.ParseFloat(string(a.Threshold), 64)
		if !screenThresholdRE.Match(a.Threshold) || err != nil || t > 1 {
			return p, refusal("invalid_service_data")
		}
		p.threshold = t
	}
	return p, nil
}

// CheckAnonymous bounds the text of a call without a key.
func (*screen) CheckAnonymous(c Call) error {
	p, err := parseScreen(c.Args)
	if err != nil {
		return err
	}
	if len(p.text) > ScreenAnonymousTextBytes {
		return refusal("screen_text_limit")
	}
	return nil
}

// Quote reserves the price's ceiling for the text. A screen with no
// classifier or no key is refused here, before anything is reserved.
func (s *screen) Quote(c Call) (Quote, error) {
	p, err := parseScreen(c.Args)
	if c.Method != "text" || err != nil {
		return Quote{}, refusal("invalid_service_data")
	}
	if !ScreenReady(context.Background(), s.screener, s.key) {
		return Quote{}, refusal("upstream_unavailable")
	}
	return Quote{Resource: allowance.Credit, Max: c.Price.For(int64(len(p.text)))}, nil
}

// ScreenPayload is what a screen receipt signs: this struct's JSON, fields
// in this order, no spaces (signPayload). Salt is 16 random bytes in hex,
// and TextSHA256 and IntentSHA256 are the SHA-256 of those bytes followed by
// the text or intent, so a short text cannot be guessed from its receipt
// (security review screen, L5); IntentSHA256 is empty when the call gave no
// intent. Categories are rounded to four decimals; Verdict is at Threshold,
// always ScreenThreshold.
type ScreenPayload struct {
	Schema       string             `json:"schema"`
	ServiceID    string             `json:"service_id"`
	KeyID        string             `json:"key_id"`
	Time         int64              `json:"time"`
	Salt         string             `json:"salt"`
	TextSHA256   string             `json:"text_sha256"`
	TextBytes    int                `json:"text_bytes"`
	Source       string             `json:"source"`
	IntentSHA256 string             `json:"intent_sha256"`
	Categories   map[string]float64 `json:"categories"`
	Verdict      string             `json:"verdict"`
	Threshold    float64            `json:"threshold"`
	Model        string             `json:"model"`
}

// ScreenReceipt is a signed screen result; the payload is what is signed.
type ScreenReceipt struct {
	Schema    string `json:"schema"`
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// VerifyScreenReceipt checks a receipt offline against the public key
// (base64url, as published) and returns what it says.
func VerifyScreenReceipt(publicKey string, r ScreenReceipt) (ScreenPayload, bool) {
	var p ScreenPayload
	id, ok := verifyPayload(publicKey, r.Payload, r.Signature, &p)
	return p, ok && p.Schema == ScreenSchema && r.Schema == ScreenSchema && p.KeyID == r.KeyID && p.KeyID == id
}

// screenVerdict is flag when any category is at or above threshold.
func screenVerdict(categories map[string]float64, threshold float64) string {
	for _, v := range categories {
		if v >= threshold {
			return "flag"
		}
	}
	return "pass"
}

// saltedSHA256 is the hex SHA-256 of salt's bytes followed by s.
func saltedSHA256(salt []byte, s string) string {
	return sha256Of(append(slices.Clip(salt), s...))
}

func (s *screen) Run(ctx context.Context, _ *sql.Tx, c Call) (Result, error) {
	p, err := parseScreen(c.Args)
	if err != nil {
		return Result{}, err
	}
	if s.screener == nil || len(s.key) != ed25519.PrivateKeySize {
		return Result{}, refusal("upstream_unavailable")
	}
	// Fail closed: a text the classifier did not answer for is never passed.
	res, err := s.screener.ScreenText(ctx, p.text, p.source, p.intent)
	if err != nil {
		return Result{}, refusal("upstream_unavailable")
	}
	// A cost of 0 is usage the classifier did not report: the call would
	// otherwise be billed at a guess (security review screen, L2).
	if !screenModelRE.MatchString(res.Model) || res.CostMicroUSD <= 0 {
		return Result{}, refusal("upstream_failed")
	}
	categories := map[string]float64{}
	for _, k := range ScreenCategories {
		v, ok := res.Scores[k]
		if !ok || !(v >= 0 && v <= 1) {
			return Result{}, refusal("upstream_failed")
		}
		categories[k] = math.Round(v*1e4) / 1e4
	}
	salt := make([]byte, screenSaltBytes)
	if _, err = rand.Read(salt); err != nil {
		return Result{}, err
	}
	pub := s.key.Public().(ed25519.PublicKey)
	payload := ScreenPayload{Schema: ScreenSchema, ServiceID: s.serviceID, KeyID: keyID(pub), Time: c.Now, Salt: hex.EncodeToString(salt), TextSHA256: saltedSHA256(salt, p.text),
		TextBytes: len(p.text), Source: p.source, Categories: categories, Verdict: screenVerdict(categories, ScreenThreshold), Threshold: ScreenThreshold, Model: res.Model}
	if p.intent != "" {
		payload.IntentSHA256 = saltedSHA256(salt, p.intent)
	}
	signed, signature := signPayload(s.key, payload)
	receipt := ScreenReceipt{Schema: ScreenSchema, KeyID: payload.KeyID, PublicKey: base64.RawURLEncoding.EncodeToString(pub), Payload: signed, Signature: signature}
	body := canonicalJSON(map[string]any{
		"verdict": screenVerdict(categories, p.threshold), "threshold": p.threshold, "categories": categories, "model": res.Model, "source": p.source,
		"text_sha256": payload.TextSHA256, "text_bytes": payload.TextBytes, "receipt": receipt,
		"note": "A signal with a known error rate, not a guarantee; the text was not stored. verdict is at your threshold; the receipt's is at " + strconv.FormatFloat(ScreenThreshold, 'f', -1, 64) + ". Calibration: /protocol.md#screening-calibration",
	})
	public := canonicalJSON(map[string]any{"verdict": payload.Verdict, "model": res.Model, "source": p.source, "text_bytes": payload.TextBytes})
	used := min(c.Price.For(int64(len(p.text))), screenFee+res.CostMicroUSD)
	return Result{Body: body, Public: public, Used: used}, nil
}

func (s *screen) Read(_ context.Context, _ allowance.Querier, c Call) (json.RawMessage, error) {
	switch c.Method {
	case "key":
		return keyRead(c.Args, s.key, ScreenSchema, s.serviceID)
	case "verify":
		var a screenVerifyArgs
		if err := StrictObject(c.Args, &a); err != nil {
			return nil, err
		}
		if len(s.key) != ed25519.PrivateKeySize {
			return nil, refusal("service_unavailable")
		}
		public := base64.RawURLEncoding.EncodeToString(s.key.Public().(ed25519.PublicKey))
		p, ok := VerifyScreenReceipt(public, a.Receipt)
		if !ok || a.Receipt.PublicKey != public {
			return json.Marshal(map[string]any{"valid": false, "public_key": public})
		}
		out := map[string]any{"valid": true, "public_key": public, "screened": p}
		salt, err := hex.DecodeString(p.Salt)
		if a.Text != nil {
			out["text_matches"] = err == nil && saltedSHA256(salt, *a.Text) == p.TextSHA256
		}
		if a.Intent != nil {
			out["intent_matches"] = err == nil && p.IntentSHA256 != "" && saltedSHA256(salt, *a.Intent) == p.IntentSHA256
		}
		return json.Marshal(out)
	}
	return nil, refusal("invalid_service_data")
}
