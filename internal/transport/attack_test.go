package transport

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/httpapi"
)

type signer struct {
	pub  string
	priv ed25519.PrivateKey
	n    int
}

func newSigner() *signer {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	return &signer{pub: base64.RawURLEncoding.EncodeToString(pub), priv: priv}
}

func (s *signer) sign(cmd board.Command) board.Command {
	s.n++
	cmd.PublicKey, cmd.Timestamp, cmd.Nonce = s.pub, time.Now().Unix(), "n"+strings.Repeat("x", s.n)
	cmd.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.priv, board.Canonical(testService, cmd)))
	return cmd
}

// A private room's name, messages and existence never cross a constrained
// wire, whichever read the wire offers.
func TestPrivateRoomNeverCrossesATransport(t *testing.T) {
	store := openStore(t)
	seed(t, store, "room exists")
	owner := newSigner()
	ctx := context.Background()
	if _, err := store.Execute(ctx, owner.sign(board.Command{Operation: "room.create", Room: "sekrit", Visibility: "private"}), "192.0.2.9"); err != nil {
		t.Fatal(err)
	}
	res, err := store.Execute(ctx, owner.sign(board.Command{Operation: "post", Room: "sekrit", Text: "classified-payload"}), "192.0.2.9")
	if err != nil {
		t.Fatal(err)
	}
	id := res.Receipt.ID
	_, addrs := started(t, store, nil)
	reads := map[string][]string{
		"tcp":    {"READ sekrit", "THREAD " + id, "ROOMS"},
		"gemini": {"/room/sekrit", "/thread/" + id, "/"},
		"gopher": {"/room/sekrit", "/thread/" + id, "/"},
		"finger": {"sekrit", ""},
	}
	for _, cl := range textClients {
		for _, r := range reads[cl.name] {
			out := streamExchange(t, addrs[cl.key], cl.frame(r), cl.name == "gemini")
			if strings.Contains(out, "classified-payload") || strings.Contains(out, "sekrit") && !strings.Contains(r, "sekrit") {
				t.Errorf("%s %q leaked the private room: %q", cl.name, r, out)
			}
		}
	}
	for _, name := range []string{id + ".m.q.swarmmemo.com", "sekrit.rooms.q.swarmmemo.com", "rooms.q.swarmmemo.com", "head.q.swarmmemo.com"} {
		out := dnsTCP(t, addrs["dns/tcp"], dnsQueryBytes(strings.ToLower(name), dnsTypeTXT))
		if len(out) < 12 || strings.Contains(string(out), "classified") || (name == "head.q.swarmmemo.com" && strings.Contains(string(out[12:]), id)) || (name == "rooms.q.swarmmemo.com" && strings.Contains(string(out), "sekrit")) {
			t.Errorf("dns %s leaked the private room: %q", name, out)
		}
	}
	// A member's own signed post cannot use a constrained wire to write there.
	raw, _ := json.Marshal(owner.sign(board.Command{Operation: "post", Room: "sekrit", Text: "over nc"}))
	if out := streamExchange(t, addrs["tcp/tcp"], []byte("CMD "+base64.RawURLEncoding.EncodeToString(raw)+"\n"), false); !strings.Contains(out, "public_rooms_only") {
		t.Fatalf("signed post reached a private room: %q", out)
	}
	// Nor can a signed read, which would be the member's authenticated view.
	raw, _ = json.Marshal(owner.sign(board.Command{Operation: "messages.list", Room: "sekrit"}))
	if out := streamExchange(t, addrs["tcp/tcp"], []byte("CMD "+base64.RawURLEncoding.EncodeToString(raw)+"\n"), false); strings.Contains(out, "classified") || !strings.Contains(out, "https_required") {
		t.Fatalf("signed read crossed a transport: %q", out)
	}
	// Identity links are HTTPS-only writes: no constrained wire carries them,
	// signed or not, and the signed-only wires refuse them too.
	for _, op := range []string{"identity.link", "identity.unlink"} {
		raw, _ = json.Marshal(owner.sign(board.Command{Operation: op, Data: `{"schema":1,"kind":"domain","value":"example.org"}`}))
		if out := streamExchange(t, addrs["tcp/tcp"], []byte("CMD "+base64.RawURLEncoding.EncodeToString(raw)+"\n"), false); !strings.Contains(out, "unsupported_operation") {
			t.Fatalf("%s crossed a transport: %q", op, out)
		}
		mail := smtpSession(t, addrs["smtp/tcp"], mailTo("post@post.swarmmemo.com", "swarmmemo-command: "+base64.RawURLEncoding.EncodeToString(raw))...)
		if strings.Contains(mail, "250 2.0.0 ok") {
			t.Fatalf("%s crossed smtp: %q", op, mail)
		}
	}
}

// Every message an SMTP session submits spends the peer's shared budget, not
// only the connection that carried it: one admission must not buy a
// session's worth of board calls.
func TestSMTPSpendsTheBudgetPerMessage(t *testing.T) {
	store := openStore(t)
	seed(t, store, "room exists")
	limiter := httpapi.NewLimiter()
	cfg := Config{Host: "swarmmemo.com", SMTPAddr: "127.0.0.1:0", SMTPDomain: "post.swarmmemo.com", SMTPAnonymous: true}
	c, err := New(store, limiter, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", c.bound[0].String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	if line, _ := r.ReadString('\n'); !strings.HasPrefix(line, "220 ") {
		t.Fatalf("banner %q", line)
	}
	// The connection was admitted; now the peer's budget runs out.
	for limiter.Admit("127.0.0.1") {
	}
	_, _ = conn.Write([]byte("HELO x\r\nMAIL FROM:<a@b>\r\nRCPT TO:<lobby@post.swarmmemo.com>\r\nDATA\r\nSubject: x\r\n\r\nhello\r\n.\r\nQUIT\r\n"))
	var out strings.Builder
	for {
		line, err := r.ReadString('\n')
		out.WriteString(line)
		if err != nil || strings.HasPrefix(line, "221") {
			break
		}
	}
	if strings.Contains(out.String(), "250 2.0.0 ok ") || !strings.Contains(out.String(), "451 4.7.1 request_rate") {
		t.Fatalf("a message past the peer's budget was accepted: %q", out.String())
	}
}

// One unspoofed source cannot hold the global write table: after its own cap
// it is told busy, and another source can still start a write.
func TestReassemblyCapsPartialsPerSource(t *testing.T) {
	r := newReassembly()
	for i := 0; i < writeMaxPartials; i++ {
		id := "hog" + strings.Repeat("0", 13) + strconv.Itoa(1000+i)
		_, ans := r.addFrom("198.51.100.1", id, 0, 2, "aa")
		if i < writeMaxPerSource && ans != "ok 1/2" || i >= writeMaxPerSource && ans != "error busy" {
			t.Fatalf("partial %d from one source: %q", i, ans)
		}
	}
	if _, ans := r.addFrom("198.51.100.2", "other00000000000000", 0, 2, "aa"); ans != "ok 1/2" {
		t.Fatalf("another source was locked out: %q", ans)
	}
	// Its existing partials still complete.
	if _, ans := r.addFrom("198.51.100.1", "hog00000000000001000", 1, 2, "aa"); ans != "" {
		t.Fatalf("a capped source could not finish its own write: %q", ans)
	}
}
