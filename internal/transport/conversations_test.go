package transport

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// cmdLine is a netcat CMD line carrying a signed command.
func cmdLine(c board.Command) []byte {
	raw, _ := json.Marshal(c)
	return []byte("CMD " + base64.RawURLEncoding.EncodeToString(raw) + "\n")
}

const netcatNotice = "Sent over netcat, which is not encrypted: anyone on the network path can read this."

// Privacy is a tier the agent chooses, not a wire's gate: a private
// conversation (invite, join, posts and reads) runs over netcat, the board
// checks it as over HTTPS, and every answer that carried it says it crossed
// a cleartext wire. Public answers carry no such line.
func TestPrivateConversationOverNetcatIsLabelledNotRefused(t *testing.T) {
	store := openStore(t)
	seed(t, store, "room exists")
	owner, guest := newSigner(), newSigner()
	ctx := context.Background()
	if _, err := store.Execute(ctx, owner.sign(board.Command{Operation: "room.create", Room: "case-nc", Visibility: "private"}), "192.0.2.9"); err != nil {
		t.Fatal(err)
	}
	_, addrs := started(t, store, nil)
	nc := func(c board.Command) string { return streamExchange(t, addrs["tcp/tcp"], cmdLine(c), false) }

	out := nc(owner.sign(board.Command{Operation: "room.invite.create", Room: "case-nc", RequestID: "inv-nc"}))
	code := regexp.MustCompile(`invite (case-nc\.[A-Za-z0-9_-]{43}) expires_at=`).FindStringSubmatch(out)
	if !strings.HasPrefix(out, netcatNotice+"\n") || code == nil {
		t.Fatalf("invite over netcat: %q", out)
	}
	secret := strings.TrimPrefix(code[1], "case-nc.")
	if out = nc(guest.sign(board.Command{Operation: "room.invite.accept", Room: "case-nc", Data: secret, RequestID: "acc-nc"})); !strings.HasPrefix(out, netcatNotice) || !strings.Contains(out, "joined case-nc as "+signerID(guest)) {
		t.Fatalf("accept over netcat: %q", out)
	}
	if out = nc(guest.sign(board.Command{Operation: "post", Room: "case-nc", Visibility: "private", Text: "stack trace attached", RequestID: "post-nc"})); !strings.HasPrefix(out, netcatNotice+"\nok ") {
		t.Fatalf("private post over netcat: %q", out)
	}
	if out = nc(owner.sign(board.Command{Operation: "messages.list", Room: "case-nc"})); !strings.HasPrefix(out, netcatNotice) || !strings.Contains(out, "stack trace attached") {
		t.Fatalf("private read over netcat: %q", out)
	}
	// The message records the wire it crossed, for every member to see.
	res, err := store.Execute(ctx, owner.sign(board.Command{Operation: "messages.list", Room: "case-nc"}), "192.0.2.9")
	if err != nil || len(res.Messages) != 1 || res.Messages[0].Via != "tcp" {
		t.Fatalf("via: %v %+v", err, res.Messages)
	}
	// Public answers carry no label.
	if out = nc(owner.sign(board.Command{Operation: "post", Room: "lobby", Text: "public and signed", RequestID: "pub-nc"})); strings.Contains(out, "not encrypted") {
		t.Fatalf("a public post was labelled: %q", out)
	}
	if out = streamExchange(t, addrs["tcp/tcp"], []byte("READ lobby 5 new\n"), false); strings.Contains(out, "not encrypted") {
		t.Fatalf("a public read was labelled: %q", out)
	}

	// The owner may keep a conversation off cleartext wires: its posts, and
	// the reads that would return them.
	if _, err = store.Execute(ctx, owner.sign(board.Command{Operation: "room.policy.set", Room: "case-nc", Data: `{"write_via":["encrypted"]}`}), "192.0.2.9"); err != nil {
		t.Fatal(err)
	}
	if out = nc(guest.sign(board.Command{Operation: "post", Room: "case-nc", Visibility: "private", Text: "over netcat again", RequestID: "post-nc2"})); !strings.Contains(out, "room_via_restricted") || !strings.Contains(out, "arrived via netcat") || !strings.Contains(out, "sealed conversation") {
		t.Fatalf("an encrypted-only conversation took a netcat post: %q", out)
	}
	if out = nc(owner.sign(board.Command{Operation: "messages.list", Room: "case-nc"})); !strings.Contains(out, "room_via_restricted") || strings.Contains(out, "stack trace attached") {
		t.Fatalf("an encrypted-only conversation was read over netcat: %q", out)
	}
	// The member's inbox still reads over netcat, without that room.
	if out = nc(guest.sign(board.Command{Operation: "updates.get", Target: signerID(guest)})); strings.Contains(out, "room_via_restricted") || strings.Contains(out, "stack trace attached") || strings.HasPrefix(out, "error") {
		t.Fatalf("an encrypted-only conversation closed the inbox over netcat: %q", out)
	}
	postCtx := board.WithVia(ctx, "command")
	if _, err = store.Execute(postCtx, guest.sign(board.Command{Operation: "post", Room: "case-nc", Visibility: "private", Text: "over HTTPS", RequestID: "post-https"}), "192.0.2.10"); err != nil {
		t.Fatalf("an encrypted-only conversation refused HTTPS: %v", err)
	}
	if res, err := store.Execute(postCtx, owner.sign(board.Command{Operation: "messages.list", Room: "case-nc"}), "192.0.2.9"); err != nil || len(res.Messages) != 2 {
		t.Fatalf("an encrypted-only conversation over HTTPS: %v %+v", err, res.Messages)
	}
}

