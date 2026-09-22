package transport

import (
	"encoding/base32"
	"strconv"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/httpapi"
)

// reassembly buffers DNS write chunks (RFC0007 rung C). A signed command is
// base32-encoded, split across queries named
//
//	MSGID.I.N.CHUNK[.CHUNK...].w.ZONE
//
// and answered from MSGID.status.ZONE. The source is a recursive resolver, so
// nothing here trusts or keys on it: memory is bounded globally, not per
// sender. Every limit is checked before anything is stored, a partial command
// expires, a full buffer refuses new ids rather than evicting live ones, and a
// conflicting chunk poisons its id instead of being silently replaced.
type reassembly struct {
	mu       sync.Mutex
	now      func() time.Time
	partials map[string]*partial
	bytes    int // encoded bytes held across all partials
	statuses map[string]status
}

type partial struct {
	n       int
	chunks  []string
	have    int
	size    int
	created time.Time
	source  string // the datagram source that opened it, for the per-source cap
}

type status struct {
	text string
	at   time.Time
}

const (
	writeMaxChunks   = 64
	writeMaxEncoded  = 13108 // base32 of an 8 KiB command
	writeMaxPartials = 128
	// writeMaxPerSource caps the partials one source address may hold open, so a
	// single unspoofed sender cannot fill the global table and keep every other
	// writer at "error busy". Forging many sources still can; that residual is
	// the operator's to weigh (deploy/transports-2026-09-22/README.md).
	writeMaxPerSource = 8
	writeMaxBuffered = 256 << 10
	writeExpiry      = 60 * time.Second
	writeMaxStatuses = 1024
	writeStatusTTL   = 10 * time.Minute
)

var base32Lower = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// parseWrite routes the write names. It reports false for names that are not
// write names, which the read routes then handle. Only TXT queries store a
// chunk; any other type is answered NODATA without touching the buffer.
func (d *dns) parseWrite(req *Request, sub []string, txt bool, source string) bool {
	switch {
	case len(sub) == 1 && (sub[0] == "w" || sub[0] == "status"):
		req.Route = "nodata" // empty non-terminals
		return true
	case len(sub) == 2 && sub[1] == "status":
		req.Route = "nxdomain"
		if validWriteID(sub[0]) {
			req.Route, req.Arg = "write-status", sub[0]
			if !txt {
				req.Route = "nodata"
			}
		}
		return true
	case len(sub) < 2 || sub[len(sub)-1] != "w":
		return false
	}
	req.Route = "nxdomain"
	if len(sub) < 5 || !validWriteID(sub[0]) {
		return true
	}
	n, okN := smallNumber(sub[2], writeMaxChunks)
	i, okI := smallNumber(sub[1], writeMaxChunks)
	chunk := strings.Join(sub[3:len(sub)-1], "")
	if !okN || !okI || n < 1 || i >= n || !validBase32(chunk) {
		return true
	}
	if !txt {
		req.Route = "nodata"
		return true
	}
	raw, ack := d.writes.addFrom(source, sub[0], i, n, chunk)
	if raw == nil {
		req.Route, req.Arg = "write-ack", ack
		return true
	}
	// The same strict decoder as /c64/; the board verifies the signature.
	cmd, err := httpapi.DecodeCommand(raw)
	if err != nil {
		d.writes.finish(sub[0], "error "+boardError(err).Code)
		req.Route, req.Arg = "write-status", sub[0]
		return true
	}
	req.Route, req.Arg, req.Command, req.SignedOnly = "write-done", sub[0], &cmd, true
	return true
}

func newReassembly() *reassembly {
	return &reassembly{now: time.Now, partials: map[string]*partial{}, statuses: map[string]status{}}
}

