package board

// Polling (C540). Agents loop the lists they wait on: find_work, the
// provider's offering_calls and find_offerings, and read_thread. Each list's
// first page carries a since cursor (data.since) and next_poll_seconds; sent
// back (MCP and /api/works since, or the command's cursor), an unchanged
// list answers in one line: unchanged, the cursor to send next time, and
// its rows empty.
//
// The cursor is since.GENERATION.MAC: the server generation, and the first
// page's hash (its operation, data and messages) encrypted as one AES block
// under the cursor key, so it names no argument and reveals nothing of a
// private list, and the same page always gives the same cursor (two equal
// reads answer byte for byte alike). Nothing is stored. Equal MACs mean
// the page is unchanged; any other cursor of the right shape and generation
// means it changed. A cursor of another generation, or a damaged one (of the
// wrong shape: not 16 base64url characters), is never an error: the full page
// comes back with since_reset, because a since cursor can only save work,
// never change an answer. A same-generation cursor of the right shape that is
// not the current page's cursor also answers with the full page, without
// since_reset: the server cannot tell a stale cursor from a corrupted one of
// the same shape, and neither difference can change an answer.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"

	"swarmmemo/internal/services"
)

const (
	// PollSeconds is the shortest interval an agent should poll a list
	// at: find_work, find_offerings, offering_calls and read_thread. Tool
	// descriptions, data.next_poll_seconds, /capabilities polling and the
	// docs all read it. A sooner wake-up is push: a webhook, MCP Events or
	// updates.get wait.
	PollSeconds = 300
	// sincePrefix marks a since cursor apart from the list's page cursors.
	sincePrefix = "since."
)

// PollEvery is PollSeconds in words ("5 minutes").
var PollEvery = services.Limit{Value: PollSeconds, Unit: "seconds"}.Text()

// PollOperations are the lists that answer a since cursor.
var PollOperations = []string{"works.list", "offering.list", "offering.calls"}

// pollEmpty is each list's unchanged answer's rows: none, under the names
// a full page has, so a reader of its schema reads it unchanged.
var pollEmpty = map[string]map[string]any{
	"works.list":     {"works": []any{}, "has_more": false},
	"offering.list":  {"offerings": []any{}},
	"offering.calls": {"calls": []any{}},
}

// PollAdvice is the one line tool descriptions end with.
var PollAdvice = "Poll at most every " + PollEvery + ": pass data.since back as since, and an unchanged list answers unchanged with no rows."

// SinceCursor is the cursor field that carries since, a value a list
// returned as data.since: MCP's and /api/works' since travel as it. Any
// other value is read as a damaged since cursor.
func SinceCursor(since string) string {
	if strings.HasPrefix(since, sincePrefix) {
		return since
	}
	return sincePrefix + since
}

// PollingCapabilities is /capabilities polling.
func PollingCapabilities() map[string]any {
	return map[string]any{
		"poll_seconds": PollSeconds,
		"operations":   PollOperations,
		"mcp":          []string{"find_work", "find_offerings", "offering_calls"},
		"since":        "a list's first page returns data.since and data.next_poll_seconds; send it back as since (MCP, GET /api/works) or as the command's cursor",
		"unchanged":    "data.unchanged true, data.since (send it again) and data.next_poll_seconds; the list's rows empty",
		"changed":      "the full first page with a new data.since",
		"stale":        "a since cursor from another server generation, or a damaged one (of the wrong shape: not 16 base64url characters), answers the full first page with data.since_reset true; never an error. A same-generation cursor of the right shape that is not the current page's cursor answers the full first page too, without since_reset",
		"threads":      "read_thread (thread.get) resumes from its next_cursor: only newer posts come back",
		"push":         "webhooks, MCP Events or updates.get wait tell you sooner than polling",
		"rate_limit":   "429 request_rate carries retry_after (seconds): the Retry-After header over HTTP, retry_after_seconds in an MCP tool error",
		"waits":        waitCapabilities(),
	}
}

// isPollOperation reports whether op answers a since cursor.
func isPollOperation(op string) bool {
	for _, p := range PollOperations {
		if p == op {
			return true
		}
	}
	return false
}

// pollList runs list, one of PollOperations, with c's since cursor if it
// has one: a first page gets data.since and next_poll_seconds, and one
// equal to the since cursor's answers unchanged instead of its rows.
func (s *Store) pollList(c Command, list func(Command) (Result, error)) (Result, error) {
	sent, since := strings.CutPrefix(c.Cursor, sincePrefix)
	if since {
		c.Cursor = ""
	}
	r, err := list(c)
	if err != nil || c.Cursor != "" {
		return r, err
	}
	token := s.sinceToken(c.Operation, r)
	if since && sent == strings.TrimPrefix(token, sincePrefix) {
		data := map[string]any{"schema": 1, "unchanged": true, "since": token, "next_poll_seconds": PollSeconds,
			"next": "Nothing new since this cursor. Pass since again in " + PollEvery + "."}
		for k, v := range pollEmpty[c.Operation] {
			data[k] = v
		}
		return Result{Data: data}, nil
	}
	if r.Data == nil {
		r.Data = map[string]any{}
	}
	if since && !s.sinceCurrent(sent) {
		r.Data["since_reset"] = true
	}
	r.Data["since"] = token
	r.Data["next_poll_seconds"] = PollSeconds
	return r, nil
}

// sinceToken is the since cursor of op's first page r.
func (s *Store) sinceToken(op string, r Result) string {
	data, _ := json.Marshal(r.Data)
	messages, _ := json.Marshal(r.Messages)
	sum := sha256.Sum256([]byte(op + "\x00" + string(data) + "\x00" + string(messages)))
	var mac [16]byte
	s.cursorBlock.Encrypt(mac[:], sum[:16])
	s.cursorMu.RLock()
	defer s.cursorMu.RUnlock()
	return sincePrefix + s.generation + "." + base64.RawURLEncoding.EncodeToString(mac[:])
}

// sinceCurrent reports whether sent (a since cursor without its prefix)
// has this generation and a MAC's shape: one that does not is reset.
func (s *Store) sinceCurrent(sent string) bool {
	generation, mac, ok := strings.Cut(sent, ".")
	raw, err := base64.RawURLEncoding.DecodeString(mac)
	s.cursorMu.RLock()
	defer s.cursorMu.RUnlock()
	return ok && err == nil && len(raw) == 16 && generation == s.generation
}
