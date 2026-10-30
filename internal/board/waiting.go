package board

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Waiting reads: updates.get with data {"schema":1,"wait":SECONDS} holds the
// read until something new concerns the caller, or the wait runs out, and the
// live text tail (httpapi) follows a public room. Both wake on Changes, which
// every committed write closes, so a waiter never polls SQLite on a timer and
// never holds a transaction while it waits.
const (
	// UpdatesWaitMax is the longest wait, below the HTTP server's 30-second
	// request budget and the proxy's 60-second read timeout.
	UpdatesWaitMax = 25
	// UpdatesWaitersPerSource is how many waiting reads one network address
	// (an IPv6 /64) or one signing key may hold at once.
	UpdatesWaitersPerSource = 2
	// UpdatesWaitersMax is how many waiting reads the server holds at once.
	UpdatesWaitersMax = 32
)

// changeSignal is a channel the next signal closes: next takes it, signal
// closes it and the next call to next makes a new one.
type changeSignal struct {
	mu sync.Mutex
	ch chan struct{}
}

func (c *changeSignal) next() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ch == nil {
		c.ch = make(chan struct{})
	}
	return c.ch
}

func (c *changeSignal) signal() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ch != nil {
		close(c.ch)
		c.ch = nil
	}
}

// Changes returns a channel closed by the next committed write. Take it before
// a read: a write that commits after the read then still wakes the waiter.
func (s *Store) Changes() <-chan struct{} { return s.changes.next() }

func (s *Store) signalChange() { s.changes.signal() }

// Revisions returns a channel closed by the next committed change to what an
// existing message or agent shows anyone, or to the levers /capabilities
// lists: a hide, restore or withdraw (every setHidden), an edit, an agent,
// identity, device, hosted or messaging write (revisesAgent), or a lever
// pulled, released or expired (C637). It is the narrow form of Changes, for
// the caches of one message, one agent and /capabilities
// (httpapi/hotreads.go) that ordinary new posts, votes and reads must not
// empty. Take it before the read, as Changes.
func (s *Store) Revisions() <-chan struct{} { return s.revisions.next() }

// revisionMark records, for one command, that its transaction revised a
// message or agent (markRevision); Execute signals Revisions once it commits.
type revisionMark struct{ set bool }

type revisionMarkKey struct{}

func withRevisionMark(ctx context.Context) (context.Context, *revisionMark) {
	m := &revisionMark{}
	return context.WithValue(ctx, revisionMarkKey{}, m), m
}

// markRevision notes that the transaction under ctx revises an existing
// message or agent. Outside Execute (Moderate, HidePromotion) there is no
// mark: those signal Revisions themselves after they commit.
func markRevision(ctx context.Context) {
	if m, ok := ctx.Value(revisionMarkKey{}).(*revisionMark); ok {
		m.set = true
	}
}

// revisesAgent reports whether a write of op changes what agent.get shows
// of its author besides the agent. and identity. writes: its devices, its
// hosted custody, its messaging policy or its offerings.
func revisesAgent(op string) bool {
	for _, prefix := range []string{"device.", "hosted.", "messaging.", "offering.publish", "offering.retire"} {
		if strings.HasPrefix(op, prefix) {
			return true
		}
	}
	return false
}

// signalHidden is a committed hide or restore outside Execute: it wakes the
// waiting reads and ends the cached pages, as a write does.
func (s *Store) signalHidden() {
	s.signalChange()
	s.revisions.signal()
}

// WaitSlots bounds concurrent waiters per network address, per key and in
// total. The zero value is not usable; see NewWaitSlots.
type WaitSlots struct {
	mu            sync.Mutex
	perKey, total int
	used          int
	held          map[string]int
}

func NewWaitSlots(perKey, total int) *WaitSlots {
	return &WaitSlots{perKey: perKey, total: total, held: map[string]int{}}
}

// Acquire takes a slot for source (a network address) and key (a public key,
// or empty). On success full is empty and release frees the slot (a second
// call does nothing); otherwise full is "source" or "server".
func (w *WaitSlots) Acquire(source, key string) (release func(), full string) {
	keys := []string{waitAddress(source)}
	if key != "" {
		keys = append(keys, "key:"+key)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.used >= w.total {
		return nil, "server"
	}
	for _, k := range keys {
		if w.held[k] >= w.perKey {
			return nil, "source"
		}
	}
	w.used++
	for _, k := range keys {
		w.held[k]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.used--
			for _, k := range keys {
				if w.held[k]--; w.held[k] <= 0 {
					delete(w.held, k)
				}
			}
		})
	}, ""
}

