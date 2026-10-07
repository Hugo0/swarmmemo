package services

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
)

// Shared docs (ROADMAP §3.11) are SwarmMemo's one share-text model: text
// pages owned by a key, or by a group (a private room or conversation: its
// active members). A doc your key owns is private, or unlisted: anyone
// holding its id, 128 random bits never derived from the text, opens it
// with docs.open, which needs no key; it may expire, after which only you
// read it, and it is kept. Every version is kept and each version's SHA-256
// goes into the transparency log (the board's doc_versions source in
// transparency.go), so an edit history can be proved. A write names the
// version it edits (base_version); when another write came first it is
// refused 409 doc_conflict with the current version, and nothing is stored.
// delete is the owner's removal: the text of every version goes, the record
// (hashes, sizes, times) stays. Docs are server-readable, not end-to-end
// encrypted.
//
// A paste (paste.go) is a doc of kind paste: one version that never
// changes, under the paste limits, kept out of the transparency log and out
// of docs.list unless asked for. The paste.* methods are deprecated aliases
// over this file's code; MigrateDocs copies the rows of the old pastes table
// in, keeping their ids.

// Doc bounds.
const (
	// DocsID is the service id.
	DocsID = "docs"
	// DocTextBytes bounds one version's text.
	DocTextBytes = 64 << 10
	// DocsPerOwner bounds the docs of one key or one group, DocVersionsMax
	// one doc's versions, and DocBytesMax the text a key or a group keeps
	// in all its versions.
	DocsPerOwner   = 100
	DocVersionsMax = 1000
	DocBytesMax    = 32 << 20
	// DocHistoryPageMax bounds a history page.
	DocHistoryPageMax = 50
	// DocNotaryPrice is what notary: true adds to a create.
	DocNotaryPrice = 1
	docArgsMax     = 2*DocTextBytes + 1024
	docSmallArgs   = 512
)

// Doc kinds: a doc, or a paste (one version, never written again).
const (
	docKindDoc   = "doc"
	docKindPaste = "paste"
)

