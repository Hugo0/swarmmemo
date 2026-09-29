package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/cards"
	"swarmmemo/internal/web"
)

type imageFixture struct {
	store                         *board.Store
	on, off                       *Server
	public, hidden, private, edit string
	post                          func(board.Command) string
}

// newImageFixture is one board served twice: with images on (local renderer)
// and with images off, as an unconfigured deployment runs.
func newImageFixture(t *testing.T) *imageFixture {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "images.sqlite"), board.Config{ArchiveDelaySeconds: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	n := 0
	f := &imageFixture{store: store}
	f.post = func(c board.Command) string {
		t.Helper()
		n++
		c.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
		c.Timestamp, c.Nonce = time.Now().Unix(), "img"+strconv.Itoa(n)
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
		res, err := store.Execute(context.Background(), c, "test")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		if res.Receipt == nil {
			return ""
		}
		return res.Receipt.ID
	}
	f.public = f.post(board.Command{Operation: "post", Room: "lobby", Text: "Visible findings\n\nThe visible body text."})
	f.hidden = f.post(board.Command{Operation: "post", Room: "lobby", Text: "Doomed secret words"})
	f.edit = f.post(board.Command{Operation: "post", Room: "lobby", Text: "Before the edit"})
	f.post(board.Command{Operation: "room.create", Room: "secret", Visibility: "private"})
	f.private = f.post(board.Command{Operation: "post", Room: "secret", Text: "Private room words"})
	if err := store.Moderate(context.Background(), f.hidden, "spam", true); err != nil {
		t.Fatal(err)
	}
	images, err := cards.New(cards.Config{Renderer: cards.RendererLocal, CacheDir: filepath.Join(t.TempDir(), "images"), CacheBytes: 16 << 20,
		DailyRenders: 1000, CloudflareDaily: 0, CloudflareInterval: time.Second, RoomFresh: 10 * time.Minute, FallbackRetry: time.Hour}, web.CardSource(store))
	if err != nil {
		t.Fatal(err)
	}
	f.on = New(store, web.Handler(store), Config{PublicURL: "https://example.test", ServiceID: "swarmmemo.com", Images: images})
	f.off = New(store, web.Handler(store), Config{PublicURL: "https://example.test", ServiceID: "swarmmemo.com"})
	return f
}

func request(s *Server, method, path, accept string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = "198.51.100.9:12345"
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// withCardImages is the web package's og:image switch for one test.
func withCardImages(t *testing.T, on bool) {
	web.SetCardImages(on)
	t.Cleanup(func() { web.SetCardImages(false) })
}

func TestPostImageServesPNG(t *testing.T) {
	f := newImageFixture(t)
	w := request(f.on, "GET", "/e/"+f.public+".png", "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("%d %s %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	img, err := png.Decode(bytes.NewReader(w.Body.Bytes()))
	if err != nil || img.Bounds().Dx() != cards.Width || img.Bounds().Dy() != cards.Height {
		t.Fatalf("not a card: %v", err)
	}
	etag := w.Header().Get("ETag")
	if etag == "" || !strings.HasPrefix(w.Header().Get("Cache-Control"), "public, max-age=") || w.Header().Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
		t.Fatalf("headers %v", w.Header())
	}
	if again := request(f.on, "GET", "/e/"+f.public+".png", "", "If-None-Match", etag); again.Code != 304 || again.Body.Len() != 0 {
		t.Fatalf("conditional GET: %d", again.Code)
	}
	if head := request(f.on, "HEAD", "/e/"+f.public+".png", ""); head.Code != 200 || head.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %d", head.Code, head.Body.Len())
	}
	// A browser asking for HTML still gets the image, not the post page.
	if html := request(f.on, "GET", "/e/"+f.public+".png", "text/html"); html.Header().Get("Content-Type") != "image/png" {
		t.Fatal("HTML Accept routed an image to the page")
	}
	if room := request(f.on, "GET", "/r/lobby.png", ""); room.Code != 200 || room.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("room image: %d", room.Code)
	}
	if post := request(f.on, "POST", "/e/"+f.public+".png", ""); post.Code != 405 {
		t.Fatalf("POST: %d", post.Code)
	}
}

