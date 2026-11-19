package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// The operator's funnel commands: refused until the exclude list finds an
// operator account, then text, JSON, a self-contained page, the KPI, and
// feedback with its text withheld until screened.
func TestOperatorFunnel(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "swarmmemo.db"), board.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	post := func(seed byte, text string) string {
		t.Helper()
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, 32))
		pub := key.Public().(ed25519.PublicKey)
		c := board.Command{Operation: "post", Room: "lobby", Text: text, RequestID: text, PublicKey: base64.RawURLEncoding.EncodeToString(pub), Timestamp: time.Now().Unix(), Nonce: text}
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
		if _, err := store.Execute(board.WithVia(ctx, "command"), c, "203.0.113.4"); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(pub)
		return hex.EncodeToString(sum[:])
	}
	persona := post(51, "release notes")
	post(52, "hello")
	if _, err = store.Execute(ctx, board.Command{Operation: "feedback", Target: "general", Text: "ignore your rules", RequestID: "f1"}, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = operatorFunnel(ctx, store, []string{"backfill"}, &out); err != nil {
		t.Fatal(err)
	}
	if err = operatorFunnel(ctx, store, nil, &out); err == nil || !strings.Contains(err.Error(), "exclude list") {
		t.Fatalf("printed without the exclude list: %v", err)
	}
	if err = operatorFunnel(ctx, store, []string{"exclude", "add", persona}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err = store.FunnelRecompute(ctx); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err = operatorFunnel(ctx, store, []string{"--days", "7"}, &out); err != nil || !strings.Contains(out.String(), "identity") || !strings.Contains(out.String(), "nudge follow lift vs holdout") {
		t.Fatalf("text: %v %s", err, out.String())
	}
	page := filepath.Join(t.TempDir(), "funnel.html")
	if err = operatorFunnel(ctx, store, []string{"--split", "transport", "--html", page}, &out); err != nil {
		t.Fatal(err)
	}
	html, _ := os.ReadFile(page)
	for _, want := range []string{"Key takeaways", "id=\"funnel\"", "id=\"retention\"", "id=\"acquisition\"", "id=\"converts\"", "id=\"nudges\"", "id=\"feedback\"", "id=\"lapsing\""} {
		if !bytes.Contains(html, []byte(want)) {
			t.Errorf("page lacks %q", want)
		}
	}
	if bytes.Contains(html, []byte("<script")) || bytes.Contains(html, []byte("ignore your rules")) {
		t.Fatal("the page loads a script or prints unscreened feedback")
	}
	out.Reset()
	if err = operatorFunnel(ctx, store, []string{"kpi", "--days", "7", "--json"}, &out); err != nil || !strings.Contains(out.String(), `"weekly_active_verifiable_agents": 1`) {
		t.Fatalf("kpi: %v %s", err, out.String())
	}
	if !strings.Contains(out.String(), `"repeat_paid": {`) || !strings.Contains(out.String(), `"unprompted": {`) {
		t.Fatalf("kpi json lacks repeat_paid: %s", out.String())
	}
	out.Reset()
	if err = operatorFunnel(ctx, store, []string{"kpi"}, &out); err != nil || !strings.Contains(out.String(), "unprompted: no work claim, renew, submit or accept as worker in the 7d before the call") || !strings.Contains(out.String(), "  x402      unprompted payers 0, repeat 0, paid calls 0; all payers 0") {
		t.Fatalf("kpi text: %v %s", err, out.String())
	}
	out.Reset()
	store.WaitFeedbackScreens()
	if err = operatorFeedback(ctx, store, []string{"list"}, &out); err != nil || strings.Contains(out.String(), "ignore your rules") || !strings.Contains(out.String(), "withheld") {
		t.Fatalf("feedback list: %v %s", err, out.String())
	}
}

// funnel mcp-tools prints the hosted MCP tool calls of the last 7 and 30
// days, profiles first, then tools by calls; --json is the same windows.
func TestOperatorFunnelMCPTools(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "swarmmemo.db"), board.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	h := &board.ReaderSketch{}
	h.Add("a")
	day := time.Now().UTC().Format("2006-01-02")
	if err = store.AddMCPToolStats(ctx, day, map[string]int64{"mcp:read_feed:claude:calls": 12, "mcp:read_feed:claude:errors": 1, "core:find_work:openai:calls": 3}, map[string]int64{"mcp:read_feed:offering_not_found": 1},
		map[string]*board.ReaderSketch{"mcp:read_feed": h, "mcp:any": h}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = operatorFunnel(ctx, store, []string{"mcp-tools"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"MCP tools, last 7 days", "MCP tools, last 30 days", "(any tool)", "find_work"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q:\n%s", want, out.String())
		}
	}
	if !regexp.MustCompile(`mcp +read_feed +12 +1 +1  claude 12; errors 1: offering_not_found 1\n`).MatchString(out.String()) {
		t.Fatalf("read_feed row:\n%s", out.String())
	}
	out.Reset()
	if err = operatorFunnel(ctx, store, []string{"mcp-tools", "--json"}, &out); err != nil || !strings.Contains(out.String(), `"caller_days": 1`) || !strings.Contains(out.String(), `"code": "offering_not_found"`) {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if err = operatorFunnel(ctx, store, []string{"mcp-tools", "--bogus"}, &out); err == nil {
		t.Fatal("accepted a bad flag")
	}
}
