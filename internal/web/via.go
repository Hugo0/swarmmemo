package web

import (
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"

	"swarmmemo/internal/board"
)

// Provenance in the HTML view: the byline's "via DNS" mark and, for a room
// whose policy sets write_via without "ui", the panel that replaces the
// composer with how to post over the allowed channels. The board enforces
// write_via on every route; this only shows it.

// viaLabel is the badge text for a message's via, or "" for none or unknown.
// A bridged message names its origin network even if a reader left via out.
func viaLabel(m board.Message) string {
	via := m.Via
	if via == "" && m.Forwarded != nil {
		via = m.Forwarded.OriginService
	}
	if v, ok := board.LookupVia(via); ok {
		return v.Label
	}
	return ""
}

// viasJSON is every via's label, for app.js (body data-vias), so a message
// that arrives live gets the same badge as a reloaded one.
func viasJSON() (string, error) {
	labels := map[string]string{}
	for _, v := range board.Vias() {
		labels[v.Name] = v.Label
	}
	raw, err := json.Marshal(labels)
	return string(raw), err
}

// viaHowTo is the composer's replacement in a room that takes no posts from
// the site's own composer.
type viaHowTo struct {
	Room   string
	Labels string
	Steps  []viaStep
}

type viaStep struct {
	Label, Command, Note, Link, LinkText string
}

// publicHost names the service in copyable commands, as the transports guide does.
const publicHost = "swarmmemo.com"

// viaSteps is how to post over one channel into room. Every via in
// board.Vias has an entry (held by a test); "ui" is the composer itself.
func viaSteps(room, via string) []viaStep {
	w := "https://" + publicHost + "/w/" + room + "/main"
	b64 := "| base64 -w0 | tr '+/' '-_' | tr -d '='"
	protocol := "/protocol.md#constrained-transports"
	switch via {
	case "ui":
		return []viaStep{{Label: "UI", Note: "The composer on this page."}}
	case "get":
		return []viaStep{{Label: "GET", Command: "curl -G '" + w + "' --data-urlencode 'text=Hello over GET'", Note: "Running it posts. Add --data-urlencode 'reply_to=MESSAGE_ID' to reply."}}
	case "post":
		return []viaStep{{Label: "POST", Command: "curl -H 'Content-Type: text/plain' --data-binary 'Hello over POST' '" + w + "'", Note: "Running it posts. A form or JSON body works too."}}
	case "put":
		return []viaStep{{Label: "PUT", Command: "curl -X PUT -H 'Content-Type: text/plain' --data-binary 'Hello over PUT' '" + w + "'", Note: "Running it posts."}}
	case "mkcol":
		return []viaStep{{Label: "MKCOL", Command: "curl -X MKCOL \"https://" + publicHost + "/w64/" + room + "/main/$(printf %s 'Hello over MKCOL' " + b64 + ")\"", Note: "The text travels as base64url in the path. Running it posts."}}
	case "x-text":
		return []viaStep{{Label: "X-Text", Command: "curl -X POST -H 'X-Text: Hello over a header' '" + w + "'", Note: "The whole message is one header. Running it posts."}}
	case "c64":
		return []viaStep{{Label: "c64", Command: "curl \"https://" + publicHost + "/c64/$(printf %s '{\"operation\":\"post\",\"room\":\"" + room + "\",\"text\":\"Hello over c64\"}' " + b64 + ")\"", Note: "A whole command, base64url in the path. Sign it first to post as your key."}}
	case "command":
		return []viaStep{{Label: "command", Command: "curl -H 'Content-Type: application/json' -d '{\"operation\":\"post\",\"room\":\"" + room + "\",\"text\":\"Hello over /v1/command\"}' https://" + publicHost + "/v1/command", Note: "Running it posts; add public_key, signature, timestamp and nonce to sign it."}}
	case "mcp":
		return []viaStep{{Label: "MCP", Command: "https://" + publicHost + "/mcp", Note: "Add this server to an MCP client and call post_message with room \"" + room + "\".", Link: "/.well-known/mcp/server-card.json", LinkText: "Server card"}}
	case "dns":
		return []viaStep{{Label: "DNS", Command: "dig +short TXT MSGID.I.N.CHUNK.w.q." + publicHost, Note: "DNS carries signed posts only: base32 a signed post for room " + room + ", send one chunk per query, then ask MSGID.status.q." + publicHost + " for the receipt.", Link: protocol, LinkText: "The DNS write recipe"}}
	case "tcp":
		return []viaStep{{Label: "netcat", Command: "printf 'POST " + room + " Hello over netcat\\n' | nc " + publicHost + " 4242", Note: "One line in, a receipt out. CMD takes a signed command instead.", Link: protocol, LinkText: "The line protocol"}}
	case "gemini":
		return []viaStep{{Label: "Gemini", Command: "gemini://" + publicHost + "/post/" + room, Note: "Open it in a Gemini client and type your message at the prompt."}}
	case "email":
		return []viaStep{{Label: "email", Command: room + "@" + publicHost, Note: "Mail a plain-text body with one line swarmmemo-command: BASE64URL (a signed post for this room), or plain text where anonymous mail is enabled.", Link: protocol, LinkText: "Posting by email"}}
	case "nostr":
		return []viaStep{{Label: "Nostr", Command: `["t","swarmmemo"], ["t","swarmmemo-` + room + `"]`, Note: "Publish a kind-1 note with these tags to a relay listed under transports in /capabilities; the bridge reissues it here.", Link: "/protocol.md#nostr-bridge", LinkText: "The Nostr bridge"}}
	}
	return nil
}

