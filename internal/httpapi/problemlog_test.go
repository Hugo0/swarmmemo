package httpapi

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

func TestCommandProblemLineFormat(t *testing.T) {
	key := "ed25519:" + strings.Repeat("ab", 32)
	line := commandProblemLine("message.get", "not_found", 404, "command", problemActor(key), 0)
	want := regexp.MustCompile(`^command_problem op=message\.get code=not_found status=404 via=command actor=[0-9a-f]{8}$`)
	if !want.MatchString(line) {
		t.Fatalf("line = %q", line)
	}
	if strings.Contains(line, key) || strings.Contains(line, "ab"+"abab") {
		t.Fatalf("line carries the key: %q", line)
	}
	if got := commandProblemLine("post", "rate_limited", 429, "mcp", problemActor(""), 3); got != "command_problem op=post code=rate_limited status=429 via=mcp actor=anon suppressed=3" {
		t.Fatalf("line = %q", got)
	}
	// Anything that is not a short fixed-table token (text, an IP, a
	// header value) never reaches the line.
	for _, bad := range []string{"hello world", "203.0.113.7:443", "Bearer abc", "a=b", strings.Repeat("x", 65), "Ünï"} {
		line := commandProblemLine(bad, bad, 400, bad, "anon", 0)
		if strings.Contains(line, bad) || strings.Contains(line, "203.0.113") {
			t.Fatalf("line keeps %q: %q", bad, line)
		}
		if strings.Count(line, "=other") != 3 {
			t.Fatalf("line = %q", line)
		}
	}
}

func TestCommandProblemLimiter(t *testing.T) {
	var l problemLimiter
	now := time.Unix(1_800_000_000, 0)
	k := problemKey{"post", "not_found"}
	if ok, n := l.admit(k, now); !ok || n != 0 {
		t.Fatalf("first = %v %d", ok, n)
	}
	if ok, _ := l.admit(k, now.Add(time.Second)); ok {
		t.Fatal("second within 10 s was written")
	}
	if ok, _ := l.admit(k, now.Add(9*time.Second)); ok {
		t.Fatal("third within 10 s was written")
	}
	// Another (op, code) has its own slot.
	if ok, _ := l.admit(problemKey{"post", "conflict"}, now.Add(2*time.Second)); !ok {
		t.Fatal("other key suppressed")
	}
	if ok, n := l.admit(k, now.Add(10*time.Second)); !ok || n != 2 {
		t.Fatalf("after 10 s = %v suppressed=%d, want true 2", ok, n)
	}
	if ok, n := l.admit(k, now.Add(21*time.Second)); !ok || n != 0 {
		t.Fatalf("count not reset: %v %d", ok, n)
	}
}

func TestProblemRecorderNotesWrittenProblem(t *testing.T) {
	rec := httptest.NewRecorder()
	var got *problemRecorder
	withProblemLog(rec, httptest.NewRequest("POST", "/v1/command", nil), "command", func(w http.ResponseWriter, r *http.Request) {
		noteCommand(w, "message.get", "")
		writeError(&indentWriter{ResponseWriter: w}, &board.Error{Status: 404, Code: "not_found", Message: "No such message."})
		got = findProblemRecorder(w)
	})
	if got == nil || got.op != "message.get" || got.code != "not_found" || got.status != 404 || got.via != "command" || got.actor != "" {
		t.Fatalf("recorder = %+v", got)
	}
}
