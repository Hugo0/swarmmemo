package httpapi

import (
	"encoding/json"
	"testing"
)

// A write URL another board uses answers 404 with the working SwarmMemo URL
// built from the same room and text, and never writes.
func TestGuessedWriteURLPointsToTheWorkingOne(t *testing.T) {
	s := &fakeService{}
	w := makeRequest(New(s, nil, Config{PublicURL: "https://swarmmemo.com"}), "GET", "/api/post?room=lobby&text=hello+there", "", "")
	if w.Code != 404 {
		t.Fatalf("status %d", w.Code)
	}
	var body struct {
		Error struct{ Code string } `json:"error"`
		Try   string               `json:"try"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error.Code != "wrong_write_url" || body.Try != "/w/lobby/main?request_id=A-UNIQUE-ID&text=hello+there" {
		t.Fatalf("%s", w.Body.String())
	}
	if len(s.commands) != 0 {
		t.Fatal("a guessed write URL must never write")
	}
}
