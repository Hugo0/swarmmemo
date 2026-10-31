package httpapi

import (
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// read_thread of a thread the caller cannot read answers exactly as a
// missing id does, for every caller and wherever the private posts sit:
// its root, a reply, a public reply whose parent is private and a private
// reply under a public root (C670, realbtr's follow-up on PR #45). MCP
// connects anonymously or as a hosted identity (a path token or a bearer
// header); a signed key and an app's token read the same thread.get over
// /v1/command (an app token is never an MCP credential), and GET
// /api/thread/ID is the anonymous HTTP read. A member of a masked app room
// reads the thread with aliases, never keys.
func TestReadThreadPrivateRoomParity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.sqlite")
	store, s := hostedServerAt(t, path, board.Features{})
	ctx := t.Context()
	seed := func(c string) ed25519.PrivateKey { return ed25519.NewKeyFromSeed([]byte(strings.Repeat(c, 32))) }
	app, owner, stranger, alice, bob := ed25519.NewKeyFromSeed(make([]byte, 32)), seed("w"), seed("x"), seed("a"), seed("b")
	exec := func(key ed25519.PrivateKey, c board.Command) board.Result {
		t.Helper()
		res, err := store.Execute(ctx, signedNow(key, c), "198.51.100.9")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		return res
	}
	exec(app, board.Command{Operation: "app.register", Data: `{"schema":1,"name":"Hex Arena","homepage":"https://app.example"}`, RequestID: "a"})
	for i, k := range []ed25519.PrivateKey{owner, stranger, alice, bob} {
		exec(k, board.Command{Operation: "agent.register", Handle: []string{"owner", "stranger", "alice", "bobby"}[i]})
	}
	client := exec(app, board.Command{Operation: "oauth.client.register", RequestID: "c",
		Data: `{"schema":1,"name":"Hex Arena","redirect_uris":["` + testRedirect + `"],"scopes":["identity","app-rooms"]}`}).Data["client"].(board.OAuthAppClient)
	request, cookie := consent(t, s, authorizeQuery(client.ID, testRedirect, "st", map[string]string{"scope": "app-rooms", "resource": ""}))
	back, _ := continued(t, s, cookie, submit(s, request, cookie, map[string]string{"action": "create"}, nil))
	_, tokens, _ := tokenRequest(s, url.Values{"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {client.ID},
		"redirect_uri": {testRedirect}, "code_verifier": {testVerifier}})
	access, _ := tokens["access_token"].(string)
	person := jwtSubject(t, access)
	hosted := newIdentity(t, s, "")["token"].(string)
	post := func(key ed25519.PrivateKey, room, replyTo, text string) string {
		t.Helper()
		return exec(key, board.Command{Operation: "post", Room: room, ReplyTo: replyTo, Text: text, RequestID: randomNonce()}).Receipt.ID
	}

	// A private room only its owner reads, and an app room the person
	// signed in to the app is not in.
	exec(owner, board.Command{Operation: "room.create", Room: "hidden-lab", Visibility: "private"})
	privRoot := post(owner, "hidden-lab", "", "secret root")
	privReply := post(owner, "hidden-lab", privRoot, "secret reply")
	backroom := "app/" + client.App + "/backroom"
	exec(app, board.Command{Operation: "room.create", Room: backroom, Members: []string{keyFP(alice)}, RequestID: "r1"})
	appRoot := post(alice, backroom, "", "backroom root")
	pubRoot := post(alice, "lobby", "", "public root")
	post(bob, "lobby", pubRoot, "public reply")

	type caller struct {
		name string
		read func(id string) (answer string, ok bool)
	}
	mcpRead := func(path, auth string) func(string) (string, bool) {
		return func(id string) (string, bool) {
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "read_thread", "arguments": map[string]any{"message_id": id}}})
			out := mcpRequest(t, s, path, auth, string(raw))
			result, _ := json.Marshal(out["result"])
			return string(result), out["error"] == nil && dig(out, "result", "isError") != true
		}
	}
	httpRead := func(method, target, body string, headers map[string]string) (string, bool) {
		w := oauthDo(s, method, target, "application/json", body, headers)
		return fmt.Sprintf("%d %s", w.Code, w.Body.String()), w.Code == 200
	}
	callers := []caller{
		{"anonymous MCP", mcpRead("/mcp", "")},
		{"hosted identity MCP (path token)", mcpRead("/mcp/t/"+hosted, "")},
		{"hosted identity MCP (bearer, assistant)", mcpRead(web.AssistantMCPPath, "Bearer "+hosted)},
		{"anonymous GET /api/thread", func(id string) (string, bool) { return httpRead("GET", "/api/thread/"+id, "", nil) }},
		{"signed key /v1/command", func(id string) (string, bool) {
			raw, _ := json.Marshal(signedNow(stranger, board.Command{Operation: "thread.get", MessageID: id}))
			return httpRead("POST", "/v1/command", string(raw), nil)
		}},
		{"app token /v1/command", func(id string) (string, bool) {
			return httpRead("POST", "/v1/command", `{"operation":"thread.get","message_id":"`+id+`"}`, map[string]string{"Authorization": "Bearer " + access})
		}},
	}
	// summary is what a public thread's read says about its size and posts.
	summary := func(answer string) string {
		var out map[string]any
		_, body, _ := strings.Cut(answer, " ")
		if json.Unmarshal([]byte(body), &out) != nil {
			_ = json.Unmarshal([]byte(answer), &out)
			out, _ = out["structuredContent"].(map[string]any)
		}
		var ids []string
		for _, m := range dig(out, "messages").([]any) {
			ids = append(ids, fmt.Sprint(dig(m, "id")))
		}
		return fmt.Sprint(ids, dig(out, "data", "replies"), dig(out, "data", "last_reply_at"), dig(out, "data", "total"), dig(out, "data", "has_more"))
	}
	before := map[string]string{}
	for _, c := range callers {
		if answer, ok := c.read(pubRoot); ok {
			before[c.name] = summary(answer)
		} else if c.name != "app token /v1/command" {
			t.Fatalf("%s: the public thread: %s", c.name, answer)
		}
	}

	// Threads that cross rooms: no post makes one (a reply goes in its
	// parent's room), so they are written as rows, as a legacy board may
	// hold them. A private reply under the public root, a public reply to
	// the private root, and a public reply to an id nobody has.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	crossDown := post(owner, "hidden-lab", "", "private answer to a public post")
	crossUp := post(alice, "lobby", "", "public answer to a private post")
	orphan := post(alice, "lobby", "", "public answer to nothing")
	for id, parent := range map[string]string{crossDown: pubRoot, crossUp: privRoot, orphan: strings.Repeat("e", 32)} {
		if _, err := raw.Exec("UPDATE events SET reply_to=? WHERE id=?", parent, id); err != nil {
			t.Fatal(err)
		}
	}

	missing := strings.Repeat("0", 32)
	secrets := []string{crossDown, privRoot, "private answer", "secret", keyFP(owner)}
	for _, c := range callers {
		want, _ := c.read(missing)
		if strings.Contains(want, missing) {
			t.Fatalf("%s: the missing-id answer echoes the id: %s", c.name, want)
		}
		hidden := map[string]string{"private root": privRoot, "private reply": privReply, "public reply to a private root": crossUp,
			"private reply under a public root": crossDown, "public reply to no post": orphan, "app room, not a member": appRoot}
		for placement, id := range hidden {
			if got, _ := c.read(id); got != want {
				t.Errorf("%s, %s: answers differently from a missing id:\n%s\n%s", c.name, placement, got, want)
			}
		}
		// The public thread reads as it did before the private reply under it.
		if prior, ok := before[c.name]; ok {
			answer, _ := c.read(pubRoot)
			if got := summary(answer); got != prior {
				t.Errorf("%s: the public thread changed with a private reply under it: %s, was %s", c.name, got, prior)
			}
			for _, secret := range secrets {
				if strings.Contains(answer, secret) {
					t.Errorf("%s: the public thread names %q: %s", c.name, secret, answer)
				}
			}
		}
	}
	// The private room's member reads its thread, and only its own posts.
	member := exec(owner, board.Command{Operation: "thread.get", MessageID: privReply})
	if len(member.Messages) != 2 || member.Messages[0].ID != privRoot || member.Messages[1].ID != privReply || member.Data["replies"] != int64(1) {
		t.Fatalf("the member's thread: %+v %v", member.Messages, member.Data)
	}

	// A masked app room: its members read the thread under aliases, over a
	// signed key and the app's token alike; anyone else reads a missing id.
	mafia := "app/" + client.App + "/mafia"
	exec(app, board.Command{Operation: "room.create", Room: mafia, Members: []string{person, keyFP(alice), keyFP(bob)}, Data: `{"reveal_on_close":true}`, RequestID: "r2"})
	maskedRoot := post(alice, mafia, "", "I am the villager")
	post(bob, mafia, maskedRoot, "sure you are")
	reads := map[string]string{}
	reads["alice, signed"], _ = httpRead("POST", "/v1/command", func() string {
		raw, _ := json.Marshal(signedNow(alice, board.Command{Operation: "thread.get", MessageID: maskedRoot}))
		return string(raw)
	}(), nil)
	reads["the person, app token"], _ = callers[5].read(maskedRoot)
	for who, answer := range reads {
		if !strings.HasPrefix(answer, "200 ") || strings.Count(answer, `"author":"`+board.MaskAliasPrefix) != 2 || !strings.Contains(answer, `"masked":true`) {
			t.Fatalf("%s reads the masked thread: %s", who, answer)
		}
		for _, secret := range []string{keyFP(bob), signedNow(bob, board.Command{}).PublicKey, "bobby", keyFP(alice), "alice"} {
			if strings.Contains(answer, secret) {
				t.Errorf("%s reads %q in the masked thread: %s", who, secret, answer)
			}
		}
	}
	for _, c := range callers[:5] {
		want, _ := c.read(missing)
		if got, _ := c.read(maskedRoot); got != want {
			t.Errorf("%s: a masked room's thread answers differently from a missing id:\n%s\n%s", c.name, got, want)
		}
	}
}
