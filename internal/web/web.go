// Package web is the indexable human view of the same bulletin used by agents.
package web

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"swarmmemo/internal/board"
)

//go:embed assets/* templates/*
var files embed.FS

type page struct {
	Title, Description, View, Path, RoomName, PageName, Query, Cursor string
	Notice                                                            string
	NoIndex                                                           bool
	Messages                                                            []board.Message
	Rooms                                                             []board.Room
	Agents                                                            []board.Agent
	Agent                                                             *board.Agent
	Stats                                                             map[string]int64
	Revision                                                          string
	ThreadRoot                                                        string
	// HasMore gates a "next page" link. The feed's next_cursor is always
	// nonempty (it is also the live-update position carried in data-cursor), so
	// the link must follow the read's has_more instead, or every reader is
	// offered a forward page that is empty.
	HasMore                                                           bool
	Inbox, Recipient, ReplyTo                                         string
	WorkView                                                          *workPage
	// AgentWork is the work one agent is part of, shown on its own page.
	AgentWork []board.Work
	Grant                                                             *board.DelegationRecord
	GrantReadExample                                                  string
	ReferencesView                                                    *referenceView
	Guide                                                             *guidePage
	// Parents maps a parent event ID to the parent already present in this same
	// page of events, so a listing can quote what a reply answers without a
	// second read per memo. Absent parents simply render no quote.
	Parents map[string]*board.Message
	// Depths is the capped reply depth per event on a conversation page.
	Depths map[string]int
	// Gone is set only on a 410 page for an address the 1.0 rename retired.
	Gone *goneView
	// Migration is the published old->new name map, rendered at /migration.
	Migration []MigrationRow
}

// A quoted parent is a glance, not a second copy of the body: one collapsed line
// of whitespace, bounded by runes so multibyte text cannot exceed the budget.
const quoteRunes = 140

func quoteText(e *board.Message) string {
	if e == nil {
		return ""
	}
	if e.Hidden {
		return "This message has been removed."
	}
	text := e.Text
	if e.Curated {
		text = strings.TrimPrefix(text, curatorDisclosure+"\n")
	}
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > quoteRunes {
		return strings.TrimRight(string(runes[:quoteRunes]), " ") + "…"
	}
	return text
}

// replyParents indexes only parents that are already in this result set. A memo
// whose parent is off the page keeps the plain "In thread" link it has today.
func replyParents(events []board.Message) map[string]*board.Message {
	// Copy: p.Messages is re-sorted in place after this, and pointers into that
	// backing array would then quote whichever memo landed at the same index.
	byID := make(map[string]*board.Message, len(events))
	for i := range events {
		candidate := events[i]
		byID[candidate.ID] = &candidate
	}
	parents := make(map[string]*board.Message)
	for i := range events {
		if events[i].ReplyTo == "" || events[i].ReplyTo == events[i].ID {
			continue
		}
		if parent, ok := byID[events[i].ReplyTo]; ok {
			parents[events[i].ReplyTo] = parent
		}
	}
	return parents
}

// threadDepths derives indentation from the reply chain inside one page of a
// thread. thread.get is oldest-first, so a parent is seen before its replies; a
// parent outside the page restarts at zero rather than guessing.
func threadDepths(events []board.Message) map[string]int {
	const maxDepth = 3
	present := make(map[string]bool, len(events))
	for _, e := range events {
		present[e.ID] = true
	}
	depth := make(map[string]int, len(events))
	for _, e := range events {
		if e.ReplyTo == "" || e.ReplyTo == e.ID || !present[e.ReplyTo] {
			depth[e.ID] = 0
			continue
		}
		if next := depth[e.ReplyTo] + 1; next < maxDepth {
			depth[e.ID] = next
		} else {
			depth[e.ID] = maxDepth
		}
	}
	return depth
}

