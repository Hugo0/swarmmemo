package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

func TestLoggedStyleSourceHashValidation(t *testing.T) {
	hash := strings.Repeat("a", 64)
	for _, tt := range []struct{ detail, want string }{
		{`{"sha256":"` + hash + `"}`, hash},
		{`{"sha256":"` + strings.Repeat("A", 64) + `"}`, ""},
		{`{"sha256":"` + strings.Repeat("a", 63) + `"}`, ""},
		{`{"sha256":"` + strings.Repeat("a", 63) + `<"}`, ""},
		{`{`, ""}, {`{}`, ""}, {strings.Repeat(" ", 4097), ""},
	} {
		if got := loggedStyleSourceHash(tt.detail); got != tt.want {
			t.Errorf("detail %q: got %q, want %q", tt.detail, got, tt.want)
		}
	}
}

func TestModlogStyleSourceHashAndServedCSS(t *testing.T) {
	s, _, _ := roomStore(t)
	ctx := context.Background()
	if _, err := s.Execute(ctx, board.Command{Operation: "post", Room: "wires", Text: "open room"}, "test"); err != nil {
		t.Fatal(err)
	}
	source := ":scope { color: red; }"
	data, err := json.Marshal(map[string]string{"css": source})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.OperatorRoom(ctx, board.Command{Operation: "room.style.set", Room: "wires", Data: string(data)})
	if err != nil {
		t.Fatal(err)
	}
	path, ok := result.Data["stylesheet"].(string)
	if !ok || path == "" {
		t.Fatal("missing stylesheet path")
	}
	served := get(t, s, path, "swarmmemo.com")
	sum := sha256.Sum256([]byte(source))
	hash := hex.EncodeToString(sum[:])
	servedSum := sha256.Sum256(served.Body.Bytes())
	if served.Code != 200 || servedSum == sum {
		t.Fatalf("served status %d, served digest equals source: %v", served.Code, servedSum == sum)
	}
	log := render(s, "/modlog/wires")
	body := log.Body.String()
	for _, want := range []string{`Logged source <code title="SHA-256: ` + hash + `" data-copy="` + hash + `"`, "the served stylesheet is sanitized and scoped, so its digest differs."} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in modlog", want)
		}
	}
	if log.Code != 200 {
		t.Fatalf("log status %d", log.Code)
	}
	if _, err := s.OperatorRoom(ctx, board.Command{Operation: "room.style.clear", Room: "wires"}); err != nil {
		t.Fatal(err)
	}
	after := render(s, "/modlog/wires").Body.String()
	if strings.Count(after, `Logged source <code`) != 1 || !strings.Contains(after, `data-copy="`+hash+`"`) {
		t.Fatal("historical source hash not retained exactly once after clear")
	}
}
