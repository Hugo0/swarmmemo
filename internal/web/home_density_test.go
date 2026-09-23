package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

func TestHomePreviewRetainsOneFullBodyAndNativeConversationLink(t *testing.T) {
	text := "A multiline memo <script>inert</script>\n\n" + strings.Repeat("Complete text survives the preview.\n", 50)
	event := board.Message{ID: "density-message", Room: "lobby", Page: "main", Kind: "note", Text: text, ReplyTo: "parent"}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		if c.Operation == "room.get" {
			return board.Result{OK: true, Room: &board.Room{Name: "lobby", Visibility: "public"}}, nil
		}
		if c.Operation == "messages.list" || c.Operation == "thread.get" {
			return board.Result{OK: true, Messages: []board.Message{event}, Data: map[string]any{"root_id": event.ID}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	for _, path := range []string{"/", "/r/lobby", "/e/" + event.ID} {
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		body := w.Body.String()
		if strings.Contains(body, "memo-preview-toggle") || strings.Contains(body, ">Show more</button>") {
			t.Fatal("inline expansion must be a measured JS enhancement, not inert SSR controls")
		}
		if path == "/" && !strings.Contains(body, `<a href="/docs#ways-to-post">Post with a GET or a POST. No account, no SDK.</a>`) {
			t.Fatal("home must offer an inert GET-posting documentation link")
		}
		if path == "/" && !strings.Contains(body, `<meta name="description" content="A free bulletin board for AI agents. Post with GET or POST, find agents, and pick up a thread. No account, SDK, or wallet required.">`) {
			t.Fatal("home metadata must explain GET or POST discovery")
		}
		// A conversation page also quotes the start of the post in its description
		// and OpenGraph tags; the body itself must appear once, in full.
		main := body[strings.Index(body, "<main"):]
		if strings.Count(main, "Complete text survives the preview.") != 50 || strings.Count(main, `class="memo-text"`) != 1 {
			t.Fatal("preview must retain exactly one complete original body")
		}
		if strings.Contains(body, "<script>inert") || !strings.Contains(body, "&lt;script&gt;inert&lt;/script&gt;") {
			t.Fatal("preview must escape untrusted content")
		}
		if path == "/" || path == "/r/lobby" {
			for _, want := range []string{`class="read-conversation" href="/e/density-message">In thread</a>`, `aria-label="Report message" title="Report message"`} {
				if !strings.Contains(body, want) {
					t.Errorf("missing native preview context %s", want)
				}
			}
			// Two links to this conversation and no more: the timestamp permalink,
			// which is the way in, and the thread context on the location line.
			if strings.Contains(body, `class="reply-ref"`) || !strings.Contains(body, `<a class="memo-time" href="/e/density-message">`) || strings.Count(body, `href="/e/density-message"`) != 2 {
				t.Fatal("a listed reply carries its permalink and its thread context, and no other duplicate link")
			}
			if strings.Contains(body, `>Open</a>`) {
				t.Fatal("the Open action is gone; the card itself opens")
			}
		} else if strings.Contains(body, `class="read-conversation"`) || strings.Contains(body, `class="memo-files"`) {
			t.Fatal("home-only presentation must not change full conversation pages")
		}
	}
}

func TestHomeHiddenPreviewNeverRetainsBodyOrFileDisclosure(t *testing.T) {
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		return board.Result{OK: true, Messages: []board.Message{{ID: "removed", Kind: "note", Hidden: true, Text: "private sentinel must never render", Reason: "Operator removal"}}}, nil
	}}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	if strings.Contains(body, "private sentinel") || strings.Contains(body, `class="memo-files"`) || strings.Contains(body, `class="memo-text"`) {
		t.Fatal("hidden messages must not retain a clipped original")
	}
	if !strings.Contains(body, "Operator removal") || !strings.Contains(body, `<a class="memo-time" href="/e/removed">`) {
		t.Fatal("hidden message must retain only its removal notice and its permalink")
	}
}

func TestListingMetadataOmitsOnlyRedundantDefaults(t *testing.T) {
	for _, kind := range []string{"note", "request"} {
		for _, page := range []string{"main", "a-nondefault-page-with-long-readable-context"} {
			for _, route := range []string{"/", "/r/lobby", "/e/short"} {
				t.Run(kind+"/"+page+route, func(t *testing.T) {
					event := board.Message{ID: "short", Room: "lobby", Page: page, Kind: kind, Text: "Hi."}
					s := &testService{execute: func(c board.Command) (board.Result, error) {
						if c.Operation == "room.get" {
							return board.Result{OK: true, Room: &board.Room{Name: "lobby", Visibility: "public"}}, nil
						}
						return board.Result{OK: true, Messages: []board.Message{event}, Data: map[string]any{"root_id": event.ID}}, nil
					}}
					w := httptest.NewRecorder()
					Handler(s).ServeHTTP(w, httptest.NewRequest("GET", route, nil))
					body := w.Body.String()
					full := route == "/e/short"
					for marker, want := range map[string]bool{
						`class="kind kind-` + kind + `"`: full || kind != "note",
						`class="page-label"`:             full || page != "main",
						`class="memo-room"`:              route != "/r/lobby",
						// The permalink is the timestamp on every route; thread context
						// is context, so it sits on the location line and only for a reply.
						`<a class="memo-time" href="/e/short">`: true,
						`class="read-conversation"`:             false,
						`>Open</a>`:                             false,
					} {
						if strings.Contains(body, marker) != want {
							t.Errorf("marker %s: want %v", marker, want)
						}
					}
					if !strings.Contains(body, `<p class="memo-text">Hi.</p>`) {
						t.Fatal("short body changed")
					}
				})
			}
		}
	}
}

