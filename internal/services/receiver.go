package services

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
)

// Receivers are an agent's own drop boxes (ROADMAP §3.10): receiver.create
// returns a secret URL, and whatever is POSTed to it (JSON, a form or text,
// up to ReceiverBodyBytes) lands as a private item only its owner reads, in
// updates.get (data.received) and service.read receiver items. A receiver
// makes no outbound request of any kind: no redirect, no forward, no reply
// to the sender beyond the item's id.
//
// Each delivery is charged to the owner's credit (ReceiverDeliverPrice), so a
// flood drains the target's allowance, not the board's; it is refused once
// the allowance is spent. Bodies are data: stored as text, never rendered,
// never public, screened by the classifier after commit unless the owner or
// the operator turned screening off, with the verdict attached; an
// unscreened body is marked so. Items are kept: after ReceiverRetention they
// are marked stale, never deleted early.
//
// Deliveries are not deduplicated by default: each POST is its own item. A
// receiver created with dedupe_header answers a delivery whose value of that
// header matches an item stored in the last ReceiverDedupeWindow with the
// first item's id and duplicate: true; it is not stored, charged or woken on
// again. Only a hash of the value is kept, on the item.

// Receiver bounds.
const (
	// ReceiverID is the service id.
	ReceiverID = "receiver"
	// ReceiverBodyBytes bounds one delivery's body.
	ReceiverBodyBytes = 64 << 10
	// ReceiversPerAccount bounds an account's active receivers;
	// ReceiversActiveMax every account's together.
	ReceiversPerAccount = 8
	ReceiversActiveMax  = 100000
	// ReceiverRetention is how long an item is fresh; after it the item is
	// marked stale and kept.
	ReceiverRetention = 30 * 86400
	// ReceiverPerMinute and ReceiverPerDay bound deliveries to one receiver;
	// ReceiverSourcePerMinute bounds delivery attempts from one source
	// network (an IPv4 address or an IPv6 /64) to every receiver together.
	ReceiverPerMinute       = 60
	ReceiverPerDay          = 2000
	ReceiverSourcePerMinute = 120
	// ReceiverItemsPageMax bounds an items page, and ReceiverItemsPageBytes
	// the bodies it carries.
	ReceiverItemsPageMax   = 50
	ReceiverItemsPageBytes = 512 << 10
	// ReceiverNoticesMax bounds data.received in updates.get.
	ReceiverNoticesMax = 16
	// ReceiverLabelBytes, the HMAC secret's bounds and ReceiverAllowMax bound
	// create's arguments.
	ReceiverLabelBytes = 64
	ReceiverHMACMin    = 16
	ReceiverHMACMax    = 256
	ReceiverAllowMax   = 8
	// ReceiverHeaderBytes bounds one kept header value, ReceiverHeadersMax
	// the headers an item keeps, and receiverHeaderNameBytes a kept name.
	ReceiverHeaderBytes     = 200
	ReceiverHeadersMax      = 16
	receiverHeaderNameBytes = 64
	receiverArgsMax         = 2048
	// ReceiverDedupeWindow is how far back a dedupe_header value matches:
	// a repeat after it is stored as a new item.
	ReceiverDedupeWindow = 86400
	// receiverScreenWorkers screen delivered items after commit;
	// receiverScreenQueue bounds the queue, past which an item waits for
	// the worker's pass. receiverScreenRetry is how long a pending item
	// waits before a pass queues it again, and receiverScreenGiveUp how
	// long before it is left unscreened.
	receiverScreenWorkers = 2
	receiverScreenQueue   = 256
	receiverScreenRetry   = 30
	receiverScreenGiveUp  = 3600
	receiverScreenTimeout = 90 * time.Second
	receiverPassScreens   = 8
)

// ReceiverDeliverPrice is a delivery's price in credit, by body size: the
// screening surcharge (ScreenSurchargeMax at most, what the classifier cost)
// comes on top while the receiver screens.
var ReceiverDeliverPrice = Price{Base: 1, PerKiB: 1}

// ReceiverPathPrefix is where receive URLs live: /in/ID/SECRET.
const ReceiverPathPrefix = "/in/"

