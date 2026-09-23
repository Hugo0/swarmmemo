package nostr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Relay client bounds. Everything a relay can make this process hold is
// capped: one frame, the per-relay outbox, and the time any call may take.
const (
	MaxRelays    = 8
	maxFrame     = MaxEventBytes + 4096 // one event plus its envelope
	outboxSize   = 32                   // queued publishes per relay
	dialTimeout  = 15 * time.Second
	writeTimeout = 10 * time.Second
	pingEvery    = 45 * time.Second
	minBackoff   = 2 * time.Second
	maxBackoff   = 5 * time.Minute
	stableAfter  = time.Minute // a session this long resets the backoff
)

// ValidateRelayURL accepts wss:// URLs, and ws:// only to a loopback host.
func ValidateRelayURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return "", fmt.Errorf("relay %q must be a wss:// URL without credentials, query or fragment", raw)
	}
	switch u.Scheme {
	case "wss":
	case "ws":
		host := u.Hostname()
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", fmt.Errorf("relay %q: plain ws:// is allowed only to loopback", raw)
		}
	default:
		return "", fmt.Errorf("relay %q must use wss://", raw)
	}
	return u.String(), nil
}

// PoolConfig configures a Pool.
type PoolConfig struct {
	Relays []string
	// Filter, when set, is subscribed on every connection; a fresh "since"
	// is set on each (re)connect.
	Filter map[string]any
	// Since is how far back each subscription reaches.
	Since time.Duration
	// OnEvent receives one raw event object from a subscription, on that
	// relay's reader goroutine. It must return promptly; reading waits for it.
	OnEvent func(relay string, raw json.RawMessage)
	// Now is the clock; tests may replace it.
	Now func() time.Time
}

// Stats are the pool's counters.
type Stats struct {
	Connects, Disconnects, Frames, BadFrames, Notices, Closed atomic.Int64
	Published, Accepted, Refused, Dropped                     atomic.Int64
}

// Pool keeps one connection per relay, reconnecting with jittered backoff.
type Pool struct {
	cfg    PoolConfig
	relays []*relay
	Stats  Stats
	subID  string
}

type relay struct {
	url       string
	out       chan []byte
	connected atomic.Bool
}

// NewPool validates the relay list. It connects nothing; Run does.
func NewPool(cfg PoolConfig) (*Pool, error) {
	if len(cfg.Relays) == 0 || len(cfg.Relays) > MaxRelays {
		return nil, fmt.Errorf("configure 1-%d relays", MaxRelays)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	p := &Pool{cfg: cfg, subID: fmt.Sprintf("swarmmemo-%08x", rand.Uint32())}
	seen := map[string]bool{}
	for _, raw := range cfg.Relays {
		u, err := ValidateRelayURL(raw)
		if err != nil {
			return nil, err
		}
		if seen[u] {
			continue
		}
		seen[u] = true
		p.relays = append(p.relays, &relay{url: u, out: make(chan []byte, outboxSize)})
	}
	return p, nil
}

// URLs are the validated relay URLs.
func (p *Pool) URLs() []string {
	out := make([]string, len(p.relays))
	for i, r := range p.relays {
		out[i] = r.url
	}
	return out
}

// Connected is how many relays have a live session.
func (p *Pool) Connected() int {
	n := 0
	for _, r := range p.relays {
		if r.connected.Load() {
			n++
		}
	}
	return n
}

// Run keeps every relay connected until ctx ends, then returns once every
// relay goroutine has stopped.
func (p *Pool) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, r := range p.relays {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.keep(ctx, r)
		}()
	}
	wg.Wait()
}

// Publish queues one signed event for every relay. It never blocks: a relay
// whose outbox is full drops the event and counts it.
func (p *Pool) Publish(e Event) error {
	frame, err := json.Marshal([]any{"EVENT", e})
	if err != nil {
		return err
	}
	for _, r := range p.relays {
		select {
		case r.out <- frame:
		default:
			p.Stats.Dropped.Add(1)
		}
	}
	return nil
}

