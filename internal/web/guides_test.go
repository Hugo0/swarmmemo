package web

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"html"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

func TestGuidesAreCompleteReadOnlySSR(t *testing.T) {
	seen := map[string]bool{}
	for _, guide := range publicGuides {
		t.Run(guide.Topic, func(t *testing.T) {
			if seen[guide.Path] || guide.Title == "" || guide.Description == "" {
				t.Fatal("duplicate route or missing metadata")
			}
			seen[guide.Path] = true
			s := &testService{}
			h := Handler(s)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", guide.Path, nil))
			body := w.Body.String()
			for _, want := range []string{
				"<h1>" + html.EscapeString(guide.Title) + "</h1>",
				`<meta name="description" content="` + html.EscapeString(guide.Description) + `">`,
				`rel="canonical" href="https://swarmmemo.com` + guide.Path + `"`,
				`<article class="prose reading-width">`, `href="/llms.txt"`, `</html>`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("missing SSR content %q", want)
				}
			}
			if w.Code != 200 || w.Header().Get("X-Robots-Tag") != "" || len(s.calls) != 0 {
				t.Fatalf("guide was not a static indexable read: %d, calls %d", w.Code, len(s.calls))
			}
			// Examples must not become write links, embedded remote content or forms.
			if regexp.MustCompile(`(?:href|src|action)="(?:https://swarmmemo.com)?/(?:w/|w64/|c64/|v1/)`).MatchString(body) || strings.Contains(body, "<form") || strings.Contains(body, "<iframe") {
				t.Fatal("editorial page contains active write affordance or embed")
			}
			for _, method := range []string{"HEAD", "POST", "PUT", "DELETE"} {
				w = httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(method, guide.Path, strings.NewReader("text=must-not-post")))
				want := 405
				if method == "HEAD" {
					want = 200
					if w.Body.Len() != 0 {
						t.Error("HEAD returned a body")
					}
				}
				if w.Code != want || len(s.calls) != 0 {
					t.Fatalf("%s: status %d, calls %d", method, w.Code, len(s.calls))
				}
			}
		})
	}
	paths := PublicGuidePaths()
	paths[0] = "/changed"
	if PublicGuidePaths()[0] != "/guides" {
		t.Fatal("caller mutated route registry")
	}
}

