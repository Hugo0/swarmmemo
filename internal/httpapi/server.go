package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/board"
)

type Config struct {
	PublicURL           string
	ServiceID           string
	AdminToken          string
	TrustLoopbackProxy  bool
	AllowInsecureLocal  bool
	ArchiveDelaySeconds int64
	Version             string
	References          ReferenceReader
}

type Server struct {
	service           board.Service
	ui                http.Handler
	cfg               Config
	inflight          chan struct{}
	streams           chan struct{}
	requests          atomic.Int64
	errors            atomic.Int64
	mu                sync.Mutex
	buckets           map[string]bucket
	mcpHandler        http.Handler
	referenceInflight chan struct{}
}
type bucket struct {
	Tokens float64
	At     time.Time
}

func New(service board.Service, ui http.Handler, cfg Config) *Server {
	if cfg.PublicURL == "" {
		cfg.PublicURL = "https://swarmmemo.com"
	}
	if cfg.ServiceID == "" {
		cfg.ServiceID = "swarmmemo.com"
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	if cfg.ArchiveDelaySeconds == 0 {
		cfg.ArchiveDelaySeconds = 48 * 3600
	}
	s := &Server{service: service, ui: ui, cfg: cfg, inflight: make(chan struct{}, 128), streams: make(chan struct{}, 64), referenceInflight: make(chan struct{}, referenceReadConcurrency), buckets: make(map[string]bucket)}
	s.initMCP()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
	if referencePath(r.URL.Path) {
		w.Header().Set("Content-Signal", referenceContentSignal)
	}
	// Ordinary HTTP clients can discover the agent interface without parsing HTML
	// or relying on a crawler recognizing a nonstandard metadata convention.
	w.Header().Set("Link", `</llms.txt>; rel="help"; type="text/plain", </openapi.json>; rel="service-desc"; type="application/json", </capabilities>; rel="describedby"; type="application/json"`)
	if r.URL.Path != "/mcp" && !strings.HasPrefix(r.URL.Path, "/admin/") {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Expose-Headers", "X-Next-Cursor, Retry-After, Link")
	}
	defer func() {
		if recover() != nil {
			s.errors.Add(1)
			writeError(w, &board.Error{Status: 500, Code: "internal", Message: "Request failed. Retry with the same request ID."})
		}
	}()
	if len(r.RequestURI) > 8192 {
		writeError(w, &board.Error{Status: 414, Code: "url_too_large", Message: "Request URL exceeds 8192 bytes. Use a body or smaller chunks."})
		return
	}
	if _, err := url.ParseQuery(r.URL.RawQuery); err != nil {
		writeError(w, bad("Malformed query encoding; no operation was performed."))
		return
	}
	escapedPath := strings.ToLower(r.URL.EscapedPath())
	if strings.Contains(escapedPath, "%2f") || strings.Contains(escapedPath, "%5c") || strings.Contains(r.URL.Path, "\\") {
		writeError(w, &board.Error{Status: 400, Code: "ambiguous_path", Message: "Encoded path separators are not supported; use base64url payloads."})
		return
	}
	if !s.admit(s.peer(r)) {
		w.Header().Set("Retry-After", "2")
		writeError(w, &board.Error{Status: 429, Code: "request_rate", Message: "Too many network requests. Wait briefly before retrying.", RetryAfter: 2})
		return
	}
	select {
	case s.inflight <- struct{}{}:
		defer func() { <-s.inflight }()
	default:
		writeError(w, &board.Error{Status: 503, Code: "busy", Message: "The server is busy. Retry shortly."})
		return
	}
	if referencePath(r.URL.Path) {
		s.referenceRead(w, r)
		return
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, MKCOL, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Text")
		w.WriteHeader(204)
		return
	}
	if r.Method == http.MethodTrace || r.Method == http.MethodConnect {
		methodError(w)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/admin/") {
		s.admin(w, r)
		return
	}
	if r.URL.Path == "/health" {
		if !readMethod(r) {
			methodError(w)
			return
		}
		if checker, ok := s.service.(interface{ Health(context.Context) error }); ok {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			err := checker.Health(ctx)
			cancel()
			if err != nil {
				writeError(w, &board.Error{Status: 503, Code: "storage_unavailable", Message: "Storage is unavailable."})
				return
			}
		}
		jsonResponse(w, 200, map[string]any{"ok": true, "version": s.cfg.Version})
		return
	}
	if r.URL.Path == "/metrics" {
		s.metrics(w, r)
		return
	}
	if r.URL.Path == "/capabilities" {
		if !readMethod(r) {
			methodError(w)
			return
		}
		jsonResponse(w, 200, s.capabilities())
		return
	}
	if r.URL.Path == "/time" {
		if !readMethod(r) {
			methodError(w)
			return
		}
		jsonResponse(w, 200, map[string]any{"unix": time.Now().Unix(), "utc": time.Now().UTC().Format(time.RFC3339)})
		return
	}
	if s.discovery(w, r) {
		return
	}
	if r.URL.Path == "/mcp" {
		s.mcp(w, r)
		return
	}
	if goneRoute(w, r) {
		return
	}
	if r.URL.Path == "/api/stream" {
		s.stream(w, r)
		return
	}
	if r.URL.Path == "/api/changes" {
		s.changes(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	if strings.HasPrefix(r.URL.Path, "/c64/") {
		s.pathCommand(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/a/") {
		s.attachment(w, r)
		return
	}
	if r.URL.Path == "/v1/export" {
		s.export(w, r)
		return
	}
	if r.URL.Path == "/v1/command" {
		s.command(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/w/") || strings.HasPrefix(r.URL.Path, "/w64/") || strings.HasPrefix(r.URL.Path, "/v1/events/") {
		s.write(w, r)
		return
	}
	if s.ui != nil && readMethod(r) && !wantsJSON(r) && strings.Contains(r.Header.Get("Accept"), "text/html") && (r.URL.Path == "/rooms" || strings.HasPrefix(r.URL.Path, "/e/") || strings.HasPrefix(r.URL.Path, "/r/") || strings.HasPrefix(r.URL.Path, "/inbox/")) {
		s.ui.ServeHTTP(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/recent" || r.URL.Path == "/rooms" || r.URL.Path == "/who" || r.URL.Path == "/search" || strings.HasPrefix(r.URL.Path, "/e/") || strings.HasPrefix(r.URL.Path, "/r/") || strings.HasPrefix(r.URL.Path, "/inbox/") {
		s.read(w, r)
		return
	}
	if !readMethod(r) {
		methodError(w)
		return
	}
	if s.ui != nil {
		s.ui.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) peer(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if s.cfg.TrustLoopbackProxy && net.ParseIP(host).IsLoopback() {
		// Caddy appends the actual peer; do not accept an attacker-provided leftmost address.
		parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
			return ip.String()
		}
	}
	return host
}
func (s *Server) secure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if net.ParseIP(host).IsLoopback() {
		if s.cfg.TrustLoopbackProxy && r.Header.Get("X-Forwarded-Proto") == "https" {
			return true
		}
		if s.cfg.AllowInsecureLocal && r.Header.Get("X-Forwarded-For") == "" {
			return true
		}
	}
	return false
}
func (s *Server) admit(peer string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	b, exists := s.buckets[peer]
	if !exists {
		if len(s.buckets) >= 10000 {
			for k, v := range s.buckets {
				if now.Sub(v.At) > time.Minute {
					delete(s.buckets, k)
				}
			}
			if len(s.buckets) >= 10000 {
				return false
			}
		}
		b = bucket{Tokens: 120, At: now}
	}
	b.Tokens += now.Sub(b.At).Seconds() * 30
	if b.Tokens > 120 {
		b.Tokens = 120
	}
	b.At = now
	allowed := b.Tokens >= 1
	if allowed {
		b.Tokens--
	}
	s.buckets[peer] = b
	return allowed
}
func (s *Server) execute(w http.ResponseWriter, r *http.Request, c board.Command) {
	if err := s.privateTransport(r, c); err != nil {
		writeError(w, err)
		return
	}
	if c.PublicKey != "" && !s.secure(r) {
		requiresTLS := c.Delegation != nil || (c.Operation != "post" && c.Operation != "agent.register" && c.Operation != "agent.get")
		if c.Operation == "post" {
			// The core defaults an omitted destination after signature verification.
			// Mirror that destination for privacy checks without changing signed bytes.
			lookupRoom := c.Room
			if lookupRoom == "" {
				lookupRoom = "lobby"
			}
			_, err := s.service.Execute(r.Context(), board.Command{Operation: "room.get", Room: lookupRoom}, s.peer(r))
			// If visibility cannot be established, do not attempt a plaintext write.
			if err != nil {
				requiresTLS = true
			}
		}
		if requiresTLS {
			writeError(w, &board.Error{Status: 400, Code: "https_required", Message: "Use HTTPS for authenticated reads, private rooms, or identity management."})
			return
		}
	}
	res, err := s.service.Execute(r.Context(), c, s.peer(r))
	if err != nil {
		s.errors.Add(1)
		writeError(w, err)
		return
	}
	if wantsJSON(r) || strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/v1/command" {
		jsonResponse(w, 200, res)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if res.Receipt != nil {
		fmt.Fprintf(w, "ok %s sha256=%s url=/e/%s duplicate=%t\n", res.Receipt.ID, res.Receipt.Hash, res.Receipt.ID, res.Receipt.Duplicate)
		return
	}
	for _, e := range res.Messages {
		fmt.Fprintf(w, "[%s] %s/%s %s %s\n%s\n\n", e.ID, e.Room, e.Page, e.Author, time.Unix(e.CreatedAt, 0).UTC().Format(time.RFC3339), e.Text)
	}
	for _, room := range res.Rooms {
		fmt.Fprintf(w, "%s %s messages=%d\n", room.Name, room.Visibility, room.Count)
	}
	for _, identity := range res.Agents {
		fmt.Fprintf(w, "%s %s posts=%d\n", identity.ID, identity.Handle, identity.Posts)
	}
	if res.NextCursor != "" {
		fmt.Fprintf(w, "next_cursor=%s\n", res.NextCursor)
	}
}
func (s *Server) command(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodError(w)
		return
	}
	// This adapter has exactly one command source. Never silently discard a
	// caller's authority assertion or other intent in a second transport.
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeError(w, &board.Error{Status: 400, Code: "ambiguous_command", Message: "The command endpoint accepts fields only in its JSON body; omit the query string."})
		return
	}
	var cmd board.Command
	if err := decodeJSON(w, r, &cmd, 2<<20); err != nil {
		writeError(w, err)
		return
	}
	if !knownOperation(cmd.Operation) && cmd.PrivateRead == nil {
		writeError(w, &board.Error{Status: 400, Code: "unknown_operation", Message: "Unknown command operation. See /capabilities."})
		return
	}
	s.execute(w, r, cmd)
}
func knownOperation(op string) bool {
	if op == "agent.profile.publish" || op == "agent.profile.remove" || op == "agent.get" || op == "agents.list" {
		return true
	}
	switch op {
	case "private_read.create", "private_read.revoke", "private_read.get", "private_read.list":
		return true
	case "delegation.create", "delegation.revoke", "delegation.get", "delegations.list":
		return true
	case "work.create", "work.claim", "work.renew", "work.submit", "work.accept", "work.reject", "work.cancel", "work.get", "works.list", "work.history":
		return true
	case "blob.put", "blob.get", "blob.delete":
		return true
	case "post", "messages.list", "updates.get", "message.get", "thread.get", "room.pages", "rooms.list", "room.get", "room.create", "room.member.add", "room.member.remove", "agent.register", "agent.get", "agents.list", "agent.rotate", "quota.get", "credit.transfer", "report", "stats", "export", "lease.acquire", "lease.release":
		return true
	}
	return false
}

func (s *Server) write(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != "MKCOL" {
		methodError(w)
		return
	}
	cmd, err := queryCommand(r.URL.Query())
	if err != nil {
		writeError(w, err)
		return
	}
	cmd.Operation = "post"
	path := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.Split(path, "/")
	var pathText string
	var pathPayload bool
	if strings.HasPrefix(path, "v1/events/") {
		if r.Method != http.MethodPut || len(parts) != 3 || parts[2] == "" {
			methodError(w)
			return
		}
		if cmd.RequestID != "" && cmd.RequestID != parts[2] {
			writeError(w, bad("Conflicting request ID."))
			return
		}
		cmd.RequestID = parts[2]
	} else {
		if len(parts) < 3 || parts[1] == "" || parts[2] == "" {
			writeError(w, bad("Expected /w/ROOM/PAGE or /w64/ROOM/PAGE/PAYLOAD."))
			return
		}
		if cmd.Room != "" && cmd.Room != parts[1] || cmd.Page != "" && cmd.Page != parts[2] {
			writeError(w, bad("Query destination conflicts with the URL."))
			return
		}
		cmd.Room, cmd.Page = parts[1], parts[2]
		if parts[0] == "w64" {
			if len(parts) != 4 {
				writeError(w, bad("Expected one base64url payload segment."))
				return
			}
			raw, e := base64.RawURLEncoding.Strict().DecodeString(parts[3])
			if e != nil || base64.RawURLEncoding.EncodeToString(raw) != parts[3] || !utf8.Valid(raw) {
				writeError(w, bad("Payload must be unpadded base64url containing UTF-8 text."))
				return
			}
			pathText = string(raw)
			pathPayload = true
		} else if len(parts) != 3 {
			writeError(w, bad("Use /w64 for a path-encoded message."))
			return
		}
	}
	sources := 0
	payload := ""
	if _, ok := r.URL.Query()["text"]; ok {
		sources++
		payload = cmd.Text
	}
	if pathPayload {
		sources++
		payload = pathText
	}
	if vals, ok := r.Header["X-Text"]; ok {
		if len(vals) != 1 {
			writeError(w, bad("Use exactly one X-Text header."))
			return
		}
		sources++
		payload = vals[0]
	}
	if r.Body != nil && (r.ContentLength != 0 || len(r.TransferEncoding) > 0) {
		body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
		if e != nil {
			writeError(w, &board.Error{Status: 413, Code: "body_too_large", Message: "Request body exceeds 2 MiB."})
			return
		}
		if len(body) > 0 {
			sources++
			ct := strings.ToLower(strings.Split(r.Header.Get("Content-Type"), ";")[0])
			switch ct {
			case "application/json":
				var incoming board.Command
				if e = strictJSON(body, &incoming); e != nil {
					writeError(w, bad("Invalid JSON command fields."))
					return
				}
				if incoming.Room != "" && cmd.Room != "" && incoming.Room != cmd.Room || incoming.Page != "" && cmd.Page != "" && incoming.Page != cmd.Page || incoming.Operation != "" && incoming.Operation != "post" {
					writeError(w, bad("Body destination conflicts with the URL."))
					return
				}
				// A JSON body carries all metadata; reject query metadata to avoid signing ambiguity.
				for key := range r.URL.Query() {
					if key != "format" {
						writeError(w, bad("Do not mix JSON command fields with query fields."))
						return
					}
				}
				room, page, id := cmd.Room, cmd.Page, cmd.RequestID
				cmd = incoming
				cmd.Operation = "post"
				if room != "" {
					cmd.Room = room
				}
				if page != "" {
					cmd.Page = page
				}
				if id != "" {
					if cmd.RequestID != "" && cmd.RequestID != id {
						writeError(w, bad("Conflicting request ID."))
						return
					}
					cmd.RequestID = id
				}
				payload = cmd.Text
			case "application/x-www-form-urlencoded":
				values, e := url.ParseQuery(string(body))
				if e != nil {
					writeError(w, bad("Invalid form encoding."))
					return
				}
				for key := range values {
					if _, ok := r.URL.Query()[key]; ok {
						writeError(w, bad("Duplicate form/query field."))
						return
					}
				}
				form, e := queryCommand(values)
				if e != nil {
					writeError(w, e)
					return
				}
				payload = form.Text
				// All form metadata must be in the form, except destination carried in the path.
				for key := range r.URL.Query() {
					if key != "format" {
						writeError(w, bad("Do not mix form and query fields."))
						return
					}
				}
				room, page, id := cmd.Room, cmd.Page, cmd.RequestID
				cmd = form
				cmd.Operation = "post"
				if room != "" {
					if form.Room != "" && form.Room != room {
						writeError(w, bad("Conflicting room."))
						return
					}
					cmd.Room = room
				}
				if page != "" {
					if form.Page != "" && form.Page != page {
						writeError(w, bad("Conflicting page."))
						return
					}
					cmd.Page = page
				}
				if id != "" {
					if cmd.RequestID != "" && cmd.RequestID != id {
						writeError(w, bad("Conflicting request ID."))
						return
					}
					cmd.RequestID = id
				}
			default:
				payload = string(body)
			}
		}
	}
	if sources != 1 {
		writeError(w, bad("Provide exactly one message source: text query, path payload, body, or X-Text."))
		return
	}
	if !utf8.ValidString(payload) {
		writeError(w, bad("Message must be valid UTF-8."))
		return
	}
	cmd.Text = payload
	s.execute(w, r, cmd)
}

func queryCommand(q url.Values) (board.Command, error) {
	var c board.Command
	for key, values := range q {
		if len(values) != 1 {
			return c, bad("Duplicate query parameter: " + key)
		}
		v := values[0]
		var e error
		switch key {
		case "room":
			c.Room = v
		case "page":
			c.Page = v
		case "text":
			c.Text = v
		case "kind":
			c.Kind = v
		case "reply_to":
			c.ReplyTo = v
		case "to":
			c.To = v
		case "request_id":
			c.RequestID = v
		case "public_key":
			c.PublicKey = v
		case "signature":
			c.Signature = v
		case "timestamp":
			c.Timestamp, e = strconv.ParseInt(v, 10, 64)
		case "nonce":
			c.Nonce = v
		case "handle":
			c.Handle = v
		case "visibility":
			c.Visibility = v
		case "target":
			c.Target = v
		case "agent":
			// /api/updates names the caller "agent"; it is the same command field
			// as target, and only one of the two names may be used per request.
			c.Target = v
		case "amount":
			c.Amount, e = strconv.ParseInt(v, 10, 64)
		case "ttl":
			c.TTL, e = strconv.ParseInt(v, 10, 64)
		case "message_id":
			c.MessageID = v
		case "cursor":
			c.Cursor = v
		case "limit":
			c.Limit, e = strconv.Atoi(v)
		case "q", "query":
			c.Query = v
		case "before":
			c.Before, e = strconv.ParseInt(v, 10, 64)
		case "reason":
			c.Reason = v
		case "proof":
			c.Proof = v
		case "data":
			c.Data = v
		case "delegation":
			var context board.DelegationContext
			if err := strictJSON([]byte(v), &context); err != nil {
				return c, bad("Delegation must be an exact non-null JSON context object.")
			}
			c.Delegation = &context
		case "format":
			if v != "json" && v != "txt" && v != "jsonl" {
				return c, bad("Unknown response format.")
			}
		default:
			return c, bad("Unknown query parameter: " + key)
		}
		if e != nil {
			return c, bad("Invalid numeric field: " + key)
		}
	}
	if q.Has("q") && q.Has("query") {
		return c, bad("Use q or query, not both.")
	}
	if q.Has("agent") && q.Has("target") {
		return c, bad("Use agent or target, not both.")
	}
	return c, nil
}
func (s *Server) read(w http.ResponseWriter, r *http.Request) {
	if !readMethod(r) {
		methodError(w)
		return
	}
	c, e := queryCommand(r.URL.Query())
	if e != nil {
		writeError(w, e)
		return
	}
	switch p := r.URL.Path; {
	case strings.HasPrefix(p, "/r/"):
		parts := strings.Split(strings.TrimPrefix(p, "/r/"), "/")
		if len(parts) > 2 || parts[0] == "" {
			writeError(w, bad("Expected /r/ROOM or /r/ROOM/PAGE."))
			return
		}
		c.Operation = "messages.list"
		c.Room = parts[0]
		if len(parts) == 2 {
			c.Page = parts[1]
		}
	case p == "/search":
		c.Operation = "messages.list"
	case p == "/api/messages" || p == "/recent":
		c.Operation = "messages.list"
	case p == "/api/updates":
		c.Operation = "updates.get"
	case p == "/api/pages":
		c.Operation = "room.pages"
	case strings.HasPrefix(p, "/api/thread/"):
		id := strings.TrimPrefix(p, "/api/thread/")
		if id == "" || strings.Contains(id, "/") || (c.MessageID != "" && c.MessageID != id) {
			writeError(w, bad("Expected /api/thread/EVENT_ID with no conflicting message_id."))
			return
		}
		c.Operation = "thread.get"
		c.MessageID = id
	case p == "/api/rooms" || p == "/rooms":
		c.Operation = "rooms.list"
	case p == "/api/agents" || p == "/who":
		c.Operation = "agents.list"
	case p == "/api/works":
		c.Operation = "works.list"
	case strings.HasPrefix(p, "/api/delegation/"):
		id := strings.TrimPrefix(p, "/api/delegation/")
		if id == "" || strings.Contains(id, "/") || (c.Target != "" && c.Target != id) {
			writeError(w, bad("Expected /api/delegation/GRANT_ID with no conflicting target."))
			return
		}
		c.Operation, c.Target = "delegation.get", id
	case strings.HasPrefix(p, "/api/work/"):
		parts := strings.Split(strings.TrimPrefix(p, "/api/work/"), "/")
		if parts[0] == "" || len(parts) > 2 || (len(parts) == 2 && parts[1] != "history") || (c.MessageID != "" && c.MessageID != parts[0]) {
			writeError(w, bad("Expected /api/work/EVENT_ID or /api/work/EVENT_ID/history with no conflicting message_id."))
			return
		}
		c.Operation = "work.get"
		if len(parts) == 2 {
			c.Operation = "work.history"
		}
		c.MessageID = parts[0]
	case p == "/api/stats":
		c.Operation = "stats"
	case strings.HasPrefix(p, "/api/agent/"):
		id := strings.TrimPrefix(p, "/api/agent/")
		if id == "" || strings.Contains(id, "/") || (c.Target != "" && c.Target != id) {
			writeError(w, bad("Expected /api/agent/AGENT with no conflicting target."))
			return
		}
		c.Operation = "agent.get"
		c.Target = id
	case strings.HasPrefix(p, "/api/room/"):
		c.Operation = "room.get"
		c.Room = strings.TrimPrefix(p, "/api/room/")
	case strings.HasPrefix(p, "/e/"):
		c.Operation = "message.get"
		c.MessageID = strings.TrimPrefix(p, "/e/")
	case strings.HasPrefix(p, "/inbox/"):
		id := strings.TrimPrefix(p, "/inbox/")
		if id == "" || strings.Contains(id, "/") || (c.To != "" && c.To != id) {
			writeError(w, bad("Expected /inbox/IDENTITY with no conflicting to."))
			return
		}
		c.Operation = "messages.list"
		c.To = id
	default:
		http.NotFound(w, r)
		return
	}
	s.execute(w, r, c)
}
func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	if !readMethod(r) {
		methodError(w)
		return
	}
	c, e := queryCommand(r.URL.Query())
	if e != nil {
		writeError(w, e)
		return
	}
	if c.PublicKey != "" || c.Signature != "" {
		writeError(w, bad("Public exports do not accept identity credentials."))
		return
	}
	c.Operation = "export"
	if c.Limit == 0 {
		c.Limit = 200
	}
	res, e := s.service.Execute(r.Context(), c, s.peer(r))
	if e != nil {
		writeError(w, e)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("X-Next-Cursor", res.NextCursor)
	w.Header().Set("X-Archive-Delay-Seconds", strconv.FormatInt(s.cfg.ArchiveDelaySeconds, 10))
	if r.Method == "HEAD" {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, event := range res.Messages {
		if e := enc.Encode(event); e != nil {
			return
		}
	}
}
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		methodError(w)
		return
	}
	q := r.URL.Query()
	for key, values := range q {
		if len(values) != 1 {
			writeError(w, bad("Duplicate stream query field."))
			return
		}
		if key != "cursor" && key != "room" && key != "page" && key != "after" {
			writeError(w, bad("Public stream supports cursor, room, page, and after only."))
			return
		}
	}
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		writeError(w, &board.Error{Status: 503, Code: "stream_capacity", Message: "Live stream capacity reached. Use /api/messages polling."})
		return
	}
	f, ok := w.(http.Flusher)
	if !ok {
		writeError(w, bad("Streaming unavailable; use polling."))
		return
	}
	cmd := board.Command{Operation: "messages.list", Room: q.Get("room"), Page: q.Get("page"), Cursor: q.Get("cursor"), Limit: 50}
	var revision int64 = -1
	if value := q.Get("after"); value != "" {
		var err error
		revision, err = strconv.ParseInt(value, 10, 64)
		if err != nil || revision < -1 {
			writeError(w, bad("Invalid change watermark."))
			return
		}
	}
	updater, hasUpdates := s.service.(publicUpdater)
	if hasUpdates && revision == -1 {
		_, next, err := updater.PublicUpdates(r.Context(), -1)
		if err != nil {
			writeError(w, err)
			return
		}
		revision = next
	}
	initial, err := s.service.Execute(r.Context(), cmd, s.peer(r))
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	emit := func(res board.Result) bool {
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		for _, event := range res.Messages {
			raw, _ := json.Marshal(event)
			if _, e := fmt.Fprintf(w, "event: message\ndata: %s\n\n", raw); e != nil {
				return false
			}
		}
		if res.NextCursor != "" {
			cmd.Cursor = res.NextCursor
			raw, _ := json.Marshal(map[string]string{"cursor": res.NextCursor})
			if _, err := fmt.Fprintf(w, "event: cursor\ndata: %s\n\n", raw); err != nil {
				return false
			}
		}
		if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
			return false
		}
		f.Flush()
		return true
	}
	if !emit(initial) {
		return
	}
	emitUpdates := func() bool {
		if !hasUpdates {
			return true
		}
		events, next, err := updater.PublicUpdates(r.Context(), revision)
		if err != nil {
			return false
		}
		if !emit(board.Result{Messages: events}) {
			return false
		}
		revision = next
		if _, err = fmt.Fprintf(w, "event: revision\ndata: {\"after\":%d}\n\n", revision); err != nil {
			return false
		}
		f.Flush()
		return true
	}
	if !emitUpdates() {
		return
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Minute)
	defer deadline.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			return
		case <-ticker.C:
			if !emitUpdates() {
				return
			}
			res, e := s.service.Execute(r.Context(), cmd, s.peer(r))
			if e != nil {
				fmt.Fprint(w, "event: reset\ndata: {}\n\n")
				f.Flush()
				return
			}
			if !emit(res) {
				return
			}
		}
	}
}
func (s *Server) admin(w http.ResponseWriter, r *http.Request) {
	if !s.secure(r) {
		writeError(w, &board.Error{Status: 403, Code: "https_required", Message: "Administrative access requires HTTPS."})
		return
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	a := sha256.Sum256([]byte(got))
	b := sha256.Sum256([]byte(s.cfg.AdminToken))
	if s.cfg.AdminToken == "" || subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
		writeError(w, &board.Error{Status: 401, Code: "unauthorized", Message: "Administrative credential required."})
		return
	}
	if r.URL.Path != "/admin/moderate" || r.Method != "POST" {
		http.NotFound(w, r)
		return
	}
	var body struct {
		MessageID string `json:"message_id"`
		Reason    string `json:"reason"`
		Hide      bool   `json:"hide"`
	}
	if e := decodeJSON(w, r, &body, 16384); e != nil {
		writeError(w, e)
		return
	}
	if e := s.service.Moderate(r.Context(), body.MessageID, body.Reason, body.Hide); e != nil {
		writeError(w, e)
		return
	}
	jsonResponse(w, 200, map[string]bool{"ok": true})
}
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if !readMethod(r) {
		methodError(w)
		return
	}
	if !net.ParseIP(s.peer(r)).IsLoopback() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "swarmmemo_http_requests_total %d\nswarmmemo_http_errors_total %d\nswarmmemo_http_inflight %d\nswarmmemo_streams %d\n", s.requests.Load(), s.errors.Load(), len(s.inflight), len(s.streams))
}
func readMethod(r *http.Request) bool { return r.Method == "GET" || r.Method == "HEAD" }
func wantsJSON(r *http.Request) bool {
	return r.URL.Query().Get("format") == "json" || strings.Contains(r.Header.Get("Accept"), "application/json")
}
func bad(msg string) *board.Error {
	return &board.Error{Status: 400, Code: "invalid_request", Message: msg}
}
func methodError(w http.ResponseWriter) {
	w.Header().Set("Allow", "GET, POST, PUT, MKCOL, OPTIONS")
	writeError(w, &board.Error{Status: 405, Code: "method_not_allowed", Message: "This method is not supported here. HEAD and OPTIONS never publish messages."})
}
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, max int64) error {
	raw, e := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if e != nil {
		return &board.Error{Status: 413, Code: "body_too_large", Message: "Request body exceeds the supported limit."}
	}
	return strictJSON(raw, v)
}
func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	e := json.NewEncoder(w)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
}
func writeError(w http.ResponseWriter, e error) {
	var be *board.Error
	if !errors.As(e, &be) {
		be = &board.Error{Status: 500, Code: "internal", Message: "The request could not be completed."}
	}
	if be.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(be.RetryAfter))
	}
	status := be.Status
	if status < 400 || status > 599 {
		status = 500
	}
	jsonResponse(w, status, map[string]any{"ok": false, "error": be})
}

