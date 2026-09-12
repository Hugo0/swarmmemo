package board

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"mime"
	"strings"
	"unicode"
	"unicode/utf8"
)

const blobColumns = "id,room,filename,media_type,hash,size,created_at,expires_at,deleted"

func scanBlob(row scanner) (Attachment, error) {
	var b Attachment
	err := row.Scan(&b.ID, &b.Room, &b.Filename, &b.MediaType, &b.Hash, &b.Size, &b.CreatedAt, &b.ExpiresAt, &b.Deleted)
	return b, err
}

func (s *Store) blob(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if c.Operation == "blob.put" {
		if err := requireSigned(a); err != nil {
			return Result{}, err
		}
		r, err := roomAccess(ctx, tx, c.Room, a)
		if err != nil {
			return Result{}, err
		}
		if c.Visibility != "" && c.Visibility != r.Visibility {
			return Result{}, problem(409, "visibility_mismatch", "Requested visibility does not match the existing room; the attachment was not stored.")
		}
		data, err := base64.RawURLEncoding.DecodeString(c.Data)
		if err != nil || base64.RawURLEncoding.EncodeToString(data) != c.Data {
			return Result{}, problem(400, "invalid_base64", "Attachment data must use canonical unpadded base64url.")
		}
		if len(data) == 0 || len(data) > 1<<20 {
			return Result{}, problem(413, "attachment_size", "Attachments must contain 1–1048576 decoded bytes.")
		}
		filename := c.Filename
		if filename == "" {
			filename = "attachment.bin"
		}
		if !utf8.ValidString(filename) || strings.ContainsAny(filename, "/\\") || strings.IndexFunc(filename, unicode.IsControl) >= 0 || filename == "." || filename == ".." {
			return Result{}, problem(400, "invalid_filename", "Use a filename without paths or control characters.")
		}
		mediaType := c.MediaType
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		if _, _, err = mime.ParseMediaType(mediaType); err != nil {
			return Result{}, problem(400, "invalid_media_type", "Use a valid MIME content type.")
		}
		ttl := c.TTL
		if ttl == 0 {
			ttl = 30 * 86400
		}
		if ttl < 1 || ttl > 30*86400 {
			return Result{}, problem(400, "invalid_ttl", "Attachment TTL must be 1–2592000 seconds (30 days).")
		}
		if err = s.charge(ctx, tx, a, int64(len(data)+len(filename)+len(mediaType)+512), now); err != nil {
			return Result{}, err
		}
		hash := sha256.Sum256(data)
		b := Attachment{ID: randomID(), Room: c.Room, Filename: filename, MediaType: mediaType, Hash: hex.EncodeToString(hash[:]), Size: int64(len(data)), CreatedAt: now, ExpiresAt: now + ttl}
		if _, err = tx.ExecContext(ctx, "INSERT INTO blobs(id,room,account,filename,media_type,hash,size,created_at,expires_at,data) VALUES(?,?,?,?,?,?,?,?,?,?)", b.ID, b.Room, a.account, b.Filename, b.MediaType, b.Hash, b.Size, b.CreatedAt, b.ExpiresAt, data); err != nil {
			return Result{}, err
		}
		// Signed bytes are verified before admission; only the blob bytes and hash
		// persist. Receipts do not duplicate the base64 payload in the request log.
		return Result{Data: map[string]any{"blob": b}}, nil
	}
	id := c.MessageID
	if id == "" {
		id = c.Target
	}
	b, err := scanBlob(tx.QueryRowContext(ctx, "SELECT "+blobColumns+" FROM blobs WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, problem(404, "not_found", "Attachment not found.")
	}
	if err != nil {
		return Result{}, err
	}
	r, err := roomAccess(ctx, tx, b.Room, a)
	if err != nil {
		return Result{}, problem(404, "not_found", "Attachment not found.")
	}
	if c.Operation == "blob.delete" {
		if err = requireSigned(a); err != nil {
			return Result{}, err
		}
		var owner string
		if err = tx.QueryRowContext(ctx, "SELECT account FROM blobs WHERE id=?", id).Scan(&owner); err != nil {
			return Result{}, err
		}
		if owner != a.account && r.Owner != a.account {
			return Result{}, problem(403, "owner_required", "Only the attachment author or room owner can delete it.")
		}
		if !b.Deleted {
			if err = s.charge(ctx, tx, a, 256, now); err != nil {
				return Result{}, err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE blobs SET deleted=1,data=NULL WHERE id=?", id); err != nil {
				return Result{}, err
			}
			if r.Visibility == "public" {
				if _, err = tx.ExecContext(ctx, "INSERT INTO changes(event_id,changed_at,urgent) SELECT event_id,?,1 FROM event_attachments WHERE blob_id=?", now, id); err != nil {
					return Result{}, err
				}
			}
			if err = audit(ctx, tx, c.Operation, a.id, id, c.Reason, now); err != nil {
				return Result{}, err
			}
		}
		b.Deleted = true
		b.Expired = b.ExpiresAt <= now
		return Result{Data: map[string]any{"blob": b}}, nil
	}
	if b.Deleted || b.ExpiresAt <= now {
		return Result{}, problem(410, "attachment_gone", "This attachment was deleted or its stated retention period expired.")
	}
	var total, visible int
	if err = tx.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(CASE WHEN e.hidden=0 THEN 1 ELSE 0 END),0) FROM event_attachments ea JOIN events e ON e.id=ea.event_id WHERE ea.blob_id=?", id).Scan(&total, &visible); err != nil {
		return Result{}, err
	}
	if total > 0 && visible == 0 {
		return Result{}, problem(404, "not_found", "Attachment not found.")
	}
	var data []byte
	if err = tx.QueryRowContext(ctx, "SELECT data FROM blobs WHERE id=?", id).Scan(&data); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"blob": b, "data": base64.RawURLEncoding.EncodeToString(data)}}, nil
}

