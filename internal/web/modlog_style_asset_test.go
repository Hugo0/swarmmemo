package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"swarmmemo/internal/board"
	"testing"
)

func TestModlogStyleAssetLinksAttachment(t *testing.T) {
	s, _, _ := roomStore(t)
	if _, err := s.Execute(context.Background(), board.Command{Operation: "post", Room: "wires", Text: "open room"}, "test"); err != nil {
		t.Fatal(err)
	}
	gif := []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;")
	a, err := s.OperatorAsset(context.Background(), "wires", "cat.gif", gif)
	if err != nil {
		t.Fatal(err)
	}
	w := render(s, "/modlog/wires")
	body := w.Body.String()
	correct := `href="/a/` + a.ID + `"`
	wrong := `href="/agent/` + a.ID + `"`
	if w.Code != 200 || !strings.Contains(body, "Added a style asset") || !strings.Contains(body, correct) || strings.Contains(body, wrong) {
		t.Fatalf("expected asset link and no incorrect agent link; status %d", w.Code)
	}
	if got := render(s, "/agent/"+a.ID).Code; got != 404 {
		t.Fatalf("expected invalid agent 404, got %d", got)
	}
	t.Logf("asset %s rendered with the correct attachment route", a.ID)
}

// The log names each style asset by its filename, escaped, with its media type
// and size, and previews an inline image at a fixed size, lazily (PR #52); a
// font gets a text sample that modlog-font.js sets in it on view (PR #54).
func TestModlogStyleAssetNamesAndPreviews(t *testing.T) {
	s, owner, _ := roomStore(t)
	owner.run(t, s, board.Command{Operation: "post", Room: "wires", Text: "opens room"})
	owner.run(t, s, board.Command{Operation: "post", Room: "cables", Text: "opens room"})
	gif := []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;")
	ctx := context.Background()
	img, err := s.OperatorAsset(ctx, "wires", `<cat & "friends">.gif`, gif)
	if err != nil {
		t.Fatal(err)
	}
	font, err := s.OperatorAsset(ctx, "wires", "term.woff2", append([]byte("wOF2"), make([]byte, 60)...))
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.OperatorAsset(ctx, "cables", "other.gif", gif)
	if err != nil {
		t.Fatal(err)
	}
	w := render(s, "/modlog/wires")
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	for _, want := range []string{`href="/a/` + img.ID + `"`, `src="/a/` + img.ID + `"`, `>&lt;cat &amp; &#34;friends&#34;&gt;.gif</a>`, `width="96" height="64" loading="lazy"`, `href="/a/` + font.ID + `">term.woff2</a> · font/woff2 · 64 bytes`, `.gif</a> · image/gif · ` + strconv.Itoa(len(gif)) + ` bytes`, `<p class="modlog-font" data-font-url="/a/` + font.ID + `">`, `/assets/modlog-font.js?v=`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, `src="/a/`+font.ID+`"`) || strings.Contains(body, `data-font-url="/a/`+img.ID+`"`) || strings.Contains(body, `<cat`) {
		t.Error("font previewed as an image, image sampled as a font, or filename not escaped")
	}
	// Names come from the room's own blobs only, and a missing id has none.
	names, err := s.StyleAssetNames(ctx, "wires", []string{img.ID, img.ID, other.ID, "missing-id-123456"})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[img.ID].Filename != `<cat & "friends">.gif` || names[img.ID].MediaType != "image/gif" {
		t.Fatalf("names: %+v", names)
	}
}

// namerService is a service that names style assets as the store would.
type namerService struct {
	testService
	names map[string]board.Attachment
}

func (s *namerService) StyleAssetNames(_ context.Context, room string, ids []string) (map[string]board.Attachment, error) {
	out := map[string]board.Attachment{}
	for _, id := range ids {
		if b, ok := s.names[id]; ok && b.Room == room {
			out[id] = b
		}
	}
	return out, nil
}

