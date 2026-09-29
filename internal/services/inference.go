package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/safenet"
)

// Inference limits. Every one is a hard cap: a call that would exceed one is
// refused before anything is reserved, or fails and is refunded.
const (
	// InferencePromptBytes bounds the summed content of a call's messages:
	// 16 KiB, the same as a post's text (published as inference_prompt_bytes).
	InferencePromptBytes = 16 << 10
	// InferenceArgsMax bounds complete's args JSON (published as
	// inference_args_bytes, apart from the 4 KiB service_args_bytes of the
	// other services): a full prompt JSON-escaped, with room for the message
	// wrappers and options.
	InferenceArgsMax = 2*InferencePromptBytes + 2048
	// InferenceMessagesMax bounds a call's messages.
	InferenceMessagesMax = 16
	// InferenceOutputTokensMax is the most max_tokens any call or model may ask.
	InferenceOutputTokensMax = 4096
	// InferenceDefaultMaxTokens is max_tokens when a call leaves it out.
	InferenceDefaultMaxTokens = 256
	// InferenceAnonymousModel is the one model alias an unsigned call may
	// name, and InferenceAnonymousMaxTokens its most output tokens.
	// InferenceAnonymousPromptBytes bounds the summed message content of an
	// unsigned call (security review 1.21, L2).
	InferenceAnonymousModel       = "small"
	InferenceAnonymousMaxTokens   = 256
	InferenceAnonymousPromptBytes = 2 << 10
	// InferenceInputTokensMax bounds the input tokens a call can be charged
	// for: a token is at least one byte of prompt, plus a fixed allowance per
	// message and per request for the chat template.
	InferenceInputTokensMax = InferencePromptBytes + inferenceMessageTokens*InferenceMessagesMax + inferenceRequestTokens
	// InferenceOutputBytes bounds the output text kept from one reply.
	InferenceOutputBytes = 32 << 10
	// InferenceStoredBodyBytes bounds an inference call record (prompt,
	// output and model), above other services' 64 KiB so that a full prompt
	// and a full reply fit, JSON-escaped. An output that escapes past it is
	// kept cut, marked output_truncated with the full output's sha256 and
	// size: the call is still answered and charged, as the upstream was paid.
	InferenceStoredBodyBytes = StoredBodyCeilingBytes
	// InferenceResponseBytes bounds the raw upstream response read.
	InferenceResponseBytes = 256 << 10
	// InferenceRequestBytes bounds the request body sent upstream.
	InferenceRequestBytes = 128 << 10
	// InferenceMaxDuration bounds one call, every failover attempt included.
	InferenceMaxDuration = 90 * time.Second
	// InferenceConfigBytes bounds the INFERENCE_CONFIG file.
	InferenceConfigBytes = 64 << 10
	// InferenceKeyFileBytes bounds an upstream key file.
	InferenceKeyFileBytes = 4096
	// InferenceSuspendedFor is how long an upstream that answered 401 or 403
	// (a revoked key or a suspended account) is skipped before it is tried again.
	InferenceSuspendedFor = 10 * time.Minute

	inferenceMessageTokens  = 16
	inferenceRequestTokens  = 64
	inferenceUpstreamsMax   = 8
	inferenceModelsMax      = 32
	inferenceAliasesMax     = 32
	inferenceRouteMax       = 4
	inferenceExtraBytes     = 1024
	inferenceTimeoutDefault = 20 * time.Second
	inferenceTimeoutMax     = 60 * time.Second
	inferenceSpendCapMax    = 1 << 40
	inferencePerMTokMax     = 1 << 32
	inferenceDialTimeout    = 5 * time.Second
	inferenceTokenReportMax = 1 << 31
	inferenceKeyMinBytes    = 8
)

// Upstream kinds.
const (
	KindOpenAICompat = "openai_compat" // any OpenAI-compatible chat-completions API
	KindWorkersAI    = "workers_ai"    // Cloudflare Workers AI, native REST API
)

// WorkersAIBaseURL is a workers_ai upstream's base URL when the config leaves
// it out.
const WorkersAIBaseURL = "https://api.cloudflare.com/client/v4"

// InferenceConfig is the operator's INFERENCE_CONFIG: the upstreams, each
// with its own model allowlist and prices, and the model aliases callers
// name, each an ordered list of upstream models to fail over through. It
// holds no secret: every key lives in a file the config names by path.
type InferenceConfig struct {
	Upstreams []*InferenceUpstream
	Models    map[string][]InferenceRoute // alias → ordered route

	// Screener, when set, sees every prompt before it is sent and every
	// output before it is recorded. The server sets none; the steward wires
	// one (Jev) later.
	Screener InferenceScreener

	aliases []string // sorted
	byName  map[string]*InferenceUpstream
	// dial and allowHTTP exist only for tests (export_test.go): an httptest
	// upstream is plain HTTP on loopback, which the safe dialer refuses.
	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	allowHTTP bool
}

// InferenceUpstream is one named upstream instance.
type InferenceUpstream struct {
	Name          string                    `json:"name"`
	Kind          string                    `json:"kind"`
	BaseURL       string                    `json:"base_url"`
	KeyFile       string                    `json:"key_file"`
	AccountID     string                    `json:"account_id"`
	TimeoutMS     int64                     `json:"timeout_ms"`
	DailySpendCap int64                     `json:"daily_spend_cap"`
	Models        map[string]InferenceModel `json:"models"`
	Extra         json.RawMessage           `json:"extra"`

	host    string // host:port every request goes to
	timeout time.Duration
}

// InferenceModel is one allowlisted upstream model.
type InferenceModel struct {
	Price           InferencePrice `json:"price"`
	MaxOutputTokens int64          `json:"max_output_tokens"`
}

