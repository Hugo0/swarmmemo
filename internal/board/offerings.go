package board

// Agent offerings over x402 (RFC 0017). An agent with a handle and a
// verified wallet link publishes a signed, priced offering (offering.publish);
// anyone, with or without a key, buys a call of it: the board answers 402
// with an x402 requirement that pays the provider's own linked wallet, the
// caller signs it, and the board has the facilitator verify the payment and
// keeps the signed authorization, sealed, until the provider claims the call
// (offering.claim), when the facilitator settles it to the provider. A
// declined call, or one nobody claims within the claim window, lapses with
// nothing charged. The board never holds USDC and never has a key that can
// move any.
//
// This file is the listing: its schema, publish, retire and the public reads.
// offering_calls.go is the call: quote, intake, claim, decline, lapse and
// the operator's resolve.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/services"
)

// Schema 28 (RFC 0017, C168). offerings is each listing as it stands,
// offering_revisions every signed revision (calls bind to one), and
// offering_calls each call: its input, its payment's public terms, the
// sealed authorization (auth, wiped once settled, declined or lapsed) and
// sha256 of the caller's poll secret. answer* and refund_tx are the seams of
// the answer and the refund (RFC 0017 part B).
const offeringSchema = `
CREATE TABLE IF NOT EXISTS offerings (
 account TEXT NOT NULL, name TEXT NOT NULL, rev INTEGER NOT NULL CHECK(rev > 0),
 agent TEXT NOT NULL, listing TEXT NOT NULL,
 price INTEGER NOT NULL CHECK(price > 0), pay_to TEXT NOT NULL,
 claim_window INTEGER NOT NULL, sla INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('active','retired')),
 screen TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 PRIMARY KEY(account,name));
CREATE INDEX IF NOT EXISTS offerings_listed ON offerings(state,updated_at);
CREATE TABLE IF NOT EXISTS offering_revisions (
 account TEXT NOT NULL, name TEXT NOT NULL, rev INTEGER NOT NULL,
 agent TEXT NOT NULL, public_key TEXT NOT NULL, signature TEXT NOT NULL, signed_payload TEXT NOT NULL,
 listing TEXT NOT NULL, created_at INTEGER NOT NULL,
 PRIMARY KEY(account,name,rev));
CREATE INDEX IF NOT EXISTS offering_revisions_day ON offering_revisions(account,created_at);
CREATE TABLE IF NOT EXISTS offering_calls (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE,
 account TEXT NOT NULL, name TEXT NOT NULL, rev INTEGER NOT NULL,
 caller TEXT NOT NULL DEFAULT '', payer TEXT NOT NULL,
 amount INTEGER NOT NULL CHECK(amount > 0), network TEXT NOT NULL, asset TEXT NOT NULL, pay_to TEXT NOT NULL,
 input TEXT NOT NULL, input_sha256 TEXT NOT NULL,
 input_screen TEXT NOT NULL DEFAULT 'pending' CHECK(input_screen IN ('pending','pass','flag','unscreened')),
 input_scores TEXT NOT NULL DEFAULT '',
 auth TEXT NOT NULL DEFAULT '', auth_key TEXT NOT NULL DEFAULT '',
 auth_nonce TEXT NOT NULL UNIQUE, quote_hash TEXT NOT NULL, valid_before INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('verifying','authorized','settling','paid','failed','unknown','declined','lapsed','answered','overdue','refunded')),
 reason TEXT NOT NULL DEFAULT '', retryable INTEGER NOT NULL DEFAULT 0,
 tx_hash TEXT NOT NULL DEFAULT '', poll_hash TEXT NOT NULL,
 answer TEXT NOT NULL DEFAULT '', answer_sha256 TEXT NOT NULL DEFAULT '', refund_tx TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, claim_by INTEGER NOT NULL, claimed_at INTEGER NOT NULL DEFAULT 0,
 due_at INTEGER NOT NULL DEFAULT 0, settled_at INTEGER NOT NULL DEFAULT 0,
 answered_at INTEGER NOT NULL DEFAULT 0, closed_at INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX IF NOT EXISTS offering_calls_tx ON offering_calls(tx_hash) WHERE tx_hash<>'';
CREATE INDEX IF NOT EXISTS offering_calls_provider ON offering_calls(account,seq);
CREATE INDEX IF NOT EXISTS offering_calls_open ON offering_calls(state,claim_by) WHERE state IN ('verifying','authorized','settling');
CREATE INDEX IF NOT EXISTS offering_calls_payer ON offering_calls(payer,state);
`

