package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/board"
	"swarmmemo/internal/httpapi"
	"swarmmemo/internal/nostr"
)

// The Nostr bridge (RFC0007) is a reissuing bridge, not a listener: it keeps
// outbound WebSocket connections to configured relays.
//
// Inbound, a kind-1 event tagged ["t","swarmmemo"] becomes an anonymous post
// whose allowance is keyed on the event's Nostr key and whose provenance the
// service records (board.Forwarded). The Nostr signature is verified here, as
// admission; it authorises nothing on the board, where the post is unsigned.
// It enters through the same policy and board call as every other wire.
//
// Outbound (a separate switch), public top-level posts are mirrored as kind-1
// events signed by the bridge's own key, each naming the SwarmMemo message,
// its body hash and its author so a reader can check it against the board.
const (
	nostrTag           = "swarmmemo"
	nostrRoomPrefix    = "swarmmemo-"
	nostrWindow        = 10 * time.Minute // created_at must be this close to now
	nostrSeen          = 8192             // event ids remembered for dedupe
	nostrKeyBurst      = 5                // posts per Nostr key, then one per minute
	nostrKeyRate       = 1.0 / 60
	nostrBridgeBurst   = 20 // posts through the bridge as a whole, then one per 5 s
	nostrBridgeRate    = 0.2
	nostrDailyBytes    = 4 << 20 // content the bridge as a whole may post per UTC day
	nostrQueue         = 64      // mirror posts waiting to publish
	nostrMirrorText    = 1000
	nostrPollEvery     = 15 * time.Second
	nostrHold          = time.Minute // a post waits this long, for moderation, before mirroring
	nostrPublishGap    = 10 * time.Second
	nostrMirrorTagName = "swarmmemo" // ["swarmmemo", message id, body sha256, author]
)

type nostrBridge struct {
	core      *Core
	pool      *nostr.Pool
	key       *nostr.Key // nil unless publishing
	keyHex    string
	publicURL string
	stats     *counters
	perRelay  *httpapi.Limiter
	perKey    *httpapi.Limiter
	bridge    *httpapi.Limiter
	seen      *seenSet
	daily     dailyBudget
	queue     chan board.Message
	dropped   atomic.Int64
	mirrored  atomic.Int64
	now       func() time.Time
	pollEvery time.Duration
	hold      time.Duration
	gap       time.Duration
}

func newNostrBridge(c *Core, cfg Config) (*nostrBridge, error) {
	b := &nostrBridge{core: c, publicURL: strings.TrimRight(cfg.PublicURL, "/"), stats: &counters{},
		perRelay: httpapi.NewLimiter(), perKey: httpapi.NewLimiterRate(nostrKeyBurst, nostrKeyRate),
		bridge: httpapi.NewLimiterRate(nostrBridgeBurst, nostrBridgeRate), seen: newSeenSet(nostrSeen), daily: dailyBudget{max: nostrDailyBytes},
		now: time.Now, pollEvery: nostrPollEvery, hold: nostrHold, gap: nostrPublishGap}
	if b.publicURL == "" {
		b.publicURL = "https://" + cfg.Host
	}
	if cfg.NostrPublish {
		if cfg.NostrKeyFile == "" {
			return nil, errors.New("SWARMMEMO_NOSTR_PUBLISH needs SWARMMEMO_NOSTR_KEY_FILE (create it with: swarmmemo nostr keygen FILE)")
		}
		k, err := nostr.LoadKeyFile(cfg.NostrKeyFile)
		if err != nil {
			return nil, err
		}
		b.key, b.keyHex = &k, k.PublicHex()
		b.queue = make(chan board.Message, nostrQueue)
	}
	pool, err := nostr.NewPool(nostr.PoolConfig{Relays: cfg.NostrRelays, Since: nostrWindow, OnEvent: b.receive,
		Filter: map[string]any{"kinds": []int{1}, "#t": []string{nostrTag}}, Now: func() time.Time { return b.now() }})
	if err != nil {
		return nil, fmt.Errorf("SWARMMEMO_NOSTR_RELAYS: %w", err)
	}
	b.pool = pool
	return b, nil
}

