package board

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOnlineBackupRestore(t *testing.T) {
	s := openTest(t, Config{})
	receipt := run(t, s, Command{Operation: "post", Text: "survives complete host loss", RequestID: "backup-fixture"}).Receipt
	dest := filepath.Join(t.TempDir(), "restored.db")
	if err := s.Backup(context.Background(), dest); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(context.Background(), dest); err == nil {
		t.Fatal("backup overwrote existing destination")
	}
	st, err := os.Stat(dest)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("unsafe backup mode", err)
	}
	restored, err := Open(dest, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err = restored.Integrity(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := run(t, restored, Command{Operation: "message.get", MessageID: receipt.ID})
	if len(r.Messages) != 1 || r.Messages[0].Text != "survives complete host loss" {
		t.Fatal(r)
	}
	if err = restored.RotateGeneration(context.Background()); err != nil {
		t.Fatal(err)
	}
	fails(t, restored, Command{Operation: "messages.list", Cursor: receipt.Cursor}, "cursor_reset")
}

func TestModerationQueue(t *testing.T) {
	s := openTest(t, Config{})
	id := run(t, s, Command{Operation: "post", Text: "review this"}).Receipt.ID
	run(t, s, Command{Operation: "report", MessageID: id, Reason: "test report"})
	queue, err := s.PendingReports(context.Background(), 25)
	if err != nil || len(queue) != 1 {
		t.Fatal(queue, err)
	}
	before := run(t, s, Command{Operation: "message.get", MessageID: id})
	if before.Messages[0].Hidden {
		t.Fatal("report alone hid content")
	}
	if err = s.Moderate(context.Background(), id, "operator reviewed", true); err != nil {
		t.Fatal(err)
	}
	queue, err = s.PendingReports(context.Background(), 25)
	if err != nil || len(queue) != 0 {
		t.Fatal(queue, err)
	}
}
