package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"swarmmemo/internal/board"
)

func sharedReceiptValidator(t *testing.T, s http.Handler) *jsonschema.Resolved {
	t.Helper()
	var spec struct {
		Components struct{ Schemas map[string]json.RawMessage }
	}
	if err := json.Unmarshal(makeRequest(s, "GET", "/openapi.json", "", "").Body.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(spec.Components.Schemas["SharedReceipt"], &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// postResult decodes a post response twice: once typed, and once raw so the
// published schema is checked against the bytes a client actually receives.
func postResult(t *testing.T, s http.Handler, validator *jsonschema.Resolved, method, path, body string) board.Result {
	t.Helper()
	w := makeRequest(s, method, path, body, "application/json")
	var result board.Result
	var raw struct {
		SharedReceipt map[string]any `json:"shared_receipt"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || json.Unmarshal(w.Body.Bytes(), &raw) != nil || result.Receipt == nil || result.SharedReceipt == nil {
		t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
	}
	if err := validator.Validate(raw.SharedReceipt); err != nil {
		t.Fatalf("shared_receipt drifted from its published schema: %v\n%s", err, w.Body.String())
	}
	return result
}

// readBack performs the cold read a receipt points at, through the same handler.
func readBack(t *testing.T, s http.Handler, shared *board.SharedReceipt) board.Message {
	t.Helper()
	path, ok := strings.CutPrefix(shared.Publication.ReadBack, "https://swarmmemo.com")
	if !ok {
		t.Fatalf("read_back is not an absolute service URL: %s", shared.Publication.ReadBack)
	}
	w := makeRequest(s, "GET", path, "", "")
	var result board.Result
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Messages) != 1 {
		t.Fatalf("read_back %s: %d %s", path, w.Code, w.Body.String())
	}
	return result.Messages[0]
}

func TestSharedReceiptReadsBackTheSameBody(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "receipts.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := New(store, nil, Config{ServiceID: "swarmmemo.com"})
	validator := sharedReceiptValidator(t, s)
	const text = "Receipt check <é>   with a literal + sign"
	for _, tc := range []struct{ name, method, path, body string }{
		{"command", "POST", "/v1/command", `{"operation":"post","room":"receipts","text":"` + strings.ReplaceAll(text, " ", ` `) + `","request_id":"shared-1"}`},
		{"get", "GET", "/w/receipts/main?format=json&request_id=shared-2&text=Receipt%20check%20%3C%C3%A9%3E%20%E2%80%A8%20with%20a%20literal%20%2B%20sign", ""},
	} {
		first := postResult(t, s, validator, tc.method, tc.path, tc.body)
		shared := first.SharedReceipt
		body := sha256.Sum256([]byte(text))
		if shared.Service != "swarmmemo.com" || shared.Agreement.BodySHA256 != hex.EncodeToString(body[:]) || shared.Agreement.BodySHA256 != first.Receipt.Hash {
			t.Fatalf("%s: agreement does not restate the body hash: %+v", tc.name, shared)
		}
		if shared.Agreement.Signature != "none" || shared.Agreement.CanonicalSHA256 != "" || shared.Agreement.Spec != "" {
			t.Fatalf("%s: unsigned post claims a signature layer: %+v", tc.name, shared.Agreement)
		}
		if shared.Acceptance.ID != first.Receipt.ID || shared.Acceptance.AcceptedAt != first.Receipt.AcceptedAt || !strings.HasPrefix(shared.Acceptance.RequestID, "shared-") || shared.Acceptance.Duplicate {
			t.Fatalf("%s: acceptance does not restate the receipt: %+v", tc.name, shared.Acceptance)
		}
		if shared.Publication.Visibility != "public" || shared.Publication.State != "unknown" {
			t.Fatalf("%s: publication must be unknown until read: %+v", tc.name, shared.Publication)
		}
		stored := readBack(t, s, shared)
		if stored.ID != shared.Acceptance.ID || stored.Hash != shared.Agreement.BodySHA256 || stored.Text != text {
			t.Fatalf("%s: read-back disagrees with the receipt: %+v", tc.name, stored)
		}
		// An exact retry is the same acceptance, flagged as such.
		again := postResult(t, s, validator, tc.method, tc.path, tc.body)
		if !again.SharedReceipt.Acceptance.Duplicate || again.SharedReceipt.Acceptance.ID != shared.Acceptance.ID || again.SharedReceipt.Agreement.BodySHA256 != shared.Agreement.BodySHA256 {
			t.Fatalf("%s: retry receipt: %+v", tc.name, again.SharedReceipt)
		}
	}
}

func TestSignedSharedReceiptBindsTheStoredCanonicalBytes(t *testing.T) {
	_, handler, sign := defaultRoomTLSFixture(t, "public")
	validator := sharedReceiptValidator(t, handler)
	command := sign(board.Command{Operation: "post", Text: "Signed receipt check", RequestID: "signed-shared-1"})
	body, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	result := postResult(t, handler, validator, "POST", "https://swarmmemo.com/v1/command", string(body))
	shared := result.SharedReceipt
	if result.Next != nil || shared.Agreement.Signature != "verified" || shared.Agreement.Spec != "swarmmemo-canonical/1" || shared.Agreement.Vector != "https://swarmmemo.com/clients/python/signing-vector.json" {
		t.Fatalf("signed agreement: %+v next=%+v", shared.Agreement, result.Next)
	}
	if shared.Acceptance.RequestID != "signed-shared-1" || shared.Publication.Visibility != "public" {
		t.Fatalf("signed acceptance/publication: %+v", shared)
	}
	stored := readBack(t, handler, shared)
	canonical := sha256.Sum256([]byte(stored.SignedPayload))
	if stored.SignedPayload == "" || hex.EncodeToString(canonical[:]) != shared.Agreement.CanonicalSHA256 || stored.Hash != shared.Agreement.BodySHA256 {
		t.Fatalf("canonical_sha256 does not match the stored signed payload: %+v %+v", shared.Agreement, stored)
	}
	// The vector the receipt names must be the one the service publishes.
	if w := makeRequest(handler, "GET", "/clients/python/signing-vector.json", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"canonical"`) {
		t.Fatalf("named vector unavailable: %d", w.Code)
	}
}

func TestPrivateSharedReceiptSaysReadBackNeedsAuthority(t *testing.T) {
	store, handler, sign := defaultRoomTLSFixture(t, "private")
	command := sign(board.Command{Operation: "post", Text: "Private receipt check", RequestID: "private-shared-1"})
	body, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	shared := postResult(t, handler, sharedReceiptValidator(t, handler), "POST", "https://swarmmemo.com/v1/command", string(body)).SharedReceipt
	if shared.Publication.Visibility != "private" || shared.Publication.State != "unknown" {
		t.Fatalf("private publication: %+v", shared.Publication)
	}
	// An unauthorized read-back is refused. That is not evidence of absence:
	// the authorized read below finds the same bytes.
	path, _ := strings.CutPrefix(shared.Publication.ReadBack, "https://swarmmemo.com")
	if w := makeRequest(handler, "GET", path, "", ""); w.Code == 200 {
		t.Fatalf("private read-back served anonymously: %s", w.Body.String())
	}
	authorized, err := store.Execute(context.Background(), sign(board.Command{Operation: "message.get", MessageID: shared.Acceptance.ID}), "fixture")
	if err != nil || len(authorized.Messages) != 1 || authorized.Messages[0].Hash != shared.Agreement.BodySHA256 {
		t.Fatalf("authorized read-back: %v %+v", err, authorized.Messages)
	}
}
