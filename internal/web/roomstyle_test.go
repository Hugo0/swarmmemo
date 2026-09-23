package web

import (
	"context"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/roomstyle"
)

// styledService is a testService whose rooms carry stylesheets.
type styledService struct {
	testService
	styles map[string]string
}

func (s *styledService) RoomStyle(_ context.Context, room string) (*RoomStyle, error) {
	if css, ok := s.styles[room]; ok {
		return &RoomStyle{Source: css, Owner: strings.Repeat("ab", 32)}, nil
	}
	return nil, nil
}

const markdownBody = "# A guide\n\nSee [the docs](https://example.com/docs).\n\n- one\n- two\n\n| a | b | c |\n|:--|--:|:-:|\n| 1 | 2 | 3 |\n"

const key = "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"

// Every message shape a room page can render, so the canvas test sees all trust UI.
func variety(room string) []board.Message {
	image := board.Attachment{ID: "0123456789abcdef0123456789abcdef", Filename: "a.png", MediaType: "image/png", Size: 9, Hash: strings.Repeat("0", 64)}
	file := board.Attachment{ID: "fedcba9876543210fedcba9876543210", Filename: "notes.txt", MediaType: "text/plain", Size: 9, Hash: strings.Repeat("1", 64)}
	return []board.Message{
		{ID: "m1", Sequence: 1, Room: room, Page: "main", Kind: "note", Text: "anonymous", Handle: "claimed", Via: "dns"},
		{ID: "m2", Sequence: 2, Room: room, Page: "main", Kind: "request", Text: "signed", PublicKey: key, Author: key, Handle: "weaver", To: key, ReplyTo: "m1"},
		{ID: "m3", Sequence: 3, Room: room, Page: "ideas", Kind: "imported", Curated: true, Text: curatorDisclosure + "\nsummary\nSource: https://example.com/x"},
		{ID: "m4", Sequence: 4, Room: room, Page: "main", Kind: "simulation", Text: "sim", PublicKey: key, Author: key, DelegationID: key},
		{ID: "m5", Sequence: 5, Room: room, Page: "main", Kind: "note", Text: "gone", Hidden: true, Reason: "Spam."},
		{ID: "m6", Sequence: 6, Room: room, Page: "main", Kind: "result", Text: "files", PublicKey: key, Author: key, Attachments: []board.Attachment{image, image, file}},
		{ID: "m7", Sequence: 7, Room: room, Page: "main", Kind: "note", Format: board.PostFormatMarkdown, ReplyTo: "m1", PublicKey: key, Author: key, Text: markdownBody},
	}
}

func styled(styles map[string]string, visibility string) *styledService {
	s := &styledService{styles: styles}
	s.execute = func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			r := &board.Room{Name: c.Room, Visibility: visibility, Policy: &board.RoomPolicy{Write: "open", Reply: "anyone", Rules: "Be kind."}}
			if c.Room == "wire" {
				r.Policy.WriteVia = []string{"dns"}
			}
			return board.Result{OK: true, Room: r}, nil
		case "messages.list":
			return board.Result{OK: true, Messages: variety(c.Room)}, nil
		case "thread.get":
			// A Markdown root, so the conversation renders as an article.
			messages := variety("lobby")
			messages[0].Format, messages[0].Text, messages[0].PublicKey, messages[0].Author = board.PostFormatMarkdown, markdownBody, key, key
			return board.Result{OK: true, Messages: messages, Data: map[string]any{"root_id": "m1"}}, nil
		}
		return board.Result{OK: true}, nil
	}
	return s
}

func get(t *testing.T, s board.Service, path, host string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Host = host
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, r)
	return w
}

var (
	tagPattern   = regexp.MustCompile(`<(/?)([a-zA-Z][a-zA-Z0-9]*)([^>]*?)(/?)>`)
	classPattern = regexp.MustCompile(`\bclass="([^"]*)"`)
	voidTags     = []string{"img", "input", "br", "meta", "link", "hr", "path", "circle", "line", "source"}
)

