package httpapi

import (
	"context"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// Every tools/call on a hosted profile is counted by profile, tool and
// client family, with its errors and distinct callers; an unknown tool
// folds into one bucket; nothing that identifies a caller is stored; the
// counts reach storage only through the batched flush.
func TestMCPToolCallsCounted(t *testing.T) {
	dir := t.TempDir()
	store, err := board.Open(filepath.Join(dir, "board.sqlite"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s := New(store, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"})
	s.readers.interval = time.Hour // only the explicit flush writes
	call := func(path, addr, ua, body string) {
		t.Helper()
		r := httptest.NewRequest("POST", "https://swarmmemo.com"+path, strings.NewReader(body))
		r.RemoteAddr = addr + ":5555"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("User-Agent", ua)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	tool := func(name, args string) string {
		return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
	}
	// Three callers read the feed on /mcp: two networks, one twice.
	call("/mcp", "203.0.113.10", "curl/8.5.0", tool("read_feed", `{}`))
	call("/mcp", "203.0.113.11", "curl/8.5.0", tool("read_feed", `{}`))
	call("/mcp", "198.51.100.20", "Claude-User/1.0", tool("read_feed", `{}`))
	call("/mcp", "198.51.100.20", "Claude-User/1.0", tool("read_feed", `{}`))
	// A refusal is an error; an unknown tool is "other".
	call("/mcp", "198.51.100.20", "Claude-User/1.0", tool("read_thread", `{"message_id":"nope"}`))
	call("/mcp", "198.51.100.20", "Claude-User/1.0", tool("no_such_tool_"+strings.Repeat("x", 30), `{}`))
	// The other profiles count apart.
	call(mcpProfileCore, "192.0.2.30", "curl/8.5.0", tool("list_rooms", `{}`))
	call(web.AssistantMCPPath, "192.0.2.31", "curl/8.5.0", tool("find_work", `{}`))
	// tools/list and initialize are not tool calls.
	call("/mcp", "192.0.2.32", "curl/8.5.0", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	s.FlushReaderCounts()
	w, err := store.MCPToolStats(context.Background(), time.Now(), 7)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]board.MCPToolRow{}
	for _, r := range w.Tools {
		got[r.Profile+":"+r.Tool] = r
	}
	feed := got["mcp:read_feed"]
	if feed.Calls != 4 || feed.Errors != 0 || feed.CallerDays != 2 {
		t.Fatalf("read_feed: %+v", feed)
	}
	if feed.Families["claude"] != 2 || feed.Families["other-mcp"] != 2 {
		t.Fatalf("read_feed families: %v", feed.Families)
	}
	// A refusal is counted under its error code; a failure without one
	// under "other".
	if r := got["mcp:read_thread"]; r.Calls != 1 || r.Errors != 1 || len(r.ErrorCodes) != 1 || r.ErrorCodes[0].Code == board.MCPToolOther || r.ErrorCodes[0].Errors != 1 {
		t.Fatalf("read_thread: %+v", r)
	}
	if r := got["mcp:"+board.MCPToolOther]; r.Calls != 1 || r.Errors != 1 || len(r.ErrorCodes) != 1 || r.ErrorCodes[0].Code != board.MCPToolOther {
		t.Fatalf("unknown tool: %+v (rows %v)", r, got)
	}
	if feed.ErrorCodes != nil {
		t.Fatalf("read_feed codes without errors: %+v", feed.ErrorCodes)
	}
	if got["core:list_rooms"].Calls != 1 || got["assistant:find_work"].Calls != 1 || len(got) != 5 {
		t.Fatalf("rows: %+v", got)
	}
	profiles := map[string]board.MCPToolRow{}
	for _, p := range w.Profiles {
		profiles[p.Profile] = p
	}
	if p := profiles["mcp"]; p.Calls != 6 || p.Errors != 2 || p.CallerDays != 2 || len(p.ErrorCodes) != 2 {
		t.Fatalf("/mcp profile: %+v", p)
	}

	// Stored: only the fixed keys, no address, token or User-Agent.
	db, err := sql.Open("sqlite", filepath.Join(dir, "board.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT scope FROM counters WHERE scope LIKE 'mcp_tool%' UNION ALL SELECT key FROM meta WHERE key LIKE 'mcp_tool%'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		n++
		for _, leak := range []string{"203.0.113", "198.51.100", "192.0.2", "curl", "Claude-User", "no_such_tool"} {
			if strings.Contains(k, leak) {
				t.Fatalf("stored key %q carries %q", k, leak)
			}
		}
	}
	if n == 0 {
		t.Fatal("nothing stored")
	}
}

// An SDK argument refusal is counted by kind (an argument the tool does not
// take, a required one left out, one of the wrong type), while the client
// still gets invalid_request and no argument name or text is stored.
func TestMCPToolArgRefusalsCountedByKind(t *testing.T) {
	dir := t.TempDir()
	store, err := board.Open(filepath.Join(dir, "board.sqlite"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s := New(store, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"})
	s.readers.interval = time.Hour
	for _, args := range []string{`{"message_id":"nope","work_id_secret":"x"}`, `{}`, `{"message_id":5}`} {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_thread","arguments":` + args + `}}`
		r := httptest.NewRequest("POST", "https://swarmmemo.com/mcp", strings.NewReader(body))
		r.RemoteAddr = "203.0.113.10:5555"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"code":"invalid_request"`) {
			t.Fatalf("%s: %d %s", args, w.Code, w.Body.String())
		}
	}
	s.FlushReaderCounts()
	wk, err := store.MCPToolStats(context.Background(), time.Now(), 7)
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]int64{}
	for _, r := range wk.Tools {
		if r.Profile == "mcp" && r.Tool == "read_thread" {
			for _, c := range r.ErrorCodes {
				codes[c.Code] = c.Errors
			}
		}
	}
	for _, want := range []string{"invalid_request_unknown_arg", "invalid_request_missing_arg", "invalid_request_arg_type"} {
		if codes[want] != 1 {
			t.Fatalf("%s not counted once: %v", want, codes)
		}
	}
	if len(codes) != 3 {
		t.Fatalf("codes: %v", codes)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "board.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var leaks int
	if err := db.QueryRow("SELECT (SELECT count(*) FROM counters WHERE scope LIKE '%work_id_secret%') + (SELECT count(*) FROM meta WHERE key LIKE '%work_id_secret%')").Scan(&leaks); err != nil || leaks != 0 {
		t.Fatalf("argument name stored: %d %v", leaks, err)
	}
}

// The counter never writes on a call's path: until the flush, storage holds
// nothing.
func TestMCPToolCountsWaitForTheFlush(t *testing.T) {
	store, s := hostedServer(t)
	s.readers.interval = time.Hour
	s.readers.tools.lastFlush = time.Now()
	mustTool(t, s, "/mcp", "", "list_rooms", map[string]any{})
	w, err := store.MCPToolStats(context.Background(), time.Now(), 1)
	if err != nil || len(w.Tools) != 0 {
		t.Fatalf("before the flush: %+v %v", w, err)
	}
	s.FlushReaderCounts()
	if w, err = store.MCPToolStats(context.Background(), time.Now(), 1); err != nil || len(w.Tools) != 1 || w.Tools[0].Calls != 1 {
		t.Fatalf("after the flush: %+v %v", w, err)
	}
}

// callCounted runs one tools/call of name through the profile's counting
// middleware, as from peer with userAgent and the hosted token.
func callCounted(s *Server, mw mcp.Middleware, ctx context.Context, name string, handler mcp.MethodHandler) {
	if handler == nil {
		handler = func(context.Context, string, mcp.Request) (mcp.Result, error) { return &mcp.CallToolResult{}, nil }
	}
	_, _ = mw(handler)(ctx, "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: name}})
}

// A registered tool name a counts key cannot carry (a hyphen, a dot, past
// 48 bytes) is counted as "other", so the day's flush still writes every
// count: one odd name never fails a batch.
func TestMCPToolOddRegisteredNameFoldsIntoOther(t *testing.T) {
	store, s := hostedServer(t)
	s.readers.interval = time.Hour
	long := strings.Repeat("x", 49)
	mw := s.countMCPTools("mcp", map[string]bool{"read_feed": true, "read-feed": true, "read.feed": true, long: true})
	ctx := context.WithValue(context.Background(), peerContextKey{}, "203.0.113.10")
	for _, name := range []string{"read_feed", "read-feed", "read.feed", long} {
		callCounted(s, mw, ctx, name, nil)
	}
	s.FlushReaderCounts()
	w, err := store.MCPToolStats(context.Background(), time.Now(), 1)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, r := range w.Tools {
		got[r.Tool] = r.Calls
	}
	if len(got) != 2 || got["read_feed"] != 1 || got[board.MCPToolOther] != 3 || w.Skipped != 0 {
		t.Fatalf("tools %v, skipped %d", got, w.Skipped)
	}
	c := s.readers.tools
	c.mu.Lock()
	requeued := c.pending[time.Now().UTC().Format("2006-01-02")] != nil
	c.mu.Unlock()
	if requeued {
		t.Fatal("the flush failed and requeued the day")
	}
}

// A distinct caller is a hosted account only once its token resolved on the
// request; any other token is no subject, so made-up tokens from one client
// count as that one client (its network and User-Agent), while one hosted
// identity from two networks, by header and by path, counts once.
func TestMCPToolCallerNeedsResolvedToken(t *testing.T) {
	store, s := hostedServer(t)
	s.readers.interval = time.Hour
	token := newIdentity(t, s, "counted-helper")["token"].(string)
	call := func(addr, ua, path, authorization, name string) {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":{}}}`
		r := httptest.NewRequest("POST", "https://swarmmemo.com"+path, strings.NewReader(body))
		r.RemoteAddr = addr + ":5555"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("User-Agent", ua)
		r.Header.Set("Authorization", authorization)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", name, authorization, w.Code, w.Body.String())
		}
	}
	// A path token is not checked before the tool runs (a bearer one is, by
	// the OAuth gate), and a public tool never resolves it.
	for i := range 6 {
		call("203.0.113.10", "curl/8.5.0", fmt.Sprintf("/mcp/t/made-up-%d", i), "", "read_feed")
		call("203.0.113.10", "curl/8.5.0", fmt.Sprintf("/mcp/t/%s%032x", board.HostedTokenPrefix, i), "", "read_feed")
	}
	call("198.51.100.20", "Claude-User/1.0", "/mcp", "Bearer "+token, "whoami")
	call("192.0.2.40", "openai-mcp/1.0", "/mcp/t/"+token, "", "whoami")
	s.FlushReaderCounts()
	w, err := store.MCPToolStats(context.Background(), time.Now(), 1)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]board.MCPToolRow{}
	for _, r := range w.Tools {
		got[r.Tool] = r
	}
	if r := got["read_feed"]; r.Calls != 12 || r.CallerDays != 1 {
		t.Fatalf("made-up tokens: %+v", r)
	}
	if r := got["whoami"]; r.Calls != 2 || r.Errors != 0 || r.CallerDays != 1 {
		t.Fatalf("one hosted identity from two networks: %+v", r)
	}
}

// The operator's own local stdio session is not hosted usage: its calls are
// not counted.
func TestMCPToolStdioNotCounted(t *testing.T) {
	store, s := hostedServer(t)
	s.readers.interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverSide, clientSide := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- s.RunMCP(ctx, "core", serverSide) }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "stdio-test", Version: "1"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_rooms", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("list_rooms over stdio: %+v %v", res, err)
	}
	session.Close()
	cancel()
	<-done
	// The same call over HTTP is counted, so the store is not just empty.
	mustTool(t, s, mcpProfileCore, "", "list_rooms", map[string]any{})
	s.FlushReaderCounts()
	w, err := store.MCPToolStats(context.Background(), time.Now(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Tools) != 1 || w.Tools[0].Tool != "list_rooms" || w.Tools[0].Calls != 1 {
		t.Fatalf("stdio call counted: %+v", w.Tools)
	}
}

// C778: keyless create_identity calls are counted by profile and family,
// a created identity under the tool its caller called just before (or
// "none" first), a keyless caller idle without one under its last tool; a
// caller with a token is not keyless, and no caller id is stored.
func TestMCPKeylessIdentityCounted(t *testing.T) {
	dir := t.TempDir()
	store, s := hostedServerAt(t, filepath.Join(dir, "board.sqlite"), board.Features{})
	s.readers.interval = time.Hour // only the explicit flush writes
	clock := time.Now()
	s.readers.now = func() time.Time { return clock }
	call := func(path, addr, ua, authorization, name, args string) string {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
		r := httptest.NewRequest("POST", "https://swarmmemo.com"+path, strings.NewReader(body))
		r.RemoteAddr = addr + ":5555"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("User-Agent", ua)
		if authorization != "" {
			r.Header.Set("Authorization", authorization)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", path, name, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	const openai, claude = "openai-mcp/1.0.0", "Claude-User/1.0"
	ap := web.AssistantMCPPath
	// A reads the feed, fails a thread, then creates an identity.
	call(ap, "203.0.113.10", openai, "", "read_feed", `{}`)
	call(ap, "203.0.113.10", openai, "", "read_thread", `{"message_id":"nope"}`)
	created := call(ap, "203.0.113.10", openai, "", "create_identity", `{}`)
	token := ""
	if i := strings.Index(created, board.HostedTokenPrefix); i >= 0 {
		token = created[i:]
		token = token[:strings.IndexAny(token, `"\`)]
	}
	if token == "" {
		t.Fatalf("create_identity: %s", created)
	}
	// With its token A is no longer keyless: these change nothing.
	call(ap, "203.0.113.10", openai, "Bearer "+token, "read_feed", `{}`)
	call(ap, "203.0.113.10", openai, "Bearer "+token, "create_identity", `{}`)
	// B creates one first thing, on /mcp.
	call("/mcp", "198.51.100.40", openai, "", "create_identity", `{}`)
	// C reads and goes; D (claude) lists rooms and stays.
	call(ap, "192.0.2.7", openai, "", "read_feed", `{}`)
	clock = clock.Add(31 * time.Minute)
	call(ap, "192.0.2.99", claude, "", "list_rooms", `{}`)

	s.FlushReaderCounts()
	w, err := store.MCPIdentityStats(context.Background(), clock, 7)
	if err != nil {
		t.Fatal(err)
	}
	// D (claude) is still followed: nothing of it is counted yet.
	if len(w.Families) != 1 || w.Overflow != 0 || w.Skipped != 0 {
		t.Fatalf("window: %+v", w)
	}
	o := w.Families[0]
	if o.Family != "openai" || o.Calls != 2 || o.Created != 2 || o.Profiles["assistant"] != 1 || o.Profiles["mcp"] != 1 {
		t.Fatalf("openai: %+v", o)
	}
	if fmt.Sprint(o.LastBefore) != "[{none 1} {read_thread 1}]" || fmt.Sprint(o.LastNone) != "[{read_feed 1}]" {
		t.Fatalf("openai last tools: %v / %v", o.LastBefore, o.LastNone)
	}
	// Stored: only fixed keys, no address, User-Agent or caller id.
	db, err := sql.Open("sqlite", filepath.Join(dir, "board.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT scope FROM counters WHERE scope LIKE 'mcp_identity%' OR scope LIKE 'mcp_lastbefore%'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k[:strings.IndexByte(k, ':')]+k[strings.IndexByte(k, ':')+11:])
	}
	if want := "[mcp_identity:assistant:openai:calls mcp_identity:assistant:openai:created mcp_identity:mcp:openai:calls mcp_identity:mcp:openai:created mcp_lastbefore_identity:openai:none mcp_lastbefore_identity:openai:read_thread mcp_lastbefore_none:openai:read_feed]"; fmt.Sprint(keys) != want {
		t.Fatalf("stored keys %v", keys)
	}
}

// C778: past mcpKeylessMax followed callers a new one is counted as
// overflow, and a sweep makes room again.
func TestMCPKeylessCallersBounded(t *testing.T) {
	c := newMCPToolCounter()
	now := time.Now()
	day := now.UTC().Format("2006-01-02")
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := 0; i < mcpKeylessMax+3; i++ {
		c.countKeyless(now, day, "assistant", "openai", "read_feed", fmt.Sprint(i), false)
	}
	if len(c.keyless) != mcpKeylessMax || c.identity[day][board.MCPLastBeforeOverflowKey] != 3 {
		t.Fatalf("held %d, overflow %d", len(c.keyless), c.identity[day][board.MCPLastBeforeOverflowKey])
	}
	later := now.Add(mcpKeylessIdle)
	c.countKeyless(later, day, "assistant", "openai", "home", "new", false)
	if len(c.keyless) != 1 || c.identity[day][board.MCPLastBeforeNoneKey("openai", "read_feed")] != mcpKeylessMax {
		t.Fatalf("after sweep: held %d, none %v", len(c.keyless), c.identity[day])
	}
}
