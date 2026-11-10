package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/board"
)

// C757: the operator's private view of failed commands. A command that
// ends in a problem (4xx/5xx) on POST /v1/command, a /call/ (or /c64/)
// route or an MCP tool call writes one server-log line (journald):
//
//	command_problem op=OP [tried=NAME] code=CODE status=N|unknown via=command|call|mcp actor=HEX8|anon [work=HEX8] [suppressed=N]
//
// Only names from fixed tables (the operation, the problem code), the
// status and a short hash of the signer: never text, payloads, message
// ids, IPs, tokens or headers. C836: a work.* operation's line carries
// work=, the same short hash of the work id it names (message_id), so the
// operator can tell which task a retry loop hits by hashing candidates. C759: beside op=unknown (an operation
// /v1/command does not have) or op=other (an MCP tool this server does not
// serve), tried= is the name the caller sent when it is a short token
// (problemTried), so the log shows which names agents guess. An MCP tool
// we do not serve is status=404 code=unknown_tool; an MCP failure with no
// status is status=unknown. Per (op, code, tried) at most one line per
// problemLogEvery; the lines held back are counted into the next one.
// Nothing reaches a response, a route or the database.

const (
	problemLogEvery = 10 * time.Second
	problemLogKeys  = 1024 // bound on the limiter's table
)

type problemKey struct{ op, code, tried string }

type problemSlot struct {
	last       time.Time
	suppressed int
}

// problemLimiter is the per-(op, code) rate limit of command_problem lines.
type problemLimiter struct {
	mu    sync.Mutex
	slots map[problemKey]*problemSlot
}

// admit reports whether a line for key may be written at now, and how many
// were suppressed since the last one written (counted into this one).
func (l *problemLimiter) admit(key problemKey, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.slots == nil {
		l.slots = map[problemKey]*problemSlot{}
	}
	slot := l.slots[key]
	if slot == nil {
		if len(l.slots) >= problemLogKeys {
			for k, v := range l.slots {
				if now.Sub(v.last) >= problemLogEvery {
					delete(l.slots, k)
				}
			}
			if len(l.slots) >= problemLogKeys {
				return false, 0
			}
		}
		l.slots[key] = &problemSlot{last: now}
		return true, 0
	}
	if now.Sub(slot.last) < problemLogEvery {
		slot.suppressed++
		return false, 0
	}
	n := slot.suppressed
	slot.last, slot.suppressed = now, 0
	return true, n
}

var commandProblems problemLimiter

// problemName keeps a log field to a short token of [a-z0-9._/-]; anything
// else (never expected: every value comes from a fixed table) is "other".
func problemName(s string) string {
	if s == "" || len(s) > 64 {
		return "other"
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '/' || c == '-') {
			return "other"
		}
	}
	return s
}

// problemTried is an attempted operation or tool name kept for tried=:
// ASCII-case-folded, then a token of ^[a-z0-9][a-z0-9._-]{0,47}$, else ""
// (absent). Never text, ids, IPs or headers: none of them fits.
func problemTried(s string) string {
	if s == "" || len(s) > 48 {
		return ""
	}
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
			b[i] = c
		}
		alnum := c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if !alnum && (i == 0 || c != '.' && c != '_' && c != '-') {
			return ""
		}
	}
	return string(b)
}

// problemKeepsTried reports whether op's line carries tried=: an unknown
// operation or MCP tool (C759), or the service a service.call or
// service.read named (C861: invalid_service_data reached 8 agents in a day
// and the log could not say which service), or the public handle an
// agent.get named (C870: 47 not_found lookups in a week, names unknown).
func problemKeepsTried(op string) bool {
	return op == "unknown" || op == "other" || op == "service.call" || op == "service.read" || op == "tools.call" || op == "agent.get"
}

// problemToolID is the tool id a tools.call's data names (C863: 15
// global_quota_exhausted and 9 invalid_service_data tools.call refusals in
// half a day, and the log could not say which tool), or "" when unreadable.
func problemToolID(data string) string {
	var env struct {
		Args struct {
			ID string `json:"id"`
		} `json:"args"`
	}
	if json.Unmarshal([]byte(data), &env) != nil {
		return ""
	}
	// tried= takes no ':' (it keeps host:port out), so swarmmemo:fetch.page
	// logs as swarmmemo.fetch.page.
	return strings.ReplaceAll(env.Args.ID, ":", ".")
}

// problemActor is the first 8 hex digits of sha256(id), or "anon".
func problemActor(id string) string {
	if id == "" {
		return "anon"
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:4])
}

// commandProblemLine is the log line (see the comment at the top).
// status 0 is "unknown"; tried is written only where problemKeepsTried(op).
// work is the hashed work id (problemActor of it), or "" for none.
func commandProblemLine(op, tried, code string, status int, via, actor, work string, suppressed int) string {
	op = problemName(op)
	line := "command_problem op=" + op
	if tried = problemTried(tried); tried != "" && problemKeepsTried(op) {
		line += " tried=" + tried
	}
	st := "unknown"
	if status > 0 {
		st = strconv.Itoa(status)
	}
	line += " code=" + problemName(code) + " status=" + st + " via=" + problemName(via) + " actor=" + actor
	if work != "" {
		line += " work=" + work
	}
	if suppressed > 0 {
		line += " suppressed=" + strconv.Itoa(suppressed)
	}
	return line
}

