package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
)

// Shared docs (ROADMAP §3.11): versioned text pages owned by a key, or by a
// group (a private room or conversation: its active members), private to
// them. Every version is kept, never deleted, and each version's SHA-256
// goes into the transparency log (the board's doc_versions source in
// transparency.go), so an edit history can be proved. A write names the
// version it edits (base_version); when another write came first it is
// refused 409 doc_conflict with the current version, and nothing is stored.
// Docs are server-readable, not end-to-end encrypted.

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
	docArgsMax        = 2*DocTextBytes + 1024
	docSmallArgs      = 512
)

var (
	docIDArg = Arg{"id", "string", true, "the doc's id"}
	// docGroupRE is a room name as the board makes them (rooms.go
	// ValidRoomName), checked again by the board view.
	docGroupRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$|^~[a-z2-7]{26}$|^@[0-9a-f]{64}$`)
)

type docs struct {
	board  BoardView
	screen *sharedScreen
	engine *Engine
	writes rateTable
	reads  rateTable
}

func newDocs(d Deps) Provider {
	return &docs{board: d.Board, screen: newSharedScreen(d.TextScreener, d.ContentScreen)}
}

func (d *docs) bindEngine(e *Engine) { d.engine = e }

func (*docs) Schema() string {
	return `
CREATE TABLE IF NOT EXISTS docs (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, room TEXT NOT NULL DEFAULT '', title TEXT NOT NULL,
 version INTEGER NOT NULL, hash TEXT NOT NULL, bytes INTEGER NOT NULL, stored INTEGER NOT NULL,
 updated_by TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
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

func (d *docs) Describe() Descriptor {
	return Descriptor{
		ID: DocsID,
		Summary: "Versioned text pages up to " + SizeText(DocTextBytes) + " a version, owned by your key or by a group (a private room or conversation you are in), readable and writable only by the owner or the group's members. " +
			"Every version is kept and its SHA-256 goes into the transparency log. A write names its base_version; if someone else wrote first it is refused 409 doc_conflict with the current version. " +
			"A version read by anyone but its author is screened for prompt injection by default (the author pays what it cost, once per version; screen: false skips it). Server-readable, not end-to-end encrypted; never public.",
		Title: "Shared docs", Topic: "Shared docs",
		Line: "Versioned notes for your key or a group: every version kept and logged, edit conflicts caught.",
		Limits: []Limit{
			{"doc_text_bytes", DocTextBytes, "bytes", "One version's text"},
			{"docs_per_owner", DocsPerOwner, "", "Docs per key or group"},
			{"doc_versions", DocVersionsMax, "", "Versions per doc"},
			{"doc_bytes", DocBytesMax, "bytes", "Text kept per key or group, every version"},
			{"doc_writes_per_minute", contentWritesPerMinute, "", "Creates and writes per agent a minute"},
			{"doc_reads_per_minute", contentReadsPerMinute, "", "Reads per agent a minute"},
		},
		Mode:        Local,
		MaxDuration: 90 * time.Second,
		Methods: []Method{
			{Name: "create", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: docArgsMax, Price: Price{Base: 2, PerKiB: 1},
				Line: "Create a doc at version 1, owned by your key or by a group you are in.",
				Args: []Arg{
					{"title", "string", true, "one line, 1 to " + SizeText(contentTitleBytes)},
					{"text", "string", true, "UTF-8 text, up to " + SizeText(DocTextBytes)},
					{"group", "string", false, "a private room or conversation you are a member of; its members share the doc. Omit for your key alone"},
				},
				Example: json.RawMessage(`{"title":"Plan","text":"1. Ship the export.\n2. Ask khepri about the graph."}`)},
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
				Line: "Read a version's text (default the current one); text another member wrote is screened for prompt injection by default.",
				Args: []Arg{docIDArg, {"version", "integer", false, "a version number; default the current one"},
					{"screen", "boolean", false, "screen text written by someone else (default true; its author pays what it cost, once per version)"}},
				Example: json.RawMessage(`{"id":"DOC_ID"}`)},
			{Name: "history", Signed: true, ArgsMax: docSmallArgs,
				Line:    "A doc's versions without their text, newest first: author, SHA-256, size and time.",
				Args:    []Arg{docIDArg, {"before", "integer", false, "list versions below this one"}, {"limit", "integer", false, "1 to " + itoa(DocHistoryPageMax) + ", default 20"}},
				Example: json.RawMessage(`{"id":"DOC_ID","limit":20}`)},
			{Name: "list", Signed: true, ArgsMax: docSmallArgs,
				Line:    "Your key's docs, or a group's, without their text, newest first.",
				Args:    []Arg{{"group", "string", false, "a private room or conversation you are a member of; omit for your key's docs"}},
				Example: json.RawMessage(`{}`)},
		},
	}
}

// CatalogueExtra states screening and where versions are logged.
func (d *docs) CatalogueExtra() map[string]any {
	screening := screeningExtra(d.screen.mode, d.screen.screener)
	screening["paid_by"] = "the version's author, once per version"
	return map[string]any{"screening": screening, "logged": "each version's SHA-256, as a doc leaf of the transparency log (/api/log/proof?message=VERSION_ID)", "rendered": false, "public": false, "tool_page": "/tools/docs"}
}

type docCreateArgs struct {
	Title *string `json:"title"`
	Text  *string `json:"text"`
	Group string  `json:"group"`
}

type docWriteArgs struct {
	ID          string          `json:"id"`
	BaseVersion json.RawMessage `json:"base_version"`
	Text        *string         `json:"text"`
	Title       *string         `json:"title"`
}

type docReadArgs struct {
	ID      string          `json:"id"`
	Version json.RawMessage `json:"version"`
	Screen  *bool           `json:"screen"`
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

func parseDocCreate(raw json.RawMessage) (docCreateArgs, error) {
	var a docCreateArgs
	if err := StrictObject(raw, &a); err != nil {
		return a, err
	}
	if err := docText(a.Text, a.Title, true); err != nil {
		return a, err
	}
	if a.Group != "" && !docGroupRE.MatchString(a.Group) {
		return a, refusal("doc_group_not_found")
	}
	return a, nil
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
	if !contentIDRE.MatchString(a.ID) {
		return a, 0, badArg(contentIDRule)
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

func (d *docs) Quote(c Call) (Quote, error) {
	switch c.Method {
	case "create":
		a, err := parseDocCreate(c.Args)
		if err != nil {
			return Quote{}, err
		}
		return Quote{Resource: allowance.Credit, Max: c.Price.For(int64(len(*a.Text)))}, nil
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
		return Quote{Resource: allowance.Credit, Max: c.Price.For(0)}, nil
	}
	return Quote{}, refusal("invalid_service_data")
}

// ModeFor runs a read that asks for screening after commit; everything else
// in the command's transaction.
func (d *docs) ModeFor(c Call) Mode {
	if c.Method != "read" {
		return Local
	}
	a, _, err := parseDocRead(c.Args)
	if err != nil {
		return Local
	}
	return d.screen.callMode(a.Screen)
}

// Admit bounds writes and reads per account, and refuses a read of a doc
// the caller may not read before anything is reserved.
func (d *docs) Admit(ctx context.Context, q allowance.Querier, c Call) error {
	if c.Method != "read" {
		return d.writes.admit(c.Subject.ID, contentWritesPerMinute, c.Now)
	}
	if err := d.reads.admit(c.Subject.ID, contentReadsPerMinute, c.Now); err != nil {
		return err
	}
	a, _, err := parseDocRead(c.Args)
	if err != nil {
		return err
	}
	_, err = d.readable(ctx, q, a.ID, c.Subject.ID)
	return err
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
}

type docRow struct {
	DocView
	account string
	stored  int64
}

const docColumns = "id,title,room,version,hash,bytes,updated_by,created_at,updated_at,account,stored"

func scanDoc(row interface{ Scan(...any) error }) (docRow, error) {
	var r docRow
	err := row.Scan(&r.ID, &r.Title, &r.Owner.Group, &r.Version, &r.Hash, &r.Bytes, &r.UpdatedBy, &r.CreatedAt, &r.UpdatedAt, &r.account, &r.stored)
	r.Owner.Kind = "key"
	if r.Owner.Group != "" {
		r.Owner.Kind = "group"
	}
	return r, err
}

// readable is the doc account may read and write: its key's own, or a
// group's it is an active member of now. Anything else is doc_not_found.
func (d *docs) readable(ctx context.Context, q allowance.Querier, id, account string) (docRow, error) {
	r, err := scanDoc(q.QueryRowContext(ctx, "SELECT "+docColumns+" FROM docs WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, refusal("doc_not_found")
	}
	if err != nil {
		return r, err
	}
	if r.Owner.Group == "" {
		if r.account != account {
			return r, refusal("doc_not_found")
		}
		return r, nil
	}
	ok, err := d.member(ctx, q, account, r.Owner.Group)
	if err != nil {
		return r, err
	}
	if !ok {
		return r, refusal("doc_not_found")
	}
	return r, nil
}

func (d *docs) member(ctx context.Context, q allowance.Querier, account, group string) (bool, error) {
	if d.board == nil || !docGroupRE.MatchString(group) {
		return false, nil
	}
	return d.board.Member(ctx, q, account, group)
}

// ownerUsage is the docs and stored bytes of a key (group "") or a group.
func ownerUsage(ctx context.Context, q allowance.Querier, account, group string) (count, stored int64, err error) {
	if group == "" {
		err = q.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(stored),0) FROM docs WHERE account=? AND room=''", account).Scan(&count, &stored)
	} else {
		err = q.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(stored),0) FROM docs WHERE room=?", group).Scan(&count, &stored)
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

// DocConflict is a 409 doc_conflict's details: the doc as it stands, whose
// version a write must name to go through. Its text is read with docs.read.
type DocConflict struct {
	Current DocView `json:"current"`
}

func (d *docs) Run(ctx context.Context, tx *sql.Tx, c Call) (Result, error) {
	if c.Method == "read" {
		var q allowance.Querier = tx
		if tx == nil {
			if d.engine == nil {
				return Result{}, errors.New("docs: no engine for a remote read")
			}
			q = d.engine.cfg.DB
		}
		return d.read(ctx, q, c, tx == nil)
	}
	if tx == nil {
		return Result{}, errors.New("docs: writes run only inside the command's transaction")
	}
	switch c.Method {
	case "create":
		a, err := parseDocCreate(c.Args)
		if err != nil {
			return Result{}, err
		}
		if a.Group != "" {
			ok, err := d.member(ctx, tx, c.Subject.ID, a.Group)
			if err != nil {
				return Result{}, err
			}
			if !ok {
				return Result{}, refusal("doc_group_not_found")
			}
		}
		count, stored, err := ownerUsage(ctx, tx, c.Subject.ID, a.Group)
		if err != nil {
			return Result{}, err
		}
		size := int64(len(*a.Text))
		if count >= DocsPerOwner || stored+size > DocBytesMax {
			return Result{}, refusal("doc_limit")
		}
		id, hash := newCallID(), sha256Of([]byte(*a.Text))
		if _, err = tx.ExecContext(ctx, "INSERT INTO docs(id,account,room,title,version,hash,bytes,stored,updated_by,created_at,updated_at) VALUES(?,?,?,?,1,?,?,?,?,?,?)",
			id, c.Subject.ID, a.Group, *a.Title, hash, size, size, c.Subject.KeyID, c.Now, c.Now); err != nil {
			return Result{}, err
		}
		vid, err := insertVersion(ctx, tx, id, 1, *a.Title, *a.Text, hash, c.Subject, c.Now)
		if err != nil {
			return Result{}, err
		}
		return d.written(ctx, tx, c, id, vid, size)
	case "write":
		a, base, err := parseDocWrite(c.Args)
		if err != nil {
			return Result{}, err
		}
		r, err := d.readable(ctx, tx, a.ID, c.Subject.ID)
		if err != nil {
			return Result{}, err
		}
		if base != r.Version {
			return Result{}, &allowance.Err{Code: "doc_conflict", Details: DocConflict{Current: r.DocView}}
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
		res, err := tx.ExecContext(ctx, "UPDATE docs SET title=?, version=?, hash=?, bytes=?, stored=stored+?, updated_by=?, updated_at=? WHERE id=? AND version=?",
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
		return d.written(ctx, tx, c, r.ID, vid, size)
	}
	return Result{}, refusal("invalid_service_data")
}

// written is a create's or a write's answer: the doc as it now stands and
// the new version's id, its leaf's ref in the transparency log.
func (d *docs) written(ctx context.Context, tx *sql.Tx, c Call, id, versionID string, size int64) (Result, error) {
	r, err := scanDoc(tx.QueryRowContext(ctx, "SELECT "+docColumns+" FROM docs WHERE id=?", id))
	if err != nil {
		return Result{}, err
	}
	body, _ := json.Marshal(map[string]any{"doc": r.DocView, "version_id": versionID,
		"log": "/api/log/proof?message=" + versionID})
	public, _ := json.Marshal(map[string]any{"bytes": size})
	return Result{Body: body, Used: c.Price.For(size), Public: public}, nil
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

// read answers docs.read: the version's record and, unless screening
// withheld it, its text in the first answer only (Result.Once). remote is
// true after commit, where a fresh screen may ask the classifier.
func (d *docs) read(ctx context.Context, q allowance.Querier, c Call, remote bool) (Result, error) {
	a, version, err := parseDocRead(c.Args)
	if err != nil {
		return Result{}, err
	}
	r, err := d.readable(ctx, q, a.ID, c.Subject.ID)
	if err != nil {
		return Result{}, err
	}
	if version == 0 {
		version = r.Version
	}
	var v DocVersion
	var text, author, verdict string
	var hosted bool
	err = q.QueryRowContext(ctx, "SELECT id,version,title,hash,bytes,author_key,created_at,text,author,hosted,verdict FROM doc_versions WHERE doc=? AND version=?", r.ID, version).
		Scan(&v.ID, &v.Version, &v.Title, &v.Hash, &v.Bytes, &v.Author, &v.CreatedAt, &text, &author, &hosted, &verdict)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, refusal("doc_version_not_found")
	}
	if err != nil {
		return Result{}, err
	}
	own := author == c.Subject.ID
	wants := d.screen.wants(a.Screen)
	state, fresh := "unavailable", (*TextVerdict)(nil)
	if !own && wants && verdict == "" && remote {
		payer := allowance.Subject{ID: author, KeyID: v.Author, Signed: true, Hosted: hosted}
		store := func(ctx context.Context, tx *sql.Tx, verdict string, cost, now int64) (bool, error) {
			res, err := tx.ExecContext(ctx, "UPDATE doc_versions SET verdict=?, screen_cost=?, screened_at=? WHERE id=? AND verdict=''", verdict, cost, now, v.ID)
			if err != nil {
				return false, err
			}
			n, err := res.RowsAffected()
			return n == 1, err
		}
		if fresh, state, err = d.screen.run(ctx, d.engine, "doc:"+v.ID, payer, text, ledger.Ref{Service: DocsID, Op: "screen", Method: "read"}, store); err != nil {
			return Result{}, err
		}
	}
	sc := screenFor(own, wants, verdict, state, fresh)
	out := map[string]any{"doc": r.DocView, "version": v, "own": own, "screened": sc.Screened, "screen": sc.Screen, "untrusted": !own, "rendered": false}
	if sc.Verdict != nil {
		out["verdict"] = sc.Verdict
	}
	if !own {
		out["note"] = UntrustedNote
	}
	res := Result{Used: c.Price.For(0), Public: json.RawMessage(`{}`)}
	if sc.Withheld {
		out["withheld"] = true
		out["withheld_note"] = "Screening flagged this version, or is still screening it: read it again shortly, or with screen: false to read it unscreened."
	} else {
		res.Once = canonicalJSON(map[string]string{"text": text})
	}
	res.Body, _ = json.Marshal(out)
	return res, nil
}

type docHistoryArgs struct {
	ID     string          `json:"id"`
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
		r, err := d.readable(ctx, q, a.ID, c.Subject.ID)
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
		out := map[string]any{"doc": r.DocView, "versions": versions}
		if r.Owner.Group != "" {
			out["note"] = "Titles are written by the group's members: " + UntrustedNote
		}
		if n := len(versions); int64(n) == limit && versions[n-1].Version > 1 {
			out["next_before"] = versions[n-1].Version
		}
		return json.Marshal(out)
	case "list":
		var a struct {
			Group string `json:"group"`
		}
		if err := StrictObject(c.Args, &a); err != nil {
			return nil, err
		}
		query, arg := "SELECT "+docColumns+" FROM docs WHERE account=? AND room='' ORDER BY created_at DESC, id LIMIT ?", c.Subject.ID
		if a.Group != "" {
			ok, err := d.member(ctx, q, c.Subject.ID, a.Group)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, refusal("doc_group_not_found")
			}
			query, arg = "SELECT "+docColumns+" FROM docs WHERE room=? ORDER BY created_at DESC, id LIMIT ?", a.Group
		}
		rows, err := q.QueryContext(ctx, query, arg, DocsPerOwner)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		list := []DocView{}
		for rows.Next() {
			r, err := scanDoc(rows)
			if err != nil {
				return nil, err
			}
			list = append(list, r.DocView)
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
		_, stored, err := ownerUsage(ctx, q, c.Subject.ID, a.Group)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"docs": list, "usage": map[string]any{"docs": len(list), "docs_max": DocsPerOwner, "bytes": stored, "bytes_max": DocBytesMax}}
		if a.Group != "" {
			out["note"] = "Titles are written by the group's members: " + UntrustedNote
		}
		return json.Marshal(out)
	}
	return nil, refusal("invalid_service_data")
}
