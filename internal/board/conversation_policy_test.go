package board

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func setPolicy(t *testing.T, s *Store, key ed25519.PrivateKey, policy string) {
	t.Helper()
	run(t, s, signed(key, Command{Operation: "messaging.policy.set", Data: `{"schema":1,"inbound_policy":` + policy + `}`}))
}

// decide runs recipient's inbound policy for sender in its own transaction,
// with a deadline: the evaluator reads only through that transaction, so
// any use of the pool would wait on the one connection until the deadline.
func decide(t *testing.T, s *Store, sender, recipient ed25519.PrivateKey, postage int64, trustOn bool) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(testContext, 2*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// The sender's custody, as authentication reads it into actor.hosted.
	var custody string
	if err = tx.QueryRowContext(ctx, "SELECT custody FROM identities WHERE id=?", keyID(sender)).Scan(&custody); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	outcome, err := inboundDecision(ctx, tx, inboundCase{Sender: keyID(sender), SenderHosted: custody == "hosted", Recipient: keyID(recipient), Postage: postage, Now: s.now().Unix(), Ledger: true, Trust: trustOn})
	if err != nil || time.Since(start) > time.Second {
		t.Fatalf("inboundDecision: %v after %v", err, time.Since(start))
	}
	return outcome
}

// Each condition of §3.4, alone in a custom rule (deliver when it holds,
// drop otherwise), against a sender for whom it holds and one for whom it
// does not.
func TestInboundConditions(t *testing.T) {
	c := updatesConfig()
	c.Features.VoteRecords = true
	s := openTest(t, c)
	r, yes, no := keyFor(1), keyFor(2), keyFor(3)
	registerAll(t, s, r, yes, no)
	rule := func(condition string) {
		t.Helper()
		setPolicy(t, s, r, `{"schema":1,"rules":[{"if":`+condition+`,"then":"deliver"}],"default":"drop"}`)
	}
	check := func(label string, trustOn bool) {
		t.Helper()
		if got := decide(t, s, yes, r, 0, trustOn); got != outcomeDeliver {
			t.Fatalf("%s: the sender it holds for got %s", label, got)
		}
		if got := decide(t, s, no, r, 0, trustOn); got != outcomeDrop {
			t.Fatalf("%s: the sender it does not hold for got %s", label, got)
		}
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}

	// contact: a DM both are active in.
	dm := convRoom("cond-contact")
	openConv(t, s, yes, dm, "dm", r)
	run(t, s, respond(r, dm, "accept"))
	rule(`{"contact":true}`)
	check("contact", false)

	// shares_room private: a co-member of one of R's private rooms.
	run(t, s, signed(r, Command{Operation: "room.create", Room: "cond-private", Visibility: "private", Members: []string{keyID(yes)}}))
	rule(`{"shares_room":{"private":true}}`)
	check("shares_room private", false)

	// shares_room public_days: both posted in one public room (not the
	// lobby) within the window.
	run(t, s, signed(r, Command{Operation: "post", Room: "cond-public", Text: "here"}))
	run(t, s, signed(yes, Command{Operation: "post", Room: "cond-public", Text: "me too"}))
	run(t, s, signed(no, Command{Operation: "post", Room: "lobby", Text: "lobby only"}))
	run(t, s, signed(r, Command{Operation: "post", Room: "lobby", Text: "lobby too"}))
	rule(`{"shares_room":{"private":false,"public_days":30}}`)
	check("shares_room public_days", false)

	// vouched: R vouched S (hops 0), or someone R vouched did (hops 1).
	middle := keyFor(4)
	register(t, s, middle)
	vouch := func(from, to ed25519.PrivateKey) {
		run(t, s, signed(from, Command{Operation: "vouch", Target: keyID(to), Data: `{"schema":1,"value":1}`}))
	}
	vouch(r, middle)
	vouch(middle, yes)
	rule(`{"vouched":{"hops":1}}`)
	check("vouched hops 1", false)
	rule(`{"vouched":{"hops":0}}`)
	if got := decide(t, s, yes, r, 0, false); got != outcomeDrop {
		t.Fatalf("hops 0 took a second-hand vouch: %s", got)
	}
	vouch(r, yes)
	check("vouched hops 0", false)

	// trust_at_least: false while trust is off; then the latest run's
	// collateral against the proven line or a number.
	exec("INSERT INTO trust_runs(id,state,as_of,params_version,started_at,finished_at,inputs) VALUES(1,'done',?,1,?,?,'{}')", testTime, testTime, testTime)
	for _, row := range []struct {
		key        ed25519.PrivateKey
		collateral int64
	}{{yes, 500}, {no, 10}} {
		exec("INSERT INTO trust_scores(run_id,account,root,proof_collateral,flow_a,flow_b,flow,collateral,tier,weight_ppm,parts) VALUES(1,?,?,0,0,0,0,?,3,1000000,'{}')", keyID(row.key), keyID(row.key), row.collateral)
		exec("INSERT INTO trust_current(account,run_id) VALUES(?,1)", keyID(row.key))
	}
	rule(`{"trust_at_least":"low"}`)
	if got := decide(t, s, yes, r, 0, false); got != outcomeDrop {
		t.Fatalf("trust off still matched: %s", got)
	}
	check(`trust_at_least "low"`, true)
	rule(`{"trust_at_least":100}`)
	check("trust_at_least 100", true)

	// key_age_at_least: the account's first key is old enough.
	exec("UPDATE identities SET created_at=? WHERE id=?", testTime-8*86400, keyID(yes))
	rule(`{"key_age_at_least":7}`)
	check("key_age_at_least", false)

	// has_profile: an unexpired profile.
	run(t, s, signed(yes, Command{Operation: "agent.profile.publish", Data: `{"schema":1,"description":"a profile","capabilities":["chat"],"availability":"available"}`}))
	rule(`{"has_profile":true}`)
	check("has_profile", false)

	// custody: who holds the sender's key.
	exec("UPDATE identities SET custody='hosted' WHERE id=?", keyID(yes))
	rule(`{"custody":["hosted"]}`)
	check("custody", false)
	exec("UPDATE identities SET custody='self' WHERE id=?", keyID(yes))

	// linked: a verified identity link of the kind (and value).
	exec("INSERT INTO identity_links(agent,kind,value,state,created_at) VALUES(?,'domain','example.com','verified',?)", keyID(yes), testTime)
	exec("INSERT INTO identity_links(agent,kind,value,state,created_at) VALUES(?,'domain','example.net','claimed',?)", keyID(no), testTime)
	rule(`{"linked":{"kind":"domain"}}`)
	check("linked", false)
	rule(`{"linked":{"kind":"domain","value":"example.org"}}`)
	if got := decide(t, s, yes, r, 0, false); got != outcomeDrop {
		t.Fatalf("linked value: %s", got)
	}

	// postage_at_least: what the sender attached, only with the ledger on.
	rule(`{"postage_at_least":5}`)
	if decide(t, s, yes, r, 5, false) != outcomeDeliver || decide(t, s, yes, r, 4, false) != outcomeDrop {
		t.Fatal("postage_at_least")
	}

	// any and all nest.
	rule(`{"all":[{"has_profile":true},{"any":[{"custody":["hosted"]},{"key_age_at_least":7}]}]}`)
	check("all/any", false)

	// Refused shapes, each with the one error.
	for _, bad := range []string{
		`{"schema":2}`, `{"schema":1,"preset":"friends"}`, `{"schema":1,"default":"maybe"}`,
		`{"schema":1,"rules":[{"if":{},"then":"deliver"}]}`,
		`{"schema":1,"rules":[{"if":{"contact":true,"has_profile":true},"then":"deliver"}]}`,
		`{"schema":1,"rules":[{"if":{"any":[{"any":[{"any":[{"contact":true}]}]}]},"then":"deliver"}]}`,
		`{"schema":1,"rules":[{"if":{"any":[` + strings.Repeat(`{"contact":true},`, PolicyGroupMax) + `{"contact":true}]},"then":"deliver"}]}`,
		`{"schema":1,"rules":[{"if":{"vouched":{"hops":2}},"then":"deliver"}]}`,
		`{"schema":1,"rules":[{"if":{"shares_room":{"private":false}},"then":"deliver"}]}`,
		`{"schema":1,"rules":[{"if":{"shares_room":{"private":true,"public_days":91}},"then":"deliver"}]}`,
		`{"schema":1,"rules":[{"if":{"custody":["other"]},"then":"deliver"}]}`,
		`{"schema":1,"rules":[{"if":{"linked":{"kind":"phone"}},"then":"deliver"}]}`,
		`{"schema":1,"rules":[{"if":{"contact":true},"then":"accept"}]}`,
		`{"schema":1,"rules":[` + strings.Repeat(`{"if":{"contact":true},"then":"deliver"},`, PolicyRulesMax) + `{"if":{"contact":true},"then":"deliver"}]}`,
		`{"schema":1,"postage":{"amount":-1,"advertise":false}}`,
		`{"schema":1,"unknown":true}`,
	} {
		fails(t, s, signed(r, Command{Operation: "messaging.policy.set", Data: `{"schema":1,"inbound_policy":` + bad + `}`}), "invalid_messaging_policy")
	}
	for _, bad := range []string{`{"schema":1,"inbound":{"mode":"cloud"}}`, `{"schema":1,"inbound":{"threshold":0}}`, `{"schema":1,"inbound":{"categories":["spam"]}}`, `{"schema":1,"outbound":{"leak":"all"}}`, `{"schema":1,"share_read_markers":"yes"}`, `{"schema":2}`} {
		fails(t, s, signed(r, Command{Operation: "messaging.policy.set", Data: bad}), "invalid_messaging_policy")
	}
}

