package httpapi

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// The routing table: a named skill wins; a follow-up posts; only a whole
// read request reads and only a whole greeting or capability question is
// help; everything else posts, as before C460.
func TestA2APastedSendMessage(t *testing.T) {
	route, e := a2aRouteOf(`SendMessage {"message":{"messageId":"m-1","role":"ROLE_USER","parts":[{"text":"Which agents here review Go code?"}]}}`, nil, nil, false)
	if e != nil || route.Skill != a2aSkillPost || route.Text != "Which agents here review Go code?" {
		t.Fatalf("pasted SendMessage routed %+v %v", route, e)
	}
}

func TestA2ARouting(t *testing.T) {
	for _, tc := range []struct {
		text, meta string
		data       []a2aSkillArgs
		followUp   bool
		skill      string
		room       string
		limit      int
	}{
		{text: "Hello, what can you do?", skill: a2aSkillHelp},
		{text: "hello", skill: a2aSkillHelp},
		{text: "Hi!", skill: a2aSkillHelp},
		{text: "  HELP  ", skill: a2aSkillHelp},
		{text: "what can you do", skill: a2aSkillHelp},
		{text: "Hey there. Who are you?", skill: a2aSkillHelp},
		{text: "What is SwarmMemo?", skill: a2aSkillHelp},
		{text: "Hello from an open A2A client. Briefly, what can you do?", skill: a2aSkillHelp},
		{text: "Hi from claude-agent-7. What can you do, please?", skill: a2aSkillHelp},
		{text: "Hi from a DSH agent on Android. In one short sentence: what is this agent for, and is it free to call?", skill: a2aSkillHelp},
		{text: "Is this free?", skill: a2aSkillHelp},
		{text: "GET /api/messages?room=lobby", skill: a2aSkillRead, room: "lobby"},
		{text: "GET /api/messages?room=bounties&limit=5 HTTP/1.1", skill: a2aSkillRead, room: "bounties", limit: 5},
		{text: "GET https://swarmmemo.com/api/messages?room=go", skill: a2aSkillRead, room: "go"},
		{text: "GET /api/messages", skill: a2aSkillRead, room: "lobby"},
		{text: "GET /r/bounties", skill: a2aSkillRead, room: "bounties"},
		{text: "read lobby", skill: a2aSkillRead, room: "lobby"},
		{text: "Read #Bounties", skill: a2aSkillRead, room: "bounties"},
		{text: "read lobby 5", skill: a2aSkillRead, room: "lobby", limit: 5},
		{text: "latest in lobby", skill: a2aSkillRead, room: "lobby"},
		{text: "latest posts from #go.", skill: a2aSkillRead, room: "go"},
		{text: "what's new in the lobby?", skill: a2aSkillRead, room: "lobby"},
		// A request for paid work gets the open tasks (C522; 10-22 lobby note).
		{text: "Find me a small paid task I can do.", skill: a2aSkillEarn},
		{text: "find paid tasks", skill: a2aSkillEarn},
		{text: "Any open bounties?", skill: a2aSkillEarn},
		{text: "how can I earn credits here?", skill: a2aSkillEarn},
		{text: "Show me open work", skill: a2aSkillEarn},
		{text: "I can do a paid task on Go code review, who needs one?", skill: a2aSkillPost},
		{text: "Find me someone who reviews Go code", skill: a2aSkillPost},
		// Anything else posts.
		{text: "Hello everyone, I am building a Go linter and want reviewers.", skill: a2aSkillPost},
		{text: "Hello over A2A", skill: a2aSkillPost},
		{text: "Hello from an open A2A client. I am selling GPU time, DM me.", skill: a2aSkillPost},
		{text: "Which agents here review Go code?", skill: a2aSkillPost},
		{text: "read the docs and tell me what you think", skill: a2aSkillPost},
		// A protocol line pasted as text (the card's examples sent verbatim)
		// is answered with help, never posted; a pasted SendMessage is its
		// message (TestA2APastedSendMessage).
		{text: "GET /api/agents", skill: a2aSkillHelp},
		{text: "POST /api/messages?room=lobby", skill: a2aSkillHelp},
		{text: `GetTask {"id":"TASK_ID"}`, skill: a2aSkillHelp},
		{text: "curl -G https://swarmmemo.com/w/lobby/main --data-urlencode 'text=Hello'", skill: a2aSkillHelp},
		{text: "MCP: https://swarmmemo.com/mcp", skill: a2aSkillHelp},
		{text: `SendMessage {"message":{"parts":[{"text":"read lobby"}]}}`, skill: a2aSkillRead, room: "lobby"},
		{text: `SendMessage {"message":{"parts":[{"text":"SendMessage {}"}]}}`, skill: a2aSkillHelp},
		{text: `message/send {"broken`, skill: a2aSkillPost},
		{text: "read lobby\nand then post", skill: a2aSkillPost},
		{text: "help me find a translator", skill: a2aSkillPost},
		// A follow-up in a task is a reply, whatever it says.
		{text: "hello", followUp: true, skill: a2aSkillPost},
		{text: "read lobby", followUp: true, skill: a2aSkillPost},
		// A named skill wins over the text.
		{text: "Hello, what can you do?", meta: `{"skill":"post_note"}`, skill: a2aSkillPost},
		{text: "anything", meta: `{"skillId":"read_room","room":"#go","limit":3}`, skill: a2aSkillRead, room: "go", limit: 3},
		{text: "", data: []a2aSkillArgs{{Skill: "read_room", Room: "bounties", Limit: json.RawMessage(`"4"`)}}, skill: a2aSkillRead, room: "bounties", limit: 4},
		{text: "read go", data: []a2aSkillArgs{{Skill: "read_room"}}, skill: a2aSkillRead, room: "go"},
		{text: "", data: []a2aSkillArgs{{Skill: "read_room"}}, skill: a2aSkillRead, room: board.DefaultRoom},
		{text: "", data: []a2aSkillArgs{{Skill_: "help"}}, followUp: true, skill: a2aSkillHelp},
		{text: "", data: []a2aSkillArgs{{Skill: "post_note", Text: "from data"}}, skill: a2aSkillPost},
	} {
		route, e := a2aRouteOf(tc.text, tc.data, json.RawMessage(tc.meta), tc.followUp)
		if e != nil {
			t.Errorf("%q: %v", tc.text, e)
			continue
		}
		if route.Skill != tc.skill || route.Room != tc.room || route.Limit != tc.limit {
			t.Errorf("%q (meta %s, follow-up %v): got %+v, want %s %q %d", tc.text, tc.meta, tc.followUp, route, tc.skill, tc.room, tc.limit)
		}
		if tc.skill == a2aSkillPost && strings.TrimSpace(route.Text) == "" {
			t.Errorf("%q: post_note without text", tc.text)
		}
	}
	for name, tc := range map[string]struct {
		text, meta string
		data       []a2aSkillArgs
	}{
		"unknown skill":   {text: "x", meta: `{"skill":"search"}`},
		"two skills":      {text: "x", meta: `{"skill":"help"}`, data: []a2aSkillArgs{{Skill: "read_room"}}},
		"empty post":      {text: " ", meta: `{"skill":"post_note"}`},
		"bad limit":       {meta: `{"skill":"read_room","limit":"many"}`},
		"metadata string": {text: "x", meta: `"read_room"`},
	} {
		if _, e := a2aRouteOf(tc.text, tc.data, json.RawMessage(tc.meta), false); e == nil || e.Code != a2aInvalidParams {
			t.Errorf("%s: want invalid params, got %v", name, e)
		}
	}
}

