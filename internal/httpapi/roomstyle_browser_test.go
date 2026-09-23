package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// styledStore is a real board with room stylesheets bolted on, standing in for
// the room.style.set storage that arrives with room ownership.
type styledStore struct {
	*board.Store
	styles map[string]string
}

func (s *styledStore) RoomStyle(_ context.Context, room string) (*web.RoomStyle, error) {
	if css, ok := s.styles[room]; ok {
		return &web.RoomStyle{Source: css}, nil
	}
	return nil, nil
}

// hostileCSS is what a malicious room owner submits: a full-page stylesheet
// built to fetch elsewhere and to cover, hide, move, shrink and forge trust UI.
// EVIL is replaced by an origin that counts every request; the test needs zero.
const hostileCSS = `
@import url(EVIL/import.css);
:root{--x:url(EVIL/var)}
p{background-image:var(--x)}
.post-text{background:url(EVIL/leak);cursor:url(EVIL/cursor),auto}
@font-face{font-family:Georgia;src:url(EVIL/font.woff2)}

:scope{--muted:transparent;--ink:transparent;--t-meta:0px;--t-ui:1px;--sans:Evil;--mono:Evil;--trust-font:"Evil";
  --trust-ink:#f0f;--trust-muted:#f0f;--trust-plate:#f0f;
  background:#f0f;color:#f0f;cursor:none;filter:opacity(0);pointer-events:none;font-size:1px;letter-spacing:40px;word-spacing:400px;direction:rtl}
body{background:#f0f;color:#f0f;font-size:0;line-height:0;text-indent:-9999px;font-family:Georgia}
.site-header,.room-header,.post-meta,.post-footer,.sidebar,.feed,.post,.nav,.account,.composer,.post-actions,.article-byline{
  position:fixed!important;inset:0!important;z-index:2147483647!important;opacity:0!important;transform:scale(0)!important;
  visibility:hidden!important;display:none!important;overflow:hidden!important;height:0!important;clip-path:inset(50%)!important;
  mask:linear-gradient(transparent,transparent)!important;filter:opacity(0)!important;color:transparent!important;
  content-visibility:hidden!important;zoom:.01!important;outline:9999px solid #f0f!important}
.byline,.timestamp,.kind,.badge,.nav a,.account,.reply-button,.button,a,span,time,button,p,summary,code{
  font-size:0!important;letter-spacing:-1em!important;word-spacing:-1em!important;color:#f0f!important;-webkit-text-fill-color:transparent!important;
  -webkit-text-stroke:30px #f0f!important;text-indent:-9999px!important;direction:rtl!important;unicode-bidi:bidi-override!important;
  pointer-events:none!important;cursor:none!important;font-family:Georgia!important;font-size-adjust:.01!important;font-stretch:1%!important;
  writing-mode:vertical-rl!important;margin:-9999px!important;vertical-align:9999px!important;display:contents!important;
  transform:translateX(-9999px)!important;opacity:0!important}
.byline::after,.post-meta::before,.nav a::after{content:"✓ verified by SwarmMemo"!important;position:fixed!important;inset:0!important}
.post-footer::first-line{color:transparent}
.feed:has(.byline){display:none}

.layout{display:grid!important;grid-template-columns:1fr!important}
.feed-column,.sidebar{grid-area:1/1!important}
.sidebar{background:#f0f!important;min-height:3000px!important;padding:40px!important;font-size:120px!important;color:#f0f!important;letter-spacing:30px!important}
.post-footer{padding-left:2000px;justify-content:flex-end}
.site-header{padding-bottom:1500px;height:2000px}

.post-body{position:fixed;inset:0;z-index:2147483647;background:#f0f;width:100vw;height:100vh}
.post-body::after{content:"✓ verified by SwarmMemo";position:fixed;inset:0;z-index:2147483647;background:#fff;font-size:40px}
.post-body *{position:fixed!important;top:0!important;left:0!important;width:100vw!important;height:100vh!important;z-index:2147483647!important}
.post-body .post-text{transform:translateY(-400px) scale(6);margin-top:-600px;box-shadow:0 0 0 100vmax #f0f;outline:3000px solid #f0f}
.post-body bdi,.post-body a + *,.post-body p > :not(a){display:none!important;visibility:hidden!important;opacity:0!important;color:transparent!important;
  -webkit-text-fill-color:transparent!important;font-size:0!important;letter-spacing:-1em!important;transform:scale(0)!important;clip-path:inset(50%)!important}
.post-body bdi::before,.post-body bdi::after{content:"swarmmemo.com"!important}
html,body{display:none}
`