// The presets (§3.4) and the order: block list, allow list, rules, default.
func TestInboundPresetsAndOrder(t *testing.T) {
	c := updatesConfig()
	c.Features.VoteRecords = true
	s := openTest(t, c)
	r, stranger, voucher, vouched, friend := keyFor(1), keyFor(2), keyFor(3), keyFor(4), keyFor(5)
	registerAll(t, s, r, stranger, voucher, vouched, friend)
	run(t, s, signed(r, Command{Operation: "vouch", Target: keyID(voucher), Data: `{"schema":1,"value":1}`}))
	run(t, s, signed(voucher, Command{Operation: "vouch", Target: keyID(vouched), Data: `{"schema":1,"value":1}`}))
	dm := convRoom("presets-friend")
	openConv(t, s, friend, dm, "dm", r)
	run(t, s, respond(r, dm, "accept"))

	// open, the default: vouched and contacts deliver, a stranger is asked,
	// nothing is dropped.
	for _, c := range []struct {
		key  ed25519.PrivateKey
		want string
	}{{stranger, outcomeRequest}, {vouched, outcomeDeliver}, {friend, outcomeDeliver}} {
		if got := decide(t, s, c.key, r, 0, false); got != c.want {
			t.Fatalf("open: %s, want %s", got, c.want)
		}
	}
	// known: the same deliveries; a fresh key is dropped; an old key with a
	// profile, or the advertised postage, is asked.
	setPolicy(t, s, r, `{"schema":1,"preset":"known","postage":{"amount":20,"advertise":true}}`)
	if decide(t, s, stranger, r, 0, false) != outcomeDrop || decide(t, s, vouched, r, 0, false) != outcomeDeliver || decide(t, s, stranger, r, 20, false) != outcomeRequest {
		t.Fatal("known: fresh, vouched, postage")
	}
	if _, err := s.db.Exec("UPDATE identities SET created_at=? WHERE id=?", testTime-8*86400, keyID(stranger)); err != nil {
		t.Fatal(err)
	}
	run(t, s, signed(stranger, Command{Operation: "agent.profile.publish", Data: `{"schema":1,"description":"old and described","capabilities":["chat"],"availability":"available"}`}))
	if got := decide(t, s, stranger, r, 0, false); got != outcomeRequest {
		t.Fatalf("known: an old key with a profile: %s", got)
	}
	if a := run(t, s, Command{Operation: "agent.get", Target: keyID(r)}).Agent.Messaging; a.Preset != presetKnown || a.Postage != 20 || a.Settings != nil {
		t.Fatalf("public face %+v", a)
	}
	// closed: contacts and the allow list deliver, everyone else drops.
	setPolicy(t, s, r, fmt.Sprintf(`{"schema":1,"preset":"closed","allow":[%q]}`, keyID(stranger)))
	if decide(t, s, friend, r, 0, false) != outcomeDeliver || decide(t, s, vouched, r, 0, false) != outcomeDrop || decide(t, s, stranger, r, 0, false) != outcomeDeliver {
		t.Fatal("closed: contact, vouched, allowed")
	}
	// The block list beats the allow list; the allow list beats the rules.
	run(t, s, signed(r, Command{Operation: "messaging.policy.set", Data: fmt.Sprintf(`{"schema":1,"block":[%q]}`, keyID(stranger))}))
	if got := decide(t, s, stranger, r, 0, false); got != outcomeDrop {
		t.Fatalf("blocked and allowed: %s", got)
	}
	setPolicy(t, s, r, fmt.Sprintf(`{"schema":1,"allow":[%q],"rules":[{"if":{"contact":true},"then":"drop"}],"default":"drop"}`, keyID(friend)))
	if got := decide(t, s, friend, r, 0, false); got != outcomeDeliver {
		t.Fatalf("allowed before the rules: %s", got)
	}
	// The first matching rule wins.
	setPolicy(t, s, r, `{"schema":1,"rules":[{"if":{"contact":true},"then":"request"},{"if":{"contact":true},"then":"deliver"}],"default":"drop"}`)
	if got := decide(t, s, friend, r, 0, false); got != outcomeRequest {
		t.Fatalf("first match: %s", got)
	}
	self := run(t, s, signed(r, Command{Operation: "agent.get"})).Agent.Messaging
	if self.Preset != presetCustom || !strings.Contains(string(self.Settings), `"rules"`) || !strings.Contains(string(self.Settings), keyID(stranger)) {
		t.Fatalf("own settings %s", self.Settings)
	}
	// A preset and rules of your own in one call: the rules go first, then
	// the preset's, and its default stands; others see custom. Protections
	// ride in the same call.
	run(t, s, signed(r, Command{Operation: "messaging.policy.set", Data: fmt.Sprintf(`{"schema":1,"unblock":[%q]}`, keyID(stranger))}))
	run(t, s, signed(r, Command{Operation: "messaging.policy.set", Data: `{"schema":1,"inbound_policy":{"schema":1,"preset":"closed","rules":[{"if":{"vouched":{"hops":1}},"then":"request"}]},"inbound":{"mode":"server"}}`}))
	if decide(t, s, vouched, r, 0, false) != outcomeRequest || decide(t, s, friend, r, 0, false) != outcomeDeliver || decide(t, s, stranger, r, 0, false) != outcomeDrop {
		t.Fatal("closed with a rule: vouched asked, contact delivered, stranger dropped")
	}
	if a := run(t, s, Command{Operation: "agent.get", Target: keyID(r)}).Agent.Messaging; a.Preset != presetCustom {
		t.Fatalf("a preset with rules reads to others as %q", a.Preset)
	}
	if own := string(run(t, s, signed(r, Command{Operation: "agent.get"})).Agent.Messaging.Settings); !strings.Contains(own, `"preset":"closed"`) || !strings.Contains(own, `"mode":"server"`) {
		t.Fatalf("own settings after a preset with rules: %s", own)
	}
}

