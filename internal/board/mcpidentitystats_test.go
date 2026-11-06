package board

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// C778: AddMCPIdentityStats stores only checked keys, skips the rest, and
// MCPIdentityStats sums a window by family, last tools most first.
func TestMCPIdentityStatsRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "board.sqlite"), Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	today, yesterday, old := now.Format("2006-01-02"), now.AddDate(0, 0, -1).Format("2006-01-02"), now.AddDate(0, 0, -9).Format("2006-01-02")
	for day, counts := range map[string]map[string]int64{
		today: {MCPIdentityKey("assistant", "openai", "calls"): 3, MCPIdentityKey("assistant", "openai", "created"): 2,
			MCPLastBeforeIdentityKey("openai", "read_feed"): 1, MCPLastBeforeIdentityKey("openai", MCPIdentityFirst): 1,
			MCPLastBeforeNoneKey("openai", "read_thread"): 5, MCPLastBeforeOverflowKey: 2,
			"identity:assistant:openai:bogus": 1, "lastbefore_none:openai:Bad Tool": 1, MCPIdentityKey("nope", "openai", "calls"): 1},
		yesterday: {MCPIdentityKey("mcp", "openai", "created"): 1, MCPIdentityKey("mcp", "openai", "calls"): 1, MCPLastBeforeIdentityKey("openai", "read_feed"): 1,
			MCPLastBeforeNoneKey("claude", "list_rooms"): 4},
		old: {MCPIdentityKey("mcp", "openai", "created"): 50},
	} {
		if err := store.AddMCPIdentityStats(ctx, day, counts); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AddMCPIdentityStats(ctx, "2026-13-01", nil); err == nil {
		t.Fatal("bad day accepted")
	}
	w, err := store.MCPIdentityStats(ctx, now, 7)
	if err != nil {
		t.Fatal(err)
	}
	if w.Overflow != 2 || w.Skipped != 3 || len(w.Families) != 2 {
		t.Fatalf("window: %+v", w)
	}
	o := w.Families[0]
	if o.Family != "openai" || o.Calls != 4 || o.Created != 3 || o.Profiles["assistant"] != 2 || o.Profiles["mcp"] != 1 {
		t.Fatalf("openai: %+v", o)
	}
	if fmt.Sprint(o.LastBefore) != "[{read_feed 2} {none 1}]" || fmt.Sprint(o.LastNone) != "[{read_thread 5}]" {
		t.Fatalf("openai last: %v %v", o.LastBefore, o.LastNone)
	}
	if c := w.Families[1]; c.Family != "claude" || c.Created != 0 || fmt.Sprint(c.LastNone) != "[{list_rooms 4}]" {
		t.Fatalf("claude: %+v", c)
	}
	if _, err := store.MCPIdentityStats(ctx, now, 0); err == nil {
		t.Fatal("0 days accepted")
	}
}
