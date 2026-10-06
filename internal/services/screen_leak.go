package services

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"slices"
	"strconv"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/leakscan"
)

// LeakScreener is the classifier behind screen.leak (RFC0013 §5.3):
// moderation's Jev asked whether text an agent is about to send carries
// credentials, personal data, private infrastructure or excess code, for
// an audience of "public", "conversation" or "sealed". It answers like
// TextScreener; any error means the text was not screened.
type LeakScreener interface {
	ScreenLeak(ctx context.Context, text, audience string) (TextScreen, error)
	ScreenAvailable(ctx context.Context) bool
}

// screen.leak: the deterministic patterns of internal/leakscan (mode
// "patterns", free), or those and the classifier's four categories
// (mode "full", screen.text's price), over text an agent is about to send.
// Like screen.text it is stateless, signs a receipt with the notary key and
// fails closed.
const (
	// LeakSchema names the leak receipt format.
	LeakSchema = "swarmmemo-leak/1"
	// leakPatternsPrice is mode patterns' whole price: none. It runs the
	// published patterns locally, with no classifier, and never needs Jev.
	// Every description of it reads LeakPatternsPriceText.
	leakPatternsPrice = 0
	leakArgsMax       = 6*ScreenTextBytes + 256
)

// LeakPatternsPriceText is mode patterns' price in words, from the price
// the quote charges: "free", "1 credit" or "N credits".
func LeakPatternsPriceText() string { return creditsText(leakPatternsPrice) }

// creditsText is n credits in words.
func creditsText(n int64) string {
	switch n {
	case 0:
		return "free"
	case 1:
		return "1 credit"
	}
	return strconv.FormatInt(n, 10) + " credits"
}

// LeakCategories are what mode full scores (the patterns find the first
// three, and financial numbers besides); each counts in the verdict by
// leakscan.Actions. LeakAudiences and LeakModes are the values audience and
// mode take.
var (
	LeakCategories = []string{leakscan.Credentials, leakscan.PersonalData, leakscan.PrivateInfrastructure, "excess_code"}
	LeakAudiences  = []string{"public", "conversation", "sealed"}
	LeakModes      = []string{"patterns", "full"}
)

// leakMethod is screen's "leak" method in the catalogue.
var leakMethod = Method{Name: "leak", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: leakArgsMax, Price: ScreenPrice,
	Line:      "Check text you are about to send for secrets, personal data and private infrastructure: findings with byte offsets, a redacted copy and a signed receipt.",
	PriceNote: "mode patterns: " + LeakPatternsPriceText() + "; mode full: as text, " + strconv.Itoa(screenFee) + " + the classifier's token cost, at most " + strconv.Itoa(screenBase) + " + " + strconv.Itoa(screenPerKiB) + " per KiB of text, the rest refunded",
	Args: []Arg{
		{"text", "string", true, "the text you are about to send, up to " + SizeText(ScreenTextBytes) + " (" + SizeText(ScreenAnonymousTextBytes) + " without a key); hashed, never stored"},
		{"audience", "string", false, "who will read it: public (the default), conversation or sealed"},
		{"mode", "string", false, "patterns (the default: the published patterns only) or full (patterns and the classifier)"},
		{"threshold", "number", false, "0 to 1: a classifier category at or above it counts in the answer's verdict; default 0.6, which the receipt always uses"},
	},
	Example:        json.RawMessage(`{"text":"Deploy with DB_PASSWORD=hunter2hunter to db.prod.internal.","audience":"conversation"}`),
	ExampleMaxCost: leakPatternsPrice,
	Anonymous:      true, AnonymousLabel: "leak checks", AnonymousNote: "text up to " + SizeText(ScreenAnonymousTextBytes),
	AnonymousRate: AnonRate{CallerPerMinute: 10, CallerPerDay: 100, AllPerMinute: 60, AllPerDay: 3000}}

type leakArgs struct {
	Text      *string         `json:"text"`
	Audience  string          `json:"audience"`
	Mode      string          `json:"mode"`
	Threshold json.RawMessage `json:"threshold"`
}

type leakPlan struct {
	text, audience, mode string
	threshold            float64
}

