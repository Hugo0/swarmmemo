package board

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Read helpers for the public HTML view of versioned posts. They read public
// rooms only, like PublicUpdates: the web never renders private content, and
// these carry no caller identity to check membership against.

// CurrentVersion is the newest version of a message and how many versions the
// message has, including the original.
type CurrentVersion struct {
	Message  Message
	Versions int
}

func (s *Store) publicRead(ctx context.Context, read func(context.Context, *sql.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return read(ctx, tx)
}

func (s *Store) scanPublic(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]Message, error) {
	rows, err := tx.QueryContext(ctx, "SELECT "+eventColumns+" FROM events e JOIN rooms r ON r.name=e.room WHERE r.visibility='public' AND "+query, args...)
	if err != nil {
		return nil, err
	}
	events := []Message{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		events = append(events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return events, s.loadAttachments(ctx, tx, events, s.now().Unix())
}

// PublicCurrentVersions maps each given original message ID to its newest
// version. IDs without a newer version, and non-public ones, are absent.
func (s *Store) PublicCurrentVersions(ctx context.Context, ids []string) (map[string]CurrentVersion, error) {
	current := map[string]CurrentVersion{}
	if len(ids) == 0 {
		return current, nil
	}
	if len(ids) > 200 {
		ids = ids[:200]
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	err := s.publicRead(ctx, func(ctx context.Context, tx *sql.Tx) error {
		counts := map[string]int{}
		rows, err := tx.QueryContext(ctx, "SELECT origin,count(*) FROM events WHERE origin IN ("+marks+") GROUP BY origin", args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var origin string
			var n int
			if err = rows.Scan(&origin, &n); err != nil {
				rows.Close()
				return err
			}
			counts[origin] = n + 1
		}
		rows.Close()
		heads, err := s.scanPublic(ctx, tx, "e.seq IN (SELECT max(seq) FROM events WHERE origin IN ("+marks+") GROUP BY origin)", args...)
		if err != nil {
			return err
		}
		for _, head := range heads {
			current[head.Origin()] = CurrentVersion{Message: head, Versions: counts[head.Origin()]}
		}
		return nil
	})
	return current, err
}

// PublicVersions returns every version of the message that id belongs to,
// oldest first, starting with the original. It is empty when id is unknown or
// not public.
func (s *Store) PublicVersions(ctx context.Context, id string) ([]Message, error) {
	var versions []Message
	err := s.publicRead(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var origin string
		err := tx.QueryRowContext(ctx, "SELECT CASE WHEN origin<>'' THEN origin ELSE id END FROM events WHERE id=?", id).Scan(&origin)
		if errors.Is(err, sql.ErrNoRows) {
			versions = []Message{}
			return nil
		}
		if err != nil {
			return err
		}
		versions, err = s.scanPublic(ctx, tx, "(e.id=? OR e.origin=?) ORDER BY e.seq LIMIT ?", origin, origin, MaxVersions)
		return err
	})
	return versions, err
}

// PublicRoomArticles lists the current versions of articles rooted in one
// public room by the given key fingerprints, most recently updated first. It
// is how the web finds the posts that replace its hand-built guide pages; a
// version is by the same key as its original, and both are checked.
func (s *Store) PublicRoomArticles(ctx context.Context, room string, authors []string, limit int) ([]Message, error) {
	if len(authors) == 0 || room == "" {
		return []Message{}, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	if len(authors) > 32 {
		authors = authors[:32]
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(authors)), ",")
	args := []any{room}
	for _, a := range authors {
		args = append(args, a)
	}
	args = append(args, limit)
	for _, a := range authors {
		args = append(args, a)
	}
	args = append(args, limit)
	var articles []Message
	err := s.publicRead(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		articles, err = s.scanPublic(ctx, tx, `e.seq IN (
 SELECT coalesce((SELECT max(v.seq) FROM events v WHERE v.origin=o.id),o.seq) FROM events o
 WHERE o.room=? AND o.author IN (`+marks+`) AND o.format='markdown' AND o.reply_to='' AND o.supersedes='' AND o.public_key<>'' AND o.hidden=0 ORDER BY o.seq DESC LIMIT ?)
 AND e.author IN (`+marks+`) AND e.hidden=0 AND e.format='markdown' AND e.kind<>'simulation' ORDER BY e.created_at DESC,e.seq DESC LIMIT ?`, args...)
		return err
	})
	return articles, err
}

// PublicArticles lists current versions of long-form posts for the sitemap,
// most recently updated first. An article is a signed root post whose author
// chose Markdown, in a public room, still Markdown and visible in its current
// version, and not a simulation. Signing is required for Markdown at all, so
// anonymous posts cannot fill the sitemap.
func (s *Store) PublicArticles(ctx context.Context, limit int) ([]Message, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	var articles []Message
	err := s.publicRead(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		articles, err = s.scanPublic(ctx, tx, `e.seq IN (
 SELECT coalesce((SELECT max(v.seq) FROM events v WHERE v.origin=o.id),o.seq) FROM events o INDEXED BY events_articles
 WHERE o.format='markdown' AND o.reply_to='' AND o.supersedes='' AND o.public_key<>'' AND o.hidden=0 ORDER BY o.seq DESC LIMIT ?)
 AND e.hidden=0 AND e.format='markdown' AND e.kind<>'simulation' ORDER BY e.created_at DESC,e.seq DESC LIMIT ?`, 4*limit, limit)
		return err
	})
	return articles, err
}
