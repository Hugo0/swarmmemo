package httpapi

// The transparency log's public routes (board/transparency.go): signed
// checkpoints, inclusion and consistency proofs, the leaves, OpenTimestamps
// anchors and the signed per-agent record. All are anonymous GETs, cacheable;
// a response pinned to a checkpoint size never changes, but for the text a
// message proof carries, which a hide withdraws, and a proof's anchor until
// it is confirmed.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"swarmmemo/internal/board"
)

// transparencyReader is the store side of the routes.
type transparencyReader interface {
	ReadLogCheckpoint(ctx context.Context, size int64) (board.LogCheckpoint, error)
	ReadLogProof(ctx context.Context, index int64, message string, size int64) (board.LogInclusion, error)
	ReadNotaryProof(ctx context.Context, hash string, size int64) (board.LogInclusion, error)
	ReadLogConsistency(ctx context.Context, from, to int64) (board.LogConsistency, error)
	ReadLogLeaves(ctx context.Context, start, end int64) ([]board.LogEntry, int64, error)
	ReadLogAnchors(ctx context.Context, before int64, limit int) ([]board.LogAnchor, error)
	ReadLogAnchorFile(ctx context.Context, size int64) ([]byte, error)
	ReadLogRecord(ctx context.Context, who string) (board.SignedRecord, error)
	ReadLogPromise(ctx context.Context, message string, index int64) (board.LogPromiseStatus, error)
	LogVerifierKey() string
}

// LogPaths are the routes, for /capabilities and the docs.
var LogPaths = map[string]string{
	"checkpoint":  "/api/log/checkpoint",
	"note":        "/api/log/checkpoint/note",
	"proof":       "/api/log/proof?message=ID",
	"notary":      "/api/log/proof?notary=HASH",
	"promise":     board.LogPromisePath + "?message=ID",
	"consistency": "/api/log/consistency?from=SIZE&to=SIZE",
	"leaves":      "/api/log/leaves?start=0&end=256",
	"anchors":     "/api/log/anchors",
	"record":      "/api/record/HANDLE_OR_FINGERPRINT",
	"verifier":    "/clients/python/verify_log.py",
	"page":        "/verify",
}

// logQuery parses the named non-negative integers; absent ones are -1.
func logQuery(r *http.Request, names ...string) (map[string]int64, error) {
	q := r.URL.Query()
	out := map[string]int64{}
	for name := range q {
		if name != "format" && !strings.Contains(" "+strings.Join(names, " ")+" ", " "+name+" ") {
			return nil, bad("Unknown query parameter " + strconv.Quote(name) + "; expected " + strings.Join(names, ", ") + ".")
		}
	}
	for _, name := range names {
		out[name] = -1
		if v := q.Get(name); v != "" && name != "message" && name != "notary" && name != "format" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 || strconv.FormatInt(n, 10) != v {
				return nil, bad(name + " must be a non-negative integer.")
			}
			out[name] = n
		}
	}
	return out, nil
}

// cacheFor marks a public response cacheable: briefly while it can change,
// for a day once pinned to a checkpoint.
func cacheFor(w http.ResponseWriter, pinned bool) {
	if pinned {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=60")
	}
}

