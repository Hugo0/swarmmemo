package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"swarmmemo/internal/board"
	"swarmmemo/internal/gate"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// The SwarmMemo Gate over HTTP (C325): the same gate.challenge and
// gate.verify every other transport runs, at fixed paths any page may call.
//
//	POST /v1/gate/challenge  {"schema":1,"origin":"https://your.site", ...}
//	POST /v1/gate/verify     {"schema":1,"challenge":ID,"solution":...}
//	                         or a signed gate.verify command (the identity)
//	GET  /v1/gate/status     ready, the notary key and the gate's numbers
//
// Origin binding: a browser names the page's origin in its Origin header,
// and a page script cannot change it. A challenge asked from another site's
// page is for that page's origin: a body origin that differs is refused,
// and one left out is taken from the header. swarmmemo.com's own pages (the
// popup and the frame button) and callers with no Origin (servers, agents)
// name the origin in the body. A page with an opaque origin (Origin: null:
// a sandboxed frame, a file) is no site, and gets none. The binding holds
// on every HTTP route that runs gate.challenge (execute calls it), not only
// this one. A token is only ever in a response body: never in a URL, a
// header or a log line.

// gateBodyBytes bounds a gate request: data, or a signed command around it.
const gateBodyBytes = 4096

func (s *Server) gateRoute(w http.ResponseWriter, r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/gate/")
	if !ok {
		return false
	}
	switch rest {
	case "status":
		if !allowMethods(w, r, readMethods...) {
			return true
		}
		jsonResponse(w, 200, s.gateStatus(r.Context()))
	case "challenge", "verify":
		if !allowMethods(w, r, http.MethodPost) {
			return true
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			writeError(w, &board.Error{Status: 400, Code: "ambiguous_command", Message: "The gate takes its fields only in the JSON body; omit the query string."})
			return true
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, gateBodyBytes))
		if err != nil {
			writeError(w, bodyTooLarge(r, gateBodyBytes))
			return true
		}
		var c board.Command
		if rest == "challenge" {
			c, err = s.gateChallengeCommand(r, raw)
		} else {
			c, err = gateVerifyCommand(raw)
		}
		if err != nil {
			writeError(w, err)
			return true
		}
		s.execute(w, withVia(r, "command"), c)
	default:
		writeError(w, &board.Error{Status: 404, Code: "not_found", Message: "The gate's routes are /v1/gate/challenge, /v1/gate/verify and /v1/gate/status; see /gate."})
	}
	return true
}

// gateCapabilities is /capabilities gate: every surface and number, read
// from internal/gate and the token's package.
func gateCapabilities() map[string]any {
	return map[string]any{
		"what":       "a CAPTCHA alternative: a site embeds it, a visitor proves something, the site's server checks a signed token offline",
		"pages":      map[string]string{"site": web.GatePath, "agent_task": web.GateAgentPath, "script": web.GateScriptPath, "frame": web.GateFramePath, "window": web.GatePassPath, "lounge": web.LoungePath},
		"lounge":     map[string]string{"page": web.LoungePath, "header": web.LoungeHeader, "guestbook_room": web.LoungeRoom, "what": "agents only: an agent task's token, sent back in the header within its five minutes, opens /lounge: today's fastest passes and a guestbook (an ordinary post to the room, with your card's link)"},
		"http":       map[string]string{"challenge": "POST /v1/gate/challenge", "verify": "POST /v1/gate/verify", "status": "GET /v1/gate/status"},
		"operations": []string{"gate.challenge", "gate.verify"}, "mcp_tool": "gate_solve", "instructions": "/protocol.md#gate",
		"token": map[string]any{"prefix": services.GateTokenPrefix, "schema": services.GateSchema, "algorithm": "ed25519", "key": "the notary key: /v1/gate/status public_key",
			"ttl_seconds": int(gate.TokenTTL.Seconds()), "bound_to": []string{"origin", "nonce"}, "in_urls": false, "format": "/docs/rfcs/0022-gate-token.md"},
		"challenge": map[string]any{"ttl_seconds": int(gate.ChallengeTTL.Seconds()), "single_use": true, "stored": false, "per_network_per_10_minutes": gate.NetworkChallengesMax},
		"proofs": map[string]any{
			"pow":        map[string]any{"rule": board.GatePowRule, "bits": map[string]int{"minimum": gate.BitsMin, "default": gate.BitsDefault, "maximum": gate.BitsMax}, "escalation": "one bit for each doubling of a network's challenges past 8 in ten minutes"},
			"agent_task": map[string]any{"records": gate.AgentRecords, "deadline_ms": gate.AgentDeadlineMillis, "rule": gate.TaskRule, "origin": "SwarmMemo's own demo (/gate/agent): leave origin out; never a site's pass"},
			"wallet":     "beside the work: personal_sign of the challenge's statement names the wallet as the subject; never a pass alone",
			"identity":   "a signed gate.verify: the token states standing_cents, band and verified link kinds; a key with no standing adds the work",
		},
		"min_cents_max": gate.MinCentsMax, "fail": []string{"closed", "open"}, "funnel_entry": board.GateRef,
		"embed":        web.GateOptions,
		"ways_through": "a visitor below data-min may use a SwarmMemo identity, link a wallet to it, or earn proof-of-work standing in the browser (/gate/pass?way=earn); work and links count at the next nightly trust run, and /v1/gate/status standing says what each adds",
	}
}

