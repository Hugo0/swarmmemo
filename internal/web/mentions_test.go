package web

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// A post links each @handle of a registered agent to the agent's page, in
// plain text and Markdown alike; an unknown handle and a code span stay text.
func TestPostLinksRegisteredMentions(t *testing.T) {
	f := newArticleFixture(t)
	c := board.Command{Operation: "agent.register", Handle: "writer", PublicKey: base64.RawURLEncoding.EncodeToString(f.key.Public().(ed25519.PublicKey)), Timestamp: time.Now().Unix(), Nonce: "mention-register"}
	c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(f.key, board.Canonical("swarmmemo.com", c)))
	if _, err := f.store.Execute(context.Background(), c, "test"); err != nil {
		t.Fatal(err)
	}
	agent, err := f.store.Execute(context.Background(), board.Command{Operation: "agent.get", Target: "writer"}, "test")
	if err != nil || agent.Agent == nil {
		t.Fatalf("agent.get: %v", err)
	}
	link := `<a class="mention" href="/agent/` + agent.Agent.ID + `">@Writer</a>`
	for name, data := range map[string]string{"text": "", "markdown": markdownData} {
		id := f.post(board.Command{Room: "lobby", Text: "thanks @Writer, not @ghost, not `@writer`", Data: data})
		body := f.get("/e/" + id).Body.String()
		if strings.Count(body, `class="mention"`) != 1 || !strings.Contains(body, link) {
			t.Errorf("%s: want one link %s", name, link)
		}
		if strings.Contains(body, `href="/agent/ghost"`) {
			t.Errorf("%s: an unknown handle was linked", name)
		}
	}
}
