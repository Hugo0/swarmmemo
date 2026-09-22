package transport

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/board"
)

// FuzzDNSQuery drives hostile datagrams through the whole DNS adapter. The
// invariants are the security properties: no panic, a question echoed only
// from inside the packet, labels within DNS bounds, and no answer that could
// amplify (never larger than the budget, which UDP sets to twice the query).
func FuzzDNSQuery(f *testing.F) {
	for _, seed := range [][]byte{
		dnsQueryBytes("head.q.swarmmemo.com", dnsTypeTXT),
		dnsQueryBytes("lobby.rooms.q.swarmmemo.com", dnsTypeTXT),
		dnsQueryBytes(strings.Repeat("a", 32)+".m.q.swarmmemo.com", dnsTypeTXT),
		dnsQueryBytes("q.swarmmemo.com", dnsTypeSOA),
		dnsQueryBytes("q.swarmmemo.com", dnsTypeANY),
		{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xc0, 12, 0, 16, 0, 1},
		{},
	} {
		f.Add(seed)
	}
	d, err := newDNS("q.swarmmemo.com", "swarmmemo.com", "swarmmemo.com")
	if err != nil {
		f.Fatal(err)
	}
	d.writes = newReassembly() // the write names are parsed from the same hostile packets
	f.Add(dnsQueryBytes("abcdefghij0123456789.0.2.mfzwi.w.q.swarmmemo.com", dnsTypeTXT))
	f.Add(dnsQueryBytes("abcdefghij0123456789.1.2.mfzwi.w.q.swarmmemo.com", dnsTypeTXT))
	f.Add(dnsQueryBytes("abcdefghij0123456789.status.q.swarmmemo.com", dnsTypeTXT))
	long := board.Result{
		Messages: []board.Message{{ID: strings.Repeat("a", 32), Sequence: 1, Room: "lobby", Page: "main", Text: strings.Repeat("\x1bé", 3000)}},
		Rooms:    []board.Room{{Name: "lobby", Count: 3}},
	}
	f.Fuzz(func(t *testing.T, packet []byte) {
		q, err := parseDNSQuery(packet)
		if err == nil {
			if len(q.raw) > len(packet) || !bytes.Contains(packet, q.raw) {
				t.Fatal("question not taken from the packet")
			}
			total := 0
			for _, l := range q.labels {
				if len(l) == 0 || len(l) > 63 {
					t.Fatalf("label bound: %q", l)
				}
				total += len(l) + 1
			}
			if total > 254 {
				t.Fatal("name bound")
			}
		}
		for _, runErr := range []error{nil, errInternal, &board.Error{Status: 404}} {
			req, perr := d.Parse(packet)
			req.Budget = 2 * len(packet)
			res := long
			if perr != nil {
				res, runErr = board.Result{}, perr
			} else if req.Command != nil && permitted(*req.Command) != nil {
				t.Fatalf("dns built a command the policy refuses: %+v", req.Command)
			}
			out := d.Render(req, res, runErr)
			if len(out) > 2*len(packet) {
				t.Fatalf("amplified: %d bytes for %d", len(out), len(packet))
			}
			if len(out) > 0 && (len(out) < 12 || out[2]&0x80 == 0) {
				t.Fatal("answer is not a response")
			}
		}
	})
}

// FuzzLineProtocol drives hostile lines through framing and the TCP grammar:
// no panic, the frame never exceeds its cap, and every command it builds is
// one the shared policy accepts or refuses, never an unknown shape.
func FuzzLineProtocol(f *testing.F) {
	for _, seed := range []string{"READ lobby 5\n", "POST lobby hi\r\n", "CMD e30\n", "THREAD x\n", "\n", "READ\x00\n", "CMD " + strings.Repeat("A", 9000)} {
		f.Add([]byte(seed))
	}
	p := lineProtocol{}
	max := p.Limits().Request
	f.Fuzz(func(t *testing.T, in []byte) {
		frame, err := readLine(bufio.NewReader(bytes.NewReader(in)), max)
		if err != nil {
			return
		}
		if len(frame) > max || bytes.IndexByte(frame, '\n') >= 0 {
			t.Fatal("frame bound")
		}
		req, err := p.Parse(frame)
		out := p.Render(Request{Route: req.Route, Budget: p.Limits().Response}, board.Result{Messages: []board.Message{{Text: string(frame)}}}, err)
		if len(out) > p.Limits().Response || !utf8.Valid(out) {
			t.Fatal("render bound")
		}
		if err == nil && req.Command != nil {
			switch req.Command.Operation {
			case "messages.list", "thread.get", "rooms.list", "post":
				if permitted(*req.Command) != nil {
					t.Fatalf("grammar built a refused command: %+v", req.Command)
				}
			default:
				_ = permitted(*req.Command) // CMD: any decoded shape; the policy decides
			}
		}
	})
}

