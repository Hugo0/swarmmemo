// Package fakerelay is an in-process Nostr relay for tests: an httptest
// server speaking just enough NIP-01 to exercise the bridge offline.
package fakerelay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Relay records what clients send and lets a test push frames to them.
type Relay struct {
	server *httptest.Server
	mu     sync.Mutex
	conns  map[*websocket.Conn]string // connection -> its subscription id
	reqs   []json.RawMessage          // REQ filters received
	events []json.RawMessage          // EVENT objects published by clients
	accept bool                       // answer publishes with OK true
	change chan struct{}
}

// New starts a relay; Close stops it.
func New() *Relay {
	r := &Relay{conns: map[*websocket.Conn]string{}, accept: true, change: make(chan struct{}, 1)}
	r.server = httptest.NewServer(http.HandlerFunc(r.serve))
	return r
}

// URL is the relay's ws:// address.
func (r *Relay) URL() string { return "ws" + strings.TrimPrefix(r.server.URL, "http") }

// Close drops every client and stops the server.
func (r *Relay) Close() {
	r.Drop()
	r.server.Close()
}

func (r *Relay) notify() {
	select {
	case r.change <- struct{}{}:
	default:
	}
}

func (r *Relay) serve(w http.ResponseWriter, req *http.Request) {
	c, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20)
	r.mu.Lock()
	r.conns[c] = ""
	r.mu.Unlock()
	r.notify()
	defer func() {
		r.mu.Lock()
		delete(r.conns, c)
		r.mu.Unlock()
		c.CloseNow()
		r.notify()
	}()
	for {
		_, data, err := c.Read(req.Context())
		if err != nil {
			return
		}
		var parts []json.RawMessage
		if json.Unmarshal(data, &parts) != nil || len(parts) < 2 {
			continue
		}
		var label string
		_ = json.Unmarshal(parts[0], &label)
		switch label {
		case "REQ":
			var sub string
			_ = json.Unmarshal(parts[1], &sub)
			r.mu.Lock()
			r.conns[c] = sub
			if len(parts) > 2 {
				r.reqs = append(r.reqs, parts[2])
			}
			r.mu.Unlock()
		case "EVENT":
			var e struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(parts[1], &e)
			r.mu.Lock()
			r.events = append(r.events, parts[1])
			ok := r.accept
			r.mu.Unlock()
			reply, _ := json.Marshal([]any{"OK", e.ID, ok, ""})
			ctx, cancel := context.WithTimeout(req.Context(), time.Second)
			_ = c.Write(ctx, websocket.MessageText, reply)
			cancel()
		}
		r.notify()
	}
}

// Subscribed is how many connections have an open subscription.
func (r *Relay) Subscribed() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, sub := range r.conns {
		if sub != "" {
			n++
		}
	}
	return n
}

// SubscriptionID is one open subscription's id, or "".
func (r *Relay) SubscriptionID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, sub := range r.conns {
		if sub != "" {
			return sub
		}
	}
	return ""
}

// Filters are the REQ filters received so far.
func (r *Relay) Filters() []json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]json.RawMessage(nil), r.reqs...)
}

// Published are the events clients sent.
func (r *Relay) Published() []json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]json.RawMessage(nil), r.events...)
}

// SendEvent delivers an event object to every subscription.
func (r *Relay) SendEvent(event []byte) {
	r.each(func(sub string) []byte {
		frame, _ := json.Marshal([]any{"EVENT", sub, json.RawMessage(event)})
		return frame
	})
}

// SendRaw sends one frame, as is, to every subscribed connection.
func (r *Relay) SendRaw(frame []byte) { r.each(func(string) []byte { return frame }) }

func (r *Relay) each(frame func(sub string) []byte) {
	r.mu.Lock()
	targets := map[*websocket.Conn]string{}
	for c, sub := range r.conns {
		if sub != "" {
			targets[c] = sub
		}
	}
	r.mu.Unlock()
	for c, sub := range targets {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = c.Write(ctx, websocket.MessageText, frame(sub))
		cancel()
	}
}

// Drop closes every client connection, as a relay restart would.
func (r *Relay) Drop() {
	r.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(r.conns))
	for c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.Unlock()
	for _, c := range conns {
		c.CloseNow()
	}
}

// Wait polls cond until it holds or the timeout passes.
func (r *Relay) Wait(timeout time.Duration, cond func() bool) bool {
	deadline := time.After(timeout)
	for !cond() {
		select {
		case <-r.change:
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			return cond()
		}
	}
	return true
}