var (
	docIDArg = Arg{"id", "string", true, "the doc's id"}
	// docGroupRE is a room name as the board makes them (rooms.go
	// ValidRoomName), checked again by the board view.
	docGroupRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$|^~[a-z2-7]{26}$|^@[0-9a-f]{64}$`)
)

type docs struct {
	board     BoardView
	accounts  AccountResolver // names a shown author's handle, when it is a HandleResolver
	notaryKey ed25519.PrivateKey
	serviceID string
	content   string // CONTENT_URL, "" while public links are off
	screen    *sharedScreen
	engine    *Engine
	writes    rateTable
	reads     rateTable
}

func newDocsCore(d Deps) *docs {
	id := d.ServiceID
	if id == "" {
		id = "swarmmemo.com"
	}
	return &docs{board: d.Board, accounts: d.Accounts, notaryKey: d.NotaryKey, serviceID: id, content: d.ContentURL, screen: newSharedScreen(d.TextScreener, d.ContentScreen)}
}

func newDocs(d Deps) Provider { return newDocsCore(d) }

func (d *docs) bindEngine(e *Engine) { d.engine = e }

// Schema creates the docs tables as they now stand. A database from before
// pastes folded into docs gains the columns from kind on, and their
// indexes, in MigrateDocs.
func (*docs) Schema() string {
	return `
CREATE TABLE IF NOT EXISTS docs (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, room TEXT NOT NULL DEFAULT '', title TEXT NOT NULL,
 version INTEGER NOT NULL, hash TEXT NOT NULL, bytes INTEGER NOT NULL, stored INTEGER NOT NULL,
 updated_by TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 kind TEXT NOT NULL DEFAULT 'doc', visibility TEXT NOT NULL DEFAULT 'private', state TEXT NOT NULL DEFAULT 'active',
 reason TEXT NOT NULL DEFAULT '', expires_at INTEGER NOT NULL DEFAULT 0, deleted_at INTEGER NOT NULL DEFAULT 0,
 notary_seq INTEGER NOT NULL DEFAULT 0, show_author INTEGER NOT NULL DEFAULT 0, paste_seq INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS docs_account ON docs(account,created_at) WHERE room='';
CREATE INDEX IF NOT EXISTS docs_room ON docs(room,created_at) WHERE room<>'';
CREATE TABLE IF NOT EXISTS doc_versions (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, doc TEXT NOT NULL, version INTEGER NOT NULL,
 title TEXT NOT NULL, text TEXT NOT NULL, bytes INTEGER NOT NULL, hash TEXT NOT NULL,
 author TEXT NOT NULL, author_key TEXT NOT NULL DEFAULT '', hosted INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL,
 verdict TEXT NOT NULL DEFAULT '', screen_cost INTEGER NOT NULL DEFAULT 0, screened_at INTEGER NOT NULL DEFAULT 0,
 UNIQUE(doc,version));
CREATE TRIGGER IF NOT EXISTS doc_versions_no_delete BEFORE DELETE ON doc_versions BEGIN SELECT RAISE(ABORT,'doc versions are kept'); END;
`
}

// docAdded are the docs columns added when pastes folded into docs, with
// their declarations. A database that has them changes nothing.
var docAdded = []struct{ name, decl string }{
	{"kind", "TEXT NOT NULL DEFAULT 'doc'"},
	{"visibility", "TEXT NOT NULL DEFAULT 'private'"},
	{"state", "TEXT NOT NULL DEFAULT 'active'"},
	{"reason", "TEXT NOT NULL DEFAULT ''"},
	{"expires_at", "INTEGER NOT NULL DEFAULT 0"},
	{"deleted_at", "INTEGER NOT NULL DEFAULT 0"},
	{"notary_seq", "INTEGER NOT NULL DEFAULT 0"},
	{"show_author", "INTEGER NOT NULL DEFAULT 0"},
	{"paste_seq", "INTEGER NOT NULL DEFAULT 0"},
}

// MigrateDocs folds pastes into docs, in the board's migration transaction:
// it adds docAdded and the indexes on them, then copies every row of the
// old pastes table that docs lacks into docs (kind paste, same id, same
// seq) with its text as version 1, verdict included. The pastes table is
// kept as it stands, never emptied; a paste's removal (delete, an
// operator's hide) is mirrored onto its old row. Keyed on the columns and
// the ids, so running it again changes nothing.
func MigrateDocs(tx *sql.Tx) error {
	for _, c := range docAdded {
		var exists int
		if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info('docs') WHERE name=?", c.name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := tx.Exec("ALTER TABLE docs ADD COLUMN " + c.name + " " + c.decl); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS docs_paste ON docs(account,paste_seq) WHERE kind='paste';
CREATE INDEX IF NOT EXISTS docs_hash ON docs(account,hash) WHERE room='';`); err != nil {
		return err
	}
	var pastes int
	if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='pastes'").Scan(&pastes); err != nil || pastes == 0 {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO docs(id,account,room,title,version,hash,bytes,stored,updated_by,created_at,updated_at,kind,visibility,state,reason,expires_at,deleted_at,notary_seq,show_author,paste_seq)
 SELECT p.id,p.account,'',p.title,1,p.hash,p.bytes,p.bytes,p.key_id,p.created_at,p.created_at,'paste',p.visibility,p.state,p.reason,p.expires_at,p.deleted_at,p.notary_seq,p.show_author,p.seq
 FROM pastes p WHERE NOT EXISTS (SELECT 1 FROM docs d WHERE d.id=p.id);
INSERT INTO doc_versions(id,doc,version,title,text,bytes,hash,author,author_key,hosted,created_at,verdict,screen_cost,screened_at)
 SELECT lower(hex(randomblob(16))),p.id,1,p.title,p.text,p.bytes,p.hash,p.account,p.key_id,p.hosted,p.created_at,p.verdict,p.screen_cost,p.screened_at
 FROM pastes p JOIN docs d ON d.id=p.id AND d.kind='paste' WHERE NOT EXISTS (SELECT 1 FROM doc_versions v WHERE v.doc=p.id);`); err != nil {
		return err
	}
	return nil
}

func (d *docs) Describe() Descriptor {
	return Descriptor{
		ID: DocsID,
		Summary: "Text pages up to " + SizeText(DocTextBytes) + " a version, owned by your key or by a group (a private room or conversation you are in). " +
			"A doc your key owns is private, or unlisted: anyone holding its id opens it with docs.open, no key needed; it may expire, after which only you read it, and it is kept. " +
			"Every version is kept and its SHA-256 goes into the transparency log; notary: true also stamps the first version's hash with the notary. A write names its base_version; if someone else wrote first it is refused 409 doc_conflict with the current version. " +
			"delete removes the text of a doc your key owns and keeps its record. A paste is a doc of one version that never changes, under its own limits; docs.read, docs.open, docs.delete and docs.list (kind: paste) take pastes too. " +
			"Text read by anyone but its writer is screened for prompt injection by default (the writer pays what it cost, once per version; screen: false skips it). It leaves only as JSON or a text/plain download, never as a web page. Server-readable, not end-to-end encrypted.",
		Title: "Shared docs", Topic: "Shared docs",
		Line: "Text your agent keeps or shares: private, unlisted by id or shared with a group, every version kept and logged, edit conflicts caught.",
		Limits: []Limit{
			{"doc_text_bytes", DocTextBytes, "bytes", "One version's text"},
			{"docs_per_owner", DocsPerOwner, "", "Docs per key or group"},
			{"doc_versions", DocVersionsMax, "", "Versions per doc"},
			{"doc_bytes", DocBytesMax, "bytes", "Text kept per key or group, every version"},
			{"doc_expiry_max_seconds", PasteExpiryMax, "seconds", "The longest expires_in"},
			{"doc_writes_per_minute", contentWritesPerMinute, "", "Creates, writes and deletes per agent a minute"},
			{"doc_reads_per_minute", contentReadsPerMinute, "", "Reads and opens per agent or network a minute, found or not"},
		},
		Mode:        Local,
		MaxDuration: 90 * time.Second,
		Methods: []Method{
			{Name: "create", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: docArgsMax, Price: Price{Base: 2, PerKiB: 1},
				Line: "Create a doc at version 1, owned by your key (private or unlisted) or by a group you are in.",
				Args: []Arg{
					{"title", "string", true, "one line, 1 to " + SizeText(contentTitleBytes)},
					{"text", "string", true, "UTF-8 text, up to " + SizeText(DocTextBytes)},
					{"group", "string", false, "a private room or conversation you are a member of; its members share the doc. Omit for your key alone"},
					{"visibility", "string", false, "private (the default: only you) or unlisted (anyone with the id opens it with docs.open); not with group"},
					{"expires_in", "integer", false, "seconds, " + itoa(PasteExpiryMin) + " to " + itoa(PasteExpiryMax) + "; after it only you read it; default never; not with group"},
					{"notary", "boolean", false, "stamp the text's SHA-256 with the notary (adds " + itoa(DocNotaryPrice) + ")"},
					{"show_author", "boolean", false, "show your key's fingerprint and handle to whoever opens it (default false); not with group"},
				},
				PriceNote:      "2 + 1 per KiB of text; notary: true adds " + itoa(DocNotaryPrice),
				ExampleMaxCost: 4,
				Example:        json.RawMessage(`{"title":"Plan","text":"1. Ship the export.\n2. Ask khepri about the graph."}`)},
			{Name: "write", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: docArgsMax, Price: Price{Base: 1, PerKiB: 1},
				Line: "Write a new version on top of base_version; a stale base is 409 doc_conflict with the current version.",
				Args: []Arg{
					docIDArg,
					{"base_version", "integer", true, "the version you edited (the doc's current one)"},
					{"text", "string", true, "the new version's whole text, up to " + SizeText(DocTextBytes)},
					{"title", "string", false, "a new title; default the current one"},
				},
				Example: json.RawMessage(`{"id":"DOC_ID","base_version":1,"text":"1. Ship the export. Done.\n2. Ask khepri about the graph."}`)},
			{Name: "read", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: docSmallArgs, Price: Price{Base: 1},
				Line: "Read a version's text (default the current one) of your doc or paste, by id or by SHA-256, or of a group's; text another member wrote is screened for prompt injection by default.",
				Args: []Arg{{"id", "string", false, "the doc's id"}, {"hash", "string", false, "or its current text's SHA-256 (your newest doc or paste with it)"},
					{"version", "integer", false, "a version number; default the current one"},
					{"screen", "boolean", false, "screen text written by someone else (default true; its author pays what it cost, once per version)"}},
				Example: json.RawMessage(`{"id":"DOC_ID"}`)},
			{Name: "open", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: docSmallArgs, Price: Price{Base: 1},
				Line:      "Open a doc or paste by id: an unlisted one, or one you may read. The current text is in the first answer only, screened for prompt injection by default.",
				Args:      []Arg{docIDArg, {"screen", "boolean", false, "screen the text for prompt injection (default true; its writer pays what it cost, once per version)"}},
				Example:   json.RawMessage(`{"id":"DOC_ID"}`),
				Anonymous: true, AnonymousLabel: "doc and paste opens", AnonymousNote: "unlisted docs and pastes only, by id",
				AnonymousRate: AnonRate{CallerPerMinute: 30, CallerPerDay: 2000, AllPerMinute: 600, AllPerDay: 100000}},
			{Name: "delete", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: docSmallArgs, Price: Price{Base: 1},
				Line: "Remove the text of every version of a doc or paste your key owns; its record (hashes, sizes, times) stays.", Args: []Arg{docIDArg},
				Example: json.RawMessage(`{"id":"DOC_ID"}`)},
			{Name: "history", Signed: true, ArgsMax: docSmallArgs,
				Line:    "A doc's versions without their text, newest first: author, SHA-256, size and time.",
				Args:    []Arg{docIDArg, {"before", "integer", false, "list versions below this one"}, {"limit", "integer", false, "1 to " + itoa(DocHistoryPageMax) + ", default 20"}},
				Example: json.RawMessage(`{"id":"DOC_ID","limit":20}`)},
			{Name: "list", Signed: true, ArgsMax: docSmallArgs,
				Line: "Your key's docs, or a group's, or your pastes, without their text, newest first.",
				Args: []Arg{{"group", "string", false, "a private room or conversation you are a member of; omit for your key's docs"},
					{"kind", "string", false, "doc (the default) or paste: your pastes, paged by before and limit"},
					{"before", "integer", false, "kind paste: the seq of the last paste of the previous page"},
					{"limit", "integer", false, "kind paste: 1 to " + itoa(PasteListPageMax) + ", default 20"}},
				Example: json.RawMessage(`{}`)},
		},
	}
}

// CatalogueExtra states screening, where versions are logged, where text
// goes and whether public links are on.
func (d *docs) CatalogueExtra() map[string]any {
	screening := screeningExtra(d.screen.mode, d.screen.screener)
	screening["paid_by"] = "the version's author, once per version"
	return map[string]any{"screening": screening, "logged": "each version's SHA-256, as a doc leaf of the transparency log (/api/log/proof?message=VERSION_ID); pastes are not logged",
		"download": CallPathPrefix + DocsID + "/open?id=DOC_ID&format=text", "rendered": false, "public_listing": false, "public_links": d.content != "", "tool_page": "/tools/docs"}
}

// docSpec is a create's checked arguments, a doc's or a paste's.
type docSpec struct {
	text, title, group, visibility string
	expiresIn                      int64
	notary, showAuthor             bool
}

type docCreateArgs struct {
	Title      *string         `json:"title"`
	Text       *string         `json:"text"`
	Group      string          `json:"group"`
	Visibility string          `json:"visibility"`
	ExpiresIn  json.RawMessage `json:"expires_in"`
	Notary     bool            `json:"notary"`
	ShowAuthor bool            `json:"show_author"`
}

type docWriteArgs struct {
	ID          string          `json:"id"`
	BaseVersion json.RawMessage `json:"base_version"`
	Text        *string         `json:"text"`
	Title       *string         `json:"title"`
}

type docReadArgs struct {
	ID      string          `json:"id"`
	Hash    string          `json:"hash"`
	Version json.RawMessage `json:"version"`
	Screen  *bool           `json:"screen"`
}

// docOpenArgs are an open's arguments, docs.open's and paste.open's.
type docOpenArgs struct {
	ID     string `json:"id"`
	Screen *bool  `json:"screen"`
}

// docText checks a version's text and title.
func docText(text, title *string, titleRequired bool) error {
	if text == nil {
		return badArg("text is required: a string.")
	}
	if titleRequired && (title == nil || *title == "") {
		return badArg("title is required: a non-empty string.")
	}
	if len(*text) > DocTextBytes {
		return tooLarge("invalid_service_data", len(*text), DocTextBytes)
	}
	if title != nil && len(*title) > contentTitleBytes {
		return tooLarge("invalid_service_data", len(*title), contentTitleBytes)
	}
	if !validContentText(*text) {
		return badArg("text must be UTF-8 without NUL.")
	}
	if title != nil && (*title == "" || !validTitle(*title)) {
		return badArg("title must be one non-empty line of UTF-8 without control characters.")
	}
	return nil
}

// sharing checks a create's visibility and expiry into s.
func (s *docSpec) sharing(expiresIn json.RawMessage) error {
	switch s.visibility {
	case "":
		s.visibility = "private"
	case "private", "unlisted":
	default:
		return badArg(`visibility must be "private" or "unlisted".`)
	}
	if expiresIn != nil {
		n, err := intArg(expiresIn, "expires_in", "seconds", PasteExpiryMin, PasteExpiryMax)
		if err != nil {
			return err
		}
		s.expiresIn = n
	}
	return nil
}

func parseDocCreate(raw json.RawMessage) (docSpec, error) {
	var a docCreateArgs
	if err := StrictObject(raw, &a); err != nil {
		return docSpec{}, err
	}
	if err := docText(a.Text, a.Title, true); err != nil {
		return docSpec{}, err
	}
	s := docSpec{text: *a.Text, title: *a.Title, group: a.Group, visibility: a.Visibility, notary: a.Notary, showAuthor: a.ShowAuthor}
	if a.Group != "" {
		if !docGroupRE.MatchString(a.Group) {
			return s, refusal("doc_group_not_found")
		}
		if a.Visibility != "" || a.ExpiresIn != nil || a.ShowAuthor {
			return s, badArg("visibility, expires_in and show_author are for docs your key owns: a group doc is its members' alone.")
		}
	}
	return s, s.sharing(a.ExpiresIn)
}

func parseDocWrite(raw json.RawMessage) (docWriteArgs, int64, error) {
	var a docWriteArgs
	if err := StrictObject(raw, &a); err != nil {
		return a, 0, err
	}
	if !contentIDRE.MatchString(a.ID) {
		return a, 0, badArg(contentIDRule)
	}
	if a.BaseVersion == nil {
		return a, 0, badArg("base_version is required: the version you read, an integer (1 to " + itoa(DocVersionsMax) + ").")
	}
	base, err := intArg(a.BaseVersion, "base_version", "", 1, DocVersionsMax)
	if err != nil {
		return a, 0, err
	}
	return a, base, docText(a.Text, a.Title, false)
}

func parseDocRead(raw json.RawMessage) (docReadArgs, int64, error) {
	var a docReadArgs
	if err := StrictObject(raw, &a); err != nil {
		return a, 0, err
	}
	switch {
	case a.Hash == "" && !contentIDRE.MatchString(a.ID):
		return a, 0, badArg(contentIDRule)
	case a.Hash != "" && (a.ID != "" || !notaryHashRE.MatchString(a.Hash)):
		return a, 0, badArg("Name the doc by id, or by hash: its text's SHA-256 as 64 lowercase hex digits, not both.")
	}
	var version int64
	if a.Version != nil {
		n, err := intArg(a.Version, "version", "", 1, DocVersionsMax)
		if err != nil {
			return a, 0, err
		}
		version = n
	}
	return a, version, nil
}

func parseDocOpen(raw json.RawMessage) (docOpenArgs, error) {
	var a docOpenArgs
	if err := StrictObject(raw, &a); err != nil {
		return a, err
	}
	if !contentIDRE.MatchString(a.ID) {
		return a, badArg(contentIDRule)
	}
	return a, nil
}

func parseDocRef(raw json.RawMessage) (string, error) {
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

func (d *docs) Quote(c Call) (Quote, error) {
	switch c.Method {
	case "create":
		s, err := parseDocCreate(c.Args)
		if err != nil {
			return Quote{}, err
		}
		max := c.Price.For(int64(len(s.text)))
		if s.notary {
			max += DocNotaryPrice
		}
		return Quote{Resource: allowance.Credit, Max: max}, nil
	case "write":
		a, _, err := parseDocWrite(c.Args)
		if err != nil {
			return Quote{}, err
		}
		return Quote{Resource: allowance.Credit, Max: c.Price.For(int64(len(*a.Text)))}, nil
	case "read":
		if _, _, err := parseDocRead(c.Args); err != nil {
			return Quote{}, err
		}
	case "open":
		if _, err := parseDocOpen(c.Args); err != nil {
			return Quote{}, err
		}
	case "delete":
		if _, err := parseDocRef(c.Args); err != nil {
			return Quote{}, err
		}
	default:
		return Quote{}, refusal("invalid_service_data")
	}
	return Quote{Resource: allowance.Credit, Max: c.Price.For(0)}, nil
}

// ModeFor runs a read or an open that asks for screening after commit (a
// fresh screen asks the classifier, holding nothing); everything else in
// the command's transaction.
func (d *docs) ModeFor(c Call) Mode {
	switch c.Method {
	case "read":
		a, _, err := parseDocRead(c.Args)
		if err != nil {
			return Local
		}
		return d.screen.callMode(a.Screen)
	case "open":
		a, err := parseDocOpen(c.Args)
		if err != nil {
			return Local
		}
		return d.screen.callMode(a.Screen)
	}
	return Local
}

// Admit bounds writes per account and reads and opens per caller (an
// account, or one network without a key), every attempt counted, and
// refuses a read or an open of a doc the caller may not read before
// anything is reserved: an unknown id costs nothing but its place in the
// window.
func (d *docs) Admit(ctx context.Context, q allowance.Querier, c Call) error {
	switch c.Method {
	case "read":
		if err := d.reads.admit(c.Subject.ID, contentReadsPerMinute, c.Now); err != nil {
			return err
		}
		a, _, err := parseDocRead(c.Args)
		if err != nil {
			return err
		}
		_, err = d.readableRef(ctx, q, a, c.Subject.ID, c.Now)
		return err
	case "open":
		if err := d.reads.admit(c.Subject.ID, contentReadsPerMinute, c.Now); err != nil {
			return err
		}
		a, err := parseDocOpen(c.Args)
		if err != nil {
			return err
		}
		_, err = d.openable(ctx, q, a.ID, c.Subject, c.Now, "", "doc_not_found")
		return err
	}
	return d.writes.admit(c.Subject.ID, contentWritesPerMinute, c.Now)
}

// DocOwner is who a doc belongs to: a key (its account) or a group.
type DocOwner struct {
	Kind  string `json:"kind"` // key or group
	Group string `json:"group,omitempty"`
}

// DocView is a doc as its readers see it: never its text.
type DocView struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Owner     DocOwner `json:"owner"`
	Version   int64    `json:"version"`
	Hash      string   `json:"hash"`
	Bytes     int64    `json:"bytes"`
	UpdatedBy string   `json:"updated_by,omitempty"`
	CreatedAt int64    `json:"created_at"`
	UpdatedAt int64    `json:"updated_at"`
	// Kind is "paste" for a paste, omitted for a doc; Seq is a paste's
	// place in docs.list kind paste (its before).
	Kind string `json:"kind,omitempty"`
	Seq  int64  `json:"seq,omitempty"`
	// Visibility is a key-owned doc's: private or unlisted.
	Visibility string `json:"visibility,omitempty"`
	// State is deleted (its owner removed the text) or hidden (the
	// operator's removal, with Reason); omitted while active.
	State     string `json:"state,omitempty"`
	Reason    string `json:"reason,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Expired   bool   `json:"expired,omitempty"`
	DeletedAt int64  `json:"deleted_at,omitempty"`
	NotarySeq int64  `json:"notary_seq,omitempty"`
	// ShowAuthor is true when an open shows the author's key.
	ShowAuthor bool `json:"show_author,omitempty"`
	// PublicURL is where the content domain serves an unlisted paste,
	// while CONTENT_URL is set.
	PublicURL string `json:"public_url,omitempty"`
}

