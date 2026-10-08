package board

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

func earnCreate(s *Store, key ed25519.PrivateKey, id, title string, caps []string, reward int64) Command {
	d := map[string]any{"schema": 1, "generation": s.generation, "title": title, "capabilities": caps, "reward": reward}
	b, _ := json.Marshal(d)
	return signed(key, Command{Operation: "work.create", MessageID: id, Data: string(b), Timestamp: s.now().Unix()})
}

func earnTitles(t *testing.T, s *Store, c Command) ([]string, string) {
	t.Helper()
	r := run(t, s, c)
	var titles []string
	for _, w := range r.Data["works"].([]Work) {
		titles = append(titles, w.Title)
	}
	return titles, r.NextCursor
}

// kind=earn lists what kind=rewarded lists (open, public, a reward held),
// standing tasks tagged earn first, then the smallest reward; its cursor
// pages in that order.
func TestWorksListEarnSmallestEffortFirst(t *testing.T) {
	s := rewardStore(t)
	requester, worker := keyFor(250), keyFor(251)
	mintCredit(t, s, keyID(requester), allowance.Paid, 5000)
	create := func(title string, caps []string, reward int64) string {
		id := rewardRequest(t, s, requester, "lobby")
		run(t, s, earnCreate(s, requester, id, title, caps, reward))
		return id
	}
	create("big review", []string{"review"}, 300)
	create("standing QA", []string{"qa", WorkEarnTag}, 200)
	create("tiny proof check", []string{"verify"}, 10)
	create("standing witness", []string{WorkEarnTag}, 40)
	claimed := create("claimed one", []string{"review"}, 5)
	run(t, s, claimCommand(s, worker, claimed))
	createTestWork(t, s, requester, "lobby", "request", 0) // unpaid

	want := []string{"standing witness", "standing QA", "tiny proof check", "big review"}
	got, _ := earnTitles(t, s, Command{Operation: "works.list", Kind: WorkKindEarn})
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("earn order %q, want %q", got, want)
	}
	if rewarded := len(run(t, s, Command{Operation: "works.list", Kind: WorkKindRewarded}).Data["works"].([]Work)); rewarded != len(want) {
		t.Fatalf("earn lists %d, rewarded %d", len(want), rewarded)
	}
	var paged []string
	cursor := ""
	for i := 0; i < 10; i++ {
		page, next := earnTitles(t, s, Command{Operation: "works.list", Kind: WorkKindEarn, Limit: 1, Cursor: cursor})
		paged = append(paged, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	if fmt.Sprint(paged) != fmt.Sprint(want) {
		t.Fatalf("paged earn order %q, want %q", paged, want)
	}
	// A cursor from another listing does not page this one.
	_, other := earnTitles(t, s, Command{Operation: "works.list", Kind: WorkKindRewarded, Limit: 1})
	if _, err := s.Execute(testContext, Command{Operation: "works.list", Kind: WorkKindEarn, Limit: 1, Cursor: other}, "test-origin"); err == nil {
		t.Fatal("a rewarded cursor paged the earn listing")
	}
	// Room and query filters still apply.
	got, _ = earnTitles(t, s, Command{Operation: "works.list", Kind: WorkKindEarn, Query: WorkEarnTag})
	if fmt.Sprint(got) != fmt.Sprint(want[:2]) {
		t.Fatalf("earn tag query %q", got)
	}
}

// Every refusal for running out of credits points at the earn listing.
func TestOutOfCreditsRefusalsPointAtEarn(t *testing.T) {
	for _, code := range []string{"quota_exhausted", "not_transferable"} {
		var e *Error
		if !errors.As(allowanceError(code), &e) || !strings.HasSuffix(e.Message, EarnHint) {
			t.Fatalf("%s: %v", code, allowanceError(code))
		}
	}
	if !strings.Contains(EarnHint, "Out of credits? Earn some by doing a small paid task: /work?kind=earn") {
		t.Fatalf("hint %q", EarnHint)
	}
	// The legacy daily cap says it too.
	s := openTest(t, Config{ArchiveDelaySeconds: -1, AnonymousDailyBytes: 1200})
	for i := 0; i < 20; i++ {
		_, err := s.Execute(testContext, Command{Operation: "post", Text: fmt.Sprint("post ", i)}, "test-origin")
		if err == nil {
			continue
		}
		var e *Error
		if !errors.As(err, &e) || e.Code != "quota_exhausted" || !strings.HasSuffix(e.Message, EarnHint) {
			t.Fatalf("refusal %v", err)
		}
		return
	}
	t.Fatal("the daily cap never refused")
}