func parseLeak(raw json.RawMessage) (leakPlan, error) {
	var a leakArgs
	if err := StrictObject(raw, &a); err != nil {
		return leakPlan{}, err
	}
	p := leakPlan{audience: a.Audience, mode: a.Mode, threshold: ScreenThreshold}
	if p.audience == "" {
		p.audience = "public"
	}
	if p.mode == "" {
		p.mode = "patterns"
	}
	if a.Text != nil && len(*a.Text) > ScreenTextBytes {
		return p, tooLarge("invalid_service_data", len(*a.Text), ScreenTextBytes)
	}
	if a.Text == nil || *a.Text == "" || !slices.Contains(LeakAudiences, p.audience) || !slices.Contains(LeakModes, p.mode) {
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

// LeakPayload is what a leak receipt signs, as ScreenPayload does for
// screen.text: salted text hash and size, who it was for, the mode, how
// many pattern findings and which pattern list, the classifier's categories
// (none in mode patterns) and model, and the verdict at Threshold, always
// ScreenThreshold. Never the findings' offsets or the text.
type LeakPayload struct {
	Schema          string             `json:"schema"`
	ServiceID       string             `json:"service_id"`
	KeyID           string             `json:"key_id"`
	Time            int64              `json:"time"`
	Salt            string             `json:"salt"`
	TextSHA256      string             `json:"text_sha256"`
	TextBytes       int                `json:"text_bytes"`
	Audience        string             `json:"audience"`
	Mode            string             `json:"mode"`
	FindingsCount   int                `json:"findings_count"`
	PatternsVersion int                `json:"patterns_version"`
	Categories      map[string]float64 `json:"categories"`
	Verdict         string             `json:"verdict"`
	Threshold       float64            `json:"threshold"`
	// Model is ClassifierVersion in new receipts (empty in
	// mode patterns); earlier receipts carry the classifier's model id.
	Model string `json:"model"`
}

// VerifyLeakReceipt checks a leak receipt offline against the public key
// (base64url, as published) and returns what it says.
func VerifyLeakReceipt(publicKey string, r ScreenReceipt) (LeakPayload, bool) {
	var p LeakPayload
	id, ok := verifyPayload(publicKey, r.Payload, r.Signature, &p)
	return p, ok && p.Schema == LeakSchema && r.Schema == LeakSchema && p.KeyID == r.KeyID && p.KeyID == id
}

// leakCategories are a classifier answer's scores for LeakCategories,
// rounded; false when one is missing or not a probability.
func leakCategories(t TextScreen) (map[string]float64, bool) {
	out := make(map[string]float64, len(LeakCategories))
	for _, k := range LeakCategories {
		v, ok := t.Scores[k]
		if !ok || !(v >= 0 && v <= 1) {
			return nil, false
		}
		out[k] = math.Round(v*1e4) / 1e4
	}
	return out, true
}

// runLeak is one screen.leak call, after commit. The redacted copy is in
// the first answer only (Result.Once): the call record and a retry's
// receipt keep the findings, never the text.
func (s *screen) runLeak(ctx context.Context, c Call) (Result, error) {
	p, err := parseLeak(c.Args)
	if err != nil {
		return Result{}, err
	}
	if len(s.key) != ed25519.PrivateKeySize {
		return Result{}, refusal("upstream_unavailable")
	}
	findings := leakscan.Scan(p.text)
	categories, model, used := map[string]float64{}, "", int64(leakPatternsPrice)
	if p.mode == "full" {
		if s.leaker == nil {
			return Result{}, refusal("upstream_unavailable")
		}
		// Fail closed, as screen.text: an unanswered text is never passed.
		res, err := s.leaker.ScreenLeak(ctx, p.text, p.audience)
		if err != nil {
			return Result{}, refusal("upstream_unavailable")
		}
		var ok bool
		if categories, ok = leakCategories(res); !ok || !screenModelRE.MatchString(res.Model) || res.CostMicroUSD <= 0 {
			return Result{}, refusal("upstream_failed")
		}
		model, used = ClassifierVersion, min(c.Price.For(int64(len(p.text))), screenFee+res.CostMicroUSD)
	}
	salt := make([]byte, screenSaltBytes)
	if _, err = rand.Read(salt); err != nil {
		return Result{}, err
	}
	pub := s.key.Public().(ed25519.PublicKey)
	payload := LeakPayload{Schema: LeakSchema, ServiceID: s.serviceID, KeyID: keyID(pub), Time: c.Now, Salt: hex.EncodeToString(salt), TextSHA256: saltedSHA256(salt, p.text),
		TextBytes: len(p.text), Audience: p.audience, Mode: p.mode, FindingsCount: len(findings), PatternsVersion: leakscan.Version, Categories: categories,
		Verdict: leakscan.Verdict(findings, categories, ScreenThreshold, nil), Threshold: ScreenThreshold, Model: model}
	signed, signature := signPayload(s.key, payload)
	receipt := ScreenReceipt{Schema: LeakSchema, KeyID: payload.KeyID, PublicKey: base64.RawURLEncoding.EncodeToString(pub), Payload: signed, Signature: signature}
	if findings == nil {
		findings = []leakscan.Finding{}
	}
	body := canonicalJSON(map[string]any{
		"verdict": leakscan.Verdict(findings, categories, p.threshold, nil), "threshold": p.threshold, "mode": p.mode, "audience": p.audience,
		"findings": findings, "categories": categories, "classifier_version": model, "patterns_version": leakscan.Version,
		"text_sha256": payload.TextSHA256, "text_bytes": payload.TextBytes, "receipt": receipt,
		"note": "Findings are byte offsets into your text; redacted replaces each with «REDACTED:rule» and is in this answer only. A signal, not a guarantee: hold means a finding's category holds, warn that every finding only warns (the pattern list's actions), pass that nothing was found. Patterns: /api/screen/leak-patterns",
	})
	once := canonicalJSON(map[string]any{"redacted": leakscan.Redact(p.text, findings)})
	public := canonicalJSON(map[string]any{"verdict": payload.Verdict, "mode": p.mode, "audience": p.audience, "text_bytes": payload.TextBytes, "findings_count": len(findings)})
	return Result{Body: body, Once: once, Public: public, Used: used}, nil
}
