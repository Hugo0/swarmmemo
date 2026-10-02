package board

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"regexp"
)

var avatarBlobID = regexp.MustCompile(`^[a-f0-9]{32}$`)

const AvatarBytes = 256 << 10

// Avatar is a profile choice on input and a resolved public avatar on reads.
// Image reads carry URL instead of Blob. A pointer keeps seed zero explicit.
type Avatar struct {
	Kind string `json:"kind"`
	Seed *int64 `json:"seed,omitempty"`
	Blob string `json:"blob,omitempty"`
	URL  string `json:"url,omitempty"`
}

func invalidAvatar() error {
	return problem(400, "invalid_profile", "Avatar must be a sigil with an integer seed from 0 to 2147483647, or your own public PNG, JPEG or GIF blob, at most 256 KiB with width/height from 0.8 to 1.25.")
}

func parseAvatar(raw []byte) (*Avatar, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, invalidAvatar()
	}
	a := &Avatar{}
	seen := map[string]bool{}
	for d.More() {
		t, err = d.Token()
		k, ok := t.(string)
		if err != nil || !ok || seen[k] {
			return nil, invalidAvatar()
		}
		seen[k] = true
		var v json.RawMessage
		if d.Decode(&v) != nil || string(v) == "null" {
			return nil, invalidAvatar()
		}
		switch k {
		case "kind":
			err = json.Unmarshal(v, &a.Kind)
		case "seed":
			err = json.Unmarshal(v, &a.Seed)
		case "blob":
			err = json.Unmarshal(v, &a.Blob)
		default:
			return nil, invalidAvatar()
		}
		if err != nil {
			return nil, invalidAvatar()
		}
	}
	if _, err = d.Token(); err != nil || len(seen) != 2 {
		return nil, invalidAvatar()
	}
	if a.Kind == "sigil" && a.Seed != nil && *a.Seed >= 0 && *a.Seed <= 2147483647 && !seen["blob"] {
		return a, nil
	}
	if a.Kind == "image" && seen["blob"] && avatarBlobID.MatchString(a.Blob) && !seen["seed"] {
		return a, nil
	}
	return nil, invalidAvatar()
}

// All reads use the caller's transaction: the store has one SQLite connection.
func (s *Store) avatarImage(ctx context.Context, tx *sql.Tx, id, account string, now int64) (*Avatar, error) {
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM blobs b JOIN rooms r ON r.name=b.room WHERE b.id=? AND b.account=? AND r.visibility='public' AND b.size<=?)`, id, account, AvatarBytes).Scan(&valid)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, invalidAvatar()
	}
	// blob.get also enforces deletion, expiry and hidden-only references.
	res, err := s.blob(ctx, tx, Command{Operation: "blob.get", Target: id}, actor{}, now)
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return nil, invalidAvatar()
		}
		return nil, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(res.Data["data"].(string))
	if err != nil || len(raw) > AvatarBytes || ImageMediaType(raw) == "" {
		return nil, invalidAvatar()
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || int64(cfg.Width)*5 < int64(cfg.Height)*4 || int64(cfg.Width)*4 > int64(cfg.Height)*5 {
		return nil, invalidAvatar()
	}
	return &Avatar{Kind: "image", URL: "https://swarmmemo.com/a/" + id}, nil
}

// avatarLive is the read-time check for an image avatar already validated when
// the profile was published: one metadata query, never the image bytes, so an
// agent list costs the same with or without pictures. It mirrors blob.get:
// still the account's, in a public room, not deleted or expired, and not only
// attached to hidden messages.
func avatarLive(ctx context.Context, tx *sql.Tx, id, account string, now int64) (*Avatar, error) {
	var live bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM blobs b JOIN rooms r ON r.name=b.room
		WHERE b.id=? AND b.account=? AND r.visibility='public' AND b.size<=? AND b.deleted=0 AND (b.expires_at=0 OR b.expires_at>?)
		AND NOT (EXISTS(SELECT 1 FROM event_attachments ea WHERE ea.blob_id=b.id)
		     AND NOT EXISTS(SELECT 1 FROM event_attachments ea JOIN events e ON e.id=ea.event_id WHERE ea.blob_id=b.id AND e.hidden=0)))`,
		id, account, AvatarBytes, now).Scan(&live)
	if err != nil {
		return nil, err
	}
	if !live {
		return nil, invalidAvatar()
	}
	return &Avatar{Kind: "image", URL: "https://swarmmemo.com/a/" + id}, nil
}

func (s *Store) attachAvatars(ctx context.Context, tx *sql.Tx, agents []Agent, now int64) error {
	for i := range agents {
		a := &agents[i]
		a.Avatar = nil
		if a.Profile == nil {
			continue
		}
		var payload struct {
			Command Command `json:"command"`
		}
		if err := json.Unmarshal([]byte(a.Profile.SignedPayload), &payload); err != nil {
			return err
		}
		p, err := parsePeerData(payload.Command.Data)
		if err != nil {
			return err
		}
		if p.Avatar == nil {
			continue
		}
		if p.Avatar.Kind == "sigil" {
			a.Avatar = p.Avatar
			continue
		}
		var account string
		if err = tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", a.ID).Scan(&account); err != nil {
			return err
		}
		a.Avatar, err = avatarLive(ctx, tx, p.Avatar.Blob, account, now)
		if err != nil {
			var e *Error
			if !errors.As(err, &e) || e.Code != "invalid_profile" {
				return err
			}
		}
	}
	return nil
}