// Offering limits (RFC 0017 "Abuse"). Prices are micro-USDC.
const (
	// OfferingsPerAccount bounds the offerings one account lists, retired
	// ones included.
	OfferingsPerAccount = 8
	// OfferingPublishesPerDay bounds the revisions one account publishes
	// per UTC day (each publish is a new revision and a screen).
	OfferingPublishesPerDay = 24
	// OfferingNameChars bounds a name: lowercase letters, digits and -.
	OfferingNameChars = 40
	// OfferingTitleChars and OfferingDescriptionBytes bound the text.
	OfferingTitleChars       = 80
	OfferingDescriptionBytes = 2000
	// OfferingInputSchemaBytes bounds the input schema; OfferingInputBytes
	// one call's input.
	OfferingInputSchemaBytes = 2 << 10
	OfferingInputBytes       = 8 << 10
	// OfferingSchemaProperties bounds the input schema's properties.
	OfferingSchemaProperties = 16
	// OfferingClaimWindowDefault and OfferingSLADefault are the defaults;
	// the SLA runs from the claim, OfferingSLAMin to OfferingSLAMax. The
	// claim window defaults to the ceiling (an hour): sellers on a 15-minute
	// heartbeat missed calls on the old 900 s default (C241).
	OfferingClaimWindowDefault int64 = 3600
	OfferingSLADefault         int64 = 3600
	OfferingSLAMin             int64 = 60
	OfferingSLAMax             int64 = 7 * 86400
	// OfferingOpenCalls bounds the calls one offering holds open (being
	// verified, awaiting a claim, or settling); OfferingOpenCallsPerPayer
	// the open calls one paying wallet holds across every offering.
	OfferingOpenCalls         = 20
	OfferingOpenCallsPerPayer = 3
	// OfferingQuotesPerMinute bounds the quotes one caller (a key, or a
	// network without one) asks for; OfferingQuotesPerOfferingMinute the
	// quotes one offering gives out.
	OfferingQuotesPerMinute         = 10
	OfferingQuotesPerOfferingMinute = 60
	// OfferingCallsPageMax bounds one offering.calls page and one
	// offering.list page.
	OfferingCallsPageMax = 50
	// OfferingClaimMinimum is how long an authorization must leave to claim
	// in: a payment valid for less is refused at intake.
	OfferingClaimMinimum int64 = 60
	// OfferingDeclineReasonBytes bounds a decline's reason.
	OfferingDeclineReasonBytes = 512
	// OfferingRecordDays is the window of a listing's public record.
	OfferingRecordDays = 30
	// OfferingsKeyFileName is the sealing key's default file beside the
	// database.
	OfferingsKeyFileName = "offerings.key"
	// offeringSweepEvery spaces the lapse sweeps.
	offeringSweepEvery = 30 * time.Second
	// offeringVerifyStale is how long a call may sit verifying (a crash
	// between commit and the facilitator's answer) before it fails.
	offeringVerifyStale int64 = 600
)

var (
	offeringNameRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,` + strconv.Itoa(OfferingNameChars-1) + `}$`)
	offeringPropRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	offeringCallRE  = regexp.MustCompile(`^oc_[0-9a-f]{32}$`)
	offeringRefunds = []string{"none", "full_if_unanswered"}
)

// offeringState is the Store's offerings: the sealing AEAD and poll key
// (from OFFERINGS_KEY_FILE; nil while offerings are off), the quote rate
// buckets, the lapse sweeper and the input screens in flight.
type offeringState struct {
	aead    cipher.AEAD
	pollKey []byte
	keyID   string
	mu      sync.Mutex
	rates   map[string]privateReadBucket
	cancel  context.CancelFunc
	wg      sync.WaitGroup // the sweeper
	screens sync.WaitGroup // input screens after intake
}

// openOfferings loads the sealing key while offerings are configured and,
// either way, settles what a crash left in flight: a call left verifying
// moved nothing (failed, may be presented again); one left settling may have
// settled, so it is unknown, for the operator. Both lose their authorization.
func (s *Store) openOfferings(path string) error {
	// Crash recovery belongs to the serving process: beside a live server
	// (the operator CLI) these rows are calls in flight (C553).
	if !s.config.Operator {
		if res, err := s.db.Exec("UPDATE offering_calls SET state='failed',reason='interrupted',retryable=1,auth='',closed_at=? WHERE state='verifying'", time.Now().Unix()); err != nil {
			return err
		} else if n, _ := res.RowsAffected(); n > 0 {
			slog.Warn("Offering calls interrupted while verifying were failed (nothing moved)", "count", n)
		}
		if res, err := s.db.Exec("UPDATE offering_calls SET state='unknown',reason='interrupted',auth='' WHERE state='settling'"); err != nil {
			return err
		} else if n, _ := res.RowsAffected(); n > 0 {
			slog.Error("Offering calls were interrupted while settling: resolve each with swarmmemo offering resolve", "count", n)
		}
	}
	if s.config.Offerings == nil {
		return nil
	}
	keyFile := s.config.OfferingsKeyFile
	if keyFile == "" && path != ":memory:" && !strings.HasPrefix(path, "file::memory:") {
		keyFile = filepath.Join(filepath.Dir(path), OfferingsKeyFileName)
	}
	var key []byte
	var err error
	if keyFile == "" {
		key = make([]byte, 32)
		_, err = randRead(key)
	} else {
		key, err = s.hexKeyFile(keyFile, "offerings")
	}
	if err != nil {
		return err
	}
	seal := sha256.Sum256(append([]byte("swarmmemo-offering-seal/1\x00"), key...))
	poll := sha256.Sum256(append([]byte("swarmmemo-offering-poll/1\x00"), key...))
	id := sha256.Sum256(append([]byte("swarmmemo-offering-key-id\x00"), key...))
	clear(key)
	block, err := aes.NewCipher(seal[:])
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return err
	}
	s.offerings.aead, s.offerings.pollKey, s.offerings.keyID = aead, poll[:], fmt.Sprintf("%x", id[:8])
	s.offerings.rates = map[string]privateReadBucket{}
	return nil
}

// OfferingsEnabled reports whether offerings are on: a facilitator
// configured for them and the sealing key loaded.
func (s *Store) OfferingsEnabled() bool {
	return s.config.Offerings != nil && s.offerings.aead != nil
}

func offeringsUnavailable() error {
	return problem(404, "offerings_unavailable", "Agent offerings are not enabled on this server; see /capabilities.")
}

// offeringData is offering.publish's data, strict: the listing the provider
// signs.
type offeringData struct {
	Schema       json.Number     `json:"schema"`
	Name         string          `json:"name"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	Price        string          `json:"price"`
	PayTo        string          `json:"pay_to"`
	Input        json.RawMessage `json:"input,omitempty"`
	ClaimWindow  *int64          `json:"claim_window,omitempty"`
	SLA          *int64          `json:"sla,omitempty"`
	Refund       string          `json:"refund,omitempty"`
	ScreenAnswer *bool           `json:"screen_answer,omitempty"`
}

