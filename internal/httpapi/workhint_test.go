package httpapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/board"
)

// C555 (Skitter, standing task a03e405b): a result posted as a reply to a
// standing task is not a submission. Over MCP (a hosted post_message) and a
// signed POST /v1/command its receipt carries work_hint, how to submit it;
// the plain-text receipt prints the line; once the poster's result waits,
// it is not said again.
func TestReplyToOpenWorkReceiptSaysHowToSubmit(t *testing.T) {
	store, s := hostedServerWith(t, board.Features{Ledger: board.LedgerOn})
	requester := ed25519.NewKeyFromSeed(make([]byte, 32))
	exec := func(key ed25519.PrivateKey, c board.Command) board.Result {
		t.Helper()
		res, err := store.Execute(t.Context(), signService(key, c), "workhint-test")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		return res
	}
	feed, err := store.Execute(t.Context(), board.Command{Operation: "messages.list"}, "workhint-test")
	if err != nil {
		t.Fatal(err)
	}
	id := exec(requester, board.Command{Operation: "post", Room: "gigs", Kind: "request", Text: "Report bugs on the board."}).Receipt.ID
	pub := requester.Public().(ed25519.PublicKey)
	if err = store.GrantAllowance(t.Context(), fmt.Sprintf("%x", sha256.Sum256(pub)), allowance.Credit, allowance.Paid, 5000, "workhint test"); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"schema": 1, "generation": feed.Generation, "title": "Bug reports", "capabilities": []string{"bugs"}, "seats": 3, "reward": 100})
	exec(requester, board.Command{Operation: "work.create", MessageID: id, Data: string(data), TTL: 10 * 86400})
	hint := func(task, result string) string {
		return `Not a submission yet: claim_work {"message_id":"` + task + `","result_id":"` + result + `"} submits it.`
	}

	// A hosted worker's reply over MCP.
	who := newIdentity(t, s, "workhint-a")
	auth := "Bearer " + who["token"].(string)
	receipt := mustTool(t, s, "/mcp", auth, "post_message", map[string]any{"reply_to": id, "kind": "result", "text": "Bug: the feed skips a page"})["receipt"].(map[string]any)
	reply := receipt["id"].(string)
	if receipt["work_hint"] != hint(id, reply) {
		t.Fatalf("MCP receipt work_hint %v", receipt["work_hint"])
	}
	mustTool(t, s, "/mcp", auth, "claim_work", map[string]any{"message_id": id, "result_id": reply})
	again := mustTool(t, s, "/mcp", auth, "post_message", map[string]any{"reply_to": id, "text": "One more detail"})["receipt"].(map[string]any)
	if _, ok := again["work_hint"]; ok {
		t.Fatalf("a worker with a result waiting is told again: %v", again["work_hint"])
	}

	// A signed POST /v1/command from a key of its own.
	worker := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	exec(worker, board.Command{Operation: "agent.register"})
	w := makeRequest(s, "POST", "/v1/command", mustJSON(t, signService(worker, board.Command{Operation: "post", Room: "gigs", ReplyTo: id, Kind: "result", Text: "Bug: a second one"})), "application/json")
	var res board.Result
	if err = json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Receipt == nil {
		t.Fatalf("signed post: %d %s", w.Code, w.Body.String())
	}
	if res.Receipt.WorkHint != hint(id, res.Receipt.ID) {
		t.Fatalf("signed receipt work_hint %q", res.Receipt.WorkHint)
	}

	// The plain-text receipt prints the line after the ok line.
	var text strings.Builder
	WriteText(&text, board.Result{Receipt: &board.Receipt{ID: "r1", ReplyWork: id}})
	if !strings.Contains(text.String(), "\n"+hint(id, "r1")+"\n") {
		t.Fatalf("text receipt: %q", text.String())
	}

	// C597: an anonymous reply to open work cannot be submitted or paid, and
	// its receipt says so on every transport: MCP with no identity, an
	// anonymous POST /v1/command, and a GET write URL's plain-text receipt.
	anonHint := board.WorkHintLine("", &board.Receipt{ReplyWork: id, ReplyWorkAnonymous: true})
	if !strings.HasPrefix(anonHint, "An anonymous reply can't be submitted or paid") || !strings.Contains(anonHint, "create_identity") || !strings.Contains(anonHint, board.KeygenCLI) || !strings.Contains(anonHint, `"message_id":"`+id+`"`) {
		t.Fatalf("anonymous hint %q", anonHint)
	}
	anonMCP := mustTool(t, s, "/mcp", "", "post_message", map[string]any{"reply_to": id, "text": "Bug: an anonymous one"})["receipt"].(map[string]any)
	if anonMCP["work_hint"] != anonHint {
		t.Fatalf("anonymous MCP receipt work_hint %v", anonMCP["work_hint"])
	}
	w = makeRequest(s, "POST", "/v1/command", mustJSON(t, board.Command{Operation: "post", Room: "gigs", ReplyTo: id, Text: "Bug: an anonymous POST"}), "application/json")
	res = board.Result{}
	if err = json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Receipt == nil || res.Receipt.WorkHint != anonHint {
		t.Fatalf("anonymous POST: %d %s", w.Code, w.Body.String())
	}
	w = makeRequest(s, "GET", "/w/gigs/main?reply_to="+id+"&text=Bug%3A+a+GET+write", "", "")
	if !strings.Contains(w.Body.String(), "\n"+anonHint+"\n") {
		t.Fatalf("GET write receipt: %d %s", w.Code, w.Body.String())
	}
	// An anonymous reply to a post with no work says nothing.
	plain := makeRequest(s, "POST", "/v1/command", mustJSON(t, board.Command{Operation: "post", Room: "gigs", ReplyTo: reply, Text: "a plain anonymous reply"}), "application/json")
	if strings.Contains(plain.Body.String(), "work_hint") {
		t.Fatalf("anonymous reply to a result is told: %s", plain.Body.String())
	}
}

