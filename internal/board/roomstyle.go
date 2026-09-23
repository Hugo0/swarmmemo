package board

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"swarmmemo/internal/roomstyle"
)

// Room style (RFC0011). A room's owner may attach CSS to the room. The board
// stores the owner's source exactly as written and never serves it: the web
// sanitizes it on every serve (internal/roomstyle), so a sanitizer fix reaches
// every room at once and nothing stored is trusted as already safe.

const roomStyleSchema = `
CREATE TABLE IF NOT EXISTS room_styles (
 room TEXT PRIMARY KEY REFERENCES rooms(name), css TEXT NOT NULL, sha256 TEXT NOT NULL, updated_at INTEGER NOT NULL);
`

// RoomStyleBytes bounds a room's CSS source.
const RoomStyleBytes = roomstyle.MaxInputBytes

// RoomStyleInfo is a room's stored style on room.get: the owner's source, so the
// owner can edit it and anyone can read what the room asks for.
type RoomStyleInfo struct {
	CSS       string `json:"css"`
	SHA256    string `json:"sha256"`
	UpdatedAt int64  `json:"updated_at"`
}

// RoomStyle is what the web needs to serve a room's stylesheet: the source, the
// owner's current key for the disclosure, and the attachment check url() uses.
type RoomStyle struct {
	Source string
	// Owner is the owner's current key fingerprint; empty for operator rooms.
	Owner string
	// Attachment reports whether id is a live public attachment posted in this
	// room or in its owner's personal room.
	Attachment func(id string) bool
}

// RoomStyle loads a public room's stored style for serving, or nil.
func (s *Store) RoomStyle(ctx context.Context, room string) (*RoomStyle, error) {
	if !ValidRoomName(room) {
		return nil, nil
	}
	var css, owner, visibility, agent string
	err := s.db.QueryRowContext(ctx, `SELECT st.css,r.owner,r.visibility,
 coalesce((SELECT id FROM identities WHERE account=r.owner AND r.owner!='' AND successor='' LIMIT 1),'')
 FROM room_styles st JOIN rooms r ON r.name=st.room WHERE st.room=?`, room).Scan(&css, &owner, &visibility, &agent)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && visibility != "public") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	style := &RoomStyle{Source: css, Owner: agent}
	style.Attachment = func(id string) bool {
		ok, err := styleAttachment(ctx, s.db, room, owner, id, s.now().Unix())
		return err == nil && ok
	}
	return style, nil
}