// offeringListing is a listing as stored and shown, normalized: the price in
// canonical USDC, the defaults filled in.
type offeringListing struct {
	Name         string          `json:"name"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	Price        string          `json:"price"`
	PayTo        string          `json:"pay_to"`
	Input        json.RawMessage `json:"input,omitempty"`
	ClaimWindow  int64           `json:"claim_window"`
	SLA          int64           `json:"sla"`
	Refund       string          `json:"refund"`
	ScreenAnswer bool            `json:"screen_answer"`
}

func invalidOffering(detail string) error {
	return problem(400, "invalid_offering", "offering.publish data is {\"schema\":1,\"name\",\"title\",\"description\",\"price\":\"5.00\",\"pay_to\":\"0x…\"} with optional \"input\" (a JSON Schema subset), \"claim_window\" (seconds), \"sla\" (seconds), \"refund\" and \"screen_answer\": "+detail)
}

// parseOffering checks publish data against every rule but the wallet link
// and the caps (those read the database).
func (s *Store) parseOffering(raw string) (offeringListing, int64, error) {
	var d offeringData
	if services.StrictObject([]byte(raw), &d) != nil || d.Schema.String() != "1" {
		return offeringListing{}, 0, invalidOffering("strict JSON, schema 1, no other field.")
	}
	l := offeringListing{Name: d.Name, Title: strings.TrimSpace(d.Title), Description: strings.TrimSpace(d.Description), Refund: d.Refund, ClaimWindow: OfferingClaimWindowDefault, SLA: OfferingSLADefault, ScreenAnswer: true}
	if !offeringNameRE.MatchString(d.Name) {
		return l, 0, invalidOffering(fmt.Sprintf("name is 1 to %d lowercase letters, digits and -, starting with a letter or digit.", OfferingNameChars))
	}
	if l.Title == "" || utf8.RuneCountInString(l.Title) > OfferingTitleChars || !utf8.ValidString(l.Title) || strings.ContainsAny(l.Title, "\r\n") {
		return l, 0, invalidOffering(fmt.Sprintf("title is one line of 1 to %d characters.", OfferingTitleChars))
	}
	if len(l.Description) > OfferingDescriptionBytes || !utf8.ValidString(l.Description) {
		return l, 0, invalidOffering(fmt.Sprintf("description is at most %d bytes of UTF-8.", OfferingDescriptionBytes))
	}
	cfg := s.config.Offerings
	price, ok := services.ParseUSDC(d.Price)
	if !ok || price < services.OfferingPriceFloor || price > cfg.OfferingMaxPrice {
		return l, 0, problem(400, "offering_price", fmt.Sprintf("price is a USDC amount from %s to %s (at most 6 decimals), as a string.", services.FormatUSDC(services.OfferingPriceFloor), services.FormatUSDC(cfg.OfferingMaxPrice)))
	}
	l.Price = services.FormatUSDC(price)
	payTo, ok := services.ParseEVMAddress(d.PayTo)
	if !ok || payTo == (services.EVMAddress{}) {
		return l, 0, invalidOffering("pay_to is an EVM address, 0x and 40 hex digits: one of your verified wallet links.")
	}
	l.PayTo = payTo.String()
	if d.ClaimWindow != nil {
		l.ClaimWindow = *d.ClaimWindow
	}
	if l.ClaimWindow < services.OfferingTimeoutFloor || l.ClaimWindow > services.OfferingTimeoutCeiling {
		return l, 0, invalidOffering(fmt.Sprintf("claim_window is %d to %d seconds.", services.OfferingTimeoutFloor, services.OfferingTimeoutCeiling))
	}
	if d.SLA != nil {
		l.SLA = *d.SLA
	}
	if l.SLA < OfferingSLAMin || l.SLA > OfferingSLAMax {
		return l, 0, invalidOffering(fmt.Sprintf("sla is %d to %d seconds from the claim.", OfferingSLAMin, OfferingSLAMax))
	}
	if l.Refund == "" {
		l.Refund = "none"
	}
	if !slices.Contains(offeringRefunds, l.Refund) {
		return l, 0, invalidOffering(`refund is "none" or "full_if_unanswered".`)
	}
	if d.ScreenAnswer != nil {
		l.ScreenAnswer = *d.ScreenAnswer
	}
	if len(d.Input) > 0 && string(d.Input) != "null" {
		if len(d.Input) > OfferingInputSchemaBytes {
			return l, 0, problem(400, "invalid_input_schema", "input is at most "+strconv.Itoa(OfferingInputSchemaBytes)+" bytes "+SizeNote(len(d.Input), OfferingInputSchemaBytes, "bytes")+".")
		}
		if _, err := parseInputSchema(d.Input); err != nil {
			return l, 0, err
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, d.Input); err != nil {
			return l, 0, invalidInputSchema("input is a JSON object.")
		}
		l.Input = json.RawMessage(compact.Bytes())
	}
	return l, price, nil
}

// changeOffering is offering.publish and offering.retire.
func (s *Store) changeOffering(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if !s.OfferingsEnabled() {
		return Result{}, offeringsUnavailable()
	}
	if c.Operation == "offering.retire" {
		name := strings.TrimPrefix(c.Target, "@")
		res, err := tx.ExecContext(ctx, "UPDATE offerings SET state='retired',updated_at=? WHERE account=? AND name=? AND state='active'", now, a.account, name)
		if err != nil {
			return Result{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var state string
			if err = tx.QueryRowContext(ctx, "SELECT state FROM offerings WHERE account=? AND name=?", a.account, name).Scan(&state); errors.Is(err, sql.ErrNoRows) {
				return Result{}, problem(404, "offering_not_found", "You have no offering by that name; target is its name.")
			} else if err != nil {
				return Result{}, err
			}
		}
		o, err := loadOffering(ctx, tx, a.account, name)
		if err != nil {
			return Result{}, err
		}
		v, err := s.offeringView(ctx, tx, o, false, now)
		if err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"offering": v}}, nil
	}
	l, price, err := s.parseOffering(c.Data)
	if err != nil {
		return Result{}, err
	}
	var handle string
	if handle, err = accountHandle(ctx, tx, a.account); err != nil {
		return Result{}, err
	}
	if handle == "" {
		return Result{}, problem(403, "handle_required", "Publishing an offering needs a registered handle (agent.register), so its page is /@HANDLE/NAME.")
	}
	wallets, err := accountWallets(ctx, tx, a.account)
	if err != nil {
		return Result{}, err
	}
	payTo, _ := services.ParseEVMAddress(strings.ToLower(l.PayTo))
	if !wallets[payTo] {
		return Result{}, problem(400, "pay_to_unlinked", "pay_to must be one of your verified wallet links: callers pay it directly. Link the wallet first (standing.challenge kind wallet, then identity.link), then publish again.")
	}
	obs, _ := ctx.Value(offeringScreenKey{}).(*offeringScreenObservation)
	screen := "unscreened"
	if obs != nil && obs.data == c.Data {
		screen = obs.screen.state
		if screen == "flag" {
			return Result{}, &Error{Status: 422, Code: "offering_flagged", Message: "Screening flagged this offering's title or description (" + strings.Join(flagged(obs.screen.scores, services.ScreenCategories, services.ScreenThreshold), ", ") + "); it was not published. Agents act on listings, so a listing may not carry instructions to them. Change the text and publish again."}
		}
	}
	var listed, today int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM offerings WHERE account=? AND name<>?", a.account, l.Name).Scan(&listed); err != nil {
		return Result{}, err
	}
	if listed >= OfferingsPerAccount {
		return Result{}, problem(409, "offering_limit", fmt.Sprintf("An account lists at most %d offerings, retired ones included; publish a new revision of one of yours instead.", OfferingsPerAccount))
	}
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM offering_revisions WHERE account=? AND created_at>=?", a.account, now-now%86400).Scan(&today); err != nil {
		return Result{}, err
	}
	if today >= OfferingPublishesPerDay {
		return Result{}, rateError(now, "offering_publish_limit", fmt.Sprintf("An account publishes at most %d offering revisions per UTC day; it resets at 00:00 UTC.", OfferingPublishesPerDay))
	}
	if err = s.charge(ctx, tx, a, int64(len(a.canonical))+512, now); err != nil {
		return Result{}, err
	}
	listing, err := json.Marshal(l)
	if err != nil {
		return Result{}, err
	}
	// A new title or an incompatible input starts a new version: reviews
	// of the earlier one stay listed, out of the counts (offering_versions.go).
	version, change := int64(1), ""
	var prevListing string
	switch err = tx.QueryRowContext(ctx, "SELECT listing,version FROM offerings WHERE account=? AND name=?", a.account, l.Name).Scan(&prevListing, &version); {
	case errors.Is(err, sql.ErrNoRows):
		version = 1
	case err != nil:
		return Result{}, err
	default:
		var prev offeringListing
		if json.Unmarshal([]byte(prevListing), &prev) == nil {
			if change = offeringChange(prev, l); change != "" {
				version++
			}
		}
	}
	var rev int64
	if err = tx.QueryRowContext(ctx, `INSERT INTO offerings(account,name,rev,agent,listing,price,pay_to,claim_window,sla,state,screen,created_at,updated_at,version)
VALUES(?,?,1,?,?,?,?,?,?,'active',?,?,?,?)
ON CONFLICT(account,name) DO UPDATE SET rev=offerings.rev+1,agent=excluded.agent,listing=excluded.listing,price=excluded.price,pay_to=excluded.pay_to,
 claim_window=excluded.claim_window,sla=excluded.sla,state='active',screen=excluded.screen,updated_at=excluded.updated_at,version=excluded.version
RETURNING rev`, a.account, l.Name, a.id, string(listing), price, l.PayTo, l.ClaimWindow, l.SLA, screen, now, now, version).Scan(&rev); err != nil {
		return Result{}, err
	}
	earlier := 0
	if change != "" {
		if earlier, err = s.setOfferingSubjectsVersion(ctx, tx, a.account, l.Name, version); err != nil {
			return Result{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO offering_revisions(account,name,rev,agent,public_key,signature,signed_payload,listing,created_at) VALUES(?,?,?,?,?,?,?,?,?)",
		a.account, l.Name, rev, a.id, a.publicKey, c.Signature, string(a.canonical), string(listing), now); err != nil {
		return Result{}, err
	}
	o, err := loadOffering(ctx, tx, a.account, l.Name)
	if err != nil {
		return Result{}, err
	}
	v, err := s.offeringView(ctx, tx, o, false, now)
	if err != nil {
		return Result{}, err
	}
	data := map[string]any{"offering": v}
	if change != "" {
		data["new_version"] = map[string]any{"version": version, "because": change, "earlier_reviews": earlier,
			"note": fmt.Sprintf("This publish started version %d of the offering, because %s. Reviews written before it are now marked as %s: they stay listed on its page but no longer count in its reviews' counts or score. A description, price or timing edit keeps the version.", version, change, EarlierVersionLabel)}
	}
	return Result{Data: data}, nil
}

// offeringScreenObservation is the screen preflightOfferingScreen ran for
// one publish, outside the transaction.
type offeringScreenObservation struct {
	data   string
	screen messageScreen
}

type offeringScreenKey struct{}

// preflightOfferingScreen screens a publish's title and description before
// the command's transaction opens, only for a signed key whose data parses:
// a listing is text agents act on, screened like a post.
func (s *Store) preflightOfferingScreen(ctx context.Context, cmd Command, a actor) context.Context {
	if cmd.Operation != "offering.publish" || !a.signed || !s.OfferingsEnabled() {
		return ctx
	}
	l, _, err := s.parseOffering(cmd.Data)
	if err != nil {
		return ctx
	}
	// Only a publish that can succeed is screened: a handle, pay_to one of
	// the account's verified wallet links, and today's revisions under the
	// cap. Anything else is refused in the transaction, unscreened.
	now := s.now().Unix()
	var ready bool
	if s.db.QueryRowContext(ctx, `SELECT i.handle<>'' AND EXISTS(SELECT 1 FROM identity_links l JOIN identities k ON k.id=l.agent
 WHERE k.account=i.account AND l.kind='wallet' AND l.state='verified' AND lower(l.value)=lower(?))
 AND (SELECT count(*) FROM offering_revisions r WHERE r.account=i.account AND r.created_at>=?)<?
 FROM identities i WHERE i.id=?`, l.PayTo, now-now%86400, OfferingPublishesPerDay, a.id).Scan(&ready) != nil || !ready {
		return ctx
	}
	screen := s.screenText(ctx, l.Title+"\n\n"+l.Description, payerSwarmMemo)
	return context.WithValue(ctx, offeringScreenKey{}, &offeringScreenObservation{data: cmd.Data, screen: screen})
}

// accountWallets is the set of verified wallet links of account's keys: a
// work requester's paying wallets (RFC 0016) and an offering's pay_to
// (RFC 0017).
func accountWallets(ctx context.Context, tx *sql.Tx, account string) (map[services.EVMAddress]bool, error) {
	return requesterWallets(ctx, tx, account)
}

// offeringRow is one row of offerings with its listing.
type offeringRow struct {
	Account, Name, Agent, PayTo, State, Screen string
	Rev, Price, ClaimWindow, SLA               int64
	// Version counts the publishes that changed what is reviewed
	// (offering_versions.go); Rev counts every publish.
	Version          int64
	Created, Updated int64
	Listing          offeringListing
}

const offeringCols = "account,name,rev,agent,listing,price,pay_to,claim_window,sla,state,screen,created_at,updated_at,version"

func scanOffering(scan func(...any) error) (offeringRow, error) {
	var o offeringRow
	var listing string
	if err := scan(&o.Account, &o.Name, &o.Rev, &o.Agent, &listing, &o.Price, &o.PayTo, &o.ClaimWindow, &o.SLA, &o.State, &o.Screen, &o.Created, &o.Updated, &o.Version); err != nil {
		return o, err
	}
	return o, json.Unmarshal([]byte(listing), &o.Listing)
}

func loadOffering(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, account, name string) (offeringRow, error) {
	o, err := scanOffering(q.QueryRowContext(ctx, "SELECT "+offeringCols+" FROM offerings WHERE account=? AND name=?", account, name).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return o, problem(404, "offering_not_found", "No such offering; read offering.list for the offerings listed.")
	}
	return o, err
}

// offeringProvider resolves a provider address (a handle, @handle, or a
// fingerprint) to its account.
func offeringProvider(ctx context.Context, tx *sql.Tx, alias string) (string, error) {
	alias = strings.ToLower(strings.TrimPrefix(alias, "@"))
	var account string
	var err error
	switch {
	case fingerprintRE.MatchString(alias):
		err = tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=? OR account=? LIMIT 1", alias, alias).Scan(&account)
	case handleRE.MatchString(alias):
		err = tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE handle=? AND successor=''", alias).Scan(&account)
	default:
		return "", errOfferingTarget()
	}
	if errors.Is(err, sql.ErrNoRows) && !fingerprintRE.MatchString(alias) {
		// A former handle still leads to its account's offerings (C249).
		var ok bool
		if account, ok, err = formerHandleAccount(ctx, tx, alias); err == nil && !ok {
			err = sql.ErrNoRows
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", problem(404, "offering_not_found", "No agent has this address, so no offering either.")
	}
	return account, err
}

// formerHandleAccount is the account that last held handle, while no key
// holds it now (C249). The audit's agent.register and handle.claim rows are
// the handle history (the signed record reads them the same way), so a
// renamed agent's old offering addresses keep leading to it, and stop the
// moment another agent registers the handle: an old handle never captures
// a new holder's address, nor the new holder the old one's reviews.
func formerHandleAccount(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, handle string) (string, bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT i.account FROM audit au JOIN identities i ON i.id=au.actor
 WHERE au.operation IN ('agent.register','handle.claim') AND au.detail=?1 AND NOT EXISTS(SELECT 1 FROM identities h WHERE h.handle=?1)
 ORDER BY au.seq DESC LIMIT 1`, strings.ToLower(handle))
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	var account string
	ok := rows.Next()
	if ok {
		err = rows.Scan(&account)
	}
	if err == nil {
		err = rows.Err()
	}
	return account, ok && err == nil, err
}

