package services

// corroborate answers "what would it cost to fake this identity?" for an EVM
// address set, as a public utility (RFC0015 §3.4, §13): Corroborate's
// resolver, run by the board's loopback sidecar (workers/corroborate) against
// a pinned registry revision, reading every credential from public chains.
//
// It is a free read: no key, no credit, nothing recorded. A network (the
// anonymous subject) and a key each have their own per-minute and per-day
// window; answers are cached by address set and block for an hour, so a
// repeat costs neither the caller's window nor the chains anything. When the
// sidecar cannot answer, the read is refused with service_unavailable and a
// retry_after: never a zero score.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/corroborate"
)

// CorroborateID is the service id.
const CorroborateID = "corroborate"

const (
	corroborateArgsMax  = 1024
	corroborateCacheMax = 2000
	// CorroborateCacheTTL is how long a complete answer is kept;
	// corroboratePartialTTL one where some checks could not be read.
	CorroborateCacheTTL   = time.Hour
	corroboratePartialTTL = time.Minute
	// corroborateCallerMax bounds the remembered windows, as public_data's.
	corroborateCallerMax = 100000
	// RefusalCorroborateUnavailable is the refusal the board answers with
	// 503 service_unavailable and the sidecar's retry_after.
	RefusalCorroborateUnavailable = "corroborate_unavailable"
)

// corroborateLimits are each caller's resolves (cache misses) per minute and
// per UTC day: a network without a key, or a key.
var corroborateLimits = map[bool]pdLimit{false: {10, 200}, true: {30, 2000}}

type corroborateEntry struct {
	body  json.RawMessage
	until time.Time
}

type corroborateSvc struct {
	client *corroborate.Client
	now    func() time.Time

	mu      sync.Mutex
	cache   map[string]corroborateEntry
	callers map[string]*pdWindow
	locks   [32]sync.Mutex // striped by cache key: one resolve per address set at a time
}

func newCorroborate(d Deps) Provider {
	return &corroborateSvc{client: d.Corroborate, now: time.Now, cache: map[string]corroborateEntry{}, callers: map[string]*pdWindow{}}
}

func (*corroborateSvc) Describe() Descriptor {
	return Descriptor{
		ID: CorroborateID,
		Summary: "What it would cost an adversary to fake the identity behind an EVM address set, by Corroborate's rule: credentials grouped by the trust root they actually check, " +
			"the strongest per root counted, roots summed, each priced at min(forge, rent) times its age curve. Read from public chains against a pinned registry revision. " +
			"A score, never a verdict. resolve args: {\"addresses\":\"0x...,0x...\"} (1 to " + itoa(corroborate.MaxAddresses) + "), optional \"as_of\" (a Sepolia registry block).",
		Title: "Corroborate", Topic: "Cost to fake",
		Line: "What faking the identity behind up to " + itoa(corroborate.MaxAddresses) + " EVM addresses would cost: a score, never a verdict.",
		Limits: []Limit{
			{"corroborate_addresses", corroborate.MaxAddresses, "", "Addresses in one resolve"},
			{"corroborate_resolves_per_minute_without_key", corroborateLimits[false].PerMinute, "", "Resolves per network a minute, without a key (cached answers are not counted)"},
			{"corroborate_resolves_per_day_without_key", corroborateLimits[false].PerDay, "", "Resolves per network a UTC day, without a key"},
			{"corroborate_resolves_per_minute", corroborateLimits[true].PerMinute, "", "Resolves per agent a minute"},
			{"corroborate_resolves_per_day", corroborateLimits[true].PerDay, "", "Resolves per agent a UTC day"},
			{"corroborate_cache_seconds", int64(CorroborateCacheTTL / time.Second), "seconds", "How long an answer is kept for the same address set"},
		},
		Mode: Remote,
		Methods: []Method{
			{Name: "resolve", ArgsMax: corroborateArgsMax,
				Line: "Score an address set: total cents to fake it, the log score, each trust root's contribution with its forge, rent and age, and the registry revision it was priced at.",
				Args: []Arg{
					{"addresses", "string", true, "1 to " + itoa(corroborate.MaxAddresses) + " EVM addresses, comma-separated (or a JSON array); mixed case must be a valid EIP-55 checksum. Name only addresses you know belong together"},
					{"as_of", "integer", false, "a Sepolia block: price against the registry as it stood then"},
				},
				Example:  json.RawMessage(`{"addresses":"0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045"}`),
				Keywords: []string{"proof of personhood", "sybil", "cost to fake", "personhood score", "wallet reputation", "humanity check", "world id", "proof of humanity"}},
		},
		MaxDuration: corroborate.Timeout + 5*time.Second,
	}
}

