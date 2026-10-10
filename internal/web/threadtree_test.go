package web

import (
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

func treeMessage(id, parent string, seq, created, score int64) board.Message {
	m := board.Message{ID: id, Sequence: seq, CreatedAt: created, Room: "lobby", Page: "main", Kind: "note", Text: "text " + id, ReplyTo: parent, Type: "message", Visibility: "public"}
	if score != 0 {
		m.Votes = &board.VoteCounts{Up: max(score, 0), Down: max(-score, 0), Score: score}
	}
	return m
}

// A conversation page draws its reply tree like a Hacker News comment page:
// a reply under its parent whatever its age, siblings by hot with the newest
// first among equals, indentation capped with a "continue thread" link.
func TestConversationIsAReplyTree(t *testing.T) {
	now := time.Now().Unix()
	events := []board.Message{
		treeMessage("root", "", 1, now-9000, 0),
		treeMessage("old", "root", 2, now-8000, 0),
		treeMessage("a", "root", 3, now-7000, 0),
		treeMessage("b", "a", 4, now-6000, 0),
		treeMessage("c", "b", 5, now-5000, 0),
		treeMessage("d", "c", 6, now-4000, 0),
		treeMessage("e", "d", 7, now-3000, 0),
		treeMessage("f", "e", 8, now-2000, 0),
		treeMessage("liked", "root", 9, now-1000, 3),
		treeMessage("late", "old", 10, now-500, 0),
		treeMessage("orphan", "elsewhere", 11, now-100, 0),
	}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		return board.Result{OK: true, Messages: events, Data: map[string]any{"root_id": "root"}}, nil
	}}
	body := render(s, "/e/root").Body.String()
	for id, class := range map[string]string{"root": `class="memo"`, "liked": `class="memo memo-depth-1"`, "a": `class="memo memo-depth-1"`, "b": `class="memo memo-depth-2"`,
		"e": `class="memo memo-depth-5"`, "late": `class="memo memo-depth-2"`, "orphan": `class="memo"`} {
		if !strings.Contains(body, class+` id="e-`+id+`"`) {
			t.Errorf("%s: expected %s", id, class)
		}
	}
	// Siblings: the voted reply first, then the newest; a late reply sits under
	// its parent, not at the end of the page.
	order := []string{"root", "liked", "a", "b", "c", "d", "e", "old", "late", "orphan"}
	at := -1
	for _, id := range order {
		i := strings.Index(body, `id="e-`+id+`"`)
		if i < at {
			t.Fatalf("%s is out of tree order", id)
		}
		at = i
	}
	if strings.Contains(body, `id="e-f"`) || strings.Contains(body, "memo-depth-6") {
		t.Fatal("indentation must stop at five levels")
	}
	if !strings.Contains(body, `<a class="memo-continue" href="/e/e?sub=1">Continue thread →</a>`) {
		t.Fatal("the deepest drawn reply must continue its thread")
	}
	// The head line: parent · next · [–], linked in the page.
	for _, want := range []string{`<a class="memo-parent" href="#e-a">parent</a>`, `<a class="memo-next" href="#e-a">next</a>`, `<a class="memo-next" href="#e-old">next</a>`,
		`<a class="memo-collapse" href="/e/b?sub=1" data-collapse="b"`, `<a class="memo-parent" href="/e/elsewhere">parent</a>`, `data-depth="2" data-parent="a"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	// Listings quote; conversation pages indent. They must not do both.
	if strings.Contains(body, `class="memo-quote"`) || strings.Contains(body, `class="reply-ref"`) {
		t.Fatal("a conversation page shows structure, not quotes or reply references")
	}
	// A branch on its own: the subthread page, which no search engine indexes.
	w := render(s, "/e/e?sub=1")
	sub := w.Body.String()
	if !strings.Contains(sub, `class="memo memo-focus" id="e-e"`) || !strings.Contains(sub, `class="memo memo-depth-1" id="e-f"`) || strings.Contains(sub, `id="e-a"`) || w.Header().Get("X-Robots-Tag") == "" {
		t.Fatal("a subthread starts at its branch and is not indexed")
	}
	if !strings.Contains(sub, `href="/e/root#e-e">↑ The whole conversation</a>`) || !strings.Contains(sub, `<a class="memo-parent" href="/e/d">parent</a>`) {
		t.Fatal("a subthread links up to its parent and the whole conversation")
	}
	// Opened at a reply too deep to draw from the root, the page starts at its parent.
	if deep := render(s, "/e/f").Body.String(); !strings.Contains(deep, `class="memo" id="e-e"`) || !strings.Contains(deep, `class="memo memo-depth-1 memo-focus" id="e-f"`) {
		t.Fatal("a deep permalink must still show its message")
	}
}

// Hot (the default) and Top nest each post's replies under it, a bounded few,
// with a link to the rest; New is the flat stream, with no thread reads.
func TestHotFeedNestsRepliesAndNewStaysFlat(t *testing.T) {
	now := time.Now().Unix()
	post := treeMessage("post", "", 1, now-5000, 2)
	thread := []board.Message{post}
	for i, id := range []string{"r1", "r2", "r3", "r4", "r5", "r6", "r7"} {
		thread = append(thread, treeMessage(id, "post", int64(i+2), now-int64(4000-i*100), 0))
	}
	thread = append(thread, treeMessage("deep", "r7", 9, now-10, 0))
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: "lobby", Visibility: "public"}}, nil
		case "messages.list":
			if strings.Contains(c.Data, `"sort":"new"`) {
				return board.Result{OK: true, Messages: thread}, nil
			}
			return board.Result{OK: true, Messages: []board.Message{post}, Data: map[string]any{"has_more": false}}, nil
		case "thread.get":
			return board.Result{OK: true, Messages: thread, Data: map[string]any{"root_id": c.MessageID}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	for _, path := range []string{"/", "/r/lobby", "/r/lobby?sort=top"} {
		body := render(s, path).Body.String()
		if !strings.Contains(body, `id="ranked-feed"`) || !strings.Contains(body, `class="memo memo-depth-1" id="e-r7"`) || !strings.Contains(body, `class="memo memo-depth-2" id="e-deep"`) {
			t.Fatalf("%s: replies must nest under their post", path)
		}
		// The newest reply first among equals; six drawn, the rest a link away.
		if strings.Index(body, `id="e-r7"`) > strings.Index(body, `id="e-r6"`) || strings.Contains(body, `id="e-r1"`) || strings.Contains(body, `id="e-r2"`) {
			t.Fatalf("%s: siblings must be newest first among equals, at most six replies", path)
		}
		if !strings.Contains(body, `<a class="memo-more" href="/e/post">More replies in the conversation →</a>`) || strings.Contains(body, `class="memo-quote"`) {
			t.Fatalf("%s: a nested reply needs no quote; the rest of the thread is a link", path)
		}
		if !strings.Contains(body, `class="votes" data-vote-id="post"`) || !strings.Contains(body, `<a class="vote-button" href="/me" data-vote="1" aria-label="Upvote"`) {
			t.Fatalf("%s: every post has its vote arrows", path)
		}
	}
	s.calls = nil
	body := render(s, "/r/lobby?sort=new").Body.String()
	if strings.Contains(body, "memo-depth-") || strings.Contains(body, `class="memo-nav"`) || !strings.Contains(body, `<a class="memo-quote" href="/e/post">`) {
		t.Fatal("New is flat and quotes each reply's parent")
	}
	for _, c := range s.calls {
		if c.Operation == "thread.get" {
			t.Fatal("New must not read threads")
		}
	}
}

// ▲ and ▼ sit at a post's top left, before its head line, and are links to
// the key flow until app.js finds a key: never a GET that writes.
func TestVoteArrowsLeadEveryPost(t *testing.T) {
	m := treeMessage("voted", "", 1, time.Now().Unix(), 4)
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		if c.Operation == "room.get" {
			return board.Result{OK: true, Room: &board.Room{Name: "lobby", Visibility: "public"}}, nil
		}
		return board.Result{OK: true, Messages: []board.Message{m}, Data: map[string]any{"root_id": m.ID}}, nil
	}}
	for _, path := range []string{"/", "/?sort=new", "/r/lobby", "/e/voted"} {
		body := render(s, path).Body.String()
		card := body[strings.Index(body, `id="e-voted"`):]
		votes, head, actions := strings.Index(card, `class="votes"`), strings.Index(card, `class="memo-head"`), strings.Index(card, `class="memo-actions"`)
		if votes < 0 || votes > head || strings.Contains(card[actions:], `class="votes"`) {
			t.Fatalf("%s: the arrows lead the post, once", path)
		}
		for _, want := range []string{`aria-label="Upvote"`, `aria-label="Downvote"`, `<span class="vote-score" aria-label="Score 4">4</span>`, `title="Voting needs a signing key in this browser. Make one on Me."`} {
			if !strings.Contains(card, want) {
				t.Errorf("%s: missing %s", path, want)
			}
		}
		if arrows := card[votes:head]; strings.Count(arrows, `href="/me"`) != 2 || strings.Contains(arrows, "?") {
			t.Fatalf("%s: a vote is never a GET link", path)
		}
	}
}
