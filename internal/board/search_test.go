package board

import (
	"context"
	"slices"
	"testing"
)

func TestSearchStem(t *testing.T) {
	for in, want := range map[string]string{
		"flippa's": "flippa", "flippa": "flippa", "wordle": "wordl", "wordles": "wordl",
		"websites": "websit", "website": "websit", "companies": "compan", "company": "compan",
		"running": "run", "runs": "run", "posted": "post", "posts": "post", "boxes": "box",
		"this": "this", "ai": "ai", "uses": "use",
	} {
		if got := searchStem(in); got != want {
			t.Errorf("searchStem(%q) = %q, want %q", in, got, want)
		}
	}
	if got := searchTerms("What is the Flippa listing?"); !slices.Equal(got, []string{"flippa", "list"}) {
		t.Fatalf("terms: %v", got)
	}
	if got := searchTerms("the"); !slices.Equal(got, []string{"the"}) {
		t.Fatalf("stop-only query: %v", got)
	}
}

func TestSearchRelevance(t *testing.T) {
	terms := searchTerms("flippa")
	for text, want := range map[string]int{
		"Sold it on Flippa's marketplace": 3,
		"see flippa.com for listings":     3,
		"flippashop.io is a clone":        2,
		"buyflippa deals":                 1,
		"nothing here":                    0,
	} {
		if got := searchRelevance(text, "flippa", terms); got != want {
			t.Errorf("%q: %d, want %d", text, got, want)
		}
	}
	// A short term matches only as a word.
	if got := searchRelevance("she said hello", "ai", searchTerms("ai")); got != 0 {
		t.Fatalf("ai in said: %d", got)
	}
	if got := searchRelevance("an AI agent", "ai", searchTerms("ai")); got != 3 {
		t.Fatalf("ai: %d", got)
	}
}

func searchIDs(t *testing.T, s *Store, c Command) []string {
	t.Helper()
	c.Operation = "messages.list"
	res := run(t, s, c)
	ids := []string{}
	for _, m := range res.Messages {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestSearchMatchesStemsAndDomains(t *testing.T) {
	s := openTest(t, Config{})
	possessive := postAs(t, s, keyFor(41), Command{Room: "lobby", Text: "I sold a site through Flippa's auction last week"})
	domain := postAs(t, s, keyFor(42), Command{Room: "lobby", Text: "Listings are at flippa.com/browse"})
	style := postAs(t, s, keyFor(43), Command{Room: "lobby", Text: "A Wordle-style puzzle for agents"})
	plural := postAs(t, s, keyFor(44), Command{Room: "lobby", Text: "We build websites for agents"})
	other := postAs(t, s, keyFor(45), Command{Room: "lobby", Text: "Nothing about it here"})

	got := searchIDs(t, s, Command{Query: "Flippa"})
	if len(got) != 2 || !slices.Contains(got, possessive) || !slices.Contains(got, domain) {
		t.Fatalf("Flippa: %v", got)
	}
	if got := searchIDs(t, s, Command{Query: "wordles"}); !slices.Equal(got, []string{style}) {
		t.Fatalf("wordles: %v", got)
	}
	if got := searchIDs(t, s, Command{Query: "website"}); !slices.Equal(got, []string{plural}) {
		t.Fatalf("website: %v", got)
	}
	// Every word must be held, in any order; the in-order read agrees.
	if got := searchIDs(t, s, Command{Query: "agents puzzles"}); !slices.Equal(got, []string{style}) {
		t.Fatalf("agents puzzles: %v", got)
	}
	if got := searchIDs(t, s, Command{Query: "flippa", Data: `{"sort":"new"}`}); len(got) != 2 {
		t.Fatalf("sort=new: %v", got)
	}
	if slices.Contains(searchIDs(t, s, Command{Query: "flippa"}), other) {
		t.Fatal("unrelated post matched")
	}
}

func TestSearchRanksSpamAndLowQualityLast(t *testing.T) {
	s := openTest(t, Config{})
	ctx := context.Background()
	good := postAs(t, s, keyFor(51), Command{Room: "lobby", Text: "How I built my website with a static generator"})
	low := postAs(t, s, keyFor(52), Command{Room: "lobby", Text: "website website buy cheap website traffic now"})
	flagged := postAs(t, s, keyFor(53), Command{Room: "lobby", Text: "Free website!!! click here"})
	partial := postAs(t, s, keyFor(54), Command{Room: "lobby", Text: "Notes on websitebuilders and their pricing"})
	hidden := postAs(t, s, keyFor(55), Command{Room: "lobby", Text: "My website is the best website"})
	if err := s.RecordQuality(ctx, good, 0.9, "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordQuality(ctx, low, 0.05, "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordFlag(ctx, flagged, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE events SET hidden=1 WHERE id=?", hidden); err != nil {
		t.Fatal(err)
	}
	got := searchIDs(t, s, Command{Query: "website"})
	if len(got) != 4 || slices.Contains(got, hidden) {
		t.Fatalf("results: %v", got)
	}
	// Whole-word matches by merit, then the weaker match; the flagged and the
	// low-quality post after every other.
	if got[0] != good || got[1] != partial || !slices.Contains(got[2:], low) || !slices.Contains(got[2:], flagged) {
		t.Fatalf("order: %v (good %s partial %s low %s flagged %s)", got, good, partial, low, flagged)
	}
	if slices.Contains(searchIDs(t, s, Command{Query: "website", Data: `{"sort":"new"}`}), hidden) {
		t.Fatal("hidden post in sort=new search")
	}
}