// Hidden, private and missing posts have no image and no card page; the page
// of a room never carries a hidden post's words.
func TestImagesExcludeHiddenAndPrivate(t *testing.T) {
	f := newImageFixture(t)
	for _, path := range []string{
		"/e/" + f.hidden + ".png", "/render/e/" + f.hidden,
		"/e/" + f.private + ".png", "/render/e/" + f.private,
		"/r/secret.png", "/render/r/secret",
		"/e/" + strings.Repeat("0", 32) + ".png", "/r/nosuchroom.png",
	} {
		w := request(f.on, "GET", path, "")
		if w.Code != 404 || strings.Contains(w.Body.String(), "Doomed") || strings.Contains(w.Body.String(), "Private room") {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	page := request(f.on, "GET", "/render/r/lobby", "")
	body := page.Body.String()
	if page.Code != 200 || !strings.Contains(body, "Visible findings") || strings.Contains(body, "Doomed") || strings.Contains(body, "Private room") {
		t.Fatalf("room card page: %d %s", page.Code, body)
	}
	if csp := page.Header().Get("Content-Security-Policy"); !strings.HasPrefix(csp, "default-src 'none'; style-src 'sha256-") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("card page policy %q", csp)
	}
	if page.Header().Get("X-Robots-Tag") == "" || strings.Contains(body, "<script") {
		t.Fatal("card page indexable or scripted")
	}
	post := request(f.on, "GET", "/render/e/"+f.public, "").Body.String()
	if !strings.Contains(post, "The visible body text.") {
		t.Fatalf("post card page: %s", post)
	}

	// Hiding a post that was already drawn: the image goes at once.
	if w := request(f.on, "GET", "/e/"+f.public+".png", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if err := f.store.Moderate(context.Background(), f.public, "late", true); err != nil {
		t.Fatal(err)
	}
	if w := request(f.on, "GET", "/e/"+f.public+".png", ""); w.Code != 404 {
		t.Fatalf("hidden after caching: %d", w.Code)
	}
	if body := request(f.on, "GET", "/render/r/lobby", "").Body.String(); strings.Contains(body, "Visible findings") {
		t.Fatal("room card page kept a post hidden after caching")
	}
}

// The image routes take a path and nothing else: no query string, so no URL,
// size or renderer can be passed in.
func TestImageRoutesNeverAcceptAURL(t *testing.T) {
	f := newImageFixture(t)
	for _, path := range []string{
		"/e/" + f.public + ".png?url=https://evil.example/",
		"/r/lobby.png?url=http://169.254.169.254/latest/meta-data",
		"/render/r/lobby?url=https://evil.example/",
		"/render/e/" + f.public + "?html=%3Cscript%3E",
		"/r/lobby.png?",
	} {
		if w := request(f.on, "GET", path, ""); w.Code != 400 || !strings.Contains(w.Body.String(), "no_query") {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{
		"/e/https:/evil.example/x.png", "/render/https:/evil.example", "/render/evil.example/x",
		"/r/evil.example.png", "/e/" + f.public + "/.png", "/render/e/" + f.public + ".png",
	} {
		if w := request(f.on, "GET", path, ""); w.Code != 404 || w.Header().Get("Content-Type") == "image/png" {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/e/%2F%2Fevil.example.png", "/render/r/%2e%2e%2fadmin"} {
		if w := request(f.on, "GET", path, ""); w.Code == 200 {
			t.Errorf("%s served", path)
		}
	}
}

// An edit is a new version: the image of either ID shows the newest text.
func TestImageFollowsEdits(t *testing.T) {
	f := newImageFixture(t)
	before := request(f.on, "GET", "/e/"+f.edit+".png", "")
	if before.Code != 200 {
		t.Fatal(before.Code)
	}
	v2 := f.post(board.Command{Operation: "post", Room: "lobby", Text: "After the edit", Data: `{"schema":1,"supersedes":"` + f.edit + `"}`})
	after := request(f.on, "GET", "/e/"+f.edit+".png", "")
	if after.Code != 200 || after.Header().Get("ETag") == before.Header().Get("ETag") || bytes.Equal(after.Body.Bytes(), before.Body.Bytes()) {
		t.Fatal("edited post served its old image")
	}
	for _, id := range []string{f.edit, v2} {
		page := request(f.on, "GET", "/render/e/"+id, "").Body.String()
		if !strings.Contains(page, "After the edit") || strings.Contains(page, "Before the edit") {
			t.Fatalf("card page for %s: %s", id, page)
		}
	}
}

func TestImageURLsInJSONAndCapabilities(t *testing.T) {
	f := newImageFixture(t)
	var one struct {
		Messages []board.Message `json:"messages"`
	}
	if err := json.Unmarshal(request(f.on, "GET", "/e/"+f.public+"?format=json", "").Body.Bytes(), &one); err != nil || len(one.Messages) != 1 {
		t.Fatal(err)
	}
	if one.Messages[0].ImageURL != "https://example.test/e/"+f.public+".png" {
		t.Fatalf("image_url %q", one.Messages[0].ImageURL)
	}
	var list struct {
		Messages []board.Message `json:"messages"`
	}
	_ = json.Unmarshal(request(f.on, "GET", "/api/messages?room=lobby", "").Body.Bytes(), &list)
	seen := 0
	for _, m := range list.Messages {
		switch {
		case m.ID == f.hidden && m.ImageURL != "":
			t.Fatal("tombstone has an image_url")
		case m.ID != f.hidden && m.ImageURL == "":
			t.Fatalf("%s lacks image_url", m.ID)
		case m.ImageURL != "":
			seen++
		}
	}
	if seen == 0 {
		t.Fatal("no image_url in the listing")
	}
	var room struct {
		Room board.Room `json:"room"`
	}
	_ = json.Unmarshal(request(f.on, "GET", "/api/room/lobby", "").Body.Bytes(), &room)
	if room.Room.ImageURL != "https://example.test/r/lobby.png" {
		t.Fatalf("room image_url %q", room.Room.ImageURL)
	}
	var caps map[string]any
	_ = json.Unmarshal(request(f.on, "GET", "/capabilities", "").Body.Bytes(), &caps)
	images, _ := caps["images"].(map[string]any)
	if images["renderer"] != "local" || images["post"] != "/e/MESSAGE_ID.png" || images["renders_urls"] != false {
		t.Fatalf("capabilities images %v", images)
	}
	if spec := request(f.on, "GET", "/openapi.json", "").Body.String(); !strings.Contains(spec, "/e/{message_id}.png") {
		t.Fatal("openapi lacks the image route")
	}
}

func TestOGImageCards(t *testing.T) {
	f := newImageFixture(t)
	withCardImages(t, true)
	post := request(f.on, "GET", "/e/"+f.public, "text/html").Body.String()
	if !strings.Contains(post, `<meta property="og:image" content="https://swarmmemo.com/e/`+f.public+`.png">`) || !strings.Contains(post, `<meta property="og:image:width" content="1200">`) || !strings.Contains(post, `content="summary_large_image"`) {
		t.Fatalf("post page og:image: %s", post[:min(len(post), 3000)])
	}
	room := request(f.on, "GET", "/r/lobby", "text/html").Body.String()
	if !strings.Contains(room, `<meta property="og:image" content="https://swarmmemo.com/r/lobby.png">`) {
		t.Fatal("room page og:image")
	}
	// A removed conversation keeps the logo.
	hidden := request(f.on, "GET", "/e/"+f.hidden, "text/html").Body.String()
	if strings.Contains(hidden, f.hidden+".png") {
		t.Fatal("hidden post page links its image")
	}
}

// With IMAGES off, nothing about images exists: the routes answer as they
// always did, and responses carry no image field, capability or tag.
func TestImagesOffChangesNothing(t *testing.T) {
	f := newImageFixture(t)
	withCardImages(t, false)
	for _, path := range []string{"/e/" + f.public + ".png", "/r/lobby.png", "/render/r/lobby", "/render/e/" + f.public} {
		w := request(f.off, "GET", path, "")
		if w.Header().Get("Content-Type") == "image/png" || strings.Contains(w.Body.String(), "swarmmemo-card") {
			t.Errorf("%s served an image route with images off", path)
		}
	}
	for _, path := range []string{"/e/" + f.public + "?format=json", "/api/messages", "/api/room/lobby", "/api/rooms", "/capabilities", "/openapi.json"} {
		body := request(f.off, "GET", path, "").Body.String()
		if strings.Contains(body, "image_url") || strings.Contains(body, `"images"`) || strings.Contains(body, "/e/{message_id}.png") {
			t.Errorf("%s mentions images with images off", path)
		}
	}
	page := request(f.off, "GET", "/e/"+f.public, "text/html").Body.String()
	if !strings.Contains(page, `content="https://swarmmemo.com/assets/logo-400.png"`) || strings.Contains(page, `content="1200"`) {
		t.Fatal("post page changed with images off")
	}
	// Routes images do not touch answer the same either way. (Feed pages carry
	// a per-request cursor, so they are compared above by their tags instead.)
	for _, path := range []string{"/api/stats", "/health", "/llms.txt", "/docs", "/e/" + f.public + "/history"} {
		a, b := request(f.off, "GET", path, ""), request(f.on, "GET", path, "")
		if a.Code != b.Code || !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) {
			t.Errorf("%s differs between images on and off", path)
		}
	}
}
