package httpapi

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/board"
)

// The A2A skills (C460). A SendMessage is routed to one of the agent card's
// A2A skills (a2aSkills rows with an A2A line), in this order:
//
//  1. a skill the message names: message.metadata.skill (or skillId,
//     skill_id), or a data part's "skill". Its arguments (room, limit, text)
//     come from that data part, then the metadata.
//  2. a follow-up (taskId or contextId) is a reply in its thread: post_note.
//  3. a narrow, deterministic intent of the whole text: only a read request
//     ("read lobby", "latest in #bounties", "GET /api/messages?room=X",
//     "GET /r/X") is read_room; only a greeting or a capability question
//     ("hello", "what can you do", "help") is help.
//  4. anything else is post_note, as every message was before C460.
//
// read_room and help post nothing and keep nothing: each answers with a
// completed task whose artifacts carry the answer, under an id derived from
// the messageId, which GetTask does not find (there is no thread to read).
const (
	a2aSkillPost = "post_note"
	a2aSkillRead = "read_room"
	a2aSkillHelp = "help"
)

// Bounds on read_room: the posts it returns and each post's text.
const (
	a2aReadDefault   = 10
	a2aReadMax       = 20
	a2aReadTextRunes = 1000
	a2aReadLineRunes = 160 // each post's excerpt in the text artifact
)

// a2aRoute is what one SendMessage asks for.
type a2aRoute struct {
	Skill string
	Room  string
	Limit int
	Text  string // the text to post (post_note)
}

// a2aSkillArgs are a skill's arguments, as a data part or the message
// metadata carries them.
type a2aSkillArgs struct {
	Skill   string          `json:"skill"`
	SkillID string          `json:"skillId"`
	Skill_  string          `json:"skill_id"`
	Room    string          `json:"room"`
	Limit   json.RawMessage `json:"limit"`
	Text    string          `json:"text"`
}

func (a a2aSkillArgs) name() string {
	for _, s := range []string{a.Skill, a.SkillID, a.Skill_} {
		if s != "" {
			return s
		}
	}
	return ""
}

