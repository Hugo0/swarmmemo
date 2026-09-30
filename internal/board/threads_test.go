package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func conversationPost(t *testing.T, s *Store, room, page, parent, kind, text string) string {
	t.Helper()
	return run(t, s, Command{Operation: "post", Room: room, Page: page, ReplyTo: parent, Kind: kind, Text: text}).Receipt.ID
}

func eventIDs(events []Message) []string {
	ids := []string{}
	for _, event := range events {
		ids = append(ids, event.ID)
	}
	return ids
}

func TestThreadChronologicalPaginationAndChildAnchor(t *testing.T) {
	s := openTest(t, Config{})
	root := conversationPost(t, s, "work", "main", "", "request", "Review this proposal")
	left := conversationPost(t, s, "work", "main", root, "note", "First reply")
	conversationPost(t, s, "work", "unrelated", "", "note", "Not in the thread")
	right := conversationPost(t, s, "work", "other-page", root, "note", "Second reply")
	leaf := conversationPost(t, s, "work", "other-page", left, "result", "A nested result")
	conversationPost(t, s, "another", "main", "", "note", "Another room")
	first := run(t, s, Command{Operation: "thread.get", MessageID: leaf, Limit: 2})
	if got, want := eventIDs(first.Messages), []string{root, left}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first page=%v want %v", got, want)
	}
	if first.Data["root_id"] != root || first.Data["requested_message_id"] != leaf || first.Data["room"] != "work" || first.Data["has_more"] != true {
		t.Fatalf("thread metadata: %+v", first.Data)
	}
	second := run(t, s, Command{Operation: "thread.get", MessageID: root, Cursor: first.NextCursor, Limit: 2})
	if got, want := eventIDs(second.Messages), []string{right, leaf}; !reflect.DeepEqual(got, want) {
		t.Fatalf("second page=%v want %v", got, want)
	}
	if second.Data["has_more"] != false || second.NextCursor == "" {
		t.Fatal("final page must retain a polling cursor")
	}
	newReply := conversationPost(t, s, "work", "main", left, "note", "Reply after traversal")
	third := run(t, s, Command{Operation: "thread.get", MessageID: right, Cursor: second.NextCursor, Limit: 2})
	if got := eventIDs(third.Messages); !reflect.DeepEqual(got, []string{newReply}) {
		t.Fatalf("new branch reply skipped: %v", got)
	}
	empty := run(t, s, Command{Operation: "thread.get", MessageID: root, Cursor: third.NextCursor})
	if len(empty.Messages) != 0 || empty.Data["has_more"] != false {
		t.Fatalf("unexpected repeated events: %+v", empty)
	}
	all := run(t, s, Command{Operation: "thread.get", MessageID: root, Cursor: "start", Limit: 200})
	if got := eventIDs(all.Messages); !reflect.DeepEqual(got, []string{root, left, right, leaf, newReply}) {
		t.Fatalf("full transcript=%v", got)
	}
}

func TestThreadCursorScopeAndGeneration(t *testing.T) {
	s := openTest(t, Config{})
	firstRoot := conversationPost(t, s, "one", "main", "", "note", "One")
	secondRoot := conversationPost(t, s, "one", "main", "", "note", "Two")
	conversationPost(t, s, "one", "z-last", "", "note", "Page Z")
	thread := run(t, s, Command{Operation: "thread.get", MessageID: firstRoot})
	fails(t, s, Command{Operation: "thread.get", MessageID: secondRoot, Cursor: thread.NextCursor}, "invalid_cursor")
	fails(t, s, Command{Operation: "messages.list", Cursor: thread.NextCursor}, "invalid_cursor")
	fails(t, s, Command{Operation: "thread.get", MessageID: firstRoot, Cursor: s.cursor(0)}, "invalid_cursor")
	fails(t, s, Command{Operation: "thread.get", MessageID: firstRoot, Cursor: s.cursorFor(0, 1)}, "invalid_cursor")
	pages := run(t, s, Command{Operation: "room.pages", Room: "one", Limit: 1})
	fails(t, s, Command{Operation: "thread.get", MessageID: firstRoot, Cursor: pages.NextCursor}, "invalid_cursor")
	fails(t, s, Command{Operation: "room.pages", Room: "one", Cursor: thread.NextCursor}, "invalid_cursor")
	if err := s.RotateGeneration(testContext); err != nil {
		t.Fatal(err)
	}
	fails(t, s, Command{Operation: "thread.get", MessageID: firstRoot, Cursor: thread.NextCursor}, "cursor_reset")
}

