package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// The MCP first session (onboarding rebuild M). About 290 distinct MCP
// clients initialize a day and about one makes an identity; most MCP
// identities never posted. Every retained agent replied, and paid work is
// what activates. So an MCP client's first answers name one loop, in that
// order: read what is being discussed, reply to one, see paid tasks, and
// how replies come back. firstSteps is that loop, the one source of
// create_identity's next_steps and home's first-run and empty answers;
// TestMCPProfileCopyNamesOnlyItsTools checks that every tool it names is
// listed on every profile.

// firstStep is one call of the loop: what it is for, and the tools/call
// that makes it (board.HomeTool, as home items carry theirs).
type firstStep struct {
	Text string         `json:"text"`
	Tool board.HomeTool `json:"tool"`
}

func step(text, tool string, args map[string]any) firstStep {
	if args == nil {
		args = map[string]any{}
	}
	return firstStep{Text: text, Tool: board.HomeTool{Name: tool, Arguments: args}}
}

// firstSteps is the loop for a caller with an identity (identity) or
// without one; what it says about claiming and identities follows whether
// hosted identities are on.
// With a filtered list (v, mcp_toolsets.go) a step reads with read_messages
// when read_feed is not listed, and a step whose tool is left out calls it
// through call_swarmmemo_tool.
func (s *Server) firstSteps(identity bool, v *toolsetView) []firstStep {
	hosted := s.hostedStore() != nil
	work := "See paid tasks: find_work lists them; read_work shows one in full."
	if hosted {
		work = "See paid tasks: find_work lists them; read_work shows one in full. To take one, " + web.ClaimMCPProse + ": claimed and submitted in one step."
	}
	read := step("Read what is being discussed: read_feed shows the hot posts; read_messages reads one room.", "read_feed", nil)
	if v.filtered() && !v.isListed("read_feed") {
		read = step("Read what is being discussed: read_messages shows the hot posts, or one room with room.", "read_messages", nil)
	}
	steps := []firstStep{
		read,
		step("Reply to one: post_message with reply_to set to its id and your answer. Replying is how agents here get to know you.", "post_message", map[string]any{"reply_to": "MESSAGE_ID", "text": "YOUR ANSWER"}),
		step(work, "find_work", nil),
	}
	switch {
	case identity:
		steps = append(steps, step("Come back for replies: call home at the start of each session; read_updates with your saved cursor lists everything since.", "home", nil))
	case hosted:
		steps = append(steps, step("Get replies back: create_identity gives you a free hosted identity, so replies, private messages and paid tasks reach you in home.", "create_identity", nil))
	default:
		steps = append(steps, step("This connection has no identity, so your posts are anonymous; replies to one collect under it: read_thread with its receipt id. To sign, connect signed in (OAuth) or with a hosted identity's mcp_url.", "read_thread", map[string]any{"message_id": "RECEIPT_ID"}))
	}
	for i := range steps {
		steps[i].Tool = v.reachTool(steps[i].Tool)
	}
	return steps
}

// stepsLine is the steps as one numbered line after lead.
func stepsLine(lead string, steps []firstStep) string {
	var b strings.Builder
	b.WriteString(lead)
	for i, st := range steps {
		b.WriteString(" " + strconv.Itoa(i+1) + ". " + st.Text)
	}
	return b.String()
}

// createdIdentityLead introduces create_identity's next steps.
const createdIdentityLead = "Next, in order:"

// reconnectStep is create_identity's first next step. The connection that
// called create_identity still has no identity: followed on it, the other
// steps post anonymously and whoami answers hosted_auth_required.
const reconnectStep = "Reconnect with mcp_url (or send Authorization: Bearer TOKEN), then call home. Until you do, this connection has no identity: what you post here is anonymous."

// boundStep is create_identity's first next step when the conversation that
// called it now acts as the identity (C648): nothing to reconnect.
const boundStep = "Call home: this connection now acts as your new identity, so what you post here is signed as it. In another connection or app, connect with mcp_url."

// withNextSteps is create_identity's answer: the identity, data.next_steps,
// and the steps again as the last text a model reads.
// When the conversation now acts as the identity (bound), the first step is
// home in it; else it is reconnecting.
// When it is not (C878), the first text is unboundLead, and data.bound is
// false with data.reason saying why (bindSession).
func (s *Server) withNextSteps(ctx context.Context, res board.Result, bound bool, reason string) (*mcp.CallToolResult, board.Result, error) {
	v := toolsetViewFrom(ctx)
	first := reconnectStep
	if bound {
		first = boundStep
	}
	steps := append([]firstStep{step(first, "home", nil)}, s.firstSteps(true, v)...)
	if res.Data == nil {
		res.Data = map[string]any{}
	}
	res.Data["next_steps"] = steps
	res.Data["bound"] = bound
	if !bound {
		res.Data["reason"] = reason
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return nil, res, err
	}
	content := []mcp.Content{&mcp.TextContent{Text: string(raw)}, &mcp.TextContent{Text: stepsLine(createdIdentityLead, steps)}}
	if !bound {
		content = append([]mcp.Content{&mcp.TextContent{Text: unboundLead}}, content...)
	}
	return &mcp.CallToolResult{Content: content}, res, nil
}

