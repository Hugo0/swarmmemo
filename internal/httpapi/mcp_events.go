package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// MCP Events (board/mcpevents.go) on the hosted MCP endpoints. The SDK serves
// protocol 2026-07-28 itself (server/discover, per-request _meta, the
// Mcp-Method header) beside the legacy initialize handshake, but has no
// events methods, so this layer answers events/list, events/subscribe and
// events/unsubscribe (and Smithery's ai.smithery/events aliases) before the
// SDK sees them, and adds the events capability to its server/discover
// answer. A 2025-06-18 client sees exactly what it saw before.

// eventsStore is what MCP Events need from the board (board.Store).
type eventsStore interface {
	MCPEventPrincipal(ctx context.Context, token string, now int64) (board.MCPPrincipal, error)
	SubscribeMCPEvent(ctx context.Context, p board.MCPPrincipal, r board.MCPEventRequest) (map[string]any, error)
	UnsubscribeMCPEvent(ctx context.Context, p board.MCPPrincipal, r board.MCPEventRequest) (map[string]any, error)
}

// eventsStore is the board's MCP Events, or nil while the outbound sender is
// off (nothing would ever be delivered) or the board has none.
func (s *Server) eventsStore() eventsStore {
	if !s.cfg.PushDelivery {
		return nil
	}
	e, _ := s.service.(eventsStore)
	return e
}

// mcpVersions are the protocol versions the SDK serves, newest first, as its
// server/discover lists them (TestMCPEventsDiscover holds them equal).
var mcpVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// The modern protocol's error codes (2026-07-28 basic/index#error-codes).
const (
	rpcHeaderMismatch      = -32020
	rpcUnsupportedProtocol = -32022
)

// smitheryEvents is the Smithery triggers extension's identifier; its
// methods are the events methods under this prefix.
const smitheryEvents = "ai.smithery/events"

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// eventsMethod is the events method a JSON-RPC method names ("list",
// "subscribe", "unsubscribe") and whether it came through Smithery's alias.
func eventsMethod(method string) (name string, smithery, known bool) {
	if rest, ok := strings.CutPrefix(method, smitheryEvents+"/"); ok {
		method, smithery = "events/"+rest, true
	}
	switch method {
	case "events/list", "events/subscribe", "events/unsubscribe":
		return strings.TrimPrefix(method, "events/"), smithery, true
	}
	return "", false, false
}

// peekRPC reads a POST body that fits the SDK's limit and puts it back. A
// body over the limit, a batch or anything unparsable goes to the SDK as it
// came, for its own answer.
func peekRPC(r *http.Request) (rpcEnvelope, bool) {
	var env rpcEnvelope
	if r.Body == nil {
		return env, false
	}
	head, err := io.ReadAll(io.LimitReader(r.Body, board.CommandBodyBytes+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
	if err != nil || len(head) > board.CommandBodyBytes {
		return env, false
	}
	trimmed := bytes.TrimSpace(head)
	if len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(trimmed, &env) != nil {
		return env, false
	}
	return env, true
}

// mcpEventsIntercept answers the events methods and wraps the writer of a
// server/discover or initialize answer to add the events capability. It
// reports whether it answered; finish, when not nil, completes the wrapped
// answer after the SDK wrote it.
func (s *Server) mcpEventsIntercept(w http.ResponseWriter, r *http.Request, token string) (answered bool, out http.ResponseWriter, finish func()) {
	if r.Method != http.MethodPost || s.eventsStore() == nil {
		return false, w, nil
	}
	env, ok := peekRPC(r)
	if !ok {
		return false, w, nil
	}
	if name, smithery, known := eventsMethod(env.Method); known {
		s.serveEvents(w, r, env, name, smithery, token)
		return true, w, nil
	}
	switch env.Method {
	case "server/discover":
		b := &bufferedWriter{header: http.Header{}}
		return false, b, func() { b.finish(w, addEventsCapability(true)) }
	case "initialize":
		// A legacy client learns of Smithery's extension (its documented
		// discovery); a 2025-06-18 or older one sees no change at all.
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(env.Params, &p) == nil && p.ProtocolVersion >= "2025-11-25" {
			b := &bufferedWriter{header: http.Header{}}
			return false, b, func() { b.finish(w, addEventsCapability(false)) }
		}
	}
	return false, w, nil
}

// addEventsCapability adds events (modern) and the Smithery extension to a
// result's capabilities.
func addEventsCapability(modern bool) func(result map[string]any) {
	return func(result map[string]any) {
		caps, ok := result["capabilities"].(map[string]any)
		if !ok {
			return
		}
		if modern {
			caps["events"] = map[string]any{}
		}
		extensions, _ := caps["extensions"].(map[string]any)
		if extensions == nil {
			extensions = map[string]any{}
		}
		extensions[smitheryEvents] = map[string]any{}
		caps["extensions"] = extensions
	}
}

// bufferedWriter holds an SDK answer so a capability can be added to it.
type bufferedWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedWriter) Header() http.Header         { return b.header }
func (b *bufferedWriter) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *bufferedWriter) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}
func (b *bufferedWriter) Flush() {}

