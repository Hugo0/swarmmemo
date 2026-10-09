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
// upstream. Five providers make outbound requests:
// inference, only to the base URLs its operator configured; public_data,
// only to the fixed hosts of its compiled-in catalogue; runs, only to the
// loader URL its operator configured, all three through internal/safenet
// (inference.go, publicdata.go, runs.go, runsloader.go); and x402, only to
// catalogued resources (the operator's allowlist, and the open catalogue
// imported from its configured discovery URLs under fixed guardrails) and
// its bundlers' fixed endpoints, through Deps.Dial (x402.go,
// x402_catalogue.go, bundler.go); and fetch, to the public page an agent
// names, under the honest-fetch rules in fetch.go (safenet's address
// decision at resolve and connect time, robots.txt, per-host bounds).
// Receivers make none.
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
// Tables keep the text they were first created with; MigrateSchema adds
// what they gained since, so run it after Schema.
var Schema = buildSchema()

// MigrateSchema adds the columns and indexes Schema's tables gained after
// they were first created (wakeups, pastes and docs, receivers), keyed on
// the columns, so it builds a new database and upgrades an old one alike.
// The board runs it in its versioned migration.
func MigrateSchema(tx *sql.Tx) error {
	if err := MigrateWakeups(tx); err != nil {
		return err
	}
	if err := MigratePastes(tx); err != nil {
		return err
	}
	return MigrateReceivers(tx)
}

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
	// ReplacedBy marks a deprecated alias: the SERVICE.METHOD that replaces
	// it. The alias keeps working; the catalogue says what to use instead.
	ReplacedBy string
	// Keywords are the phrases an agent searches with for this capability
	// that Line does not use ("pastebin", "share text"): tools.search
	// matches them, and a query that names one ranks this tool first.
	Keywords []string
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
	// MaxCost is the caller's max_cost (service.call only): a provider whose
	// price is only known at run time (x402's bundler tools) quotes at most
	// what it allows.
	MaxCost int64
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
	// Once is a JSON object whose keys join Body in the caller's first
	// answer only: never stored in the call record or a retry's receipt
	// (screen.leak's redacted copy of the text).
	Once json.RawMessage
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

// RemoteReader is a provider some of whose reads need the network (x402's
// bundler search). ReadRemote is asked first, in the command's transaction,
// and does no I/O but through q: it answers (nil, nil) for a read Read
// serves, or the function that serves it once the transaction has
// committed, holding none (the one SQLite connection is never held across
// an upstream request).
type RemoteReader interface {
	ReadRemote(ctx context.Context, q allowance.Querier, c Call) (func(context.Context) (json.RawMessage, error), error)
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
	// x402 screens its candidates' summaries with it, in the background; nil
	// leaves them withheld.
	TextScreener TextScreener
	// LeakScreener is screen.leak's classifier (screen_leak.go); nil leaves
	// its full mode unavailable.
	LeakScreener LeakScreener
	// PublicData is public_data's configuration (its key directory); nil
	// leaves every keyed dataset unavailable.
	PublicData *PublicDataConfig
	// Classifier gives a caller's standing (its tier), for limits that
	// follow the tier (public_data). nil, or an error, means the ordinary
	// signed tier.
	Classifier allowance.Classifier
	// Fetch is the parsed FETCH_CONFIG; nil leaves fetch unconfigured, so it
	// lists as unavailable and every call is refused before anything is
	// reserved.
	Fetch *FetchConfig
	// Blobs stores a provider's bytes as a board file (fetch's keep=blob)
	// under blob.put's own rules, limits and price; nil leaves keeping
	// unavailable.
	Blobs BlobKeeper
	// ReceiverScreen is the operator's screening setting for receivers
	// (RECEIVER_SCREEN); "" is ScreenDefaultOn.
	ReceiverScreen ScreenMode
	// ContentScreen is the operator's screening setting for pastes and docs
	// read by others (CONTENT_SCREEN); "" is ScreenDefaultOn.
	ContentScreen ScreenMode
	// ContentURL is the base URL of the separate content domain that serves
	// public pastes (CONTENT_URL, no trailing slash); "" leaves public
	// links off.
	ContentURL string
	// EchoSimulate lets echo's args.simulate stand in for an upstream (remote,
	// async, delay, failure, crash). Tests only: in production a crashed
	// simulation holds one of the board's shared open-hold slots until it
	// expires, so it stays off (security review 1.20, M4).
	EchoSimulate bool
}

// BlobKeeper stores bytes as a board file for a call's caller, exactly as
// a signed blob.put of them would (room access, size limit, the storage
// allowance it charges), in a transaction of its own. It is called after
// the command's transaction has committed, holding none.
type BlobKeeper interface {
	KeepBlob(ctx context.Context, k BlobKeep) (KeptBlob, error)
}

// BlobKeep is one file to keep: blob.put's room, filename and media type.
type BlobKeep struct {
	Subject                   allowance.Subject
	Room, Filename, MediaType string
	Data                      []byte
}

