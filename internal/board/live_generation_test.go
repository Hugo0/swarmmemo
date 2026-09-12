package board

import (
	"errors"
	"testing"
)

func TestPublicCorrectionsBindGenerationAndPreserveLegacy(t *testing.T) {
	s := openTest(t, Config{})
	_, watermark, generation, err := s.PublicUpdatesGeneration(testContext, -1, "")
	if err != nil || len(generation) != 32 || watermark != 0 {
		t.Fatalf("bootstrap: watermark=%d generation=%q err=%v", watermark, generation, err)
	}
	p := run(t, s, Command{Operation: "post", Text: "generation fixture"})
	for _, c := range []Command{{Operation: "messages.list"}, {Operation: "message.get", MessageID: p.Receipt.ID}, {Operation: "thread.get", MessageID: p.Receipt.ID}} {
		if got := run(t, s, c).Generation; got != generation {
			t.Fatalf("%s generation=%q want=%q", c.Operation, got, generation)
		}
	}
	if err = s.Moderate(testContext, p.Receipt.ID, "fixture", true); err != nil {
		t.Fatal(err)
	}
	events, next, same, err := s.PublicUpdatesGeneration(testContext, watermark, generation)
	if err != nil || same != generation || next <= watermark || len(events) != 1 || !events[0].Hidden {
		t.Fatalf("bound corrections: %d %q %+v %v", next, same, events, err)
	}
	if err = s.RotateGeneration(testContext); err != nil {
		t.Fatal(err)
	}
	for _, after := range []int64{-1, 0, next} {
		events, _, leaked, err := s.PublicUpdatesGeneration(testContext, after, generation)
		var typed *Error
		if !errors.As(err, &typed) || typed.Status != 409 || typed.Code != "cursor_reset" || len(events) != 0 || leaked != "" {
			t.Fatalf("old generation accepted: after=%d events=%+v gen=%q err=%v", after, events, leaked, err)
		}
	}
	_, _, current, err := s.PublicUpdatesGeneration(testContext, -1, "")
	if err != nil || current == generation || current == "" {
		t.Fatalf("new bootstrap: %q %v", current, err)
	}
	if got := run(t, s, Command{Operation: "messages.list"}).Generation; got != current {
		t.Fatal("event page has stale generation")
	}
	legacy, legacyNext, err := s.PublicUpdates(testContext, 0)
	if err != nil || len(legacy) != 1 || legacyNext != next {
		t.Fatal("legacy correction contract changed")
	}
}
