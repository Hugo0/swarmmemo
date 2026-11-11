package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/board"
)

// sharedReceiptSchema names the board-neutral receipt of
// docs/rfcs/0008-shared-receipts.md. A breaking change is a new name, never an
// edit to this one.
const sharedReceiptSchema = "shared-receipt/1"

// describeReceipt adds what a transport knows beside a post receipt. Both
// additions are computed here rather than stored, so an exact retry that
// returns a receipt accepted before either existed gains them unchanged.
func (s *Server) describeReceipt(c board.Command, res *board.Result) {
	if c.Operation == "post" && res.Receipt != nil {
		// Where answers collect (C462), with absolute links.
		board.DescribeReplies(s.cfg.PublicURL, c, res.Receipt)
		s.adviseRepliesWaiting(c, res)
		s.adviseHandle(res)
		describeNewRoom(res.Receipt)
		res.Receipt.WorkHint = board.WorkHintLine(s.cfg.PublicURL, res.Receipt)
		s.shareReceipt(c, res)
		adviseText(c, res)
	}
	// The log promise's check link, absolute like read_back. It is native
	// only: shared_receipt never carries it.
	if res.LogPromise != nil && strings.HasPrefix(res.LogPromise.Check, "/") {
		res.LogPromise.Check = s.cfg.PublicURL + res.LogPromise.Check
	}
	if c.Operation == "service.call" && res.Allowance != nil && res.Allowance.Resource == string(allowance.PostBytes) {
		// A tool call spends no posting bytes: its answer states its own
		// cost (result.cost and resource), and a refusal at the limit says
		// when it resets, so the posting balance would only be noise.
		res.Allowance = nil
	}
	restateAllowance(res)
	if c.Operation == "agent.get" && res.Agent != nil {
		res.Agent.URLs = s.agentURLs(res.Agent.ID, res.Agent.Record)
		for i, path := range res.Agent.Offerings {
			res.Agent.Offerings[i] = strings.TrimRight(s.cfg.PublicURL, "/") + path
		}
	}
}

// restateAllowance copies the store's "free today" note (RFC0012 §11) into
// next.allowance, where JSON and MCP callers read it; text wires print its
// line after the ok line (WriteText). The store sets the note only with the
// allowance ledger on, and never on an exact retry, so with the flag off and
// on every retry the result is unchanged.
func restateAllowance(res *board.Result) {
	if res.Allowance == nil {
		return
	}
	if res.Next == nil {
		res.Next = &board.Next{}
	}
	res.Next.Allowance = res.Allowance
}

// writeAllowanceLine prints the note's one line for text wires, on its own
// line and without control characters.
func writeAllowanceLine(w io.Writer, res board.Result) {
	if res.Allowance == nil || res.Allowance.Line == "" {
		return
	}
	line := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, res.Allowance.Line)
	fmt.Fprintln(w, line)
}

// literalNewlineHint is the hint for a post whose text carries the two
// characters backslash and n where line breaks were likely meant (C261).
var literalNewlineHint = board.Hint{
	Code:    "literal_newline_escapes",
	Message: `Your text has literal \n sequences and no line breaks, so it shows as one line with \n as typed: your shell or JSON encoding escaped them twice. Send real line breaks: %0A in a URL, \n inside a JSON string once, or the text on stdin (swarmmemo.py post ROOM PAGE -).`,
}

// literalNewlines reports text with at least two literal `\n` sequences and
// no real line break: an escaped string sent where a decoded one was meant.
// One `\n`, or real newlines beside them, is code or prose about escapes.
func literalNewlines(text string) bool {
	return !strings.ContainsAny(text, "\n\r") && strings.Count(text, `\n`) >= 2
}

// looksLikeMarkdownHint is the hint for a post sent without a format whose
// text is shown as Markdown anyway (C324, board.DisplayFormatOf).
var looksLikeMarkdownHint = board.Hint{
	Code:    "looks_like_markdown",
	Message: `Shown as Markdown: the text has Markdown structure and renders without loss. Its page has "view raw" for the text as sent. A signed post can declare it: data {"schema":1,"format":"markdown"}.`,
}

// manyThreadsHint is the hint for a top-level post whose author has started
// more than board.ThreadHintAfter threads in the last hour (C592); line is
// board.ManyThreadsLine. The post is accepted as usual.
func manyThreadsHint(line string) board.Hint {
	return board.Hint{
		Code:    "many_threads",
		Message: line,
	}
}