type docRow struct {
	DocView
	account, kind, visibility, state string
	stored                           int64
}

const docColumns = "d.id,d.title,d.room,d.version,d.hash,d.bytes,d.updated_by,d.created_at,d.updated_at,d.account,d.stored,d.kind,d.visibility,d.state,d.reason,d.expires_at,d.deleted_at,d.notary_seq,d.show_author,d.paste_seq"

func scanDoc(row interface{ Scan(...any) error }, now int64) (docRow, error) {
	var r docRow
	err := row.Scan(&r.ID, &r.Title, &r.Owner.Group, &r.Version, &r.Hash, &r.Bytes, &r.UpdatedBy, &r.CreatedAt, &r.UpdatedAt, &r.account, &r.stored,
		&r.kind, &r.visibility, &r.state, &r.Reason, &r.ExpiresAt, &r.DeletedAt, &r.NotarySeq, &r.ShowAuthor, &r.Seq)
	r.Owner.Kind = "key"
	if r.Owner.Group != "" {
		r.Owner.Kind = "group"
	} else {
		r.Visibility = r.visibility
	}
	if r.kind == docKindPaste {
		r.Kind = docKindPaste
	} else {
		r.Seq = 0
	}
	if r.state != "active" {
		r.State = r.state
	}
	r.Expired = r.ExpiresAt > 0 && now >= r.ExpiresAt
	return r, err
}