// classesByCanvas walks the page's tags and reports every class used inside a
// canvas and every class used outside one. inFree maps each class (and each
// id, as "#id", and each form control, as "<tag>") used inside a free-hook
// element to that element's class.
func classesByCanvas(t *testing.T, body string) (inside, outside map[string]bool, inFree map[string]string) {
	t.Helper()
	inside, outside, inFree = map[string]bool{}, map[string]bool{}, map[string]string{}
	type open struct {
		tag    string
		canvas bool
		free   string
	}
	var stack []open
	depth := 0
	free := func() string {
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i].free != "" {
				return stack[i].free
			}
		}
		return ""
	}
	for _, m := range tagPattern.FindAllStringSubmatch(body, -1) {
		closing, tag, attrs, self := m[1] == "/", strings.ToLower(m[2]), m[3], m[4] == "/"
		if closing {
			for len(stack) > 0 {
				top := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if top.canvas {
					depth--
				}
				if top.tag == tag {
					break
				}
			}
			continue
		}
		var classes []string
		if c := classPattern.FindStringSubmatch(attrs); c != nil {
			classes = strings.Fields(c[1])
		}
		if holder := free(); holder != "" {
			for _, c := range classes {
				inFree[c] = holder
			}
			if id := idPattern.FindStringSubmatch(attrs); id != nil {
				inFree["#"+id[1]] = holder
			}
			if slices.Contains([]string{"button", "textarea", "input", "select", "form"}, tag) {
				inFree["<"+tag+">"] = holder
			}
		}
		for _, c := range classes {
			if depth > 0 {
				inside[c] = true
			} else {
				outside[c] = true
			}
		}
		if self || slices.Contains(voidTags, tag) {
			continue
		}
		canvas := slices.Contains(classes, "room-canvas")
		if canvas {
			depth++
		}
		holder := ""
		for _, c := range classes {
			if slices.Contains(roomstyle.FreeClasses, c) {
				holder = c
			}
		}
		stack = append(stack, open{tag, canvas, holder})
	}
	if depth != 0 {
		t.Fatalf("unbalanced canvas markup")
	}
	return inside, outside, inFree
}

var idPattern = regexp.MustCompile(`\bid="([^"]*)"`)

// trustUI is the reserved set: bylines and controls, whose look a room may
// colour but never hide, cover, shrink or re-letter. Each is pinned in style.css.
var trustUI = []string{".author", ".worker-label", ".reply-button", ".report-button", ".compose-destination",
	".compose-actions .button", "[id=compose-identity]", ".site-nav a", ".workspace-link", ".article-byline",
	".composer>summary", "[id=memo-text]", "[id=clear-reply]", "[id=compose-settings]>summary"}

// trustClasses and trustIDs are what no free-hook element may contain: the
// bylines, the controls and the composer.
var (
	trustClasses = []string{"author", "worker-label", "reply-button", "report-button", "memo-actions", "memo-bottom", "compose-destination", "compose-actions",
		"composer", "site-nav", "workspace-link", "article-byline", "room-style-strip", "memo", "room-canvas", "feed"}
	trustIDs = []string{"#compose", "#compose-form", "#memo-text", "#compose-identity", "#clear-reply", "#room-style-toggle"}
)

