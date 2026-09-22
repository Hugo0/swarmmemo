package transport

import (
	"encoding/binary"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

func testDNS(t *testing.T) *dns {
	t.Helper()
	d, err := newDNS("q.swarmmemo.com", "swarmmemo.com", "swarmmemo.com")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func rcode(msg []byte) int { return int(binary.BigEndian.Uint16(msg[2:4]) & 0xf) }

func flag(msg []byte, bit uint16) bool { return binary.BigEndian.Uint16(msg[2:4])&bit != 0 }

// answerOnce parses and renders a query against a fixed result, as the
// middleware would with the given budget.
func answerOnce(t *testing.T, d *dns, query []byte, res board.Result, runErr error, budget int) []byte {
	t.Helper()
	req, err := d.Parse(query)
	req.Budget = budget
	if err == nil {
		err = runErr
	} else {
		res = board.Result{}
	}
	return d.Render(req, res, err)
}

func TestDNSRefusesWhatAnAuthoritativeTXTServerMustNot(t *testing.T) {
	d := testDNS(t)
	for name, tc := range map[string]struct {
		query []byte
		rcode int
	}{
		"ANY":                {dnsQueryBytes("head.q.swarmmemo.com", dnsTypeANY), rcodeRefused},
		"AXFR":               {dnsQueryBytes("q.swarmmemo.com", dnsTypeAXFR), rcodeRefused},
		"IXFR":               {dnsQueryBytes("q.swarmmemo.com", dnsTypeIXFR), rcodeRefused},
		"other zone":         {dnsQueryBytes("example.com", dnsTypeTXT), rcodeRefused},
		"parent zone":        {dnsQueryBytes("swarmmemo.com", dnsTypeTXT), rcodeRefused},
		"unknown name":       {dnsQueryBytes("nope.q.swarmmemo.com", dnsTypeTXT), rcodeNXDomain},
		"bad label":          {dnsQueryBytes("h*d.q.swarmmemo.com", dnsTypeTXT), rcodeNXDomain},
		"empty non-terminal": {dnsQueryBytes("m.q.swarmmemo.com", dnsTypeA()), rcodeOK},
		"A on head":          {dnsQueryBytes("head.q.swarmmemo.com", dnsTypeA()), rcodeOK},
	} {
		out := answerOnce(t, d, tc.query, board.Result{}, nil, 512)
		if out == nil || rcode(out) != tc.rcode || !flag(out, 0x8000) || !flag(out, 0x0400) {
			t.Errorf("%s: %x", name, out)
			continue
		}
		if binary.BigEndian.Uint16(out[6:8]) != 0 {
			t.Errorf("%s: answered with records", name)
		}
	}
	// A CH-class query (version.bind and friends) is refused, not answered.
	q := dnsQueryBytes("q.swarmmemo.com", dnsTypeTXT)
	binary.BigEndian.PutUint16(q[len(q)-2:], 3)
	if out := answerOnce(t, d, q, board.Result{}, nil, 512); rcode(out) != rcodeRefused {
		t.Fatal("CH class answered")
	}
}

func dnsTypeA() uint16 { return 1 }

func TestDNSNeverAnswersResponsesOrGarbage(t *testing.T) {
	d := testDNS(t)
	resp := dnsQueryBytes("head.q.swarmmemo.com", dnsTypeTXT)
	resp[2] |= 0x80
	if out := answerOnce(t, d, resp, board.Result{}, nil, 512); out != nil {
		t.Fatal("answered a response packet; two servers could loop")
	}
	if out := answerOnce(t, d, []byte{1, 2, 3}, board.Result{}, nil, 512); out != nil {
		t.Fatal("answered a runt")
	}
	// A compression pointer inside the question is malformed for a query.
	ptr := []byte{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xc0, 12, 0, 16, 0, 1}
	if out := answerOnce(t, d, ptr, board.Result{}, nil, 512); rcode(out) != rcodeFormErr {
		t.Fatalf("pointer loop: %x", out)
	}
	notify := dnsQueryBytes("q.swarmmemo.com", dnsTypeSOA)
	notify[2] |= 4 << 3 // opcode NOTIFY
	if out := answerOnce(t, d, notify, board.Result{}, nil, 512); rcode(out) != rcodeNotImp {
		t.Fatal("opcode other than QUERY was served")
	}
}

func TestDNSEchoesTheQuestionExactly(t *testing.T) {
	d := testDNS(t)
	q := dnsQueryBytes("HeAd.Q.SwArMmEmO.cOm", dnsTypeTXT)
	res := board.Result{Messages: []board.Message{{ID: strings.Repeat("a", 32), Sequence: 9}}}
	out := answerOnce(t, d, q, res, nil, 4096)
	if string(out[12:len(q)]) != string(q[12:]) || binary.BigEndian.Uint16(out[0:2]) != 0x1234 {
		t.Fatal("0x20 case or id not echoed; resolvers would discard the answer")
	}
	if s := txtStrings(t, out); len(s) != 2 || s[0] != "seq=9" {
		t.Fatalf("head: %q", s)
	}
}

// Over UDP the budget is twice the query. An answer that does not fit goes
// out as header and question with TC set, never larger than the query.
func TestDNSTruncatesRatherThanAmplifies(t *testing.T) {
	d := testDNS(t)
	id := strings.Repeat("b", 32)
	q := dnsQueryBytes(id+".m.q.swarmmemo.com", dnsTypeTXT)
	res := board.Result{Messages: []board.Message{{ID: id, Room: "lobby", Page: "main", Author: "anonymous", Text: strings.Repeat("x", 5000)}}}
	udp := answerOnce(t, d, q, res, nil, 2*len(q))
	if len(udp) > len(q) || !flag(udp, 0x0200) || binary.BigEndian.Uint16(udp[6:8]) != 0 {
		t.Fatalf("udp amplified: %d bytes for a %d byte query", len(udp), len(q))
	}
	tcp := answerOnce(t, d, q, res, nil, d.Limits().Response)
	s := txtStrings(t, tcp)
	text := strings.Join(s[1:len(s)-1], "")
	if flag(tcp, 0x0200) || len(text) != dnsTextCap || !strings.HasPrefix(s[len(s)-1], "truncated; https://swarmmemo.com/e/") {
		t.Fatalf("tcp message: %d strings, %d text bytes", len(s), len(text))
	}
	for _, str := range s {
		if len(str) > 255 {
			t.Fatal("TXT string over 255 bytes")
		}
	}
	// Small answers fit in the UDP budget.
	soa := dnsQueryBytes("q.swarmmemo.com", dnsTypeNS)
	if out := answerOnce(t, d, soa, board.Result{}, nil, 2*len(soa)); flag(out, 0x0200) || binary.BigEndian.Uint16(out[6:8]) != 1 {
		t.Fatalf("NS did not fit: %x", out)
	}
}

func TestDNSRoutesBuildOnlyPublicReads(t *testing.T) {
	d := testDNS(t)
	for name, op := range map[string]string{
		"head.q.swarmmemo.com":                         "messages.list",
		"rooms.q.swarmmemo.com":                        "rooms.list",
		"lobby.rooms.q.swarmmemo.com":                  "messages.list",
		strings.Repeat("c", 32) + ".m.q.swarmmemo.com": "message.get",
	} {
		req, err := d.Parse(dnsQueryBytes(name, dnsTypeTXT))
		if err != nil || req.Command == nil || req.Command.Operation != op || permitted(*req.Command) != nil {
			t.Errorf("%s: %+v %v", name, req.Command, err)
		}
	}
	if _, err := newDNS("bad zone!", "swarmmemo.com", "swarmmemo.com"); err == nil {
		t.Fatal("accepted an invalid zone")
	}
}

func TestDNSBoardErrorsMapToRcodes(t *testing.T) {
	d := testDNS(t)
	q := dnsQueryBytes("nothere.rooms.q.swarmmemo.com", dnsTypeTXT)
	if out := answerOnce(t, d, q, board.Result{}, &board.Error{Status: 404, Code: "not_found"}, 512); rcode(out) != rcodeNXDomain {
		t.Fatal("missing room is not NXDOMAIN")
	}
	if out := answerOnce(t, d, q, board.Result{}, errInternal, 512); rcode(out) != rcodeServFail {
		t.Fatal("internal error is not SERVFAIL")
	}
}
