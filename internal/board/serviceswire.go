package board

// Service middleware wiring (RFC0012 §3), owned by builder C: services.list,
// service.read and service.call on every wire, over the engine in
// internal/services. With SERVICES empty (the default) nothing is built and
// every service operation answers 503 service_unavailable, as before.

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
)

// ServiceDataBytes bounds the data of one service.call while services are
// enabled: room for a full memory value (MemoryValueBytes) JSON-escaped.
// Other operations keep the 64 KiB data bound.
const ServiceDataBytes = services.MemoryPutArgsMax + 3072

func init() {
	// SERVICES may name exactly the built-in providers, so a new provider
	// needs no change in this package.
	KnownServices = services.Known()
}

// servicesState is the service engine the Store carries (Store.services); nil
// while SERVICES is empty.
type servicesState struct {
	engine     *services.Engine
	inference  *services.InferenceConfig  // nil unless SERVICES names inference
	runs       *services.RunsConfig       // nil unless SERVICES names runs
	publicData *services.PublicDataConfig // nil unless SERVICES names public_data
	fetch      *services.FetchConfig      // nil unless SERVICES names fetch and FETCH_CONFIG loads
	notaryKey  ed25519.PrivateKey         // nil unless SERVICES names notary, runs, screen or paste
	screener   services.TextScreener      // screen's classifier; nil unless MODERATION is on
	leaker     services.LeakScreener      // screen.leak's classifier (mode full); nil unless MODERATION is on
	// content caches ContentStats (contentwire.go) for contentStatsTTL.
	content contentStatsCache
	// wake caches WakeStats (wakenotarywire.go) for contentStatsTTL.
	wake wakeStatsCache
}

func (s *Store) openServices() error {
	if len(s.config.Features.Services) == 0 {
		return nil
	}
	if s.config.Features.ServiceEnabled("inference") {
		cfg, err := services.LoadInferenceConfig(s.config.Features.InferenceConfig)
		if err != nil {
			return err
		}
		if s.config.Features.Moderation {
			cfg.Screener = inferenceScreener{s} // prompts and outputs, through moderation's inference surfaces
		}
		s.services.inference = cfg
	}
	if s.config.Features.ServiceEnabled("runs") {
		cfg, err := services.LoadRunsConfig(s.config.Features.RunsConfig)
		if err != nil {
			return err
		}
		if cfg.ServiceID == "" && s.config.ServiceID != "" {
			cfg.ServiceID = s.config.ServiceID
		}
		if s.config.Features.Moderation && cfg.Screen == "required" {
			// Without MODERATION, "required" refuses every run; "static"
			// keeps the built-in miner rules.
			cfg.Screener = runScreener{s}
		}
		s.services.runs = cfg
	}
	if s.config.Features.ServiceEnabled("public_data") {
		dir := s.config.Features.PublicDataKeyDir
		if dir == "" {
			dir = services.PublicDataKeyDir
		}
		cfg, err := services.NewPublicDataConfig(dir)
		if err != nil {
			return err
		}
		s.services.publicData = cfg
	}
	if s.config.Features.ServiceEnabled("fetch") {
		cfg, err := services.LoadFetchConfig(s.config.Features.FetchConfig)
		if err != nil {
			return err
		}
		s.services.fetch = cfg
	}
	if s.config.Features.Moderation {
		s.services.screener, s.services.leaker = moderationScreener{s}, moderationScreener{s}
	}
	if s.config.Features.ServiceEnabled("notary") || s.config.Features.ServiceEnabled("runs") || s.config.Features.ServiceEnabled("screen") || s.config.Features.ServiceEnabled("paste") {
		key, err := services.LoadOrCreateNotaryKey(s.config.NotaryKeyFile)
		if err != nil {
			return err
		}
		s.services.notaryKey = key
	}
	s.services.engine = s.newServiceEngine(s.serviceMeter(), s.ledger.params)
	return nil
}

