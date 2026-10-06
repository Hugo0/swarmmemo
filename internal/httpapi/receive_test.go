package httpapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/board"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
	"swarmmemo/internal/web"
)

// receiverServer is a real store with receivers and wake-ups on, hosted
// identities on, and the fake ledger.
func receiverServer(t *testing.T) (*board.Store, *Server) {
	t.Helper()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kek := filepath.Join(t.TempDir(), "hosted-kek")
	if err := os.WriteFile(kek, []byte(base64.RawURLEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	features := board.Features{Services: []string{"receiver", "wakeup"}}
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", Features: features, HostedKEKFile: kek})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	store.UseServiceMeter(servicestest.NewMeter(1<<30), &servicestest.Params{})
	return store, New(store, nil, Config{Features: features, PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"})
}

func postTo(h *Server, url, body, contentType string) *httptest.ResponseRecorder {
	return makeRequest(h, "POST", url, body, contentType)
}

func TestReceiveRouteEndToEnd(t *testing.T) {
	_, h := receiverServer(t)
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	digest := sha256.Sum256(key.Public().(ed25519.PublicKey))
	agent := hex.EncodeToString(digest[:])
	command := func(c board.Command) map[string]any {
		t.Helper()
		body, _ := json.Marshal(signService(key, c))
		w := makeRequest(h, "POST", "https://swarmmemo.com/v1/command", string(body), "application/json")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", c.Operation, w.Code, w.Body.String())
		}
		return decodeResult(t, w.Body.Bytes())
	}
	createCmd := signService(key, board.Command{Operation: "service.call", Target: "receiver", Data: `{"schema":1,"method":"create","args":{"label":"jobs","screen":false},"max_cost":5}`, RequestID: "recv-create"})
	send := func(c board.Command) map[string]any {
		t.Helper()
		body, _ := json.Marshal(c)
		w := makeRequest(h, "POST", "https://swarmmemo.com/v1/command", string(body), "application/json")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", c.Operation, w.Code, w.Body.String())
		}
		return decodeResult(t, w.Body.Bytes())
	}
	created := send(createCmd)
	url, _ := dig(created, "data", "result", "url").(string)
	if !strings.HasPrefix(url, "https://swarmmemo.com/in/") {
		t.Fatalf("create: %+v", created)
	}
	// An exact retry returns the receipt without the URL: it was shown once.
	again := send(createCmd)
	if dig(again, "data", "result", "url") != nil || dig(again, "data", "result", "receiver", "id") != dig(created, "data", "result", "receiver", "id") {
		t.Fatalf("a retry never shows the URL again: %+v", again)
	}

	// GET and HEAD are a provider's reachability check: 200 "ok", nothing
	// echoed, nothing stored (the items read below finds one item).
	for _, method := range []string{"GET", "HEAD"} {
		w := makeRequest(h, method, url+"?challenge=%3Cscript%3E&hub.challenge=echo-me", "", "")
		body := w.Body.String()
		if w.Code != 200 || (method == "GET" && body != "ok") || (method == "HEAD" && body != "") || strings.Contains(body, "echo-me") ||
			!strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Robots-Tag") != "noindex" {
			t.Fatalf("%s a receive URL: %d %q %v", method, w.Code, body, w.Header())
		}
	}
	if w := makeRequest(h, "PUT", url, "{}", "application/json"); w.Code != 405 || w.Header().Get("Allow") != "POST, GET, HEAD" {
		t.Fatalf("PUT a receive URL: %d %s", w.Code, w.Body.String())
	}
	// A provider's event id is kept for deduping; its credentials never are.
	req := httptest.NewRequest("POST", url, strings.NewReader(`{"status":"done","note":"<script>alert(1)</script>"}`))
	req.RemoteAddr = "198.51.100.8:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Colony-Event-Id", "evt_42")
	req.Header.Set("Authorization", "Bearer nope")
	req.Header.Set("Cookie", "session=nope")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 202 || !strings.Contains(w.Body.String(), `"item":`) || strings.Contains(w.Body.String(), agent) {
		t.Fatalf("deliver: %d %s", w.Code, w.Body.String())
	}
	if w = postTo(h, url, "x="+strings.Repeat("y", services.ReceiverBodyBytes), "application/x-www-form-urlencoded"); w.Code != 413 || !strings.Contains(w.Body.String(), "receiver_too_large") {
		t.Fatalf("too large: %d %s", w.Code, w.Body.String())
	}
	if w = postTo(h, url, "\x89PNG", "image/png"); w.Code != 415 {
		t.Fatalf("binary: %d %s", w.Code, w.Body.String())
	}
	// Another valid last character (one in 16 secrets already ends in A).
	wrong := url[:len(url)-1] + "A"
	if strings.HasSuffix(url, "A") {
		wrong = url[:len(url)-1] + "E"
	}
	if w = postTo(h, wrong, `{}`, "application/json"); w.Code != 404 || !strings.Contains(w.Body.String(), "receiver_not_found") {
		t.Fatalf("a wrong secret: %d %s", w.Code, w.Body.String())
	}
	if w = postTo(h, "https://swarmmemo.com/in/nothing", `{}`, "application/json"); w.Code != 404 {
		t.Fatalf("a malformed URL: %d", w.Code)
	}
	// A probe of an unknown URL answers exactly as a POST does.
	for _, u := range []string{wrong, "https://swarmmemo.com/in/nothing"} {
		post := postTo(h, u, `{}`, "application/json")
		for _, method := range []string{"GET", "HEAD"} {
			if p := makeRequest(h, method, u, "", ""); p.Code != post.Code || (method == "GET" && p.Body.String() != post.Body.String()) {
				t.Fatalf("%s %s: %d %s, POST %d %s", method, u, p.Code, p.Body.String(), post.Code, post.Body.String())
			}
		}
	}

	// The owner's own signed updates.get carries data.received; anyone
	// else's read of the agent's updates does not.
	own := command(board.Command{Operation: "updates.get", Target: agent})
	received, _ := dig(own, "data", "received").([]any)
	if len(received) != 1 || dig(received[0], "content_type") != "application/json" || dig(received[0], "body") != nil {
		t.Fatalf("own updates: %+v", own["data"])
	}
	public := decodeResult(t, makeRequest(h, "GET", "https://swarmmemo.com/api/updates?agent="+agent, "", "").Body.Bytes())
	if _, ok := dig(public, "data").(map[string]any)["received"]; ok {
		t.Fatalf("public updates show received items: %+v", public["data"])
	}
	read := command(board.Command{Operation: "service.read", Target: "receiver", Data: `{"schema":1,"method":"items","args":{}}`})
	list, _ := dig(read, "data", "result", "items").([]any)
	if len(list) != 1 || dig(list[0], "body") != `{"status":"done","note":"<script>alert(1)</script>"}` || dig(list[0], "untrusted") != true {
		t.Fatalf("items: %+v", read)
	}
	if kept, _ := dig(list[0], "headers").(map[string]any); kept["x-colony-event-id"] != "evt_42" || kept["authorization"] != nil || kept["cookie"] != nil {
		t.Fatalf("kept headers: %+v", kept)
	}
	// An unsigned read of the items is refused.
	if w = makeRequest(h, "GET", "https://swarmmemo.com/c64/"+base64.RawURLEncoding.EncodeToString([]byte(`{"operation":"service.read","target":"receiver","data":"{\"schema\":1,\"method\":\"items\"}"}`)), "", ""); w.Code != 401 {
		t.Fatalf("unsigned items: %d %s", w.Code, w.Body.String())
	}
	// A deleted receiver's URL answers a probe as it answers a POST.
	rid, _ := dig(created, "data", "result", "receiver", "id").(string)
	command(board.Command{Operation: "service.call", Target: "receiver", Data: `{"schema":1,"method":"delete","args":{"id":"` + rid + `"},"max_cost":1}`, RequestID: "recv-delete"})
	post := postTo(h, url, `{}`, "application/json")
	for _, method := range []string{"GET", "HEAD"} {
		if p := makeRequest(h, method, url, "", ""); post.Code != 404 || p.Code != 404 || (method == "GET" && p.Body.String() != post.Body.String()) {
			t.Fatalf("%s a deleted receiver: %d %s, POST %d", method, p.Code, p.Body.String(), post.Code)
		}
	}
}

