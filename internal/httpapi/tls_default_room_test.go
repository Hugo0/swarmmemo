package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

func TestPlaintextSignedPostFailsClosedWhenRoomLookupFails(t *testing.T) {
	service := &fakeService{err: errors.New("temporary storage failure")}
	handler := New(service, nil, Config{ServiceID: "swarmmemo.com"})
	response := makeRequest(handler, "POST", "http://swarmmemo.com/v1/command", `{"operation":"post","text":"sensitive","public_key":"test-key"}`, "application/json")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"https_required"`) {
		t.Fatalf("uncertain privacy must require TLS: %d %s", response.Code, response.Body.String())
	}
	if len(service.commands) != 1 || service.commands[0].Operation != "room.get" || service.commands[0].Room != "lobby" {
		t.Fatalf("write attempted after failed visibility lookup: %+v", service.commands)
	}
}

func defaultRoomTLSFixture(t *testing.T, visibility string) (*board.Store, http.Handler, func(board.Command) board.Command) {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "board.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nonce := 0
	sign := func(c board.Command) board.Command {
		nonce++
		c.PublicKey = base64.RawURLEncoding.EncodeToString(public)
		c.Timestamp = time.Now().Unix()
		c.Nonce = fmt.Sprintf("default-room-tls-%d", nonce)
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, board.Canonical("swarmmemo.com", c)))
		return c
	}
	for _, c := range []board.Command{{Operation: "agent.register"}, {Operation: "room.create", Room: "lobby", Visibility: visibility}} {
		if _, err := store.Execute(context.Background(), sign(c), "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	return store, New(store, nil, Config{ServiceID: "swarmmemo.com"}), sign
}

func TestDefaultPrivateRoomRejectsPlaintextSignedPost(t *testing.T) {
	store, handler, sign := defaultRoomTLSFixture(t, "private")
	command := sign(board.Command{Operation: "post", Text: "Private default-room message", RequestID: "default-private-post"})
	body, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := makeRequest(handler, "POST", "http://swarmmemo.com/v1/command", string(body), "application/json")
	if plaintext.Code != http.StatusBadRequest || !strings.Contains(plaintext.Body.String(), `"https_required"`) {
		t.Fatalf("private default room accepted plaintext or returned wrong error: %d %s", plaintext.Code, plaintext.Body.String())
	}
	before, err := store.Execute(context.Background(), sign(board.Command{Operation: "messages.list", Room: "lobby"}), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Messages) != 0 {
		t.Fatal("rejected plaintext request persisted a private post")
	}
	// Reusing the exact signed envelope over HTTPS must succeed: the precheck
	// must neither consume its nonce nor insert defaults into signed bytes.
	secure := makeRequest(handler, "POST", "https://swarmmemo.com/v1/command", string(body), "application/json")
	if secure.Code != http.StatusOK {
		t.Fatalf("HTTPS default-room post failed: %d %s", secure.Code, secure.Body.String())
	}
	after, err := store.Execute(context.Background(), sign(board.Command{Operation: "messages.list", Room: "lobby"}), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Messages) != 1 || after.Messages[0].Text != command.Text || after.Messages[0].Room != "lobby" {
		t.Fatalf("unexpected stored private messages: %+v", after.Messages)
	}
	if after.Messages[0].SignedPayload != string(board.Canonical("swarmmemo.com", command)) {
		t.Fatal("transport normalization changed the signed command")
	}
}

func TestDefaultPublicRoomRetainsPlaintextPostingCompatibility(t *testing.T) {
	store, handler, sign := defaultRoomTLSFixture(t, "public")
	command := sign(board.Command{Operation: "post", Text: "Signed public default-room message", RequestID: "default-public-post"})
	body, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	response := makeRequest(handler, "POST", "http://swarmmemo.com/v1/command", string(body), "application/json")
	if response.Code != http.StatusOK {
		t.Fatalf("public default-room signed HTTP post failed: %d %s", response.Code, response.Body.String())
	}
	anonymous := makeRequest(handler, "POST", "http://swarmmemo.com/v1/command", `{"operation":"post","text":"Anonymous public default-room message"}`, "application/json")
	if anonymous.Code != http.StatusOK {
		t.Fatalf("public default-room anonymous HTTP post failed: %d %s", anonymous.Code, anonymous.Body.String())
	}
	result, err := store.Execute(context.Background(), board.Command{Operation: "messages.list", Room: "lobby"}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("want two public posts, got %d", len(result.Messages))
	}
	for _, event := range result.Messages {
		if event.PublicKey != "" && event.SignedPayload != string(board.Canonical("swarmmemo.com", command)) {
			t.Fatal("public HTTP precheck changed signed command")
		}
	}
}