// InferencePrice is a model's price in ledger units (resource credit):
// Base + ceil((input tokens × InputPerMTok + output tokens × OutputPerMTok) / 1e6).
type InferencePrice struct {
	Base          int64 `json:"base"`
	InputPerMTok  int64 `json:"input_per_mtok"`
	OutputPerMTok int64 `json:"output_per_mtok"`
}

// For is the price of in input and out output tokens (both bounded by the
// caps above, so nothing overflows).
func (p InferencePrice) For(in, out int64) int64 {
	return p.Base + (in*p.InputPerMTok+out*p.OutputPerMTok+999_999)/1_000_000
}

// InferenceRoute is one step of an alias's failover order.
type InferenceRoute struct {
	Upstream string `json:"upstream"`
	Model    string `json:"model"`
}

// InferenceScreener is the moderation hook point (the steward's Jev screen).
// An error fails the call closed and nothing is charged. A prompt verdict
// with Hide refuses the call (content_refused, nothing charged); an output
// verdict with Hide keeps the call and its charge, and records and returns
// the output's hash and size but not its text.
type InferenceScreener interface {
	Screen(ctx context.Context, in ScreenInput) (ScreenVerdict, error)
}

// ScreenInput is what a screener sees: Stage "prompt" (Text is the messages
// as JSON) or "output" (Text is the reply). Signed is false for a call
// without a key: a screener that cannot judge its text (the classifier is
// down or over its spend cap) must then answer an error, never let it
// through flagged, so the call fails closed (security review 1.21, L1).
type ScreenInput struct {
	Stage  string
	Model  string // the caller's alias
	Text   string
	Signed bool
}

type ScreenVerdict struct {
	Hide   bool
	Reason string // one short public sentence
}

var (
	inferenceAliasRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	inferenceUpstreamRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	inferenceModelRE    = regexp.MustCompile(`^[A-Za-z0-9@][A-Za-z0-9@._:/+-]{0,127}$`)
	inferenceAccountRE  = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
	inferenceFinishRE   = regexp.MustCompile(`^[a-z_]{1,32}$`)
	inferenceTempRE     = regexp.MustCompile(`^(0|[1-9][0-9]?)(\.[0-9]{1,3})?$`)
	// Request fields the provider sets itself; extra may not override them.
	inferenceReserved = []string{"model", "messages", "max_tokens", "temperature", "stream", "n", "stream_options"}
)

type inferenceConfigFile struct {
	Schema    json.RawMessage             `json:"schema"`
	Upstreams []*InferenceUpstream        `json:"upstreams"`
	Models    map[string][]InferenceRoute `json:"models"`
}

// LoadInferenceConfig reads and validates the INFERENCE_CONFIG file. It never
// opens a key file: a missing key only makes that upstream unavailable.
func LoadInferenceConfig(path string) (*InferenceConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("INFERENCE_CONFIG: %w", err)
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, InferenceConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("INFERENCE_CONFIG: %w", err)
	}
	if len(body) > InferenceConfigBytes {
		return nil, fmt.Errorf("INFERENCE_CONFIG: larger than %d bytes", InferenceConfigBytes)
	}
	return ParseInferenceConfig(body)
}

// ParseInferenceConfig validates a config body strictly (unknown or repeated
// keys are errors). Its errors name the field at fault, never a value read
// from a key file.
func ParseInferenceConfig(body []byte) (*InferenceConfig, error) {
	return parseInferenceConfig(body, false)
}

func parseInferenceConfig(body []byte, allowHTTP bool) (*InferenceConfig, error) {
	var file inferenceConfigFile
	if err := StrictObject(body, &file); err != nil {
		return nil, errors.New(`INFERENCE_CONFIG: not a strict JSON object {"schema":1,"upstreams":[...],"models":{...}} (unknown, repeated or mistyped field)`)
	}
	if string(file.Schema) != "1" {
		return nil, errors.New("INFERENCE_CONFIG: schema must be 1")
	}
	cfg := &InferenceConfig{Upstreams: file.Upstreams, Models: file.Models, byName: map[string]*InferenceUpstream{}, allowHTTP: allowHTTP}
	if len(cfg.Upstreams) == 0 || len(cfg.Upstreams) > inferenceUpstreamsMax {
		return nil, fmt.Errorf("INFERENCE_CONFIG: 1 to %d upstreams", inferenceUpstreamsMax)
	}
	for i, up := range cfg.Upstreams {
		if up == nil {
			return nil, fmt.Errorf("INFERENCE_CONFIG: upstreams[%d] is null", i)
		}
		if err := up.validate(allowHTTP); err != nil {
			return nil, fmt.Errorf("INFERENCE_CONFIG: upstream %q: %w", up.Name, err)
		}
		if cfg.byName[up.Name] != nil {
			return nil, fmt.Errorf("INFERENCE_CONFIG: upstream %q is listed twice", up.Name)
		}
		cfg.byName[up.Name] = up
	}
	if len(cfg.Models) == 0 || len(cfg.Models) > inferenceAliasesMax {
		return nil, fmt.Errorf("INFERENCE_CONFIG: 1 to %d model aliases", inferenceAliasesMax)
	}
	for alias, route := range cfg.Models {
		if !inferenceAliasRE.MatchString(alias) {
			return nil, fmt.Errorf("INFERENCE_CONFIG: model alias %q must match %s", alias, inferenceAliasRE)
		}
		if len(route) == 0 || len(route) > inferenceRouteMax {
			return nil, fmt.Errorf("INFERENCE_CONFIG: model %q needs 1 to %d upstream models", alias, inferenceRouteMax)
		}
		seen := map[InferenceRoute]bool{}
		for _, r := range route {
			up := cfg.byName[r.Upstream]
			if up == nil {
				return nil, fmt.Errorf("INFERENCE_CONFIG: model %q names unknown upstream %q", alias, r.Upstream)
			}
			if _, ok := up.Models[r.Model]; !ok {
				return nil, fmt.Errorf("INFERENCE_CONFIG: model %q names %q, which upstream %q does not allowlist", alias, r.Model, r.Upstream)
			}
			if seen[r] {
				return nil, fmt.Errorf("INFERENCE_CONFIG: model %q lists %s/%s twice", alias, r.Upstream, r.Model)
			}
			seen[r] = true
		}
		cfg.aliases = append(cfg.aliases, alias)
	}
	slices.Sort(cfg.aliases)
	return cfg, nil
}

