package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

type workPrivacyFixture struct {
	t                             *testing.T
	store                         *board.Store
	server                        *httptest.Server
	owner, worker                 ed25519.PrivateKey
	publicID, privateID, hiddenID string
	generation, workerID          string
	nonce                         int
}

func newWorkPrivacyFixture(t *testing.T) *workPrivacyFixture {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "work-privacy.sqlite"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f := &workPrivacyFixture{t: t, store: store}
	for _, key := range []*ed25519.PrivateKey{&f.owner, &f.worker} {
		_, *key, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
	}
	hash := sha256.Sum256(f.worker.Public().(ed25519.PublicKey))
	f.workerID = hex.EncodeToString(hash[:])
	f.run(f.sign(f.worker, board.Command{Operation: "agent.register"}))
	for _, room := range []struct{ name, visibility string }{{"public-work", "public"}, {"private-work", "private"}} {
		f.run(f.sign(f.owner, board.Command{Operation: "room.create", Room: room.name, Visibility: room.visibility, Members: []string{f.workerID}}))
	}
	f.generation = f.run(board.Command{Operation: "messages.list"}).Generation
	create := func(room, title, reason string) string {
		id := f.run(f.sign(f.owner, board.Command{Operation: "post", Room: room, Kind: "request", Text: title + " brief"})).Receipt.ID
		data, _ := json.Marshal(map[string]any{"schema": 1, "generation": f.generation, "title": title, "capabilities": []string{"review"}})
		f.run(f.sign(f.owner, board.Command{Operation: "work.create", MessageID: id, Data: string(data)}))
		f.run(f.transition(f.worker, board.Command{Operation: "work.claim", MessageID: id, TTL: 120}))
		f.run(f.transition(f.owner, board.Command{Operation: "work.reject", MessageID: id, Amount: 1, Reason: reason}))
		return id
	}
	f.publicID = create("public-work", "Public fixture title", "Public fixture reason")
	f.privateID = create("private-work", "PRIVATE_TITLE_SENTINEL", "PRIVATE_REASON_SENTINEL")
	f.hiddenID = create("public-work", "HIDDEN_TITLE_SENTINEL", "HIDDEN_REASON_SENTINEL")
	if err := store.Moderate(context.Background(), f.hiddenID, "HIDDEN_MODERATION_SENTINEL", true); err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewTLSServer(New(store, nil, Config{ServiceID: "swarmmemo.com"}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *workPrivacyFixture) sign(key ed25519.PrivateKey, c board.Command) board.Command {
	f.nonce++
	c.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	c.Timestamp = time.Now().Unix()
	c.Nonce = fmt.Sprintf("work-privacy-%d", f.nonce)
	c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
	return c
}

func (f *workPrivacyFixture) transition(key ed25519.PrivateKey, c board.Command) board.Command {
	c.Data = `{"schema":1,"generation":"` + f.generation + `"}`
	return f.sign(key, c)
}

func (f *workPrivacyFixture) run(c board.Command) board.Result {
	f.t.Helper()
	r, err := f.store.Execute(context.Background(), c, "work-privacy-fixture")
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *workPrivacyFixture) request(method, path, body string) (int, string) {
	f.t.Helper()
	r, err := http.NewRequest(method, f.server.URL+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	r.Header.Set("Accept", "application/json, text/event-stream")
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
	}
	response, err := f.server.Client().Do(r)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		f.t.Fatal(err)
	}
	if path != "/mcp" && response.Header.Get("Cache-Control") != "no-store" {
		f.t.Fatal("work response is cacheable")
	}
	return response.StatusCode, string(data)
}

func sameWorkPrivacyJSON(a, b string) bool {
	var left, right any
	if json.Unmarshal([]byte(a), &left) != nil || json.Unmarshal([]byte(b), &right) != nil {
		return false
	}
	l, _ := json.Marshal(left)
	r, _ := json.Marshal(right)
	return string(l) == string(r)
}

func (f *workPrivacyFixture) command(transport string, c board.Command) (int, string) {
	f.t.Helper()
	data, err := json.Marshal(c)
	if err != nil {
		f.t.Fatal(err)
	}
	if transport == "c64" {
		return f.request("GET", "/c64/"+base64.RawURLEncoding.EncodeToString(data), "")
	}
	return f.request("POST", "/v1/command", string(data))
}

func (f *workPrivacyFixture) noSecrets(body string) {
	f.t.Helper()
	for _, secret := range []string{f.privateID, f.hiddenID, "PRIVATE_TITLE_SENTINEL", "PRIVATE_REASON_SENTINEL", "HIDDEN_TITLE_SENTINEL", "HIDDEN_REASON_SENTINEL", "HIDDEN_MODERATION_SENTINEL"} {
		if strings.Contains(body, secret) {
			f.t.Fatalf("public response leaked %q: %s", secret, body)
		}
	}
}

func TestWorkRealHTTPAndMCPPrivacy(t *testing.T) {
	f := newWorkPrivacyFixture(t)
	missing := strings.Repeat("f", 32)
	for _, suffix := range []string{"", "/history"} {
		status, baseline := f.request("GET", "/api/work/"+missing+suffix, "")
		if status != 404 {
			t.Fatalf("missing work: %d %s", status, baseline)
		}
		for _, id := range []string{f.privateID, f.hiddenID} {
			status, body := f.request("GET", "/api/work/"+id+suffix, "")
			if status != 404 || body != baseline {
				t.Fatalf("existence oracle: %d %s vs %s", status, body, baseline)
			}
			f.noSecrets(body)
		}
		status, body := f.request("GET", "/api/work/"+f.publicID+suffix, "")
		if status != 200 || !strings.Contains(body, f.publicID) {
			t.Fatalf("public work unavailable: %d %s", status, body)
		}
		f.noSecrets(body)
	}
	for _, transport := range []string{"json", "c64"} {
		for _, op := range []string{"work.get", "work.history"} {
			_, baseline := f.command(transport, board.Command{Operation: op, MessageID: missing})
			for _, id := range []string{f.privateID, f.hiddenID} {
				status, body := f.command(transport, board.Command{Operation: op, MessageID: id})
				if status != 404 || body != baseline {
					t.Fatalf("%s %s oracle: %d %s", transport, op, status, body)
				}
				f.noSecrets(body)
			}
		}
	}
	for _, path := range []string{"/api/works", "/api/works?query=TITLE_SENTINEL", "/api/works?room=private-work"} {
		_, body := f.request("GET", path, "")
		f.noSecrets(body)
	}
	mcp := func(name string, arguments map[string]any) string {
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": arguments}})
		status, body := f.request("POST", "/mcp", string(data))
		if status != 200 {
			t.Fatalf("MCP transport failed: %d %s", status, body)
		}
		f.noSecrets(body)
		return body
	}
	for _, tool := range []string{"read_work", "read_work_history"} {
		baseline := mcp(tool, map[string]any{"message_id": missing})
		if !strings.Contains(baseline, `"isError":true`) {
			t.Fatalf("MCP missing work succeeded: %s", baseline)
		}
		for _, id := range []string{f.privateID, f.hiddenID} {
			if body := mcp(tool, map[string]any{"message_id": id}); body != baseline {
				t.Fatalf("MCP existence oracle: %s vs %s", body, baseline)
			}
		}
		if body := mcp(tool, map[string]any{"message_id": f.publicID}); !strings.Contains(body, f.publicID) || strings.Contains(body, `"isError":true`) {
			t.Fatalf("MCP public work missing: %s", body)
		}
	}
	for _, args := range []map[string]any{{}, {"query": "TITLE_SENTINEL"}, {"room": "private-work"}} {
		mcp("find_work", args)
	}
}

func TestWorkRealHTTPMembershipRevocationAndExactEnvelope(t *testing.T) {
	f := newWorkPrivacyFixture(t)
	claim := f.transition(f.worker, board.Command{Operation: "work.claim", MessageID: f.privateID, TTL: 120, RequestID: "same-private-claim"})
	status, acknowledgement := f.command("json", claim)
	if status != 200 {
		t.Fatalf("private claim failed: %d %s", status, acknowledgement)
	}
	status, replay := f.command("c64", claim)
	if status != 200 || !sameWorkPrivacyJSON(replay, acknowledgement) {
		t.Fatalf("cross-transport exact replay changed: %d %s", status, replay)
	}
	reads := []board.Command{
		f.sign(f.worker, board.Command{Operation: "works.list", Room: "private-work"}),
		f.sign(f.worker, board.Command{Operation: "work.get", MessageID: f.privateID}),
		f.sign(f.worker, board.Command{Operation: "work.history", MessageID: f.privateID}),
	}
	for _, c := range reads {
		status, body := f.command("json", c)
		otherStatus, other := f.command("c64", c)
		if status != 200 || otherStatus != 200 || body != other || !strings.Contains(body, "PRIVATE_TITLE_SENTINEL") {
			t.Fatalf("signed scoped read failed: %s %d %s", c.Operation, status, body)
		}
	}
	status, body := f.command("json", f.sign(f.worker, board.Command{Operation: "works.list"}))
	if status != 200 {
		t.Fatal(body)
	}
	f.noSecrets(body) // Signing never implicitly enumerates all private memberships.
	remove := f.sign(f.owner, board.Command{Operation: "room.member.remove", Room: "private-work", Target: f.workerID})
	if status, body := f.command("c64", remove); status != 200 {
		t.Fatalf("remove member: %d %s", status, body)
	}
	for _, transport := range []string{"json", "c64"} {
		for _, c := range reads {
			status, body := f.command(transport, c) // Exact prior reads must recheck membership, not cache permissions.
			if status != 404 {
				t.Fatalf("revoked member read succeeded: %s %d %s", c.Operation, status, body)
			}
			f.noSecrets(body)
		}
		status, body := f.command(transport, claim)
		if status != 200 || !sameWorkPrivacyJSON(body, acknowledgement) {
			t.Fatalf("historical acknowledgement changed after revocation: %d %s", status, body)
		}
		for _, forbidden := range []string{"PRIVATE_TITLE_SENTINEL", "PRIVATE_REASON_SENTINEL", "signed_payload", "public_key", "worker"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("historical retry refreshed private projection: %s", body)
			}
		}
		status, body = f.command(transport, f.transition(f.worker, board.Command{Operation: "work.renew", MessageID: f.privateID, Amount: 2, TTL: 180}))
		if status != 404 {
			t.Fatalf("revoked member transition succeeded: %d %s", status, body)
		}
		f.noSecrets(body)
	}
	history := f.run(f.sign(f.owner, board.Command{Operation: "work.history", MessageID: f.privateID})).Data["transitions"].([]board.WorkTransition)
	if len(history) != 4 || history[3].SignedPayload != string(board.Canonical("swarmmemo.com", claim)) || history[3].Signature != claim.Signature {
		t.Fatal("transport changed canonical bytes or replay fabricated transitions")
	}
}