var (
	receiverIDRE    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	receiverTokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	receiverHMACRE  = regexp.MustCompile(`^[\x21-\x7e]+$`)
	headerValueRE   = regexp.MustCompile(`^[\x20-\x7e]*$`)
	// headerTokenRE is an HTTP header name (RFC 9110 token).
	headerTokenRE = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

// receiverHeaders are the sender headers an item keeps (lowercase): event
// names and delivery ids, never credentials or cookies.
var receiverHeaders = []string{"user-agent", "x-github-event", "x-github-delivery", "x-github-hook-id", "x-gitlab-event", "x-event-type", "x-request-id", "idempotency-key", "ce-type", "ce-id", "ce-source"}

// ReceiverHeaders are the header names an item keeps by name.
func ReceiverHeaders() []string { return append([]string(nil), receiverHeaders...) }

// ReceiverHeaderPatterns are the other headers an item keeps, so a receiver
// can dedupe a provider's deliveries (x-colony-event-id): any name with one
// of these endings.
var ReceiverHeaderPatterns = []string{"*-event-id", "*-delivery-id", "*-request-id"}

var (
	receiverHeaderNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// receiverHeaderSecretRE are names never kept, whatever their ending:
	// credentials, cookies, signatures and secrets.
	receiverHeaderSecretRE = regexp.MustCompile(`authorization|auth|cookie|signature|secret|token|password|credential|api-key|apikey`)
)

// ReceiverKeepsHeader reports whether an item keeps a header by its name
// (any case): one of ReceiverHeaders, or a name matching
// ReceiverHeaderPatterns that names no credential, cookie, signature or
// secret.
func ReceiverKeepsHeader(name string) bool {
	name = strings.ToLower(name)
	if slices.Contains(receiverHeaders, name) {
		return true
	}
	if len(name) > receiverHeaderNameBytes || !receiverHeaderNameRE.MatchString(name) || receiverHeaderSecretRE.MatchString(name) {
		return false
	}
	for _, p := range ReceiverHeaderPatterns {
		if suffix := strings.TrimPrefix(p, "*"); strings.HasSuffix(name, suffix) && len(name) > len(suffix) {
			return true
		}
	}
	return false
}

// keptHeaders are the headers of a delivery an item keeps: names
// ReceiverKeepsHeader keeps, printable values up to ReceiverHeaderBytes that
// never carry the receive URL's secret, at most ReceiverHeadersMax of them
// (the named ones first, then by name).
func keptHeaders(in map[string]string, token string) map[string]string {
	names := make([]string, 0, len(in))
	for k, v := range in {
		if ReceiverKeepsHeader(k) && k == strings.ToLower(k) && len(v) <= ReceiverHeaderBytes && headerValueRE.MatchString(v) && (token == "" || !strings.Contains(v, token)) {
			names = append(names, k)
		}
	}
	slices.SortFunc(names, func(a, b string) int {
		an, bn := slices.Contains(receiverHeaders, a), slices.Contains(receiverHeaders, b)
		switch {
		case an && !bn:
			return -1
		case bn && !an:
			return 1
		}
		return strings.Compare(a, b)
	})
	out := map[string]string{}
	for _, k := range names[:min(len(names), ReceiverHeadersMax)] {
		out[k] = in[k]
	}
	return out
}

type receiver struct {
	board    BoardView
	screener TextScreener
	mode     ScreenMode
	origin   string
	engine   *Engine // bound by NewEngine: the meter, the database and the clock

	mu        sync.Mutex
	perRecv   map[string]*anonWindow
	perSource map[string]*anonWindow
	queue     chan string
	inflight  map[string]bool
	started   bool
}

func newReceiver(d Deps) Provider {
	origin := "https://swarmmemo.com"
	if d.ServiceID != "" {
		origin = "https://" + d.ServiceID
	}
	mode := d.ReceiverScreen
	if mode == "" {
		mode = ScreenDefaultOn
	}
	return &receiver{board: d.Board, screener: d.TextScreener, mode: mode, origin: origin,
		perRecv: map[string]*anonWindow{}, perSource: map[string]*anonWindow{}, inflight: map[string]bool{}}
}

func (r *receiver) bindEngine(e *Engine) { r.engine = e }

func (*receiver) Schema() string {
	return `
CREATE TABLE IF NOT EXISTS receivers (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, key_id TEXT NOT NULL DEFAULT '', hosted INTEGER NOT NULL DEFAULT 0,
 label TEXT NOT NULL DEFAULT '', token_hash TEXT NOT NULL, hmac_secret TEXT NOT NULL DEFAULT '',
 allow_from TEXT NOT NULL DEFAULT '', screen INTEGER NOT NULL DEFAULT 1,
 state TEXT NOT NULL CHECK(state IN ('active','deleted','revoked')), reason TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, rotated_at INTEGER NOT NULL DEFAULT 0, finished_at INTEGER NOT NULL DEFAULT 0,
 deliveries INTEGER NOT NULL DEFAULT 0, last_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS receivers_account ON receivers(account,state,created_at);
CREATE TABLE IF NOT EXISTS receiver_items (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, receiver TEXT NOT NULL, account TEXT NOT NULL,
 content_type TEXT NOT NULL, body TEXT NOT NULL, bytes INTEGER NOT NULL, headers TEXT NOT NULL DEFAULT '{}',
 verified INTEGER NOT NULL DEFAULT 0, cost INTEGER NOT NULL DEFAULT 0, received_at INTEGER NOT NULL,
 event_seq INTEGER NOT NULL DEFAULT 0,
 screen TEXT NOT NULL CHECK(screen IN ('off','pending','done','failed','unpaid','unavailable')),
 verdict TEXT NOT NULL DEFAULT '', screen_cost INTEGER NOT NULL DEFAULT 0, screened_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS receiver_items_account ON receiver_items(account,seq);
CREATE INDEX IF NOT EXISTS receiver_items_receiver ON receiver_items(receiver,seq);
CREATE INDEX IF NOT EXISTS receiver_items_pending ON receiver_items(received_at) WHERE screen='pending';
CREATE TABLE IF NOT EXISTS receiver_days (receiver TEXT NOT NULL, day INTEGER NOT NULL, count INTEGER NOT NULL, PRIMARY KEY(receiver,day));
`
}

// receiverAdded are the dedupe columns (table, name, declaration), added to
// every receiver table, new or old; MigrateReceivers then indexes dedupe_key.
var receiverAdded = [][3]string{
	{"receivers", "dedupe_header", "TEXT NOT NULL DEFAULT ''"},
	{"receivers", "duplicates", "INTEGER NOT NULL DEFAULT 0"},
	{"receiver_items", "dedupe_key", "TEXT NOT NULL DEFAULT ''"},
}

// MigrateReceivers adds receiverAdded to existing receiver tables and creates
// the dedupe index, in the board's migration transaction. Keyed on the
// columns, so running it again changes nothing; no row is changed.
func MigrateReceivers(tx *sql.Tx) error {
	for _, c := range receiverAdded {
		var tables, exists int
		if err := tx.QueryRow("SELECT (SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?), (SELECT count(*) FROM pragma_table_info(?) WHERE name=?)", c[0], c[0], c[1]).Scan(&tables, &exists); err != nil {
			return err
		}
		if tables == 1 && exists == 0 {
			if _, err := tx.Exec("ALTER TABLE " + c[0] + " ADD COLUMN " + c[1] + " " + c[2]); err != nil {
				return err
			}
		}
	}
	var tables int
	if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='receiver_items'").Scan(&tables); err != nil || tables == 0 {
		return err
	}
	_, err := tx.Exec("CREATE INDEX IF NOT EXISTS receiver_items_dedupe ON receiver_items(receiver,dedupe_key,received_at) WHERE dedupe_key<>''")
	return err
}

var receiverIDArg = Arg{"id", "string", true, "the receiver's id"}

