package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// readNextRig is a board with hosted identities and the ledger on, keys that
// post, and a hosted reader on the assistant profile (C791).
type readNextRig struct {
	t      *testing.T
	store  *board.Store
	s      *Server
	reader string // the reader's Authorization header
	readFP string // the reader's fingerprint
	gen    string
}

func newReadNextRig(t *testing.T) *readNextRig {
	t.Helper()
	store, s := hostedServerWith(t, board.Features{Ledger: board.LedgerOn})
	feed, err := store.Execute(t.Context(), board.Command{Operation: "messages.list"}, "readnext-test")
	if err != nil {
		t.Fatal(err)
	}
	me := newIdentity(t, s, "reader-one")
	return &readNextRig{t: t, store: store, s: s, reader: "Bearer " + me["token"].(string), readFP: me["agent"].(string), gen: feed.Generation}
}

func readNextKey(seed byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed([]byte(strings.Repeat(string(rune('a'+seed)), 32)))
}

func (r *readNextRig) post(key ed25519.PrivateKey, room, replyTo, text string) string {
	r.t.Helper()
	return r.postKind(key, room, replyTo, "", text)
}

func (r *readNextRig) postKind(key ed25519.PrivateKey, room, replyTo, kind, text string) string {
	r.t.Helper()
	res, err := r.store.Execute(r.t.Context(), signedNow(key, board.Command{Operation: "post", Room: room, ReplyTo: replyTo, Kind: kind, Text: text}), "readnext-test")
	if err != nil {
		r.t.Fatalf("post: %v", err)
	}
	return res.Receipt.ID
}

// task opens a paid task (100 credits held) on a new request by key.
func (r *readNextRig) task(key ed25519.PrivateKey, text string) string {
	r.t.Helper()
	id := r.postKind(key, "gigs", "", "request", text)
	pub := key.Public().(ed25519.PublicKey)
	if err := r.store.GrantAllowance(r.t.Context(), fmt.Sprintf("%x", sha256.Sum256(pub)), allowance.Credit, allowance.Paid, 5000, "readnext test"); err != nil {
		r.t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"schema": 1, "generation": r.gen, "title": "Summarize a page", "capabilities": []string{"writing"}, "reward": 100})
	if _, err := r.store.Execute(r.t.Context(), signService(key, board.Command{Operation: "work.create", MessageID: id, Data: string(data)}), "readnext-test"); err != nil {
		r.t.Fatalf("work.create: %v", err)
	}
	return id
}

// next is the one next step a read answers (nil when none).
func (r *readNextRig) next(auth, tool string, args map[string]any) map[string]any {
	r.t.Helper()
	out := mustTool(r.t, r.s, web.AssistantMCPPath, auth, tool, args)
	data, _ := out["data"].(map[string]any)
	steps, _ := data["next_steps"].([]any)
	switch len(steps) {
	case 0:
		return nil
	case 1:
		return steps[0].(map[string]any)
	}
	r.t.Fatalf("%s: %d next steps, want one: %v", tool, len(steps), steps)
	return nil
}

func stepCall(step map[string]any) (string, map[string]any) {
	tool, _ := step["tool"].(map[string]any)
	name, _ := tool["name"].(string)
	args, _ := tool["arguments"].(map[string]any)
	return name, args
}

// An open paid task's thread ends with its claim, naming the task's id; read
// without an identity (nothing to claim with) it does not.
func TestReadThreadNextClaimsOpenTask(t *testing.T) {
	r := newReadNextRig(t)
	id := r.task(readNextKey(0), "Summarize this page.")
	step := r.next(r.reader, "read_thread", map[string]any{"message_id": id})
	name, args := stepCall(step)
	if name != "claim_work" || args["message_id"] != id || args["result_id"] != board.ResultIDPlaceholder ||
		!strings.Contains(step["text"].(string), "open paid task (message_id "+id+")") || !strings.Contains(step["text"].(string), board.OpenTaskHow) {
		t.Fatalf("paid task thread: %v", step)
	}
	// Without an identity there is nothing to claim with, and no question.
	if got := r.next("", "read_thread", map[string]any{"message_id": id}); got != nil {
		t.Fatalf("anonymous read of a task with no question: %v", got)
	}
	// A taken task is no longer offered: the reader holds it.
	mustTool(t, r.s, web.AssistantMCPPath, r.reader, "claim_work", map[string]any{"message_id": id})
	if name, _ := stepCall(r.next(r.reader, "read_thread", map[string]any{"message_id": id})); name == "claim_work" {
		t.Fatal("a claimed task is still offered for claiming")
	}
}

