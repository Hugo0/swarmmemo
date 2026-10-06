package services

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
)

// Pastes (ROADMAP §3.11): text an agent stores and shares by id. A paste is
// its owner's alone (private, the default) or unlisted: anyone holding its
// id, 128 random bits never derived from the text, opens it with
// paste.open, which needs no key. Each paste is addressed by its text's
// SHA-256 as well, for its owner; notary: true stamps that hash with the
// notary at create. A paste may expire: after that only its owner reads
// it, and it is kept. delete is the owner's removal: the text goes, the
// record (hash, size, times) stays.

// Paste bounds.
const (
	// PasteID is the service id.
	PasteID = "paste"
	// PasteTextBytes bounds a paste's text.
	PasteTextBytes = 64 << 10
	// PastesPerDay bounds an account's new pastes per UTC day, and
	// PasteBytesMax the text it keeps in all (pastes not deleted).
	PastesPerDay  = 200
	PasteBytesMax = 16 << 20
	// PasteExpiryMin and PasteExpiryMax bound expires_in.
	PasteExpiryMin = 60
	PasteExpiryMax = 365 * 86400
	// PasteListPageMax bounds a list page.
	PasteListPageMax = 100
	// PasteNotaryPrice is what notary: true adds to create.
	PasteNotaryPrice = 1
	// PastePathPrefix is where the content domain will serve a public
	// paste: CONTENT_URL + /p/ID.
	PastePathPrefix = "/p/"
	pasteArgsMax    = 2*PasteTextBytes + 1024
	pasteSmallArgs  = 512
)

var pasteIDArg = Arg{"id", "string", true, "the paste's id"}

type paste struct {
	notaryKey ed25519.PrivateKey
	serviceID string
	content   string // CONTENT_URL, "" while public links are off
	screen    *sharedScreen
	engine    *Engine
	writes    rateTable
	opens     rateTable
}

func newPaste(d Deps) Provider {
	id := d.ServiceID
	if id == "" {
		id = "swarmmemo.com"
	}
	return &paste{notaryKey: d.NotaryKey, serviceID: id, content: d.ContentURL, screen: newSharedScreen(d.TextScreener, d.ContentScreen)}
}

func (p *paste) bindEngine(e *Engine) { p.engine = e }

func (*paste) Schema() string {
	return `
CREATE TABLE IF NOT EXISTS pastes (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, account TEXT NOT NULL, key_id TEXT NOT NULL DEFAULT '',
 hosted INTEGER NOT NULL DEFAULT 0, title TEXT NOT NULL DEFAULT '', text TEXT NOT NULL, bytes INTEGER NOT NULL, hash TEXT NOT NULL,
 visibility TEXT NOT NULL CHECK(visibility IN ('private','unlisted')), state TEXT NOT NULL CHECK(state IN ('active','deleted','hidden')),
 reason TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL DEFAULT 0, deleted_at INTEGER NOT NULL DEFAULT 0,
 notary_seq INTEGER NOT NULL DEFAULT 0, verdict TEXT NOT NULL DEFAULT '', screen_cost INTEGER NOT NULL DEFAULT 0,
 screened_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS pastes_account ON pastes(account,seq);
CREATE INDEX IF NOT EXISTS pastes_hash ON pastes(account,hash);
`
}