// A hosted identity has the receiver tools; they sign as it.
func TestReceiverHostedTools(t *testing.T) {
	_, h := receiverServer(t)
	tools := listTools(t, h, "/mcp")
	for _, name := range []string{"receiver_create", "receiver_rotate", "receiver_delete", "receiver_list", "receiver_items"} {
		if tools[name].Name == "" {
			t.Fatalf("%s is not listed", name)
		}
	}
	if tools["receiver_items"].Annotations["readOnlyHint"] != true || tools["receiver_create"].Annotations["readOnlyHint"] == true {
		t.Fatalf("annotations: %+v %+v", tools["receiver_items"].Annotations, tools["receiver_create"].Annotations)
	}
	if _, failure := callTool(t, h, "/mcp", "", "receiver_create", map[string]any{}); !strings.Contains(failure, "hosted") {
		t.Fatalf("without an identity: %q", failure)
	}
	data := newIdentity(t, h, "")
	token := data["token"].(string)
	// The assistant profile is an allowlist a directory reviewed: receivers
	// are not on it.
	if assistant := listTools(t, h, web.AssistantMCPPath); assistant["receiver_create"].Name != "" {
		t.Fatal("the assistant profile must not grow receiver tools by itself")
	}
	created := mustTool(t, h, "/mcp/t/"+token, "", "receiver_create", map[string]any{"label": "callbacks"})
	url, _ := dig(created, "data", "result", "url").(string)
	if !strings.HasPrefix(url, "https://swarmmemo.com/in/") {
		t.Fatalf("receiver_create: %+v", created)
	}
	if w := postTo(h, url, "result: 42", "text/plain"); w.Code != 202 {
		t.Fatalf("deliver: %d %s", w.Code, w.Body.String())
	}
	got := mustTool(t, h, "/mcp/t/"+token, "", "receiver_items", map[string]any{})
	if list, _ := dig(got, "data", "result", "items").([]any); len(list) != 1 || dig(list[0], "body") != "result: 42" {
		t.Fatalf("receiver_items: %+v", got)
	}
}