// transparencyRoute serves /api/log/... and /api/record/...; false leaves
// the request to other routes.
func (s *Server) transparencyRoute(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	if !strings.HasPrefix(p, "/api/log/") && !strings.HasPrefix(p, "/api/record/") {
		return false
	}
	store, ok := s.service.(transparencyReader)
	if !ok {
		writeError(w, &board.Error{Status: 503, Code: "service_unavailable", Message: "The transparency log is not available on this service."})
		return true
	}
	if !readMethod(r) {
		methodError(w)
		return true
	}
	ctx := r.Context()
	fail := func(err error) bool { writeError(w, err); return true }
	switch {
	case p == "/api/log/checkpoint" || p == "/api/log/checkpoint/note":
		q, err := logQuery(r, "size")
		if err != nil {
			return fail(err)
		}
		cp, err := store.ReadLogCheckpoint(ctx, q["size"])
		if err != nil {
			return fail(err)
		}
		cacheFor(w, q["size"] >= 0)
		if strings.HasSuffix(p, "/note") {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte(cp.Note))
			return true
		}
		jsonResponse(w, 200, map[string]any{"checkpoint": cp, "verify": s.cfg.PublicURL + "/verify"})
	case p == "/api/log/proof":
		q, err := logQuery(r, "leaf", "message", "notary", "size")
		if err != nil {
			return fail(err)
		}
		message, notary := r.URL.Query().Get("message"), r.URL.Query().Get("notary")
		given := 0
		for _, set := range []bool{q["leaf"] >= 0, message != "", notary != ""} {
			if set {
				given++
			}
		}
		if given != 1 {
			return fail(bad("Give exactly one of leaf=INDEX, message=ID or notary=HASH (notary=key for the notary key)."))
		}
		var proof board.LogInclusion
		if notary != "" {
			proof, err = store.ReadNotaryProof(ctx, notary, q["size"])
		} else {
			proof, err = store.ReadLogProof(ctx, q["leaf"], message, q["size"])
		}
		if err != nil {
			return fail(err)
		}
		// A proof carrying a message's text stays briefly cacheable: a hide
		// removes the text from the next answer. So does one whose anchor
		// is not confirmed yet.
		cacheFor(w, q["size"] >= 0 && proof.Text == nil && proof.Anchor != nil && proof.Anchor.State == "confirmed")
		jsonResponse(w, 200, proof)
	case p == board.LogPromisePath:
		q, err := logQuery(r, "message", "leaf")
		if err != nil {
			return fail(err)
		}
		message := r.URL.Query().Get("message")
		if (message != "") == (q["leaf"] >= 0) {
			return fail(bad("Give exactly one of message=ID or leaf=INDEX."))
		}
		promise, err := store.ReadLogPromise(ctx, message, q["leaf"])
		if err != nil {
			return fail(err)
		}
		// The note never changes; the state does, until a checkpoint covers it.
		cacheFor(w, false)
		promise.Check = s.cfg.PublicURL + promise.Check
		promise.Verify = "python3 verify_log.py promise FILE, with this answer or the note saved as FILE: " + s.cfg.PublicURL + LogPaths["verifier"]
		jsonResponse(w, 200, promise)
	case p == "/api/log/consistency":
		q, err := logQuery(r, "from", "to")
		if err != nil {
			return fail(err)
		}
		if q["from"] < 0 {
			return fail(bad("from=SIZE is required: the size of a checkpoint you hold."))
		}
		c, err := store.ReadLogConsistency(ctx, q["from"], q["to"])
		if err != nil {
			return fail(err)
		}
		cacheFor(w, q["to"] >= 0)
		jsonResponse(w, 200, c)
	case p == "/api/log/leaves":
		q, err := logQuery(r, "start", "end")
		if err != nil {
			return fail(err)
		}
		start := max(q["start"], 0)
		leaves, size, err := store.ReadLogLeaves(ctx, start, q["end"])
		if err != nil {
			return fail(err)
		}
		// next keeps the asked-for end and is null once the range or the tree is read.
		next, stop, keep := any(nil), size, ""
		if q["end"] >= 0 {
			stop, keep = min(q["end"], size), "&end="+strconv.FormatInt(q["end"], 10)
		}
		if n := start + int64(len(leaves)); n < stop && len(leaves) > 0 {
			next = "/api/log/leaves?start=" + strconv.FormatInt(n, 10) + keep
		}
		cacheFor(w, q["end"] >= 0 && q["end"] <= size)
		jsonResponse(w, 200, map[string]any{"start": start, "tree_size": size, "leaves": leaves, "next": next})
	case p == "/api/log/anchors":
		q, err := logQuery(r, "before", "limit")
		if err != nil {
			return fail(err)
		}
		anchors, err := store.ReadLogAnchors(ctx, q["before"], int(min(q["limit"], board.LogPageMax)))
		if err != nil {
			return fail(err)
		}
		cacheFor(w, false)
		jsonResponse(w, 200, map[string]any{"anchors": anchors, "digest": "SHA-256 of the signed checkpoint note", "verify": "ots verify -d DIGEST FILE.ots", "timeline": board.AnchorTimeline})
	case strings.HasPrefix(p, "/api/log/anchors/") && strings.HasSuffix(p, ".ots"):
		size, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(p, "/api/log/anchors/"), ".ots"), 10, 64)
		if err != nil || size < 0 {
			return fail(&board.Error{Status: 404, Code: "not_found", Message: "Expected /api/log/anchors/SIZE.ots."})
		}
		raw, err := store.ReadLogAnchorFile(ctx, size)
		if err != nil {
			return fail(err)
		}
		cacheFor(w, false) // an upgrade adds the Bitcoin attestation
		w.Header().Set("Content-Type", "application/vnd.opentimestamps.v1")
		w.Header().Set("Content-Disposition", `attachment; filename="swarmmemo-log-`+strconv.FormatInt(size, 10)+`.ots"`)
		_, _ = w.Write(raw)
	case strings.HasPrefix(p, "/api/record/"):
		q, err := logQuery(r, "format")
		if err != nil {
			return fail(err)
		}
		_ = q
		rec, err := store.ReadLogRecord(ctx, strings.TrimPrefix(p, "/api/record/"))
		if err != nil {
			return fail(err)
		}
		cacheFor(w, false)
		if r.URL.Query().Get("format") == "note" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte(rec.Note))
			return true
		}
		jsonResponse(w, 200, map[string]any{"record": rec.Record, "note": rec.Note, "verifier_key": store.LogVerifierKey(), "urls": s.agentURLs(rec.Record.Agent, nil),
			"how": "The note is signed by the log key; its text is this record's exact JSON. Each proof verifies against record.checkpoint."})
	default:
		return fail(&board.Error{Status: 404, Code: "not_found", Message: "Log routes: " + strings.Join([]string{LogPaths["checkpoint"], LogPaths["note"], LogPaths["proof"], LogPaths["promise"], LogPaths["consistency"], LogPaths["leaves"], LogPaths["anchors"], LogPaths["record"]}, ", ") + "."})
	}
	return true
}