func (s *Store) attachToPost(ctx context.Context, tx *sql.Tx, c Command, eventID string, now int64) error {
	seen := map[string]bool{}
	for position, id := range c.Attachments {
		if seen[id] {
			return problem(400, "duplicate_attachment", "List each attachment ID only once.")
		}
		seen[id] = true
		b, err := scanBlob(tx.QueryRowContext(ctx, "SELECT "+blobColumns+" FROM blobs WHERE id=?", id))
		if errors.Is(err, sql.ErrNoRows) {
			return problem(404, "not_found", "Attachment not found in this room.")
		}
		if err != nil {
			return err
		}
		if b.Room != c.Room {
			return problem(404, "not_found", "Attachment not found in this room.")
		}
		if b.Deleted || b.ExpiresAt <= now {
			return problem(410, "attachment_gone", "An attachment was deleted or expired; upload a new copy.")
		}
		// A hidden-only reference cannot be republished to circumvent moderation.
		var refs, visible int
		if err = tx.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(CASE WHEN e.hidden=0 THEN 1 ELSE 0 END),0) FROM event_attachments ea JOIN events e ON e.id=ea.event_id WHERE ea.blob_id=?", id).Scan(&refs, &visible); err != nil {
			return err
		}
		if refs > 0 && visible == 0 {
			return problem(404, "not_found", "Attachment not found in this room.")
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO event_attachments(event_id,blob_id,position) VALUES(?,?,?)", eventID, id, position); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) loadAttachments(ctx context.Context, tx *sql.Tx, events []Message, now int64) error {
	for i := range events {
		if events[i].Hidden {
			continue
		}
		rows, err := tx.QueryContext(ctx, "SELECT "+"b.id,b.room,b.filename,b.media_type,b.hash,b.size,b.created_at,b.expires_at,b.deleted"+" FROM blobs b JOIN event_attachments ea ON ea.blob_id=b.id WHERE ea.event_id=? ORDER BY ea.position", events[i].ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			b, err := scanBlob(rows)
			if err != nil {
				rows.Close()
				return err
			}
			b.Expired = b.ExpiresAt <= now
			events[i].Attachments = append(events[i].Attachments, b)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// PruneExpiredBlobs removes expired payload bytes, retaining their metadata and
// immutable message references. Invoke from the operator maintenance timer.
func (s *Store) PruneExpiredBlobs(ctx context.Context) (int64, error) {
	r, err := s.db.ExecContext(ctx, "UPDATE blobs SET data=NULL WHERE expires_at<=? AND data IS NOT NULL", s.now().Unix())
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}