// Fetch over /call/ without a key, as /tools/fetch shows it: the board's own
// hosts are refused before anything is reserved or requested, in words.
func TestFetchCallRoute(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "fetch.json")
	if err := os.WriteFile(cfg, []byte(`{"schema":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	features := board.Features{Services: []string{"fetch"}, FetchConfig: cfg, Ledger: board.LedgerOn, AnonPrefix: true}
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", Features: features})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	p := ledger.DefaultAllowanceParams()
	rp := p.Resources[allowance.Credit]
	rp.Budget, rp.SpendCeiling, rp.InboundCap = 1_600_000, 1_600_000, 1_600_000
	rp.Cap = []int64{400_000, 200_000, 100_000, 2000}
	rp.Floor = []int64{1600, 1600, 1600, 2000}
	rp.RootCap = []int64{1_600_000, 800_000, 400_000, 2000}
	rp.ShareMaxPPM = []int64{1_000_000, 1_000_000, 1_000_000, 100_000}
	if _, err = store.SetAllowanceParams(t.Context(), ledger.AllowanceNamespace, p.Marshal(), "test", 0); err != nil {
		t.Fatal(err)
	}
	h := New(store, nil, Config{Features: features, PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", AllowInsecureLocal: true})
	w := makeRequest(h, "GET", "https://swarmmemo.com/call/fetch/page?url=https://swarmmemo.com/w/lobby?text=hi", "", "")
	if w.Code != 403 || !strings.Contains(w.Body.String(), `"fetch_denied"`) || !strings.Contains(w.Body.String(), "Nothing was charged") {
		t.Fatalf("own host: %d %s", w.Code, w.Body.String())
	}
	w = makeRequest(h, "POST", "https://swarmmemo.com/call/fetch/page", "url=http%3A%2F%2F10.0.0.1%2F", "application/x-www-form-urlencoded")
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"fetch_address_blocked"`) {
		t.Fatalf("private address: %d %s", w.Code, w.Body.String())
	}
	if w = makeRequest(h, "GET", "https://swarmmemo.com/call/fetch/page?url=https://example.com/&max_bytes=65536", "", ""); w.Code != 400 {
		t.Fatalf("more than 8 KiB without a key: %d %s", w.Code, w.Body.String())
	}
	tools := listTools(t, h, "/mcp")
	if tools["fetch_page"].Name == "" {
		t.Fatal("fetch_page is not an MCP tool")
	}
}

