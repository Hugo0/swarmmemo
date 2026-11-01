package board

import (
	"strings"
	"testing"
)

// capturePages runs each command and returns the page queries, shapes and
// arguments readEvents ran for it.
type capturedPage struct {
	query, shape string
	args         []any
}

func capturePages(t *testing.T, s *Store, cmds ...Command) []capturedPage {
	t.Helper()
	var pages []capturedPage
	eventsPageHook = func(query, shape string, args []any) {
		pages = append(pages, capturedPage{query, shape, append([]any{}, args...)})
	}
	t.Cleanup(func() { eventsPageHook = nil })
	out := make([]capturedPage, 0, len(cmds))
	for _, c := range cmds {
		n := len(pages)
		run(t, s, c)
		if len(pages) == n {
			t.Fatalf("%s %+v ran no page query", c.Operation, c)
		}
		out = append(out, pages[len(pages)-1])
	}
	return out
}

// C688: the page shapes behind the 6 s stalls of 2026-11-01 walk an index
// in seq order: one author's or one inbox's posts, a room, and the first
// post probe. None scans events or sorts the matches in a temp B-tree.
func TestReadEventsPageShapesUseIndexes(t *testing.T) {
	s := openTest(t, Config{})
	k := keyFor(31)
	run(t, s, signed(k, Command{Operation: "room.create", Room: "lab", Visibility: "public", Timestamp: s.now().Unix()}))
	postAs(t, s, k, Command{Room: "lab", Text: "hello"})
	who := keyID(k)
	cmds := []Command{
		{Operation: "messages.list", Room: "lab"},
		{Operation: "messages.list", Room: "lab", Cursor: "start"},
		{Operation: "messages.list", Room: "lab", Kind: "note", Query: "x"},
		{Operation: "messages.list", Target: who},
		{Operation: "messages.list", Target: who, Query: "x"},
		{Operation: "messages.list", Room: "lab", Target: who},
		{Operation: "messages.list", To: who},
		{Operation: "messages.list", Room: "lab", Data: `{"first_post":"unanswered"}`},
		{Operation: "messages.list", Data: `{"scope":"all","first_post":"only"}`},
	}
	for i, p := range capturePages(t, s, cmds...) {
		rows, err := s.db.Query("EXPLAIN QUERY PLAN "+p.query, p.args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		joined := strings.Join(plan, "\n")
		for _, d := range plan {
			if d == "SCAN e" || strings.HasPrefix(d, "SCAN e ") || d == "SCAN p" || strings.HasPrefix(d, "SCAN p ") || strings.Contains(d, "SCAN events") || strings.Contains(d, "TEMP B-TREE") {
				t.Fatalf("%+v: %q in plan:\n%s", cmds[i], d, joined)
			}
		}
		if strings.Contains(p.query, "events p INDEXED BY") && !strings.Contains(joined, "SEARCH p USING INDEX events_author") {
			t.Fatalf("%+v: the first post probe left events_author:\n%s", cmds[i], joined)
		}
	}
}

// The slow-read shape names columns and operators, never a value.
func TestReadShapeHasNoValues(t *testing.T) {
	s := openTest(t, Config{})
	k := keyFor(32)
	run(t, s, signed(k, Command{Operation: "room.create", Room: "secretroom", Visibility: "public", Timestamp: s.now().Unix()}))
	postAs(t, s, k, Command{Room: "secretroom", Page: "secretpage", Text: "needle"})
	who := keyID(k)
	pages := capturePages(t, s,
		Command{Operation: "messages.list", Room: "secretroom", Page: "secretpage", Kind: "secretkind", Query: "needlevalue", Target: who, Cursor: "start"},
		Command{Operation: "messages.list", To: who, Query: "needlevalue"},
		Command{Operation: "messages.list", Target: "secrethandle"},
	)
	for _, p := range pages {
		for _, v := range []string{"secretroom", "secretpage", "secretkind", "needlevalue", "secrethandle", who, who[:16]} {
			if strings.Contains(p.shape, v) {
				t.Fatalf("shape carries %q: %s", v, p.shape)
			}
		}
		if !strings.HasSuffix(p.shape, " ORDER BY e.seq DESC") && !strings.HasSuffix(p.shape, " ORDER BY e.seq ASC") {
			t.Fatalf("shape: %s", p.shape)
		}
	}
	if got := pages[0].shape; !strings.Contains(got, "e.room=?") || !strings.Contains(got, "e.account=?") || !strings.Contains(got, "instr(lower(e.text),lower(?))>0") {
		t.Fatalf("shape lost its filters: %s", got)
	}
	long := whereShape([]string{"a=?", strings.Repeat("x", 200)}, "DESC")
	if want := "a=? AND " + strings.Repeat("x", shapeTermMax) + "... ORDER BY e.seq DESC"; long != want {
		t.Fatalf("whereShape: %q", long)
	}
}