// finish writes the held answer to w, its JSON-RPC result passed through
// edit when it is one. Anything else (an error, an SSE stream) goes as it was.
func (b *bufferedWriter) finish(w http.ResponseWriter, edit func(map[string]any)) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	body := b.body.Bytes()
	if b.status == http.StatusOK && strings.HasPrefix(b.header.Get("Content-Type"), "application/json") {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		var msg map[string]any
		if decoder.Decode(&msg) == nil {
			if result, ok := msg["result"].(map[string]any); ok {
				edit(result)
				if edited, err := json.Marshal(msg); err == nil {
					body = append(edited, '\n')
				}
			}
		}
	}
	for k, v := range b.header {
		w.Header()[k] = v
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(b.status)
	_, _ = w.Write(body)
}

// serveEvents answers one events method. A modern request (2026-07-28 _meta
// or header) is held to the transport's rules: the MCP-Protocol-Version and
// Mcp-Method headers must match the body (400 HeaderMismatch), and its
// version must be one the extension is defined at (400
// UnsupportedProtocolVersion). A legacy-era request without _meta is served
// too, as Smithery's are.
func (s *Server) serveEvents(w http.ResponseWriter, r *http.Request, env rpcEnvelope, name string, smithery bool, token string) {
	if len(env.ID) == 0 || string(env.ID) == "null" {
		w.WriteHeader(http.StatusAccepted) // a notification: nothing to answer, nothing done
		return
	}
	reply := func(field string, status int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": env.ID, field: value})
	}
	var params struct {
		Meta map[string]any `json:"_meta"`
	}
	if len(env.Params) > 0 && json.Unmarshal(env.Params, &params) != nil {
		reply("error", http.StatusOK, &board.EventError{Code: board.EventInvalidParams, Message: "params must be an object."})
		return
	}
	version, _ := params.Meta["io.modelcontextprotocol/protocolVersion"].(string)
	header := r.Header.Get("MCP-Protocol-Version")
	modern := version != "" || header >= board.MCPEventsVersion
	if modern {
		if header != version || r.Header.Get("Mcp-Method") != env.Method {
			reply("error", http.StatusBadRequest, map[string]any{"code": rpcHeaderMismatch, "message": fmt.Sprintf("Header mismatch: MCP-Protocol-Version %q and Mcp-Method %q must match the body's protocol version %q and method %q.", header, r.Header.Get("Mcp-Method"), version, env.Method)})
			return
		}
		if version != board.MCPEventsVersion {
			reply("error", http.StatusBadRequest, map[string]any{"code": rpcUnsupportedProtocol, "message": "Unsupported protocol version", "data": map[string]any{"supported": mcpVersions, "requested": version}})
			return
		}
	}
	result, err := s.eventsCall(r.Context(), name, smithery, env.Params, token)
	if err != nil {
		reply("error", http.StatusOK, eventsRPCError(err))
		return
	}
	if modern {
		result["resultType"] = "complete"
		result["_meta"] = map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "swarmmemo", "version": s.cfg.Version}}
	}
	reply("result", http.StatusOK, result)
}

func (s *Server) eventsCall(ctx context.Context, name string, smithery bool, params json.RawMessage, token string) (map[string]any, error) {
	if name == "list" {
		return map[string]any{"events": board.MCPEventCatalog()}, nil
	}
	p, err := s.eventsPrincipal(ctx, token)
	if err != nil {
		return nil, err
	}
	req, err := board.ParseMCPEventRequest(params, smithery, name == "unsubscribe")
	if err != nil {
		return nil, err
	}
	if name == "unsubscribe" {
		return s.eventsStore().UnsubscribeMCPEvent(ctx, p, req)
	}
	return s.eventsStore().SubscribeMCPEvent(ctx, p, req)
}