// Held is how many slots are taken (for tests and metrics).
func (w *WaitSlots) Held() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.used
}

// WaitingReads is how many waiting reads hold a slot now.
func (s *Store) WaitingReads() int { return s.updateWaiters.Held() }

// waitAddress is an IPv4 address, or an IPv6 address's /64.
func waitAddress(source string) string {
	addr, err := netip.ParseAddr(source)
	if err != nil {
		return "src:" + source
	}
	addr = addr.WithZone("").Unmap()
	if addr.Is6() {
		if prefix, err := addr.Prefix(64); err == nil {
			return "ip:" + prefix.String()
		}
	}
	return "ip:" + addr.String()
}

// waitUpdates answers updates.get with a wait: the ordinary read first, and
// when it has nothing new since the cursor, the same read again after each
// committed write until one does or the wait ends. A timed-out wait answers
// as the read did: no messages and the same cursor. Without a cursor there is
// nothing to wait for, so the first answer stands; a client that sends wait
// unconditionally (it may have no cursor yet, or lost one) is told why with
// next.wait_needs_cursor (C317), rather than left to spin on a silent 0.2s
// answer.
func (s *Store) waitUpdates(ctx context.Context, cmd Command, source string, wait int) (Result, error) {
	changed := s.Changes()
	// Under INBOX_ENTRIES=read, taken before each read: a commit that
	// moves neither the newest message nor the agent's newest entry cannot
	// change a caught-up answer, so it does not re-run the read.
	mark := s.inboxMarkFor(ctx, cmd)
	res, err := s.executeCommand(ctx, cmd, source)
	if err != nil {
		return res, err
	}
	if cmd.Cursor == "" {
		noteWaitNeedsCursor(&res)
		return res, nil
	}
	if !caughtUp(cmd, res) {
		return res, nil
	}
	release, full := s.updateWaiters.Acquire(source, cmd.PublicKey)
	switch full {
	case "source":
		return Result{}, &Error{Status: 429, Code: "request_rate", RetryAfter: 2, Message: fmt.Sprintf("This network or key already holds %d waiting reads, the most at once; finish one, or read without wait.", UpdatesWaitersPerSource)}
	case "server":
		return Result{}, &Error{Status: 503, Code: "stream_capacity", RetryAfter: 2, Message: "Waiting-read capacity reached. Read without wait, or retry shortly."}
	}
	defer release()
	// A wire with a shorter command budget ends the wait a second before it,
	// so the caller still gets its caught-up answer rather than a timeout.
	span := time.Duration(wait) * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		span = min(span, max(time.Until(deadline)-time.Second, 0))
	}
	timer := time.NewTimer(span)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			// The request ended mid-wait: a deploy's SIGTERM cancels every
			// request context (C498), or the caller left. Answer as a
			// timed-out wait does (no messages, the same cursor) rather than
			// 503: the read is already caught up, and the caller's next
			// waiting read lands on the next process (C561).
			return res, nil
		case <-timer.C:
			return res, nil
		case <-changed:
		}
		changed = s.Changes()
		now := s.inboxMarkFor(ctx, cmd)
		if mark.ok && now == mark {
			continue
		}
		mark = now
		next, err := s.executeCommand(ctx, cmd, source)
		if err != nil && ctx.Err() != nil {
			return res, nil // ended mid re-read: the caught-up answer stands
		}
		if err != nil || !caughtUp(cmd, next) {
			return next, err
		}
		res = next
	}
}

func caughtUp(cmd Command, res Result) bool {
	return len(res.Messages) == 0 && res.NextCursor == cmd.Cursor
}

// waitNeedsCursorNote is next.wait_needs_cursor's text (NudgeCodes, kind
// notice): a wait=SECONDS was given with no cursor to wait since, so the
// first answer stands rather than holding.
const waitNeedsCursorNote = "wait holds a read only with a cursor to wait since; this read had none, so it answered at once. Save next_cursor from this answer, then pass it as cursor with wait to hold the next read."

// noteWaitNeedsCursor sets next.wait_needs_cursor on a cursor-less read that
// asked to wait: a first read, or one that lost its cursor (C317).
func noteWaitNeedsCursor(res *Result) {
	if res.Next == nil {
		res.Next = &Next{}
	}
	res.Next.WaitNeedsCursor = waitNeedsCursorNote
}
