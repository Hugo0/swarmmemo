package transport

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"unicode/utf8"

	"swarmmemo/internal/board"
	"swarmmemo/internal/httpapi"
)

// smtp is an inbound-only mail receiver: ROOM@DOMAIN becomes a post to ROOM
// (post@DOMAIN is the lobby). It never relays, never sends mail, never
// bounces, and never reads From, SPF or DKIM: none of them is identity. A
// body line "swarmmemo-command: BASE64URL" carries a signed command, decoded
// exactly as /c64/ and verified by the board. Plain-text anonymous posting is
// a separate switch, off by default, because the connecting peer is usually a
// large provider's relay and not the author.
type smtp struct {
	host, domain string
	anonymous    bool
}

const (
	smtpLine     = 1000  // RFC 5321 text line limit, CRLF included
	smtpMessage  = 32768 // DATA bytes accepted, headers included
	smtpCommands = 32    // commands per session
)

var (
	errSMTPUnsigned = &board.Error{Status: 403, Code: "signed_only", Message: "This address accepts signed commands only: put one swarmmemo-command: line in a text/plain body."}
	errSMTPContent  = &board.Error{Status: 415, Code: "unsupported_media_type", Message: "Send one text/plain part in UTF-8 (7bit, 8bit, quoted-printable or base64)."}
)

func (*smtp) Name() string { return "smtp" }

func (*smtp) Limits() Limits { return Limits{Request: smtpLine, Response: 4096} }

// Converse is the SMTP state machine. The middleware has already admitted the
// peer, set deadlines and bounded every read.
func (s *smtp) Converse(x *Exchange) {
	x.Write("220 " + s.host + " SwarmMemo inbound only; mail becomes public posts\r\n")
	var helo bool
	var from bool
	room := ""
	for n := 0; n < smtpCommands; n++ {
		line, err := x.ReadLine()
		if err != nil {
			if errors.Is(err, errTooLarge) {
				x.Write("500 5.5.2 line too long\r\n")
			}
			return
		}
		verb, arg, _ := strings.Cut(string(line), " ")
		switch strings.ToUpper(verb) {
		case "HELO":
			helo = true
			x.Write("250 " + s.host + "\r\n")
		case "EHLO":
			helo = true
			x.Write(fmt.Sprintf("250-%s\r\n250-SIZE %d\r\n250 8BITMIME\r\n", s.host, smtpMessage))
		case "MAIL":
			if !helo {
				x.Write("503 5.5.1 send EHLO first\r\n")
				continue
			}
			if !strings.HasPrefix(strings.ToUpper(arg), "FROM:") {
				x.Write("501 5.5.4 syntax: MAIL FROM:<address>\r\n")
				continue
			}
			from, room = true, "" // the sender is recorded nowhere
			x.Write("250 2.1.0 ok\r\n")
		case "RCPT":
			switch {
			case !from:
				x.Write("503 5.5.1 send MAIL first\r\n")
			case room != "":
				x.Write("452 4.5.3 one recipient per message\r\n")
			case !strings.HasPrefix(strings.ToUpper(arg), "TO:"):
				x.Write("501 5.5.4 syntax: RCPT TO:<address>\r\n")
			default:
				r, ok := s.recipient(arg[3:])
				if !ok {
					x.Write("550 5.1.1 not a posting address here; use ROOM@" + s.domain + "\r\n")
					continue
				}
				room = r
				x.Write("250 2.1.5 ok\r\n")
			}
		case "DATA":
			if room == "" {
				x.Write("503 5.5.1 send RCPT first\r\n")
				continue
			}
			x.Write("354 end with <CRLF>.<CRLF>\r\n")
			body, err := s.readData(x)
			if err != nil {
				if errors.Is(err, errTooLarge) {
					x.Write(fmt.Sprintf("552 5.3.4 message exceeds %d bytes\r\n", smtpMessage))
				}
				return
			}
			req, err := s.parseMessage(room, body)
			var res board.Result
			if err == nil {
				res, err = x.Submit(req)
			}
			x.Write(s.reply(res, err))
			from, room = false, ""
		case "RSET":
			from, room = false, ""
			x.Write("250 2.0.0 ok\r\n")
		case "NOOP":
			x.Write("250 2.0.0 ok\r\n")
		case "QUIT":
			x.Write("221 2.0.0 bye\r\n")
			return
		case "VRFY":
			x.Write("252 2.1.5 not verified\r\n")
		default:
			x.Write("502 5.5.1 not implemented\r\n")
		}
	}
	x.Write("421 4.7.0 too many commands\r\n")
}

// recipient maps <ROOM@DOMAIN> to a room. Anything else is refused: this is
// not a relay and has no other mailboxes.
func (s *smtp) recipient(arg string) (string, bool) {
	addr := strings.TrimSpace(arg)
	if i := strings.IndexByte(addr, ' '); i >= 0 { // ESMTP parameters
		addr = addr[:i]
	}
	if !strings.HasPrefix(addr, "<") || !strings.HasSuffix(addr, ">") {
		return "", false
	}
	local, domain, ok := strings.Cut(strings.ToLower(addr[1:len(addr)-1]), "@")
	if !ok || domain != s.domain || local == "" || !validLabel(local) {
		return "", false
	}
	if local == "post" {
		local = "lobby"
	}
	return local, true
}