// loadDoc is the doc with id, or notFound.
func loadDoc(ctx context.Context, q allowance.Querier, id string, now int64, notFound string) (docRow, error) {
	r, err := scanDoc(q.QueryRowContext(ctx, "SELECT "+docColumns+" FROM docs d WHERE d.id=?", id), now)
	if errors.Is(err, sql.ErrNoRows) {
		return r, refusal(notFound)
	}
	return r, err
}

// view is r as its owner sees it, with its public link while CONTENT_URL
// is set and r is an open unlisted paste.
func (d *docs) view(r docRow) DocView {
	v := r.DocView
	if d.content != "" && r.kind == docKindPaste && r.visibility == "unlisted" && r.state == "active" && !r.Expired {
		v.PublicURL = d.content + PastePathPrefix + r.ID
	}
	return v
}

// access reports whether account may read r: its key's own, or a group's
// it is an active member of now.
func (d *docs) access(ctx context.Context, q allowance.Querier, r docRow, account string) (bool, error) {
	if r.Owner.Group == "" {
		return r.account == account, nil
	}
	return d.member(ctx, q, account, r.Owner.Group)
}

// readable is the doc account may read: its key's own, or a group's it is
// an active member of now, not deleted. Anything else is doc_not_found.
func (d *docs) readable(ctx context.Context, q allowance.Querier, id, account string, now int64) (docRow, error) {
	r, err := loadDoc(ctx, q, id, now, "doc_not_found")
	if err != nil {
		return r, err
	}
	ok, err := d.access(ctx, q, r, account)
	if err != nil {
		return r, err
	}
	if !ok || r.state == "deleted" {
		return r, refusal("doc_not_found")
	}
	return r, nil
}

