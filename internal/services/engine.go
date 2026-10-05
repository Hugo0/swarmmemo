package services

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"regexp"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
)

// Meter is the part of the ledger (RFC0012 §2.5) the engine spends through;
// *ledger.Ledger implements it.
type Meter interface {
	Spend(ctx context.Context, q allowance.Querier, s allowance.Subject, r allowance.Resource, units int64, ref ledger.Ref, now int64) (ledger.Receipt, error)
	Reserve(ctx context.Context, q allowance.Querier, s allowance.Subject, r allowance.Resource, max int64, requestKey string, ref ledger.Ref, ttl int64, now int64) (ledger.Hold, error)
	Commit(ctx context.Context, q allowance.Querier, holdID string, used int64, now int64) (ledger.Receipt, error)
	Refund(ctx context.Context, q allowance.Querier, holdID string, reason string, now int64) error
}

var _ Meter = (*ledger.Ledger)(nil)

// Engine bounds. Every loop, map and stored body the engine keeps is bounded
// by one of these.
const (
	// RemoteSlots bounds Remote runs in flight at once (§2.5).
	RemoteSlots = 4
	// StoredBodyBytes bounds a Remote or Async result kept for status reads,
	// unless the provider's Descriptor.StoredBodyMax raises it, to at most
	// StoredBodyCeilingBytes. StoredBodyMaxBytes is the usual raised bound.
	StoredBodyBytes        = 16 << 10
	StoredBodyMaxBytes     = 64 << 10
	StoredBodyCeilingBytes = 128 << 10
	// PublicBytes bounds a call's public record.
	PublicBytes = 1 << 10
	// JobDataBytes bounds a job's provider state.
	JobDataBytes = 8 << 10
	// SettleAttempts bounds how often a job is tried before it is left to
	// expire (and settle as unknown).
	SettleAttempts = 3
	// WorkBatch and ReconcileBatch bound one worker pass.
	WorkBatch      = 16
	ReconcileBatch = 100
	// MaxDurationMax bounds a provider's MaxDuration.
	MaxDurationMax = 10 * time.Minute
	// holdGrace is added to MaxDuration for the hold TTL (§3.1).
	holdGrace = 30
	// settleMargin is the part of the hold TTL, in seconds, a Remote run
	// always leaves for its settlement.
	settleMargin = holdGrace / 2
	// MinWriteUnits is the least a Local write call is charged.
	MinWriteUnits = 1
	// rateEntriesMax bounds the read-rate table.
	rateEntriesMax = 1 << 14
)

// Config wires an Engine.
type Config struct {
	DB       *sql.DB   // for the second, settling transaction of Remote and Async calls
	Registry *Registry // enabled providers
	Meter    Meter
	Params   allowance.ParamsSource // prices; nil means version 0
	Now      func() int64           // the store's clock
	// ReadsPerMinute bounds service.read per subject; HoldsPerAccount and
	// HoldsTotal bound Remote and Async calls running at once.
	ReadsPerMinute, HoldsPerAccount, HoldsTotal int
}

// Engine is the service middleware: catalogue, strict parsing, pricing,
// metering, the call record, idempotency and the Remote and Async flows.
type Engine struct {
	cfg       Config
	slots     chan struct{}
	anonSlots chan struct{} // unsigned remote calls: at most AnonymousRemoteSlots of slots
	base      context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	rateMu    sync.Mutex
	rates     map[string]rateWindow
	anonMu    sync.Mutex
	anonRates map[string]*anonWindow // "service.method" (every anonymous caller) and "service.method|subject"
}

type rateWindow struct{ minute, count int64 }

// NewEngine builds an engine. Start runs its worker.
func NewEngine(cfg Config) *Engine {
	if cfg.ReadsPerMinute <= 0 {
		cfg.ReadsPerMinute = 60
	}
	if cfg.HoldsPerAccount <= 0 {
		cfg.HoldsPerAccount = 2
	}
	if cfg.HoldsTotal <= 0 {
		cfg.HoldsTotal = 64
	}
	if cfg.Now == nil {
		cfg.Now = func() int64 { return time.Now().Unix() }
	}
	base, cancel := context.WithCancel(context.Background())
	return &Engine{cfg: cfg, slots: make(chan struct{}, RemoteSlots), anonSlots: make(chan struct{}, AnonymousRemoteSlots), base: base, cancel: cancel, rates: map[string]rateWindow{}, anonRates: map[string]*anonWindow{}}
}

// Registry is the engine's provider registry.
func (e *Engine) Registry() *Registry { return e.cfg.Registry }

// Request is one service.call or service.read.
type Request struct {
	Service    string // the command's target
	Data       string // the command's data
	Subject    allowance.Subject
	RequestKey string // service.call: "id:REQUEST_ID" or "nonce:NONCE"
}

// Outcome is a service.call's answer. After is set for Remote and Async
// calls: the board runs it once the command's transaction has committed.
type Outcome struct {
	Data  map[string]any
	After func() (map[string]any, error)
}

