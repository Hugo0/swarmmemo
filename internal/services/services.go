// Package services is the provider interface, the catalogue and the metering
// middleware behind service.call and service.read (RFC0012 §3).
//
// A provider is one file: a type that implements Provider (and Settler when it
// is asynchronous), plus one line in builtins (registry.go). Nothing else
// changes: the catalogue, prices, the strict data parser, metering, the call
// record, idempotency and the Remote and Async flows are the Engine's
// (engine.go), and a provider's tables join Schema through its own Schema
// method.
//
// Memory, wakeup and notary are local, screen asks moderation's classifier
// (Deps.TextScreener), and echo's Remote and Async paths only simulate an
// upstream. Four providers make outbound requests:
// inference, only to the base URLs its operator configured; public_data,
// only to the fixed hosts of its compiled-in catalogue; runs, only to the
// loader URL its operator configured, all three through internal/safenet
// (inference.go, publicdata.go, runs.go, runsloader.go); and x402, only to
// catalogued resources (the operator's allowlist, and the open catalogue
// imported from its configured discovery URLs under fixed guardrails) and
// its bundlers' fixed endpoints, through Deps.Dial (x402.go,
// x402_catalogue.go, bundler.go).
package services

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"time"

	"swarmmemo/internal/allowance"
)

// Schema is migration fragment C (RFC0012 §7), applied after ledger.Schema:
// the call record and job tables, then each built-in provider's own tables.
// Every statement only creates a table or an index if it does not exist.
var Schema = buildSchema()

const coreSchema = `
CREATE TABLE IF NOT EXISTS service_calls (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, service TEXT NOT NULL, method TEXT NOT NULL,
 request_key TEXT NOT NULL, hold_id TEXT NOT NULL, resource TEXT NOT NULL, cost INTEGER NOT NULL DEFAULT 0,
 max_cost INTEGER NOT NULL DEFAULT 0, mode TEXT NOT NULL DEFAULT 'local' CHECK(mode IN ('local','remote','async')),
 state TEXT NOT NULL CHECK(state IN ('running','done','failed','unknown')), public TEXT NOT NULL DEFAULT '{}',
 body TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
 prices_version INTEGER NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL DEFAULT 0,
 finished_at INTEGER NOT NULL DEFAULT 0, UNIQUE(account,request_key));
CREATE INDEX IF NOT EXISTS service_calls_account ON service_calls(account,created_at);
CREATE INDEX IF NOT EXISTS service_calls_running ON service_calls(expires_at) WHERE state='running';
CREATE TABLE IF NOT EXISTS service_jobs (
 id TEXT PRIMARY KEY, call_id TEXT NOT NULL REFERENCES service_calls(id), service TEXT NOT NULL,
 account TEXT NOT NULL, data TEXT NOT NULL DEFAULT '',
 due_at INTEGER NOT NULL, state TEXT NOT NULL CHECK(state IN ('scheduled','running','done','failed','cancelled')),
 attempts INTEGER NOT NULL DEFAULT 0, leased_until INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS service_jobs_due ON service_jobs(due_at) WHERE state='scheduled';
CREATE INDEX IF NOT EXISTS service_jobs_leased ON service_jobs(leased_until) WHERE state='running';
`

// Mode is where a call runs. Local: inside the command's transaction, no I/O.
// Remote: after commit, under a deadline. Async: settles later (wake-ups, runs).
type Mode int

const (
	Local Mode = iota
	Remote
	Async
)

func (m Mode) String() string {
	switch m {
	case Remote:
		return "remote"
	case Async:
		return "async"
	}
	return "local"
}

type Method struct {
	Name     string
	Write    bool // service.call (true) or service.read (false)
	Signed   bool
	Resource allowance.Resource // what a call is priced in; "" for free reads
	ArgsMax  int                // bytes of args JSON
	// Price is the compiled-in price (parameter version 0) of a write; the
	// "services" parameter namespace may replace it (params.go).
	Price Price
	// The catalogue (catalog.go): a one-line description, the arguments,
	// valid example args, a note when the price depends on more than the
	// arguments, and the max_cost the examples send in that case.
	Line           string
	Args           []Arg
	Example        json.RawMessage
	PriceNote      string
	ExampleMaxCost int64
	// Anonymous marks a write anyone may call without a key (anonymous.go):
	// billed in credit to the caller's network, within that network's free
	// daily share. AnonymousLabel names the service in the one "no key
	// needed" line ("public data"), AnonymousNote says what an unsigned call
	// may not do that a signed one may, and AnonymousRate bounds unsigned
	// calls per network and for every anonymous caller together.
	Anonymous      bool
	AnonymousLabel string
	AnonymousNote  string
	AnonymousRate  AnonRate
}