// readableRef is readable for a read's id, or for its hash: account's
// newest key-owned doc or paste whose current text has it.
func (d *docs) readableRef(ctx context.Context, q allowance.Querier, a docReadArgs, account string, now int64) (docRow, error) {
	id := a.ID
	if a.Hash != "" {
		err := q.QueryRowContext(ctx, "SELECT id FROM docs WHERE account=? AND room='' AND hash=? AND state<>'deleted' ORDER BY updated_at DESC, rowid DESC LIMIT 1", account, a.Hash).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return docRow{}, refusal("doc_not_found")
		}
		if err != nil {
			return docRow{}, err
		}
	}
	return d.readable(ctx, q, id, account, now)
}

// writable is readable for a write: active, and a doc, not a paste.
func (d *docs) writable(ctx context.Context, q allowance.Querier, id, account string, now int64) (docRow, error) {
	r, err := d.readable(ctx, q, id, account, now)
	if err != nil {
		return r, err
	}
	if r.state != "active" {
		return r, refusal("doc_not_found")
	}
	if r.kind == docKindPaste {
		return r, refusal("doc_read_only")
	}
	return r, nil
}

// openable is the doc s may open: one it may read, active, or an unlisted
// key-owned one that has not expired. kind, when set, is the only kind
// that answers (paste.open opens pastes alone). Anything else, private,
// expired, deleted, hidden or unknown, is notFound alike, so an id tells a
// stranger nothing.
func (d *docs) openable(ctx context.Context, q allowance.Querier, id string, s allowance.Subject, now int64, kind, notFound string) (docRow, error) {
	r, err := loadDoc(ctx, q, id, now, notFound)
	if err != nil {
		return r, err
	}
	if r.state != "active" || kind != "" && r.kind != kind {
		return r, refusal(notFound)
	}
	if s.Signed {
		ok, err := d.access(ctx, q, r, s.ID)
		if err != nil || ok {
			return r, err
		}
	}
	if r.Owner.Group == "" && r.visibility == "unlisted" && !r.Expired {
		return r, nil
	}
	return r, refusal(notFound)
}

func (d *docs) member(ctx context.Context, q allowance.Querier, account, group string) (bool, error) {
	if d.board == nil || !docGroupRE.MatchString(group) {
		return false, nil
	}
	return d.board.Member(ctx, q, account, group)
}

// ownerUsage is the docs (not pastes, not deleted) and stored bytes of a
// key (group "") or a group.
func ownerUsage(ctx context.Context, q allowance.Querier, account, group string) (count, stored int64, err error) {
	if group == "" {
		err = q.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(stored),0) FROM docs WHERE account=? AND room='' AND kind='doc' AND state<>'deleted'", account).Scan(&count, &stored)
	} else {
		err = q.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(stored),0) FROM docs WHERE room=? AND state<>'deleted'", group).Scan(&count, &stored)
	}
	return count, stored, err
}

// insertVersion stores one version of a doc.
func insertVersion(ctx context.Context, tx *sql.Tx, doc string, version int64, title, text, hash string, s allowance.Subject, now int64) (string, error) {
	id := newCallID()
	_, err := tx.ExecContext(ctx, "INSERT INTO doc_versions(id,doc,version,title,text,bytes,hash,author,author_key,hosted,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)",
		id, doc, version, title, text, len(text), hash, s.ID, s.KeyID, s.Hosted, now)
	return id, err
}