// FuzzTextWires renders hostile message text on Gemini, Gopher and finger and
// checks that nothing escapes its container.
func FuzzTextWires(f *testing.F) {
	f.Add("```\n=> gemini://evil/ x\n\tfake\tsel\th\t70\n.\n\x1b[2J")
	f.Add(strings.Repeat("é\n", 500))
	g, gh, fg := gemini{host: "h", port: "1965"}, gopher{host: "h", port: "70"}, finger{host: "h"}
	f.Fuzz(func(t *testing.T, text string) {
		res := board.Result{Messages: []board.Message{{ID: "i", Room: "r", Page: "p", Text: text}}}
		out := string(g.Render(Request{Route: "room", Arg: "r", Budget: 65536}, res, nil))
		pre := false
		for _, line := range strings.Split(out, "\n")[1:] {
			if strings.HasPrefix(line, "```") {
				pre = !pre
				continue
			}
			if !pre && strings.HasPrefix(line, "=>") && !strings.HasPrefix(line, "=> /") {
				t.Fatalf("gemini link injected: %q", line)
			}
		}
		if pre {
			t.Fatal("gemini fence left open")
		}
		menu := string(gh.Render(Request{Route: "room", Budget: 65536}, res, nil))
		if !strings.HasSuffix(menu, ".\r\n") {
			t.Fatal("gopher menu unterminated")
		}
		for _, line := range strings.Split(strings.TrimSuffix(menu, ".\r\n"), "\r\n") {
			if line == "" {
				continue
			}
			if strings.Count(line, "\t") != 3 || strings.ContainsAny(line, "\r\n\x1b") {
				t.Fatalf("gopher item forged: %q", line)
			}
		}
		fout := fg.Render(Request{Budget: fg.Limits().Response}, res, nil)
		if len(fout) > fg.Limits().Response || bytes.ContainsAny(fout, "\x1b\x07") {
			t.Fatal("finger bound or control")
		}
	})
}

// FuzzReassembly feeds hostile chunk sequences to the DNS write buffer. The
// invariants are its memory bounds and its bookkeeping.
func FuzzReassembly(f *testing.F) {
	f.Add([]byte{0, 0, 2, 'a', 'b', 1, 1, 2, 'c'})
	f.Add([]byte{3, 5, 64, 'z', 'z', 3, 5, 63, 'y'})
	f.Fuzz(func(t *testing.T, ops []byte) {
		r := newReassembly()
		now := time.Unix(0, 0)
		r.now = func() time.Time { return now }
		for len(ops) >= 4 {
			id := "id" + strings.Repeat("x", 14) + string(rune('a'+ops[0]%26))
			i, n := int(ops[1])%(writeMaxChunks+1), int(ops[2])%(writeMaxChunks+1)
			size := int(ops[3])
			ops = ops[4:]
			if n < 1 || i >= n {
				continue
			}
			chunk := strings.Repeat(string(rune('a'+size%26)), size+1)
			raw, ans := r.add(id, i, n, chunk)
			if raw == nil && ans == "" {
				t.Fatal("no answer and no command")
			}
			if len(raw) > writeMaxEncoded {
				t.Fatal("command bound")
			}
			held := 0
			for _, p := range r.partials {
				held += p.size
			}
			if held != r.bytes || r.bytes > writeMaxBuffered || len(r.partials) > writeMaxPartials || len(r.statuses) > writeMaxStatuses {
				t.Fatalf("bounds: held %d bytes %d partials %d", held, r.bytes, len(r.partials))
			}
			if size%7 == 0 {
				now = now.Add(writeExpiry / 3)
			}
		}
	})
}

// FuzzSMTP drives a whole hostile SMTP session through Converse and a hostile
// message through parseMessage: no panic, termination, and only UTF-8 text
// posts to the addressed room.
func FuzzSMTP(f *testing.F) {
	f.Add([]byte("EHLO x\r\nMAIL FROM:<a@b>\r\nRCPT TO:<post@post.h>\r\nDATA\r\nSubject: s\r\n\r\nhi\r\n..dot\r\n.\r\nQUIT\r\n"))
	f.Add([]byte("DATA\r\nRCPT TO:<>\r\nMAIL FROM:\r\nHELO\r\n"))
	f.Add([]byte("Content-Transfer-Encoding: base64\r\n\r\n////\r\n"))
	s := &smtp{host: "h", domain: "post.h", anonymous: true}
	f.Fuzz(func(t *testing.T, in []byte) {
		if req, err := s.parseMessage("lobby", in); err == nil && req.Command != nil {
			if req.Command.Operation == "post" && !req.SignedOnly && (req.Command.Room != "lobby" || !utf8.ValidString(req.Command.Text) || len(req.Command.Text) > smtpMessage) {
				t.Fatalf("bad post: %+v", req.Command)
			}
		}
		server, client := net.Pipe()
		go func() {
			_, _ = client.Write(in)
			_ = client.Close()
		}()
		go func() { _, _ = io.Copy(io.Discard, client) }()
		x := &Exchange{conn: server, r: bufio.NewReader(server), max: smtpLine, budget: dialogueBytes, stats: &counters{},
			submit: func(req Request) (board.Result, error) {
				if req.Command == nil {
					t.Error("submitted nothing")
				}
				return board.Result{Receipt: &board.Receipt{ID: "r"}}, nil
			}}
		done := make(chan struct{})
		go func() { s.Converse(x); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("session did not terminate")
		}
		_ = server.Close()
	})
}

func TestTCPLengthFrameNeverAllocatesPastTheCap(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(binary.BigEndian.AppendUint16(nil, 513))
	if _, err := readFrame(bufio.NewReader(&buf), LengthPrefixed, 512); err != errTooLarge {
		t.Fatalf("oversize length accepted: %v", err)
	}
}