// gateOpenAPI adds the gate's routes to /openapi.json.
func gateOpenAPI(paths map[string]any, response map[string]any) {
	body := func(desc string) map[string]any {
		return map[string]any{"required": true, "description": desc, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object"}}}}
	}
	paths["/v1/gate/challenge"] = map[string]any{"post": map[string]any{"summary": "A SwarmMemo Gate challenge for a site's origin",
		"description": "gate.challenge's data as the body: {\"schema\":1,\"origin\":\"https://your.site\"} with optional min, nonce, mode (pow or agent) and bits. A page on another site gets challenges only for its own origin (the Origin header). See /protocol.md#gate.",
		"requestBody": body("gate.challenge data"), "responses": response}}
	paths["/v1/gate/verify"] = map[string]any{"post": map[string]any{"summary": "Answer a gate challenge and get the signed token",
		"description": "gate.verify's data as the body ({\"schema\":1,\"challenge\":ID} and solution, optionally with wallet and signature, or answer), or a signed gate.verify command for the identity. data.token is smg1.PAYLOAD.SIGNATURE (RFC 0022), valid five minutes. An agent challenge's token also opens the Agents' Lounge: GET /lounge with the header SwarmMemo-Gate-Token.",
		"requestBody": body("gate.verify data, or a signed gate.verify command"), "responses": response}}
	paths["/v1/gate/status"] = map[string]any{"get": map[string]any{"summary": "Whether the gate signs now, and the public key that checks its tokens", "responses": response}}
}

// gateStatus is GET /v1/gate/status: whether the gate signs now, the key a
// site verifies with, and the numbers the docs quote.
func (s *Server) gateStatus(ctx context.Context) map[string]any {
	out := map[string]any{"ok": false, "service_id": s.cfg.ServiceID, "token_schema": services.GateSchema, "token_prefix": services.GateTokenPrefix,
		"token_ttl": int(gate.TokenTTL.Seconds()), "challenge_ttl": int(gate.ChallengeTTL.Seconds()),
		"bits":              map[string]int{"minimum": gate.BitsMin, "default": gate.BitsDefault, "maximum": gate.BitsMax},
		"agent_deadline_ms": gate.AgentDeadlineMillis, "public_key_url": "/api/notary/key", "docs": web.GatePath}
	if g, ok := s.service.(interface {
		GateReady() bool
		GatePending() int
		NotaryPublicKey() (string, string)
	}); ok && g.GateReady() {
		pub, id := g.NotaryPublicKey()
		out["ok"], out["public_key"], out["key_id"], out["pending"] = true, pub, id, g.GatePending()
	}
	// C348: what raising standing from a gate earns, for the ways a refused
	// visitor sees.
	if g, ok := s.service.(interface {
		GateStanding(context.Context) map[string]any
	}); ok {
		if standing := g.GateStanding(ctx); standing != nil {
			out["standing"] = standing
		}
	}
	return out
}

// gateChallengeCommand is gate.challenge from a request: the body as its
// data (execute binds the origin, bindGateChallenge).
func (s *Server) gateChallengeCommand(r *http.Request, raw []byte) (board.Command, error) {
	var fields map[string]json.RawMessage
	if err := strictJSON(raw, &fields); err != nil {
		return board.Command{}, err
	}
	return board.Command{Operation: "gate.challenge", Data: string(raw)}, nil
}

// bindGateChallenge binds a gate.challenge's origin to the page that asks
// (see above), on whichever HTTP route it came: a page on another site gets
// a challenge for its own origin only, and an opaque origin gets none.
func (s *Server) bindGateChallenge(r *http.Request, c *board.Command) error {
	header := r.Header.Get("Origin")
	if header == "" || s.ownOrigin(r, header) {
		return nil
	}
	if header == "null" {
		return &board.Error{Status: 403, Code: "invalid_origin", Message: "This page has no origin of its own (a sandboxed frame or a local file), so it gets no gate challenge. Ask from the site's page, or from its server."}
	}
	refused := &board.Error{Status: 403, Code: "invalid_origin", Message: "A page may ask for a challenge only for its own origin: leave origin out, or name " + header + "."}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(c.Data), &fields) != nil || fields == nil {
		return nil // not an object: gate.challenge refuses it as data
	}
	for name := range fields {
		// The data's decoder matches field names in any case: a second
		// spelling of origin could name another site.
		if name != "origin" && strings.EqualFold(name, "origin") {
			return refused
		}
	}
	if named, ok := fields["origin"]; ok {
		var origin string
		if json.Unmarshal(named, &origin) != nil || origin != header {
			return refused
		}
		return nil
	}
	if c.PublicKey != "" {
		return refused // signed data cannot take the header's origin: name it
	}
	fields["origin"], _ = json.Marshal(header)
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	c.Data = string(data)
	return nil
}

// gateVerifyCommand is gate.verify from a request: a signed command (it
// has an operation) as it came, or the body as unsigned data.
func gateVerifyCommand(raw []byte) (board.Command, error) {
	trimmed := bytes.TrimSpace(raw)
	var probe struct {
		Operation *string `json:"operation"`
	}
	if json.Unmarshal(trimmed, &probe) == nil && probe.Operation != nil {
		var c board.Command
		if err := strictJSON(trimmed, &c); err != nil {
			return board.Command{}, err
		}
		if c.Operation != "gate.verify" {
			return board.Command{}, &board.Error{Status: 400, Code: "invalid_request", Message: "/v1/gate/verify takes gate.verify's data, or a signed gate.verify command."}
		}
		return c, nil
	}
	// Anything else is data, which gate.verify reads strictly.
	return board.Command{Operation: "gate.verify", Data: string(trimmed)}, nil
}
