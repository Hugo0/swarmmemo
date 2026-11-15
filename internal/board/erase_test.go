package board

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// C1012: an operator erasure over a real engine. The account's posts (every
// version, public and private) read as tombstones hidden by their author with
// the reason "erased by owner" and keep no text, signature or handle; its
// profile, links, memory, backups, files and write records go; a moderated
// post keeps its decision; a reply by another account and that account's
// rows stay byte-identical; the export carries tombstones the HF publisher
// accepts (hidden_by author); the erased key signs nothing more; a dry run
// changes nothing; a second run is a no-op; erasure_log records counts only.
func TestEraseAccount(t *testing.T) {
	s := openTest(t, Config{})
	ctx := WithRequestSignals(WithVia(context.Background(), "command"), RequestSignals{UserAgent: "erase-canary/1", RemoteAddr: "127.0.0.1:5000", ForwardedFor: "203.0.113.9"})
	exec := func(c Command) Result {
		t.Helper()
		r, err := s.Execute(ctx, c, "203.0.113.9")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		return r
	}
	alice, bob := keyFor(201), keyFor(202)
	a, b := keyID(alice), keyID(bob)
	register(t, s, bob)
	exec(signed(alice, Command{Operation: "agent.register", Handle: "alice-erase"}))
	first := exec(signed(alice, Command{Operation: "post", Room: "lobby", Text: "alice secret one", RequestID: "e1"})).Receipt.ID
	edit := exec(signed(alice, Command{Operation: "post", Room: "lobby", Text: "alice secret one, edited", Data: `{"schema":1,"supersedes":"` + first + `"}`, RequestID: "e2"})).Receipt.ID
	moderated := exec(signed(alice, Command{Operation: "post", Room: "lobby", Text: "alice rude post", RequestID: "e3"})).Receipt.ID
	exec(signed(bob, Command{Operation: "post", Room: "lobby", Text: "bob replies", ReplyTo: first, RequestID: "b1"}))
	bobOwn := exec(signed(bob, Command{Operation: "post", Room: "lobby", Text: "bob own words", RequestID: "b2"})).Receipt.ID
	room := convRoom("erase-dm")
	openConv(t, s, alice, room, "dm", bob)
	dm := exec(signed(alice, Command{Operation: "post", Room: room, Text: "alice private words", RequestID: "e4"})).Receipt.ID
	exec(signed(alice, Command{Operation: "agent.profile.publish", Data: testPeerData}))
	if _, err := s.db.Exec("UPDATE events SET hidden=1,hidden_by='operator',reason='harassment' WHERE id=?", moderated); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO memory_items(account,key,value,visibility,bytes,version,created_at,updated_at) VALUES(?1,'k','alice memory','private',12,1,1,1)",
		"INSERT INTO memory_items(account,key,value,visibility,bytes,version,created_at,updated_at) VALUES(?2,'k','bob memory','private',10,1,1,1)",
		"INSERT INTO key_backups(account,key_id,scheme,credential_sha256,salt,iv,ciphertext,version,created_at,updated_at) VALUES(?1,?1,'s','c','s','i','alice-ct',1,1,1)",
		"INSERT INTO identity_links(agent,kind,value,state,created_at) VALUES(?1,'github','alice-gh','verified',1)",
		"INSERT INTO blobs(id,room,account,filename,media_type,hash,size,created_at,expires_at,data) VALUES('blob-a','lobby',?1,'alice.txt','text/plain','h',3,1,0,x'616263')",
		"INSERT INTO feedback(id,subject,account,about,text,created_at) VALUES('fb-a','s',?1,'a','alice feedback',1)",
	} {
		if _, err := s.db.Exec(q, a, b); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM write_signals WHERE account=?", a); n == 0 {
		t.Fatal("no write signals recorded for alice")
	}
	if rawSignalCount(t, s) == 0 {
		t.Fatal("no raw records")
	}
	bobBefore := eraseRows(t, s, b)

	// Open work refuses.
	if _, err := s.db.Exec("INSERT INTO works(id,requester,title,capabilities,state,generation,created_at,updated_at,deadline,worker) VALUES(?,?,'t','[]','claimed','g',1,1,1,?)", bobOwn, b, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OperatorEraseAccount(testContext, a, "req-1", true); err == nil || !strings.Contains(err.Error(), "work it claimed") {
		t.Fatalf("open work: %v", err)
	}
	if _, err := s.db.Exec("DELETE FROM works WHERE id=?", bobOwn); err != nil {
		t.Fatal(err)
	}
	// Personal data as the request reference refuses.
	if _, err := s.OperatorEraseAccount(testContext, a, "alice@example.com", false); err == nil {
		t.Fatal("an email address as the reference was accepted")
	}

	// Dry run: exact counts, nothing changed.
	textBefore := sqlCount(t, s, "SELECT count(*) FROM events WHERE account=? AND text<>''", a)
	dry, err := s.OperatorEraseAccount(testContext, a, "req-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || eraseCount(dry, "events", "hidden") != 3 || dry.RawErased == 0 {
		t.Fatalf("dry run: %+v", dry)
	}
	if sqlCount(t, s, "SELECT count(*) FROM events WHERE account=? AND text<>''", a) != textBefore || sqlCount(t, s, "SELECT count(*) FROM erasure_log") != 0 {
		t.Fatal("the dry run changed the database")
	}

	r, err := s.OperatorEraseAccount(testContext, a, "req-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if r.DryRun || len(r.ReceiptHash) != 64 || eraseCount(r, "events", "hidden") != 3 {
		t.Fatalf("receipt: %+v", r)
	}
	for _, id := range []string{first, edit, dm} {
		var text, sig, payload, handle, by, reason string
		var hidden bool
		if err = s.db.QueryRow("SELECT text,signature,payload,handle,hidden,hidden_by,reason FROM events WHERE id=?", id).Scan(&text, &sig, &payload, &handle, &hidden, &by, &reason); err != nil {
			t.Fatal(err)
		}
		if text != "" || sig != "" || payload != "" || handle != "" || !hidden || by != hiddenByAuthor || reason != ErasedReason {
			t.Fatalf("%s after erase: %q %q %q %q %v %q %q", id, text, sig, payload, handle, hidden, by, reason)
		}
	}
	var by, reason, text string
	if err = s.db.QueryRow("SELECT hidden_by,reason,text FROM events WHERE id=?", moderated).Scan(&by, &reason, &text); err != nil || by != "operator" || reason != "harassment" || text != "" {
		t.Fatalf("moderated post: %q %q %q %v", by, reason, text, err)
	}
	for q, want := range map[string]int64{
		"SELECT count(*) FROM peer_cards WHERE account=?1":                                   0,
		"SELECT count(*) FROM memory_items WHERE account=?1":                                 0,
		"SELECT count(*) FROM key_backups WHERE account=?1":                                  0,
		"SELECT count(*) FROM identity_links WHERE agent=?1":                                 0,
		"SELECT count(*) FROM blobs WHERE account=?1 AND (data IS NOT NULL OR filename<>'')": 0,
		"SELECT count(*) FROM feedback WHERE account=?1 AND text<>''":                        0,
		"SELECT count(*) FROM write_signals WHERE account=?1":                                0,
		"SELECT count(*) FROM identities WHERE id=?1 AND handle='' AND revoked_at>0":         1,
		"SELECT count(*) FROM conversation_members WHERE account=?1 AND state='active'":      0,
		"SELECT count(*) FROM erasure_log WHERE account=?1 AND reference='req-1'":            1,
		"SELECT count(*) FROM audit WHERE operation='account.erase' AND target=?1":           1,
	} {
		if got := sqlCount(t, s, q, a); got != want {
			t.Errorf("%s: %d, want %d", q, got, want)
		}
	}
	var raw int
	if err = s.rawSignals.db.QueryRow("SELECT count(*) FROM raw_signals WHERE account=?", a).Scan(&raw); err != nil || raw != 0 {
		t.Fatalf("raw records left: %d %v", raw, err)
	}
	var logged string
	if err = s.db.QueryRow("SELECT counts||link_digests FROM erasure_log WHERE account=?", a).Scan(&logged); err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"alice", "alice secret", "github"} {
		if strings.Contains(logged, leak) {
			t.Fatalf("erasure_log holds %q: %s", leak, logged)
		}
	}
	if got := eraseRows(t, s, b); got != bobBefore {
		t.Fatalf("bob's rows changed:\n%s\n%s", bobBefore, got)
	}

	// Reads: the room shows tombstones, the reply still resolves.
	res := exec(Command{Operation: "messages.list", Room: "lobby"})
	raw2, _ := json.Marshal(res)
	for _, leak := range []string{"alice secret", "alice rude", "alice-erase"} {
		if strings.Contains(string(raw2), leak) {
			t.Fatalf("a read shows %q", leak)
		}
	}
	if !strings.Contains(string(raw2), ErasedReason) || !strings.Contains(string(raw2), "bob replies") {
		t.Fatalf("room read after erase: %s", raw2)
	}
	// The export queues tombstones hidden by the author.
	if sqlCount(t, s, "SELECT count(*) FROM changes WHERE event_id IN (?,?)", first, edit) < 4 {
		t.Fatal("no public correction queued for the erased posts")
	}

	// The erased key signs nothing more.
	_, err = s.Execute(testContext, signed(alice, Command{Operation: "post", Room: "lobby", Text: "back", RequestID: "e9"}), "test-origin")
	if errCode(err) != "key_revoked" || !strings.Contains(err.Error(), "erased") {
		t.Fatalf("a write by the erased key: %v", err)
	}

	// A second run (a restore replay) changes nothing.
	again, err := s.OperatorEraseAccount(testContext, a, "req-1-replay", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range again.Counts {
		if c.Rows != 0 {
			t.Errorf("replay %s %s: %d rows", c.Store, c.Action, c.Rows)
		}
	}
	if entries, err := s.ErasureLog(testContext); err != nil || len(entries) != 2 || entries[0].Account != a {
		t.Fatalf("erasure log: %+v %v", entries, err)
	}
	if _, err = s.db.Exec("DELETE FROM erasure_log"); err == nil {
		t.Fatal("the erasure log was deleted")
	}
}

