package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

func endorsementServer(t *testing.T, features board.Features) (*board.Store, *Server) {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "board.db"), board.Config{ServiceID: "swarmmemo.com", Features: features})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, New(store, nil, Config{ServiceID: "swarmmemo.com", Features: features})
}

func readRecords(t *testing.T, body string) []board.EndorsementRecord {
	t.Helper()
	var out []board.EndorsementRecord
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		var r board.EndorsementRecord
		dec := json.NewDecoder(strings.NewReader(scanner.Text()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("line %q: %v", scanner.Text(), err)
		}
		out = append(out, r)
	}
	return out
}

// Every vouch in the stream verifies offline from the line alone, and paging
// with X-Next-Cursor reaches each record once.
func TestEndorsementStreamOverHTTP(t *testing.T) {
	features := board.Features{VoteRecords: true, ExportEndorsements: true}
	store, s := endorsementServer(t, features)
	alice, bob, carol := newRoomKey(t), newRoomKey(t), newRoomKey(t)
	for _, k := range []*roomKey{alice, bob, carol} {
		if _, err := store.Execute(context.Background(), k.sign(board.Command{Operation: "agent.register"}), "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range []struct {
		from   *roomKey
		target string
		data   string
	}{{bob, alice.id, `{"schema":1,"value":1,"sponsor":true}`}, {carol, alice.id, `{"schema":1,"value":1}`}, {alice, bob.id, `{"schema":1,"value":1}`}} {
		if _, err := store.Execute(context.Background(), c.from.sign(board.Command{Operation: "vouch", Target: c.target, Data: c.data}), "fixture"); err != nil {
			t.Fatalf("vouch %d: %v", i, err)
		}
	}
	var all []board.EndorsementRecord
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("stream never ended")
		}
		w := get(s, "/v1/export?stream=endorsements&limit=2&cursor="+url.QueryEscape(cursor), "")
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/x-ndjson") || w.Header().Get("X-Next-Cursor") == "" {
			t.Fatalf("page %d: %d %v %s", pages, w.Code, w.Header(), w.Body)
		}
		page := readRecords(t, w.Body.String())
		all = append(all, page...)
		next := w.Header().Get("X-Next-Cursor")
		if len(page) == 0 || next == cursor {
			break
		}
		cursor = next
	}
	if len(all) != 3 {
		t.Fatalf("records: %+v", all)
	}
	for i, r := range all {
		if r.Seq != int64(i+1) || r.Type != "vouch" {
			t.Fatalf("record %d: %+v", i, r)
		}
		if err := board.VerifyEndorsementRecord("swarmmemo.com", r); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	// HEAD returns the headers only.
	r := httptest.NewRequest("HEAD", "/v1/export?stream=endorsements", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("X-Next-Cursor") == "" {
		t.Fatalf("HEAD: %d %q", w.Code, w.Body)
	}
	for _, bad := range []string{"stream=messages", "stream=endorsements&limit=0", "stream=endorsements&limit=1001", "stream=endorsements&room=lobby", "stream=endorsements&stream=endorsements", "stream=endorsements&cursor=nope"} {
		if w := get(s, "/v1/export?"+bad, ""); w.Code != 400 {
			t.Fatalf("%s: %d %s", bad, w.Code, w.Body)
		}
	}
	// The message export is unchanged by the stream.
	if w := get(s, "/v1/export", ""); w.Code != 200 || strings.Contains(w.Body.String(), `"vouch"`) {
		t.Fatalf("message export: %d %s", w.Code, w.Body)
	}
}

// With EXPORT_ENDORSEMENTS off, /v1/export answers a stream parameter as it
// always did.
func TestEndorsementStreamOffIsTodaysExport(t *testing.T) {
	_, off := endorsementServer(t, board.Features{VoteRecords: true})
	_, plain := endorsementServer(t, board.Features{})
	for _, path := range []string{"/v1/export?stream=endorsements", "/v1/export?stream=x"} {
		a, b := get(off, path, ""), get(plain, path, "")
		if a.Code != 400 || a.Code != b.Code || a.Body.String() != b.Body.String() {
			t.Fatalf("%s: %d %s vs %d %s", path, a.Code, a.Body, b.Code, b.Body)
		}
	}
}

// /capabilities votes.in_exports follows EXPORT_ENDORSEMENTS (§8.4); with
// every flag off the votes object is today's.
func TestVotesCapabilityFollowsTheEndorsementFlags(t *testing.T) {
	for _, tc := range []struct {
		features            board.Features
		inExports, recorded bool
	}{
		{board.Features{}, false, false},
		{board.Features{VoteRecords: true}, false, true},
		{board.Features{VoteRecords: true, ExportEndorsements: true}, true, true},
	} {
		_, s := endorsementServer(t, tc.features)
		votes := s.capabilities()["votes"].(map[string]any)
		_, recorded := votes["endorsements"]
		if votes["in_exports"] != tc.inExports || recorded != tc.recorded {
			t.Fatalf("%+v: %v", tc.features, votes)
		}
	}
}
