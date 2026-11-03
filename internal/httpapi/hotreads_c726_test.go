package httpapi

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// The agent directory (/api/agents) is kept for anonymous readers by its
// whole query, when that names only what is listed; the kept answer is the
// store's bytes exactly. Credentials, a cookie, a signed or any other query
// read the store and are never kept (C726).
func TestHotReadsKeepTheAgentDirectory(t *testing.T) {
	f := newHotFixture(t)
	key := ed25519.NewKeyFromSeed([]byte(strings.Repeat("d", 32)))
	f.run(t, signService(key, board.Command{Operation: "post", Room: "lobby", Text: "listed", RequestID: "dir-1"}))
	for i, target := range []string{"/api/agents", "/api/agents?format=json", "/api/agents?sort=new&limit=5", "/api/agents?query=notary"} {
		first := f.getWith(target, "198.51.100.8:1000")
		if first.Code != 200 || f.store.count("agents.list") != i+1 {
			t.Fatalf("%s: %d after %d reads: %s", target, first.Code, f.store.count("agents.list"), first.Body)
		}
		again := f.getWith(target, "192.0.2.77:2000")
		if again.Body.String() != first.Body.String() || again.Header().Get("Content-Type") != first.Header().Get("Content-Type") || f.store.count("agents.list") != i+1 {
			t.Fatalf("%s: the repeat read the store (%d) or differs", target, f.store.count("agents.list"))
		}
	}
	// The kept answer is what an uncached read answers.
	fresh := f.getWith("/api/agents", "198.51.100.8:1000", "Authorization", "Bearer x")
	if fresh.Code == 200 && fresh.Body.String() != f.getWith("/api/agents", "192.0.2.1:1").Body.String() {
		t.Fatalf("kept directory differs from a fresh read")
	}
	kept := f.s.hotCache().Len()
	for _, c := range []struct {
		target string
		header []string
	}{
		{"/api/agents", []string{"Authorization", "Bearer x"}},
		{"/api/agents", []string{"Cookie", "a=b"}},
		{"/api/agents?public_key=AAAA&signature=BBBB&timestamp=1&nonce=n", nil},
		{"/api/agents?room=lobby", nil},
		{"/who", nil},
	} {
		n := f.store.count("agents.list")
		f.getWith(c.target, "198.51.100.8:1000", c.header...)
		f.getWith(c.target, "198.51.100.8:1000", c.header...)
		if f.store.count("agents.list") == n+1 {
			t.Fatalf("%s %v was kept", c.target, c.header)
		}
	}
	if f.s.hotCache().Len() != kept {
		t.Fatalf("keyed reads were kept: %d entries, want %d", f.s.hotCache().Len(), kept)
	}
	f.advance(hotReadTTL)
	n := f.store.count("agents.list")
	if f.getWith("/api/agents", "198.51.100.8:1000"); f.store.count("agents.list") != n+1 {
		t.Fatal("kept past hotReadTTL")
	}
}

// An anonymous agent read the cache does not hold takes one of
// agentReadTurns turns; with none free within agentReadWait it is answered
// 503 busy with Retry-After, while a kept answer and a keyed read are still
// served (C726).
func TestHotReadsCapAgentReads(t *testing.T) {
	f := newHotFixture(t)
	key := ed25519.NewKeyFromSeed([]byte(strings.Repeat("t", 32)))
	f.run(t, signService(key, board.Command{Operation: "post", Room: "lobby", Text: "turns", RequestID: "turn-1"}))
	agent := "/api/agent/" + fingerprintOf(key)
	if w := f.getWith(agent, "198.51.100.8:1000"); w.Code != 200 {
		t.Fatalf("first read: %d %s", w.Code, w.Body)
	}
	f.s.hotCache()
	for range agentReadTurns {
		f.s.hot.agents <- struct{}{}
	}
	defer func() {
		for range agentReadTurns {
			<-f.s.hot.agents
		}
	}()
	if w := f.getWith(agent, "192.0.2.1:1000"); w.Code != 200 {
		t.Fatalf("a kept answer waited for a turn: %d", w.Code)
	}
	start := time.Now()
	n := f.store.count("agent.get")
	busy := f.getWith(agent+"?format=json", "192.0.2.1:1000")
	if busy.Code != 503 || busy.Header().Get("Retry-After") != "5" || !strings.Contains(busy.Body.String(), `"busy"`) || f.store.count("agent.get") != n {
		t.Fatalf("past the turns: %d %v %s (store reads %d)", busy.Code, busy.Header(), busy.Body, f.store.count("agent.get")-n)
	}
	if waited := time.Since(start); waited < agentReadWait {
		t.Fatalf("busy after %v, before waiting %v", waited, agentReadWait)
	}
	if w := f.getWith(agent+"?room=lobby", "192.0.2.1:1000"); w.Code == 503 {
		t.Fatalf("a keyed read waited for a turn: %d", w.Code)
	}
	if f.s.hotCache().Len() != 1 {
		t.Fatalf("a busy answer was kept: %d entries", f.s.hotCache().Len())
	}
}
