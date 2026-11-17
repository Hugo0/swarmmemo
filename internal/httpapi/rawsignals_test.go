package httpapi

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C452: an accepted write over HTTP and over MCP leaves a sealed raw record
// with the client address the server trusts, the TCP peer, the proxy's
// X-Forwarded-For chain and every header, credential-bearing ones redacted;
// the stored blob never holds the address or a credential in clear, and a
// read leaves no record.
func TestRawSignalsOverHTTPAndMCP(t *testing.T) {
	dir := t.TempDir()
	store, err := board.Open(filepath.Join(dir, "board.sqlite"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err = store.SetAllowanceParams(t.Context(), board.PostingParamsNamespace, []byte(`{"anonymous_top_level_per_hour":1000,"anonymous_top_level_per_room_day":1000}`), "test", 0); err != nil {
		t.Fatal(err)
	}
	s := New(store, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", Version: "test", TrustLoopbackProxy: true})
	const (
		clientIP   = "198.51.100.66"
		authSecret = "Basic c2VjcmV0LWNyZWQtYzQ1Mg=="
		cookie     = "sid=cookie-secret-c452"
		apiKey     = "api-key-secret-c452"
	)
	send := func(path, body string, extra map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("POST", "https://swarmmemo.com"+path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:4000"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("X-Forwarded-For", "10.9.8.7, "+clientIP)
		r.Header.Set("User-Agent", "canary-c452/1.0")
		r.Header.Set("Authorization", authSecret)
		r.Header.Set("Cookie", cookie)
		r.Header.Set("X-Api-Key", apiKey)
		r.Header.Set("Referer", "https://blog.example/post/1?text=secret-query-c452")
		for k, v := range extra {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("POST %s: %d %s", path, w.Code, w.Body.String())
		}
		return w
	}
	check := func(r board.RawSignal, via string) {
		t.Helper()
		if r.Unreadable != "" || r.Operation != "post" || r.Via != via || r.IP != clientIP || r.RemoteAddr != "127.0.0.1:4000" || r.ForwardedFor != "10.9.8.7, "+clientIP || r.Proto != "HTTP/1.1" || r.Room != "lobby" {
			t.Fatalf("raw record %+v", r)
		}
		if got := r.Headers["User-Agent"]; len(got) != 1 || got[0] != "canary-c452/1.0" {
			t.Fatalf("user agent %v", r.Headers)
		}
		for _, h := range []string{"Authorization", "Cookie", "X-Api-Key"} {
			if got := r.Headers[h]; len(got) != 1 || got[0] != "<redacted>" {
				t.Fatalf("%s not redacted: %v", h, got)
			}
		}
		if got := r.Headers["Referer"]; len(got) != 1 || got[0] != "https://blog.example/post/1?<redacted>" {
			t.Fatalf("referer %v", got)
		}
	}

	w := send("/v1/command", `{"operation":"post","room":"lobby","text":"raw over http","request_id":"raw-http"}`, nil)
	var res board.Result
	if err = json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Receipt == nil {
		t.Fatalf("%s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), clientIP) {
		t.Fatalf("the address in the answer: %s", w.Body.String())
	}
	records, err := store.RawSignalsForObject(t.Context(), res.Receipt.ID)
	if err != nil || len(records) != 1 {
		t.Fatalf("http raw records %v %v", records, err)
	}
	check(records[0], "command")

	send("/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_message","arguments":{"room":"lobby","text":"raw over mcp"},"_meta":{"io.modelcontextprotocol/clientInfo":{"name":"canary-mcp-client","version":"4.5.2"}}}}`, nil)
	raw, err := sql.Open("sqlite", filepath.Join(dir, board.RawSignalsDBFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var mcpObject, mcpVia string
	if err = raw.QueryRow("SELECT object_id,via FROM raw_signals ORDER BY seq DESC LIMIT 1").Scan(&mcpObject, &mcpVia); err != nil {
		t.Fatal(err)
	}
	if mcpObject == res.Receipt.ID || mcpObject == "" {
		t.Fatalf("no MCP record: %q via %q", mcpObject, mcpVia)
	}
	records, err = store.RawSignalsForObject(t.Context(), mcpObject)
	if err != nil || len(records) != 1 {
		t.Fatalf("mcp raw records %v %v", records, err)
	}
	check(records[0], mcpVia)
	if c := records[0].MCPClient; c == nil || c.Name != "canary-mcp-client" || c.Version != "4.5.2" {
		t.Fatalf("mcp client %+v", c)
	}

	// A read leaves no record.
	r := httptest.NewRequest("GET", "https://swarmmemo.com/api/messages?room=lobby", nil)
	r.RemoteAddr = "127.0.0.1:4000"
	r.Header.Set("X-Forwarded-For", clientIP)
	s.ServeHTTP(httptest.NewRecorder(), r)
	var n int
	if err = raw.QueryRow("SELECT count(*) FROM raw_signals").Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d raw records, want 2 (two posts, no read): %v", n, err)
	}

	// Sealed: neither the address nor a credential is stored in clear.
	rows, err := raw.Query("SELECT created_at,operation,object_id,account,via,key_id,sealed FROM raw_signals")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var at int64
		var op, obj, account, via, key string
		var sealed []byte
		if err = rows.Scan(&at, &op, &obj, &account, &via, &key, &sealed); err != nil {
			t.Fatal(err)
		}
		all := bytes.Join([][]byte{[]byte(op), []byte(obj), []byte(account), []byte(via), []byte(key), sealed}, []byte{0})
		for _, secret := range []string{clientIP, "canary-c452", "c2VjcmV0", "cookie-secret", apiKey, "secret-query", "canary-mcp-client"} {
			if bytes.Contains(all, []byte(secret)) {
				t.Fatalf("%q stored in clear", secret)
			}
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
}
