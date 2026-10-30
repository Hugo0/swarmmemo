package board

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// C618: a signed reply to open work that its author never claimed with
// stays on the author's home as unclaimed_result, with the claim that
// submits it, until it is claimed, the work closes, or 7 days pass.
func TestHomeUnclaimedResult(t *testing.T) {
	s := rewardStore(t)
	owner, worker, hidden, edited, closer, late := keyFor(250), keyFor(251), keyFor(252), keyFor(253), keyFor(254), keyFor(255)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	reply := func(key ed25519.PrivateKey, id, data string) string {
		t.Helper()
		return run(t, s, signed(key, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: "My result " + randomID()[:8], Data: data, Timestamp: s.now().Unix()})).Receipt.ID
	}
	unclaimed := func(key ed25519.PrivateKey) []HomeItem {
		t.Helper()
		var out []HomeItem
		for _, it := range run(t, s, signed(key, Command{Operation: "home", Timestamp: s.now().Unix()})).Data["items"].([]HomeItem) {
			if it.Kind == "unclaimed_result" {
				out = append(out, it)
			}
		}
		return out
	}

	standing := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, standing, map[string]any{"seats": 3, "reward": 300}, 10*86400))

	// A signed reply to an open standing task: listed first, with the claim
	// that submits it on every transport.
	r := reply(worker, standing, "")
	got := run(t, s, signed(worker, Command{Operation: "home", Timestamp: s.now().Unix()})).Data
	items := got["items"].([]HomeItem)
	if len(items) == 0 || items[0].Kind != "unclaimed_result" || items[0].Count != 1 || got["nothing_needs_you"] != false {
		t.Fatalf("worker home: %+v", got)
	}
	it := items[0]
	if it.Call["operation"] != "work.claim" || it.Call["message_id"] != standing || it.Call["target"] != r || !strings.Contains(it.Call["data"].(string), `"generation"`) ||
		it.Tool.Name != "claim_work" || it.Tool.Arguments["message_id"] != standing || it.Tool.Arguments["result_id"] != r {
		t.Fatalf("unclaimed_result call: %+v", it)
	}
	if !strings.Contains(it.Text, "not submitted yet") {
		t.Fatalf("unclaimed_result text: %q", it.Text)
	}
	if _, work, own, ok := s.WaitingFor(testContext, signed(worker, Command{Operation: "messages.list"})); !ok || !own || work != 1 {
		t.Fatalf("waiting work for the worker: %d", work)
	}
	// Read publicly, it is not there.
	for _, pit := range run(t, s, Command{Operation: "home", Target: keyID(worker)}).Data["items"].([]HomeItem) {
		if pit.Kind == "unclaimed_result" {
			t.Fatal("a public home lists unclaimed_result")
		}
	}

	// The requester's own reply is never one.
	reply(owner, standing, "")
	if u := unclaimed(owner); len(u) != 0 {
		t.Fatalf("the requester is told to claim its own task: %+v", u)
	}

	// An anonymous reply is never one.
	anon := run(t, s, Command{Operation: "post", Room: "lobby", ReplyTo: standing, Text: "anonymous result"}).Receipt.ID
	var anonAccount string
	if err := s.db.QueryRow("SELECT account FROM events WHERE id=?", anon).Scan(&anonAccount); err != nil {
		t.Fatal(err)
	}
	if err := s.publicRead(testContext, func(ctx context.Context, tx *sql.Tx) error {
		u, err := s.unclaimedResults(ctx, tx, anonAccount, s.now().Unix(), s.generation)
		if err == nil && len(u) != 0 {
			t.Errorf("an anonymous reply is listed: %+v", u)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// A hidden reply is not.
	h := reply(hidden, standing, "")
	if _, err := s.db.Exec("UPDATE events SET hidden=1 WHERE id=?", h); err != nil {
		t.Fatal(err)
	}
	if u := unclaimed(hidden); len(u) != 0 {
		t.Fatalf("a hidden reply is listed: %+v", u)
	}

	// An edited reply is listed by its newest version, never the old one.
	first := reply(edited, standing, "")
	second := reply(edited, standing, `{"schema":1,"supersedes":"`+first+`"}`)
	if u := unclaimed(edited); len(u) != 1 || u[0].Call["target"] != second {
		t.Fatalf("an edited reply: %+v (first %s second %s)", u, first, second)
	}

	// Claimed with the reply: gone, and a further reply says nothing while
	// that result waits.
	run(t, s, workWith(s, worker, Command{Operation: "work.claim", MessageID: standing, Target: r}, nil))
	if u := unclaimed(worker); len(u) != 0 {
		t.Fatalf("listed after the claim: %+v", u)
	}
	reply(worker, standing, "")
	if u := unclaimed(worker); len(u) != 0 {
		t.Fatalf("listed while a result waits: %+v", u)
	}

	// Closed work: listed while open, gone once cancelled.
	plain := createTestWork(t, s, owner, "lobby", "request", 0)
	reply(closer, plain, "")
	if u := unclaimed(closer); len(u) != 1 || u[0].Call["message_id"] != plain {
		t.Fatalf("a reply to open work: %+v", u)
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: plain, Reason: "no longer needed"}))
	if u := unclaimed(closer); len(u) != 0 {
		t.Fatalf("listed after the work closed: %+v", u)
	}

	// Older than 7 days: gone, though the task is still open.
	reply(late, standing, "")
	if u := unclaimed(late); len(u) != 1 {
		t.Fatalf("a fresh reply: %+v", u)
	}
	at := s.now().Add((UnclaimedResultDays + 1) * 24 * time.Hour)
	s.now = func() time.Time { return at }
	if u := unclaimed(late); len(u) != 0 {
		t.Fatalf("listed after %d days: %+v", UnclaimedResultDays, u)
	}
}

