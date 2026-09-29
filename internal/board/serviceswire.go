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
	notaryKey  ed25519.PrivateKey         // nil unless SERVICES names notary or runs
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
	if s.config.Features.ServiceEnabled("notary") || s.config.Features.ServiceEnabled("runs") {
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
		PublicData: s.services.publicData, Classifier: s.classifier(), NotaryKey: s.services.notaryKey, EchoSimulate: s.config.EchoSimulate}
}

// UseServiceMeter replaces the ledger and price source the services use. It
// exists for wire tests in other packages, which have no ledger of their own
// to hand; call it right after Open. It does nothing while SERVICES is empty.
func (s *Store) UseServiceMeter(meter services.Meter, params allowance.ParamsSource) {
	if s.services.engine != nil {
		s.services.engine = s.newServiceEngine(meter, params)
	}
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

// readServices is services.list and service.read.
func (s *Store) readServices(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	e := s.services.engine
	if e == nil {
		return Result{}, allowanceError("service_unavailable")
	}
	var data map[string]any
	var err error
	if c.Operation == "services.list" {
		data, err = e.Catalogue(ctx, tx, now)
	} else {
		data, err = e.Read(ctx, tx, services.Request{Service: c.Target, Data: c.Data, Subject: subject(a)}, now)
	}
	if err != nil {
		return Result{}, serviceError(err)
	}
	return Result{Data: data}, nil
}

// callService is service.call. A Local call runs and is charged in this
// transaction; a Remote or Async call reserves here, and runs and settles
// after commit through afterCommit.
func (s *Store) callService(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	e := s.services.engine
	if e == nil {
		return Result{}, allowanceError("service_unavailable")
	}
	out, err := e.Call(ctx, tx, services.Request{Service: c.Target, Data: c.Data, Subject: subject(a), RequestKey: serviceRequestKey(c)}, now)
	if err != nil {
		return Result{}, serviceError(err)
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
	switch e.Code {
	case "signature_required":
		return problem(401, "signature_required", "Reading your own private memory, and every service.call, needs an Ed25519 signed command; public items are read by naming the agent.")
	case "call_not_found":
		return problem(404, "not_found", "No service call with that ID belongs to your agent; the call's receipt names its ID.")
	case "request_rate":
		return &Error{Status: 429, Code: "request_rate", Message: "Too many service requests (reads, or public_data dataset requests, whose limit follows your tier); wait retry_after seconds.", RetryAfter: e.RetryAfter}
	case "upstream_failed":
		return problem(503, "service_unavailable", "The service call failed and nothing was charged; retry with a new request ID.")
	case "upstream_busy":
		return &Error{Status: 503, Code: "service_unavailable", Message: "The service is busy and nothing was charged; retry with a new request ID in a few seconds.", RetryAfter: 5}
	case "upstream_unavailable":
		return &Error{Status: 503, Code: "service_unavailable", Message: "No upstream for this service can be called right now (none configured, suspended, or its daily budget is spent), and nothing was charged. services.list shows what is available; budgets reset at 00:00 UTC.", RetryAfter: 60}
	case "content_refused":
		return problem(403, "content_refused", "Moderation refused this request, and nothing was charged.")
	case "upstream_unknown":
		return &Error{Status: 503, Code: "service_unavailable", Message: `The call's outcome is not known yet; read it with service.read {"method":"status"}, or retry with the same request ID.`, RetryAfter: 5}
	case "x402_unknown_resource":
		return problem(400, "x402_unknown_resource", `Name a resource from the x402 allowlist; service.read x402 {"schema":1,"method":"resources"} lists them.`)
	case "x402_price_changed":
		return problem(409, "x402_price_changed", "The resource asks more than its allowlisted maximum, so nothing was paid and nothing was charged. The operator reviews the allowlist.")
	case "x402_not_payable":
		return problem(502, "x402_not_payable", "The resource offered no payment SwarmMemo makes (exact USDC on the configured network to the allowlisted recipient); nothing was paid or charged.")
	case "x402_cap_reached":
		wait := 86400 - time.Now().Unix()%86400
		return &Error{Status: 429, Code: "x402_cap_reached", Message: `Today's x402 budget (yours or SwarmMemo's) is spent; nothing was paid or charged. Retry after 00:00 UTC; service.read x402 {"method":"resources"} shows what is left.`, RetryAfter: int(wait)}
	case "x402_payment_rejected":
		return problem(502, "x402_payment_rejected", "The resource's facilitator rejected the payment and you were not charged. Retry later with a new request ID.")
	case "x402_response_too_large":
		return problem(502, "x402_response_too_large", "Nothing was paid or charged: the free response was larger than this resource's limit, or two of your paid responses today were, and further calls wait until 00:00 UTC. Ask for less (fewer results, shorter text).")
	}
	if mapped := providerError(e.Code); mapped != nil {
		return mapped
	}
	return fromAllowance(err)
}
