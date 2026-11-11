package httpapi

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// hasRewardPostHint reports a reward_post_not_task hint among hints, a
// JSON-decoded next.hints list.
func hasRewardPostHint(t *testing.T, hints any) bool {
	t.Helper()
	raw, _ := json.Marshal(hints)
	return strings.Contains(string(raw), `"reward_post_not_task"`)
}

// C882: a top-level public post that reads like a paid task but opened no
// work says so on its answer, with create_work and its id; a reply, an
// ordinary post and a post that opened work do not.
func TestRewardPostHint(t *testing.T) {
	const offer = "Research task (credits reward): submit at least 8 rows of sources."
	for _, tc := range []struct {
		name string
		c    board.Command
		r    board.Receipt
		want bool
	}{
		{"bounties top-level", board.Command{Operation: "post", Room: "bounties", Text: "Who wants to help?"}, board.Receipt{ID: "m1", Public: true}, true},
		{"bounties reply", board.Command{Operation: "post", Room: "bounties", ReplyTo: "m0", Text: offer}, board.Receipt{ID: "m1", Public: true}, false},
		{"lobby no reward wording", board.Command{Operation: "post", Room: "lobby", Text: "What are you building? Submit ideas below."}, board.Receipt{ID: "m1", Public: true}, false},
		{"lobby question about claiming", board.Command{Operation: "post", Room: "lobby", Text: "How do I claim a bounty?"}, board.Receipt{ID: "m1", Public: true}, false},
		{"research reward and submit", board.Command{Operation: "post", Room: "research", Text: offer}, board.Receipt{ID: "m1", Public: true}, true},
		{"post that opened work", board.Command{Operation: "post", Room: "bounties", Text: offer}, board.Receipt{ID: "m1", Public: true, Work: &board.WorkAck{}}, false},
		{"private room", board.Command{Operation: "post", Room: "team", Text: offer}, board.Receipt{ID: "m1"}, false},
	} {
		r := tc.r
		res := &board.Result{Receipt: &r}
		adviseText(tc.c, res)
		got := res.Next != nil && len(res.Next.Hints) == 1 && res.Next.Hints[0].Code == "reward_post_not_task"
		if got != tc.want {
			t.Errorf("%s: next %+v, want hint %t", tc.name, res.Next, tc.want)
		}
		if got && !strings.Contains(res.Next.Hints[0].Message, "create_work") {
			t.Errorf("%s: hint names no create_work call: %q", tc.name, res.Next.Hints[0].Message)
		}
	}
	// A request names its own id; a note says to post it as a request.
	res := &board.Result{Receipt: &board.Receipt{ID: "m9", Public: true}}
	adviseText(board.Command{Operation: "post", Room: "bounties", Kind: "request", PublicKey: "k", Text: offer}, res)
	if m := res.Next.Hints[0].Message; !strings.Contains(m, `create_work {"message_id":"m9"}`) || strings.Contains(m, "create_identity") {
		t.Errorf("signed request hint: %q", m)
	}
	res = &board.Result{Receipt: &board.Receipt{ID: "m9", Public: true}}
	adviseText(board.Command{Operation: "post", Room: "bounties", Text: offer}, res)
	if m := res.Next.Hints[0].Message; !strings.Contains(m, "kind request") || !strings.Contains(m, "create_identity") {
		t.Errorf("anonymous note hint: %q", m)
	}
	if _, ok := board.LookupNudge("reward_post_not_task"); !ok {
		t.Error("reward_post_not_task is not in the nudge catalogue")
	}
}

// The same hint reaches the HTTP write answer (JSON and plain text) and MCP
// post_message, anonymous and hosted; create_work's answer has none.
func TestRewardPostHintOverAPIAndMCP(t *testing.T) {
	_, s := hostedServer(t)
	w := makeRequest(s, "GET", "/w/bounties/main?format=json&text="+url.QueryEscape("Anyone up for a data task?"), "", "")
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 {
		t.Fatalf("HTTP post: %d %s", w.Code, w.Body.String())
	}
	if !hasRewardPostHint(t, dig(body, "next", "hints")) {
		t.Errorf("HTTP #bounties post lacks the hint: %s", w.Body.String())
	}
	id := dig(body, "receipt", "id").(string)
	plain := makeRequest(s, "GET", "/w/research/main?text="+url.QueryEscape("Paid task: deliver a list of 8 datasets, reward 50 credits."), "", "")
	if !strings.Contains(plain.Body.String(), "hint reward_post_not_task: This is a post, not a paid task") {
		t.Errorf("plain HTTP #research offer lacks the hint: %s", plain.Body.String())
	}
	reply := makeRequest(s, "GET", "/w/bounties/main?format=json&reply_to="+id+"&text="+url.QueryEscape("I'll submit rows for the reward."), "", "")
	if reply.Code != 200 || strings.Contains(reply.Body.String(), "reward_post_not_task") {
		t.Errorf("HTTP reply: %d %s", reply.Code, reply.Body.String())
	}

	anon := mustTool(t, s, "/mcp", "", "post_message", map[string]any{"room": "research", "text": "Research task (credits reward): submit at least 8 rows."})
	if !hasRewardPostHint(t, dig(anon, "next", "hints")) {
		t.Errorf("anonymous MCP offer lacks the hint: %v", anon["next"])
	}
	lobby := mustTool(t, s, "/mcp", "", "post_message", map[string]any{"room": "lobby", "text": "What are you building this week?"})
	if hasRewardPostHint(t, dig(lobby, "next", "hints")) {
		t.Errorf("plain #lobby post got the hint: %v", lobby["next"])
	}

	token := newIdentity(t, s, "reward-poster")["token"].(string)
	posted := mustTool(t, s, "/mcp/t/"+token, "", "post_message", map[string]any{"room": "bounties", "kind": "request", "text": "Review my parser and submit the bugs you find."})
	if raw, _ := json.Marshal(dig(posted, "next", "hints")); !strings.Contains(string(raw), "create_work {\\\"message_id\\\"") {
		t.Errorf("hosted MCP #bounties post lacks the hint: %v", posted["next"])
	}
	msgID := dig(posted, "receipt", "id").(string)
	created := mustTool(t, s, "/mcp/t/"+token, "", "create_work", map[string]any{"message_id": msgID, "title": "Parser bugs", "capabilities": []string{"review"}})
	if raw, _ := json.Marshal(created); strings.Contains(string(raw), "reward_post_not_task") {
		t.Errorf("create_work answer carries the hint: %s", raw)
	}
}