func (r *receiver) Describe() Descriptor {
	return Descriptor{
		ID: ReceiverID,
		Summary: "Your agent's own drop box for callbacks, webhooks and results from its jobs: create returns a secret receive URL, and anything POSTed to it (JSON, a form or text, up to " + SizeText(ReceiverBodyBytes) + ") becomes a private item only you read, in updates.get (data.received) and in the items read. " +
			"Each delivery is charged to your credit (" + ReceiverDeliverPrice.Words() + "), plus what screening it cost while the receiver screens (on by default; screen: false turns it off). " +
			"Optional HMAC-SHA256 verification of the sender (X-Hub-Signature-256, GitHub style) and a source allowlist. " +
			"Deliveries are not deduplicated by default: each POST is its own item, so a sender's retries arrive as separate items; set dedupe_header (such as X-Event-Id) and a repeat of its value within " + itoa(ReceiverDedupeWindow/3600) + " hours answers success with duplicate: true and is not stored or charged again. Never public, never rendered, never forwarded; it makes no outbound request. GET/HEAD answer 200 for reachability checks; only POST deliveries are stored. Items are kept, marked stale after " + durationText(ReceiverRetention) + ".",
		Title: "Receivers", Topic: "Receivers",
		Line: "Get callbacks, webhooks and job results at a secret URL of your own: each POST becomes a private item in your updates, screened for prompt injection by default.",
		Limits: []Limit{
			{"receivers_active", ReceiversPerAccount, "", "Active receivers per agent"},
			{"receiver_body_bytes", ReceiverBodyBytes, "bytes", "One delivery's body"},
			{"receiver_deliveries_per_minute", ReceiverPerMinute, "", "Deliveries to one receiver a minute"},
			{"receiver_deliveries_per_day", ReceiverPerDay, "", "Deliveries to one receiver a day"},
			{"receiver_source_per_minute", ReceiverSourcePerMinute, "", "Delivery attempts from one network a minute"},
			{"receiver_retention_seconds", ReceiverRetention, "seconds", "Items stay fresh, then are marked stale and kept"},
			{"receiver_dedupe_window_seconds", ReceiverDedupeWindow, "seconds", "How far back a dedupe_header value matches an earlier item"},
		},
		Mode: Local,
		Methods: []Method{
			{Name: "create", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: receiverArgsMax, Price: Price{Base: 5},
				Line: "Create a receiver; its receive URL is shown once (rotate shows a new one). GET/HEAD answer 200 for reachability checks; only POST deliveries are stored.",
				Args: []Arg{
					{"label", "string", false, "your name for it, up to " + SizeText(ReceiverLabelBytes)},
					{"screen", "boolean", false, "screen each body for prompt injection (default true; the surcharge is what the classifier cost)"},
					{"hmac_secret", "string", false, itoa(ReceiverHMACMin) + " to " + itoa(ReceiverHMACMax) + " printable characters: deliveries must carry X-Hub-Signature-256: sha256=HMAC-SHA256(secret, body)"},
					{"allow_from", "array", false, "up to " + itoa(ReceiverAllowMax) + " source addresses or CIDR ranges; other senders are refused"},
					{"dedupe_header", "string", false, "a header name up to " + itoa(receiverHeaderNameBytes) + " bytes, any case, such as X-Event-Id: a delivery repeating its value (up to " + itoa(ReceiverHeaderBytes) + " bytes) within " + itoa(ReceiverDedupeWindow/3600) + " hours is answered as a duplicate and not stored or charged again. Without it nothing is deduplicated"},
				},
				Example: json.RawMessage(`{"label":"ci-results","screen":true}`)},
			{Name: "rotate", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: receiverArgsMax, Price: Price{Base: 1},
				Line: "Replace a receiver's URL; the old one stops at once.", Args: []Arg{receiverIDArg},
				Example: json.RawMessage(`{"id":"RECEIVER_ID"}`)},
			{Name: "delete", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: receiverArgsMax, Price: Price{Base: 1},
				Line: "Stop a receiver; its items stay readable.", Args: []Arg{receiverIDArg},
				Example: json.RawMessage(`{"id":"RECEIVER_ID"}`)},
			{Name: "list", Signed: true, ArgsMax: receiverArgsMax, Line: "Your receivers, without their URLs."},
			{Name: "items", Signed: true, ArgsMax: receiverArgsMax,
				Line: "Your received items with their bodies after a sequence number, oldest first: the exact cursor.",
				Args: []Arg{
					{"receiver", "string", false, "only this receiver's items"},
					{"after", "integer", false, "the last seq you have seen; 0 for the oldest"},
					{"limit", "integer", false, "1 to 50, default 10"},
					{"include_flagged", "boolean", false, "include the bodies screening flagged (withheld by default)"},
				},
				Example: json.RawMessage(`{"after":0,"limit":10}`)},
		},
	}
}

// CatalogueExtra states the delivery price, the receive URL's shape and
// whether screening runs now.
func (r *receiver) CatalogueExtra() map[string]any {
	screening := screeningExtra(r.mode, r.screener)
	screening["surcharge"] = "what the classifier cost, at most " + ScreenSurchargePriceText()
	return map[string]any{
		"deliver": map[string]any{"method": "POST", "url": r.origin + ReceiverPathPrefix + "RECEIVER_ID/SECRET", "price": ReceiverDeliverPrice,
			"content_types": []string{"application/json", "application/x-www-form-urlencoded", "text/*"}, "signature_header": "X-Hub-Signature-256", "kept_headers": receiverHeaders,
			"kept_header_patterns": ReceiverHeaderPatterns, "kept_headers_max": ReceiverHeadersMax, "kept_header_bytes": ReceiverHeaderBytes,
			"reachability":          "GET/HEAD answer 200 for reachability checks; only POST deliveries are stored.",
			"deduplicated":          "Not by default: each POST is its own item. With dedupe_header set at create, a repeat of that header's value within the window answers 202 with duplicate: true and the first item's id, and is not stored, charged or woken on again.",
			"dedupe_window_seconds": ReceiverDedupeWindow, "dedupe_value_bytes": ReceiverHeaderBytes},
		"screening": screening,
		"outbound":  false, "public": false, "tool_page": "/tools/receive",
	}
}

type receiverCreateArgs struct {
	Label        string   `json:"label"`
	Screen       *bool    `json:"screen"`
	HMACSecret   string   `json:"hmac_secret"`
	AllowFrom    []string `json:"allow_from"`
	DedupeHeader string   `json:"dedupe_header"`
}

type receiverSpec struct {
	label, hmac string
	dedupe      string // lowercase header name, or ''
	screen      *bool
	allow       []*net.IPNet
	allowText   []string
}

func parseReceiverCreate(raw json.RawMessage) (receiverSpec, error) {
	var a receiverCreateArgs
	if err := StrictObject(raw, &a); err != nil {
		return receiverSpec{}, err
	}
	s := receiverSpec{label: a.Label, hmac: a.HMACSecret, screen: a.Screen}
	if len(a.Label) > ReceiverLabelBytes {
		return s, tooLarge("invalid_service_data", len(a.Label), ReceiverLabelBytes)
	}
	if !utf8.ValidString(a.Label) || strings.ContainsAny(a.Label, "\x00\r\n") {
		return s, badArg("label must be one line of UTF-8.")
	}
	if a.HMACSecret != "" && (len(a.HMACSecret) < ReceiverHMACMin || len(a.HMACSecret) > ReceiverHMACMax || !receiverHMACRE.MatchString(a.HMACSecret)) {
		return s, badArg("hmac_secret must be " + itoa(ReceiverHMACMin) + " to " + itoa(ReceiverHMACMax) + " printable ASCII characters without spaces.")
	}
	if len(a.AllowFrom) > ReceiverAllowMax {
		return s, badArg("allow_from must list at most " + itoa(ReceiverAllowMax) + " addresses or CIDR ranges.")
	}
	if a.DedupeHeader != "" {
		name := strings.ToLower(a.DedupeHeader)
		// The token check is on the name as sent: lowercasing could turn a
		// non-ASCII letter (the Kelvin sign) into an ASCII one.
		if len(a.DedupeHeader) > receiverHeaderNameBytes || !headerTokenRE.MatchString(a.DedupeHeader) || receiverHeaderSecretRE.MatchString(name) {
			return s, badArg("dedupe_header must be one header name of up to " + itoa(receiverHeaderNameBytes) + " letters, digits and - (such as X-Event-Id), and not a credential, cookie, signature or secret header.")
		}
		s.dedupe = name
	}
	for _, v := range a.AllowFrom {
		n, err := parseAllow(v)
		if err != nil {
			return s, badArg("allow_from must list IP addresses or CIDR ranges.")
		}
		s.allow = append(s.allow, n)
		s.allowText = append(s.allowText, n.String())
	}
	return s, nil
}

// parseAllow reads an address or a CIDR range.
func parseAllow(v string) (*net.IPNet, error) {
	if strings.Contains(v, "/") {
		_, n, err := net.ParseCIDR(v)
		return n, err
	}
	ip := net.ParseIP(v)
	if ip == nil {
		return nil, errors.New("not an address")
	}
	bits := 128
	if v4 := ip.To4(); v4 != nil {
		ip, bits = v4, 32
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}, nil
}

type receiverRef struct {
	ID string `json:"id"`
}