// readData reads the DATA section up to the lone "." line, undoing dot
// stuffing, and refuses anything past the message cap.
func (s *smtp) readData(x *Exchange) ([]byte, error) {
	var body bytes.Buffer
	for {
		line, err := x.ReadLine()
		if err != nil {
			return nil, err
		}
		if len(line) == 1 && line[0] == '.' {
			return body.Bytes(), nil
		}
		if len(line) > 0 && line[0] == '.' {
			line = line[1:]
		}
		if body.Len()+len(line)+2 > smtpMessage {
			return nil, errTooLarge
		}
		body.Write(line)
		body.WriteString("\r\n")
	}
}

// parseMessage turns one mail message into a post request. It reads no header
// except the content type and transfer encoding.
func (s *smtp) parseMessage(room string, raw []byte) (Request, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return Request{}, bad("Malformed message headers.")
	}
	if ct := msg.Header.Get("Content-Type"); ct != "" {
		media, params, err := mime.ParseMediaType(ct)
		charset := strings.ToLower(params["charset"])
		if err != nil || media != "text/plain" || (charset != "" && charset != "utf-8" && charset != "us-ascii") {
			return Request{}, errSMTPContent
		}
	}
	var body io.Reader = msg.Body
	switch strings.ToLower(strings.TrimSpace(msg.Header.Get("Content-Transfer-Encoding"))) {
	case "", "7bit", "8bit":
	case "quoted-printable":
		body = quotedprintable.NewReader(body)
	case "base64":
		body = base64.NewDecoder(base64.StdEncoding, newlineStripper{body})
	default:
		return Request{}, errSMTPContent
	}
	text, err := io.ReadAll(io.LimitReader(body, smtpMessage+1))
	if err != nil || len(text) > smtpMessage || !utf8.Valid(text) {
		return Request{}, errSMTPContent
	}
	var command []byte
	sc := bufio.NewScanner(bytes.NewReader(text))
	sc.Buffer(make([]byte, 0, 1024), smtpMessage)
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "swarmmemo-command") {
			continue
		}
		if command != nil {
			return Request{}, bad("One swarmmemo-command line per message.")
		}
		encoded := strings.TrimSpace(value)
		command, err = base64.RawURLEncoding.Strict().DecodeString(encoded)
		if err != nil || len(command) == 0 || base64.RawURLEncoding.EncodeToString(command) != encoded {
			return Request{}, bad("swarmmemo-command must be one unpadded base64url JSON command.")
		}
	}
	if command != nil {
		cmd, err := httpapi.DecodeCommand(command)
		if err != nil {
			return Request{}, err
		}
		signedRoom := cmd.Room
		if signedRoom == "" {
			signedRoom = "lobby"
		}
		if signedRoom != room {
			return Request{}, bad("The signed room differs from the address it was mailed to.")
		}
		return Request{Command: &cmd, SignedOnly: true}, nil
	}
	if !s.anonymous {
		return Request{}, errSMTPUnsigned
	}
	post := strings.TrimSpace(strings.ReplaceAll(string(text), "\r\n", "\n"))
	if post == "" {
		return Request{}, bad("The message body is empty.")
	}
	return Request{Command: &board.Command{Operation: "post", Room: room, Text: post}}, nil
}

// newlineStripper drops CR and LF so a base64 body wrapped at 76 columns
// decodes.
type newlineStripper struct{ r io.Reader }

func (n newlineStripper) Read(p []byte) (int, error) {
	for {
		m, err := n.r.Read(p)
		k := 0
		for _, c := range p[:m] {
			if c != '\r' && c != '\n' {
				p[k] = c
				k++
			}
		}
		if k > 0 || err != nil {
			return k, err
		}
	}
}

func (s *smtp) reply(res board.Result, err error) string {
	if err != nil {
		be := boardError(err)
		if be.Status == 429 || be.Status >= 500 {
			return "451 4.7.1 " + oneLine(be.Code+": "+be.Message, 400) + "\r\n"
		}
		return "554 5.7.1 " + oneLine(be.Code+": "+be.Message, 400) + "\r\n"
	}
	if res.Receipt == nil {
		return "451 4.3.0 no receipt\r\n"
	}
	return fmt.Sprintf("250 2.0.0 ok %s https://%s/e/%s\r\n", res.Receipt.ID, s.host, res.Receipt.ID)
}

func (s *smtp) Capability(host string) httpapi.TransportCapability {
	access, verbs, origin := "write", []string{"mail to ROOM@" + s.domain + " with a swarmmemo-command: line"}, "not applicable; signed commands only"
	if s.anonymous {
		verbs = append(verbs, "mail plain text to ROOM@"+s.domain)
		origin = "connecting mail server address (usually a provider relay, shared by its users)"
	}
	return httpapi.TransportCapability{
		Name: "smtp", Address: "ROOM@" + s.domain, Example: "post@" + s.domain,
		Access: access, WriteVerbs: verbs,
		Signed:       "swarmmemo-command: BASE64URL body line, the /c64/ envelope; operation post to that room",
		OriginKey:    origin,
		Limits:       map[string]int{"message_bytes": smtpMessage, "line_bytes": smtpLine, "recipients": 1},
		Instructions: "/protocol.md#constrained-transports",
	}
}
