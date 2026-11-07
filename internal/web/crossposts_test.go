package web

import (
	"crypto/rand"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C592: a ranked entry's line names up to copiesShown other rooms, each once,
// and counts the rest.
func TestCopiesLine(t *testing.T) {
	c := &board.MessageCopies{Count: 6, IDs: []string{"a", "b", "c", "d", "e", "f"}, Rooms: []string{"research", "research", "tools", "", "meta", "ideas"}}
	got := copiesLineOf(c)
	want := copiesLine{Links: []copyLink{{"research", "a"}, {"tools", "c"}, {"meta", "e"}}, More: 3}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	// Rooms unknown (an older cached ranking): only the count.
	if got := copiesLineOf(&board.MessageCopies{Count: 2, IDs: []string{"a", "b"}}); got.Links != nil || got.More != 2 {
		t.Fatalf("no rooms: %+v", got)
	}
}

// C601: a key's burst of similar posts in a room draws as one entry, its
// newest, with the others behind "+N similar"; each keeps its own page.
func TestRoomFoldsSimilarBurst(t *testing.T) {
	s, owner, _ := roomStore(t)
	var ids []string
	for i := range 5 {
		ids = append(ids, owner.run(t, s, board.Command{Operation: "post", Room: "lobby", Text: fmt.Sprintf("x402 Trust's public grade for tool %d is B", i) + uniqueTail()}).Receipt.ID)
	}
	other := owner.run(t, s, board.Command{Operation: "post", Room: "lobby", Text: "A question about something else entirely"}).Receipt.ID
	for _, path := range []string{"/r/lobby?sort=new", "/r/lobby"} {
		body := render(s, path).Body.String()
		if !strings.Contains(body, `id="e-`+other+`"`) || !strings.Contains(body, `id="e-`+ids[4]+`"`) {
			t.Fatalf("%s: the burst's first post or the other post is missing", path)
		}
		for _, id := range ids[:4] {
			if strings.Contains(body, `id="e-`+id+`"`) || !strings.Contains(body, `<li><a href="/e/`+id+`">x402 Trust&#39;s public grade for tool`) {
				t.Fatalf("%s: %s is not folded under the first", path, id)
			}
		}
		if strings.Count(body, `class="memo-similar small muted"`) != 1 || !strings.Contains(body, "+4 similar</summary>") {
			t.Fatalf("%s: want one +4 similar", path)
		}
	}
	if body := render(s, "/e/"+ids[0]).Body.String(); !strings.Contains(body, `id="e-`+ids[0]+`"`) {
		t.Fatal("a folded post's own page is missing it")
	}
}

// uniqueTail makes a fixture's template text one of a kind: random words,
// so one key's or network's posts are never a near-copy flood (repeat_flood),
// which compares word pairs with digits removed.
func uniqueTail() string {
	b := make([]byte, 40)
	_, _ = rand.Read(b)
	out := []byte(" ~")
	for i, c := range b {
		if i%5 == 0 {
			out = append(out, ' ')
		}
		out = append(out, 'a'+c%26)
	}
	return string(out)
}

// fillerText is n bytes of random lowercase words, a sentence's worth of
// length without repeating another post's word pairs (repeat_flood).
func fillerText(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	out := make([]byte, 0, n)
	for i, c := range b {
		switch {
		case i == n-1:
			out = append(out, '.')
		case i > 0 && c%7 == 0 && out[len(out)-1] != ' ':
			out = append(out, ' ')
		default:
			out = append(out, 'a'+c%26)
		}
	}
	return string(out)
}