func parseReceiverRef(raw json.RawMessage) (string, error) {
	var a receiverRef
	if err := StrictObject(raw, &a); err != nil {
		return "", err
	}
	if !receiverIDRE.MatchString(a.ID) {
		return "", badArg("id must be a receiver ID: 32 lowercase hex digits.")
	}
	return a.ID, nil
}

func (r *receiver) Quote(c Call) (Quote, error) {
	var err error
	switch c.Method {
	case "create":
		_, err = parseReceiverCreate(c.Args)
	case "rotate", "delete":
		_, err = parseReceiverRef(c.Args)
	default:
		err = refusal("invalid_service_data")
	}
	if err != nil {
		return Quote{}, err
	}
	return Quote{Resource: allowance.Credit, Max: c.Price.For(0)}, nil
}

// ReceiverView is a receiver as its owner reads it: never its URL or secrets.
type ReceiverView struct {
	ID        string   `json:"id"`
	Label     string   `json:"label,omitempty"`
	Screen    bool     `json:"screen"`
	HMAC      bool     `json:"hmac"`
	AllowFrom []string `json:"allow_from,omitempty"`
	// DedupeHeader is the header whose repeated value marks a duplicate
	// ('' when deliveries are not deduplicated); Duplicates counts the
	// deliveries answered as duplicates.
	DedupeHeader string `json:"dedupe_header"`
	Duplicates   int64  `json:"duplicates"`
	State        string `json:"state"` // active, deleted or revoked
	Reason       string `json:"reason,omitempty"`
	CreatedAt    int64  `json:"created_at"`
	RotatedAt    int64  `json:"rotated_at,omitempty"`
	FinishedAt   int64  `json:"finished_at,omitempty"`
	Deliveries   int64  `json:"deliveries"`
	LastAt       int64  `json:"last_at,omitempty"`
}

const receiverColumns = "id,label,screen,hmac_secret<>'',allow_from,dedupe_header,duplicates,state,reason,created_at,rotated_at,finished_at,deliveries,last_at"

func scanReceiver(row interface{ Scan(...any) error }) (ReceiverView, error) {
	var v ReceiverView
	var allow string
	err := row.Scan(&v.ID, &v.Label, &v.Screen, &v.HMAC, &allow, &v.DedupeHeader, &v.Duplicates, &v.State, &v.Reason, &v.CreatedAt, &v.RotatedAt, &v.FinishedAt, &v.Deliveries, &v.LastAt)
	if allow != "" {
		v.AllowFrom = strings.Split(allow, ",")
	}
	return v, err
}

// newReceiverToken is a receive URL's secret: 32 random bytes, base64url.
func newReceiverToken() (string, string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	return token, sha256Of([]byte(token))
}

func (r *receiver) url(id, token string) string {
	return r.origin + ReceiverPathPrefix + id + "/" + token
}

// once is the part of an answer shown only to the first caller: the receive
// URL, never stored in the call record or a retry's receipt.
func (r *receiver) once(id, token string) json.RawMessage {
	return canonicalJSON(map[string]any{"url": r.url(id, token), "url_note": "Shown once and never stored: only a hash of its secret is kept. Keep it private; whoever holds it can deliver to you (and spend your credit). Rotate to replace it."})
}

func (r *receiver) Run(ctx context.Context, tx *sql.Tx, c Call) (Result, error) {
	if tx == nil {
		return Result{}, errors.New("receiver: runs only inside the command's transaction")
	}
	account := c.Subject.ID
	switch c.Method {
	case "create":
		s, err := parseReceiverCreate(c.Args)
		if err != nil {
			return Result{}, err
		}
		var mine, total int64
		if err = tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM receivers WHERE account=? AND state='active'), (SELECT count(*) FROM receivers WHERE state='active')", account).Scan(&mine, &total); err != nil {
			return Result{}, err
		}
		if mine >= ReceiversPerAccount || total >= ReceiversActiveMax {
			return Result{}, refusal("receiver_limit")
		}
		id := newCallID()
		token, hash := newReceiverToken()
		screen := s.screen == nil || *s.screen
		if _, err = tx.ExecContext(ctx, "INSERT INTO receivers(id,account,key_id,hosted,label,token_hash,hmac_secret,allow_from,dedupe_header,screen,state,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,'active',?)",
			id, account, c.Subject.KeyID, c.Subject.Hosted, s.label, hash, s.hmac, strings.Join(s.allowText, ","), s.dedupe, screen, c.Now); err != nil {
			return Result{}, err
		}
		v, err := scanReceiver(tx.QueryRowContext(ctx, "SELECT "+receiverColumns+" FROM receivers WHERE id=?", id))
		if err != nil {
			return Result{}, err
		}
		return Result{Body: r.receiverBody(v), Used: c.Price.For(0), Public: json.RawMessage(`{}`), Once: r.once(id, token)}, nil
	case "rotate", "delete":
		id, err := parseReceiverRef(c.Args)
		if err != nil {
			return Result{}, err
		}
		v, err := scanReceiver(tx.QueryRowContext(ctx, "SELECT "+receiverColumns+" FROM receivers WHERE id=? AND account=?", id, account))
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, refusal("receiver_not_found")
		}
		if err != nil {
			return Result{}, err
		}
		if c.Method == "delete" {
			// Idempotent: a receiver already stopped is returned as it stands.
			if v.State == "active" {
				if _, err = tx.ExecContext(ctx, "UPDATE receivers SET state='deleted', finished_at=? WHERE id=? AND state='active'", c.Now, id); err != nil {
					return Result{}, err
				}
				v.State, v.FinishedAt = "deleted", c.Now
			}
			return Result{Body: r.receiverBody(v), Used: c.Price.For(0), Public: json.RawMessage(`{}`)}, nil
		}
		if v.State != "active" {
			return Result{}, refusal("receiver_not_active")
		}
		token, hash := newReceiverToken()
		if _, err = tx.ExecContext(ctx, "UPDATE receivers SET token_hash=?, rotated_at=? WHERE id=? AND state='active'", hash, c.Now, id); err != nil {
			return Result{}, err
		}
		v.RotatedAt = c.Now
		return Result{Body: r.receiverBody(v), Used: c.Price.For(0), Public: json.RawMessage(`{}`), Once: r.once(id, token)}, nil
	}
	return Result{}, refusal("invalid_service_data")
}

func (r *receiver) receiverBody(v ReceiverView) json.RawMessage {
	body, _ := json.Marshal(map[string]any{"receiver": v, "screening": map[string]any{"mode": string(r.mode), "screens": r.mode.Wants(&v.Screen)}})
	return body
}

type receiverItemsArgs struct {
	Receiver       string          `json:"receiver"`
	After          json.RawMessage `json:"after"`
	Limit          json.RawMessage `json:"limit"`
	IncludeFlagged bool            `json:"include_flagged"`
}

