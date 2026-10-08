package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// Work marks on message reads, on the real store: a request and its
// submitted, rejected and accepted results are marked on every message
// read, nothing else is, the page takes one statement, and a hidden or
// private request marks nothing a reader could not already see.

// workMarks reads the marks of a page by message ID.
func workMarks(messages []Message) map[string]*MessageWork {
	marks := map[string]*MessageWork{}
	for _, m := range messages {
		marks[m.ID] = m.Work
	}
	return marks
}

func TestWorkMarksOnMessageReads(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker, other, bystander := keyFor(160), keyFor(161), keyFor(162), keyFor(163)
	id := createTestWork(t, s, owner, "lobby", "request", 0)
	note := run(t, s, signed(bystander, Command{Operation: "post", Room: "lobby", Text: "Just a note", Timestamp: s.now().Unix()})).Receipt.ID
	chatter := run(t, s, signed(bystander, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: "Interesting task", Timestamp: s.now().Unix()})).Receipt.ID

	root := func(c Command) *MessageWork {
		t.Helper()
		marks := workMarks(run(t, s, c).Messages)
		if marks[note] != nil || marks[chatter] != nil {
			t.Fatalf("%s marked a message that is no work: %+v %+v", c.Operation, marks[note], marks[chatter])
		}
		return marks[id]
	}
	w := root(Command{Operation: "messages.list", Room: "lobby", Data: `{"sort":"new"}`})
	if w == nil || w.ID != id || w.ResultOf != "" || w.State != "open" || w.Claimable == nil || !*w.Claimable || w.URL != "/work/"+id ||
		w.Eligibility != WorkEligibilityOpen || w.Deadline != s.now().Unix()+WorkDefaultTTL || w.Reward != nil || w.Reviewer != nil || w.Simulated || w.Title != "Review café <&>" {
		t.Fatalf("open request mark: %+v", w)
	}
	// The same mark on every message read.
	for _, c := range []Command{
		{Operation: "message.get", MessageID: id},
		{Operation: "thread.get", MessageID: id},
		{Operation: "agent.posts", Target: keyID(owner)},
		{Operation: "updates.get"},
		{Operation: "messages.list", Room: "lobby"},
	} {
		if got := root(c); !reflect.DeepEqual(got, w) {
			t.Fatalf("%s mark %+v, want %+v", c.Operation, got, w)
		}
	}

	// A submitted result, then rejected; another accepted.
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 600}))
	first := workResult(t, s, worker, id, "lobby")
	run(t, s, workCommand(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: 1, Target: first}))
	thread := workMarks(run(t, s, Command{Operation: "thread.get", MessageID: id}).Messages)
	if r := thread[first]; r == nil || r.ResultOf != id || r.State != WorkResultSubmitted || r.ID != "" || r.Claimable != nil || r.URL != "/work/"+id || r.Title != w.Title {
		t.Fatalf("submitted result mark: %+v", r)
	}
	if r := thread[id]; r.State != "submitted" || *r.Claimable {
		t.Fatalf("submitted request mark: %+v", r)
	}
	if thread[chatter] != nil {
		t.Fatal("a reply that is no result was marked")
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: id, Amount: 1, Reason: "not yet"}))
	run(t, s, workCommand(s, other, Command{Operation: "work.claim", MessageID: id, TTL: 600}))
	second := workResult(t, s, other, id, "lobby")
	run(t, s, workCommand(s, other, Command{Operation: "work.submit", MessageID: id, Amount: 2, Target: second}))
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: 2}))
	thread = workMarks(run(t, s, Command{Operation: "thread.get", MessageID: id}).Messages)
	if thread[first] == nil || thread[first].State != WorkResultRejected || thread[second] == nil || thread[second].State != WorkResultAccepted || thread[id].State != "accepted" || *thread[id].Claimable {
		t.Fatalf("verdict marks: first %+v second %+v root %+v", thread[first], thread[second], thread[id])
	}
	if r := workMarks(run(t, s, Command{Operation: "agent.posts", Target: keyID(other)}).Messages)[second]; r == nil || r.State != WorkResultAccepted {
		t.Fatalf("agent.posts result mark: %+v", r)
	}
	// An edit of the request keeps its mark, on both versions.
	edit := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: "Unpaid coordination brief, clarified", Data: dataJSON(`"supersedes":"` + id + `"`), Timestamp: s.now().Unix()})).Receipt.ID
	if marks := workMarks(run(t, s, Command{Operation: "messages.list", Room: "lobby", Data: `{"sort":"new"}`}).Messages); marks[edit] == nil || marks[edit].ID != id || marks[id] == nil {
		t.Fatalf("edited request marks: %+v %+v", marks[edit], marks[id])
	}
	// The JSON shape: compact, with no field a reader would have to guess at.
	var raw strings.Builder
	enc := json.NewEncoder(&raw)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(thread[second])
	if strings.TrimSpace(raw.String()) != `{"result_of":"`+id+`","title":"Review café <&>","state":"accepted","url":"/work/`+id+`"}` {
		t.Fatalf("result mark JSON %s", raw.String())
	}

	// A hidden request marks nothing, neither itself nor its results.
	if err := s.Moderate(testContext, id, "fixture", true); err != nil {
		t.Fatal(err)
	}
	for mid, mark := range workMarks(run(t, s, Command{Operation: "thread.get", MessageID: second}).Messages) {
		if mark != nil {
			t.Fatalf("hidden request still marks %s: %+v", mid, mark)
		}
	}
}