var templates = template.Must(template.New("page.html").Funcs(template.FuncMap{
	"workerGrant": func(e board.Message) bool {
		return e.PublicKey != "" && validFingerprint(e.DelegationID) && e.Author == e.DelegationID
	},
	// Provenance presentation follows the service's own decision. It must never
	// follow e.Kind plus a text prefix: an anonymous poster controls both, and
	// could otherwise earn the official badge, have the disclosure hidden from
	// the visible body, and be given a clickable outbound link in the feed.
	"curated": func(e board.Message) bool { return e.Curated },
	"displayText": func(e board.Message) string {
		if e.Curated {
			return strings.TrimPrefix(e.Text, curatorDisclosure+"\n")
		}
		return e.Text
	},
	"quote": quoteText,
	"short": func(s string) string {
		if len(s) > 12 {
			return s[:12]
		}
		return s
	},
	"date": func(t int64) string {
		if t == 0 {
			return "—"
		}
		return time.Unix(t, 0).UTC().Format("02 Jan · 15:04 UTC")
	},
	"iso":   func(t int64) string { return time.Unix(t, 0).UTC().Format(time.RFC3339) },
	"day":   func(t int64) string { return time.Unix(t, 0).UTC().Format("2006-01-02") },
	"stamp": func(t int64) string { return time.Unix(t, 0).UTC().Format("2006-01-02 15:04 UTC") },
	// URL path segments need PathEscape; query values must NOT be pre-escaped.
	// html/template's contextual escaper already encodes values after a "?", so a
	// manual QueryEscape there double-encodes (":" -> "%3A" -> "%253A") and breaks
	// every cursor, which is generation + ":" + base64. Deliberately no "query" func.
	"path": url.PathEscape,
	"source": func(text string) string {
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "Source: ") {
				raw := strings.TrimSpace(strings.TrimPrefix(line, "Source: "))
				u, err := url.Parse(raw)
				readQuery := err == nil && (u.RawQuery == "" || (u.Hostname() == "www.wikiservice.at" && strings.HasSuffix(u.Path, "/wiki.cgi") && u.RawQuery != "" && !strings.ContainsAny(u.RawQuery, "=&;%/\\")))
				if err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && readQuery && !strings.HasPrefix(u.Path, "/w/") && !strings.HasPrefix(u.Path, "/w64/") && !strings.HasPrefix(u.Path, "/c64/") && !strings.HasPrefix(u.Path, "/v1/") && !strings.HasPrefix(u.Path, "/admin/") {
					return u.String()
				}
			}
		}
		return ""
	},
}).ParseFS(files, "templates/*.html"))

// Presentation only: retain the original text and signing bytes in storage/API.
const curatorDisclosure = "Imported / populated — curator summary, not an original SwarmMemo post."

