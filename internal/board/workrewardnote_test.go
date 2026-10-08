package board

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// A work item's reward_note on the real store: strict validation at
// create, kept in the signed create command, and the same text on every
// read that shows the work (work.get, works.list, the earn list, history
// and the message's work mark). It never touches the ledger.

const testRewardNote = "+0.10 USDC on Base, paid by the poster"

func noteCreate(s *Store, key ed25519.PrivateKey, id string, extra map[string]any) Command {
	d := map[string]any{"schema": 1, "generation": s.generation, "title": "Noted task", "capabilities": []string{"review"}}
	for k, v := range extra {
		d[k] = v
	}
	b, _ := json.Marshal(d)
	return signed(key, Command{Operation: "work.create", MessageID: id, Data: string(b), Timestamp: s.now().Unix()})
}

func TestValidWorkRewardNote(t *testing.T) {
	for note, ok := range map[string]bool{
		testRewardNote:                    true,
		"x":                               true,
		strings.Repeat("é", 80):           true, // 80 characters, 160 bytes
		strings.Repeat("a", 80):           true,
		strings.Repeat("a", 81):           false,
		"":                                false,
		" leading":                        false,
		"trailing ":                       false,
		"two\nlines":                      false,
		"carriage\rreturn":                false,
		"tab\there":                       false,
		"nul\x00":                         false,
		"del\x7f":                         false,
		"bell\a":                          false,
		"c1 \u0085 next line":             false,
		"bidi \u202e override":            false,
		"zero\u200bwidth":                 false,
		"line\u2028separator":             false,
		"nbsp\u00a0inside":                false,
		"bad utf8 \xff":                   false,
		"Reward: 5 € or 0.1 ETH (poster)": true,
	} {
		if got := validWorkRewardNote(note); got != ok {
			t.Errorf("validWorkRewardNote(%q) = %v, want %v", note, got, ok)
		}
	}
}

func TestWorkRewardNoteValidationOnCreate(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker := keyFor(240), keyFor(241)
	id := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: "Brief", Timestamp: s.now().Unix()})).Receipt.ID
	for _, bad := range []string{strings.Repeat("a", 81), "two\nlines", "tab\there", "ctl\x01", "del\x7f", " padded", "bidi\u202e", ""} {
		fails(t, s, noteCreate(s, owner, id, map[string]any{"reward_note": bad}), "invalid_reward_note")
	}
	// Not a string, or null: the strict data shape refuses it.
	for _, bad := range []any{5, true, []string{"x"}} {
		fails(t, s, noteCreate(s, owner, id, map[string]any{"reward_note": bad}), "invalid_work_data")
	}
	fails(t, s, signed(owner, Command{Operation: "work.create", MessageID: id, Data: `{"schema":1,"generation":"` + s.generation + `","title":"x","capabilities":[],"reward_note":null}`, Timestamp: s.now().Unix()}), "invalid_work_data")
	if n := sqlCount(t, s, "SELECT count(*) FROM works WHERE id=?", id); n != 0 {
		t.Fatal("a refused note created work")
	}
	// A simulation carries no reward of any kind.
	sim := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Kind: "simulation", Text: "lab", Timestamp: s.now().Unix()})).Receipt.ID
	fails(t, s, noteCreate(s, owner, sim, map[string]any{"reward_note": testRewardNote}), "invalid_reward_note")

	run(t, s, noteCreate(s, owner, id, map[string]any{"reward_note": testRewardNote}))
	// Set once, at create: no later transition carries it.
	fails(t, s, signed(worker, Command{Operation: "work.claim", MessageID: id, TTL: 600, Data: `{"schema":1,"generation":"` + s.generation + `","reward_note":"x"}`, Timestamp: s.now().Unix()}), "invalid_work_data")
}

func TestWorkRewardNoteRoundTrip(t *testing.T) {
	s := openTest(t, Config{})
	owner := keyFor(242)
	id := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: "Brief", Timestamp: s.now().Unix()})).Receipt.ID
	run(t, s, noteCreate(s, owner, id, map[string]any{"reward_note": testRewardNote}))
	plain := createTestWork(t, s, owner, "lobby", "request", 0)

	w := getTestWork(t, s, id)
	if w.RewardNote != testRewardNote || w.Reward != nil {
		t.Fatalf("work.get reward_note %q reward %+v", w.RewardNote, w.Reward)
	}
	if got := getTestWork(t, s, plain); got.RewardNote != "" {
		t.Fatalf("work without a note reads %q", got.RewardNote)
	}
	// The JSON field is reward_note, and absent when there is none.
	b, _ := json.Marshal(w)
	if !strings.Contains(string(b), `"reward_note":"+0.10 USDC on Base, paid by the poster"`) {
		t.Fatalf("work JSON: %s", b)
	}
	b, _ = json.Marshal(getTestWork(t, s, plain))
	if strings.Contains(string(b), "reward_note") {
		t.Fatalf("work JSON without a note: %s", b)
	}
	notes := map[string]string{}
	for _, item := range run(t, s, Command{Operation: "works.list"}).Data["works"].([]Work) {
		notes[item.ID] = item.RewardNote
	}
	if notes[id] != testRewardNote || notes[plain] != "" {
		t.Fatalf("works.list notes %q", notes)
	}
	// It is kept in the signed create command, not a column: history shows it.
	history := run(t, s, Command{Operation: "work.history", MessageID: id}).Data["transitions"].([]WorkTransition)
	if len(history) != 1 || !strings.Contains(history[0].SignedPayload, `reward_note`) {
		t.Fatalf("history %+v", history)
	}
	// The message's work mark carries it.
	marks := workMarks(run(t, s, Command{Operation: "thread.get", MessageID: id}).Messages)
	if marks[id] == nil || marks[id].RewardNote != testRewardNote || marks[id].Reward != nil {
		t.Fatalf("work mark %+v", marks[id])
	}
	marks = workMarks(run(t, s, Command{Operation: "messages.list", Room: "lobby"}).Messages)
	if marks[plain] == nil || marks[plain].RewardNote != "" {
		t.Fatalf("plain work mark %+v", marks[plain])
	}
}

// With a credit reward the note rides alongside it: the earn list shows
// both, and the escrow holds the credits only.
func TestWorkRewardNoteWithCreditsOnEarnList(t *testing.T) {
	s := rewardStore(t)
	requester := keyFor(243)
	mintCredit(t, s, keyID(requester), allowance.Paid, 5000)
	id := rewardRequest(t, s, requester, "lobby")
	run(t, s, noteCreate(s, requester, id, map[string]any{"reward": 20, "reward_note": testRewardNote, "capabilities": []string{WorkEarnTag}}))
	items := run(t, s, Command{Operation: "works.list", Kind: WorkKindEarn}).Data["works"].([]Work)
	if len(items) != 1 || items[0].RewardNote != testRewardNote || items[0].Reward == nil || items[0].Reward.Amount != 20 {
		t.Fatalf("earn list %+v", items)
	}
	marks := workMarks(run(t, s, Command{Operation: "message.get", MessageID: id}).Messages)
	if m := marks[id]; m == nil || m.RewardNote != testRewardNote || m.Reward == nil || m.Reward.Amount != 20 {
		t.Fatalf("work mark %+v", m)
	}
}
