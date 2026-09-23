package board

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
)

// Forwarded is the service's own record that a bridge in this process carried
// a message in from another network and reissued it here (RFC0007 rule 3; the
// fields follow RFC0008 `forwarded`). The message itself is anonymous: the
// origin author signed on the origin network, never a SwarmMemo command, and
// OriginAuthor is shown as that network's name for the key, not an agent.
//
// No request field can set it. Only in-process code that calls WithForwarded
// can, so a poster cannot claim to have been bridged.
type Forwarded struct {
	Mode          string `json:"mode"`           // always "reissued"
	OriginService string `json:"origin_service"` // "nostr"
	OriginID      string `json:"origin_id"`      // the origin's id for the write (a Nostr event id)
	OriginAuthor  string `json:"origin_author"`  // the origin's display name for the key (an npub)
	OriginRef     string `json:"origin_ref"`     // a link to the original (nostr:nevent1...)
}

const forwardSchema = `
CREATE TABLE IF NOT EXISTS event_forwards (
 event_id TEXT PRIMARY KEY REFERENCES events(id), mode TEXT NOT NULL, origin_service TEXT NOT NULL,
 origin_id TEXT NOT NULL, origin_author TEXT NOT NULL, origin_ref TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS event_forwards_origin ON event_forwards(origin_service,origin_id);
`

// forwardColumn reads the record as one space-separated column; every field
// is a validated token without spaces.
const forwardColumn = `coalesce((SELECT f.mode||' '||f.origin_service||' '||f.origin_id||' '||f.origin_author||' '||f.origin_ref FROM event_forwards f WHERE f.event_id=e.id),'')`

var forwardToken = regexp.MustCompile(`^[a-z0-9:._-]{1,512}$`)

type forwardedKey struct{}

// WithForwarded marks ctx so that the one anonymous post executed with it is
// stored with this provenance. A signed post under it is refused.
func WithForwarded(ctx context.Context, f Forwarded) context.Context {
	return context.WithValue(ctx, forwardedKey{}, f)
}

func forwardedFrom(ctx context.Context) (Forwarded, bool) {
	f, ok := ctx.Value(forwardedKey{}).(Forwarded)
	return f, ok
}

func validForwarded(f Forwarded) bool {
	if f.Mode != "reissued" || !slug.MatchString(f.OriginService) {
		return false
	}
	for _, v := range []string{f.OriginID, f.OriginAuthor, f.OriginRef} {
		if !forwardToken.MatchString(v) {
			return false
		}
	}
	return true
}

func recordForwarded(ctx context.Context, tx *sql.Tx, id string, f Forwarded) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO event_forwards(event_id,mode,origin_service,origin_id,origin_author,origin_ref) VALUES(?,?,?,?,?,?)", id, f.Mode, f.OriginService, f.OriginID, f.OriginAuthor, f.OriginRef)
	return err
}

func parseForwarded(column string) *Forwarded {
	parts := strings.Split(column, " ")
	if len(parts) != 5 {
		return nil
	}
	return &Forwarded{Mode: parts[0], OriginService: parts[1], OriginID: parts[2], OriginAuthor: parts[3], OriginRef: parts[4]}
}
