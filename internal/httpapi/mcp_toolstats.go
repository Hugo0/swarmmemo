package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"swarmmemo/internal/board"
)

// Per-tool MCP call counters (board/mcptoolstats.go): every tools/call on a
// hosted MCP profile adds one call (and one error when it failed) under its
// profile, tool and client family, a failed call one under its error code
// (mcpToolErrorCode), and a daily-salted caller id to the
// day's distinct-caller sketch of the tool and of the profile. Like the
// reader counters (readerstats.go) a call only touches memory under a short
// mutex; the background flush writes the day's totals in one transaction at
// most every readerFlushInterval, and FlushReaderCounts writes the rest at
// shutdown. Nothing is stored that identifies a caller.
//
// The caller id is 16 hex of HMAC-SHA256(daySalt, subject): the hosted
// account when the call's token resolved to one during the request (the
// OAuth gate or the tool's own hosted caller: no extra read here), else the
// network prefix (IPv4 /24, IPv6 /48) and the User-Agent, as readers are
// told apart. A token that resolved to nothing is never a subject, so made-up
// bearer tokens cannot inflate distinct callers. The salt is its own, per
// UTC day, in memory only. Calls from OwnNetworks are counted but are not
// callers. Local sessions (stdio and in-memory, localMCPContext) are not
// counted at all: they are the operator's own process, not hosted usage.

// mcpToolKeysMax bounds the keys held in memory per day: profiles times
// registered tools times families is already bounded; this is a backstop.
const mcpToolKeysMax = 4096

type mcpToolStatsStore interface {
	AddMCPToolStats(context.Context, string, map[string]int64, map[string]int64, map[string]*board.ReaderSketch) error
}

// mcpIdentityStatsStore stores the keyless identity counts (C778).
type mcpIdentityStatsStore interface {
	AddMCPIdentityStats(context.Context, string, map[string]int64) error
}

// Keyless identity creation (C778, board/mcpidentitystats.go): a tools/call
// that carries no hosted token (no path or bearer token, no bound session)
// is a keyless caller's. Its create_identity calls are counted, and those
// that succeed under the tool the same caller called just before. The
// caller is its MCP session when it sent one this server minted, else its
// network prefix and User-Agent (as distinct callers are told apart), held
// as 16 hex of HMAC-SHA256 under a process-only key with its last tool and
// family: at most mcpKeylessMax callers, a further one counted as overflow
// until a sweep frees room. A caller idle mcpKeylessIdle without an
// identity is counted under its last tool (lastbefore_none) on the day of
// that call. Callers still followed at shutdown are dropped, not counted.
// Calls from OwnNetworks are not followed or counted.
const (
	mcpKeylessMax   = 10000
	mcpKeylessIdle  = 30 * time.Minute
	mcpKeylessSweep = time.Minute
	mcpCreateTool   = "create_identity"
)

type keylessCaller struct {
	family, tool string
	seen         time.Time
}

type mcpToolCounter struct {
	mu        sync.Mutex
	pending   map[string]map[string]int64               // UTC day -> board.MCPToolKey -> count
	codes     map[string]map[string]int64               // UTC day -> board.MCPToolCodeKey -> failed calls
	sketches  map[string]map[string]*board.ReaderSketch // UTC day -> board.MCPToolDistinctKey -> callers
	salt      readerSalt
	identity  map[string]map[string]int64 // UTC day -> board.MCPIdentityKey and kin -> count
	keyless   map[string]*keylessCaller   // keyless caller id -> its last call
	keylessID []byte                      // the caller ids' HMAC key, memory only
	lastSweep time.Time
	lastFlush time.Time
	flushing  atomic.Bool
	flushMu   sync.Mutex
}

func newMCPToolCounter() *mcpToolCounter {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	return &mcpToolCounter{pending: map[string]map[string]int64{}, codes: map[string]map[string]int64{}, sketches: map[string]map[string]*board.ReaderSketch{},
		identity: map[string]map[string]int64{}, keyless: map[string]*keylessCaller{}, keylessID: key}
}