// serviceMeter is the ledger the services spend through: the store's one
// ledger, with its classifier, levers and parameters (openLedger runs first).
func (s *Store) serviceMeter() services.Meter { return s.allowanceLedger() }

func (s *Store) newServiceEngine(meter services.Meter, params allowance.ParamsSource) *services.Engine {
	return services.NewEngine(services.Config{
		DB:              s.db,
		Registry:        services.NewBuiltinRegistry(s.config.Features.Services, s.serviceDeps()),
		Meter:           meter,
		Params:          params,
		Now:             func() int64 { return s.now().Unix() },
		ReadsPerMinute:  MemoryReadsPerMinute,
		HoldsPerAccount: HoldsPerAccount,
		HoldsTotal:      HoldsTotal,
	})
}

// serviceDeps is what the board lends the providers: the account resolver,
// the database, the board view, the service ID, the tier classifier, the
// SSRF-safe webhook dialer (x402's) and each provider's parsed configuration.
func (s *Store) serviceDeps() services.Deps {
	return services.Deps{Accounts: accountResolver{}, DB: s.db, Dial: s.webhookDial, X402: s.config.X402,
		Inference: s.services.inference, Runs: s.services.runs, Board: serviceBoardView{}, ServiceID: s.config.ServiceID,
		PublicData: s.services.publicData, Classifier: s.classifier(), NotaryKey: s.services.notaryKey, TextScreener: s.services.screener, LeakScreener: s.services.leaker,
		ReceiverScreen: s.config.Features.ReceiverScreen, ContentScreen: s.config.Features.ContentScreen, ContentURL: s.config.Features.ContentURL,
		Fetch: s.services.fetch, EchoSimulate: s.config.EchoSimulate}
}

// UseServiceMeter replaces the ledger and price source the services use. It
// exists for wire tests in other packages, which have no ledger of their own
// to hand; call it right after Open. It does nothing while SERVICES is empty.
func (s *Store) UseServiceMeter(meter services.Meter, params allowance.ParamsSource) {
	if s.services.engine != nil {
		s.services.engine = s.newServiceEngine(meter, params)
	}
}

// UseTextScreener replaces screen's classifier (moderation's Jev). It exists
// for wire tests, which have no Jev; call it right after Open, before
// UseServiceMeter. It does nothing while SERVICES is empty.
func (s *Store) UseTextScreener(ts services.TextScreener) {
	if s.services.engine != nil {
		s.services.screener = ts
		s.services.engine = s.newServiceEngine(s.serviceMeter(), s.ledger.params)
	}
}

// UseLeakScreener replaces screen.leak's classifier, as UseTextScreener
// does screen.text's; for wire tests.
func (s *Store) UseLeakScreener(ls services.LeakScreener) {
	if s.services.engine != nil {
		s.services.leaker = ls
		s.services.engine = s.newServiceEngine(s.serviceMeter(), s.ledger.params)
	}
}

// X402StatsDays is the range /stats draws; the API's default.
const X402StatsDays = 7

// X402Stats is the pay-per-call relay's public spend summary for /stats and
// /api/stats/x402: USDC paid per UTC day, the caps and the catalogue's size;
// nil while x402 is off or unconfigured.
func (s *Store) X402Stats(ctx context.Context, days int) (*services.X402Stats, error) {
	if s.services.engine == nil {
		return nil, nil
	}
	return s.services.engine.Registry().ReadX402Stats(ctx, s.db, s.now().Unix(), days)
}

// X402Vet vets (or unvets) an open x402 catalogue resource: swarmmemo x402
// vet ID. Operator only; the running relay picks it up at its next
// catalogue load.
func (s *Store) X402Vet(ctx context.Context, id string, vet bool) (services.X402Vetting, error) {
	return services.VetX402(ctx, s.db, id, vet, s.now().Unix())
}

func (s *Store) startServices(ctx context.Context) {
	if s.services.engine != nil {
		s.services.engine.Start(ctx, time.Second)
	}
}