func (up *InferenceUpstream) validate(allowHTTP bool) error {
	if !inferenceUpstreamRE.MatchString(up.Name) {
		return fmt.Errorf("name must match %s", inferenceUpstreamRE)
	}
	switch up.Kind {
	case KindOpenAICompat:
		if up.AccountID != "" {
			return errors.New("account_id is for workers_ai only")
		}
	case KindWorkersAI:
		if !inferenceAccountRE.MatchString(up.AccountID) {
			return errors.New("workers_ai needs account_id (letters and digits)")
		}
		if up.BaseURL == "" {
			up.BaseURL = WorkersAIBaseURL
		}
	default:
		return fmt.Errorf("kind must be %s or %s", KindOpenAICompat, KindWorkersAI)
	}
	host, err := inferenceBaseURL(up.BaseURL, allowHTTP)
	if err != nil {
		return err
	}
	up.BaseURL = strings.TrimSuffix(up.BaseURL, "/")
	up.host = host
	if !strings.HasPrefix(up.KeyFile, "/") || strings.ContainsAny(up.KeyFile, "\x00\n\r") {
		return errors.New("key_file must be an absolute path (the key itself never goes in the config or the environment)")
	}
	switch {
	case up.TimeoutMS == 0:
		up.timeout = inferenceTimeoutDefault
	case up.TimeoutMS < 1000 || time.Duration(up.TimeoutMS)*time.Millisecond > inferenceTimeoutMax:
		return fmt.Errorf("timeout_ms must be 1000 to %d", inferenceTimeoutMax/time.Millisecond)
	default:
		up.timeout = time.Duration(up.TimeoutMS) * time.Millisecond
	}
	if up.DailySpendCap < 1 || up.DailySpendCap > inferenceSpendCapMax {
		return fmt.Errorf("daily_spend_cap is required: 1 to %d ledger units", int64(inferenceSpendCapMax))
	}
	if len(up.Models) == 0 || len(up.Models) > inferenceModelsMax {
		return fmt.Errorf("1 to %d allowlisted models", inferenceModelsMax)
	}
	for name, m := range up.Models {
		if !validUpstreamModel(name) {
			return fmt.Errorf("model %q must match %s, without .. or empty path segments", name, inferenceModelRE)
		}
		// Base >= 1: every call costs something, so the daily cap binds.
		if m.Price.Base < 1 || m.Price.Base > PriceMax || m.Price.InputPerMTok < 0 || m.Price.InputPerMTok > inferencePerMTokMax ||
			m.Price.OutputPerMTok < 0 || m.Price.OutputPerMTok > inferencePerMTokMax {
			return fmt.Errorf("model %q: price.base must be 1 to %d and the per-token prices 0 to %d", name, int64(PriceMax), int64(inferencePerMTokMax))
		}
		if m.MaxOutputTokens < 1 || m.MaxOutputTokens > InferenceOutputTokensMax {
			return fmt.Errorf("model %q: max_output_tokens must be 1 to %d", name, InferenceOutputTokensMax)
		}
	}
	if up.Extra != nil {
		var extra map[string]json.RawMessage
		if len(up.Extra) > inferenceExtraBytes || up.Extra[0] != '{' || json.Unmarshal(up.Extra, &extra) != nil {
			return fmt.Errorf("extra must be a JSON object of at most %d bytes", inferenceExtraBytes)
		}
		for _, k := range inferenceReserved {
			if _, ok := extra[k]; ok {
				return fmt.Errorf("extra may not set %q", k)
			}
		}
	}
	return nil
}

func validUpstreamModel(name string) bool {
	if !inferenceModelRE.MatchString(name) || strings.Contains(name, "..") {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." {
			return false
		}
	}
	return true
}

// inferenceBaseURL accepts a plain https URL with no credentials, query or
// fragment, and returns the host:port every request must connect to.
func inferenceBaseURL(raw string, allowHTTP bool) (string, error) {
	bad := errors.New("base_url must be an https URL with no credentials, query or fragment")
	if raw == "" || len(raw) > 512 || strings.ContainsAny(raw, " \t\r\n\\") {
		return "", bad
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Hostname() == "" || strings.HasSuffix(u.Path, "//") {
		return "", bad
	}
	port := u.Port()
	switch {
	case u.Scheme == "https":
		if port == "" {
			port = "443"
		}
	case u.Scheme == "http" && allowHTTP:
		if port == "" {
			port = "80"
		}
	default:
		return "", bad
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

// inference is the inference service: one method, complete, over the
// configured upstreams, non-streaming, metered in credit by token usage.
type inference struct {
	cfg    *InferenceConfig
	db     *sql.DB
	client *http.Client
	hosts  map[string]bool

	mu        sync.Mutex
	suspended map[string]time.Time // upstream → skipped until
}

func newInference(d Deps) Provider {
	p := &inference{cfg: d.Inference, db: d.DB, suspended: map[string]time.Time{}, hosts: map[string]bool{}}
	if p.cfg != nil {
		for _, up := range p.cfg.Upstreams {
			p.hosts[up.host] = true
		}
		p.client = p.newClient()
	}
	return p
}

// newClient is the only HTTP client inference uses: it connects only to a
// configured upstream's host, only to public addresses (internal/safenet),
// never through a proxy, and never follows a redirect.
func (p *inference) newClient() *http.Client {
	dial := p.cfg.dial
	if dial == nil {
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return safenet.Dial(ctx, network, addr, inferenceDialTimeout)
		}
	}
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if !p.hosts[addr] {
					return nil, errNotUpstream
				}
				return dial(ctx, network, addr)
			},
			TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:    inferenceDialTimeout,
			MaxIdleConns:           inferenceUpstreamsMax * 2,
			MaxIdleConnsPerHost:    2,
			IdleConnTimeout:        60 * time.Second,
			MaxResponseHeaderBytes: 16 << 10,
		},
	}
}

