package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Backup takes a consistent online SQLite snapshot, including committed WAL.
// An existing destination is never overwritten. Protect this file like the live
// database: it includes private messages, permissions and recovery material.
func (s *Store) Backup(ctx context.Context, dest string) error {
	abs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if _, err = s.db.ExecContext(ctx, "VACUUM INTO ?", abs); err != nil {
		return fmt.Errorf("snapshot incomplete at %s: %w", abs, err)
	}
	f, err = os.OpenFile(abs, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	dir, err := os.Open(filepath.Dir(abs))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Store) Health(ctx context.Context) error {
	var value string
	return s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='generation'").Scan(&value)
}

func (s *Store) Integrity(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var result string
		if err = rows.Scan(&result); err != nil {
			return err
		}
		if result != "ok" {
			return errors.New("SQLite integrity check failed: " + result)
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	var table string
	var rowid sql.NullInt64
	var parent string
	var fkid int
	err = s.db.QueryRowContext(ctx, "PRAGMA foreign_key_check").Scan(&table, &rowid, &parent, &fkid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("SQLite foreign key check failed")
}

type ModerationReport struct {
	ID         string `json:"id"`
	MessageID  string `json:"message_id"`
	Room       string `json:"room"`
	Visibility string `json:"visibility"`
	Text       string `json:"text"`
	Reason     string `json:"reason"`
	CreatedAt  int64  `json:"created_at"`
}

// PendingReports is operator-only, never part of the public Execute surface.
func (s *Store) PendingReports(ctx context.Context, limit int) ([]ModerationReport, error) {
	if limit < 1 || limit > 100 {
		limit = 25
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.id,p.event_id,e.room,r.visibility,e.text,p.reason,p.created_at FROM reports p JOIN events e ON e.id=p.event_id JOIN rooms r ON r.name=e.room WHERE p.resolved=0 ORDER BY p.created_at,p.id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModerationReport{}
	for rows.Next() {
		var r ModerationReport
		if err = rows.Scan(&r.ID, &r.MessageID, &r.Room, &r.Visibility, &r.Text, &r.Reason, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