func (p *paste) Describe() Descriptor {
	return Descriptor{
		ID: PasteID,
		Summary: "Store text up to " + SizeText(PasteTextBytes) + " and share it by id: private to your key by default, or unlisted, so anyone holding its id opens it with paste.open, no key needed. " +
			"Addressed by its SHA-256 too; notary: true stamps that hash with the notary. Optional expiry: an expired paste is kept for you and unreadable to others. " +
			"Text reaches others only as JSON or a text/plain download, never as a web page, screened for prompt injection by default (the owner pays what screening cost, once per paste; screen: false skips it).",
		Title: "Paste", Topic: "Paste",
		Line: "Share text by id: private or unlisted, optional expiry, addressed by its SHA-256, notarised on request.",
		Limits: []Limit{
			{"paste_text_bytes", PasteTextBytes, "bytes", "One paste's text"},
			{"pastes_per_day", PastesPerDay, "", "New pastes per agent per UTC day"},
			{"paste_bytes", PasteBytesMax, "bytes", "Text kept per agent, every paste not deleted"},
			{"paste_expiry_max_seconds", PasteExpiryMax, "seconds", "The longest expires_in"},
			{"paste_writes_per_minute", contentWritesPerMinute, "", "Creates and deletes per agent a minute"},
			{"paste_opens_per_minute", contentReadsPerMinute, "", "Opens per agent or network a minute, found or not"},
		},
		Mode:        Local,
		MaxDuration: 90 * time.Second,
		Methods: []Method{
			{Name: "create", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: pasteArgsMax, Price: Price{Base: 2, PerKiB: 1},
				Line: "Store a paste; the answer has its id and SHA-256.",
				Args: []Arg{
					{"text", "string", true, "UTF-8 text, up to " + SizeText(PasteTextBytes)},
					{"title", "string", false, "one line, up to " + SizeText(contentTitleBytes)},
					{"visibility", "string", false, "private (the default: only you) or unlisted (anyone with the id)"},
					{"expires_in", "integer", false, "seconds, " + itoa(PasteExpiryMin) + " to " + itoa(PasteExpiryMax) + "; after it only you read it; default never"},
					{"notary", "boolean", false, "stamp the text's SHA-256 with the notary (adds " + itoa(PasteNotaryPrice) + ")"},
				},
				PriceNote:      "2 + 1 per KiB of text; notary: true adds " + itoa(PasteNotaryPrice),
				ExampleMaxCost: 4,
				Example:        json.RawMessage(`{"text":"Build log for run 42: all green.","visibility":"unlisted","expires_in":86400}`)},
			{Name: "delete", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: pasteSmallArgs, Price: Price{Base: 1},
				Line: "Remove a paste's text; its record (hash, size, times) stays.", Args: []Arg{pasteIDArg},
				Example: json.RawMessage(`{"id":"PASTE_ID"}`)},
			{Name: "open", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: pasteSmallArgs, Price: Price{Base: 1},
				Line:      "Open a paste by id: an unlisted paste, or your own. The text is in the first answer only, screened for prompt injection by default.",
				Args:      []Arg{pasteIDArg, {"screen", "boolean", false, "screen the text for prompt injection (default true; the owner pays what it cost, once per paste)"}},
				Example:   json.RawMessage(`{"id":"PASTE_ID"}`),
				Anonymous: true, AnonymousLabel: "paste opens", AnonymousNote: "unlisted pastes only, by id",
				AnonymousRate: AnonRate{CallerPerMinute: 30, CallerPerDay: 2000, AllPerMinute: 600, AllPerDay: 100000}},
			{Name: "get", Signed: true, ArgsMax: pasteSmallArgs,
				Line:    "Read one of your pastes with its text, by id or by SHA-256.",
				Args:    []Arg{{"id", "string", false, "the paste's id"}, {"hash", "string", false, "or its text's SHA-256 (your newest paste with it)"}},
				Example: json.RawMessage(`{"id":"PASTE_ID"}`)},
			{Name: "list", Signed: true, ArgsMax: pasteSmallArgs,
				Line:    "Your pastes without their text, newest first.",
				Args:    []Arg{{"before", "integer", false, "the seq of the last paste of the previous page"}, {"limit", "integer", false, "1 to " + itoa(PasteListPageMax) + ", default 20"}},
				Example: json.RawMessage(`{"limit":20}`)},
		},
	}
}

// CatalogueExtra states screening, where text goes and whether public links
// are on.
func (p *paste) CatalogueExtra() map[string]any {
	screening := screeningExtra(p.screen.mode, p.screen.screener)
	screening["paid_by"] = "the owner, once per paste"
	return map[string]any{"screening": screening, "download": CallPathPrefix + PasteID + "/open?id=PASTE_ID&format=text", "rendered": false, "public_links": p.content != "", "tool_page": "/tools/paste"}
}

type pasteCreateArgs struct {
	Text       *string         `json:"text"`
	Title      string          `json:"title"`
	Visibility string          `json:"visibility"`
	ExpiresIn  json.RawMessage `json:"expires_in"`
	Notary     bool            `json:"notary"`
}

type pasteSpec struct {
	text, title, visibility string
	expiresIn               int64
	notary                  bool
}

