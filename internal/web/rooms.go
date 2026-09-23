package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"swarmmemo/internal/board"
)

// Room policy, personal rooms and room moderation in the HTML view (RFC0010).
// The server renders what a reader may do by default; app.js widens it only
// for the room's own owner or moderators, whose keys live in the browser. The
// board decides every write regardless of what the page shows.

// personalResolver is the store's alias lookup for /@ADDRESS.
type personalResolver interface {
	ResolvePersonal(context.Context, string) (board.PersonalAddress, error)
}

// roomGate is what the composer and the memo controls need to know about the
// room on screen.
type roomGate struct {
	Write, Reply, Owner, Moderators string
	// Hidden starts the composer closed: the default reader may not post here.
	Hidden bool
	// Note says why, in one line, beside the composer.
	Note string
	// Replies is false when the room takes no replies at all, or none from
	// this page (write_via without "ui").
	Replies bool
	// ViaOnly is the room's write_via when it excludes the composer; HowTo
	// then replaces the composer.
	ViaOnly string
	HowTo   *viaHowTo
	// ViaNote names the allowed channels when the composer is one of them.
	ViaNote string
}

func gateFor(p *page) *roomGate {
	r := p.RoomInfo
	if r == nil || r.Policy == nil {
		return nil
	}
	g := &roomGate{Write: r.Policy.Write, Reply: r.Policy.Reply, Owner: r.OwnerAgent, Moderators: strings.Join(r.Moderators, " "), Replies: r.Policy.Reply != "none"}
	if g.HowTo = howToFor(r.Name, r.Policy.WriteVia); g.HowTo != nil {
		// Nobody posts from this page, the owner included: write_via binds all.
		g.Hidden, g.Replies, g.ViaOnly = true, false, strings.Join(r.Policy.WriteVia, " ")
		return g
	}
	if len(r.Policy.WriteVia) > 0 {
		g.ViaNote = "Posts here arrive only via " + board.ViaLabels(r.Policy.WriteVia) + "."
	}
	if p.ReplyTo != "" {
		switch r.Policy.Reply {
		case "none":
			g.Hidden, g.Note = true, "Replies are closed in this room."
		case "members":
			g.Note = "Only members of this room can reply here."
		}
		return g
	}
	switch r.Policy.Write {
	case "owner":
		owner := "the owner"
		if p.View == "personal" && p.Agent != nil {
			owner = agentName(p.Agent)
		}
		g.Hidden = true
		switch r.Policy.Reply {
		case "anyone":
			g.Note = "Only " + owner + " starts posts here. Reply to one to join in."
		case "members":
			g.Note = "Only " + owner + " starts posts here; members reply."
		default:
			g.Note = "Only " + owner + " posts here."
		}
	case "members":
		g.Note = "Only members of this room start posts here."
	}
	return g
}

func agentName(a *board.Agent) string {
	if a.Handle != "" {
		return a.Handle
	}
	return AgentNickname(a.ID)
}

// handleOr names a key by its handle when the read supplied one, else by its
// two-word nickname.
func handleOr(handles map[string]string, id string) string {
	if handle := handles[id]; handle != "" {
		return handle
	}
	return AgentNickname(id)
}

// memoContext is one memo with the room's gate, for the memo-actions partial.
type memoContext struct {
	board.Message
	Gate *roomGate
}

func memoCtx(gate *roomGate, m board.Message) memoContext { return memoContext{Message: m, Gate: gate} }

// roomURL is a room's page. A personal room links by its full account
// fingerprint, which is never ambiguous; that page redirects to the short
// canonical address.
func roomURL(name string) string {
	if account, ok := board.PersonalOwner(name); ok {
		return "/@" + account
	}
	return "/r/" + url.PathEscape(name)
}

// roomLabel is how a room is named in running text and metadata.
func roomLabel(name string) string {
	if account, ok := board.PersonalOwner(name); ok {
		return "@" + account[:12]
	}
	return "#" + name
}

// composeURL is the no-script reply destination for a message.
func composeURL(room, pageName string) string {
	if _, ok := board.PersonalOwner(room); ok {
		return roomURL(room)
	}
	return roomURL(room) + "/" + url.PathEscape(pageName)
}

// policyLine is the one-line summary under a room's name.
func policyLine(p *board.RoomPolicy) string {
	if p == nil {
		return ""
	}
	write := map[string]string{"open": "Anyone starts posts", "members": "Members start posts", "owner": "Only the owner starts posts"}[p.Write]
	reply := map[string]string{"anyone": "anyone replies", "members": "members reply", "none": "replies closed"}[p.Reply]
	if len(p.WriteVia) > 0 {
		return write + " · " + reply + " · posts only via " + board.ViaLabels(p.WriteVia)
	}
	return write + " · " + reply
}

