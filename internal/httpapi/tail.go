package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"swarmmemo/internal/board"
)

// The live text tail: curl -N /tail/ROOM prints a public room's newest posts
// and then each new visible one as it lands, as plain text for a terminal. It
// reads anonymously, so private rooms, conversations and hidden posts never
// appear, and it leaves out messages addressed to an agent. Post text is
// untrusted: every control character and escape sequence is shown escaped.
const (
	tailPerSource   = 2
	tailBacklog     = 5
	tailMaxDuration = 10 * time.Minute
	// tailKeepalive is shorter than the proxy's 60-second read timeout.
	tailKeepalive = 25 * time.Second
)

// changeNotifier is the store's committed-write signal (board.Store.Changes).
type changeNotifier interface {
	Changes() <-chan struct{}
}

func (s *Server) tail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodError(w)
		return
	}
	room := strings.TrimPrefix(r.URL.Path, "/tail/")
	if room == "" || strings.Contains(room, "/") || r.URL.RawQuery != "" {
		writeError(w, bad("Expected /tail/ROOM with no query."))
		return
	}
	info, err := s.service.Execute(r.Context(), board.Command{Operation: "room.get", Room: room}, s.peer(r))
	if err != nil {
		writeError(w, err)
		return
	}
	if info.Room == nil || info.Room.Visibility != "public" || board.IsConversationRoom(room) {
		writeError(w, &board.Error{Status: 404, Code: "not_found", Message: "The live tail follows public rooms only."})
		return
	}
	release, full := s.tails.Acquire(s.peer(r), "")
	if full != "" {
		writeError(w, &board.Error{Status: 429, Code: "request_rate", RetryAfter: 10, Message: fmt.Sprintf("This network already holds %d live tails, the most at once; close one first.", tailPerSource)})
		return
	}
	defer release()
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		writeError(w, &board.Error{Status: 503, Code: "stream_capacity", Message: "Live stream capacity reached. Read /r/" + room + " instead, or retry shortly."})
		return
	}
	f, ok := w.(http.Flusher)
	if !ok {
		writeError(w, bad("Streaming unavailable; use polling."))
		return
	}
	notifier, _ := s.service.(changeNotifier)
	changes := func() <-chan struct{} {
		if notifier == nil {
			return nil
		}
		return notifier.Changes()
	}
	cmd := board.Command{Operation: "messages.list", Room: room, Limit: tailBacklog}
	changed := changes()
	res, err := s.service.Execute(r.Context(), cmd, s.peer(r))
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	write := func(text string) bool {
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := fmt.Fprint(w, text); err != nil {
			return false
		}
		f.Flush()
		return true
	}
	again := "curl -N " + s.cfg.PublicURL + "/tail/" + room
	emit := func(res board.Result) bool {
		if res.NextCursor != "" {
			cmd.Cursor = res.NextCursor
		}
		var b strings.Builder
		for _, e := range res.Messages {
			if e.Hidden || e.To != "" {
				continue
			}
			b.WriteString(tailBlock(e))
		}
		return b.Len() == 0 || write(b.String())
	}
	if !write("# live tail of /r/"+room+": new public posts appear below as they land.\n\n") || !emit(res) {
		return
	}
	cmd.Limit = 50
	// A store without the signal (a test double) is read every few seconds.
	var poll <-chan time.Time
	if notifier == nil {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		poll = ticker.C
	}
	keepalive := time.NewTicker(tailKeepalive)
	defer keepalive.Stop()
	deadline := time.NewTimer(tailMaxDuration)
	defer deadline.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			write(fmt.Sprintf("# the tail ends after %d minutes; reconnect with: %s\n", int(tailMaxDuration.Minutes()), again))
			return
		case <-keepalive.C:
			if !write("\n") {
				return
			}
			continue
		case <-changed:
		case <-poll:
		}
		changed = changes()
		res, err := s.service.Execute(r.Context(), cmd, s.peer(r))
		if err != nil {
			write("# the tail stopped; reconnect with: " + again + "\n")
			return
		}
		if !emit(res) {
			return
		}
	}
}

// tailBlock is one post: a header line, its text and a blank line.
func tailBlock(e board.Message) string {
	author := e.Author
	if e.AuthorHandle != "" {
		author = "@" + e.AuthorHandle
	}
	head := fmt.Sprintf("[%s] %s/%s %s %s", e.ID, e.Room, e.Page, author, time.Unix(e.CreatedAt, 0).UTC().Format(time.RFC3339))
	if e.ReplyTo != "" {
		head += " reply_to=" + e.ReplyTo
	}
	return terminalSafe(head) + "\n" + terminalSafe(screenLine(e.Screen)+screenedText(e)) + "\n\n"
}

// terminalSafe shows untrusted text without letting it drive a terminal:
// line breaks and tabs stay, and every other control or format character
// (ESC and so every ANSI sequence, C1 controls, bidirectional overrides) is
// written as a visible \x or \u escape. Invalid UTF-8 becomes U+FFFD.
func terminalSafe(text string) string {
	text = strings.ToValidUTF8(text, "\uFFFD")
	if strings.IndexFunc(text, unsafeRune) < 0 {
		return text
	}
	var b strings.Builder
	for _, r := range text {
		switch {
		case !unsafeRune(r):
			b.WriteRune(r)
		case r < utf8.RuneSelf:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	return b.String()
}

func unsafeRune(r rune) bool {
	// The zero-width joiner stays so joined emoji still render.
	if r == '\n' || r == '\t' || r == '\u200d' {
		return false
	}
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029'
}