// CallRecord is the call as the caller sees it.
type CallRecord struct {
	ID            string `json:"id"`
	State         string `json:"state"` // running, done, failed, unknown
	Mode          string `json:"mode"`
	Resource      string `json:"resource"`
	MaxCost       int64  `json:"max_cost"`
	Cost          int64  `json:"cost"`
	PricesVersion int64  `json:"prices_version"`
	CreatedAt     int64  `json:"created_at"`
	ExpiresAt     int64  `json:"expires_at,omitempty"`
	FinishedAt    int64  `json:"finished_at,omitempty"`
	DueAt         int64  `json:"due_at,omitempty"`
	Error         string `json:"error,omitempty"` // failed: why; nothing was charged
	// RequestID is an unsigned call's request_id, its retry key; the board
	// sets it on the answer (a random one when the caller gave none).
	RequestID string `json:"request_id,omitempty"`
}

func receiptData(r ledger.Receipt) map[string]any {
	return map[string]any{"resource": string(r.Resource), "used": r.Used, "refunded": r.Refunded, "hold_id": r.HoldID, "params_version": r.ParamsVersion}
}

func callData(service, method string, rec CallRecord, body json.RawMessage, receipt map[string]any) map[string]any {
	d := map[string]any{"service": service, "method": method, "call": rec}
	if len(body) > 0 {
		d["result"] = body
	}
	if receipt != nil {
		d["receipt"] = receipt
	}
	if rec.State == "running" {
		d["status"] = map[string]any{"operation": "service.read", "target": service, "data": map[string]any{"schema": 1, "method": "status", "args": map[string]any{"call": rec.ID}}}
	}
	return d
}

// prices reads the current price table in the command's transaction.
func (e *Engine) prices(ctx context.Context, q allowance.Querier, now int64) (int64, Prices, error) {
	if e.cfg.Params == nil {
		return 0, DefaultPrices(), nil
	}
	version, body, err := e.cfg.Params.Params(ctx, q, ParamsNamespace, now)
	if err != nil {
		return 0, nil, err
	}
	if len(body) == 0 {
		return version, DefaultPrices(), nil
	}
	prices, err := ParseParams(body)
	if err != nil {
		return 0, nil, refusal("service_unavailable")
	}
	return version, prices, nil
}

// Catalogue is services.list: every enabled service with its methods and
// current prices (catalog.go).
func (e *Engine) Catalogue(ctx context.Context, q allowance.Querier, now int64) (map[string]any, error) {
	version, prices, err := e.prices(ctx, q, now)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"services": e.cfg.Registry.catalog(prices, true), "prices_version": version, "call": "service.call", "read": "service.read",
		"status": map[string]any{"operation": "service.read", "data": map[string]any{"schema": 1, "method": "status", "args": map[string]any{"call": "CALL_ID"}}},
	}, nil
}

// Anonymous reports whether data names a method of service that takes an
// unsigned service.call (Method.Anonymous). It parses leniently enough to
// choose an error: anything it cannot read is not anonymous.
func (e *Engine) Anonymous(service, data string) bool {
	d, err := ParseData(data, true)
	if err != nil {
		return false
	}
	p, err := e.cfg.Registry.Lookup(service)
	if err != nil {
		return false
	}
	m, ok := p.Describe().method(d.Method)
	return ok && m.Write && m.Anonymous
}

// resolve finds the provider and method a request names.
func (e *Engine) resolve(req Request, d Data, write bool) (Provider, Descriptor, Method, error) {
	p, err := e.cfg.Registry.Lookup(req.Service)
	if err != nil {
		return nil, Descriptor{}, Method{}, err
	}
	desc := p.Describe()
	m, ok := desc.method(d.Method)
	if !ok || m.Write != write {
		return nil, Descriptor{}, Method{}, refusal("invalid_service_data")
	}
	if len(d.Args) > m.ArgsMax {
		return nil, Descriptor{}, Method{}, tooLarge("invalid_service_data", len(d.Args), m.ArgsMax)
	}
	// Only a method the catalogue marks Anonymous takes an unsigned call,
	// whether or not it is marked Signed (security review 1.21, L7).
	if write && !req.Subject.Signed && !m.Anonymous {
		return nil, Descriptor{}, Method{}, refusal("anonymous_not_allowed")
	}
	if !write && m.Signed && !req.Subject.Signed {
		return nil, Descriptor{}, Method{}, refusal("signature_required")
	}
	return p, desc, m, nil
}