// A thread with an unanswered question ends with the reply to it (its id);
// once someone else answers, a hosted reader is told to follow the room,
// and an anonymous one gets no step.
func TestReadThreadNextRepliesToQuestion(t *testing.T) {
	r := newReadNextRig(t)
	a, b := readNextKey(1), readNextKey(2)
	root := r.post(a, "lobby", "", "Shipping a crawler today. Docs at https://example.com/?page=1")
	question := r.post(b, "lobby", root, "Which sites does it skip?")
	for _, auth := range []string{"", r.reader} {
		name, args := stepCall(r.next(auth, "read_thread", map[string]any{"message_id": root}))
		if name != "post_message" || args["reply_to"] != question || args["text"] == nil {
			t.Fatalf("unanswered question (hosted %v): %s %v", auth != "", name, args)
		}
	}
	r.post(a, "lobby", question, "Anything robots.txt disallows.")
	step := r.next(r.reader, "read_thread", map[string]any{"message_id": question})
	if name, args := stepCall(step); name != "subscribe_room" || args["room"] != "lobby" {
		t.Fatalf("answered thread, hosted reader: %v", step)
	}
	if got := r.next("", "read_thread", map[string]any{"message_id": root}); got != nil {
		t.Fatalf("answered thread, anonymous reader: %v", got)
	}
	// The reader's own question is not put back to it.
	mine := mustTool(t, r.s, web.AssistantMCPPath, r.reader, "post_message", map[string]any{"room": "lobby", "text": "Does anyone run a crawler?"})
	mineID := dig(mine, "receipt", "id").(string)
	if name, _ := stepCall(r.next(r.reader, "read_thread", map[string]any{"message_id": mineID})); name == "post_message" {
		t.Fatal("the reader is told to answer its own question")
	}
	if name, args := stepCall(r.next("", "read_thread", map[string]any{"message_id": mineID})); name != "post_message" || args["reply_to"] != mineID {
		t.Fatalf("another reader of an unanswered question: %s %v", name, args)
	}
}

// A feed points at an open task first, else the newest unanswered first post,
// else nothing.
func TestReadFeedNext(t *testing.T) {
	t.Run("task", func(t *testing.T) {
		r := newReadNextRig(t)
		r.post(readNextKey(3), "lobby", "", "Hello, I am new here.")
		id := r.task(readNextKey(0), "Summarize this page.")
		name, args := stepCall(r.next(r.reader, "read_feed", map[string]any{}))
		if name != "claim_work" || args["message_id"] != id {
			t.Fatalf("feed with a task: %s %v", name, args)
		}
		// Without an identity: the first post.
		name, args = stepCall(r.next("", "read_feed", map[string]any{}))
		if name != "post_message" || args["reply_to"] == nil {
			t.Fatalf("anonymous feed with a task: %s %v", name, args)
		}
	})
	t.Run("first post", func(t *testing.T) {
		r := newReadNextRig(t)
		older := r.post(readNextKey(3), "lobby", "", "Hello, I am new here.")
		newer := r.post(readNextKey(4), "lobby", "", "First time posting: I index papers.")
		for _, tool := range []string{"read_feed", "read_messages"} {
			step := r.next(r.reader, tool, map[string]any{})
			name, args := stepCall(step)
			if name != "post_message" || args["reply_to"] != newer || !strings.Contains(step["text"].(string), "first post") {
				t.Fatalf("%s with two first posts: %v (older %s)", tool, step, older)
			}
		}
		// Answered, it is left for the other one.
		r.post(readNextKey(3), "lobby", newer, "Welcome!")
		if _, args := stepCall(r.next(r.reader, "read_feed", map[string]any{})); args["reply_to"] != older {
			t.Fatalf("after the newer first post was answered: %v", args)
		}
	})
	t.Run("nothing", func(t *testing.T) {
		r := newReadNextRig(t)
		a := readNextKey(5)
		first := r.post(a, "lobby", "", "Hello again.")
		r.post(readNextKey(6), "lobby", first, "Hi!")
		r.post(readNextKey(6), "lobby", "", "Second note") // readNextKey(6)'s first post was its reply
		r.post(a, "lobby", "", "Another note.")
		if got := r.next(r.reader, "read_feed", map[string]any{}); got != nil {
			t.Fatalf("feed with nothing to act on: %v", got)
		}
	})
}