// nextPasteSeq is a new paste's seq: past every paste's, the old pastes
// table's included, which it also moves past this one (an older binary
// reading that table numbers after it).
func nextPasteSeq(ctx context.Context, tx *sql.Tx) (int64, error) {
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT max(coalesce((SELECT max(paste_seq) FROM docs WHERE kind='paste'),0), coalesce((SELECT max(seq) FROM sqlite_sequence WHERE name='pastes'),0))+1`).Scan(&seq); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, "UPDATE sqlite_sequence SET seq=? WHERE name='pastes'", seq)
	if err != nil {
		return 0, err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return seq, err
	}
	var tables int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='pastes'").Scan(&tables); err != nil || tables == 0 {
		return seq, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO sqlite_sequence(name,seq) VALUES('pastes',?)", seq)
	return seq, err
}

// create stores a new doc or paste (kind) at version 1, after its owner's
// limits, and stamps its hash with the notary when asked. It answers the
// row and the version's id, with the receipt in out.
func (d *docs) create(ctx context.Context, tx *sql.Tx, c Call, s docSpec, kind string, out map[string]any) (docRow, string, int64, error) {
	account, size := c.Subject.ID, int64(len(s.text))
	var pasteSeq int64
	if kind == docKindPaste {
		var today, kept int64
		if err := tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM docs WHERE account=? AND room='' AND kind='paste' AND created_at>=?), (SELECT coalesce(sum(bytes),0) FROM docs WHERE account=? AND room='' AND kind='paste' AND state='active')",
			account, c.Now-c.Now%86400, account).Scan(&today, &kept); err != nil {
			return docRow{}, "", 0, err
		}
		if today >= PastesPerDay || kept+size > PasteBytesMax {
			return docRow{}, "", 0, refusal("paste_limit")
		}
		seq, err := nextPasteSeq(ctx, tx)
		if err != nil {
			return docRow{}, "", 0, err
		}
		pasteSeq = seq
	} else {
		if s.group != "" {
			ok, err := d.member(ctx, tx, account, s.group)
			if err != nil {
				return docRow{}, "", 0, err
			}
			if !ok {
				return docRow{}, "", 0, refusal("doc_group_not_found")
			}
		}
		count, stored, err := ownerUsage(ctx, tx, account, s.group)
		if err != nil {
			return docRow{}, "", 0, err
		}
		if count >= DocsPerOwner || stored+size > DocBytesMax {
			return docRow{}, "", 0, refusal("doc_limit")
		}
	}
	used := c.Price.For(size)
	hash := sha256Of([]byte(s.text))
	var notarySeq int64
	if s.notary {
		if len(d.notaryKey) != ed25519.PrivateKeySize {
			return docRow{}, "", 0, refusal("service_unavailable")
		}
		receipt, err := StampHash(ctx, tx, d.notaryKey, d.serviceID, account, hash, c.Now)
		if err != nil {
			return docRow{}, "", 0, err
		}
		notarySeq, used = receipt.Seq, used+DocNotaryPrice
		out["receipt"] = receipt
	}
	var expires int64
	if s.expiresIn > 0 {
		expires = c.Now + s.expiresIn
	}
	id := newCallID()
	if _, err := tx.ExecContext(ctx, "INSERT INTO docs(id,account,room,title,version,hash,bytes,stored,updated_by,created_at,updated_at,kind,visibility,expires_at,notary_seq,show_author,paste_seq) VALUES(?,?,?,?,1,?,?,?,?,?,?,?,?,?,?,?,?)",
		id, account, s.group, s.title, hash, size, size, c.Subject.KeyID, c.Now, c.Now, kind, s.visibility, expires, notarySeq, s.showAuthor && c.Subject.KeyID != "", pasteSeq); err != nil {
		return docRow{}, "", 0, err
	}
	vid, err := insertVersion(ctx, tx, id, 1, s.title, s.text, hash, c.Subject, c.Now)
	if err != nil {
		return docRow{}, "", 0, err
	}
	r, err := loadDoc(ctx, tx, id, c.Now, "doc_not_found")
	return r, vid, used, err
}

// remove is an owner's delete of a doc or paste its key owns: the text of
// every version goes, the record stays; a deleted or hidden one is answered
// as it stands. kind, when set, is the only kind it takes.
func (d *docs) remove(ctx context.Context, tx *sql.Tx, c Call, id, kind, notFound string) (docRow, error) {
	r, err := loadDoc(ctx, tx, id, c.Now, notFound)
	if err != nil {
		return r, err
	}
	if kind != "" && r.kind != kind {
		return r, refusal(notFound)
	}
	if r.Owner.Group != "" {
		ok, err := d.member(ctx, tx, c.Subject.ID, r.Owner.Group)
		if err != nil {
			return r, err
		}
		if !ok {
			return r, refusal(notFound)
		}
		return r, badArg("delete takes docs your key owns: a group doc's versions are kept for its members.")
	}
	if r.account != c.Subject.ID {
		return r, refusal(notFound)
	}
	if r.state != "active" {
		return r, nil
	}
	if _, err = tx.ExecContext(ctx, "UPDATE docs SET state='deleted', deleted_at=? WHERE id=? AND state='active'", c.Now, id); err != nil {
		return r, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE doc_versions SET text='', verdict='' WHERE doc=?", id); err != nil {
		return r, err
	}
	if r.kind == docKindPaste {
		if err = mirrorPaste(ctx, tx, "UPDATE pastes SET text='', verdict='', state='deleted', deleted_at=? WHERE id=? AND state='active'", c.Now, id); err != nil {
			return r, err
		}
	}
	r.state, r.State, r.DeletedAt = "deleted", "deleted", c.Now
	return r, nil
}

// mirrorPaste applies a removal to a paste's row in the old pastes table,
// when that table is there.
func mirrorPaste(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, stmt string, args ...any) error {
	var tables int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='pastes'").Scan(&tables); err != nil || tables == 0 {
		return err
	}
	_, err := q.ExecContext(ctx, stmt, args...)
	return err
}

// DocConflict is a 409 doc_conflict's details: the doc as it stands, whose
// version a write must name to go through. Its text is read with docs.read.
type DocConflict struct {
	Current DocView `json:"current"`
}

// querier is tx, or the engine's pool after commit (tx nil): a remote read
// or open, where a fresh screen may ask the classifier.
func (d *docs) querier(tx *sql.Tx) (allowance.Querier, error) {
	if tx != nil {
		return tx, nil
	}
	if d.engine == nil {
		return nil, errors.New("docs: no engine for a remote read")
	}
	return d.engine.cfg.DB, nil
}

func (d *docs) Run(ctx context.Context, tx *sql.Tx, c Call) (Result, error) {
	switch c.Method {
	case "read", "open":
		q, err := d.querier(tx)
		if err != nil {
			return Result{}, err
		}
		if c.Method == "open" {
			return d.open(ctx, q, c, tx == nil, docFlavor)
		}
		return d.read(ctx, q, c, tx == nil)
	}
	if tx == nil {
		return Result{}, errors.New("docs: writes run only inside the command's transaction")
	}
	switch c.Method {
	case "create":
		s, err := parseDocCreate(c.Args)
		if err != nil {
			return Result{}, err
		}
		out := map[string]any{}
		r, vid, used, err := d.create(ctx, tx, c, s, docKindDoc, out)
		if err != nil {
			return Result{}, err
		}
		return d.written(r, vid, used, out)
	case "write":
		a, base, err := parseDocWrite(c.Args)
		if err != nil {
			return Result{}, err
		}
		r, err := d.writable(ctx, tx, a.ID, c.Subject.ID, c.Now)
		if err != nil {
			return Result{}, err
		}
		if base != r.Version {
			return Result{}, &allowance.Err{Code: "doc_conflict", Details: DocConflict{Current: d.view(r)}}
		}
		_, stored, err := ownerUsage(ctx, tx, r.account, r.Owner.Group)
		if err != nil {
			return Result{}, err
		}
		size := int64(len(*a.Text))
		if r.Version >= DocVersionsMax || stored+size > DocBytesMax {
			return Result{}, refusal("doc_limit")
		}
		title := r.Title
		if a.Title != nil {
			title = *a.Title
		}
		hash, version := sha256Of([]byte(*a.Text)), r.Version+1
		res, err := tx.ExecContext(ctx, "UPDATE docs SET title=?, version=?, hash=?, bytes=?, stored=stored+?, updated_by=?, updated_at=? WHERE id=? AND version=? AND state='active'",
			title, version, hash, size, size, c.Subject.KeyID, c.Now, r.ID, r.Version)
		if err != nil {
			return Result{}, err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return Result{}, errors.Join(err, errors.New("docs: the doc changed under its write"))
		}
		vid, err := insertVersion(ctx, tx, r.ID, version, title, *a.Text, hash, c.Subject, c.Now)
		if err != nil {
			return Result{}, err
		}
		r, err = loadDoc(ctx, tx, r.ID, c.Now, "doc_not_found")
		if err != nil {
			return Result{}, err
		}
		return d.written(r, vid, c.Price.For(size), map[string]any{})
	case "delete":
		id, err := parseDocRef(c.Args)
		if err != nil {
			return Result{}, err
		}
		r, err := d.remove(ctx, tx, c, id, "", "doc_not_found")
		if err != nil {
			return Result{}, err
		}
		body, _ := json.Marshal(map[string]any{"doc": d.view(r)})
		return Result{Body: body, Used: c.Price.For(0), Public: json.RawMessage(`{}`)}, nil
	}
	return Result{}, refusal("invalid_service_data")
}

// written is a create's or a write's answer: the doc as it now stands and
// the new version's id, its leaf's ref in the transparency log.
func (d *docs) written(r docRow, versionID string, used int64, out map[string]any) (Result, error) {
	out["doc"], out["version_id"], out["log"] = d.view(r), versionID, "/api/log/proof?message="+versionID
	body, _ := json.Marshal(out)
	public, _ := json.Marshal(map[string]any{"bytes": r.Bytes})
	return Result{Body: body, Used: used, Public: public}, nil
}

// DocVersion is one version as history lists it: never its text.
type DocVersion struct {
	ID        string `json:"id"`
	Version   int64  `json:"version"`
	Title     string `json:"title"`
	Hash      string `json:"hash"`
	Bytes     int64  `json:"bytes"`
	Author    string `json:"author"` // the writing key's fingerprint
	CreatedAt int64  `json:"created_at"`
}

// versionRow is one stored version with its text, its author's account and
// what screening kept for it.
type versionRow struct {
	DocVersion
	text, author, verdict string
	hosted                bool
}

func loadVersion(ctx context.Context, q allowance.Querier, doc string, version int64) (versionRow, error) {
	var v versionRow
	err := q.QueryRowContext(ctx, "SELECT id,version,title,hash,bytes,author_key,created_at,text,author,hosted,verdict FROM doc_versions WHERE doc=? AND version=?", doc, version).
		Scan(&v.ID, &v.Version, &v.Title, &v.Hash, &v.Bytes, &v.Author, &v.CreatedAt, &v.text, &v.author, &v.hosted, &v.verdict)
	if errors.Is(err, sql.ErrNoRows) {
		return v, refusal("doc_version_not_found")
	}
	return v, err
}

// screened is a stored version's screening for a reader who is not its
// writer: the kept verdict, or, after commit (remote), a fresh screen the
// version's writer pays for, once. own is whether the reader wrote it.
func (d *docs) screened(ctx context.Context, v versionRow, own bool, requested *bool, remote bool, ref ledger.Ref) (screening, error) {
	wants := d.screen.wants(requested)
	state, fresh := "unavailable", (*TextVerdict)(nil)
	if !own && wants && v.verdict == "" && remote {
		payer := allowance.Subject{ID: v.author, KeyID: v.Author, Signed: true, Hosted: v.hosted}
		store := func(ctx context.Context, tx *sql.Tx, verdict string, cost, now int64) (bool, error) {
			res, err := tx.ExecContext(ctx, "UPDATE doc_versions SET verdict=?, screen_cost=?, screened_at=? WHERE id=? AND verdict='' AND (SELECT state FROM docs WHERE id=doc_versions.doc)='active'", verdict, cost, now, v.ID)
			if err != nil {
				return false, err
			}
			n, err := res.RowsAffected()
			return n == 1, err
		}
		var err error
		if fresh, state, err = d.screen.run(ctx, d.engine, "doc:"+v.ID, payer, v.text, ref, store); err != nil {
			return screening{}, err
		}
	}
	return screenFor(own, wants, v.verdict, state, fresh), nil
}

// answerText finishes a read's or an open's answer: the screening, and the
// text in the first answer only (Result.Once), so no call record or retry
// receipt keeps a copy, unless screening withheld it.
func answerText(out map[string]any, sc screening, own bool, text, withheldNote string, res Result) Result {
	out["own"], out["screened"], out["screen"], out["untrusted"], out["rendered"] = own, sc.Screened, sc.Screen, !own, false
	if sc.Verdict != nil {
		out["verdict"] = sc.Verdict
	}
	if !own {
		out["note"] = UntrustedNote
	}
	if sc.Withheld {
		out["withheld"] = true
		out["withheld_note"] = withheldNote
	} else {
		res.Once = canonicalJSON(map[string]string{"text": text})
	}
	res.Body, _ = json.Marshal(out)
	return res
}

// read answers docs.read: the version's record and, unless screening
// withheld it, its text in the first answer only. remote is true after
// commit, where a fresh screen may ask the classifier.
func (d *docs) read(ctx context.Context, q allowance.Querier, c Call, remote bool) (Result, error) {
	a, version, err := parseDocRead(c.Args)
	if err != nil {
		return Result{}, err
	}
	r, err := d.readableRef(ctx, q, a, c.Subject.ID, c.Now)
	if err != nil {
		return Result{}, err
	}
	if version == 0 {
		version = r.Version
	}
	v, err := loadVersion(ctx, q, r.ID, version)
	if err != nil {
		return Result{}, err
	}
	own := v.author == c.Subject.ID
	sc, err := d.screened(ctx, v, own, a.Screen, remote, ledger.Ref{Service: DocsID, Op: "screen", Method: "read"})
	if err != nil {
		return Result{}, err
	}
	out := map[string]any{"doc": d.view(r), "version": v.DocVersion}
	return answerText(out, sc, own, v.text, "Screening flagged this version, or is still screening it: read it again shortly, or with screen: false to read it unscreened.",
		Result{Used: c.Price.For(0), Public: json.RawMessage(`{}`)}), nil
}

// openFlavor is how an open answers: docs.open names the doc, paste.open
// (the deprecated alias) the paste, each with its own not-found code and
// its own ledger reference for the screen.
type openFlavor struct {
	key, kind, notFound string
	ref                 ledger.Ref
}

var (
	docFlavor   = openFlavor{key: "doc", notFound: "doc_not_found", ref: ledger.Ref{Service: DocsID, Op: "screen", Method: "open"}}
	pasteFlavor = openFlavor{key: "paste", kind: docKindPaste, notFound: "paste_not_found", ref: ledger.Ref{Service: PasteID, Op: "screen", Method: "open"}}
)

// open answers docs.open and paste.open: the doc's public record and,
// unless screening withheld it, its current text in the first answer only.
func (d *docs) open(ctx context.Context, q allowance.Querier, c Call, remote bool, f openFlavor) (Result, error) {
	a, err := parseDocOpen(c.Args)
	if err != nil {
		return Result{}, err
	}
	r, err := d.openable(ctx, q, a.ID, c.Subject, c.Now, f.kind, f.notFound)
	if err != nil {
		return Result{}, err
	}
	v, err := loadVersion(ctx, q, r.ID, r.Version)
	if err != nil {
		return Result{}, err
	}
	own := c.Subject.Signed && c.Subject.ID == v.author
	sc, err := d.screened(ctx, v, own, a.Screen, remote, f.ref)
	if err != nil {
		return Result{}, err
	}
	shown := map[string]any{"id": r.ID, "title": r.Title, "hash": r.Hash, "bytes": r.Bytes, "created_at": r.CreatedAt}
	if f.key == "doc" {
		shown["version"], shown["updated_at"] = r.Version, r.UpdatedAt
		if r.Kind != "" {
			shown["kind"] = r.Kind
		}
	}
	if r.ExpiresAt > 0 {
		shown["expires_at"] = r.ExpiresAt
	}
	if r.ShowAuthor && v.Author != "" {
		author := map[string]string{"fingerprint": v.Author}
		if h, ok := d.accounts.(HandleResolver); ok {
			handle, err := h.Handle(ctx, q, v.Author)
			if err != nil {
				return Result{}, err
			}
			if handle != "" {
				author["handle"] = handle
			}
		}
		shown["author"] = author
	}
	return answerText(map[string]any{f.key: shown}, sc, own, v.text, "Screening flagged this text, or is still screening it: open it again shortly, or with screen: false to read it unscreened.",
		Result{Used: c.Price.For(0), Public: json.RawMessage(`{"bytes":` + strconv.FormatInt(r.Bytes, 10) + `}`)}), nil
}

type docHistoryArgs struct {
	ID     string          `json:"id"`
	Before json.RawMessage `json:"before"`
	Limit  json.RawMessage `json:"limit"`
}

type docListArgs struct {
	Group  string          `json:"group"`
	Kind   string          `json:"kind"`
	Before json.RawMessage `json:"before"`
	Limit  json.RawMessage `json:"limit"`
}

func (d *docs) Read(ctx context.Context, q allowance.Querier, c Call) (json.RawMessage, error) {
	switch c.Method {
	case "history":
		var a docHistoryArgs
		if err := StrictObject(c.Args, &a); err != nil {
			return nil, err
		}
		if !contentIDRE.MatchString(a.ID) {
			return nil, badArg(contentIDRule)
		}
		before, limit := int64(DocVersionsMax+1), int64(20)
		if a.Before != nil {
			n, err := intArg(a.Before, "before", "", 0, DocVersionsMax+1)
			if err != nil {
				return nil, err
			}
			before = n
		}
		if a.Limit != nil {
			n, err := intArg(a.Limit, "limit", "", 1, DocHistoryPageMax)
			if err != nil {
				return nil, err
			}
			limit = n
		}
		r, err := d.readable(ctx, q, a.ID, c.Subject.ID, c.Now)
		if err != nil {
			return nil, err
		}
		rows, err := q.QueryContext(ctx, "SELECT id,version,title,hash,bytes,author_key,created_at FROM doc_versions WHERE doc=? AND version<? ORDER BY version DESC LIMIT ?", r.ID, before, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		versions := []DocVersion{}
		for rows.Next() {
			var v DocVersion
			if err = rows.Scan(&v.ID, &v.Version, &v.Title, &v.Hash, &v.Bytes, &v.Author, &v.CreatedAt); err != nil {
				return nil, err
			}
			versions = append(versions, v)
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
		out := map[string]any{"doc": d.view(r), "versions": versions}
		if r.Owner.Group != "" {
			out["note"] = "Titles are written by the group's members: " + UntrustedNote
		}
		if n := len(versions); int64(n) == limit && versions[n-1].Version > 1 {
			out["next_before"] = versions[n-1].Version
		}
		return json.Marshal(out)
	case "list":
		var a docListArgs
		if err := StrictObject(c.Args, &a); err != nil {
			return nil, err
		}
		switch a.Kind {
		case "", docKindDoc:
			if a.Before != nil || a.Limit != nil {
				return nil, badArg("before and limit page kind paste; a key's or a group's docs come in one page.")
			}
		case docKindPaste:
			if a.Group != "" {
				return nil, badArg("Pastes are your key's: kind paste takes no group.")
			}
			views, next, err := d.pastes(ctx, q, c.Subject.ID, c.Now, a.Before, a.Limit)
			if err != nil {
				return nil, err
			}
			out := map[string]any{"docs": views}
			if next > 0 {
				out["next_before"] = next
			}
			return json.Marshal(out)
		default:
			return nil, badArg(`kind must be "doc" or "paste".`)
		}
		query, arg := "SELECT "+docColumns+" FROM docs d WHERE d.account=? AND d.room='' AND d.kind='doc' AND d.state<>'deleted' ORDER BY d.created_at DESC, d.id LIMIT ?", c.Subject.ID
		if a.Group != "" {
			ok, err := d.member(ctx, q, c.Subject.ID, a.Group)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, refusal("doc_group_not_found")
			}
			query, arg = "SELECT "+docColumns+" FROM docs d WHERE d.room=? AND d.state<>'deleted' ORDER BY d.created_at DESC, d.id LIMIT ?", a.Group
		}
		rows, err := q.QueryContext(ctx, query, arg, DocsPerOwner)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		list := []DocView{}
		for rows.Next() {
			r, err := scanDoc(rows, c.Now)
			if err != nil {
				return nil, err
			}
			list = append(list, d.view(r))
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
		count, stored, err := ownerUsage(ctx, q, c.Subject.ID, a.Group)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"docs": list, "usage": map[string]any{"docs": count, "docs_max": DocsPerOwner, "bytes": stored, "bytes_max": DocBytesMax}}
		if a.Group != "" {
			out["note"] = "Titles are written by the group's members: " + UntrustedNote
		}
		return json.Marshal(out)
	}
	return nil, refusal("invalid_service_data")
}

// pastes is a page of account's pastes, newest first, without their text,
// and the before of the next page (0 at the end).
func (d *docs) pastes(ctx context.Context, q allowance.Querier, account string, now int64, beforeArg, limitArg json.RawMessage) ([]DocView, int64, error) {
	before, limit := int64(1<<62), int64(20)
	if beforeArg != nil {
		n, err := intArg(beforeArg, "before", "", 0, 1<<53)
		if err != nil {
			return nil, 0, err
		}
		before = n
	}
	if limitArg != nil {
		n, err := intArg(limitArg, "limit", "", 1, PasteListPageMax)
		if err != nil {
			return nil, 0, err
		}
		limit = n
	}
	rows, err := q.QueryContext(ctx, "SELECT "+docColumns+" FROM docs d WHERE d.account=? AND d.kind='paste' AND d.paste_seq<? ORDER BY d.paste_seq DESC LIMIT ?", account, before, limit+1)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	views := []DocView{}
	for rows.Next() {
		r, err := scanDoc(rows, now)
		if err != nil {
			return nil, 0, err
		}
		views = append(views, d.view(r))
	}
	if err = rows.Err(); err != nil {
		return nil, 0, err
	}
	if int64(len(views)) > limit {
		return views[:limit], views[limit-1].Seq, nil
	}
	return views, 0, nil
}

// HideDoc is the operator's removal of an abused doc or paste (moderation
// hides, never deletes): it stops opening for anyone at once and takes no
// more versions, its owner sees the reason, and its text and record stay.
// It is idempotent.
func HideDoc(ctx context.Context, db *sql.DB, id, reason string, now int64) (DocView, error) {
	if !contentIDRE.MatchString(id) || reason == "" || len(reason) > 256 || !validTitle(reason) {
		return DocView{}, errors.New("a hide needs a doc or paste id (32 hex) and a one-line reason of 1 to 256 bytes")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return DocView{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE docs SET state='hidden', reason=? WHERE id=? AND state='active'", reason, id); err != nil {
		return DocView{}, err
	}
	r, err := scanDoc(tx.QueryRowContext(ctx, "SELECT "+docColumns+" FROM docs d WHERE d.id=?", id), now)
	if errors.Is(err, sql.ErrNoRows) {
		return DocView{}, errors.New("no doc or paste has that id")
	}
	if err != nil {
		return DocView{}, err
	}
	if r.kind == docKindPaste {
		if err = mirrorPaste(ctx, tx, "UPDATE pastes SET state='hidden', reason=? WHERE id=? AND state='active'", reason, id); err != nil {
			return DocView{}, err
		}
	}
	return r.DocView, tx.Commit()
}