func TestPrivateThreadAuthorizationAndHiddenRoot(t *testing.T) {
	s := openTest(t, Config{})
	owner, member, outsider := keyFor(31), keyFor(32), keyFor(33)
	register(t, s, owner)
	register(t, s, member)
	register(t, s, outsider)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "secret", Visibility: "private", Members: []string{keyID(member)}}))
	root := run(t, s, signed(owner, Command{Operation: "post", Room: "secret", Text: "Private root secret"})).Receipt.ID
	child := run(t, s, signed(member, Command{Operation: "post", Room: "secret", Text: "Private response", ReplyTo: root})).Receipt.ID
	for _, id := range []string{root, child, "missing"} {
		for _, command := range []Command{{Operation: "thread.get", MessageID: id}, signed(outsider, Command{Operation: "thread.get", MessageID: id, Cursor: "invalid"})} {
			_, err := s.Execute(testContext, command, "test-origin")
			var be *Error
			if !errors.As(err, &be) || be.Status != 404 || be.Code != "not_found" || be.Message != "Thread not found." {
				t.Fatalf("existence-dependent unauthorized error: %v", err)
			}
		}
	}
	allowed := run(t, s, signed(member, Command{Operation: "thread.get", MessageID: child, Limit: 1}))
	if !reflect.DeepEqual(eventIDs(allowed.Messages), []string{root}) {
		t.Fatal("member cannot read root")
	}
	if err := s.Moderate(testContext, root, "Review complete", true); err != nil {
		t.Fatal(err)
	}
	hidden := run(t, s, signed(member, Command{Operation: "thread.get", MessageID: child}))
	if len(hidden.Messages) != 2 || !hidden.Messages[0].Hidden || hidden.Messages[0].Text != "" || hidden.Messages[0].Signature != "" || hidden.Messages[0].SignedPayload != "" || hidden.Messages[1].Text != "Private response" {
		t.Fatalf("hidden root or descendants incorrect: %+v", hidden.Messages)
	}
	run(t, s, signed(owner, Command{Operation: "room.member.remove", Room: "secret", Target: keyID(member)}))
	fails(t, s, signed(member, Command{Operation: "thread.get", MessageID: root, Cursor: allowed.NextCursor}), "not_found")
}

func TestRoomPageDirectorySnapshotAndModeration(t *testing.T) {
	s := openTest(t, Config{})
	conversationPost(t, s, "project", "alpha", "", "note", "Alpha first")
	conversationPost(t, s, "project", "alpha", "", "note", "Alpha second")
	conversationPost(t, s, "project", "gamma", "", "note", "Gamma")
	removed := conversationPost(t, s, "project", "secret-name", "", "note", "Hidden only")
	if err := s.Moderate(testContext, removed, "Removed", true); err != nil {
		t.Fatal(err)
	}
	first := run(t, s, Command{Operation: "room.pages", Room: "project", Limit: 1})
	pages := first.Data["pages"].([]PageSummary)
	if len(pages) != 1 || pages[0].Name != "alpha" || pages[0].Count != 2 || pages[0].UpdatedAt != testTime || first.Data["has_more"] != true {
		t.Fatalf("directory first page: %+v", first.Data)
	}
	conversationPost(t, s, "project", "beta", "", "note", "Created after directory snapshot")
	conversationPost(t, s, "project", "alpha", "", "note", "Also after snapshot")
	next := run(t, s, Command{Operation: "room.pages", Room: "project", Limit: 1, Cursor: first.NextCursor})
	pages = next.Data["pages"].([]PageSummary)
	if len(pages) != 1 || pages[0].Name != "gamma" || next.Data["has_more"] != false || next.NextCursor != "" {
		t.Fatalf("snapshot directory leaked inserted/hidden page: %+v", next)
	}
	fresh := run(t, s, Command{Operation: "room.pages", Room: "project", Cursor: "start"})
	pages = fresh.Data["pages"].([]PageSummary)
	if len(pages) != 3 || pages[0].Count != 3 || pages[1].Name != "beta" {
		t.Fatalf("new traversal did not pick up new pages: %+v", pages)
	}
	conversationPost(t, s, "other", "main", "", "note", "Other room")
	fails(t, s, Command{Operation: "room.pages", Room: "other", Cursor: first.NextCursor}, "invalid_cursor")
	fails(t, s, Command{Operation: "room.pages"}, "invalid_slug")
	fails(t, s, Command{Operation: "room.pages", Room: "project", Kind: "note"}, "unexpected_field")
}

