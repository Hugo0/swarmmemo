package web

import (
	"context"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

func TestModlogStyleAssetPreview(t *testing.T) {
	s, owner, _ := roomStore(t)
	owner.run(t, s, board.Command{Operation: "post", Room: "wires", Text: "opens room"})
	gif := []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;")
	img, err := s.OperatorAsset(context.Background(), "wires", `<cat & "friends">.gif`, gif)
	if err != nil {
		t.Fatal(err)
	}
	font, err := s.OperatorAsset(context.Background(), "wires", "term.woff2", append([]byte("wOF2"), make([]byte, 60)...))
	if err != nil {
		t.Fatal(err)
	}
	w := render(s, "/modlog/wires")
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	for _, want := range []string{`href="/a/` + img.ID + `"`, `src="/a/` + img.ID + `"`, `&lt;cat &amp; &#34;friends&#34;&gt;.gif`, `href="/a/` + font.ID + `"`, `term.woff2`, `loading="lazy"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, `src="/a/`+font.ID+`"`) || strings.Contains(body, `href="/agent/`+img.ID+`"`) {
		t.Error("font previewed or image linked as agent")
	}
}

func TestModlogStyleAssetMissingAndPrivacy(t *testing.T) {
	calls := 0
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			return board.Result{Room: &board.Room{Name: "wires", Visibility: "public"}}, nil
		case "room.modlog":
			return board.Result{Data: map[string]any{"entries": []board.ModerationEntry{{Action: "style.asset", Actor: "operator", Target: "missing-id-123456"}, {Action: "style.asset", Actor: "operator", Target: "other-room-id-123"}}}}, nil
		case "blob.get":
			calls++
			if c.MessageID == "other-room-id-123" {
				return board.Result{Data: map[string]any{"blob": board.Attachment{Room: "private", Filename: "secret.gif", MediaType: "image/gif"}}}, nil
			}
			return board.Result{}, nil
		}
		return board.Result{}, nil
	}}
	w := render(s, "/modlog/wires")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `>missing-id-1</a>`) || !strings.Contains(body, `>other-room-i</a>`) || strings.Contains(body, "secret.gif") || strings.Contains(body, `class="modlog-asset-preview"`) || calls != 2 {
		t.Fatalf("unexpected fallback: status=%d calls=%d body=%s", w.Code, calls, body)
	}
}
