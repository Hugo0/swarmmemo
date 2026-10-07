package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

func tailFixture(t *testing.T) (*board.Store, *Server) {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "tail.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, New(store, nil, Config{ServiceID: "swarmmemo.com"})
}

// /api/updates?wait=SECONDS is updates.get with data.wait: a caught-up read
// that times out answers empty with the same cursor.
func TestUpdatesWaitQuery(t *testing.T) {
	_, s := tailFixture(t)
	makeRequest(s, "GET", "/w/lobby/main?text=seed", "", "")
	var first board.Result
	if w := makeRequest(s, "GET", "/api/updates", "", ""); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &first) != nil {
		t.Fatalf("first read: %d %s", w.Code, w.Body.String())
	}
	start := time.Now()
	w := makeRequest(s, "GET", "/api/updates?wait=1&cursor="+url.QueryEscape(first.NextCursor), "", "")
	var res board.Result
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &res) != nil || len(res.Messages) != 0 || res.NextCursor != first.NextCursor {
		t.Fatalf("a timed-out wait must answer empty with the same cursor: %d %s", w.Code, w.Body.String())
	}
	if time.Since(start) < time.Second {
		t.Fatal("the read did not wait")
	}
	for _, path := range []string{"/api/updates?wait=soon", "/api/updates?wait=1&wait=2", `/api/updates?wait=1&data={}`, "/api/updates?wait=26&cursor=" + url.QueryEscape(first.NextCursor)} {
		if w := makeRequest(s, "GET", path, "", ""); w.Code != 400 {
			t.Fatalf("%s: want 400, got %d %s", path, w.Code, w.Body.String())
		}
	}
}

// The text tail follows one public room: backlog, then new posts as they
// land, never hidden, private or addressed ones, with terminal escapes shown
// escaped; its slots are capped per address and freed on disconnect.
func TestTailStreamsPublicPostsSafely(t *testing.T) {
	store, s := tailFixture(t)
	owner, stranger := newRoomKey(t), newRoomKey(t)
	exec := func(c board.Command) board.Result {
		t.Helper()
		res, err := store.Execute(context.Background(), c, "fixture")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		return res
	}
	exec(owner.sign(board.Command{Operation: "room.create", Room: "vault", Visibility: "private"}))
	exec(board.Command{Operation: "post", Room: "lobby", Text: "BACKLOG visible"})
	hidden := exec(board.Command{Operation: "post", Room: "lobby", Text: "BACKLOG hidden"}).Receipt.ID
	if err := store.Moderate(context.Background(), hidden, "test", true); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s)
	defer server.Close()

	if res, err := http.Get(server.URL + "/tail/vault"); err != nil || res.StatusCode == 200 {
		t.Fatalf("a private room must not tail: %v %v", res, err)
	} else {
		res.Body.Close()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	open := func() (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/tail/lobby", nil)
		return http.DefaultClient.Do(req)
	}
	res, err := open()
	if err != nil || res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("tail: %v %v", res, err)
	}
	lines := make(chan string, 100)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	var seen []string
	readUntil := func(marker string) {
		t.Helper()
		timeout := time.After(10 * time.Second)
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("the tail closed before %q: %q", marker, seen)
				}
				seen = append(seen, line)
				if strings.Contains(line, marker) {
					return
				}
			case <-timeout:
				t.Fatalf("no %q in the tail: %q", marker, seen)
			}
		}
	}
	readUntil("BACKLOG visible")
	exec(owner.sign(board.Command{Operation: "post", Room: "vault", Text: "PRIVATE text"}))
	exec(stranger.sign(board.Command{Operation: "post", Room: "lobby", Text: "ADDRESSED text", To: owner.id}))
	exec(board.Command{Operation: "post", Room: "lobby", Text: "LIVE \x1b[31mred\x1b[0m \u202eevil\x07 done"})
	readUntil("LIVE")
	exec(board.Command{Operation: "post", Room: "lobby", Text: "LAST post"})
	readUntil("LAST post")
	all := strings.Join(seen, "\n")
	for _, leak := range []string{"BACKLOG hidden", "PRIVATE text", "ADDRESSED text", "\x1b", "\u202e", "\x07"} {
		if strings.Contains(all, leak) {
			t.Fatalf("the tail showed %q:\n%s", leak, all)
		}
	}
	if !strings.Contains(all, `LIVE \x1b[31mred\x1b[0m \u202eevil\x07 done`) {
		t.Fatalf("control characters must be shown escaped:\n%s", all)
	}

	// Two tails per address; the third is refused.
	second, err := open()
	if err != nil || second.StatusCode != 200 {
		t.Fatalf("second tail: %v %v", second, err)
	}
	defer second.Body.Close()
	third, err := open()
	if err != nil || third.StatusCode != 429 {
		t.Fatalf("a third tail from one address must be 429: %v %v", third, err)
	}
	third.Body.Close()
	if s.tails.Held() != 2 || len(s.streams) != 2 {
		t.Fatalf("held tails %d, streams %d", s.tails.Held(), len(s.streams))
	}
	cancel()
	for deadline := time.Now().Add(5 * time.Second); s.tails.Held() != 0 || len(s.streams) != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("closed tails kept their slots: %d, streams %d", s.tails.Held(), len(s.streams))
		}
	}
}

func TestTerminalSafe(t *testing.T) {
	for in, want := range map[string]string{
		"plain\ttext\nline":   "plain\ttext\nline",
		"\x1b[2J\x1b]0;x\x07": `\x1b[2J\x1b]0;x\x07`,
		"a\rb\x00c\x7f":       `a\x0db\x00c\x7f`,
		"\u0085\u009b31m":     `\u0085\u009b31m`,
		"bidi \u202e \u2066":  `bidi \u202e \u2066`,
		"line\u2028sep":       `line\u2028sep`,
		"bad \xff utf8":       "bad \uFFFD utf8",
		"family 👩\u200d👧 ok":  "family 👩\u200d👧 ok",
	} {
		if got := terminalSafe(in); got != want {
			t.Errorf("terminalSafe(%q) = %q, want %q", in, got, want)
		}
	}
}
