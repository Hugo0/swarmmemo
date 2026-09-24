package board

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dataJSON(fields string) string { return `{"schema":1,` + fields + `}` }

func get(t *testing.T, s *Store, id string) Message {
	t.Helper()
	return run(t, s, Command{Operation: "message.get", MessageID: id}).Messages[0]
}

func TestPostDataIsSignedAndStrict(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(201)
	markdown := dataJSON(`"format":"markdown"`)
	fails(t, s, Command{Operation: "post", Text: "# Hi", Data: markdown}, "signature_required")
	for _, bad := range []string{
		`{"schema":1}`,
		`{"schema":2,"format":"markdown"}`,
		`{"format":"markdown"}`,
		`{"schema":1,"format":"html"}`,
		`{"schema":1,"format":null}`,
		`{"schema":1,"format":"markdown","format":"markdown"}`,
		`{"schema":1,"format":"markdown","theme":"x"}`,
		`{"schema":1,"supersedes":"not-an-id"}`,
		`{"schema":1.0,"format":"markdown"}`,
		`{"schema":1,"format":"markdown"} {}`,
		`[1]`,
		`{"schema":1,"format":"markdown","pad":"` + strings.Repeat("x", 1100) + `"}`,
	} {
		fails(t, s, signed(key, Command{Operation: "post", Text: "# Hi", Data: bad}), "invalid_post_data")
	}
	receipt := run(t, s, signed(key, Command{Operation: "post", Text: "# Hi\n\nBody", Data: markdown})).Receipt
	m := get(t, s, receipt.ID)
	if m.Format != "markdown" || m.Supersedes != "" || m.SupersededBy != "" || !strings.Contains(m.SignedPayload, `"data":"{\"schema\":1,\"format\":\"markdown\"}"`) {
		t.Fatalf("stored markdown post: %+v", m)
	}
	// Plain posts are unchanged: no format, and no data in the signed bytes.
	plain := get(t, s, run(t, s, signed(key, Command{Operation: "post", Text: "plain"})).Receipt.ID)
	if plain.Format != "" || strings.Contains(plain.SignedPayload, `"data"`) {
		t.Fatalf("plain post changed: %+v", plain)
	}
	// A removed post keeps no trace of how its body was written.
	if err := s.Moderate(testContext, receipt.ID, "test", true); err != nil {
		t.Fatal(err)
	}
	if m = get(t, s, receipt.ID); m.Format != "" || m.Text != "" {
		t.Fatalf("tombstone kept payload metadata: %+v", m)
	}
}

