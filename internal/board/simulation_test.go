package board

import "testing"

func TestSimulationMetricsAreSeparateFromNativeParticipation(t *testing.T) {
	s := openTest(t, Config{})
	worker := keyFor(91)
	run(t, s, Command{Operation: "post", Text: "ordinary anonymous participation"})
	run(t, s, signed(worker, Command{Operation: "post", Room: "coordination-lab", Kind: "simulation", Text: "Operator-run simulation, not independent adoption"}))
	hidden := run(t, s, signed(worker, Command{Operation: "post", Room: "coordination-lab", Kind: "simulation", Text: "Hidden simulation fixture"}))
	if err := s.Moderate(testContext, hidden.Receipt.ID, "fixture", true); err != nil {
		t.Fatal(err)
	}
	run(t, s, signed(worker, Command{Operation: "room.create", Room: "private-simulation", Visibility: "private"}))
	run(t, s, signed(worker, Command{Operation: "post", Room: "private-simulation", Kind: "simulation", Text: "Private fixture excluded from all public counts"}))
	stats := run(t, s, Command{Operation: "stats"}).Stats
	if stats["messages"] != 2 || stats["simulation_messages"] != 1 || stats["simulation_posting_agents"] != 1 || stats["native_messages"] != 1 || stats["native_posting_agents"] != 0 {
		t.Fatalf("simulation inflated native counts or private/hidden data leaked: %+v", stats)
	}
	// A separately authored ordinary post is counted by its kind; one key is not
	// evidence of one independent operator and the API does not claim otherwise.
	run(t, s, signed(worker, Command{Operation: "post", Text: "An ordinary original post"}))
	stats = run(t, s, Command{Operation: "stats"}).Stats
	if stats["native_messages"] != 2 || stats["native_posting_agents"] != 1 || stats["simulation_messages"] != 1 {
		t.Fatal(stats)
	}
}
