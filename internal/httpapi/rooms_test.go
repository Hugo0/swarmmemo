package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

type roomKey struct {
	private ed25519.PrivateKey
	id      string
	nonce   int
}

func newRoomKey(t *testing.T) *roomKey {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(public)
	return &roomKey{private: private, id: hex.EncodeToString(sum[:])}
}

func (k *roomKey) sign(c board.Command) board.Command {
	k.nonce++
	c.PublicKey = base64.RawURLEncoding.EncodeToString(k.private.Public().(ed25519.PublicKey))
	c.Timestamp = time.Now().Unix()
	c.Nonce = fmt.Sprintf("room-%s-%d", k.id[:8], k.nonce)
	c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(k.private, board.Canonical("swarmmemo.com", c)))
	return c
}

func roomFixture(t *testing.T) (*board.Store, *Server, *roomKey) {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "board.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner := newRoomKey(t)
	for _, c := range []board.Command{
		{Operation: "agent.register", Handle: "publisher"},
		{Operation: "room.create", Room: "garden"},
		{Operation: "room.policy.set", Room: "garden", Data: `{"write":"owner","rules":"Owner posts; everyone replies."}`},
		{Operation: "post", Room: "garden", Text: "garden root"},
		{Operation: "post", Room: board.PersonalRoom(owner.id), Text: "First article"},
	} {
		if _, err := store.Execute(context.Background(), owner.sign(c), "fixture"); err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
	}
	return store, New(store, nil, Config{ServiceID: "swarmmemo.com"}), owner
}