func (b *nostrBridge) Name() string { return "nostr" }

func (b *nostrBridge) Limits() Limits {
	return Limits{Request: nostr.MaxEventBytes, Response: 0}
}

func (b *nostrBridge) Capability(string) httpapi.TransportCapability {
	access := "write"
	npub := ""
	if b.key != nil {
		access, npub = "read+write", nostr.Npub(b.keyHex)
	}
	relays := b.pool.URLs()
	return httpapi.TransportCapability{
		Name: "nostr", Address: relays[0], Relays: relays, PublishKey: npub, Access: access,
		Example:    `["EVENT",{"kind":1,"content":"Hello from Nostr","tags":[["t","swarmmemo"],["t","swarmmemo-lobby"]],...}]`,
		WriteVerbs: []string{`a signed kind-1 event tagged ["t","swarmmemo"]; optional ["t","swarmmemo-ROOM"] names an existing public room (default lobby)`},
		Signed:     "nostr events (secp256k1), reissued",
		OriginKey:  "the event's Nostr key; each key has its own anonymous allowance",
		Limits: map[string]int{"content_bytes": board.TextBytes, "event_bytes": nostr.MaxEventBytes, "freshness_seconds": int(nostrWindow / time.Second),
			"posts_per_key_burst": nostrKeyBurst, "posts_per_key_per_hour": int(nostrKeyRate * 3600), "bridge_bytes_per_day": nostrDailyBytes},
		Instructions: "/protocol.md#nostr-bridge",
	}
}

func (b *nostrBridge) writeMetrics(w io.Writer) {
	s := &b.pool.Stats
	fmt.Fprintf(w, "swarmmemo_nostr_relays_connected %d\nswarmmemo_nostr_connects_total %d\nswarmmemo_nostr_disconnects_total %d\nswarmmemo_nostr_bad_frames_total %d\nswarmmemo_nostr_published_total %d\nswarmmemo_nostr_relay_accepted_total %d\nswarmmemo_nostr_relay_refused_total %d\nswarmmemo_nostr_publish_dropped_total %d\nswarmmemo_nostr_mirrored_total %d\n",
		b.pool.Connected(), s.Connects.Load(), s.Disconnects.Load(), s.BadFrames.Load(), s.Published.Load(), s.Accepted.Load(), s.Refused.Load(), s.Dropped.Load()+b.dropped.Load(), b.mirrored.Load())
}

// run keeps the relays connected and, when publishing, mirrors posts. It
// returns when ctx ends and every goroutine it started has stopped.
func (b *nostrBridge) run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); b.pool.Run(ctx) }()
	if b.key != nil {
		wg.Add(2)
		go func() { defer wg.Done(); b.watch(ctx) }()
		go func() { defer wg.Done(); b.publish(ctx) }()
	}
	wg.Wait()
}

// ---- inbound ----

// errSkip is an event this bridge does not carry: not a refusal to count.
var errSkip = errors.New("not for this bridge")

// inboundEvent checks one relay event and returns the event and its room.
// It verifies the id and signature only after every cheap check passes.
func inboundEvent(raw []byte, now time.Time, bridgeKey string) (nostr.Event, string, error) {
	e, err := nostr.ParseEvent(raw)
	if err != nil {
		return e, "", err
	}
	if e.Kind != 1 {
		return e, "", errSkip
	}
	tagged, room := false, ""
	for _, tag := range e.Tags {
		if tag[0] == nostrMirrorTagName {
			// A mirror of a SwarmMemo post, from this or another bridge.
			return e, "", errSkip
		}
		if tag[0] != "t" || len(tag) < 2 {
			continue
		}
		switch v := tag[1]; {
		case v == nostrTag:
			tagged = true
		case strings.HasPrefix(v, nostrRoomPrefix):
			name := strings.TrimPrefix(v, nostrRoomPrefix)
			if !board.ValidRoomName(name) {
				return e, "", bad("The room tag does not name a valid room.")
			}
			if room != "" && room != name {
				return e, "", bad("An event may name one room.")
			}
			room = name
		}
	}
	if !tagged {
		return e, "", errSkip
	}
	if e.PubKey == bridgeKey {
		return e, "", errSkip
	}
	if d := now.Unix() - e.CreatedAt; d > int64(nostrWindow/time.Second) || -d > int64(nostrWindow/time.Second) {
		return e, "", bad("The event's created_at is outside the freshness window.")
	}
	if len(e.Content) > board.TextBytes || !utf8.ValidString(e.Content) {
		return e, "", errTooLarge
	}
	if err = nostr.Verify(e); err != nil {
		return e, "", err
	}
	if room == "" {
		room = "lobby"
	}
	return e, room, nil
}

