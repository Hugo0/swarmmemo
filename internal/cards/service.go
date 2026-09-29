package cards

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Renderers.
const (
	RendererLocal      = "local"
	RendererCloudflare = "cloudflare"
)

// RoomItems is how many of a room's newest posts its card shows.
const RoomItems = 4

// Config is the operator's image settings; ConfigFromEnv reads them.
type Config struct {
	Renderer string
	CacheDir string
	// CacheBytes caps the cache directory; least recently used cards go first.
	CacheBytes int64
	// DailyRenders caps fresh renders of any kind per UTC day. Past it every
	// uncached card is the static placeholder until the day turns.
	DailyRenders int
	// CloudflareDaily caps Cloudflare calls per UTC day and CloudflareInterval
	// spaces them; past either, cards are drawn locally instead.
	CloudflareDaily    int
	CloudflareInterval time.Duration
	CloudflareAccount  string
	CloudflareToken    string
	// RoomFresh is how long a room card may lag new posts. It never lags a hide
	// or an edit of a post it shows.
	RoomFresh time.Duration
	// FallbackRetry is when a card drawn locally because Cloudflare was
	// unavailable is tried with Cloudflare again.
	FallbackRetry time.Duration
}

const (
	defaultCacheBytes         = 256 << 20
	defaultDailyRenders       = 2000
	defaultCloudflareDaily    = 100
	defaultCloudflareInterval = 10 * time.Second
	defaultRoomFresh          = 10 * time.Minute
	defaultFallbackRetry      = time.Hour
)

// ConfigFromEnv reads IMAGES and its settings. enabled is false, with a zero
// Config, unless IMAGES=true; every other variable is then ignored.
func ConfigFromEnv(getenv func(string) string, dataDir string) (cfg Config, enabled bool, err error) {
	switch getenv("IMAGES") {
	case "", "false":
		return Config{}, false, nil
	case "true":
	default:
		return Config{}, false, errors.New("IMAGES must be true or false")
	}
	cfg = Config{Renderer: RendererLocal, CacheDir: filepath.Join(dataDir, "images"), CacheBytes: defaultCacheBytes,
		DailyRenders: defaultDailyRenders, CloudflareDaily: defaultCloudflareDaily, CloudflareInterval: defaultCloudflareInterval,
		RoomFresh: defaultRoomFresh, FallbackRetry: defaultFallbackRetry}
	if v := getenv("IMAGES_RENDERER"); v != "" {
		cfg.Renderer = v
	}
	if v := getenv("IMAGES_CACHE_DIR"); v != "" {
		cfg.CacheDir = v
	}
	number := func(key string, into *int64, lo, hi int64) {
		v := getenv(key)
		if v == "" || err != nil {
			return
		}
		n, e := strconv.ParseInt(v, 10, 64)
		if e != nil || n < lo || n > hi {
			err = fmt.Errorf("%s must be a whole number from %d to %d", key, lo, hi)
			return
		}
		*into = n
	}
	cacheMiB, renders, cfDaily, cfInterval := cfg.CacheBytes>>20, int64(cfg.DailyRenders), int64(cfg.CloudflareDaily), int64(cfg.CloudflareInterval/time.Second)
	number("IMAGES_CACHE_MIB", &cacheMiB, 1, 1<<16)
	number("IMAGES_DAILY_RENDERS", &renders, 0, 1_000_000)
	number("IMAGES_CLOUDFLARE_DAILY", &cfDaily, 0, 100_000)
	number("IMAGES_CLOUDFLARE_INTERVAL_SECONDS", &cfInterval, 1, 3600)
	if err != nil {
		return Config{}, false, err
	}
	cfg.CacheBytes, cfg.DailyRenders, cfg.CloudflareDaily, cfg.CloudflareInterval = cacheMiB<<20, int(renders), int(cfDaily), time.Duration(cfInterval)*time.Second
	switch cfg.Renderer {
	case RendererLocal:
	case RendererCloudflare:
		cfg.CloudflareAccount = getenv("IMAGES_CLOUDFLARE_ACCOUNT")
		path := getenv("IMAGES_CLOUDFLARE_TOKEN_FILE")
		if path == "" {
			return Config{}, false, errors.New("IMAGES_RENDERER=cloudflare needs IMAGES_CLOUDFLARE_TOKEN_FILE")
		}
		raw, e := readSmall(path, 4096)
		if e != nil {
			return Config{}, false, fmt.Errorf("read IMAGES_CLOUDFLARE_TOKEN_FILE: %w", e)
		}
		cfg.CloudflareToken = strings.TrimSpace(string(raw))
	default:
		return Config{}, false, errors.New("IMAGES_RENDERER must be local or cloudflare")
	}
	return cfg, true, nil
}

func readSmall(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return raw, nil
}