// keylessCallerID is the id a keyless caller is followed under: its MCP
// session's when it has one, else its network prefix and User-Agent.
func (c *mcpToolCounter) keylessCallerID(session, prefix, userAgent string) string {
	mac := hmac.New(sha256.New, c.keylessID)
	if session != "" {
		mac.Write([]byte("s\x00" + session))
	} else {
		mac.Write([]byte("h\x00" + prefix + "\x00" + userAgent))
	}
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

// addIdentity counts one under key on day. c.mu is held.
func (c *mcpToolCounter) addIdentity(day, key string) {
	counts := c.identity[day]
	if counts == nil {
		counts = map[string]int64{}
		c.identity[day] = counts
	}
	if _, held := counts[key]; held || len(counts) < mcpToolKeysMax {
		counts[key]++
	}
}

// countKeyless follows one keyless call of tool (C778). c.mu is held.
func (c *mcpToolCounter) countKeyless(now time.Time, day, profile, family, tool, id string, failed bool) {
	c.sweepKeyless(now)
	if tool == mcpCreateTool {
		c.addIdentity(day, board.MCPIdentityKey(profile, family, "calls"))
		if !failed {
			c.addIdentity(day, board.MCPIdentityKey(profile, family, "created"))
			before := board.MCPIdentityFirst
			if e := c.keyless[id]; e != nil {
				before = e.tool
				delete(c.keyless, id)
			}
			c.addIdentity(day, board.MCPLastBeforeIdentityKey(family, before))
			return
		}
	}
	e := c.keyless[id]
	if e == nil {
		if len(c.keyless) >= mcpKeylessMax {
			c.addIdentity(day, board.MCPLastBeforeOverflowKey)
			return
		}
		e = &keylessCaller{}
		c.keyless[id] = e
	}
	e.family, e.tool, e.seen = family, tool, now
}

// sweepKeyless counts and forgets the keyless callers idle mcpKeylessIdle,
// at most every mcpKeylessSweep. c.mu is held.
func (c *mcpToolCounter) sweepKeyless(now time.Time) {
	if now.Sub(c.lastSweep) < mcpKeylessSweep {
		return
	}
	c.lastSweep = now
	for id, e := range c.keyless {
		if now.Sub(e.seen) >= mcpKeylessIdle {
			c.addIdentity(e.seen.UTC().Format("2006-01-02"), board.MCPLastBeforeNoneKey(e.family, e.tool))
			delete(c.keyless, id)
		}
	}
}

// mcpToolProfile is the counters' name of a profile.
func mcpToolProfile(p mcpProfile) string {
	if p.core {
		return "core"
	}
	if p.assistant {
		return "assistant"
	}
	return "mcp"
}

type hostedResolvedKey struct{}

// hostedResolved is the hosted account a request's token resolved to, once
// something on the request's path resolved it.
type hostedResolved struct{ account atomic.Pointer[string] }

// withHostedResolution gives ctx a place to note the resolved account,
// unless it has one.
func withHostedResolution(ctx context.Context) context.Context {
	if _, ok := ctx.Value(hostedResolvedKey{}).(*hostedResolved); ok {
		return ctx
	}
	return context.WithValue(ctx, hostedResolvedKey{}, &hostedResolved{})
}

// noteHostedAccount records that the request's token resolved to account.
func noteHostedAccount(ctx context.Context, account string) {
	if h, ok := ctx.Value(hostedResolvedKey{}).(*hostedResolved); ok && account != "" {
		h.account.Store(&account)
	}
}

// resolvedHostedAccount is the account noted for the request, or "".
func resolvedHostedAccount(ctx context.Context) string {
	if h, ok := ctx.Value(hostedResolvedKey{}).(*hostedResolved); ok {
		if a := h.account.Load(); a != nil {
			return *a
		}
	}
	return ""
}

// countMCPTools counts every tools/call the profile's server answers: the
// outermost receiving middleware, so it sees the final result (a tool's
// refusal, the SDK's argument refusal, an unknown tool). registered is the
// profile's tools; any other name, and a registered one a counts key cannot
// carry (board.ValidMCPToolName), is board.MCPToolOther, so the board never
// sees a key it skips.
func (s *Server) countMCPTools(profile string, registered map[string]bool) mcp.Middleware {
	counted := map[string]bool{}
	for name, ok := range registered {
		if ok && board.ValidMCPToolName(name) {
			counted[name] = true
		}
	}
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			call, isCall := req.(*mcp.CallToolRequest)
			isCall = isCall && method == "tools/call"
			note := &dispatchNote{}
			if isCall {
				ctx = context.WithValue(withHostedResolution(ctx), dispatchNoteKey{}, note)
			}
			res, err := next(ctx, method, req)
			if isCall {
				name := board.MCPToolOther
				if call.Params != nil && counted[call.Params.Name] {
					name = call.Params.Name
				}
				// A call through call_swarmmemo_tool is the tool it called,
				// counted once more as dispatched (mcp_dispatch.go).
				dispatched := note.target != "" && counted[note.target]
				if dispatched {
					name = note.target
				}
				failed := err != nil
				r, _ := res.(*mcp.CallToolResult)
				if r != nil && r.IsError {
					failed = true
				}
				code := ""
				if failed {
					code = mcpToolErrorCode(r, err)
					status := 0
					if be := mcpToolError(r, err); be != nil {
						status = be.Status
					}
					tried, served := "", true
					if call.Params != nil {
						tried, served = call.Params.Name, registered[call.Params.Name]
					}
					if note.tried != "" {
						tried = note.tried // C867: the name call_swarmmemo_tool was asked for
					}
					logMCPProblem(ctx, name, tried, code, status, served) // C757, C759 (problemlog.go)
				}
				s.countMCPTool(ctx, profile, name, code, dispatched)
			}
			return res, err
		}
	}
}