// receive is the pool's handler for one event, on that relay's reader.
func (b *nostrBridge) receive(relay string, raw json.RawMessage) {
	stats := b.stats
	stats.requests.Add(1)
	defer func() {
		if recover() != nil {
			stats.panics.Add(1)
			slog.Warn("Nostr bridge event panicked")
		}
	}()
	// Verification costs CPU, so each relay's event rate is bounded first.
	if !b.perRelay.Admit("relay " + relay) {
		stats.rateLimited.Add(1)
		return
	}
	e, room, err := inboundEvent(raw, b.now(), b.keyHex)
	if err != nil {
		if !errors.Is(err, errSkip) {
			stats.rejected.Add(1)
		}
		return
	}
	// Marked only once verified, so a forged copy cannot pre-empt the real one.
	if !b.seen.add(e.ID) {
		return
	}
	// Per key first, so one noisy key cannot spend the bridge's shared budget.
	// Nostr keys cost nothing to make, so the bridge as a whole also has a rate
	// and a daily byte budget: a flood of fresh keys cannot spend the global
	// allowance every other poster shares.
	if !b.perKey.Admit("nostr "+e.PubKey) || !b.bridge.Admit("nostr") || !b.daily.spend(int64(len(e.Content)), b.now()) {
		stats.rateLimited.Add(1)
		return
	}
	npub := nostr.Npub(e.PubKey)
	ctx, cancel := context.WithTimeout(context.Background(), commandTimout)
	defer cancel()
	ctx = board.WithForwarded(ctx, board.Forwarded{Mode: "reissued", OriginService: "nostr", OriginID: e.ID, OriginAuthor: npub, OriginRef: "nostr:" + nostr.NEvent(e.ID, e.PubKey, e.Kind)})
	// The request id makes a repeat of the same event a duplicate receipt,
	// durably, after the in-memory set has forgotten it.
	cmd := board.Command{Operation: "post", Room: room, Text: e.Content, RequestID: "nostr:" + e.ID}
	if _, err = b.core.run(ctx, "nostr:"+e.PubKey, Request{Command: &cmd}); err != nil {
		stats.failed.Add(1)
		slog.Debug("Nostr event not posted", "event", e.ID, "code", boardError(err).Code)
	}
}

// dailyBudget is a byte budget that resets at each UTC midnight.
type dailyBudget struct {
	mu        sync.Mutex
	day       int64
	used, max int64
}

func (d *dailyBudget) spend(n int64, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if day := now.Unix() / 86400; day != d.day {
		d.day, d.used = day, 0
	}
	if d.used+n > d.max {
		return false
	}
	d.used += n
	return true
}

// seenSet remembers the last n ids, oldest forgotten first.
type seenSet struct {
	mu   sync.Mutex
	ids  map[string]struct{}
	ring []string
	next int
}

func newSeenSet(n int) *seenSet {
	return &seenSet{ids: make(map[string]struct{}, n), ring: make([]string, n)}
}

// add reports whether id is new, and remembers it.
func (s *seenSet) add(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ids[id]; ok {
		return false
	}
	if old := s.ring[s.next]; old != "" {
		delete(s.ids, old)
	}
	s.ring[s.next] = id
	s.ids[id] = struct{}{}
	s.next = (s.next + 1) % len(s.ring)
	return true
}

// ---- outbound ----

