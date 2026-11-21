package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// Export head ceiling (C1178). /v1/export positions (export_changes.seq)
// only grow, and a reader such as the Hugging Face publisher keeps the
// cursor of the last position it read. A database restored from an older
// copy has a lower head, so every row it positions after the restore (the
// erasure replay's tombstones, blob deletions, newly ready posts) would land
// at positions a reader has already passed, and never be read: an erased
// handle would stay in the dataset.
//
// So the positions are reserved in blocks in a side file beside the
// database (ExportHeadFileName, like erasure-log.jsonl), outside anything a
// restore replaces: before an export commits a position above the reserved
// ceiling, it writes a ceiling one block higher to the file (and to meta
// export_ceiling). The file's ceiling is therefore at or above every
// position ever served. A clean Close records the head it left; a start
// whose head is not that one (a restored copy, or a start after a crash)
// raises the export_changes head to the ceiling before it serves, with an
// export.head_raise audit row, so every row positioned from then on lands
// above every cursor ever issued. Readers trigger nothing; no row moves; the
// cursor format is unchanged. The cost is a gap of at most one block of
// positions after each such start (positions were never dense: readers
// check only that they increase). A start without the side file whose
// database has given out or reserved positions (a copy on a new host, or
// from before C1178), or with a file older than the database, puts the
// head exportHeadLostMargin above everything known, with a warning and the
// audit row: never lower. An unreadable side file refuses the start. The
// file is never bundled into a snapshot: a copy of it can only be stale.

// ExportHeadFileName is the export head side file, beside the database.
const ExportHeadFileName = "export-head.json"

// exportHeadBlock is how many positions one write of the side file
// reserves (a variable for the tests).
var exportHeadBlock int64 = 1024

// exportHeadLostMargin is how far above the database's own reservation a
// start without the side file puts the head.
const exportHeadLostMargin int64 = 1 << 20

// exportHeadMeta is the meta key of the ceiling the database last reserved.
const exportHeadMeta = "export_ceiling"

// exportHeadFile is the side file's content.
type exportHeadFile struct {
	Ceiling int64 `json:"ceiling"`
	// CleanHead is the export head a clean Close left; absent while the
	// server runs (or after a crash).
	CleanHead *int64 `json:"clean_head,omitempty"`
	// Owner is the start that last took the file, so an older process
	// closing after a newer one started never marks it clean.
	Owner string `json:"owner,omitempty"`
}

// exportHeadState is the Store's side of it.
type exportHeadState struct {
	mu      sync.Mutex
	path    string // "" in memory or read-only
	owner   string // "" for an operator open, which never takes the file
	ceiling int64
}

func exportHeadPath(path string) string {
	if path == "" || path == ":memory:" || len(path) >= 13 && path[:13] == "file::memory:" {
		return ""
	}
	return filepath.Join(filepath.Dir(path), ExportHeadFileName)
}

func readExportHead(path string) (exportHeadFile, bool, error) {
	var f exportHeadFile
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, false, nil
	}
	if err != nil {
		return f, false, err
	}
	if err = json.Unmarshal(raw, &f); err != nil || f.Ceiling < 0 {
		return f, false, fmt.Errorf("unreadable export head file %s: fix it by hand, never delete it", path)
	}
	return f, true, nil
}

