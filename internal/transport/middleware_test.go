package transport

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/httpapi"
)

// The middleware is shared: the same oversize frame is refused by every wire,
// counted the same way, and the connection closes.
func TestOversizeFrameRefusedIdentically(t *testing.T) {
	store := openStore(t)
	c, addrs := started(t, store, nil)
	for _, cl := range textClients {
		stats := c.stats[cl.name]
		before := stats.rejected.Load()
		out := streamExchange(t, addrs[cl.key], cl.frame(strings.Repeat("a", 9000)), cl.name == "gemini")
		if !strings.Contains(out, "request_too_large") {
			t.Errorf("%s did not refuse an oversize frame: %q", cl.name, out)
		}
		if stats.rejected.Load() != before+1 {
			t.Errorf("%s oversize not counted", cl.name)
		}
	}
	// DNS over TCP: a length prefix beyond the query cap is refused before any
	// allocation, and the connection closes without an answer.
	before := c.stats["dns"].rejected.Load()
	if out := streamExchange(t, addrs["dns/tcp"], []byte{0xff, 0xff, 1, 2, 3}, false); out != "" {
		t.Fatalf("dns answered an oversize frame: %x", out)
	}
	if c.stats["dns"].rejected.Load() != before+1 {
		t.Fatal("dns oversize not counted")
	}
	// SMTP: an overlong command line is refused the same way, and counted.
	before = c.stats["smtp"].rejected.Load()
	if out := streamExchange(t, addrs["smtp/tcp"], []byte(strings.Repeat("a", 9000)+"\r\n"), false); !strings.Contains(out, "500 5.5.2 line too long") {
		t.Fatalf("smtp oversize: %q", out)
	}
	if c.stats["smtp"].rejected.Load() != before+1 {
		t.Fatal("smtp oversize not counted")
	}
	// Over UDP an oversize datagram gets silence.
	if out := dnsUDP(t, addrs["dns/udp"], make([]byte, 600)); out != nil {
		t.Fatal("dns answered an oversize datagram")
	}
}

