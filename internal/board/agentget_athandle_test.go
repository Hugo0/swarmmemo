package board

import "testing"

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
}