func (s *Store) stopServices() {
	if s.services.engine != nil {
		s.services.engine.Stop()
	}
}

// WorkServices runs one pass of the service worker (due async jobs, expired
// calls) now; the running store does this every second.
func (s *Store) WorkServices(ctx context.Context) (int, error) {
	if s.services.engine == nil {
		return 0, nil
	}
	return s.services.engine.Work(ctx)
}

// accountResolver maps an agent key fingerprint to its continuity account.
type accountResolver struct{}

func (accountResolver) Account(ctx context.Context, q allowance.Querier, agent string) (string, bool, error) {
	var account string
	err := q.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", agent).Scan(&account)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return account, err == nil, err
}

// Handle is the handle of the agent key fingerprint agent, "" for none or an
// unknown agent (services.HandleResolver).
func (accountResolver) Handle(ctx context.Context, q allowance.Querier, agent string) (string, error) {
	var handle string
	err := q.QueryRowContext(ctx, "SELECT handle FROM identities WHERE id=?", agent).Scan(&handle)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return handle, err
}

// readServices is services.list and service.read.
func (s *Store) readServices(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	e := s.services.engine
	if e == nil {
		return Result{}, allowanceError("service_unavailable")
	}
	if c.Operation == "services.list" {
		data, err := e.Catalogue(ctx, tx, now)
		if err != nil {
			return Result{}, serviceError(err)
		}
		data["without_key"] = s.noKey(ctx, tx, now)
		return Result{Data: data}, nil
	}
	out, err := e.ReadOutcome(ctx, tx, services.Request{Service: c.Target, Data: c.Data, Subject: subject(a)}, now)
	if err != nil {
		return Result{}, serviceError(err)
	}
	res := Result{Data: out.Data}
	if after := out.After; after != nil {
		// A read that needs the network (x402's Frames search) runs once
		// this transaction has committed, holding no connection.
		res.afterCommit = func() (Result, error) {
			data, err := after()
			if err != nil {
				return Result{}, serviceError(err)
			}
			return Result{Data: data}, nil
		}
	}
	return res, nil
}

// callService is service.call. A Local call runs and is charged in this
// transaction; a Remote or Async call reserves here, and runs and settles
// after commit through afterCommit.
func (s *Store) callService(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	e := s.services.engine
	if e == nil {
		if err := requireSigned(a); err != nil {
			return Result{}, err
		}
		return Result{}, allowanceError("service_unavailable")
	}
	if !a.signed {
		if err := s.admitUnsignedCall(ctx, tx, c, now); err != nil {
			s.logUnsignedCall(ctx, c, a, "", err)
			return Result{}, err
		}
	}
	out, err := e.Call(ctx, tx, services.Request{Service: c.Target, Data: c.Data, Subject: subject(a), RequestKey: serviceRequestKey(c)}, now)
	if err != nil {
		err = s.callError(ctx, tx, now, err, a)
		if !a.signed {
			s.logUnsignedCall(ctx, c, a, "", err)
		}
		return Result{}, err
	}
	if !a.signed {
		s.logUnsignedCall(ctx, c, a, callState(out.Data), nil)
	}
	res := Result{Data: out.Data}
	if after := out.After; after != nil {
		res.afterCommit = func() (Result, error) {
			data, err := after()
			if err != nil {
				return Result{}, serviceError(err)
			}
			return Result{Data: data}, nil
		}
	}
	return res, nil
}