// One limiter: exhausting a peer's HTTP budget exhausts it on every
// connection-oriented wire too, while UDP, whose source can be forged, keeps
// its own table and cannot be used to lock a real client out.
func TestRateLimitIsSharedAndKeyedOnPeer(t *testing.T) {
	store := openStore(t)
	seed(t, store, "room exists")
	limiter := httpapi.NewLimiter()
	c, addrs := started(t, store, limiter)
	api := httpapi.New(store, nil, httpapi.Config{Limiter: limiter})
	for limiter.Admit("127.0.0.1") {
	}
	w := httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest("GET", "/api/rooms", nil)) // another peer
	if w.Code != 200 {
		t.Fatalf("another peer was limited: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/api/rooms", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	w = httptest.NewRecorder()
	api.ServeHTTP(w, r)
	if w.Code != 429 {
		t.Fatalf("HTTP did not share the exhausted budget: %d", w.Code)
	}
	for _, cl := range textClients {
		before := c.stats[cl.name].rateLimited.Load()
		out := streamExchange(t, addrs[cl.key], cl.frame(readRequests[cl.name]), cl.name == "gemini")
		if !strings.Contains(out, "request_rate") && !strings.HasPrefix(out, "44 ") {
			t.Errorf("%s ignored the shared limit: %q", cl.name, out)
		}
		if c.stats[cl.name].rateLimited.Load() != before+1 {
			t.Errorf("%s limit not counted", cl.name)
		}
	}
	if out := dnsTCP(t, addrs["dns/tcp"], dnsQueryBytes("head.q.swarmmemo.com", dnsTypeTXT)); out != nil {
		t.Fatal("dns over tcp ignored the shared limit")
	}
	before := c.stats["smtp"].rateLimited.Load()
	if out := smtpSession(t, addrs["smtp/tcp"], "EHLO x", "QUIT"); out != "" || c.stats["smtp"].rateLimited.Load() != before+1 {
		t.Fatalf("smtp ignored the shared limit: %q", out)
	}
	if out := dnsUDP(t, addrs["dns/udp"], dnsQueryBytes("q.swarmmemo.com", dnsTypeSOA)); out == nil {
		t.Fatal("udp shared the HTTP table; forged sources could lock out real clients")
	}
}

func TestPerPeerConnectionCap(t *testing.T) {
	c := &Core{peers: map[string]int{}}
	for i := 0; i < maxPeerConns; i++ {
		if !c.acquirePeer("198.51.100.7") {
			t.Fatal("refused below the cap")
		}
	}
	if c.acquirePeer("198.51.100.7") {
		t.Fatal("one peer exceeded the cap")
	}
	if !c.acquirePeer("198.51.100.8") {
		t.Fatal("one peer's cap blocked another")
	}
	for i := 0; i < maxPeerConns; i++ {
		c.releasePeer("198.51.100.7")
	}
	c.releasePeer("198.51.100.8")
	if len(c.peers) != 0 {
		t.Fatalf("peer table leaks: %v", c.peers)
	}
}

// A conversation that drips one line just inside the idle deadline still ends
// at the total deadline: per-line deadlines never extend it.
func TestDialogueIdleDeadlineNeverPassesTheTotal(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	defer server.Close()
	x := &Exchange{conn: server, r: bufio.NewReader(server), max: 100, budget: 1000, stats: &counters{}, end: time.Now().Add(50 * time.Millisecond)}
	start := time.Now()
	if _, err := x.ReadLine(); err == nil || time.Since(start) > frameTimeout/2 {
		t.Fatalf("read outlived the conversation deadline: %v after %v", err, time.Since(start))
	}
}

func TestPanicIsRecoveredOnEveryWire(t *testing.T) {
	store := &recorder{Service: openStore(t)}
	store.panics.Store(true)
	c, addrs := started(t, store, nil)
	for _, cl := range textClients {
		_ = streamExchange(t, addrs[cl.key], cl.frame(readRequests[cl.name]), cl.name == "gemini")
		if c.stats[cl.name].panics.Load() != 1 {
			t.Errorf("%s panic not recovered and counted", cl.name)
		}
	}
	_ = dnsTCP(t, addrs["dns/tcp"], dnsQueryBytes("head.q.swarmmemo.com", dnsTypeTXT))
	_ = dnsUDP(t, addrs["dns/udp"], dnsQueryBytes("head.q.swarmmemo.com", dnsTypeTXT))
	if c.stats["dns"].panics.Load() != 2 {
		t.Errorf("dns panics: %d", c.stats["dns"].panics.Load())
	}
	_ = smtpSession(t, addrs["smtp/tcp"], mailTo("post@post.swarmmemo.com", "swarmmemo-command: "+base64.RawURLEncoding.EncodeToString(signedPost(t, "lobby", "x", "p1")))...)
	if c.stats["smtp"].panics.Load() != 1 {
		t.Errorf("smtp panic not recovered and counted")
	}
	store.panics.Store(false)
	if out := streamExchange(t, addrs["tcp/tcp"], []byte("HELP\n"), false); !strings.Contains(out, "READ <room>") {
		t.Fatalf("listener died after a panic: %q", out)
	}
}

func TestCapabilitiesAdvertiseExactlyWhatIsEnabled(t *testing.T) {
	store := openStore(t)
	none, err := New(store, nil, ConfigFromEnv(func(string) string { return "" }, "https://swarmmemo.com"))
	if err != nil || len(none.Capabilities()) != 0 || len(none.listeners) != 0 {
		t.Fatalf("an unconfigured deploy enabled something: %v %v", err, none.Capabilities())
	}
	caps := func(cfg httpapi.Config) []any {
		w := httptest.NewRecorder()
		httpapi.New(store, nil, cfg).ServeHTTP(w, httptest.NewRequest("GET", "/capabilities", nil))
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		list, ok := body["transports"].([]any)
		if !ok {
			t.Fatalf("transports is not a list: %v", body["transports"])
		}
		return list
	}
	if got := caps(httpapi.Config{Transports: none.Capabilities()}); len(got) != 0 {
		t.Fatalf("disabled transports advertised: %v", got)
	}
	env := map[string]string{"SWARMMEMO_TRANSPORT_TCP_ADDR": ":4242", "SWARMMEMO_TRANSPORT_FINGER_ADDR": ":79"}
	some, err := New(store, nil, ConfigFromEnv(func(k string) string { return env[k] }, "https://swarmmemo.com"))
	if err != nil {
		t.Fatal(err)
	}
	got := caps(httpapi.Config{Transports: some.Capabilities()})
	if len(got) != 2 {
		t.Fatalf("expected tcp and finger only: %v", got)
	}
	tcp := got[0].(map[string]any)
	if tcp["name"] != "tcp" || tcp["address"] != "swarmmemo.com:4242" || tcp["access"] != "read+write" || len(tcp["write_verbs"].([]any)) == 0 {
		t.Fatalf("tcp entry: %v", tcp)
	}
	fg := got[1].(map[string]any)
	if fg["name"] != "finger" || fg["access"] != "read" || len(fg["write_verbs"].([]any)) != 0 {
		t.Fatalf("finger entry: %v", fg)
	}
	all, err := New(store, nil, allConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, c := range all.Capabilities() {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "dns,tcp,gemini,gopher,finger,smtp" {
		t.Fatalf("all: %v", names)
	}
	if _, err := New(store, nil, Config{GeminiAddr: ":1965"}); err == nil {
		t.Fatal("gemini enabled without a certificate")
	}
}

// Hostile message text reaches every text wire as inert data: no terminal
// control sequences, no gemtext links outside the fence, no forged menu items.
func TestHostileTextIsInertOnEveryWire(t *testing.T) {
	store := openStore(t)
	seed(t, store, "evil\x1b[2J\x1b]0;owned\a\n```\n=> gemini://evil.example/ click\n\tfake\tsel\thost\t70\n.\n‮gnp.exe")
	_, addrs := started(t, store, nil)
	for _, cl := range textClients {
		out := streamExchange(t, addrs[cl.key], cl.frame(readRequests[cl.name]), cl.name == "gemini")
		if !strings.Contains(out, "evil") {
			t.Fatalf("%s did not read the message: %q", cl.name, out)
		}
		if strings.ContainsAny(out, "\x1b\a‮") {
			t.Errorf("%s passed control characters through", cl.name)
		}
		switch cl.name {
		case "gemini":
			// Read it the way a gemtext client does: only lines outside a
			// preformatted block are links, and every one must be ours.
			pre := false
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "```") {
					pre = !pre
					continue
				}
				if !pre && strings.HasPrefix(line, "=>") && !strings.HasPrefix(line, "=> /") {
					t.Errorf("gemini content escaped the fence: %q", line)
				}
			}
			if pre {
				t.Error("gemini left a preformatted block open")
			}
		case "gopher":
			for _, line := range strings.Split(out, "\r\n") {
				if strings.Contains(line, "fake") && (line[0] != 'i' || strings.Count(line, "\t") != 3) {
					t.Errorf("gopher content forged a menu item: %q", line)
				}
			}
		}
	}
}

func TestLineProtocolGrammar(t *testing.T) {
	p := lineProtocol{}
	for _, tc := range []struct {
		line, op string
		ok       bool
	}{
		{"READ lobby", "messages.list", true}, {"read lobby/main 50", "messages.list", true}, {"READ lobby 51", "", false},
		{"READ", "", false}, {"THREAD abc", "thread.get", true}, {"ROOMS", "rooms.list", true}, {"HELP", "", true}, {"", "", true},
		{"POST lobby hello there", "post", true}, {"POST lobby", "", false}, {"POST lobby    ", "", false},
		{"CMD !!!", "", false}, {"CMD e30", "", true}, {"DELETE everything", "", false}, {"READ lobby\xff", "", false},
	} {
		req, err := p.Parse([]byte(tc.line))
		if (err == nil) != tc.ok {
			t.Errorf("%q: err %v", tc.line, err)
			continue
		}
		if tc.op != "" && (req.Command == nil || req.Command.Operation != tc.op) {
			t.Errorf("%q: %+v", tc.line, req.Command)
		}
	}
	req, _ := p.Parse([]byte("POST lobby  two  spaces "))
	if req.Command.Text != " two  spaces " {
		t.Fatalf("post text altered: %q", req.Command.Text)
	}
}

func TestReadLineBounds(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		err      bool
	}{
		{"abc\n", "abc", false}, {"abc\r\n", "abc", false}, {"abc", "abc", false}, {"", "", true},
		{strings.Repeat("x", 16) + "\n", "", true}, {strings.Repeat("x", 15) + "\n", strings.Repeat("x", 15), false},
	} {
		got, err := readLine(bufio.NewReader(strings.NewReader(tc.in)), 15)
		if (err != nil) != tc.err || string(got) != tc.want {
			t.Errorf("%q: %q %v", tc.in, got, err)
		}
	}
}

func TestFitAndClean(t *testing.T) {
	if got := fit("line one\nline two\n", 10); got != "" {
		t.Fatalf("fit below the marker size: %q", got)
	}
	long := strings.Repeat("é line\n", 100)
	if got := fit(long, 120); len(got) > 120 || !strings.HasSuffix(got, truncated) || !utf8.ValidString(got) {
		t.Fatalf("fit: %d %q", len(got), got)
	}
	in := "a\x00b\x1bc\u009bd\te\nf\xff"
	if got := clean(in); got != "a�b�c�d\te\nf�" {
		t.Fatalf("clean: %q", got)
	}
}
