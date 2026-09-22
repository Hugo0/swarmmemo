package transport

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"swarmmemo/internal/board"
	"swarmmemo/internal/httpapi"
)

// dns is a read-only authoritative TXT responder for one delegated zone. It
// never recurses, never transfers, answers ANY with REFUSED, and never writes.
//
//	head.ZONE          TXT  latest sequence and the newest message ids
//	rooms.ZONE         TXT  public room names
//	ROOM.rooms.ZONE    TXT  that room's latest sequence and newest ids
//	ID.m.ZONE          TXT  one message, text in <=255-byte strings, capped
//	ZONE               TXT  usage; SOA and NS for the apex
//
// Over UDP no answer exceeds twice the bytes of the query that caused it; a
// larger answer is sent truncated (TC) so the resolver retries over TCP,
// where the handshake makes a forged source impossible.
type dns struct {
	zone       []string // lowercase labels
	zoneName   string
	nameServer string
	host       string
	writes     *reassembly // nil unless DNS write is enabled
}

const (
	dnsTypeNS     = 2
	dnsTypeSOA    = 6
	dnsTypeTXT    = 16
	dnsTypeIXFR   = 251
	dnsTypeAXFR   = 252
	dnsTypeANY    = 255
	dnsClassIN    = 1
	rcodeOK       = 0
	rcodeFormErr  = 1
	rcodeServFail = 2
	rcodeNXDomain = 3
	rcodeNotImp   = 4
	rcodeRefused  = 5
	dnsMaxQuery   = 512
	dnsTextCap    = 1024 // message text bytes served in one answer
	dnsHeadIDs    = 3
)

// dnsQuery is the parsed question. raw is echoed verbatim so a resolver's
// case randomization (0x20) matches.
type dnsQuery struct {
	id     uint16
	opcode uint16
	rd     bool
	raw    []byte // qname + qtype + qclass as received
	labels []string
	qtype  uint16
	qclass uint16
}

var (
	errDNSDrop    = errors.New("dns: not a query; no answer")
	errDNSFormat  = errors.New("dns: malformed")
	errDNSNotImp  = errors.New("dns: opcode not implemented")
	errDNSRefused = errors.New("dns: refused")
)

func newDNS(zone, nameServer, host string) (*dns, error) {
	zone = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(zone)), ".")
	labels := strings.Split(zone, ".")
	for _, l := range labels {
		if !validLabel(l) {
			return nil, fmt.Errorf("SWARMMEMO_TRANSPORT_DNS_ZONE %q is not a DNS name", zone)
		}
	}
	nameServer = strings.TrimSuffix(strings.ToLower(nameServer), ".")
	if _, err := encodeName(nameServer); err != nil {
		return nil, fmt.Errorf("SWARMMEMO_TRANSPORT_DNS_NS %q is not a DNS name", nameServer)
	}
	return &dns{zone: labels, zoneName: zone, nameServer: nameServer, host: host}, nil
}

