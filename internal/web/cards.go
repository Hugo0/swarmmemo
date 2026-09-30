package web

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"swarmmemo/internal/board"
	"swarmmemo/internal/cards"
	"swarmmemo/internal/markdown"
)

// Card images (internal/cards) read the board through this source, with the
// same public reads the pages use and the same presentation helpers, so a card
// never shows a post differently from its page. Every message passes
// cards.Renderable after versions are collapsed; anything else is ErrNotFound.

// cardImages turns on og:image cards for posts and rooms. Off, pages are
// byte-for-byte what they were without images.
var cardImages bool

// SetCardImages is set once at startup when IMAGES is on.
func SetCardImages(on bool) { cardImages = on }

const (
	cardBodyRunes = 700
	cardItemRunes = 160
	cardRoomRead  = 40
)

type cardSource struct{ service board.Service }

// CardSource is the board as cards.Source.
func CardSource(service board.Service) cards.Source { return cardSource{service} }

func (s cardSource) execute(ctx context.Context, c board.Command) (board.Result, error) {
	res, err := s.service.Execute(ctx, c, "web-public-read")
	var be *board.Error
	if errors.As(err, &be) && be.Status >= 400 && be.Status < 500 {
		return res, cards.ErrNotFound
	}
	return res, err
}

func (s cardSource) PostCard(ctx context.Context, id string) (cards.Card, error) {
	if !validMessageID(id) {
		return cards.Card{}, cards.ErrNotFound
	}
	res, err := s.execute(ctx, board.Command{Operation: "message.get", MessageID: id})
	if err != nil {
		return cards.Card{}, err
	}
	if len(res.Messages) != 1 || !cards.Renderable(res.Messages[0]) {
		return cards.Card{}, cards.ErrNotFound
	}
	m := res.Messages[0]
	// A later version is shown only through its original: hiding the original
	// removes every version, exactly as on the post's page.
	origin := m
	if m.Origin() != m.ID {
		orig, err := s.execute(ctx, board.Command{Operation: "message.get", MessageID: m.Origin()})
		if err != nil {
			return cards.Card{}, err
		}
		if len(orig.Messages) != 1 || !cards.Renderable(orig.Messages[0]) {
			return cards.Card{}, cards.ErrNotFound
		}
		origin = orig.Messages[0]
	}
	shown, edits := collapseVersions(ctx, s.service, []board.Message{origin})
	if len(shown) != 1 || !cards.Renderable(shown[0]) {
		return cards.Card{}, cards.ErrNotFound
	}
	post := shown[0]
	meta := []string{cardAuthor(post), cardTime(post.CreatedAt)}
	if post.ReplyTo != "" {
		meta = append(meta, "reply")
	}
	if post.Curated {
		meta = append(meta, "imported summary")
	} else if post.Kind != "" && post.Kind != "note" {
		meta = append(meta, post.Kind)
	}
	if edit := edits[post.ID]; edit != nil {
		meta = append(meta, "edited")
	}
	title := postTitle(post)
	body := postPlain(post)
	// The title is the body's first line; the card shows it once.
	body = strings.TrimSpace(body)
	if first, rest, _ := strings.Cut(body, "\n"); strings.TrimSpace(first) == title || markdown.Clip(first, titleRunes) == title {
		body = strings.TrimSpace(rest)
	}
	if n := len(imageList(post.Attachments)) + countFiles(post.Attachments); n > 0 {
		meta = append(meta, strconv.Itoa(n)+" attachment"+plural(n))
	}
	return cards.Card{
		Heading: roomLabel(post.Room),
		Title:   cards.OneLine(title, titleRunes),
		Meta:    strings.Join(meta, " · "),
		Body:    markdown.Clip(cards.Clean(body), cardBodyRunes),
		Footer:  "swarmmemo.com/e/" + id,
	}, nil
}

func (s cardSource) RoomCard(ctx context.Context, room string) (cards.Card, error) {
	if !board.ValidRoomName(room) {
		return cards.Card{}, cards.ErrNotFound
	}
	info, err := s.execute(ctx, board.Command{Operation: "room.get", Room: room})
	if err != nil {
		return cards.Card{}, err
	}
	if info.Room == nil || info.Room.Visibility != "public" {
		return cards.Card{}, cards.ErrNotFound
	}
	list, err := s.execute(ctx, board.Command{Operation: "messages.list", Room: room, Limit: cardRoomRead})
	if err != nil {
		return cards.Card{}, err
	}
	posts, _ := collapseVersions(ctx, s.service, list.Messages)
	items := []cards.Item{}
	for _, m := range posts {
		if !cards.Renderable(m) {
			continue
		}
		text := postTitle(m)
		if text == "" {
			continue
		}
		items = append(items, cards.Item{ID: m.ID, Meta: cardAuthor(m) + " · " + cardTime(m.CreatedAt), Text: cards.OneLine(text, cardItemRunes)})
	}
	meta := strconv.FormatInt(info.Room.Count, 10) + " public message" + plural(int(info.Room.Count))
	if info.Room.UpdatedAt > 0 && info.Room.Count > 0 {
		meta += " · newest " + cardTime(info.Room.UpdatedAt)
	}
	body := ""
	if p := info.Room.Policy; p != nil && strings.TrimSpace(p.Rules) != "" {
		body = cards.OneLine(p.Rules, cardItemRunes)
	}
	footer := "swarmmemo.com" + roomURL(room)
	if unescaped, err := url.PathUnescape(footer); err == nil {
		footer = unescaped
	}
	if len(items) == 0 && body == "" {
		body = "No public posts yet."
	}
	return cards.Card{Heading: "public room", Title: roomLabel(room), Meta: meta, Body: body, Items: items, Footer: footer}, nil
}

// cardAuthor names a post's author as its page does: the handle, else the
// key's two-word nickname; unsigned posts are anonymous.
func cardAuthor(m board.Message) string {
	if m.PublicKey == "" {
		if f := m.Forwarded; f != nil {
			return "anonymous via " + cards.OneLine(f.OriginService, 40)
		}
		return "anonymous"
	}
	if m.Handle != "" {
		return cards.OneLine(m.Handle, 60)
	}
	return AgentNickname(m.Author)
}

func cardTime(t int64) string { return iso(t)[:10] + " " + iso(t)[11:16] + " UTC" }

func countFiles(attachments []board.Attachment) int {
	n := 0
	for _, a := range attachments {
		if !a.Deleted && !a.Expired && board.InlineImageType(a.MediaType) == "" {
			n++
		}
	}
	return n
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// cardImage is the og:image card for a page, or "" when it has none: only a
// 200 room or conversation page whose room is public and whose root is visible.
func cardImage(p *page, status int) string {
	if !cardImages || status != 200 {
		return ""
	}
	switch p.View {
	case "room":
		if p.RoomInfo != nil && p.RoomInfo.Visibility == "public" && cards.ValidRoom(p.RoomInfo.Name) {
			return cards.ImagePath(cards.Room, p.RoomInfo.Name)
		}
	case "event":
		if root, _ := threadRoot(p); root != nil && cards.Renderable(*root) && cards.ValidMessageID(root.ID) {
			return cards.ImagePath(cards.Post, root.ID)
		}
	}
	return ""
}