// Call is service.call, in the command's transaction (§3.1 call flow).
func (e *Engine) Call(ctx context.Context, tx *sql.Tx, req Request, now int64) (_ Outcome, err error) {
	anonymous := !req.Subject.Signed
	// An unsigned call is an anonymous subject's (one network prefix), with a
	// request_id as its retry key: it has no nonce.
	if req.RequestKey == "" || anonymous && (!strings.HasPrefix(req.Subject.ID, "anon:") || !strings.HasPrefix(req.RequestKey, "id:")) {
		return Outcome{}, refusal("signature_required")
	}
	d, err := ParseData(req.Data, true)
	if err != nil {
		return Outcome{}, err
	}
	p, desc, m, err := e.resolve(req, d, true)
	if err != nil {
		return Outcome{}, err
	}
	if anonymous {
		if checker, ok := p.(AnonymousChecker); ok {
			if err = checker.CheckAnonymous(Call{Service: desc.ID, Method: m.Name, Args: d.Args, Subject: req.Subject, RequestKey: req.RequestKey, Now: now}); err != nil {
				return Outcome{}, err
			}
		}
	}
	version, prices, err := e.prices(ctx, tx, now)
	if err != nil {
		return Outcome{}, err
	}
	c := Call{Service: desc.ID, Method: m.Name, Args: d.Args, Subject: req.Subject, RequestKey: req.RequestKey, Now: now, PricesVersion: version, Price: prices.of(desc.ID, m), Prices: prices, MaxCost: d.MaxCost}
	quote, err := p.Quote(c)
	if err != nil {
		return Outcome{}, err
	}
	if quote.Resource != m.Resource || quote.Max < 0 {
		return Outcome{}, errors.New("services: provider quoted outside its method")
	}
	// A price above the caller's ceiling is refused before anything runs or
	// is spent, so a price change never surprises an agent.
	if quote.Max > d.MaxCost {
		return Outcome{}, refusal("price_exceeds_max")
	}
	c.Quoted = quote.Max
	if admitter, ok := p.(Admitter); ok {
		if err = admitter.Admit(ctx, tx, c); err != nil {
			return Outcome{}, err
		}
	}
	if anonymous {
		// The call counts in the anonymous windows only if it goes through:
		// a refusal from here on (the ledger's quota_exhausted, a full hold
		// table) gives its place back, so one network cannot fill the windows
		// every network shares with free refused calls (security review
		// 1.21, M2).
		release, aerr := e.admitAnonymous(desc.ID, m, req.Subject.ID, now)
		if aerr != nil {
			return Outcome{}, aerr
		}
		defer func() {
			if err != nil {
				release()
			}
		}()
	}
	mode := desc.Mode
	if moder, ok := p.(Moder); ok {
		mode = moder.ModeFor(c)
	}
	rec := CallRecord{ID: newCallID(), State: "done", Mode: mode.String(), Resource: string(quote.Resource), MaxCost: quote.Max, PricesVersion: version, CreatedAt: now}
	ref := ledger.Ref{Service: desc.ID, Op: "service.call", Method: m.Name}
	if mode == Local {
		res, err := p.Run(ctx, tx, c)
		if err != nil {
			return Outcome{}, err
		}
		if res.Used < 0 || res.Used > quote.Max || res.Job != nil || !json.Valid(res.Body) {
			return Outcome{}, errors.New("services: provider result outside its quote")
		}
		// A write always meets the ledger: one that would cost nothing (a
		// repeated stamp, schedule or cancel) is charged one unit, so an
		// account with nothing left cannot keep writing records for free
		// (security review 1.20, M6).
		if m.Write && res.Used == 0 && quote.Max >= MinWriteUnits {
			res.Used = MinWriteUnits
		}
		var receipt map[string]any
		if res.Used > 0 {
			r, err := e.cfg.Meter.Spend(ctx, tx, req.Subject, quote.Resource, res.Used, ref, now)
			if err != nil {
				return Outcome{}, err
			}
			receipt = receiptData(r)
		}
		rec.Cost, rec.FinishedAt = res.Used, now
		if err = e.insertCall(ctx, tx, req, c, rec, "", publicOf(res.Public), 0); err != nil {
			return Outcome{}, err
		}
		data := callData(desc.ID, m.Name, rec, res.Body, receipt)
		if len(res.Once) == 0 {
			return Outcome{Data: data}, nil
		}
		// The receipt the board stores is Data; the answer is After's.
		return Outcome{Data: data, After: func() (map[string]any, error) { return withOnce(data, res.Once), nil }}, nil
	}
	if desc.MaxDuration <= 0 || desc.MaxDuration > MaxDurationMax {
		return Outcome{}, errors.New("services: remote provider without a valid MaxDuration")
	}
	var running, total int
	if err = tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM service_calls WHERE account=? AND state='running'), (SELECT count(*) FROM service_calls WHERE state='running')", req.Subject.ID).Scan(&running, &total); err != nil {
		return Outcome{}, err
	}
	if running >= e.cfg.HoldsPerAccount || total >= e.cfg.HoldsTotal {
		return Outcome{}, &allowance.Err{Code: "hold_limit", RetryAfter: 5}
	}
	if anonymous {
		var anon int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM service_calls WHERE state='running' AND account LIKE 'anon:%'").Scan(&anon); err != nil {
			return Outcome{}, err
		}
		if anon >= AnonymousHoldsTotal {
			return Outcome{}, &allowance.Err{Code: "hold_limit", RetryAfter: 5}
		}
	}
	ttl := int64(desc.MaxDuration/time.Second) + holdGrace
	hold, err := e.cfg.Meter.Reserve(ctx, tx, req.Subject, quote.Resource, quote.Max, req.RequestKey, ref, ttl, now)
	if err != nil {
		return Outcome{}, err
	}
	rec.State, rec.ExpiresAt = "running", now+ttl
	if err = e.insertCall(ctx, tx, req, c, rec, hold.ID, "{}", rec.ExpiresAt); err != nil {
		return Outcome{}, err
	}
	pc := pendingCall{provider: p, desc: desc, call: c, rec: rec, account: req.Subject.ID, mode: mode}
	return Outcome{Data: callData(desc.ID, m.Name, rec, nil, nil), After: func() (map[string]any, error) { return e.finish(pc) }}, nil
}

