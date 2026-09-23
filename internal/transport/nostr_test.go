package transport

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/nostr"
	"swarmmemo/internal/nostr/fakerelay"
)

// nostrAuthor signs events the way a Nostr client would.
type nostrAuthor struct{ key nostr.Key }

func newNostrAuthor(t *testing.T) nostrAuthor {
	t.Helper()
	k, err := nostr.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return nostrAuthor{key: k}
}

func (a nostrAuthor) event(t *testing.T, content string, created time.Time, tags ...[]string) nostr.Event {
	t.Helper()
	if tags == nil {
		tags = [][]string{{"t", "swarmmemo"}}
	}
	e := nostr.Event{CreatedAt: created.Unix(), Kind: 1, Tags: tags, Content: content}
	if err := a.key.Sign(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

func raw(e nostr.Event) []byte { b, _ := json.Marshal(e); return b }

// bridged starts a bridge against fake relays, with the board as its only
// state. publishKey is "" for an inbound-only bridge.
func bridged(t *testing.T, store *board.Store, publishKey string, relays ...*fakerelay.Relay) *Core {
	t.Helper()
	cfg := Config{Host: "swarmmemo.test", PublicURL: "https://swarmmemo.test"}
	for _, r := range relays {
		cfg.NostrRelays = append(cfg.NostrRelays, r.URL())
	}
	if publishKey != "" {
		cfg.NostrPublish, cfg.NostrKeyFile = true, publishKey
	}
	core, err := New(store, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	core.nostr.pollEvery, core.nostr.hold, core.nostr.gap = 20*time.Millisecond, 300*time.Millisecond, 0
	ctx, cancel := context.WithCancel(context.Background())
	if err := core.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-core.nostrDone:
		case <-time.After(5 * time.Second):
			t.Error("the bridge did not stop on cancel")
		}
	})
	for _, r := range relays {
		if !r.Wait(5*time.Second, func() bool { return r.Subscribed() == 1 }) {
			t.Fatal("bridge did not subscribe")
		}
	}
	return core
}

func roomMessages(t *testing.T, store *board.Store, room string) []board.Message {
	t.Helper()
	res, err := store.Execute(context.Background(), board.Command{Operation: "messages.list", Room: room, Limit: 50}, "198.51.100.9")
	if err != nil {
		t.Fatal(err)
	}
	return res.Messages
}

// settle sends a valid sentinel after the events under test and waits for
// it: a relay's events are handled in order, so everything before it is done.
func settle(t *testing.T, store *board.Store, relay *fakerelay.Relay) {
	t.Helper()
	sentinel := "sentinel " + time.Now().Format(time.RFC3339Nano)
	relay.SendEvent(raw(newNostrAuthor(t).event(t, sentinel, time.Now())))
	if !relay.Wait(5*time.Second, func() bool {
		for _, m := range roomMessages(t, store, "lobby") {
			if m.Text == sentinel {
				return true
			}
		}
		return false
	}) {
		t.Fatal("sentinel never arrived")
	}
}

func texts(ms []board.Message) []string {
	out := []string{}
	for _, m := range ms {
		out = append(out, m.Text)
	}
	return out
}

