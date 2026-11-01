package board

// The read-timeout line (C700). A read with a two-second query budget that
// runs out answers 503 and logs one WARN line: the operation, the read's
// filters, the signer's account cut to 12 hex (or "anon") and the elapsed
// milliseconds, so a timeout that only one client sees can be told apart
// from a slow database. It logs no text, bodies or full keys, and at most one
// line per operation per readTimeoutLogEvery; the next line counts the ones
// it held back.

import (
	"log/slog"
	"regexp"
	"sync"
	"time"
)

const readTimeoutLogEvery = 10 * time.Second

type readTimeoutLog struct {
	mu   sync.Mutex
	last map[string]time.Time
	held map[string]int
	warn func(msg string, args ...any) // nil: slog.Warn; tests set it
}

var logShortHexRE = regexp.MustCompile(`^[0-9a-f]{12,}$`)

// logShort is an identifier fit for a log line: hex (a fingerprint, a work
// ID) cut to 12, a handle as written, anything else only as "other".
func logShort(v string) string {
	switch {
	case v == "":
		return ""
	case logShortHexRE.MatchString(v):
		return v[:12]
	case handleRE.MatchString(v) && len(v) <= 32:
		return v
	}
	return "other"
}

func (s *Store) logReadTimeout(operation string, shape []any, a actor, start time.Time) {
	l := &s.readTimeouts
	now := time.Now()
	l.mu.Lock()
	if l.last == nil {
		l.last, l.held = map[string]time.Time{}, map[string]int{}
	}
	if t, ok := l.last[operation]; ok && now.Sub(t) < readTimeoutLogEvery {
		l.held[operation]++
		l.mu.Unlock()
		return
	}
	l.last[operation] = now
	held := l.held[operation]
	delete(l.held, operation)
	warn := l.warn
	l.mu.Unlock()
	signer := "anon"
	if a.signed && a.account != "" {
		signer = logShort(a.account)
	}
	attrs := append([]any{"operation", operation, "signer", signer, "elapsed_ms", now.Sub(start).Milliseconds()}, shape...)
	if held > 0 {
		attrs = append(attrs, "held_back", held)
	}
	if warn == nil {
		warn = slog.Warn
	}
	warn("read timeout", attrs...)
}
