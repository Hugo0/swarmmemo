package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

func TestClientFamilyClassification(t *testing.T) {
	for _, tc := range []struct{ ua, want string }{
		{"Cursor/1.4.2 (darwin arm64)", "cursor-grok"},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Cursor/0.48.9 Chrome/132.0.6834.210 Electron/34.3.4 Safari/537.36", "cursor-grok"},
		{"Grok-Bot/1.0 (+https://x.ai)", "cursor-grok"},
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ChatGPT-User/1.0; +https://openai.com/bot)", "openai"},
		{"openai-mcp/1.0.0", "openai"},
		{"codex_cli_rs/0.36.0 (Mac OS 15.6.1; arm64) ghostty/1.1.3", "openai"},
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; GPTBot/1.2; +https://openai.com/gptbot)", "crawlers"},
		{"Mozilla/5.0 (compatible; OAI-SearchBot/1.0; +https://openai.com/searchbot)", "crawlers"},
		{"meta-externalfetcher/1.1 (+https://developers.facebook.com/docs/sharing/webmasters/crawler)", "meta-muse"},
		{"Meta-Muse/0.9", "meta-muse"},
		{"meta-externalagent/1.1 (+https://developers.facebook.com/docs/sharing/webmasters/crawler)", "crawlers"},
		{"claude-code/1.0.83 (external, cli)", "claude"},
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Claude-User/1.0; +Claude-User@anthropic.com)", "claude"},
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ClaudeBot/1.0; +claudebot@anthropic.com)", "crawlers"},
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Claude-SearchBot/1.0; +https://www.anthropic.com)", "crawlers"},
		{"GeminiCLI/0.1.5 (linux; x64)", "gemini"},
		{"gemini-cli/0.1.5", "gemini"},
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Perplexity-User/1.0; +https://perplexity.ai/perplexity-user)", "perplexity"},
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; PerplexityBot/1.0; +https://perplexity.ai/perplexitybot)", "crawlers"},
		{"curl/8.5.0", "scripts"},
		{"python-httpx/0.27.0", "scripts"},
		{"python-requests/2.32.3", "scripts"},
		{"node", "scripts"},
		{"Go-http-client/1.1", "scripts"},
		{"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0 Safari/537.36", "browsers"},
		{"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", "crawlers"},
		{"SomeCrawler/2.0", "crawlers"},
		{"precursor-tool/1.0", "other"},
		{"museum-guide/1.0", "other"},
		{"", "other"},
		{strings.Repeat("x", 600) + " claude-code/1.0", "other"}, // past the bounded prefix
	} {
		if got := userAgentClient(tc.ua); got != tc.want {
			t.Errorf("userAgentClient(%q) = %q, want %q", tc.ua, got, tc.want)
		}
	}
	for _, tc := range []struct{ name, ua, family, unknown string }{
		{"Cursor", "node", "cursor-grok", ""},
		{"cursor-vscode", "", "cursor-grok", ""},
		{"grok", "", "cursor-grok", ""},
		{"openai-mcp", "", "openai", ""},
		{"ChatGPT", "", "openai", ""},
		{"claude-code", "", "claude", ""},
		{"claude-ai", "", "claude", ""},
		{"Claude-User", "", "claude", ""},
		{"gemini-cli-mcp-client", "", "gemini", ""},
		{"perplexity", "", "perplexity", ""},
		{"Meta Muse", "", "meta-muse", ""},
		{"Zeta Agent/2", "python-httpx/0.27.0", "other-mcp", "zeta-agent-2"},
		{"mcp-remote", "claude-code/1.0.83", "claude", "mcp-remote"},
		{"", "curl/8.5.0", "other-mcp", ""},
		{strings.Repeat("Q", 100), "", "other-mcp", strings.Repeat("q", board.ClientNameMaxChars)},
	} {
		r := httptest.NewRequest("POST", "/mcp", nil)
		r.Header.Set("User-Agent", tc.ua)
		family, unknown := mcpClient(r, tc.name)
		if family != tc.family || unknown != tc.unknown {
			t.Errorf("mcpClient(%q, UA %q) = %q, %q; want %q, %q", tc.name, tc.ua, family, unknown, tc.family, tc.unknown)
		}
	}
	// At /mcp, a request no assistant names is an MCP client, not a script.
	for ua, want := range map[string]string{"node": "other-mcp", "curl/8.5.0": "other-mcp", "claude-code/1.0": "claude"} {
		r := httptest.NewRequest("POST", "/mcp", nil)
		r.Header.Set("User-Agent", ua)
		if got := requestClient(r); got != want {
			t.Errorf("requestClient(/mcp, %q) = %q, want %q", ua, got, want)
		}
	}
	// Every family the table can produce is one the store accepts.
	known := map[string]bool{}
	for _, f := range board.ClientFamilies {
		known[f] = true
	}
	for _, tok := range clientTokens {
		if !known[tok.family] {
			t.Errorf("token %q maps to unknown family %q", tok.token, tok.family)
		}
	}
	for _, f := range uaFamilyClients {
		if !known[f] {
			t.Errorf("uaFamilyClients maps to unknown family %q", f)
		}
	}
}