func TestSupersession(t *testing.T) {
	s := openTest(t, Config{})
	author, stranger := keyFor(202), keyFor(203)
	v1 := run(t, s, signed(author, Command{Operation: "post", Room: "guides", Page: "howto", Text: "# Guide\n\nFirst draft.", Data: dataJSON(`"format":"markdown"`)})).Receipt
	reply := run(t, s, signed(stranger, Command{Operation: "post", Room: "guides", Page: "howto", Text: "nice", ReplyTo: v1.ID})).Receipt
	supersede := func(target, text string) Command {
		return signed(author, Command{Operation: "post", Room: "guides", Page: "howto", Text: text, Data: dataJSON(`"format":"markdown","supersedes":"` + target + `"`)})
	}
	// Only the signing key, only in place.
	fails(t, s, signed(stranger, Command{Operation: "post", Room: "guides", Page: "howto", Text: "hijack", Data: dataJSON(`"supersedes":"` + v1.ID + `"`)}), "supersede_forbidden")
	fails(t, s, signed(author, Command{Operation: "post", Room: "guides", Page: "other", Text: "moved", Data: dataJSON(`"supersedes":"` + v1.ID + `"`)}), "supersede_mismatch")
	fails(t, s, signed(author, Command{Operation: "post", Room: "guides", Page: "howto", ReplyTo: reply.ID, Text: "reparented", Data: dataJSON(`"supersedes":"` + v1.ID + `"`)}), "supersede_mismatch")
	run(t, s, signed(author, Command{Operation: "post", Room: "elsewhere", Text: "elsewhere"}))
	fails(t, s, signed(author, Command{Operation: "post", Room: "elsewhere", Text: "cross-room", Data: dataJSON(`"supersedes":"` + v1.ID + `"`)}), "not_found")
	fails(t, s, signed(author, Command{Operation: "post", Room: "guides", Page: "howto", Text: "ghost", Data: dataJSON(`"supersedes":"` + strings.Repeat("a", 32) + `"`)}), "not_found")
	anonymous := run(t, s, Command{Operation: "post", Room: "guides", Page: "howto", Text: "anon"}).Receipt
	fails(t, s, signed(author, Command{Operation: "post", Room: "guides", Page: "howto", Text: "not yours", Data: dataJSON(`"supersedes":"` + anonymous.ID + `"`)}), "supersede_forbidden")

	command := supersede(v1.ID, "# Guide\n\nSecond draft.")
	v2 := run(t, s, command).Receipt
	// An exact retry is the same acceptance, not a third version.
	if again := run(t, s, command).Receipt; again.ID != v2.ID || !again.Duplicate {
		t.Fatalf("retry made a new version: %+v", again)
	}
	// Versions form a line: the old head cannot be forked.
	fails(t, s, supersede(v1.ID, "fork"), "already_superseded")

	old, current := get(t, s, v1.ID), get(t, s, v2.ID)
	if old.SupersededBy != v2.ID || old.Supersedes != "" || old.Hash != v1.Hash || old.Text != "# Guide\n\nFirst draft." {
		t.Fatalf("old version changed: %+v", old)
	}
	if current.Supersedes != v1.ID || current.SupersededBy != "" || current.Origin() != v1.ID {
		t.Fatalf("new version links: %+v", current)
	}
	// Every version, and a reply to any version, belongs to the original's thread.
	late := run(t, s, signed(stranger, Command{Operation: "post", Room: "guides", Page: "howto", Text: "on v2", ReplyTo: v2.ID})).Receipt
	for _, id := range []string{v1.ID, v2.ID, late.ID, reply.ID} {
		thread := run(t, s, Command{Operation: "thread.get", MessageID: id})
		if thread.Data["root_id"] != v1.ID {
			t.Fatalf("thread of %s rooted at %v", id, thread.Data["root_id"])
		}
		ids := []string{}
		for _, m := range thread.Messages {
			ids = append(ids, m.ID)
		}
		if strings.Join(ids, ",") != strings.Join([]string{v1.ID, reply.ID, v2.ID, late.ID}, ",") {
			t.Fatalf("thread membership %v", ids)
		}
	}
	// A version of a reply stays where the reply was.
	replyV2 := run(t, s, signed(stranger, Command{Operation: "post", Room: "guides", Page: "howto", Text: "nicer", ReplyTo: v1.ID, Data: dataJSON(`"supersedes":"` + reply.ID + `"`)})).Receipt
	if got := len(run(t, s, Command{Operation: "thread.get", MessageID: v1.ID}).Messages); got != 5 {
		t.Fatalf("thread with a reply version has %d messages", got)
	}

	current2, err := s.PublicCurrentVersions(testContext, []string{v1.ID, reply.ID, late.ID})
	if err != nil {
		t.Fatal(err)
	}
	if current2[v1.ID].Message.ID != v2.ID || current2[v1.ID].Versions != 2 || current2[reply.ID].Message.ID != replyV2.ID || len(current2) != 2 {
		t.Fatalf("current versions: %+v", current2)
	}
	history, err := s.PublicVersions(testContext, v2.ID)
	if err != nil || len(history) != 2 || history[0].ID != v1.ID || history[1].ID != v2.ID {
		t.Fatalf("history: %v %+v", err, history)
	}

	// The chain is bounded.
	head := v2.ID
	for n := 3; n <= MaxVersions; n++ {
		head = run(t, s, supersede(head, "# Guide\n\nDraft")).Receipt.ID
	}
	fails(t, s, supersede(head, "# Guide\n\nOne too many"), "version_limit")
	if history, _ = s.PublicVersions(testContext, v1.ID); len(history) != MaxVersions {
		t.Fatalf("history length %d", len(history))
	}
}

// sitemapArticles is the Markdown thread roots the sitemap lists, each at its
// newest version under its first ID.
func sitemapArticles(t *testing.T, s *Store) []Message {
	t.Helper()
	var articles []Message
	if err := s.PublicSitemapPosts(testContext, 0, 1000, func(m Message) {
		if m.Format == PostFormatMarkdown {
			articles = append(articles, m)
		}
	}); err != nil {
		t.Fatal(err)
	}
	return articles
}

