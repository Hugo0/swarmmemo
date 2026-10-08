package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"

	"swarmmemo/internal/board"
)

// C55: /api/agent/ID (by fingerprint or handle) and /api/record/WHO carry
// absolute urls on the configured public URL, by fingerprint, so a reader of
// one reaches the others without guessing paths. MCP read_agent and
// agent_record carry the same.
func TestAgentAnswersCarryURLs(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "urls.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := New(store, nil, Config{ServiceID: "swarmmemo.com", PublicURL: "https://board.example/"})
	ctx := context.Background()
	signedPost := func(seed byte, handle string) string {
		key := ed25519.NewKeyFromSeed(append([]byte{seed}, make([]byte, 31)...))
		raw, _ := json.Marshal(signService(key, board.Command{Operation: "post", Room: "lobby", Text: "urls test", Handle: handle}))
		if w := makeRequest(s, "POST", "https://swarmmemo.com/v1/command", string(raw), "application/json"); w.Code != 200 {
			t.Fatalf("post: %d %s", w.Code, w.Body)
		}
		sum := sha256.Sum256(key.Public().(ed25519.PublicKey))
		return hex.EncodeToString(sum[:])
	}
	named := signedPost(1, "urlbot")
	plain := signedPost(2, "")
	if _, err = store.SignCheckpoint(ctx); err != nil {
		t.Fatal(err)
	}
	want := func(id string) board.AgentURLs {
		return board.AgentURLs{Web: "https://board.example/agent/" + id, API: "https://board.example/api/agent/" + id, Record: "https://board.example/api/record/" + id}
	}
	for _, tc := range []struct{ who, id string }{{"urlbot", named}, {named, named}, {plain, plain}} {
		w := get(s, "/api/agent/"+tc.who, "")
		var agent struct {
			Agent struct {
				Record *board.AgentRecord
				URLs   *board.AgentURLs `json:"urls"`
			}
		}
		if err := json.Unmarshal(w.Body.Bytes(), &agent); err != nil || w.Code != 200 || agent.Agent.URLs == nil || agent.Agent.Record == nil {
			t.Fatalf("/api/agent/%s: %d %s", tc.who, w.Code, w.Body)
		}
		u := want(tc.id)
		u.Proof = "https://board.example" + agent.Agent.Record.ProofURL
		if *agent.Agent.URLs != u {
			t.Errorf("/api/agent/%s urls = %+v, want %+v", tc.who, *agent.Agent.URLs, u)
		}
		w = get(s, "/api/record/"+tc.who, "")
		var rec struct {
			URLs *board.AgentURLs `json:"urls"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil || w.Code != 200 || rec.URLs == nil || *rec.URLs != want(tc.id) {
			t.Errorf("/api/record/%s: %d %s", tc.who, w.Code, w.Body)
		}
		_, out, err := s.mcpAgentRecord(ctx, agentRecordInput{Agent: tc.who})
		if err != nil || *out.Data["urls"].(*board.AgentURLs) != want(tc.id) {
			t.Errorf("agent_record %s: %v %s", tc.who, err, mustJSON(t, out))
		}
	}
	out := mustTool(t, s, "/mcp", "", "read_agent", map[string]any{"target": "urlbot"})
	if got := dig(out, "agent", "urls", "record"); got != want(named).Record {
		t.Fatalf("read_agent urls: %s", mustJSON(t, out))
	}
}
