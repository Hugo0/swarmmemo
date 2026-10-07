package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"swarmmemo/internal/allowance"
)

// Pastes (ROADMAP §3.11) are docs (docs.go) of kind paste: one version that
// never changes, private or unlisted, optionally expiring, addressed by its
// text's SHA-256 too. The paste.* methods are deprecated aliases kept so
// every paste id, URL and client keeps working: each runs the docs code
// with the paste's own limits, prices and answer shape, and names the doc
// method that replaces it. A paste may expire: after that only its owner
// reads it, and it is kept. delete is the owner's removal: the text goes,
// the record (hash, size, times) stays.
//
// Pastes used to live in their own table. MigrateDocs copies its rows into
// docs at startup, ids and seqs unchanged; the table stays as it was and
// takes no new rows.

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
	// PasteExpiryMin and PasteExpiryMax bound expires_in, a paste's and a
	// doc's.
	PasteExpiryMin = 60
	PasteExpiryMax = 365 * 86400
	// PasteListPageMax bounds a list page.
	PasteListPageMax = 100
	// PasteNotaryPrice is what notary: true adds to create.
	PasteNotaryPrice = DocNotaryPrice
	// PastePathPrefix is where the content domain will serve a public
	// paste: CONTENT_URL + /p/ID.
	PastePathPrefix = "/p/"
	pasteArgsMax    = 2*PasteTextBytes + 1024
	pasteSmallArgs  = 512
)

var pasteIDArg = Arg{"id", "string", true, "the paste's id"}

// paste is the deprecated alias: its own rate windows over the docs code.
type paste struct{ *docs }

func newPaste(d Deps) Provider { return &paste{newDocsCore(d)} }

// Schema keeps the old pastes table, the source MigrateDocs copies from; it
// takes no new rows.
func (*paste) Schema() string {
	return `
CREATE TABLE IF NOT EXISTS pastes (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, account TEXT NOT NULL, key_id TEXT NOT NULL DEFAULT '',
 hosted INTEGER NOT NULL DEFAULT 0, title TEXT NOT NULL DEFAULT '', text TEXT NOT NULL, bytes INTEGER NOT NULL, hash TEXT NOT NULL,
 visibility TEXT NOT NULL CHECK(visibility IN ('private','unlisted')), state TEXT NOT NULL CHECK(state IN ('active','deleted','hidden')),
 reason TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL DEFAULT 0, deleted_at INTEGER NOT NULL DEFAULT 0,
 notary_seq INTEGER NOT NULL DEFAULT 0, verdict TEXT NOT NULL DEFAULT '', screen_cost INTEGER NOT NULL DEFAULT 0,
 screened_at INTEGER NOT NULL DEFAULT 0, show_author INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS pastes_account ON pastes(account,seq);
CREATE INDEX IF NOT EXISTS pastes_hash ON pastes(account,hash);
`
}

// pasteAdded are the pastes columns added after the table: show_author.
var pasteAdded = []string{"show_author"}

// MigratePastes brings the old pastes table up to date (pasteAdded) and
// folds it into docs (MigrateDocs), in the board's migration transaction.
// Additive and keyed on the columns and ids, so running it again changes
// nothing.
func MigratePastes(tx *sql.Tx) error {
	for _, name := range pasteAdded {
		var tables, exists int
		if err := tx.QueryRow("SELECT (SELECT count(*) FROM sqlite_master WHERE type='table' AND name='pastes'), (SELECT count(*) FROM pragma_table_info('pastes') WHERE name=?)", name).Scan(&tables, &exists); err != nil {
			return err
		}
		if tables == 1 && exists == 0 {
			if _, err := tx.Exec("ALTER TABLE pastes ADD COLUMN " + name + " INTEGER NOT NULL DEFAULT 0"); err != nil {
				return err
			}
		}
	}
	return MigrateDocs(tx)
}

// HandleResolver is an AccountResolver that also names an agent key's
// handle ("" for none); an open uses it for a shown author.
type HandleResolver interface {
	Handle(ctx context.Context, q allowance.Querier, agent string) (string, error)
}

// pasteDeprecated starts each alias's line.
func pasteDeprecated(use string) string { return "Deprecated: use " + use + ". " }

