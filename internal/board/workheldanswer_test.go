package board

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A held answer (C734) on the real store: a work.reveal before close is
// checked against answer_commit and kept by the board, outside every signed
// record. work.get shows it to the requester and the named reviewer; the
// worker, any other signer and an anonymous reader see held and the commit;
// no other read and no other table carries the text.

const heldSecret = "expected: rows=6 sum=PRIVATE-c734-91f2"

func TestWorkHeldAnswerReaders(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker, judge, other := keyFor(230), keyFor(231), keyFor(232), keyFor(233)
	register(t, s, judge)
	commit := WorkAnswerCommit(testSalt, heldSecret)
	id := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: "Derive the six fields independently", Timestamp: s.now().Unix()})).Receipt.ID
	run(t, s, noteCreate(s, owner, id, map[string]any{"answer_commit": commit, "reviewer": keyID(judge)}))

	// Only the requester holds it, and only the committed values.
	fails(t, s, revealCommand(s, worker, id, heldSecret, testSalt), "work_forbidden")
	fails(t, s, revealCommand(s, judge, id, heldSecret, testSalt), "work_forbidden")
	fails(t, s, revealCommand(s, owner, id, heldSecret+" ", testSalt), "reveal_mismatch")
	history := len(run(t, s, Command{Operation: "work.history", MessageID: id}).Data["transitions"].([]WorkTransition))
	ack := run(t, s, revealCommand(s, owner, id, heldSecret, testSalt)).Data["ack"].(WorkAck)
	if !strings.HasPrefix(ack.Note, "Held, not published") || strings.Contains(ack.Note, heldSecret) {
		t.Fatalf("hold ack %+v", ack)
	}
	if n := len(run(t, s, Command{Operation: "work.history", MessageID: id}).Data["transitions"].([]WorkTransition)); n != history {
		t.Fatalf("a hold added a transition: %d to %d", history, n)
	}
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 600}))
	result := workResult(t, s, worker, id, "lobby")
	run(t, s, workCommand(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: 1, Target: result}))

	read := func(c Command) Work {
		t.Helper()
		return run(t, s, c).Data["work"].(Work)
	}
	for name, w := range map[string]Work{
		"requester": read(signed(owner, Command{Operation: "work.get", MessageID: id})),
		"reviewer":  read(signed(judge, Command{Operation: "work.get", MessageID: id})),
	} {
		k := w.KnownAnswer
		if k == nil || !k.Held || k.State != WorkAnswerSealed || k.Answer != heldSecret || k.Salt != testSalt || k.Verified {
			t.Fatalf("%s reads %+v", name, k)
		}
	}
	for name, c := range map[string]Command{
		"anonymous": {Operation: "work.get", MessageID: id},
		"worker":    signed(worker, Command{Operation: "work.get", MessageID: id}),
		"other":     signed(other, Command{Operation: "work.get", MessageID: id}),
	} {
		k := read(c).KnownAnswer
		if k == nil || !k.Held || k.Commit != commit || k.Answer != "" || k.Salt != "" {
			t.Fatalf("%s reads %+v", name, k)
		}
	}

	// No other read carries the text: lists, history, threads, messages,
	// search, the worker's updates.
	for name, c := range map[string]Command{
		"works.list":    {Operation: "works.list"},
		"signed list":   signed(owner, Command{Operation: "works.list"}),
		"work.history":  {Operation: "work.history", MessageID: id},
		"thread":        {Operation: "thread.get", MessageID: id},
		"messages":      {Operation: "messages.list", Room: "lobby"},
		"search":        {Operation: "messages.list", Room: "lobby", Query: "PRIVATE"},
		"worker update": signed(worker, Command{Operation: "updates.get"}),
		"worker work":   signed(worker, Command{Operation: "work.get", MessageID: id}),
	} {
		b, _ := json.Marshal(run(t, s, c))
		if strings.Contains(string(b), "PRIVATE-c734") || strings.Contains(string(b), testSalt) {
			t.Fatalf("%s carries the held answer: %s", name, b)
		}
	}

	// Stored once, in meta: no other table (transitions, events, exports,
	// the search index, the transparency log, requests) holds it.
	if where := tablesHolding(t, s, "PRIVATE-c734"); len(where) != 1 || where[0] != "meta" {
		t.Fatalf("the held answer is stored in %v", where)
	}

	// After the verdict the requester publishes it, verified against the
	// commit everyone saw: anyone reads it then.
	run(t, s, workCommand(s, judge, Command{Operation: "work.accept", MessageID: id, Amount: 1}))
	s.now = func() time.Time { return time.Unix(testTime+60, 0) }
	run(t, s, revealCommand(s, owner, id, heldSecret, testSalt))
	if k := read(Command{Operation: "work.get", MessageID: id}).KnownAnswer; k.State != WorkAnswerRevealed || k.Answer != heldSecret || !k.Verified || !k.Held {
		t.Fatalf("revealed %+v", k)
	}
}

// tablesHolding names every table with a text or blob cell containing want.
func tablesHolding(t *testing.T, s *Store, want string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND sql NOT LIKE 'CREATE VIRTUAL%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	var found []string
	for _, table := range tables {
		r, err := s.db.Query(fmt.Sprintf(`SELECT * FROM "%s"`, table))
		if err != nil {
			continue // a shadow table of a virtual table
		}
		cols, _ := r.Columns()
		hit := false
		for r.Next() && !hit {
			cells := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range cells {
				ptrs[i] = &cells[i]
			}
			if err = r.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for _, c := range cells {
				switch v := c.(type) {
				case string:
					hit = hit || strings.Contains(v, want)
				case []byte:
					hit = hit || strings.Contains(string(v), want)
				}
			}
		}
		r.Close()
		if hit {
			found = append(found, table)
		}
	}
	return found
}