// mcpToolErrorCode is a failed call's error code: the board.Error a tool's
// refusal carries (its structured content, structuredToolErrors, or the
// result's error), else board.MCPToolOther. Only the code is kept, never
// the message or the arguments; an SDK argument refusal is split by kind
// (mcpArgRefusal).
func mcpToolErrorCode(r *mcp.CallToolResult, err error) string {
	be := mcpToolError(r, err)
	if be == nil || !board.ValidMCPToolCode(be.Code) {
		return board.MCPToolOther
	}
	if be.Code == "invalid_request" {
		return mcpArgRefusal(be.Message)
	}
	return be.Code
}

// mcpToolError is the board.Error a failed call carries, or nil.
func mcpToolError(r *mcp.CallToolResult, err error) *board.Error {
	var be *board.Error
	switch {
	case err != nil:
		errors.As(err, &be)
	case r != nil:
		if m, ok := r.StructuredContent.(map[string]any); ok {
			switch e := m["error"].(type) {
			case *board.Error:
				be = e
			case toolErrorObject:
				be = e.Error
			case map[string]any:
				if c, ok := e["code"].(string); ok {
					msg, _ := e["message"].(string)
					status, _ := e["status"].(int)
					if f, ok := e["status"].(float64); ok {
						status = int(f)
					}
					be = &board.Error{Code: c, Message: msg, Status: status}
				}
			}
		}
		if be == nil {
			errors.As(r.GetError(), &be)
		}
	}
	return be
}

// mcpArgRefusal is the counted code of an invalid_request: the kind of the
// SDK's argument refusal (structuredToolErrors), told from its text and
// never keeping it, so a funnel leak says which fix it needs: an argument
// the tool does not take, a required one left out, or one of the wrong
// type. Any other invalid_request stays as it is.
func mcpArgRefusal(msg string) string {
	switch {
	case strings.Contains(msg, "unexpected additional properties"):
		return "invalid_request_unknown_arg"
	case strings.Contains(msg, "missing properties"):
		return "invalid_request_missing_arg"
	case strings.Contains(msg, " has type "), strings.Contains(msg, "cannot unmarshal"):
		return "invalid_request_arg_type"
	}
	return "invalid_request"
}

