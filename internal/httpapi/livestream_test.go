package httpapi

// The authenticated live stream (livestream.go): an app's token streams
// only the rooms its scopes reach, a signed key its app rooms and inbox,
// within a second of the write, resumable with Last-Event-ID, and a masked
// room's events carry no key.

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

type sseEvent struct{ id, event, data string }

// openStream GETs /api/stream?query with headers and returns its events
// as they arrive; the stream closes with the test.
func openStream(t *testing.T, base, query string, headers map[string]string) (<-chan sseEvent, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/stream?"+query, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan sseEvent, 64)
	if res.StatusCode != 200 {
		res.Body.Close()
		close(events)
		return events, res.StatusCode
	}
	go func() {
		defer res.Body.Close()
		defer close(events)
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 1<<16), 1<<20)
		var e sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if e.event != "" {
					events <- e
				}
				e = sseEvent{}
			case strings.HasPrefix(line, "id: "):
				e.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				e.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				e.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return events, 200
}

// nextMessage is the next message event within wait, or fails.
func nextMessage(t *testing.T, events <-chan sseEvent, wait time.Duration) (sseEvent, board.Message) {
	t.Helper()
	timeout := time.After(wait)
	for {
		select {
		case e, ok := <-events:
			if !ok {
				t.Fatal("the stream ended")
			}
			if e.event != "message" {
				continue
			}
			var m board.Message
			if err := json.Unmarshal([]byte(e.data), &m); err != nil {
				t.Fatal(err)
			}
			return e, m
		case <-timeout:
			t.Fatalf("no message within %v", wait)
		}
	}
}

// noMessage fails on a message event within wait.
func noMessage(t *testing.T, events <-chan sseEvent, wait time.Duration) {
	t.Helper()
	timeout := time.After(wait)
	for {
		select {
		case e, ok := <-events:
			if !ok {
				return
			}
			if e.event == "message" {
				t.Fatalf("unexpected message: %s", e.data)
			}
		case <-timeout:
			return
		}
	}
}

func waitReady(t *testing.T, events <-chan sseEvent) sseEvent {
	t.Helper()
	select {
	case e := <-events:
		if e.event != "ready" || e.id == "" {
			t.Fatalf("first event = %+v", e)
		}
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no ready event")
	}
	return sseEvent{}
}

// jwtSubject is an app token's sub, unchecked.
func jwtSubject(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct{ Sub string }
	if json.Unmarshal(raw, &claims) != nil || claims.Sub == "" {
		t.Fatalf("token claims: %s", raw)
	}
	return claims.Sub
}

