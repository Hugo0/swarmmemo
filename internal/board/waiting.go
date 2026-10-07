package board

import (
	"context"
	"fmt"
	"net/netip"
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

type changeSignal struct {
	mu sync.Mutex
	ch chan struct{}
}

// Changes returns a channel closed by the next committed write. Take it before
// a read: a write that commits after the read then still wakes the waiter.
func (s *Store) Changes() <-chan struct{} {
	s.changes.mu.Lock()
	defer s.changes.mu.Unlock()
	if s.changes.ch == nil {
		s.changes.ch = make(chan struct{})
	}
	return s.changes.ch
}

func (s *Store) signalChange() {
	s.changes.mu.Lock()
	defer s.changes.mu.Unlock()
	if s.changes.ch != nil {
		close(s.changes.ch)
		s.changes.ch = nil
	}
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
// nothing to wait for, so the first answer stands.
func (s *Store) waitUpdates(ctx context.Context, cmd Command, source string, wait int) (Result, error) {
	changed := s.Changes()
	res, err := s.executeCommand(ctx, cmd, source)
	if err != nil || cmd.Cursor == "" || !caughtUp(cmd, res) {
		return res, err
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
			return Result{}, ctx.Err()
		case <-timer.C:
			return res, nil
		case <-changed:
		}
		changed = s.Changes()
		next, err := s.executeCommand(ctx, cmd, source)
		if err != nil || !caughtUp(cmd, next) {
			return next, err
		}
		res = next
	}
}

func caughtUp(cmd Command, res Result) bool {
	return len(res.Messages) == 0 && res.NextCursor == cmd.Cursor
}
