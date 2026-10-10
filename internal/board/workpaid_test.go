package board

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// paidWork posts a request in room and opens work on it with title, caps and
// the optional reward (credits) and reward_note.
func paidWork(t *testing.T, s *Store, key ed25519.PrivateKey, room, title string, caps []string, reward int64, note string) string {
	t.Helper()
	id := run(t, s, signed(key, Command{Operation: "post", Room: room, Kind: "request", Text: title + " brief", Timestamp: s.now().Unix()})).Receipt.ID
	d := map[string]any{"schema": 1, "generation": s.generation, "title": title, "capabilities": caps}
	if reward > 0 {
		d["reward"] = reward
	}
	if note != "" {
		d["reward_note"] = note
	}
	b, _ := json.Marshal(d)
	run(t, s, signed(key, Command{Operation: "work.create", MessageID: id, Data: string(b), Timestamp: s.now().Unix()}))
	return id
}

func openPaidIDs(t *testing.T, s *Store, terms []string, limit int) []string {
	t.Helper()
	works, err := s.PublicOpenPaidWorkNaming(testContext, terms, limit)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, w := range works {
		ids = append(ids, w.ID)
	}
	return ids
}

// C141: the framework pages' paid tasks are public open work with a reward
// held or a reward_note whose title names a term as a whole word, or whose
// capability is the term, newest first. Unpaid, cancelled, private,
// hidden and other-framework work is not, nor a title holding the term
// inside a longer word.
func TestPublicOpenPaidWorkNaming(t *testing.T) {
	s := rewardStore(t)
	requester := keyFor(248)
	mintCredit(t, s, keyID(requester), allowance.Paid, 5000)
	terms := []string{"OpenAI Agents SDK", "openai-agents", "openai agents"}
	start := s.now().Unix()
	credits := paidWork(t, s, requester, "lobby", "Run the OpenAI Agents guide live", []string{"testing"}, 100, "")
	s.now = func() time.Time { return time.Unix(start+10, 0) }
	capOnly := paidWork(t, s, requester, "lobby", "Untitled live run", []string{"openai-agents"}, 0, "+0.05 USDC")
	paidWork(t, s, requester, "lobby", "Run the OpenAI Agentsmith guide", []string{"testing"}, 0, "+0.05 USDC")
	paidWork(t, s, requester, "lobby", "Run the Letta guide live", []string{"testing"}, 0, "+0.05 USDC")
	paidWork(t, s, requester, "lobby", "OpenAI Agents, unpaid", []string{"testing"}, 0, "")
	cancelled := paidWork(t, s, requester, "lobby", "OpenAI Agents cancelled", []string{"testing"}, 0, "+0.05 USDC")
	run(t, s, signed(requester, Command{Operation: "work.cancel", MessageID: cancelled, Reason: "done", Data: fmt.Sprintf(`{"schema":1,"generation":%q}`, s.generation), Timestamp: s.now().Unix()}))
	hidden := paidWork(t, s, requester, "lobby", "OpenAI Agents hidden", []string{"testing"}, 0, "+0.05 USDC")
	if err := s.Moderate(testContext, hidden, "hide", true); err != nil {
		t.Fatal(err)
	}
	run(t, s, signedNow(s, requester, Command{Operation: "room.create", Room: "paid-private", Visibility: "private"}))
	paidWork(t, s, requester, "paid-private", "OpenAI Agents private", []string{"testing"}, 0, "+0.05 USDC")

	got := openPaidIDs(t, s, terms, 3)
	if !slices.Equal(got, []string{capOnly, credits}) {
		t.Fatalf("open paid work naming OpenAI Agents: %v, want newest first %s, %s", got, capOnly, credits)
	}
	works, _ := s.PublicOpenPaidWorkNaming(testContext, terms, 3)
	for _, w := range works {
		if w.ID == credits && (w.Reward != 100 || w.RewardNote != "") || w.ID == capOnly && (w.Reward != 0 || w.RewardNote != "+0.05 USDC") {
			t.Errorf("reward of %s: %+v", w.ID, w)
		}
	}
	if got := openPaidIDs(t, s, terms, 1); len(got) != 1 {
		t.Fatalf("limit 1 returned %v", got)
	}
	if got := openPaidIDs(t, s, []string{"crewai", " "}, 3); len(got) != 0 {
		t.Fatalf("no CrewAI work, got %v", got)
	}
	if got := openPaidIDs(t, s, nil, 3); len(got) != 0 {
		t.Fatalf("no terms, got %v", got)
	}
}

func TestNamesTermIsAWholeWord(t *testing.T) {
	for _, tc := range []struct {
		title string
		caps  []string
		want  bool
	}{
		{"Run the Letta guide", nil, true},
		{"letta: end to end", nil, true},
		{"Run /for/letta live", nil, true},
		{"Lettabox adapter", nil, false},
		{"Paletta adapter", nil, false},
		{"Untitled", []string{"letta"}, true},
		{"Untitled", []string{"lettabox"}, false},
		{"Run Letta2 live", nil, false},
		{"ÄLetta", nil, false},
	} {
		if got := namesTerm(tc.title, tc.caps, []string{"letta"}); got != tc.want {
			t.Errorf("namesTerm(%q, %v) = %v, want %v", tc.title, tc.caps, got, tc.want)
		}
	}
}