func (p *Pool) keep(ctx context.Context, r *relay) {
	backoff := minBackoff
	for ctx.Err() == nil {
		started := time.Now()
		err := p.session(ctx, r)
		r.connected.Store(false)
		if ctx.Err() != nil {
			return
		}
		p.Stats.Disconnects.Add(1)
		slog.Info("Nostr relay disconnected", "relay", r.url, "error", short(err))
		if time.Since(started) > stableAfter {
			backoff = minBackoff
		}
		// Full jitter in [backoff/2, backoff), so relays that dropped together
		// do not all return at once.
		wait := backoff/2 + time.Duration(rand.Int64N(int64(backoff/2)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// session is one connection: dial, subscribe, then read, write and ping until
// something fails. Any panic ends the session, not the process.
func (p *Pool) session(ctx context.Context, r *relay) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("session panicked: %v", v)
		}
	}()
	dialCtx, cancelDial := context.WithTimeout(ctx, dialTimeout)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("relay redirects are not followed") }}
	conn, _, err := websocket.Dial(dialCtx, r.url, &websocket.DialOptions{HTTPClient: client, CompressionMode: websocket.CompressionDisabled})
	cancelDial()
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxFrame)
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	write := func(frame []byte) error {
		wctx, cancelWrite := context.WithTimeout(sctx, writeTimeout)
		defer cancelWrite()
		return conn.Write(wctx, websocket.MessageText, frame)
	}
	if p.cfg.Filter != nil {
		filter := make(map[string]any, len(p.cfg.Filter)+1)
		for k, v := range p.cfg.Filter {
			filter[k] = v
		}
		filter["since"] = p.cfg.Now().Add(-p.cfg.Since).Unix()
		req, err := json.Marshal([]any{"REQ", p.subID, filter})
		if err != nil {
			return err
		}
		if err = write(req); err != nil {
			return err
		}
	}
	r.connected.Store(true)
	p.Stats.Connects.Add(1)
	failed := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() {
			if v := recover(); v != nil {
				failed <- fmt.Errorf("writer panicked: %v", v)
			}
		}()
		ping := time.NewTicker(pingEvery)
		defer ping.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case frame := <-r.out:
				if err := write(frame); err != nil {
					failed <- err
					return
				}
				p.Stats.Published.Add(1)
			case <-ping.C:
				pctx, cancelPing := context.WithTimeout(sctx, writeTimeout)
				err := conn.Ping(pctx)
				cancelPing()
				if err != nil {
					failed <- err
					return
				}
			}
		}
	}()
	go func() {
		defer wg.Done()
		defer func() {
			if v := recover(); v != nil {
				failed <- fmt.Errorf("reader panicked: %v", v)
			}
		}()
		for {
			typ, data, err := conn.Read(sctx)
			if err != nil {
				failed <- err
				return
			}
			p.Stats.Frames.Add(1)
			if typ != websocket.MessageText {
				p.Stats.BadFrames.Add(1)
				continue
			}
			if err := p.frame(r, data); err != nil {
				failed <- err
				return
			}
		}
	}()
	select {
	case err = <-failed:
	case <-ctx.Done():
		err = ctx.Err()
	}
	// Stop both halves before returning, so no goroutine outlives its session.
	cancel()
	conn.CloseNow()
	wg.Wait()
	return err
}

// frame handles one relay message. Only a closed subscription ends the
// session (so it is reopened); anything malformed is counted and skipped.
func (p *Pool) frame(r *relay, data []byte) error {
	var parts []json.RawMessage
	if json.Unmarshal(data, &parts) != nil || len(parts) < 2 || len(parts) > 4 {
		p.Stats.BadFrames.Add(1)
		return nil
	}
	var label string
	if json.Unmarshal(parts[0], &label) != nil {
		p.Stats.BadFrames.Add(1)
		return nil
	}
	switch label {
	case "EVENT":
		var sub string
		if len(parts) != 3 || json.Unmarshal(parts[1], &sub) != nil || sub != p.subID || p.cfg.OnEvent == nil {
			p.Stats.BadFrames.Add(1)
			return nil
		}
		p.cfg.OnEvent(r.url, parts[2])
	case "OK":
		var accepted bool
		if len(parts) >= 3 && json.Unmarshal(parts[2], &accepted) == nil && accepted {
			p.Stats.Accepted.Add(1)
		} else {
			p.Stats.Refused.Add(1)
		}
	case "CLOSED":
		var sub string
		if json.Unmarshal(parts[1], &sub) == nil && sub == p.subID {
			p.Stats.Closed.Add(1)
			return errors.New("relay closed the subscription")
		}
	case "NOTICE":
		p.Stats.Notices.Add(1)
	case "EOSE", "AUTH":
	default:
		p.Stats.BadFrames.Add(1)
	}
	return nil
}

func short(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