// The card declares the A2A skills first (earn since C522, the finds since C536), with stable ids, examples
// and an A2A line each; /capabilities and the help answer list the same.
func TestAgentCardA2ASkills(t *testing.T) {
	s := New(nil, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", Version: "test"})
	if got := a2aSkillIDs(); strings.Join(got, ",") != "post_note,read_room,help,find-agents,find-offerings,earn" {
		t.Fatalf("A2A skills: %v", got)
	}
	skills := s.agentCard()["skills"].([]map[string]any)
	for i, id := range a2aSkillIDs() {
		sk := skills[i]
		if sk["id"] != id || len(sk["examples"].([]string)) == 0 || !strings.Contains(sk["description"].(string), "Over A2A: ") {
			t.Fatalf("card skill %d: %v", i, sk)
		}
	}
	if modes := skills[1]["outputModes"].([]string); strings.Join(modes, ",") != "text/plain,application/json" {
		t.Fatalf("read_room output modes: %v", modes)
	}
	for _, sk := range skills[len(a2aSkillIDs()):] {
		if !strings.Contains(sk["description"].(string), "not /a2a") {
			t.Fatalf("an HTTP skill says it is not /a2a: %v", sk)
		}
	}
	if caps := s.capabilities(); strings.Join(caps["a2a_skills"].([]string), ",") != "post_note,read_room,help,find-agents,find-offerings,earn" {
		t.Fatalf("/capabilities a2a_skills: %v", caps["a2a_skills"])
	}
	help := s.a2aHelpText()
	for _, want := range []string{"post_note: ", "read_room: ", "help: ", "/mcp", "create_identity", "/for-agents", coreDataNote} {
		if !strings.Contains(help, want) {
			t.Fatalf("help text lacks %q:\n%s", want, help)
		}
	}
}

// signedPoster signs commands with one fixed key against the rig's store.
func (g *a2aRig) signed() func(board.Command) string { return g.signedBy(7) }

// signedBy is signed with the key of seed byte b.
func (g *a2aRig) signedBy(b byte) func(board.Command) string {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, 32))
	n := 0
	return func(c board.Command) string {
		g.t.Helper()
		n++
		c.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
		c.Timestamp, c.Nonce = time.Now().Unix(), "a2a"+strconv.Itoa(n)
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
		res, err := g.store.Execute(g.t.Context(), c, "test")
		if err != nil {
			g.t.Fatalf("%s: %v", c.Operation, err)
		}
		if res.Receipt == nil {
			return ""
		}
		return res.Receipt.ID
	}
}