// C641: a standing task whose per-agent-day seat the worker already used
// takes no claim from it today, so home offers none: no unclaimed_result,
// its count agrees, and the receipt carries no claim hint either.
func TestHomeUnclaimedResultSeatUsed(t *testing.T) {
	s := rewardStore(t)
	owner, worker := keyFor(240), keyFor(241)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	standing := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, standing, map[string]any{"seats": 3, "reward": 300, "per_agent_day": 1}, 10*86400))
	reply := func() Result {
		t.Helper()
		return run(t, s, signed(worker, Command{Operation: "post", Room: "lobby", ReplyTo: standing, Text: "My result " + randomID()[:8], Timestamp: s.now().Unix()}))
	}
	r := reply().Receipt.ID
	run(t, s, workWith(s, worker, Command{Operation: "work.claim", MessageID: standing, Target: r}, nil))
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r, Amount: 1}))

	second := reply()
	if second.Receipt.ReplyWork != "" {
		t.Fatalf("receipt offers a claim with the seat used: %q", second.Receipt.ReplyWork)
	}
	got := run(t, s, signed(worker, Command{Operation: "home", Timestamp: s.now().Unix()})).Data
	for _, it := range got["items"].([]HomeItem) {
		if it.Kind == "unclaimed_result" {
			t.Fatalf("home offers a claim work.claim refuses: %+v", it)
		}
	}
	if _, work, _, ok := s.WaitingFor(testContext, signed(worker, Command{Operation: "messages.list"})); !ok || work != 0 {
		t.Fatalf("waiting work counts the unclaimable reply: %d", work)
	}
	// work.get says the same, and the claim is indeed refused.
	read := run(t, s, signed(worker, Command{Operation: "work.get", MessageID: standing})).Data["work"].(Work)
	if read.Eligible == nil || *read.Eligible {
		t.Fatalf("work.get eligible: %+v", read.Eligible)
	}
	fails(t, s, workWith(s, worker, Command{Operation: "work.claim", MessageID: standing, Target: second.Receipt.ID}, nil), "seat_limit")
}