func parsePasteCreate(raw json.RawMessage) (pasteSpec, error) {
	var a pasteCreateArgs
	if err := StrictObject(raw, &a); err != nil {
		return pasteSpec{}, err
	}
	if a.Text == nil {
		return pasteSpec{}, badArg("text is required: a string.")
	}
	s := pasteSpec{text: *a.Text, title: a.Title, visibility: a.Visibility, notary: a.Notary}
	if len(s.text) > PasteTextBytes {
		return s, tooLarge("invalid_service_data", len(s.text), PasteTextBytes)
	}
	if len(s.title) > contentTitleBytes {
		return s, tooLarge("invalid_service_data", len(s.title), contentTitleBytes)
	}
	if !validContentText(s.text) {
		return s, badArg("text must be UTF-8 without NUL.")
	}
	if !validTitle(s.title) {
		return s, badArg("title must be one line of UTF-8 without control characters.")
	}
	switch s.visibility {
	case "":
		s.visibility = "private"
	case "private", "unlisted":
	default:
		return s, badArg(`visibility must be "private" or "unlisted".`)
	}
	if a.ExpiresIn != nil {
		n, err := intArg(a.ExpiresIn, "expires_in", "seconds", PasteExpiryMin, PasteExpiryMax)
		if err != nil {
			return s, err
		}
		s.expiresIn = n
	}
	return s, nil
}

type pasteOpenArgs struct {
	ID     string `json:"id"`
	Screen *bool  `json:"screen"`
}

func parsePasteOpen(raw json.RawMessage) (pasteOpenArgs, error) {
	var a pasteOpenArgs
	if err := StrictObject(raw, &a); err != nil {
		return a, err
	}
	if !contentIDRE.MatchString(a.ID) {
		return a, badArg(contentIDRule)
	}
	return a, nil
}

func parsePasteRef(raw json.RawMessage) (string, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := StrictObject(raw, &a); err != nil {
		return "", err
	}
	if !contentIDRE.MatchString(a.ID) {
		return "", badArg(contentIDRule)
	}
	return a.ID, nil
}

func (p *paste) Quote(c Call) (Quote, error) {
	switch c.Method {
	case "create":
		s, err := parsePasteCreate(c.Args)
		if err != nil {
			return Quote{}, err
		}
		max := c.Price.For(int64(len(s.text)))
		if s.notary {
			max += PasteNotaryPrice
		}
		return Quote{Resource: allowance.Credit, Max: max}, nil
	case "delete":
		if _, err := parsePasteRef(c.Args); err != nil {
			return Quote{}, err
		}
	case "open":
		if _, err := parsePasteOpen(c.Args); err != nil {
			return Quote{}, err
		}
	default:
		return Quote{}, refusal("invalid_service_data")
	}
	return Quote{Resource: allowance.Credit, Max: c.Price.For(0)}, nil
}

// ModeFor runs an open that asks for screening after commit (a fresh screen
// asks the classifier, holding nothing); everything else runs in the
// command's transaction.
func (p *paste) ModeFor(c Call) Mode {
	if c.Method != "open" {
		return Local
	}
	a, err := parsePasteOpen(c.Args)
	if err != nil {
		return Local
	}
	return p.screen.callMode(a.Screen)
}

// Admit bounds writes per account and opens per caller (an account, or one
// network without a key), every attempt counted, and refuses an open of a
// paste the caller may not read before anything is reserved: an unknown id
// costs nothing but its place in the window.
func (p *paste) Admit(ctx context.Context, q allowance.Querier, c Call) error {
	if c.Method != "open" {
		return p.writes.admit(c.Subject.ID, contentWritesPerMinute, c.Now)
	}
	if err := p.opens.admit(c.Subject.ID, contentReadsPerMinute, c.Now); err != nil {
		return err
	}
	a, err := parsePasteOpen(c.Args)
	if err != nil {
		return err
	}
	_, err = readablePaste(ctx, q, a.ID, c.Subject, c.Now)
	return err
}