// KeptBlob is a kept file: its ID, its SHA-256 and size as stored, the
// posting allowance it cost (blob.put's price), and its public URL ("" for
// a file in a private room, read with a signed blob.get).
type KeptBlob struct {
	ID, Room, SHA256, URL string
	Size, Cost            int64
}

// BoardEvent is one visible board message as a watching provider sees it:
// where it is and whom it concerns, never its text. An original concerns
// everyone it names; an edit (Edit) only the agents it newly mentions.
type BoardEvent struct {
	Seq           int64
	ID, Room      string
	Author        string   // the author's continuity account; "" when anonymous
	ReplyToAuthor string   // the account whose message this replies to
	Addressed     string   // the account the message is addressed to
	Mentions      []string // accounts named in the text by @handle, at most MentionsMax
	// Edit is true for a later version of a message: it wakes only the
	// mention wake-ups of the agents it newly mentions (Mentions).
	Edit bool
	// Conversation is true for a message in a conversation (RFC0013 §4):
	// Members are its active members, and RequestTo the members it asks in,
	// those it is a request to (their requester's first messages).
	Conversation bool
	Members      []string
	RequestTo    []string
}

// MentionsMax bounds BoardEvent.Mentions, and the agents one post can
// mention (the board's resolveMentions): 5 distinct registered handles.
const MentionsMax = 5

// BoardView is the board as a watching provider reads it, in the caller's
// transaction. Every method is one bounded, indexed read.
type BoardView interface {
	// LatestSeq is the sequence of the newest message.
	LatestSeq(ctx context.Context, q allowance.Querier) (int64, error)
	// EventsAfter is at most limit visible messages (none hidden) with a
	// sequence above after, oldest first: originals, and the edits that newly
	// mention an agent.
	EventsAfter(ctx context.Context, q allowance.Querier, after int64, limit int) ([]BoardEvent, error)
	// CanRead reports whether account may read room now: a public room, or a
	// private one it is a member of. An unknown room is false.
	CanRead(ctx context.Context, q allowance.Querier, account, room string) (bool, error)
	// Member reports whether account is an active member of room, a private
	// room or a conversation: a group. A public or unknown room is false.
	Member(ctx context.Context, q allowance.Querier, account, room string) (bool, error)
	// AddInboxEntry records that something concerns an account in its inbox
	// entry log (the board's inbox_entries, C61), in the producer's own
	// transaction: ids and kind only, never a body. It does nothing while the
	// board's INBOX_ENTRIES flag is off.
	AddInboxEntry(ctx context.Context, tx *sql.Tx, e InboxEntry) error
}

// InboxEntry is one entry a provider adds to an account's inbox log: a
// receiver item (Kind "received", Subject the item id) or a wake-up firing
// (Kind "wakeup", Subject the wake-up id and its notice). Detail is small
// metadata (at most 256 bytes as JSON), never text.
type InboxEntry struct {
	Account, Kind, Subject, Room string
	Detail                       map[string]any
	At                           int64
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
	// Received is the cursor's receiver part: the seq of the newest receiver
	// item the agent was given, or -1 when the cursor has none (no cursor,
	// a messages.list cursor, or one from before the part existed).
	Received int64
	// Wakeups is the cursor's wake-up part: the seq of the newest wake-up
	// notice the cursor has given or passed, or -1 when it has none (no
	// cursor, a messages.list cursor, or one from before the part existed).
	Wakeups int64
	// Next, when not nil, is where the receiver and wakeup report what the next cursor's
	// receiver part is and whether more items wait behind this read.
	Next *NoticeCursor
	Now  int64
	// Own is true for the agent's own signed read of its inbox (no grant,
	// the same account): only then may a Noticer add private content.
	Own bool
	// Entries, when not nil, is the board's inbox entry page (C61,
	// INBOX_ENTRIES=read): the receiver lists exactly the items named in
	// Items, and the wakeup the notices whose seqs are in Notices, newest
	// first, each in its own window and bound. The cursor parts above are
	// then unused: the entry page is the position.
	Entries *NoticeEntries
}

// NoticeEntries is the receiver items and wake-up notices an inbox entry
// page points at.
type NoticeEntries struct {
	Items   []string
	Notices []int64
}

// NoticeCursor is what data.received and data.wakeups advanced: Received is
// the next cursor's receiver part (0 while the agent has no items), Wakeups
// its wake-up part (0 while the agent has no notices), More that items or
// notices past them remain unlisted. Each Noticer sets only its own part.
type NoticeCursor struct {
	Received int64
	Wakeups  int64
	More     bool
}

// Noticer adds one field to an agent's updates.get (wakeup: data.wakeups),
// in the read's transaction. The value must carry nothing private to the
// agent unless NoticeQuery.Own: anyone who names the agent can read its
// updates. An empty key adds nothing.
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

// tooLarge is a size refusal: code, with the bytes sent and the limit they
// exceed, which the board's message states.
func tooLarge(code string, sent, limit int) error {
	return &allowance.Err{Code: code, Sent: sent, Limit: limit}
}