// admitUnsignedCall is what an unsigned service.call must pass before the
// engine sees it: a method the catalogue marks anonymous, credit for the
// anonymous tier (none while the signed-services lever is pulled), and a
// request_id (its only retry key: it has no nonce).
func (s *Store) admitUnsignedCall(ctx context.Context, tx *sql.Tx, c Command, now int64) error {
	if !s.services.engine.Anonymous(c.Target, c.Data) {
		return s.anonymousNotAllowed(ctx, tx, now)
	}
	// The kill switch (the signed-services lever) and a zero anonymous
	// credit share both land here, before anything is parsed or reserved.
	if n := s.noKey(ctx, tx, now); !n.Available {
		return problem(403, "signed_only", "Service calls without a key are off right now: "+n.Why+". Sign the command with an Ed25519 key.")
	}
	// Everyone on the caller's network shares its request_id namespace, so
	// a short or guessable one could already be taken (security review
	// 1.21, L3).
	// Execute makes one when it is left out.
	if len(c.RequestID) < AnonymousRequestIDMin {
		return problem(400, "invalid_request", fmt.Sprintf("request_id is too short: leave it out and one is made for you, or use %d or more random characters, new per call (for example request_id=%s). Everyone on your network shares one request_id namespace, so a short one may already be taken.", AnonymousRequestIDMin, services.NewRequestID()))
	}
	return nil
}

// stampRequestID puts an unsigned call's request_id in its answer, as
// call.request_id, and when the board made it, says how to retry with it.
func stampRequestID(res *Result, requestID string, generated bool) {
	if res.Data == nil {
		return
	}
	switch call := res.Data["call"].(type) {
	case services.CallRecord:
		call.RequestID = requestID
		res.Data["call"] = call
	case *services.CallRecord:
		call.RequestID = requestID
	case map[string]any:
		call["request_id"] = requestID
	default:
		return
	}
	if generated {
		if res.Next == nil {
			res.Next = &Next{}
		}
		res.Next.Retry = "To retry this call without being charged twice, send the same fields with request_id=" + requestID + "; a new call needs no request_id."
	}
}

// AnonymousRequestIDMin is the shortest request_id an unsigned service.call
// takes.
const AnonymousRequestIDMin = services.AnonymousRequestIDMin

// anonymousNotAllowed is the refusal of an unsigned call to a method that
// needs a key; it names the methods that do not and, when there is one,
// what signing gets the caller.
func (s *Store) anonymousNotAllowed(ctx context.Context, tx *sql.Tx, now int64) error {
	methods := services.AnonymousMethods(services.Catalog(s.config.Features.Services))
	msg := "This service method needs an Ed25519 signed command."
	if len(methods) > 0 && s.noKey(ctx, tx, now).Available {
		msg += " Without a key you can call " + strings.Join(methods, ", ") + "; see " + ServicesCatalogueURL + "."
	}
	if offer := s.freeCredit(ctx, tx, now); offer != nil {
		msg += " " + offer.Line + " " + offer.Signing
	}
	return problem(401, "signature_required", msg)
}

// callError maps an engine refusal, in words for the caller: a caller
// without a key hears about its network's share, not an agent's.
func (s *Store) callError(ctx context.Context, tx *sql.Tx, now int64, err error, a actor) error {
	var e *allowance.Err
	if !errors.As(err, &e) {
		return serviceError(err)
	}
	switch {
	case e.Code == "anonymous_not_allowed":
		return s.anonymousNotAllowed(ctx, tx, now)
	case !a.signed && e.Code == "quota_exhausted":
		return &Error{Status: 429, Code: "quota_exhausted", Message: "This network's free credit for calls without a key is spent for today; it resets at 00:00 UTC. A signed key has its own, larger share.", RetryAfter: e.RetryAfter}
	case !a.signed && e.Code == "global_quota_exhausted" && e.RetryAfter > 0 && e.RetryAfter < untilMidnight(now):
		// The anonymous tier's day is released hour by hour (ledger
		// tier4Room): this hour's part is spent, not the day.
		return &Error{Status: 429, Code: "global_quota_exhausted", Message: "Free credit for calls without a key is used up for this hour across every network; the day's share is released hour by hour, so more is available at the top of the hour (retry_after). A signed key draws from its own tier.", RetryAfter: e.RetryAfter}
	case !a.signed && e.Code == "global_quota_exhausted":
		return &Error{Status: 429, Code: "global_quota_exhausted", Message: "Free credit for calls without a key is used up for today across every network; it resets at 00:00 UTC. A signed key draws from its own tier.", RetryAfter: e.RetryAfter}
	case !a.signed && e.Code == "notary_limit":
		return &Error{Status: 429, Code: "notary_limit", Message: fmt.Sprintf("This network made %d notary receipts without a key today, the most allowed; the count resets at 00:00 UTC.", services.NotaryPerAnonymousDay)}
	}
	return serviceError(err)
}

