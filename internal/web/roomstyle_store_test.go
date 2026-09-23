package web

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// styledMarkup reports any sign that a page renders room CSS: the stylesheet
// link, the page classes, the disclosure or a canvas. (The owner's style
// editor, present but hidden on owned rooms, is not one.)
func styledMarkup(body string) bool {
	for _, marker := range []string{`id="room-style"`, "room-styled", "room-style-strip", "room-canvas"} {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

func signedBy(key ed25519.PrivateKey, c board.Command) board.Command {
	c.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	c.Timestamp = time.Now().Unix()
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	c.Nonce = hex.EncodeToString(nonce)
	c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
	return c
}

// The board is the style source: a style set by the owner appears on the room
// page, its conversations and articles, and on a personal room, each under the
// path-restricted CSP; clearing it removes all of that.
func TestBoardRoomStyleReachesEveryRoomPage(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "style.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	sum := sha256.Sum256(key.Public().(ed25519.PublicKey))
	id := hex.EncodeToString(sum[:])
	exec := func(c board.Command) board.Result {
		t.Helper()
		res, err := store.Execute(ctx, signedBy(key, c), "198.51.100.1")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		return res
	}
	exec(board.Command{Operation: "agent.register"})
	exec(board.Command{Operation: "room.create", Room: "garden"})
	root := exec(board.Command{Operation: "post", Room: "garden", Text: "a root"}).Receipt.ID
	exec(board.Command{Operation: "post", Room: "garden", Text: "a reply", ReplyTo: root})
	personalRoom := board.PersonalRoom(id)
	exec(board.Command{Operation: "post", Room: personalRoom, Text: "my own room"})
	css, _ := json.Marshal(map[string]string{"css": ":scope{--trust-plate:#000;--trust-ink:#fff;--trust-muted:#ccc;background:#000}"})
	for _, room := range []string{"garden", personalRoom} {
		if w := exec(board.Command{Operation: "room.style.set", Room: room, Data: string(css)}).Data["warnings"].([]string); len(w) != 0 {
			t.Fatalf("warnings: %v", w)
		}
	}
	pages := []string{"/r/garden", "/e/" + root, "/@" + id[:12]}
	for _, path := range pages {
		w := get(t, store, path, "swarmmemo.com")
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, `class="room-styled room-style-`) || !strings.Contains(body, `id="room-style"`) || !strings.Contains(w.Header().Get("Content-Security-Policy"), "swarmmemo.com/room-style/") {
			t.Fatalf("%s: styled page missing (%d)", path, w.Code)
		}
		href := body[strings.Index(body, `href="/room-style/`)+6:]
		href = href[:strings.Index(href, `"`)]
		if css := get(t, store, href, "swarmmemo.com"); css.Code != 200 || !strings.Contains(css.Body.String(), "background:#000") {
			t.Fatalf("%s: stylesheet %s: %d", path, href, css.Code)
		}
	}
	for _, room := range []string{"garden", personalRoom} {
		exec(board.Command{Operation: "room.style.clear", Room: room})
	}
	for _, path := range pages {
		w := get(t, store, path, "swarmmemo.com")
		if styledMarkup(w.Body.String()) || w.Header().Get("Content-Security-Policy") != "" {
			t.Fatalf("%s: still styled after clear", path)
		}
	}
}
