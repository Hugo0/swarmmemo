package transport

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// Room policy lives in the board's one post path, so every constrained wire
// obeys it without knowing it exists; none of them carries the room
// governance operations; and none can open a room in the "@" namespace.
func TestRoomPolicyHoldsOnEveryWire(t *testing.T) {
	store := openStore(t)
	seed(t, store, "room exists")
	ctx := context.Background()
	owner, stranger := newSigner(), newSigner()
	for _, c := range []board.Command{
		{Operation: "room.create", Room: "garden"},
		{Operation: "room.policy.set", Room: "garden", Data: `{"write":"owner"}`},
		{Operation: "post", Room: "garden", Text: "the owner's post"},
	} {
		if _, err := store.Execute(ctx, owner.sign(c), "192.0.2.9"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Execute(ctx, stranger.sign(board.Command{Operation: "agent.register"}), "192.0.2.9"); err != nil {
		t.Fatal(err)
	}
	root, err := store.Execute(ctx, board.Command{Operation: "messages.list", Room: "garden"}, "x")
	if err != nil || len(root.Messages) != 1 {
		t.Fatal(err)
	}
	rootID := root.Messages[0].ID
	cfg := allConfig(t)
	cfg.DNSWrite = true
	cfg.SMTPAnonymous = true
	_, addrs := startedWith(t, store, cfg)
	cmd := func(c board.Command) string {
		raw, _ := json.Marshal(c)
		return base64.RawURLEncoding.EncodeToString(raw)
	}

	if out := streamExchange(t, addrs["tcp/tcp"], []byte("POST garden anonymous top level\n"), false); !strings.Contains(out, "room_write_restricted") {
		t.Fatalf("tcp anonymous: %q", out)
	}
	if out := streamExchange(t, addrs["gemini/tcp"], []byte("gemini://localhost/post/garden?anonymous%20top%20level\r\n"), true); strings.Contains(out, "ok ") {
		t.Fatalf("gemini anonymous: %q", out)
	}
	if out := streamExchange(t, addrs["tcp/tcp"], []byte("CMD "+cmd(stranger.sign(board.Command{Operation: "post", Room: "garden", Text: "signed top level"}))+"\n"), false); !strings.Contains(out, "room_write_restricted") {
		t.Fatalf("tcp signed stranger: %q", out)
	}
	if out := smtpSession(t, addrs["smtp/tcp"], mailTo("garden@post.swarmmemo.com", "anonymous by mail")...); strings.Contains(out, "250 2.0.0 ok") {
		t.Fatalf("smtp anonymous: %q", out)
	}
	mailed := cmd(stranger.sign(board.Command{Operation: "post", Room: "garden", Text: "signed by mail"}))
	if out := smtpSession(t, addrs["smtp/tcp"], mailTo("garden@post.swarmmemo.com", "swarmmemo-command: "+mailed)...); strings.Contains(out, "250 2.0.0 ok") {
		t.Fatalf("smtp signed stranger: %q", out)
	}
	raw, _ := json.Marshal(stranger.sign(board.Command{Operation: "post", Room: "garden", Text: "signed by resolver", RequestID: "dns-policy"}))
	var last []string
	for _, name := range writeNames("policy0123456789abcd", raw, 150) {
		last = txtStrings(t, dnsUDP(t, addrs["dns/udp"], dnsQueryBytes(name, dnsTypeTXT)))
	}
	if len(last) == 0 || strings.HasPrefix(last[0], "ok ") {
		t.Fatalf("dns signed stranger: %q", last)
	}
	// Replies stay open, and the owner still posts over a wire.
	if out := streamExchange(t, addrs["tcp/tcp"], []byte("CMD "+cmd(stranger.sign(board.Command{Operation: "post", Room: "garden", Text: "a reply", ReplyTo: rootID}))+"\n"), false); !strings.HasPrefix(out, "ok ") {
		t.Fatalf("tcp signed reply: %q", out)
	}
	if out := streamExchange(t, addrs["tcp/tcp"], []byte("CMD "+cmd(owner.sign(board.Command{Operation: "post", Room: "garden", Text: "owner over nc"}))+"\n"), false); !strings.HasPrefix(out, "ok ") {
		t.Fatalf("tcp owner: %q", out)
	}
	res, err := store.Execute(ctx, board.Command{Operation: "messages.list", Room: "garden"}, "x")
	if err != nil || len(res.Messages) != 3 {
		t.Fatalf("garden holds %d messages, want root, reply and owner post: %v", len(res.Messages), err)
	}

	// No wire carries the governance operations, signed or not.
	for _, c := range []board.Command{
		{Operation: "room.policy.set", Room: "garden", Data: `{"write":"open"}`},
		{Operation: "room.moderator.add", Room: "garden", Target: strings.Repeat("a", 64)},
		{Operation: "room.moderator.remove", Room: "garden", Target: strings.Repeat("a", 64)},
		{Operation: "room.owner.transfer", Room: "garden", Target: strings.Repeat("a", 64)},
		{Operation: "room.hide", MessageID: rootID, Reason: "x"},
		{Operation: "room.restore", MessageID: rootID, Reason: "x"},
		{Operation: "room.modlog", Room: "garden"},
		{Operation: "room.style.set", Room: "garden", Data: `{"css":":scope{color:#000}"}`},
		{Operation: "room.style.clear", Room: "garden"},
		{Operation: "room.style.check", Room: "garden", Data: `{"css":":scope{color:#000}"}`},
	} {
		if out := streamExchange(t, addrs["tcp/tcp"], []byte("CMD "+cmd(owner.sign(c))+"\n"), false); !strings.Contains(out, "unsupported_operation") {
			t.Fatalf("%s crossed tcp: %q", c.Operation, out)
		}
		if out := smtpSession(t, addrs["smtp/tcp"], mailTo("garden@post.swarmmemo.com", "swarmmemo-command: "+cmd(owner.sign(c)))...); strings.Contains(out, "250 2.0.0 ok") {
			t.Fatalf("%s crossed smtp: %q", c.Operation, out)
		}
	}
	if res, _ := store.Execute(ctx, board.Command{Operation: "room.get", Room: "garden"}, "x"); res.Room.Policy.Write != "owner" || res.Room.Style != nil {
		t.Fatalf("a wire changed the policy: %+v", res.Room.Policy)
	}

	// No wire opens a room in the "@" namespace, its own or another's, and
	// nobody but the owner starts posts in an existing personal room over one.
	ownRoom := board.PersonalRoom(signerID(owner))
	for _, c := range []board.Command{{Operation: "post", Room: ownRoom, Text: "first article over nc"}, {Operation: "post", Room: "@x", Text: "squat"}} {
		if out := streamExchange(t, addrs["tcp/tcp"], []byte("CMD "+cmd(owner.sign(c))+"\n"), false); strings.HasPrefix(out, "ok ") {
			t.Fatalf("tcp opened %s: %q", c.Room, out)
		}
	}
	if _, err := store.Execute(ctx, owner.sign(board.Command{Operation: "post", Room: ownRoom, Text: "first article"}), "192.0.2.9"); err != nil {
		t.Fatal(err)
	}
	if out := streamExchange(t, addrs["tcp/tcp"], []byte("POST "+ownRoom+" squat\n"), false); !strings.Contains(out, "room_write_restricted") {
		t.Fatalf("tcp anonymous in a personal room: %q", out)
	}
	if out := streamExchange(t, addrs["tcp/tcp"], []byte("CMD "+cmd(owner.sign(board.Command{Operation: "post", Room: ownRoom, Text: "second article"}))+"\n"), false); !strings.HasPrefix(out, "ok ") {
		t.Fatalf("tcp owner in its personal room: %q", out)
	}
}

func signerID(s *signer) string {
	raw, _ := base64.RawURLEncoding.DecodeString(s.pub)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