// callState is a call's state from its answer, for the log.
func callState(data map[string]any) string {
	if rec, ok := data["call"].(services.CallRecord); ok {
		return rec.State
	}
	return ""
}

// logUnsignedCall writes one line per unsigned service.call, so abuse can be
// reviewed daily: the caller's pseudonym (salted, changes daily), the
// service, method, channel and client product, and the outcome. Never the
// address, the arguments or the result.
func (s *Store) logUnsignedCall(ctx context.Context, c Command, a actor, state string, err error) {
	method := "?"
	if d, perr := services.ParseData(c.Data, true); perr == nil {
		method = d.Method
	}
	attrs := []any{"subject", a.account, "service", c.Target, "method", method, "via", ViaFrom(ctx), "client", clientFrom(ctx)}
	if err != nil {
		code := "error"
		var be *Error
		if errors.As(err, &be) {
			code = be.Code
		}
		slog.Info("Service call without a key refused", append(attrs, "code", code)...)
		return
	}
	slog.Info("Service call without a key", append(attrs, "state", state)...)
}

// noKey is services.list's without_key: what an agent without a key can
// call and how much, from the allowance parameters and the levers in force.
func (s *Store) noKey(ctx context.Context, q allowance.Querier, now int64) services.NoKey {
	catalog := services.Catalog(s.config.Features.Services)
	// Unsigned inference fails closed without moderation's screen,
	// screen.text without a classifier that can answer, and screen.leak
	// without the notary key, so nothing offers them.
	off := map[string]bool{"inference.complete": s.services.inference == nil || s.services.inference.Screener == nil,
		"screen.text": !services.ScreenReady(ctx, s.services.screener, s.services.notaryKey), "screen.leak": len(s.services.notaryKey) != ed25519.PrivateKeySize,
		"fetch.page": s.services.fetch == nil}
	for i := range catalog {
		for j := range catalog[i].Methods {
			if off[catalog[i].ID+"."+catalog[i].Methods[j].Name] {
				catalog[i].Methods[j].Anonymous = false
			}
		}
	}
	why := ""
	var credits, all int64
	switch {
	case s.config.Features.Ledger == LedgerOff:
		why = "the allowance ledger is off"
	default:
		p, _, err := s.ledger.led.Params(ctx, q, now)
		if err != nil {
			why = "the allowance parameters cannot be read"
			break
		}
		rp := p.Resources[allowance.Credit]
		if rp == nil {
			why = "no credit resource"
			break
		}
		credits = rp.Cap[3]
		all = rp.Budget * rp.ShareMaxPPM[3] / 1_000_000
		snap, err := s.leverSnapshot(ctx, q)
		if err != nil {
			why = "the levers cannot be read"
			break
		}
		lv := snap.levers(now)
		if lv.Tier4SharePPM >= 0 {
			all = min(all, rp.Budget*lv.Tier4SharePPM/1_000_000)
		}
		if cut, ok := lv.BudgetCutPPM[allowance.Credit]; ok {
			all -= all * max(0, min(cut, 1_000_000)) / 1_000_000
		}
		if lv.SignedServices || lv.SignedOnly || lv.ProvenOnly {
			why = "paused by a public lever; see /api/levers"
		}
	}
	return services.NoKeyFor("", catalog, credits, all, why)
}

// NoKey is what an agent without a key can call today (services.list's
// without_key), for the discovery surfaces. It reads outside any command.
func (s *Store) NoKey(ctx context.Context) services.NoKey {
	if len(s.config.Features.Services) == 0 {
		return services.NoKey{}
	}
	return s.noKey(ctx, s.db, s.now().Unix())
}