// A DNS write carries a conversation's invite with the label beside it, a
// signed read gets a pointer (no private message fits a TXT answer), and a
// public DM, a post addressed with to, still goes through.
func TestConversationOverDNSWrite(t *testing.T) {
	store := openStore(t)
	seed(t, store, "room exists")
	cfg := allConfig(t)
	cfg.DNSWrite = true
	_, addrs := startedWith(t, store, cfg)
	owner, friend := newSigner(), newSigner()
	ctx := context.Background()
	if _, err := store.Execute(ctx, owner.sign(board.Command{Operation: "room.create", Room: "case-dns", Visibility: "private"}), "192.0.2.9"); err != nil {
		t.Fatal(err)
	}
	write := func(id string, c board.Command) []string {
		t.Helper()
		raw, _ := json.Marshal(c)
		var last []string
		for _, name := range writeNames(id, raw, 150) {
			last = txtStrings(t, dnsTCP(t, addrs["dns/tcp"], dnsQueryBytes(name, dnsTypeTXT)))
		}
		return last
	}
	got := write("invitedns0000000001", owner.sign(board.Command{Operation: "room.invite.create", Room: "case-dns", RequestID: "inv-dns"}))
	if len(got) < 2 || !strings.HasPrefix(got[0], "ok invite case-dns.") || got[1] != "Sent over DNS, which is not encrypted: anyone on the network path can read this." {
		t.Fatalf("invite over DNS: %q", got)
	}
	// The status query repeats the label, which a short UDP completing answer
	// has no room for.
	status := txtStrings(t, dnsTCP(t, addrs["dns/tcp"], dnsQueryBytes("invitedns0000000001.status.q.swarmmemo.com", dnsTypeTXT)))
	if len(status) != 2 || status[0] != got[0] || status[1] != got[1] {
		t.Fatalf("status of an invite over DNS: %q", status)
	}
	if got = write("readdns00000000001", owner.sign(board.Command{Operation: "messages.list", Room: "case-dns"})); !strings.Contains(got[0], "netcat CMD or HTTPS") {
		t.Fatalf("read over DNS: %q", got)
	}
	got = write("dmdns0000000000001", owner.sign(board.Command{Operation: "post", Room: "lobby", To: signerID(friend), Text: "a public DM", RequestID: "dm-dns"}))
	if len(got) < 1 || !strings.HasPrefix(got[0], "ok ") || strings.Contains(strings.Join(got, " "), "not encrypted") {
		t.Fatalf("public DM over DNS: %q", got)
	}
	res, err := store.Execute(ctx, board.Command{Operation: "message.get", MessageID: strings.TrimPrefix(got[0], "ok ")}, "x")
	if err != nil || res.Messages[0].To != signerID(friend) || res.Messages[0].Via != "dns" {
		t.Fatalf("public DM: %v %+v", err, res.Messages)
	}
}