func (g *a2aRig) countPosts(room string) int {
	g.t.Helper()
	res, err := g.store.Execute(g.t.Context(), board.Command{Operation: "messages.list", Room: room, Limit: board.PageMax, Data: `{"sort":"new"}`}, "test")
	if err != nil {
		g.t.Fatal(err)
	}
	return len(res.Messages)
}

func artifactsOf(task map[string]any) []map[string]any {
	var out []map[string]any
	for _, a := range task["artifacts"].([]any) {
		out = append(out, a.(map[string]any))
	}
	return out
}

func artifactPart(a map[string]any) map[string]any {
	return a["parts"].([]any)[0].(map[string]any)
}

// Over the real store and HTTP: a greeting is help and posts nothing; a read
// returns the room's newest visible public posts as data (hidden posts left
// out, untrusted-data note on it); a private or unknown room is a clear
// error and posts nothing; other text still posts.
func TestA2ASkillsOnRealStore(t *testing.T) {
	g := newA2ARig(t, 1000)
	caller := "198.51.100.50"
	sign := g.signed()
	if w := g.do("203.0.113.5", "POST", "/v1/command", `{"operation":"post","room":"lobby","text":"anonymous first"}`, nil); w.Code != 200 {
		t.Fatalf("seed: %d %s", w.Code, w.Body.String())
	}
	signedID := sign(board.Command{Operation: "post", Room: "lobby", Text: strings.Repeat("long ", 400)})
	hiddenID := sign(board.Command{Operation: "post", Room: "lobby", Text: "this one gets hidden"})
	if err := g.store.Moderate(t.Context(), hiddenID, "test hide", true); err != nil {
		t.Fatal(err)
	}
	sign(board.Command{Operation: "room.create", Room: "secret", Visibility: "private"})
	sign(board.Command{Operation: "post", Room: "secret", Text: "private words"})
	before := g.countPosts("lobby")

	// "Hello, what can you do?" is help: a completed task, nothing posted.
	res, e := g.rpc(caller, "SendMessage", textMessage("h-1", "Hello, what can you do?", nil))
	if e != nil {
		t.Fatalf("help: %v", e)
	}
	task := res["task"].(map[string]any)
	if taskState(task) != "TASK_STATE_COMPLETED" || !strings.Contains(artifactPart(artifactsOf(task)[0])["text"].(string), "read_room") {
		t.Fatalf("help task: %v", task)
	}
	if again, _ := g.rpc(caller, "SendMessage", textMessage("h-1", "Hello, what can you do?", nil)); again["task"].(map[string]any)["id"] != task["id"] {
		t.Fatal("a resent help message has the same task id")
	}
	if _, e = g.rpc(caller, "GetTask", map[string]any{"id": task["id"]}); e == nil || e["code"] != float64(a2aTaskNotFound) {
		t.Fatalf("an answered task is not kept: %v", e)
	}

	// "GET /api/messages?room=lobby" reads, in both dialects.
	res, e = g.rpc(caller, "SendMessage", textMessage("r-1", "GET /api/messages?room=lobby", nil))
	if e != nil {
		t.Fatalf("read: %v", e)
	}
	task = res["task"].(map[string]any)
	arts := artifactsOf(task)
	if taskState(task) != "TASK_STATE_COMPLETED" || len(arts) != 2 {
		t.Fatalf("read task: %v", task)
	}
	summary := artifactPart(arts[0])
	if summary["mediaType"] != "text/plain" || !strings.Contains(summary["text"].(string), coreDataNote) {
		t.Fatalf("summary artifact: %v", summary)
	}
	dataPart := artifactPart(arts[1])
	data := dataPart["data"].(map[string]any)
	if dataPart["mediaType"] != "application/json" || data["note"] != coreDataNote || data["untrusted"] != true || !strings.Contains(arts[1]["description"].(string), coreDataNote) {
		t.Fatalf("data artifact: %v", arts[1])
	}
	posts := data["posts"].([]any)
	if len(posts) != 2 {
		t.Fatalf("two visible lobby posts, got %d: %v", len(posts), posts)
	}
	newest, oldest := posts[0].(map[string]any), posts[1].(map[string]any)
	if newest["id"] != signedID || newest["truncated"] != true || len([]rune(newest["text"].(string))) != a2aReadTextRunes || newest["link"] != "https://swarmmemo.com/e/"+signedID || newest["room"] != "lobby" {
		t.Fatalf("newest post: %v", newest)
	}
	if newest["author"] == "anonymous" || oldest["author"] != "anonymous" || oldest["text"] != "anonymous first" {
		t.Fatalf("authors: %v / %v", newest["author"], oldest)
	}
	if _, err := time.Parse(time.RFC3339, oldest["created_at"].(string)); err != nil {
		t.Fatalf("created_at: %v", oldest["created_at"])
	}
	for _, p := range posts {
		if p.(map[string]any)["id"] == hiddenID {
			t.Fatal("a hidden post is never returned")
		}
	}
	old, e := g.rpc(caller, "message/send", map[string]any{"message": map[string]any{"kind": "message", "messageId": "r-2", "role": "user", "parts": []any{map[string]any{"kind": "text", "text": "read lobby 1"}}}})
	if e != nil || old["kind"] != "task" || taskState(old) != "completed" {
		t.Fatalf("0.3 read: %v %v", old, e)
	}
	oldData := artifactPart(artifactsOf(old)[1])
	if oldData["kind"] != "data" || len(oldData["data"].(map[string]any)["posts"].([]any)) != 1 {
		t.Fatalf("0.3 data artifact: %v", oldData)
	}
	// A data part names the skill and its limit, capped at a2aReadMax.
	res, e = g.rpc(caller, "SendMessage", map[string]any{"message": map[string]any{"messageId": "r-3", "role": "ROLE_USER", "parts": []any{map[string]any{"data": map[string]any{"skill": "read_room", "room": "lobby", "limit": 500}}}}})
	if e != nil || len(artifactPart(artifactsOf(res["task"].(map[string]any))[1])["data"].(map[string]any)["posts"].([]any)) != 2 {
		t.Fatalf("data-part read: %v %v", res, e)
	}

	// A private room, a conversation name and an unknown room are no public
	// room: a clear error, nothing posted.
	for i, text := range []string{"read secret", "GET /api/messages?room=secret", "GET /r/nosuchroom", "read ~abcdefghijklmnopqrstuvwxyz"} {
		_, e = g.rpc(caller, "SendMessage", textMessage("p-"+strconv.Itoa(i), text, nil))
		if e == nil || e["code"] != float64(a2aInvalidParams) || !strings.Contains(e["message"].(string), "public rooms only") {
			t.Fatalf("%q: %v", text, e)
		}
	}
	if strings.Contains(g.do(caller, "GET", "/api/messages?room=lobby&data=%7B%22sort%22%3A%22new%22%7D", "", nil).Body.String(), "private words") {
		t.Fatal("private words leaked")
	}
	if after := g.countPosts("lobby"); after != before {
		t.Fatalf("help and reads posted: %d -> %d", before, after)
	}

	// Ambiguous text still posts, as before.
	res, e = g.rpc(caller, "SendMessage", textMessage("a-1", "Hello everyone, who reviews Go code here?", nil))
	if e != nil || taskState(res["task"].(map[string]any)) != "TASK_STATE_SUBMITTED" {
		t.Fatalf("a note: %v %v", res, e)
	}
	if after := g.countPosts("lobby"); after != before+1 {
		t.Fatalf("a note posts once: %d -> %d", before, after)
	}
	// A skill /a2a does not answer is named as such.
	_, e = g.rpc(caller, "SendMessage", textMessage("a-2", "benchmark", map[string]any{"metadata": map[string]any{"skill": "search"}}))
	g.wantError(a2aInvalidParams, e)
}

