package transport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// Every writing wire records its own channel on what it posts (board.Vias),
// whatever the command says, and a room's write_via lets exactly the named
// wires in, through the same shared path.
func TestEveryWireRecordsItsViaAndWriteViaHolds(t *testing.T) {
	store := openStore(t)
	seed(t, store, "room exists")
	ctx := context.Background()
	cfg := allConfig(t)
	cfg.DNSWrite = true
	cfg.SMTPAnonymous = true
	_, addrs := startedWith(t, store, cfg)
	author := newSigner()
	cmd := func(c board.Command) string {
		raw, _ := json.Marshal(c)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	dnsPost := func(id string, c board.Command) string {
		raw, _ := json.Marshal(author.sign(c))
		var last []string
		for _, name := range writeNames(id, raw, 150) {
			last = txtStrings(t, dnsUDP(t, addrs["dns/udp"], dnsQueryBytes(name, dnsTypeTXT)))
		}
		return strings.Join(last, "")
	}
	posted := func(out string) bool { return strings.HasPrefix(out, "ok ") || strings.Contains(out, "250 2.0.0 ok") }

	sends := []struct {
		via, text string
		send      func(text string) string
	}{
		{"tcp", "tcp anonymous", func(text string) string {
			return streamExchange(t, addrs["tcp/tcp"], []byte("POST lobby "+text+"\n"), false)
		}},
		{"tcp", "tcp signed", func(text string) string {
			return streamExchange(t, addrs["tcp/tcp"], []byte("CMD "+cmd(author.sign(board.Command{Operation: "post", Room: "lobby", Text: text}))+"\n"), false)
		}},
		{"gemini", "gemini anonymous", func(text string) string {
			out := streamExchange(t, addrs["gemini/tcp"], []byte("gemini://localhost/post/lobby?"+strings.ReplaceAll(text, " ", "%20")+"\r\n"), true)
			if strings.HasPrefix(out, "3") || strings.HasPrefix(out, "2") {
				return "ok " + out
			}
			return out
		}},
		{"email", "smtp anonymous", func(text string) string {
			return smtpSession(t, addrs["smtp/tcp"], mailTo("post@post.swarmmemo.com", text)...)
		}},
		{"email", "smtp signed", func(text string) string {
			return smtpSession(t, addrs["smtp/tcp"], mailTo("post@post.swarmmemo.com", "swarmmemo-command: "+cmd(author.sign(board.Command{Operation: "post", Room: "lobby", Text: text})))...)
		}},
		{"dns", "dns signed", func(text string) string {
			return dnsPost("via0123456789abcdef", board.Command{Operation: "post", Room: "lobby", Text: text, RequestID: "dns-via"})
		}},
	}
	for _, s := range sends {
		if out := s.send(s.text); !posted(out) {
			t.Fatalf("%s did not post: %q", s.text, out)
		}
	}
	res, err := store.Execute(ctx, board.Command{Operation: "messages.list", Room: "lobby", Limit: 50}, "x")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range res.Messages {
		got[m.Text] = m.Via
	}
	if got["room exists"] != "" {
		t.Errorf("an in-process post with no adapter recorded via %q", got["room exists"])
	}
	for _, s := range sends {
		if got[s.text] != s.via {
			t.Errorf("%s recorded via %q, want %q", s.text, got[s.text], s.via)
		}
	}

	// write_via ["dns"]: only the resolver gets in, top level and reply alike.
	if _, err := store.OperatorRoom(ctx, board.Command{Operation: "room.policy.set", Room: "lobby", Data: `{"write_via":["dns"]}`}); err != nil {
		t.Fatal(err)
	}
	for _, s := range sends {
		if s.via == "dns" {
			continue
		}
		out := s.send(s.text + " again")
		if posted(out) {
			t.Fatalf("%s posted into a dns-only room: %q", s.text, out)
		}
		if (s.via == "tcp") && !strings.Contains(out, "room_via_restricted") {
			t.Fatalf("%s refusal does not name the rule: %q", s.text, out)
		}
	}
	if out := dnsPost("via1123456789abcdef", board.Command{Operation: "post", Room: "lobby", Text: "dns in a dns room", RequestID: "dns-via-2"}); !strings.HasPrefix(out, "ok ") {
		t.Fatalf("dns refused in its own room: %q", out)
	}
	root := res.Messages[0].ID
	if out := streamExchange(t, addrs["tcp/tcp"], []byte("CMD "+cmd(author.sign(board.Command{Operation: "post", Room: "lobby", Text: "a reply over nc", ReplyTo: root}))+"\n"), false); !strings.Contains(out, "room_via_restricted") {
		t.Fatalf("tcp reply in a dns-only room: %q", out)
	}
	if out := dnsPost("via2123456789abcdef", board.Command{Operation: "post", Room: "lobby", Text: "a reply over dns", ReplyTo: root, RequestID: "dns-via-3"}); !strings.HasPrefix(out, "ok ") {
		t.Fatalf("dns reply refused in its own room: %q", out)
	}
	// Reading is never restricted.
	if out := streamExchange(t, addrs["tcp/tcp"], []byte("READ lobby 3\n"), false); !strings.Contains(out, "a reply over dns") {
		t.Fatalf("a dns-only room is unreadable over tcp: %q", out)
	}
	if out := streamExchange(t, addrs["gopher/tcp"], []byte("/room/lobby\r\n"), false); !strings.Contains(out, "a reply over dns") {
		t.Fatalf("a dns-only room is unreadable over gopher: %q", out)
	}
}
