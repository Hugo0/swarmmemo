package board

import "testing"

// C974: the signed record's log_entries counts only leaves its checkpoint
// covers, like key_events; a leaf appended after the checkpoint is in no tree
// the record proves against, so a stranger could not recompute it.
func TestRecordLogEntriesStopAtItsCheckpoint(t *testing.T) {
	s := openTest(t, Config{})
	steppingClock(s)
	a := keyFor(181)
	run(t, s, signed(a, Command{Operation: "agent.register", Handle: "counted-to-cp"}))
	run(t, s, signed(a, Command{Operation: "post", Room: "lobby", Text: "inside the checkpoint"}))
	if _, err := s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	run(t, s, signed(a, Command{Operation: "post", Room: "lobby", Text: "after the checkpoint"}))
	rec, err := s.ReadLogRecord(testContext, keyID(a))
	if err != nil {
		t.Fatal(err)
	}
	var covered, all int
	if err := s.db.QueryRow("SELECT count(*) FROM tlog_leaves WHERE subject=? AND idx<?", keyID(a), rec.Record.Checkpoint.Size).Scan(&covered); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM tlog_leaves WHERE subject=?", keyID(a)).Scan(&all); err != nil {
		t.Fatal(err)
	}
	if all <= covered {
		t.Fatalf("setup: want a leaf after the checkpoint, got %d covered of %d", covered, all)
	}
	if got := rec.Record.Counts.LogEntries; got != covered {
		t.Fatalf("log_entries %d, want %d (leaves under checkpoint size %d; %d in all)", got, covered, rec.Record.Checkpoint.Size, all)
	}
}