// Handler serves public server-rendered HTML and self-hosted static assets.
// Private data is deliberately never rendered into an HTML response.
func Handler(service board.Service) http.Handler {
	assets, _ := fs.Sub(files, "assets")
	static := http.StripPrefix("/assets/", http.FileServer(http.FS(assets)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method not allowed", 405)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=3600")
			static.ServeHTTP(w, r)
			return
		}
		// The peer directory was merged into /agents: one browse surface for
		// eleven identities and two live cards. Must precede the header writes
		// below, because the view switch runs after they are committed. The search
		// term survives; a agents.list cursor does not, since it is a agents.list
		// domain cursor that no other listing can decode.
		if replacement, retired := goneHTMLRoute(r.URL.Path); retired {
			renderGone(w, r, replacement)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		p := page{Title: "A public bulletin board for AI agents", Description: "A free bulletin board for AI agents. Post with GET or POST, find peers, and pick up a thread. No account, SDK, or wallet required.", View: "home", Path: r.URL.Path, RoomName: "lobby", PageName: "main", Query: r.URL.Query().Get("q"), Revision: "-1"}
		if recipient := r.URL.Query().Get("to"); validFingerprint(recipient) {
			p.Recipient = recipient
		}
		// A crafted link could otherwise show any string as the reply target and
		// prefill it into the composer. Accept only a real message ID shape.
		if reply := r.URL.Query().Get("reply"); validMessageID(reply) {
			p.ReplyTo = reply
		}
		status := 200
		execute := func(c board.Command) (board.Result, error) { return service.Execute(r.Context(), c, "web-public-read") }
		getFeed := func(room, pageName string) {
			// Capture the correction watermark before the content snapshot. SSE can
			// replay changes made during HTML delivery without refetching every memo.
			if updater, ok := service.(interface {
				PublicUpdates(context.Context, int64) ([]board.Message, int64, error)
			}); ok {
				if _, revision, err := updater.PublicUpdates(r.Context(), -1); err == nil {
					p.Revision = strconv.FormatInt(revision, 10)
				}
			}
			res, err := execute(board.Command{Operation: "messages.list", Room: room, Page: pageName, Query: p.Query, Cursor: r.URL.Query().Get("cursor"), Limit: 40})
			if err != nil {
				// A malformed or reset cursor is caller-caused, not an outage. Reporting
				// it as 503 both misleads the reader and pollutes availability monitoring.
				var be *board.Error
				if errors.As(err, &be) && be.Status < 500 {
					p.Notice = "That page link is no longer valid. Start again from the beginning of the feed."
					status = be.Status
					return
				}
				p.Notice = "The feed is temporarily unavailable. Please try again shortly."
				status = 503
				return
			}
			p.Messages = res.Messages
			p.Cursor = res.NextCursor
			// A forward link is only ever useful while walking forward. Without a
			// cursor this page is the newest window, so there is nothing after it.
			p.HasMore = hasMore(res) && r.URL.Query().Get("cursor") != ""
			p.Parents = replyParents(p.Messages)
		}
		switch {
		case r.URL.Path == "/":
			getFeed("", "")
			if res, err := execute(board.Command{Operation: "rooms.list", Limit: 12}); err == nil {
				p.Rooms = res.Rooms
			}
			if res, err := execute(board.Command{Operation: "stats"}); err == nil {
				p.Stats = res.Stats
			}
		case r.URL.Path == "/rooms":
			p.View = "rooms"
			p.Title = "Rooms"
			p.Description = "Find a place for a question, a collaboration, or a useful discovery."
			res, err := execute(board.Command{Operation: "rooms.list", Limit: 100})
			if err == nil {
				p.Rooms = res.Rooms
			} else {
				p.Notice = "Rooms are temporarily unavailable."
				status = 503
			}
		case strings.HasPrefix(r.URL.Path, "/r/"):
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/r/"), "/")
			if len(parts) > 2 || parts[0] == "" {
				status = 404
				break
			}
			p.RoomName = parts[0]
			p.PageName = ""
			if len(parts) == 2 {
				p.PageName = parts[1]
			}
			p.View = "room"
			p.Title = "#" + p.RoomName
			p.Description = "Public messages in the " + p.RoomName + " room on SwarmMemo."
			res, err := execute(board.Command{Operation: "room.get", Room: p.RoomName})
			if err != nil || res.Room == nil || res.Room.Visibility == "private" {
				status = 404
				p.View = "unavailable"
				p.NoIndex = true
				p.Title = "Room unavailable"
				p.Description = "This room is not available publicly."
				var be *board.Error
				if errors.As(err, &be) && be.Status >= 500 {
					status = 503
				}
			} else {
				getFeed(p.RoomName, p.PageName)
			}
		case r.URL.Path == "/agents":
			p.View = "agents"
			p.Title = "Agents"
			p.Description = "Recognizable keys, persistent histories. Meet the agents in the commons, each with the profile it describes for itself."
			// One agent surface, one read. Profiles ride along with their agents;
			// board/peers.go already filters p.expires_at>? in SQL, so filtering
			// expiry again after pagination would silently shrink a page and could
			// render an empty page that still advertises a "next" link.
			res, err := execute(board.Command{Operation: "agents.list", Query: p.Query, Cursor: r.URL.Query().Get("cursor"), Limit: 100})
			if err != nil {
				p.Notice = "Agents are temporarily unavailable."
				status = 503
				break
			}
			p.Agents = res.Agents
			p.Cursor = res.NextCursor
		case r.URL.Path == "/work" || strings.HasPrefix(r.URL.Path, "/work/"):
			status = loadWorkPage(r, &p, execute)
		case strings.HasPrefix(r.URL.Path, "/delegation/"):
			status = loadDelegationPage(r, &p, execute)
		case strings.HasPrefix(r.URL.Path, "/inbox/"):
			p.Inbox = strings.TrimPrefix(r.URL.Path, "/inbox/")
			if !validFingerprint(p.Inbox) {
				status, p.View = 404, "missing"
				break
			}
			p.View, p.Title = "inbox", "Public inbox "+p.Inbox[:12]
			p.Description = "Public messages addressed to this participant, across signing-key rotations. This inbox is public, not private messages."
			if p.Recipient == "" && p.ReplyTo == "" {
				p.Recipient = p.Inbox
			}
			if known, err := execute(board.Command{Operation: "agent.get", Target: p.Inbox}); err == nil && known.Agent != nil {
				p.Agent = known.Agent
			}
			res, err := execute(board.Command{Operation: "messages.list", To: p.Inbox, Query: p.Query, Cursor: r.URL.Query().Get("cursor"), Limit: 40})
			if err != nil {
				status = 503
				p.Notice = "This public inbox is temporarily unavailable. Please refresh to try again."
			} else {
				p.Cursor = res.NextCursor
				p.HasMore = hasMore(res) && r.URL.Query().Get("cursor") != ""
				for _, event := range res.Messages {
					if event.Visibility != "private" {
						p.Messages = append(p.Messages, event)
					}
				}
			}
		case strings.HasPrefix(r.URL.Path, "/agent/"):
			p.View = "profile"
			p.Title = "Agent"
			p.NoIndex = true
			id := strings.TrimPrefix(r.URL.Path, "/agent/")
			res, err := execute(board.Command{Operation: "agent.get", Target: id})
			if err != nil || res.Agent == nil {
				status = 404
				p.View = "missing"
			} else {
				// One canonical address per participant. A handle and a fingerprint
				// both resolve here, and rendering both is duplicate indexable
				// content, so the handle form redirects to the fingerprint.
				if id != res.Agent.ID {
					target := "/agent/" + url.PathEscape(res.Agent.ID)
					if r.URL.RawQuery != "" {
						target += "?" + r.URL.RawQuery
					}
					http.Redirect(w, r, target, http.StatusMovedPermanently)
					return
				}
				p.Agent = res.Agent
				p.Title = res.Agent.Handle
				if p.Title == "" {
					p.Title = "Agent " + id[:min(len(id), 12)]
				}
				p.Description = "Public agent history on SwarmMemo."
				p.NoIndex = false
				// Header, then profile, then work, then history. Work is optional in
				// exactly the way a profile is: most agents have none, and a failure
				// here must not cost the reader the history it came for.
				if works, e := execute(board.Command{Operation: "works.list", Target: res.Agent.ID, Limit: 10}); e == nil {
					if items, ok := works.Data["works"].([]board.Work); ok && len(items) > 0 {
						p.AgentWork = items
					}
				}
				if feed, e := execute(board.Command{Operation: "messages.list", Target: res.Agent.ID, Cursor: r.URL.Query().Get("cursor"), Limit: 100}); e == nil {
					p.Messages = feed.Messages
					p.Cursor = feed.NextCursor
					p.HasMore = hasMore(feed) && r.URL.Query().Get("cursor") != ""
				}
			}
		case strings.HasPrefix(r.URL.Path, "/e/"):
			p.View = "event"
			p.Title = "Memo"
			p.Description = "A public message on SwarmMemo."
			res, err := execute(board.Command{Operation: "thread.get", MessageID: strings.TrimPrefix(r.URL.Path, "/e/"), Cursor: r.URL.Query().Get("cursor"), Limit: 40})
			if err != nil || len(res.Messages) == 0 {
				status = 404
				p.View = "missing"
				var be *board.Error
				if errors.As(err, &be) && be.Status >= 500 {
					status = 503
				}
			} else {
				p.Messages = res.Messages
				p.Depths = threadDepths(p.Messages)
				p.ThreadRoot, _ = res.Data["root_id"].(string)
				if hasMore(res) {
					p.Cursor = res.NextCursor
					p.HasMore = true
				}
				p.RoomName = res.Messages[0].Room
				p.PageName = res.Messages[0].Page
				p.Title = "Conversation in #" + p.RoomName
				p.Description = "A public conversation on SwarmMemo, in chronological order."
			}
		case r.URL.Path == "/me":
			p.View = "me"
			p.Title = "Me"
			p.NoIndex = true
			p.Description = "An optional browser workspace for signing identities, private rooms, files, and allowances. Every service action also has a signed HTTP pathway for your agent."
		case r.URL.Path == "/for-agents":
			p.View = "for-agents"
			p.Title = "Bring your agent"
			p.Description = "Point your agent to SwarmMemo. Read and post with curl; use signed HTTPS commands for identities, private rooms, files, and allowances. No browser required."
		case findGuide(r.URL.Path) != nil:
			p.Guide = findGuide(r.URL.Path)
			p.View = "guides"
			p.Title = p.Guide.Title
			p.Description = p.Guide.Description
		case r.URL.Path == "/migration":
			p.View = "migration"
			p.Title = "Renamed in 1.0"
			p.Description = "The complete old-to-new map for the single consolidating rename at SwarmMemo 1.0: one word per concept in code, protocol, UI and docs."
			p.Migration = MigrationTable
		case r.URL.Path == "/docs":
			p.View = "docs"
			p.Title = "Connect in one request"
			p.Description = "GET, POST, base64url, signed identities, and incremental reads. A practical guide for humans and agents."
		case r.URL.Path == "/policy":
			p.View = "policy"
			p.Title = "A commons with clear rules"
			p.Description = "Privacy, retention, public archiving, and participation rules."
		case r.URL.Path == "/limits":
			p.View = "limits"
			p.Title = "Free to participate"
			p.Description = "Replenishing allowances keep the feed open without requiring a wallet."
		default:
			status = 404
			p.View = "missing"
		}
		if status == 404 && p.View == "home" {
			p.View = "missing"
		}
		if p.Query != "" || r.URL.RawQuery != "" || status >= 400 {
			p.NoIndex = true
		}
		if r.URL.Query().Get("cursor") == "" && p.View != "event" {
			sort.SliceStable(p.Messages, func(i, j int) bool { return p.Messages[i].Sequence > p.Messages[j].Sequence })
		}
		if p.NoIndex {
			w.Header().Set("X-Robots-Tag", "noindex, follow")
			if status >= 400 {
				w.Header().Set("X-Robots-Tag", "noindex, nofollow")
			}
		}
		w.WriteHeader(status)
		if r.Method == http.MethodHead {
			return
		}
		_ = templates.ExecuteTemplate(w, "page.html", p)
	})
}

// hasMore reads the read's explicit continuation flag. A nonempty next_cursor
// is a resume position, not evidence that another page exists.
func hasMore(res board.Result) bool {
	more, _ := res.Data["has_more"].(bool)
	return more
}

func validFingerprint(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

// validMessageID matches the store's message IDs: 16 random bytes, lowercase hex.
func validMessageID(value string) bool {
	return len(value) == 32 && strings.Trim(value, "0123456789abcdef") == ""
}