// Service serves card images and card pages.
type Service struct {
	cfg         Config
	source      Source
	cache       *diskCache
	cf          *cloudflare
	counter     *counter
	renders     chan struct{}
	placeholder []byte
	now         func() time.Time
}

// New validates cfg and opens the cache.
func New(cfg Config, source Source) (*Service, error) {
	if source == nil {
		return nil, errors.New("cards: no card source")
	}
	if cfg.CacheBytes <= 0 || cfg.CacheDir == "" {
		return nil, errors.New("cards: the cache needs a directory and a size")
	}
	if cfg.RoomFresh < 0 || cfg.FallbackRetry <= 0 {
		return nil, errors.New("cards: invalid freshness settings")
	}
	s := &Service{cfg: cfg, source: source, renders: make(chan struct{}, 2), now: time.Now}
	switch cfg.Renderer {
	case RendererLocal:
	case RendererCloudflare:
		if len(cfg.CloudflareAccount) != 32 || strings.Trim(cfg.CloudflareAccount, "0123456789abcdef") != "" {
			return nil, errors.New("IMAGES_CLOUDFLARE_ACCOUNT must be the 32-character account ID")
		}
		if len(cfg.CloudflareToken) < 20 || strings.ContainsAny(cfg.CloudflareToken, " \t\r\n") {
			return nil, errors.New("the Cloudflare token file does not hold a token")
		}
		s.cf = newCloudflare(cfg.CloudflareAccount, cfg.CloudflareToken)
	default:
		return nil, errors.New("cards: renderer must be local or cloudflare")
	}
	var err error
	if s.cache, err = openCache(cfg.CacheDir, cfg.CacheBytes); err != nil {
		return nil, fmt.Errorf("cards: open cache: %w", err)
	}
	s.counter = openCounter(filepath.Join(cfg.CacheDir, "counters.json"))
	if s.placeholder, err = DrawPNG(placeholderCard); err != nil {
		return nil, err
	}
	return s, nil
}

// placeholderCard is the static image served past the daily render cap.
var placeholderCard = Card{Heading: "", Title: "Image not drawn yet",
	Body:   "This board's daily image budget is spent. The post is unchanged and readable as text at its address; the image returns tomorrow (UTC).",
	Footer: "swarmmemo.com"}

// Image is one served card.
type Image struct {
	PNG []byte
	// ETag is the digest of what the image shows.
	ETag string
	// Placeholder marks the over-cap image, which must not be cached long.
	Placeholder bool
}

// Renderer is the configured renderer name.
func (s *Service) Renderer() string { return s.cfg.Renderer }

// load reads the card and trims a room card to what it shows. window is the
// digest of every public post the source read, for the room freshness rule.
func (s *Service) load(ctx context.Context, kind, key string) (Card, []string, error) {
	var card Card
	var err error
	switch kind {
	case Post:
		if !ValidMessageID(key) {
			return Card{}, nil, ErrNotFound
		}
		card, err = s.source.PostCard(ctx, key)
	case Room:
		if !ValidRoom(key) {
			return Card{}, nil, ErrNotFound
		}
		card, err = s.source.RoomCard(ctx, key)
	default:
		return Card{}, nil, ErrNotFound
	}
	if err != nil {
		return Card{}, nil, err
	}
	card.Kind, card.Key = kind, key
	window := card.parts()
	if len(card.Items) > RoomItems {
		card.Items = card.Items[:RoomItems]
	}
	return card, window, nil
}

// Page is the card page for /render: the bytes a renderer is given.
func (s *Service) Page(ctx context.Context, kind, key string) ([]byte, error) {
	card, _, err := s.load(ctx, kind, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			s.cache.forget(kind, key)
		}
		return nil, err
	}
	return RenderHTML(card), nil
}

// Image returns the card's PNG: from the cache when it still shows exactly
// what the board shows, else freshly rendered within the daily caps.
func (s *Service) Image(ctx context.Context, kind, key string) (Image, error) {
	card, window, err := s.load(ctx, kind, key)
	if errors.Is(err, ErrNotFound) {
		// Hidden, private or gone: drop any stored image of it at once.
		s.cache.forget(kind, key)
		return Image{}, ErrNotFound
	}
	if err != nil {
		return Image{}, err
	}
	page := RenderHTML(card)
	if page == nil {
		return Image{}, errors.New("cards: card page failed")
	}
	want := digest(page)
	if e, ok := s.cache.get(kind, key); ok && s.usable(e, kind, want, window) {
		return Image{PNG: e.PNG, ETag: e.Digest}, nil
	}
	select {
	case s.renders <- struct{}{}:
		defer func() { <-s.renders }()
	case <-ctx.Done():
		return Image{}, ctx.Err()
	}
	// Another request may have rendered this card while this one waited.
	if e, ok := s.cache.get(kind, key); ok && s.usable(e, kind, want, window) {
		return Image{PNG: e.PNG, ETag: e.Digest}, nil
	}
	now := s.now()
	if !s.counter.take(now, "renders", s.cfg.DailyRenders) {
		return Image{PNG: s.placeholder, ETag: "placeholder", Placeholder: true}, nil
	}
	png, renderer, err := s.render(ctx, card, page, now)
	if err != nil {
		return Image{}, err
	}
	parts := card.parts()
	if err := s.cache.put(kind, key, entry{Digest: want, Parts: parts, Renderer: renderer, Created: now.Unix(), PNG: png}); err != nil {
		slog.Warn("Card image cache write failed", "error", err)
	}
	return Image{PNG: png, ETag: want}, nil
}

