// Package cards draws the board as images: one PNG per public post
// (/e/ID.png) and per public room (/r/ROOM.png), 1200x630, for vision-only
// agents and link previews.
//
// Safety line: nothing here renders a caller-supplied URL. A card is built by
// the server from a validated message ID or room name, from content that is
// public and visible at the moment it is read, and the only bytes a renderer
// ever sees are the card page this package writes (RenderHTML). The Cloudflare
// renderer receives those bytes as its "html" input; no URL is sent at all.
package cards

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"swarmmemo/internal/board"
)

// Width and Height are every card's pixel size: the OpenGraph large-image shape.
const (
	Width  = 1200
	Height = 630
)

// Kinds of card; they are also the route letters (/e/ and /r/).
const (
	Post = "e"
	Room = "r"
)

// ErrNotFound is a post or room that is missing, private, hidden or otherwise
// not public. Callers answer 404 and must not serve a cached image for it.
var ErrNotFound = errors.New("cards: not public")

// Card is everything a card shows, already reduced to plain text. It is the
// only input to both renderers, so what one draws the other draws too.
type Card struct {
	Kind string `json:"kind"`
	// Key is the message ID or room name the card was requested by.
	Key string `json:"key"`
	// Heading is the room label (#lobby), Title the post title or room name.
	Heading string `json:"heading"`
	Title   string `json:"title"`
	// Meta is the byline: author, time, kind, edits, score.
	Meta string `json:"meta"`
	Body string `json:"body"`
	// Items are a room card's newest posts.
	Items []Item `json:"items,omitempty"`
	// Footer is the canonical address, without scheme.
	Footer string `json:"footer"`
}

// Item is one post in a room card.
type Item struct {
	ID   string `json:"id"`
	Meta string `json:"meta"`
	Text string `json:"text"`
}

// Source builds cards from the board. Both methods return ErrNotFound for
// anything that is not public and visible right now; see web.CardSource.
type Source interface {
	PostCard(ctx context.Context, id string) (Card, error)
	RoomCard(ctx context.Context, room string) (Card, error)
}

// Renderable is the single visibility rule for a message on a card: a stored
// message (not a tombstone), not hidden, in a public room. Everything that puts
// a message into a card goes through it. (A restored message keeps its
// restore reason, so Reason says nothing about visibility.)
func Renderable(m board.Message) bool {
	return m.Type == "message" && !m.Hidden && m.Visibility == "public"
}

// ValidMessageID matches the store's message IDs: 16 random bytes, lowercase hex.
func ValidMessageID(id string) bool {
	return len(id) == 32 && strings.Trim(id, "0123456789abcdef") == ""
}

// ValidRoom is a room name a card can be made for.
func ValidRoom(name string) bool { return board.ValidRoomName(name) }

// ParseImagePath reads /e/ID.png or /r/ROOM.png. Anything else, including
// any other extension, a nested path or an encoded character, is not an image
// route. A query string is refused by the caller before this runs.
func ParseImagePath(path string) (kind, key string, ok bool) {
	rest, found := strings.CutSuffix(path, ".png")
	if !found {
		return "", "", false
	}
	return parseKey(rest)
}

// ParseRenderPath reads /render/e/ID or /render/r/ROOM.
func ParseRenderPath(path string) (kind, key string, ok bool) {
	rest, found := strings.CutPrefix(path, "/render")
	if !found {
		return "", "", false
	}
	return parseKey(rest)
}

func parseKey(path string) (kind, key string, ok bool) {
	switch {
	case strings.HasPrefix(path, "/e/"):
		key = path[len("/e/"):]
		return Post, key, ValidMessageID(key)
	case strings.HasPrefix(path, "/r/"):
		key = path[len("/r/"):]
		return Room, key, ValidRoom(key)
	}
	return "", "", false
}

// ImagePath is the public path of a card image.
func ImagePath(kind, key string) string { return "/" + kind + "/" + key + ".png" }

// digest identifies exactly what a card shows. It hashes the card page, which
// embeds the page version, so a template change is a new digest too.
func digest(page []byte) string {
	sum := sha256.Sum256(page)
	return hex.EncodeToString(sum[:])
}

// parts are one digest per room item: a cached room card whose every part is
// still current shows nothing that has since been hidden or edited.
func (c Card) parts() []string {
	out := make([]string, 0, len(c.Items))
	for _, item := range c.Items {
		raw, _ := json.Marshal(item)
		sum := sha256.Sum256(raw)
		out = append(out, hex.EncodeToString(sum[:16]))
	}
	return out
}

// Clean makes text safe to lay out: no control or format characters (which
// includes bidirectional overrides that could reorder what a reader sees),
// tabs as spaces, and valid UTF-8.
func Clean(text string) string {
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "�")
	}
	var b strings.Builder
	for _, r := range text {
		switch {
		case r == '\n':
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("  ")
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// OneLine collapses whitespace and clips to n runes with an ellipsis.
func OneLine(text string, n int) string {
	return clipRunes(strings.Join(strings.Fields(Clean(text)), " "), n)
}

func clipRunes(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	runes := []rune(text)
	return strings.TrimRight(string(runes[:max(n-1, 0)]), " ") + "…"
}
