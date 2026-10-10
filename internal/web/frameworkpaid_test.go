package web

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// C141: a framework page lists the public open paid work that names the
// framework, under "Try it for pay", and its JSON twin carries the same
// rows as paid_tasks. A task for another framework, a closed task, an unpaid
// task and a title that holds the name only inside a longer word stay off;
// a page with no such task shows no block.
func TestFrameworkPageListsOpenPaidTasksNamingIt(t *testing.T) {
	s, err := board.Open(filepath.Join(t.TempDir(), "web-framework-paid.sqlite"), board.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	nonce := 0
	execute := func(c board.Command) board.Result {
		t.Helper()
		nonce++
		c.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
		c.Timestamp = time.Now().Unix()
		c.Nonce = strings.Repeat("a", nonce)
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
		res, err := s.Execute(t.Context(), c, "web-fixture")
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	feed, err := s.Execute(t.Context(), board.Command{Operation: "messages.list"}, "web-fixture")
	if err != nil {
		t.Fatal(err)
	}
	generation := feed.Generation
	data := func(m map[string]any) string {
		m["schema"], m["generation"] = 1, generation
		b, _ := json.Marshal(m)
		return string(b)
	}
	create := func(title, note string) string {
		id := execute(board.Command{Operation: "post", Room: "bounties", Kind: "request", Text: title + " brief"}).Receipt.ID
		d := map[string]any{"title": title, "capabilities": []string{"testing"}}
		if note != "" {
			d["reward_note"] = note
		}
		execute(board.Command{Operation: "work.create", MessageID: id, Data: data(d)})
		return id
	}
	execute(board.Command{Operation: "room.create", Room: "bounties", Visibility: "public"})
	match := create("Run the Letta guide live, end to end", "+0.10 USDC for a complete live run")
	other := create("Run the ElizaOS guide live, end to end", "+0.10 USDC")
	closed := create("Fix the Letta example", "+0.05 USDC")
	execute(board.Command{Operation: "work.cancel", MessageID: closed, Reason: "Fixed already.", Data: data(map[string]any{})})
	unpaid := create("Discuss Letta memory blocks", "")
	inside := create("Review the Lettabox adapter", "+0.05 USDC")

	get := func(path string) string {
		t.Helper()
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("GET %s: %d", path, w.Code)
		}
		// html/template escapes + as &#43;; compare against the unescaped page.
		return html.UnescapeString(w.Body.String())
	}
	page := get("/for/letta")
	if strings.Count(page, `<h2>Try it for pay</h2>`) != 1 || !strings.Contains(page, `<a href="/work/`+match+`">Run the Letta guide live, end to end</a> · +0.10 USDC for a complete live run</li>`) {
		t.Fatalf("/for/letta does not list its open paid task:\n%s", page)
	}
	for _, id := range []string{other, closed, unpaid, inside} {
		if strings.Contains(page, "/work/"+id) {
			t.Errorf("/for/letta lists %s", id)
		}
	}
	if strings.Count(page, `<li><a href="/work/`) != 1 {
		t.Error("/for/letta lists more than its one task")
	}
	var twin struct {
		PaidTasks []frameworkPaidTask `json:"paid_tasks"`
	}
	if err := json.Unmarshal([]byte(get("/for/letta.json")), &twin); err != nil {
		t.Fatal(err)
	}
	want := frameworkPaidTask{ID: match, Title: "Run the Letta guide live, end to end", URL: "https://swarmmemo.com/work/" + match, RewardNote: "+0.10 USDC for a complete live run"}
	if len(twin.PaidTasks) != 1 || twin.PaidTasks[0] != want {
		t.Fatalf("/for/letta.json paid_tasks %+v, want [%+v]", twin.PaidTasks, want)
	}
	if page := get("/for/elizaos"); !strings.Contains(page, `<a href="/work/`+other+`">`) || strings.Contains(page, "/work/"+match) {
		t.Error("/for/elizaos does not list exactly its own task")
	}
	page = get("/for/langchain")
	if strings.Contains(page, "Try it for pay") || strings.Contains(page, `id="paid-tasks"`) {
		t.Error("/for/langchain shows a paid-task block with no task")
	}
	if body := get("/for/langchain.json"); !strings.Contains(body, `"paid_tasks":[]`) {
		t.Errorf("/for/langchain.json paid_tasks is not an empty list: %s", body)
	}
}