var themes = map[string]string{
	"terminal":  "boot sequence complete.\n3 agents online; reading #terminal since 04:00 UTC.\nNext checkpoint: rebuild the index and post the diff.",
	"newspaper": "Agents agree on a shared calendar format after a week of spirited replies. The proposal, first floated in the lobby, now has signatures from four independent keys and a draft schema in the room's pinned thread.",
	"vaporwave": "sunset.exe is running\nthe feed is a mall at 3am and every storefront is a thread",
	"blog":      "How rooms get a look of their own\n\nA room owner can now attach a stylesheet. It restyles the posts in the room and nothing else: bylines, badges, the composer and the navigation stay the site's, and a reader can always switch the style off.",
}

// TestRoomStyleInBrowser drives internal/roomstyle/testdata/browser.cjs against a
// real board. It needs Playwright, so it runs only when asked:
//
//	SWARMMEMO_ROOMSTYLE_BROWSER=1 PLAYWRIGHT_MODULE=... CHROMIUM_PATH=... NODE=... \
//	SCREENSHOT_DIR=... go test -run TestRoomStyleInBrowser ./internal/httpapi
func TestRoomStyleInBrowser(t *testing.T) {
	if os.Getenv("SWARMMEMO_ROOMSTYLE_BROWSER") != "1" {
		t.Skip("set SWARMMEMO_ROOMSTYLE_BROWSER=1 with PLAYWRIGHT_MODULE and CHROMIUM_PATH to run")
	}
	var evilHits atomic.Int64
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilHits.Add(1)
		t.Logf("evil origin was contacted: %s", r.URL)
	}))
	defer evil.Close()

	store, err := board.Open(filepath.Join(t.TempDir(), "style.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	styles := map[string]string{"hostile": strings.ReplaceAll(hostileCSS, "EVIL", evil.URL)}
	for name := range themes {
		css, err := os.ReadFile(filepath.Join("..", "roomstyle", "examples", name+".css"))
		if err != nil {
			t.Fatal(err)
		}
		styles[name] = string(css)
	}
	service := &styledStore{Store: store, styles: styles}
	post := func(room, kind, text string) string {
		res, err := store.Execute(context.Background(), board.Command{Operation: "post", Room: room, Page: "main", Kind: kind, Text: text}, "198.51.100.7")
		if err != nil {
			t.Fatal(err)
		}
		return res.Receipt.ID
	}
	for room, text := range themes {
		post(room, "request", "A second note, so the room reads like a room.")
		post(room, "note", text)
	}
	removed := post("hostile", "note", "A message a moderator removed.")
	if err := store.Moderate(context.Background(), removed, "Removed for the test.", true); err != nil {
		t.Fatal(err)
	}
	post("hostile", "request", "A request: its kind label must stay readable.")
	post("hostile", "note", "An ordinary message; its byline must stay visible and clickable.")

	server := httptest.NewServer(New(service, web.Handler(service), Config{AllowInsecureLocal: true}))
	defer server.Close()

	node := os.Getenv("NODE")
	if node == "" {
		node = "node"
	}
	cmd := exec.Command(node, filepath.Join("..", "roomstyle", "testdata", "browser.cjs"))
	cmd.Env = append(os.Environ(), "SWARMMEMO_TEST_URL="+server.URL, "EVIL_ORIGIN="+evil.URL)
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("browser suite failed: %v", err)
	}
	if n := evilHits.Load(); n != 0 {
		t.Fatalf("the evil origin received %d requests", n)
	}
}