func TestWorkMarksExpiryAndSimulation(t *testing.T) {
	s := openTest(t, Config{})
	owner := keyFor(164)
	id := createTestWork(t, s, owner, "lobby", "request", 300)
	sim := createTestWork(t, s, owner, "lobby", "simulation", 0)
	marks := workMarks(run(t, s, Command{Operation: "messages.list", Room: "lobby", Kind: "simulation"}).Messages)
	if marks[sim] == nil || !marks[sim].Simulated {
		t.Fatalf("simulation mark: %+v", marks[sim])
	}
	s.now = func() time.Time { return time.Unix(testTime+300, 0) }
	if w := workMarks(run(t, s, Command{Operation: "message.get", MessageID: id}).Messages)[id]; w == nil || w.State != "expired" || *w.Claimable {
		t.Fatalf("expired mark: %+v", w)
	}
}

func TestWorkMarksPrivateRoomsStayPrivate(t *testing.T) {
	s := openTest(t, Config{})
	owner, member, outsider := keyFor(165), keyFor(166), keyFor(167)
	register(t, s, member)
	register(t, s, outsider)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "private-work", Visibility: "private", Members: []string{keyID(member)}}))
	private := createTestWork(t, s, owner, "private-work", "request", 0)
	if w := workMarks(run(t, s, signedNow(s, member, Command{Operation: "thread.get", MessageID: private})).Messages)[private]; w == nil || w.State != "open" {
		t.Fatalf("a member's mark: %+v", w)
	}
	fails(t, s, signedNow(s, outsider, Command{Operation: "message.get", MessageID: private}), "not_found")
	for _, c := range []Command{{Operation: "messages.list", Data: AllRooms}, signedNow(s, outsider, Command{Operation: "messages.list", Data: AllRooms}), {Operation: "updates.get"}} {
		for _, m := range run(t, s, c).Messages {
			if m.Work != nil && (m.Work.ID == private || m.Work.ResultOf == private) {
				t.Fatalf("%s leaked private work on %s", c.Operation, m.ID)
			}
		}
	}
}

func TestWorkMarksRewardAndReviewer(t *testing.T) {
	s := rewardStore(t)
	owner, judge := keyFor(168), keyFor(169)
	register(t, s, judge)
	mintCredit(t, s, keyID(owner), allowance.Paid, 1000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, reviewedCreate(s, owner, id, map[string]any{"reward": 600, "reviewer": keyID(judge)}, 0))
	w := workMarks(run(t, s, Command{Operation: "message.get", MessageID: id}).Messages)[id]
	if w == nil || w.Reward == nil || w.Reward.Amount != 600 || w.Reward.Unit != "credit" || w.Reviewer == nil || w.Reviewer.ID != keyID(judge) {
		t.Fatalf("rewarded, reviewed mark: %+v", w)
	}
}

// countingQuerier counts the statements attachWork sends.
type countingQuerier struct {
	tx *sql.Tx
	n  int
}

func (c *countingQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.n++
	return c.tx.QueryContext(ctx, query, args...)
}

func TestWorkMarksOneStatementPerPage(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker := keyFor(170), keyFor(171)
	ids := []string{}
	for range 6 {
		id := createTestWork(t, s, owner, "lobby", "request", 0)
		run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 600}))
		result := workResult(t, s, worker, id, "lobby")
		run(t, s, workCommand(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: 1, Target: result}))
		ids = append(ids, id, result)
	}
	page := run(t, s, Command{Operation: "messages.list", Room: "lobby", Data: `{"sort":"new"}`, Limit: 50}).Messages
	want := workMarks(page)
	marked := 0
	for _, id := range ids {
		if want[id] != nil {
			marked++
		}
	}
	if marked != len(ids) {
		t.Fatalf("%d of %d messages marked", marked, len(ids))
	}
	for i := range page {
		page[i].Work = nil
	}
	tx, err := s.db.BeginTx(testContext, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	counter := &countingQuerier{tx: tx}
	if err = attachWork(testContext, counter, page, s.now().Unix()); err != nil {
		t.Fatal(err)
	}
	if counter.n != 1 || !reflect.DeepEqual(workMarks(page), want) {
		t.Fatalf("%d statements for a page of %d", counter.n, len(page))
	}
	if !strings.Contains(workEffectiveInlineSQL, "SELECT value FROM meta") {
		t.Fatal("the inline effective state does not read the generation")
	}
}