// /api/stats/daily counts receivers, stored deliveries and wake-ups by kind,
// today included: counts only, never an id, a key, a label, a URL or a body.
func TestDailyStatsCountReceiversAndWakeups(t *testing.T) {
	_, h := receiverServer(t)
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	n := 0
	call := func(service, method string, args map[string]any) map[string]any {
		t.Helper()
		n++
		data, _ := json.Marshal(map[string]any{"schema": 1, "method": method, "args": args, "max_cost": 1000})
		body, _ := json.Marshal(signService(key, board.Command{Operation: "service.call", Target: service, Data: string(data), RequestID: "wake-stats-" + strconv.Itoa(n)}))
		w := makeRequest(h, "POST", "https://swarmmemo.com/v1/command", string(body), "application/json")
		if w.Code != 200 {
			t.Fatalf("%s.%s: %d %s", service, method, w.Code, w.Body.String())
		}
		return decodeResult(t, w.Body.Bytes())
	}
	created := call("receiver", "create", map[string]any{"label": "secret-label", "screen": false})
	url, _ := dig(created, "data", "result", "url").(string)
	rid, _ := dig(created, "data", "result", "receiver", "id").(string)
	call("receiver", "create", map[string]any{"label": "second"})
	call("wakeup", "schedule", map[string]any{"key": "secret-once", "at": time.Now().Unix() + 3600})
	call("wakeup", "schedule", map[string]any{"key": "hourly", "every": 3600})
	call("wakeup", "schedule", map[string]any{"key": "replies", "on": "reply"})
	call("wakeup", "schedule", map[string]any{"key": "inbox", "on": "received"})
	// One stored delivery fires the received wake-up; a refused one counts
	// nothing.
	if w := postTo(h, url, `{"status":"secret body"}`, "application/json"); w.Code != 202 {
		t.Fatalf("deliver: %d %s", w.Code, w.Body.String())
	}
	if w := postTo(h, url, "\x89PNG", "image/png"); w.Code != 415 {
		t.Fatalf("binary: %d %s", w.Code, w.Body.String())
	}

	w := makeRequest(h, "GET", "/api/stats/daily?days=2", "", "")
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("daily: %d %s", w.Code, body)
	}
	for _, leak := range []string{rid, url[strings.LastIndex(url, "/")+1:], "secret", "hourly", "inbox"} {
		if leak == "" || strings.Contains(body, leak) {
			t.Fatalf("daily stats carry %q: %s", leak, body)
		}
	}
	type wakeups struct {
		Scheduled map[string]int64 `json:"scheduled"`
		Fired     int64            `json:"fired"`
	}
	type receivers struct {
		Created    int64 `json:"created"`
		Deliveries int64 `json:"deliveries"`
	}
	var got struct {
		Daily []struct {
			Receivers *receivers `json:"receivers"`
			Wakeups   *wakeups   `json:"wakeups"`
		} `json:"daily"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Daily) != 2 || got.Daily[0].Receivers == nil || got.Daily[1].Wakeups == nil {
		t.Fatalf("daily: %v %s", err, body)
	}
	today, before := got.Daily[1], got.Daily[0]
	if *today.Receivers != (receivers{Created: 2, Deliveries: 1}) {
		t.Fatalf("today's receivers: %+v", *today.Receivers)
	}
	if s := today.Wakeups.Scheduled; s["one_shot"] != 1 || s["event"] != 2 || s["recurring"] != 1 || today.Wakeups.Fired != 1 {
		t.Fatalf("today's wake-ups: %+v", *today.Wakeups)
	}
	if *before.Receivers != (receivers{}) || before.Wakeups == nil || before.Wakeups.Fired != 0 || len(before.Wakeups.Scheduled) != 3 {
		t.Fatalf("yesterday: %s", body)
	}
}