// serviceRequestKey is the call's retry key, the same one the requests table
// uses: the request ID when given, else the signed nonce.
func serviceRequestKey(c Command) string {
	if c.RequestID != "" {
		return "id:" + c.RequestID
	}
	return "nonce:" + c.Nonce
}

// serviceRetry rewrites the stored result of an exact service.call retry
// (§2.5): a call still running is 409 request_in_flight with retry_after, a
// finished Remote or Async call returns its final result, a failed one its
// failure. A Local call's stored result is already final.
func (s *Store) serviceRetry(ctx context.Context, tx *sql.Tx, a actor, stored Result, now int64) (Result, error) {
	if s.services.engine == nil || stored.Data == nil {
		return stored, nil
	}
	data, err := s.services.engine.Retry(ctx, tx, a.account, stored.Data, now)
	if err != nil {
		return Result{}, serviceError(err)
	}
	stored.Data = data
	return stored, nil
}

// serviceError maps the engine's refusals; the RFC0012 codes go through
// fromAllowance, the others keep codes the service already returns.
func serviceError(err error) error {
	var e *allowance.Err
	if !errors.As(err, &e) {
		return err
	}
	// A size refusal states the bytes sent against the limit.
	sent := ""
	if e.Limit > 0 {
		sent = " " + SizeNote(e.Sent, e.Limit, "bytes")
		switch e.Code {
		case "invalid_service_data":
			return problem(400, e.Code, "A value in the service data is too long"+sent+"; send less. Each method's limits are in services.list and /capabilities.")
		case "invalid_memory_key":
			return problem(400, e.Code, fmt.Sprintf(memoryKeyRule, MemoryKeyBytes, sent))
		case "screen_text_limit":
			return screenTextLimit(sent)
		}
	}
	// An argument-level refusal names the argument and what it takes.
	if e.Code == "invalid_service_data" && e.Message != "" {
		return problem(400, e.Code, e.Message+" Each method's args are in services.list.")
	}
	switch e.Code {
	case "signature_required":
		return problem(401, "signature_required", "Reading your own private memory, and every service.call, needs an Ed25519 signed command; public items are read by naming the agent.")
	case "call_not_found":
		return problem(404, "not_found", "No service call with that ID belongs to your agent; the call's receipt names its ID.")
	case "request_rate":
		return &Error{Status: 429, Code: "request_rate", Message: "Too many service requests (reads, or public_data dataset requests, whose limit follows your tier); wait retry_after seconds.", RetryAfter: e.RetryAfter}
	case services.RefusalAnonymousRate:
		return &Error{Status: 429, Code: "request_rate", Message: "This network has made as many calls of this method without a key as it may for now (the method's anonymous_rate, per network); wait retry_after seconds, or sign the command.", RetryAfter: e.RetryAfter}
	case services.RefusalAnonymousRateAll:
		return &Error{Status: 429, Code: "request_rate", Message: "Calls of this method without a key are at their limit for every network together right now (the method's anonymous_rate); wait retry_after seconds, or sign the command: a signed call is not counted against it.", RetryAfter: e.RetryAfter}
	case "upstream_failed":
		return problem(503, "service_unavailable", "The service call failed and nothing was charged; retry with a new request ID.")
	case "upstream_busy":
		return &Error{Status: 503, Code: "service_unavailable", Message: "The service is busy and nothing was charged; retry with a new request ID in a few seconds.", RetryAfter: 5}
	case "upstream_unavailable":
		return &Error{Status: 503, Code: "service_unavailable", Message: "No upstream for this service can be called right now (none configured, suspended, or its daily budget is spent), and nothing was charged. services.list shows what is available; budgets reset at 00:00 UTC.", RetryAfter: 60}
	case "content_refused":
		return problem(403, "content_refused", "Moderation refused this request, and nothing was charged.")
	case "anonymous_limit":
		return problem(401, "signature_required", fmt.Sprintf("Without a key, inference takes model %q, max_tokens up to %d and messages of up to %d bytes of text in all%s; sign the command for other models, longer prompts or longer answers.", services.InferenceAnonymousModel, services.InferenceAnonymousMaxTokens, services.InferenceAnonymousPromptBytes, sent))
	case "anonymous_unscreened":
		return &Error{Status: 503, Code: "service_unavailable", Message: "Inference without a key needs moderation, which is not running now, and nothing was charged; sign the command, or retry later.", RetryAfter: 60}
	case "upstream_unknown":
		return &Error{Status: 503, Code: "service_unavailable", Message: `The call's outcome is not known yet; read it with service.read {"method":"status"}, or retry with the same request ID.`, RetryAfter: 5}
	case "x402_unknown_resource":
		return problem(400, "x402_unknown_resource", `Name a resource from the x402 catalogue; service.read x402 {"schema":1,"method":"resources"} lists them. A tool is called by the tool: id a recent tools_search returned: service.read x402 {"schema":1,"method":"tools_search","args":{"query":"weather forecast for a city"}}.`)
	case "tool_unvetted":
		return problem(403, "tool_unvetted", "This tool is not vetted, and this board calls only vetted tools (vetted: true in tools_search). Nothing was paid or charged; pick a vetted hit.")
	case "tool_denied":
		return problem(403, "tool_denied", "This board does not call this tool: its host or category is on the operator's denylist. Nothing was paid or charged; pick another hit from tools_search.")
	case "tool_unavailable":
		return &Error{Status: 502, Code: "tool_unavailable", Message: "This tool is not live or not payable right now. Nothing was paid or charged; pick another hit from tools_search, or retry later with a new request ID.", RetryAfter: 300}
	case "tool_price_over_cap":
		return problem(409, "tool_price_over_cap", "The tool's live price is above this board's maximum for a tool (tools.max_price in tools_search). Nothing was paid or charged; pick a cheaper hit.")
	case "x402_unvetted":
		return problem(403, "x402_unvetted", `This resource is not callable: it is an unvetted candidate from the open catalogue (vetted: false), or it was withdrawn after payments that got no answer. Nothing was paid or charged. It becomes callable once the operator vets it; service.read x402 {"schema":1,"method":"resources"} marks what is callable.`)
	case "x402_price_changed":
		return problem(409, "x402_price_changed", "The resource asks more than its allowlisted maximum, so nothing was paid and nothing was charged. The operator reviews the allowlist.")
	case "x402_not_payable":
		return problem(502, "x402_not_payable", "The resource offered no payment SwarmMemo makes (exact USDC on the configured network to the allowlisted recipient); nothing was paid or charged.")
	case "x402_cap_reached":
		return &Error{Status: 429, Code: "x402_cap_reached", Message: `Today's x402 budget (yours or SwarmMemo's) is spent; nothing was paid or charged. Retry after 00:00 UTC; service.read x402 {"method":"resources"} shows what is left.`, RetryAfter: untilMidnight(time.Now().Unix())}
	case "x402_payment_rejected":
		return problem(502, "x402_payment_rejected", "The resource's facilitator rejected the payment and you were not charged. Retry later with a new request ID.")
	case "x402_response_too_large":
		return problem(502, "x402_response_too_large", "Nothing was paid or charged: the free response was larger than this resource's limit, or two of your paid responses today were, and further calls wait until 00:00 UTC. Ask for less (fewer results, shorter text).")
	}
	if e.Code == "doc_conflict" {
		return &Error{Status: 409, Code: "doc_conflict", Message: "Someone wrote this doc since your base_version. details.current is the doc as it stands: read it with docs.read, merge, and write again with its version as base_version. Nothing was stored or charged.", Details: e.Details}
	}
	if mapped := providerError(e.Code); mapped != nil {
		return mapped
	}
	return fromAllowance(err)
}