func (e *Engine) insertCall(ctx context.Context, tx *sql.Tx, req Request, c Call, rec CallRecord, holdID, public string, expires int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO service_calls(id,account,service,method,request_key,hold_id,resource,cost,max_cost,mode,state,public,prices_version,created_at,expires_at,finished_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, rec.ID, req.Subject.ID, c.Service, c.Method, req.RequestKey, holdID, rec.Resource, rec.Cost, rec.MaxCost, rec.Mode, rec.State, public, rec.PricesVersion, rec.CreatedAt, expires, rec.FinishedAt)
	return err
}

func publicOf(raw json.RawMessage) string {
	if len(raw) == 0 || len(raw) > PublicBytes || !json.Valid(raw) || raw[0] != '{' {
		return "{}"
	}
	return string(raw)
}

type pendingCall struct {
	provider Provider
	desc     Descriptor
	call     Call
	rec      CallRecord
	account  string
	mode     Mode
}

// finish runs a Remote or Async call after the command committed, holding no
// transaction, then settles it in a second short transaction.
func (e *Engine) finish(pc pendingCall) (map[string]any, error) {
	if e.base.Err() != nil { // stopping: the call expires and settles as unknown
		return nil, refusal("upstream_unknown")
	}
	e.wg.Add(1)
	defer e.wg.Done()
	deadline := pc.desc.MaxDuration
	wait := time.NewTimer(deadline)
	defer wait.Stop()
	if !pc.call.Subject.Signed {
		// Unsigned calls share AnonymousRemoteSlots of the slots, so they can
		// never keep a signed call from an upstream.
		select {
		case e.anonSlots <- struct{}{}:
			defer func() { <-e.anonSlots }()
		case <-wait.C:
			return e.settle(e.base, pc.account, pc.rec.ID, "", Result{}, refusal("upstream_busy"))
		case <-e.base.Done():
			return nil, refusal("upstream_unknown")
		}
	}
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	case <-wait.C:
		return e.settle(e.base, pc.account, pc.rec.ID, "", Result{}, refusal("upstream_busy"))
	case <-e.base.Done():
		return nil, refusal("upstream_unknown")
	}
	// The run must end, and settle, before the hold expires: waiting for a
	// slot used some of it. With too little left, the call is refused as
	// busy and refunded, before any upstream is asked (security review 1.20,
	// M5); otherwise the run's deadline is cut to what is left.
	left := time.Duration(pc.rec.ExpiresAt-e.cfg.Now()-settleMargin) * time.Second
	if left < max(time.Second, pc.desc.MaxDuration/4) {
		return e.settle(e.base, pc.account, pc.rec.ID, "", Result{}, refusal("upstream_busy"))
	}
	deadline = min(deadline, left)
	ctx, cancel := context.WithTimeout(e.base, deadline)
	defer cancel()
	res, err := pc.provider.Run(ctx, nil, pc.call)
	if errors.Is(err, ErrCrash) {
		// Stand-in for a crash: nothing settles; the call stays running
		// until its hold expires, then settles at its maximum as unknown.
		return nil, refusal("upstream_unknown")
	}
	if err == nil && pc.mode == Async && res.Job != nil {
		return e.schedule(pc, *res.Job)
	}
	data, err := e.settle(e.base, pc.account, pc.rec.ID, "", res, err)
	if err != nil || len(res.Once) == 0 {
		return data, err
	}
	return withOnce(data, res.Once), nil
}

// withOnce is a finished call's data with once's keys added to its result,
// for the caller's first answer; data itself, as stored, is left alone. A
// call that did not finish with a result gets nothing.
func withOnce(data map[string]any, once json.RawMessage) map[string]any {
	body, ok := data["result"].(json.RawMessage)
	var merged, extra map[string]json.RawMessage
	if !ok || json.Unmarshal(body, &merged) != nil || json.Unmarshal(once, &extra) != nil {
		return data
	}
	maps.Copy(merged, extra)
	raw, err := json.Marshal(merged)
	if err != nil {
		return data
	}
	out := maps.Clone(data)
	out["result"] = json.RawMessage(raw)
	return out
}

