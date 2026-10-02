package board

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strings"
)

// The authenticated scope keeps a position from being reused with different
// filters. Permissions are still evaluated on every read, inside its transaction.
func olderScope(c Command, opts ListOptions) []byte {
	if opts.Scope == "front" {
		opts.Scope = ""
	}
	scope, _ := json.Marshal([]string{c.Room, c.Page, c.To, c.Target, c.Kind, c.Query, opts.Scope})
	hash := sha256.Sum256(scope)
	return hash[:]
}

func (s *Store) olderCursor(seq int64, c Command, opts ListOptions) string {
	s.cursorMu.RLock()
	defer s.cursorMu.RUnlock()
	plain := make([]byte, 8)
	binary.BigEndian.PutUint64(plain, uint64(seq))
	ad := append([]byte("older-v1:"+s.generation+":"), olderScope(c, opts)...)
	sealed := s.cursorCipher.Seal(nil, nil, plain, ad)
	return s.generation + ":" + base64.RawURLEncoding.EncodeToString(sealed)
}

func (s *Store) parseOlderCursor(value string, c Command, opts ListOptions) (int64, error) {
	s.cursorMu.RLock()
	defer s.cursorMu.RUnlock()
	parts := strings.Split(value, ":")
	if len(parts) != 2 || len(value) > 4096 {
		return 0, problem(400, "invalid_cursor", "Use an older_cursor returned by this server with the same filters.")
	}
	if parts[0] != s.generation {
		return 0, problem(409, "cursor_reset", "The server generation changed. Start again from the newest page.")
	}
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return 0, problem(400, "invalid_cursor", "Invalid older cursor.")
	}
	ad := append([]byte("older-v1:"+s.generation+":"), olderScope(c, opts)...)
	plain, err := s.cursorCipher.Open(nil, nil, sealed, ad)
	if err != nil || len(plain) != 8 {
		return 0, problem(400, "invalid_cursor", "Older cursor is invalid or belongs to another feed or filter.")
	}
	seq := int64(binary.BigEndian.Uint64(plain))
	if seq <= 0 {
		return 0, problem(400, "invalid_cursor", "Invalid older cursor position.")
	}
	return seq, nil
}

func (s *Store) attachOlderCursor(ctx context.Context, tx *sql.Tx, res *Result, c Command, opts ListOptions, where []string, args []any) error {
	var oldest int64
	if len(res.Messages) == 0 {
		if res.OlderCursor == "" {
			return nil
		}
		// An empty bounded front-page scan can resume from its scanned edge,
		// but only advertise continuation if matching history remains there.
		var err error
		oldest, err = s.parseOlderCursor(res.OlderCursor, c, opts)
		if err != nil {
			return err
		}
	} else {
		oldest = res.Messages[0].internalSequence
		for _, event := range res.Messages[1:] {
			oldest = min(oldest, event.internalSequence)
		}
	}
	var more bool
	query := "SELECT EXISTS(SELECT 1 FROM events e JOIN rooms r ON r.name=e.room WHERE " + strings.Join(where, " AND ") + " AND e.seq<?)"
	if err := tx.QueryRowContext(ctx, query, append(append([]any{}, args...), oldest)...).Scan(&more); err != nil {
		return err
	}
	if more {
		res.OlderCursor = s.olderCursor(oldest, c, opts)
	} else {
		res.OlderCursor = ""
	}
	if c.Older != "" {
		res.Data["has_more"] = more
	}
	return nil
}