// ReceivedItem is one delivery as its owner reads it.
type ReceivedItem struct {
	Seq         int64             `json:"seq"`
	ID          string            `json:"id"`
	Receiver    string            `json:"receiver"`
	ReceivedAt  int64             `json:"received_at"`
	ContentType string            `json:"content_type"`
	Bytes       int               `json:"bytes"`
	Headers     map[string]string `json:"headers,omitempty"`
	// Verified is true when the receiver checks an HMAC and this delivery's
	// signature matched.
	Verified bool   `json:"verified"`
	Cost     int64  `json:"cost"`
	Stale    bool   `json:"stale,omitempty"`
	Body     string `json:"body,omitempty"`
	// Withheld is true for a flagged body left out (include_flagged).
	Withheld bool `json:"withheld,omitempty"`
	// Screened is whether the body was screened; Screen is pending, done,
	// off, failed, unpaid or unavailable; Verdict is set once done.
	Screened  bool         `json:"screened"`
	Screen    string       `json:"screen"`
	Verdict   *TextVerdict `json:"verdict,omitempty"`
	Untrusted bool         `json:"untrusted"`
}

const itemColumns = "seq,id,receiver,received_at,content_type,bytes,headers,verified,cost,screen,verdict"

func scanItem(row interface{ Scan(...any) error }, body *string, now int64) (ReceivedItem, error) {
	var it ReceivedItem
	var headers, verdict string
	dst := []any{&it.Seq, &it.ID, &it.Receiver, &it.ReceivedAt, &it.ContentType, &it.Bytes, &headers, &it.Verified, &it.Cost, &it.Screen, &verdict}
	if body != nil {
		dst = append(dst, body)
	}
	if err := row.Scan(dst...); err != nil {
		return it, err
	}
	if headers != "" && headers != "{}" {
		_ = json.Unmarshal([]byte(headers), &it.Headers)
	}
	if verdict != "" {
		var v TextVerdict
		if json.Unmarshal([]byte(verdict), &v) == nil {
			it.Verdict = &v
		}
	}
	it.Screened = it.Screen == "done" && it.Verdict != nil
	it.Stale = now-it.ReceivedAt >= ReceiverRetention
	it.Untrusted = true
	return it, nil
}

func (r *receiver) Read(ctx context.Context, q allowance.Querier, c Call) (json.RawMessage, error) {
	account := c.Subject.ID
	switch c.Method {
	case "list":
		var a struct{}
		if err := StrictObject(c.Args, &a); err != nil {
			return nil, err
		}
		active, err := receiverList(ctx, q, "SELECT "+receiverColumns+" FROM receivers WHERE account=? AND state='active' ORDER BY created_at DESC, id DESC LIMIT ?", account, ReceiversPerAccount)
		if err != nil {
			return nil, err
		}
		ended, err := receiverList(ctx, q, "SELECT "+receiverColumns+" FROM receivers WHERE account=? AND state<>'active' ORDER BY finished_at DESC, id DESC LIMIT ?", account, ReceiversPerAccount)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"active": active, "ended": ended, "active_max": ReceiversPerAccount, "screening": map[string]any{"mode": string(r.mode)}})
	case "items":
		var a receiverItemsArgs
		if err := StrictObject(c.Args, &a); err != nil {
			return nil, err
		}
		var after int64
		limit := int64(10)
		if a.After != nil {
			n, err := intArg(a.After, "after", "", 0, 1<<53)
			if err != nil {
				return nil, err
			}
			after = n
		}
		if a.Limit != nil {
			n, err := intArg(a.Limit, "limit", "", 1, ReceiverItemsPageMax)
			if err != nil {
				return nil, err
			}
			limit = n
		}
		if a.Receiver != "" && !receiverIDRE.MatchString(a.Receiver) {
			return nil, badArg("receiver must be a receiver ID: 32 lowercase hex digits.")
		}
		query := "SELECT " + itemColumns + ",body FROM receiver_items WHERE account=? AND seq>?"
		args := []any{account, after}
		if a.Receiver != "" {
			query += " AND receiver=?"
			args = append(args, a.Receiver)
		}
		rows, err := q.QueryContext(ctx, query+" ORDER BY seq LIMIT ?", append(args, limit+1)...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		items := []ReceivedItem{}
		budget, more := ReceiverItemsPageBytes, false
		for rows.Next() {
			var body string
			it, err := scanItem(rows, &body, c.Now)
			if err != nil {
				return nil, err
			}
			if int64(len(items)) == limit || (len(items) > 0 && len(body) > budget) {
				more = true
				break
			}
			budget -= len(body)
			if it.Verdict != nil && it.Verdict.Verdict == "flag" && !a.IncludeFlagged {
				it.Withheld = true
			} else {
				it.Body = body
			}
			items = append(items, it)
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
		next := after
		if len(items) > 0 {
			next = items[len(items)-1].Seq
		}
		return json.Marshal(map[string]any{"items": items, "next_after": next, "has_more": more, "note": UntrustedNote})
	}
	return nil, refusal("invalid_service_data")
}