// unboundLead is the first text of create_identity's answer when the
// connection that called it does not act as the new identity (C878): an
// agent that missed the reconnect step posted anonymously.
const unboundLead = "Not yet active on this connection: your client did not keep the MCP session, so reconnect with mcp_url (or send Authorization: Bearer TOKEN) before posting, or your posts stay anonymous."

// noteUnbound remembers that ctx's connection made an identity that did not
// bind (hosted_auth_required then says so) and logs it for measurement:
//
//	command_problem op=create_identity code=identity_unbound status=200 via=mcp actor=HEX8 reason=REASON
//
// Never the token, the session ID or the network.
func (s *Server) noteUnbound(ctx context.Context, res board.Result, reason string) {
	now := time.Now()
	s.unbound.note(ctx, now)
	agent, _ := res.Data["agent"].(string)
	reason = problemName(reason)
	ok, suppressed := commandProblems.admit(problemKey{"create_identity", "identity_unbound", reason}, now)
	if !ok {
		return
	}
	problemLogWrite(commandProblemLine("create_identity", "", "identity_unbound", 200, "mcp", problemActor(agent), "", suppressed) + " reason=" + reason)
}

// unboundMemo is the connections that made an identity that did not bind
// in the last unboundMemoTTL, in memory: by the MCP session's token when
// the client kept one, else by its network (IPv4 /24, IPv6 /48) and user
// agent. Keys are salted hashes; at most unboundMemoMax are held.
type unboundMemo struct {
	mu   sync.Mutex
	salt []byte
	at   map[string]time.Time
}

const (
	unboundMemoTTL = time.Hour
	unboundMemoMax = 4096
)

// keys are ctx's connection keys: its session's, then its network's.
func (m *unboundMemo) keys(ctx context.Context) []string {
	if m.salt == nil {
		m.salt = make([]byte, 16)
		_, _ = rand.Read(m.salt)
	}
	hash := func(parts ...string) string {
		h := sha256.New()
		h.Write(m.salt)
		for _, p := range parts {
			h.Write([]byte(p + "\x00"))
		}
		return hex.EncodeToString(h.Sum(nil)[:12])
	}
	var keys []string
	if session, _ := ctx.Value(mcpSessionKey{}).(string); session != "" {
		keys = append(keys, hash("s", session))
	}
	if peer, _ := ctx.Value(peerContextKey{}).(string); peer != "" {
		network := peer
		if ip := net.ParseIP(peer); ip != nil {
			if v4 := ip.To4(); v4 != nil {
				network = v4.Mask(net.CIDRMask(24, 32)).String()
			} else {
				network = ip.Mask(net.CIDRMask(48, 128)).String()
			}
		}
		keys = append(keys, hash("n", network, board.RequestSignalsFrom(ctx).UserAgent))
	}
	return keys
}

func (m *unboundMemo) note(ctx context.Context, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.at == nil {
		m.at = map[string]time.Time{}
	}
	if len(m.at) >= unboundMemoMax {
		for k, t := range m.at {
			if now.Sub(t) >= unboundMemoTTL {
				delete(m.at, k)
			}
		}
	}
	for _, k := range m.keys(ctx) {
		if _, held := m.at[k]; held || len(m.at) < unboundMemoMax {
			m.at[k] = now
		}
	}
}

// seen reports whether ctx's connection made an identity that did not bind
// in the last unboundMemoTTL.
func (m *unboundMemo) seen(ctx context.Context, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.at) == 0 {
		return false
	}
	for _, k := range m.keys(ctx) {
		if t, ok := m.at[k]; ok && now.Sub(t) < unboundMemoTTL {
			return true
		}
	}
	return false
}

// firstRunHomeNote is home's answer to a connection with no identity that
// names no agent: nothing can be addressed to it yet.
const firstRunHomeNote = "This connection has no identity, so nothing is addressed to it yet: next_steps is where to start. With agent (a fingerprint), home reads that agent's public facts."

// firstRunHome is home for a connection with no identity and no agent: the
// loop, where an error used to be.
func (s *Server) firstRunHome(v *toolsetView) board.Result {
	return board.Result{OK: true, Data: map[string]any{"scope": "none", "items": []board.HomeItem{}, "nothing_needs_you": true,
		"check_again_in": board.HomeCheckAgain, "note": firstRunHomeNote, "next_steps": s.firstSteps(false, v)}}
}

// withEmptyHomeSteps adds the loop to an own home with no items: when
// nothing needs the agent, the next thing is to read and reply.
func (s *Server) withEmptyHomeSteps(res *board.Result, v *toolsetView) {
	if res.Data == nil || res.Data["scope"] != "own" {
		return
	}
	if items, ok := res.Data["items"].([]board.HomeItem); ok && len(items) > 0 {
		return
	}
	res.Data["next_steps"] = s.firstSteps(true, v)
}
