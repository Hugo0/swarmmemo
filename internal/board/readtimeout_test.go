package board

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestReadTimeoutLogsOneLine: a work read past its budget answers 503
// work_read_timeout and logs one WARN line with the read's shape, the
// signer's account cut to 12 hex and the elapsed ms; never text or a full
// key; a second timeout within the window is held back and counted on the
// next line (C700).
func TestReadTimeoutLogsOneLine(t *testing.T) {
	s := openTest(t, Config{})
	var lines []string
	s.readTimeouts.warn = func(msg string, args ...any) {
		var b strings.Builder
		slog.New(slog.NewTextHandler(&b, nil)).Warn(msg, args...)
		lines = append(lines, b.String())
	}
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	expired, cancel := context.WithDeadline(testContext, time.Now().Add(-time.Second))
	defer cancel()
	account := keyID(keyFor(7))
	a := actor{id: account, account: account, signed: true}
	c := Command{Operation: "works.list", Kind: "open", Limit: 10, Cursor: "c", Query: "secret words",
		Data: `{"schema":1,"worker":"` + account + `","claimable":true,"summary":true}`}
	_, err = s.readWork(expired, tx, c, a, testTime)
	var e *Error
	if !errors.As(err, &e) || e.Code != "work_read_timeout" || e.Status != 503 {
		t.Fatalf("err = %v, want 503 work_read_timeout", err)
	}
	if len(lines) != 1 {
		t.Fatalf("lines = %q, want one", lines)
	}
	line := lines[0]
	for _, want := range []string{"level=WARN", `msg="read timeout"`, "operation=works.list", "signer=" + account[:12], "elapsed_ms=",
		"kind=open", "limit=10", "has_cursor=true", "worker=" + account[:12], `eligible_for=""`, `standing=""`, "claimable=true", "summary=true", "has_query=true"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q lacks %q", line, want)
		}
	}
	if strings.Contains(line, account) || strings.Contains(line, "secret") {
		t.Errorf("line %q carries a full key or query text", line)
	}
	// Held back within the window; anonymous agent.posts logs its own line.
	if _, err = s.readWork(expired, tx, c, a, testTime); !errors.As(err, &e) || e.Code != "work_read_timeout" {
		t.Fatalf("second err = %v", err)
	}
	if _, err = s.readAgentPosts(expired, tx, Command{Operation: "agent.posts", Target: "@poster"}, actor{}, testTime); !errors.As(err, &e) || e.Code != "agent_posts_timeout" {
		t.Fatalf("agent.posts err = %v", err)
	}
	if len(lines) != 2 || !strings.Contains(lines[1], "operation=agent.posts signer=anon") || !strings.Contains(lines[1], "target=poster") {
		t.Fatalf("lines = %q, want the agent.posts line second", lines)
	}
	s.readTimeouts.last["works.list"] = time.Now().Add(-readTimeoutLogEvery)
	if _, err = s.readWork(expired, tx, c, a, testTime); err == nil {
		t.Fatal("works.list under an expired budget succeeded")
	}
	if _, err = s.readWork(expired, tx, Command{Operation: "work.get", MessageID: "nope"}, actor{}, testTime); err == nil {
		t.Fatal("work.get under an expired budget succeeded")
	}
	if len(lines) != 4 || !strings.Contains(lines[2], "held_back=1") || !strings.Contains(lines[3], "operation=work.get") {
		t.Fatalf("lines = %q", lines)
	}
}