func (p *paste) Describe() Descriptor {
	return Descriptor{
		ID: PasteID,
		Summary: "Deprecated aliases kept so every paste id and URL keeps working: a paste is a shared doc of one version that never changes, and docs does all of this (docs.create with visibility unlisted, docs.open, docs.read, docs.delete, docs.list with kind paste). " +
			"A paste holds up to " + SizeText(PasteTextBytes) + " of text, private to your key by default or unlisted, so anyone holding its id opens it with paste.open or docs.open, no key needed. " +
			"Addressed by its SHA-256 too; notary: true stamps that hash with the notary. Optional expiry: an expired paste is kept for you and unreadable to others. " +
			"Text reaches others only as JSON or a text/plain download, never as a web page, screened for prompt injection by default (the owner pays what screening cost, once per paste; screen: false skips it).",
		Title: "Paste", Topic: "",
		Line: "Deprecated aliases of shared docs: share text by id, private or unlisted, optional expiry, addressed by its SHA-256.",
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
			{Name: "create", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: pasteArgsMax, Price: Price{Base: 2, PerKiB: 1}, ReplacedBy: "docs.create",
				Line: pasteDeprecated(`docs.create with visibility "unlisted"`) + "Store a paste; the answer has its id and SHA-256.",
				Args: []Arg{
					{"text", "string", true, "UTF-8 text, up to " + SizeText(PasteTextBytes)},
					{"title", "string", false, "one line, up to " + SizeText(contentTitleBytes)},
					{"visibility", "string", false, "private (the default: only you) or unlisted (anyone with the id)"},
					{"expires_in", "integer", false, "seconds, " + itoa(PasteExpiryMin) + " to " + itoa(PasteExpiryMax) + "; after it only you read it; default never"},
					{"notary", "boolean", false, "stamp the text's SHA-256 with the notary (adds " + itoa(PasteNotaryPrice) + ")"},
					{"show_author", "boolean", false, "show your key's fingerprint and handle to whoever opens it (default false)"},
				},
				PriceNote:      "2 + 1 per KiB of text; notary: true adds " + itoa(PasteNotaryPrice),
				ExampleMaxCost: 4,
				Example:        json.RawMessage(`{"text":"Build log for run 42: all green.","visibility":"unlisted","expires_in":86400}`)},
			{Name: "delete", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: pasteSmallArgs, Price: Price{Base: 1}, ReplacedBy: "docs.delete",
				Line: pasteDeprecated("docs.delete") + "Remove a paste's text; its record (hash, size, times) stays.", Args: []Arg{pasteIDArg},
				Example: json.RawMessage(`{"id":"PASTE_ID"}`)},
			{Name: "open", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: pasteSmallArgs, Price: Price{Base: 1}, ReplacedBy: "docs.open",
				Line:      pasteDeprecated("docs.open") + "Open a paste by id: an unlisted paste, or your own. The text is in the first answer only, screened for prompt injection by default.",
				Args:      []Arg{pasteIDArg, {"screen", "boolean", false, "screen the text for prompt injection (default true; the owner pays what it cost, once per paste)"}},
				Example:   json.RawMessage(`{"id":"PASTE_ID"}`),
				Anonymous: true, AnonymousLabel: "doc and paste opens", AnonymousNote: "unlisted pastes only, by id",
				AnonymousRate: AnonRate{CallerPerMinute: 30, CallerPerDay: 2000, AllPerMinute: 600, AllPerDay: 100000}},
			{Name: "get", Signed: true, ArgsMax: pasteSmallArgs, ReplacedBy: "docs.read",
				Line:    pasteDeprecated("docs.read") + "Read one of your pastes with its text, by id or by SHA-256.",
				Args:    []Arg{{"id", "string", false, "the paste's id"}, {"hash", "string", false, "or its text's SHA-256 (your newest paste with it)"}},
				Example: json.RawMessage(`{"id":"PASTE_ID"}`)},
			{Name: "list", Signed: true, ArgsMax: pasteSmallArgs, ReplacedBy: "docs.list",
				Line:    pasteDeprecated(`docs.list with kind "paste"`) + "Your pastes without their text, newest first.",
				Args:    []Arg{{"before", "integer", false, "the seq of the last paste of the previous page"}, {"limit", "integer", false, "1 to " + itoa(PasteListPageMax) + ", default 20"}},
				Example: json.RawMessage(`{"limit":20}`)},
		},
	}
}