func (*inference) Schema() string {
	return `
CREATE TABLE IF NOT EXISTS inference_spend (
 upstream TEXT NOT NULL, day INTEGER NOT NULL, units INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(upstream,day));
`
}

func (*inference) Describe() Descriptor {
	return Descriptor{
		ID: "inference",
		Summary: "Chat completion from a small hosted model, non-streaming. args: {\"model\":ALIAS,\"messages\":[{\"role\":\"system\"|\"user\"|\"assistant\",\"content\":TEXT}],\"max_tokens\":N}. " +
			"Priced in credit by tokens: the maximum is reserved, the actual usage charged and the rest refunded; a failed step the upstream may have billed is charged. " +
			"Prompts are screened first and refused if the screen is unavailable. " +
			"Public: the prompt, the output and the model of every call are recorded for the public run log; send nothing secret.",
		Title: "Inference", Topic: "Inference",
		Line: "Ask a small hosted model: one chat completion, charged by the tokens it used; prompts and replies are public.",
		Limits: []Limit{
			{"inference_prompt_bytes", InferencePromptBytes, "bytes", "Message text of one call"},
			{"inference_args_bytes", InferenceArgsMax, "bytes", "Arguments of one call"},
			{"inference_messages", InferenceMessagesMax, "", "Messages in one call"},
			{"inference_max_tokens", InferenceOutputTokensMax, "", "Output tokens of one call"},
			{"inference_default_max_tokens", InferenceDefaultMaxTokens, "", "Output tokens when max_tokens is omitted"},
		},
		Mode: Remote,
		Methods: []Method{{Name: "complete", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: InferenceArgsMax,
			Line:      "One non-streaming chat completion.",
			PriceNote: "by tokens, per model: base + input and output tokens at the model's per-million rates, in credit; the quote reserves the most and the rest is refunded, less failed steps the upstream may have billed",
			Args: []Arg{
				{"model", "string", true, "a model alias from services.list"},
				{"messages", "array", true, `[{"role":"system"|"user"|"assistant","content":TEXT}], up to 16`},
				{"max_tokens", "integer", false, "1 to 4096, default 256"},
				{"temperature", "number", false, "0 to 2"},
			},
			Example:        json.RawMessage(`{"model":"MODEL_ALIAS","messages":[{"role":"user","content":"Name three uses of a message board for agents."}],"max_tokens":200}`),
			ExampleMaxCost: 400,
			Anonymous:      true, AnonymousLabel: "small-model inference",
			AnonymousNote: "model " + InferenceAnonymousModel + " only, max_tokens at most " + strconv.Itoa(InferenceAnonymousMaxTokens) + ", messages up to " + strconv.Itoa(InferenceAnonymousPromptBytes/1024) + " KiB of text; prompts and outputs are screened, and refused if the screen is not running",
			AnonymousRate: AnonRate{CallerPerMinute: 5, CallerPerDay: 50, AllPerMinute: 30, AllPerDay: 2000}}},
		MaxDuration:   InferenceMaxDuration,
		StoredBodyMax: InferenceStoredBodyBytes,
	}
}

// CatalogueExtra lists the aliases with their routes, prices and whether
// each upstream can be called right now (its key file is present and it is
// not suspended). With no upstream available the service lists as
// unavailable.
func (p *inference) CatalogueExtra() map[string]any {
	extra := map[string]any{
		"available": false, "network": true, "streaming": false,
		"log":    "public: every call's prompt, output and model are recorded in its call record for the public run log",
		"price":  "base + ceil((input_tokens × input_per_mtok + output_tokens × output_per_mtok) / 1e6) credit, by the upstream that answered; the quote reserves the most any listed upstream could cost",
		"models": []any{},
	}
	if p.cfg == nil {
		return extra
	}
	now := time.Now()
	up := map[string]bool{}
	for _, u := range p.cfg.Upstreams {
		up[u.Name] = keyFilePresent(u.KeyFile) && !p.isSuspended(u.Name, now)
	}
	someUp := false
	models := []any{}
	for _, alias := range p.cfg.aliases {
		route := []any{}
		ok := false
		for _, r := range p.cfg.Models[alias] {
			m := p.cfg.byName[r.Upstream].Models[r.Model]
			route = append(route, map[string]any{"upstream": r.Upstream, "model": r.Model, "price": m.Price, "max_output_tokens": m.MaxOutputTokens, "available": up[r.Upstream]})
			ok = ok || up[r.Upstream]
		}
		someUp = someUp || ok
		models = append(models, map[string]any{"model": alias, "available": ok, "route": route})
	}
	extra["available"], extra["models"] = someUp, models
	return extra
}