// schedule records an Async call's job; the worker settles it when due.
func (e *Engine) schedule(pc pendingCall, job Job) (map[string]any, error) {
	now := e.cfg.Now()
	if job.DueAt < now {
		job.DueAt = now
	}
	if len(job.Data) > JobDataBytes || (len(job.Data) > 0 && !json.Valid(job.Data)) || job.DueAt >= pc.rec.ExpiresAt {
		return e.settle(e.base, pc.account, pc.rec.ID, "", Result{}, refusal("upstream_failed"))
	}
	if job.ID == "" {
		job.ID = newCallID()
	}
	tx, err := e.cfg.DB.BeginTx(e.base, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(e.base, "INSERT INTO service_jobs(id,call_id,service,account,data,due_at,state,created_at) VALUES(?,?,?,?,?,?,'scheduled',?)", job.ID, pc.rec.ID, pc.desc.ID, pc.account, string(job.Data), job.DueAt, now); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	rec := pc.rec
	rec.DueAt = job.DueAt
	return callData(pc.desc.ID, pc.call.Method, rec, nil, nil), nil
}

// settle commits a finished call's cost (or refunds it) and records the
// outcome, in one short transaction. A call that is no longer running (the
// reconciler marked it unknown) is only reported.
func (e *Engine) settle(ctx context.Context, account, callID, jobID string, res Result, runErr error) (map[string]any, error) {
	now := e.cfg.Now()
	tx, err := e.cfg.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	row, err := loadCall(ctx, tx, account, callID)
	if err != nil {
		return nil, err
	}
	if row.rec.State != "running" {
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return row.view()
	}
	if runErr == nil && (res.Used < 0 || res.Used > row.rec.MaxCost || len(res.Body) > e.storedBodyMax(row.service) || !json.Valid(res.Body)) {
		runErr = refusal("upstream_failed")
	}
	jobState := "done"
	if runErr != nil {
		code := "upstream_failed"
		var ae *allowance.Err
		if errors.As(runErr, &ae) && ae.Code != "upstream_unknown" {
			code = ae.Code
		}
		state := "failed"
		if err = e.cfg.Meter.Refund(ctx, tx, row.holdID, code, now); err != nil {
			if !errors.As(err, &ae) {
				return nil, err
			}
			// The hold was already settled (expired): the call's outcome is unknown.
			state = "unknown"
		}
		jobState = "failed"
		if _, err = tx.ExecContext(ctx, "UPDATE service_calls SET state=?, cost=CASE WHEN ?='unknown' THEN max_cost ELSE 0 END, error=?, finished_at=? WHERE id=? AND state='running'", state, state, code, now, callID); err != nil {
			return nil, err
		}
	} else {
		receipt, err := e.cfg.Meter.Commit(ctx, tx, row.holdID, res.Used, now)
		var ae *allowance.Err
		switch {
		case errors.As(err, &ae):
			_, err = tx.ExecContext(ctx, "UPDATE service_calls SET state='unknown', cost=max_cost, finished_at=? WHERE id=? AND state='running'", now, callID)
		case err == nil:
			_ = receipt
			_, err = tx.ExecContext(ctx, "UPDATE service_calls SET state='done', cost=?, body=?, public=?, finished_at=? WHERE id=? AND state='running'", res.Used, string(res.Body), publicOf(res.Public), now, callID)
		}
		if err != nil {
			return nil, err
		}
	}
	if jobID != "" {
		if _, err = tx.ExecContext(ctx, "UPDATE service_jobs SET state=?, leased_until=0 WHERE id=?", jobState, jobID); err != nil {
			return nil, err
		}
	}
	if row, err = loadCall(ctx, tx, account, callID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return row.view()
}

// storedBodyMax is the largest result the service's call record keeps.
func (e *Engine) storedBodyMax(service string) int {
	p, ok := e.cfg.Registry.providers[service]
	if !ok {
		return StoredBodyBytes
	}
	if n := p.Describe().StoredBodyMax; n > 0 {
		return min(n, StoredBodyCeilingBytes)
	}
	return StoredBodyBytes
}

type callRow struct {
	rec             CallRecord
	service, method string
	holdID, body    string
	errCode         string
}

func loadCall(ctx context.Context, q allowance.Querier, account, id string) (callRow, error) {
	var r callRow
	err := q.QueryRowContext(ctx, `SELECT c.id,c.state,c.mode,c.resource,c.max_cost,c.cost,c.prices_version,c.created_at,c.expires_at,c.finished_at,
 c.service,c.method,c.hold_id,c.body,c.error,COALESCE((SELECT max(due_at) FROM service_jobs j WHERE j.call_id=c.id),0)
 FROM service_calls c WHERE c.id=? AND c.account=?`, id, account).Scan(&r.rec.ID, &r.rec.State, &r.rec.Mode, &r.rec.Resource, &r.rec.MaxCost, &r.rec.Cost, &r.rec.PricesVersion, &r.rec.CreatedAt, &r.rec.ExpiresAt, &r.rec.FinishedAt,
		&r.service, &r.method, &r.holdID, &r.body, &r.errCode, &r.rec.DueAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, refusal("call_not_found")
	}
	return r, err
}

// data is the call's record and, once done, its result.
func (r callRow) data() map[string]any {
	var body json.RawMessage
	if r.body != "" {
		body = json.RawMessage(r.body)
	}
	if r.rec.State == "failed" {
		r.rec.Error = failureCode(r.errCode)
	}
	return callData(r.service, r.method, r.rec, body, nil)
}

// view is what the caller of the call (or its exact retry) is told: its data,
// or for a failed call the refusal it failed with.
func (r callRow) view() (map[string]any, error) {
	if r.rec.State == "failed" {
		return nil, refusal(failureCode(r.errCode))
	}
	return r.data(), nil
}

// failureCode keeps a provider's refusal only when the board knows it.
func failureCode(code string) string {
	switch code {
	case "upstream_busy", "upstream_unavailable", "content_refused", "invalid_service_data", "memory_not_found", "memory_limit", "invalid_memory_key",
		"service_unavailable", "x402_unknown_resource", "x402_price_changed", "x402_not_payable", "x402_cap_reached",
		"x402_payment_rejected", "x402_response_too_large", "x402_unvetted", "anonymous_unscreened",
		"price_exceeds_max", "frames_unvetted", "frames_denied", "frames_unavailable", "frames_price_over_cap":
		return code
	}
	return "upstream_failed"
}

// Retry answers an exact retry of a service.call whose result the board
// stored (§2.5 idempotency): a finished call returns its stored or final
// result; a call still running is 409 request_in_flight with retry_after.
// account is the one the call was made by: for an anonymous retry across
// midnight, yesterday's pseudonym, under which its receipt was found.
func (e *Engine) Retry(ctx context.Context, q allowance.Querier, account string, stored map[string]any, now int64) (map[string]any, error) {
	call, _ := stored["call"].(map[string]any)
	state, _ := call["state"].(string)
	id, _ := call["id"].(string)
	if state != "running" || id == "" {
		return stored, nil
	}
	row, err := loadCall(ctx, q, account, id)
	if err != nil {
		return nil, err
	}
	if row.rec.State == "running" {
		if now < row.rec.ExpiresAt {
			wait := row.rec.ExpiresAt - now
			if wait > 60 {
				wait = 60
			}
			return nil, &allowance.Err{Code: "request_in_flight", RetryAfter: int(wait)}
		}
		row.rec.State, row.rec.Cost = "unknown", row.rec.MaxCost
	}
	return row.view()
}

var callIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Read is service.read for a caller that holds no transaction the read
// could block (tests, tools): ReadOutcome, with its after-commit part run at
// once. The board uses ReadOutcome.
func (e *Engine) Read(ctx context.Context, q allowance.Querier, req Request, now int64) (map[string]any, error) {
	out, err := e.ReadOutcome(ctx, q, req, now)
	if err != nil || out.After == nil {
		return out.Data, err
	}
	return out.After()
}

// remoteReadTimeout bounds a read's after-commit part (RemoteReader).
const remoteReadTimeout = 30 * time.Second

// ReadOutcome is service.read, in the command's transaction: a provider's
// free read method, or the generic "status" of one of the caller's own
// calls. A read that needs the network (RemoteReader) answers with After,
// which the board runs once the transaction has committed.
func (e *Engine) ReadOutcome(ctx context.Context, q allowance.Querier, req Request, now int64) (Outcome, error) {
	d, err := ParseData(req.Data, false)
	if err != nil {
		return Outcome{}, err
	}
	if d.Method == "status" {
		if _, err = e.cfg.Registry.Lookup(req.Service); err != nil {
			return Outcome{}, err
		}
		if err = e.allowRead(req.Subject.ID, now); err != nil {
			return Outcome{}, err
		}
		data, err := e.status(ctx, q, req, d, now)
		return Outcome{Data: data}, err
	}
	p, desc, m, err := e.resolve(req, d, false)
	if err != nil {
		return Outcome{}, err
	}
	reader, ok := p.(Reader)
	if !ok {
		return Outcome{}, refusal("invalid_service_data")
	}
	if err = e.allowRead(req.Subject.ID, now); err != nil {
		return Outcome{}, err
	}
	version, prices, err := e.prices(ctx, q, now)
	if err != nil {
		return Outcome{}, err
	}
	c := Call{Service: desc.ID, Method: m.Name, Args: d.Args, Subject: req.Subject, Now: now, PricesVersion: version, Prices: prices}
	if rr, ok := p.(RemoteReader); ok {
		after, err := rr.ReadRemote(ctx, q, c)
		if err != nil {
			return Outcome{}, err
		}
		if after != nil {
			return Outcome{After: func() (map[string]any, error) {
				ctx, cancel := context.WithTimeout(e.base, remoteReadTimeout)
				defer cancel()
				body, err := after(ctx)
				if err != nil {
					return nil, err
				}
				return map[string]any{"service": desc.ID, "method": m.Name, "result": body}, nil
			}}, nil
		}
	}
	body, err := reader.Read(ctx, q, c)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Data: map[string]any{"service": desc.ID, "method": m.Name, "result": body}}, nil
}

func (e *Engine) status(ctx context.Context, q allowance.Querier, req Request, d Data, now int64) (map[string]any, error) {
	if !req.Subject.Signed {
		return nil, refusal("signature_required")
	}
	var args struct {
		Call string `json:"call"`
	}
	if err := StrictObject(d.Args, &args); err != nil || !callIDRE.MatchString(args.Call) {
		return nil, refusal("invalid_service_data")
	}
	row, err := loadCall(ctx, q, req.Subject.ID, args.Call)
	if err != nil {
		return nil, err
	}
	if row.service != req.Service {
		return nil, refusal("call_not_found")
	}
	if row.rec.State == "running" && now >= row.rec.ExpiresAt {
		row.rec.State, row.rec.Cost = "unknown", row.rec.MaxCost
	}
	return row.data(), nil
}

// allowRead is the per-subject read budget (a fixed one-minute window).
func (e *Engine) allowRead(subject string, now int64) error {
	minute := now / 60
	e.rateMu.Lock()
	defer e.rateMu.Unlock()
	w := e.rates[subject]
	if w.minute != minute {
		w = rateWindow{minute: minute}
	}
	if w.count >= int64(e.cfg.ReadsPerMinute) {
		return &allowance.Err{Code: "request_rate", RetryAfter: int(60 - now%60)}
	}
	w.count++
	if _, ok := e.rates[subject]; !ok && len(e.rates) >= rateEntriesMax {
		for k, v := range e.rates {
			if v.minute != minute {
				delete(e.rates, k)
			}
		}
		if len(e.rates) >= rateEntriesMax {
			clear(e.rates)
		}
	}
	e.rates[subject] = w
	return nil
}

// Start runs the worker: due Async jobs settle, and calls whose hold expired
// (a crash) are marked unknown. Stop ends it and waits for in-flight runs.
func (e *Engine) Start(ctx context.Context, every time.Duration) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-e.base.Done():
				return
			case <-t.C:
				_, _ = e.Work(e.base)
			}
		}
	}()
}

