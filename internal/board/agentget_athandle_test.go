package board

import (
	"context"
	"testing"
)

// C864: agent.get takes "@handle" as the handle, as agent.posts does.
func TestAgentGetAtHandle(t *testing.T) {
	s := openTest(t, Config{})
	person := keyFor(241)
	register(t, s, person)
	run(t, s, signed(person, Command{Operation: "agent.register", Handle: "athandle"}))
	for _, target := range []string{"athandle", "@athandle", "@AtHandle"} {
		if a := run(t, s, signed(person, Command{Operation: "agent.get", Target: target})).Agent; a == nil || a.ID != keyID(person) {
			t.Fatalf("agent.get %q = %+v", target, a)
		}
	}
	fails(t, s, signed(person, Command{Operation: "agent.get", Target: "@nobody-here"}), "not_found")
	// agentFingerprint (works.list agent=, record reads) takes it too.
	for _, who := range []string{"athandle", "@athandle"} {
		if id, err := agentFingerprint(context.Background(), s.db, who); err != nil || id != keyID(person) {
			t.Fatalf("agentFingerprint %q = %q, %v", who, id, err)
		}
	}
}

// C865: home agent= takes a handle or @handle, as its invalid_agent says.
func TestHomeAgentHandle(t *testing.T) {
	s := openTest(t, Config{})
	person := keyFor(242)
	register(t, s, person)
	run(t, s, signed(person, Command{Operation: "agent.register", Handle: "homehandle"}))
	want := run(t, s, Command{Operation: "home", Target: keyID(person)}).Data["agent"]
	if want == nil {
		t.Fatal("home has no agent field")
	}
	for _, target := range []string{"homehandle", "@HomeHandle"} {
		if got := run(t, s, Command{Operation: "home", Target: target}).Data["agent"]; got != want {
			t.Fatalf("home %q agent = %v, want %v", target, got, want)
		}
	}
	fails(t, s, Command{Operation: "home", Target: "no-such-handle"}, "agent_not_found")
	fails(t, s, Command{Operation: "home", Target: "bad target!"}, "invalid_agent")
	fails(t, s, Command{Operation: "home"}, "invalid_request")
}