func TestPrivatePageDirectoryAndHiddenOnlyPages(t *testing.T) {
	s := openTest(t, Config{})
	owner, other := keyFor(41), keyFor(42)
	register(t, s, owner)
	register(t, s, other)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "private-pages", Visibility: "private"}))
	hidden := run(t, s, signed(owner, Command{Operation: "post", Room: "private-pages", Page: "hidden-page", Text: "Removed"})).Receipt.ID
	run(t, s, signed(owner, Command{Operation: "post", Room: "private-pages", Page: "visible-page", Text: "Visible to members"}))
	if err := s.Moderate(testContext, hidden, "Removed", true); err != nil {
		t.Fatal(err)
	}
	fails(t, s, Command{Operation: "room.pages", Room: "private-pages"}, "not_found")
	fails(t, s, signed(other, Command{Operation: "room.pages", Room: "private-pages", Cursor: "invalid"}), "not_found")
	allowed := run(t, s, signed(owner, Command{Operation: "room.pages", Room: "private-pages"})).Data["pages"].([]PageSummary)
	if len(allowed) != 1 || allowed[0].Name != "visible-page" {
		t.Fatalf("private directory exposed hidden page: %+v", allowed)
	}
}

func TestInboxKeyRotationContinuityAndKindFilter(t *testing.T) {
	s := openTest(t, Config{})
	old, next, sender := keyFor(51), keyFor(52), keyFor(53)
	register(t, s, old)
	register(t, s, sender)
	first := run(t, s, Command{Operation: "post", Room: "work", Page: "tasks", Kind: "request", To: keyID(old), Text: "Before rotation"}).Receipt.ID
	rotate := signed(old, Command{Operation: "agent.rotate", Target: base64.RawURLEncoding.EncodeToString(next.Public().(ed25519.PublicKey))})
	rotate.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(next, Canonical("swarmmemo.com", rotate)))
	run(t, s, rotate)
	second := run(t, s, Command{Operation: "post", Room: "work", Page: "tasks", Kind: "request", To: keyID(next), Text: "After rotation"}).Receipt.ID
	run(t, s, Command{Operation: "post", Room: "work", Page: "tasks", Kind: "note", To: keyID(old), Text: "Other kind"})
	run(t, s, signed(next, Command{Operation: "room.create", Room: "private-inbox", Visibility: "private"}))
	private := run(t, s, signed(next, Command{Operation: "post", Room: "private-inbox", Kind: "request", To: keyID(old), Text: "Private inbox message"})).Receipt.ID
	for _, target := range []string{keyID(old), keyID(next)} {
		public := run(t, s, Command{Operation: "messages.list", To: target, Kind: "request", Cursor: "start"})
		if !reflect.DeepEqual(eventIDs(public.Messages), []string{first, second}) {
			t.Fatalf("public rotation inbox %s: %v", target, eventIDs(public.Messages))
		}
		if public.Messages[0].To != keyID(old) || public.Messages[1].To != keyID(next) {
			t.Fatal("stored recipient rewritten by continuity lookup")
		}
		member := run(t, s, signed(next, Command{Operation: "messages.list", To: target, Kind: "request", Cursor: "start"}))
		if !reflect.DeepEqual(eventIDs(member.Messages), []string{first, second, private}) {
			t.Fatal("signed inbox did not preserve authorized private results")
		}
	}
	filtered := run(t, s, Command{Operation: "messages.list", Room: "work", Page: "tasks", To: keyID(next), Kind: "request", Query: "After", Cursor: "start", Limit: 1})
	if !reflect.DeepEqual(eventIDs(filtered.Messages), []string{second}) {
		t.Fatal("combined filter mismatch")
	}
	unknown := keyID(keyFor(54))
	unknownPost := run(t, s, Command{Operation: "post", To: unknown, Text: "Mail before registration"}).Receipt.ID
	if got := eventIDs(run(t, s, Command{Operation: "messages.list", To: unknown}).Messages); !reflect.DeepEqual(got, []string{unknownPost}) {
		t.Fatal("unregistered addressee exact-match behavior lost")
	}
	fails(t, s, Command{Operation: "messages.list", To: "not-a-fingerprint"}, "invalid_recipient")
	fails(t, s, Command{Operation: "messages.list", Kind: "Request"}, "invalid_slug")
}

