package board

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"strconv"
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
		if len(data) == 0 || len(data) > AttachmentBytes {
			return Result{}, problem(413, "attachment_size", fmt.Sprintf("A file must be 1 byte to %d KiB, decoded.", AttachmentBytes>>10))
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
		// A declared image type must be borne out by the bytes: the page renders
		// what the metadata claims, so a mismatch would put an unrenderable or
		// mislabelled file into an <img>. SVG is refused outright.
		if base := InlineImageType(mediaType); strings.HasPrefix(strings.ToLower(mediaType), "image/") {
			if base == "" || ImageMediaType(data) != base {
				return Result{}, problem(400, "invalid_image", "An image attachment must be "+inlineImageFormats+", its bytes must match its declared media type, and it must be under "+strconv.Itoa(InlineImagePixels/(1<<20))+" megapixels. SVG is not accepted because it can carry script; send it as a download type instead.")
			}
		}
		// Files are kept like message text: no server-imposed lifetime. An explicit
		// ttl is the uploader's own removal choice and is honoured.
		ttl, expires := c.TTL, int64(0)
		if ttl < 0 || ttl > AttachmentMaxTTL {
			return Result{}, problem(400, "invalid_ttl", "A file ttl is optional: omit it to keep the file, or give a positive number of seconds after which it is removed.")
		}
		if ttl > 0 {
			expires = now + ttl
		}
		if err = s.charge(ctx, tx, a, int64(len(data)+len(filename)+len(mediaType)+512), now); err != nil {
			return Result{}, err
		}
		hash := sha256.Sum256(data)
		b := Attachment{ID: randomID(), Room: c.Room, Filename: filename, MediaType: mediaType, Hash: hex.EncodeToString(hash[:]), Size: int64(len(data)), CreatedAt: now, ExpiresAt: expires}
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
		b.Expired = blobExpired(b.ExpiresAt, now)
		return Result{Data: map[string]any{"blob": b}}, nil
	}
	if b.Deleted || blobExpired(b.ExpiresAt, now) {
		return Result{}, problem(410, "attachment_gone", "This attachment was deleted, or the ttl its uploader set has passed.")
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
		if b.Deleted || blobExpired(b.ExpiresAt, now) {
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
			b.Expired = blobExpired(b.ExpiresAt, now)
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

// AttachmentMaxTTL bounds an explicit ttl only so now+ttl cannot overflow; it is
// not a retention limit.
const AttachmentMaxTTL = int64(1) << 40

// blobExpired reports whether an uploader-set ttl has passed. expires_at 0 means
// the file has no expiry.
func blobExpired(expiresAt, now int64) bool { return expiresAt != 0 && expiresAt <= now }

// legacyBlobLifetime was the server default and cap before files were kept
// indefinitely (2026-09-23). Clients sent it by default, so it is not treated
// as an uploader's choice.
const legacyBlobLifetime = 30 * 86400

// extendLegacyBlobs clears the old server-imposed expiry from live files, once.
// Bytes already pruned cannot be recovered and are left as they are; shorter
// explicit ttls are kept.
func extendLegacyBlobs(tx *sql.Tx, now int64) (int64, error) {
	var done int
	if err := tx.QueryRow("SELECT count(*) FROM meta WHERE key='blob_legacy_expiry_cleared'").Scan(&done); err != nil || done > 0 {
		return 0, err
	}
	r, err := tx.Exec("UPDATE blobs SET expires_at=0 WHERE deleted=0 AND data IS NOT NULL AND expires_at>? AND expires_at-created_at=?", now, legacyBlobLifetime)
	if err != nil {
		return 0, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec("INSERT INTO meta(key,value) VALUES('blob_legacy_expiry_cleared',?)", strconv.FormatInt(n, 10))
	return n, err
}

// PruneExpiredBlobs removes payload bytes whose uploader-set ttl passed,
// retaining their metadata and immutable message references. Files without a
// ttl (expires_at 0) are never touched. Invoke from the operator maintenance timer.
func (s *Store) PruneExpiredBlobs(ctx context.Context) (int64, error) {
	r, err := s.db.ExecContext(ctx, "UPDATE blobs SET data=NULL WHERE expires_at>0 AND expires_at<=? AND data IS NOT NULL", s.now().Unix())
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}