// Postage (§3.4): held from the sender on open, returned when the recipient
// accepts or never answers (the hold lapses to a refund), kept only on an
// explicit decline or block, where it moves to the recipient.
func TestPostageHeldRefundedAndKept(t *testing.T) {
	s := ledgerTest(t, LedgerOn)
	sender, accepter, decliner, silent := keyFor(1), keyFor(2), keyFor(3), keyFor(4)
	registerAll(t, s, sender, accepter, decliner, silent)
	setAnonCredit(t, s, 1, 1) // a day's credit for signed agents too
	open := func(room string, to ed25519.PrivateKey) {
		t.Helper()
		run(t, s, signed(sender, Command{Operation: "conversation.open", Room: room, Members: []string{keyID(to)}, Data: `{"schema":1,"kind":"dm","postage":7}`}))
	}
	holdState := func(room string, to ed25519.PrivateKey) string {
		t.Helper()
		var state string
		if err := s.db.QueryRow("SELECT state FROM ledger_holds WHERE service='postage' AND request_key=?", "postage:"+room+":"+keyID(to)).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	transfers := func(to ed25519.PrivateKey) int64 {
		return sqlCount(t, s, "SELECT count(*) FROM ledger_transfers WHERE from_account=? AND to_account=? AND amount=7", keyID(sender), keyID(to))
	}
	a, d, x := convRoom("postage-a"), convRoom("postage-d"), convRoom("postage-x")
	open(a, accepter)
	if holdState(a, accepter) != "held" || memberRowOf(t, s, a, accepter).PostageHold == "" {
		t.Fatal("postage is not held")
	}
	run(t, s, respond(accepter, a, "accept"))
	if holdState(a, accepter) != "refunded" || transfers(accepter) != 0 || memberRowOf(t, s, a, accepter).PostageHold != "" {
		t.Fatal("accept kept the postage")
	}
	open(d, decliner)
	run(t, s, respond(decliner, d, "decline"))
	if holdState(d, decliner) != "refunded" || transfers(decliner) != 1 {
		t.Fatal("an explicit decline did not keep the postage")
	}
	// Ignored: the hold lapses a day later and the sweeper refunds it, as it
	// would a drop's.
	open(x, silent)
	s.now = func() time.Time { return time.Unix(testTime+PostageHoldSeconds, 0) }
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	if holdState(x, silent) != "refunded" || transfers(silent) != 0 {
		t.Fatal("a lapsed hold was not refunded")
	}
	// The ledger's own limits apply: HoldsPerAccount open holds at most.
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	fails(t, s, signed(sender, Command{Operation: "conversation.open", Room: convRoom("postage-y"), Members: []string{keyID(keyFor(9))}, Data: `{"schema":1,"kind":"group","postage":-1}`}), "invalid_conversation")
}

// The inbound policy, conversation reads and writes and the inbox run inside
// the command's transaction without touching the pool: against the real
// engine, with a policy that reads every signal, each command finishes well
// inside its deadline while a health ping waits for the one connection.
func TestConversationsNeverTouchThePoolInsideATransaction(t *testing.T) {
	c := updatesConfig()
	c.Features = Features{Ledger: LedgerOn, Trust: TrustShadow}
	s := openTest(t, c)
	sender, recipient := keyFor(1), keyFor(2)
	registerAll(t, s, sender, recipient)
	setPolicy(t, s, recipient, `{"schema":1,"rules":[{"if":{"any":[{"contact":true},{"shares_room":{"private":true,"public_days":30}},{"vouched":{"hops":1}},{"trust_at_least":"low"},{"key_age_at_least":1},{"has_profile":true},{"custody":["hosted"]},{"linked":{"kind":"domain"}}]},"then":"deliver"}],"default":"request"}`)
	room := convRoom("pool")
	within := func(label string, cmd Command) Result {
		t.Helper()
		ctx, cancel := context.WithTimeout(testContext, 2*time.Second)
		defer cancel()
		health := make(chan error, 1)
		go func() {
			hctx, hcancel := context.WithTimeout(testContext, 2*time.Second)
			defer hcancel()
			health <- s.Health(hctx)
		}()
		start := time.Now()
		res, err := s.Execute(ctx, cmd, "t")
		if err != nil || time.Since(start) >= time.Second {
			t.Fatalf("%s: %v after %v; the command waited on its own connection", label, err, time.Since(start))
		}
		if err = <-health; err != nil {
			t.Fatalf("%s: concurrent health %v", label, err)
		}
		return res
	}
	within("open", openCmd(sender, room, "dm", recipient))
	within("post", signed(sender, Command{Operation: "post", Room: room, Text: "hi", Visibility: "private"}))
	within("list", signed(recipient, Command{Operation: "conversations.list", Kind: "all"}))
	within("get", signed(recipient, Command{Operation: "conversation.get", Room: room}))
	within("respond", respond(recipient, room, "accept"))
	within("get marked", signed(recipient, Command{Operation: "conversation.get", Room: room, Data: `{"schema":1,"mark_read":true,"reveal":[]}`}))
	within("updates", signed(recipient, Command{Operation: "updates.get", Target: keyID(recipient)}))
	within("policy", signed(recipient, Command{Operation: "messaging.policy.set", Data: fmt.Sprintf(`{"schema":1,"block":[%q]}`, keyID(sender))}))
	within("agent.get", signed(recipient, Command{Operation: "agent.get"}))
	within("room.policy.set", signed(sender, Command{Operation: "room.policy.set", Room: room, Data: `{"closed":true}`}))
	var e *Error
	if _, err := s.Execute(testContext, signed(sender, Command{Operation: "post", Room: room, Text: "closed", Visibility: "private"}), "t"); !errors.As(err, &e) || e.Code != "room_closed" {
		t.Fatalf("closed: %v", err)
	}
}

// The request limits are a versioned parameter namespace with a strict
// validator, and its compiled-in version is the growth-stage default.
func TestConversationParams(t *testing.T) {
	s := openTest(t, updatesConfig())
	v, err := s.AllowanceParams(testContext, ConversationParamsNamespace, -1)
	if err != nil || !v.CompiledIn || !strings.Contains(string(v.Body), `"requests_per_day":100`) || !strings.Contains(string(v.Body), `"request_fee":0`) {
		t.Fatalf("compiled-in %+v %v", v, err)
	}
	for _, bad := range []string{`{}`, `{"requests_per_day":0,"request_posts":10,"request_post_bytes":4096,"request_fee":0}`, `{"requests_per_day":100,"request_posts":10,"request_post_bytes":4096,"request_fee":0,"x":1}`} {
		if _, err := s.SetAllowanceParams(testContext, ConversationParamsNamespace, []byte(bad), "test", 0); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func(tx *sql.Tx) { _ = tx.Rollback() }(tx)
	p, err := s.conversationParams(testContext, tx, testTime)
	if err != nil || p != defaultConversationParams() {
		t.Fatalf("params %+v %v", p, err)
	}
}
