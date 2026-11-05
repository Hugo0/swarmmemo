package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// C757: the operator's private view of failed commands. A command that
// ends in a problem (4xx/5xx) on POST /v1/command, a /call/ (or /c64/)
// route or an MCP tool call writes one server-log line (journald):
//
//	command_problem op=OP code=CODE status=N via=command|call|mcp actor=HEX8|anon [suppressed=N]
//
// Only names from fixed tables (the operation, the problem code), the
// status and a short hash of the signer: never text, payloads, message
// ids, IPs, tokens or headers. Per (op, code) at most one line per
// problemLogEvery; the lines held back are counted into the next one.
// Nothing reaches a response, a route or the database.

const (
	problemLogEvery = 10 * time.Second
	problemLogKeys  = 1024 // bound on the limiter's table
)

type problemKey struct{ op, code string }

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

// problemActor is the first 8 hex digits of sha256(id), or "anon".
func problemActor(id string) string {
	if id == "" {
		return "anon"
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:4])
}

// commandProblemLine is the log line (see the comment at the top).
func commandProblemLine(op, code string, status int, via, actor string, suppressed int) string {
	line := "command_problem op=" + problemName(op) + " code=" + problemName(code) + " status=" + strconv.Itoa(status) + " via=" + problemName(via) + " actor=" + actor
	if suppressed > 0 {
		line += " suppressed=" + strconv.Itoa(suppressed)
	}
	return line
}

// logCommandProblem writes the line for one failed command, rate-limited.
func logCommandProblem(op, code string, status int, via, actorID string) {
	if status < 400 {
		return
	}
	op, code = problemName(op), problemName(code)
	ok, suppressed := commandProblems.admit(problemKey{op, code}, time.Now())
	if !ok {
		return
	}
	slog.Info(commandProblemLine(op, code, status, via, problemActor(actorID), suppressed))
}

// problemRecorder wraps the response of a /v1/command or /call/ request:
// writeErrorNext notes the problem it writes (noteProblem), the handler
// names the operation and signer once it knows them (noteCommand), and the
// line is written when the request ends (flush).
type problemRecorder struct {
	http.ResponseWriter
	via, op, actor string
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
		logCommandProblem(op, p.code, p.status, p.via, p.actor)
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

// withProblemLog runs h with a recorder that logs its problem, if any.
func withProblemLog(w http.ResponseWriter, r *http.Request, via string, h func(http.ResponseWriter, *http.Request)) {
	p := &problemRecorder{ResponseWriter: w, via: via}
	defer p.flush()
	h(p, r)
}

// logMCPProblem is the MCP side: one failed tool call (countMCPTools).
func logMCPProblem(ctx context.Context, tool, code string, status int) {
	if status < 400 || status > 599 {
		status = 500
	}
	logCommandProblem(tool, code, status, "mcp", resolvedHostedAccount(ctx))
}