// formerHandles are the handles account held before its current one that
// still lead to it (formerHandleAccount), newest first, at most
// formerHandlesMax.
func formerHandles(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, account string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT au.detail FROM audit au JOIN identities i ON i.id=au.actor
 WHERE i.account=?1 AND au.operation IN ('agent.register','handle.claim') AND au.detail NOT IN ('','`+ErasedDetail+`')
 AND NOT EXISTS(SELECT 1 FROM identities h WHERE h.handle=au.detail)
 AND NOT EXISTS(SELECT 1 FROM audit la JOIN identities li ON li.id=la.actor WHERE la.operation IN ('agent.register','handle.claim') AND la.detail=au.detail AND la.seq>au.seq AND li.account<>?1)
 GROUP BY au.detail ORDER BY max(au.seq) DESC LIMIT ?2`, account, formerHandlesMax)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err = rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// formerHandlesMax bounds the former addresses an offering's subject lists.
const formerHandlesMax = 8

// OfferingMoved is the current path of the offering at /@ALIAS/NAME when
// ALIAS is a handle its provider held before (formerHandleAccount), so a
// GET there answers 308 to it; "" for every other address (C249).
func (s *Store) OfferingMoved(ctx context.Context, alias, name string) (string, error) {
	alias = strings.ToLower(strings.TrimPrefix(alias, "@"))
	if !s.OfferingsEnabled() || fingerprintRE.MatchString(alias) || !handleRE.MatchString(alias) || !offeringNameRE.MatchString(name) {
		return "", nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	account, ok, err := formerHandleAccount(ctx, tx, alias)
	if err != nil || !ok {
		return "", err
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM offerings WHERE account=? AND name=?)", account, name).Scan(&exists); err != nil || !exists {
		return "", err
	}
	_, handle, err := providerOf(ctx, tx, account)
	if err != nil {
		return "", err
	}
	return offeringPath(account, handle, name), nil
}

// offeringTarget resolves "HANDLE/NAME" (an optional leading @) to the
// offering's account and name.
func offeringTarget(ctx context.Context, tx *sql.Tx, target string) (string, string, error) {
	alias, name, ok := strings.Cut(strings.TrimPrefix(target, "@"), "/")
	if !ok || !offeringNameRE.MatchString(name) {
		return "", "", errOfferingTarget()
	}
	account, err := offeringProvider(ctx, tx, alias)
	return account, name, err
}

// providerOf is the account's current key and its handle.
func providerOf(ctx context.Context, tx *sql.Tx, account string) (agent, handle string, err error) {
	err = tx.QueryRowContext(ctx, "SELECT id,handle FROM identities WHERE account=? AND successor='' AND role='primary' ORDER BY handle='' , created_at DESC LIMIT 1", account).Scan(&agent, &handle)
	if errors.Is(err, sql.ErrNoRows) {
		return account, "", nil
	}
	return agent, handle, err
}

// agentOfferings are the pages of an account's active offerings, by name,
// for agent.get: the profile points at them, offering.get reads one.
func agentOfferings(ctx context.Context, tx *sql.Tx, account string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT name FROM offerings WHERE account=? AND state='active' ORDER BY name LIMIT ?", account, OfferingCallsPageMax)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if err = rows.Err(); err != nil || len(names) == 0 {
		return nil, err
	}
	_, handle, err := providerOf(ctx, tx, account)
	if err != nil {
		return nil, err
	}
	for i, name := range names {
		names[i] = offeringPath(account, handle, name)
	}
	return names, nil
}

// offeringPath is the offering's public path, by handle while it has one.
func offeringPath(account, handle, name string) string {
	if handle != "" {
		return "/@" + handle + "/" + name
	}
	return "/@" + account + "/" + name
}

// effectiveClaimWindow is the claim window a call gets: the listing's,
// clamped to the facilitator's largest maxTimeoutSeconds.
func (s *Store) effectiveClaimWindow(w int64) int64 {
	if cfg := s.config.Offerings; cfg != nil && cfg.OfferingMaxTimeout > 0 && w > cfg.OfferingMaxTimeout {
		return cfg.OfferingMaxTimeout
	}
	return w
}

// offeringView is a listing as every read shows it: the signed revision,
// the payment terms and, with record, its public 30-day record, its
// provider's responses and whether it is listed (offering_responses.go).
func (s *Store) offeringView(ctx context.Context, tx *sql.Tx, o offeringRow, record bool, now int64) (map[string]any, error) {
	agent, handle, err := providerOf(ctx, tx, o.Account)
	if err != nil {
		return nil, err
	}
	var publicKey, signature, payload string
	if err = tx.QueryRowContext(ctx, "SELECT public_key,signature,signed_payload FROM offering_revisions WHERE account=? AND name=? AND rev=?", o.Account, o.Name, o.Rev).Scan(&publicKey, &signature, &payload); err != nil {
		return nil, err
	}
	l := o.Listing
	v := map[string]any{
		"provider": map[string]any{"fingerprint": agent, "handle": handle, "account": o.Account},
		"name":     o.Name, "rev": o.Rev, "version": o.Version, "state": o.State, "title": l.Title, "description": l.Description,
		"description_is_untrusted": true,
		"price":                    l.Price, "units": o.Price, "unit": "USDC",
		"pay_to": l.PayTo, "claim_window": s.effectiveClaimWindow(o.ClaimWindow), "sla": o.SLA, "refund": l.Refund,
		"screen_answer": l.ScreenAnswer, "screen": o.Screen,
		"path": offeringPath(o.Account, handle, o.Name), "url": "https://" + s.config.ServiceID + offeringPath(o.Account, handle, o.Name),
		"created_at": o.Created, "updated_at": o.Updated,
		"signed": map[string]any{"public_key": publicKey, "signature": signature, "signed_payload": payload, "signer": o.Agent},
	}
	if len(l.Input) > 0 {
		v["input"] = l.Input
	}
	if cfg := s.config.Offerings; cfg != nil {
		v["network"], v["asset"] = cfg.Network, cfg.Asset.String()
	}
	if record {
		if v["record"], err = offeringRecord(ctx, tx, o.Account, o.Name, now); err != nil {
			return nil, err
		}
		responses, responsive, err := offeringResponses(ctx, tx, o.Account, now)
		if err != nil {
			return nil, err
		}
		v["responses"], v["listed"] = responses, o.State == "active" && responsive
		if o.State == "active" && !responsive {
			v["unlisted"] = "Not in listings: " + OfferingListedRule + ". It is still callable here."
		}
	}
	return v, nil
}

// offeringRecord is a listing's public record over OfferingRecordDays: calls
// by outcome and distinct paying wallets, leaving out calls paid from the
// provider's own linked wallets (self-dealing would buy a record for the gas
// and the fee). Counts only; never an amount.
func offeringRecord(ctx context.Context, tx *sql.Tx, account, name string, now int64) (map[string]any, error) {
	self, err := accountWallets(ctx, tx, account)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT payer,state,claim_by,due_at,created_at,claimed_at FROM offering_calls WHERE account=? AND name=? AND created_at>=? AND state NOT IN ('verifying','failed') LIMIT 5000", account, name, now-OfferingRecordDays*86400)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{"calls": 0, "paid": 0, "answered": 0, "declined": 0, "lapsed": 0, "overdue": 0, "refunded": 0}
	payers := map[string]bool{}
	var latencies []int64
	for rows.Next() {
		var payer, state string
		var claimBy, dueAt, created, claimed int64
		if err = rows.Scan(&payer, &state, &claimBy, &dueAt, &created, &claimed); err != nil {
			rows.Close()
			return nil, err
		}
		if a, ok := services.ParseEVMAddress(strings.ToLower(payer)); ok && self[a] {
			continue
		}
		counts["calls"]++
		payers[strings.ToLower(payer)] = true
		if claimed > 0 && claimed >= created {
			latencies = append(latencies, claimed-created)
		}
		switch effectiveCallState(state, claimBy, dueAt, now) {
		case "paid", "settling", "unknown":
			counts["paid"]++
		case "answered":
			counts["paid"]++
			counts["answered"]++
		case "overdue":
			counts["paid"]++
			counts["overdue"]++
		case "refunded":
			counts["paid"]++
			counts["refunded"]++
		case "declined":
			counts["declined"]++
		case "lapsed":
			counts["lapsed"]++
		}
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	out := map[string]any{"days": OfferingRecordDays, "payers": len(payers), "excludes": "calls paid from the provider's own linked wallets"}
	for k, n := range counts {
		out[k] = n
	}
	// median_claim_seconds: from a call's arrival to the provider's claim,
	// over the claimed calls; absent while none was claimed.
	if len(latencies) > 0 {
		slices.Sort(latencies)
		m := latencies[len(latencies)/2]
		if len(latencies)%2 == 0 {
			m = (latencies[len(latencies)/2-1] + m) / 2
		}
		out["median_claim_seconds"] = m
	}
	return out, nil
}

// readOfferings is offering.list and offering.get, public reads.
func (s *Store) readOfferings(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if !s.OfferingsEnabled() {
		return Result{}, offeringsUnavailable()
	}
	if c.Operation == "offering.get" {
		account, name, err := offeringTarget(ctx, tx, c.Target)
		if err != nil {
			return Result{}, err
		}
		o, err := loadOffering(ctx, tx, account, name)
		if err != nil {
			return Result{}, err
		}
		v, err := s.offeringView(ctx, tx, o, true, now)
		if err != nil {
			return Result{}, err
		}
		sum, err := s.offeringReviews(ctx, tx, account, name, now)
		if err != nil {
			return Result{}, err
		}
		if sum != nil {
			v["reviews"] = sum
		}
		return Result{Data: map[string]any{"offering": v}}, nil
	}
	limit := c.Limit
	if err := limitInRange("Call list limit", limit, OfferingCallsPageMax); err != nil {
		return Result{}, err
	}
	if limit == 0 {
		limit = 20
	}
	offset := 0
	if c.Cursor != "" {
		n, err := strconv.Atoi(c.Cursor)
		if err != nil || n <= 0 || n > 10_000 {
			return Result{}, errCursorNextPage()
		}
		offset = n
	}
	where, args := []string{offeringListedTerm(now)}, []any{}
	if c.Target != "" {
		account, err := offeringProvider(ctx, tx, c.Target)
		if err != nil {
			return Result{}, err
		}
		where, args = append(where, "account=?"), append(args, account)
	}
	if q := strings.ToLower(strings.TrimSpace(c.Query)); q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q) + "%"
		where = append(where, `(name LIKE ? ESCAPE '\' OR lower(json_extract(listing,'$.title')) LIKE ? ESCAPE '\' OR lower(json_extract(listing,'$.description')) LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like)
	}
	// Listed and ranked as every listing is (offering_responses.go).
	rows, err := tx.QueryContext(ctx, "SELECT "+offeringCols+" FROM offerings o WHERE "+strings.Join(where, " AND ")+offeringListOrder(now)+" LIMIT ? OFFSET ?",
		append(args, limit+1, offset)...)
	if err != nil {
		return Result{}, err
	}
	var list []offeringRow
	for rows.Next() {
		o, err := scanOffering(rows.Scan)
		if err != nil {
			rows.Close()
			return Result{}, err
		}
		list = append(list, o)
	}
	if err = closeRows(rows); err != nil {
		return Result{}, err
	}
	out := []map[string]any{}
	data := map[string]any{"schema": 1}
	for i, o := range list {
		if i == limit {
			data["next_cursor"] = strconv.Itoa(offset + limit)
			break
		}
		v, err := s.offeringView(ctx, tx, o, true, now)
		if err != nil {
			return Result{}, err
		}
		delete(v, "signed")
		out = append(out, v)
	}
	data["offerings"] = out
	return Result{Data: data}, nil
}