// eventsPrincipal is the identity a subscription belongs to: the hosted
// identity the connection's token (path, bearer or OAuth) names. The spec
// requires an authenticated principal for subscribe and unsubscribe, so an
// anonymous connection may list events but subscribe to none, public ones
// included. Calls count against the token's tool-call rate.
func (s *Server) eventsPrincipal(ctx context.Context, token string) (board.MCPPrincipal, error) {
	if token == "" || s.hostedStore() == nil {
		return board.MCPPrincipal{}, &board.EventError{Code: board.EventForbidden, Message: "Subscribing acts for an identity: sign in to SwarmMemo from your app (OAuth), or call create_identity and reconnect with its mcp_url. An anonymous connection can list events, not subscribe."}
	}
	p, err := s.eventsStore().MCPEventPrincipal(ctx, token, time.Now().Unix())
	if err != nil {
		return board.MCPPrincipal{}, &board.EventError{Code: board.EventForbidden, Message: apiError(err).Message}
	}
	sum := sha256.Sum256([]byte(token))
	if !s.hostedLimiter.Admit("hosted:" + hex.EncodeToString(sum[:16])) {
		return board.MCPPrincipal{}, &board.EventError{Code: board.EventResourceExhausted, Message: fmt.Sprintf("This hosted identity makes up to %d calls a minute; wait briefly before retrying.", hostedRatePerMinute), Data: map[string]any{"limit": "request_rate"}}
	}
	return p, nil
}

// mcpEventsCapabilities is /capabilities mcp_events: the MCP methods'
// contract, at parity with events/list and the docs.
func (s *Server) mcpEventsCapabilities() map[string]any {
	return map[string]any{
		"enabled": s.eventsStore() != nil, "protocol_version": board.MCPEventsVersion, "endpoints": []string{"/mcp", web.AssistantMCPPath},
		"methods": []string{"events/list", "events/subscribe", "events/unsubscribe"},
		"aliases": []string{smitheryEvents + "/list", smitheryEvents + "/subscribe", smitheryEvents + "/unsubscribe"},
		"events":  board.MCPEventNames(), "delivery": []string{"webhook"},
		"identity":       "subscribe and unsubscribe act for the connection's hosted identity (OAuth sign-in, or a token in the URL or a bearer header); an anonymous connection can call events/list only",
		"verification":   "a signed POST {\"type\":\"verification\",\"challenge\":NONCE} that must answer 2xx with {\"challenge\":NONCE} before the subscription is active",
		"signature":      "Standard Webhooks: webhook-id, webhook-timestamp and webhook-signature v1,base64(HMAC-SHA256(whsec_ secret bytes, id.timestamp.body)); X-MCP-Subscription-Id names the subscription",
		"payload":        "event, ids, room, author fingerprint, handle, signed and trust tier, links; a public post's excerpt only after Jev screening allowed it; never a private body, never a hidden post; untrusted: true",
		"callback":       "https on port 443 at a public address, re-checked on every connection; redirects never followed",
		"max_body_bytes": board.MCPEventMaxBodyBytes, "excerpt_chars": board.MCPEventExcerptChars, "ttl_seconds_max": board.MCPEventTTLSeconds,
		"no_expiry": false, "replay": false, "maximum_subscriptions": board.MCPEventMaxPerAccount, "maximum_retained": board.MCPEventMaxRetained,
		"fanout_per_event": board.MCPEventMaxFanout, "maximum_deliveries_per_hour": board.WebhookMaxDeliveriesHour, "maximum_attempts": board.WebhookMaxAttempts,
		"disable_after_consecutive_failures": board.WebhookDisableFailures, "verifications_per_hour": board.MCPEventVerificationsPerHour,
		"list_and_cancel": []string{"list_event_subscriptions", "cancel_event_subscription", "webhook.list", "webhook.delete"},
		"instructions":    "/protocol.md#mcp-events",
	}
}

// eventsRPCError is the JSON-RPC error for err: an EventError as it is, and
// anything else an internal error that says nothing about the server.
func eventsRPCError(err error) any {
	var e *board.EventError
	if errors.As(err, &e) {
		return e
	}
	return map[string]any{"code": -32603, "message": "Internal error; try again."}
}