type clientsDaily struct {
	Daily []struct {
		Day     string `json:"day"`
		Clients struct {
			Families map[string]map[string]json.RawMessage `json:"families"`
			Unknown  int64                                 `json:"unknown_mcp_clients"`
		} `json:"clients"`
	} `json:"daily"`
}

func TestArrivalsByClientEndToEnd(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "clients.db")
	store, err := board.Open(dbPath, board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := New(store, web.Handler(store), Config{ServiceID: "swarmmemo.com"})
	send := func(method, path, ua, addr, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = addr + ":4444"
		r.Header.Set("User-Agent", ua)
		if method == "POST" {
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Accept", "application/json, text/event-stream")
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		// /api/services answers 404 while no service is enabled; the request
		// to the discovery surface still counts.
		if w.Code >= 400 && path != "/api/services" {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w
	}
	const chrome = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0 Safari/537.36"
	send("GET", "/llms.txt", "claude-code/1.0.83 (external, cli)", "203.0.113.10", "")
	send("GET", "/capabilities", "curl/8.5.0", "203.0.113.11", "")
	send("GET", "/api/services", "python-httpx/0.27.0", "203.0.113.12", "")
	send("GET", "/llms.txt", "Mozilla/5.0 (compatible; GPTBot/1.2; +https://openai.com/gptbot)", "203.0.113.13", "")
	send("GET", "/for-agents", chrome, "203.0.113.14", "")
	send("HEAD", "/llms.txt", "curl/8.5.0", "203.0.113.11", "") // not a discovery read
	initialize := func(name, ua, addr string) {
		send("POST", "/mcp", ua, addr, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"`+name+`","version":"1.2.3"}}}`)
	}
	initialize("Cursor", "node", "203.0.113.20")
	initialize("openai-mcp", "", "203.0.113.21")
	initialize("Zeta Agent", "node", "203.0.113.22")
	initialize("Zeta Agent", "node", "198.51.100.22")
	initialize("solo-agent", "node", "203.0.113.23")
	// A hosted MCP tool call: an MCP client, anonymous, posting for the first time.
	send("POST", "/mcp", "node", "203.0.113.24", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"post_message","arguments":{"text":"hello from an mcp client"}}}`)
	// A script on another network (anonymous callers are told apart by their
	// /24 or /64, and each is counted once a day).
	send("POST", "/w/lobby/main", "python-requests/2.32.3", "192.0.2.25", `{"text":"hello from a script"}`)
	send("POST", "/w/lobby/main", "python-requests/2.32.3", "192.0.2.26", `{"text":"again from a script"}`)

	daily := func(days int) clientsDaily {
		t.Helper()
		w := makeRequest(s, "GET", "/api/stats/daily?days="+strconv.Itoa(days), "", "")
		var body clientsDaily
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Daily) != days {
			t.Fatalf("daily: %d %s", w.Code, w.Body.String())
		}
		return body
	}
	s.FlushReaderCounts()
	// Today: only discovery and MCP initializes, and the unknown names counted.
	today := daily(1).Daily[0]
	want := map[string]map[string]int64{
		"claude":      {"discovery": 1},
		"scripts":     {"discovery": 2},
		"crawlers":    {"discovery": 1},
		"browsers":    {"discovery": 1},
		"cursor-grok": {"mcp_initialize": 1},
		"openai":      {"mcp_initialize": 1},
		"other-mcp":   {"mcp_initialize": 3},
	}
	if len(today.Clients.Families) != len(want) || today.Clients.Unknown != 2 {
		t.Fatalf("today: %+v", today.Clients)
	}
	for family, metrics := range want {
		got := today.Clients.Families[family]
		if len(got) != len(metrics) {
			t.Fatalf("%s publishes %v today", family, got)
		}
		for m, n := range metrics {
			if string(got[m]) != strconv.FormatInt(n, 10) {
				t.Fatalf("%s.%s = %s, want %d", family, m, got[m], n)
			}
		}
	}
	// The commands were counted all the same, by the family of the request
	// that carried them (a hosted MCP tool call by its User-Agent).
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for scope, n := range map[string]int64{"scripts:anonymous_subjects": 1, "scripts:first_posts": 1, "other-mcp:anonymous_subjects": 1, "other-mcp:first_posts": 1} {
		var got int64
		_ = raw.QueryRow("SELECT value FROM counters WHERE scope=?", "client:"+today.Day+":family:"+scope).Scan(&got)
		if got != n {
			t.Fatalf("%s = %d, want %d", scope, got, n)
		}
	}
	// A closed day publishes its command metrics, each only from 3.
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	if err = store.AddClientCounts(context.Background(), yesterday, map[string]int64{
		"family:claude:new_keys": 4, "family:claude:first_posts": 2, "family:claude:service_calls": 3,
		"family:claude:service:inference": 3, "family:claude:service:echo": 1, "family:claude:discovery": 1,
	}); err != nil {
		t.Fatal(err)
	}
	closed := daily(2).Daily[0]
	if closed.Day != yesterday || fmt.Sprint(closed.Clients.Families) != fmt.Sprint(map[string]map[string]json.RawMessage{"claude": {
		"new_keys": json.RawMessage("4"), "service_calls": json.RawMessage("3"), "discovery": json.RawMessage("1"), "services": json.RawMessage(`{"inference":3}`),
	}}) {
		t.Fatalf("closed day: %+v", closed.Clients.Families)
	}

	// UI/API parity: the /stats table is the API's published clients summed
	// over the page's days.
	week := daily(web.ClientStatsDays)
	page := getHTML(t, s, "/stats")
	if !strings.Contains(page, "Arrivals by client") || !strings.Contains(page, "Unrecognized MCP client names, counted per day and never published: 2.") || strings.Contains(page, "zeta-agent") || strings.Contains(page, "solo-agent") {
		t.Fatal("/stats does not show the arrivals table as the API does")
	}
	cells := 0
	for _, c := range pageCells(t, page) {
		family, metric, ok := strings.Cut(c[0], ".")
		if !ok || family == "" || metric == "" || !slices.Contains(board.ClientFamilies, family) {
			continue
		}
		cells++
		var sum int64
		for _, d := range week.Daily {
			var n int64
			_ = json.Unmarshal(d.Clients.Families[family][metric], &n)
			sum += n
		}
		if c[1] != strconv.FormatInt(sum, 10) {
			t.Errorf("/stats %s = %s, API %d", c[0], c[1], sum)
		}
	}
	if cells != 7*len(board.ClientMetrics) {
		t.Fatalf("/stats shows %d arrival cells, want %d", cells, 7*len(board.ClientMetrics))
	}

	// Privacy: no address, raw User-Agent, client name (raw or reduced) or
	// version, key or pseudonym is stored anywhere.
	var tables []string
	rows, err := raw.Query("SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		_ = rows.Scan(&name)
		tables = append(tables, name)
	}
	rows.Close()
	for _, table := range tables {
		rows, err := raw.Query(`SELECT * FROM "` + table + `"`)
		if err != nil {
			t.Fatal(err)
		}
		columns, _ := rows.Columns()
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			_ = rows.Scan(pointers...)
			for _, v := range values {
				text := ""
				switch v := v.(type) {
				case string:
					text = v
				case []byte:
					text = string(v)
				}
				for _, secret := range []string{"203.0.113.", "198.51.100.", "192.0.2.", "claude-code/1.0.83", "python-httpx/0.27.0", "python-requests/2.32.3", "Chrome/129", "GPTBot/1.2", "Zeta Agent", "zeta-agent", "solo-agent", "1.2.3"} {
					if strings.Contains(text, secret) {
						t.Fatalf("table %s stored %q: %q", table, secret, text)
					}
				}
			}
		}
		rows.Close()
	}
}
