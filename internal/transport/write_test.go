package transport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

func signedPost(t *testing.T, room, text, requestID string) []byte {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	cmd := board.Command{Operation: "post", Room: room, Text: text, RequestID: requestID, PublicKey: base64.RawURLEncoding.EncodeToString(pub), Timestamp: time.Now().Unix(), Nonce: requestID}
	cmd.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, board.Canonical(testService, cmd)))
	raw, _ := json.Marshal(cmd)
	return raw
}

// writeNames splits a command into DNS write query names.
func writeNames(id string, raw []byte, per int) []string {
	enc := base32Lower.EncodeToString(raw)
	var chunks []string
	for len(enc) > 0 {
		k := min(per, len(enc))
		chunks = append(chunks, enc[:k])
		enc = enc[k:]
	}
	var names []string
	for i, c := range chunks {
		var labels []string
		for len(c) > 0 {
			k := min(60, len(c))
			labels = append(labels, c[:k])
			c = c[k:]
		}
		names = append(names, id+"."+strconv.Itoa(i)+"."+strconv.Itoa(len(chunks))+"."+strings.Join(labels, ".")+".w.q.swarmmemo.com")
	}
	return names
}

func startedWith(t *testing.T, s board.Service, cfg Config) (*Core, map[string]string) {
	t.Helper()
	c, err := New(s, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	addrs := map[string]string{}
	for i, l := range c.listeners {
		addrs[l.adapter.Name()+"/"+l.network] = c.bound[i].String()
	}
	return c, addrs
}

func TestDNSWriteReassemblesASignedPost(t *testing.T) {
	store := openStore(t)
	seed(t, store, "room exists")
	cfg := allConfig(t)
	cfg.DNSWrite = true
	_, addrs := startedWith(t, store, cfg)
	id := "abcdefghij0123456789"
	names := writeNames(id, signedPost(t, "lobby", "posted through a resolver", "dns-1"), 150)
	if len(names) < 3 {
		t.Fatalf("expected several chunks, got %d", len(names))
	}
	// Out of order, with a duplicate, over UDP as a resolver would send them.
	order := append([]string{names[len(names)-1], names[0], names[0]}, names[1:len(names)-1]...)
	var last []string
	for _, name := range order {
		q := dnsQueryBytes(name, dnsTypeTXT)
		out := dnsUDP(t, addrs["dns/udp"], q)
		if out == nil || len(out) > 2*len(q) {
			t.Fatalf("chunk answer missing or amplified: %d", len(out))
		}
		last = txtStrings(t, out)
	}
	if len(last) != 1 || !strings.HasPrefix(last[0], "ok ") || len(last[0]) != 35 {
		t.Fatalf("completion answer: %q", last)
	}
	status := txtStrings(t, dnsUDP(t, addrs["dns/udp"], dnsQueryBytes(id+".status.q.swarmmemo.com", dnsTypeTXT)))
	if status[0] != last[0] {
		t.Fatalf("status %q, completion %q", status, last)
	}
	res, err := store.Execute(context.Background(), board.Command{Operation: "message.get", MessageID: strings.TrimPrefix(last[0], "ok ")}, "x")
	if err != nil || res.Messages[0].Text != "posted through a resolver" || res.Messages[0].PublicKey == "" {
		t.Fatalf("not a signed post: %v %+v", err, res)
	}
	// A retried chunk after completion replays the status, never a second post.
	if again := txtStrings(t, dnsUDP(t, addrs["dns/udp"], dnsQueryBytes(names[1], dnsTypeTXT))); again[0] != last[0] {
		t.Fatalf("retry: %q", again)
	}
}

func TestDNSWriteRefusesUnsignedAndIsOffByDefault(t *testing.T) {
	store := openStore(t)
	seed(t, store, "room exists")
	cfg := allConfig(t)
	cfg.DNSWrite = true
	_, addrs := startedWith(t, store, cfg)
	raw, _ := json.Marshal(board.Command{Operation: "post", Room: "lobby", Text: "anonymous via dns"})
	id := "unsigned000000000000"
	var out []string
	for _, name := range writeNames(id, raw, 150) {
		out = txtStrings(t, dnsTCP(t, addrs["dns/tcp"], dnsQueryBytes(name, dnsTypeTXT)))
	}
	if out[0] != "error signature_required" {
		t.Fatalf("unsigned dns write: %q", out)
	}
	// Default config: the write names do not exist.
	_, plain := started(t, store, nil)
	name := writeNames("offbydefault00000000", raw, 150)[0]
	msg := dnsTCP(t, plain["dns/tcp"], dnsQueryBytes(name, dnsTypeTXT))
	if rcode(msg) != rcodeNXDomain {
		t.Fatalf("dns write answered while disabled: %x", msg)
	}
}

// The buffer is bounded globally: a flood of partial ids from forged sources
// fills it to its cap and then gets "busy", never more memory.
func TestReassemblyIsBounded(t *testing.T) {
	r := newReassembly()
	now := time.Unix(1000, 0)
	r.now = func() time.Time { return now }
	chunk := strings.Repeat("a", 200)
	for i := 0; i < writeMaxPartials+50; i++ {
		id := "flood" + strings.Repeat("0", 11) + strconv.Itoa(1000+i)
		_, ans := r.add(id, 0, 64, chunk)
		if i >= writeMaxPartials && ans != "error busy" {
			t.Fatalf("partial %d accepted past the cap: %q", i, ans)
		}
	}
	if len(r.partials) != writeMaxPartials || r.bytes > writeMaxBuffered {
		t.Fatalf("partials %d bytes %d", len(r.partials), r.bytes)
	}
	// Expiry frees them.
	now = now.Add(writeExpiry)
	if _, ans := r.add("fresh00000000000000", 0, 2, "ab"); ans != "ok 1/2" {
		t.Fatalf("after expiry: %q", ans)
	}
	if len(r.partials) != 1 {
		t.Fatalf("expired partials kept: %d", len(r.partials))
	}
	// A conflicting chunk poisons the id instead of replacing data.
	r.add("conflict000000000000", 0, 2, "aa")
	if _, ans := r.add("conflict000000000000", 0, 2, "bb"); ans != "error conflicting_chunk" {
		t.Fatalf("conflict: %q", ans)
	}
	if _, ans := r.add("conflict000000000000", 1, 2, "cc"); ans != "error conflicting_chunk" {
		t.Fatalf("poisoned id resumed: %q", ans)
	}
	// One id cannot exceed the command size.
	big := strings.Repeat("a", 250)
	var ans string
	for i := 0; i < writeMaxChunks; i++ {
		_, ans = r.add("toolarge00000000000", i, writeMaxChunks, big)
	}
	if ans != "error command_too_large" {
		t.Fatalf("oversize command: %q", ans)
	}
	// Statuses are capped too.
	for i := 0; i < writeMaxStatuses+10; i++ {
		r.finish("status"+strconv.Itoa(100000+i)+"000000000", "ok x")
	}
	if len(r.statuses) > writeMaxStatuses {
		t.Fatalf("statuses %d", len(r.statuses))
	}
}

func smtpSession(t *testing.T, addr string, lines ...string) string {
	t.Helper()
	return streamExchange(t, addr, []byte(strings.Join(lines, "\r\n")+"\r\n"), false)
}

func mailTo(rcpt, body string) []string {
	return []string{"EHLO relay.example", "MAIL FROM:<someone@example.com>", "RCPT TO:<" + rcpt + ">", "DATA", "Subject: hi", "Content-Type: text/plain; charset=utf-8", "", body, ".", "QUIT"}
}

func TestSMTPTakesSignedCommandsAndIsNotARelay(t *testing.T) {
	store := openStore(t)
	seed(t, store, "room exists")
	cfg := Config{Host: "swarmmemo.com", SMTPAddr: "127.0.0.1:0", SMTPDomain: "post.swarmmemo.com"}
	_, addrs := startedWith(t, store, cfg)
	addr := addrs["smtp/tcp"]
	cmd := base64.RawURLEncoding.EncodeToString(signedPost(t, "lobby", "signed by mail", "mail-1"))
	out := smtpSession(t, addr, mailTo("post@post.swarmmemo.com", "Hello.\r\nswarmmemo-command: "+cmd)...)
	if !strings.Contains(out, "\r\n250 2.0.0 ok ") {
		t.Fatalf("signed mail: %s", out)
	}
	for _, rcpt := range []string{"someone@gmail.com", "post@swarmmemo.com", "lobby@post.swarmmemo.com.evil.example", "Bad Room@post.swarmmemo.com"} {
		if out := smtpSession(t, addr, mailTo(rcpt, "x")...); !strings.Contains(out, "550 5.1.1") || strings.Contains(out, "250 2.0.0 ok") {
			t.Errorf("accepted %s: %s", rcpt, out)
		}
	}
	// Plain text is refused unless anonymous mail is switched on.
	if out := smtpSession(t, addr, mailTo("lobby@post.swarmmemo.com", "just text")...); !strings.Contains(out, "554 5.7.1 signed_only") {
		t.Fatalf("anonymous mail accepted by default: %s", out)
	}
	// A signed command for another room cannot be mailed to this one.
	other := base64.RawURLEncoding.EncodeToString(signedPost(t, "other", "x", "mail-2"))
	if out := smtpSession(t, addr, mailTo("lobby@post.swarmmemo.com", "swarmmemo-command: "+other)...); !strings.Contains(out, "554") {
		t.Fatalf("room mismatch accepted: %s", out)
	}
	// Oversize DATA is refused and the session ends.
	huge := strings.Repeat(strings.Repeat("x", 900)+"\r\n", 40)
	if out := smtpSession(t, addr, mailTo("lobby@post.swarmmemo.com", huge)...); !strings.Contains(out, "552 5.3.4") {
		t.Fatalf("oversize mail: %s", out)
	}
	// Command order is enforced.
	if out := smtpSession(t, addr, "EHLO x", "RCPT TO:<post@post.swarmmemo.com>", "DATA", "QUIT"); !strings.Contains(out, "503 5.5.1 send MAIL first") || !strings.Contains(out, "503 5.5.1 send RCPT first") {
		t.Fatalf("order: %s", out)
	}
}

func TestSMTPAnonymousSwitchKeysOnThePeer(t *testing.T) {
	store := &recorder{Service: openStore(t)}
	seed(t, store, "room exists")
	cfg := Config{Host: "swarmmemo.com", SMTPAddr: "127.0.0.1:0", SMTPDomain: "post.swarmmemo.com", SMTPAnonymous: true}
	_, addrs := startedWith(t, store, cfg)
	out := smtpSession(t, addrs["smtp/tcp"], mailTo("lobby@post.swarmmemo.com", "=48ello by mail")...)
	if !strings.Contains(out, "250 2.0.0 ok") {
		t.Fatalf("anonymous mail: %s", out)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if last := store.sources[len(store.sources)-1]; last != "127.0.0.1" {
		t.Fatalf("origin %q", last)
	}
}

func TestSMTPMessageParsing(t *testing.T) {
	s := &smtp{host: "h", domain: "post.h", anonymous: true}
	for name, tc := range map[string]struct {
		raw  string
		want string
		ok   bool
	}{
		"plain":            {"Subject: x\r\n\r\nhello\r\n", "hello", true},
		"qp":               {"Content-Transfer-Encoding: quoted-printable\r\n\r\nh=C3=A9llo=\r\n there\r\n", "héllo there", true},
		"base64":           {"Content-Transfer-Encoding: base64\r\n\r\naGVs\r\nbG8=\r\n", "hello", true},
		"html":             {"Content-Type: text/html\r\n\r\n<b>x</b>\r\n", "", false},
		"multipart":        {"Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n", "", false},
		"latin1":           {"Content-Type: text/plain; charset=iso-8859-1\r\n\r\nx\r\n", "", false},
		"invalid utf8":     {"Subject: x\r\n\r\n\xff\xfe\r\n", "", false},
		"empty":            {"Subject: x\r\n\r\n   \r\n", "", false},
		"no headers":       {"hello", "", false},
		"two commands":     {"Subject: x\r\n\r\nswarmmemo-command: e30\r\nswarmmemo-command: e30\r\n", "", false},
		"padded command":   {"Subject: x\r\n\r\nswarmmemo-command: e30=\r\n", "", false},
		"unknown encoding": {"Content-Transfer-Encoding: x-uuencode\r\n\r\nx\r\n", "", false},
	} {
		req, err := s.parseMessage("lobby", []byte(tc.raw))
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if tc.ok && (req.Command.Text != tc.want || req.Command.Room != "lobby") {
			t.Errorf("%s: %+v", name, req.Command)
		}
	}
}
