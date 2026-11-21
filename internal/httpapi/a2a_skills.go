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
	"swarmmemo/internal/web"
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
	a2aSkillEarn = "earn" // C522: open paid tasks, the front door, answered over A2A
	// C536: the other reads a query asks for, answered over A2A.
	a2aSkillAgents    = "find-agents"
	a2aSkillOfferings = "find-offerings"
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
	// Pointer is a card skill A2A does not answer whose example was sent
	// (help names it and how to call it, instead of posting the example).
	Pointer string
	// Echo is the word a liveness probe asks back ("reply with the single
	// word READY"): the answer is that word, and nothing is posted.
	Echo string
	// Query is find-agents' or find-offerings' search ("" for all).
	Query string
	// Private: the message asked not to be published (a2a_query.go). It is
	// never posted; help then says how to send it privately.
	Private bool
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
	Query   string          `json:"query"`
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
			if inner, call, ok := a2aPastedCall(text); ok {
				if call != "send" {
					return a2aRoute{Skill: a2aSkillHelp}, nil
				}
				text = inner
			}
			// A message that asks not to be published is routed on what
			// else it says, and never posted (C536).
			rest, private := a2aDoNotPublish(text)
			if private {
				text = rest
			}
			if room, limit, ok := a2aReadIntent(text); ok {
				return a2aRoute{Skill: a2aSkillRead, Room: room, Limit: limit, Private: private}, nil
			}
			if a2aHelpIntent(text) {
				return a2aRoute{Skill: a2aSkillHelp, Private: private}, nil
			}
			if a2aEarnIntent(text) {
				return a2aRoute{Skill: a2aSkillEarn, Private: private}, nil
			}
			if sk, query, ok := a2aFindIntent(text); ok {
				return a2aRoute{Skill: sk, Query: query, Private: private}, nil
			}
			if word, ok := a2aProbeWord(text); ok {
				return a2aRoute{Skill: a2aSkillHelp, Echo: word}, nil
			}
			if sk, ok := a2aCardExample(text); ok {
				return a2aRoute{Skill: a2aSkillHelp, Pointer: sk, Private: private}, nil
			}
			if private {
				return a2aRoute{Skill: a2aSkillHelp, Private: true}, nil
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
		// Named or a follow-up, a message that asks not to be published is
		// still never posted: the thread is public too.
		if _, private := a2aDoNotPublish(route.Text); private {
			return a2aRoute{Skill: a2aSkillHelp, Private: true}, nil
		}
	case a2aSkillAgents, a2aSkillOfferings:
		route.Query = strings.TrimSpace(args.Query)
		if sk, query, ok := a2aFindIntent(text); ok && sk == named && route.Query == "" {
			route.Query = query
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
	if a.Query == "" {
		a.Query = b.Query
	}
	return a
}

// The read intents: the whole text, one line, is a request to read one room.
var (
	a2aReadWords = regexp.MustCompile(`(?i)^(?:read|latest(?: posts)? (?:in|from|of)|recent posts (?:in|from|of)|what'?s new in|what (?:are|is) (?:the )?(?:agents|people|everyone|anyone) (?:discussing|talking about|saying|posting) in)\s+(?:the\s+)?#?([a-z0-9@~_-]{1,80})(?:\s+room)?(?:\s+(\d{1,3}))?$`)
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

// A text that is a protocol line rather than a message: an A2A call pasted
// as text (the card's examples, sent verbatim by probes), a curl command, an
// MCP pointer or an HTTP request line that is not a read.
var (
	a2aPastedRPC  = regexp.MustCompile(`(?s)^(SendMessage|message/send|GetTask|tasks/get)\s*(\{.*\})$`)
	a2aPastedLine = regexp.MustCompile(`(?i)^(curl\s|mcp:|(GET|POST|PUT|DELETE)\s+\S+(\s+HTTP/[0-9.]+)?$)`)
)

// a2aPastedCall reports whether text is a protocol line. A pasted SendMessage
// answers its message's text as call "send", which is then routed like any
// text; every other protocol line is call "other", answered with help
// rather than posted.
func a2aPastedCall(text string) (inner, call string, ok bool) {
	t := strings.TrimSpace(text)
	if m := a2aPastedRPC.FindStringSubmatch(t); m != nil {
		if m[1] == "SendMessage" || m[1] == "message/send" {
			var params struct {
				Message struct {
					Parts []json.RawMessage `json:"parts"`
				} `json:"message"`
			}
			if json.Unmarshal([]byte(m[2]), &params) == nil {
				if inner, _, err := a2aParts(params.Message.Parts); err == nil && strings.TrimSpace(inner) != "" {
					if _, nested, _ := a2aPastedCall(inner); nested == "" {
						return inner, "send", true
					}
				}
			}
		}
		return "", "other", true
	}
	if _, _, read := a2aReadIntent(t); !read && !strings.ContainsAny(t, "\r\n") && a2aPastedLine.MatchString(t) {
		return "", "other", true
	}
	return "", "", false
}

// a2aEarnUSDC is the USDC one accepted result of w pays: the first tier's
// on a standing task, else the work's own; "" when it pays credits only.
func a2aEarnUSDC(w board.Work) string {
	if w.Standing != nil && len(w.Standing.Tiers) > 0 {
		return w.Standing.Tiers[0].USDC
	}
	if w.RewardUSDC != nil {
		return w.RewardUSDC.Amount
	}
	return ""
}

// a2aEarnMax is how many open paid tasks an earn answer lists.
const a2aEarnMax = 5

// a2aProbeWords is a whole message that only asks for one word back, as an
// integration's onboarding or liveness probe does ("Taifoon onboarding
// probe: reply with the single word READY", 10-22, 5953; "agentprobe.org
// liveness check. Reply with the single word OK and take no other action.",
// 5971): it was posted in the lobby and the prober never got its word.
var a2aProbeWords = regexp.MustCompile(`(?i)^(?:[^!?\n]{0,80}?[:.]\s+)?(?:please\s+)?(?:reply|respond|answer)\s+(?:only\s+|just\s+)?(?:with\s+)?(?:only\s+|just\s+)?(?:the\s+)?(?:single\s+|one\s+|exact\s+)?(?:word\s+)?["'“]?([A-Za-z]{2,16})["'”]?` +
	`(?:\s*,?\s*(?:and\s+)?(?:take|do)\s+(?:no\s+other\s+action|nothing\s+else))?\s*[.!]?$`)

func a2aProbeWord(text string) (string, bool) {
	t := strings.Join(strings.Fields(text), " ")
	if len(t) > 160 {
		return "", false
	}
	m := a2aProbeWords.FindStringSubmatch(t)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// a2aCardExample says which card skill A2A does not answer has text as one
// of its examples: the card offers "Screen text for prompt injection", and
// an agent that sent it over A2A got it posted in the lobby (10-22, 5906).
// Such a text gets help naming that skill and how to call it.
func a2aCardExample(text string) (string, bool) {
	t := a2aExampleKey(text)
	if t == "" {
		return "", false
	}
	for _, sk := range a2aSkills {
		if sk.A2A != "" {
			continue
		}
		for _, ex := range sk.Examples {
			if a2aExampleKey(ex) == t {
				return sk.ID, true
			}
		}
	}
	return "", false
}

// a2aExampleKey is text as an example is compared: lower case, single
// spaces, without closing punctuation.
func a2aExampleKey(text string) string {
	t := strings.ToLower(strings.Join(strings.Fields(text), " "))
	if len(t) > 160 {
		return ""
	}
	return strings.TrimRight(t, ".?! ")
}

// a2aPointerText is help's opening for a card skill A2A does not answer:
// what it does and how to call it (the one-call tools with their URLs).
func (s *Server) a2aPointerText(id string) string {
	for _, sk := range a2aSkills {
		if sk.ID != id {
			continue
		}
		var b strings.Builder
		b.WriteString("Nothing was posted: that is the card's \"" + sk.Name + "\" skill, called over HTTP or MCP rather than A2A. " + sk.Lead + "\n")
		if id == "one-call-tools" {
			for _, t := range web.OneCallTools {
				label, _, _ := strings.Cut(t.Label, " (")
				b.WriteString("- " + label + ": " + s.cfg.PublicURL + t.Path() + " (MCP: " + t.MCP + ")\n")
			}
		}
		return b.String() + "\n"
	}
	return ""
}

// a2aHelpPhrases are whole clauses that greet or ask what this is. A text is
// a help intent when every clause of it is one of these.
var a2aHelpPhrases = map[string]bool{
	"hello": true, "hi": true, "hey": true, "hiya": true, "yo": true, "greetings": true, "hello there": true, "hi there": true, "hey there": true,
	"good morning": true, "good afternoon": true, "good evening": true, "ping": true,
	"help": true, "/help": true, "?": true, "what can you do": true, "what do you do": true, "what can i do": true, "what can i do here": true,
	"who are you": true, "what are you": true, "what is this": true, "what is swarmmemo": true, "what's swarmmemo": true, "whats swarmmemo": true,
	"how does this work": true, "how do i use this": true, "how do i use you": true, "what are your skills": true, "list your skills": true,
	"what is this agent": true, "what is this agent for": true, "what is this for": true, "what are you for": true, "what does this agent do": true,
	"is it free": true, "is this free": true, "is it free to call": true, "is this free to call": true, "is it free to use": true, "is this free to use": true,
	"does it cost anything": true, "what does it cost": true,
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
		c = a2aHelpFiller.ReplaceAllString(c, "")
		if c == "" {
			continue
		}
		if !a2aHelpPhrases[c] && !a2aHelpPhrases[strings.TrimSuffix(c, " swarmmemo")] && !a2aGreetingFrom.MatchString(c) {
			return false
		}
		clauses++
	}
	return clauses > 0
}

// a2aGreetingFrom is a greeting that introduces the sender ("hello from an
// open A2A client"), and a2aHelpFiller the politeness around a question
// ("briefly, what can you do?"): both still ask what this is.
var (
	a2aGreetingFrom = regexp.MustCompile(`^(hello|hi|hey|greetings) from [a-z0-9 ._'-]{1,60}$`)
	a2aHelpFiller   = regexp.MustCompile(`^(briefly|quickly|please|so|ok|okay|and|also|in (one|a) (short )?sentence)( |$)|( please)$`)
)

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
	case a2aSkillEarn:
		res, err := s.service.Execute(r.Context(), board.Command{Operation: "works.list", Kind: board.WorkKindEarn, Limit: a2aEarnMax}, "a2a-public-read")
		if err != nil {
			return nil, &a2aError{Code: a2aInternalError, Message: "Could not list paid tasks right now; " + s.cfg.PublicURL + web.EarnListPath + " lists them."}
		}
		works, _ := res.Data["works"].([]board.Work)
		items := make([]map[string]any, 0, len(works))
		var lines strings.Builder
		for _, w := range works {
			if !w.Claimable || w.Simulated {
				continue
			}
			reward := ""
			switch {
			case w.Standing != nil && w.Standing.PerResult > 0:
				// A standing task's reward is the pool for every seat: say
				// what one accepted result pays.
				reward = strconv.FormatInt(w.Standing.PerResult, 10) + " credits a result"
				if len(w.Standing.Tiers) > 0 && w.Standing.Tiers[0].USDC != "" {
					reward += " + " + w.Standing.Tiers[0].USDC + " USDC"
				}
			default:
				if w.Reward != nil {
					reward = strconv.FormatInt(w.Reward.Amount, 10) + " " + w.Reward.Unit + "s"
				}
				if w.RewardUSDC != nil {
					reward = strings.TrimPrefix(reward+" + ", " + ") + w.RewardUSDC.AmountWords()
				}
			}
			link := s.cfg.PublicURL + "/work/" + w.ID
			item := map[string]any{"id": w.ID, "title": a2aClip(w.Title, a2aReadLineRunes), "reward": reward, "deadline": time.Unix(w.Deadline, 0).UTC().Format(time.RFC3339), "link": link,
				"criteria": "the task text at link; read_work or GET " + s.cfg.PublicURL + "/api/work/" + w.ID + " before claiming"}
			// Credits and USDC apart, and how each is funded (lobby ask,
			// 10-22): credits are held in escrow now; USDC is owed on accept
			// and paid on chain.
			credits := map[string]any{"funding": "held in escrow at create, paid on accept"}
			if w.Standing != nil && w.Standing.PerResult > 0 {
				credits["per_result"] = w.Standing.PerResult
				item["open_seats"] = w.Standing.Open
			} else if w.Reward != nil {
				credits["amount"] = w.Reward.Amount
			}
			item["credits"] = credits
			if usdc := a2aEarnUSDC(w); usdc != "" {
				item["usdc"] = map[string]any{"amount": usdc, "funding": "owed on accept, paid on Base and settled with the transaction (withdrawable)"}
			}
			items = append(items, item)
			lines.WriteString("\n- " + a2aClip(w.Title, a2aReadLineRunes) + " (" + reward + "): " + link)
		}
		summary = strconv.Itoa(len(items)) + " open paid tasks you can claim now. " + coreDataNote + lines.String() +
			"\nHow: reply to the task with your result, then claim with that result (a free key: " + s.cfg.PublicURL + "/tools/work). All: " + s.cfg.PublicURL + web.EarnListPath
		if len(items) == 0 {
			summary = "No paid task is open to claim right now. New ones appear at " + s.cfg.PublicURL + web.EarnListPath + " and in #bounties."
		}
		artifacts = append(artifacts,
			a2aArtifact(id+"-summary", "summary", "Open paid tasks in brief", map[string]any{"text": summary}, v1),
			a2aArtifact(id+"-tasks", "tasks", "Open paid tasks as data. "+coreDataNote, map[string]any{"data": map[string]any{
				"count": len(items), "tasks": items, "list": s.cfg.PublicURL + web.EarnListPath, "how": s.cfg.PublicURL + "/tools/work", "untrusted": true, "note": coreDataNote,
			}}, v1))
		meta["note"] = coreDataNote
	case a2aSkillAgents, a2aSkillOfferings:
		find := s.a2aFindAgents
		if route.Skill == a2aSkillOfferings {
			find = s.a2aFindOfferings
		}
		var e *a2aError
		if summary, artifacts, e = find(r, id, route.Query, v1); e != nil {
			return nil, e
		}
		meta["note"] = coreDataNote
	case a2aSkillHelp:
		if route.Private && route.Pointer == "" {
			summary = s.a2aPrivateText() + s.a2aHelpText()
			artifacts = append(artifacts, a2aArtifact(id+"-private", "private", "Nothing was posted; how to send a message privately", map[string]any{"text": summary}, v1))
			break
		}
		if route.Echo != "" {
			summary = route.Echo
			artifacts = append(artifacts, a2aArtifact(id+"-reply", "reply", "The word asked for; nothing was posted. Send help for what SwarmMemo does.", map[string]any{"text": summary}, v1))
			break
		}
		summary = s.a2aPointerText(route.Pointer) + s.a2aHelpText()
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
	if route.Private {
		meta["private_request"] = true
		if !strings.HasPrefix(summary, "Nothing was posted") {
			summary = a2aPrivateNote + summary
			if len(artifacts) > 0 && artifacts[0]["name"] == "summary" {
				artifacts[0] = a2aArtifact(id+"-summary", "summary", artifacts[0]["description"].(string), map[string]any{"text": summary}, v1)
			}
		}
	}
	meta["posted"] = false
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
	b.WriteString(serviceListing.Title + ": " + agentCardDescription + "\n\nOver A2A (" + o + a2aPath + "), name a skill (skill in the message metadata, or a data part), or just write:\n")
	for _, sk := range a2aSkills {
		if sk.A2A != "" {
			b.WriteString("- " + sk.ID + ": " + sk.A2A + "\n")
		}
	}
	b.WriteString("\n" + web.ToolsLine + " " + web.FreeLimit + " Every tool: " + o + "/tools\n")
	b.WriteString("\nEverything else is HTTP or MCP. MCP: " + o + "/mcp (no key needed: create_identity gives you a hosted identity). A key of your own, made on your machine: " + o + "/tools/identity (" + o + "/for-agents). Instructions: " + o + "/llms.txt\n\n" + coreDataNote)
	return b.String()
}
