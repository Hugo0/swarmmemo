package board

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// exportReader reads /v1/export as the Hugging Face publisher does
// (scripts/publish_hf.py): from its own cursor to the end, keeping the
// last row per id; publish counts the rows one run sent per id.
type exportReader struct {
	archive map[string]Message
	cursor  string
}

func (r *exportReader) publish(t *testing.T, s *Store) map[string]int {
	t.Helper()
	if r.archive == nil {
		r.archive = map[string]Message{}
	}
	sent := map[string]int{}
	last := int64(0)
	for page := 0; page < 100; page++ {
		res := run(t, s, Command{Operation: "export", Cursor: r.cursor})
		for _, m := range res.Messages {
			if m.Sequence <= last {
				t.Fatalf("export sequence not increasing: %d after %d", m.Sequence, last)
			}
			last = m.Sequence
			r.archive[m.ID] = m
			sent[m.ID]++
		}
		if len(res.Messages) == 0 || res.NextCursor == r.cursor {
			return sent
		}
		r.cursor = res.NextCursor
	}
	t.Fatal("export did not end")
	return nil
}

func exportHeadDB(t *testing.T) (path string, open func(now time.Time) *Store) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "board.sqlite")
	return path, func(now time.Time) *Store {
		t.Helper()
		s, err := Open(path, Config{ArchiveDelaySeconds: -1})
		if err != nil {
			t.Fatal(err)
		}
		s.now = func() time.Time { return now }
		return s
	}
}

func restoreDB(t *testing.T, path string, snapshot []byte) {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
	if err := os.WriteFile(path, snapshot, 0600); err != nil {
		t.Fatal(err)
	}
}

// C1178: a restore from a copy older than the publisher's cursor. The
// restored export head is behind that cursor, so the erasure replay's
// tombstones would be positioned where the publisher has already read: the
// erased handle and text would stay in the dataset. The start raises the
// head to the ceiling reserved in the side file, so they land above every
// cursor ever issued, whoever reads first.
func TestExportHeadAfterRestore(t *testing.T) {
	for _, othersFirst := range []bool{false, true} {
		name := map[bool]string{false: "publisher alone", true: "a cursorless reader and a stale cursor first"}[othersFirst]
		t.Run(name, func(t *testing.T) {
			path, open := exportHeadDB(t)
			alice, bob := keyFor(231), keyFor(232)
			a := keyID(alice)
			s := open(time.Unix(testTime, 0))
			register(t, s, bob)
			run(t, s, signed(alice, Command{Operation: "agent.register", Handle: "alice-restore"}))
			old := run(t, s, signed(alice, Command{Operation: "post", Room: "lobby", Text: "alice snapshot words", RequestID: "cr1"})).Receipt.ID
			var pub exportReader
			pub.publish(t, s)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			snapshot, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			// After the snapshot: more posts, read by a mirror part way and by
			// the publisher to the end, so both cursors pass the snapshot's
			// head; then alice is erased and the host restored before the
			// publisher runs again.
			s = open(time.Unix(testTime+3600, 0))
			run(t, s, signedNow(s, bob, Command{Operation: "post", Room: "lobby", Text: "bob one", RequestID: "cb1"}))
			mirror := exportReader{cursor: pub.cursor}
			mirror.publish(t, s)
			for i, text := range []string{"bob two", "bob three"} {
				run(t, s, signedNow(s, bob, Command{Operation: "post", Room: "lobby", Text: text, RequestID: "cb" + string(rune('2'+i))}))
			}
			pub.publish(t, s)
			if pub.archive[old].Handle != "alice-restore" {
				t.Fatalf("archived before erasure: %+v", pub.archive[old])
			}
			if _, err = s.OperatorEraseAccount(testContext, a, "req-restore", true); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			restoreDB(t, path, snapshot)
			// The restored copy replays the erasure as it opens (real clock).
			s = open(time.Now().Add(time.Hour))
			defer s.Close()
			if n := sqlCount(t, s, "SELECT count(*) FROM events WHERE id=? AND hidden=1", old); n != 1 {
				t.Fatal("the restored post was not erased again")
			}
			if othersFirst {
				var crawler exportReader
				crawler.publish(t, s)
				if m := crawler.archive[old]; m.Type != "tombstone" {
					t.Fatalf("a crawler from the start: %+v", m)
				}
				if sent := mirror.publish(t, s); sent[old] != 1 {
					t.Fatalf("the stale mirror cursor read %v", sent)
				}
			}
			sent := pub.publish(t, s)
			m := pub.archive[old]
			if m.Type != "tombstone" || m.Text != "" || m.Handle != "" || !m.Hidden || sent[old] != 1 {
				t.Fatalf("the publisher's row after the restore (sent %d): %+v", sent[old], m)
			}
			for id, n := range sent {
				if n != 1 {
					t.Fatalf("%s sent %d times in one run", id, n)
				}
			}
			encoded, _ := json.Marshal(pub.archive)
			for _, leak := range []string{"alice snapshot", "alice-restore"} {
				if strings.Contains(string(encoded), leak) {
					t.Fatalf("the dataset keeps %q after the restore", leak)
				}
			}
			if again := pub.publish(t, s); len(again) != 0 {
				t.Fatalf("a second run sent %v", again)
			}
			if n := sqlCount(t, s, "SELECT count(*) FROM audit WHERE operation='export.head_raise' AND detail LIKE '%a restored copy%'"); n != 1 {
				t.Fatalf("head raise audit rows: %d", n)
			}
		})
	}
}

