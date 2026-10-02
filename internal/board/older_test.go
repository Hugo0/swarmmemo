package board

import (
	"fmt"
	"strings"
	"testing"
)

func TestOlderTraversalAndScope(t *testing.T) {
	s := openTest(t, Config{})
	owner := keyFor(177)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "history"}))
	var ids []string
	post := func(room, page, kind, text string) string {
		return run(t, s, signed(owner, Command{Operation: "post", Room: room, Page: page, Kind: kind, Text: text})).Receipt.ID
	}
	for i := 0; i < 9; i++ {
		ids = append(ids, post("history", "notes", "request", fmt.Sprintf("needle %d", i)))
		post("history", "other", "offer", "unrelated")
	}
	run(t, s, signed(owner, Command{Operation: "room.hide", MessageID: ids[4], Reason: "hidden fixture"}))
	base := Command{Operation: "messages.list", Room: "history", Page: "notes", Kind: "request", Target: keyID(owner), Data: `{"sort":"new"}`, Limit: 3}
	for _, query := range []string{"", "needle"} {
		c := base
		c.Query = query
		var got []string
		previous := int64(1<<63 - 1)
		for page := 0; ; page++ {
			res := run(t, s, c)
			for _, m := range res.Messages {
				if m.Sequence >= previous {
					t.Fatal("older page did not strictly descend")
				}
				previous = m.Sequence
				if m.ID == ids[4] && (!m.Hidden || m.Text != "") {
					t.Fatalf("hidden post leaked: %+v", m)
				}
				got = append(got, m.ID)
			}
			if res.OlderCursor == "" {
				break
			}
			if page > 5 {
				t.Fatal("traversal did not end")
			}
			// New arrivals and nonmatching rows must neither move the boundary
			// nor shift an offset into the already delivered history.
			post("history", "other", "request", "interleaved arrival")
			post("lobby", "main", "request", "interleaved elsewhere")
			c.Older = res.OlderCursor
		}
		var want []string
		for i := len(ids) - 1; i >= 0; i-- {
			if query == "" || i != 4 {
				want = append(want, ids[i])
			}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("gaps or duplicates: %v, want %v", got, want)
		}
	}
	first := run(t, s, base)
	for name, change := range map[string]func(*Command){
		"room":           func(c *Command) { c.Room = "lobby" },
		"page":           func(c *Command) { c.Page = "other" },
		"author":         func(c *Command) { c.Target = "" },
		"kind":           func(c *Command) { c.Kind = "offer" },
		"query":          func(c *Command) { c.Query = "needle" },
		"recipient":      func(c *Command) { c.To = keyID(owner) },
		"scope":          func(c *Command) { c.Data = `{"sort":"new","scope":"all"}` },
		"forward cursor": func(c *Command) { c.Cursor = "start" },
		"ranked":         func(c *Command) { c.Data = `{"sort":"hot"}` },
		"no sort":        func(c *Command) { c.Data = "" },
		"garbage":        func(c *Command) { c.Older = "garbage" },
		"wrong domain":   func(c *Command) { c.Older = first.NextCursor },
	} {
		t.Run(name, func(t *testing.T) {
			c := base
			c.Older = first.OlderCursor
			change(&c)
			fails(t, s, c, "invalid_cursor")
		})
	}
	c := base
	c.Cursor = first.OlderCursor
	fails(t, s, c, "invalid_cursor")
	c.Cursor, c.Older = "", "foreign:"+strings.Split(first.OlderCursor, ":")[1]
	fails(t, s, c, "cursor_reset")
	// A second origin with its own cipher cannot consume this origin's cursor.
	other := openTest(t, Config{})
	c = Command{Operation: "messages.list", Data: `{"sort":"new"}`, Older: first.OlderCursor}
	fails(t, other, c, "cursor_reset")
}

func TestOlderKeepsMatchingArrivalsOutAndVisibilityCurrent(t *testing.T) {
	s := openTest(t, Config{})
	owner := keyFor(178)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "history"}))
	var ids []string
	for i := 0; i < 9; i++ {
		ids = append(ids, run(t, s, signed(owner, Command{Operation: "post", Room: "history", Text: fmt.Sprint(i)})).Receipt.ID)
	}
	c := Command{Operation: "messages.list", Room: "history", Limit: 3, Data: `{"sort":"new"}`}
	for page := 0; page < 3; page++ {
		res := run(t, s, c)
		if len(res.Messages) != 3 {
			t.Fatalf("page %d: %+v", page, res)
		}
		for i, m := range res.Messages {
			if m.ID != ids[8-page*3-i] {
				t.Fatal("arrival shifted backward page")
			}
		}
		if (res.OlderCursor == "") != (page == 2) {
			t.Fatal("wrong end boundary")
		}
		c.Older = res.OlderCursor
		run(t, s, signed(owner, Command{Operation: "post", Room: "history", Text: "new arrival"}))
	}
	first := run(t, s, c)
	// Membership is checked again, even with a valid cursor.
	if _, err := s.db.Exec("UPDATE rooms SET visibility='private' WHERE name='history'"); err != nil {
		t.Fatal(err)
	}
	c.Older = first.OlderCursor
	if _, err := s.Execute(t.Context(), c, "test"); err == nil {
		t.Fatal("cursor granted access to a private room")
	}
	res := run(t, s, signed(owner, c))
	if len(res.Messages) == 0 {
		t.Fatal("owner lost private history")
	}
}
