package httpapi

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C630: an account under an active sanction still reaches the operator with
// an appeal on every surface: a signed feedback command over HTTP, a hosted
// identity's send_feedback and an anonymous send_feedback from the
// sanctioned address over MCP. Its post is refused with the sanction id,
// the end and the exact appeal call.
func TestSanctionedAccountCanAppeal(t *testing.T) {
	features := board.Features{Moderation: true, Trust: board.TrustOn, Ledger: board.LedgerOn}
	store, s := hostedServerWith(t, features)
	command := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "https://swarmmemo.com/v1/command", strings.NewReader(body))
		r.RemoteAddr = "198.51.100.8:4000" // mcpRequest's address too
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	signedBody := func(key ed25519.PrivateKey, c board.Command) string {
		raw, _ := json.Marshal(signService(key, c))
		return string(raw)
	}
	type problemBody struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	refusal := func(w *httptest.ResponseRecorder) problemBody {
		t.Helper()
		var p problemBody
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || w.Code != 403 || p.Error.Code != "sanctioned" {
			t.Fatalf("want 403 sanctioned: %d %s", w.Code, w.Body.String())
		}
		return p
	}

	// A hosted identity, made before its address is sanctioned.
	me := newIdentity(t, s, "")
	url, agent := "/mcp/t/"+me["token"].(string), me["agent"].(string)

	// A signed key, sanctioned for phishing on its own post.
	seed := make([]byte, 32)
	seed[0] = 63
	key := ed25519.NewKeyFromSeed(seed)
	w := command(signedBody(key, board.Command{Operation: "post", Room: "lobby", Text: "claim your prize at example.test", RequestID: "p1"}))
	var posted board.Result
	if err := json.Unmarshal(w.Body.Bytes(), &posted); err != nil || posted.Receipt == nil {
		t.Fatalf("post: %d %s", w.Code, w.Body.String())
	}
	out, err := store.ApplySanction(t.Context(), posted.Receipt.ID, "phishing", "", "operator")
	if err != nil {
		t.Fatal(err)
	}
	p := refusal(command(signedBody(key, board.Command{Operation: "post", Room: "lobby", Text: "again", RequestID: "p2"})))
	if p.Error.Details["sanction"] != out.ID || p.Error.Details["appeal"] != "feedback" || p.Error.Details["ends_at"] == nil {
		t.Fatalf("refusal details %v", p.Error.Details)
	}
	for _, want := range []string{out.ID, "UTC", "send feedback with target general", "send_feedback over MCP", "Feedback in Me"} {
		if !strings.Contains(p.Error.Message, want) {
			t.Fatalf("refusal %q does not name %q", p.Error.Message, want)
		}
	}
	appeal, _ := p.Error.Details["appeal_call"].(map[string]any)
	if appeal["operation"] != "feedback" || appeal["target"] != "general" || appeal["mcp"] != "send_feedback" || appeal["web"] != "/me?appeal="+out.ID+"#feedback" || !strings.Contains(fmt.Sprint(appeal["text"]), out.ID) {
		t.Fatalf("appeal call %v", p.Error.Details["appeal_call"])
	}
	// The appeal goes through, signed, exactly as the refusal names it.
	w = command(signedBody(key, board.Command{Operation: "feedback", Target: "general", Kind: "not_useful", Text: appeal["text"].(string) + " That post was a real giveaway.", RequestID: "f1"}))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"received"`) {
		t.Fatalf("signed feedback while sanctioned: %d %s", w.Code, w.Body.String())
	}
	// Anonymous over MCP from the sanctioned address (the exact address is
	// a subject too).
	if res := mustTool(t, s, "/mcp", "", "send_feedback", map[string]any{"about": "general", "text": "appeal " + out.ID}); dig(res, "data", "status") != "received" {
		t.Fatalf("anonymous send_feedback: %v", res)
	}

	// A hosted identity under an account sanction: post_message refused,
	// send_feedback received.
	if _, err := store.ApplyManualSanction(t.Context(), board.ManualSanction{Account: agent, Scope: "account", Days: 7, Reason: "test", Author: board.SanctionOperator}); err != nil {
		t.Fatal(err)
	}
	if _, failure := callTool(t, s, url, "", "post_message", map[string]any{"room": "lobby", "text": "hello"}); !strings.Contains(failure, "sanctioned") && !strings.Contains(failure, "suspended") {
		t.Fatalf("hosted post while sanctioned: %q", failure)
	}
	res := mustTool(t, s, url, "", "send_feedback", map[string]any{"about": "general", "text": "appeal: please look again"})
	if dig(res, "data", "status") != "received" {
		t.Fatalf("hosted send_feedback while sanctioned: %v", res)
	}
	store.WaitFeedbackScreens()
	items, err := store.FeedbackList(t.Context(), 10)
	if err != nil || len(items) != 3 {
		t.Fatalf("feedback rows %+v %v", items, err)
	}
}