// usable decides whether a cached card may be served for what the board shows
// now. Exact digest: yes. A room card may lag new posts for RoomFresh, but
// only while every post it shows is still in the room, visible and unedited.
func (s *Service) usable(e entry, kind, want string, window []string) bool {
	age := s.now().Sub(time.Unix(e.Created, 0))
	if e.Renderer != s.cfg.Renderer && age > s.cfg.FallbackRetry {
		return false
	}
	if e.Digest == want {
		return true
	}
	if kind != Room || age < 0 || age > s.cfg.RoomFresh || len(e.Parts) == 0 {
		return false
	}
	current := make(map[string]bool, len(window))
	for _, p := range window {
		current[p] = true
	}
	for _, p := range e.Parts {
		if !current[p] {
			return false
		}
	}
	return true
}

func (s *Service) render(ctx context.Context, card Card, page []byte, now time.Time) ([]byte, string, error) {
	if s.cf != nil && s.counter.spaced(now, s.cfg.CloudflareInterval) && s.counter.take(now, "cloudflare", s.cfg.CloudflareDaily) {
		png, err := s.cf.screenshot(ctx, page)
		if err == nil {
			return png, RendererCloudflare, nil
		}
		if errors.Is(err, ErrRendererLimited) {
			// Cloudflare's own quota: stop asking for a while.
			s.counter.pause(now.Add(5 * time.Minute))
		}
		slog.Warn("Cloudflare card render failed; drawing locally", "error", err)
	}
	png, err := DrawPNG(card)
	if err != nil {
		return nil, "", err
	}
	return png, RendererLocal, nil
}

// Forget drops a card's stored image (after a hide, restore or edit).
func (s *Service) Forget(kind, key string) { s.cache.forget(kind, key) }

// Capabilities is the /capabilities "images" object.
func (s *Service) Capabilities() map[string]any {
	return map[string]any{
		"post": "/e/MESSAGE_ID.png", "room": "/r/ROOM.png", "page": "/render/e/MESSAGE_ID and /render/r/ROOM",
		"media_type": "image/png", "width": Width, "height": Height, "renderer": s.cfg.Renderer,
		"fallback_renderer": RendererLocal, "field": "image_url", "query_parameters": false, "renders_urls": false,
		"content":          "public, visible posts and public rooms only; hidden, private and removed posts return 404",
		"room_lag_seconds": int(s.cfg.RoomFresh / time.Second), "daily_render_cap": s.cfg.DailyRenders,
		"over_cap": "a static placeholder image", "instructions": "/protocol.md#post-and-room-images",
	}
}

// counter holds the per-UTC-day render counts and the Cloudflare spacing,
// persisted beside the cache so a restart does not reset the budget.
type counter struct {
	path string
	mu   sync.Mutex
	st   counterState
}

type counterState struct {
	Day   string         `json:"day"`
	Count map[string]int `json:"count"`
	// Next is the earliest Unix time for the next Cloudflare call.
	Next int64 `json:"next"`
}

func openCounter(path string) *counter {
	c := &counter{path: path}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &c.st)
	}
	return c
}

func (c *counter) rollLocked(now time.Time) {
	day := now.UTC().Format("2006-01-02")
	if c.st.Day != day || c.st.Count == nil {
		c.st.Day, c.st.Count = day, map[string]int{}
	}
}

// take counts one use of name against limit and reports whether it fit.
func (c *counter) take(now time.Time, name string, limit int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollLocked(now)
	if c.st.Count[name] >= limit {
		return false
	}
	c.st.Count[name]++
	c.saveLocked()
	return true
}

// spaced reserves the next Cloudflare slot if the interval has passed.
func (c *counter) spaced(now time.Time, interval time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Unix() < c.st.Next {
		return false
	}
	c.st.Next = now.Add(interval).Unix()
	return true
}

func (c *counter) pause(until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if until.Unix() > c.st.Next {
		c.st.Next = until.Unix()
		c.saveLocked()
	}
}

func (c *counter) get(now time.Time, name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollLocked(now)
	return c.st.Count[name]
}

func (c *counter) saveLocked() {
	raw, err := json.Marshal(c.st)
	if err != nil {
		return
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err == nil {
		_ = os.Rename(tmp, c.path)
	}
}