// An asset that is not served (missing, or another room's) keeps its short id
// and no preview, and the page never downloads a file to name it.
func TestModlogStyleAssetUnavailable(t *testing.T) {
	s := &namerService{names: map[string]board.Attachment{"other-room-id-123": {Room: "private", Filename: "secret.gif", MediaType: "image/gif"}}}
	s.execute = func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			return board.Result{Room: &board.Room{Name: "wires", Visibility: "public"}}, nil
		case "room.modlog":
			return board.Result{Data: map[string]any{"entries": []board.ModerationEntry{{Action: "style.asset", Actor: "operator", Target: "missing-id-123456"}, {Action: "style.asset", Actor: "operator", Target: "other-room-id-123"}}}}, nil
		}
		return board.Result{}, nil
	}
	w := render(s, "/modlog/wires")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `>missing-id-1</a>`) || !strings.Contains(body, `>other-room-i</a>`) || strings.Contains(body, "secret.gif") || strings.Contains(body, "image/gif") || strings.Contains(body, `class="modlog-asset"`) || strings.Contains(body, `class="modlog-font"`) {
		t.Fatalf("unexpected fallback: status=%d body=%s", w.Code, body)
	}
	for _, c := range s.calls {
		if c.Operation == "blob.get" {
			t.Fatal("the log page downloaded an asset to name it")
		}
	}
}

// Against the real store: an asset whose every post is hidden loses its name,
// metadata, preview and sample until a post is restored, and keeps a short-id
// link meanwhile (PR #54).
func TestModlogStyleAssetFollowsVisibility(t *testing.T) {
	s, owner, _ := roomStore(t)
	owner.run(t, s, board.Command{Operation: "post", Room: "wires", Text: "open room"})
	ctx := context.Background()
	font, err := s.OperatorAsset(ctx, "wires", "secret-face.woff2", append([]byte("wOF2"), make([]byte, 60)...))
	if err != nil {
		t.Fatal(err)
	}
	post := func(text string) string {
		return owner.run(t, s, board.Command{Operation: "post", Room: "wires", Text: text, Attachments: []string{font.ID}}).Receipt.ID
	}
	first, second := post("first use"), post("second use")
	check := func(shown bool) {
		t.Helper()
		w := render(s, "/modlog/wires")
		body := w.Body.String()
		if w.Code != 200 {
			t.Fatalf("status %d", w.Code)
		}
		for _, marker := range []string{"secret-face.woff2", "font/woff2 · 64 bytes", `data-font-url="/a/` + font.ID + `"`} {
			if strings.Contains(body, marker) != shown {
				t.Errorf("shown=%v but %q present=%v", shown, marker, !shown)
			}
		}
		if !shown && !strings.Contains(body, `href="/a/`+font.ID+`">`+font.ID[:12]+`</a>`) {
			t.Error("missing short-id link")
		}
	}
	check(true)
	if err := s.Moderate(ctx, first, "hide one", true); err != nil {
		t.Fatal(err)
	}
	check(true) // still served through the second post
	if err := s.Moderate(ctx, second, "hide both", true); err != nil {
		t.Fatal(err)
	}
	check(false)
	if err := s.Moderate(ctx, first, "restore one", false); err != nil {
		t.Fatal(err)
	}
	check(true)
}

// A private room's log is not public, so nothing in it is named.
func TestModlogPrivateRoomNamesNothing(t *testing.T) {
	s, owner, _ := roomStore(t)
	owner.run(t, s, board.Command{Operation: "room.create", Room: "vault", Visibility: "private"})
	owner.run(t, s, board.Command{Operation: "blob.put", Room: "vault", Filename: "private-marker.txt", MediaType: "text/plain", Data: "c2VjcmV0"})
	if w := render(s, "/modlog/vault"); w.Code != 404 || strings.Contains(w.Body.String(), "private-marker.txt") {
		t.Fatalf("private log served: status %d", w.Code)
	}
}

