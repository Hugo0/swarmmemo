package cards

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSource is a board in memory: posts by ID and rooms by name, each
// removable to stand in for a hide.
type fakeSource struct {
	mu    sync.Mutex
	posts map[string]Card
	rooms map[string]Card
	reads int
}

func newFakeSource() *fakeSource {
	return &fakeSource{posts: map[string]Card{}, rooms: map[string]Card{}}
}

func (f *fakeSource) PostCard(_ context.Context, id string) (Card, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	c, ok := f.posts[id]
	if !ok {
		return Card{}, ErrNotFound
	}
	return c, nil
}

func (f *fakeSource) RoomCard(_ context.Context, room string) (Card, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	c, ok := f.rooms[room]
	if !ok {
		return Card{}, ErrNotFound
	}
	c.Items = append([]Item(nil), c.Items...)
	return c, nil
}

func (f *fakeSource) setPost(id string, c Card) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts[id] = c
}

func (f *fakeSource) setRoom(name string, c Card) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rooms[name] = c
}

func (f *fakeSource) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.posts, id)
}

const postID = "0123456789abcdef0123456789abcdef"

func newTestService(t *testing.T, src Source, mutate func(*Config)) (*Service, *time.Time) {
	t.Helper()
	cfg := Config{Renderer: RendererLocal, CacheDir: filepath.Join(t.TempDir(), "images"), CacheBytes: 8 << 20,
		DailyRenders: 100, CloudflareDaily: 100, CloudflareInterval: time.Second, RoomFresh: 10 * time.Minute, FallbackRetry: time.Hour}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := New(cfg, src)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, &now
}

func renders(s *Service) int { return s.counter.get(s.now(), "renders") }

func decodeCard(t *testing.T, raw []byte) {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("not a PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != Width || b.Dy() != Height {
		t.Fatalf("card is %v, want %dx%d", b, Width, Height)
	}
}

func TestParsePaths(t *testing.T) {
	for _, tc := range []struct {
		path, kind, key string
		ok              bool
	}{
		{"/e/" + postID + ".png", Post, postID, true},
		{"/r/lobby.png", Room, "lobby", true},
		{"/r/@" + strings.Repeat("a", 64) + ".png", Room, "@" + strings.Repeat("a", 64), true},
		{"/e/" + strings.ToUpper(postID) + ".png", "", "", false},
		{"/e/" + postID + ".PNG", "", "", false},
		{"/e/" + postID + ".png.png", "", "", false},
		{"/e/" + postID, "", "", false},
		{"/e/" + postID + "/x.png", "", "", false},
		{"/r/lobby/main.png", "", "", false},
		{"/r/Lobby.png", "", "", false},
		{"/r/.png", "", "", false},
		{"/r/../etc.png", "", "", false},
		{"/x/lobby.png", "", "", false},
		{"/e/https://evil.example/a.png", "", "", false},
		{"/r/lobby.png?url=https://evil.example", "", "", false},
	} {
		kind, key, ok := ParseImagePath(tc.path)
		if ok != tc.ok || (ok && (kind != tc.kind || key != tc.key)) {
			t.Errorf("ParseImagePath(%q) = %q %q %v", tc.path, kind, key, ok)
		}
	}
	if kind, key, ok := ParseRenderPath("/render/e/" + postID); !ok || kind != Post || key != postID {
		t.Fatal("render post path")
	}
	if kind, key, ok := ParseRenderPath("/render/r/lobby"); !ok || kind != Room || key != "lobby" {
		t.Fatal("render room path")
	}
	for _, bad := range []string{"/render/e/" + postID + ".png", "/render/https://evil.example", "/render/r/", "/render//r/lobby", "/renderx/r/lobby"} {
		if _, _, ok := ParseRenderPath(bad); ok {
			t.Errorf("ParseRenderPath(%q) accepted", bad)
		}
	}
}