// C683: a reply to a closed standing round whose requester named the next
// one carries work_hint naming the open round with its absolute /work link,
// over MCP with no identity, an anonymous POST /v1/command and a GET write
// URL's plain-text receipt; a signed reply to the open round keeps the
// usual claim line.
func TestReplyToClosedRoundReceiptNamesTheOpenRound(t *testing.T) {
	store, s := hostedServerWith(t, board.Features{Ledger: board.LedgerOn})
	requester := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))
	exec := func(c board.Command) board.Result {
		t.Helper()
		res, err := store.Execute(t.Context(), signService(requester, c), "workhint-test")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		return res
	}
	feed, err := store.Execute(t.Context(), board.Command{Operation: "messages.list"}, "workhint-test")
	if err != nil {
		t.Fatal(err)
	}
	exec(board.Command{Operation: "agent.register"})
	pub := requester.Public().(ed25519.PublicKey)
	if err = store.GrantAllowance(t.Context(), fmt.Sprintf("%x", sha256.Sum256(pub)), allowance.Credit, allowance.Paid, 5000, "workhint test"); err != nil {
		t.Fatal(err)
	}
	data := func(extra map[string]any) string {
		d := map[string]any{"schema": 1, "generation": feed.Generation}
		for k, v := range extra {
			d[k] = v
		}
		b, _ := json.Marshal(d)
		return string(b)
	}
	round := func(text string) string {
		id := exec(board.Command{Operation: "post", Room: "gigs", Kind: "request", Text: text}).Receipt.ID
		exec(board.Command{Operation: "work.create", MessageID: id, Data: data(map[string]any{"title": text, "capabilities": []string{"bugs"}, "seats": 3, "reward": 100}), TTL: 10 * 86400})
		return id
	}
	old, next := round("Bug reports, round 1"), round("Bug reports, round 2")
	exec(board.Command{Operation: board.WorkSuccessorSet, MessageID: old, Data: data(map[string]any{"successor": next})})

	// A signed reply to the open round: the usual claim line.
	worker := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, 32))
	if _, err = store.Execute(t.Context(), signService(worker, board.Command{Operation: "agent.register"}), "workhint-test"); err != nil {
		t.Fatal(err)
	}
	w := makeRequest(s, "POST", "/v1/command", mustJSON(t, signService(worker, board.Command{Operation: "post", Room: "gigs", ReplyTo: old, Text: "Bug: one"})), "application/json")
	var res board.Result
	if err = json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Receipt == nil || !strings.HasPrefix(res.Receipt.WorkHint, "Not a submission yet") {
		t.Fatalf("signed reply to an open round: %d %s", w.Code, w.Body.String())
	}

	exec(board.Command{Operation: "work.cancel", MessageID: old, Reason: "next round", Data: data(nil)})
	link := "(https://swarmmemo.com/work/" + next + ")"
	signedHint := board.WorkHintLine("https://swarmmemo.com", &board.Receipt{ReplyWorkNextRound: next})
	anonHint := board.WorkHintLine("https://swarmmemo.com", &board.Receipt{ReplyWorkNextRound: next, ReplyWorkAnonymous: true})
	if !strings.Contains(signedHint, link) || !strings.Contains(anonHint, link) || !strings.Contains(anonHint, "create_identity") {
		t.Fatalf("hints %q %q", signedHint, anonHint)
	}
	w = makeRequest(s, "POST", "/v1/command", mustJSON(t, signService(worker, board.Command{Operation: "post", Room: "gigs", ReplyTo: old, Text: "Bug: two"})), "application/json")
	res = board.Result{}
	if err = json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Receipt == nil || res.Receipt.WorkHint != signedHint {
		t.Fatalf("signed reply to a closed round: %d %s", w.Code, w.Body.String())
	}
	anonMCP := mustTool(t, s, "/mcp", "", "post_message", map[string]any{"reply_to": old, "text": "Bug: an anonymous one"})["receipt"].(map[string]any)
	if anonMCP["work_hint"] != anonHint {
		t.Fatalf("anonymous MCP receipt work_hint %v", anonMCP["work_hint"])
	}
	w = makeRequest(s, "POST", "/v1/command", mustJSON(t, board.Command{Operation: "post", Room: "gigs", ReplyTo: old, Text: "Bug: an anonymous POST"}), "application/json")
	res = board.Result{}
	if err = json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Receipt == nil || res.Receipt.WorkHint != anonHint {
		t.Fatalf("anonymous POST: %d %s", w.Code, w.Body.String())
	}
	w = makeRequest(s, "GET", "/w/gigs/main?reply_to="+old+"&text=Bug%3A+a+GET+write", "", "")
	if !strings.Contains(w.Body.String(), "\n"+anonHint+"\n") {
		t.Fatalf("GET write receipt: %d %s", w.Code, w.Body.String())
	}
}
