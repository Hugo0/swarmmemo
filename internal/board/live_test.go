package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestPublicUpdatesModerationAndAttachmentCorrections(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(81)
	run(t, s, Command{Operation: "post", Room: "public", Text: "room setup"})
	blob := upload(t, s, key, "public", "untrusted binary", 0)
	post := run(t, s, signed(key, Command{Operation: "post", Room: "public", Text: "remove this text", Attachments: []string{blob.ID}}))
	events, watermark, err := s.PublicUpdates(testContext, -1)
	if err != nil || len(events) != 0 || watermark != 0 {
		t.Fatalf("initial watermark: %d events=%d err=%v", watermark, len(events), err)
	}
	if err = s.Moderate(testContext, post.Receipt.ID, "hidden", true); err != nil {
		t.Fatal(err)
	}
	events, hiddenPosition, err := s.PublicUpdates(testContext, watermark)
	if err != nil || len(events) != 1 || hiddenPosition <= watermark {
		t.Fatalf("missing immediate removal: %v %+v", err, events)
	}
	removed := events[0]
	if removed.ID != post.Receipt.ID || removed.Type != "tombstone" || !removed.Hidden || removed.Text != "" || removed.Signature != "" || removed.SignedPayload != "" || len(removed.Attachments) != 0 {
		t.Fatalf("removal retained content or provenance: %+v", removed)
	}
	if err = s.Moderate(testContext, post.Receipt.ID, "restored", false); err != nil {
		t.Fatal(err)
	}
	events, restoredPosition, err := s.PublicUpdates(testContext, hiddenPosition)
	if err != nil || len(events) != 1 || events[0].Hidden || events[0].Text != "remove this text" || len(events[0].Attachments) != 1 || restoredPosition <= hiddenPosition {
		t.Fatalf("missing restoration: %v %+v", err, events)
	}
	run(t, s, signed(key, Command{Operation: "blob.delete", MessageID: blob.ID}))
	events, deletedPosition, err := s.PublicUpdates(testContext, restoredPosition)
	if err != nil || len(events) != 1 || len(events[0].Attachments) != 1 || !events[0].Attachments[0].Deleted || deletedPosition <= restoredPosition {
		t.Fatalf("missing attachment deletion: %v %+v", err, events)
	}
	events, unchanged, err := s.PublicUpdates(testContext, deletedPosition)
	if err != nil || len(events) != 0 || unchanged != deletedPosition {
		t.Fatal("empty update page advanced its watermark")
	}
	events, current, err := s.PublicUpdates(testContext, -1)
	if err != nil || len(events) != 0 || current != deletedPosition {
		t.Fatal("watermark initialization replayed history or missed latest change")
	}
}

func TestPublicUpdatesExcludePrivateChanges(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(82)
	run(t, s, signed(key, Command{Operation: "room.create", Room: "secret", Visibility: "private"}))
	post := run(t, s, signed(key, Command{Operation: "post", Room: "secret", Text: "private never public"}))
	if err := s.Moderate(testContext, post.Receipt.ID, "private moderation", true); err != nil {
		t.Fatal(err)
	}
	// Defense in depth: an incorrectly queued private change must remain inaccessible.
	if _, err := s.db.Exec("INSERT INTO changes(event_id,changed_at,urgent) VALUES(?,?,1)", post.Receipt.ID, testTime); err != nil {
		t.Fatal(err)
	}
	for _, after := range []int64{-1, 0} {
		events, next, err := s.PublicUpdates(testContext, after)
		if err != nil || len(events) != 0 || next != 0 {
			t.Fatalf("private change leaked at after=%d: next=%d events=%+v err=%v", after, next, events, err)
		}
	}
}

func TestPublicUpdatesPaginationBoundsAndNoGaps(t *testing.T) {
	for _, test := range []struct {
		name, text string
		count      int
	}{
		{"count", "short", 105},
		{"bytes", strings.Repeat("x", 16300), 12},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := openTest(t, Config{})
			want := map[string]bool{}
			for i := 0; i < test.count; i++ {
				post := run(t, s, Command{Operation: "post", Room: "public", Text: fmt.Sprintf("%d %s", i, test.text)})
				want[post.Receipt.ID] = true
				// A visible correction carries the entire text and exercises byte bounds.
				if err := s.Moderate(testContext, post.Receipt.ID, "reviewed", false); err != nil {
					t.Fatal(err)
				}
			}
			var after int64
			seen := map[string]bool{}
			pages := 0
			for ; pages < 200; pages++ {
				events, next, err := s.PublicUpdates(testContext, after)
				if err != nil {
					t.Fatal(err)
				}
				if len(events) == 0 {
					if next != after {
						t.Fatal("empty page moved revision")
					}
					break
				}
				if len(events) > 100 || next <= after {
					t.Fatal("count bound or monotonic revision failed")
				}
				bytes := 0
				for _, event := range events {
					encoded, _ := json.Marshal(event)
					bytes += len(encoded)
					if seen[event.ID] || !want[event.ID] {
						t.Fatal("duplicate or unexpected correction")
					}
					seen[event.ID] = true
				}
				if len(events) > 1 && bytes > 64<<10 {
					t.Fatal("multi-event page exceeded byte bound")
				}
				after = next
			}
			if len(seen) != len(want) || pages < 2 || pages >= 200 {
				t.Fatalf("pagination lost updates: seen=%d want=%d pages=%d", len(seen), len(want), pages)
			}
		})
	}
}

func TestPublicUpdatesInvalidRevisionAndCancellation(t *testing.T) {
	s := openTest(t, Config{})
	_, _, err := s.PublicUpdates(testContext, -2)
	var problem *Error
	if !errors.As(err, &problem) || problem.Code != "invalid_revision" {
		t.Fatalf("expected invalid revision: %v", err)
	}
	ctx, cancel := context.WithCancel(testContext)
	cancel()
	if _, _, err = s.PublicUpdates(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not honored: %v", err)
	}
}

func TestPublicUpdatesDeliverOneOversizedSignedEventIntact(t *testing.T) {
	s := openTest(t, Config{})
	text := strings.Repeat("\"", 16000)
	post := run(t, s, signed(keyFor(83), Command{Operation: "post", Text: text}))
	if err := s.Moderate(testContext, post.Receipt.ID, "reviewed", false); err != nil {
		t.Fatal(err)
	}
	events, next, err := s.PublicUpdates(testContext, 0)
	if err != nil || len(events) != 1 || events[0].Text != text || next <= 0 {
		t.Fatalf("large signed update truncated or missing: events=%d err=%v", len(events), err)
	}
	encoded, err := json.Marshal(events[0])
	if err != nil || len(encoded) <= 64<<10 || events[0].SignedPayload == "" || events[0].Signature == "" {
		t.Fatal("fixture did not exercise oversized complete provenance")
	}
}