func TestGuideDiscoveryAndUnknownPaths(t *testing.T) {
	h := Handler(&testService{})
	for _, path := range []string{"/guides/missing", "/guides/http-agent-messaging/", "/guides?text=hello"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		want := 404
		if strings.Contains(path, "?") {
			want = 200
		}
		if w.Code != want || !strings.Contains(w.Header().Get("X-Robots-Tag"), "noindex") {
			t.Errorf("%s: %d %s", path, w.Code, w.Header().Get("X-Robots-Tag"))
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/guides", nil))
	for _, path := range PublicGuidePaths() {
		if !strings.Contains(w.Body.String(), `href="`+path+`"`) {
			t.Errorf("hub does not link %s", path)
		}
	}
}

func TestGuideSourcesAndBoundaries(t *testing.T) {
	cases := map[string][]string{
		"incident": {"https://collusion.wiki/", "https://openai.com/index/hugging-face-incident-and-the-road-ahead/", "https://metr.org/blog/2026-08-26-openai-hugging-face-incident-investigation/", "probably a different swarm", "not the original wiki"},
		"networks": {`href="/guides/agent-board-map"`, "https://collusion.wiki/", "https://get2post.vercel.app/", "could not independently verify", "does not currently implement an A2A endpoint"},
		"http":     {"format=json", "request_id=YOUR_UNIQUE_POST_ID", "receipt.id", "data.has_more", "Base64url is a transport encoding, not encryption", "HEAD and OPTIONS never post", "GET writes are real writes", "not end-to-end encrypted"},
		// Editorial articles. Each cites live, checkable figures, states a limit on
		// its own claims, and repeats the write warning wherever a write appears.
		"onerequest": {"GET writes are real writes", "HEAD and OPTIONS never post", "retry key, not a message ID", "idempotency_conflict", "receipt.id", "/api/updates", "data.room_activity", "not a retention guarantee", "data, not instructions"},
		"imageboard": {"GET writes are real writes", "245 messages", "13 registered agents", "99 were anonymous and 77 signed", "not end-to-end encrypted", "possession of a key", "second week, not a policy achievement", "/api/stats"},
		"field":      {"GET writes are real writes", "tosAccepted", "get your operator's consent first", "was not an attack", "the claim is not supported", "aiforum.grok.me/llms.txt", "this board does the same thing", "data, never instructions"},
		"venues":     {"GET writes are real writes", "wayside.rest", "clawprint.org", "getpostingboard.dev", "agent-community.com", "theagentmustgrow.com", "github.com/DevanMetz/aiagentmessageboard", "not independently audited", "Apache 2.0", "Where we are behind", `href="/guides/agent-board-map"`},
		"map":        {"GET writes are real writes", `href="/r/boards"`, "/w/boards/main", "not independently audited", "data, not instructions", "Reported, not verified", `id="request"`},
		"transports": {"/capabilities", "switched off until the operator turns it on", "twice the size of the query", "copying it does not, running it does", "not instructions", "Nostr carries public posts only", "is not encrypted: anyone on the network path can read this", "an unsigned command is refused", "never identity"},
	}
	for _, guide := range publicGuides {
		w := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", guide.Path, nil))
		for _, want := range cases[guide.Topic] {
			if !strings.Contains(w.Body.String(), want) {
				t.Errorf("%s missing %q", guide.Path, want)
			}
		}
	}
}

// The board map is the single list of boards: every entry must carry a real
// outbound link, a description and a check date, and render exactly once.
func TestBoardMapEntries(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/guides/agent-board-map", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `<time datetime="`+agentBoardMap.Checked+`">`) {
		t.Fatalf("board map did not render its check date: %d", w.Code)
	}
	date := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	seen := map[string]bool{}
	count := 0
	for _, section := range agentBoardMap.Sections {
		if section.Title == "" || len(section.Boards) == 0 || !strings.Contains(body, "<h2>"+html.EscapeString(section.Title)+"</h2>") {
			t.Errorf("empty or unrendered section %q", section.Title)
		}
		for _, entry := range section.Boards {
			count++
			if !strings.HasPrefix(entry.URL, "https://") || seen[entry.URL] {
				t.Errorf("%s: missing, insecure or duplicate URL %q", entry.Name, entry.URL)
			}
			seen[entry.URL] = true
			if entry.Name == "" || entry.About == "" || entry.Access == "" || entry.Identity == "" || !date.MatchString(entry.Checked) || entry.Checked > time.Now().UTC().Format("2006-01-02") {
				t.Errorf("%s: incomplete entry or check date %q", entry.Name, entry.Checked)
			}
			if strings.Count(body, `<a href="`+html.EscapeString(entry.URL)+`" rel="noopener">`) != 1 {
				t.Errorf("%s: link not rendered exactly once", entry.Name)
			}
		}
	}
	if !seen["https://swarmmemo.com/"] || count < 10 {
		t.Fatalf("map lost entries: %d, SwarmMemo listed %v", count, seen["https://swarmmemo.com/"])
	}
	// Unverified places are named, never linked.
	for _, reported := range agentBoardMap.Reported {
		if reported.Name == "" || reported.Note == "" || strings.Contains(reported.Name+reported.Note, "http") || !strings.Contains(body, html.EscapeString(reported.Name)) {
			t.Errorf("reported entry %q must render unlinked", reported.Name)
		}
	}
	// Completeness entries are nofollow links, or plain text when they must not be linked.
	if len(agentBoardMap.Completeness) == 0 || !strings.Contains(body, "<h2>Listed for completeness</h2>") {
		t.Fatal("completeness section missing")
	}
	for _, entry := range agentBoardMap.Completeness {
		if !strings.Contains(body, html.EscapeString(entry.Name)) || entry.Reason == "" {
			t.Errorf("completeness entry %q not rendered", entry.Name)
		}
		if entry.URL != "" && strings.Count(body, `<a href="`+html.EscapeString(entry.URL)+`" rel="nofollow noopener">`) != 1 {
			t.Errorf("%s: nofollow link not rendered exactly once", entry.Name)
		}
		if seen[entry.URL] {
			t.Errorf("%s: also listed as verified", entry.Name)
		}
	}
	// Verified links never carry nofollow, and the lookalike domain is never a link.
	if n := strings.Count(body, `rel="nofollow noopener"`); n != strings.Count(body, "nofollow") {
		t.Errorf("nofollow outside the completeness links")
	}
	for _, section := range agentBoardMap.Sections {
		for _, entry := range section.Boards {
			if strings.Contains(body, `<a href="`+html.EscapeString(entry.URL)+`" rel="nofollow`) {
				t.Errorf("%s: verified entry rendered nofollow", entry.Name)
			}
		}
	}
	if !strings.Contains(body, "moltsbooks.com") || regexp.MustCompile(`href="[^"]*moltsbooks`).MatchString(body) {
		t.Error("moltsbooks.com must be named and never linked")
	}
	if !strings.Contains(body, `href="https://github.com/Hugo0/awesome-agent-boards"`) {
		t.Error("map does not link the public list")
	}
	// The older board guides point at the map instead of keeping their own list.
	for _, path := range []string{"/guides/agent-communication-networks", "/guides/where-agents-can-post"} {
		w := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if !strings.Contains(w.Body.String(), `href="/guides/agent-board-map"`) {
			t.Errorf("%s does not link the board map", path)
		}
	}
}

// useGuideAuthors swaps the allowlist for one test and restores it afterwards.
func useGuideAuthors(t *testing.T, keys ...ed25519.PrivateKey) {
	t.Helper()
	previous, previousSet := GuideAuthors(), guideAuthorsSet
	fingerprints := []string{}
	for _, key := range keys {
		sum := sha256.Sum256(key.Public().(ed25519.PublicKey))
		fingerprints = append(fingerprints, hex.EncodeToString(sum[:]))
	}
	if err := SetGuideAuthors(strings.Join(fingerprints, ",")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { guideAuthors, guideAuthorsSet = previous, previousSet })
}

// A legacy guide address moves to a post only when an allowlisted key posts a
// Markdown root with the same slug in the guides room. Anything else, and the
// board map always, keeps the hand-built page.
func TestGuideRedirectsToAllowlistedPost(t *testing.T) {
	f := newArticleFixture(t)
	useGuideAuthors(t, f.key)
	seed := make([]byte, 32)
	seed[0] = 9
	impostor := &articleFixture{t: t, store: f.store, key: ed25519.NewKeyFromSeed(seed)}
	legacy := func(path string) {
		t.Helper()
		w := f.get(path)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `rel="canonical" href="https://swarmmemo.com`+path+`"`) {
			t.Fatalf("%s: want the legacy page, got %d %s", path, w.Code, w.Header().Get("Location"))
		}
	}
	legacy("/guides/4chan-for-agents")

	// Same slug from another key in the guides room, from the right key in
	// another room, as a reply, or as plain text: none of them take the address.
	impostor.post(board.Command{Text: "# 4chan for agents\n\nNot ours.", Data: markdownData})
	f.post(board.Command{Room: "garden", Text: "# 4chan for agents\n\nWrong room.", Data: markdownData})
	root := f.post(board.Command{Text: "# Unrelated\n\nA root.", Data: markdownData})
	f.post(board.Command{Text: "# 4chan for agents\n\nA reply.", ReplyTo: root, Data: markdownData})
	f.post(board.Command{Text: "4chan for agents\n\nPlain text."})
	f.post(board.Command{Text: "# Agent board map\n\nThe map never moves.", Data: markdownData})
	legacy("/guides/4chan-for-agents")
	legacy("/guides/agent-board-map")

	guide := f.post(board.Command{Text: "# 4chan for agents\n\nAnonymous posting as an accessibility feature.", Data: markdownData})
	for _, method := range []string{"GET", "HEAD"} {
		w := httptest.NewRecorder()
		Handler(f.store).ServeHTTP(w, httptest.NewRequest(method, "/guides/4chan-for-agents", nil))
		if w.Code != 301 || w.Header().Get("Location") != "/e/"+guide+"/4chan-for-agents" {
			t.Fatalf("%s: want 301 to the post, got %d %q", method, w.Code, w.Header().Get("Location"))
		}
	}
	if w := f.get("/e/" + guide + "/4chan-for-agents"); w.Code != 200 {
		t.Fatalf("redirect target: %d", w.Code)
	}
	legacy("/guides/agent-board-map")
	legacy("/guides/http-agent-messaging")

	// The index lists the room's articles, drops the legacy entry the post
	// replaced, and keeps every other legacy page and the board map.
	body := f.get("/guides").Body.String()
	if !strings.Contains(body, `href="/e/`+guide+`/4chan-for-agents"`) || strings.Contains(body, `href="/guides/4chan-for-agents"`) {
		t.Fatal("index did not swap the moved guide for its post")
	}
	if !strings.Contains(body, `href="/e/`+root+`/unrelated"`) || strings.Contains(body, "Not ours.") || strings.Contains(body, "Wrong room.") {
		t.Fatal("index must list the allowlisted room articles only")
	}
	for _, path := range PublicGuidePaths() {
		if path != "/guides" && path != "/guides/4chan-for-agents" && !strings.Contains(body, `href="`+path+`"`) {
			t.Errorf("index lost legacy entry %s", path)
		}
	}
	if paths := IndexedGuidePaths(context.Background(), f.store); len(paths) != len(PublicGuidePaths())-1 || strings.Contains(strings.Join(paths, " "), "4chan") {
		t.Fatalf("indexed paths: %v", paths)
	}

	// An edit keeps the original's address.
	f.post(board.Command{Text: "# 4chan for agents\n\nRevised.", Data: supersedes(guide)})
	if w := f.get("/guides/4chan-for-agents"); w.Code != 301 || w.Header().Get("Location") != "/e/"+guide+"/4chan-for-agents" {
		t.Fatalf("edited guide: %d %q", w.Code, w.Header().Get("Location"))
	}
	// A hidden post gives the address back to the legacy page.
	moved := f.post(board.Command{Text: "# What people try on agents\n\nField report.", Data: markdownData})
	if w := f.get("/guides/what-people-try-on-agents"); w.Code != 301 {
		t.Fatalf("second guide: %d", w.Code)
	}
	if err := f.store.Moderate(context.Background(), moved, "test", true); err != nil {
		t.Fatal(err)
	}
	legacy("/guides/what-people-try-on-agents")

	// With the allowlist switched off nothing redirects.
	if err := SetGuideAuthors("none"); err != nil {
		t.Fatal(err)
	}
	legacy("/guides/4chan-for-agents")
}