// Raw and write records of a post under an open report stay as evidence.
func TestEraseHoldsEvidence(t *testing.T) {
	s := openTest(t, Config{})
	ctx := WithRequestSignals(WithVia(context.Background(), "command"), RequestSignals{UserAgent: "erase-canary/1"})
	alice := keyFor(203)
	a := keyID(alice)
	var ids []string
	for _, text := range []string{"reported", "plain"} {
		r, err := s.Execute(ctx, signed(alice, Command{Operation: "post", Room: "lobby", Text: text, RequestID: text}), "203.0.113.9")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.Receipt.ID)
	}
	if _, err := s.db.Exec("INSERT INTO reports(id,event_id,actor,reason,created_at) VALUES('rp1',?,'x','doxxing',1)", ids[0]); err != nil {
		t.Fatal(err)
	}
	r, err := s.OperatorEraseAccount(testContext, a, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if r.RawHeld != 1 || r.RawErased != 1 {
		t.Fatalf("raw erased %d held %d", r.RawErased, r.RawHeld)
	}
	if sqlCount(t, s, "SELECT count(*) FROM write_signals WHERE account=? AND object_id=?", a, ids[0]) != 1 ||
		sqlCount(t, s, "SELECT count(*) FROM write_signals WHERE account=? AND object_id=?", a, ids[1]) != 0 {
		t.Fatal("write signals: the reported post's should stay, the other's go")
	}
	if sqlCount(t, s, "SELECT count(*) FROM reports WHERE id='rp1' AND reason='doxxing'") != 1 {
		t.Fatal("the report was touched")
	}
}

func eraseCount(r ErasureReceipt, store, action string) int64 {
	for _, c := range r.Counts {
		if c.Store == store && strings.Contains(c.Action, action) {
			return c.Rows
		}
	}
	return -1
}

// eraseRows is another account's rows, to compare before and after.
func eraseRows(t *testing.T, s *Store, account string) string {
	t.Helper()
	var out strings.Builder
	for _, q := range []string{
		"SELECT id||text||signature||payload||handle||hidden||reason FROM events WHERE account=?1 ORDER BY seq",
		"SELECT key||value FROM memory_items WHERE account=?1",
		"SELECT id||handle||revoked_at FROM identities WHERE account=?1",
	} {
		rows, err := s.db.Query(q, account)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var v string
			if err = rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			out.WriteString(v + "\n")
		}
		rows.Close()
	}
	return out.String()
}

// Schema 54 (erasure_log): a populated schema-53 database migrates to
// exactly a new database's schema and keeps every row.
func TestSchema54Upgrade(t *testing.T) { schema29UpgradeFrom(t, 53) }
