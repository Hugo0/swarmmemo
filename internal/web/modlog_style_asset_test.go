package web

import (
	"context"
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

// The log names each style asset by its filename, escaped, and previews an
// inline image at a fixed size, lazily; a font keeps a plain link (PR #52).
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
	for _, want := range []string{`href="/a/` + img.ID + `"`, `src="/a/` + img.ID + `"`, `>&lt;cat &amp; &#34;friends&#34;&gt;.gif</a>`, `width="96" height="64" loading="lazy"`, `href="/a/` + font.ID + `">term.woff2</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, `src="/a/`+font.ID+`"`) || strings.Contains(body, `<cat`) {
		t.Error("font previewed, or filename not escaped")
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
	if w.Code != 200 || !strings.Contains(body, `>missing-id-1</a>`) || !strings.Contains(body, `>other-room-i</a>`) || strings.Contains(body, "secret.gif") || strings.Contains(body, `class="modlog-asset"`) {
		t.Fatalf("unexpected fallback: status=%d body=%s", w.Code, body)
	}
	for _, c := range s.calls {
		if c.Operation == "blob.get" {
			t.Fatal("the log page downloaded an asset to name it")
		}
	}
}
