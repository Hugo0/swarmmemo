package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// slowAgentService holds every agent.get until gate closes, like the slow
// /api/agent/ID reads of the 2026-10-30 burst; other reads answer at once.
type slowAgentService struct {
	fakeService
	gate    chan struct{}
	waiting atomic.Int64
}

func (f *slowAgentService) Execute(ctx context.Context, c board.Command, peer string) (board.Result, error) {
	if c.Operation == "agent.get" {
		f.waiting.Add(1)
		<-f.gate
	}
	return f.fakeService.Execute(ctx, c, peer)
}

func shareRequest(s *Server, method, path, client, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:4000"
	r.Header.Set("X-Forwarded-For", client)
	r.Header.Set("X-Forwarded-Proto", "https")
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// burst sends n concurrent anonymous GET /api/agent/ID from addresses
// client(i), waits until each one is either answered or held in the service,
// and returns the held count, the answers by status, and a release.
func burst(t *testing.T, s *Server, svc *slowAgentService, n int, client func(int) string) (held int, statuses map[int]int, release func()) {
	t.Helper()
	var mu sync.Mutex
	statuses = map[int]int{}
	var answered atomic.Int64
	var wg sync.WaitGroup
	before := svc.waiting.Load()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := shareRequest(s, "GET", "/api/agent/agent"+fmt.Sprint(i), client(i), "")
			mu.Lock()
			statuses[w.Code]++
			mu.Unlock()
			if w.Code == 429 {
				var body struct {
					Error struct {
						Code       string `json:"code"`
						RetryAfter int    `json:"retry_after"`
					} `json:"error"`
				}
				_ = json.Unmarshal(w.Body.Bytes(), &body)
				if body.Error.Code != "request_rate" || w.Header().Get("Retry-After") != "2" || body.Error.RetryAfter != 2 {
					t.Errorf("over-share answer: code %q Retry-After %q retry_after %d", body.Error.Code, w.Header().Get("Retry-After"), body.Error.RetryAfter)
				}
			}
			answered.Add(1)
		}(i)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		h := int(svc.waiting.Load() - before)
		mu.Lock()
		done := int(answered.Load())
		mu.Unlock()
		if h+done == n {
			held = h
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("burst did not settle: %d held, %d answered of %d", h, done, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return held, statuses, func() { close(svc.gate); wg.Wait() }
}

// TestBurstFromOneNetworkDoesNotShedOthers reproduces the C635 burst: 400
// slow anonymous reads from 200 addresses in one /24 (each under the
// per-address rate) take only their share of the pool; the rest are told
// 429 request_rate with Retry-After, and another network's reads, a signed
// command and /health are all still served.
func TestBurstFromOneNetworkDoesNotShedOthers(t *testing.T) {
	svc := &slowAgentService{gate: make(chan struct{})}
	s := New(svc, nil, Config{TrustLoopbackProxy: true})
	held, statuses, release := burst(t, s, svc, 400, func(i int) string { return fmt.Sprintf("43.173.0.%d", i%200+1) })
	defer release()
	free := cap(s.inflight) - anonReserve
	if held == 0 || 2*held > free+1 || statuses[429] != 400-held || statuses[503] != 0 {
		t.Fatalf("one /24 held %d of %d anonymous slots; answers %v", held, free, statuses)
	}
	for i := 0; i < 5; i++ {
		if w := shareRequest(s, "GET", "/api/messages?room=lobby", "198.51.100.7", ""); w.Code != 200 {
			t.Fatalf("another network's read %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if w := shareRequest(s, "GET", "/api/messages?room=lobby", "43.173.0.9", ""); w.Code != 429 {
		t.Fatalf("the bursting network got %d, want 429", w.Code)
	}
	if w := shareRequest(s, "POST", "/v1/command", "43.173.0.9", `{"operation":"rooms.list","public_key":"key","signature":"sig"}`); w.Code != 200 {
		t.Fatalf("a signed command from the bursting network: %d %s", w.Code, w.Body.String())
	}
	if w := shareRequest(s, "GET", "/health", "43.173.0.9", ""); w.Code == 429 || w.Code == 503 {
		t.Fatalf("/health from the bursting network: %d", w.Code)
	}
}

// TestBurstFromManyNetworksLeavesRoom: four ranges of many /24s each (the
// incident's 43.172/16, 43.173/16, 47.79/16 and 161.120/16) together still
// leave slots for a fifth network, because each wide range is held to half of
// what is free when it asks.
func TestBurstFromManyNetworksLeavesRoom(t *testing.T) {
	svc := &slowAgentService{gate: make(chan struct{})}
	s := New(svc, nil, Config{TrustLoopbackProxy: true})
	ranges := []string{"43.172", "43.173", "47.79", "161.120"}
	held, statuses, release := burst(t, s, svc, 800, func(i int) string {
		return fmt.Sprintf("%s.%d.%d", ranges[i%4], i/4%50, i%200+1)
	})
	defer release()
	free := cap(s.inflight) - anonReserve
	if left := free - held; left < 8 || statuses[503] != 0 {
		t.Fatalf("four ranges held %d of %d anonymous slots; answers %v", held, free, statuses)
	}
	for i := 0; i < 5; i++ {
		if w := shareRequest(s, "GET", "/api/messages?room=lobby", "2001:db8::7", ""); w.Code != 200 {
			t.Fatalf("another network's read %d: %d %s", i, w.Code, w.Body.String())
		}
	}
}

// TestSharesOnlyBiteUnderLoad: an otherwise idle pool lets one network hold
// a third of it (38 reads at once), so a legitimate agent never meets the
// rule in normal use.
func TestSharesOnlyBiteUnderLoad(t *testing.T) {
	var table shareTable
	free := 128 - anonReserve
	for i := 0; i < 38; i++ {
		if !table.acquire("192.0.2.0", "192.0.0.0/16", free-i) {
			t.Fatalf("an idle pool refused read %d", i)
		}
	}
	if table.acquire("192.0.2.0", "192.0.0.0/16", free-38) {
		t.Fatal("one network took more than half of what was free")
	}
	// A network holding nothing is let in while any slot is free.
	if !table.acquire("198.51.100.0", "198.51.0.0/16", 1) {
		t.Fatal("a new network was refused with a slot free")
	}
	table.release("198.51.100.0", "198.51.0.0/16")
	for i := 0; i < 38; i++ {
		table.release("192.0.2.0", "192.0.0.0/16")
	}
	if len(table.held) != 0 {
		t.Fatalf("shares left after release: %v", table.held)
	}
}

func TestShareKeys(t *testing.T) {
	own, _ := ParseOwnNetworks("203.0.113.0/24")
	s := New(&fakeService{}, nil, Config{OwnNetworks: own})
	for peer, want := range map[string][2]string{
		"43.173.5.9":      {"43.173.5.0", "43.173.0.0/16"},
		"2001:db8:1:2::9": {"2001:db8:1::", "2001:db8::/32"},
		"not-an-ip":       {"not-an-ip", "not-an-ip"},
		"127.0.0.1":       {},
		"203.0.113.4":     {},
	} {
		narrow, wide, ok := s.shareKeys(peer)
		if ok != (want != [2]string{}) || narrow != want[0] || wide != want[1] {
			t.Errorf("%s: %q %q %v", peer, narrow, wide, ok)
		}
	}
}