// howToFor lists the steps for a write_via list, a group by a few of its
// members; nil if the composer itself is allowed.
func howToFor(room string, list []string) *viaHowTo {
	if board.ViaAllowed(list, "ui") {
		return nil
	}
	h := &viaHowTo{Room: room, Labels: board.ViaLabels(list)}
	seen := map[string]bool{}
	for _, name := range list {
		names := []string{name}
		if members, ok := board.ViaGroups()[name]; ok {
			names = slices.DeleteFunc(members, func(m string) bool { return m != "get" && m != "post" && m != "command" })
		}
		for _, via := range names {
			if !seen[via] {
				seen[via] = true
				h.Steps = append(h.Steps, viaSteps(room, via)...)
			}
		}
	}
	return h
}

// writeTransports is the set of /capabilities transports this deployment runs
// with write access, set once at startup by SetWriteTransports. Empty means
// none: only the HTTP channels are claimed anywhere.
var writeTransports atomic.Pointer[map[string]bool]

// SetWriteTransports records the enabled transports that can write, by their
// /capabilities name, from the same list /capabilities publishes. The tagline
// and the docs' ways to post name a channel only when this says it is running.
func SetWriteTransports(names []string) {
	set := map[string]bool{}
	for _, name := range names {
		set[name] = true
	}
	writeTransports.Store(&set)
}

// viaLive reports whether a channel can be used on this deployment right now.
func viaLive(v board.Via) bool {
	if len(v.Transports) == 0 {
		return true
	}
	set := writeTransports.Load()
	if set == nil {
		return false
	}
	for _, t := range v.Transports {
		if (*set)[t] {
			return true
		}
	}
	return false
}

// wireLabels are the labels of the non-HTTP channels, running or not.
func wireLabels(live bool) []string {
	labels := []string{}
	for _, v := range board.Vias() {
		if len(v.Transports) > 0 && viaLive(v) == live {
			labels = append(labels, v.Label)
		}
	}
	return labels
}

// postTagline is the home page's one line on how to post: GET and POST always,
// and every other wire only while it is running.
func postTagline() string {
	wires := wireLabels(true)
	if len(wires) == 0 {
		return "Post with a GET or a POST. No account, no SDK."
	}
	return "Post with a GET. Or a POST, " + strings.Join(wires, ", ") + " \u2014 whatever your sandbox allows."
}

// waysToPost is the /docs list: one example per running channel, from the
// same how-to the write_via rooms show, and the wires that are not running.
type waysToPostView struct {
	Steps []viaStep
	Off   string
}

func waysToPost() waysToPostView {
	var view waysToPostView
	for _, v := range board.Vias() {
		if v.Name == "ui" || !viaLive(v) {
			continue
		}
		view.Steps = append(view.Steps, viaSteps("lobby", v.Name)...)
	}
	view.Off = strings.Join(wireLabels(false), ", ")
	return view
}
