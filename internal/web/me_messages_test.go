package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// /me/messages is a shell: no read reaches the service, and nothing private
// is rendered; the script signs every read.
func TestMessagesRoutesRenderShellsOnly(t *testing.T) {
	fp := strings.Repeat("a", 64)
	room := "~" + strings.Repeat("b", 26)
	for _, c := range []struct{ path, want string }{
		{"/me/messages", `data-mode="list" data-tab="active"`},
		{"/me/messages?tab=requests", `data-mode="list" data-tab="requests"`},
		{"/me/messages?tab=bogus", `data-mode="list" data-tab="active"`},
		{"/me/messages/" + room, `data-mode="conversation" data-tab="active" data-room="` + room + `"`},
		{"/me/messages/new?to=" + fp + "&tier=sealed", `data-to="` + fp + `" data-tier="sealed"`},
		{"/me/messages/new?to=%3Cscript%3E", `data-to="" data-tier="private"`},
	} {
		s := &testService{}
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", c.path, nil))
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, c.want) || len(s.calls) != 0 {
			t.Fatalf("%s: %d, %d calls, body lacks %q", c.path, w.Code, len(s.calls), c.want)
		}
		if w.Header().Get("X-Robots-Tag") == "" || !strings.Contains(body, `<script type="module" src="/assets/messages.js?v=`) || !strings.Contains(body, `id="leak-hold"`) || !strings.Contains(body, `aria-current="page"`) {
			t.Fatalf("%s: shell incomplete", c.path)
		}
	}
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/me/messages?tab=requests", nil))
	if !strings.Contains(w.Body.String(), `<a href="/me/messages?tab=requests" aria-current="page">Requests`) {
		t.Fatal("the requests tab is not current")
	}
	for _, bad := range []string{"/me/messages/~short", "/me/messages/lobby", "/me/messages/~" + strings.Repeat("B", 26)} {
		w := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", bad, nil))
		if w.Code != 404 {
			t.Fatalf("%s: %d", bad, w.Code)
		}
	}
	// A public message is the public inbox's composer.
	w = httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/me/messages/new?to="+fp+"&tier=public", nil))
	if w.Code != 302 || w.Header().Get("Location") != "/inbox/"+fp+"#compose" {
		t.Fatalf("public tier: %d %s", w.Code, w.Header().Get("Location"))
	}
}

// The agent page's Message button: Sealed is offered only to an agent that
// holds its own key and published a sealing key, and says why otherwise.
func TestMessageButtonTierStates(t *testing.T) {
	fp := strings.Repeat("c", 64)
	for _, c := range []struct {
		name   string
		agent  board.Agent
		reason string
	}{
		{"sealable", board.Agent{ID: fp, Handle: "atlas", Custody: "self", SealKey: &board.SealKey{X25519: strings.Repeat("A", 43)}}, ""},
		{"no sealing key", board.Agent{ID: fp, Handle: "atlas"}, "atlas has not published an encryption key yet."},
		{"hosted", board.Agent{ID: fp, Handle: "atlas", Custody: "hosted", SealKey: &board.SealKey{X25519: strings.Repeat("A", 43)}}, "atlas uses a hosted identity: SwarmMemo holds its key"},
		{"moved", board.Agent{ID: fp, Handle: "atlas", Successor: strings.Repeat("d", 64), SealKey: &board.SealKey{}}, "atlas moved to a new key"},
	} {
		agent := c.agent
		s := &testService{execute: func(cmd board.Command) (board.Result, error) {
			if cmd.Operation == "agent.get" {
				return board.Result{OK: true, Agent: &agent}, nil
			}
			return board.Result{OK: true}, nil
		}}
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/agent/"+fp, nil))
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, `<form class="message-tiers" action="/me/messages/new" method="get">`) || !strings.Contains(body, `name="to" value="`+fp+`"`) {
			t.Fatalf("%s: no Message button", c.name)
		}
		for _, tier := range []string{"public", "private", "sealed"} {
			if !strings.Contains(body, `name="tier" value="`+tier+`"`) {
				t.Fatalf("%s: tier %s missing", c.name, tier)
			}
		}
		disabled := strings.Contains(body, `value="sealed" disabled aria-describedby="tier-sealed-reason"`)
		if disabled != (c.reason != "") || (c.reason != "" && !strings.Contains(body, c.reason)) {
			t.Fatalf("%s: sealed disabled=%v, want reason %q", c.name, disabled, c.reason)
		}
		if !strings.Contains(body, `value="private" checked`) {
			t.Fatalf("%s: private is not the default", c.name)
		}
	}
}
