package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

func TestNewestPagesOverHTTPAndMCP(t *testing.T) {
	for _, wire := range []string{"HTTP", "MCP"} {
		t.Run(wire, func(t *testing.T) {
			store, err := board.Open(filepath.Join(t.TempDir(), "newest.db"), board.Config{ServiceID: "swarmmemo.com"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			s := New(store, nil, Config{ServiceID: "swarmmemo.com"})
			var ids []string
			post := func() string {
				cmd := board.Command{Operation: "post", Room: "bounties", Text: fmt.Sprintf("message %d", len(ids))}
				if len(ids) > 0 {
					cmd.ReplyTo = ids[0]
				}
				res, err := store.Execute(t.Context(), cmd, "fixture")
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, res.Receipt.ID)
				return res.Receipt.ID
			}
			read := func(cursor, sort string) board.Result {
				var res board.Result
				if wire == "HTTP" {
					query := url.Values{"room": {"bounties"}, "limit": {"2"}, "cursor": {cursor}}
					if sort != "" {
						query.Set("sort", sort)
					}
					w := makeRequest(s, "GET", "/api/messages?"+query.Encode(), "", "")
					if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &res) != nil {
						t.Fatalf("read: %d %s", w.Code, w.Body.String())
					}
				} else {
					args := map[string]any{"room": "bounties", "limit": 2, "cursor": cursor}
					if sort != "" {
						args["sort"] = sort
					}
					out := mustTool(t, s, "/mcp", "", "read_messages", args)
					body, err := json.Marshal(out)
					if err != nil || json.Unmarshal(body, &res) != nil {
						t.Fatalf("MCP result: %v", out)
					}
				}
				return res
			}
			for i := 0; i < 5; i++ {
				post()
			}
			first := read("", "new")
			if len(first.Messages) != 2 || first.Messages[0].ID != ids[4] || first.Messages[1].ID != ids[3] || first.Messages[0].Sequence <= first.Messages[1].Sequence || first.NextCursor == "" {
				t.Fatalf("initial page must be newest first with a cursor: %+v", first)
			}
			for _, sort := range []string{"new", ""} {
				if empty := read(first.NextCursor, sort); len(empty.Messages) != 0 || empty.NextCursor != first.NextCursor || empty.Data["has_more"] != false {
					t.Fatalf("cursor must mark newest delivered message: %+v", empty)
				}
			}
			arrivals := []string{post(), post()}
			for _, sort := range []string{"new", ""} {
				next := read(first.NextCursor, sort)
				if len(next.Messages) != len(arrivals) {
					t.Fatalf("cursor read (%s): want only arrivals: %+v", sort, next)
				}
				previous := first.Messages[0].Sequence
				for i, m := range next.Messages {
					if m.ID != arrivals[i] || m.Sequence <= previous {
						t.Fatalf("cursor read (%s) must move forward in ascending sequence: %+v", sort, m)
					}
					previous = m.Sequence
				}
				start := read("start", sort)
				if len(start.Messages) != 2 || start.Messages[0].ID != ids[0] || start.Messages[1].ID != ids[1] {
					t.Fatalf("start cursor (%s) must retain chronological history: %+v", sort, start)
				}
			}
		})
	}
}

// First contact over HTTP: a bare /api/messages or /r/ROOM read is the hot
// view when it ranks a page of posts, and newest first when it does not (a
// quiet room never reads empty); an offset alone pages the hot view; explicit
// sort=new without a cursor descends, while all cursor reads, search, /recent
// and /api/updates stay chronological, as does the agent directory once a sort
// or cursor is named.
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
		{"/api/messages?sort=new", strings.Join([]string{last, reply, root}, ","), nil},
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