// PasteView is a paste as its owner sees it: never its text.
type PasteView struct {
	Seq        int64  `json:"seq"`
	ID         string `json:"id"`
	Title      string `json:"title,omitempty"`
	Hash       string `json:"hash"`
	Bytes      int64  `json:"bytes"`
	Visibility string `json:"visibility"`
	State      string `json:"state"` // active, deleted, or hidden by the operator
	Reason     string `json:"reason,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  int64  `json:"expires_at,omitempty"`
	Expired    bool   `json:"expired,omitempty"`
	DeletedAt  int64  `json:"deleted_at,omitempty"`
	NotarySeq  int64  `json:"notary_seq,omitempty"`
	// PublicURL is where the content domain serves an unlisted paste, while
	// CONTENT_URL is set.
	PublicURL string `json:"public_url,omitempty"`
}

// pasteRow is a stored paste.
type pasteRow struct {
	PasteView
	account, keyID, text, verdict string
	hosted                        bool
}

const pasteColumns = "seq,id,title,hash,bytes,visibility,state,reason,created_at,expires_at,deleted_at,notary_seq,account,key_id,hosted,text,verdict"

func scanPaste(row interface{ Scan(...any) error }, now int64) (pasteRow, error) {
	var r pasteRow
	err := row.Scan(&r.Seq, &r.ID, &r.Title, &r.Hash, &r.Bytes, &r.Visibility, &r.State, &r.Reason, &r.CreatedAt, &r.ExpiresAt, &r.DeletedAt, &r.NotarySeq, &r.account, &r.keyID, &r.hosted, &r.text, &r.verdict)
	r.Expired = r.ExpiresAt > 0 && now >= r.ExpiresAt
	return r, err
}

func loadPaste(ctx context.Context, q allowance.Querier, id string, now int64) (pasteRow, error) {
	r, err := scanPaste(q.QueryRowContext(ctx, "SELECT "+pasteColumns+" FROM pastes WHERE id=?", id), now)
	if errors.Is(err, sql.ErrNoRows) {
		return r, refusal("paste_not_found")
	}
	return r, err
}

// readablePaste is the paste s may open: its own (not deleted or hidden),
// or an unlisted paste that has not expired. Anything else, private,
// expired, deleted, hidden or unknown, is paste_not_found alike, so an id
// tells a stranger nothing.
func readablePaste(ctx context.Context, q allowance.Querier, id string, s allowance.Subject, now int64) (pasteRow, error) {
	r, err := loadPaste(ctx, q, id, now)
	if err != nil {
		return r, err
	}
	own := s.Signed && s.ID == r.account
	if r.State != "active" || !own && (r.Visibility != "unlisted" || r.Expired) {
		return r, refusal("paste_not_found")
	}
	return r, nil
}

func (p *paste) view(r pasteRow) PasteView {
	v := r.PasteView
	if p.content != "" && v.Visibility == "unlisted" && v.State == "active" && !v.Expired {
		v.PublicURL = p.content + PastePathPrefix + v.ID
	}
	return v
}

func (p *paste) Run(ctx context.Context, tx *sql.Tx, c Call) (Result, error) {
	if c.Method == "open" {
		var q allowance.Querier = tx
		if tx == nil {
			if p.engine == nil {
				return Result{}, errors.New("paste: no engine for a remote open")
			}
			q = p.engine.cfg.DB
		}
		return p.open(ctx, q, c, tx == nil)
	}
	if tx == nil {
		return Result{}, errors.New("paste: writes run only inside the command's transaction")
	}
	account := c.Subject.ID
	switch c.Method {
	case "create":
		s, err := parsePasteCreate(c.Args)
		if err != nil {
			return Result{}, err
		}
		var today, kept int64
		if err = tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM pastes WHERE account=? AND created_at>=?), (SELECT coalesce(sum(bytes),0) FROM pastes WHERE account=? AND state='active')",
			account, c.Now-c.Now%86400, account).Scan(&today, &kept); err != nil {
			return Result{}, err
		}
		if today >= PastesPerDay || kept+int64(len(s.text)) > PasteBytesMax {
			return Result{}, refusal("paste_limit")
		}
		hash := sha256Of([]byte(s.text))
		var expires int64
		if s.expiresIn > 0 {
			expires = c.Now + s.expiresIn
		}
		used := c.Price.For(int64(len(s.text)))
		out := map[string]any{}
		var notarySeq int64
		if s.notary {
			if len(p.notaryKey) != ed25519.PrivateKeySize {
				return Result{}, refusal("service_unavailable")
			}
			receipt, err := StampHash(ctx, tx, p.notaryKey, p.serviceID, account, hash, c.Now)
			if err != nil {
				return Result{}, err
			}
			notarySeq, used = receipt.Seq, used+PasteNotaryPrice
			out["receipt"] = receipt
		}
		id := newCallID()
		if _, err = tx.ExecContext(ctx, "INSERT INTO pastes(id,account,key_id,hosted,title,text,bytes,hash,visibility,state,created_at,expires_at,notary_seq) VALUES(?,?,?,?,?,?,?,?,?,'active',?,?,?)",
			id, account, c.Subject.KeyID, c.Subject.Hosted, s.title, s.text, len(s.text), hash, s.visibility, c.Now, expires, notarySeq); err != nil {
			return Result{}, err
		}
		r, err := loadPaste(ctx, tx, id, c.Now)
		if err != nil {
			return Result{}, err
		}
		out["paste"] = p.view(r)
		body, _ := json.Marshal(out)
		public, _ := json.Marshal(map[string]any{"bytes": len(s.text)})
		return Result{Body: body, Used: used, Public: public}, nil
	case "delete":
		id, err := parsePasteRef(c.Args)
		if err != nil {
			return Result{}, err
		}
		r, err := loadPaste(ctx, tx, id, c.Now)
		if err == nil && r.account != account {
			err = refusal("paste_not_found")
		}
		if err != nil {
			return Result{}, err
		}
		// Idempotent: a deleted paste is returned as it stands.
		if r.State == "active" {
			if _, err = tx.ExecContext(ctx, "UPDATE pastes SET text='', verdict='', state='deleted', deleted_at=? WHERE id=? AND state='active'", c.Now, id); err != nil {
				return Result{}, err
			}
			r.State, r.DeletedAt = "deleted", c.Now
		}
		body, _ := json.Marshal(map[string]any{"paste": p.view(r)})
		return Result{Body: body, Used: c.Price.For(0), Public: json.RawMessage(`{}`)}, nil
	}
	return Result{}, refusal("invalid_service_data")
}

// open answers paste.open: the paste's record and, unless screening withheld
// it, its text in the first answer only (Result.Once), so no call record or
// retry receipt keeps a copy. remote is true after commit, where a fresh
// screen may ask the classifier.
func (p *paste) open(ctx context.Context, q allowance.Querier, c Call, remote bool) (Result, error) {
	a, err := parsePasteOpen(c.Args)
	if err != nil {
		return Result{}, err
	}
	r, err := readablePaste(ctx, q, a.ID, c.Subject, c.Now)
	if err != nil {
		return Result{}, err
	}
	own := c.Subject.Signed && c.Subject.ID == r.account
	wants := p.screen.wants(a.Screen)
	state, fresh := "unavailable", (*TextVerdict)(nil)
	if !own && wants && r.verdict == "" && remote {
		payer := allowance.Subject{ID: r.account, KeyID: r.keyID, Signed: true, Hosted: r.hosted}
		store := func(ctx context.Context, tx *sql.Tx, verdict string, cost, now int64) (bool, error) {
			res, err := tx.ExecContext(ctx, "UPDATE pastes SET verdict=?, screen_cost=?, screened_at=? WHERE id=? AND state='active' AND verdict=''", verdict, cost, now, r.ID)
			if err != nil {
				return false, err
			}
			n, err := res.RowsAffected()
			return n == 1, err
		}
		if fresh, state, err = p.screen.run(ctx, p.engine, "paste:"+r.ID, payer, r.text, ledger.Ref{Service: PasteID, Op: "screen", Method: "open"}, store); err != nil {
			return Result{}, err
		}
	}
	sc := screenFor(own, wants, r.verdict, state, fresh)
	view := p.view(r)
	shown := map[string]any{"id": view.ID, "title": view.Title, "hash": view.Hash, "bytes": view.Bytes, "created_at": view.CreatedAt}
	if view.ExpiresAt > 0 {
		shown["expires_at"] = view.ExpiresAt
	}
	out := map[string]any{"paste": shown, "own": own, "screened": sc.Screened, "screen": sc.Screen, "untrusted": !own, "rendered": false}
	if sc.Verdict != nil {
		out["verdict"] = sc.Verdict
	}
	if !own {
		out["note"] = UntrustedNote
	}
	res := Result{Used: c.Price.For(0), Public: json.RawMessage(`{"bytes":` + strconv.FormatInt(r.Bytes, 10) + `}`)}
	if sc.Withheld {
		out["withheld"] = true
		out["withheld_note"] = "Screening flagged this text, or is still screening it: open it again shortly, or with screen: false to read it unscreened."
	} else {
		res.Once = canonicalJSON(map[string]string{"text": r.text})
	}
	res.Body, _ = json.Marshal(out)
	return res, nil
}

type pasteGetArgs struct {
	ID   string `json:"id"`
	Hash string `json:"hash"`
}

type pasteListArgs struct {
	Before json.RawMessage `json:"before"`
	Limit  json.RawMessage `json:"limit"`
}

func (p *paste) Read(ctx context.Context, q allowance.Querier, c Call) (json.RawMessage, error) {
	account := c.Subject.ID
	switch c.Method {
	case "get":
		var a pasteGetArgs
		if err := StrictObject(c.Args, &a); err != nil {
			return nil, err
		}
		var row *sql.Row
		switch {
		case contentIDRE.MatchString(a.ID) && a.Hash == "":
			row = q.QueryRowContext(ctx, "SELECT "+pasteColumns+" FROM pastes WHERE id=? AND account=?", a.ID, account)
		case a.ID == "" && notaryHashRE.MatchString(a.Hash):
			row = q.QueryRowContext(ctx, "SELECT "+pasteColumns+" FROM pastes WHERE account=? AND hash=? ORDER BY seq DESC LIMIT 1", account, a.Hash)
		default:
			return nil, refusal("invalid_service_data")
		}
		r, err := scanPaste(row, c.Now)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, refusal("paste_not_found")
		}
		if err != nil {
			return nil, err
		}
		out := map[string]any{"paste": p.view(r)}
		if r.State != "deleted" {
			out["text"] = r.text
		}
		return json.Marshal(out)
	case "list":
		var a pasteListArgs
		if err := StrictObject(c.Args, &a); err != nil {
			return nil, err
		}
		before, limit := int64(1<<62), int64(20)
		if a.Before != nil {
			n, err := intArg(a.Before, "before", "", 0, 1<<53)
			if err != nil {
				return nil, err
			}
			before = n
		}
		if a.Limit != nil {
			n, err := intArg(a.Limit, "limit", "", 1, PasteListPageMax)
			if err != nil {
				return nil, err
			}
			limit = n
		}
		rows, err := q.QueryContext(ctx, "SELECT "+pasteColumns+" FROM pastes WHERE account=? AND seq<? ORDER BY seq DESC LIMIT ?", account, before, limit+1)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		pastes := []PasteView{}
		for rows.Next() {
			r, err := scanPaste(rows, c.Now)
			if err != nil {
				return nil, err
			}
			pastes = append(pastes, p.view(r))
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
		out := map[string]any{"pastes": pastes}
		if int64(len(pastes)) > limit {
			out["pastes"] = pastes[:limit]
			out["next_before"] = pastes[limit-1].Seq
		}
		return json.Marshal(out)
	}
	return nil, refusal("invalid_service_data")
}

// HidePaste is the operator's removal of an abused paste (moderation hides,
// never deletes): it stops opening for anyone at once, its owner sees the
// reason, and its text and record stay. It is idempotent.
func HidePaste(ctx context.Context, db *sql.DB, id, reason string, now int64) (PasteView, error) {
	if !contentIDRE.MatchString(id) || reason == "" || len(reason) > 256 || !validTitle(reason) {
		return PasteView{}, errors.New("paste hide needs a paste id (32 hex) and a one-line reason of 1 to 256 bytes")
	}
	if _, err := db.ExecContext(ctx, "UPDATE pastes SET state='hidden', reason=? WHERE id=? AND state='active'", reason, id); err != nil {
		return PasteView{}, err
	}
	r, err := scanPaste(db.QueryRowContext(ctx, "SELECT "+pasteColumns+" FROM pastes WHERE id=?", id), now)
	if errors.Is(err, sql.ErrNoRows) {
		return PasteView{}, errors.New("no paste has that id")
	}
	return r.PasteView, err
}
