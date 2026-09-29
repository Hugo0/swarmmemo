package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

func TestLeverCommand(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "board.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	lever := func(line string) (string, error) {
		var out bytes.Buffer
		err := operatorLever(ctx, store, strings.Fields(line), &out)
		return out.String(), err
	}
	until := strconv.FormatInt(time.Now().Unix()+3600, 10)
	for _, line := range []string{
		"pull signed-only --reason flood --actor steward",
		"pull tier4-shrink 250000 --reason=flood --until " + until,
		"pull cut-budget post_bytes 500000 --reason flood",
		"pull block-prefix 203.0.113.0/24 --reason one-network",
	} {
		if out, err := lever(line); err != nil || !strings.Contains(out, `"action": "pull"`) {
			t.Fatalf("%s: %v %s", line, err, out)
		}
	}
	out, err := lever("list")
	var report board.LeverReport
	if err != nil || json.Unmarshal([]byte(out), &report) != nil || strings.Join(report.Pulled, ",") != "block-prefix,cut-budget,signed-only,tier4-shrink" || strings.Contains(out, "203.0.113") {
		t.Fatalf("list: %v %s", err, out)
	}
	for _, line := range []string{"release signed-only --reason over", "release cut-budget post_bytes", "release block-prefix 203.0.113.0/24", "release tier4-shrink"} {
		if out, err := lever(line); err != nil || !strings.Contains(out, `"action": "release"`) {
			t.Fatalf("%s: %v %s", line, err, out)
		}
	}
	if out, err = lever("expire"); err != nil || !strings.Contains(out, `"expired": 0`) {
		t.Fatalf("expire: %v %s", err, out)
	}
	for _, line := range []string{"", "pull", "pull signed-only", "pull signed-only --reason", "pull signed-only --reason a --reason b", "pull signed-only --bogus x --reason a",
		"pull signed-only --reason a --until soon", "release", "release signed-only", "release signed-only --until 5", "list extra", "frobnicate"} {
		if _, err := lever(line); err == nil {
			t.Fatalf("%q accepted", line)
		}
	}
}