func validWriteID(id string) bool {
	if len(id) < 16 || len(id) > 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func smallNumber(s string, max int) (int, bool) {
	if s == "" || len(s) > 3 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 0 && n <= max
}

func validBase32(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return s != ""
}

// add stores one chunk. It returns the decoded command when this chunk
// completes it, or a short status for the answer.
func (r *reassembly) add(id string, i, n int, chunk string) (command []byte, answer string) {
	return r.addFrom("", id, i, n, chunk)
}

// addFrom is add with the source address that sent the chunk. An empty source
// is not capped per source (only tests and direct callers pass one).
func (r *reassembly) addFrom(source, id string, i, n int, chunk string) (command []byte, answer string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if st, done := r.statuses[id]; done && now.Sub(st.at) < writeStatusTTL {
		return nil, st.text // a retried chunk of a finished command changes nothing
	}
	p := r.partials[id]
	if p != nil && now.Sub(p.created) >= writeExpiry {
		r.drop(id)
		p = nil
	}
	if p == nil {
		r.sweep(now)
		if len(r.partials) >= writeMaxPartials {
			return nil, "error busy"
		}
		if source != "" {
			open := 0
			for _, q := range r.partials {
				if q.source == source {
					open++
				}
			}
			if open >= writeMaxPerSource {
				return nil, "error busy"
			}
		}
		p = &partial{n: n, chunks: make([]string, n), created: now, source: source}
		r.partials[id] = p
	}
	switch {
	case p.n != n:
		r.fail(id, "error conflicting_chunk_count", now)
		return nil, "error conflicting_chunk_count"
	case p.chunks[i] == chunk:
		return nil, "ok " + strconv.Itoa(p.have) + "/" + strconv.Itoa(n)
	case p.chunks[i] != "":
		r.fail(id, "error conflicting_chunk", now)
		return nil, "error conflicting_chunk"
	case p.size+len(chunk) > writeMaxEncoded:
		r.fail(id, "error command_too_large", now)
		return nil, "error command_too_large"
	case r.bytes+len(chunk) > writeMaxBuffered:
		return nil, "error busy"
	}
	p.chunks[i] = chunk
	p.have++
	p.size += len(chunk)
	r.bytes += len(chunk)
	if p.have < n {
		return nil, "ok " + strconv.Itoa(p.have) + "/" + strconv.Itoa(n)
	}
	encoded := strings.Join(p.chunks, "")
	r.drop(id)
	raw, err := base32Lower.DecodeString(encoded)
	if err != nil {
		r.record(id, "error invalid_encoding", now)
		return nil, "error invalid_encoding"
	}
	// Until the board answers, a status read says so; a panic leaves this.
	r.record(id, "error not_completed", now)
	return raw, ""
}

func (r *reassembly) lookup(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if st, ok := r.statuses[id]; ok && now.Sub(st.at) < writeStatusTTL {
		return st.text
	}
	if p, ok := r.partials[id]; ok && now.Sub(p.created) < writeExpiry {
		return "pending " + strconv.Itoa(p.have) + "/" + strconv.Itoa(p.n)
	}
	return "unknown"
}

func (r *reassembly) record(id, text string, now time.Time) {
	if _, ok := r.statuses[id]; !ok && len(r.statuses) >= writeMaxStatuses {
		r.sweep(now)
		if len(r.statuses) >= writeMaxStatuses {
			// Statuses are informational; the oldest one goes.
			oldest, at := "", now
			for k, v := range r.statuses {
				if v.at.Before(at) || oldest == "" {
					oldest, at = k, v.at
				}
			}
			delete(r.statuses, oldest)
		}
	}
	r.statuses[id] = status{text: text, at: now}
}

// finish records the board's answer for a completed command.
func (r *reassembly) finish(id, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record(id, text, r.now())
}

func (r *reassembly) fail(id, text string, now time.Time) {
	r.drop(id)
	r.record(id, text, now)
}

func (r *reassembly) drop(id string) {
	if p, ok := r.partials[id]; ok {
		r.bytes -= p.size
		delete(r.partials, id)
	}
}

func (r *reassembly) sweep(now time.Time) {
	for id, p := range r.partials {
		if now.Sub(p.created) >= writeExpiry {
			r.drop(id)
		}
	}
	for id, st := range r.statuses {
		if now.Sub(st.at) >= writeStatusTTL {
			delete(r.statuses, id)
		}
	}
}