func (e *Engine) Stop() {
	e.cancel()
	e.wg.Wait()
}

// Work is one bounded worker pass; it returns how many jobs and calls it
// settled or reconciled.
func (e *Engine) Work(ctx context.Context) (int, error) {
	now := e.cfg.Now()
	rows, err := e.cfg.DB.QueryContext(ctx, "SELECT id FROM service_jobs WHERE (state='scheduled' AND due_at<=?) OR (state='running' AND leased_until<?) ORDER BY due_at, id LIMIT ?", now, now, WorkBatch)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		done, err := e.workJob(ctx, id, now)
		if err != nil {
			return n, err
		}
		if done {
			n++
		}
	}
	r, err := e.reconcile(ctx, now)
	if err != nil {
		return n + r, err
	}
	n += r
	// Providers with a clock of their own (Worker), each bounded by itself.
	for _, id := range e.cfg.Registry.Enabled() {
		p, _ := e.cfg.Registry.Lookup(id)
		if w, ok := p.(Worker); ok {
			k, err := w.Work(ctx, e.cfg.DB, e.cfg.Now())
			n += k
			if err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

// Notices is what the enabled providers add to one agent's updates.get, by
// data key, in the read's transaction; nil when none adds anything.
func (e *Engine) Notices(ctx context.Context, q allowance.Querier, nq NoticeQuery) (map[string]any, error) {
	var out map[string]any
	for _, id := range e.cfg.Registry.Enabled() {
		p, _ := e.cfg.Registry.Lookup(id)
		n, ok := p.(Noticer)
		if !ok {
			continue
		}
		key, value, err := n.Notices(ctx, q, nq)
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = map[string]any{}
		}
		out[key] = value
	}
	return out, nil
}

func (e *Engine) workJob(ctx context.Context, jobID string, now int64) (bool, error) {
	tx, err := e.cfg.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var callID, service, account, data, state string
	var dueAt, attempts, leased, expires int64
	err = tx.QueryRowContext(ctx, `SELECT j.call_id,j.service,j.account,j.data,j.state,j.due_at,j.attempts,j.leased_until,c.expires_at
 FROM service_jobs j JOIN service_calls c ON c.id=j.call_id WHERE j.id=? AND c.state='running'`, jobID).Scan(&callID, &service, &account, &data, &state, &dueAt, &attempts, &leased, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, "UPDATE service_jobs SET state='failed' WHERE id=? AND state IN ('scheduled','running')", jobID)
		if err == nil {
			err = tx.Commit()
		}
		return false, err
	}
	if err != nil {
		return false, err
	}
	due := (state == "scheduled" && dueAt <= now) || (state == "running" && leased < now)
	if !due {
		return false, tx.Commit()
	}
	p, lookupErr := e.cfg.Registry.Lookup(service)
	settler, ok := p.(Settler)
	if lookupErr != nil || !ok || attempts >= SettleAttempts || now >= expires {
		// Left to expire: the reconciler marks the call unknown and the
		// sweeper settles its hold at the maximum.
		_, err = tx.ExecContext(ctx, "UPDATE service_jobs SET state='failed', leased_until=0 WHERE id=?", jobID)
		if err == nil {
			err = tx.Commit()
		}
		return false, err
	}
	deadline := p.Describe().MaxDuration
	if _, err = tx.ExecContext(ctx, "UPDATE service_jobs SET state='running', attempts=attempts+1, leased_until=? WHERE id=?", now+int64(deadline/time.Second)+5, jobID); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	runCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	res, err := settler.Settle(runCtx, Job{ID: jobID, DueAt: dueAt, Data: json.RawMessage(data)})
	if errors.Is(err, ErrCrash) {
		return false, nil // the lease lapses and a later pass tries again
	}
	if _, err = e.settle(ctx, account, callID, jobID, res, err); err != nil {
		var ae *allowance.Err
		if !errors.As(err, &ae) {
			return false, err
		}
	}
	return true, nil
}

