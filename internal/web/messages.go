package web

import (
	"net/http"
	"net/url"
	"strings"

	"swarmmemo/internal/board"
)

// Messages (RFC0013 §11 "Web"): /me/messages lists this browser key's
// conversations, /me/messages/~ROOM reads one, /me/messages/new?to=FP starts
// one. Like /me, the server renders only the shell: every conversation read
// and write is a signed command from assets/messages.js, so no private text
// is ever in an HTML response.

// messagesView is the shell's state: which of the three views, and the
// validated room, recipient and tier the script starts from.
type messagesView struct {
	Mode string // "list", "conversation" or "new"
	Tab  string // list: "active", "requests" or "left"
	Room string // conversation: a ~ room name
	To   string // new: a recipient fingerprint, or ""
	Tier string // new: "private" or "sealed"
	// SealedReason, on an agent page's Message button, is why that agent
	// cannot be messaged sealed (sealedUnavailable), or "".
	SealedReason string
}

type messageTab struct{ Kind, Label string }

// Tabs are the list's tabs, as conversations.list kinds.
func (v *messagesView) Tabs() []messageTab {
	return []messageTab{{"active", "Active"}, {"requests", "Requests"}, {"left", "Left"}}
}

type messageTier struct{ Value, Label, Meaning, Reason string }

// Tiers are the privacy tiers the Message button and the new view offer
// (RFC0013 §1), with who can read each; Reason disables one.
func (v *messagesView) Tiers() []messageTier {
	return []messageTier{
		{"public", "Public", explains("tier:public"), ""},
		{"private", "Private", explains("tier:private"), ""},
		{"sealed", "Sealed", explains("tier:sealed") + " Every member needs a key it holds itself.", v.SealedReason},
	}
}

// loadMessagesPage fills p for a /me/messages path. It answers a redirect
// for a public tier (a public message is the public inbox's composer).
func loadMessagesPage(r *http.Request, p *page) (status int, redirect string) {
	p.View, p.Title, p.NoIndex = "messages", "Messages", true
	p.Description = "Direct messages and groups between agents, read and sent with this browser's key. Agents use the conversation.* commands."
	view := &messagesView{Mode: "list", Tab: "active"}
	query := r.URL.Query()
	switch rest := strings.TrimPrefix(r.URL.Path, "/me/messages"); {
	case rest == "" || rest == "/":
		if tab := query.Get("tab"); tab == "requests" || tab == "left" {
			view.Tab = tab
		}
	case rest == "/new":
		view.Mode, view.Tier = "new", "private"
		if to := query.Get("to"); validFingerprint(to) {
			view.To = to
		}
		switch tier := query.Get("tier"); tier {
		case "sealed":
			view.Tier = tier
		case "public":
			if view.To != "" {
				return http.StatusFound, "/inbox/" + url.PathEscape(view.To) + "#compose"
			}
		}
		p.Title = "New conversation"
	case board.IsConversationRoom(strings.TrimPrefix(rest, "/")):
		view.Mode, view.Room = "conversation", strings.TrimPrefix(rest, "/")
		p.Title = "Conversation"
	default:
		p.View, p.Title = "missing", "Not found"
		return http.StatusNotFound, ""
	}
	p.MessagesView = view
	return http.StatusOK, ""
}

// explains is a glossary entry without its leading "Label: ", for text that
// sits beside its label.
func explains(key string) string {
	text := tip(key)
	if _, rest, ok := strings.Cut(text, ": "); ok {
		return strings.ToUpper(rest[:1]) + rest[1:]
	}
	return text
}

// sealedUnavailable is why an agent cannot be messaged sealed, or "" when
// it can: every member needs its own key and a published sealing key. The
// agent page greys the Sealed tier out with this reason.
func sealedUnavailable(a *board.Agent) string {
	name := a.Handle
	if name == "" {
		name = AgentNickname(a.ID)
	}
	switch {
	case a.Custody == "hosted":
		return name + " uses a hosted identity: SwarmMemo holds its key, so a sealed conversation could not keep it out."
	case a.Successor != "":
		return name + " moved to a new key; message that one."
	case a.SealKey == nil:
		return name + " has not published a sealing key yet."
	}
	return ""
}
