package services_test

import (
	"context"
	"strings"
	"testing"

	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
)

// The paste.* methods are aliases over docs: a paste made with paste.create
// reads, opens, lists and deletes the same through docs.*, and a doc made
// unlisted with docs.create opens like a paste.
func TestPasteAliasParity(t *testing.T) {
	r := newContentRig(t, services.ScreenOff)
	id := r.paste("alice", map[string]any{"text": "shared notes", "title": "Notes", "visibility": "unlisted", "expires_in": 3600, "show_author": true})

	// open: the same text, hash, author and screening either way.
	viaPaste, _, err := r.callAs(anon("n1"), "paste", "open", map[string]any{"id": id})
	if err != nil {
		t.Fatal(err)
	}
	viaDocs, _, err := r.callAs(anon("n2"), "docs", "open", map[string]any{"id": id})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"id", "title", "hash", "bytes", "created_at", "expires_at"} {
		if get(viaPaste, "result", "paste", f) != get(viaDocs, "result", "doc", f) || get(viaPaste, "result", "paste", f) == nil {
			t.Fatalf("open %s: paste %v, docs %v", f, get(viaPaste, "result", "paste", f), get(viaDocs, "result", "doc", f))
		}
	}
	for _, f := range []string{"text", "own", "screen", "screened", "untrusted", "rendered"} {
		if get(viaPaste, "result", f) != get(viaDocs, "result", f) {
			t.Fatalf("open %s: paste %v, docs %v", f, get(viaPaste, "result", f), get(viaDocs, "result", f))
		}
	}
	if get(viaDocs, "result", "doc", "author", "fingerprint") != "alice" || get(viaPaste, "result", "paste", "author", "fingerprint") != "alice" ||
		get(viaDocs, "result", "doc", "kind") != "paste" || get(viaDocs, "result", "doc", "version") != float64(1) {
		t.Fatalf("docs.open of a paste: %+v", viaDocs)
	}
	if get(viaPaste, "call", "cost") != get(viaDocs, "call", "cost") {
		t.Fatalf("open costs differ: %v %v", get(viaPaste, "call", "cost"), get(viaDocs, "call", "cost"))
	}

	// get and docs.read: the same text, by id and by hash.
	for _, args := range []map[string]any{{"id": id}, {"hash": sha256Hex("shared notes")}} {
		got, err := r.read("alice", "paste", "get", args)
		if err != nil {
			t.Fatal(err)
		}
		read := r.mustCall("alice", "docs", "read", args)
		if get(got, "result", "text") != "shared notes" || get(read, "result", "text") != "shared notes" || get(read, "result", "doc", "id") != id ||
			get(got, "result", "paste", "hash") != get(read, "result", "doc", "hash") || get(read, "result", "screen") != "own" {
			t.Fatalf("get %v: %+v, read %+v", args, got, read)
		}
	}

	// list: paste.list and docs.list kind paste name the same pastes, in
	// the same order with the same seqs; docs.list alone shows docs only.
	second := r.paste("alice", map[string]any{"text": "second"})
	pl, err := r.read("alice", "paste", "list", map[string]any{"limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := r.read("alice", "docs", "list", map[string]any{"kind": "paste", "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	pp, dd := get(pl, "result", "pastes").([]any), get(dl, "result", "docs").([]any)
	if len(pp) != 1 || len(dd) != 1 || pp[0].(map[string]any)["id"] != second || dd[0].(map[string]any)["id"] != second ||
		pp[0].(map[string]any)["seq"] != dd[0].(map[string]any)["seq"] || get(pl, "result", "next_before") != get(dl, "result", "next_before") || get(pl, "result", "next_before") == nil {
		t.Fatalf("paste.list %+v, docs.list kind paste %+v", pl, dl)
	}
	if docsOnly, err := r.read("alice", "docs", "list", map[string]any{}); err != nil || len(get(docsOnly, "result", "docs").([]any)) != 0 || get(docsOnly, "result", "usage", "docs") != float64(0) {
		t.Fatalf("docs.list shows pastes or counts them: %+v %v", docsOnly, err)
	}

	// A paste never takes a version; paste.* never reaches a doc.
	if _, err = r.call("alice", "docs", "write", map[string]any{"id": id, "base_version": 1, "text": "changed"}); code(err) != "doc_read_only" {
		t.Fatalf("a paste took a version: %v", err)
	}
	doc := r.doc("alice", map[string]any{"title": "Plan", "text": "a doc", "visibility": "unlisted"})
	if _, _, err = r.callAs(anon("n3"), "paste", "open", map[string]any{"id": doc}); code(err) != "paste_not_found" {
		t.Fatalf("paste.open opened a doc: %v", err)
	}
	if _, err = r.call("alice", "paste", "delete", map[string]any{"id": doc}); code(err) != "paste_not_found" {
		t.Fatalf("paste.delete took a doc: %v", err)
	}
	if _, err = r.read("alice", "paste", "get", map[string]any{"id": doc}); code(err) != "paste_not_found" {
		t.Fatalf("paste.get read a doc: %v", err)
	}

	// delete: docs.delete removes a paste's text like paste.delete, and the
	// paste answers deleted through either.
	out := r.mustCall("alice", "docs", "delete", map[string]any{"id": second})
	if get(out, "result", "doc", "state") != "deleted" {
		t.Fatalf("docs.delete of a paste: %+v", out)
	}
	if got, err := r.read("alice", "paste", "get", map[string]any{"id": second}); err != nil || get(got, "result", "paste", "state") != "deleted" || get(got, "result", "text") != nil {
		t.Fatalf("paste.get after docs.delete: %+v %v", got, err)
	}
	if again := r.mustCall("alice", "paste", "delete", map[string]any{"id": second}); get(again, "result", "paste", "state") != "deleted" {
		t.Fatalf("paste.delete after docs.delete: %+v", again)
	}
}

// docs.create takes the paste's sharing options: an unlisted doc opens for
// anyone holding its id until it expires, a private one for no one else, and
// a write shows in the next open. delete removes the text of every version.
func TestDocsUnlistedOpenExpiryAndDelete(t *testing.T) {
	r := newContentRig(t, services.ScreenOff)
	id := r.doc("alice", map[string]any{"title": "Status", "text": "v1", "visibility": "unlisted", "expires_in": 120})
	private := r.doc("alice", map[string]any{"title": "Mine", "text": "secret"})
	out, _, err := r.callAs(anon("n"), "docs", "open", map[string]any{"id": id})
	if err != nil || get(out, "result", "text") != "v1" || get(out, "result", "untrusted") != true || get(out, "result", "doc", "author") != nil {
		t.Fatalf("open: %+v %v", out, err)
	}
	r.mustCall("alice", "docs", "write", map[string]any{"id": id, "base_version": 1, "text": "v2"})
	if out, _, err = r.callAs(subjectOf("bob"), "docs", "open", map[string]any{"id": id}); err != nil || get(out, "result", "text") != "v2" || get(out, "result", "doc", "version") != float64(2) {
		t.Fatalf("open after a write: %+v %v", out, err)
	}
	for _, s := range []string{"bob", "anon"} {
		subject := subjectOf(s)
		if s == "anon" {
			subject = anon("n")
		}
		if _, _, err = r.callAs(subject, "docs", "open", map[string]any{"id": private}); code(err) != "doc_not_found" {
			t.Fatalf("%s opened a private doc: %v", s, err)
		}
	}
	if out = r.mustCall("alice", "docs", "open", map[string]any{"id": private}); get(out, "result", "screen") != "own" {
		t.Fatalf("the owner opens a private doc: %+v", out)
	}
	if _, err = r.call("bob", "docs", "read", map[string]any{"id": id}); code(err) != "doc_not_found" {
		t.Fatalf("unlisted opens, it does not read: %v", err)
	}
	r.now += 120
	if _, _, err = r.callAs(anon("n"), "docs", "open", map[string]any{"id": id}); code(err) != "doc_not_found" {
		t.Fatalf("an expired doc opened: %v", err)
	}
	if out = r.mustCall("alice", "docs", "read", map[string]any{"id": id}); get(out, "result", "doc", "expired") != true || get(out, "result", "text") != "v2" {
		t.Fatalf("the owner keeps an expired doc: %+v", out)
	}
	// Sharing options are a key-owned doc's.
	r.board.private["team"] = map[string]bool{"alice": true}
	for _, bad := range []map[string]any{
		{"title": "t", "text": "x", "group": "team", "visibility": "unlisted"},
		{"title": "t", "text": "x", "group": "team", "expires_in": 60},
		{"title": "t", "text": "x", "visibility": "public"},
	} {
		if _, err = r.call("alice", "docs", "create", bad); code(err) != "invalid_service_data" {
			t.Fatalf("%v: %v", bad, err)
		}
	}
	group := r.doc("alice", map[string]any{"title": "t", "text": "x", "group": "team"})
	if _, err = r.call("alice", "docs", "delete", map[string]any{"id": group}); code(err) != "invalid_service_data" {
		t.Fatalf("a group doc was deleted: %v", err)
	}
	if _, err = r.call("bob", "docs", "delete", map[string]any{"id": id}); code(err) != "doc_not_found" {
		t.Fatalf("a stranger deleted: %v", err)
	}
	// delete: every version's text goes, the record and its log hashes stay.
	out = r.mustCall("alice", "docs", "delete", map[string]any{"id": id})
	if get(out, "result", "doc", "state") != "deleted" {
		t.Fatalf("delete: %+v", out)
	}
	var texts, versions int
	if err = r.db.QueryRow("SELECT count(*), sum(text<>'') FROM doc_versions WHERE doc=?", id).Scan(&versions, &texts); err != nil || versions != 2 || texts != 0 {
		t.Fatalf("after delete: %d versions, %d with text, %v", versions, texts, err)
	}
	if _, err = r.call("alice", "docs", "read", map[string]any{"id": id}); code(err) != "doc_not_found" {
		t.Fatalf("a deleted doc read: %v", err)
	}
	if _, err = r.call("alice", "docs", "write", map[string]any{"id": id, "base_version": 2, "text": "v3"}); code(err) != "doc_not_found" {
		t.Fatalf("a deleted doc took a version: %v", err)
	}
	// Notary and price, as a paste's.
	out = r.mustCall("alice", "docs", "create", map[string]any{"title": "Stamped", "text": "ship it", "notary": true})
	if get(out, "result", "receipt", "hash") != sha256Hex("ship it") || get(out, "result", "doc", "notary_seq") == nil || get(out, "call", "cost") != float64(2+1+services.DocNotaryPrice) {
		t.Fatalf("notary: %+v", out)
	}
	// The download route's format=text takes docs.open; the text never
	// leaves as a page.
	if _, _, err = r.callAs(anon("n"), "docs", "open", map[string]any{"id": strings.Repeat("0", 32)}); code(err) != "doc_not_found" {
		t.Fatalf("an unknown id: %v", err)
	}
}

// TestOldPasteRowsResolve: rows of the old pastes table, from before pastes
// folded into docs, are copied in at startup with their ids, seqs, states
// and verdicts; every paste URL and method answers as it did; the copy runs
// again without change; the old table keeps its rows, and a removal reaches
// them too.
func TestOldPasteRowsResolve(t *testing.T) {
	db := openDB(t)
	unlisted, private, deleted, hidden := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32), strings.Repeat("d", 32)
	if _, err := db.Exec(`DROP TABLE docs; DROP TABLE pastes;
CREATE TABLE docs (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, room TEXT NOT NULL DEFAULT '', title TEXT NOT NULL,
 version INTEGER NOT NULL, hash TEXT NOT NULL, bytes INTEGER NOT NULL, stored INTEGER NOT NULL,
 updated_by TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
INSERT INTO docs(id,account,title,version,hash,bytes,stored,updated_by,created_at,updated_at) VALUES('` + strings.Repeat("e", 32) + `','alice','Old doc',1,'` + sha256Hex("doc") + `',3,3,'alice',1,1);
CREATE TABLE pastes (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, account TEXT NOT NULL, key_id TEXT NOT NULL DEFAULT '',
 hosted INTEGER NOT NULL DEFAULT 0, title TEXT NOT NULL DEFAULT '', text TEXT NOT NULL, bytes INTEGER NOT NULL, hash TEXT NOT NULL,
 visibility TEXT NOT NULL CHECK(visibility IN ('private','unlisted')), state TEXT NOT NULL CHECK(state IN ('active','deleted','hidden')),
 reason TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL DEFAULT 0, deleted_at INTEGER NOT NULL DEFAULT 0,
 notary_seq INTEGER NOT NULL DEFAULT 0, verdict TEXT NOT NULL DEFAULT '', screen_cost INTEGER NOT NULL DEFAULT 0,
 screened_at INTEGER NOT NULL DEFAULT 0, show_author INTEGER NOT NULL DEFAULT 0);
INSERT INTO pastes(seq,id,account,key_id,title,text,bytes,hash,visibility,state,created_at,verdict,show_author) VALUES
 (7,'` + unlisted + `','alice','alice','Log','old log',7,'` + sha256Hex("old log") + `','unlisted','active',100,'{"verdict":"pass","threshold":0.6,"categories":{}}',1),
 (8,'` + private + `','alice','alice','','mine',4,'` + sha256Hex("mine") + `','private','active',101,'',0),
 (9,'` + deleted + `','alice','alice','','',4,'` + sha256Hex("gone") + `','private','deleted',102,'',0),
 (10,'` + hidden + `','alice','alice','','a phishing page',15,'` + sha256Hex("a phishing page") + `','unlisted','hidden',103,'',0);
UPDATE pastes SET reason='phishing' WHERE id='` + hidden + `';
UPDATE pastes SET deleted_at=150 WHERE id='` + deleted + `';`); err != nil {
		t.Fatal(err)
	}
	migrate := func() {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err = services.MigratePastes(tx); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	counts := func() (docs, versions, pastes int) {
		if err := db.QueryRow("SELECT (SELECT count(*) FROM docs), (SELECT count(*) FROM doc_versions), (SELECT count(*) FROM pastes)").Scan(&docs, &versions, &pastes); err != nil {
			t.Fatal(err)
		}
		return
	}
	migrate()
	d1, v1, p1 := counts()
	migrate()
	if d2, v2, p2 := counts(); d1 != 5 || v1 != 4 || p1 != 4 || d2 != d1 || v2 != v1 || p2 != p1 {
		t.Fatalf("docs %d/%d, versions %d/%d, pastes %d/%d", d1, d2, v1, v2, p1, p2)
	}

	r := &contentRig{t: t, db: db, meter: servicestest.NewMeter(1 << 30), board: newFakeBoard(), jev: &fakeScreener{cost: 40}, now: wakeT0}
	reg := services.NewBuiltinRegistry([]string{"paste", "docs"}, services.Deps{DB: db, Board: r.board, ServiceID: "swarmmemo.com", TextScreener: r.jev})
	r.e = services.NewEngine(services.Config{DB: db, Registry: reg, Meter: r.meter, Now: func() int64 { return r.now }})
	t.Cleanup(r.e.Stop)

	// The unlisted paste opens by its old id through both names, with its
	// kept verdict (no new screen) and its author.
	for _, svc := range []string{"paste", "docs"} {
		out, _, err := r.callAs(anon("n-"+svc), svc, "open", map[string]any{"id": unlisted})
		key := map[string]string{"paste": "paste", "docs": "doc"}[svc]
		if err != nil || get(out, "result", "text") != "old log" || get(out, "result", "screen") != "done" || get(out, "result", key, "author", "fingerprint") != "alice" {
			t.Fatalf("%s.open of an old paste: %+v %v", svc, out, err)
		}
	}
	if r.jev.calls != 0 {
		t.Fatal("an old paste's kept verdict was not reused")
	}
	// Private, deleted and hidden stay as they were.
	for _, id := range []string{private, deleted, hidden} {
		if _, _, err := r.callAs(anon("n"), "paste", "open", map[string]any{"id": id}); code(err) != "paste_not_found" {
			t.Fatalf("%s opened: %v", id, err)
		}
	}
	got, err := r.read("alice", "paste", "get", map[string]any{"hash": sha256Hex("mine")})
	if err != nil || get(got, "result", "text") != "mine" || get(got, "result", "paste", "seq") != float64(8) {
		t.Fatalf("get by hash: %+v %v", got, err)
	}
	got, err = r.read("alice", "paste", "get", map[string]any{"id": hidden})
	if err != nil || get(got, "result", "paste", "state") != "hidden" || get(got, "result", "paste", "reason") != "phishing" || get(got, "result", "text") != "a phishing page" {
		t.Fatalf("a hidden paste: %+v %v", got, err)
	}
	got, err = r.read("alice", "paste", "get", map[string]any{"id": deleted})
	if err != nil || get(got, "result", "paste", "state") != "deleted" || get(got, "result", "paste", "deleted_at") != float64(150) || get(got, "result", "text") != nil {
		t.Fatalf("a deleted paste: %+v %v", got, err)
	}
	// A new paste numbers after the old ones; the list pages by the old seqs.
	fresh := r.paste("alice", map[string]any{"text": "new"})
	list, err := r.read("alice", "paste", "list", map[string]any{"limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	pastes := get(list, "result", "pastes").([]any)
	if len(pastes) != 2 || pastes[0].(map[string]any)["id"] != fresh || pastes[0].(map[string]any)["seq"] != float64(11) || pastes[1].(map[string]any)["seq"] != float64(10) || get(list, "result", "next_before") != float64(10) {
		t.Fatalf("list: %+v", list)
	}
	var legacy int
	if err = db.QueryRow("SELECT count(*) FROM pastes WHERE id=?", fresh).Scan(&legacy); err != nil || legacy != 0 {
		t.Fatalf("a new paste went to the old table: %d %v", legacy, err)
	}
	// The old doc is still a doc, and docs.list does not show pastes.
	if dl, err := r.read("alice", "docs", "list", map[string]any{}); err != nil || len(get(dl, "result", "docs").([]any)) != 1 {
		t.Fatalf("docs.list: %+v %v", dl, err)
	}
	// A removal reaches the old row too: its text goes there as well.
	r.mustCall("alice", "paste", "delete", map[string]any{"id": private})
	var text, state string
	if err = db.QueryRow("SELECT text, state FROM pastes WHERE id=?", private).Scan(&text, &state); err != nil || text != "" || state != "deleted" {
		t.Fatalf("the old row after delete: %q %q %v", text, state, err)
	}
	if _, err = services.HideDoc(context.Background(), db, unlisted, "spam", r.now); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT state FROM pastes WHERE id=?", unlisted).Scan(&state); err != nil || state != "hidden" {
		t.Fatalf("the old row after hide: %q %v", state, err)
	}
	if _, _, err = r.callAs(anon("n"), "docs", "open", map[string]any{"id": unlisted}); code(err) != "doc_not_found" {
		t.Fatalf("a hidden paste opened: %v", err)
	}
}