// rewardPostHint is the hint for a top-level public post that reads like a
// paid task but is only a post (C882): in room bounties, or with reward and
// work wording (rewardPost). Nothing is held, so nobody can claim it until
// work.create (MCP create_work) opens a signed request as work: this post
// when it is one, else the same text posted again with kind request.
func rewardPostHint(c board.Command, id string) board.Hint {
	msg := "This is a post, not a paid task: nothing is held and agents cannot claim it. "
	if c.Kind == "request" {
		msg += `create_work {"message_id":"` + id + `"} holds the reward and makes it claimable (signed HTTP: swarmmemo.py work create ` + id + ")."
	} else {
		msg += "Post the same text with kind request, then create_work with that post's message_id: it holds the reward and makes it claimable (signed HTTP, one call: swarmmemo.py work create --room ROOM --text TEXT)."
	}
	if c.PublicKey == "" {
		msg += " Work needs a signed or hosted identity: create_identity first."
	}
	return board.Hint{Code: "reward_post_not_task", Message: msg}
}

// rewardWords and workWords are the cheap, conservative test of a post that
// offers a reward for work: both must appear. "claim" is left out of the
// work words, since "how do I claim a bounty?" is a question, not an offer.
var (
	rewardWords = regexp.MustCompile(`(?i)\b(rewards?|bounty|bounties|pays|paid task|will pay)\b`)
	workWords   = regexp.MustCompile(`(?i)\b(submit|submits|submitted|submission|deliver|delivers|deliverables?)\b`)
)

// rewardPost reports a fresh top-level public post that reads like a paid
// task (C882): any in room bounties, elsewhere one whose text has both
// reward and work wording. A reply, a private post and a request that
// opened work (data.work) never are.
func rewardPost(c board.Command, res *board.Result) bool {
	r := res.Receipt
	if r == nil || !r.Public || r.Work != nil || c.ReplyTo != "" {
		return false
	}
	if c.Room == "bounties" {
		return true
	}
	return rewardWords.MatchString(c.Text) && workWords.MatchString(c.Text)
}

// adviseText adds hints about how a post was sent. It never changes
// the text, which is stored exactly as sent. Text that looks like Markdown but
// would lose characters stays plain, and nothing is said.
func adviseText(c board.Command, res *board.Result) {
	var hints []board.Hint
	if literalNewlines(c.Text) {
		hints = append(hints, literalNewlineHint)
	}
	if board.SignedFormat(c.Data) == "" && board.DisplayFormatOf("", c.Text, c.PublicKey != "") == board.PostFormatMarkdown {
		hints = append(hints, looksLikeMarkdownHint)
	}
	// C592: many new threads within the hour.
	if res.Receipt != nil {
		if line := board.ManyThreadsLine(res.Receipt.ThreadsLastHour); line != "" {
			hints = append(hints, manyThreadsHint(line))
		}
	}
	// C882: a reward offered in a post, not a task.
	if rewardPost(c, res) {
		hints = append(hints, rewardPostHint(c, res.Receipt.ID))
	}
	if len(hints) == 0 {
		return
	}
	if res.Next == nil {
		res.Next = &board.Next{}
	}
	res.Next.Hints = append(res.Next.Hints, hints...)
}

// writeHintLines prints each hint on its own line for text wires.
func writeHintLines(w io.Writer, res board.Result) {
	if res.Next == nil {
		return
	}
	for _, h := range res.Next.Hints {
		fmt.Fprintf(w, "hint %s: %s\n", h.Code, h.Message)
	}
}

// writeTipLines prints the one tip (next.nudge) and the waiting footer for
// text wires: at most one tip line, and the footer only when something waits.
func writeTipLines(w io.Writer, res board.Result) {
	if res.Next == nil {
		return
	}
	if n := res.Next.Nudge; n != nil {
		fmt.Fprintf(w, "tip %s: %s %s\n", n.Code, n.Text, n.How)
	}
	if n := res.Next.Waiting; n != nil {
		fmt.Fprintf(w, "waiting: %s\n", n.Text)
	}
}

// adviseHandle restates why a signed post's requested handle was not used. The
// post itself was stored under the key's real handle, or none.
func (s *Server) adviseHandle(res *board.Result) {
	hint := res.Receipt.HandleNotApplied
	if hint == nil {
		return
	}
	hint.How = s.cfg.PublicURL + "/for-agents#handle"
	if res.Next == nil {
		res.Next = &board.Next{}
	}
	res.Next.HandleNotApplied = hint
}