// CatalogueExtra states screening, where text goes, whether public links
// are on, and what replaces the aliases.
func (p *paste) CatalogueExtra() map[string]any {
	screening := screeningExtra(p.screen.mode, p.screen.screener)
	screening["paid_by"] = "the owner, once per paste"
	return map[string]any{"screening": screening, "download": CallPathPrefix + PasteID + "/open?id=PASTE_ID&format=text", "rendered": false, "public_links": p.content != "", "tool_page": "/tools/paste",
		"deprecated": true, "replaced_by": DocsID}
}

type pasteCreateArgs struct {
	Text       *string         `json:"text"`
	Title      string          `json:"title"`
	Visibility string          `json:"visibility"`
	ExpiresIn  json.RawMessage `json:"expires_in"`
	Notary     bool            `json:"notary"`
	ShowAuthor bool            `json:"show_author"`
}

type pasteGetArgs struct {
	ID   string `json:"id"`
	Hash string `json:"hash"`
}

type pasteListArgs struct {
	Before json.RawMessage `json:"before"`
	Limit  json.RawMessage `json:"limit"`
}

// parsePasteCreate checks a paste.create: docs.create's checks, with an
// optional title and no group.
func parsePasteCreate(raw json.RawMessage) (docSpec, error) {
	var a pasteCreateArgs
	if err := StrictObject(raw, &a); err != nil {
		return docSpec{}, err
	}
	if a.Text == nil {
		return docSpec{}, badArg("text is required: a string.")
	}
	s := docSpec{text: *a.Text, title: a.Title, visibility: a.Visibility, notary: a.Notary, showAuthor: a.ShowAuthor}
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
	return s, s.sharing(a.ExpiresIn)
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
		if _, err := parseDocRef(c.Args); err != nil {
			return Quote{}, err
		}
	case "open":
		if _, err := parseDocOpen(c.Args); err != nil {
			return Quote{}, err
		}
	default:
		return Quote{}, refusal("invalid_service_data")
	}
	return Quote{Resource: allowance.Credit, Max: c.Price.For(0)}, nil
}

// ModeFor runs an open that asks for screening after commit; everything
// else in the command's transaction.
func (p *paste) ModeFor(c Call) Mode {
	if c.Method != "open" {
		return Local
	}
	return p.docs.ModeFor(c)
}

// Admit bounds writes per account and opens per caller (an account, or one
// network without a key), every attempt counted, and refuses an open of a
// paste the caller may not read before anything is reserved.
func (p *paste) Admit(ctx context.Context, q allowance.Querier, c Call) error {
	if c.Method != "open" {
		return p.writes.admit(c.Subject.ID, contentWritesPerMinute, c.Now)
	}
	if err := p.reads.admit(c.Subject.ID, contentReadsPerMinute, c.Now); err != nil {
		return err
	}
	a, err := parseDocOpen(c.Args)
	if err != nil {
		return err
	}
	_, err = p.openable(ctx, q, a.ID, c.Subject, c.Now, docKindPaste, "paste_not_found")
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
	// ShowAuthor is true when an open shows the author's key.
	ShowAuthor bool `json:"show_author,omitempty"`
	// PublicURL is where the content domain serves an unlisted paste, while
	// CONTENT_URL is set.
	PublicURL string `json:"public_url,omitempty"`
}

// pasteView is a paste's doc view in the paste shape.
func pasteView(v DocView) PasteView {
	state := v.State
	if state == "" {
		state = "active"
	}
	return PasteView{Seq: v.Seq, ID: v.ID, Title: v.Title, Hash: v.Hash, Bytes: v.Bytes, Visibility: v.Visibility, State: state, Reason: v.Reason,
		CreatedAt: v.CreatedAt, ExpiresAt: v.ExpiresAt, Expired: v.Expired, DeletedAt: v.DeletedAt, NotarySeq: v.NotarySeq, ShowAuthor: v.ShowAuthor, PublicURL: v.PublicURL}
}

