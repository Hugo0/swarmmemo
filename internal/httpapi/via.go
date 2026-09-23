package httpapi

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"swarmmemo/internal/board"
)

// Provenance for the HTTP routes (board.Vias). Every write route records the
// channel it served before the one call into the board; nothing a command
// carries can change it.

// BridgeTokenMinBytes is the shortest bridge secret the server accepts.
const BridgeTokenMinBytes = 32

// Headers an operator bridge sends to claim its channel. Browsers cannot send
// them cross-origin: CORS allows only Content-Type and X-Text.
const (
	bridgeHeader      = "X-SwarmMemo-Bridge"
	bridgeTokenHeader = "X-SwarmMemo-Bridge-Token"
)

// withVia records via on the request's context.
func withVia(r *http.Request, via string) *http.Request {
	return r.WithContext(board.WithVia(r.Context(), via))
}

// sameOrigin reports a request the browser itself marked as coming from one of
// this site's pages. Sec-Fetch-Site is set by the browser and cannot be set by
// page script; a non-browser client can imitate it, which is why "ui" is
// documented as inferred, not proven.
func sameOrigin(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

// writeVia is the channel of a /w, /w64 or /v1/events write. An X-Text header
// names the channel whatever the method; the site's own no-script composer (a
// same-origin form POST) is "ui"; otherwise the method decides.
func writeVia(r *http.Request, header, form bool) string {
	switch {
	case header:
		return "x-text"
	case form && r.Method == http.MethodPost && sameOrigin(r):
		return "ui"
	}
	return strings.ToLower(r.Method)
}

// bridgeVia checks an operator bridge's claim. No claim returns "". A claim
// must name a bridge channel whose secret the operator configured, present
// that secret, and arrive over HTTPS; anything else is refused rather than
// silently posted under another channel.
func (s *Server) bridgeVia(r *http.Request) (string, error) {
	values, claimed := r.Header[http.CanonicalHeaderKey(bridgeHeader)]
	if !claimed {
		return "", nil
	}
	refuse := &board.Error{Status: 403, Code: "bridge_unverified", Message: "The " + bridgeHeader + " claim was not accepted: it needs a configured bridge channel, its secret in " + bridgeTokenHeader + ", and HTTPS. Send the request without the claim to post as yourself."}
	tokens := r.Header.Values(bridgeTokenHeader)
	if len(values) != 1 || len(tokens) != 1 || !board.BridgeVia(values[0]) || !s.secure(r) {
		return "", refuse
	}
	want := s.cfg.BridgeTokens[values[0]]
	if len(want) < BridgeTokenMinBytes || subtle.ConstantTimeCompare([]byte(tokens[0]), []byte(want)) != 1 {
		return "", refuse
	}
	return values[0], nil
}

// commandVia is the channel of /v1/command and /c64: a verified bridge claim,
// the site's own pages, or fallback.
func (s *Server) commandVia(r *http.Request, fallback string) (string, error) {
	if via, err := s.bridgeVia(r); via != "" || err != nil {
		return via, err
	}
	if fallback == "command" && sameOrigin(r) {
		return "ui", nil
	}
	return fallback, nil
}

// mcpVia marks every MCP tool call.
func mcpVia(ctx context.Context) context.Context { return board.WithVia(ctx, "mcp") }

// viaNames lists every channel value, for /capabilities.
func viaNames() []string {
	names := []string{}
	for _, v := range board.Vias() {
		names = append(names, v.Name)
	}
	return names
}