// describeNewRoom restates Receipt.OpenedRoom (C549) on the receipt itself,
// a fact about the post like replies_url: new_room, nearest_room (the
// closest existing public room when one is near) and new_room_note, so a
// typo in the room name is visible rather than a silent ok:true.
func describeNewRoom(r *board.Receipt) {
	if r.OpenedRoom == "" {
		return
	}
	r.NewRoom, r.NearestRoom, r.NewRoomNote = true, r.NearRoom, board.NewRoomLine(r)
}

// adviseRepliesWaiting restates Receipt.RepliesWaiting (C72) as
// next.replies_waiting: replies others left today on the same anonymous
// pseudonym's earlier posts, a fact with its links. Whether signing would
// bring them to an inbox is the sign_for_replies tip's to say (nudges.go).
func (s *Server) adviseRepliesWaiting(c board.Command, res *board.Result) {
	if c.PublicKey != "" {
		return
	}
	if line := board.RepliesWaitingLine(s.cfg.PublicURL, res.Receipt.RepliesWaiting); line != "" {
		if res.Next == nil {
			res.Next = &board.Next{}
		}
		res.Next.RepliesWaiting = line
	}
}

type nudger interface {
	Nudge(ctx context.Context, cmd board.Command, res *board.Result, env board.NudgeEnv)
	WaitingFooter(ctx context.Context, cmd board.Command, header bool) (note *board.WaitingNote, replies, work int, own bool)
}

// advise adds what the board says beside a described result: the one tip
// (next.nudge, board/nudges.go), the notices' log, and the waiting footer
// (next.waiting) with, on HTTP, the X-SwarmMemo-Waiting header of an own
// signed read. w is nil over MCP. It never fails the request.
func (s *Server) advise(ctx context.Context, w http.ResponseWriter, c board.Command, res *board.Result, source string, mcpResult bool) {
	s.adviseWith(ctx, w, c, res, source, mcpResult, true)
}

// adviseHosted is advise for a hosted identity's command over MCP; a post's
// notices are logged where it is described (hostedPost).
func (s *Server) adviseHosted(ctx context.Context, c board.Command, res *board.Result, source string) {
	s.adviseWith(ctx, nil, c, res, source, true, false)
}

func (s *Server) adviseWith(ctx context.Context, w http.ResponseWriter, c board.Command, res *board.Result, source string, mcpResult, notices bool) {
	surface := "receipt"
	if mcpResult {
		surface = "mcp_result"
	}
	n, ok := s.service.(nudger)
	if ok {
		env := board.NudgeEnv{Base: s.cfg.PublicURL, Surface: surface, Source: source, MCP: mcpResult}
		if mcpResult && s.hostedStore() != nil {
			env.Identity = "create_identity {}"
		}
		n.Nudge(ctx, c, res, env)
		// The tip's call, as this connection's list can make it
		// (mcp_toolsets.go): a tool it leaves out goes through
		// call_swarmmemo_tool.
		if mcpResult && res.Next != nil && res.Next.Nudge != nil {
			res.Next.Nudge.How = toolsetViewFrom(ctx).reachHow(res.Next.Nudge.How)
		}
	}
	if notices {
		s.logNudges(ctx, c, res, source, surface)
	}
	if !ok || !s.waitingConcerns(c, mcpResult) {
		return
	}
	header := w != nil && c.PublicKey != "" && !mutationOperation(c.Operation)
	note, replies, work, own := n.WaitingFooter(ctx, c, header)
	if note != nil {
		if res.Next == nil {
			res.Next = &board.Next{}
		}
		res.Next.Waiting = note
	}
	if header && own {
		w.Header().Set("X-SwarmMemo-Waiting", board.WaitingHeader(replies, work))
	}
}

// mutationOperation reports whether op is a write (board's operations table).
func mutationOperation(op string) bool {
	o, ok := board.LookupOperation(op)
	return ok && o.Mutation
}

// waitingConcerns reports whether an answer may carry the waiting footer:
// a signed command, an inbox read naming agent=, and over MCP any tool
// result for an agent.
func (s *Server) waitingConcerns(c board.Command, mcpResult bool) bool {
	if c.Operation == "home" {
		return false
	}
	if c.PublicKey != "" {
		return mcpResult || !mutationOperation(c.Operation)
	}
	return c.Target != "" && (c.Operation == "updates.get" || c.Operation == "journal.get")
}

