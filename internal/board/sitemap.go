package board

import (
	"context"
	"database/sql"
	"fmt"
)

// The sitemap lists what the service invites search engines to index: public,
// non-personal rooms with at least one visible post, and every visible,
// non-simulation thread root in them, shown at its newest version. A plain
// post waits out the archive delay (the same moderation window the public
// export observes) before it is offered; a signed Markdown article is listed at
// once, as it always was. Replies are reached through their thread's page.
// Private rooms, personal (@ACCOUNT) rooms, hidden messages, simulations and
// removed newest versions never appear.

// SitemapTitleBytes is how much of a Markdown post's current text a sitemap
// row carries: enough to derive the title and slug of its canonical address,
// without holding whole bodies for every post in memory.
const SitemapTitleBytes = 2048

// sitemapPostWhere selects the thread roots (o) that may be listed.
const sitemapPostWhere = `o.reply_to='' AND o.supersedes='' AND o.hidden=0 AND o.kind<>'simulation'
 AND o.room NOT LIKE '@%' AND (o.created_at<=? OR (o.format='markdown' AND o.public_key<>''))
 AND EXISTS(SELECT 1 FROM rooms ro WHERE ro.name=o.room AND ro.visibility='public')`

// sitemapCurrent joins a root (o) to its newest version (e), itself if unedited.
const sitemapCurrent = `e.seq=coalesce((SELECT max(v.seq) FROM events v WHERE v.origin=o.id),o.seq)`

// SitemapRoom is one listed room and the time of its newest visible post.
type SitemapRoom struct {
	Name     string
	Modified int64
}

// PublicSitemapCounts returns how many rooms and posts PublicSitemapRooms and
// PublicSitemapPosts would list in total.
func (s *Store) PublicSitemapCounts(ctx context.Context) (rooms, posts int, err error) {
	err = s.publicRead(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM rooms r WHERE r.visibility='public' AND r.name NOT LIKE '@%'
 AND EXISTS(SELECT 1 FROM events e WHERE e.room=r.name AND e.hidden=0 AND e.kind<>'simulation')`).Scan(&rooms); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM events o JOIN events e ON `+sitemapCurrent+`
 WHERE `+sitemapPostWhere+` AND e.hidden=0 AND e.kind<>'simulation'`, s.archiveCutoff()).Scan(&posts)
	})
	return rooms, posts, err
}

// PublicSitemapRooms lists rooms by name, from offset, at most limit.
func (s *Store) PublicSitemapRooms(ctx context.Context, offset, limit int) ([]SitemapRoom, error) {
	if offset < 0 || limit < 1 || limit > 50000 {
		return nil, fmt.Errorf("sitemap rooms: bad range")
	}
	rooms := []SitemapRoom{}
	err := s.publicRead(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT name,modified FROM (SELECT r.name AS name,
 (SELECT max(e.created_at) FROM events e WHERE e.room=r.name AND e.hidden=0 AND e.kind<>'simulation') AS modified
 FROM rooms r WHERE r.visibility='public' AND r.name NOT LIKE '@%') WHERE modified IS NOT NULL ORDER BY name LIMIT ? OFFSET ?`, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var room SitemapRoom
			if err := rows.Scan(&room.Name, &room.Modified); err != nil {
				return err
			}
			rooms = append(rooms, room)
		}
		return rows.Err()
	})
	return rooms, err
}

// PublicSitemapPosts calls each for listed thread roots in the order they were
// posted, from offset, at most limit. Each is the root's newest version, with
// the root's ID as Origin(), CreatedAt the time of that version, and Text cut
// to SitemapTitleBytes (empty unless Markdown). each must not keep it.
func (s *Store) PublicSitemapPosts(ctx context.Context, offset, limit int, each func(Message)) error {
	if offset < 0 || limit < 1 || limit > 50000 {
		return fmt.Errorf("sitemap posts: bad range")
	}
	return s.publicRead(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT o.id,e.kind,e.handle,e.public_key,e.created_at,e.format,
 CASE WHEN e.format='markdown' THEN substr(e.text,1,?) ELSE '' END
 FROM events o JOIN events e ON `+sitemapCurrent+`
 WHERE `+sitemapPostWhere+` AND e.hidden=0 AND e.kind<>'simulation' ORDER BY o.seq LIMIT ? OFFSET ?`,
			SitemapTitleBytes, s.archiveCutoff(), limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m Message
			if err := rows.Scan(&m.ID, &m.Kind, &m.Handle, &m.PublicKey, &m.CreatedAt, &m.Format, &m.Text); err != nil {
				return err
			}
			m.Type, m.Visibility = "message", "public"
			m.Curated = curatorPost(m.Kind, m.Handle, m.PublicKey)
			each(m)
		}
		return rows.Err()
	})
}

// archiveCutoff is the newest creation time the public export would publish now.
func (s *Store) archiveCutoff() int64 { return s.now().Unix() - s.config.ArchiveDelaySeconds }