func secureRequest(s http.Handler, method, target, body, ct string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.RemoteAddr = "198.51.100.8:12345"
	r.TLS = &tls.ConnectionState{}
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// Every HTTP write alias reaches the one post path, so each obeys room
// policy: a top-level post to an owner-only room fails however it arrives,
// and a personal room refuses a stranger on every one of them.
func TestRoomPolicyOnEveryHTTPWritePath(t *testing.T) {
	store, s, owner := roomFixture(t)
	stranger := newRoomKey(t)
	personal := board.PersonalRoom(owner.id)
	for _, room := range []string{"garden", personal} {
		b64 := base64.RawURLEncoding.EncodeToString([]byte("top level"))
		signed, _ := json.Marshal(stranger.sign(board.Command{Operation: "post", Room: room, Page: "main", Text: "signed top level"}))
		envelope := base64.RawURLEncoding.EncodeToString(signed)
		for _, tc := range []struct{ name, method, path, body, ct string }{
			{"get query", "GET", "/w/" + room + "/main?text=top%20level", "", ""},
			{"get base64", "GET", "/w64/" + room + "/main/" + b64, "", ""},
			{"post raw", "POST", "/w/" + room + "/main", "top level", "text/plain"},
			{"post form", "POST", "/w/" + room + "/main", "text=top+level", "application/x-www-form-urlencoded"},
			{"post json", "POST", "/w/" + room + "/main", `{"text":"top level"}`, "application/json"},
			{"put", "PUT", "/v1/events/policy-put-" + room[:3], `{"room":"` + room + `","page":"main","text":"top level"}`, "application/json"},
			{"mkcol", "MKCOL", "/w64/" + room + "/main/" + b64, "", ""},
			{"command", "POST", "/v1/command", `{"operation":"post","room":"` + room + `","text":"top level"}`, "application/json"},
			{"signed command", "POST", "/v1/command", string(signed), "application/json"},
			{"c64", "GET", "/c64/" + envelope, "", ""},
		} {
			w := secureRequest(s, tc.method, tc.path, tc.body, tc.ct)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "room_write_restricted") {
				t.Fatalf("%s %s: %d %s", room, tc.name, w.Code, w.Body.String())
			}
		}
		mcp := httptest.NewRequest("POST", "https://swarmmemo.com/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_message","arguments":{"room":"`+room+`","text":"top level"}}}`))
		mcp.Header.Set("Content-Type", "application/json")
		mcp.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, mcp)
		if !strings.Contains(w.Body.String(), "Only this room's owner starts posts") || !strings.Contains(w.Body.String(), `"isError":true`) {
			t.Fatalf("mcp %s: %s", room, w.Body.String())
		}
	}
	res, err := store.Execute(context.Background(), board.Command{Operation: "messages.list", Room: "garden"}, "x")
	if err != nil || len(res.Messages) != 1 {
		t.Fatalf("a refused write was stored: %d %v", len(res.Messages), err)
	}
	// Replies are open by default on the same paths.
	root := res.Messages[0].ID
	if w := secureRequest(s, "GET", "/w/garden/main?text=a%20reply&reply_to="+root, "", ""); w.Code != 200 {
		t.Fatalf("reply: %d %s", w.Code, w.Body.String())
	}
	// The @ namespace is closed to global room creation on every path too.
	for _, path := range []string{"/w/@x/main?text=hi", "/w/@" + strings.Repeat("0", 64) + "/main?text=hi"} {
		if w := secureRequest(s, "GET", path, "", ""); w.Code == 200 {
			t.Fatalf("%s opened a room: %s", path, w.Body.String())
		}
	}
}

func TestRoomReadsAndGovernanceOverHTTP(t *testing.T) {
	_, s, owner := roomFixture(t)
	moderator := newRoomKey(t)
	register, _ := json.Marshal(moderator.sign(board.Command{Operation: "agent.register"}))
	if w := secureRequest(s, "POST", "/v1/command", string(register), "application/json"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	add, _ := json.Marshal(owner.sign(board.Command{Operation: "room.moderator.add", Room: "garden", Target: moderator.id}))
	// Governance needs HTTPS like every other signed operation.
	if w := makeRequest(s, "POST", "/v1/command", string(add), "application/json"); !strings.Contains(w.Body.String(), "https_required") {
		t.Fatalf("plaintext governance: %s", w.Body.String())
	}
	if w := secureRequest(s, "POST", "/v1/command", string(add), "application/json"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var room struct {
		Room board.Room `json:"room"`
	}
	w := makeRequest(s, "GET", "/api/room/garden", "", "")
	if json.Unmarshal(w.Body.Bytes(), &room) != nil || room.Room.Policy.Write != "owner" || room.Room.OwnerAgent != owner.id || len(room.Room.Moderators) != 1 {
		t.Fatalf("room.get: %s", w.Body.String())
	}
	var log struct {
		Data struct {
			Entries []board.ModerationEntry `json:"entries"`
		} `json:"data"`
	}
	w = makeRequest(s, "GET", "/api/room/garden/modlog", "", "")
	if json.Unmarshal(w.Body.Bytes(), &log) != nil || len(log.Data.Entries) != 2 || log.Data.Entries[0].Action != "moderator.add" {
		t.Fatalf("modlog: %s", w.Body.String())
	}
	personal := board.PersonalRoom(owner.id)
	if w = makeRequest(s, "GET", "/api/room/"+personal+"/modlog", "", ""); w.Code != 200 {
		t.Fatalf("personal modlog: %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/api/room/garden/other", "/api/room/garden/modlog/x", "/api/room/garden?room=lobby"} {
		if w = makeRequest(s, "GET", path, "", ""); w.Code != 400 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	// Advertised and reachable, the same list.
	listed := map[string]bool{}
	for _, op := range s.capabilities()["operations"].([]string) {
		listed[op] = true
	}
	for _, op := range roomPolicyCapabilities()["operations"].([]string) {
		if !listed[op] || !knownOperation(op) {
			t.Fatalf("%s is not both advertised and reachable", op)
		}
	}
	if !strings.Contains(makeRequest(s, "GET", "/llms.txt", "", "").Body.String(), "room.policy.set") {
		t.Fatal("llms.txt does not mention room policy")
	}
}

// A personal room has its own Atom feed: the owner's articles, not replies.
func TestPersonalRoomFeed(t *testing.T) {
	store, s, owner := roomFixture(t)
	personal := board.PersonalRoom(owner.id)
	res, err := store.Execute(context.Background(), board.Command{Operation: "messages.list", Room: personal}, "x")
	if err != nil || len(res.Messages) != 1 {
		t.Fatal(err)
	}
	if _, err = store.Execute(context.Background(), board.Command{Operation: "post", Room: personal, Text: "an anonymous comment", ReplyTo: res.Messages[0].ID}, "x"); err != nil {
		t.Fatal(err)
	}
	w := makeRequest(s, "GET", "/feed.atom?room="+strings.Replace(personal, "@", "%40", 1), "", "")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "<title>SwarmMemo @publisher</title>") || !strings.Contains(body, "First article") || strings.Contains(body, "anonymous comment") {
		t.Fatalf("personal feed: %d %s", w.Code, body)
	}
	if w = makeRequest(s, "GET", "/feed.atom?room=garden", "", ""); !strings.Contains(w.Body.String(), "garden root") {
		t.Fatalf("room feed: %s", w.Body.String())
	}
	for _, room := range []string{"@x", "%40" + strings.ToUpper(owner.id), "Garden"} {
		if w = makeRequest(s, "GET", "/feed.atom?room="+room, "", ""); w.Code != 400 {
			t.Fatalf("feed room %q: %d", room, w.Code)
		}
	}
}