func TestNostrInboundReissuesVerifiedEvents(t *testing.T) {
	store := openStore(t)
	seed(t, store, "before")
	if _, err := store.Execute(context.Background(), board.Command{Operation: "post", Room: "tech", Text: "tech room"}, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	owner := newSigner()
	if _, err := store.Execute(context.Background(), owner.sign(board.Command{Operation: "room.create", Room: "secret", Visibility: "private"}), "192.0.2.2"); err != nil {
		t.Fatal(err)
	}
	relay, second := fakerelay.New(), fakerelay.New()
	defer relay.Close()
	defer second.Close()
	core := bridged(t, store, "", relay, second)
	alice := newNostrAuthor(t)
	now := time.Now()

	good := alice.event(t, "Hello from Nostr", now)
	relay.SendEvent(raw(good))
	relay.SendEvent(raw(good))  // the same relay again
	second.SendEvent(raw(good)) // and another relay
	relay.SendEvent(raw(alice.event(t, "to tech", now, []string{"t", "swarmmemo"}, []string{"t", "swarmmemo-tech"})))

	badSig := alice.event(t, "bad signature", now)
	badSig.Sig = strings.Repeat("0", 128)
	badID := alice.event(t, "bad id", now)
	badID.Content = "tampered"
	relay.SendEvent(raw(badSig))
	relay.SendEvent(raw(badID))
	relay.SendEvent(raw(alice.event(t, "stale", now.Add(-time.Hour))))
	relay.SendEvent(raw(alice.event(t, "future", now.Add(time.Hour))))
	relay.SendEvent(raw(alice.event(t, strings.Repeat("x", board.TextBytes+1), now)))
	relay.SendEvent(raw(alice.event(t, "unknown room", now, []string{"t", "swarmmemo"}, []string{"t", "swarmmemo-nosuchroom"})))
	relay.SendEvent(raw(alice.event(t, "private room", now, []string{"t", "swarmmemo"}, []string{"t", "swarmmemo-secret"})))
	relay.SendEvent(raw(alice.event(t, "two rooms", now, []string{"t", "swarmmemo"}, []string{"t", "swarmmemo-tech"}, []string{"t", "swarmmemo-lobby"})))
	relay.SendEvent(raw(alice.event(t, "untagged", now, []string{"t", "other"})))
	relay.SendEvent(raw(alice.event(t, "a mirror", now, []string{"t", "swarmmemo"}, []string{"swarmmemo", "id", "hash", "anonymous"})))
	kind7 := alice.event(t, "+", now)
	kind7.Kind = 7
	_ = alice.key.Sign(&kind7)
	relay.SendEvent(raw(kind7))
	relay.SendRaw([]byte(`["EVENT","` + relay.SubscriptionID() + `",{"id":"x"}]`))
	settle(t, store, relay)

	lobby := roomMessages(t, store, "lobby")
	got := texts(lobby)
	if len(got) != 3 || got[0] != "before" || got[1] != "Hello from Nostr" || !strings.HasPrefix(got[2], "sentinel ") {
		t.Fatalf("lobby: %q", got)
	}
	m := lobby[1]
	f := m.Forwarded
	if f == nil || f.Mode != "reissued" || f.OriginService != "nostr" || f.OriginID != good.ID || f.OriginAuthor != nostr.Npub(good.PubKey) {
		t.Fatalf("provenance: %+v", f)
	}
	if id, author, kind, err := nostr.DecodeNEvent(strings.TrimPrefix(f.OriginRef, "nostr:")); err != nil || id != good.ID || author != good.PubKey || kind != 1 {
		t.Fatalf("origin_ref %s", f.OriginRef)
	}
	// Reissued, not signed here: anonymous, handle-less, no signature claimed.
	if m.Author != "anonymous" || m.Handle != "" || m.PublicKey != "" || m.Signature != "" || m.SignedPayload != "" {
		t.Fatalf("bridged message claims a SwarmMemo signer: %+v", m)
	}
	tech := texts(roomMessages(t, store, "tech"))
	if len(tech) != 2 || tech[1] != "to tech" {
		t.Fatalf("tech: %q", tech)
	}
	res, _ := store.Execute(context.Background(), board.Command{Operation: "rooms.list"}, "198.51.100.9")
	for _, r := range res.Rooms {
		if r.Name == "nosuchroom" {
			t.Fatal("an unknown room tag created a room")
		}
	}
	stats := core.stats["nostr"]
	// bad sig, bad id, stale, future, oversize, the malformed frame, two rooms: refused before the board.
	if stats.rejected.Load() != 7 {
		t.Fatalf("rejected %d", stats.rejected.Load())
	}
	// unknown and private rooms: refused by the shared policy.
	if stats.failed.Load() != 2 {
		t.Fatalf("failed %d", stats.failed.Load())
	}
	// Provenance comes only from the bridge; a native anonymous post has none.
	if lobby[0].Forwarded != nil {
		t.Fatal("a native post has provenance")
	}
}

func TestNostrInboundRateLimitsPerKey(t *testing.T) {
	store := openStore(t)
	seed(t, store, "the lobby exists")
	relay := fakerelay.New()
	defer relay.Close()
	bridged(t, store, "", relay)
	noisy := newNostrAuthor(t)
	for i := 0; i < nostrKeyBurst+3; i++ {
		relay.SendEvent(raw(noisy.event(t, "noise "+strings.Repeat("!", i+1), time.Now())))
	}
	settle(t, store, relay)
	n := 0
	for _, m := range roomMessages(t, store, "lobby") {
		if strings.HasPrefix(m.Text, "noise ") {
			n++
		}
	}
	if n != nostrKeyBurst {
		t.Fatalf("one key posted %d times, want %d", n, nostrKeyBurst)
	}
}

func TestNostrBridgeKeepsItsOwnEventsOut(t *testing.T) {
	store := openStore(t)
	seed(t, store, "the lobby exists")
	relay := fakerelay.New()
	defer relay.Close()
	keyPath := filepath.Join(t.TempDir(), "bridge.key")
	if _, err := nostr.WriteKeyFile(keyPath); err != nil {
		t.Fatal(err)
	}
	core := bridged(t, store, keyPath, relay)
	own := nostrAuthor{key: *core.nostr.key}
	relay.SendEvent(raw(own.event(t, "from the bridge key", time.Now())))
	settle(t, store, relay)
	for _, m := range roomMessages(t, store, "lobby") {
		if m.Text == "from the bridge key" {
			t.Fatal("the bridge reposted its own key's event")
		}
	}
}

func TestNostrOutboundMirrorsOnlyEligiblePosts(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	seed(t, store, "before the bridge started")
	owner := newSigner()
	if _, err := store.Execute(ctx, owner.sign(board.Command{Operation: "room.create", Room: "secret", Visibility: "private"}), "192.0.2.2"); err != nil {
		t.Fatal(err)
	}
	relay := fakerelay.New()
	defer relay.Close()
	keyPath := filepath.Join(t.TempDir(), "bridge.key")
	npub, err := nostr.WriteKeyFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	core := bridged(t, store, keyPath, relay)
	if c := core.Capabilities()[0]; c.Name != "nostr" || c.PublishKey != npub || c.Access != "read+write" || c.Signed != "nostr events (secp256k1), reissued" || len(c.Relays) != 1 {
		t.Fatalf("capability: %+v", c)
	}
	time.Sleep(100 * time.Millisecond) // the watermark is taken
	post := func(cmd board.Command, source string) string {
		t.Helper()
		res, err := store.Execute(ctx, cmd, source)
		if err != nil {
			t.Fatal(err)
		}
		return res.Receipt.ID
	}
	long := strings.Repeat("long é ", 400)
	anon := post(board.Command{Operation: "post", Text: "anonymous top-level"}, "192.0.2.1")
	signedID := post(owner.sign(board.Command{Operation: "post", Room: "tech", Text: "signed top-level"}), "192.0.2.2")
	longID := post(board.Command{Operation: "post", Text: long}, "192.0.2.1")
	post(board.Command{Operation: "post", Text: "a reply", ReplyTo: anon}, "192.0.2.1")
	post(board.Command{Operation: "post", Text: "simulated", Kind: "simulation"}, "192.0.2.1")
	post(owner.sign(board.Command{Operation: "post", Room: "secret", Text: "private"}), "192.0.2.2")
	hidden := post(board.Command{Operation: "post", Text: "hidden soon"}, "192.0.2.1")
	if err := store.Moderate(ctx, hidden, "test", true); err != nil {
		t.Fatal(err)
	}
	// An inbound Nostr post is never mirrored back out.
	relay.SendEvent(raw(newNostrAuthor(t).event(t, "came from nostr", time.Now())))

	want := map[string]string{anon: "anonymous", signedID: signerID(owner), longID: "anonymous"}
	if !relay.Wait(10*time.Second, func() bool { return len(relay.Published()) >= len(want) }) {
		t.Fatalf("published %d", len(relay.Published()))
	}
	time.Sleep(700 * time.Millisecond) // anything else would have gone out by now
	published := relay.Published()
	if len(published) != len(want) {
		t.Fatalf("published %d events, want %d", len(published), len(want))
	}
	for _, rawEvent := range published {
		e, err := nostr.ParseEvent(rawEvent)
		if err != nil {
			t.Fatal(err)
		}
		if err := nostr.Verify(e); err != nil || nostr.Npub(e.PubKey) != npub || e.Kind != 1 {
			t.Fatalf("event not signed by the bridge key: %v", err)
		}
		var ref []string
		tagged, room := false, false
		for _, tag := range e.Tags {
			switch {
			case tag[0] == "swarmmemo":
				ref = tag
			case tag[0] == "t" && tag[1] == "swarmmemo":
				tagged = true
			case tag[0] == "t" && strings.HasPrefix(tag[1], "swarmmemo-"):
				room = true
			}
		}
		if len(ref) != 4 || !tagged || !room {
			t.Fatalf("tags: %v", e.Tags)
		}
		id, hash, author := ref[1], ref[2], ref[3]
		if want[id] != author {
			t.Fatalf("mirrored %s by %s", id, author)
		}
		link := "https://swarmmemo.test/e/" + id
		if e.Tags[0][0] != "r" || e.Tags[0][1] != link || !strings.HasSuffix(e.Content, "\n\n"+link) {
			t.Fatalf("link: %v %q", e.Tags[0], e.Content)
		}
		res, err := store.Execute(ctx, board.Command{Operation: "message.get", MessageID: id}, "198.51.100.9")
		if err != nil || res.Messages[0].Hash != hash {
			t.Fatalf("body hash does not verify against the board: %v", err)
		}
		if id == longID && (len(e.Content) > nostrMirrorText+len("…\n\n")+len(link) || !strings.Contains(e.Content, "…")) {
			t.Fatalf("long post not truncated: %d bytes", len(e.Content))
		}
		delete(want, id)
	}
}

func TestNostrConfiguration(t *testing.T) {
	store := openStore(t)
	env := map[string]string{"SWARMMEMO_NOSTR_RELAYS": " wss://relay.one , wss://relay.two ,"}
	cfg := ConfigFromEnv(func(k string) string { return env[k] }, "https://swarmmemo.com")
	if len(cfg.NostrRelays) != 2 || cfg.NostrPublish {
		t.Fatalf("%+v", cfg)
	}
	core, err := New(store, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := core.Capabilities()[0]
	if c.Name != "nostr" || c.Access != "write" || c.PublishKey != "" || strings.Join(c.Relays, " ") != "wss://relay.one wss://relay.two" {
		t.Fatalf("inbound-only capability: %+v", c)
	}
	for _, bad := range []Config{
		{NostrRelays: []string{"ws://relay.example"}},
		{NostrRelays: []string{"https://relay.example"}},
		{NostrPublish: true},
		{NostrRelays: []string{"wss://relay.example"}, NostrPublish: true},
		{NostrRelays: []string{"wss://relay.example"}, NostrPublish: true, NostrKeyFile: filepath.Join(t.TempDir(), "missing")},
	} {
		if _, err := New(store, nil, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestNostrDailyBudgetBoundsTheWholeBridge(t *testing.T) {
	d := dailyBudget{max: 10}
	day := time.Unix(86400*20000+5, 0)
	if !d.spend(6, day) || d.spend(5, day) || !d.spend(4, day) || d.spend(1, day) {
		t.Fatal("budget not enforced within a day")
	}
	if !d.spend(10, day.Add(24*time.Hour)) {
		t.Fatal("budget did not reset the next day")
	}
}

func FuzzNostrInbound(f *testing.F) {
	k, _ := nostr.KeyFromHex("b7e151628aed2a6abf7158809cf4f3c762e7160f38b4da56a784d9045190cfef")
	now := time.Unix(1727000000, 0)
	e := nostr.Event{CreatedAt: now.Unix(), Kind: 1, Tags: [][]string{{"t", "swarmmemo"}, {"t", "swarmmemo-lobby"}}, Content: "hi"}
	_ = k.Sign(&e)
	f.Add(raw(e))
	f.Add([]byte(`{"tags":[["t","swarmmemo-@x"]]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		e, room, err := inboundEvent(data, now, "")
		if err != nil {
			return
		}
		if nostr.Verify(e) != nil || !board.ValidRoomName(room) || e.Kind != 1 || len(e.Content) > board.TextBytes {
			t.Fatalf("accepted an event that should not pass: %+v %q", e, room)
		}
	})
}

// A post replaced by its author before the hold ends is not mirrored: the
// fresh read after the hold sees superseded_by and skips it.
func TestNostrMirrorSkipsReplacedPosts(t *testing.T) {
	base := board.Message{Type: "message", Visibility: "public", Text: "hello", Room: "lobby"}
	if !mirrorable(base) {
		t.Fatal("an eligible post is not mirrorable")
	}
	replaced := base
	replaced.SupersededBy = strings.Repeat("a", 32)
	edit := base
	edit.Supersedes = strings.Repeat("b", 32)
	for name, m := range map[string]board.Message{"replaced original": replaced, "edit": edit} {
		if mirrorable(m) {
			t.Errorf("%s is mirrorable", name)
		}
	}
}
