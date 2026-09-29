package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/cards"
)

// Post and room images (internal/cards), when IMAGES is on:
//
//	/e/MESSAGE_ID.png   a public post as a 1200x630 PNG card
//	/r/ROOM.png         a public room's newest posts as a card
//	/render/e/ID, /render/r/ROOM   the card page a renderer is given
//
// The routes take a message ID or room name in the path and nothing else: no
// query string at all, so there is no parameter through which a URL, a size
// or a renderer could be chosen. With IMAGES off none of this is reached.

const imageTimeout = 30 * time.Second

// imageRoute answers the image and card-page routes and reports whether the
// path was one of them.
func (s *Server) imageRoute(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	render := strings.HasPrefix(p, "/render/")
	if !render && !((strings.HasPrefix(p, "/e/") || strings.HasPrefix(p, "/r/")) && strings.HasSuffix(p, ".png")) {
		return false
	}
	if !readMethod(r) {
		methodError(w)
		return true
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeError(w, &board.Error{Status: 400, Code: "no_query", Message: "Image routes take no query string: the path names the post (/e/MESSAGE_ID.png) or room (/r/ROOM.png)."})
		return true
	}
	var kind, key string
	var ok bool
	if render {
		kind, key, ok = cards.ParseRenderPath(p)
	} else {
		kind, key, ok = cards.ParseImagePath(p)
	}
	if !ok {
		writeError(w, &board.Error{Status: 404, Code: "not_found", Message: "No image here. Images exist at /e/MESSAGE_ID.png and /r/ROOM.png."})
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), imageTimeout)
	defer cancel()
	if render {
		s.cardPage(w, r.WithContext(ctx), kind, key)
	} else {
		s.cardImage(w, r.WithContext(ctx), kind, key)
	}
	return true
}

func imageError(w http.ResponseWriter, err error) {
	if errors.Is(err, cards.ErrNotFound) {
		writeError(w, &board.Error{Status: 404, Code: "not_found", Message: "Not a public, visible post or room."})
		return
	}
	writeError(w, &board.Error{Status: 503, Code: "image_unavailable", Message: "The image could not be made right now. Retry shortly.", RetryAfter: 5})
}

func (s *Server) cardImage(w http.ResponseWriter, r *http.Request, kind, key string) {
	img, err := s.cfg.Images.Image(r.Context(), kind, key)
	if err != nil {
		imageError(w, err)
		return
	}
	etag := `"` + img.ETag + `"`
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Content-Disposition", `inline; filename="`+kind+"-"+key+`.png"`)
	h.Set("ETag", etag)
	// Short: a post hidden now must stop being served from shared caches soon.
	h.Set("Cache-Control", "public, max-age=300")
	if img.Placeholder {
		h.Set("Cache-Control", "public, max-age=60")
	}
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(img.PNG)))
	w.WriteHeader(200)
	if r.Method != http.MethodHead {
		_, _ = w.Write(img.PNG)
	}
}

func (s *Server) cardPage(w http.ResponseWriter, r *http.Request, kind, key string) {
	page, err := s.cfg.Images.Page(r.Context(), kind, key)
	if err != nil {
		imageError(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", cards.PageCSP+"; frame-ancestors 'none'")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Content-Length", strconv.Itoa(len(page)))
	w.WriteHeader(200)
	if r.Method != http.MethodHead {
		_, _ = w.Write(page)
	}
}

// addImageURLs gives every public, visible message and public room in a
// result its image address, so a JSON reader finds the image the page links.
func (s *Server) addImageURLs(res *board.Result) {
	if s.cfg.Images == nil {
		return
	}
	base := strings.TrimRight(s.cfg.PublicURL, "/")
	for i := range res.Messages {
		if m := &res.Messages[i]; cards.Renderable(*m) && cards.ValidMessageID(m.ID) {
			m.ImageURL = base + cards.ImagePath(cards.Post, m.ID)
		}
	}
	room := func(r *board.Room) {
		if r != nil && r.Visibility == "public" && cards.ValidRoom(r.Name) {
			r.ImageURL = base + cards.ImagePath(cards.Room, r.Name)
		}
	}
	room(res.Room)
	for i := range res.Rooms {
		room(&res.Rooms[i])
	}
}

// forgetImage drops a post's stored image after a moderation change. Serving
// already refuses hidden posts; this removes the bytes as well.
func (s *Server) forgetImage(id string) {
	if s.cfg.Images != nil && cards.ValidMessageID(id) {
		s.cfg.Images.Forget(cards.Post, id)
	}
}

func addImageOpenAPI(paths map[string]any) {
	png := map[string]any{
		"200": map[string]any{"description": "A 1200x630 PNG card", "content": map[string]any{"image/png": map[string]any{"schema": map[string]string{"type": "string", "format": "binary"}}}},
		"400": map[string]any{"description": "A query string was given; image routes take none"},
		"404": map[string]any{"description": "Not a public, visible post or room"},
		"503": map[string]any{"description": "The image could not be made right now"},
	}
	paths["/e/{message_id}.png"] = map[string]any{"get": map[string]any{
		"summary":    "A public post as an image card; the same image is the post page's og:image",
		"parameters": []map[string]any{{"name": "message_id", "in": "path", "required": true, "schema": map[string]any{"type": "string", "pattern": "^[a-f0-9]{32}$"}}},
		"responses":  png,
	}}
	paths["/r/{room}.png"] = map[string]any{"get": map[string]any{
		"summary":    "A public room's newest posts as an image card",
		"parameters": []map[string]any{{"name": "room", "in": "path", "required": true, "schema": map[string]string{"type": "string"}}},
		"responses":  png,
	}}
}
