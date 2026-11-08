package web

import (
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C831: a post revised twice shows once in its room, at its newest version,
// marked edited with a link to its history, in every order the room offers;
// the older versions stay in the record and their permalinks resolve.
func TestRoomShowsNewestVersionOnce(t *testing.T) {
	f := newArticleFixture(t)
	v1 := f.post(board.Command{Text: "Review draft one of the fetch tool."})
	v2 := f.post(board.Command{Text: "Review draft two of the fetch tool.", Data: `{"schema":1,"supersedes":"` + v1 + `"}`})
	v3 := f.post(board.Command{Text: "Review draft three of the fetch tool.", Data: `{"schema":1,"supersedes":"` + v2 + `"}`})
	for _, path := range []string{"/r/guides", "/r/guides?sort=new", "/r/guides?sort=top", "/"} {
		body := f.get(path).Body.String()
		if n := strings.Count(body, "Review draft three of the fetch tool."); n == 0 {
			t.Fatalf("%s: newest version missing", path)
		}
		if strings.Contains(body, "Review draft one") || strings.Contains(body, "Review draft two") {
			t.Fatalf("%s: an older version is listed", path)
		}
		if strings.Count(body, `data-message-id="`+v1+`"`) != 1 || strings.Contains(body, `id="e-`+v2+`"`) || strings.Contains(body, `id="e-`+v3+`"`) {
			t.Fatalf("%s: the post must be listed once, under its original's ID", path)
		}
		if !strings.Contains(body, `<a class="memo-edit-mark" href="/e/`+v1+`/history"`) || !strings.Contains(body, "edited 2×") {
			t.Fatalf("%s: edited marker missing", path)
		}
	}
	for _, id := range []string{v1, v2} {
		w := f.get("/e/" + id)
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, "Review draft three of the fetch tool.") || !strings.Contains(body, `href="/e/`+v1+`/history"`) {
			t.Fatalf("/e/%s: %d, an older permalink must resolve and point at the newest", id, w.Code)
		}
		// The original's address is the post's own; only a later superseded
		// version's permalink says it was edited since.
		earlier := `<a href="/e/` + v1 + `/history#v-` + id + `">the version you opened is in its history</a>`
		if strings.Contains(body, earlier) != (id == v2) {
			t.Fatalf("/e/%s: earlier-version notice wrong", id)
		}
	}
	if strings.Contains(f.get("/e/"+v3).Body.String(), "earlier version of the post") {
		t.Fatal("the newest version's permalink is not an earlier version")
	}
	history := f.get("/e/" + v1 + "/history").Body.String()
	for _, text := range []string{"draft one", "draft two", "draft three"} {
		if !strings.Contains(history, "Review "+text) {
			t.Fatalf("history lost %q", text)
		}
	}
}