// reconcile marks running calls whose hold has expired as unknown, charged
// at their maximum: the sweeper settles those holds the same way (§2.5).
func (e *Engine) reconcile(ctx context.Context, now int64) (int, error) {
	tx, err := e.cfg.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT id FROM service_calls WHERE state='running' AND expires_at<=? ORDER BY expires_at LIMIT ?", now, ReconcileBatch)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, "UPDATE service_calls SET state='unknown', cost=max_cost, finished_at=? WHERE id=? AND state='running'", now, id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE service_jobs SET state='failed', leased_until=0 WHERE call_id=? AND state IN ('scheduled','running')", id); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit()
}

func newCallID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// anonWindow counts unsigned calls in the current minute and UTC day.
type anonWindow struct{ minute, mcount, day, dcount int64 }

// roll starts new windows when the minute or the day changed.
func (w *anonWindow) roll(now int64) {
	if w.minute != now/60 {
		w.minute, w.mcount = now/60, 0
	}
	if w.day != now/86400 {
		w.day, w.dcount = now/86400, 0
	}
}

// room reports whether one more call fits perMinute and perDay (a zero bound
// is not enforced); retry is the seconds until the binding window resets.
func (w *anonWindow) room(perMinute, perDay, now int64) (bool, int) {
	w.roll(now)
	switch {
	case perDay > 0 && w.dcount >= perDay:
		return false, int(86400 - now%86400)
	case perMinute > 0 && w.mcount >= perMinute:
		return false, int(60 - now%60)
	}
	return true, 0
}