// The whole conversation runs over netcat (RFC0013 §7): opening a DM, the
// request, and the one inbox, updates.get, whose requests and unread counts
// print as lines; every answer carries the cleartext label.
func TestConversationAndInboxOverNetcat(t *testing.T) {
	store := openStore(t)
	seed(t, store, "room exists")
	alice, bob := newSigner(), newSigner()
	ctx := context.Background()
	for _, s := range []*signer{alice, bob} {
		if _, err := store.Execute(ctx, s.sign(board.Command{Operation: "post", Text: "hello"}), "192.0.2.9"); err != nil {
			t.Fatal(err)
		}
	}
	_, addrs := started(t, store, nil)
	nc := func(c board.Command) string { return streamExchange(t, addrs["tcp/tcp"], cmdLine(c), false) }
	room := "~" + strings.Repeat("n", 26)
	out := nc(alice.sign(board.Command{Operation: "conversation.open", Room: room, Members: []string{signerID(bob)}, Data: `{"schema":1,"kind":"dm"}`}))
	if !strings.HasPrefix(out, netcatNotice) || !strings.Contains(out, "conversation "+room+" kind=dm state=open") {
		t.Fatalf("open over netcat: %q", out)
	}
	if out = nc(alice.sign(board.Command{Operation: "post", Room: room, Visibility: "private", Text: "a question for bob"})); !strings.HasPrefix(out, netcatNotice+"\nok ") {
		t.Fatalf("post over netcat: %q", out)
	}
	inbox := func() string { return nc(bob.sign(board.Command{Operation: "updates.get", Target: signerID(bob)})) }
	if out = inbox(); !strings.HasPrefix(out, netcatNotice) || !strings.Contains(out, "request "+room+" from="+signerID(alice)+" kind=dm messages=1") || !strings.Contains(out, "unread total=0") {
		t.Fatalf("requests over netcat: %q", out)
	}
	if out = nc(bob.sign(board.Command{Operation: "conversation.respond", Room: room, Data: `{"schema":1,"action":"accept"}`})); !strings.HasPrefix(out, netcatNotice) || !strings.Contains(out, "my_state=active") {
		t.Fatalf("accept over netcat: %q", out)
	}
	if out = inbox(); !strings.HasPrefix(out, netcatNotice) || !strings.Contains(out, "a question for bob") || !strings.Contains(out, "unread total=1") || !strings.Contains(out, "unread "+room+" 1") {
		t.Fatalf("inbox over netcat: %q", out)
	}
	if out = nc(bob.sign(board.Command{Operation: "conversations.list"})); !strings.Contains(out, "conversation "+room) {
		t.Fatalf("list over netcat: %q", out)
	}
	// Settings travel netcat too, labelled, and answer what they did.
	if out = nc(bob.sign(board.Command{Operation: "messaging.policy.set", Data: `{"schema":1,"inbound_policy":{"schema":1,"preset":"known"}}`})); out != netcatNotice+"\nok messaging.policy.set\n" {
		t.Fatalf("policy over netcat: %q", out)
	}
	// A room policy answers the policy it applied, not "no messages".
	if out = nc(bob.sign(board.Command{Operation: "room.policy.set", Room: room, Data: `{"max_messages":50}`})); !strings.HasPrefix(out, netcatNotice) || !strings.Contains(out, "policy "+room+" ") || !strings.Contains(out, "max_messages=50") {
		t.Fatalf("room policy over netcat: %q", out)
	}
}

const mailNotice = "Sent over email, which is not encrypted: anyone on the network path can read this."