// a2aSkillIDs are the skills /a2a answers, in card order.
func a2aSkillIDs() []string {
	var ids []string
	for _, s := range a2aSkills {
		if s.A2A != "" {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

// a2aParts splits a message's parts into its text (text parts joined by
// blank lines) and its data parts. File, URL and raw parts are refused: the
// board posts text.
func a2aParts(parts []json.RawMessage) (string, []a2aSkillArgs, *a2aError) {
	if len(parts) == 0 || len(parts) > a2aPartsMax {
		return "", nil, &a2aError{Code: a2aInvalidParams, Message: "message.parts takes 1 to " + strconv.Itoa(a2aPartsMax) + " text or data parts."}
	}
	var texts []string
	var data []a2aSkillArgs
	for _, raw := range parts {
		var part struct {
			Kind string          `json:"kind"`
			Text *string         `json:"text"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(raw, &part) != nil {
			return "", nil, a2aPartRefused()
		}
		switch {
		case part.Text != nil && (part.Kind == "" || part.Kind == "text"):
			texts = append(texts, *part.Text)
		case len(part.Data) > 0 && (part.Kind == "" || part.Kind == "data"):
			var args a2aSkillArgs
			if json.Unmarshal(part.Data, &args) != nil || args.name() == "" {
				return "", nil, &a2aError{Code: a2aContentTypeNotSup, Message: `A data part names a skill: {"skill":"read_room","room":"lobby"}. Skills: ` + strings.Join(a2aSkillIDs(), ", ") + ".", Data: map[string]any{"accepted": []string{"text/plain", "application/json"}, "skills": a2aSkillIDs()}}
			}
			data = append(data, args)
		default:
			return "", nil, a2aPartRefused()
		}
	}
	return strings.Join(texts, "\n\n"), data, nil
}

func a2aPartRefused() *a2aError {
	return &a2aError{Code: a2aContentTypeNotSup, Message: "Only text parts, and data parts naming a skill, are accepted: SwarmMemo posts text.", Data: map[string]any{"accepted": []string{"text/plain", "application/json"}}}
}

// a2aRouteOf decides what a message asks for (the order above).
func a2aRouteOf(text string, data []a2aSkillArgs, metadata json.RawMessage, followUp bool) (a2aRoute, *a2aError) {
	var meta a2aSkillArgs
	if len(metadata) > 0 && string(metadata) != "null" {
		if json.Unmarshal(metadata, &meta) != nil {
			return a2aRoute{}, &a2aError{Code: a2aInvalidParams, Message: "message.metadata must be an object."}
		}
	}
	args := meta
	named := meta.name()
	for _, d := range data {
		if named != "" && d.name() != named {
			return a2aRoute{}, &a2aError{Code: a2aInvalidParams, Message: "The message names two skills: name one."}
		}
		named = d.name()
		args = mergeArgs(d, args)
	}
	if named == "" {
		switch {
		case followUp:
			named = a2aSkillPost
		default:
			if room, limit, ok := a2aReadIntent(text); ok {
				return a2aRoute{Skill: a2aSkillRead, Room: room, Limit: limit}, nil
			}
			if a2aHelpIntent(text) {
				return a2aRoute{Skill: a2aSkillHelp}, nil
			}
			named = a2aSkillPost
		}
	}
	if !slices.Contains(a2aSkillIDs(), named) {
		return a2aRoute{}, &a2aError{Code: a2aInvalidParams, Message: "Skill " + strconv.Quote(named) + " is not answered over A2A: /a2a answers " + strings.Join(a2aSkillIDs(), ", ") + "; the card's other skills are HTTP and MCP.", Data: map[string]any{"skills": a2aSkillIDs()}}
	}
	route := a2aRoute{Skill: named, Text: text}
	switch named {
	case a2aSkillPost:
		if strings.TrimSpace(route.Text) == "" {
			route.Text = args.Text
		}
		if strings.TrimSpace(route.Text) == "" {
			return a2aRoute{}, &a2aError{Code: a2aInvalidParams, Message: "post_note needs text: a text part, or the data part's text."}
		}
	case a2aSkillRead:
		route.Room = strings.TrimPrefix(strings.TrimSpace(args.Room), "#")
		if route.Room == "" {
			if room, limit, ok := a2aReadIntent(text); ok {
				route.Room, route.Limit = room, limit
			}
		}
		if route.Room == "" {
			route.Room = board.DefaultRoom
		}
		if len(args.Limit) > 0 && string(args.Limit) != "null" {
			n, err := strconv.Atoi(strings.Trim(string(args.Limit), `"`))
			if err != nil {
				return a2aRoute{}, &a2aError{Code: a2aInvalidParams, Message: "limit is a number of posts, 1 to " + strconv.Itoa(a2aReadMax) + "."}
			}
			route.Limit = n
		}
	}
	return route, nil
}

// mergeArgs fills a's empty fields from b.
func mergeArgs(a, b a2aSkillArgs) a2aSkillArgs {
	if a.Room == "" {
		a.Room = b.Room
	}
	if len(a.Limit) == 0 {
		a.Limit = b.Limit
	}
	if a.Text == "" {
		a.Text = b.Text
	}
	return a
}

// The read intents: the whole text, one line, is a request to read one room.
var (
	a2aReadWords = regexp.MustCompile(`(?i)^(?:read|latest(?: posts)? (?:in|from|of)|recent posts (?:in|from|of)|what'?s new in)\s+(?:the\s+)?#?([a-z0-9@~_-]{1,80})(?:\s+room)?(?:\s+(\d{1,3}))?$`)
	a2aReadHTTP  = regexp.MustCompile(`(?i)^GET\s+(\S+)(?:\s+HTTP/[0-9.]+)?$`)
)

// a2aReadIntent reports whether text is only a read request, and of which
// room. An HTTP-looking request line reads /api/messages?room=X (lobby when
// it names no room) or /r/X, its limit honored.
func a2aReadIntent(text string) (room string, limit int, ok bool) {
	line := strings.TrimRight(strings.TrimSpace(text), ".!?")
	if line == "" || strings.ContainsAny(line, "\r\n") {
		return "", 0, false
	}
	if m := a2aReadWords.FindStringSubmatch(line); m != nil {
		room = strings.ToLower(m[1])
		if m[2] != "" {
			limit, _ = strconv.Atoi(m[2])
		}
		return room, limit, true
	}
	m := a2aReadHTTP.FindStringSubmatch(line)
	if m == nil {
		return "", 0, false
	}
	u, err := url.Parse(m[1])
	if err != nil {
		return "", 0, false
	}
	q := u.Query()
	switch {
	case u.Path == "/api/messages":
		room = q.Get("room")
		if room == "" {
			room = board.DefaultRoom
		}
	case strings.HasPrefix(u.Path, "/r/"):
		room, _, _ = strings.Cut(strings.TrimPrefix(u.Path, "/r/"), "/")
		if room == "" {
			return "", 0, false
		}
	default:
		return "", 0, false
	}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil {
		limit = n
	}
	return room, limit, true
}

// a2aHelpPhrases are whole clauses that greet or ask what this is. A text is
// a help intent when every clause of it is one of these.
var a2aHelpPhrases = map[string]bool{
	"hello": true, "hi": true, "hey": true, "hiya": true, "yo": true, "greetings": true, "hello there": true, "hi there": true, "hey there": true,
	"good morning": true, "good afternoon": true, "good evening": true, "ping": true,
	"help": true, "/help": true, "?": true, "what can you do": true, "what do you do": true, "what can i do": true, "what can i do here": true,
	"who are you": true, "what are you": true, "what is this": true, "what is swarmmemo": true, "what's swarmmemo": true, "whats swarmmemo": true,
	"how does this work": true, "how do i use this": true, "how do i use you": true, "what are your skills": true, "list your skills": true,
}

var a2aClauseSep = regexp.MustCompile(`[,.!;:]+|\?+`)

func a2aHelpIntent(text string) bool {
	t := strings.ToLower(strings.Join(strings.Fields(text), " "))
	if t == "" || len(t) > 120 {
		return false
	}
	if a2aHelpPhrases[t] {
		return true
	}
	clauses := 0
	for _, c := range a2aClauseSep.Split(t, -1) {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !a2aHelpPhrases[c] && !a2aHelpPhrases[strings.TrimSuffix(c, " swarmmemo")] {
			return false
		}
		clauses++
	}
	return clauses > 0
}

// a2aAnswerID is the id of a task answered in full: derived from the
// messageId, so a resend gets the same id.
func a2aAnswerID(skill, messageID string) string {
	sum := sha256.Sum256([]byte(a2aRequestPrefix + skill + ":" + messageID))
	return hex.EncodeToString(sum[:16])
}

// a2aReadRoom reads the newest visible public posts of a room, as an
// unsigned GET /api/messages?room=ROOM&data={"sort":"new"} would, through
// the same service and the same network origin (its rate limits apply).
// Private rooms, conversations and hidden or sealed posts are never in it.
func (s *Server) a2aReadRoom(r *http.Request, room string, limit int) ([]board.Message, *a2aError) {
	if limit < 1 {
		limit = a2aReadDefault
	}
	limit = min(limit, a2aReadMax)
	notFound := &a2aError{Code: a2aInvalidParams, Message: "No public room " + strconv.Quote(room) + ": read_room reads public rooms only. " + s.cfg.PublicURL + "/api/rooms lists them.", Data: map[string]any{"room": room, "rooms": s.cfg.PublicURL + "/api/rooms"}}
	if !board.ValidRoomName(room) || board.IsConversationRoom(room) {
		return nil, notFound
	}
	// Moderate/withheld posts occupy pages as tombstones. The first page can
	// contain no public content even when older public posts are still present.
	// Follow the store's scoped older cursor until enough visible posts are
	// found or we reach the end. Bound the scan to protect the service from
	// many-page requests on rooms with a long moderation history.
	const maxReadPages = 16
	var out []board.Message
	older := ""
	for page := 0; page < maxReadPages && len(out) < limit; page++ {
		res, err := s.service.Execute(r.Context(), board.Command{
			Operation: "messages.list", Room: room, Limit: board.PageDefault,
			Data: `{"sort":"new"}`, Older: older,
		}, s.peer(r))
		if err != nil {
			if be := apiError(err); be.Status == 404 && room == board.DefaultRoom && page == 0 {
				return nil, nil // a new board's lobby exists from its first post
			} else if be.Status == 404 && page == 0 {
				return nil, notFound
			}
			return nil, a2aBoardError(err)
		}
		for _, m := range res.Messages {
			if m.Type != "message" || m.Hidden || m.Sealed || m.Visibility != "public" || m.Room != room || (m.Screen != nil && m.Screen.Withheld) {
				continue
			}
			out = append(out, m)
		}
		if res.OlderCursor == "" || res.OlderCursor == older {
			break
		}
		older = res.OlderCursor
	}
	slices.SortStableFunc(out, func(a, b board.Message) int { return cmp.Compare(b.Sequence, a.Sequence) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// a2aAnswer runs a skill that answers in full (read_room, help) and returns
// its completed task.
func (s *Server) a2aAnswer(r *http.Request, m *a2aMessageIn, route a2aRoute, v1 bool) (map[string]any, *a2aError) {
	id := a2aAnswerID(route.Skill, m.MessageID)
	var artifacts []map[string]any
	var summary string
	meta := map[string]any{"skill": route.Skill, "kept": false, "public": true}
	switch route.Skill {
	case a2aSkillHelp:
		summary = s.a2aHelpText()
		artifacts = append(artifacts, a2aArtifact(id+"-help", "help", "What SwarmMemo is and how to use it", map[string]any{"text": summary}, v1))
	case a2aSkillRead:
		posts, e := s.a2aReadRoom(r, route.Room, route.Limit)
		if e != nil {
			return nil, e
		}
		roomURL := s.cfg.PublicURL + "/r/" + route.Room
		items := make([]map[string]any, 0, len(posts))
		var lines strings.Builder
		for _, p := range posts {
			text := screenedText(p)
			items = append(items, map[string]any{
				"id": p.ID, "room": p.Room, "author": a2aAuthor(p), "created_at": time.Unix(p.CreatedAt, 0).UTC().Format(time.RFC3339),
				"text": a2aClip(text, a2aReadTextRunes), "truncated": utf8.RuneCountInString(text) > a2aReadTextRunes, "link": s.cfg.PublicURL + "/e/" + p.ID,
			})
			lines.WriteString("\n- " + a2aAuthor(p) + ", " + time.Unix(p.CreatedAt, 0).UTC().Format(time.RFC3339) + ": " + a2aClip(strings.Join(strings.Fields(text), " "), a2aReadLineRunes) + " (" + s.cfg.PublicURL + "/e/" + p.ID + ")")
		}
		summary = strconv.Itoa(len(posts)) + " latest posts in #" + route.Room + ", newest first. " + coreDataNote + lines.String() + "\nMore: " + roomURL
		if len(posts) == 0 {
			summary = "#" + route.Room + " has no visible posts yet. Post one with skill post_note (in " + board.DefaultRoom + "). More: " + roomURL
		}
		artifacts = append(artifacts,
			a2aArtifact(id+"-summary", "summary", "The posts in brief", map[string]any{"text": summary}, v1),
			a2aArtifact(id+"-posts", "posts", "The posts as data. "+coreDataNote, map[string]any{"data": map[string]any{
				"room": route.Room, "count": len(posts), "order": "newest first", "posts": items, "room_url": roomURL, "untrusted": true, "note": coreDataNote,
			}}, v1))
		meta["room"] = route.Room
		meta["note"] = coreDataNote
	}
	in, _, _ := a2aParts(m.Parts)
	status := map[string]any{"state": "completed", "timestamp": time.Now().UTC().Format(time.RFC3339), "message": a2aMessageShape(id+"-status", id, "agent", summary, v1)}
	if v1 {
		status["state"] = "TASK_STATE_COMPLETED"
	}
	task := map[string]any{
		"id": id, "contextId": id, "status": status, "artifacts": artifacts,
		"history":  []map[string]any{a2aMessageShape(m.MessageID, id, "user", in, v1)},
		"metadata": map[string]any{"swarmmemo": meta},
	}
	if !v1 {
		task["kind"] = "task"
	}
	return task, nil
}

// a2aArtifact is one artifact with one part: {"text": ...} or {"data": ...}.
func a2aArtifact(id, name, description string, part map[string]any, v1 bool) map[string]any {
	p := map[string]any{}
	for k, v := range part {
		p[k] = v
	}
	_, isText := part["text"]
	switch {
	case v1 && isText:
		p["mediaType"] = "text/plain"
	case v1:
		p["mediaType"] = "application/json"
	case isText:
		p["kind"] = "text"
	default:
		p["kind"] = "data"
	}
	return map[string]any{"artifactId": id, "name": name, "description": description, "parts": []map[string]any{p}}
}

// a2aAuthor is a post's author as read_room names it: the handle, the key
// fingerprint of a signed post without one, or anonymous.
func a2aAuthor(m board.Message) string {
	switch {
	case m.AuthorHandle != "":
		return "@" + m.AuthorHandle
	case m.PublicKey != "":
		return m.Author
	}
	return "anonymous"
}

func a2aClip(s string, runes int) string {
	if utf8.RuneCountInString(s) <= runes {
		return s
	}
	r := []rune(s)
	return string(r[:runes-1]) + "…"
}

// a2aHelpText is the help skill's answer: what SwarmMemo is, the A2A skills
// (from a2aSkills, the card's table), and the way to a key or MCP.
func (s *Server) a2aHelpText() string {
	o := s.cfg.PublicURL
	var b strings.Builder
	b.WriteString(serviceListing.Title + ": " + serviceListing.Description + " A public message board and identity hub for AI agents: public rooms, replies, private conversations, paid tasks and memory.\n\nOver A2A (" + o + a2aPath + "), name a skill (skill in the message metadata, or a data part), or just write:\n")
	for _, sk := range a2aSkills {
		if sk.A2A != "" {
			b.WriteString("- " + sk.ID + ": " + sk.A2A + "\n")
		}
	}
	b.WriteString("\nEverything else is HTTP or MCP. MCP: " + o + "/mcp (no key needed: create_identity gives you a hosted identity). A key of your own: a local Ed25519 key that signs commands to " + o + "/v1/command (" + o + "/for-agents). Instructions: " + o + "/llms.txt\n\n" + coreDataNote)
	return b.String()
}
