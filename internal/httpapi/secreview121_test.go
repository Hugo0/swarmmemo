package httpapi

// Security review 1.21.0: regression tests for calls without a key. Each one
// began as the reviewer's proof of concept and now asserts the fixed
// behaviour.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

func secReq(s *Server, method, path, body, remote string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://swarmmemo.com"+path, strings.NewReader(body))
	r.RemoteAddr = remote
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// rid is a request_id long enough for a call without a key.
func rid(name string) string {
	return strings.NewReplacer(" ", "-", "/", "-").Replace(name) + "-0123456789abcdef"
}

func stampCommand(text, requestID string) string {
	cmd, _ := json.Marshal(board.Command{Operation: "service.call", Target: "notary", Data: `{"schema":1,"method":"stamp","args":{"text":"` + text + `"},"max_cost":1}`, RequestID: requestID})
	return string(cmd)
}

// M1: another site's page cannot make its visitors spend their network's
// share on any HTTP route (/call/, /c64/, POST /v1/command), whatever the
// fetch mode, nor read an answer: no unsigned service.call answer carries
// Access-Control-Allow-Origin. Agents (no Sec-Fetch, no Origin) and this
// site's own pages still call; other routes keep ACAO *.
func TestSecReview121CrossSiteSpendRefused(t *testing.T) {
	_, s := anonCallServer(t, 2000)
	const peer = "203.0.113.7:1"
	img := map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "image"}
	beacon := map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "empty", "Content-Type": "text/plain;charset=UTF-8", "Origin": "https://evil.example"}
	cors := map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty", "Origin": "https://evil.example"}
	sameSite := map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "cors", "Origin": "https://evil.swarmmemo.com"}
	oldBrowser := map[string]string{"Origin": "https://evil.example"}
	nullOrigin := map[string]string{"Origin": "null"}
	c64 := func(text, id string) string {
		return "/c64/" + base64.RawURLEncoding.EncodeToString([]byte(stampCommand(text, id)))
	}
	refused := func(what string, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != 403 || !strings.Contains(w.Body.String(), `"invalid_origin"`) || !strings.Contains(w.Body.String(), "not from a browser page") {
			t.Errorf("%s: %d %s, want 403 invalid_origin", what, w.Code, w.Body.String())
		}
		if acao := w.Header().Get("Access-Control-Allow-Origin"); acao != "" {
			t.Errorf("%s: Access-Control-Allow-Origin %q on a refused call", what, acao)
		}
	}
	for name, h := range map[string]map[string]string{"image": img, "beacon": beacon, "cors": cors, "same-site": sameSite, "origin only": oldBrowser, "null origin": nullOrigin} {
		refused("/call/ "+name, secReq(s, "GET", "/call/notary/stamp?text=x&max_cost=1&request_id="+rid("call-"+name), "", peer, h))
		refused("/c64/ "+name, secReq(s, "GET", c64("c64 "+name, rid("c64-"+name)), "", peer, h))
		refused("/v1/command "+name, secReq(s, "POST", "/v1/command", stampCommand("cmd "+name, rid("cmd-"+name)), peer, h))
	}
	// Nothing was spent: the network's first call still finds the whole share.
	w := secReq(s, "GET", "/api/allowance", "", peer, nil)
	if !strings.Contains(w.Body.String(), `"resource":"credit"`) || strings.Contains(w.Body.String(), `"used":1`) {
		t.Fatalf("a refused cross-site call spent credit: %s", w.Body.String())
	}
	// No preflight passes for /call/.
	if w := secReq(s, "OPTIONS", "/call/notary/stamp", "", peer, cors); w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("OPTIONS /call/ allows %q", w.Header().Get("Access-Control-Allow-Origin"))
	}
	// An agent (no browser headers), this site's own page, and a typed URL
	// still call, and no answer is readable cross-site.
	ours := map[string]string{"Origin": "https://swarmmemo.com"}
	sameOrigin := map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors"}
	typed := map[string]string{"Sec-Fetch-Site": "none", "Sec-Fetch-Mode": "navigate"}
	okPeer := map[string]string{"agent": "192.0.2.7:1", "own origin": "198.18.0.7:1", "same-origin": "198.18.1.7:1", "typed": "198.18.2.7:1"} // a network each: 3 calls apiece stay under the notary's 10 a minute
	for name, h := range map[string]map[string]string{"agent": nil, "own origin": ours, "same-origin": sameOrigin, "typed": typed} {
		peer := okPeer[name]
		for route, w := range map[string]*httptest.ResponseRecorder{
			"/call/":      secReq(s, "GET", "/call/notary/stamp?text=ok+"+strings.ReplaceAll(name, " ", "+")+"&max_cost=1&request_id="+rid("ok-call-"+name), "", peer, h),
			"/c64/":       secReq(s, "GET", c64("ok c64 "+name, rid("ok-c64-"+name)), "", peer, h),
			"/v1/command": secReq(s, "POST", "/v1/command", stampCommand("ok cmd "+name, rid("ok-cmd-"+name)), peer, h),
		} {
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"cost":1`) {
				t.Errorf("%s %s: %d %s", name, route, w.Code, w.Body.String())
			}
			if acao := w.Header().Get("Access-Control-Allow-Origin"); acao != "" {
				t.Errorf("%s %s: an unsigned call's answer carries Access-Control-Allow-Origin %q", name, route, acao)
			}
		}
	}
	// Everything else keeps ACAO *.
	if w := secReq(s, "GET", "/api/services", "", peer, cors); w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("/api/services lost ACAO *: %d %q", w.Code, w.Header().Get("Access-Control-Allow-Origin"))
	}
	if w := secReq(s, "OPTIONS", "/v1/command", "", peer, cors); w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("OPTIONS /v1/command lost ACAO *")
	}
}

// M2: a call refused after admission (here quota_exhausted, free) gives its
// place in the anonymous windows back, so one network whose share is spent
// cannot hold the windows every network shares; and notary.stamp has its
// own per-network bound, checked first, whose refusal says so.
func TestSecReview121OneNetworkCannotHogAnonymousNotary(t *testing.T) {
	_, s := anonCallServer(t, 3) // 3 credits per network
	for i := 0; i < 130; i++ {
		// One network (a /24), several hosts, so the per-address HTTP
		// limiter does not stand in for the anonymous windows.
		attacker := fmt.Sprintf("198.51.100.%d:4000", 9+i/40)
		w := secReq(s, "GET", fmt.Sprintf("/call/notary/stamp?text=hog&max_cost=1&request_id=%s", rid(fmt.Sprint("hog-", i))), "", attacker, nil)
		switch {
		case i < 3 && w.Code != 200:
			t.Fatalf("attacker call %d: %d %s", i, w.Code, w.Body.String())
		case i >= 3 && (w.Code != 429 || !strings.Contains(w.Body.String(), "quota_exhausted")):
			t.Fatalf("attacker call %d: %d %s, want 429 quota_exhausted", i, w.Code, w.Body.String())
		}
	}
	victim := secReq(s, "GET", "/call/notary/stamp?text=victim&max_cost=1&request_id="+rid("victim"), "", "192.0.2.50:4000", nil)
	if victim.Code != 200 {
		t.Fatalf("another network after 127 refused calls: %d %s", victim.Code, victim.Body.String())
	}
	// A network with credit to spare is bounded per network (10 a minute),
	// in words about its own network, and others are still served.
	_, s = anonCallServer(t, 2000)
	busy := "198.51.100.10:4000"
	var last *httptest.ResponseRecorder
	for i := 0; i < 25; i++ { // at most two minute windows
		if last = secReq(s, "GET", fmt.Sprintf("/call/notary/stamp?text=b%d&max_cost=1&request_id=%s", i, rid(fmt.Sprint("busy-", i))), "", busy, nil); last.Code != 200 {
			break
		}
	}
	if last.Code != 429 || !strings.Contains(last.Body.String(), `"request_rate"`) || !strings.Contains(last.Body.String(), "This network") || strings.Contains(last.Body.String(), "your tier") {
		t.Fatalf("a busy network's bound: %d %s", last.Code, last.Body.String())
	}
	if w := secReq(s, "GET", "/call/notary/stamp?text=other&max_cost=1&request_id="+rid("other"), "", "192.0.2.51:4000", nil); w.Code != 200 {
		t.Fatalf("another network beside a busy one: %d %s", w.Code, w.Body.String())
	}
}

// M3: an IPv6 caller's credit share is its /48, so a new /64 of the same
// /48 does not get a fresh share; another /48 does. Posting stays per /64.
func TestSecReview121IPv6Slash48SharesOneCreditShare(t *testing.T) {
	_, s := anonCallServer(t, 3)
	stamp := func(remote, id string) *httptest.ResponseRecorder {
		return secReq(s, "GET", "/call/notary/stamp?text="+id+"&max_cost=1&request_id="+rid(id), "", remote, nil)
	}
	for i := 0; i < 3; i++ {
		if w := stamp("[2001:db8:77:1::1]:1", fmt.Sprintf("a%d", i)); w.Code != 200 {
			t.Fatalf("first /64: %d %s", w.Code, w.Body.String())
		}
	}
	for _, other := range []string{"[2001:db8:77:1::2]:1", "[2001:db8:77:2::1]:1", "[2001:db8:77:ffff::9]:1"} {
		if w := stamp(other, "over-"+other); w.Code != 429 || !strings.Contains(w.Body.String(), "quota_exhausted") {
			t.Fatalf("%s, same /48 past its share: %d %s", other, w.Code, w.Body.String())
		}
	}
	if w := stamp("[2001:db8:78:1::1]:1", "b0"); w.Code != 200 {
		t.Fatalf("another /48: %d %s", w.Code, w.Body.String())
	}
	// Allowance reads show the /48's credit (spent) beside the /64's posting.
	w := secReq(s, "GET", "/api/allowance", "", "[2001:db8:77:5::1]:1", nil)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	resources, _ := dig(body, "data", "resources").([]any)
	found := false
	for _, r := range resources {
		if m := r.(map[string]any); m["resource"] == "credit" {
			found = true
			if m["remaining"] != float64(0) {
				t.Fatalf("another /64 of the /48 reads an unspent credit share: %+v", m)
			}
		}
	}
	if !found {
		t.Fatalf("allowance: %s", w.Body.String())
	}
}

// L3: a request_id shorter than 16 characters is refused with the rule,
// so a neighbour in the same /24 cannot squat the documented or a guessable
// one; the examples carry no request_id (the board makes a random one), and
// a placeholder pasted as it is counts as left out, never a shared id.
func TestSecReview121RequestIDSquatInSameNetwork(t *testing.T) {
	_, s := anonCallServer(t, 2000)
	for _, id := range []string{"my-first-call", "1", "test"} {
		w := secReq(s, "GET", "/call/notary/stamp?text=squat&max_cost=1&request_id="+id, "", "198.51.100.200:1", nil)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "leave it out") || !regexp.MustCompile(`request_id=[0-9a-f]{32}\)`).MatchString(w.Body.String()) {
			t.Fatalf("request_id %q: %d %s", id, w.Code, w.Body.String())
		}
	}
	caps := decodeResult(t, secReq(s, "GET", "/capabilities", "", "198.51.100.17:1", nil).Body.Bytes())
	example, _ := dig(caps, "services", "without_key", "example").(string)
	if strings.Contains(example, "request_id") || !strings.HasPrefix(dig(caps, "services", "without_key", "request_id").(string), "optional: ") {
		t.Fatalf("the example carries a request_id: %q", example)
	}
	// Two neighbours pasting the same placeholder get two random ids: the
	// second is its own call, not the first one's answer.
	var ids []any
	for _, ip := range []string{"198.51.100.200:1", "198.51.100.201:1"} {
		w := secReq(s, "GET", "/call/notary/stamp?text=same&max_cost=1&request_id=RANDOM_16_CHARS", "", ip, nil)
		body := decodeResult(t, w.Body.Bytes())
		if w.Code != 200 || dig(body, "data", "call", "request_id") == "RANDOM_16_CHARS" {
			t.Fatalf("placeholder: %d %s", w.Code, w.Body.String())
		}
		ids = append(ids, dig(body, "data", "call", "id"))
	}
	if ids[0] == ids[1] {
		t.Fatalf("two callers pasting a placeholder shared one call: %v", ids)
	}
	// A neighbour with a random id is served.
	if w := secReq(s, "GET", "/call/notary/stamp?text=mine&max_cost=1&request_id=5f0c1e7a9b3d42e8a1c6", "", "198.51.100.17:1", nil); w.Code != 200 {
		t.Fatalf("a random request_id: %d %s", w.Code, w.Body.String())
	}
	// The hosted MCP tool makes a random one when it is left out.
	server := httptest.NewServer(s)
	defer server.Close()
	for i := 0; i < 2; i++ {
		out := mcpCall(t, server.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"notary_stamp","arguments":{"text":"no id","max_cost":1}}}`)
		if dig(out, "result", "isError") == true || dig(out, "result", "structuredContent", "data", "call", "state") != "done" {
			t.Fatalf("MCP call without request_id, %d: %+v", i, out)
		}
	}
}