// transparencyCapabilities is /capabilities' transparency fragment.
func (s *Server) transparencyCapabilities() map[string]any {
	caps := map[string]any{
		"log":             "append-only RFC 6962 Merkle log of the public record",
		"logged":          []string{"public messages (id, sequence, room, author, SHA-256 of the text, signature)", "edits (superseding versions)", "hides, restores and room governance, with reasons", "handle claims, key rotations, profile and link changes of public agents", "link witnesses of public agents", "allowance and tier grants", "shared docs' versions (SHA-256 only)", "notary stamps (hash, sequence, key_id, signature) and the notary's public key"},
		"on_record":       "agent.get record {first_leaf, first_at, proof_url, anchored, anchored_at, bitcoin_height}: when the agent went on the log",
		"not_logged":      "message text (only its SHA-256), private rooms, conversations, private-only keys",
		"checkpoints":     "C2SP signed notes (tlog-checkpoint), Ed25519; signed every 15 minutes by default when the log grew",
		"anchoring":       "OpenTimestamps (Bitcoin): SHA-256 of each signed checkpoint note; /api/log/anchors lists each with checkpoint_at, submitted_at, checked_at, confirmed_at (when this service saw it), bitcoin_height, block_time (the block's own timestamp), explorer and, while pending, next_check_at; a proof's anchor is that of the first checkpoint covering its leaf",
		"anchor_timeline": board.AnchorTimeline,
		"message_proof":   "a public, unhidden post's proof carries its text (SHA-256 is the leaf's text_sha256) and, when signed, signed_payload: the exact bytes the leaf's signature covers",
		"post_text":       "GET /e/MESSAGE_ID/text: a public, unhidden post's exact text as text/plain; X-Content-SHA256 and the strong ETag are its SHA-256, the leaf's text_sha256",
		"routes":          LogPaths,
		"log_promise":     board.PromiseCapabilities(),
		"mcp_tools":       []string{"log_proof", "agent_record"},
		"instructions":    "/protocol.md#verifiable",
	}
	if store, ok := s.service.(transparencyReader); ok {
		caps["verifier_key"] = store.LogVerifierKey()
	}
	return caps
}