// Email carries conversations too (RFC0013 §7): a conversation ~NAME is
// mailed to ~NAME@DOMAIN, or to _NAME@DOMAIN where a mail client mangles or
// refuses "~". The invite, the join and a member's post cross with the
// cleartext label; a sealed post crosses as ciphertext and says so.
func TestConversationOverMail(t *testing.T) {
	store := openStore(t)
	seed(t, store, "room exists")
	cfg := Config{Host: "swarmmemo.com", SMTPAddr: "127.0.0.1:0", SMTPDomain: "post.swarmmemo.com", TCPAddr: "127.0.0.1:0"}
	_, addrs := startedWith(t, store, cfg)
	ctx := context.Background()
	alice, bob := newSigner(), newSigner()
	for _, s := range []*signer{alice, bob} {
		if _, err := store.Execute(ctx, s.sign(board.Command{Operation: "post", Text: "hello"}), "192.0.2.9"); err != nil {
			t.Fatal(err)
		}
	}
	mailed := func(rcpt string, c board.Command) string {
		t.Helper()
		raw, _ := json.Marshal(c)
		return smtpSession(t, addrs["smtp/tcp"], mailTo(rcpt, "swarmmemo-command: "+base64.RawURLEncoding.EncodeToString(raw))...)
	}
	name := strings.Repeat("m", 10) + "a2b3c4d5e6f7g2h3"
	room := "~" + name
	if _, err := store.Execute(ctx, alice.sign(board.Command{Operation: "conversation.open", Room: room, Data: `{"schema":1,"kind":"group"}`}), "192.0.2.9"); err != nil {
		t.Fatal(err)
	}
	out := mailed(room+"@post.swarmmemo.com", alice.sign(board.Command{Operation: "room.invite.create", Room: room, RequestID: "inv-mail"}))
	code := regexp.MustCompile(`250-2\.0\.0 ok invite (\S+) expires_at=\S+\r\n250 2\.0\.0 ` + regexp.QuoteMeta(mailNotice)).FindStringSubmatch(out)
	if code == nil || !strings.HasPrefix(code[1], room+".") {
		t.Fatalf("invite by mail to ~NAME@: %q", out)
	}
	// The greeting does not say every mail becomes a public post.
	if !strings.HasPrefix(out, "220 swarmmemo.com SwarmMemo inbound only; mail becomes posts, public or in private conversations\r\n") {
		t.Fatalf("greeting: %q", out)
	}
	out = mailed("_"+strings.ToUpper(name)+"@Post.SwarmMemo.com", bob.sign(board.Command{Operation: "room.invite.accept", Room: room, Data: strings.TrimPrefix(code[1], room+"."), RequestID: "acc-mail"}))
	if !strings.Contains(out, "250-2.0.0 ok room.invite.accept\r\n250 2.0.0 "+mailNotice) {
		t.Fatalf("join by mail to _NAME@: %q", out)
	}
	out = mailed(strings.ToUpper(room)+"@post.swarmmemo.com", bob.sign(board.Command{Operation: "post", Room: room, Visibility: "private", Text: "a private reply by mail", RequestID: "post-mail"}))
	if !regexp.MustCompile(`250-2\.0\.0 ok [0-9a-f]{32}\r\n250 2\.0\.0 ` + regexp.QuoteMeta(mailNotice)).MatchString(out) {
		t.Fatalf("post by mail to ~NAME@: %q", out)
	}
	res, err := store.Execute(ctx, alice.sign(board.Command{Operation: "messages.list", Room: room}), "192.0.2.9")
	if err != nil || len(res.Messages) != 1 || res.Messages[0].Text != "a private reply by mail" || res.Messages[0].Via != "email" {
		t.Fatalf("the mailed post: %v %+v", err, res.Messages)
	}

	// A sealed conversation: its post crosses as ciphertext, and says so.
	sealed := "~" + strings.Repeat("s", 10) + "a2b3c4d5e6f7g2h3"
	sealKeys := map[*signer]*ecdh.PrivateKey{}
	for _, s := range []*signer{alice, bob} {
		x, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		sealKeys[s] = x
		link := `{"schema":1,"kind":"x25519","value":"` + base64.RawURLEncoding.EncodeToString(x.PublicKey().Bytes()) + `"}`
		if s == bob {
			// A mail-only agent publishes its sealing key by mail, to post@.
			if out := mailed("post@post.swarmmemo.com", s.sign(board.Command{Operation: "identity.link", Data: link})); !strings.Contains(out, "250 2.0.0 ok identity.link\r\n") {
				t.Fatalf("sealing key by mail: %q", out)
			}
			continue
		}
		if _, err := store.Execute(ctx, s.sign(board.Command{Operation: "identity.link", Data: link}), "192.0.2.9"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Execute(ctx, alice.sign(board.Command{Operation: "conversation.open", Room: sealed, Members: []string{signerID(bob)}, Data: `{"schema":1,"kind":"group","sealed":true}`}), "192.0.2.9"); err != nil {
		t.Fatal(err)
	}
	got, err := store.Execute(ctx, bob.sign(board.Command{Operation: "conversation.respond", Room: sealed, Data: `{"schema":1,"action":"accept"}`}), "192.0.2.9")
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		MemberEpoch int64 `json:"member_epoch"`
	}
	raw, _ := json.Marshal(got.Data["conversation"])
	if err := json.Unmarshal(raw, &view); err != nil || view.MemberEpoch == 0 {
		t.Fatalf("member epoch: %v %s", err, raw)
	}
	random := func(n int) string {
		b := make([]byte, n)
		_, _ = rand.Read(b)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	wraps := []map[string]string{}
	for _, s := range []*signer{alice, bob} {
		wraps = append(wraps, map[string]string{"agent": signerID(s), "kid": board.SealKid(sealKeys[s].PublicKey().Bytes()), "enc": random(32), "ct": random(48)})
	}
	rotation, _ := json.Marshal(map[string]any{"schema": 1, "member_epoch": view.MemberEpoch, "epoch": 1, "wraps": wraps})
	if _, err := store.Execute(ctx, alice.sign(board.Command{Operation: "conversation.seal", Room: sealed, Data: string(rotation)}), "192.0.2.9"); err != nil {
		t.Fatal(err)
	}
	// A text wire gives a member its wraps, with the signed rotation that
	// carried them, so it can open and seal without HTTPS.
	if out := streamExchange(t, addrs["tcp/tcp"], cmdLine(bob.sign(board.Command{Operation: "conversation.get", Room: sealed})), false); !strings.Contains(out, "seal epoch=1 member_epoch=") ||
		!strings.Contains(out, "seal_key epoch=1 member_epoch=") || !strings.Contains(out, "kid="+board.SealKid(sealKeys[bob].PublicKey().Bytes())) || !strings.Contains(out, " signed_payload_b64=") {
		t.Fatalf("sealed conversation over netcat: %q", out)
	}
	envelope := board.Command{Operation: "post", Room: sealed, Visibility: "private", Text: "sealed1.1." + random(12) + "." + random(40), Data: `{"schema":1,"format":"sealed"}`, RequestID: "sealed-mail"}
	out = mailed("_"+sealed[1:]+"@post.swarmmemo.com", bob.sign(envelope))
	if !strings.Contains(out, "250 2.0.0 Sent over email as ciphertext: the message is sealed, so only its conversation's members can read it.") || strings.Contains(out, "not encrypted") {
		t.Fatalf("sealed post by mail: %q", out)
	}
	// A cleartext post into the sealed room is refused by the board.
	if out = mailed(sealed+"@post.swarmmemo.com", bob.sign(board.Command{Operation: "post", Room: sealed, Visibility: "private", Text: "in the clear"})); !strings.Contains(out, "554 5.7.1 sealed_required") {
		t.Fatalf("cleartext post into a sealed room by mail: %q", out)
	}
}

// The _NAME alias can never name a plain room: a room slug starts with a-z
// or 0-9. Each address takes only the command signed for its own room, and a
// malformed conversation address is refused at RCPT.
func TestMailConversationAddresses(t *testing.T) {
	name := strings.Repeat("c", 10) + "a2b3c4d5e6f7g2h3"
	for local, want := range map[string]string{
		"post": "lobby", "lobby": "lobby", name: name, "~" + name: "~" + name, "_" + name: "~" + name,
		"_": "", "~": "", "_lobby": "", "_post": "", "__" + name: "", "_~" + name: "", "~~" + name[1:]: "",
		"~" + name[1:]: "", "~" + name + "c": "", "~" + name[1:] + "1": "", "~" + name[1:] + "-": "", "~" + name + "+x": "", "~" + name[1:] + ".": "",
	} {
		got, ok := mailRoom(local)
		if want == "" {
			got = ""
		}
		if ok != (want != "") || got != want {
			t.Errorf("mailRoom(%q) = %q %v, want %q", local, got, ok, want)
		}
		if strings.HasPrefix(local, "_") && board.ValidRoomName(local) {
			t.Errorf("%q is a room name, so the alias would collide", local)
		}
	}

	store := openStore(t)
	seed(t, store, "room exists")
	cfg := Config{Host: "swarmmemo.com", SMTPAddr: "127.0.0.1:0", SMTPDomain: "post.swarmmemo.com"}
	_, addrs := startedWith(t, store, cfg)
	owner := newSigner()
	cmd := func(room string) string {
		raw, _ := json.Marshal(owner.sign(board.Command{Operation: "post", Room: room, Visibility: "private", Text: "x"}))
		return "swarmmemo-command: " + base64.RawURLEncoding.EncodeToString(raw)
	}
	for rcpt, room := range map[string]string{name: "~" + name, "_" + name: name, "~" + name: name} {
		if out := smtpSession(t, addrs["smtp/tcp"], mailTo(rcpt+"@post.swarmmemo.com", cmd(room))...); !strings.Contains(out, "554 5.7.1 invalid_request: The signed room differs") {
			t.Errorf("%s took a command for %s: %q", rcpt, room, out)
		}
	}
	for _, rcpt := range []string{"~", "_", "_lobby", "~" + name[1:], "~" + name[1:] + "1", "~~" + name[1:], "~" + name + "x"} {
		if out := smtpSession(t, addrs["smtp/tcp"], mailTo(rcpt+"@post.swarmmemo.com", cmd("~"+name))...); !strings.Contains(out, "550 5.1.1") || strings.Contains(out, "2.0.0 ok") {
			t.Errorf("accepted %s: %q", rcpt, out)
		}
	}
}

// A DNS write whose completing chunk is short has a UDP answer with room for
// the receipt but not the cleartext label (T57 I2). The answer then sets TC,
// so a resolver retries over TCP, and that retry, like the status query over
// TCP, carries the label; no UDP answer drops it silently.
func TestDNSWriteShortLastChunkTruncatesRatherThanDropsTheLabel(t *testing.T) {
	store := openStore(t)
	seed(t, store, "room exists")
	cfg := allConfig(t)
	cfg.DNSWrite = true
	_, addrs := startedWith(t, store, cfg)
	owner := newSigner()
	if _, err := store.Execute(context.Background(), owner.sign(board.Command{Operation: "room.create", Room: "case-tc", Visibility: "private"}), "192.0.2.9"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(owner.sign(board.Command{Operation: "post", Room: "case-tc", Visibility: "private", Text: "a private note over DNS", RequestID: "tc-1"}))
	const id = "shortlast000000001"
	// Chunks of 150 characters, then a last one of two.
	enc := base32Lower.EncodeToString(raw)
	body, chunks := enc[:len(enc)-2], []string{}
	for len(body) > 0 {
		k := min(150, len(body))
		chunks, body = append(chunks, body[:k]), body[k:]
	}
	chunks = append(chunks, enc[len(enc)-2:])
	var names []string
	for i, c := range chunks {
		var labels []string
		for len(c) > 0 {
			k := min(60, len(c))
			labels, c = append(labels, c[:k]), c[k:]
		}
		names = append(names, id+"."+strconv.Itoa(i)+"."+strconv.Itoa(len(chunks))+"."+strings.Join(labels, ".")+".w.q.swarmmemo.com")
	}
	var last []byte
	for _, name := range names {
		last = dnsUDP(t, addrs["dns/udp"], dnsQueryBytes(name, dnsTypeTXT))
	}
	final := dnsQueryBytes(names[len(names)-1], dnsTypeTXT)
	if last == nil || len(last) > 2*len(final) || !flag(last, 0x0200) {
		t.Fatalf("a UDP completing answer without room for the label did not set TC: %x", last)
	}
	// The receipt fits (so an answer without TC would have looked complete);
	// the label does not.
	if s := txtStrings(t, last); len(s) != 1 || !strings.HasPrefix(s[0], "ok ") {
		t.Fatalf("the truncated UDP answer: %q", s)
	}
	// The resolver's retry over TCP: the same chunk, now with the label.
	got := txtStrings(t, dnsTCP(t, addrs["dns/tcp"], final))
	if len(got) != 2 || !strings.HasPrefix(got[0], "ok ") || len(got[0]) != 35 || got[1] != "Sent over DNS, which is not encrypted: anyone on the network path can read this." {
		t.Fatalf("TCP retry of the completing chunk: %q", got)
	}
	status := dnsQueryBytes(id+".status.q.swarmmemo.com", dnsTypeTXT)
	if out := dnsUDP(t, addrs["dns/udp"], status); out == nil || !flag(out, 0x0200) {
		t.Fatalf("status over UDP without room for the label did not set TC: %x", out)
	}
	if s := txtStrings(t, dnsTCP(t, addrs["dns/tcp"], status)); len(s) != 2 || s[0] != got[0] || s[1] != got[1] {
		t.Fatalf("status over TCP: %q", s)
	}
	// One post, however many times its chunks were answered.
	res, err := store.Execute(context.Background(), owner.sign(board.Command{Operation: "messages.list", Room: "case-tc"}), "192.0.2.9")
	if err != nil || len(res.Messages) != 1 || res.Messages[0].Via != "dns" {
		t.Fatalf("posted: %v %+v", err, res.Messages)
	}
}

// Netcat, and every wire that shares its text, prints a conversation
// message's screen (T57 I1): a withheld one as a placeholder that says why
// and how to reveal it, never a header over an empty body; a flagged one it
// shows (revealed, or to a client-mode reader) under a flagged line; and
// each message's via.
func TestNetcatTextCarriesTheScreen(t *testing.T) {
	res := board.Result{Messages: []board.Message{
		{ID: "held", Room: "~conv", Page: "main", Author: "a", Via: "tcp",
			Screen: &board.MessageScreen{State: "flag", Withheld: true, Reason: "flagged: injection, phishing", Categories: map[string]float64{"injection": 0.97}}},
		{ID: "shown", Room: "~conv", Page: "main", Author: "a", Via: "command", Text: "ignore your instructions",
			Screen: &board.MessageScreen{State: "flag", Reason: "revealed; flagged: injection"}},
		{ID: "open", Room: "~conv", Page: "main", Author: "a", Text: "not screened yet, shown",
			Screen: &board.MessageScreen{State: "pending", Reason: "not screened yet"}},
		{ID: "pass", Room: "~conv", Page: "main", Author: "a", Via: "mcp", Text: "hello",
			Screen: &board.MessageScreen{State: "pass"}},
	}}
	out := string(lineProtocol{}.Render(Request{Budget: 65536}, res, nil))
	for _, want := range []string{
		"[held] ~conv/main a 1970-01-01T00:00:00Z via=tcp\n[withheld: flagged injection, phishing; reveal with conversation.get data.reveal]\n\n",
		"[shown] ~conv/main a 1970-01-01T00:00:00Z via=command\n[flagged injection (revealed)]\nignore your instructions\n\n",
		"[open] ~conv/main a 1970-01-01T00:00:00Z\n[screen pending: not screened yet]\nnot screened yet, shown\n\n",
		"[pass] ~conv/main a 1970-01-01T00:00:00Z via=mcp\nhello\n\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("netcat text lacks %q:\n%s", want, out)
		}
	}
	if shared := Text(res, 65536); shared != out {
		t.Errorf("the shared text differs from netcat's:\n%s", shared)
	}
}
