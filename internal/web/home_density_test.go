package web

import (
	"html"
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
		target := path
		if !strings.HasPrefix(path, "/e/") {
			target += "?sort=new" // the flat stream that quotes; Hot nests instead
		}
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", target, nil))
		body := w.Body.String()
		if strings.Contains(body, "memo-preview-toggle") || strings.Contains(body, ">Show more</button>") {
			t.Fatal("inline expansion must be a measured JS enhancement, not inert SSR controls")
		}
		if path == "/" && !strings.Contains(body, `<p class="hero-ways">Post from anywhere: <a href="/docs#ways-to-post">HTTP</a>`) {
			t.Fatal("home must offer an inert link to how to post")
		}
		if path == "/" && !strings.Contains(body, `<meta name="description" content="`+html.EscapeString(ShortDescription)+`">`) {
			t.Fatal("home metadata must be the copy kit's short description")
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
			for _, want := range []string{`class="read-conversation" href="/e/density-message">In thread</a>`, `aria-label="Report post" title="Report post"`} {
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
						`class="kind kind-` + kind: full || kind != "note",
						`class="page-label"`:       full || page != "main",
						`class="memo-room"`:        route != "/r/lobby",
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
					if !strings.Contains(body, `<div class="memo-text">Hi.</div>`) {
						t.Fatal("short body changed")
					}
				})
			}
		}
	}
}

// P05: a reply in a listing quotes the parent it answers, never with a per-memo
// read and never as a second full body. A service without the one-statement
// parent read (parentReader) quotes only parents already on the page.
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
	for _, path := range []string{"/?sort=new", "/r/lobby?sort=new"} {
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
			t.Errorf("%s: a parent this service cannot read must not be quoted", path)
		}
		// A reply whose parent is quoted needs no In thread link: the quote is the context.
		if strings.Count(body, `class="read-conversation"`) != 1 || !strings.Contains(body, `class="read-conversation" href="/e/off-page"`) {
			t.Errorf("%s: only the unquoted reply keeps its In thread link", path)
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

// A reply whose parent is not on the page still quotes it in a listing: the
// store reads every missing parent in one statement, public rooms only, and
// the quote names the author once, escaped and bounded.
func TestListingQuotesParentsOffThePage(t *testing.T) {
	s, owner, mod := roomStore(t)
	parent := owner.run(t, s, board.Command{Operation: "post", Room: "lobby", Text: "Which cache header <b>should</b> I send?"}).Receipt.ID
	mod.run(t, s, board.Command{Operation: "post", Room: "lobby", Text: "Use an ETag answer.", ReplyTo: parent})
	for _, path := range []string{"/?q=ETag", "/r/lobby?q=ETag"} {
		body := render(s, path).Body.String()
		main := body[strings.Index(body, "<main"):]
		if strings.Contains(main, "Which cache header <b>") || !strings.Contains(main, `<a class="memo-quote" href="/e/`+parent+`"><span class="memo-quote-author">⌘ writer</span><span class="memo-quote-text">Which cache header &lt;b&gt;should&lt;/b&gt; I send?</span></a>`) {
			t.Fatalf("%s: the off-page parent is not quoted", path)
		}
		if strings.Contains(main, `class="read-conversation"`) {
			t.Errorf("%s: a quoted reply needs no In thread link", path)
		}
	}
	got, err := s.PublicMessagesByID(t.Context(), []string{parent, "unknown"})
	if err != nil || len(got) != 1 || got[0].ID != parent {
		t.Fatalf("PublicMessagesByID: %v %v", got, err)
	}
}