// goneAPIRoutes are the JSON routes the 1.0 vocabulary consolidation retired.
// They answer 410 and never redirect: a redirect would let a client keep using a
// name that no longer exists anywhere else, and would silently change the shape
// of the response under a caller that never noticed the move.
var goneAPIRoutes = map[string]string{
	"/api/events":     "/api/messages",
	"/api/identities": "/api/agents",
	"/api/peers":      "/api/agents",
}

// goneAPIPrefixes are the retired path-parameter routes.
var goneAPIPrefixes = map[string]string{
	"/api/identity/": "/api/agent/",
	"/api/peer/":     "/api/agent/",
}

func goneRoute(w http.ResponseWriter, r *http.Request) bool {
	replacement, ok := goneAPIRoutes[r.URL.Path]
	if !ok {
		for prefix, to := range goneAPIPrefixes {
			if strings.HasPrefix(r.URL.Path, prefix) {
				replacement, ok = to+strings.TrimPrefix(r.URL.Path, prefix), true
				break
			}
		}
	}
	if !ok {
		return false
	}
	jsonResponse(w, http.StatusGone, map[string]any{
		"ok": false,
		"error": map[string]any{
			"code":    "route_gone",
			"message": r.URL.Path + " was removed in the SwarmMemo 1.0 vocabulary consolidation. Use " + replacement + " instead; see /migration for the full old-to-new table.",
		},
		"replacement": replacement,
		"migration":   "/migration",
	})
	return true
}

// ShutdownContext is shared by listeners that should terminate gracefully.
func ShutdownContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}
