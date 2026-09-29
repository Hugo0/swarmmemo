package board

// Security review 1.21.0: regression tests for calls without a key in the
// store.

import (
	"sync/atomic"
	"testing"
	"time"
)

// L5: an unsigned caller's pseudonym is derived at the command's own clock.
// A command whose first clock read falls just before midnight used to be
// named under that day's salt while the ledger charged the next day, so the
// next day had two subjects for one network (and two shares).
func TestSecReview121PseudonymAtTheCommandsClock(t *testing.T) {
	s := openAnonServices(t, 3, 100_000)
	day := testTime / 86400
	var reads atomic.Int64
	s.now = func() time.Time {
		if reads.Add(1) == 1 {
			return time.Unix((day+1)*86400-1, 0) // 23:59:59, the first read only
		}
		return time.Unix((day+1)*86400+1, 0)
	}
	for i, id := range []string{"midnight-1", "midnight-2", "midnight-3"} {
		if _, err := stampFrom(s, "198.51.100.7", id, id, 1); err != nil {
			t.Fatalf("stamp %d: %v", i, err)
		}
	}
	var subjects int
	if err := s.db.QueryRow("SELECT count(DISTINCT subject) FROM allowance_claims WHERE resource='credit' AND day=?", day+1).Scan(&subjects); err != nil {
		t.Fatal(err)
	}
	if subjects != 1 {
		t.Fatalf("one network has %d credit subjects on one day", subjects)
	}
}

// L6: an exact retry just after midnight of a call made just before it is
// found under yesterday's pseudonym, and so is the call it names: a call
// still running answers request_in_flight, not call_not_found.
func TestSecReview121RetryAcrossMidnightFindsTheCall(t *testing.T) {
	s := openAnonServices(t, 3, 100_000)
	day := testTime / 86400
	d0At(s, (day+1)*86400-600) // 23:50
	if _, err := stampFrom(s, "198.51.100.7", "late", "across-midnight", 1); err != nil {
		t.Fatal(err)
	}
	// Stand-in for a remote call still running: the notary answers at once,
	// so its stored answer and its record are set back to running.
	if _, err := s.db.Exec(`UPDATE requests SET result=replace(result,'"state":"done"','"state":"running"') WHERE request_key=?`, "id:"+anonID("across-midnight")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE service_calls SET state='running', expires_at=? WHERE account LIKE 'anon:%'", (day+1)*86400+3600); err != nil {
		t.Fatal(err)
	}
	d0At(s, (day+1)*86400+600) // 00:10, inside the salt's grace
	_, err := stampFrom(s, "198.51.100.7", "late", "across-midnight", 1)
	if code, status := codeOf(err); code != "request_in_flight" || status != 409 {
		t.Fatalf("a retry across midnight of a running call: %v", err)
	}
}