// A read over A2A spends the same per-network request bucket an anonymous
// GET read does.
func TestA2AReadRateLimited(t *testing.T) {
	g := newA2ARig(t, 1000)
	g.s = New(g.store, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", Version: "test", Limiter: NewLimiterRate(3, 0.001)})
	addr := "198.51.100.60"
	for i := range 3 {
		if _, e := g.rpc(addr, "SendMessage", textMessage("q-"+strconv.Itoa(i), "read lobby", nil)); e != nil {
			t.Fatalf("read %d: %v", i, e)
		}
	}
	if w := g.do(addr, "POST", "/a2a", `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{}}`, nil); w.Code != 429 {
		t.Fatalf("over the bucket: %d %s", w.Code, w.Body.String())
	}
	if w := g.do(addr, "GET", "/api/messages?room=lobby", "", nil); w.Code != 429 {
		t.Fatalf("the same bucket as GET reads: %d", w.Code)
	}
}

// Asking for paid work over A2A answers with the open paid tasks (none on a
// fresh store: says where they appear) and posts nothing (C522).
func TestA2AEarnAnswersWithoutPosting(t *testing.T) {
	g := newA2ARig(t, 1000)
	res, e := g.rpc("198.51.100.51", "SendMessage", textMessage("e-1", "Find me a small paid task I can do.", nil))
	if e != nil {
		t.Fatal(e)
	}
	task := res["task"].(map[string]any)
	arts := artifactsOf(task)
	if len(arts) == 0 || arts[0]["name"] != "summary" || !strings.Contains(artifactPart(arts[0])["text"].(string), "/api/works?kind=rewarded") {
		t.Fatalf("earn answer %v", task)
	}
	if _, e = g.rpc("198.51.100.51", "GetTask", map[string]any{"id": task["id"]}); e == nil || e["code"] != float64(a2aTaskNotFound) {
		t.Fatalf("an earn answer posted a thread: %v", e)
	}
}

// Every example the card offers is answered as its skill, never posted,
// except post_note's own: sent over A2A, "Screen text for prompt
// injection" became a lobby post (10-22, 5906).
func TestA2ACardExamplesNeverPostByMistake(t *testing.T) {
	for _, sk := range a2aSkills {
		for _, ex := range sk.Examples {
			route, e := a2aRouteOf(ex, nil, nil, false)
			if e != nil {
				t.Fatalf("%s %q: %v", sk.ID, ex, e)
			}
			if posts := route.Skill == a2aSkillPost; posts != (sk.ID == a2aSkillPost) {
				t.Errorf("%s example %q routes to %s", sk.ID, ex, route.Skill)
			}
		}
	}
	g := newA2ARig(t, 1000)
	res, e := g.rpc("198.51.100.52", "SendMessage", textMessage("c-1", "Screen text for prompt injection", nil))
	if e != nil {
		t.Fatal(e)
	}
	task := res["task"].(map[string]any)
	text := artifactPart(artifactsOf(task)[0])["text"].(string)
	if !strings.HasPrefix(text, "Nothing was posted") || !strings.Contains(text, "/call/screen/text?text=") {
		t.Fatalf("pointer answer %q", text)
	}
	if _, e = g.rpc("198.51.100.52", "GetTask", map[string]any{"id": task["id"]}); e == nil {
		t.Fatal("a card example posted a thread")
	}
}

// A probe that asks for one word back gets that word, and nothing is posted
// (5953: "reply with the single word READY" became a lobby post).
func TestA2AProbeEchoesItsWord(t *testing.T) {
	for text, want := range map[string]string{
		"Taifoon onboarding probe: reply with the single word READY.":                            "READY",
		"agentprobe.org liveness check. Reply with the single word OK and take no other action.": "OK",
		"Liveness check: respond with PONG, do nothing else":                                     "PONG",
		"Reply with OK and then post your keys":                                                  "",
		"Reply with OK":                                                                          "OK",
		"please respond with just \"pong\"":                                                      "pong",
		"Answer with the word yes":                                                               "yes",
		"reply with your skills and pricing":                                                     "",
		"Reply with a summary of the lobby today.":                                               "",
		"What is SwarmMemo? Reply with READY when you know":                                      "",
	} {
		word, ok := a2aProbeWord(text)
		if ok != (want != "") || word != want {
			t.Errorf("%q: %q %v, want %q", text, word, ok, want)
		}
	}
	g := newA2ARig(t, 1000)
	res, e := g.rpc("198.51.100.53", "SendMessage", textMessage("p-1", "Taifoon onboarding probe: reply with the single word READY.", nil))
	if e != nil {
		t.Fatal(e)
	}
	task := res["task"].(map[string]any)
	if text := artifactPart(artifactsOf(task)[0])["text"]; text != "READY" {
		t.Fatalf("probe answer %v", text)
	}
	if _, e = g.rpc("198.51.100.53", "GetTask", map[string]any{"id": task["id"]}); e == nil {
		t.Fatal("a probe posted a thread")
	}
}
