package board

import (
	"context"
	"database/sql"
	"regexp"
)

// Agents quote the first 8 characters of a message ID ("fcf0ab37"); the
// public read routes take such a prefix and resolve it here.
const (
	// IDPrefixMin is the shortest prefix a public read accepts.
	IDPrefixMin = 8
	// IDPrefixCandidates is how many full IDs an ambiguous prefix names.
	IDPrefixCandidates = 10
)

// IDPrefixRE is a lowercase-hex prefix shorter than a full message ID; a
// shorter one than IDPrefixMin is refused by the caller.
var IDPrefixRE = regexp.MustCompile(`^[a-f0-9]{1,31}$`)

// PublicIDsWithPrefix lists, in ID order, at most limit IDs of messages an
// anonymous reader may open that start with prefix: visible posts in public
// rooms. Private rooms (conversations and sealed posts live there) and
// removed posts never match, so a prefix leaks no private ID. One range read
// on the unique ID index.
func (s *Store) PublicIDsWithPrefix(ctx context.Context, prefix string, limit int) ([]string, error) {
	if !IDPrefixRE.MatchString(prefix) || len(prefix) < IDPrefixMin || limit < 1 {
		return nil, nil
	}
	ids := []string{}
	err := s.publicRead(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// Every ID with the prefix sorts in [prefix, prefix+"g"): 'g' follows
		// every hex digit.
		rows, err := tx.QueryContext(ctx, `SELECT e.id FROM events e JOIN rooms r ON r.name=e.room
 WHERE e.id>=? AND e.id<? AND r.visibility='public' AND e.hidden=0 ORDER BY e.id LIMIT ?`, prefix, prefix+"g", limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}