// The log shows the SHA-256 its operator entry recorded, bound to the target
// and only as 64 lowercase hex; anything else shows no hash.
func TestLoggedStyleAssetHash(t *testing.T) {
	hash := strings.Repeat("a", 64)
	detail := `{"id":"asset","sha256":"` + hash + `"}`
	tests := []struct {
		name  string
		entry board.ModerationEntry
		want  string
	}{
		{"valid", board.ModerationEntry{Actor: "operator", Target: "asset", Detail: detail}, hash},
		{"wrong target", board.ModerationEntry{Actor: "operator", Target: "other", Detail: detail}, ""},
		{"no id", board.ModerationEntry{Actor: "operator", Target: "", Detail: `{"sha256":"` + hash + `"}`}, ""},
		{"wrong actor", board.ModerationEntry{Actor: "key", Target: "asset", Detail: detail}, ""},
		{"malformed", board.ModerationEntry{Actor: "operator", Target: "asset", Detail: `{`}, ""},
		{"uppercase", board.ModerationEntry{Actor: "operator", Target: "asset", Detail: `{"id":"asset","sha256":"` + strings.Repeat("A", 64) + `"}`}, ""},
		{"injection", board.ModerationEntry{Actor: "operator", Target: "asset", Detail: `{"id":"asset","sha256":"` + strings.Repeat("a", 56) + `<script>"}`}, ""},
		{"oversize", board.ModerationEntry{Actor: "operator", Target: "asset", Detail: strings.Repeat(" ", 4097)}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := loggedStyleAssetHash(tt.entry); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestModlogStyleAssetLoggedHash(t *testing.T) {
	s, _, _ := roomStore(t)
	if _, err := s.Execute(context.Background(), board.Command{Operation: "post", Room: "wires", Text: "open room"}, "test"); err != nil {
		t.Fatal(err)
	}
	gif := []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;")
	a, err := s.OperatorAsset(context.Background(), "wires", "cat.gif", gif)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(gif)
	hash := hex.EncodeToString(sum[:])
	w := render(s, "/modlog/wires")
	body := w.Body.String()
	if w.Code != 200 || a.Hash != hash || !strings.Contains(body, `Logged <code title="SHA-256: `+hash+`" data-copy="`+hash+`" data-copy-label="Copy SHA-256">sha256: `+hash[:12]) {
		t.Fatalf("missing logged hash: status=%d body=%s", w.Code, body)
	}
}

func TestLoggedStyleSourceHash(t *testing.T) {
	hash := strings.Repeat("a", 64)
	for _, tt := range []struct{ name, detail, want string }{
		{"valid", `{"sha256":"` + hash + `","bytes":22}`, hash},
		{"asset detail", `{"id":"asset","sha256":"` + hash + `"}`, ""},
		{"uppercase", `{"sha256":"` + strings.Repeat("A", 64) + `"}`, ""},
		{"short", `{"sha256":"` + strings.Repeat("a", 63) + `"}`, ""},
		{"injection", `{"sha256":"` + strings.Repeat("a", 56) + `<script>"}`, ""},
		{"malformed", `{`, ""},
		{"empty", `{}`, ""},
		{"oversize", strings.Repeat(" ", 4097), ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := loggedStyleSourceHash(board.ModerationEntry{Action: "style", Detail: tt.detail}); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

// TestModlogStyleSourceHash: a style entry shows the logged source CSS hash,
// which differs from the served (sanitized, scoped) stylesheet's, and keeps it
// after the style is cleared.
func TestModlogStyleSourceHash(t *testing.T) {
	s, _, _ := roomStore(t)
	ctx := context.Background()
	if _, err := s.Execute(ctx, board.Command{Operation: "post", Room: "wires", Text: "open room"}, "test"); err != nil {
		t.Fatal(err)
	}
	source := ":scope { color: red; }"
	data, _ := json.Marshal(map[string]string{"css": source})
	res, err := s.OperatorRoom(ctx, board.Command{Operation: "room.style.set", Room: "wires", Data: string(data)})
	if err != nil {
		t.Fatal(err)
	}
	sheet, _ := res.Data["stylesheet"].(string)
	served := get(t, s, sheet, "swarmmemo.com")
	sum := sha256.Sum256([]byte(source))
	hash := hex.EncodeToString(sum[:])
	if served.Code != 200 || sha256.Sum256(served.Body.Bytes()) == sum {
		t.Fatalf("served %q: status %d, or its digest equals the source's", sheet, served.Code)
	}
	want := `Logged source CSS <code title="SHA-256: ` + hash + `" data-copy="` + hash + `" data-copy-label="Copy SHA-256">sha256: ` + hash[:12]
	if w := render(s, "/modlog/wires"); w.Code != 200 || !strings.Contains(w.Body.String(), want) {
		t.Fatalf("missing logged source hash: status=%d", w.Code)
	}
	if _, err := s.OperatorRoom(ctx, board.Command{Operation: "room.style.clear", Room: "wires"}); err != nil {
		t.Fatal(err)
	}
	if after := render(s, "/modlog/wires").Body.String(); strings.Count(after, "Logged source CSS") != 1 || !strings.Contains(after, want) {
		t.Fatal("cleared style lost or duplicated its logged source hash")
	}
}
