package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// First contact over HTTP: a bare /api/messages or /r/ROOM read is the hot
// view when it ranks a page of posts, and newest first when it does not (a
// quiet room never reads empty); an offset alone pages the hot view; sort=new,
// a cursor, a search, /recent and /api/updates stay chronological, as does the
// agent directory once a sort or cursor is named.
func TestFirstContactDefaultsOverHTTP(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "rank.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := New(store, nil, Config{ServiceID: "swarmmemo.com"})
	post := func(text, reply string) string {
		q := url.Values{"text": {text}, "format": {"json"}}
		if reply != "" {
			q.Set("reply_to", reply)
		}
		var res board.Result
		if w := makeRequest(s, "GET", "/w/lobby/main?"+q.Encode(), "", ""); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &res) != nil || res.Receipt == nil {
			t.Fatalf("post: %s", w.Body.String())
		}
		return res.Receipt.ID
	}
	root := post("root", "")
	reply := post("a reply", root)
	last := post("last", "")
	read := func(path string) (order string, sort any) {
		t.Helper()
		var res board.Result
		if w := makeRequest(s, "GET", path, "", ""); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &res) != nil {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		ids := []string{}
		for _, m := range res.Messages {
			ids = append(ids, m.ID)
		}
		return strings.Join(ids, ","), res.Data["sort"]
	}
	hot := strings.Join([]string{last, root}, ",")
	chronological := strings.Join([]string{root, reply, last}, ",")
	for _, c := range []struct {
		path, order string
		sort        any
	}{
		{"/api/messages?limit=2", hot, "hot"},
		{"/api/messages?room=lobby&limit=2", hot, "hot"},
		{"/r/lobby?format=json&limit=2", hot, "hot"},
		{"/r/lobby/main?format=json&limit=2", hot, "hot"},
		{"/api/messages?offset=1&limit=1", root, "hot"},
		{"/api/messages", chronological, "new"},
		{"/api/messages?limit=20", chronological, "new"},
		{"/r/lobby?format=json", chronological, "new"},
		{"/api/messages?sort=new", chronological, nil},
		{"/api/messages?cursor=start", chronological, nil},
		{"/api/messages?q=a", strings.Join([]string{reply, last}, ","), nil},
		{"/recent?format=json", chronological, nil},
		{"/api/updates", chronological, nil},
	} {
		if order, sort := read(c.path); order != c.order || sort != c.sort {
			t.Errorf("%s: order %s sort %v, want %s %v", c.path, order, sort, c.order, c.sort)
		}
	}
	// curl's plain text of a room is the same view.
	if text := makeRequest(s, "GET", "/r/lobby?limit=2", "", "").Body.String(); strings.Contains(text, "a reply") || !strings.Contains(text, "last") {
		t.Fatalf("/r/lobby text: %s", text)
	}
	var agents board.Result
	if w := makeRequest(s, "GET", "/api/agents", "", ""); json.Unmarshal(w.Body.Bytes(), &agents) != nil || agents.Data["sort"] != "hot" {
		t.Fatalf("/api/agents: %s", w.Body.String())
	}
	if w := makeRequest(s, "GET", "/api/agents?sort=new", "", ""); w.Code != 200 || strings.Contains(w.Body.String(), `"sort":"hot"`) {
		t.Fatalf("/api/agents?sort=new: %s", w.Body.String())
	}
}

// The hosted read_messages tool reads the hot view by default and passes an
// explicit sort or offset through.
func TestMCPReadMessagesDefaultsToHot(t *testing.T) {
	s, svc := catalogServer(board.Features{})
	server := httptest.NewServer(s)
	defer server.Close()
	for _, c := range []struct{ args, data string }{
		{`{}`, `{"sort":"hot"}`},
		{`{"room":"lobby"}`, `{"sort":"hot"}`},
		{`{"cursor":"start"}`, ``},
		{`{"query":"x"}`, ``},
		{`{"sort":"new"}`, `{"sort":"new"}`},
		{`{"sort":"top","offset":20}`, `{"sort":"top","offset":20}`},
		{`{"scope":"all"}`, `{"sort":"hot","scope":"all"}`},
		{`{"scope":"all","cursor":"start"}`, `{"scope":"all"}`},
	} {
		out := mcpCall(t, server.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_messages","arguments":`+c.args+`}}`)
		if out["error"] != nil {
			t.Fatalf("%s: %v", c.args, out["error"])
		}
		svc.mu.Lock()
		last := svc.commands[len(svc.commands)-1]
		svc.mu.Unlock()
		if last.Operation != "messages.list" || last.Data != c.data {
			t.Errorf("%s: data %q, want %q", c.args, last.Data, c.data)
		}
	}
}

// The all-rooms feed over HTTP is the front page; scope=all and a room read
// reach the rest, and a bad scope is refused.
func TestFrontPageOverHTTP(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "front.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := New(store, nil, Config{ServiceID: "swarmmemo.com"})
	if w := makeRequest(s, "GET", "/w/bounties/main?text=a+bounty&format=json", "", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for path, want := range map[string]bool{
		"/api/messages": false, "/api/messages?sort=new": false, "/api/messages?cursor=start": false,
		"/api/messages?scope=all": true, "/api/messages?scope=all&sort=new": true, "/api/messages?room=bounties": true, "/r/bounties?format=json": true,
	} {
		if got := strings.Contains(makeRequest(s, "GET", path, "", "").Body.String(), "a bounty"); got != want {
			t.Errorf("%s: shows the bounty %v, want %v", path, got, want)
		}
	}
	if w := makeRequest(s, "GET", "/api/messages?scope=everything", "", ""); w.Code != 400 {
		t.Fatalf("bad scope: %d", w.Code)
	}
}