// FuzzParsePaths: whatever the path, an accepted one names exactly a valid
// message ID or room and round-trips; nothing resembling a URL, query,
// traversal or encoded byte gets through.
func FuzzParsePaths(f *testing.F) {
	for _, seed := range []string{"/e/" + postID + ".png", "/r/lobby.png", "/render/r/lobby", "/render/e/" + postID,
		"/e/%2e%2e.png", "/r/a/b.png", "/e/http://x.png", "/r/lobby.png?x", "/r/@" + strings.Repeat("0", 64) + ".png", "/r/\u202elobby.png"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, path string) {
		check := func(kind, key string) {
			if kind != Post && kind != Room {
				t.Fatalf("kind %q", kind)
			}
			if kind == Post && !ValidMessageID(key) || kind == Room && !ValidRoom(key) {
				t.Fatalf("invalid key %q accepted", key)
			}
			if strings.ContainsAny(key, "/?#%:\\. \x00") {
				t.Fatalf("key %q carries a URL character", key)
			}
		}
		if kind, key, ok := ParseImagePath(path); ok {
			check(kind, key)
			if ImagePath(kind, key) != path {
				t.Fatalf("%q does not round-trip", path)
			}
		}
		if kind, key, ok := ParseRenderPath(path); ok {
			check(kind, key)
			if "/render/"+kind+"/"+key != path {
				t.Fatalf("%q does not round-trip", path)
			}
		}
	})
}

func TestRenderHTMLIsSelfContainedAndEscaped(t *testing.T) {
	page := string(RenderHTML(Card{Kind: Post, Heading: "#lobby", Title: `<script>alert(1)</script>`,
		Meta: `" onload="x`, Body: `<img src="https://evil.example/x.png"> <link rel=stylesheet href=//evil.example>`, Footer: "swarmmemo.com/e/" + postID}))
	// Post text may mention URLs; it must stay text. No element or attribute
	// that fetches anything may appear.
	for _, forbidden := range []string{"<script", "<img", "<link", "<iframe", "<object", ` src="`, ` href="`, "url(", "@import"} {
		if strings.Contains(page, forbidden) {
			t.Errorf("card page contains %q", forbidden)
		}
	}
	if !strings.Contains(page, `<meta http-equiv="Content-Security-Policy" content="default-src &#39;none&#39;; style-src &#39;sha256-`) {
		t.Errorf("card page lacks its CSP meta: %s", page)
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Error("title not escaped")
	}
	if !strings.Contains(PageCSP, styleHash()) || strings.Contains(PageCSP, "unsafe") {
		t.Errorf("policy %q", PageCSP)
	}
}

func TestDrawPNG(t *testing.T) {
	long := strings.Repeat("word ", 400) + strings.Repeat("x", 300) + "\n\n\n\nend 中文 🙂 \u202eevil"
	for _, c := range []Card{
		{Kind: Post, Heading: "#lobby", Title: long, Meta: long, Body: long, Footer: long},
		{Kind: Room, Heading: "public room", Title: "#lobby", Items: []Item{{Meta: long, Text: long}, {Meta: "a", Text: "b"}, {}, {}, {}, {}}},
		{},
		placeholderCard,
	} {
		raw, err := DrawPNG(c)
		if err != nil {
			t.Fatal(err)
		}
		decodeCard(t, raw)
		if len(raw) > MaxImageBytes {
			t.Fatalf("local card is %d bytes", len(raw))
		}
	}
}

func TestWrap(t *testing.T) {
	got := wrap("aaa bbb ccc\n\n\n\nddd", 7, 10)
	want := []string{"aaa bbb", "ccc", "", "ddd"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("wrap = %q", got)
	}
	got = wrap(strings.Repeat("x", 20), 8, 2)
	if len(got) != 2 || got[0] != "xxxxxxxx" || !strings.HasSuffix(got[1], "…") || len([]rune(got[1])) != 8 {
		t.Fatalf("hard wrap = %q", got)
	}
	if wrap("x", 0, 3) != nil || wrap("x", 3, 0) != nil {
		t.Fatal("degenerate wrap")
	}
}

func TestCleanDropsControlAndBidi(t *testing.T) {
	if got := Clean("a\x00b\u202ec\u2066d\te\nf\xff"); got != "abcd  e\nf\uFFFD" {
		t.Fatalf("Clean = %q", got)
	}
}