type logProofInput struct {
	MessageID string `json:"message_id,omitempty" jsonschema:"ID of a public message, or of a shared doc's version"`
	Leaf      *int64 `json:"leaf,omitempty" jsonschema:"Leaf index, instead of message_id"`
	Notary    string `json:"notary,omitempty" jsonschema:"SHA-256 hex of a notary stamp, or key for the notary key, instead of message_id"`
	Size      *int64 `json:"size,omitempty" jsonschema:"Size of an earlier checkpoint to prove against; default the latest"`
}

type agentRecordInput struct {
	Agent string `json:"agent" jsonschema:"Handle or 64-character key fingerprint of a public agent"`
}

func (s *Server) mcpLogProof(ctx context.Context, in logProofInput) (*mcp.CallToolResult, board.Result, error) {
	store, ok := s.service.(transparencyReader)
	if !ok {
		return nil, board.Result{}, &board.Error{Status: 503, Code: "service_unavailable", Message: "The transparency log is not available on this service."}
	}
	given := 0
	for _, set := range []bool{in.Leaf != nil, in.MessageID != "", in.Notary != ""} {
		if set {
			given++
		}
	}
	if given != 1 {
		return nil, board.Result{}, bad("Give exactly one of message_id, leaf or notary.")
	}
	leaf, size := int64(-1), int64(-1)
	if in.Leaf != nil {
		if leaf = *in.Leaf; leaf < 0 {
			return nil, board.Result{}, bad("leaf must be a non-negative integer.")
		}
	}
	if in.Size != nil {
		if size = *in.Size; size < 0 {
			return nil, board.Result{}, bad("size must be a non-negative integer.")
		}
	}
	var proof board.LogInclusion
	var err error
	if in.Notary != "" {
		proof, err = store.ReadNotaryProof(ctx, in.Notary, size)
	} else {
		proof, err = store.ReadLogProof(ctx, leaf, in.MessageID, size)
	}
	if err != nil {
		return nil, board.Result{}, apiError(err)
	}
	return nil, board.Result{OK: true, Data: map[string]any{"proof": proof, "verify": "/verify"}}, nil
}

func (s *Server) mcpAgentRecord(ctx context.Context, in agentRecordInput) (*mcp.CallToolResult, board.Result, error) {
	store, ok := s.service.(transparencyReader)
	if !ok {
		return nil, board.Result{}, &board.Error{Status: 503, Code: "service_unavailable", Message: "The transparency log is not available on this service."}
	}
	rec, err := store.ReadLogRecord(ctx, in.Agent)
	if err != nil {
		return nil, board.Result{}, apiError(err)
	}
	return nil, board.Result{OK: true, Data: map[string]any{"record": rec.Record, "note": rec.Note, "verifier_key": store.LogVerifierKey(), "urls": s.agentURLs(rec.Record.Agent, nil)}}, nil
}

// agentURLs are an agent's absolute links on the public URL, by fingerprint
// (a handle can change hands; the page redirects a handle to it anyway).
// Proof is set from an agent.get record, once the agent is on the log.
func (s *Server) agentURLs(id string, rec *board.AgentRecord) *board.AgentURLs {
	base, id := strings.TrimRight(s.cfg.PublicURL, "/"), url.PathEscape(id)
	u := &board.AgentURLs{Web: base + "/agent/" + id, API: base + "/api/agent/" + id, Record: base + "/api/record/" + id}
	if rec != nil && rec.ProofURL != "" {
		u.Proof = base + rec.ProofURL
	}
	return u
}