func TestSupersessionExportAndArticles(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1})
	author := keyFor(204)
	markdown := dataJSON(`"format":"markdown"`)
	run(t, s, signed(author, Command{Operation: "room.create", Room: "vault", Visibility: "private"}))
	run(t, s, signed(author, Command{Operation: "post", Room: "vault", Text: "# Private", Data: markdown}))
	v1 := run(t, s, signed(author, Command{Operation: "post", Room: "guides", Text: "# Article\n\nOne.", Data: markdown})).Receipt
	run(t, s, signed(author, Command{Operation: "post", Room: "guides", Text: "# Reply", ReplyTo: v1.ID, Data: markdown}))
	run(t, s, signed(author, Command{Operation: "post", Room: "guides", Text: "plain root"}))
	run(t, s, signed(author, Command{Operation: "post", Room: "guides", Kind: "simulation", Text: "# Sim", Data: markdown}))
	s.now = func() time.Time { return time.Unix(testTime+60, 0) }
	v2 := run(t, s, signed(author, Command{Operation: "post", Room: "guides", Text: "# Article v2\n\nTwo.", Timestamp: testTime + 60, Data: dataJSON(`"format":"markdown","supersedes":"` + v1.ID + `"`)})).Receipt

	articles := sitemapArticles(t, s)
	if len(articles) != 1 || articles[0].ID != v1.ID || !strings.HasPrefix(articles[0].Text, "# Article v2") || articles[0].CreatedAt != testTime+60 {
		t.Fatalf("articles: %+v", articles)
	}

	export := run(t, s, Command{Operation: "export", Before: testTime + 120})
	byID := map[string]map[string]any{}
	for _, m := range export.Messages {
		encoded, _ := json.Marshal(m)
		var row map[string]any
		_ = json.Unmarshal(encoded, &row)
		byID[m.ID] = row
	}
	if row := byID[v1.ID]; row["format"] != "markdown" || row["superseded_by"] != nil || row["supersedes"] != nil {
		t.Fatalf("original export row: %v", row)
	}
	if row := byID[v2.ID]; row["supersedes"] != v1.ID || row["format"] != "markdown" {
		t.Fatalf("version export row: %v", row)
	}
	// The live read still carries the derived pointer.
	if get(t, s, v1.ID).SupersededBy != v2.ID {
		t.Fatal("live read lost superseded_by")
	}
}

// Schema 11 adds three columns and indexes. A schema 10 database, including its
// existing rows, must open, gain them, and read back unchanged.
func TestSchema11PostDataMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema10.sqlite")
	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	legacy := run(t, s, Command{Operation: "post", Text: "from schema 10"}).Receipt
	if _, err = s.db.Exec(`DROP INDEX events_supersedes; DROP INDEX events_origin; DROP INDEX events_articles;
 ALTER TABLE events DROP COLUMN format; ALTER TABLE events DROP COLUMN supersedes; ALTER TABLE events DROP COLUMN origin;
 PRAGMA user_version=10`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	if got := sqlCount(t, s, "PRAGMA user_version"); got != SchemaVersion {
		t.Fatalf("user_version %d", got)
	}
	if got := sqlCount(t, s, "SELECT count(*) FROM pragma_index_list('events') WHERE name IN ('events_supersedes','events_origin','events_articles')"); got != 3 {
		t.Fatalf("indexes after migration: %d", got)
	}
	if m := get(t, s, legacy.ID); m.Text != "from schema 10" || m.Format != "" || m.Supersedes != "" {
		t.Fatalf("legacy row: %+v", m)
	}
	key := keyFor(205)
	v1 := run(t, s, signed(key, Command{Operation: "post", Text: "a", Data: dataJSON(`"format":"markdown"`)})).Receipt
	run(t, s, signed(key, Command{Operation: "post", Text: "b", Data: dataJSON(`"supersedes":"` + v1.ID + `"`)}))
}

// A hide on any version removes the message: hiding the original (the ID the
// web shows for an edited post) stops further versions and drops the post
// from the article sitemap and the guide redirects.
func TestHiddenOriginalStopsVersions(t *testing.T) {
	s := openTest(t, Config{})
	owner, author := keyFor(141), keyFor(142)
	register(t, s, owner)
	register(t, s, author)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "club"}))
	v1 := run(t, s, signed(author, Command{Operation: "post", Room: "club", Text: "benign"})).Receipt.ID
	v2 := run(t, s, signed(author, Command{Operation: "post", Room: "club", Text: "abusive", Data: dataJSON(`"supersedes":"` + v1 + `"`)})).Receipt.ID
	run(t, s, signed(owner, Command{Operation: "room.hide", MessageID: v1, Reason: "abuse"}))
	fails(t, s, signed(author, Command{Operation: "post", Room: "club", Text: "more", Data: dataJSON(`"supersedes":"` + v2 + `"`)}), "supersede_hidden")

	a1 := run(t, s, signed(author, Command{Operation: "post", Room: "blog", Text: "# T\n\nok", Data: dataJSON(`"format":"markdown"`)})).Receipt.ID
	run(t, s, signed(author, Command{Operation: "post", Room: "blog", Text: "# T\n\nspam", Data: dataJSON(`"format":"markdown","supersedes":"` + a1 + `"`)}))
	if err := s.Moderate(testContext, a1, "spam", true); err != nil {
		t.Fatal(err)
	}
	articles := sitemapArticles(t, s)
	guides, err := s.PublicRoomArticles(testContext, "blog", []string{keyID(author)}, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range append(articles, guides...) {
		if a.Origin() == a1 {
			t.Fatalf("hidden article listed through its edit %s", a.ID)
		}
	}
}
