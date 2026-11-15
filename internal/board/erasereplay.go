package board

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Erasure replay (C1012, build 2). erasure_log lives in the database, so a
// restored older copy has lost the erasures made after it was taken. Every
// erasure is therefore also appended to a side file beside the database
// (ErasureLogFileName, like log.key and signals-raw.key), and a writable
// Open replays each side entry the database lacks through the idempotent
// EraseAccount before the service starts; erasure_log rows the side file
// lacks (a crash between commit and append) are appended to it. The side
// file holds what erasure_log holds: fingerprints, times, counts, link
// digests and receipt hashes, never content.

// ErasureLogFileName is the side copy of erasure_log, beside the database.
const ErasureLogFileName = "erasure-log.jsonl"

// erasureSideEntry is one side-file line: an erasure_log row.
type erasureSideEntry struct {
	Account       string       `json:"account"`
	ErasedAt      int64        `json:"erased_at"`
	Reference     string       `json:"reference,omitempty"`
	Counts        []EraseCount `json:"counts"`
	LinkDigests   []string     `json:"link_digests"`
	RawHeld       int64        `json:"raw_held"`
	ReceiptSHA256 string       `json:"receipt_sha256"`
}

func erasureSideFromRun(run erasureRun) erasureSideEntry {
	r := run.receipt
	return erasureSideEntry{Account: r.Account, ErasedAt: r.ErasedAt, Reference: r.Reference, Counts: r.Counts, LinkDigests: run.digests, RawHeld: r.RawHeld, ReceiptSHA256: r.ReceiptHash}
}

// erasureSidePath is the side file for the database at path; "" for an
// in-memory board.
func erasureSidePath(path string) string {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file::memory:") {
		return ""
	}
	return filepath.Join(filepath.Dir(path), ErasureLogFileName)
}

// appendErasureSide appends one entry, as one write, and syncs it.
func (s *Store) appendErasureSide(e erasureSideEntry) error {
	if s.erasureSide == "" {
		return nil
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.erasureSide, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(line, '\n')); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// readErasureSide reads the side file; a missing one is empty. A torn last
// line (a crash mid-append) is skipped: its row is in the database, and the
// sync below appends it again.
func readErasureSide(path string) ([]erasureSideEntry, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []erasureSideEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		var e erasureSideEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil || !fingerprintRE.MatchString(e.Account) || len(e.ReceiptSHA256) != 64 {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// replayErasures is the writable Open's restore step: side entries missing
// from erasure_log are erased again, each in its own transaction, with its
// original row; rows missing from the side file are appended to it.
func (s *Store) replayErasures(ctx context.Context) (int, error) {
	if s.erasureSide == "" {
		return 0, nil
	}
	side, err := readErasureSide(s.erasureSide)
	if err != nil {
		return 0, fmt.Errorf("erasure side file %s: %w", s.erasureSide, err)
	}
	have := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, "SELECT account,erased_at,reference,counts,link_digests,raw_held,receipt_sha256 FROM erasure_log ORDER BY seq")
	if err != nil {
		return 0, err
	}
	var logged []erasureSideEntry
	for rows.Next() {
		var e erasureSideEntry
		var counts, digests string
		if err = rows.Scan(&e.Account, &e.ErasedAt, &e.Reference, &counts, &digests, &e.RawHeld, &e.ReceiptSHA256); err != nil {
			rows.Close()
			return 0, err
		}
		_ = json.Unmarshal([]byte(counts), &e.Counts)
		_ = json.Unmarshal([]byte(digests), &e.LinkDigests)
		have[e.ReceiptSHA256] = true
		logged = append(logged, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	inSide := map[string]bool{}
	replayed := 0
	for _, e := range side {
		inSide[e.ReceiptSHA256] = true
		if have[e.ReceiptSHA256] {
			continue
		}
		have[e.ReceiptSHA256] = true
		if err = s.replayErasure(ctx, e); err != nil {
			return replayed, fmt.Errorf("replaying the erasure of %s (receipt %s): %w", e.Account, e.ReceiptSHA256, err)
		}
		replayed++
	}
	for _, e := range logged {
		if !inSide[e.ReceiptSHA256] {
			if err = s.appendErasureSide(e); err != nil {
				return replayed, fmt.Errorf("erasure side file %s: %w", s.erasureSide, err)
			}
		}
	}
	return replayed, nil
}

// replayErasure erases one account again, as of now, and restores its
// erasure_log row as first written. It writes no audit row the log would
// take as a second leaf; the replay is in the row's own audit trail as
// account.erase.replay.
func (s *Store) replayErasure(ctx context.Context, e erasureSideEntry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().Unix()
	keep, err := eraseEvidence(ctx, tx, e.Account, now)
	if err != nil {
		return err
	}
	if _, err = EraseAccount(ctx, tx, e.Account, now, keep); err != nil {
		return err
	}
	ids, err := eraseIDs(ctx, tx, e.Account)
	if err != nil {
		return err
	}
	counts, _ := json.Marshal(e.Counts)
	digests, _ := json.Marshal(e.LinkDigests)
	if e.LinkDigests == nil {
		digests = []byte("[]")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO erasure_log(account,erased_at,reference,counts,link_digests,raw_held,receipt_sha256) VALUES(?,?,?,?,?,?,?)",
		e.Account, e.ErasedAt, e.Reference, string(counts), string(digests), e.RawHeld, e.ReceiptSHA256); err != nil {
		return err
	}
	if err = audit(ctx, tx, "account.erase.replay", "operator", e.Account, "replayed from the erasure side file after a restore; "+erasureReceiptDetail+e.ReceiptSHA256, now); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.rawErase(ctx, ids, keep)
}