// Refusals of admitAnonymous: the caller's own network is past its bound
// (AnonRate.CallerPerMinute, CallerPerDay), or every network together is
// (AllPerMinute, AllPerDay). The board answers both 429 request_rate, each
// in its own words.
const (
	RefusalAnonymousRate    = "anonymous_rate"
	RefusalAnonymousRateAll = "anonymous_rate_all"
)

// admitAnonymous applies m's AnonymousRate to an unsigned call: first the
// caller's own windows, then the windows every anonymous caller shares, so a
// caller past its own bound never touches the shared ones. A call counts
// only when both have room, and release gives its place back when the call
// is refused after admission (Call does that). The table is bounded; when it
// is full of live callers it fails closed.
func (e *Engine) admitAnonymous(service string, m Method, subject string, now int64) (release func(), err error) {
	r := m.AnonymousRate
	key := service + "." + m.Name
	e.anonMu.Lock()
	defer e.anonMu.Unlock()
	window := func(k string) (*anonWindow, error) {
		if w := e.anonRates[k]; w != nil {
			return w, nil
		}
		if len(e.anonRates) >= rateEntriesMax {
			for k, w := range e.anonRates {
				if w.day != now/86400 {
					delete(e.anonRates, k)
				}
			}
			if len(e.anonRates) >= rateEntriesMax {
				return nil, &allowance.Err{Code: RefusalAnonymousRateAll, RetryAfter: 60}
			}
		}
		w := &anonWindow{}
		e.anonRates[k] = w
		return w, nil
	}
	var counted []*anonWindow
	if r.CallerPerMinute > 0 || r.CallerPerDay > 0 {
		mine, err := window(key + "|" + subject)
		if err != nil {
			return nil, err
		}
		if ok, retry := mine.room(r.CallerPerMinute, r.CallerPerDay, now); !ok {
			return nil, &allowance.Err{Code: RefusalAnonymousRate, RetryAfter: retry}
		}
		counted = append(counted, mine)
	}
	all, err := window(key)
	if err != nil {
		return nil, err
	}
	if ok, retry := all.room(r.AllPerMinute, r.AllPerDay, now); !ok {
		return nil, &allowance.Err{Code: RefusalAnonymousRateAll, RetryAfter: retry}
	}
	counted = append(counted, all)
	for _, w := range counted {
		w.mcount++
		w.dcount++
	}
	minute, day := now/60, now/86400
	return func() {
		e.anonMu.Lock()
		defer e.anonMu.Unlock()
		for _, w := range counted {
			// A window that rolled over since then started without this call.
			if w.minute == minute && w.mcount > 0 {
				w.mcount--
			}
			if w.day == day && w.dcount > 0 {
				w.dcount--
			}
		}
	}, nil
}