func TestCacheHitAndInvalidation(t *testing.T) {
	src := newFakeSource()
	src.setPost(postID, Card{Heading: "#lobby", Title: "First", Body: "one", Footer: "x"})
	s, _ := newTestService(t, src, nil)
	ctx := context.Background()

	first, err := s.Image(ctx, Post, postID)
	if err != nil {
		t.Fatal(err)
	}
	decodeCard(t, first.PNG)
	again, err := s.Image(ctx, Post, postID)
	if err != nil || !bytes.Equal(first.PNG, again.PNG) || again.ETag != first.ETag || renders(s) != 1 {
		t.Fatalf("second read was not a cache hit: renders=%d err=%v", renders(s), err)
	}
	// A restart keeps the cache and the day's count.
	s2, _ := newTestService(t, src, func(c *Config) { c.CacheDir = s.cfg.CacheDir })
	if img, err := s2.Image(ctx, Post, postID); err != nil || img.ETag != first.ETag || renders(s2) != 1 {
		t.Fatalf("cache or count lost across restart: renders=%d", renders(s2))
	}

	// An edit changes what the card shows: re-rendered, not served stale.
	src.setPost(postID, Card{Heading: "#lobby", Title: "Second", Body: "two", Footer: "x"})
	edited, err := s.Image(ctx, Post, postID)
	if err != nil || edited.ETag == first.ETag || renders(s) != 2 {
		t.Fatalf("edit not re-rendered: renders=%d", renders(s))
	}

	// A hide: not found, and the stored image is gone from disk too.
	src.remove(postID)
	if _, err := s.Image(ctx, Post, postID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hidden post served: %v", err)
	}
	if _, ok := s.cache.get(Post, postID); ok {
		t.Fatal("hidden post's image still cached")
	}
	if _, err := os.Stat(filepath.Join(s.cfg.CacheDir, cacheName(Post, postID))); !os.IsNotExist(err) {
		t.Fatal("hidden post's file still on disk")
	}
	if _, err := s.Page(ctx, Post, postID); !errors.Is(err, ErrNotFound) {
		t.Fatal("hidden post's card page served")
	}
}

func TestRoomCardLagsNewPostsButNeverHides(t *testing.T) {
	src := newFakeSource()
	items := []Item{{ID: "a", Meta: "m", Text: "alpha"}, {ID: "b", Meta: "m", Text: "bravo"}, {ID: "c", Meta: "m", Text: "charlie"}, {ID: "d", Meta: "m", Text: "delta"}, {ID: "e", Meta: "m", Text: "echo"}}
	src.setRoom("lobby", Card{Title: "#lobby", Meta: "5 public messages", Items: items})
	s, now := newTestService(t, src, nil)
	ctx := context.Background()
	first, err := s.Image(ctx, Room, "lobby")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := s.Page(ctx, Room, "lobby")
	if strings.Contains(string(page), "echo") || !strings.Contains(string(page), "delta") {
		t.Fatalf("room card shows %d items, want %d", strings.Count(string(page), "<li>"), RoomItems)
	}

	// A new post within RoomFresh: the cached card still shows only current posts.
	*now = now.Add(time.Minute)
	src.setRoom("lobby", Card{Title: "#lobby", Meta: "6 public messages", Items: append([]Item{{ID: "f", Meta: "m", Text: "foxtrot"}}, items...)})
	if img, err := s.Image(ctx, Room, "lobby"); err != nil || img.ETag != first.ETag || renders(s) != 1 {
		t.Fatalf("new post forced a re-render inside RoomFresh: renders=%d", renders(s))
	}
	// Hiding a post the cached card shows: re-rendered at once.
	src.setRoom("lobby", Card{Title: "#lobby", Meta: "5 public messages", Items: []Item{{ID: "f", Meta: "m", Text: "foxtrot"}, items[0], items[2], items[3], items[4]}})
	hidden, err := s.Image(ctx, Room, "lobby")
	if err != nil || hidden.ETag == first.ETag || renders(s) != 2 {
		t.Fatalf("hidden post kept in the room card: renders=%d", renders(s))
	}
	page, _ = s.Page(ctx, Room, "lobby")
	if strings.Contains(string(page), "bravo") {
		t.Fatal("room card page shows a hidden post")
	}
	// Past RoomFresh a new post is drawn.
	*now = now.Add(11 * time.Minute)
	src.setRoom("lobby", Card{Title: "#lobby", Meta: "6 public messages", Items: append([]Item{{ID: "g", Meta: "m", Text: "golf"}}, items...)})
	if img, _ := s.Image(ctx, Room, "lobby"); img.ETag == hidden.ETag || renders(s) != 3 {
		t.Fatalf("stale room card past RoomFresh: renders=%d", renders(s))
	}
}