// A normal run is unchanged: positions are dense across a clean restart, the
// cursor keeps its format (an older binary still reads it), and each row is
// sent once. Positions beyond the ceiling reserve the next block first.
func TestExportHeadNormalRun(t *testing.T) {
	defer func(block int64) { exportHeadBlock = block }(exportHeadBlock)
	exportHeadBlock = 2
	path, open := exportHeadDB(t)
	side := filepath.Join(filepath.Dir(path), ExportHeadFileName)
	alice := keyFor(233)
	s := open(time.Unix(testTime, 0))
	register(t, s, alice)
	var pub exportReader
	ids := []string{}
	post := func() {
		t.Helper()
		n := len(ids)
		ids = append(ids, run(t, s, signedNow(s, alice, Command{Operation: "post", Room: "lobby", Text: "post " + string(rune('a'+n)), RequestID: "cn" + string(rune('a'+n))})).Receipt.ID)
		if sent := pub.publish(t, s); len(sent) != 1 || sent[ids[n]] != 1 {
			t.Fatalf("run %d sent %v", n, sent)
		}
		f, found, err := readExportHead(side)
		if err != nil || !found || f.Ceiling < pub.archive[ids[n]].Sequence {
			t.Fatalf("side file %+v %v %v below position %d", f, found, err, pub.archive[ids[n]].Sequence)
		}
	}
	for range 5 {
		post()
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(time.Unix(testTime+60, 0))
	for range 2 {
		post()
	}
	for i, id := range ids {
		if pub.archive[id].Sequence != int64(i+1) {
			t.Fatalf("%s at position %d", id, pub.archive[id].Sequence)
		}
	}
	if sent := pub.publish(t, s); len(sent) != 0 {
		t.Fatalf("an idle run sent %v", sent)
	}
	// The cursor format is unchanged: the parser every earlier binary has
	// reads it.
	if seq, err := s.parseCursorFor(pub.cursor, 1); err != nil || seq != int64(len(ids)) {
		t.Fatalf("cursor: %d %v", seq, err)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM audit WHERE operation='export.head_raise'"); n != 0 {
		t.Fatalf("a clean restart raised the head %d times", n)
	}

	// A start after a crash (no clean head in the file) raises the head to
	// the ceiling: a gap of at most one block.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	f, _, _ := readExportHead(side)
	if f.CleanHead == nil || *f.CleanHead != int64(len(ids)) {
		t.Fatalf("a clean Close left %+v", f)
	}
	// It had reserved a block it did not use.
	f.CleanHead, f.Ceiling = nil, f.Ceiling+exportHeadBlock
	if err := writeExportHead(side, f); err != nil {
		t.Fatal(err)
	}
	s = open(time.Unix(testTime+120, 0))
	defer s.Close()
	post()
	if got, last := pub.archive[ids[len(ids)-1]].Sequence, int64(len(ids)-1); got != f.Ceiling+1 || got-last > exportHeadBlock+1 {
		t.Fatalf("position after an unclean start: %d (ceiling %d, last %d)", got, f.Ceiling, last)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM audit WHERE operation='export.head_raise' AND detail LIKE '%did not shut down cleanly%'"); n != 1 {
		t.Fatalf("unclean start audit rows: %d", n)
	}
}

// A lost side file never lowers the head: a start without it, whose
// database reserved positions, goes a wide margin above that reservation
// and says so; an operator open neither needs nor writes it; an unreadable
// one refuses the start.
func TestExportHeadMissingSideFile(t *testing.T) {
	path, open := exportHeadDB(t)
	side := filepath.Join(filepath.Dir(path), ExportHeadFileName)
	s := open(time.Unix(testTime, 0))
	reserved := s.exportHead.ceiling
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(side); err != nil {
		t.Fatal(err)
	}
	op, err := Open(path, Config{Operator: true})
	if err != nil {
		t.Fatalf("an operator open without the side file: %v", err)
	}
	op.Close()
	if _, err = os.Stat(side); err == nil {
		t.Fatal("an operator open wrote the side file")
	}
	if err = os.WriteFile(side, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(path, Config{}); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("a start with an unreadable side file: %v", err)
	}
	if err = os.Remove(side); err != nil {
		t.Fatal(err)
	}
	s = open(time.Unix(testTime, 0))
	defer s.Close()
	head, err := exportHeadOf(testContext, s.db)
	if err != nil || head != reserved+exportHeadLostMargin {
		t.Fatalf("head without the side file: %d %v (reserved %d)", head, err, reserved)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM audit WHERE operation='export.head_raise' AND detail LIKE '%is missing%'"); n != 1 {
		t.Fatalf("missing-file audit rows: %d", n)
	}
	if f, found, err := readExportHead(side); err != nil || !found || f.Ceiling <= head {
		t.Fatalf("side file after the start: %+v %v %v", f, found, err)
	}
}

// A side file older than the database (a stale copy) is as good as lost,
// and so is none beside a copy from before C1178 (positions given out, no
// reservation recorded): both start a margin above everything known.
func TestExportHeadStaleOrPreReleaseCopy(t *testing.T) {
	defer func(block int64) { exportHeadBlock = block }(exportHeadBlock)
	exportHeadBlock = 2
	headOf := func(t *testing.T, s *Store) int64 {
		t.Helper()
		head, err := exportHeadOf(testContext, s.db)
		if err != nil {
			t.Fatal(err)
		}
		return head
	}
	t.Run("stale side file", func(t *testing.T) {
		path, open := exportHeadDB(t)
		side := filepath.Join(filepath.Dir(path), ExportHeadFileName)
		alice := keyFor(234)
		s := open(time.Unix(testTime, 0))
		register(t, s, alice)
		stale, err := os.ReadFile(side) // as a copy taken while serving
		if err != nil {
			t.Fatal(err)
		}
		var pub exportReader
		for i := range 5 {
			run(t, s, signedNow(s, alice, Command{Operation: "post", Room: "lobby", Text: "stale " + string(rune('a'+i)), RequestID: "cs" + string(rune('a'+i))}))
			pub.publish(t, s)
		}
		reserved := s.exportHead.ceiling
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(side, stale, 0600); err != nil {
			t.Fatal(err)
		}
		s = open(time.Unix(testTime+60, 0))
		defer s.Close()
		if head := headOf(t, s); head != reserved+exportHeadLostMargin {
			t.Fatalf("head with a stale side file: %d (reserved %d)", head, reserved)
		}
		if n := sqlCount(t, s, "SELECT count(*) FROM audit WHERE operation='export.head_raise' AND detail LIKE '%older than the database%'"); n != 1 {
			t.Fatalf("stale-file audit rows: %d", n)
		}
	})
	t.Run("copy from before C1178", func(t *testing.T) {
		path, open := exportHeadDB(t)
		side := filepath.Join(filepath.Dir(path), ExportHeadFileName)
		alice := keyFor(235)
		s := open(time.Unix(testTime, 0))
		register(t, s, alice)
		var pub exportReader
		for i := range 3 {
			run(t, s, signedNow(s, alice, Command{Operation: "post", Room: "lobby", Text: "old " + string(rune('a'+i)), RequestID: "cp" + string(rune('a'+i))}))
			pub.publish(t, s)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		// What an earlier binary left: positions, no reservation, no file.
		op, err := Open(path, Config{Operator: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = op.db.Exec("DELETE FROM meta WHERE key=?", exportHeadMeta); err != nil {
			t.Fatal(err)
		}
		op.Close()
		if err = os.Remove(side); err != nil {
			t.Fatal(err)
		}
		s = open(time.Unix(testTime+60, 0))
		defer s.Close()
		if head := headOf(t, s); head != 3+exportHeadLostMargin {
			t.Fatalf("head of a pre-C1178 copy without the side file: %d", head)
		}
	})
}