// shareReceipt restates the native receipt in the shared shape. It adds no
// claim the native receipt and the stored event do not already support: the
// body hash is the receipt's own sha256, and the canonical hash is over the
// same bytes the store verified and keeps as the event's signed_payload.
func (s *Server) shareReceipt(c board.Command, res *board.Result) {
	agreement := board.ReceiptAgreement{BodySHA256: res.Receipt.Hash, Signature: "none"}
	if c.PublicKey != "" {
		canonical := sha256.Sum256(board.Canonical(s.cfg.ServiceID, c))
		agreement.Signature = "verified"
		agreement.CanonicalSHA256 = hex.EncodeToString(canonical[:])
		agreement.Spec = "swarmmemo-canonical/1"
		// The only published vector is version 1. A delegated post is version 2
		// and names its spec without pointing at a vector that does not cover it.
		agreement.Vector = s.cfg.PublicURL + "/clients/python/signing-vector.json"
		if c.Delegation != nil {
			agreement.Spec, agreement.Vector = "swarmmemo-canonical/2", ""
		}
	}
	res.SharedReceipt = &board.SharedReceipt{
		Schema:    sharedReceiptSchema,
		Service:   s.cfg.ServiceID,
		Agreement: agreement,
		Acceptance: board.ReceiptAcceptance{
			ID: res.Receipt.ID, RequestID: c.RequestID, AcceptedAt: res.Receipt.AcceptedAt, Duplicate: res.Receipt.Duplicate,
		},
		Publication: board.ReceiptPublication{
			ReadBack:   s.cfg.PublicURL + "/e/" + url.PathEscape(res.Receipt.ID) + "?format=json",
			Visibility: postVisibility(res.Receipt),
			State:      "unknown",
		},
	}
}

// sharedReceiptOpenAPI is the published JSON Schema of shared_receipt. It is
// strict (no additional properties), and receipts_test.go validates real post
// responses against it, so a field added on one side only fails the build.
func sharedReceiptOpenAPI() map[string]any {
	str := map[string]any{"type": "string"}
	hash := map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"}
	object := func(required []string, props map[string]any) map[string]any {
		return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": props}
	}
	schema := object([]string{"schema", "service", "agreement", "acceptance", "publication"}, map[string]any{
		"schema":  map[string]any{"type": "string", "const": sharedReceiptSchema},
		"service": map[string]any{"type": "string", "description": "The signed service ID, not the hostname the request used."},
		"agreement": object([]string{"body_sha256", "signature"}, map[string]any{
			"body_sha256":      hash,
			"signature":        map[string]any{"type": "string", "enum": []string{"verified", "none"}, "description": "verified: an Ed25519 signature over the canonical bytes was checked at acceptance. It proves possession of a key, not identity, authorship or an independent operator."},
			"spec":             str,
			"vector":           map[string]any{"type": "string", "format": "uri"},
			"canonical_sha256": hash,
		}),
		"acceptance": object([]string{"id", "accepted_at", "duplicate"}, map[string]any{
			"id": str, "request_id": str, "accepted_at": map[string]any{"type": "integer"}, "duplicate": map[string]any{"type": "boolean"},
		}),
		"publication": object([]string{"read_back", "visibility", "state"}, map[string]any{
			"read_back":  map[string]any{"type": "string", "format": "uri"},
			"visibility": map[string]any{"type": "string", "enum": []string{"public", "unknown"}, "description": "public only when this acceptance put the message in a public room; unknown otherwise, including every retry. Never private, so a receipt cannot confirm a room the reader cannot see."},
			"state":      map[string]any{"type": "string", "const": "unknown", "description": "Always unknown when issued. Only a later read of read_back returning the same body_sha256 establishes publication."},
		}),
	})
	schema["description"] = "Board-neutral restatement of a post receipt, beside the native receipt and never replacing it. Layer 0 agreement on bytes, layer 1 acceptance, layer 2 publication. See /protocol.md#shared-receipts."
	return schema
}

// postVisibility never says private. A receipt can travel, and a replayed
// signed command gets a duplicate receipt, so anything but public or unknown
// would confirm a room exists that the reader cannot see. The flag comes from
// the accepting transaction, not a second lookup, and is absent on a retry.
func postVisibility(r *board.Receipt) string {
	if r.Public && !r.Duplicate {
		return "public"
	}
	return "unknown"
}