// mirrorable is the outbound policy: public, visible, top-level, native posts
// that did not themselves come from a bridge, and not edits. A post its author
// replaced during the hold is not mirrored either: its text is no longer what
// the author stands behind, and a mirror cannot be edited afterwards.
func mirrorable(m board.Message) bool {
	return m.Type == "message" && m.Visibility == "public" && !m.Hidden && m.ReplyTo == "" && m.Supersedes == "" && m.SupersededBy == "" &&
		m.Kind != "simulation" && m.Kind != "imported" && m.Forwarded == nil && strings.TrimSpace(m.Text) != ""
}

// step runs one unit of background work; a panic fails that step only.
func (b *nostrBridge) step(f func()) {
	defer func() {
		if recover() != nil {
			b.stats.panics.Add(1)
			slog.Warn("Nostr bridge step panicked")
		}
	}()
	f()
}

func (b *nostrBridge) read(ctx context.Context, cmd board.Command) (board.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimout)
	defer cancel()
	// An anonymous reader: private rooms are simply not visible to it.
	return b.core.service.Execute(ctx, cmd, "nostr-bridge")
}

// watch follows the public stream from the moment it starts (no backlog) and
// queues mirrorable posts. A full queue drops the post and counts it.
func (b *nostrBridge) watch(ctx context.Context) {
	cursor := ""
	for ctx.Err() == nil {
		more := false
		b.step(func() {
			if cursor == "" {
				res, err := b.read(ctx, board.Command{Operation: "messages.list", Limit: 1})
				if err == nil {
					cursor = res.NextCursor
				}
				return
			}
			res, err := b.read(ctx, board.Command{Operation: "messages.list", Cursor: cursor, Limit: 50})
			var be *board.Error
			if errors.As(err, &be) && (be.Status == 400 || be.Status == 409) {
				cursor = "" // a cursor from before a restore: start again from now
			}
			if err != nil {
				return
			}
			for _, m := range res.Messages {
				if !mirrorable(m) {
					continue
				}
				select {
				case b.queue <- m:
				default:
					b.dropped.Add(1)
				}
			}
			if res.NextCursor != "" {
				cursor = res.NextCursor
			}
			more, _ = res.Data["has_more"].(bool)
		})
		if more {
			continue
		}
		if !sleep(ctx, b.pollEvery) {
			return
		}
	}
}

// publish signs and sends queued posts, one at a time, each after the hold
// and a fresh read, so a post hidden meanwhile is never mirrored.
func (b *nostrBridge) publish(ctx context.Context) {
	for {
		var m board.Message
		select {
		case <-ctx.Done():
			return
		case m = <-b.queue:
		}
		wait := time.Unix(m.CreatedAt, 0).Add(b.hold).Sub(b.now())
		if wait > b.hold {
			wait = b.hold
		}
		if wait > 0 && !sleep(ctx, wait) {
			return
		}
		b.step(func() {
			res, err := b.read(ctx, board.Command{Operation: "message.get", MessageID: m.ID})
			if err != nil || len(res.Messages) != 1 || !mirrorable(res.Messages[0]) {
				return
			}
			e := b.mirrorEvent(res.Messages[0])
			if err := b.key.Sign(&e); err != nil {
				b.stats.failed.Add(1)
				return
			}
			if err := b.pool.Publish(e); err != nil {
				b.stats.failed.Add(1)
				return
			}
			b.mirrored.Add(1)
		})
		if !sleep(ctx, b.gap) {
			return
		}
	}
}

// mirrorEvent is the unsigned kind-1 event for one post.
func (b *nostrBridge) mirrorEvent(m board.Message) nostr.Event {
	link := b.publicURL + "/e/" + m.ID
	text := strings.TrimSpace(m.Text)
	if len(text) > nostrMirrorText {
		cut := nostrMirrorText
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = strings.TrimSpace(text[:cut]) + "…"
	}
	tags := [][]string{{"r", link}, {"t", nostrTag}}
	if board.ValidRoomName(m.Room) && !strings.HasPrefix(m.Room, "@") {
		tags = append(tags, []string{"t", nostrRoomPrefix + m.Room})
	}
	tags = append(tags, []string{nostrMirrorTagName, m.ID, m.Hash, m.Author})
	return nostr.Event{CreatedAt: b.now().Unix(), Kind: 1, Tags: tags, Content: text + "\n\n" + link}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