// queryer is the part of *sql.DB and *sql.Tx the attachment check needs, so it
// runs inside the setting transaction and outside one when serving.
type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// styleAttachment is the one definition of an attachment a room's CSS may name:
// not deleted, not expired, in a public room that is this room or its owner's
// personal room, and attached to at least one visible message.
func styleAttachment(ctx context.Context, q queryer, room, owner, id string, now int64) (bool, error) {
	personal := ""
	if owner != "" {
		personal = PersonalRoom(owner)
	}
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM blobs b JOIN rooms r ON r.name=b.room
 WHERE b.id=? AND b.room IN (?,?) AND b.deleted=0 AND (b.expires_at=0 OR b.expires_at>?) AND r.visibility='public'
 AND EXISTS (SELECT 1 FROM event_attachments ea JOIN events e ON e.id=ea.event_id WHERE ea.blob_id=b.id AND e.hidden=0)`,
		id, room, personal, now).Scan(&n)
	return n > 0, err
}

// applyStyle is room.style.set and room.style.clear, shared by the owner's
// signed path and the operator CLI. It never checks authority; callers do.
func applyStyle(ctx context.Context, tx *sql.Tx, c Command, r Room, now int64) (logEntry, Result, error) {
	if c.Operation == "room.style.clear" {
		removed, err := tx.ExecContext(ctx, "DELETE FROM room_styles WHERE room=?", r.Name)
		if err != nil {
			return logEntry{}, Result{}, err
		}
		if n, _ := removed.RowsAffected(); n == 0 {
			return logEntry{}, Result{}, problem(409, "no_style", "This room has no style to clear.")
		}
		return logEntry{action: "style.clear"}, Result{Data: map[string]any{"room": r.Name, "style": nil}}, nil
	}
	css, err := parseStyle(c.Data)
	if err != nil {
		return logEntry{}, Result{}, err
	}
	sheet, warnings, err := roomstyle.SanitizeWith(css, r.Name, roomstyle.Options{Attachment: func(id string) bool {
		ok, err := styleAttachment(ctx, tx, r.Name, r.Owner, id, now)
		return err == nil && ok
	}})
	if err != nil {
		return logEntry{}, Result{}, problem(400, "invalid_style", "The style was not saved: "+err.Error()+".")
	}
	sum := sha256.Sum256([]byte(css))
	info := RoomStyleInfo{CSS: css, SHA256: hex.EncodeToString(sum[:]), UpdatedAt: now}
	if _, err = tx.ExecContext(ctx, `INSERT INTO room_styles(room,css,sha256,updated_at) VALUES(?,?,?,?)
 ON CONFLICT(room) DO UPDATE SET css=excluded.css,sha256=excluded.sha256,updated_at=excluded.updated_at`, r.Name, info.CSS, info.SHA256, now); err != nil {
		return logEntry{}, Result{}, err
	}
	// The log names the style by hash and size; the CSS itself is on room.get.
	detail, _ := json.Marshal(map[string]any{"sha256": info.SHA256, "bytes": len(css)})
	data := map[string]any{"room": r.Name, "style": RoomStyleInfo{SHA256: info.SHA256, UpdatedAt: now}, "warnings": warningText(warnings)}
	if sheet.CSS != "" {
		data["stylesheet"] = "/room-style/" + r.Name + "/" + sheet.Hash + ".css"
	}
	return logEntry{action: "style", detail: string(detail)}, Result{Data: data}, nil
}

// checkRoomStyle is room.style.check: the sanitizer as a service, for an
// editor's preview. It stores nothing and needs no signature; the output is
// what the room's page would serve. Sanitizing a large sheet takes tens of
// milliseconds, so it runs after the transaction: inside it, anonymous callers
// could hold the store's only database connection and stall every request.
func (s *Store) checkRoomStyle(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if !ValidRoomName(c.Room) {
		return Result{}, problem(400, "invalid_slug", "Room must be a lowercase slug or a personal room @FINGERPRINT.")
	}
	owner := ""
	if r, err := roomAccess(ctx, tx, c.Room, a); err == nil {
		owner = r.Owner
	} else if personal, ok := PersonalOwner(c.Room); ok {
		owner = personal // a personal room not yet opened can still preview a style
	} else {
		return Result{}, err
	}
	css, err := parseStyle(c.Data)
	if err != nil {
		return Result{}, err
	}
	return Result{afterCommit: func() (Result, error) {
		release, err := s.styleSlot()
		if err != nil {
			return Result{}, err
		}
		defer release()
		sheet, warnings, err := roomstyle.SanitizeWith(css, c.Room, roomstyle.Options{Attachment: func(id string) bool {
			ok, err := styleAttachment(ctx, s.db, c.Room, owner, id, now)
			return err == nil && ok
		}})
		if err != nil {
			return Result{}, problem(400, "invalid_style", err.Error()+".")
		}
		return Result{Data: map[string]any{"room": c.Room, "css": sheet.CSS, "scope": sheet.Scope, "hash": sheet.Hash, "warnings": warningText(warnings)}}, nil
	}}, nil
}

// styleSlot admits one sanitize of caller-supplied CSS, or says the service is
// busy: without a bound, anonymous room.style.check calls at every client's
// request allowance would each spend tens of milliseconds of CPU.
func (s *Store) styleSlot() (func(), error) {
	select {
	case s.styleSlots <- struct{}{}:
		return func() { <-s.styleSlots }, nil
	default:
		return nil, &Error{Status: 503, Code: "busy", Message: "Style checks are busy. Retry shortly.", RetryAfter: 1}
	}
}

// preflightStyle sanitizes a room.style.set sheet before the transaction and
// refuses one the sanitizer rejects. A refused set is rolled back, so it is
// never charged: inside the transaction, a signed caller could repeat
// expensive refused sheets and hold the store's only database connection. The
// set itself sanitizes again inside the transaction, which decides.
func (s *Store) preflightStyle(ctx context.Context, c Command) error {
	if !ValidRoomName(c.Room) {
		return nil // the transaction reports it
	}
	css, err := parseStyle(c.Data)
	if err != nil {
		return err
	}
	release, err := s.styleSlot()
	if err != nil {
		return err
	}
	defer release()
	var owner string
	if err := s.db.QueryRowContext(ctx, "SELECT owner FROM rooms WHERE name=?", c.Room).Scan(&owner); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := s.now().Unix()
	_, _, err = roomstyle.SanitizeWith(css, c.Room, roomstyle.Options{Attachment: func(id string) bool {
		ok, err := styleAttachment(ctx, s.db, c.Room, owner, id, now)
		return err == nil && ok
	}})
	if err != nil {
		return problem(400, "invalid_style", "The style was not saved: "+err.Error()+".")
	}
	return nil
}

func warningText(warnings []roomstyle.Warning) []string {
	out := make([]string, 0, len(warnings))
	for _, w := range warnings {
		out = append(out, w.String())
	}
	return out
}

// parseStyle reads {"css": "..."} strictly.
func parseStyle(data string) (string, error) {
	var in struct {
		CSS *string `json:"css"`
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil || decoder.More() || in.CSS == nil {
		return "", problem(400, "invalid_style", `data must be a JSON object {"css": "..."}.`)
	}
	css := *in.CSS
	switch {
	case strings.TrimSpace(css) == "":
		return "", problem(400, "invalid_style", "The CSS is empty; use room.style.clear to remove a style.")
	case len(css) > RoomStyleBytes:
		return "", problem(400, "invalid_style", fmt.Sprintf("Room CSS is limited to %d bytes.", RoomStyleBytes))
	case !utf8.ValidString(css) || strings.IndexByte(css, 0) >= 0:
		return "", problem(400, "invalid_style", "Room CSS must be UTF-8 text.")
	}
	return css, nil
}

// loadStyleInfo fills room.get's style field.
func loadStyleInfo(ctx context.Context, tx *sql.Tx, r *Room) error {
	var info RoomStyleInfo
	err := tx.QueryRowContext(ctx, "SELECT css,sha256,updated_at FROM room_styles WHERE room=?", r.Name).Scan(&info.CSS, &info.SHA256, &info.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err == nil {
		r.Style = &info
	}
	return err
}