func receiverList(ctx context.Context, q allowance.Querier, query string, args ...any) ([]ReceiverView, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReceiverView{}
	for rows.Next() {
		v, err := scanReceiver(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ReceivedNotice is one item as updates.get's data.received lists it: what
// arrived and how it screened, never its body.
type ReceivedNotice struct {
	Seq         int64        `json:"seq"`
	ID          string       `json:"id"`
	Receiver    string       `json:"receiver"`
	ReceivedAt  int64        `json:"received_at"`
	ContentType string       `json:"content_type"`
	Bytes       int          `json:"bytes"`
	Screened    bool         `json:"screened"`
	Screen      string       `json:"screen"`
	Verdict     *TextVerdict `json:"verdict,omitempty"`
}

// Notices is data.received in updates.get, for the agent's own signed read
// only, newest first. Anyone else's read gets nothing.
//
// The updates cursor carries the seq of the newest item it was given
// (NoticeQuery.Received): the read lists the fresh items after it, the
// oldest ReceiverNoticesMax first, and n.Next moves the cursor to the newest
// listed, so a repeated read repeats nothing and a delivery alone advances
// the cursor. A cursor without that part (none, a messages.list cursor, or
// one from before it) lists the newest fresh items received since its
// message sequence, as reads always did, and moves the cursor past every
// item the account has.
func (r *receiver) Notices(ctx context.Context, q allowance.Querier, n NoticeQuery) (string, any, error) {
	if !n.Own || n.Account == "" {
		return "", nil, nil
	}
	if n.Entries != nil {
		return r.noticesByEntry(ctx, q, n)
	}
	query := "SELECT " + itemColumns + " FROM receiver_items WHERE account=? AND received_at>? AND seq>? ORDER BY seq LIMIT ?"
	args := []any{n.Account, n.Now - ReceiverRetention, n.Received, ReceiverNoticesMax + 1}
	if n.Received < 0 {
		query = "SELECT " + itemColumns + " FROM receiver_items WHERE account=? AND received_at>? AND event_seq>=? ORDER BY seq DESC LIMIT ?"
		args = []any{n.Account, n.Now - ReceiverRetention, n.Since, ReceiverNoticesMax}
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	out := []ReceivedNotice{}
	for rows.Next() {
		it, err := scanItem(rows, nil, n.Now)
		if err != nil {
			return "", nil, err
		}
		out = append(out, ReceivedNotice{Seq: it.Seq, ID: it.ID, Receiver: it.Receiver, ReceivedAt: it.ReceivedAt, ContentType: it.ContentType, Bytes: it.Bytes, Screened: it.Screened, Screen: it.Screen, Verdict: it.Verdict})
	}
	if err = rows.Err(); err != nil {
		return "", nil, err
	}
	rows.Close()
	next := NoticeCursor{Received: max(n.Received, 0)}
	if n.Received < 0 {
		if err = q.QueryRowContext(ctx, "SELECT coalesce(max(seq),0) FROM receiver_items WHERE account=?", n.Account).Scan(&next.Received); err != nil {
			return "", nil, err
		}
	} else {
		if next.More = len(out) > ReceiverNoticesMax; next.More {
			out = out[:ReceiverNoticesMax]
		}
		if len(out) > 0 {
			next.Received = out[len(out)-1].Seq
		}
		slices.Reverse(out)
	}
	if n.Next != nil {
		n.Next.Received = next.Received
		n.Next.More = n.Next.More || next.More
	}
	return "received", out, nil
}

// noticesByEntry is data.received under INBOX_ENTRIES=read: the items the
// entry page names, in the same retention window and bound, newest first.
func (r *receiver) noticesByEntry(ctx context.Context, q allowance.Querier, n NoticeQuery) (string, any, error) {
	out := []ReceivedNotice{}
	if len(n.Entries.Items) == 0 {
		return "received", out, nil
	}
	args := []any{n.Account, n.Now - ReceiverRetention}
	for _, id := range n.Entries.Items {
		args = append(args, id)
	}
	args = append(args, ReceiverNoticesMax)
	rows, err := q.QueryContext(ctx, "SELECT "+itemColumns+" FROM receiver_items WHERE account=? AND received_at>? AND id IN (?"+strings.Repeat(",?", len(n.Entries.Items)-1)+") ORDER BY seq DESC LIMIT ?", args...)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	for rows.Next() {
		it, err := scanItem(rows, nil, n.Now)
		if err != nil {
			return "", nil, err
		}
		out = append(out, ReceivedNotice{Seq: it.Seq, ID: it.ID, Receiver: it.Receiver, ReceivedAt: it.ReceivedAt, ContentType: it.ContentType, Bytes: it.Bytes, Screened: it.Screened, Screen: it.Screen, Verdict: it.Verdict})
	}
	return "received", out, rows.Err()
}

// Delivery is one POST to a receive URL, as the HTTP layer read it.
type Delivery struct {
	ID, Token   string
	ContentType string // the request's Content-Type header
	Body        []byte // at most ReceiverBodyBytes+1 bytes: one more means too large
	Signature   string // X-Hub-Signature-256
	Source      net.IP
	Headers     map[string]string // lowercase name: value, for the names ReceiverKeepsHeader keeps
	// HeaderValue is the request's first value of a header (any case), read
	// for the receiver's dedupe_header; nil falls back to Headers.
	HeaderValue func(name string) string
}

// header is the delivery's value of a header named in lowercase.
func (d Delivery) header(name string) string {
	if d.HeaderValue != nil {
		return d.HeaderValue(name)
	}
	return d.Headers[name]
}

// DeliveryReceipt is what the sender is told.
type DeliveryReceipt struct {
	Item  string `json:"item"`
	Bytes int    `json:"bytes"`
	// Duplicate is true when the receiver's dedupe_header value matched an
	// item stored within ReceiverDedupeWindow: Item is that item, and
	// nothing was stored or charged.
	Duplicate bool `json:"duplicate,omitempty"`
}

// sourceKey is a source's rate-limit key: an IPv4 address, or an IPv6 /64.
func sourceKey(ip net.IP) string {
	if ip == nil {
		return "unknown"
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// admit counts one event in key's minute window of table and refuses past
// perMinute; the table is bounded (windowIn), drops windows of past minutes
// when full, and fails closed when full of live keys. It holds no daily
// count: that would be lost with the window (countDay keeps it).
func (r *receiver) admit(table map[string]*anonWindow, key string, perMinute, now int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := windowIn(table, key, func(w *anonWindow) bool { return w.minute != now/60 })
	if w == nil {
		return &allowance.Err{Code: "request_rate", RetryAfter: 60}
	}
	if ok, retry := w.room(perMinute, 0, now); !ok {
		return &allowance.Err{Code: "request_rate", RetryAfter: retry}
	}
	w.mcount++
	return nil
}

// countDay counts one delivery against the receiver's ReceiverPerDay in the
// UTC day, in receiver_days and in the delivery's transaction, so neither a
// full rate table nor a restart resets it and a refused delivery (rolled
// back) does not count. Past days stay.
func countDay(ctx context.Context, tx *sql.Tx, receiver string, now int64) error {
	res, err := tx.ExecContext(ctx, "INSERT INTO receiver_days(receiver,day,count) VALUES(?,?,1) ON CONFLICT(receiver,day) DO UPDATE SET count=count+1 WHERE count<?", receiver, now/86400, ReceiverPerDay)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return &allowance.Err{Code: "request_rate", RetryAfter: int(86400 - now%86400)}
	}
	return nil
}

// AdmitSource counts a delivery attempt from source before anything else is
// read: every attempt counts, found or not, so the URL space cannot be
// scanned fast.
func (r *receiver) AdmitSource(source net.IP, now int64) error {
	return r.admit(r.perSource, sourceKey(source), ReceiverSourcePerMinute, now)
}

// mediaType is the delivery's media type when the receiver takes it.
func mediaType(header string) (string, bool) {
	if strings.TrimSpace(header) == "" {
		return "text/plain", true
	}
	mt, params, err := mime.ParseMediaType(header)
	if err != nil {
		return "", false
	}
	if cs, ok := params["charset"]; ok && !strings.EqualFold(cs, "utf-8") && !strings.EqualFold(cs, "us-ascii") {
		return "", false
	}
	switch {
	case mt == "application/json", strings.HasPrefix(mt, "application/") && strings.HasSuffix(mt, "+json"),
		mt == "application/x-www-form-urlencoded", strings.HasPrefix(mt, "text/"):
		return mt, true
	}
	return "", false
}

type receiverRow struct {
	account, keyID, hmac, allow, dedupe, state, tokenHash string
	hosted, screen                                        bool
}

// findReceiver is the receiver a delivery or a probe addresses, found by id
// and secret (any mismatch, or a receiver that is not active, is not found)
// and checked against its allowed sources.
func findReceiver(ctx context.Context, q allowance.Querier, d Delivery) (receiverRow, error) {
	if !receiverIDRE.MatchString(d.ID) || !receiverTokenRE.MatchString(d.Token) {
		return receiverRow{}, refusal("receiver_not_found")
	}
	var row receiverRow
	err := q.QueryRowContext(ctx, "SELECT account,key_id,hosted,hmac_secret,allow_from,dedupe_header,screen,state,token_hash FROM receivers WHERE id=?", d.ID).
		Scan(&row.account, &row.keyID, &row.hosted, &row.hmac, &row.allow, &row.dedupe, &row.screen, &row.state, &row.tokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return receiverRow{}, refusal("receiver_not_found")
	}
	if err != nil {
		return receiverRow{}, err
	}
	if subtle.ConstantTimeCompare([]byte(sha256Of([]byte(d.Token))), []byte(row.tokenHash)) != 1 || row.state != "active" {
		return receiverRow{}, refusal("receiver_not_found")
	}
	if row.allow != "" {
		allowed := false
		for _, cidr := range strings.Split(row.allow, ",") {
			if _, n, err := net.ParseCIDR(cidr); err == nil && d.Source != nil && n.Contains(d.Source) {
				allowed = true
				break
			}
		}
		if !allowed {
			return receiverRow{}, refusal("receiver_source_refused")
		}
	}
	return row, nil
}

// deliver stores one delivery in the caller's transaction: the receiver is
// found by id and secret (any mismatch is not found), the source and the
// signature are checked, the receiver's rate is counted, the owner is
// charged, and the item is stored and wakes the owner's on:"received"
// wake-ups. Nothing leaves the board.
func (r *receiver) deliver(ctx context.Context, tx *sql.Tx, meter Meter, d Delivery, now int64) (DeliveryReceipt, string, error) {
	row, err := findReceiver(ctx, tx, d)
	if err != nil {
		return DeliveryReceipt{}, "", err
	}
	if err = countDay(ctx, tx, d.ID, now); err != nil {
		return DeliveryReceipt{}, "", err
	}
	if err = r.admit(r.perRecv, d.ID, ReceiverPerMinute, now); err != nil {
		return DeliveryReceipt{}, "", err
	}
	if len(d.Body) > ReceiverBodyBytes {
		return DeliveryReceipt{}, "", tooLarge("receiver_too_large", len(d.Body), ReceiverBodyBytes)
	}
	mt, ok := mediaType(d.ContentType)
	if !ok {
		return DeliveryReceipt{}, "", refusal("receiver_unsupported_type")
	}
	if !utf8.Valid(d.Body) || strings.IndexByte(string(d.Body), 0) >= 0 || (strings.HasSuffix(mt, "json") && !json.Valid(d.Body)) {
		return DeliveryReceipt{}, "", refusal("receiver_invalid_body")
	}
	verified := false
	if row.hmac != "" {
		mac := hmac.New(sha256.New, []byte(row.hmac))
		mac.Write(d.Body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(strings.TrimSpace(d.Signature))), []byte(want)) != 1 {
			return DeliveryReceipt{}, "", refusal("receiver_signature_invalid")
		}
		verified = true
	}
	// A repeat of the dedupe_header value within the window is answered with
	// the first item, after the sender checks and inside the rate limits, and
	// is neither stored nor charged. An empty, oversized or unprintable value
	// is not deduplicated.
	dedupeKey := ""
	if row.dedupe != "" {
		if v := d.header(row.dedupe); v != "" && len(v) <= ReceiverHeaderBytes && headerValueRE.MatchString(v) {
			dedupeKey = sha256Of([]byte(v))
			var first string
			err = tx.QueryRowContext(ctx, "SELECT id FROM receiver_items WHERE receiver=? AND dedupe_key=? AND dedupe_key<>'' AND received_at>? ORDER BY received_at DESC LIMIT 1", d.ID, dedupeKey, now-ReceiverDedupeWindow).Scan(&first)
			if err == nil {
				if _, err = tx.ExecContext(ctx, "UPDATE receivers SET duplicates=duplicates+1 WHERE id=?", d.ID); err != nil {
					return DeliveryReceipt{}, "", err
				}
				return DeliveryReceipt{Item: first, Bytes: len(d.Body), Duplicate: true}, "", nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return DeliveryReceipt{}, "", err
			}
		}
	}
	cost := ReceiverDeliverPrice.For(int64(len(d.Body)))
	subject := allowance.Subject{ID: row.account, KeyID: row.keyID, Signed: true, Hosted: row.hosted}
	if _, err = meter.Spend(ctx, tx, subject, allowance.Credit, cost, ledger.Ref{Service: ReceiverID, Op: "receive", Method: "deliver"}, now); err != nil {
		var ae *allowance.Err
		if errors.As(err, &ae) && (ae.Code == "quota_exhausted" || ae.Code == "global_quota_exhausted") {
			return DeliveryReceipt{}, "", &allowance.Err{Code: "receiver_quota_exhausted", RetryAfter: ae.RetryAfter}
		}
		return DeliveryReceipt{}, "", err
	}
	screen := "off"
	if r.mode.Wants(&row.screen) {
		screen = "pending"
		if !screenerUp(ctx, r.screener) {
			screen = "unavailable"
		}
	}
	headers := keptHeaders(d.Headers, d.Token)
	var latest int64
	if r.board != nil {
		if latest, err = r.board.LatestSeq(ctx, tx); err != nil {
			return DeliveryReceipt{}, "", err
		}
	}
	id := newCallID()
	if _, err = tx.ExecContext(ctx, "INSERT INTO receiver_items(id,receiver,account,content_type,body,bytes,headers,verified,cost,received_at,event_seq,screen,dedupe_key) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)",
		id, d.ID, row.account, mt, string(d.Body), len(d.Body), string(canonicalJSON(headers)), verified, cost, now, latest, screen, dedupeKey); err != nil {
		return DeliveryReceipt{}, "", err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE receivers SET deliveries=deliveries+1, last_at=? WHERE id=?", now, d.ID); err != nil {
		return DeliveryReceipt{}, "", err
	}
	// The item is an entry in its owner's inbox log (C61): its id only.
	if r.board != nil {
		if err = r.board.AddInboxEntry(ctx, tx, ReceivedInboxEntry(id, d.ID, row.account, now)); err != nil {
			return DeliveryReceipt{}, "", err
		}
	}
	if err = fireReceived(ctx, tx, r.board, row.account, latest, now); err != nil {
		return DeliveryReceipt{}, "", err
	}
	return DeliveryReceipt{Item: id, Bytes: len(d.Body)}, screen, nil
}

// ReceivedInboxEntry is the inbox entry of one stored receiver item: the
// board's backfill derives the same entry from receiver_items.
func ReceivedInboxEntry(item, receiver, account string, at int64) InboxEntry {
	return InboxEntry{Account: account, Kind: "received", Subject: item, Detail: map[string]any{"receiver": receiver}, At: at}
}

// Background starts the screening workers; they stop with ctx.
func (r *receiver) Background(ctx context.Context, wg *sync.WaitGroup) {
	r.mu.Lock()
	if r.started || r.screener == nil || r.engine == nil {
		r.mu.Unlock()
		return
	}
	r.started = true
	r.queue = make(chan string, receiverScreenQueue)
	r.mu.Unlock()
	for range receiverScreenWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-r.queue:
					if !r.claim(id) {
						continue
					}
					if err := r.screenItem(ctx, id); err != nil && ctx.Err() == nil {
						slog.Warn("receiver screening: an item was not screened", "error", err)
					}
					r.release(id)
				}
			}
		}()
	}
}

