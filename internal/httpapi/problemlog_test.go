package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"swarmmemo/internal/board"
)

func TestCommandProblemLineFormat(t *testing.T) {
	key := "ed25519:" + strings.Repeat("ab", 32)
	line := commandProblemLine("message.get", "", "not_found", 404, "command", problemActor(key), "", 0)
	want := regexp.MustCompile(`^command_problem op=message\.get code=not_found status=404 via=command actor=[0-9a-f]{8}$`)
	if !want.MatchString(line) {
		t.Fatalf("line = %q", line)
	}
	if strings.Contains(line, key) || strings.Contains(line, "ab"+"abab") {
		t.Fatalf("line carries the key: %q", line)
	}
	if got := commandProblemLine("post", "", "rate_limited", 429, "mcp", problemActor(""), "", 3); got != "command_problem op=post code=rate_limited status=429 via=mcp actor=anon suppressed=3" {
		t.Fatalf("line = %q", got)
	}
	// Anything that is not a short fixed-table token (text, an IP, a
	// header value) never reaches the line.
	for _, bad := range []string{"hello world", "203.0.113.7:443", "Bearer abc", "a=b", strings.Repeat("x", 65), "Ünï"} {
		line := commandProblemLine(bad, bad, bad, 400, bad, "anon", "", 0)
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
	k := problemKey{"post", "not_found", ""}
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
	if ok, _ := l.admit(problemKey{"post", "conflict", ""}, now.Add(2*time.Second)); !ok {
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

// captureProblemLines collects the command_problem lines f writes.
func captureProblemLines(t *testing.T, f func()) []string {
	t.Helper()
	var lines []string
	old := problemLogWrite
	problemLogWrite = func(line string) { lines = append(lines, line) }
	defer func() { problemLogWrite = old }()
	f()
	return lines
}

// C759: an MCP call to a tool we do not serve is 404 unknown_tool with the
// name it sent as tried=, not a 500; a failure with no status is
// status=unknown; a real 500 stays a 500.
func TestMCPProblemUnknownToolAndStatus(t *testing.T) {
	_, s := hostedServer(t)
	s.readers.interval = time.Hour
	mw := s.countMCPTools("mcp", map[string]bool{"read_feed": true})
	ctx := context.Background()
	unknown := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "unknown tool"}
	}
	lines := captureProblemLines(t, func() {
		callCounted(s, mw, ctx, "Get_Messages-c759", unknown)
		callCounted(s, mw, ctx, "swarmmemo:home-c867", unknown)
		callCounted(s, mw, ctx, "read_feed", func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
			dispatchNoteFrom(ctx).tried = "swarmmemo/find_work-c867" // C867: call_swarmmemo_tool's unknown name
			return refusalResult(&board.Error{Status: 404, Code: "unknown_tool", Message: "x"}), nil
		})
		callCounted(s, mw, ctx, "read_feed", func(context.Context, string, mcp.Request) (mcp.Result, error) {
			return nil, errors.New("boom")
		})
		callCounted(s, mw, ctx, "read_feed", func(context.Context, string, mcp.Request) (mcp.Result, error) {
			return refusalResult(&board.Error{Status: 500, Code: "internal_c759", Message: "x"}), nil
		})
	})
	want := []string{
		"command_problem op=other tried=get_messages-c759 code=unknown_tool status=404 via=mcp actor=anon",
		"command_problem op=other tried=swarmmemo.home-c867 code=unknown_tool status=404 via=mcp actor=anon",
		"command_problem op=other tried=swarmmemo.find_work-c867 code=unknown_tool status=404 via=mcp actor=anon",
		"command_problem op=read_feed code=other status=unknown via=mcp actor=anon",
		"command_problem op=read_feed code=internal_c759 status=500 via=mcp actor=anon",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// tried= is only a short token: spaces, non-ASCII capitals, over 48 bytes,
// a leading separator drop it; it never shows beside a known op.
func TestCommandProblemTried(t *testing.T) {
	for in, want := range map[string]string{
		"message.list": "message.list", "Read_Feed": "read_feed", strings.Repeat("a", 48): strings.Repeat("a", 48),
		"get messages": "", "\u212Aelvin": "", "Ünï": "", strings.Repeat("a", 49): "", "-x": "", "": "", "a=b": "", "1.2.3.4:80": "",
	} {
		if got := problemTried(in); got != want {
			t.Errorf("problemTried(%q) = %q, want %q", in, got, want)
		}
		line := commandProblemLine("unknown", in, "unknown_operation", 400, "command", "anon", "", 0)
		if want == "" && strings.Contains(line, "tried=") {
			t.Errorf("line keeps %q: %q", in, line)
		}
	}
	if line := commandProblemLine("post", "post", "conflict", 409, "command", "anon", "", 0); strings.Contains(line, "tried=") {
		t.Fatalf("tried beside a known op: %q", line)
	}
	// /v1/command with an unknown operation notes what it tried.
	rec := httptest.NewRecorder()
	var got *problemRecorder
	_, s := hostedServer(t)
	body := strings.NewReader(`{"operation":"Message.Fetch"}`)
	withProblemLog(rec, httptest.NewRequest("POST", "/v1/command", body), "command", func(w http.ResponseWriter, r *http.Request) {
		s.command(w, r)
		got = findProblemRecorder(w)
	})
	if got == nil || got.op != "" || got.tried != "Message.Fetch" || got.status != 400 {
		t.Fatalf("recorder = %+v", got)
	}
}

// The limiter keys on tried too, and its table stays bounded.
func TestCommandProblemLimiterTriedBounded(t *testing.T) {
	var l problemLimiter
	now := time.Unix(1_800_000_000, 0)
	if ok, _ := l.admit(problemKey{"other", "unknown_tool", "a"}, now); !ok {
		t.Fatal("first")
	}
	if ok, _ := l.admit(problemKey{"other", "unknown_tool", "b"}, now); !ok {
		t.Fatal("another tried has its own slot")
	}
	for i := 0; i < 3*problemLogKeys; i++ {
		l.admit(problemKey{"other", "unknown_tool", strings.Repeat("x", i%40) + string(rune('a'+i%26)) + time.Duration(i).String()}, now)
	}
	if len(l.slots) > problemLogKeys {
		t.Fatalf("table %d > %d", len(l.slots), problemLogKeys)
	}
}

// C836: a work.* operation's line names the work hashed (work=HEX8), never
// the id itself; any other operation's line has no work=.
func TestCommandProblemWorkHash(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	lines := captureProblemLines(t, func() {
		logCommandProblem("work.claim", "", "seats_full_c836", 409, "command", "", id)
		logCommandProblem("message.get", "", "not_found_c836", 404, "command", "", id)
		logCommandProblem("work.claim", "", "work_none_c836", 409, "command", "", "")
	})
	want := []string{
		"command_problem op=work.claim code=seats_full_c836 status=409 via=command actor=anon work=" + problemActor(id),
		"command_problem op=message.get code=not_found_c836 status=404 via=command actor=anon",
		"command_problem op=work.claim code=work_none_c836 status=409 via=command actor=anon",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") || strings.Contains(strings.Join(lines, ""), id) {
		t.Fatalf("lines =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// C861: a failed service.call or service.read names its service as
// tried=, so the operator can tell which service an error clusters on;
// other known operations still carry no tried=.
func TestCommandProblemServiceTried(t *testing.T) {
	lines := captureProblemLines(t, func() {
		logCommandProblem("service.call", "notary", "invalid_service_data_c861", 400, "command", "", "")
		logCommandProblem("service.read", "Memory", "not_found_c861", 404, "command", "", "")
		logCommandProblem("service.call", "bad service", "invalid_service_data_c861b", 400, "command", "", "")
		logCommandProblem("post", "notary", "conflict_c861", 409, "command", "", "")
	})
	want := []string{
		"command_problem op=service.call tried=notary code=invalid_service_data_c861 status=400 via=command actor=anon",
		"command_problem op=service.read tried=memory code=not_found_c861 status=404 via=command actor=anon",
		"command_problem op=service.call code=invalid_service_data_c861b status=400 via=command actor=anon",
		"command_problem op=post code=conflict_c861 status=409 via=command actor=anon",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// C863: a tools.call names the tool it routed to as tried=.
func TestCommandProblemToolTried(t *testing.T) {
	if got := problemToolID(`{"schema":1,"method":"call","args":{"id":"swarmmemo:fetch.page","args":{}}}`); got != "swarmmemo.fetch.page" {
		t.Fatalf("problemToolID = %q", got)
	}
	if got := problemToolID(`not json`); got != "" {
		t.Fatalf("problemToolID(bad) = %q", got)
	}
	lines := captureProblemLines(t, func() {
		logCommandProblem("tools.call", "swarmmemo.fetch.page", "global_quota_exhausted_c863", 429, "call", "", "")
	})
	if want := "command_problem op=tools.call tried=swarmmemo.fetch.page code=global_quota_exhausted_c863 status=429 via=call actor=anon"; len(lines) != 1 || lines[0] != want {
		t.Fatalf("lines = %q", lines)
	}
}
