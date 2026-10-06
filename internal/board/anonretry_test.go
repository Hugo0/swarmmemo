package board

import (
	"encoding/json"
	"strings"
	"testing"
)

func anonRetryEvents(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM events").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func anonRetryCharged(t *testing.T, s *Store) int64 {
	t.Helper()
	var used int64
	if err := s.db.QueryRow("SELECT COALESCE(SUM(used),0) FROM quota WHERE actor<>'global'").Scan(&used); err != nil {
		t.Fatal(err)
	}
	return used
}

func anonRetryExec(t *testing.T, s *Store, c Command, source string) Result {
	t.Helper()
	r, err := s.Execute(testContext, c, source)
	if err != nil || !r.OK || r.Receipt == nil {
		t.Fatalf("execute from %s: %+v %v", source, r, err)
	}
	return r
}

// One intended anonymous public post that an edge transport delivers from two
// networks is one post: the second gets the original receipt and is not
// charged.
func TestAnonCrossNetworkRetryIsOnePost(t *testing.T) {
	for _, prefix := range []bool{false, true} {
		s := openTest(t, Config{ArchiveDelaySeconds: -1, Features: Features{AnonPrefix: prefix}})
		d0At(s, testTime)
		c := Command{Operation: "post", Room: "lobby", Text: "one intended post", RequestID: "edge-retry"}
		first := anonRetryExec(t, s, c, "198.51.100.7")
		charged := anonRetryCharged(t, s)
		d0At(s, testTime+2)
		moved := anonRetryExec(t, s, c, "203.0.113.9")
		if !moved.Receipt.Duplicate || moved.Receipt.ID != first.Receipt.ID || moved.Receipt.Hash != first.Receipt.Hash ||
			moved.Receipt.Cursor != first.Receipt.Cursor || moved.Receipt.AcceptedAt != first.Receipt.AcceptedAt {
			t.Fatalf("prefix=%v: cross-network retry %+v, original %+v", prefix, moved.Receipt, first.Receipt)
		}
		// Only the public receipt travels: nothing else from the first network.
		encoded, _ := json.Marshal(moved)
		if moved.Allowance != nil || moved.Next != nil || len(moved.Data) != 0 || strings.Contains(string(encoded), "anon:") {
			t.Fatalf("prefix=%v: cross-network retry carries more than the receipt: %s", prefix, encoded)
		}
		if got := anonRetryEvents(t, s); got != 1 {
			t.Fatalf("prefix=%v: %d events, want 1", prefix, got)
		}
		if got := anonRetryCharged(t, s); got != charged {
			t.Fatalf("prefix=%v: retry charged %d more", prefix, got-charged)
		}
	}
}

// The same request_id with different bytes from another network is a new
// post, as before.
func TestAnonCrossNetworkDifferentBytesPostsTwice(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1})
	d0At(s, testTime)
	first := anonRetryExec(t, s, Command{Operation: "post", Text: "first wording", RequestID: "shared-id"}, "198.51.100.7")
	second := anonRetryExec(t, s, Command{Operation: "post", Text: "second wording", RequestID: "shared-id"}, "203.0.113.9")
	if second.Receipt.Duplicate || second.Receipt.ID == first.Receipt.ID || anonRetryEvents(t, s) != 2 {
		t.Fatalf("different bytes collapsed: %+v %+v", first.Receipt, second.Receipt)
	}
}

// After the window an identical request from another network is a new post.
func TestAnonCrossNetworkRetryWindow(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1})
	c := Command{Operation: "post", Text: "same words later", RequestID: "late-id"}
	d0At(s, testTime)
	first := anonRetryExec(t, s, c, "198.51.100.7")
	d0At(s, testTime+AnonCrossNetworkRetrySeconds)
	inside := anonRetryExec(t, s, c, "203.0.113.9")
	if !inside.Receipt.Duplicate || inside.Receipt.ID != first.Receipt.ID {
		t.Fatalf("retry at the window's edge: %+v", inside.Receipt)
	}
	d0At(s, testTime+AnonCrossNetworkRetrySeconds+1)
	late := anonRetryExec(t, s, c, "192.0.2.44")
	if late.Receipt.Duplicate || late.Receipt.ID == first.Receipt.ID || anonRetryEvents(t, s) != 2 {
		t.Fatalf("retry after the window: %+v", late.Receipt)
	}
}

// A signed post's retry scope is its key, unchanged: a different key's
// identical-looking command is its own post, and an anonymous post never
// answers for a signed one.
func TestAnonCrossNetworkRetryLeavesSignedAlone(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1})
	d0At(s, testTime)
	anon := anonRetryExec(t, s, Command{Operation: "post", Text: "signed words", RequestID: "signed-id"}, "198.51.100.7")
	c := Command{Operation: "post", Text: "signed words", RequestID: "signed-id", Nonce: "n1", Timestamp: testTime}
	one := anonRetryExec(t, s, signed(keyFor(61), c), "203.0.113.9")
	two := anonRetryExec(t, s, signed(keyFor(62), c), "203.0.113.9")
	if one.Receipt.Duplicate || two.Receipt.Duplicate || one.Receipt.ID == anon.Receipt.ID || one.Receipt.ID == two.Receipt.ID {
		t.Fatalf("signed posts matched across actors: %+v %+v %+v", anon.Receipt, one.Receipt, two.Receipt)
	}
	again := anonRetryExec(t, s, signed(keyFor(61), c), "192.0.2.44")
	if !again.Receipt.Duplicate || again.Receipt.ID != one.Receipt.ID || anonRetryEvents(t, s) != 3 {
		t.Fatalf("signed retry: %+v", again.Receipt)
	}
}

// A post to a recipient is never matched across networks.
func TestAnonCrossNetworkRetryLeavesDirectAlone(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1})
	d0At(s, testTime)
	to := keyID(keyFor(63))
	c := Command{Operation: "post", Text: "for one reader", To: to, RequestID: "dm-id"}
	first := anonRetryExec(t, s, c, "198.51.100.7")
	moved := anonRetryExec(t, s, c, "203.0.113.9")
	if moved.Receipt.Duplicate || moved.Receipt.ID == first.Receipt.ID || anonRetryEvents(t, s) != 2 {
		t.Fatalf("direct post matched across networks: %+v %+v", first.Receipt, moved.Receipt)
	}
}

// The lookup uses its partial index, never a table scan.
func TestAnonCrossNetworkRetryUsesIndex(t *testing.T) {
	s := openTest(t, Config{})
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+anonRetryQuery, "id:x", "d", 0, "anon:x")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if joined := strings.Join(plan, "; "); !strings.Contains(joined, "requests_anon_key") || strings.Contains(joined, "SCAN") {
		t.Fatalf("plan: %s", joined)
	}
}