type Descriptor struct {
	ID      string // "memory"
	Summary string
	// Title names the service's section of docs/PROTOCOL.md ("Wake-ups"),
	// Line says what it gives an agent in one sentence, Topic groups it in
	// "What SwarmMemo gives agents" (empty for a test service), and Limits
	// are the bounds it enforces (catalog.go).
	Title, Line, Topic string
	Limits             []Limit
	Mode               Mode
	Methods            []Method
	MaxDuration        time.Duration // Remote/Async: bound on one Run; the hold TTL is this + 30 s
	// StoredBodyMax bounds a Remote or Async result kept in the call record;
	// 0 means StoredBodyBytes, and it is never more than StoredBodyCeilingBytes.
	StoredBodyMax int
}

type Call struct {
	Service, Method string
	Args            json.RawMessage // strict JSON, at most Method.ArgsMax bytes
	Subject         allowance.Subject
	RequestKey      string
	Now             int64
	PricesVersion   int64
	// Price is this method's price under PricesVersion.
	Price Price
	// Prices is the whole price table under PricesVersion (a reader that
	// quotes its provider's write methods uses it).
	Prices Prices
	// Quoted is the call's quote (Quote.Max), set once quoted: what Run may
	// use at most, so a provider whose inputs can change between the quote
	// and the run (x402's catalogue) refuses rather than exceed it. Zero
	// before the quote.
	Quoted int64
}

type Quote struct {
	Resource allowance.Resource
	Max      int64 // the most this call can cost
}

type Result struct {
	Body   json.RawMessage // returned to the caller
	Used   int64           // <= Quote.Max; the rest is refunded
	Public json.RawMessage // what the public call record shows: sizes and hashes, never private content
	Job    *Job            // Async: the hold stays open until the job settles
}

// Job is an Async call waiting to settle. Data is the provider's own state
// for Settle (at most JobDataBytes), stored with the job.
type Job struct {
	ID    string
	DueAt int64
	Data  json.RawMessage
}

type Provider interface {
	Describe() Descriptor
	Quote(c Call) (Quote, error)                                 // pure, bounded, no I/O
	Run(ctx context.Context, tx *sql.Tx, c Call) (Result, error) // tx != nil only for Local
}

type Settler interface { // Async providers
	Settle(ctx context.Context, job Job) (Result, error) // outside any transaction
}

// Reader serves a provider's service.read methods. A read is free, runs in the
// command's transaction and is never metered or recorded.
type Reader interface {
	Read(ctx context.Context, q allowance.Querier, c Call) (json.RawMessage, error)
}

// Moder lets a provider choose a call's mode from its arguments (echo's
// simulate); without it every call runs in the descriptor's mode.
type Moder interface {
	ModeFor(c Call) Mode
}

// Admitter is consulted in the command's transaction after the quote and
// before anything is reserved: a per-caller admission check such as a rate
// limit. It must be cheap, bounded and do no I/O but through q; a refusal
// (request_rate with retry_after) leaves nothing reserved or recorded.
type Admitter interface {
	Admit(ctx context.Context, q allowance.Querier, c Call) error
}

// Cataloguer adds provider-specific fields (availability, models and their
// prices) to the provider's services.list entry. It is read on every list,
// so it must be cheap and bounded.
type Cataloguer interface {
	CatalogueExtra() map[string]any
}

// Schemer is a provider with tables of its own; its statements join Schema.
type Schemer interface {
	Schema() string
}