// writeExportHead replaces the side file (synced, then renamed into place)
// and never lowers its ceiling.
func writeExportHead(path string, f exportHeadFile) error {
	if cur, found, err := readExportHead(path); err != nil {
		return err
	} else if found && cur.Ceiling > f.Ceiling {
		f.Ceiling = cur.Ceiling
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err = file.Write(append(raw, '\n')); err == nil {
		err = file.Sync()
	}
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// exportHeadOf is the highest export position the database has given out.
func exportHeadOf(ctx context.Context, q rowQuerier) (int64, error) {
	var head int64
	err := q.QueryRowContext(ctx, "SELECT max(coalesce((SELECT max(seq) FROM sqlite_sequence WHERE name='export_changes'),0),coalesce((SELECT max(seq) FROM export_changes),0))").Scan(&head)
	return head, err
}

func exportHeadReserved(ctx context.Context, q rowQuerier) (int64, error) {
	var value string
	err := q.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", exportHeadMeta).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(value, 10, 64)
}

func setExportHeadReserved(ctx context.Context, tx *sql.Tx, ceiling int64) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", exportHeadMeta, strconv.FormatInt(ceiling, 10))
	return err
}

// openExportHead is a writable Open's step before it serves (operator says
// whether this is the operator CLI, which only loads the ceiling).
func (s *Store) openExportHead(ctx context.Context, path string, operator bool) error {
	h := &s.exportHead
	if h.path = exportHeadPath(path); h.path == "" {
		return nil
	}
	file, found, err := readExportHead(h.path)
	if err != nil {
		return err
	}
	reserved, err := exportHeadReserved(ctx, s.db)
	if err != nil {
		return err
	}
	h.ceiling = max(file.Ceiling, reserved)
	if operator {
		// The operator CLI never exports; it neither takes nor needs the file.
		return nil
	}
	h.owner = randomID()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	head, err := exportHeadOf(ctx, tx)
	if err != nil {
		return err
	}
	// The side file is gone while the database has given out positions (a
	// copy started on another host, a drill, a copy from before C1178), or
	// it is older than the database (its ceiling below what the database
	// reserved or gave out): what the live file reserved later is unknown,
	// so the head goes a wide margin above everything known, loudly.
	lost := !found && (reserved > 0 || head > 0) || found && (file.Ceiling < reserved || file.Ceiling < head)
	if lost {
		h.ceiling = max(file.Ceiling, reserved, head) + exportHeadLostMargin
	}
	if (lost || found && (file.CleanHead == nil || *file.CleanHead != head)) && h.ceiling > head {
		reason := "the last start did not shut down cleanly"
		switch {
		case lost && !found:
			reason = fmt.Sprintf("%s is missing; restore the host's own copy beside the database (C1178)", h.path)
			slog.Warn("export head file missing: the export head goes a margin above what the database reserved", "file", h.path, "reserved", reserved, "head", h.ceiling)
		case lost:
			reason = fmt.Sprintf("%s is older than the database (ceiling %d, reserved %d, head %d) (C1178)", h.path, file.Ceiling, reserved, head)
			slog.Warn("export head file older than the database: the export head goes a margin above it", "file", h.path, "ceiling", file.Ceiling, "reserved", reserved, "head", h.ceiling)
		case file.CleanHead != nil:
			reason = fmt.Sprintf("the last clean shutdown left the head at %d (a restored copy)", *file.CleanHead)
		}
		if err = setExportSequence(ctx, tx, h.ceiling); err != nil {
			return err
		}
		if err = audit(ctx, tx, "export.head_raise", "server", "export_changes", fmt.Sprintf("export head raised from %d to the reserved %d before serving: %s", head, h.ceiling, reason), s.now().Unix()); err != nil {
			return err
		}
		head = h.ceiling
	}
	if h.ceiling <= head {
		h.ceiling = head + exportHeadBlock
	}
	if err = writeExportHead(h.path, exportHeadFile{Ceiling: h.ceiling, Owner: h.owner}); err != nil {
		return err
	}
	if err = setExportHeadReserved(ctx, tx, h.ceiling); err != nil {
		return err
	}
	return tx.Commit()
}

// setExportSequence sets export_changes' AUTOINCREMENT head.
func setExportSequence(ctx context.Context, tx *sql.Tx, head int64) error {
	res, err := tx.ExecContext(ctx, "UPDATE sqlite_sequence SET seq=? WHERE name='export_changes'", head)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_, err = tx.ExecContext(ctx, "INSERT INTO sqlite_sequence(name,seq) VALUES('export_changes',?)", head)
	}
	return err
}

// reserveExportHead runs after an export positions rows and before it
// commits: a head above the reserved ceiling reserves the next block first.
func (s *Store) reserveExportHead(ctx context.Context, tx *sql.Tx) error {
	h := &s.exportHead
	if h.path == "" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	head, err := exportHeadOf(ctx, tx)
	if err != nil || head <= h.ceiling {
		return err
	}
	ceiling := head + exportHeadBlock
	cur, _, err := readExportHead(h.path)
	if err != nil {
		return err
	}
	if err = writeExportHead(h.path, exportHeadFile{Ceiling: ceiling, Owner: cur.Owner}); err != nil {
		return err
	}
	if err = setExportHeadReserved(ctx, tx, ceiling); err != nil {
		return err
	}
	h.ceiling = ceiling
	return nil
}

// closeExportHead records the head a clean Close leaves, if this start
// still owns the file.
func (s *Store) closeExportHead() {
	h := &s.exportHead
	if h.path == "" || h.owner == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cur, found, err := readExportHead(h.path)
	if err != nil || !found || cur.Owner != h.owner {
		return
	}
	head, err := exportHeadOf(context.Background(), s.db)
	if err != nil || head > h.ceiling {
		return
	}
	_ = writeExportHead(h.path, exportHeadFile{Ceiling: h.ceiling, CleanHead: &head, Owner: h.owner})
}