// The trust-UI rule, checked on the rendered page rather than trusted to the
// templates. A canvas (the body zone, where room CSS may do almost anything)
// holds only body classes; bylines and controls are outside every canvas and
// every free-hook element, where the page-zone rules and the pins apply; every
// trust element is pinned; every hook exists.
func TestCanvasHoldsOnlyContentAndTrustUIStaysOutside(t *testing.T) {
	seen := map[string]bool{}
	for _, path := range []string{"/r/lobby", "/e/m1", "/e/m2", "/r/wire"} {
		room := "lobby"
		if path == "/r/wire" {
			room = "wire"
		}
		sheet, _, _ := roomstyle.Sanitize("p{color:red}", room)
		w := get(t, styled(map[string]string{room: "p{color:red}"}, "public"), path, "swarmmemo.com")
		body := w.Body.String()
		if !strings.Contains(body, `<html lang="en" class="room-styled `+sheet.Scope+`">`) {
			t.Errorf("%s: <html> does not carry the room classes", path)
		}
		if !regexp.MustCompile(`<body [^>]*>\n<p class="room-style-strip" id="room-style-strip">`).MatchString(body) {
			t.Errorf("%s: the disclosure is not the first thing in <body>", path)
		}
		inside, outside, inFree := classesByCanvas(t, body)
		if len(inside) == 0 {
			t.Fatalf("%s: no canvas rendered", path)
		}
		for _, trust := range append(slices.Clone(trustClasses), trustIDs...) {
			if holder, ok := inFree[trust]; ok {
				t.Errorf("%s: %s is inside a .%s, where free-zone CSS can hide or cover it", path, trust, holder)
			}
		}
		for c := range inside {
			seen[c] = true
			if !slices.Contains(roomstyle.BodyClasses, c) && !slices.Contains(roomstyle.BodyTrustClasses, c) {
				t.Errorf("%s: class %q is inside a canvas, where body-zone CSS can hide it", path, c)
			}
		}
		for c := range outside {
			seen[c] = true
			if slices.Contains(roomstyle.BodyClasses, c) {
				t.Errorf("%s: body class %q is used outside a canvas", path, c)
			}
		}
		for _, trust := range []string{"memo-bottom", "author", "room-style-strip", "memo-actions", "report-button", "topbar"} {
			if !outside[trust] || inside[trust] {
				t.Errorf("%s: trust class %q not rendered outside the canvas", path, trust)
			}
		}
		// A listing previews Markdown as a title; a conversation renders it in full.
		if (path == "/r/lobby" && !inside["memo-title"]) || (path == "/e/m1" && (!inside["md-host"] || !inside["article-body"] || !inside["md-table"])) {
			t.Errorf("%s: the Markdown fixture did not render inside a canvas", path)
		}
	}
	for name, class := range roomstyle.Hooks {
		if !seen[class] {
			t.Errorf("hook .%s names .%s, which no room page renders", name, class)
		}
	}
	css, _ := files.ReadFile("assets/style.css")
	layer := string(css[strings.Index(string(css), "@layer room-trust{"):])
	layer = layer[:strings.Index(layer, "\n}\n")]
	for _, trust := range append(trustUI, ".room-canvas .md-host", "#room-style-strip", ":where(.room-canvas){", "prefers-reduced-motion:reduce") {
		if !strings.Contains(layer, trust) {
			t.Errorf("trust element %q is not pinned in @layer room-trust", trust)
		}
	}
	// Pins are literals or tokens a room cannot redefine.
	for _, token := range regexp.MustCompile(`var\((--[a-z0-9-]+)`).FindAllStringSubmatch(layer, -1) {
		if !slices.Contains(roomstyle.TrustKnobs(), token[1]) && !slices.Contains(roomstyle.SiteTokens(), token[1]) {
			t.Errorf("pin uses %s, which a room could define", token[1])
		}
	}
	root := string(css[strings.Index(string(css), ":root{"):])
	root = root[:strings.Index(root, "}")]
	declared := regexp.MustCompile(`(--[a-z0-9-]+):`).FindAllStringSubmatch(root, -1)
	if len(declared) != len(roomstyle.SiteTokens()) {
		t.Errorf("style.css declares %d tokens, roomstyle knows %d", len(declared), len(roomstyle.SiteTokens()))
	}
	for _, d := range declared {
		if !slices.Contains(roomstyle.SiteTokens(), d[1]) {
			t.Errorf("site token %s is unknown to roomstyle, so a room could redefine it", d[1])
		}
	}
}