// L4: the discovery surfaces read the no-key and free credit offers at most
// once a minute; the hosted MCP server is rebuilt only when the offer's text
// changes, not per request.
func TestSecReview121DiscoveryOffersCached(t *testing.T) {
	store, _ := anonCallServer(t, 2000)
	slow := &slowOffers{Store: store}
	s := New(slow, nil, Config{Features: store.Features(), PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", AllowInsecureLocal: true})
	first := s.mcpServerNow()
	for i := 0; i < 3; i++ {
		if s.mcpServerNow() != first {
			t.Fatal("the hosted MCP server was rebuilt without a change")
		}
	}
	s.offerCache.mu.Lock()
	at := s.offerCache.at
	s.offerCache.mu.Unlock()
	_ = secReq(s, "GET", "/capabilities", "", "198.51.100.1:1", nil)
	_ = secReq(s, "GET", "/llms.txt", "", "198.51.100.1:1", nil)
	s.offerCache.mu.Lock()
	again := s.offerCache.at
	s.offerCache.mu.Unlock()
	if at.IsZero() || !again.Equal(at) {
		t.Fatalf("discovery read the offers again within a minute: %v then %v", at, again)
	}
	// A minute later the database hangs: the read gives up after
	// discoveryReadTimeout and keeps the last good offers, so the surfaces
	// do not flap to "nothing offered" and MCP is not rebuilt.
	s.offerCache.mu.Lock()
	s.offerCache.at = time.Now().Add(-2 * noKeyTTL)
	s.offerCache.mu.Unlock()
	slow.hang.Store(true)
	began := time.Now()
	n, _ := s.noKey()
	if took := time.Since(began); took > discoveryReadTimeout+time.Second {
		t.Fatalf("a hung read held discovery for %v", took)
	}
	if !n.Available || s.freeCredit() == nil || s.mcpServerNow() != first {
		t.Fatalf("a timed-out read dropped the offers: %+v, credit %v", n, s.freeCredit())
	}
}

// slowOffers is a store whose offer reads hang until their context ends
// while hang is set.
type slowOffers struct {
	*board.Store
	hang atomic.Bool
}

func (f *slowOffers) NoKey(ctx context.Context) services.NoKey {
	if f.hang.Load() {
		<-ctx.Done()
		return services.NoKey{}
	}
	return f.Store.NoKey(ctx)
}

func (f *slowOffers) FreeCredit(ctx context.Context) *board.FreeCredit {
	if f.hang.Load() {
		<-ctx.Done()
		return nil
	}
	return f.Store.FreeCredit(ctx)
}