func (p *paste) Run(ctx context.Context, tx *sql.Tx, c Call) (Result, error) {
	if c.Method == "open" {
		q, err := p.querier(tx)
		if err != nil {
			return Result{}, err
		}
		return p.open(ctx, q, c, tx == nil, pasteFlavor)
	}
	if tx == nil {
		return Result{}, errors.New("paste: writes run only inside the command's transaction")
	}
	switch c.Method {
	case "create":
		s, err := parsePasteCreate(c.Args)
		if err != nil {
			return Result{}, err
		}
		out := map[string]any{}
		r, _, used, err := p.create(ctx, tx, c, s, docKindPaste, out)
		if err != nil {
			return Result{}, err
		}
		out["paste"] = pasteView(p.view(r))
		body, _ := json.Marshal(out)
		public, _ := json.Marshal(map[string]any{"bytes": len(s.text)})
		return Result{Body: body, Used: used, Public: public}, nil
	case "delete":
		id, err := parseDocRef(c.Args)
		if err != nil {
			return Result{}, err
		}
		r, err := p.remove(ctx, tx, c, id, docKindPaste, "paste_not_found")
		if err != nil {
			return Result{}, err
		}
		body, _ := json.Marshal(map[string]any{"paste": pasteView(p.view(r))})
		return Result{Body: body, Used: c.Price.For(0), Public: json.RawMessage(`{}`)}, nil
	}
	return Result{}, refusal("invalid_service_data")
}

func (p *paste) Read(ctx context.Context, q allowance.Querier, c Call) (json.RawMessage, error) {
	account := c.Subject.ID
	switch c.Method {
	case "get":
		var a pasteGetArgs
		if err := StrictObject(c.Args, &a); err != nil {
			return nil, err
		}
		const own = " FROM docs d JOIN doc_versions v ON v.doc=d.id AND v.version=d.version WHERE d.account=? AND d.room='' AND d.kind='paste'"
		var row *sql.Row
		switch {
		case contentIDRE.MatchString(a.ID) && a.Hash == "":
			row = q.QueryRowContext(ctx, "SELECT "+docColumns+",v.text"+own+" AND d.id=?", account, a.ID)
		case a.ID == "" && notaryHashRE.MatchString(a.Hash):
			row = q.QueryRowContext(ctx, "SELECT "+docColumns+",v.text"+own+" AND d.hash=? ORDER BY d.paste_seq DESC LIMIT 1", account, a.Hash)
		default:
			return nil, refusal("invalid_service_data")
		}
		var text string
		r, err := scanDoc(scanWith{row, &text}, c.Now)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, refusal("paste_not_found")
		}
		if err != nil {
			return nil, err
		}
		out := map[string]any{"paste": pasteView(p.view(r))}
		if r.state != "deleted" {
			out["text"] = text
		}
		return json.Marshal(out)
	case "list":
		var a pasteListArgs
		if err := StrictObject(c.Args, &a); err != nil {
			return nil, err
		}
		views, next, err := p.pastes(ctx, q, account, c.Now, a.Before, a.Limit)
		if err != nil {
			return nil, err
		}
		pastes := make([]PasteView, len(views))
		for i, v := range views {
			pastes[i] = pasteView(v)
		}
		out := map[string]any{"pastes": pastes}
		if next > 0 {
			out["next_before"] = next
		}
		return json.Marshal(out)
	}
	return nil, refusal("invalid_service_data")
}

// scanWith scans a row's docColumns and then extra.
type scanWith struct {
	row   interface{ Scan(...any) error }
	extra *string
}

func (s scanWith) Scan(dest ...any) error { return s.row.Scan(append(dest, s.extra)...) }

// HidePaste is HideDoc for a paste, answered in the paste shape: the
// operator's removal of an abused paste. It stops opening for anyone at
// once, its owner sees the reason, and its text and record stay.
func HidePaste(ctx context.Context, db *sql.DB, id, reason string, now int64) (PasteView, error) {
	v, err := HideDoc(ctx, db, id, reason, now)
	if err != nil {
		return PasteView{}, err
	}
	return pasteView(v), nil
}