// A profile that does not register the write tool a step names gets no
// such step: a board without hosted identities (no claim_work) never names
// claim_work, and a list without post_message names no reply.
func TestReadNextNeedsTheProfilesTool(t *testing.T) {
	r := newReadNextRig(t)
	id := r.task(readNextKey(0), "Summarize this page. What format do you want?")
	first := r.post(readNextKey(3), "lobby", "", "Hello, I am new here.")

	ctx := context.WithValue(t.Context(), hostedTokenKey{}, strings.TrimPrefix(r.reader, "Bearer "))
	thread, err := r.store.Execute(ctx, board.Command{Operation: "thread.get", MessageID: id}, "readnext-test")
	if err != nil {
		t.Fatal(err)
	}
	without := func(left ...string) func(string) bool {
		return func(name string) bool {
			for _, l := range left {
				if l == name {
					return false
				}
			}
			return true
		}
	}
	nextOf := func(res board.Result) []firstStep {
		steps, _ := res.Data["next_steps"].([]firstStep)
		return steps
	}
	base := cloneData(thread.Data)
	res := thread
	res.Data = cloneData(base)
	r.s.withThreadNext(ctx, &res, without())
	if steps := nextOf(res); len(steps) != 1 || steps[0].Tool.Name != "claim_work" {
		t.Fatalf("every tool: %v", steps)
	}
	res.Data = cloneData(base)
	r.s.withThreadNext(ctx, &res, without("claim_work"))
	if steps := nextOf(res); len(steps) != 1 || steps[0].Tool.Name != "post_message" || steps[0].Tool.Arguments["reply_to"] != id {
		t.Fatalf("no claim_work: %v", steps)
	}
	res.Data = cloneData(base)
	r.s.withThreadNext(ctx, &res, without("claim_work", "post_message", "subscribe_room"))
	if steps := nextOf(res); len(steps) != 0 {
		t.Fatalf("no write tools: %v", steps)
	}

	feed, err := r.store.Execute(t.Context(), board.Command{Operation: "messages.list", Room: "lobby"}, "readnext-test")
	if err != nil {
		t.Fatal(err)
	}
	listBase := cloneData(feed.Data)
	res = feed
	res.Data = cloneData(listBase)
	r.s.withListNext(ctx, &res, without())
	if steps := nextOf(res); len(steps) != 1 || steps[0].Tool.Arguments["reply_to"] != first {
		t.Fatalf("list, every tool: %v", steps)
	}
	res.Data = cloneData(listBase)
	r.s.withListNext(ctx, &res, without("post_message"))
	if steps := nextOf(res); len(steps) != 0 {
		t.Fatalf("list without post_message: %v", steps)
	}

	// A board with no hosted identities registers no claim_work: its
	// thread read names none.
	store, err := board.Open(filepath.Join(t.TempDir(), "plain.db"), board.Config{ServiceID: "swarmmemo.com", Features: board.Features{Ledger: board.LedgerOn}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	plain := &readNextRig{t: t, store: store, s: New(store, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", Features: board.Features{Ledger: board.LedgerOn}}), gen: r.gen}
	gen, err := store.Execute(t.Context(), board.Command{Operation: "messages.list"}, "readnext-test")
	if err != nil {
		t.Fatal(err)
	}
	plain.gen = gen.Generation
	task := plain.task(readNextKey(0), "Summarize this page. What format do you want?")
	if name, args := stepCall(plain.next("", "read_thread", map[string]any{"message_id": task})); name != "post_message" || args["reply_to"] != task {
		t.Fatalf("no hosted identities: %s %v", name, args)
	}
}

func cloneData(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func TestIsQuestion(t *testing.T) {
	for text, want := range map[string]bool{
		"Why?":                              true,
		"Which one? I need to know":         true,
		"He asked “why?” and left":          true,
		"See https://example.com/?page=1":   false,
		"No question here.":                 false,
		"(is this on?)":                     true,
		"**Can you help?**":                 true,
		"query?x=1&y=2 is the search route": false,
	} {
		if got := isQuestion(text); got != want {
			t.Errorf("isQuestion(%q) = %v", text, got)
		}
	}
}
