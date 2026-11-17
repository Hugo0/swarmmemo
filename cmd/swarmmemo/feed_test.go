package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

func TestFeedReplayCommand(t *testing.T) {
	ctx := context.Background()
	store, err := board.Open(filepath.Join(t.TempDir(), "swarmmemo.db"), board.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.SetAllowanceParams(ctx, board.PostingParamsNamespace, []byte(`{"anonymous_top_level_per_hour":1000,"anonymous_top_level_per_room_day":1000}`), "test", 0); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Essay 1: On names", "Essay 2: On memory", "Essay 3: On drift", "Would you trust a forked agent?"} {
		if _, err = store.Execute(ctx, board.Command{Operation: "post", Room: "lobby", Text: text}, "192.0.2.1"); err != nil {
			t.Fatal(err)
		}
	}
	profile := filepath.Join(t.TempDir(), "flat.json")
	if err = os.WriteFile(profile, []byte(`{"diversity":{"per_author":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = operatorFeed(ctx, store, []string{"replay", "--against", profile, "--top", "4"}, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 5 || !strings.Contains(lines[0], "flat.json") || !strings.Contains(lines[0], "default") {
		t.Fatalf("replay:\n%s", out.String())
	}
	// The default moves the third essay down past the others: diversity.
	if !strings.Contains(out.String(), "[d]") {
		t.Fatalf("no post moved down:\n%s", out.String())
	}
	for _, args := range [][]string{{}, {"rerun"}, {"replay", "--top", "0"}, {"replay", "extra"}, {"replay", "--at", "yesterday"}, {"replay", "--profile", filepath.Join(t.TempDir(), "missing.json")}} {
		if err := operatorFeed(ctx, store, args, &out); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
}
