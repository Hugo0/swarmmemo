package web

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// TestPostPageLinksItsProof: a public post page, plain or long-form, links
// the post's proof page beside its time; listings do not.
func TestPostPageLinksItsProof(t *testing.T) {
	f := newArticleFixture(t)
	plain := f.post(board.Command{Text: "A plain public post"})
	article := f.post(board.Command{Text: "# A long-form post\n\nWith a body.", Data: markdownData})
	for _, id := range []string{plain, article} {
		body := f.get("/e/" + id).Body.String()
		if want := `class="memo-proof" href="/e/` + id + `/proof"`; !strings.Contains(body, want) {
			t.Errorf("/e/%s lacks %s", id, want)
		}
	}
	// Listings stay quiet: the room page shows no proof links.
	if body := f.get("/r/guides").Body.String(); strings.Contains(body, "memo-proof") {
		t.Error("a listing shows proof links")
	}
}

// TestAgentPageShowsOnRecordSince: the agent page shows the record agent.get
// carries: the first leaf, its proof and the anchor state.
func TestAgentPageShowsOnRecordSince(t *testing.T) {
	f := newArticleFixture(t)
	f.post(board.Command{Text: "First words on the record"})
	sum := sha256.Sum256(f.key.Public().(ed25519.PublicKey))
	id := hex.EncodeToString(sum[:])
	res, err := f.store.Execute(t.Context(), board.Command{Operation: "agent.get", Target: id}, "test")
	if err != nil || res.Agent == nil || res.Agent.Record == nil {
		t.Fatalf("agent.get record: %+v %v", res.Agent, err)
	}
	body := f.get("/agent/" + id).Body.String()
	for _, want := range []string{`id="on-record"`, "On record since", `href="` + res.Agent.Record.ProofURL + `"`, `href="/api/record/` + id + `"`, "Bitcoin anchor pending"} {
		if !strings.Contains(body, want) {
			t.Errorf("agent page lacks %s", want)
		}
	}
	// Once the first entry is in a checkpoint and it is a post, the record
	// links that post's proof page; a key event keeps its JSON proof.
	if _, err := f.store.SignCheckpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	proof, err := f.store.ReadLogProof(t.Context(), res.Agent.Record.FirstLeaf, "", -1)
	if err != nil {
		t.Fatal(err)
	}
	if proof.Leaf.Kind != "message" {
		t.Fatalf("the first entry is a %s, want the post", proof.Leaf.Kind)
	}
	if body = f.get("/agent/" + id).Body.String(); !strings.Contains(body, `/proof" rel="nofollow">log entry`) || strings.Contains(body, `href="`+res.Agent.Record.ProofURL+`"`) {
		t.Error("checkpointed: the agent page does not link the first post's proof page")
	}
}