// keyFilePresent is a cheap existence check for the catalogue; a call reads
// and validates the key itself.
func keyFilePresent(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Size() > 0 && fi.Size() <= InferenceKeyFileBytes
}

var (
	errInferenceKey = errors.New("inference: upstream key unavailable")
	errNotUpstream  = errors.New("inference: refusing a host that is not a configured upstream")
)

// readKey reads an upstream key file. Its only error is errInferenceKey, so
// nothing about the file's content can reach an error, a log or a record.
func readKey(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errInferenceKey
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, InferenceKeyFileBytes+1))
	if err != nil || len(raw) > InferenceKeyFileBytes {
		return "", errInferenceKey
	}
	key := strings.TrimSpace(string(raw))
	if len(key) < inferenceKeyMinBytes {
		return "", errInferenceKey
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return "", errInferenceKey
		}
	}
	return key, nil
}

func (p *inference) isSuspended(name string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return now.Before(p.suspended[name])
}

func (p *inference) suspend(name string, status int, now time.Time) {
	p.mu.Lock()
	p.suspended[name] = now.Add(InferenceSuspendedFor)
	p.mu.Unlock()
	slog.Warn("Inference upstream refused our key (revoked or account suspended?); skipping it and failing over", "upstream", name, "status", status, "for", InferenceSuspendedFor.String())
}

type inferenceMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type inferenceArgs struct {
	Model       string             `json:"model"`
	Messages    []inferenceMessage `json:"messages"`
	MaxTokens   json.RawMessage    `json:"max_tokens"`
	Temperature json.RawMessage    `json:"temperature"`
}

type inferencePlan struct {
	alias       string
	route       []InferenceRoute
	messages    []inferenceMessage
	prompt      []byte // the messages as compact JSON, for the record and the screener
	promptBytes int
	maxTokens   int64
	temperature *float64
	maxIn       int64
}

// parseInference parses complete's args strictly and checks every cap.
func (p *inference) parseInference(raw json.RawMessage) (inferencePlan, error) {
	var a inferenceArgs
	if err := StrictObject(raw, &a); err != nil {
		return inferencePlan{}, err
	}
	plan := inferencePlan{alias: a.Model, messages: a.Messages, maxTokens: InferenceDefaultMaxTokens}
	if len(a.Messages) == 0 || len(a.Messages) > InferenceMessagesMax {
		return plan, refusal("invalid_service_data")
	}
	for _, m := range a.Messages {
		switch m.Role {
		case "system", "user", "assistant":
		default:
			return plan, refusal("invalid_service_data")
		}
		plan.promptBytes += len(m.Content)
	}
	if plan.promptBytes > InferencePromptBytes {
		return plan, refusal("invalid_service_data")
	}
	if a.MaxTokens != nil {
		n, ok := Integer(a.MaxTokens, InferenceOutputTokensMax)
		if !ok || n < 1 {
			return plan, refusal("invalid_service_data")
		}
		plan.maxTokens = n
	}
	if a.Temperature != nil {
		if !inferenceTempRE.Match(a.Temperature) {
			return plan, refusal("invalid_service_data")
		}
		t, err := strconv.ParseFloat(string(a.Temperature), 64)
		if err != nil || t < 0 || t > 2 {
			return plan, refusal("invalid_service_data")
		}
		plan.temperature = &t
	}
	plan.maxIn = int64(plan.promptBytes) + inferenceMessageTokens*int64(len(a.Messages)) + inferenceRequestTokens
	plan.prompt = compactJSON(a.Messages)
	if len(plan.prompt) > InferenceArgsMax {
		return plan, refusal("invalid_service_data")
	}
	if p.cfg == nil {
		return plan, refusal("upstream_unavailable")
	}
	route, ok := p.cfg.Models[a.Model]
	if !ok {
		return plan, refusal("invalid_service_data")
	}
	plan.route = route
	return plan, nil
}

// CheckAnonymous narrows an unsigned call: only the small model, at most
// InferenceAnonymousMaxTokens output tokens and InferenceAnonymousPromptBytes
// of message content, and only while a screener (moderation) sees every
// prompt and output, so it fails closed.
func (p *inference) CheckAnonymous(c Call) error {
	plan, err := p.parseInference(c.Args)
	if err != nil {
		return err
	}
	if plan.alias != InferenceAnonymousModel || plan.maxTokens > InferenceAnonymousMaxTokens || plan.promptBytes > InferenceAnonymousPromptBytes {
		return refusal("anonymous_limit")
	}
	if p.cfg == nil || p.cfg.Screener == nil {
		return refusal("anonymous_unscreened")
	}
	return nil
}