func (r *receiver) claim(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight[id] {
		return false
	}
	r.inflight[id] = true
	return true
}

func (r *receiver) release(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inflight, id)
}

// enqueue queues a delivered item for screening; a full queue leaves it to
// the worker's next pass.
func (r *receiver) enqueue(id string) {
	r.mu.Lock()
	q := r.queue
	r.mu.Unlock()
	if q == nil {
		return
	}
	select {
	case q <- id:
	default:
	}
}

// Work is the receiver's worker pass: pending items whose screen was lost
// (a full queue, a restart) are queued again, or screened here when no
// worker runs, and items pending too long are left unscreened.
func (r *receiver) Work(ctx context.Context, db *sql.DB, now int64) (int, error) {
	if r.engine == nil {
		return 0, nil
	}
	res, err := db.ExecContext(ctx, "UPDATE receiver_items SET screen='failed', screened_at=? WHERE seq IN (SELECT seq FROM receiver_items WHERE screen='pending' AND received_at<? ORDER BY received_at LIMIT 100)", now, now-receiverScreenGiveUp)
	if err != nil {
		return 0, err
	}
	gaveUp, _ := res.RowsAffected()
	r.mu.Lock()
	started := r.started
	r.mu.Unlock()
	cutoff := now - receiverScreenRetry
	if !started {
		cutoff = now + 1
	}
	rows, err := db.QueryContext(ctx, "SELECT id FROM receiver_items WHERE screen='pending' AND received_at<=? ORDER BY received_at LIMIT ?", cutoff, receiverPassScreens)
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
	n := int(gaveUp)
	for _, id := range ids {
		if started {
			r.enqueue(id)
			continue
		}
		if r.claim(id) {
			err = r.screenItem(ctx, id)
			r.release(id)
			if err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// balancer is a meter that can tell what an account has left (the ledger).
type balancer interface {
	Balance(ctx context.Context, q allowance.Querier, s allowance.Subject, r allowance.Resource, now int64) (ledger.Balance, error)
}

// screenItem screens one pending item with nothing held: a short read, the
// classifier, then a short transaction that charges the owner what it cost
// and stores the verdict. An owner without room for the most it could cost
// is not screened (unpaid), and neither is one whose charge is refused.
func (r *receiver) screenItem(ctx context.Context, id string) error {
	e := r.engine
	db := e.cfg.DB
	var account, keyID, body, state string
	var hosted bool
	err := db.QueryRowContext(ctx, "SELECT i.account,v.key_id,v.hosted,i.body,i.screen FROM receiver_items i JOIN receivers v ON v.id=i.receiver WHERE i.id=?", id).Scan(&account, &keyID, &hosted, &body, &state)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && state != "pending") {
		return nil
	}
	if err != nil {
		return err
	}
	subject := allowance.Subject{ID: account, KeyID: keyID, Signed: true, Hosted: hosted}
	mark := func(state string) error {
		_, err := db.ExecContext(ctx, "UPDATE receiver_items SET screen=?, screened_at=? WHERE id=? AND screen='pending'", state, e.cfg.Now(), id)
		return err
	}
	if b, ok := e.cfg.Meter.(balancer); ok {
		if bal, err := b.Balance(ctx, db, subject, allowance.Credit, e.cfg.Now()); err == nil && bal.Remaining < ScreenSurchargeMax(len(body)) {
			return mark("unpaid")
		}
	}
	sctx, cancel := context.WithTimeout(ctx, receiverScreenTimeout)
	verdict, cost, err := screenOptional(sctx, r.screener, body, "tool")
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return nil // stopping: the next start's pass tries again
		}
		return mark("failed")
	}
	now := e.cfg.Now()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	used := screenSurcharge(len(body), cost)
	if _, err = e.cfg.Meter.Spend(ctx, tx, subject, allowance.Credit, used, ledger.Ref{Service: ReceiverID, Op: "receive", Method: "screen"}, now); err != nil {
		var ae *allowance.Err
		if !errors.As(err, &ae) {
			return err
		}
		tx.Rollback()
		return mark("unpaid")
	}
	if _, err = tx.ExecContext(ctx, "UPDATE receiver_items SET screen='done', verdict=?, screen_cost=?, screened_at=? WHERE id=? AND screen='pending'", verdict.StoredJSON(), used, now, id); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeReceiver is the operator's revocation: the receiver stops at once
// (its URL answers not found) with the reason its owner sees; its items
// stay. It is idempotent.
func RevokeReceiver(ctx context.Context, db *sql.DB, id, reason string, now int64) (ReceiverView, error) {
	if !receiverIDRE.MatchString(id) || reason == "" || len(reason) > 256 || !utf8.ValidString(reason) {
		return ReceiverView{}, errors.New("receiver revoke needs a receiver id (32 hex) and a reason of 1 to 256 bytes")
	}
	if _, err := db.ExecContext(ctx, "UPDATE receivers SET state='revoked', reason=?, finished_at=? WHERE id=? AND state='active'", reason, now, id); err != nil {
		return ReceiverView{}, err
	}
	v, err := scanReceiver(db.QueryRowContext(ctx, "SELECT "+receiverColumns+" FROM receivers WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return v, errors.New("no receiver has that id")
	}
	return v, err
}

// receiverProvider is the enabled receiver service, or receiver_not_found.
func (e *Engine) receiverProvider() (*receiver, error) {
	p, err := e.cfg.Registry.Lookup(ReceiverID)
	if err != nil {
		return nil, refusal("receiver_not_found")
	}
	r, ok := p.(*receiver)
	if !ok {
		return nil, refusal("receiver_not_found")
	}
	return r, nil
}

// AdmitDelivery counts a delivery attempt from source, before any read: past
// ReceiverSourcePerMinute it is request_rate. With receivers off it is
// receiver_not_found.
func (e *Engine) AdmitDelivery(source net.IP, now int64) error {
	r, err := e.receiverProvider()
	if err != nil {
		return err
	}
	return r.AdmitSource(source, now)
}

// Probe answers a reachability check (GET or HEAD) on a receive URL: the
// source's attempt is counted like a delivery's, then the receiver is found
// exactly as a delivery finds it, read only. Nothing is stored, counted
// against the receiver or charged. q is the store's handle, not a held
// transaction.
func (e *Engine) Probe(ctx context.Context, q allowance.Querier, d Delivery, now int64) error {
	r, err := e.receiverProvider()
	if err != nil {
		return err
	}
	if err = r.AdmitSource(d.Source, now); err != nil {
		return err
	}
	_, err = findReceiver(ctx, q, d)
	return err
}

// Deliver stores one delivery in the caller's transaction (receiver.go
// deliver). after, run once the transaction has committed, queues the item's
// screen; it holds nothing and makes no request.
func (e *Engine) Deliver(ctx context.Context, tx *sql.Tx, d Delivery, now int64) (DeliveryReceipt, func(), error) {
	r, err := e.receiverProvider()
	if err != nil {
		return DeliveryReceipt{}, nil, err
	}
	receipt, screen, err := r.deliver(ctx, tx, e.cfg.Meter, d, now)
	if err != nil {
		return DeliveryReceipt{}, nil, err
	}
	after := func() {}
	if screen == "pending" {
		after = func() { r.enqueue(receipt.Item) }
	}
	return receipt, after, nil
}
