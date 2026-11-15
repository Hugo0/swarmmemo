package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// readOnlyCommands are operator reads, each run below under the read-only
// open beside a live store (C553).
var readOnlyCommands = [][]string{
	{"stats", "referrers"}, {"stats", "assistants"}, {"reports"},
	{"funnel"}, {"funnel", "--days", "7"}, {"funnel", "--json"}, {"funnel", "kpi"}, {"funnel", "mcp-tools"}, {"funnel", "exclude", "list"},
	{"feedback", "list"}, {"params", "show", "allowance"},
	{"moderation", "queue"}, {"moderation", "show", "absent"}, {"moderation", "log"}, {"moderation", "stats"}, {"moderation", "alerts"}, {"moderation", "spend"}, {"moderation", "policy"},
	{"sanction", "list"}, {"sanction", "ladder"}, {"erase", "log"}, {"fetch", "denylist"},
	{"signals", "cluster"}, {"signals", "account", strings.Repeat("ab", 32)}, {"signals", "message", "absent"},
	{"feed", "replay"}, {"offering", "unknown"}, {"topup", "unknown"}, {"bonds"}, {"room", "lobby", "asset", "list"},
}

// TestOperatorReadsOpenReadOnly: every operator read opens the database
// read-only, runs without writing (no "readonly database" refusal, and the
// live connection's data_version never moves), and the writes keep a
// read-write open.
func TestOperatorReadsOpenReadOnly(t *testing.T) {
	for _, c := range readOnlyCommands {
		if !operatorReadOnly(c[0], c[1:]) {
			t.Errorf("%v should open read-only", c)
		}
	}
	for _, c := range [][]string{
		// integrity stays read-write: a schema-bump rollout runs the new binary's
		// integrity on a clone of the live snapshot so it migrates (rollout 350, 10-24).
		{"integrity"},
		{"params", "set", "allowance", "f.json"}, {"moderate", "ID", "hide", "spam"}, {"room", "lobby", "policy", "{}"}, {"room", "lobby", "asset", "put", "f"},
		{"funnel", "backfill"}, {"funnel", "recompute"}, {"funnel", "exclude", "add", "x"}, {"moderation", "approve", "x"}, {"moderation", "quality-backfill"},
		{"lever", "list"}, {"trust", "show", "x"}, {"maintenance"}, {"backup", "f"}, {"sanction", "apply"}, {"erase", "account", "x"}, {"erase", "account", "x", "--confirm"}, {"offering", "resolve", "x", "fail"}, {"bonds", "resolve", "x", "fail"},
		{"recover-generation", "--offline-confirmed"}, {"fetch", "deny", "h", "r"}, {"feedback"},
	} {
		if operatorReadOnly(c[0], c[1:]) {
			t.Errorf("%v writes; it must keep the read-write open", c)
		}
	}

	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	t.Setenv("MODERATION", "true")
	features, err := featuresFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "swarmmemo.db")
	live, err := board.Open(path, board.Config{ServiceID: "swarmmemo.com", Features: features, Moderation: moderationConfig()})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	// Something to read: two agents' posts, feedback, and the funnel with
	// one persona excluded (the reports refuse to print without one).
	ctx := t.Context()
	post := func(seed byte, text string) string {
		t.Helper()
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, 32))
		pub := key.Public().(ed25519.PublicKey)
		c := board.Command{Operation: "post", Room: "lobby", Text: text, RequestID: text, PublicKey: base64.RawURLEncoding.EncodeToString(pub), Timestamp: time.Now().Unix(), Nonce: text}
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
		if _, err := live.Execute(board.WithVia(ctx, "command"), c, "203.0.113.4"); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(pub)
		return hex.EncodeToString(sum[:])
	}
	persona := post(51, "release notes")
	post(52, "hello")
	if _, err = live.Execute(ctx, board.Command{Operation: "feedback", Target: "general", Text: "works", RequestID: "f1"}, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	if _, err = live.FunnelBackfill(ctx, time.Now().Unix()-86400); err != nil {
		t.Fatal(err)
	}
	if _, err = live.FunnelExclude(ctx, persona, true); err != nil {
		t.Fatal(err)
	}
	if _, err = live.FunnelRecompute(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(5000)"}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	conn, err := raw.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	version := func() (v int64) {
		if err := conn.QueryRowContext(t.Context(), "PRAGMA data_version").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := version()
	stdout, args := os.Stdout, os.Args
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	os.Stdout = devnull
	defer func() { os.Stdout, os.Args = stdout, args }()
	for _, c := range readOnlyCommands {
		os.Args = append([]string{"swarmmemo"}, c...)
		err := operator(c[0])
		// Only the absent queue item is refused; a write the read-only
		// open refused would fail here too.
		if absent := c[0] == "moderation" && c[1] == "show"; (err != nil) != absent || absent && !strings.Contains(err.Error(), "not found") {
			t.Errorf("%v: %v", c, err)
		}
	}
	if after := version(); after != before {
		t.Fatalf("operator reads changed the database (data_version %d -> %d)", before, after)
	}
}