// CatalogueExtra says the service is free and what an answer means.
func (s *corroborateSvc) CatalogueExtra() map[string]any {
	return map[string]any{"available": s.client != nil, "network": true, "free": true, "tool_page": "/tools/corroborate",
		"unit": "usd_cent", "score": "log10(1 + total_cents)", "verdict": "none: a score, never a yes or no; the threshold is yours"}
}

type corroborateArgs struct {
	Addresses json.RawMessage `json:"addresses"`
	AsOf      json.RawMessage `json:"as_of"`
}

// parseCorroborate returns the address set, EIP-55 checksummed, deduplicated
// and sorted (the cache key and the sidecar's request alike), and the block.
func parseCorroborate(raw json.RawMessage) ([]string, int64, error) {
	var a corroborateArgs
	if err := StrictObject(raw, &a); err != nil {
		return nil, 0, err
	}
	var list []string
	var one string
	switch {
	case len(a.Addresses) == 0:
		return nil, 0, badArg("addresses is required: 1 to " + itoa(corroborate.MaxAddresses) + " EVM addresses, comma-separated.")
	case json.Unmarshal(a.Addresses, &one) == nil:
		for _, p := range strings.Split(one, ",") {
			if p = strings.TrimSpace(p); p != "" {
				list = append(list, p)
			}
		}
	case json.Unmarshal(a.Addresses, &list) != nil:
		return nil, 0, badArg("addresses takes EVM addresses, comma-separated or as a JSON array of strings.")
	}
	if len(list) == 0 || len(list) > corroborate.MaxAddresses {
		return nil, 0, badArg("addresses takes 1 to " + itoa(corroborate.MaxAddresses) + " EVM addresses.")
	}
	var out []string
	for _, s := range list {
		// 0x and 40 hex digits; mixed case only as a valid EIP-55 checksum.
		addr, ok := ParseEVMAddress(s)
		if !ok || !strings.HasPrefix(s, "0x") {
			return nil, 0, badArg("addresses takes EVM addresses: 0x and 40 hex digits, all one case or a valid EIP-55 checksum.")
		}
		if c := addr.String(); !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	var asOf int64
	if len(a.AsOf) > 0 {
		n, ok := Integer(a.AsOf, 1<<53)
		if !ok || n < corroborate.RegistryGenesisBlock {
			return nil, 0, badArg("as_of is a Sepolia block number, " + itoa(corroborate.RegistryGenesisBlock) + " (the registry's first) or later.")
		}
		asOf = n
	}
	return out, asOf, nil
}

func corroborateKey(addresses []string, asOf int64) string {
	key := strings.ToLower(strings.Join(addresses, ","))
	if asOf != 0 {
		key += "@" + itoa(asOf)
	}
	return key
}

// Quote and Run are never reached: corroborate has no write method.
func (*corroborateSvc) Quote(Call) (Quote, error) { return Quote{}, refusal("invalid_service_data") }

func (*corroborateSvc) Run(context.Context, *sql.Tx, Call) (Result, error) {
	return Result{}, refusal("invalid_service_data")
}

// Read is never reached for resolve: ReadRemote serves it.
func (*corroborateSvc) Read(context.Context, allowance.Querier, Call) (json.RawMessage, error) {
	return nil, refusal("invalid_service_data")
}

// ReadRemote checks the arguments and the caller's window in the command's
// transaction (no I/O), and answers from the cache when it can; otherwise it
// returns the resolve, which runs once the transaction has committed.
func (s *corroborateSvc) ReadRemote(_ context.Context, _ allowance.Querier, c Call) (func(context.Context) (json.RawMessage, error), error) {
	addresses, asOf, err := parseCorroborate(c.Args)
	if err != nil {
		return nil, err
	}
	if s.client == nil {
		return nil, &allowance.Err{Code: RefusalCorroborateUnavailable, RetryAfter: 300}
	}
	key := corroborateKey(addresses, asOf)
	if body, ok := s.cached(key); ok {
		return func(context.Context) (json.RawMessage, error) { return body, nil }, nil
	}
	if err := s.admit(c); err != nil {
		return nil, err
	}
	return func(ctx context.Context) (json.RawMessage, error) { return s.resolve(ctx, key, addresses, asOf) }, nil
}

// admit counts one resolve in the caller's window: a network without a
// key, or the signed caller's account.
func (s *corroborateSvc) admit(c Call) error {
	limit := corroborateLimits[c.Subject.Signed]
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.callers[c.Subject.ID]
	if w == nil {
		if len(s.callers) >= corroborateCallerMax {
			for k, v := range s.callers {
				if v.minute != c.Now/60 && v.day != c.Now/86400 {
					delete(s.callers, k)
				}
			}
			if len(s.callers) >= corroborateCallerMax {
				return &allowance.Err{Code: "request_rate", RetryAfter: 60}
			}
		}
		w = &pdWindow{}
		s.callers[c.Subject.ID] = w
	}
	if ok, retry := w.take(limit, c.Now, 1); !ok {
		return &allowance.Err{Code: "request_rate", RetryAfter: retry}
	}
	return nil
}

func (s *corroborateSvc) cached(key string) (json.RawMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cache[key]
	if !ok {
		return nil, false
	}
	if !s.now().Before(e.until) {
		delete(s.cache, key)
		return nil, false
	}
	return e.body, true
}

func (s *corroborateSvc) store(key string, body json.RawMessage, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if len(s.cache) >= corroborateCacheMax {
		for k, e := range s.cache {
			if !now.Before(e.until) {
				delete(s.cache, k)
			}
		}
		for k := range s.cache {
			if len(s.cache) < corroborateCacheMax {
				break
			}
			delete(s.cache, k)
		}
	}
	s.cache[key] = corroborateEntry{body: body, until: now.Add(ttl)}
}

// corroborateAnswer is the result: the sidecar's answer, re-encoded field by
// field (nothing it adds passes through), and whether it came from the cache.
type corroborateAnswer struct {
	*corroborate.Result
	Cached bool `json:"cached"`
}

func (s *corroborateSvc) resolve(ctx context.Context, key string, addresses []string, asOf int64) (json.RawMessage, error) {
	lock := &s.locks[stripe(key, len(s.locks))]
	lock.Lock()
	defer lock.Unlock()
	if body, ok := s.cached(key); ok {
		return body, nil
	}
	r, err := s.client.Resolve(ctx, addresses, asOf)
	var unavailable *corroborate.ErrUnavailable
	var bad *corroborate.ErrRequest
	switch {
	case errors.As(err, &unavailable):
		return nil, &allowance.Err{Code: RefusalCorroborateUnavailable, RetryAfter: unavailable.RetryAfter}
	case errors.As(err, &bad) && bad.Code == "as_of_unavailable":
		return nil, badArg("as_of cannot be answered here: this resolver prices against its pinned registry revision only. Leave as_of out.")
	case errors.As(err, &bad):
		return nil, badArg("addresses takes 1 to " + itoa(corroborate.MaxAddresses) + " EVM addresses, and as_of a Sepolia block from the registry's first on.")
	case err != nil:
		return nil, &allowance.Err{Code: RefusalCorroborateUnavailable, RetryAfter: 30}
	}
	body, err := json.Marshal(corroborateAnswer{Result: r})
	if err != nil {
		return nil, &allowance.Err{Code: RefusalCorroborateUnavailable, RetryAfter: 30}
	}
	ttl := CorroborateCacheTTL
	if r.Checks.Unavailable > 0 {
		ttl = corroboratePartialTTL
	}
	cached, _ := json.Marshal(corroborateAnswer{Result: r, Cached: true})
	s.store(key, cached, ttl)
	return body, nil
}

// stripe picks one of n locks for key (FNV-1a).
func stripe(key string, n int) int {
	h := uint32(2166136261)
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return int(h % uint32(n))
}
