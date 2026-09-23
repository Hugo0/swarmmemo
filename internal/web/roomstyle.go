package web

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/roomstyle"
)

// RoomStyle is a room's stored stylesheet: the owner's raw CSS, sanitized on
// every serve, so a sanitizer fix applies to every room at once and nothing
// stored is ever trusted as already safe.
type RoomStyle = board.RoomStyle

// RoomStyleSource is implemented by a service that stores room stylesheets:
// the board, through room.style.set.
type RoomStyleSource interface {
	RoomStyle(ctx context.Context, room string) (*RoomStyle, error)
}

// roomStyleView drives the disclosure strip, the stylesheet link and the canvases.
type roomStyleView struct {
	Class, Href, Owner, Toggle string
	// Active is false when the reader asked for ?unstyled=1: the strip stays,
	// the stylesheet and canvases do not.
	Active bool
}

// loadRoomStyle returns the sanitized stylesheet of a public room, or nil.
func loadRoomStyle(ctx context.Context, service board.Service, room string) (*RoomStyle, *roomstyle.Stylesheet) {
	source, ok := service.(RoomStyleSource)
	if !ok || room == "" {
		return nil, nil
	}
	info, err := service.Execute(ctx, board.Command{Operation: "room.get", Room: room}, "web-public-read")
	if err != nil || info.Room == nil || info.Room.Visibility == "private" {
		return nil, nil
	}
	style, err := source.RoomStyle(ctx, room)
	if err != nil || style == nil || strings.TrimSpace(style.Source) == "" {
		return nil, nil
	}
	sheet := sanitizedStyle(room, style)
	if sheet == nil {
		return nil, nil
	}
	return style, sheet
}

// Sanitizing a 32 KiB stylesheet costs tens of milliseconds and a database
// read per url(), and every view of a styled room and every stylesheet fetch
// needs the result, so an anonymous reader could make each cheap GET expensive.
// Results (including refusals) are kept per room for as long as the stored
// source is unchanged, up to styleCacheTTL, so an attachment that stops being
// nameable leaves the stylesheet within that time.
const (
	styleCacheTTL     = time.Minute
	styleCacheEntries = 256
)

type styleCacheEntry struct {
	source string
	sheet  *roomstyle.Stylesheet
	at     time.Time
}

var styleCache = struct {
	sync.Mutex
	rooms map[string]styleCacheEntry
}{rooms: map[string]styleCacheEntry{}}

func sanitizedStyle(room string, style *RoomStyle) *roomstyle.Stylesheet {
	now := time.Now()
	styleCache.Lock()
	entry, ok := styleCache.rooms[room]
	styleCache.Unlock()
	if ok && entry.source == style.Source && now.Sub(entry.at) < styleCacheTTL {
		return entry.sheet
	}
	var sheet *roomstyle.Stylesheet
	if out, _, err := roomstyle.SanitizeWith(style.Source, room, roomstyle.Options{Attachment: style.Attachment}); err == nil && out.CSS != "" {
		sheet = &out
	}
	styleCache.Lock()
	if len(styleCache.rooms) >= styleCacheEntries {
		clear(styleCache.rooms)
	}
	styleCache.rooms[room] = styleCacheEntry{source: style.Source, sheet: sheet, at: now}
	styleCache.Unlock()
	return sheet
}

// cspHost accepts the Host forms a CSP host-source can express: a DNS name or
// IPv4 address with an optional port. Anything else renders the room unstyled.
var cspHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[0-9]{1,5})?$`)

// roomStyleCSP replaces the site-wide policy on a page that links room CSS. It
// is the backstop if the sanitizer ever misses something: stylesheets, images
// and fonts may come only from the paths room CSS is allowed to name, so a
// stray url() can reach neither another origin nor a same-origin write path
// such as /w/ (CSP path matching is by prefix on the normalized URL).
func roomStyleCSP(host string) string {
	return "default-src 'self'; script-src 'self'; " +
		"style-src " + host + "/assets/ " + host + "/room-style/; " +
		"img-src " + host + "/a/ " + host + "/assets/ data:; " +
		"font-src " + host + "/a/; " +
		"connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"
}

// applyRoomStyle sets p.RoomStyle for a room or conversation page and, when the
// stylesheet will be linked, the tightened CSP. Call before headers are written.
func applyRoomStyle(w http.ResponseWriter, r *http.Request, service board.Service, p *page) {
	style, sheet := loadRoomStyle(r.Context(), service, p.RoomName)
	if sheet == nil {
		return
	}
	host := strings.ToLower(r.Host)
	if !cspHost.MatchString(host) {
		return
	}
	view := &roomStyleView{Class: sheet.Scope, Owner: style.Owner, Active: r.URL.Query().Get("unstyled") != "1"}
	view.Href = "/room-style/" + url.PathEscape(p.RoomName) + "/" + sheet.Hash + ".css"
	view.Toggle = p.Path
	if view.Active {
		view.Toggle = p.Path + "?unstyled=1"
		w.Header().Set("Content-Security-Policy", roomStyleCSP(host))
	}
	p.RoomStyle = view
}

// serveRoomStyle answers /room-style/<room>/<hash>.css. The hash pins the
// content, so the response is immutable; a stale hash is a 404, never a
// different stylesheet under the old name.
func serveRoomStyle(w http.ResponseWriter, r *http.Request, service board.Service) {
	room, file, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/room-style/"), "/")
	hash, css := strings.CutSuffix(file, ".css")
	var sheet *roomstyle.Stylesheet
	if ok && css && r.URL.RawQuery == "" {
		_, sheet = loadRoomStyle(r.Context(), service, room)
	}
	if sheet == nil || sheet.Hash != hash {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(sheet.CSS))
	}
}

// canvasClass is the canvas root class for a message's body on this page, or
// "" when the body renders plainly: no active room style, a removed message, or
// a message from another room.
func canvasClass(p page, e board.Message) string {
	if p.RoomStyle == nil || !p.RoomStyle.Active || e.Hidden || e.Room != p.RoomName {
		return ""
	}
	return p.RoomStyle.Class
}