func TestThreadInvalidParentRelationships(t *testing.T) {
	for _, scenario := range []string{"missing", "self-cycle", "later-parent", "cross-room"} {
		t.Run(scenario, func(t *testing.T) {
			s := openTest(t, Config{})
			root := conversationPost(t, s, "room", "main", "", "note", "Root")
			parent := "missing"
			switch scenario {
			case "self-cycle":
				parent = root
			case "later-parent":
				parent = conversationPost(t, s, "room", "main", "", "note", "Later")
			case "cross-room":
				parent = conversationPost(t, s, "other-room", "main", "", "note", "Other")
			}
			if _, err := s.db.Exec("UPDATE events SET reply_to=? WHERE id=?", parent, root); err != nil {
				t.Fatal(err)
			}
			fails(t, s, Command{Operation: "thread.get", MessageID: root}, "invalid_thread")
		})
	}
	s := openTest(t, Config{})
	fails(t, s, Command{Operation: "thread.get"}, "invalid_message_id")
}

// Direct fixture inserts let the bound test model an established large board
// without issuing thousands of quota-bearing application writes.
func bulkThreadFixture(t *testing.T, s *Store, root string, count int, chain bool) string {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	statement, err := tx.Prepare(`INSERT INTO events(display_seq,id,room,page,text,kind,author,account,handle,public_key,signature,payload,created_at,hash,reply_to,recipient)
 SELECT ?,?,room,page,text,kind,author,account,handle,public_key,signature,payload,created_at,hash,?,recipient FROM events WHERE id=?`)
	if err != nil {
		t.Fatal(err)
	}
	defer statement.Close()
	parent := root
	last := root
	for i := 0; i < count; i++ {
		last = fmt.Sprintf("fixture-%08d", i)
		if _, err = statement.Exec(i+2, last, parent, root); err != nil {
			t.Fatal(err)
		}
		if chain {
			parent = last
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return last
}

func TestThreadTraversalBounds(t *testing.T) {
	t.Run("ancestor depth", func(t *testing.T) {
		s := openTest(t, Config{})
		root := conversationPost(t, s, "room", "main", "", "note", "Root")
		leaf := bulkThreadFixture(t, s, root, ThreadAncestorLimit+1, true)
		tx, err := s.db.BeginTx(testContext, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		for _, id := range []string{leaf, fmt.Sprintf("fixture-%08d", ThreadAncestorLimit-1)} {
			var ref threadReference
			if err = tx.QueryRow("SELECT id,room,reply_to,seq FROM events WHERE id=?", id).Scan(&ref.id, &ref.room, &ref.parent, &ref.seq); err != nil {
				t.Fatal(err)
			}
			resolved, err := resolveThreadRoot(testContext, tx, ref)
			if id == leaf {
				var be *Error
				if !errors.As(err, &be) || be.Code != "thread_depth_limit" {
					t.Fatalf("excess depth accepted: %v", err)
				}
			} else if err != nil || resolved.id != root {
				t.Fatalf("exact depth bound rejected: %v", err)
			}
		}
	})
	t.Run("descendant work", func(t *testing.T) {
		s := openTest(t, Config{})
		root := conversationPost(t, s, "room", "main", "", "note", "Root")
		bulkThreadFixture(t, s, root, ThreadTraversalLimit, false)
		tx, err := s.db.BeginTx(testContext, nil)
		if err != nil {
			t.Fatal(err)
		}
		var ref threadReference
		if err = tx.QueryRow("SELECT id,room,seq FROM events WHERE id=?", root).Scan(&ref.id, &ref.room, &ref.seq); err != nil {
			t.Fatal(err)
		}
		_, err = threadReferences(testContext, tx, ref)
		var be *Error
		if !errors.As(err, &be) || be.Code != "thread_too_large" {
			t.Fatalf("excess traversal accepted: %v", err)
		}
		tx.Rollback()
		if _, err := s.db.Exec("DELETE FROM events WHERE id=?", fmt.Sprintf("fixture-%08d", ThreadTraversalLimit-1)); err != nil {
			t.Fatal(err)
		}
		// Check the exact row bound independently of the operational time budget:
		// race-instrumented SQLite can legitimately need more than two seconds.
		tx, err = s.db.BeginTx(testContext, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		refs, err := threadReferences(testContext, tx, ref)
		if err != nil || len(refs) != ThreadTraversalLimit || refs[0].id != root {
			t.Fatalf("exact traversal bound rejected: count=%d error=%v", len(refs), err)
		}
	})
	t.Run("response count", func(t *testing.T) {
		s := openTest(t, Config{})
		root := conversationPost(t, s, "room", "main", "", "note", "Root")
		bulkThreadFixture(t, s, root, 201, false)
		result := run(t, s, Command{Operation: "thread.get", MessageID: root, Limit: 500})
		// The byte budget may reduce this below 200; it must never exceed the
		// clamped count or lose the indication that more replies remain.
		if limitValue(500) != 200 || len(result.Messages) == 0 || len(result.Messages) > 200 || result.Data["has_more"] != true {
			t.Fatal("response count or byte cap was not applied")
		}
	})
}

func TestThreadSingleOversizedEnvelopeRemainsComplete(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(62)
	register(t, s, key)
	text := strings.Repeat("<", 16000)
	root := run(t, s, signed(key, Command{Operation: "post", Text: text})).Receipt.ID
	child := run(t, s, signed(key, Command{Operation: "post", Text: "A short reply", ReplyTo: root})).Receipt.ID
	first := run(t, s, Command{Operation: "thread.get", MessageID: root})
	if len(first.Messages) != 1 || first.Messages[0].Text != text || first.Messages[0].Signature == "" || first.Data["has_more"] != true {
		t.Fatal("oversized first envelope was truncated or prevented progress")
	}
	encoded, err := json.Marshal(first.Messages[0])
	if err != nil || len(encoded) <= conversationResponseBytes {
		t.Fatal("fixture must exceed the soft response budget after JSON escaping")
	}
	next := run(t, s, Command{Operation: "thread.get", MessageID: root, Cursor: first.NextCursor})
	if len(next.Messages) != 1 || next.Messages[0].ID != child || next.Data["has_more"] != false {
		t.Fatal("oversized envelope skipped the following reply")
	}
}

func TestThreadByteBudgetDoesNotTruncateSignedMessages(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(61)
	register(t, s, key)
	text := strings.Repeat("a", 16000)
	root := run(t, s, signed(key, Command{Operation: "post", Text: text})).Receipt.ID
	for i := 0; i < 3; i++ {
		run(t, s, signed(key, Command{Operation: "post", Text: text, ReplyTo: root}))
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 5; page++ {
		result := run(t, s, Command{Operation: "thread.get", MessageID: root, Cursor: cursor, Limit: 200})
		size := 0
		for _, event := range result.Messages {
			encoded, _ := json.Marshal(event)
			size += len(encoded)
			if event.Text != text || event.Signature == "" || seen[event.ID] {
				t.Fatal("truncated or duplicate signed event")
			}
			seen[event.ID] = true
		}
		if len(result.Messages) > 1 && size > conversationResponseBytes {
			t.Fatal("thread result exceeded byte budget")
		}
		cursor = result.NextCursor
		if result.Data["has_more"] == false {
			break
		}
	}
	if len(seen) != 4 {
		t.Fatalf("byte pagination lost messages: %d", len(seen))
	}
}