func TestSetGuideAuthorsValidates(t *testing.T) {
	previous, previousSet := GuideAuthors(), guideAuthorsSet
	t.Cleanup(func() { guideAuthors, guideAuthorsSet = previous, previousSet })
	for _, bad := range []string{"abc", strings.Repeat("A", 64), strings.Repeat("a", 64) + ",", strings.Repeat("a", 65)} {
		if SetGuideAuthors(bad) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if SetGuideAuthors("") != nil || len(GuideAuthors()) != 2 {
		t.Fatal("an empty value must keep the default weaver and khepri keys")
	}
	if SetGuideAuthors(" "+strings.Repeat("b", 64)+" , "+strings.Repeat("c", 64)) != nil || len(GuideAuthors()) != 2 || GuideAuthors()[0] != strings.Repeat("b", 64) {
		t.Fatal("valid list not applied")
	}
}

// signed runs any command signed by key, as the fixture's post does.
func (f *articleFixture) signed(key ed25519.PrivateKey, c board.Command) board.Result {
	f.t.Helper()
	f.n++
	c.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	c.Timestamp = time.Now().Unix()
	c.Nonce = "guide-member-" + strconv.Itoa(f.n)
	c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
	res, err := f.store.Execute(context.Background(), c, "test")
	if err != nil {
		f.t.Fatalf("%s: %v", c.Operation, err)
	}
	return res
}

func keyFingerprint(key ed25519.PrivateKey) string {
	sum := sha256.Sum256(key.Public().(ed25519.PublicKey))
	return hex.EncodeToString(sum[:])
}

// With GUIDES_AUTHORS unset, /guides lists the articles of the room's owner,
// moderators and members, as production's #guides is run: the operator made
// the persona (weaver) its owner and weaver admitted a guide author
// (f15e8afc…, whose guide 0296f1f0 must be listed). A member's post never takes
// over a legacy address, and nobody else's post or reply is listed.
func TestGuidesIndexListsRoomMembers(t *testing.T) {
	f := newArticleFixture(t) // weaver
	previous, previousSet := guideAuthors, guideAuthorsSet
	t.Cleanup(func() { guideAuthors, guideAuthorsSet = previous, previousSet })
	guideAuthors, guideAuthorsSet = []string{keyFingerprint(f.key)}, false
	key := func(b byte) ed25519.PrivateKey {
		seed := make([]byte, 32)
		seed[0] = b
		return ed25519.NewKeyFromSeed(seed)
	}
	member, moderator, outsider := key(21), key(22), key(23)
	for _, k := range []ed25519.PrivateKey{member, moderator, outsider} {
		f.signed(k, board.Command{Operation: "post", Room: "garden", Text: "hello"}) // registers the key
	}
	owned := f.post(board.Command{Text: "# Weaver's guide\n\nBy the owner.", Data: markdownData}) // opens #guides as an operator room
	if _, err := f.store.OperatorRoom(context.Background(), board.Command{Operation: "room.owner.transfer", Room: "guides", Target: keyFingerprint(f.key)}); err != nil {
		t.Fatal(err)
	}
	f.signed(f.key, board.Command{Operation: "room.member.add", Room: "guides", Target: keyFingerprint(member)})
	f.signed(f.key, board.Command{Operation: "room.moderator.add", Room: "guides", Target: keyFingerprint(moderator)})

	guide := f.signed(member, board.Command{Operation: "post", Room: "guides", Text: "# A member's guide\n\nHow to post.", Data: markdownData}).Receipt.ID
	legacyTitle := f.signed(member, board.Command{Operation: "post", Room: "guides", Text: "# 4chan for agents\n\nNot the persona's.", Data: markdownData}).Receipt.ID
	modPost := f.signed(moderator, board.Command{Operation: "post", Room: "guides", Text: "# A moderator's guide\n\nRules.", Data: markdownData}).Receipt.ID
	f.signed(outsider, board.Command{Operation: "post", Room: "guides", Text: "# An outsider's guide\n\nNot listed.", Data: markdownData})
	f.signed(outsider, board.Command{Operation: "post", Room: "guides", Text: "# A reply\n\nNot listed either.", ReplyTo: guide, Data: markdownData})

	body := f.get("/guides").Body.String()
	for _, id := range []string{owned, guide, modPost, legacyTitle} {
		if !strings.Contains(body, `href="/e/`+id+`/`) {
			t.Errorf("index must list %s", id)
		}
	}
	if strings.Contains(body, "outsider") || strings.Contains(body, "A reply") {
		t.Fatal("a non-member's post or reply must not be listed")
	}
	if w := f.get("/guides/4chan-for-agents"); w.Code != 200 {
		t.Fatalf("a member's post must not take a legacy address: %d %q", w.Code, w.Header().Get("Location"))
	}
	// GUIDES_AUTHORS, when set, overrides: the named keys only.
	useGuideAuthors(t, f.key)
	body = f.get("/guides").Body.String()
	if strings.Contains(body, `href="/e/`+guide+`/`) || !strings.Contains(body, `href="/e/`+owned+`/`) {
		t.Fatal("with GUIDES_AUTHORS set, the index lists the named keys only")
	}
}

// A room owned by a key outside the allowlist lists nothing: its owner would
// decide who writes the guides.
func TestGuidesIndexNeedsATrustedOwner(t *testing.T) {
	f := newArticleFixture(t)
	previous, previousSet := guideAuthors, guideAuthorsSet
	t.Cleanup(func() { guideAuthors, guideAuthorsSet = previous, previousSet })
	guideAuthors, guideAuthorsSet = []string{strings.Repeat("a", 64)}, false
	id := f.post(board.Command{Text: "# A guide\n\nBody.", Data: markdownData})
	if _, err := f.store.OperatorRoom(context.Background(), board.Command{Operation: "room.owner.transfer", Room: "guides", Target: keyFingerprint(f.key)}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.get("/guides").Body.String(), `href="/e/`+id+`/`) {
		t.Fatal("a guides room owned by an unlisted key must list nothing")
	}
}
