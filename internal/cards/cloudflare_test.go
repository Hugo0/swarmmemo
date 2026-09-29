package cards

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests never reach Cloudflare: the renderer talks to an httptest TLS
// server that records what it was sent and answers like the /screenshot API.

const testAccount = "9bc52a9b6eb401489ebf19bb5f43a302"

type fakeCloudflare struct {
	mu       sync.Mutex
	requests []map[string]any
	paths    []string
	auth     []string
	status   int
	body     func() []byte
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(raw, &req)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	status, body := f.status, f.body
	f.mu.Unlock()
	if status == 0 {
		status = 200
	}
	if status != 200 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":2001,"message":"Browser time limit exceeded for today."}]}`))
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(body())
}

// remotePNG is a valid card-sized PNG unlike any local drawing (solid grey).
func remotePNG(w, h int) []byte {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0x80
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func cloudflareService(t *testing.T, src Source, fake *fakeCloudflare, mutate func(*Config)) (*Service, *time.Time) {
	t.Helper()
	server := httptest.NewTLSServer(fake)
	t.Cleanup(server.Close)
	s, now := newTestService(t, src, func(c *Config) {
		c.Renderer, c.CloudflareAccount, c.CloudflareToken = RendererCloudflare, testAccount, "test-token-abcdefghijklmnop"
		c.CloudflareInterval = 10 * time.Second
		if mutate != nil {
			mutate(c)
		}
	})
	// Point the client at the fake: its base URL and its TLS certificate.
	// The production dialer is exercised separately in TestCloudflareDialer.
	s.cf.base = server.URL + "/client/v4"
	s.cf.client = server.Client()
	s.cf.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return s, now
}

func TestCloudflareRendersOurHTMLNeverAURL(t *testing.T) {
	src := newFakeSource()
	src.setPost(postID, Card{Heading: "#lobby", Title: "Hello", Body: "See https://evil.example/ for more", Footer: "swarmmemo.com/e/" + postID})
	fake := &fakeCloudflare{body: func() []byte { return remotePNG(Width, Height) }}
	s, _ := cloudflareService(t, src, fake, nil)
	img, err := s.Image(context.Background(), Post, postID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(img.PNG, remotePNG(Width, Height)) {
		t.Fatal("did not serve the Cloudflare image")
	}
	if len(fake.requests) != 1 {
		t.Fatalf("%d requests", len(fake.requests))
	}
	req := fake.requests[0]
	if fake.paths[0] != "POST /client/v4/accounts/"+testAccount+"/browser-rendering/screenshot" {
		t.Fatalf("path %q", fake.paths[0])
	}
	if fake.auth[0] != "Bearer test-token-abcdefghijklmnop" {
		t.Fatalf("auth %q", fake.auth[0])
	}
	if _, hasURL := req["url"]; hasURL {
		t.Fatal("a url was sent to the renderer")
	}
	page, _ := s.Page(context.Background(), Post, postID)
	if req["html"] != string(page) {
		t.Fatal("the renderer was not given exactly the card page")
	}
	if req["setJavaScriptEnabled"] != false {
		t.Fatal("JavaScript not disabled")
	}
	viewport, _ := req["viewport"].(map[string]any)
	if viewport["width"] != float64(Width) || viewport["height"] != float64(Height) {
		t.Fatalf("viewport %v", viewport)
	}
	rejected, _ := req["rejectResourceTypes"].([]any)
	if len(rejected) < 5 {
		t.Fatalf("rejectResourceTypes %v", rejected)
	}
	// Cached: a second read costs no call.
	if _, err := s.Image(context.Background(), Post, postID); err != nil || len(fake.requests) != 1 {
		t.Fatalf("cache miss: %d calls", len(fake.requests))
	}
}

func TestCloudflareFallsBackToLocal(t *testing.T) {
	ctx := context.Background()
	local := func(s *Service) []byte {
		card, _, _ := s.load(ctx, Post, postID)
		raw, _ := DrawPNG(card)
		return raw
	}
	for name, tc := range map[string]struct {
		fake   *fakeCloudflare
		mutate func(*Config)
		calls  int
	}{
		"rate limited": {fake: &fakeCloudflare{status: 429}, calls: 1},
		"server error": {fake: &fakeCloudflare{status: 500}, calls: 1},
		"wrong size":   {fake: &fakeCloudflare{body: func() []byte { return remotePNG(800, 600) }}, calls: 1},
		"not a png":    {fake: &fakeCloudflare{body: func() []byte { return []byte("<html>not an image</html>") }}, calls: 1},
		"too large":    {fake: &fakeCloudflare{body: func() []byte { return append(remotePNG(Width, Height), make([]byte, MaxImageBytes)...) }}, calls: 1},
		"daily cap":    {fake: &fakeCloudflare{body: func() []byte { return remotePNG(Width, Height) }}, mutate: func(c *Config) { c.CloudflareDaily = 0 }, calls: 0},
	} {
		t.Run(name, func(t *testing.T) {
			src := newFakeSource()
			src.setPost(postID, Card{Title: "Hello"})
			s, _ := cloudflareService(t, src, tc.fake, tc.mutate)
			img, err := s.Image(ctx, Post, postID)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(img.PNG, local(s)) {
				t.Fatal("fallback did not draw locally")
			}
			if len(tc.fake.requests) != tc.calls {
				t.Fatalf("%d Cloudflare calls, want %d", len(tc.fake.requests), tc.calls)
			}
			if e, ok := s.cache.get(Post, postID); !ok || e.Renderer != RendererLocal {
				t.Fatal("fallback not cached as local")
			}
		})
	}
}

func TestCloudflareSpacingAndRetry(t *testing.T) {
	ctx := context.Background()
	src := newFakeSource()
	other := strings.Repeat("e", 32)
	src.setPost(postID, Card{Title: "one"})
	src.setPost(other, Card{Title: "two"})
	fake := &fakeCloudflare{body: func() []byte { return remotePNG(Width, Height) }}
	s, now := cloudflareService(t, src, fake, nil)
	if _, err := s.Image(ctx, Post, postID); err != nil || len(fake.requests) != 1 {
		t.Fatal("first render")
	}
	// Inside the interval: drawn locally, no second call.
	if _, err := s.Image(ctx, Post, other); err != nil || len(fake.requests) != 1 {
		t.Fatalf("interval not respected: %d calls", len(fake.requests))
	}
	// The local stand-in is kept until FallbackRetry, then Cloudflare is asked again.
	*now = now.Add(time.Minute)
	if _, _ = s.Image(ctx, Post, other); len(fake.requests) != 1 {
		t.Fatal("local fallback retried too early")
	}
	*now = now.Add(2 * time.Hour)
	if img, _ := s.Image(ctx, Post, other); len(fake.requests) != 2 || !bytes.Equal(img.PNG, remotePNG(Width, Height)) {
		t.Fatalf("fallback not retried: %d calls", len(fake.requests))
	}
	// A 429 pauses Cloudflare for five minutes.
	fake.mu.Lock()
	fake.status = 429
	fake.mu.Unlock()
	src.setPost(postID, Card{Title: "one, edited"})
	*now = now.Add(time.Minute)
	_, _ = s.Image(ctx, Post, postID)
	calls := len(fake.requests)
	src.setPost(postID, Card{Title: "one, edited twice"})
	*now = now.Add(time.Minute)
	if _, _ = s.Image(ctx, Post, postID); len(fake.requests) != calls {
		t.Fatal("Cloudflare asked again inside the 429 pause")
	}
}

func TestCloudflareRedirectNotFollowed(t *testing.T) {
	var hits int
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.RedirectHandler(target.URL+"/elsewhere", http.StatusFound))
	defer redirect.Close()
	c := newCloudflare(testAccount, "token")
	c.base = redirect.URL
	transport := redirect.Client().Transport
	c.client.Transport = transport
	if _, err := c.screenshot(context.Background(), []byte("<p>x</p>")); err == nil || hits != 0 {
		t.Fatalf("redirect followed: err=%v hits=%d", err, hits)
	}
}

func TestCloudflareDialer(t *testing.T) {
	c := newCloudflare(testAccount, "token")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, addr := range []string{"evil.example:443", "127.0.0.1:443", "api.cloudflare.com:80", "api.cloudflare.com.evil.example:443", "169.254.169.254:443", "[::1]:443", "localhost:443"} {
		if _, err := c.dial(ctx, "tcp", addr); !errors.Is(err, errCloudflareHost) {
			t.Errorf("dial %s: %v", addr, err)
		}
	}
	// The right name resolving to a private address (rebinding) is refused
	// before any connection; so is one private answer among public ones.
	for _, answer := range [][]net.IP{{net.ParseIP("127.0.0.1")}, {net.ParseIP("104.16.0.1"), net.ParseIP("10.0.0.1")}, {net.ParseIP("::ffff:169.254.169.254")}} {
		c.lookup = func(context.Context, string) ([]net.IP, error) { return answer, nil }
		if _, err := c.dial(ctx, "tcp", "api.cloudflare.com:443"); !errors.Is(err, errCloudflareAddress) {
			t.Errorf("answer %v: %v", answer, err)
		}
	}
	c.lookup = func(context.Context, string) ([]net.IP, error) { return nil, errors.New("no such host") }
	if _, err := c.dial(ctx, "tcp", "api.cloudflare.com:443"); err == nil {
		t.Error("unresolved host dialled")
	}
	transport := c.client.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.TLSClientConfig.ServerName != cloudflareHost {
		t.Error("transport may use a proxy or another server name")
	}
}