// OfferingsCapabilities is the /capabilities "offerings" object; nil while
// offerings are off.
func (s *Store) OfferingsCapabilities() map[string]any {
	if !s.OfferingsEnabled() {
		return nil
	}
	cfg := s.config.Offerings
	caps := map[string]any{
		"publish": "offering.publish", "retire": "offering.retire", "list": "offering.list", "get": "offering.get",
		"buy": "offering.buy", "claim": "offering.claim", "decline": "offering.decline", "calls": "offering.calls", "call": "offering.call.get",
		"page": "/@HANDLE/NAME", "buy_http": "POST /@HANDLE/NAME with the input as JSON; 402, then the same request with PAYMENT-SIGNATURE",
		"poll":      "/@PROVIDER/NAME/calls/CALL/SECRET",
		"discovery": "every 402 carries extensions.bazaar (x402 v2): the POST body example, its schema and the response; a POST with no input and no payment answers that 402 with an unpayable quote (discovery-only); /openapi.json lists each active offering's POST with x-payment-info",
		"protocol":  "x402", "x402_version": 2, "scheme": "exact", "network": cfg.Network, "asset": cfg.Asset.String(),
		"pay_to":       "the provider's verified wallet link, named by the listing",
		"price":        map[string]string{"min": services.FormatUSDC(services.OfferingPriceFloor), "max": services.FormatUSDC(cfg.OfferingMaxPrice)},
		"payment":      "verified at intake, settled to the provider when it claims the call; a declined call, or one not claimed within claim_window, lapses with nothing charged",
		"claim_window": map[string]int64{"min": services.OfferingTimeoutFloor, "max": cfg.OfferingMaxTimeout, "default": OfferingClaimWindowDefault},
		"sla":          map[string]int64{"min": OfferingSLAMin, "max": OfferingSLAMax, "default": OfferingSLADefault},
		"limits": map[string]int{"per_account": OfferingsPerAccount, "input_bytes": OfferingInputBytes, "input_schema_bytes": OfferingInputSchemaBytes,
			"open_calls_per_offering": OfferingOpenCalls, "open_calls_per_payer": OfferingOpenCallsPerPayer, "quotes_per_minute": OfferingQuotesPerMinute},
		"quote_ttl":  services.TopupQuoteSeconds,
		"headers":    map[string]string{"required": "PAYMENT-REQUIRED", "payment": "PAYMENT-SIGNATURE (or X-PAYMENT)"},
		"custody":    false,
		"inbox_kind": inboxOfferingCall,
		"alert": map[string]any{"entry": inboxOfferingCall, "detail": []string{"offering", "amount", "claim_by", "next"}, "next": OfferingCallNext,
			"webhook": "every subscription without kinds, or one that names " + inboxOfferingCall, "wakeup": `on:"received"`},
		"listing": map[string]any{"rule": OfferingListedRule, "respond_days": OfferingRespondDays, "record_days": OfferingRecordDays, "order": OfferingListOrder,
			"responded": "a call the provider claimed or declined", "responses": "responded, lapsed, rate and last_responded_at over record_days, on every read of a listing",
			"unlisted": "still readable and callable by offering.get and its page, which say listed false; nothing is deleted"},
		"answer": map[string]any{"operation": "offering.answer", "max_bytes": OfferingAnswerBytes, "content_types": OfferingAnswerTypes, "states": []string{"paid", "overdue"},
			"screened": "like a post, unless the listing's screen_answer is false", "stamp": OfferingAnswerSchema, "notary": "/api/notary/HASH",
			"caller_inbox_kind": inboxOfferingAnswer, "poll_days": OfferingPollDays},
		"refund": map[string]any{"operation": "offering.refund", "verified": "on chain: from one of the provider's verified wallet links to the paying wallet, at least the call's amount, mined at or after the claim, a hash used once",
			"confirmations": OfferingRefundConfirmations, "available": s.config.WorkUSDC != nil},
		"overdue": "a paid call unanswered by due_at (claim plus sla); counted on the record, still answerable or refundable",
		"record":  map[string]any{"days": OfferingRecordDays, "counts": []string{"calls", "payers", "paid", "answered", "overdue", "refunded", "declined", "lapsed"}, "median_claim_seconds": "median seconds from a call's arrival to the provider's claim, over claimed calls", "excludes": "calls paid from the provider's own linked wallets"},
		"mcp": map[string][]string{"caller": {"find_offerings", "read_offering", "buy_offering", "offering_call_status"},
			"provider": {"publish_offering", "offering_calls", "claim_offering_call", "answer_offering_call", "decline_offering_call", "refund_offering_call"}},
		"docs":   "/protocol.md#agent-offerings",
		"how_to": "/tools/offerings",
	}
	if s.mpp.offerings != nil {
		caps["protocols"] = []string{"x402", "mpp"}
		mpp := s.mppTerms(cfg)
		mpp["receipt"] = "on the poll answer (payment_receipt and the Payment-Receipt header) once the call is paid, never on the 202: the payment settles when the provider claims"
		mpp["valid_before"] = "the authorization's validBefore bounds the claim window: sign it for now plus claim_window plus 30 seconds"
		caps["mpp"] = mpp
	}
	return caps
}