func TestLiveStreamAppTokenAndSigned(t *testing.T) {
	store, s := hostedServer(t)
	ctx := t.Context()
	app, other := ed25519.NewKeyFromSeed(make([]byte, 32)), ed25519.NewKeyFromSeed([]byte(strings.Repeat("o", 32)))
	alice, bob := ed25519.NewKeyFromSeed([]byte(strings.Repeat("a", 32))), ed25519.NewKeyFromSeed([]byte(strings.Repeat("b", 32)))
	exec := func(key ed25519.PrivateKey, c board.Command) board.Result {
		t.Helper()
		res, err := store.Execute(ctx, signedNow(key, c), "198.51.100.9")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		return res
	}
	exec(app, board.Command{Operation: "app.register", Data: `{"schema":1,"name":"Hex Arena","homepage":"https://app.example"}`, RequestID: "a"})
	exec(other, board.Command{Operation: "app.register", Data: `{"schema":1,"name":"Other","homepage":"https://other.example"}`, RequestID: "o"})
	exec(alice, board.Command{Operation: "agent.register", Handle: "alice"})
	exec(bob, board.Command{Operation: "agent.register", Handle: "bobby"})
	client := exec(app, board.Command{Operation: "oauth.client.register", RequestID: "c",
		Data: `{"schema":1,"name":"Hex Arena","redirect_uris":["` + testRedirect + `"],"scopes":["identity","app-rooms"]}`}).Data["client"].(board.OAuthAppClient)
	appFP, otherFP := client.App, keyFP(other)
	request, cookie := consent(t, s, authorizeQuery(client.ID, testRedirect, "st", map[string]string{"scope": "app-rooms", "resource": ""}))
	back, _ := continued(t, s, cookie, submit(s, request, cookie, map[string]string{"action": "create"}, nil))
	_, tokens, _ := tokenRequest(s, url.Values{"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {client.ID},
		"redirect_uri": {testRedirect}, "code_verifier": {testVerifier}})
	access, _ := tokens["access_token"].(string)
	person := jwtSubject(t, access)

	mine, theirs := "app/"+appFP+"/table", "app/"+otherFP+"/table"
	exec(app, board.Command{Operation: "room.create", Room: mine, Members: []string{person, keyFP(alice)}, RequestID: "r1"})
	exec(other, board.Command{Operation: "room.create", Room: theirs, Members: []string{person, keyFP(alice)}, RequestID: "r2"})
	// Closed after every stream's own cleanup has cancelled it (LIFO).
	server := httptest.NewServer(s)
	t.Cleanup(server.Close)

	// An app token's stream: only this app's rooms the person is in.
	bearer := map[string]string{"Authorization": "Bearer " + access}
	if _, code := openStream(t, server.URL, "room=lobby", bearer); code != 400 {
		t.Fatalf("a bearer stream with room: %d", code)
	}
	if _, code := openStream(t, server.URL, "", map[string]string{"Authorization": "Bearer " + access[:len(access)-4] + "AAAA"}); code != 401 {
		t.Fatalf("a forged token: %d", code)
	}
	events, code := openStream(t, server.URL, "", bearer)
	if code != 200 {
		t.Fatalf("bearer stream: %d", code)
	}
	waitReady(t, events)
	exec(other, board.Command{Operation: "post", Room: theirs, Text: "another app's room", RequestID: "p1"})
	exec(alice, board.Command{Operation: "post", Room: "lobby", To: person, Text: "addressed, in public", RequestID: "p2"})
	sent := time.Now()
	exec(alice, board.Command{Operation: "post", Room: mine, Text: "your move", RequestID: "p3"})
	first, m := nextMessage(t, events, 2*time.Second)
	if took := time.Since(sent); took > time.Second {
		t.Fatalf("the message took %v", took)
	}
	if m.Text != "your move" || m.Room != mine || first.id == "" {
		t.Fatalf("bearer stream carried %s", first.data)
	}
	noMessage(t, events, 300*time.Millisecond)

	// Resume: a reconnect with Last-Event-ID gets what it missed, once.
	exec(alice, board.Command{Operation: "post", Room: mine, Text: "while away", RequestID: "p4"})
	resumed, _ := openStream(t, server.URL, "", map[string]string{"Authorization": "Bearer " + access, "Last-Event-ID": first.id})
	waitReady(t, resumed)
	if _, m := nextMessage(t, resumed, 2*time.Second); m.Text != "while away" {
		t.Fatalf("resumed with %q", m.Text)
	}
	noMessage(t, resumed, 300*time.Millisecond)

	// A signed key's stream: its app rooms of every app and its inbox.
	signedQuery := func(key ed25519.PrivateKey) string {
		c := signedNow(key, board.Command{Operation: "stream.read"})
		return url.Values{"public_key": {c.PublicKey}, "signature": {c.Signature}, "timestamp": {strconv.FormatInt(c.Timestamp, 10)}, "nonce": {c.Nonce}}.Encode()
	}
	if _, code := openStream(t, server.URL, signedQuery(alice)+"&room=lobby", nil); code != 400 {
		t.Fatalf("a signed stream with room: %d", code)
	}
	bad := signedNow(alice, board.Command{Operation: "stream.read"})
	if _, code := openStream(t, server.URL, url.Values{"public_key": {bad.PublicKey}, "signature": {bad.Signature}, "timestamp": {"1"}, "nonce": {bad.Nonce}}.Encode(), nil); code != 401 {
		t.Fatalf("a tampered signed stream: %d", code)
	}
	own, _ := openStream(t, server.URL, signedQuery(alice), nil)
	waitReady(t, own)
	exec(bob, board.Command{Operation: "post", Room: "lobby", To: keyFP(alice), Text: "to alice", RequestID: "q1"})
	exec(other, board.Command{Operation: "post", Room: theirs, Text: "other app, alice there", RequestID: "q2"})
	got := map[string]bool{}
	for range 2 {
		_, m := nextMessage(t, own, 2*time.Second)
		got[m.Text] = true
	}
	if !got["to alice"] || !got["other app, alice there"] {
		t.Fatalf("signed stream = %v", got)
	}

	// A masked room's events carry no member's key.
	masked := "app/" + appFP + "/mafia"
	exec(app, board.Command{Operation: "room.create", Room: masked, Members: []string{keyFP(alice), keyFP(bob)}, Data: `{"reveal_on_close":true}`, RequestID: "r3"})
	exec(bob, board.Command{Operation: "post", Room: masked, Text: "who am I", RequestID: "q3"})
	e, m := nextMessage(t, own, 2*time.Second)
	if !m.Masked || strings.Contains(e.data, keyFP(bob)) || strings.Contains(e.data, base64.RawURLEncoding.EncodeToString(bob.Public().(ed25519.PublicKey))) || strings.Contains(e.data, "bobby") {
		t.Fatalf("masked event = %s", e.data)
	}

	// Revoked, a token's open stream ends at its next read, carrying
	// nothing written after the revoke.
	if err := store.OAuthRevoke(ctx, access, client.ID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	exec(alice, board.Command{Operation: "post", Room: mine, Text: "after the revoke", RequestID: "p5"})
	for timeout := time.After(3 * time.Second); ; {
		select {
		case e, ok := <-events:
			if !ok {
				t.Fatal("the revoked stream closed without event: end")
			}
			if e.event == "message" && strings.Contains(e.data, "after the revoke") {
				t.Fatalf("a revoked token streamed %s", e.data)
			}
			if e.event == "end" {
				return
			}
		case <-timeout:
			t.Fatal("a revoked token's stream stayed open")
		}
	}
}

func keyFP(key ed25519.PrivateKey) string {
	sum := sha256.Sum256(key.Public().(ed25519.PublicKey))
	return hex.EncodeToString(sum[:])
}

// Authenticated live streams hold at most half the shared stream capacity:
// keys anyone can mint, from many addresses, never starve the public
// /api/stream.
func TestLiveStreamsLeavePublicCapacity(t *testing.T) {
	_, s := hostedServer(t)
	for i := 0; i < cap(s.streams); i++ {
		if _, full := s.lives.Acquire("203.0."+strconv.Itoa(i/250)+"."+strconv.Itoa(i%250+1), "k"+strconv.Itoa(i)); full != "" {
			if i > cap(s.streams)/2 || i == 0 {
				t.Fatalf("full at %d of %d", i, cap(s.streams))
			}
			return
		}
	}
	t.Fatalf("live streams can take all %d shared stream slots", cap(s.streams))
}