func TestStyledRoomLinksSanitizedCSSUnderAPathRestrictedCSP(t *testing.T) {
	s := styled(map[string]string{"lobby": "@import url(//evil.example); :scope{background:#000} .author{display:none}"}, "public")
	sheet, _, _ := roomstyle.Sanitize("@import url(//evil.example); :scope{background:#000} .author{display:none}", "lobby")
	w := get(t, s, "/r/lobby", "swarmmemo.com")
	body := w.Body.String()
	href := "/room-style/lobby/" + sheet.Hash + ".css"
	for _, want := range []string{`<link rel="stylesheet" id="room-style" href="` + href + `">`, `data-room-style="` + sheet.Scope + `"`, `class="room-style-strip"`, "Custom style by", `href="/r/lobby?unstyled=1" id="room-style-toggle">View unstyled`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	csp := w.Header().Get("Content-Security-Policy")
	want := "default-src 'self'; script-src 'self'; style-src swarmmemo.com/assets/ swarmmemo.com/room-style/; img-src swarmmemo.com/a/ swarmmemo.com/assets/ data:; font-src swarmmemo.com/a/; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"
	if csp != want {
		t.Errorf("CSP\n got %q\nwant %q", csp, want)
	}

	css := get(t, s, href, "swarmmemo.com")
	if css.Code != 200 || css.Header().Get("Content-Type") != "text/css; charset=utf-8" || css.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(css.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("stylesheet response %d %v", css.Code, css.Header())
	}
	if css.Body.String() != sheet.CSS || strings.Contains(css.Body.String(), "evil") || strings.Contains(css.Body.String(), "author") {
		t.Fatalf("stylesheet body %q", css.Body.String())
	}
	for _, bad := range []string{"/room-style/lobby/0000000000000000.css", "/room-style/lobby/" + sheet.Hash, "/room-style/other/" + sheet.Hash + ".css", "/room-style/lobby/" + sheet.Hash + ".css?x=1"} {
		if got := get(t, s, bad, "swarmmemo.com"); got.Code != 404 {
			t.Errorf("%s: %d", bad, got.Code)
		}
	}
}

func TestReaderOptOutAndUnsafeHostsRenderUnstyled(t *testing.T) {
	s := styled(map[string]string{"lobby": ":scope{background:#000}"}, "public")
	w := get(t, s, "/r/lobby?unstyled=1", "swarmmemo.com")
	body := w.Body.String()
	if strings.Contains(body, `id="room-style"`) || strings.Contains(body, "room-canvas") || strings.Contains(body, "data-room-style") {
		t.Error("opt-out still links the stylesheet or renders canvases")
	}
	if !strings.Contains(body, `Room style hidden.`) || !strings.Contains(body, `href="/r/lobby" id="room-style-toggle">Show room style`) {
		t.Error("opt-out lost the disclosure or the way back")
	}
	if strings.Contains(w.Header().Get("Content-Security-Policy"), "room-style") {
		t.Error("unstyled page carries the room-style CSP")
	}
	for _, host := range []string{"[::1]:8080", "evil.example;script-src *", "Swarm Memo", ""} {
		if body := get(t, s, "/r/lobby", host).Body.String(); styledMarkup(body) {
			t.Errorf("host %q: styled without an expressible CSP", host)
		}
	}
}

func TestPrivateAndUnstyledRoomsNeverRenderRoomCSS(t *testing.T) {
	private := styled(map[string]string{"lobby": ":scope{background:#000}"}, "private")
	sheet, _, _ := roomstyle.Sanitize(":scope{background:#000}", "lobby")
	if got := get(t, private, "/room-style/lobby/"+sheet.Hash+".css", "swarmmemo.com"); got.Code != 404 {
		t.Errorf("private room stylesheet served: %d", got.Code)
	}
	if body := get(t, private, "/e/m1", "swarmmemo.com").Body.String(); styledMarkup(body) {
		t.Error("private room style rendered on a conversation page")
	}
	plain := styled(map[string]string{}, "public")
	w := get(t, plain, "/r/lobby", "swarmmemo.com")
	if styledMarkup(w.Body.String()) || strings.Contains(w.Body.String(), "room-canvas") || w.Header().Get("Content-Security-Policy") != "" {
		t.Error("a room without a style changed its page")
	}
	// A service that stores no styles (today's board) never renders any.
	if strings.Contains(get(t, &testService{}, "/r/lobby", "swarmmemo.com").Body.String(), "room-canvas") {
		t.Error("canvas without a style source")
	}
}

// countingStyles hands out a stylesheet whose url() check counts its calls.
type countingStyles struct {
	styledService
	checks int
}

func (s *countingStyles) RoomStyle(_ context.Context, room string) (*RoomStyle, error) {
	return &RoomStyle{Source: s.styles[room], Attachment: func(string) bool { s.checks++; return true }}, nil
}

// Sanitizing is the expensive part of a styled page (tens of milliseconds and a
// database read per url() for a hostile 32 KiB sheet), so anonymous page views
// and stylesheet fetches must not each pay it; a changed source still applies
// at once.
func TestStyledPageViewsReuseTheSanitizedSheet(t *testing.T) {
	sheet := strings.Repeat(".post{background:url(/a/0123456789abcdef0123456789abcdef)}", 200)
	s := &countingStyles{styledService: *styled(map[string]string{"cachedroom": sheet}, "public")}
	for range 5 {
		if w := get(t, s, "/r/cachedroom", "swarmmemo.com"); !strings.Contains(w.Body.String(), `id="room-style"`) {
			t.Fatal("room is not styled")
		}
	}
	if s.checks != 1 {
		t.Fatalf("five page views ran %d attachment checks; want one sanitize (1)", s.checks)
	}
	s.styles["cachedroom"] = ".post{color:#123456}"
	w := get(t, s, "/r/cachedroom", "swarmmemo.com")
	href := regexp.MustCompile(`href="(/room-style/[^"]+)"`).FindStringSubmatch(w.Body.String())
	if href == nil {
		t.Fatal("no stylesheet link after the source changed")
	}
	if css := get(t, s, href[1], "swarmmemo.com").Body.String(); !strings.Contains(css, "#123456") {
		t.Fatalf("a changed source was not applied at once: %q", css)
	}
}
