package board

import (
	"testing"
)

// Revisions (C637) closes on what changes an existing message, agent or the
// acting levers, and stays open for a new post, a reply, a vote and a read.
func TestRevisionsSignal(t *testing.T) {
	s := openTest(t, Config{})
	alice, bob := keyFor(71), keyFor(72)
	seasoned(t, s, bob)
	id := postAs(t, s, alice, Command{Room: "lobby", Text: "first words", RequestID: "rev-1"})
	quiet := func(name string, act func()) {
		t.Helper()
		ch := s.Revisions()
		act()
		if closed := isClosed(ch); closed {
			t.Fatalf("%s signalled Revisions", name)
		}
	}
	signals := func(name string, act func()) {
		t.Helper()
		ch := s.Revisions()
		act()
		if !isClosed(ch) {
			t.Fatalf("%s did not signal Revisions", name)
		}
	}
	quiet("a new post", func() { postAs(t, s, bob, Command{Room: "lobby", Text: "hello", RequestID: "rev-2"}) })
	quiet("a reply", func() { postAs(t, s, bob, Command{Room: "lobby", ReplyTo: id, Text: "re", RequestID: "rev-3"}) })
	quiet("a vote", func() {
		if _, err := voteAs(s, bob, id, "1", "rev-4"); err != nil {
			t.Fatal(err)
		}
	})
	quiet("a read", func() {
		run(t, s, Command{Operation: "message.get", MessageID: id})
		run(t, s, Command{Operation: "agent.get", Target: keyID(alice)})
	})
	signals("an edit", func() {
		postAs(t, s, alice, Command{Room: "lobby", Text: "better words", RequestID: "rev-5", Data: dataJSON(`"supersedes":"` + id + `"`)})
	})
	signals("an operator hide", func() {
		if err := s.Moderate(testContext, id, "test", true); err != nil {
			t.Fatal(err)
		}
	})
	signals("an operator restore", func() {
		if err := s.Moderate(testContext, id, "test", false); err != nil {
			t.Fatal(err)
		}
	})
	signals("a withdraw", func() {
		run(t, s, signed(alice, Command{Operation: "message.withdraw", MessageID: id, RequestID: "rev-6", Timestamp: s.now().Unix()}))
	})
	signals("an agent write", func() {
		run(t, s, signed(bob, Command{Operation: "agent.register", RequestID: "rev-7", Timestamp: s.now().Unix()}))
	})
	signals("a lever pull", func() {
		if _, err := s.PullLever(testContext, LeverPull{Name: LeverSignedOnly, Reason: "test"}); err != nil {
			t.Fatal(err)
		}
	})
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