// P05: a reply in a listing quotes the parent it answers, but only from the events
// already on the page — never a per-memo read — and never as a second full body.
func TestListingQuotesOnlyParentsAlreadyOnThePage(t *testing.T) {
	long := strings.Repeat("Café 雪 ", 60)
	parent := board.Message{ID: "parent", Sequence: 1, Room: "lobby", Page: "main", Kind: "note", Text: "A question\nwith <b>markup</b> and\n\nblank lines.", Handle: "asker", PublicKey: "key", Author: strings.Repeat("a", 64)}
	onPage := board.Message{ID: "on-page", Sequence: 2, Room: "lobby", Page: "main", Kind: "note", Text: "An answer.", ReplyTo: "parent"}
	offPage := board.Message{ID: "off-page", Sequence: 3, Room: "lobby", Page: "main", Kind: "note", Text: "Answers something older.", ReplyTo: "not-here"}
	verbose := board.Message{ID: "verbose", Sequence: 4, Room: "lobby", Page: "main", Kind: "note", Text: long}
	quoting := board.Message{ID: "quoting", Sequence: 5, Room: "lobby", Page: "main", Kind: "note", Text: "Short reply.", ReplyTo: "verbose"}
	events := []board.Message{parent, onPage, offPage, verbose, quoting}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		if c.Operation == "room.get" {
			return board.Result{OK: true, Room: &board.Room{Name: "lobby", Visibility: "public"}}, nil
		}
		if c.Operation == "messages.list" {
			return board.Result{OK: true, Messages: events}, nil
		}
		return board.Result{OK: true}, nil
	}}
	for _, path := range []string{"/", "/r/lobby"} {
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		body := w.Body.String()
		if strings.Count(body, `class="memo-quote"`) != 2 {
			t.Fatalf("%s: exactly the two replies whose parent is on the page may quote it", path)
		}
		if !strings.Contains(body, `<a class="memo-quote" href="/e/parent">`) || !strings.Contains(body, `<a class="memo-quote" href="/e/verbose">`) {
			t.Errorf("%s: a quote must link to the parent permalink", path)
		}
		if strings.Contains(body, `href="/e/not-here"`) {
			t.Errorf("%s: a parent that is not on the page must not be quoted or fetched", path)
		}
		// Whitespace is collapsed to one glance line and markup stays escaped.
		if !strings.Contains(body, `<span class="memo-quote-text">A question with &lt;b&gt;markup&lt;/b&gt; and blank lines.</span>`) {
			t.Errorf("%s: quoted parent text must be collapsed and escaped", path)
		}
		// One label per sender: a handle when the key chose one, otherwise the name
		// derived from the fingerprint. The hash is reachable from the byline's link
		// rather than repeated in every quote.
		if !strings.Contains(body, `<span class="memo-quote-author">⌘ asker</span>`) {
			t.Errorf("%s: quoted parent must carry its signed authorship", path)
		}
		if strings.Count(body, long) != 1 {
			t.Errorf("%s: a quote must be bounded, never a second copy of the parent body", path)
		}
		if quoted := body[strings.Index(body, `href="/e/verbose">`):]; !strings.Contains(quoted[:600], "…") {
			t.Errorf("%s: an over-long parent must be truncated", path)
		}
	}
	for _, c := range s.calls {
		if c.Operation != "messages.list" && c.Operation != "room.get" && c.Operation != "rooms.list" && c.Operation != "stats" {
			t.Fatalf("previews must not add a per-memo read: %s", c.Operation)
		}
	}
	if got := len(quoteText(&board.Message{Text: strings.Repeat("x", 500)})); got != quoteRunes+len("…") {
		t.Fatalf("quote budget not enforced: %d", got)
	}
	if quoteText(&board.Message{Hidden: true, Text: "sentinel"}) != "This message has been removed." {
		t.Fatal("a removed parent must never be quoted")
	}
}

// P05: a conversation page indents by reply structure, capped so 320px still fits.
func TestConversationIndentsByReplyStructureWithACap(t *testing.T) {
	events := []board.Message{
		{ID: "root", Sequence: 1, Room: "lobby", Page: "main", Kind: "note", Text: "root"},
		{ID: "a", Sequence: 2, Room: "lobby", Page: "main", Kind: "note", Text: "a", ReplyTo: "root"},
		{ID: "b", Sequence: 3, Room: "lobby", Page: "main", Kind: "note", Text: "b", ReplyTo: "a"},
		{ID: "c", Sequence: 4, Room: "lobby", Page: "main", Kind: "note", Text: "c", ReplyTo: "b"},
		{ID: "d", Sequence: 5, Room: "lobby", Page: "main", Kind: "note", Text: "d", ReplyTo: "c"},
		{ID: "orphan", Sequence: 6, Room: "lobby", Page: "main", Kind: "note", Text: "orphan", ReplyTo: "elsewhere"},
	}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		return board.Result{OK: true, Messages: events, Data: map[string]any{"root_id": "root"}}, nil
	}}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/e/root", nil))
	body := w.Body.String()
	for id, class := range map[string]string{"root": `class="memo"`, "a": `class="memo memo-depth-1"`, "b": `class="memo memo-depth-2"`, "c": `class="memo memo-depth-3"`, "d": `class="memo memo-depth-3"`, "orphan": `class="memo"`} {
		if !strings.Contains(body, class+` id="e-`+id+`"`) {
			t.Errorf("%s: expected %s", id, class)
		}
	}
	if strings.Contains(body, "memo-depth-4") {
		t.Fatal("indentation must cap at three levels")
	}
	// Listings quote; conversation pages indent. They must not do both.
	if strings.Contains(body, `class="memo-quote"`) {
		t.Fatal("a conversation page shows structure, not quoted duplicates")
	}
}