// problemLogWrite writes one line (tests capture it).
var problemLogWrite = func(line string) { slog.Info(line) }

// logCommandProblem writes the line for one failed command, rate-limited.
// status is 4xx/5xx, or 0 when the failure carried none (MCP only).
// workID, the work id a work.* operation names, is logged hashed (C836).
func logCommandProblem(op, tried, code string, status int, via, actorID, workID string) {
	if status != 0 && status < 400 {
		return
	}
	op, code = problemName(op), problemName(code)
	if tried = problemTried(tried); !problemKeepsTried(op) {
		tried = ""
	}
	ok, suppressed := commandProblems.admit(problemKey{op, code, tried}, time.Now())
	if !ok {
		return
	}
	work := ""
	if workID != "" && strings.HasPrefix(op, "work.") {
		work = problemActor(workID)
	}
	problemLogWrite(commandProblemLine(op, tried, code, status, via, problemActor(actorID), work, suppressed))
}

// problemRecorder wraps the response of a /v1/command or /call/ request:
// writeErrorNext notes the problem it writes (noteProblem), the handler
// names the operation and signer once it knows them (noteCommand), and the
// line is written when the request ends (flush).
type problemRecorder struct {
	http.ResponseWriter
	via, op, actor string
	tried          string // the unknown operation sent (noteTried)
	work           string // the work id a work.* operation names (noteWork)
	code           string
	status         int
}

func (p *problemRecorder) Unwrap() http.ResponseWriter { return p.ResponseWriter }

func (p *problemRecorder) Flush() {
	if f, ok := p.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (p *problemRecorder) flush() {
	if p.status >= 400 {
		op := p.op
		if op == "" {
			op = "unknown"
		}
		logCommandProblem(op, p.tried, p.code, p.status, p.via, p.actor, p.work)
	}
}

// findProblemRecorder walks w's wrappers to the request's recorder, if any.
func findProblemRecorder(w http.ResponseWriter) *problemRecorder {
	for i := 0; w != nil && i < 8; i++ {
		switch v := w.(type) {
		case *problemRecorder:
			return v
		case *indentWriter:
			w = v.ResponseWriter
		case interface{ Unwrap() http.ResponseWriter }:
			w = v.Unwrap()
		default:
			return nil
		}
	}
	return nil
}

// noteProblem records the problem a response carries (writeErrorNext).
func noteProblem(w http.ResponseWriter, code string, status int) {
	if p := findProblemRecorder(w); p != nil {
		p.code, p.status = code, status
	}
}

// noteCommand names the request's operation (from a fixed table only) and
// its signer's public key or account (hashed when logged).
func noteCommand(w http.ResponseWriter, op, actor string) {
	if p := findProblemRecorder(w); p != nil {
		p.op = op
		if actor != "" {
			p.actor = actor
		}
	}
}

// noteWork keeps the work id a command names (C836; hashed when logged,
// and only beside a work.* operation).
func noteWork(w http.ResponseWriter, id string) {
	if p := findProblemRecorder(w); p != nil {
		p.work = id
	}
}

// noteTried keeps the operation name a request sent that no table has
// (logged as tried= when it is a short token).
func noteTried(w http.ResponseWriter, op string) {
	if p := findProblemRecorder(w); p != nil {
		p.tried = op
	}
}

// withProblemLog runs h with a recorder that logs its problem, if any.
func withProblemLog(w http.ResponseWriter, r *http.Request, via string, h func(http.ResponseWriter, *http.Request)) {
	p := &problemRecorder{ResponseWriter: w, via: via}
	defer p.flush()
	h(p, r)
}

// logMCPProblem is the MCP side: one failed tool call (countMCPTools).
// tool is the counted name (board.MCPToolOther for one we do not serve),
// tried the name the call sent. A tool we do not serve is 404
// unknown_tool, not a server error; a failure with no status (or none in
// 4xx/5xx) is status=unknown rather than a claimed 500.
func logMCPProblem(ctx context.Context, tool, tried, code string, status int, served bool) {
	if !served {
		code, status = "unknown_tool", 404
	}
	if code == "unknown_tool" {
		tool = board.MCPToolOther // C867: also one call_swarmmemo_tool was asked for
	}
	if status < 400 || status > 599 {
		status = 0
	}
	// C867: clients namespace tool names (swarmmemo:home, swarmmemo/home);
	// tried= takes neither, so they log as swarmmemo.home.
	tried = strings.NewReplacer(":", ".", "/", ".").Replace(tried)
	logCommandProblem(tool, tried, code, status, "mcp", resolvedHostedAccount(ctx), "")
}