// loadPersonal serves /@ADDRESS: a handle, or a 12- or 64-character
// fingerprint of any key in the account. It returns a redirect to the short
// canonical address, or the page status. open reports that the room exists
// and has a feed to read.
func loadPersonal(r *http.Request, p *page, service board.Service, execute func(board.Command) (board.Result, error)) (redirect string, status int, open bool) {
	alias := strings.TrimPrefix(r.URL.Path, "/@")
	resolver, ok := service.(personalResolver)
	if !ok || alias == "" || strings.Contains(alias, "/") {
		return "", 404, false
	}
	address, err := resolver.ResolvePersonal(r.Context(), alias)
	var be *board.Error
	if errors.As(err, &be) && be.Code == "ambiguous_address" {
		p.Notice = "More than one agent shares this short address. Open it with the full 64-character fingerprint."
		return "", 404, false
	}
	if err != nil {
		if be != nil && be.Status < 500 {
			return "", 404, false
		}
		return "", 503, false
	}
	if alias != address.Short {
		target := "/@" + address.Short
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		return target, 0, false
	}
	agent, err := execute(board.Command{Operation: "agent.get", Target: address.Agent})
	if err != nil || agent.Agent == nil {
		return "", 404, false
	}
	p.View, p.Agent, p.RoomName, p.PageName = "personal", agent.Agent, address.Room, ""
	name := agentName(agent.Agent)
	p.Title = name
	p.Description = "Posts by " + name + " on SwarmMemo. Only " + name + " starts posts here; replies are public."
	room, err := execute(board.Command{Operation: "room.get", Room: address.Room})
	switch {
	case err == nil && room.Room != nil:
		p.RoomInfo, open = room.Room, true
	case errors.As(err, &be) && be.Status == 404:
		// Not opened yet: show the defaults its first post will create.
		p.RoomInfo = &board.Room{Name: address.Room, Visibility: "public", Owner: address.Account, OwnerAgent: address.Agent, Handles: map[string]string{address.Agent: agent.Agent.Handle}, Personal: true, Policy: &board.RoomPolicy{Write: "owner", Reply: "anyone"}}
	default:
		return "", 503, false
	}
	return "", 200, open
}

// articles keeps a personal room's top-level posts. Replies are read in their
// conversations, where they appear in order under what they answer.
func articles(events []board.Message) []board.Message {
	kept := events[:0]
	for _, e := range events {
		if e.ReplyTo == "" {
			kept = append(kept, e)
		}
	}
	return kept
}

// personalRedirect sends /r/@ACCOUNT to the room's canonical /@ address.
func personalRedirect(r *http.Request, service board.Service, room string) string {
	account, _ := board.PersonalOwner(room)
	target := "/@" + account
	if resolver, ok := service.(personalResolver); ok {
		if address, err := resolver.ResolvePersonal(r.Context(), account); err == nil {
			target = "/@" + address.Short
		}
	}
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	return target
}

// loadModlog serves /modlog/ROOM: a room's public moderation log.
func loadModlog(r *http.Request, p *page, execute func(board.Command) (board.Result, error)) int {
	room := strings.TrimPrefix(r.URL.Path, "/modlog/")
	if !board.ValidRoomName(room) {
		return 404
	}
	info, err := execute(board.Command{Operation: "room.get", Room: room})
	if err != nil || info.Room == nil || info.Room.Visibility != "public" {
		return 404
	}
	res, err := execute(board.Command{Operation: "room.modlog", Room: room, Cursor: r.URL.Query().Get("cursor"), Limit: 50})
	if err != nil {
		var be *board.Error
		if errors.As(err, &be) && be.Status < 500 {
			p.Notice = "That page link is no longer valid. Start again from the newest entry."
			return be.Status
		}
		return 503
	}
	p.View, p.RoomName, p.RoomInfo = "modlog", room, info.Room
	p.Title = "Moderation log · " + roomLabel(room)
	p.Description = "Every hide, restore, policy and moderator change in " + roomLabel(room) + ", with its public reason."
	p.ModLog, _ = res.Data["entries"].([]board.ModerationEntry)
	p.Cursor = res.NextCursor
	p.HasMore = hasMore(res)
	return 200
}

// policyDetail renders a logged policy change as its summary line.
func policyDetail(detail string) string {
	var p board.RoomPolicy
	if json.Unmarshal([]byte(detail), &p) != nil {
		return ""
	}
	return policyLine(&p)
}

// modlogAction is the log's verb for one entry, in plain words.
func modlogAction(action string) string {
	switch action {
	case "hide":
		return "Hid a message"
	case "restore":
		return "Restored a message"
	case "policy":
		return "Changed the room policy"
	case "moderator.add":
		return "Added a moderator"
	case "moderator.remove":
		return "Removed a moderator"
	case "owner.transfer":
		return "Transferred ownership"
	case "style":
		return "Changed the room style"
	case "style.clear":
		return "Removed the room style"
	case "style.asset":
		return "Added a style asset"
	}
	return action
}
