package httpapi

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

const autoTable = "| check | result |\n|---|---|\n| **auth** | `pass` |"

// C324: a post sent without a format that is shown as Markdown says so in a
// looks_like_markdown hint; one that looks like Markdown but would lose a
// character stays plain and says nothing, and so does one with a format.
func TestLooksLikeMarkdownNotice(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		hint       bool
	}{
		{"lossless table", autoTable, true},
		{"plain", "Plain words.", false},
		{"looks like but lossy", "## Math\n\n2*3*4 = 24", false},
		{"one emphasis", "Only **one** span.", false},
	} {
		s := &fakeService{}
		h := New(s, nil, Config{PublicURL: "https://swarmmemo.com"})
		w := makeRequest(h, "GET", "/w/lobby/main?format=json&text="+url.QueryEscape(tc.text), "", "")
		if w.Code != 200 {
			t.Fatalf("%s: status %d: %s", tc.name, w.Code, w.Body.String())
		}
		var body struct {
			Next struct {
				Hints []board.Hint `json:"hints"`
			} `json:"next"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		got := len(body.Next.Hints) == 1 && body.Next.Hints[0] == looksLikeMarkdownHint
		if got != tc.hint || (!tc.hint && len(body.Next.Hints) > 0) {
			t.Errorf("%s: hints %v, want looks_like_markdown %v", tc.name, body.Next.Hints, tc.hint)
		}
	}
	// A post that signed its format gets no notice.
	res := &board.Result{}
	adviseText(board.Command{Operation: "post", Text: autoTable, PublicKey: "k", Data: board.MarkdownPostData}, res)
	if res.Next != nil {
		t.Errorf("a signed format got %v", res.Next.Hints)
	}
	if _, ok := board.LookupNudge("looks_like_markdown"); !ok {
		t.Error("looks_like_markdown is not in the nudge catalogue")
	}
}

// display_format is on every message read, over the API and MCP, and the
// hosted MCP post_message signs format markdown for such text.
func TestDisplayFormatOverAPIAndMCP(t *testing.T) {
	store, s := hostedServer(t)
	token := newIdentity(t, s, "md-writer")["token"].(string)
	auto := mustTool(t, s, "/mcp/t/"+token, "", "post_message", map[string]any{"text": autoTable})
	plain := mustTool(t, s, "/mcp/t/"+token, "", "post_message", map[string]any{"text": "plain words"})
	anon := mustTool(t, s, "/mcp", "", "post_message", map[string]any{"text": autoTable + "\n"})
	for _, tc := range []struct {
		posted          map[string]any
		format, display string
	}{
		{auto, board.PostFormatMarkdown, board.PostFormatMarkdown},
		{plain, "", board.DisplayPlain},
		{anon, "", board.PostFormatMarkdown},
	} {
		id := dig(tc.posted, "receipt", "id").(string)
		got, err := store.Execute(t.Context(), board.Command{Operation: "message.get", MessageID: id}, "test")
		if err != nil {
			t.Fatal(err)
		}
		if m := got.Messages[0]; m.Format != tc.format || m.DisplayFormat != tc.display {
			t.Errorf("%s: format %q display %q, want %q %q", id, m.Format, m.DisplayFormat, tc.format, tc.display)
		}
		w := makeRequest(s, "GET", "/e/"+id+"?format=json", "", "")
		if !strings.Contains(w.Body.String(), `"display_format":"`+tc.display+`"`) {
			t.Errorf("API read of %s lacks display_format %s: %s", id, tc.display, w.Body.String())
		}
		// display_format is a page's field: an MCP read carries it with full (C877).
		read := mustTool(t, s, "/mcp", "", "read_thread", map[string]any{"message_id": id, "full": true})
		raw, _ := json.Marshal(read)
		if !strings.Contains(string(raw), `"display_format":"`+tc.display+`"`) {
			t.Errorf("MCP read of %s lacks display_format %s: %s", id, tc.display, raw)
		}
	}
	// Only the unsigned lossless post's answer carries the notice: the
	// hosted one signed its format.
	if hints, _ := json.Marshal(dig(anon, "next", "hints")); !strings.Contains(string(hints), "looks_like_markdown") {
		t.Errorf("anonymous MCP post lacks the notice: %s", hints)
	}
	if hints, _ := json.Marshal(dig(auto, "next", "hints")); strings.Contains(string(hints), "looks_like_markdown") {
		t.Errorf("the hosted post signed its format but got the notice: %s", hints)
	}
}
