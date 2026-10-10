package web

import (
	"encoding/base64"
	"html/template"
	"net/http"

	"swarmmemo/internal/board"
)

// /connect/embed is the comment embed's "Sign in with SwarmMemo" window. The
// embed opens it with its room, the embedding page's origin and a fresh
// worker key made for that site (pub). Here, on swarmmemo.com, the reader's
// own browser key signs delegation.create for that worker key, scoped to the
// room for ninety days with the site's origin in its data, and the signed
// grant goes back to the opener by postMessage addressed to that origin only.
// The worker key proves the same bytes and submits the grant itself, so no
// private key crosses origins. action=signout signs delegation.revoke instead.
//
// It is never framed (frame-ancestors 'none', X-Frame-Options DENY), sets no
// cookie and reads none: the reader's key is this origin's localStorage.

// ConnectEmbedPath is the sign-in window's path.
const ConnectEmbedPath = "/connect/embed"

// connectEmbedCSP: scripts, styles and requests only from this origin, and no
// page may frame the window that asks the reader to Allow.
const connectEmbedCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

type connectEmbedView struct {
	Room, Origin, Pub, Action, Error string
}

// connectEmbedRequest validates the window's query: exactly room, origin and
// pub, each once, plus an optional action=signout. Anything else is refused,
// never ignored.
func connectEmbedRequest(r *http.Request) (connectEmbedView, string) {
	q := r.URL.Query()
	view := connectEmbedView{Room: q.Get("room"), Origin: q.Get("origin"), Pub: q.Get("pub"), Action: q.Get("action")}
	for key, values := range q {
		if len(values) != 1 || (key != "room" && key != "origin" && key != "pub" && key != "action") {
			return view, "This sign-in link has an unexpected parameter. Start again from the comment section."
		}
	}
	if !board.ValidRoomName(view.Room) {
		return view, "This sign-in link names no valid room. Start again from the comment section."
	}
	if !board.ValidWebOrigin(view.Origin) {
		return view, "This sign-in link names no valid site origin (https://host, no path). Start again from the comment section."
	}
	key, err := base64.RawURLEncoding.DecodeString(view.Pub)
	if err != nil || len(key) != 32 || base64.RawURLEncoding.EncodeToString(key) != view.Pub {
		return view, "This sign-in link carries no valid site key. Start again from the comment section."
	}
	if view.Action != "" && view.Action != "signout" {
		return view, "This sign-in link asks for an unknown action."
	}
	return view, ""
}

var connectEmbedPage = template.Must(template.New("connect-embed").Funcs(template.FuncMap{"asset": func(name string) string { return "/assets/" + name + "?v=" + assetVersion }}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex">
<title>{{if eq .Action "signout"}}Sign out of a site{{else}}Sign in with SwarmMemo{{end}}</title><link rel="stylesheet" href="{{asset "style.css"}}"></head>
<body class="connect-embed" data-room="{{.Room}}" data-origin="{{.Origin}}" data-pub="{{.Pub}}" data-action="{{.Action}}" data-service="swarmmemo.com">
<main class="connect-embed-card">
<h1 id="connect-embed-title">{{if eq .Action "signout"}}Sign out of a site{{else}}Sign in with SwarmMemo{{end}}</h1>
{{if .Error}}<p class="form-status error" role="alert">{{.Error}}</p>{{else}}
{{if eq .Action "signout"}}<p class="connect-embed-ask">Stop <strong>{{.Origin}}</strong> commenting and voting as <strong id="connect-embed-handle">you</strong> in <strong>#{{.Room}}</strong>?</p>
<p class="small muted">This revokes the site's key now; comments it already posted stay.</p>
{{else}}<p class="connect-embed-ask">Let <strong>{{.Origin}}</strong> comment and vote as <strong id="connect-embed-handle">you</strong> in <strong>#{{.Room}}</strong>?</p>
<ul class="small muted connect-embed-terms"><li>Only in #{{.Room}}, for 90 days. Revoke it any time in <a href="/me#site-grants" target="_blank" rel="noopener">Me</a>.</li>
<li>The site gets its own key, scoped to this room; your key never leaves swarmmemo.com.</li>
<li id="connect-embed-new" hidden>Creates a free SwarmMemo key in this browser: no email, no password. Name it and back it up later in <a href="/me" target="_blank" rel="noopener">Me</a>.</li>
<li id="connect-embed-moderate" hidden>As this room's owner or moderator, the site can also hide and restore comments, on the room's public log.</li></ul>{{end}}
<div class="button-row"><button class="button primary" id="connect-embed-allow" type="button" disabled>{{if eq .Action "signout"}}Sign out{{else}}Allow{{end}}</button><button class="button secondary" id="connect-embed-cancel" type="button">Cancel</button></div>
<p id="connect-embed-status" class="form-status" role="status">Checking your key…</p>{{end}}
</main>
<script src="{{asset "memo-core.js"}}"></script><script src="{{asset "connect-embed.js"}}"></script>
</body></html>
`))

func serveConnectEmbed(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", connectEmbedCSP)
	view, problem := connectEmbedRequest(r)
	status := http.StatusOK
	if problem != "" {
		view, status = connectEmbedView{Error: problem}, http.StatusBadRequest
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_ = connectEmbedPage.Execute(w, view)
	}
}
