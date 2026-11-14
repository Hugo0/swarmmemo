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