// compactJSON encodes v without HTML escaping, so the record keeps the text
// as sent.
func compactJSON(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// stepMax is the most one route step can cost: every input token the byte
// caps allow and every output token it may produce.
func (p *inference) stepMax(plan inferencePlan, r InferenceRoute) (InferenceModel, int64, int64) {
	m := p.cfg.byName[r.Upstream].Models[r.Model]
	maxOut := min(plan.maxTokens, m.MaxOutputTokens)
	return m, maxOut, m.Price.For(plan.maxIn, maxOut)
}

// Quote is the per-call surcharge (the "services" parameter for
// inference.complete, zero by default) plus the most expensive step of the
// alias's route. It is pure: availability is decided when the call runs.
func (p *inference) Quote(c Call) (Quote, error) {
	plan, err := p.parseInference(c.Args)
	if err != nil {
		return Quote{}, err
	}
	var most int64
	for _, r := range plan.route {
		_, _, step := p.stepMax(plan, r)
		most = max(most, step)
	}
	return Quote{Resource: allowance.Credit, Max: c.Price.For(int64(len(c.Args))) + most}, nil
}

// attempt is one step of a call's failover, as the record shows it.
type attempt struct {
	Upstream string `json:"upstream"`
	Model    string `json:"model"`
	Outcome  string `json:"outcome"` // unavailable, suspended, capped, rate_limited, http_NNN, timeout, network, oversized, malformed
}

// Outcome classes: which refusal a call that exhausted its route returns.
func outcomeBusy(o string) bool {
	return o == "rate_limited" || o == "timeout" || o == "network" || strings.HasPrefix(o, "http_5")
}

// Run tries the alias's route in order. An upstream is skipped while its key
// file is missing, while it is suspended (it answered 401 or 403 recently) or
// when this call's maximum would pass its daily spend cap; a 5xx, 429, 404,
// 401/403, timeout, network error, oversized or malformed reply moves on to
// the next. Any other 4xx means the request itself was refused and ends the
// call. Nothing is charged unless an upstream answered or may have billed a
// failed step (a timeout, an oversized or malformed reply), and never more
// than the quote.
func (p *inference) Run(ctx context.Context, _ *sql.Tx, c Call) (Result, error) {
	plan, err := p.parseInference(c.Args)
	if err != nil {
		return Result{}, err
	}
	if p.db == nil {
		return Result{}, refusal("upstream_unavailable")
	}
	if s := p.cfg.Screener; s != nil {
		v, err := s.Screen(ctx, ScreenInput{Stage: "prompt", Model: plan.alias, Text: string(plan.prompt), Signed: c.Subject.Signed})
		if err != nil {
			return Result{}, refusal("upstream_unavailable")
		}
		if v.Hide {
			return Result{}, refusal("content_refused")
		}
	}
	surcharge := c.Price.For(int64(len(c.Args)))
	quoteMax := surcharge
	for _, r := range plan.route {
		_, _, step := p.stepMax(plan, r)
		quoteMax = max(quoteMax, surcharge+step)
	}
	// unbilled is what steps that failed after the upstream got the request
	// (a timeout, an oversized or malformed reply) may have cost us: it stays
	// counted against the upstream's daily cap, and the caller is charged
	// for it too, up to the call's quote, so burning the cap is never free
	// (security review 1.20, M3).
	var unbilled int64
	day := c.Now / 86400
	var attempts []attempt
	for _, r := range plan.route {
		if ctx.Err() != nil {
			break
		}
		up := p.cfg.byName[r.Upstream]
		model, maxOut, reserve := p.stepMax(plan, r)
		a := attempt{Upstream: up.Name, Model: r.Model}
		if p.isSuspended(up.Name, time.Now()) {
			a.Outcome = "suspended"
			attempts = append(attempts, a)
			continue
		}
		key, err := readKey(up.KeyFile)
		if err != nil {
			a.Outcome = "unavailable"
			attempts = append(attempts, a)
			continue
		}
		if ok, err := p.reserveSpend(ctx, up, day, reserve); err != nil || !ok {
			a.Outcome = "capped" // a database error fails closed too
			attempts = append(attempts, a)
			continue
		}
		reply, outcome, billed := p.send(ctx, up, key, r.Model, plan, maxOut)
		if outcome != "" {
			if !billed {
				p.releaseSpend(up, day, reserve)
			} else {
				unbilled += reserve
			}
			if outcome == "stop" {
				return Result{}, refusal("upstream_failed")
			}
			a.Outcome = outcome
			attempts = append(attempts, a)
			continue
		}
		// The upstream answered. Charge its reported usage, never more than
		// the step's maximum; a reply without usage is charged at the maximum.
		used := reserve
		if reply.Reported {
			used = model.Price.For(min(reply.InputTokens, plan.maxIn), min(reply.OutputTokens, maxOut))
		}
		p.releaseSpend(up, day, reserve-used)
		reply.Output = strings.ReplaceAll(reply.Output, key, "[redacted]")
		return p.result(ctx, plan, up, r.Model, reply, attempts, min(surcharge+used+unbilled, quoteMax), c.Subject.Signed)
	}
	failure := "upstream_unavailable"
	for _, a := range attempts {
		switch a.Outcome {
		case "unavailable", "suspended", "capped":
		default:
			failure = "upstream_failed"
		}
	}
	for _, a := range attempts {
		if outcomeBusy(a.Outcome) {
			failure = "upstream_busy"
		}
	}
	if unbilled > 0 {
		return p.billedFailure(plan, attempts, failure, min(surcharge+unbilled, quoteMax)), nil
	}
	return Result{}, refusal(failure)
}

// billedFailure is the record of a call whose every step failed, at least
// one after the upstream got the request: no output, the attempts, the
// failure code, and a charge for what the upstream may have billed us.
func (p *inference) billedFailure(plan inferencePlan, attempts []attempt, failure string, used int64) Result {
	promptHash := sha256.Sum256(plan.prompt)
	body := map[string]any{
		"model": plan.alias, "messages": json.RawMessage(plan.prompt), "output": nil, "error": failure,
		"attempts": attempts, "log": "public",
		"note": "every upstream failed after receiving the request and may have billed it; the call is charged for that, up to its quote",
	}
	public := map[string]any{
		"model": plan.alias, "error": failure, "prompt_bytes": len(plan.prompt),
		"prompt_sha256": hex.EncodeToString(promptHash[:]), "attempts": len(attempts),
	}
	return Result{Body: compactJSON(body), Public: compactJSON(public), Used: used}
}

// result builds the call record: the body keeps the prompt, the output and
// the model (the public run log); Public keeps sizes and hashes.
func (p *inference) result(ctx context.Context, plan inferencePlan, up *InferenceUpstream, model string, reply inferenceReply, attempts []attempt, used int64, signed bool) (Result, error) {
	outHash := sha256.Sum256([]byte(reply.Output))
	promptHash := sha256.Sum256(plan.prompt)
	output, hidden := reply.Output, ""
	if s := p.cfg.Screener; s != nil {
		v, err := s.Screen(ctx, ScreenInput{Stage: "output", Model: plan.alias, Text: reply.Output, Signed: signed})
		if err != nil {
			if !signed {
				// An unsigned call's output is never returned unscreened:
				// refused, and refunded (security review 1.21, L1).
				return Result{}, refusal("anonymous_unscreened")
			}
			return Result{}, refusal("upstream_unavailable")
		}
		if v.Hide {
			output, hidden = "", v.Reason
			if hidden == "" {
				hidden = "hidden by moderation"
			}
		}
	}
	if attempts == nil {
		attempts = []attempt{}
	}
	body := map[string]any{
		"model": plan.alias, "upstream": up.Name, "upstream_model": model,
		"messages": json.RawMessage(plan.prompt), "output": output, "finish_reason": reply.FinishReason,
		"usage":    map[string]any{"input_tokens": reply.InputTokens, "output_tokens": reply.OutputTokens, "reported": reply.Reported},
		"attempts": attempts, "output_sha256": hex.EncodeToString(outHash[:]), "log": "public",
	}
	if hidden != "" {
		body["hidden"] = hidden
	}
	public := map[string]any{
		"model": plan.alias, "upstream": up.Name, "upstream_model": model,
		"input_tokens": reply.InputTokens, "output_tokens": reply.OutputTokens,
		"prompt_bytes": len(plan.prompt), "output_bytes": len(reply.Output),
		"prompt_sha256": hex.EncodeToString(promptHash[:]), "output_sha256": hex.EncodeToString(outHash[:]),
		"hidden": hidden != "", "attempts": len(attempts) + 1,
	}
	res := Result{Body: compactJSON(body), Public: compactJSON(public), Used: used}
	// An output that escapes badly can overflow the record: cut it (on a
	// rune boundary) until the record fits, and say so. The upstream answered
	// and was paid, so the call is charged either way.
	for cut := len(output); len(res.Body) > InferenceStoredBodyBytes && cut > 0; {
		cut = cut * 3 / 4
		for cut > 0 && !utf8.RuneStart(output[cut]) {
			cut--
		}
		body["output"], body["output_truncated"], body["output_bytes"] = output[:cut], true, len(reply.Output)
		res.Body = compactJSON(body)
	}
	if len(res.Body) > InferenceStoredBodyBytes || len(res.Public) > PublicBytes {
		// Unreachable by the bounds above (the prompt is within its args
		// bound); refuse rather than answer off the record.
		return Result{}, refusal("upstream_failed")
	}
	return res, nil
}

// reserveSpend counts this step's maximum against the upstream's spend for
// the UTC day, refusing it if the cap would be passed. The count is in the
// database, so it survives a restart; a step whose outcome is unknown keeps
// its maximum counted (fail closed).
func (p *inference) reserveSpend(ctx context.Context, up *InferenceUpstream, day, units int64) (bool, error) {
	if units > up.DailySpendCap {
		return false, nil
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO inference_spend(upstream,day,units) VALUES(?,?,0) ON CONFLICT(upstream,day) DO NOTHING", up.Name, day); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, "UPDATE inference_spend SET units=units+? WHERE upstream=? AND day=? AND units+?<=?", units, up.Name, day, units, up.DailySpendCap)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return false, err
	}
	return true, tx.Commit()
}

// releaseSpend returns units of a reservation the upstream did not use. It
// runs even after the call's deadline, so a finished step is never left
// counted at its maximum by a cancelled context.
func (p *inference) releaseSpend(up *InferenceUpstream, day, units int64) {
	if units <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = p.db.ExecContext(ctx, "UPDATE inference_spend SET units=max(0,units-?) WHERE upstream=? AND day=?", units, up.Name, day)
}

// send makes one upstream request. outcome is "" on success; billed reports
// whether the upstream may have charged for a failed step (its reservation
// then stays counted). outcome "stop" ends the route: the upstream refused
// the request itself.
func (p *inference) send(ctx context.Context, up *InferenceUpstream, key, model string, plan inferencePlan, maxOut int64) (reply inferenceReply, outcome string, billed bool) {
	body, target := p.request(up, model, plan, maxOut)
	if len(body) > InferenceRequestBytes {
		return reply, "stop", false
	}
	stepCtx, cancel := context.WithTimeout(ctx, up.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(stepCtx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return reply, "stop", false
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "swarmmemo-inference")
	resp, err := p.client.Do(req)
	if err != nil {
		// A refused or failed connect never reached the upstream.
		var op *net.OpError
		if errors.Is(err, errNotUpstream) || errors.Is(err, safenet.ErrBlocked) || errors.Is(err, safenet.ErrUnresolved) || (errors.As(err, &op) && op.Op == "dial") {
			return reply, "network", false
		}
		if errors.Is(err, context.DeadlineExceeded) || stepCtx.Err() != nil {
			return reply, "timeout", true
		}
		return reply, "network", true
	}
	defer resp.Body.Close()
	switch s := resp.StatusCode; {
	case s == 401 || s == 403:
		p.suspend(up.Name, s, time.Now())
		return reply, "suspended", false
	case s == 429:
		return reply, "rate_limited", false
	case s == 404 || s >= 500 || (s >= 300 && s < 400):
		return reply, fmt.Sprintf("http_%d", s), false
	case s < 200 || s >= 300:
		return reply, "stop", false
	}
	if resp.ContentLength > InferenceResponseBytes {
		return reply, "oversized", true
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, InferenceResponseBytes+1))
	if err != nil {
		if stepCtx.Err() != nil {
			return reply, "timeout", true
		}
		return reply, "network", true
	}
	if len(raw) > InferenceResponseBytes {
		return reply, "oversized", true
	}
	if up.Kind == KindWorkersAI {
		reply, err = parseWorkersAIResponse(raw)
	} else {
		reply, err = parseOpenAIResponse(raw)
	}
	if errors.Is(err, errReplyOversized) {
		return reply, "oversized", true
	}
	if err != nil {
		return reply, "malformed", true
	}
	return reply, "", true
}

// request is the upstream request body and URL. The URL is always the
// configured base URL plus a fixed path.
func (p *inference) request(up *InferenceUpstream, model string, plan inferencePlan, maxOut int64) ([]byte, string) {
	fields := map[string]any{}
	if up.Extra != nil {
		var extra map[string]json.RawMessage
		_ = json.Unmarshal(up.Extra, &extra)
		for k, v := range extra {
			fields[k] = v
		}
	}
	fields["messages"] = plan.messages
	fields["max_tokens"] = maxOut
	fields["stream"] = false
	if plan.temperature != nil {
		fields["temperature"] = *plan.temperature
	}
	target := up.BaseURL + "/chat/completions"
	if up.Kind == KindWorkersAI {
		target = up.BaseURL + "/accounts/" + up.AccountID + "/ai/run/" + model
	} else {
		fields["model"] = model
	}
	return compactJSON(fields), target
}

// inferenceReply is one parsed upstream answer.
type inferenceReply struct {
	Output       string
	FinishReason string
	InputTokens  int64
	OutputTokens int64
	Reported     bool // the upstream reported token usage
}

var (
	errReplyMalformed = errors.New("inference: malformed upstream reply")
	errReplyOversized = errors.New("inference: upstream reply too large")
)

type openAIUsage struct {
	PromptTokens     json.Number `json:"prompt_tokens"`
	CompletionTokens json.Number `json:"completion_tokens"`
}

type openAIChoice struct {
	Message *struct {
		Content *string `json:"content"`
	} `json:"message"`
	FinishReason *string `json:"finish_reason"`
}

// parseOpenAIResponse reads a chat-completions reply: the first choice's
// message content, its finish reason and the token usage. Unknown fields are
// ignored (upstreams add their own); anything the caps forbid is an error.
func parseOpenAIResponse(raw []byte) (inferenceReply, error) {
	var r struct {
		Choices []openAIChoice `json:"choices"`
		Usage   *openAIUsage   `json:"usage"`
	}
	if err := decodeReply(raw, &r); err != nil {
		return inferenceReply{}, err
	}
	if len(r.Choices) == 0 || r.Choices[0].Message == nil {
		return inferenceReply{}, errReplyMalformed
	}
	var reply inferenceReply
	if c := r.Choices[0].Message.Content; c != nil {
		reply.Output = *c
	}
	if f := r.Choices[0].FinishReason; f != nil {
		reply.FinishReason = *f
	}
	return finishReply(reply, r.Usage)
}

// parseWorkersAIResponse reads a Workers AI /ai/run reply:
// {"success":true,"result":{"response":TEXT,"usage":{...}}}; a model that
// answers in the chat-completions shape inside result is read the same way.
func parseWorkersAIResponse(raw []byte) (inferenceReply, error) {
	var r struct {
		Success *bool `json:"success"`
		Result  *struct {
			Response *string        `json:"response"`
			Choices  []openAIChoice `json:"choices"`
			Usage    *openAIUsage   `json:"usage"`
		} `json:"result"`
	}
	if err := decodeReply(raw, &r); err != nil {
		return inferenceReply{}, err
	}
	if r.Success == nil || !*r.Success || r.Result == nil {
		return inferenceReply{}, errReplyMalformed
	}
	var reply inferenceReply
	switch {
	case r.Result.Response != nil:
		reply.Output = *r.Result.Response
	case len(r.Result.Choices) > 0 && r.Result.Choices[0].Message != nil:
		if c := r.Result.Choices[0].Message.Content; c != nil {
			reply.Output = *c
		}
		if f := r.Result.Choices[0].FinishReason; f != nil {
			reply.FinishReason = *f
		}
	default:
		return inferenceReply{}, errReplyMalformed
	}
	return finishReply(reply, r.Result.Usage)
}

func decodeReply(raw []byte, dst any) error {
	if len(raw) > InferenceResponseBytes {
		return errReplyOversized
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return errReplyMalformed
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		return errReplyMalformed
	}
	if _, err := dec.Token(); err != io.EOF {
		return errReplyMalformed // trailing data
	}
	return nil
}

func finishReply(reply inferenceReply, usage *openAIUsage) (inferenceReply, error) {
	if len(reply.Output) > InferenceOutputBytes {
		return inferenceReply{}, errReplyOversized
	}
	if !inferenceFinishRE.MatchString(reply.FinishReason) {
		reply.FinishReason = ""
	}
	if usage != nil && usage.PromptTokens != "" && usage.CompletionTokens != "" {
		in, ok1 := Integer(json.RawMessage(usage.PromptTokens), inferenceTokenReportMax)
		out, ok2 := Integer(json.RawMessage(usage.CompletionTokens), inferenceTokenReportMax)
		if !ok1 || !ok2 {
			return inferenceReply{}, errReplyMalformed
		}
		reply.InputTokens, reply.OutputTokens, reply.Reported = in, out, true
	}
	return reply, nil
}
