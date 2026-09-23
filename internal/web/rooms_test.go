package web

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

type webKey struct {
	private ed25519.PrivateKey
	id      string
	n       int
}

func newWebKey(seed byte) *webKey {
	b := make([]byte, 32)
	for i := range b {
		b[i] = seed
	}
	k := ed25519.NewKeyFromSeed(b)
	sum := sha256.Sum256(k.Public().(ed25519.PublicKey))
	return &webKey{private: k, id: hex.EncodeToString(sum[:])}
}

func (k *webKey) run(t *testing.T, s *board.Store, c board.Command) board.Result {
	t.Helper()
	k.n++
	c.PublicKey = base64.RawURLEncoding.EncodeToString(k.private.Public().(ed25519.PublicKey))
	c.Timestamp, c.Nonce = time.Now().Unix(), fmt.Sprintf("web-%d", k.n)
	c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(k.private, board.Canonical("swarmmemo.com", c)))
	res, err := s.Execute(context.Background(), c, "test")
	if err != nil {
		t.Fatalf("%s: %v", c.Operation, err)
	}
	return res
}

func roomStore(t *testing.T) (*board.Store, *webKey, *webKey) {
	t.Helper()
	s, err := board.Open(filepath.Join(t.TempDir(), "web.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	owner, mod := newWebKey(31), newWebKey(32)
	owner.run(t, s, board.Command{Operation: "agent.register", Handle: "writer"})
	mod.run(t, s, board.Command{Operation: "agent.register", Handle: "keeper"})
	return s, owner, mod
}

func render(s board.Service, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestPersonalRoomAddresses(t *testing.T) {
	s, owner, _ := roomStore(t)
	owner.run(t, s, board.Command{Operation: "post", Room: board.PersonalRoom(owner.id), Text: "An article"})
	short := "/@" + owner.id[:12]
	w := render(s, short)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "<h1>writer</h1>") || !strings.Contains(body, "An article") || !strings.Contains(body, `href="/feed.atom?room=%40`+owner.id+`"`) {
		t.Fatalf("personal page: %d %s", w.Code, body)
	}
	// Strangers are told who posts here; the composer waits for the owner.
	if !strings.Contains(body, "Only writer starts posts here. Reply to one to join in.") || !strings.Contains(body, `id="compose" open hidden`) || !strings.Contains(body, `data-room-owner="`+owner.id+`"`) {
		t.Fatal("personal page does not gate its composer")
	}
	for _, alias := range []string{"/@writer", "/@WRITER", "/@" + owner.id, "/r/" + board.PersonalRoom(owner.id)} {
		if w = render(s, alias); w.Code != 301 || w.Header().Get("Location") != short {
			t.Fatalf("%s: %d %q", alias, w.Code, w.Header().Get("Location"))
		}
	}
	for _, missing := range []string{"/@nobody", "/@" + strings.Repeat("0", 12), "/@", "/@writer/extra"} {
		if w = render(s, missing); w.Code != 404 {
			t.Fatalf("%s: %d", missing, w.Code)
		}
	}
	// The agent page links to its room.
	if w = render(s, "/agent/"+owner.id); !strings.Contains(w.Body.String(), `href="/@`+owner.id+`">Personal room`) {
		t.Fatal("agent page does not link its personal room")
	}
}

func TestRoomPageShowsPolicyAndModeration(t *testing.T) {
	s, owner, mod := roomStore(t)
	owner.run(t, s, board.Command{Operation: "room.create", Room: "garden"})
	owner.run(t, s, board.Command{Operation: "room.policy.set", Room: "garden", Data: `{"write":"owner","reply":"none","rules":"Be <b>kind</b>."}`})
	owner.run(t, s, board.Command{Operation: "room.moderator.add", Room: "garden", Target: mod.id})
	root := owner.run(t, s, board.Command{Operation: "post", Room: "garden", Text: "root"}).Receipt.ID
	owner.run(t, s, board.Command{Operation: "room.policy.set", Room: "garden", Data: `{"reply":"anyone"}`})
	reply, err := s.Execute(context.Background(), board.Command{Operation: "post", Room: "garden", Text: "spam", ReplyTo: root}, "x")
	if err != nil {
		t.Fatal(err)
	}
	mod.run(t, s, board.Command{Operation: "room.hide", MessageID: reply.Receipt.ID, Reason: "Promotion."})
	owner.run(t, s, board.Command{Operation: "room.policy.set", Room: "garden", Data: `{"reply":"none"}`})

	body := render(s, "/r/garden").Body.String()
	for _, want := range []string{
		"Only the owner starts posts · replies closed", ">writer</a>", ">keeper</a>", `href="/modlog/garden"`,
		"Be &lt;b&gt;kind&lt;/b&gt;.", "Only the owner posts here.", `id="compose" open hidden`,
		"moderators: Promotion.", `data-moderate="restore"`, `data-moderate="hide"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("room page lacks %q", want)
		}
	}
	if strings.Contains(body, "reply-button") {
		t.Fatal("a room with replies closed still offers Reply")
	}
	if strings.Contains(body, `data-moderate="hide" data-moderate-id="`+root+`" data-author="`+owner.id+`">`) {
		t.Fatal("moderator controls must start hidden")
	}
	body = render(s, "/e/"+root).Body.String()
	if !strings.Contains(body, "Replies are closed in this room.") || !strings.Contains(body, `href="/r/garden" class="text-link">← #garden`) {
		t.Fatal("thread page ignores the reply policy")
	}

	w := render(s, "/modlog/garden")
	body = w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "Hid a message") || !strings.Contains(body, "Promotion.") || !strings.Contains(body, "Added a moderator") || !strings.Contains(body, "⌘</span> keeper") {
		t.Fatalf("modlog page: %d %s", w.Code, body)
	}
	owner.run(t, s, board.Command{Operation: "room.create", Room: "circle", Visibility: "private"})
	for _, path := range []string{"/modlog/circle", "/modlog/nowhere", "/modlog/@x"} {
		if w = render(s, path); w.Code != 404 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
}

// personalStyled is a styled service that also resolves one personal room and
// answers room reads with a policy, so the room's trust UI renders.
type personalStyled struct{ *styledService }

func (personalStyled) ResolvePersonal(_ context.Context, alias string) (board.PersonalAddress, error) {
	if alias != key[:12] {
		return board.PersonalAddress{}, &board.Error{Status: 404, Code: "not_found"}
	}
	return board.PersonalAddress{Room: board.PersonalRoom(key), Account: key, Agent: key, Short: key[:12]}, nil
}

// Room CSS reaches post bodies in personal rooms too, and never the room's
// trust UI: the policy header, the composer's gate and moderator controls.
func TestRoomCanvasInPersonalAndPolicyRooms(t *testing.T) {
	personal := board.PersonalRoom(key)
	styles := map[string]string{personal: "p{color:red}", "lobby": "p{color:red}"}
	base := styled(styles, "public")
	inner := base.execute
	base.execute = func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "agent.get":
			return board.Result{OK: true, Agent: &board.Agent{ID: key, Handle: "weaver"}}, nil
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public", Owner: key, OwnerAgent: key, Moderators: []string{key}, Personal: c.Room == personal,
				Policy: &board.RoomPolicy{Write: "owner", Reply: "anyone", Rules: "Be kind."}}}, nil
		}
		return inner(c)
	}
	s := personalStyled{base}
	for _, path := range []string{"/@" + key[:12], "/r/lobby", "/e/m1"} {
		body := get(t, s, path, "swarmmemo.com").Body.String()
		inside, outside := classesByCanvas(t, body)
		if len(inside) == 0 {
			t.Fatalf("%s: no canvas rendered", path)
		}
		trusts := []string{"composer-gate", "mod-button", "room-style-strip"}
		if path != "/e/m1" { // a conversation page has no room header
			trusts = append(trusts, "room-governance", "room-facts", "room-rules")
		}
		for _, trust := range trusts {
			if !outside[trust] || inside[trust] {
				t.Errorf("%s: trust class %q not rendered outside the canvas", path, trust)
			}
		}
	}
}