func TestDailyCapServesPlaceholder(t *testing.T) {
	src := newFakeSource()
	other := strings.Repeat("f", 32)
	src.setPost(postID, Card{Title: "one"})
	src.setPost(other, Card{Title: "two"})
	s, now := newTestService(t, src, func(c *Config) { c.DailyRenders = 1 })
	ctx := context.Background()
	if img, err := s.Image(ctx, Post, postID); err != nil || img.Placeholder {
		t.Fatal("first render refused")
	}
	img, err := s.Image(ctx, Post, other)
	if err != nil || !img.Placeholder || !bytes.Equal(img.PNG, s.placeholder) || img.ETag != "placeholder" {
		t.Fatalf("over the cap: placeholder=%v err=%v", img.Placeholder, err)
	}
	decodeCard(t, img.PNG)
	if _, ok := s.cache.get(Post, other); ok {
		t.Fatal("placeholder cached as the post's image")
	}
	// Cached cards are still served past the cap; hidden ones still refused.
	if img, _ := s.Image(ctx, Post, postID); img.Placeholder {
		t.Fatal("cached card replaced by the placeholder")
	}
	src.remove(postID)
	if _, err := s.Image(ctx, Post, postID); !errors.Is(err, ErrNotFound) {
		t.Fatal("cap bypassed the visibility check")
	}
	// A new UTC day has a new budget.
	*now = now.Add(24 * time.Hour)
	if img, _ := s.Image(ctx, Post, other); img.Placeholder {
		t.Fatal("budget did not reset")
	}
	// Zero means no renders at all: fail closed.
	s0, _ := newTestService(t, src, func(c *Config) { c.DailyRenders = 0 })
	if img, _ := s0.Image(ctx, Post, other); !img.Placeholder {
		t.Fatal("zero budget rendered")
	}
}

func TestCacheSizeCapEvicts(t *testing.T) {
	src := newFakeSource()
	s, _ := newTestService(t, src, func(c *Config) { c.CacheBytes = 10 << 10 })
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		id := strings.Repeat(string("0123456789abcdef"[i]), 32)
		src.setPost(id, Card{Title: strings.Repeat("title ", i+1), Body: strings.Repeat("some words here ", 30)})
		if _, err := s.Image(ctx, Post, id); err != nil {
			t.Fatal(err)
		}
	}
	if s.cache.size() > 10<<10+MaxImageBytes {
		t.Fatalf("cache grew to %d", s.cache.size())
	}
	files, _ := filepath.Glob(filepath.Join(s.cfg.CacheDir, "*"+cacheExt))
	if len(files) == 0 || len(files) == 8 {
		t.Fatalf("eviction kept %d files", len(files))
	}
}

func TestUnknownKindsAndKeysAreNotFound(t *testing.T) {
	s, _ := newTestService(t, newFakeSource(), nil)
	ctx := context.Background()
	for _, tc := range [][2]string{{Post, "https://evil.example/"}, {Room, "../x"}, {"x", "lobby"}, {Post, postID}} {
		if _, err := s.Image(ctx, tc[0], tc[1]); !errors.Is(err, ErrNotFound) {
			t.Errorf("%v: %v", tc, err)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, on, err := ConfigFromEnv(env(map[string]string{"IMAGES_RENDERER": "cloudflare"}), "/data"); on || err != nil {
		t.Fatal("images on without IMAGES=true")
	}
	cfg, on, err := ConfigFromEnv(env(map[string]string{"IMAGES": "true"}), "/data")
	if !on || err != nil || cfg.Renderer != RendererLocal || cfg.CacheDir != "/data/images" || cfg.DailyRenders != defaultDailyRenders {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	for _, bad := range []map[string]string{
		{"IMAGES": "yes"},
		{"IMAGES": "true", "IMAGES_RENDERER": "chrome"},
		{"IMAGES": "true", "IMAGES_RENDERER": "cloudflare"},
		{"IMAGES": "true", "IMAGES_RENDERER": "cloudflare", "IMAGES_CLOUDFLARE_TOKEN_FILE": "/nonexistent"},
		{"IMAGES": "true", "IMAGES_DAILY_RENDERS": "-1"},
		{"IMAGES": "true", "IMAGES_CACHE_MIB": "0"},
	} {
		if _, _, err := ConfigFromEnv(env(bad), "/data"); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("  abcdefghijklmnopqrstuvwxyz0123456789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err = ConfigFromEnv(env(map[string]string{"IMAGES": "true", "IMAGES_RENDERER": "cloudflare", "IMAGES_CLOUDFLARE_TOKEN_FILE": token, "IMAGES_CLOUDFLARE_ACCOUNT": strings.Repeat("a", 32)}), "/data")
	if err != nil || cfg.CloudflareToken != "abcdefghijklmnopqrstuvwxyz0123456789" {
		t.Fatalf("token: %v", err)
	}
	if _, err := New(Config{Renderer: RendererCloudflare, CacheDir: t.TempDir(), CacheBytes: 1 << 20, FallbackRetry: time.Hour, CloudflareAccount: "not-an-account", CloudflareToken: cfg.CloudflareToken}, newFakeSource()); err == nil {
		t.Fatal("bad account accepted")
	}
}