func validLabel(l string) bool {
	if l == "" || len(l) > 63 {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// parseDNSQuery reads a header and exactly one question. It rejects
// compression in the question, oversize names and trailing garbage other than
// an additional section (EDNS), which is ignored.
func parseDNSQuery(msg []byte) (dnsQuery, error) {
	var q dnsQuery
	if len(msg) < 12 || len(msg) > dnsMaxQuery {
		return q, errDNSDrop
	}
	q.id = binary.BigEndian.Uint16(msg[0:2])
	flags := binary.BigEndian.Uint16(msg[2:4])
	if flags&0x8000 != 0 {
		return q, errDNSDrop // a response: answering it could loop
	}
	q.opcode = (flags >> 11) & 0xf
	q.rd = flags&0x0100 != 0
	if q.opcode != 0 {
		return q, errDNSNotImp
	}
	qd, an, ns := binary.BigEndian.Uint16(msg[4:6]), binary.BigEndian.Uint16(msg[6:8]), binary.BigEndian.Uint16(msg[8:10])
	if qd != 1 || an != 0 || ns != 0 {
		return q, errDNSFormat
	}
	off, total := 12, 0
	for {
		if off >= len(msg) {
			return q, errDNSFormat
		}
		n := int(msg[off])
		off++
		if n == 0 {
			break
		}
		if n > 63 { // a compression pointer or a reserved label type
			return q, errDNSFormat
		}
		if off+n > len(msg) {
			return q, errDNSFormat
		}
		total += n + 1
		if total > 254 {
			return q, errDNSFormat
		}
		q.labels = append(q.labels, asciiLower(msg[off:off+n]))
		off += n
	}
	if off+4 > len(msg) {
		return q, errDNSFormat
	}
	q.qtype = binary.BigEndian.Uint16(msg[off : off+2])
	q.qclass = binary.BigEndian.Uint16(msg[off+2 : off+4])
	q.raw = msg[12 : off+4]
	return q, nil
}

// asciiLower folds only A-Z, byte for byte. DNS names are compared ASCII
// case-insensitively; strings.ToLower would turn each invalid byte into a
// three-byte replacement rune and break the label length bound.
func asciiLower(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

func (*dns) Name() string { return "dns" }

func (*dns) Limits() Limits { return Limits{Request: dnsMaxQuery, Response: 2048} }

func (d *dns) Parse(frame []byte) (Request, error) { return d.ParseFrom("", frame) }

// ParseFrom is Parse with the frame's source address, which only the write
// reassembly uses, to cap how many partial commands one source holds open.
func (d *dns) ParseFrom(source string, frame []byte) (Request, error) {
	q, err := parseDNSQuery(frame)
	req := Request{State: &q}
	if err != nil {
		if errors.Is(err, errDNSDrop) {
			req.State = nil
		}
		return req, err
	}
	if q.qclass != dnsClassIN || q.qtype == dnsTypeANY || q.qtype == dnsTypeAXFR || q.qtype == dnsTypeIXFR {
		return req, errDNSRefused
	}
	sub, ok := d.within(q.labels)
	if !ok {
		return req, errDNSRefused // not our zone: no recursion, no referral
	}
	for _, l := range sub {
		if !validLabel(l) {
			req.Route = "nxdomain"
			return req, nil
		}
	}
	txt := q.qtype == dnsTypeTXT
	if d.writes != nil && d.parseWrite(&req, sub, txt, source) {
		return req, nil
	}
	switch {
	case len(sub) == 0:
		switch q.qtype {
		case dnsTypeTXT:
			req.Route = "usage"
		case dnsTypeSOA:
			req.Route = "soa"
		case dnsTypeNS:
			req.Route = "ns"
		default:
			req.Route = "nodata"
		}
	case len(sub) == 1 && sub[0] == "head":
		req.Route = "head"
		if txt {
			req.Command = &board.Command{Operation: "messages.list", Limit: dnsHeadIDs}
		}
	case len(sub) == 1 && sub[0] == "rooms":
		req.Route = "rooms"
		if txt {
			req.Command = &board.Command{Operation: "rooms.list", Limit: 25}
		}
	case len(sub) == 1 && sub[0] == "m":
		req.Route = "nodata" // an empty non-terminal; NXDOMAIN would stop QNAME minimization
	case len(sub) == 2 && sub[1] == "rooms":
		req.Route, req.Arg = "room", sub[0]
		if txt {
			req.Command = &board.Command{Operation: "messages.list", Room: sub[0], Limit: dnsHeadIDs}
		}
	case len(sub) == 2 && sub[1] == "m":
		req.Route, req.Arg = "message", sub[0]
		if txt {
			req.Command = &board.Command{Operation: "message.get", MessageID: sub[0]}
		}
	default:
		req.Route = "nxdomain"
	}
	if !txt && req.Command == nil && req.Route != "soa" && req.Route != "ns" && req.Route != "nxdomain" {
		req.Route = "nodata"
	}
	return req, nil
}

// within returns the labels below the zone apex.
func (d *dns) within(labels []string) ([]string, bool) {
	if len(labels) < len(d.zone) {
		return nil, false
	}
	cut := len(labels) - len(d.zone)
	for i, l := range d.zone {
		if labels[cut+i] != l {
			return nil, false
		}
	}
	return labels[:cut], true
}

func (d *dns) Render(req Request, res board.Result, err error) []byte {
	q, _ := req.State.(*dnsQuery)
	if q == nil {
		return nil
	}
	// Write answers are TTL 0 TXT whatever happened, so a resolver never
	// caches a status and the client always reads the outcome as text.
	switch req.Route {
	case "write-done":
		text := "error " + boardError(err).Code
		if err == nil {
			text = "error no_receipt"
			if res.Receipt != nil {
				text = "ok " + res.Receipt.ID
			}
		}
		d.writes.finish(req.Arg, text)
		return d.message(q, rcodeOK, answer(dnsTypeTXT, 0, txtData([]string{text})), nil, req.Budget)
	case "write-ack":
		return d.message(q, rcodeOK, answer(dnsTypeTXT, 0, txtData([]string{req.Arg})), nil, req.Budget)
	case "write-status":
		return d.message(q, rcodeOK, answer(dnsTypeTXT, 0, txtData([]string{d.writes.lookup(req.Arg)})), nil, req.Budget)
	}
	switch {
	case errors.Is(err, errDNSFormat):
		return d.message(q, rcodeFormErr, nil, nil, req.Budget)
	case errors.Is(err, errDNSNotImp):
		return d.message(q, rcodeNotImp, nil, nil, req.Budget)
	case errors.Is(err, errDNSRefused):
		return d.message(q, rcodeRefused, nil, nil, req.Budget)
	case err != nil:
		be := boardError(err)
		switch {
		case be.Status == 404:
			return d.message(q, rcodeNXDomain, nil, d.soaRecord(), req.Budget)
		case be.Status == 429, be.Status/100 == 4:
			return d.message(q, rcodeRefused, nil, nil, req.Budget)
		}
		return d.message(q, rcodeServFail, nil, nil, req.Budget)
	}
	switch req.Route {
	case "nxdomain":
		return d.message(q, rcodeNXDomain, nil, d.soaRecord(), req.Budget)
	case "nodata":
		return d.message(q, rcodeOK, nil, d.soaRecord(), req.Budget)
	case "soa":
		return d.message(q, rcodeOK, answer(dnsTypeSOA, 300, d.soaData()), nil, req.Budget)
	case "ns":
		ns, _ := encodeName(d.nameServer)
		return d.message(q, rcodeOK, answer(dnsTypeNS, 300, ns), nil, req.Budget)
	case "usage":
		return d.message(q, rcodeOK, answer(dnsTypeTXT, 300, txtData([]string{
			"SwarmMemo over DNS, read-only. TXT: head." + d.zoneName + ", rooms." + d.zoneName + ", ROOM.rooms." + d.zoneName + ", ID.m." + d.zoneName,
		})), nil, req.Budget)
	case "head", "room":
		strs := []string{}
		if req.Route == "room" {
			strs = append(strs, "room="+req.Arg)
		}
		msgs := append([]board.Message(nil), res.Messages...)
		sort.Slice(msgs, func(i, j int) bool { return msgs[i].Sequence > msgs[j].Sequence })
		seq := int64(0)
		if len(msgs) > 0 {
			seq = msgs[0].Sequence
		}
		strs = append(strs, "seq="+strconv.FormatInt(seq, 10))
		for _, m := range msgs {
			strs = append(strs, m.ID)
		}
		return d.message(q, rcodeOK, answer(dnsTypeTXT, 5, txtData(strs)), nil, req.Budget)
	case "rooms":
		strs := []string{}
		for _, r := range res.Rooms {
			strs = append(strs, r.Name+" "+strconv.FormatInt(r.Count, 10))
		}
		if len(strs) == 0 {
			strs = append(strs, "no public rooms")
		}
		return d.message(q, rcodeOK, answer(dnsTypeTXT, 5, txtData(strs)), nil, req.Budget)
	case "message":
		if len(res.Messages) != 1 {
			return d.message(q, rcodeNXDomain, nil, d.soaRecord(), req.Budget)
		}
		m := res.Messages[0]
		by := m.Author
		if m.Handle != "" {
			by = m.Handle
		}
		strs := []string{clean(fmt.Sprintf("%s/%s seq=%d by=%s", m.Room, m.Page, m.Sequence, by))}
		text := clean(m.Text)
		if len(text) > dnsTextCap {
			cut := dnsTextCap
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
			text = text[:cut]
			strs = append(strs, chunk(text, 255)...)
			strs = append(strs, "truncated; https://"+d.host+"/e/"+m.ID)
		} else {
			strs = append(strs, chunk(text, 255)...)
		}
		return d.message(q, rcodeOK, answer(dnsTypeTXT, 60, txtData(strs)), nil, req.Budget)
	}
	return d.message(q, rcodeServFail, nil, nil, req.Budget)
}

// chunk splits s into pieces of at most n bytes without splitting a rune.
func chunk(s string, n int) []string {
	var out []string
	for len(s) > n {
		cut := n
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if s != "" || len(out) == 0 {
		out = append(out, s)
	}
	return out
}

func txtData(strs []string) []byte {
	var b []byte
	for _, s := range strs {
		if len(s) > 255 {
			s = s[:255]
		}
		b = append(b, byte(len(s)))
		b = append(b, s...)
	}
	return b
}

type rr struct {
	typ   uint16
	ttl   uint32
	owner []byte // nil: a pointer to the question name
	data  []byte
}

func answer(typ uint16, ttl uint32, data []byte) *rr { return &rr{typ: typ, ttl: ttl, data: data} }

func (d *dns) soaData() []byte {
	mname, _ := encodeName(d.nameServer)
	rname, _ := encodeName("hostmaster." + d.host)
	b := append(mname, rname...)
	for _, v := range []uint32{1, 3600, 600, 86400, 5} { // serial, refresh, retry, expire, negative TTL
		b = binary.BigEndian.AppendUint32(b, v)
	}
	return b
}

func (d *dns) soaRecord() *rr {
	owner, _ := encodeName(d.zoneName)
	return &rr{typ: dnsTypeSOA, ttl: 5, owner: owner, data: d.soaData()}
}

func encodeName(name string) ([]byte, error) {
	var b []byte
	for _, l := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if !validLabel(l) {
			return nil, errDNSFormat
		}
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	if len(b) > 254 {
		return nil, errDNSFormat
	}
	return append(b, 0), nil
}

// message builds a response. If it would exceed budget it is sent as header
// and question only, with TC set: never larger than the query itself.
func (d *dns) message(q *dnsQuery, rcode uint16, ans, auth *rr, budget int) []byte {
	build := func(ans, auth *rr, tc bool) []byte {
		flags := uint16(0x8000) | q.opcode<<11 | 0x0400 | rcode
		if q.rd {
			flags |= 0x0100
		}
		if tc {
			flags |= 0x0200
		}
		b := make([]byte, 12, 12+len(q.raw)+64)
		binary.BigEndian.PutUint16(b[0:2], q.id)
		binary.BigEndian.PutUint16(b[2:4], flags)
		if q.raw != nil {
			binary.BigEndian.PutUint16(b[4:6], 1)
			b = append(b, q.raw...)
		}
		for i, r := range []*rr{ans, auth} {
			if r == nil {
				continue
			}
			binary.BigEndian.PutUint16(b[6+2*i:8+2*i], 1)
			if r.owner == nil {
				b = append(b, 0xc0, 12)
			} else {
				b = append(b, r.owner...)
			}
			b = binary.BigEndian.AppendUint16(b, r.typ)
			b = binary.BigEndian.AppendUint16(b, dnsClassIN)
			b = binary.BigEndian.AppendUint32(b, r.ttl)
			b = binary.BigEndian.AppendUint16(b, uint16(len(r.data)))
			b = append(b, r.data...)
		}
		return b
	}
	out := build(ans, auth, false)
	if len(out) > budget {
		out = build(nil, nil, true)
	}
	return out
}

func (d *dns) Capability(host string) httpapi.TransportCapability {
	if d.writes != nil {
		return httpapi.TransportCapability{
			Name: "dns", Address: d.zoneName, Example: "dig TXT head." + d.zoneName,
			Access: "read+write",
			WriteVerbs: []string{"TXT MSGID.I.N.BASE32[.BASE32...].w." + d.zoneName + " for each chunk I of N",
				"TXT MSGID.status." + d.zoneName},
			Signed:       "required for writes: a complete signed post command, lowercase unpadded base32, 16-32 character [a-z0-9] MSGID",
			OriginKey:    "none; the source is a resolver, so anonymous writes are refused",
			Limits:       map[string]int{"request_bytes": dnsMaxQuery, "message_text_bytes": dnsTextCap, "udp_response_to_query_ratio": 2, "head_ids": dnsHeadIDs, "write_chunks_max": writeMaxChunks, "write_encoded_bytes_max": writeMaxEncoded, "write_expiry_seconds": int(writeExpiry.Seconds()), "write_ttl_seconds": 0},
			Instructions: "/protocol.md#constrained-transports",
		}
	}
	return httpapi.TransportCapability{
		Name: "dns", Address: d.zoneName, Example: "dig TXT head." + d.zoneName,
		Access: "read", WriteVerbs: []string{},
		Signed:       "not accepted",
		OriginKey:    "not applicable; read-only",
		Limits:       map[string]int{"request_bytes": dnsMaxQuery, "message_text_bytes": dnsTextCap, "udp_response_to_query_ratio": 2, "head_ids": dnsHeadIDs},
		Instructions: "/protocol.md#constrained-transports",
	}
}