// countMCPTool records one call, failed when code (its error code) is set,
// and dispatched when it came through call_swarmmemo_tool.
// It never touches storage and never panics into the request.
func (s *Server) countMCPTool(ctx context.Context, profile, tool, code string, dispatched bool) {
	failed := code != ""
	defer func() { _ = recover() }()
	peer, _ := ctx.Value(peerContextKey{}).(string)
	if peer == "" || localMCP(ctx) || s.readers == nil {
		return
	}
	c := s.readers.tools
	if c == nil {
		return
	}
	family := board.ClientFamilyFrom(ctx)
	if family == "" {
		family = "other"
	}
	prefix, own := s.networkPrefix(peer)
	account := resolvedHostedAccount(ctx)
	userAgent := board.RequestSignalsFrom(ctx).UserAgent
	hostedToken, _ := ctx.Value(hostedTokenKey{}).(string)
	keylessID := ""
	if hostedToken == "" && account == "" && !own {
		session, _ := ctx.Value(mcpSessionKey{}).(string)
		keylessID = c.keylessCallerID(session, prefix, userAgent)
	}
	now := s.readers.now()
	day := now.UTC().Format("2006-01-02")
	due := func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		counts := c.pending[day]
		if counts == nil {
			counts = map[string]int64{}
			c.pending[day] = counts
		}
		add := func(key string) {
			if _, held := counts[key]; held || len(counts) < mcpToolKeysMax {
				counts[key]++
			}
		}
		add(board.MCPToolKey(profile, tool, family, "calls"))
		if dispatched {
			add(board.MCPToolKey(profile, tool, family, board.MCPToolDispatched))
		}
		if failed {
			add(board.MCPToolKey(profile, tool, family, "errors"))
			c.addCode(day, profile, tool, code)
		}
		if keylessID != "" {
			c.countKeyless(now, day, profile, family, tool, keylessID, failed)
		}
		if !own {
			if salt := c.salt.get(day, now); salt != nil {
				mac := hmac.New(sha256.New, salt)
				if account != "" {
					mac.Write([]byte("a\x00" + account))
				} else {
					mac.Write([]byte("h\x00" + prefix + "\x00" + userAgent))
				}
				id := hex.EncodeToString(mac.Sum(nil))[:16]
				sketches := c.sketches[day]
				if sketches == nil {
					sketches = map[string]*board.ReaderSketch{}
					c.sketches[day] = sketches
				}
				for _, k := range []string{board.MCPToolDistinctKey(profile, tool), board.MCPToolDistinctKey(profile, board.MCPToolAny)} {
					if sketches[k] == nil {
						if len(sketches) >= mcpToolKeysMax {
							continue
						}
						sketches[k] = &board.ReaderSketch{}
					}
					sketches[k].Add(id)
				}
			}
		}
		if now.Sub(c.lastFlush) < s.readers.interval {
			return false
		}
		c.lastFlush = now
		return true
	}()
	if store, ok := s.service.(mcpToolStatsStore); ok && due && c.flushing.CompareAndSwap(false, true) {
		go func() {
			defer c.flushing.Store(false)
			c.flush(store)
		}()
	}
}

// addCode counts one failed call under its code, at most
// board.MCPToolCodesMax codes per profile and tool and day held (the board
// caps what it stores the same way), any further one as board.MCPToolOther.
// c.mu is held.
func (c *mcpToolCounter) addCode(day, profile, tool, code string) {
	codes := c.codes[day]
	if codes == nil {
		codes = map[string]int64{}
		c.codes[day] = codes
	}
	key := board.MCPToolCodeKey(profile, tool, code)
	if _, held := codes[key]; !held && code != board.MCPToolOther {
		prefix, n := board.MCPToolCodeKey(profile, tool, ""), 0
		for k := range codes {
			if strings.HasPrefix(k, prefix) && k != prefix+board.MCPToolOther {
				n++
			}
		}
		if n >= board.MCPToolCodesMax || len(codes) >= mcpToolKeysMax {
			key = prefix + board.MCPToolOther
		}
	}
	if _, held := codes[key]; held || len(codes) < mcpToolKeysMax {
		codes[key]++
	}
}

