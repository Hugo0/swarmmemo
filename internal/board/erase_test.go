package board

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
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

// C1059: an erasure keeps the account's standing proofs only as keyed
// hashes: no raw GitHub login, wallet or root is left, a second run changes
// nothing, and a sign-in with the same outside account still finds the
// erased holder, so it cannot make a second identity.
func TestEraseHashesStandingAssessments(t *testing.T) {
	s := openTest(t, Config{})
	alice, bob := keyFor(205), keyFor(206)
	a, b := keyID(alice), keyID(bob)
	register(t, s, alice)
	register(t, s, bob)
	const wallet = "0x52908400098527886E0F7030069857D2E4169EE7"
	ghRoot, wRoot := "github:4242", walletRoot(wallet)
	for _, q := range []string{
		"INSERT INTO standing_assessments(agent,kind,value,root,assessed,detail,created_at) VALUES(?1,'github','alice-gh','" + ghRoot + "',300,'{\"login\":\"alice-gh\"}',1)",
		"INSERT INTO standing_assessments(agent,kind,value,root,assessed,detail,created_at) VALUES(?1,'wallet','" + wallet + "','" + wRoot + "',200,'{\"address\":\"" + wallet + "\"}',1)",
	} {
		if _, err := s.db.Exec(q, a); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	dump := func() string {
		t.Helper()
		var out string
		if err := s.db.QueryRow("SELECT coalesce(group_concat(kind||' '||value||' '||root||' '||detail,'|'),'') FROM (SELECT * FROM standing_assessments WHERE agent=? ORDER BY kind)", a).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if _, err := s.OperatorEraseAccount(testContext, a, "req-sa", true); err != nil {
		t.Fatal(err)
	}
	after := dump()
	for _, leak := range []string{"alice-gh", "4242", strings.ToLower(wallet)[2:], wallet[2:]} {
		if strings.Contains(strings.ToLower(after), strings.ToLower(leak)) {
			t.Fatalf("standing_assessments keeps %q after erase: %s", leak, after)
		}
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM standing_assessments WHERE agent=?1 AND value LIKE 'hmac:%' AND root LIKE 'hmac:%' AND detail='' AND assessed>0", a); n != 2 {
		t.Fatalf("hashed assessments: %d, want 2: %s", n, after)
	}
	if _, err := s.OperatorEraseAccount(testContext, a, "req-sa-2", true); err != nil {
		t.Fatal(err)
	}
	if again := dump(); again != after {
		t.Fatalf("a second erase changed the hashes:\n%s\n%s", after, again)
	}
	// The same GitHub account or wallet, now linked by another account,
	// still meets the erased holder; an unseen root meets nobody.
	for _, root := range []string{ghRoot, wRoot} {
		if _, err := s.db.Exec("INSERT INTO standing_assessments(agent,kind,value,root,created_at) VALUES(?,?,?,?,2)", b, strings.SplitN(root, ":", 2)[0], root, root); err != nil {
			t.Fatal(err)
		}
		owner, err := s.loginOwner(testContext, root)
		if err != nil || !owner.keyHeld {
			t.Fatalf("%s after erase: %+v %v (the erased holder is not found)", root, owner, err)
		}
		if _, err = s.db.Exec("DELETE FROM standing_assessments WHERE agent=?", b); err != nil {
			t.Fatal(err)
		}
		if owner, err = s.loginOwner(testContext, root); err != nil || !owner.keyHeld {
			t.Fatalf("%s held only by the erased account: %+v %v", root, owner, err)
		}
	}
	if owner, err := s.loginOwner(testContext, "github:9999"); err != nil || owner != (loginOwnerOf{}) {
		t.Fatalf("unseen root: %+v %v", owner, err)
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

// C1051: an erased account's offering leaves the directory: it is retired,
// its listing and pay_to are blanked and its signed revisions lose their
// bytes, while a paid call it has not answered refuses the erasure.
func TestEraseOfferings(t *testing.T) {
	s := offeringStore(t, &fakeFacilitator{})
	key := keyFor(203)
	a := keyID(key)
	provider(t, s, key, "erase-seller")
	publish(t, s, key, listing("oracle", nil))
	if list := run(t, s, Command{Operation: "offering.list"}).Data["offerings"].([]map[string]any); len(list) != 1 {
		t.Fatalf("offering.list before: %v", list)
	}
	if _, err := s.db.Exec(`INSERT INTO offering_calls(id,account,name,rev,payer,amount,network,asset,pay_to,input,input_sha256,auth_nonce,quote_hash,valid_before,state,poll_hash,created_at,claim_by)
 VALUES('call-erase',?,'oracle',1,'0xpayer',5000000,'n','a',?,'{}','h','nonce-erase','q',1,'paid','p',1,1)`, a, offeringWallet); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OperatorEraseAccount(testContext, a, "req-off", true); err == nil || !strings.Contains(err.Error(), "offering calls") {
		t.Fatalf("paid call: %v", err)
	}
	if _, err := s.db.Exec("UPDATE offering_calls SET state='answered' WHERE id='call-erase'"); err != nil {
		t.Fatal(err)
	}
	r, err := s.OperatorEraseAccount(testContext, a, "req-off", true)
	if err != nil {
		t.Fatal(err)
	}
	if eraseCount(r, "offerings", "retired") != 1 || eraseCount(r, "offering_revisions", "blanked") != 1 {
		t.Fatalf("counts %+v", r.Counts)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM offerings WHERE account=? AND (state<>'retired' OR listing<>'{}' OR pay_to<>'')", a); n != 0 {
		t.Fatal("offering kept its listing or pay_to")
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM offering_revisions WHERE account=? AND (listing<>'{}' OR signature<>'' OR signed_payload<>'' OR instr(listing,?)>0)", a, offeringWallet); n != 0 {
		t.Fatal("revision kept its signed listing")
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM offering_calls WHERE id='call-erase' AND pay_to=?", offeringWallet); n != 1 {
		t.Fatal("the paid call lost its own pay_to")
	}
	if list := run(t, s, Command{Operation: "offering.list"}).Data["offerings"].([]map[string]any); len(list) != 0 {
		t.Fatalf("offering.list after: %v", list)
	}
	got := run(t, s, Command{Operation: "offering.get", Target: a + "/oracle"}).Data["offering"].(map[string]any)
	if got["state"] != "retired" || got["pay_to"] != "" || got["title"] != "" {
		t.Fatalf("offering.get after: %v", got)
	}
	again, err := s.OperatorEraseAccount(testContext, a, "req-off-2", true)
	if err != nil {
		t.Fatal(err)
	}
	if eraseCount(again, "offerings", "retired") != 0 || eraseCount(again, "offering_revisions", "blanked") != 0 {
		t.Fatalf("second run %+v", again.Counts)
	}
}

// eraseAccountColumns are the columns that name an account or one of its
// keys; every table holding one is either a store EraseAccount handles or
// listed in eraseKeptTables with the reason it stays. C1124 added the other
// names the schema gives an account or key column (actor: requests, audit).
var eraseAccountColumns = []string{"account", "actor", "agent", "author", "caller", "created_by", "first_agent", "from_account", "invitee",
	"owner", "public_key", "requester", "sponsor", "target_account", "to_account", "used_by", "voter_account", "worker"}

// eraseContentKeyed is whether a table without an account column is keyed
// by a content hash (C1134): a single primary key column named sha256, hash
// or digest, or ending in one of those. Such a table can hold what an
// account supplied (runs_code holds a run's source), reached through the
// account's rows that name the hash, so it is handled or kept on purpose
// like an account table.
func eraseContentKeyed(pk []string) bool {
	if len(pk) != 1 {
		return false
	}
	for _, name := range []string{"sha256", "hash", "digest"} {
		if pk[0] == name || strings.HasSuffix(pk[0], "_"+name) {
			return true
		}
	}
	return false
}

// eraseHandledElsewhere are tables an erasure touches without a count of
// their own.
var eraseHandledElsewhere = map[string]string{
	"notary_receipts": "EraseToolHistory unnames the account's receipts",
	"erasure_log":     "the erasure's own record",
	"tlog_withheld":   "the erasure's withheld log leaves (C1126): indexes and operations, no content",
}

// eraseKeptTables are the tables with an account column an erasure leaves,
// and why. A new table lands here only with a reason; anything holding the
// account's words or personal data belongs in EraseAccount instead.
var eraseKeptTables = map[string]string{
	"account_breakers":        "abuse breakers on the account: numbers and a server reason",
	"agent_days":              "activity counts per day, numbers only",
	"agent_funnel":            "funnel stages, numbers and labels only",
	"apps":                    "an app's public name and homepage: a product, its house agents still run",
	"conversations":           "conversations it opened: other members' messages and the signed opening (keys and a signature) rest on them",
	"credit_topups":           "payments with wallets and transaction hashes (accounting)",
	"delegations":             "signed key delegations: public keys only, revoked with the parent's keys",
	"endorsement_records":     "votes and vouches as weights (other accounts' standing rests on them)",
	"honors":                  "operator-awarded public titles",
	"identity_house":          "an identity's app: a link to another account, no content",
	"leases":                  "short-lived room locks that expire on their own",
	"ledger_entries":          "credit ledger (accounting)",
	"ledger_holds":            "credit holds (accounting; held credits refuse an erasure)",
	"ledger_lots":             "credit lots (accounting)",
	"ledger_transfers":        "credit transfers between accounts (accounting)",
	"lever_log":               "operator lever records: the actor is the operator",
	"levers":                  "operator levers: the actor is the operator",
	"members":                 "room membership, no content",
	"moderation_decisions":    "moderation decisions (an erasure does not end a sanction)",
	"moderation_domains":      "moderation's record of a link domain: its state and the first key seen with it",
	"notary_public_keys":      "the notary's own keys",
	"oauth_assertions_used":   "content-keyed: hashes of spent sign-in assertions and their expiry, no content",
	"oauth_codes":             "one-time sign-in codes that expire within minutes; the sign-ins are revoked",
	"oauth_exchanged_revoked": "content-keyed: hashes of revoked exchanged tokens and their expiry, no content",
	"oauth_refresh_used":      "content-keyed: hashes of spent refresh tokens, their family and time, no content",
	"oauth_signing_keys":      "the server's own signing keys",
	"offering_calls":          "the other party's record of a paid call, with its own pay_to",
	"params":                  "operator parameter versions: the actor is the operator",
	"post_mentions":           "other members' posts that mention the account",
	"private_read_grants":     "signed read grants: public keys only, revoked with the owner's keys",
	"proof_bonds":             "bonds with wallets and transaction hashes (accounting)",
	"quota":                   "byte quota per day, numbers only",
	"reports":                 "reports it filed, with their reasons: moderation records (an erasure does not end a sanction)",
	"room_invites":            "invites: a secret's hash, the room and keys; other members' membership rests on them",
	"room_moderation_log":     "moderation records",
	"room_moderators":         "room roles; the keys are revoked, so they cannot act",
	"rooms":                   "rooms it created: other members' messages live in them; its keys are revoked, so it cannot act as owner",
	"runs_network_blocks":     "moderation records on code runs",
	"runs_reviews":            "moderation reviews of code by hash",
	"runs_usage":              "run counts per day, numbers only",
	"sanctions":               "sanctions (an erasure does not end a sanction)",
	"seal_epochs":             "sealed rooms' signed key epochs, which other members' messages rest on",
	"spend_limits":            "limits on revoked tokens, numbers only",
	"standing_assessments":    "proof assessments, values and roots only as keyed hashes (erasedMAC): one outside account never makes a second identity (login.go)",
	"standing_challenges":     "proof challenges that expire within the challenge window",
	"tier_grant_log":          "operator grant records (accounting)",
	"tier_grants":             "operator grants (accounting)",
	"trust_current":           "trust scores, numbers only",
	"trust_dividends":         "sponsorship dividends, numbers only",
	"trust_penalties":         "trust penalties (sanctions)",
	"trust_penalty_log":       "trust penalty records (sanctions)",
	"trust_scores":            "trust scores, numbers only",
	"trust_sponsorships":      "sponsorships as weights (other accounts' standing rests on them)",
	"votes":                   "votes as weights (other accounts' standing rests on them)",
	"vouches":                 "vouches as weights (other accounts' standing rests on them)",
	"wakeup_notices":          "fired wakeup notices: ids and times, cancelled with the wakeups",
	"webhook_rates":           "delivery counts per hour, numbers only",
	"work_review_fees":        "review fees (accounting; fees in flight refuse an erasure)",
	"work_rewards":            "work rewards (accounting; rewards in flight refuse an erasure)",
	"work_transitions":        "signed work transitions (the other party's record of the work)",
	"work_usdc":               "USDC payouts and their transaction hashes (accounting)",
	"x402_payments":           "payments with wallets and transaction hashes (accounting)",
	"x402_summary_screens":    "content-keyed: screening verdicts on outside catalogue summaries by hash, no content",
}

// C1051: every table with an account column is erased or kept on purpose,
// so a new store cannot be missed. The handled stores are the ones a real
// erasure names in its counts.
func TestEraseCoversEveryAccountTable(t *testing.T) {
	s := openTest(t, Config{})
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	counts, err := s.EraseAccount(testContext, tx, keyID(keyFor(204)), 1, EraseEvidence{})
	_ = tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	handled := map[string]bool{}
	for _, c := range counts {
		handled[c.Store] = true
	}
	for table := range eraseHandledElsewhere {
		handled[table] = true
	}
	cols := map[string]bool{}
	for _, c := range eraseAccountColumns {
		cols[c] = true
	}
	rows, err := s.db.Query("SELECT m.name,p.name,p.pk FROM sqlite_master m, pragma_table_info(m.name) p WHERE m.type='table' ORDER BY m.name,p.pk")
	if err != nil {
		t.Fatal(err)
	}
	tables := map[string]bool{}
	all := map[string]bool{}
	pks := map[string][]string{}
	for rows.Next() {
		var table, col string
		var pk int
		if err = rows.Scan(&table, &col, &pk); err != nil {
			t.Fatal(err)
		}
		all[table] = true
		if cols[col] {
			tables[table] = true
		}
		if pk > 0 {
			pks[table] = append(pks[table], col)
		}
	}
	rows.Close()
	// C1134: a content-keyed table with no account column is reached through
	// the account's rows that name the hash, and is covered the same way.
	for table := range all {
		if !tables[table] && eraseContentKeyed(pks[table]) {
			tables[table] = true
		}
	}
	if !tables["runs_code"] || !handled["runs_code"] {
		t.Errorf("runs_code (a run's source by hash): found as content-keyed %v, handled %v", tables["runs_code"], handled["runs_code"])
	}
	// C1124: the stored write answers and the audit trail name the account
	// as actor, and are erased.
	for _, want := range []string{"requests", "audit"} {
		if !tables[want] || !handled[want] {
			t.Errorf("%s: found by an account column %v, handled %v", want, tables[want], handled[want])
		}
	}
	for table := range tables {
		_, kept := eraseKeptTables[table]
		switch {
		case handled[table] && kept:
			t.Errorf("%s is erased and also listed as kept: drop it from eraseKeptTables", table)
		case !handled[table] && !kept:
			t.Errorf("%s has an account column or is content-keyed, but account erasure neither handles it nor lists it in eraseKeptTables with a reason", table)
		}
	}
	for table, why := range eraseKeptTables {
		if !tables[table] || why == "" {
			t.Errorf("eraseKeptTables lists %s, which has no account column and is not content-keyed (or no reason)", table)
		}
	}
	for table := range handled {
		if !all[table] && table != "pastes" {
			t.Errorf("erasure names store %s, which is no table", table)
		}
	}
}

// C1124: an erasure removes the account's stored write answers under every
// namespace it writes in (its account, a worker key's grant, its private-read
// controls), so an identity link's value or a wake-up's private room no
// longer survives in requests, and redacts the values it supplied in its
// audit rows (as actor or as target), keeping the rows. Another account's
// rows stay.
func TestEraseRemovesStoredAnswersAndAuditValues(t *testing.T) {
	s := openWakeTest(t, "wakeup")
	alice, bob, worker := keyFor(211), keyFor(212), keyFor(213)
	a, b := keyID(alice), keyID(bob)
	const link, den, handle = "https://alice.example.org/c1124", "alice-den-c1124", "alice-c1124"
	run(t, s, signed(alice, Command{Operation: "agent.register", Handle: handle, RequestID: "reg-1"}))
	run(t, s, signed(bob, Command{Operation: "agent.register", Handle: "bob-c1124", RequestID: "reg-1"}))
	run(t, s, signed(alice, Command{Operation: "identity.link", Data: linkJSON("url", link), RequestID: "link-1"}))
	run(t, s, signed(alice, Command{Operation: "room.create", Room: den, Visibility: "private", RequestID: "den-1"}))
	run(t, s, svcCall(alice, "wakeup", "schedule", map[string]any{"key": "den", "on": "room", "room": den}, 1, "wake-1"))
	run(t, s, signed(alice, Command{Operation: "room.create", Room: "grant-c1124", Visibility: "public", RequestID: "grant-room-1"}))
	g := enroll(t, s, alice, worker, "grant-c1124", 3600, 1<<20)
	run(t, s, childCommand(s, worker, g, Command{Operation: "post", Room: "grant-c1124", Visibility: "public", Text: "worker words", RequestID: "w-1"}))
	run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", Text: "bob words", RequestID: "b-1"}))
	for _, q := range []string{
		"INSERT INTO requests(actor,request_key,digest,result,created_at) VALUES('private-control:'||?1,'id:pr-1','d','{\"room\":\"" + den + "\"}',1)",
		"INSERT INTO audit(operation,actor,target,detail,created_at) VALUES('identity.witness',?2,?1,'identity link witnessed verified: url " + link + "',1)",
	} {
		if _, err := s.db.Exec(q, a, b); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	namespaces := "actor IN (?1,'private-control:'||?1,'delegate:'||?2)"
	own := sqlCount(t, s, "SELECT count(*) FROM requests WHERE "+namespaces, a, keyID(worker))
	for _, leak := range []string{link, den} {
		if sqlCount(t, s, "SELECT count(*) FROM requests WHERE instr(result,?)>0", leak) == 0 {
			t.Fatalf("precondition: no stored answer holds %q", leak)
		}
	}
	if sqlCount(t, s, "SELECT count(*) FROM requests WHERE actor='delegate:'||?", keyID(worker)) == 0 {
		t.Fatal("precondition: the worker key's post stored no answer")
	}
	bobRequests := sqlCount(t, s, "SELECT count(*) FROM requests WHERE actor=?", b)
	bobAuditQuery := "SELECT count(*) FROM audit WHERE actor=?1 AND target<>?2 AND detail<>?3"
	bobAudit := sqlCount(t, s, bobAuditQuery, b, a, ErasedDetail)

	r, err := s.OperatorEraseAccount(testContext, a, "req-c1124", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := eraseCount(r, "requests", "removed"); got != own {
		t.Fatalf("requests count %d, want %d: %+v", got, own, r.Counts)
	}
	if eraseCount(r, "audit", "redacted") < 3 {
		t.Fatalf("audit count: %+v", r.Counts)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM requests WHERE "+namespaces, a, keyID(worker)); n != 0 {
		t.Fatalf("%d stored answers left for the erased account", n)
	}
	for _, leak := range []string{link, den, handle} {
		if n := sqlCount(t, s, "SELECT count(*) FROM requests WHERE instr(result,?)>0", leak); n != 0 {
			t.Errorf("a stored answer still holds %q", leak)
		}
		if n := sqlCount(t, s, "SELECT count(*) FROM audit WHERE instr(detail,?)>0", leak); n != 0 {
			t.Errorf("an audit detail still holds %q", leak)
		}
	}
	for _, op := range []string{"identity.link", "agent.register", "identity.witness"} {
		if n := sqlCount(t, s, "SELECT count(*) FROM audit WHERE operation=?1 AND detail=?2 AND (actor=?3 OR target=?3)", op, ErasedDetail, a); n != 1 {
			t.Errorf("%s: %d redacted rows, want the row kept and redacted", op, n)
		}
	}
	if sqlCount(t, s, "SELECT count(*) FROM audit WHERE operation='account.erase' AND target=? AND detail LIKE '%receipt %'", a) != 1 {
		t.Fatal("the erasure's own audit row lost its receipt hash")
	}
	if sqlCount(t, s, "SELECT count(*) FROM requests WHERE actor=?", b) != bobRequests || sqlCount(t, s, bobAuditQuery, b, a, ErasedDetail) != bobAudit {
		t.Fatal("another account's stored answers or audit rows changed")
	}
	if former, err := formerHandles(testContext, s.db, a); err != nil || len(former) != 0 {
		t.Fatalf("former handles after erase: %v %v", former, err)
	}
	again, err := s.OperatorEraseAccount(testContext, a, "req-c1124-2", true)
	if err != nil {
		t.Fatal(err)
	}
	if eraseCount(again, "requests", "removed") != 0 || eraseCount(again, "audit", "redacted") != 0 {
		t.Fatalf("second run: %+v", again.Counts)
	}
}

// C1124: stored write answers are kept for good by default: the hourly
// prune deletes nothing until the operator sets a retention period (params
// namespace retention, 0 to RequestRetentionDaysMax days). With one set, it
// deletes the older answers in batches and keeps younger ones, and a signed
// exact retry of a pruned write is refused as stale, never run a second time.
func TestPruneRequestsKeepsTheRetryWindow(t *testing.T) {
	const days = 30
	s := openTest(t, Config{})
	alice := keyFor(214)
	register(t, s, alice)
	post := signed(alice, Command{Operation: "post", Room: "lobby", Text: "posted once", RequestID: "once"})
	first := run(t, s, post).Receipt.ID
	if again := run(t, s, post).Receipt; again.ID != first || !again.Duplicate {
		t.Fatalf("a retry in the window: %+v", again)
	}
	cutoff := testTime - days*86400
	if _, err := s.db.Exec(`WITH RECURSIVE n(v) AS (VALUES(0) UNION ALL SELECT v+1 FROM n WHERE v+1<?)
INSERT INTO requests(actor,request_key,digest,result,created_at) SELECT 'anon:old','id:'||v,'d','{}',? FROM n`, signalPruneBatch+1, cutoff-1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO requests(actor,request_key,digest,result,created_at) VALUES('anon:young','id:y','d','{}',?)", cutoff); err != nil {
		t.Fatal(err)
	}
	all := sqlCount(t, s, "SELECT count(*) FROM requests")
	if n, err := s.PruneWriteSignals(testContext); err != nil || sqlCount(t, s, "SELECT count(*) FROM requests") != all {
		t.Fatalf("the default (keep for good) pruned: %d %v", n, err)
	}
	for _, bad := range []string{`{"schema":1,"request_retention_days":-1}`, `{"schema":1,"request_retention_days":3651}`, `{"schema":1}`, `{"schema":2,"request_retention_days":30}`} {
		if _, err := s.SetAllowanceParams(testContext, RetentionParamsNamespace, []byte(bad), "test", 0); err == nil {
			t.Errorf("retention params %s accepted", bad)
		}
	}
	if _, err := s.SetAllowanceParams(testContext, RetentionParamsNamespace, []byte(`{"schema":1,"request_retention_days":30}`), "test", 0); err != nil {
		t.Fatal(err)
	}
	kept := sqlCount(t, s, "SELECT count(*) FROM requests WHERE actor IN ('anon:young',?)", keyID(alice))
	n, err := s.pruneRequests(testContext)
	if err != nil || n != signalPruneBatch+1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if sqlCount(t, s, "SELECT count(*) FROM requests WHERE actor='anon:old'") != 0 || sqlCount(t, s, "SELECT count(*) FROM requests WHERE actor IN ('anon:young',?)", keyID(alice)) != kept {
		t.Fatal("the prune kept an old answer or took a young one")
	}
	// Past the window the hourly maintenance takes the post's answer too.
	s.now = func() time.Time { return time.Unix(testTime+days*86400+1, 0) }
	if _, err = s.PruneWriteSignals(testContext); err != nil {
		t.Fatal(err)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM requests WHERE actor=?", keyID(alice)); n != 0 {
		t.Fatalf("%d answers kept past the window", n)
	}
	fails(t, s, post, "stale_signature")
	if n := sqlCount(t, s, "SELECT count(*) FROM events WHERE account=? AND text='posted once'", keyID(alice)); n != 1 {
		t.Fatalf("the pruned write ran again: %d posts", n)
	}
}

// C1134: runs_code holds a run's source by its hash, with no account column.
// An erasure blanks the source of the account's runs, unless an account not
// erased ran the same code: bob's shared code stays, code shared only with
// an account erased before goes. Hash and language stay; the receipt counts
// the blanked rows; a second run counts none.
func TestEraseBlanksRunSourceCode(t *testing.T) {
	s := openTest(t, Config{})
	alice, bob, carol := keyFor(221), keyFor(222), keyFor(223)
	a, b, c := keyID(alice), keyID(bob), keyID(carol)
	for _, k := range []ed25519.PrivateKey{alice, bob, carol} {
		register(t, s, k)
	}
	code := map[string]string{}
	n := 0
	ran := func(account, name string) {
		t.Helper()
		src := "export function run() { return '" + name + " c1134' }"
		code[name] = sha256Hex([]byte(src))
		n++
		for _, q := range []struct {
			query string
			args  []any
		}{
			{"INSERT OR IGNORE INTO runs_code(sha256,language,code,first_seen) VALUES(?,'javascript',?,1)", []any{code[name], src}},
			{"INSERT INTO runs_log(run_id,account,request_key,created_at,language,code_sha256,input,input_sha256,network_requested,cpu_ms_limit,wall_ms_limit,status) VALUES(?,?,?,1,'javascript',?,'null','h',0,1,1,'ok')",
				[]any{fmt.Sprintf("run-%d", n), account, fmt.Sprintf("id:r%d", n), code[name]}},
		} {
			if _, err := s.db.Exec(q.query, q.args...); err != nil {
				t.Fatalf("%s: %v", q.query, err)
			}
		}
	}
	ran(a, "alice-own")
	ran(a, "shared-bob")
	ran(b, "shared-bob")
	ran(a, "shared-carol")
	ran(c, "shared-carol")
	ran(c, "carol-own")
	ran(b, "bob-own")
	source := func(name string) string {
		t.Helper()
		var src, lang string
		if err := s.db.QueryRow("SELECT code,language FROM runs_code WHERE sha256=?", code[name]).Scan(&src, &lang); err != nil || lang != "javascript" {
			t.Fatalf("runs_code %s: %q %v (the row and its language stay)", name, lang, err)
		}
		return src
	}

	// Carol first: her own code goes, the code alice also ran stays.
	rc, err := s.OperatorEraseAccount(testContext, c, "req-c1134-c", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := eraseCount(rc, "runs_code", "blanked"); got != 1 {
		t.Fatalf("carol's runs_code count %d, want 1: %+v", got, rc.Counts)
	}
	if source("carol-own") != "" || source("shared-carol") == "" {
		t.Fatal("carol's erasure: her own source must go, code alice also ran must stay")
	}

	dry, err := s.OperatorEraseAccount(testContext, a, "req-c1134-a", false)
	if err != nil || eraseCount(dry, "runs_code", "blanked") != 2 || source("alice-own") == "" {
		t.Fatalf("preview: %+v %v", dry.Counts, err)
	}
	r, err := s.OperatorEraseAccount(testContext, a, "req-c1134-a", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := eraseCount(r, "runs_code", "blanked"); got != 2 {
		t.Fatalf("alice's runs_code count %d, want 2: %+v", got, r.Counts)
	}
	for name, want := range map[string]bool{"alice-own": false, "shared-carol": false, "shared-bob": true, "bob-own": true, "carol-own": false} {
		if got := source(name) != ""; got != want {
			t.Errorf("%s: source kept %v, want %v", name, got, want)
		}
	}
	if sqlCount(t, s, "SELECT count(*) FROM runs_code WHERE instr(code,'alice-own')>0 OR instr(code,'carol')>0") != 0 {
		t.Fatal("an erased account's run source survived")
	}
	again, err := s.OperatorEraseAccount(testContext, a, "req-c1134-a2", true)
	if err != nil || eraseCount(again, "runs_code", "blanked") != 0 {
		t.Fatalf("second run: %+v %v", again.Counts, err)
	}
}