// Deps is what the board lends a provider when it is built. Known and
// DefaultPrices build providers with the zero Deps, so a provider must accept
// nil fields.
type Deps struct {
	// Accounts resolves an agent fingerprint to its continuity account.
	Accounts AccountResolver
	// DB is the store's database, for a Remote provider's own bookkeeping
	// outside the command's transaction (inference's daily upstream spend,
	// x402's payment caps, the run log, caps and reviews).
	DB *sql.DB
	// Dial is the SSRF-safe dialer (the webhook dialer: public addresses
	// only, checked after resolution). nil means no outbound network.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// X402 is the loaded x402 configuration; nil leaves x402 unconfigured.
	X402 *X402Config
	// Runs is the parsed RUNS_CONFIG; nil leaves runs unconfigured, and every
	// call is refused as unavailable.
	Runs *RunsConfig
	// Inference is the parsed INFERENCE_CONFIG; nil leaves inference
	// unconfigured, so it lists as unavailable and every call fails closed.
	Inference *InferenceConfig
	// Board is the board's read view for a provider that watches it (wakeup).
	Board BoardView
	// ServiceID is the service identity signatures bind (the board's
	// SERVICE_ID); the notary signs it into every receipt.
	ServiceID string
	// NotaryKey is the notary's Ed25519 key (LoadOrCreateNotaryKey from
	// NOTARY_KEY_FILE), which signs notary and run receipts; nil leaves both
	// services unavailable.
	NotaryKey ed25519.PrivateKey
	// TextScreener is screen's classifier (moderation's Jev, while MODERATION
	// is on); nil leaves screen unavailable, and every call fails closed.
	TextScreener TextScreener
	// PublicData is public_data's configuration (its key directory); nil
	// leaves every keyed dataset unavailable.
	PublicData *PublicDataConfig
	// Classifier gives a caller's standing (its tier), for limits that
	// follow the tier (public_data). nil, or an error, means the ordinary
	// signed tier.
	Classifier allowance.Classifier
	// EchoSimulate lets echo's args.simulate stand in for an upstream (remote,
	// async, delay, failure, crash). Tests only: in production a crashed
	// simulation holds one of the board's shared open-hold slots until it
	// expires, so it stays off (security review 1.20, M4).
	EchoSimulate bool
}

// BoardEvent is one visible, original board message as a watching provider
// sees it: where it is and whom it concerns, never its text.
type BoardEvent struct {
	Seq           int64
	ID, Room      string
	Author        string   // the author's continuity account; "" when anonymous
	ReplyToAuthor string   // the account whose message this replies to
	Addressed     string   // the account the message is addressed to
	Mentions      []string // accounts named in the text by @handle, at most MentionsMax
}

// MentionsMax bounds BoardEvent.Mentions.
const MentionsMax = 8

// BoardView is the board as a watching provider reads it, in the caller's
// transaction. Every method is one bounded, indexed read.
type BoardView interface {
	// LatestSeq is the sequence of the newest message.
	LatestSeq(ctx context.Context, q allowance.Querier) (int64, error)
	// EventsAfter is at most limit visible original messages (no edits, none
	// hidden) with a sequence above after, oldest first.
	EventsAfter(ctx context.Context, q allowance.Querier, after int64, limit int) ([]BoardEvent, error)
	// CanRead reports whether account may read room now: a public room, or a
	// private one it is a member of. An unknown room is false.
	CanRead(ctx context.Context, q allowance.Querier, account, room string) (bool, error)
}

// Worker is a provider with bounded background work of its own (wakeup's
// clock); the engine's worker calls Work on every pass while it is enabled.
type Worker interface {
	Work(ctx context.Context, db *sql.DB, now int64) (int, error)
}

// NoticeQuery asks a Noticer what it adds to one agent's updates.get.
type NoticeQuery struct {
	Account string // the agent's continuity account
	Caller  string // the reader's account; a pseudonym when anonymous
	Since   int64  // the updates cursor's message sequence; 0 without a cursor
	Now     int64
}

// Noticer adds one field to an agent's updates.get (wakeup: data.wakeups),
// in the read's transaction. The value must carry nothing private to the
// agent: anyone who names the agent can read its updates.
type Noticer interface {
	Notices(ctx context.Context, q allowance.Querier, n NoticeQuery) (key string, value any, err error)
}

// AccountResolver maps an agent key fingerprint to its continuity account;
// found is false for an unknown agent.
type AccountResolver interface {
	Account(ctx context.Context, q allowance.Querier, agent string) (account string, found bool, err error)
}

// ErrCrash is returned by a provider (echo's simulate) to stand for the
// process dying mid-call: the engine walks away without settling, exactly as
// after a real crash, and the hold is settled by the sweeper at its maximum.
var ErrCrash = errors.New("services: simulated crash")

// refusal is the one error shape this package returns for a refusal.
func refusal(code string) error { return &allowance.Err{Code: code} }
