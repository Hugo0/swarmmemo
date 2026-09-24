package board

import (
	"strings"
	"testing"
	"time"
)

func sitemapIDs(t *testing.T, s *Store) map[string]Message {
	t.Helper()
	listed := map[string]Message{}
	if err := s.PublicSitemapPosts(testContext, 0, 50000, func(m Message) { listed[m.ID] = m }); err != nil {
		t.Fatal(err)
	}
	return listed
}

// The sitemap offers every public, visible, non-simulation thread root once it
// is past the archive delay, and nothing a reader could not index anyway.
func TestSitemapSelectsOnlyIndexableRoots(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: 3600})
	author, member := keyFor(61), keyFor(62)
	register(t, s, member)
	markdown := dataJSON(`"format":"markdown"`)
	post := func(c Command) string { return run(t, s, signed(author, c)).Receipt.ID }

	plain := post(Command{Operation: "post", Room: "lobby", Text: "An ordinary root"})
	anonymous := run(t, s, Command{Operation: "post", Room: "research", Text: "Anonymous root"}).Receipt.ID
	reply := post(Command{Operation: "post", Room: "lobby", Text: "A reply", ReplyTo: plain})
	hidden := post(Command{Operation: "post", Room: "lobby", Text: "Removed later"})
	simulation := post(Command{Operation: "post", Room: "lobby", Kind: "simulation", Text: "Operator fixture"})
	run(t, s, signed(author, Command{Operation: "room.create", Room: "secret", Visibility: "private", Members: []string{keyID(member)}}))
	private := post(Command{Operation: "post", Room: "secret", Text: "private-data"})
	own := post(Command{Operation: "post", Room: personal(author), Text: "In my own room"})
	article := post(Command{Operation: "post", Room: "guides", Text: "# Old title\n\nBody.", Data: markdown})
	edit := post(Command{Operation: "post", Room: "guides", Text: "# New title\n\nBody.", Data: dataJSON(`"format":"markdown","supersedes":"` + article + `"`)})
	removedEdit := post(Command{Operation: "post", Room: "guides", Text: "# Withdrawn\n\nBody.", Data: markdown})
	badEdit := post(Command{Operation: "post", Room: "guides", Text: "# Bad edit\n\nBody.", Data: dataJSON(`"format":"markdown","supersedes":"` + removedEdit + `"`)})
	for _, id := range []string{hidden, badEdit} {
		if err := s.Moderate(testContext, id, "test", true); err != nil {
			t.Fatal(err)
		}
	}

	// Inside the archive delay only signed Markdown articles are offered.
	listed := sitemapIDs(t, s)
	if len(listed) != 1 || listed[article].ID == "" {
		t.Fatalf("young plain posts must wait out the archive delay: %v", listed)
	}
	if m := listed[article]; !strings.HasPrefix(m.Text, "# New title") || m.Origin() != article || m.Format != PostFormatMarkdown {
		t.Fatalf("an edited article is listed as its newest version under its first ID: %+v", m)
	}

	s.now = func() time.Time { return time.Unix(testTime+3601, 0) }
	listed = sitemapIDs(t, s)
	for _, want := range []string{plain, anonymous, article} {
		if listed[want].ID == "" {
			t.Errorf("%s missing from %v", want, listed)
		}
	}
	for name, never := range map[string]string{"reply": reply, "hidden": hidden, "simulation": simulation, "private": private, "personal": own, "edit": edit, "hidden newest version": removedEdit, "hidden version": badEdit} {
		if _, ok := listed[never]; ok {
			t.Errorf("the sitemap lists a %s message", name)
		}
	}
	if listed[plain].Text != "" {
		t.Fatal("a plain post needs no text in the sitemap")
	}
	rooms, posts, err := s.PublicSitemapCounts(testContext)
	if err != nil || posts != len(listed) {
		t.Fatalf("count %d disagrees with the listing %d: %v", posts, len(listed), err)
	}
	roomList, err := s.PublicSitemapRooms(testContext, 0, 100)
	if err != nil || len(roomList) != rooms {
		t.Fatalf("rooms: %v %v (count %d)", roomList, err, rooms)
	}
	names := []string{}
	for _, room := range roomList {
		names = append(names, room.Name)
		if room.Modified <= 0 {
			t.Fatalf("room %s has no lastmod", room.Name)
		}
	}
	if strings.Join(names, ",") != "guides,lobby,research" {
		t.Fatalf("rooms listed: %v (private, personal and empty rooms never are)", names)
	}

	// Paging is by offset over one stable order.
	var paged []string
	for offset := 0; offset < posts; offset++ {
		if err := s.PublicSitemapPosts(testContext, offset, 1, func(m Message) { paged = append(paged, m.ID) }); err != nil {
			t.Fatal(err)
		}
	}
	if len(paged) != posts || paged[0] != plain {
		t.Fatalf("paged listing: %v", paged)
	}
	for _, bad := range [][2]int{{-1, 10}, {0, 0}, {0, 50001}} {
		if s.PublicSitemapPosts(testContext, bad[0], bad[1], func(Message) {}) == nil {
			t.Fatalf("range %v accepted", bad)
		}
		if _, err := s.PublicSitemapRooms(testContext, bad[0], bad[1]); err == nil {
			t.Fatalf("room range %v accepted", bad)
		}
	}
}

// A long Markdown body is cut before it reaches the sitemap: a title needs its
// first lines, not the whole text of every article in memory at once.
func TestSitemapCarriesOnlyTheStartOfLongArticles(t *testing.T) {
	s := openTest(t, Config{})
	id := run(t, s, signed(keyFor(63), Command{Operation: "post", Room: "guides", Text: "# Long\n\n" + strings.Repeat("word ", 3000), Data: dataJSON(`"format":"markdown"`)})).Receipt.ID
	m := sitemapIDs(t, s)[id]
	if len(m.Text) != SitemapTitleBytes || !strings.HasPrefix(m.Text, "# Long") {
		t.Fatalf("sitemap text: %d bytes", len(m.Text))
	}
}
