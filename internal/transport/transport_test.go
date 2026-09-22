package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/httpapi"
)

const testService = "swarmmemo.com"

func openStore(t *testing.T) *board.Store {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "t.db"), board.Config{ServiceID: testService, DailyBytes: 1 << 20, AnonymousDailyBytes: 1 << 20, GlobalDailyBytes: 1 << 24, MaxTextBytes: 16384})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func seed(t *testing.T, s board.Service, text string) string {
	t.Helper()
	res, err := s.Execute(context.Background(), board.Command{Operation: "post", Room: "lobby", Text: text}, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	return res.Receipt.ID
}

func certFiles(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	dir := t.TempDir()
	cert, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	_ = os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600)
	_ = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600)
	return cert, keyPath
}

func allConfig(t *testing.T) Config {
	cert, key := certFiles(t)
	loop := "127.0.0.1:0"
	return Config{Host: "localhost", DNSAddr: loop, DNSZone: "q.swarmmemo.com", DNSNameServer: "swarmmemo.com", TCPAddr: loop, GeminiAddr: loop, GeminiCert: cert, GeminiKey: key, GopherAddr: loop, FingerAddr: loop, SMTPAddr: loop, SMTPDomain: "post.swarmmemo.com"}
}

// started runs every transport on loopback and returns the address of each
// listener, keyed "name/network".
func started(t *testing.T, s board.Service, limiter *httpapi.Limiter) (*Core, map[string]string) {
	t.Helper()
	c, err := New(s, limiter, allConfig(t))
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

// client speaks one wire. Every client sends one request and reads the whole
// answer, the way the protocols themselves work.
type client struct {
	name  string
	key   string
	frame func(req string) []byte
}

func streamExchange(t *testing.T, addr string, payload []byte, useTLS bool) string {
	t.Helper()
	var conn net.Conn
	var err error
	if useTLS {
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, &tls.Config{InsecureSkipVerify: true, ServerName: "localhost"})
	} else {
		conn, err = net.DialTimeout("tcp", addr, 5*time.Second)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	_, _ = conn.Write(payload)
	out, _ := io.ReadAll(conn)
	return string(out)
}

var textClients = []client{
	{name: "tcp", key: "tcp/tcp", frame: func(r string) []byte { return []byte(r + "\n") }},
	{name: "gemini", key: "gemini/tcp", frame: func(r string) []byte { return []byte("gemini://localhost" + r + "\r\n") }},
	{name: "gopher", key: "gopher/tcp", frame: func(r string) []byte { return []byte(r + "\r\n") }},
	{name: "finger", key: "finger/tcp", frame: func(r string) []byte { return []byte(r + "\r\n") }},
}

// readRequests is the same read expressed in each wire's own words.
var readRequests = map[string]string{"tcp": "READ lobby 5", "gemini": "/room/lobby", "gopher": "/room/lobby", "finger": "lobby"}

func dnsQueryBytes(name string, qtype uint16) []byte {
	b := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range strings.Split(name, ".") {
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	b = append(b, 0)
	b = binary.BigEndian.AppendUint16(b, qtype)
	return binary.BigEndian.AppendUint16(b, dnsClassIN)
}

func dnsTCP(t *testing.T, addr string, query []byte) []byte {
	t.Helper()
	out := streamExchange(t, addr, append(binary.BigEndian.AppendUint16(nil, uint16(len(query))), query...), false)
	if len(out) < 2 {
		return nil
	}
	return []byte(out[2:])
}

func dnsUDP(t *testing.T, addr string, query []byte) []byte {
	t.Helper()
	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write(query)
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return nil
	}
	return buf[:n]
}

// txtStrings extracts the TXT strings of the first answer in a response.
func txtStrings(t *testing.T, msg []byte) []string {
	t.Helper()
	// Reuse the question parser on a copy that looks like a query.
	query := append([]byte{msg[0], msg[1], 0, 0, 0, 1, 0, 0, 0, 0, 0, 0}, msg[12:]...)
	q, err := parseDNSQuery(query[:min(len(query), dnsMaxQuery)])
	if err != nil || binary.BigEndian.Uint16(msg[6:8]) != 1 {
		t.Fatalf("no answer: %v %x", err, msg)
	}
	off := 12 + len(q.raw) + 2 + 8
	size := int(binary.BigEndian.Uint16(msg[off : off+2]))
	data := msg[off+2 : off+2+size]
	var out []string
	for len(data) > 0 {
		n := int(data[0])
		out = append(out, string(data[1:1+n]))
		data = data[1+n:]
	}
	return out
}

func TestSameReadThroughEveryTransport(t *testing.T) {
	store := openStore(t)
	id := seed(t, store, "hello from the shared service")
	_, addrs := started(t, store, nil)
	for _, c := range textClients {
		out := streamExchange(t, addrs[c.key], c.frame(readRequests[c.name]), c.name == "gemini")
		if !strings.Contains(out, "hello from the shared service") || !strings.Contains(out, id) {
			t.Errorf("%s read did not return the shared message:\n%s", c.name, out)
		}
	}
	// DNS reads the same message by id, and the head names it.
	for _, network := range []string{"tcp", "udp"} {
		var head []byte
		if network == "tcp" {
			head = dnsTCP(t, addrs["dns/tcp"], dnsQueryBytes("head.q.swarmmemo.com", dnsTypeTXT))
		} else {
			head = dnsUDP(t, addrs["dns/udp"], dnsQueryBytes("head.q.swarmmemo.com", dnsTypeTXT))
		}
		if network == "udp" {
			if len(head) > 2*len(dnsQueryBytes("head.q.swarmmemo.com", dnsTypeTXT)) {
				t.Fatalf("udp answer exceeds twice the query: %d", len(head))
			}
			continue
		}
		if s := txtStrings(t, head); len(s) < 2 || s[1] != id {
			t.Fatalf("head over %s: %q", network, s)
		}
	}
	msg := dnsTCP(t, addrs["dns/tcp"], dnsQueryBytes(id+".m.q.swarmmemo.com", dnsTypeTXT))
	if s := txtStrings(t, msg); len(s) < 2 || s[1] != "hello from the shared service" {
		t.Fatalf("message over dns: %q", s)
	}
}

// recorder is the board with a window onto the origin each call carried.
type recorder struct {
	board.Service
	mu      sync.Mutex
	sources []string
	panics  atomic.Bool
}

func (r *recorder) Execute(ctx context.Context, c board.Command, source string) (board.Result, error) {
	if r.panics.Load() {
		panic("hostile input reached a bug")
	}
	r.mu.Lock()
	r.sources = append(r.sources, source)
	r.mu.Unlock()
	return r.Service.Execute(ctx, c, source)
}

func TestPostsReachTheSameServiceWithThePeerAsOrigin(t *testing.T) {
	store := &recorder{Service: openStore(t)}
	seed(t, store, "room exists")
	_, addrs := started(t, store, nil)
	out := streamExchange(t, addrs["tcp/tcp"], []byte("POST lobby posted over tcp\n"), false)
	if !strings.HasPrefix(out, "ok ") {
		t.Fatalf("tcp post: %s", out)
	}
	out = streamExchange(t, addrs["gemini/tcp"], []byte("gemini://localhost/post/lobby?posted%20over%20gemini\r\n"), true)
	if !strings.HasPrefix(out, "20 ") || !strings.Contains(out, "ok ") {
		t.Fatalf("gemini post: %s", out)
	}
	for _, text := range []string{"posted over tcp", "posted over gemini"} {
		res, err := store.Execute(context.Background(), board.Command{Operation: "messages.list", Room: "lobby", Query: text}, "127.0.0.1")
		if err != nil || len(res.Messages) != 1 {
			t.Fatalf("%q not stored: %v %d", text, err, len(res.Messages))
		}
	}
	// Every call carried the TCP peer in the form HTTP's peer() produces, so an
	// anonymous post spends that address's one allowance.
	store.mu.Lock()
	for _, source := range store.sources[1:] {
		if source != "127.0.0.1" {
			t.Fatalf("origin was %q, not the peer address", source)
		}
	}
	store.mu.Unlock()
	// Read-only wires cannot post: their grammar has no write.
	for _, c := range []struct{ key, frame string }{{"gopher/tcp", "POST lobby x\r\n"}, {"finger/tcp", "POST lobby x\r\n"}} {
		out := streamExchange(t, addrs[c.key], []byte(c.frame), false)
		if strings.Contains(out, "ok ") {
			t.Fatalf("%s accepted a write: %s", c.key, out)
		}
	}
	// A transport cannot mint a room.
	out = streamExchange(t, addrs["tcp/tcp"], []byte("POST newroom hello\n"), false)
	if !strings.Contains(out, "public_rooms_only") {
		t.Fatalf("tcp created a room: %s", out)
	}
}

func TestSignedCommandOverTCPIsVerifiedByTheBoard(t *testing.T) {
	store := openStore(t)
	seed(t, store, "room exists")
	_, addrs := started(t, store, nil)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	cmd := board.Command{Operation: "post", Room: "lobby", Text: "signed over netcat", RequestID: "r1", PublicKey: base64.RawURLEncoding.EncodeToString(pub), Timestamp: time.Now().Unix(), Nonce: "n1"}
	cmd.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, board.Canonical(testService, cmd)))
	raw, _ := json.Marshal(cmd)
	out := streamExchange(t, addrs["tcp/tcp"], []byte("CMD "+base64.RawURLEncoding.EncodeToString(raw)+"\n"), false)
	if !strings.HasPrefix(out, "ok ") {
		t.Fatalf("signed post: %s", out)
	}
	cmd.Text = "tampered"
	raw, _ = json.Marshal(cmd)
	out = streamExchange(t, addrs["tcp/tcp"], []byte("CMD "+base64.RawURLEncoding.EncodeToString(raw)+"\n"), false)
	if !strings.Contains(out, "invalid_signature") {
		t.Fatalf("tampered command accepted: %s", out)
	}
	for _, op := range []string{"agent.rotate", "room.create", "credit.transfer", "blob.put"} {
		raw, _ = json.Marshal(board.Command{Operation: op})
		out = streamExchange(t, addrs["tcp/tcp"], []byte("CMD "+base64.RawURLEncoding.EncodeToString(raw)+"\n"), false)
		if !strings.Contains(out, "unsupported_operation") {
			t.Fatalf("%s crossed a constrained transport: %s", op, out)
		}
	}
}
