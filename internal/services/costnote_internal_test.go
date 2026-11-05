package services

import "testing"

// C748: cost_note reads cost, credits_spent, paid_from and allowance_left as
// one plain sentence, in every paid_from case.
func TestCostNoteEachPaidFromCase(t *testing.T) {
	left := int64(99_907)
	zero := int64(0)
	for _, c := range []struct {
		name        string
		cost, spent int64
		left        *int64
		resource    string
		want        string
	}{
		{"free call", 0, 0, &left, "credit", "Free: no cost."},
		{"free_allowance", 80, 0, &left, "credit", "80 credits from your free daily allowance (99,907 left); 0 credits spent."},
		{"free_allowance, left unknown", 80, 0, nil, "credit", "80 credits from your free daily allowance; 0 credits spent."},
		{"credits", 80, 80, &zero, "credit", "80 credits spent from your balance."},
		{"credits, mixed", 80, 50, &zero, "credit", "80 credits: 30 credits from your free daily allowance (0 left), 50 credits spent from your balance."},
		{"one credit", 1, 1, nil, "credit", "1 credit spent from your balance."},
		{"bytes", 1_200, 0, &left, "post_bytes", "1,200 bytes from your free daily allowance (99,907 left); 0 bytes spent."},
	} {
		if got := costNote(c.cost, c.spent, c.left, c.resource); got != c.want {
			t.Errorf("%s: cost_note %q, want %q", c.name, got, c.want)
		}
	}
}