// networkPrefix is peer's network (IPv4 /24, IPv6 /48), as readerNetwork,
// and whether it is in OwnNetworks.
func (s *Server) networkPrefix(peer string) (prefix string, own bool) {
	ip := net.ParseIP(peer)
	if ip == nil {
		return peer, false
	}
	for _, network := range s.readers.own {
		if network.Contains(ip) {
			return "", true
		}
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String(), false
	}
	return ip.Mask(net.CIDRMask(48, 128)).String(), false
}

// flush writes every pending day in one transaction each; a failed day is
// kept for the next flush, at most readerMaxPendingDay days.
func (c *mcpToolCounter) flush(store mcpToolStatsStore) {
	defer func() { _ = recover() }()
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
	c.mu.Lock()
	batch, codeBatch, sketchBatch, identityBatch := c.pending, c.codes, c.sketches, c.identity
	c.pending, c.codes, c.sketches, c.identity = map[string]map[string]int64{}, map[string]map[string]int64{}, map[string]map[string]*board.ReaderSketch{}, map[string]map[string]int64{}
	c.mu.Unlock()
	if is, ok := store.(mcpIdentityStatsStore); ok {
		for day, counts := range identityBatch {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := is.AddMCPIdentityStats(ctx, day, counts)
			cancel()
			if err != nil {
				c.requeueIdentity(day, counts)
			}
		}
	}
	days := map[string]bool{}
	for day := range batch {
		days[day] = true
	}
	for day := range codeBatch {
		days[day] = true
	}
	for day := range sketchBatch {
		days[day] = true
	}
	for day := range days {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := store.AddMCPToolStats(ctx, day, batch[day], codeBatch[day], sketchBatch[day])
		cancel()
		if err != nil {
			c.requeue(day, batch[day], codeBatch[day], sketchBatch[day])
		}
	}
}

// requeue puts a day's unwritten counts and sketches back under what came
// since, keeping only the most recent readerMaxPendingDay days.
func (c *mcpToolCounter) requeue(day string, counts, codes map[string]int64, sketches map[string]*board.ReaderSketch) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range []struct {
		from map[string]int64
		into map[string]map[string]int64
	}{{counts, c.pending}, {codes, c.codes}} {
		if len(m.from) == 0 {
			continue
		}
		target := m.into[day]
		if target == nil {
			target = map[string]int64{}
			m.into[day] = target
		}
		for k, n := range m.from {
			target[k] += n
		}
	}
	if len(sketches) > 0 {
		target := c.sketches[day]
		if target == nil {
			target = map[string]*board.ReaderSketch{}
			c.sketches[day] = target
		}
		for k, h := range sketches {
			if target[k] == nil {
				target[k] = h
			} else {
				target[k].Merge(h)
			}
		}
	}
	for _, m := range []func() []string{
		func() []string { return keysOf(c.pending) },
		func() []string { return keysOf(c.codes) },
		func() []string { return keysOf(c.sketches) },
	} {
		for keys := m(); len(keys) > readerMaxPendingDay; keys = m() {
			oldest := keys[0]
			for _, d := range keys {
				if d < oldest {
					oldest = d
				}
			}
			delete(c.pending, oldest)
			delete(c.codes, oldest)
			delete(c.sketches, oldest)
		}
	}
}

// requeueIdentity puts a day's unwritten identity counts back, keeping only
// the most recent readerMaxPendingDay days.
func (c *mcpToolCounter) requeueIdentity(day string, counts map[string]int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	target := c.identity[day]
	if target == nil {
		target = map[string]int64{}
		c.identity[day] = target
	}
	for k, n := range counts {
		target[k] += n
	}
	for keys := keysOf(c.identity); len(keys) > readerMaxPendingDay; keys = keysOf(c.identity) {
		slices.Sort(keys)
		delete(c.identity, keys[0])
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
