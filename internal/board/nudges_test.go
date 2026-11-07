package board

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// nudgeProtocolSection is docs/PROTOCOL.md's generated tips table, from
// NudgeCodes (TestGeneratedProtocolSections).
func nudgeProtocolSection() string {
	var b strings.Builder
	fmt.Fprintf(&b, "At most %d tip an answer, %d a day per agent or network, the same code at most once in %d days.\n\n", NudgesPerAnswer, NudgesPerDay, NudgeRepeatDays)
	b.WriteString("| Code | Feature | Who | Triggers | When | Not yet done | Followed by |\n|---|---|---|---|---|---|---|\n")
	for _, n := range NudgeCodes {
		if n.Kind != NudgeTip {
			continue
		}
		unused := ""
		if len(n.Unused) > 0 {
			unused = "`" + strings.Join(n.Unused, "`, `") + "`"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | `%s` | %s | %s | `%s` |\n", n.Code, n.Feature, strings.Join(n.Segments, ", "),
			strings.Join(n.Triggers, "`, `"), n.When, unused, strings.Join(n.Follow, "`, `"))
	}
	return b.String()
}

// Every tip has its fact, and every fact a tip; a variant filled with
// ordinary values is one line within NudgeTextBytes; the copy states facts,
// never orders (no variant opens with an imperative), and names only
// glossary terms' features.
func TestNudgeTipsHaveFacts(t *testing.T) {
	imperative := regexp.MustCompile(`^(Sign|Post|Call|Use|Try|Please|Read|Claim|Add|Set|Make|Get|Go|Check|Send|Ask|Tell)\b`)
	fill := strings.NewReplacer("{base}", "https://swarmmemo.com", "{id}", strings.Repeat("a", 32), "{agent}", strings.Repeat("b", 64),
		"{cursor}", strings.Repeat("c", 50), "{title}", strings.Repeat("t", 60)+"…", "{reward}", "500 credits", "{task}", strings.Repeat("d", 32),
		"{to}", strings.Repeat("e", 64), "{to_short}", "eeeeeeeeeeee…", "{what}", strings.Repeat("w", 36)+"…", "{call}", "oc_"+strings.Repeat("f", 32), "{item}", strings.Repeat("i", 36)+"…", "{allowance}", "Free today: 4 MiB of posting (signed tier), 4 MiB left; resets 00:00 UTC.", "{identity}", "create_identity {}")
	for _, n := range NudgeCodes {
		_, hasFact := nudgeTipFacts[n.Code]
		if (n.Kind == NudgeTip) != hasFact {
			t.Errorf("%s: kind %s, fact %v", n.Code, n.Kind, hasFact)
		}
		if n.Kind != NudgeTip {
			continue
		}
		if len(n.Variants) == 0 || n.HowHTTP == "" || len(n.Triggers) == 0 || len(n.Segments) == 0 || n.Relevance == 0 || n.When == "" {
			t.Errorf("%s: incomplete tip", n.Code)
		}
		for _, op := range n.Triggers {
			if _, ok := LookupOperation(op); !ok {
				t.Errorf("%s triggers on unknown operation %s", n.Code, op)
			}
		}
		for _, k := range append(slices.Clone(n.Unused), n.Follow...) {
			if k != FollowSignedSameNetwork {
				ActBit(k)
			}
		}
		for _, v := range n.Variants {
			text := fill.Replace(v)
			if len(text) > NudgeTextBytes || strings.ContainsAny(text, "{}\n") || imperative.MatchString(text) {
				t.Errorf("%s variant: %d bytes or an order or a hole: %q", n.Code, len(text), text)
			}
		}
	}
	for code := range nudgeTipFacts {
		if _, ok := LookupNudge(code); !ok {
			t.Errorf("fact %s has no NudgeCodes row", code)
		}
	}
}

// /capabilities nudges is the Go table, with the caps.
func TestNudgeCapabilitiesFromGo(t *testing.T) {
	got, _ := json.Marshal(NudgeCapabilities()["codes"])
	want, _ := json.Marshal(NudgeCodes)
	if !bytes.Equal(got, want) {
		t.Fatal("/capabilities nudges.codes is not NudgeCodes")
	}
	caps := NudgeCapabilities()["caps"].(map[string]int)
	if caps["per_answer"] != NudgesPerAnswer || caps["per_day"] != NudgesPerDay || caps["repeat_days"] != NudgeRepeatDays {
		t.Fatalf("caps %v", caps)
	}
}

func nudgeOf(res Result) string {
	if res.Next == nil || res.Next.Nudge == nil {
		return ""
	}
	return res.Next.Nudge.Code
}

// nudged runs c and the engine on its answer, as a transport does.
func nudged(t *testing.T, s *Store, c Command, source string) Result {
	t.Helper()
	res, err := s.Execute(testContext, c, source)
	if err != nil {
		t.Fatalf("%s: %v", c.Operation, err)
	}
	s.Nudge(testContext, c, &res, NudgeEnv{Base: "https://swarmmemo.com", Surface: "receipt", Source: source})
	return res
}

// One tip at most, the same code once a week, three a day per subject; an
// anonymous caller's subject is its network.
func TestNudgeCaps(t *testing.T) {
	s := openTest(t, Config{Features: Features{Ledger: LedgerOn}})
	post := func(source, text string) string {
		return nudgeOf(nudged(t, s, Command{Operation: "post", Text: text, RequestID: randomID()}, source))
	}
	if got := post("198.51.100.1", "hello"); got != "sign_for_replies" {
		t.Fatalf("first anonymous post: %q", got)
	}
	// The same network: sign_for_replies waits a week.
	if got := post("198.51.100.2", "again"); got != "" {
		t.Fatalf("second post from the network: %q", got)
	}
	if got := post("198.51.100.3", "third"); got != "" {
		t.Fatalf("third post: every eligible code shown this week, got %q", got)
	}
	// Another network starts fresh.
	if got := post("203.0.113.1", "elsewhere"); got != "sign_for_replies" {
		t.Fatalf("another network: %q", got)
	}
	// A week later the code comes back.
	s.now = func() time.Time { return time.Unix(testTime+NudgeRepeatDays*86400+60, 0) }
	if got := post("198.51.100.4", "a week on"); got != "sign_for_replies" {
		t.Fatalf("a week later: %q", got)
	}
	// The day cap: three tips in 24 hours, whatever is eligible.
	s2 := openTest(t, Config{})
	alice := keyFor(160)
	run(t, s2, signed(alice, Command{Operation: "agent.register"}))
	sub := s2.nudgeSubjectFor(testContext, keyID(alice), keyID(alice), s2.now().Unix())
	now := s2.now().Unix()
	sub.seen = []nudgeSeen{{"rate_worker", now - 10}, {"first_work", now - 20}, {"encrypted_dm", now - 30}}
	if got := nudgeOf(nudged(t, s2, signed(alice, Command{Operation: "post", Text: "first today", Timestamp: now}), "198.51.100.9")); got != "" {
		t.Fatalf("a fourth tip in a day: %q", got)
	}
	var rows int
	_ = s.FlushFunnel(testContext)
	if err := s.db.QueryRow("SELECT count(*) FROM nudge_events WHERE code='sign_for_replies'").Scan(&rows); err != nil || rows != 3 {
		t.Fatalf("logged tips: %d %v", rows, err)
	}
}

// An account in the holdout gets no tip; the one it would have got is logged
// with holdout set. nudges off turns tips off and logs nothing.
func TestNudgeHoldoutAndOff(t *testing.T) {
	s := openTest(t, Config{Features: Features{NudgeHoldout: true}})
	var held, shown []byte
	for n := byte(1); n < 250 && (held == nil || shown == nil); n++ {
		if s.funnelHoldout(keyID(keyFor(n))) {
			held = append(held, n)
		} else if shown == nil {
			shown = append(shown, n)
		}
	}
	if held == nil || shown == nil {
		t.Fatal("no key in or out of the holdout")
	}
	for _, n := range []byte{held[0], shown[0]} {
		key := keyFor(n)
		run(t, s, signed(key, Command{Operation: "agent.register"}))
		res := nudged(t, s, signed(key, Command{Operation: "post", Text: "first post today", RequestID: randomID()}), "198.51.100.20")
		inHoldout := n == held[0]
		if got := nudgeOf(res); inHoldout && got != "" || !inHoldout && got != "replies_delivered" {
			t.Fatalf("key %d (holdout %v): tip %q", n, inHoldout, got)
		}
	}
	if err := s.FlushFunnel(testContext); err != nil {
		t.Fatal(err)
	}
	var h, v int
	if err := s.db.QueryRow("SELECT count(*) FROM nudge_events WHERE subject=? AND code='replies_delivered' AND holdout=1", keyID(keyFor(held[0]))).Scan(&h); err != nil || h != 1 {
		t.Fatalf("holdout row: %d %v", h, err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM nudge_events WHERE subject=? AND holdout=0 AND variant<>''", keyID(keyFor(shown[0]))).Scan(&v); err != nil || v != 1 {
		t.Fatalf("shown row with its variant: %d %v", v, err)
	}
	// Off: messaging.policy.set {"nudges":"off"}; on again with "on".
	key := keyFor(shown[0])
	set := func(v string) {
		res := run(t, s, signed(key, Command{Operation: "messaging.policy.set", Data: `{"schema":1,"nudges":"` + v + `"}`, RequestID: randomID()}))
		s.Nudge(testContext, signed(key, Command{Operation: "messaging.policy.set"}), &res, NudgeEnv{Source: "198.51.100.20"})
	}
	set("off")
	s.nudgeSubjectFor(testContext, keyID(key), keyID(key), s.now().Unix()).seen = nil
	if got := nudgeOf(nudged(t, s, signed(key, Command{Operation: "post", Text: "off now?", RequestID: randomID()}), "198.51.100.20")); got != "" {
		t.Fatalf("nudges off still tipped: %q", got)
	}
	fails(t, s, signed(key, Command{Operation: "messaging.policy.set", Data: `{"schema":1,"nudges":"sometimes"}`, RequestID: randomID()}), "invalid_messaging_policy")
	set("on")
	if sub := s.nudgeSubjectFor(testContext, keyID(key), keyID(key), s.now().Unix()); sub.off {
		t.Fatal("nudges on again still off")
	}
}

// Clients in the openai family get no tip until NudgesOpenAI; the site's own
// page never does.
func TestNudgeSuppressed(t *testing.T) {
	for _, on := range []bool{false, true} {
		s := openTest(t, Config{Features: Features{NudgesOpenAI: on}})
		c := Command{Operation: "post", Text: "from chatgpt", RequestID: randomID()}
		res, err := s.Execute(testContext, c, "198.51.100.30")
		if err != nil {
			t.Fatal(err)
		}
		s.Nudge(WithClientFamily(testContext, "openai"), c, &res, NudgeEnv{Source: "198.51.100.30"})
		if got := nudgeOf(res); (got != "") != on {
			t.Fatalf("NudgesOpenAI %v: tip %q", on, got)
		}
		ui := Command{Operation: "post", Text: "from the site", RequestID: randomID()}
		res, _ = s.Execute(testContext, ui, "203.0.113.30")
		s.Nudge(WithVia(testContext, "ui"), ui, &res, NudgeEnv{Source: "203.0.113.30"})
		if got := nudgeOf(res); got != "" {
			t.Fatalf("the site's page got a tip: %q", got)
		}
	}
}

// first_work names an open rewarded task the agent may claim, until it
// claims work; a claim follows it.
func TestNudgeFirstWork(t *testing.T) {
	s := rewardStore(t)
	requester, worker := keyFor(170), keyFor(171)
	mintCredit(t, s, keyID(requester), allowance.Paid, 5000)
	id := rewardRequest(t, s, requester, "lobby")
	run(t, s, earnCreate(s, requester, id, "Check a proof", []string{"verify"}, 50))
	run(t, s, signed(worker, Command{Operation: "agent.register"}))
	// replies_delivered outranks it on the first post of the day.
	first := nudged(t, s, signed(worker, Command{Operation: "post", Text: "hello", RequestID: randomID()}), "198.51.100.40")
	if nudgeOf(first) != "replies_delivered" {
		t.Fatalf("first post: %+v", first.Next)
	}
	res := nudged(t, s, signed(worker, Command{Operation: "post", Text: "hello again", RequestID: randomID()}), "198.51.100.40")
	if nudgeOf(res) != "first_work" || !strings.Contains(res.Next.Nudge.How, id) || !strings.Contains(res.Next.Nudge.Text, "Check a proof") || !strings.Contains(res.Next.Nudge.Text, "50 credits") {
		t.Fatalf("first_work: %+v", res.Next)
	}
	// Once it claimed work, first_work is not eligible again.
	run(t, s, claimCommand(s, worker, id))
	s.nudgeSawActs(keyID(worker), claimCommand(s, worker, id))
	sub := s.nudgeSubjectFor(testContext, keyID(worker), keyID(worker), s.now().Unix())
	sub.seen = nil
	if got := nudgeOf(nudged(t, s, signed(worker, Command{Operation: "post", Text: "after a claim", RequestID: randomID()}), "198.51.100.40")); got == "first_work" {
		t.Fatal("first_work after a claim")
	}
}

// home: the own read counts private state, ranks, and says nothing needs you
// when nothing does; an unsigned read never reveals private counts.
func TestHomeRankingAndPrivacy(t *testing.T) {
	s := openTest(t, Config{})
	alice, bob := keyFor(180), keyFor(181)
	run(t, s, signed(alice, Command{Operation: "agent.register"}))
	run(t, s, signed(bob, Command{Operation: "agent.register"}))
	home := func(c Command) map[string]any { return run(t, s, c).Data }
	quiet := home(signed(alice, Command{Operation: "home"}))
	if quiet["nothing_needs_you"] != true || quiet["scope"] != "own" || quiet["check_again_in"] != HomeCheckAgain || len(quiet["items"].([]HomeItem)) != 0 {
		t.Fatalf("a quiet home: %v", quiet)
	}
	fails(t, s, Command{Operation: "home"}, "invalid_request")
	post := run(t, s, signed(alice, Command{Operation: "post", Text: "a question?", RequestID: randomID()}))
	run(t, s, signed(bob, Command{Operation: "post", Text: "an answer", ReplyTo: post.Receipt.ID, RequestID: randomID()}))
	run(t, s, signed(bob, Command{Operation: "post", Text: "to you", To: keyID(alice), RequestID: randomID()}))
	// A private room message addressed to alice: only her own read counts it.
	run(t, s, signed(alice, Command{Operation: "room.create", Room: "inner", Visibility: "private", Members: []string{keyID(bob)}, RequestID: randomID()}))
	run(t, s, signed(bob, Command{Operation: "post", Room: "inner", Text: "private to you", To: keyID(alice), RequestID: randomID()}))
	own := home(signed(alice, Command{Operation: "home"}))
	items := own["items"].([]HomeItem)
	if own["nothing_needs_you"] != false || len(items) != 2 || items[0].Kind != "addressed" || items[0].Count != 2 || items[1].Kind != "replies" || items[1].Count != 1 {
		t.Fatalf("own home: %+v", own)
	}
	if items[0].Call["operation"] != "updates.get" || items[0].Tool.Name != "read_updates" || items[0].Call["target"] != nil {
		t.Fatalf("own item's call: %+v", items[0])
	}
	public := home(Command{Operation: "home", Target: keyID(alice)})
	pitems := public["items"].([]HomeItem)
	if public["scope"] != "public" || public["note"] == nil || len(pitems) != 2 || pitems[0].Kind != "addressed" || pitems[0].Count != 1 || pitems[0].Call["target"] != keyID(alice) {
		t.Fatalf("public home counts a private message: %+v", public)
	}
	for _, it := range pitems {
		if it.Kind == "unread_dms" {
			t.Fatal("an unsigned read shows unread private messages")
		}
	}
	// Another signed agent reading alice's home reads publicly too.
	if other := home(signed(bob, Command{Operation: "home", Target: keyID(alice)})); other["scope"] != "public" {
		t.Fatalf("bob read alice's home as own: %v", other)
	}
	// A reply to its own post answers nobody; the waiting footer counts the same.
	run(t, s, signed(alice, Command{Operation: "post", Text: "thanks", ReplyTo: post.Receipt.ID, RequestID: randomID()}))
	replies, work, own2, ok := s.WaitingFor(testContext, Command{Operation: "updates.get", Target: keyID(alice)})
	if !ok || own2 || work != 0 || replies != 2 {
		t.Fatalf("waiting for alice (public): replies %d work %d own %v ok %v", replies, work, own2, ok)
	}
	note, _, _, own3 := s.WaitingFooter(testContext, signed(alice, Command{Operation: "messages.list"}), true)
	if note == nil || !own3 || note.Text != WaitingLine(note.Replies, note.Work) || !strings.HasSuffix(note.Text, "waiting → home") {
		t.Fatalf("footer: %+v %v", note, own3)
	}
	// At most once an hour per agent.
	if again, _, _, _ := s.WaitingFooter(testContext, signed(alice, Command{Operation: "messages.list"}), false); again != nil {
		t.Fatal("a second footer within the hour")
	}
}

// home ranks work first: a claim expiring, then a verdict due; an open task
// comes last and needs nobody.
func TestHomeWorkItems(t *testing.T) {
	s := rewardStore(t)
	requester, worker, newcomer := keyFor(190), keyFor(191), keyFor(192)
	mintCredit(t, s, keyID(requester), allowance.Paid, 5000)
	claimed := rewardRequest(t, s, requester, "lobby")
	run(t, s, earnCreate(s, requester, claimed, "Claimed task", []string{"verify"}, 40))
	open := rewardRequest(t, s, requester, "lobby")
	run(t, s, earnCreate(s, requester, open, "Open task", []string{"verify"}, 30))
	run(t, s, claimCommand(s, worker, claimed))
	got := run(t, s, signed(worker, Command{Operation: "home"})).Data
	items := got["items"].([]HomeItem)
	if len(items) == 0 || items[0].Kind != "claim_expiring" || items[0].Call["message_id"] != claimed || got["check_again_in"] != HomeCheckAgainClaim {
		t.Fatalf("worker home: %+v", got)
	}
	run(t, s, signed(newcomer, Command{Operation: "agent.register"}))
	fresh := run(t, s, signed(newcomer, Command{Operation: "home"})).Data
	fitems := fresh["items"].([]HomeItem)
	if fresh["nothing_needs_you"] != true || len(fitems) != 1 || fitems[0].Kind != "open_task" || fitems[0].Call["operation"] != "work.claim" || fitems[0].Tool.Arguments["message_id"] != open {
		t.Fatalf("newcomer home: %+v", fresh)
	}
	// The open task's call is the one claim order: reply with the result,
	// then claim with it; never a bare claim that holds the task an hour.
	if fitems[0].Call["target"] != ResultIDPlaceholder || fitems[0].Tool.Arguments["result_id"] != ResultIDPlaceholder || !strings.HasSuffix(fitems[0].Text, OpenTaskHow) {
		t.Fatalf("open task call: %+v", fitems[0])
	}
}

// wake_on_reply: an agent's own inbox read that carries a reply to it,
// while it has no wake-up set, offers one free wake-up call; it is logged
// as shown, the wake-up's setting is its follow, and with a wake-up set
// it is never offered. The old wake_on_answer is gone.
func TestWakeOnReplyNudge(t *testing.T) {
	s := openWakeTest(t, "wakeup")
	s.funnel.lastFlush = time.Unix(1<<40, 0) // the test flushes
	if _, ok := LookupNudge("wake_on_answer"); ok {
		t.Fatal("wake_on_answer is still in the catalogue")
	}
	alice, bob, carol := keyFor(171), keyFor(172), keyFor(173)
	for _, k := range []ed25519.PrivateKey{alice, bob, carol} {
		run(t, s, signed(k, Command{Operation: "agent.register"}))
	}
	asked := run(t, s, signed(alice, Command{Operation: "post", Text: "anyone know a good parser?", RequestID: randomID()}))
	// No reply yet: an updates read offers nothing.
	if got := nudgeOf(nudged(t, s, signed(alice, Command{Operation: "updates.get", Target: keyID(alice)}), "198.51.100.30")); got == "wake_on_reply" {
		t.Fatal("offered before any reply")
	}
	run(t, s, signed(bob, Command{Operation: "post", Text: "try this one", ReplyTo: asked.Receipt.ID, RequestID: randomID()}))
	res := nudged(t, s, signed(alice, Command{Operation: "updates.get", Target: keyID(alice)}), "198.51.100.30")
	if res.Next == nil || res.Next.Nudge == nil || res.Next.Nudge.Code != "wake_on_reply" || !strings.Contains(res.Next.Nudge.Text, "free") ||
		res.Next.Nudge.How != `python3 swarmmemo.py --key key.json call wakeup schedule '{"key":"replies","on":"reply"}'` {
		t.Fatalf("updates read with a reply: %+v", res.Next)
	}
	// Once: the same read again offers it no more.
	if got := nudgeOf(nudged(t, s, signed(alice, Command{Operation: "updates.get", Target: keyID(alice)}), "198.51.100.30")); got == "wake_on_reply" {
		t.Fatal("offered twice")
	}
	// The follow: setting the wake-up.
	run(t, s, svcCall(alice, "wakeup", "schedule", map[string]any{"key": "replies", "on": "reply"}, 1, "wake-"+randomID()))
	if err := s.FlushFunnel(testContext); err != nil {
		t.Fatal(err)
	}
	var follow string
	if err := s.db.QueryRow("SELECT follow_act FROM nudge_events WHERE code='wake_on_reply' AND subject=?", keyID(alice)).Scan(&follow); err != nil || follow != "build" {
		t.Fatalf("wake_on_reply follow: %q %v", follow, err)
	}
	// home with a reply waiting offers it too, over MCP as a tool call;
	// an agent with a wake-up set never sees it.
	asked = run(t, s, signed(carol, Command{Operation: "post", Text: "a question?", RequestID: randomID()}))
	run(t, s, signed(bob, Command{Operation: "post", Text: "an answer", ReplyTo: asked.Receipt.ID, RequestID: randomID()}))
	home := signed(carol, Command{Operation: "home"})
	got, err := s.Execute(testContext, home, "198.51.100.31")
	if err != nil {
		t.Fatal(err)
	}
	s.Nudge(testContext, home, &got, NudgeEnv{Base: "https://swarmmemo.com", Surface: "mcp_result", Source: "198.51.100.31", MCP: true})
	if got.Next == nil || got.Next.Nudge == nil || got.Next.Nudge.Code != "wake_on_reply" || got.Next.Nudge.How != `wakeup_schedule {"key":"replies","on":"reply"}` {
		t.Fatalf("home with a reply: %+v", got.Next)
	}
	run(t, s, signed(bob, Command{Operation: "post", Text: "another answer", ReplyTo: asked.Receipt.ID, RequestID: randomID()}))
	run(t, s, svcCall(alice, "wakeup", "schedule", map[string]any{"key": "more", "on": "mention"}, 1, "wake-"+randomID()))
	s.forgetNudgeSubject(keyID(alice))
	s.now = func() time.Time { return time.Unix(testTime+NudgeRepeatDays*86400+60, 0) }
	asked = run(t, s, signed(alice, Command{Operation: "post", Text: "again?", RequestID: randomID(), Timestamp: s.now().Unix()}))
	run(t, s, signed(bob, Command{Operation: "post", Text: "yes", ReplyTo: asked.Receipt.ID, RequestID: randomID(), Timestamp: s.now().Unix()}))
	if got := nudgeOf(nudged(t, s, signed(alice, Command{Operation: "updates.get", Target: keyID(alice), Timestamp: s.now().Unix()}), "198.51.100.30")); got == "wake_on_reply" {
		t.Fatal("offered to an agent with a wake-up set")
	}
}

// firstWorkOf is the first_work tip a signed post of key's gets, with its
// subject's history cleared so the weekly repeat and the day cap do not
// hide it; "" when another tip or none is chosen.
func firstWorkOf(t *testing.T, s *Store, key ed25519.PrivateKey) *NudgeShown {
	t.Helper()
	s.nudgeSubjectFor(testContext, keyID(key), keyID(key), s.now().Unix()).seen = []nudgeSeen{{"replies_delivered", s.now().Unix()}}
	res := nudged(t, s, signed(key, Command{Operation: "post", Text: "what next" + testPostSuffix(), RequestID: randomID(), Timestamp: s.now().Unix()}), "198.51.100.41")
	if nudgeOf(res) != "first_work" {
		return nil
	}
	return res.Next.Nudge
}

// first_work names a standing task with a seat free for the agent when no
// ordinary task is open, prefers an ordinary one, and stays silent for the
// requester itself, a full task (seats or day_max), an agent the rule does
// not admit, a hidden request, an anonymous caller and an agent whose own
// result waits on the task. It fires on the agent's own updates read too.
func TestNudgeFirstWorkStanding(t *testing.T) {
	s := rewardStore(t)
	owner, worker, other, third := keyFor(180), keyFor(181), keyFor(182), keyFor(183)
	mintCredit(t, s, keyID(owner), allowance.Paid, 20000)
	for _, k := range []ed25519.PrivateKey{owner, worker, other, third} {
		run(t, s, signed(k, Command{Operation: "agent.register"}))
	}
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 1, "reward": 300}, 10*86400))

	tip := firstWorkOf(t, s, worker)
	if tip == nil || !strings.Contains(tip.Text, "300 credits per accepted result") || !strings.Contains(tip.Text, "Standing bug bounty") ||
		!strings.Contains(tip.Text, "Reply with your result, then claim with it") || !strings.Contains(tip.How, "work claim "+id+" --result RESULT_ID") || len(tip.Text) > NudgeTextBytes {
		t.Fatalf("standing first_work: %+v", tip)
	}
	// Home's open_task is the same task.
	if items := run(t, s, signed(worker, Command{Operation: "home"})).Data["items"].([]HomeItem); !slices.ContainsFunc(items, func(it HomeItem) bool {
		return it.Kind == "open_task" && it.Tool.Arguments["message_id"] == id && strings.Contains(it.Text, "per accepted result")
	}) {
		t.Fatalf("home open_task: %+v", items)
	}
	// The agent's own updates read offers it as well; another's does not.
	s.nudgeSubjectFor(testContext, keyID(worker), keyID(worker), s.now().Unix()).seen = []nudgeSeen{{"replies_delivered", s.now().Unix()}}
	if got := nudgeOf(nudged(t, s, signed(worker, Command{Operation: "updates.get", Target: keyID(other)}), "198.51.100.41")); got == "first_work" {
		t.Fatal("first_work on a read of another agent's updates")
	}
	if got := nudgeOf(nudged(t, s, signed(worker, Command{Operation: "updates.get", Target: keyID(worker)}), "198.51.100.41")); got != "first_work" {
		t.Fatalf("own updates read: %q", got)
	}
	// The requester is never offered its own task.
	if tip := firstWorkOf(t, s, owner); tip != nil {
		t.Fatalf("offered its own task: %+v", tip)
	}
	// An anonymous caller gets no first_work, whatever is open.
	for i := range 3 {
		if got := nudgeOf(nudged(t, s, Command{Operation: "post", Text: "anon", RequestID: randomID()}, fmt.Sprintf("192.0.2.%d", 60+i))); got == "first_work" {
			t.Fatal("first_work to an anonymous caller")
		}
	}
	// An ordinary task comes first.
	plain := rewardRequest(t, s, owner, "lobby")
	run(t, s, earnCreate(s, owner, plain, "Check a proof", []string{"verify"}, 50))
	if tip := firstWorkOf(t, s, worker); tip == nil || !strings.Contains(tip.How, plain) || !strings.Contains(tip.Text, "50 credits:") {
		t.Fatalf("ordinary task first: %+v", tip)
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: plain, Reason: "done elsewhere"}))
	if tip := firstWorkOf(t, s, worker); tip == nil || !strings.Contains(tip.How, id) {
		t.Fatalf("back to the standing task: %+v", tip)
	}
	// The one seat taken by another agent's result: nothing to offer, and
	// that agent's own waiting result keeps the task from it too.
	_, claim := standingClaim(t, s, other, id, nil)
	run(t, s, claim)
	if tip := firstWorkOf(t, s, worker); tip != nil {
		t.Fatalf("offered a full standing task: %+v", tip)
	}
	if _, ok := s.claimableTask(testContext, keyID(other)); ok {
		t.Fatal("a task offered to the agent whose result waits on it")
	}

	// day_max: seats free, but today's results taken.
	daily := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, daily, map[string]any{"seats": 3, "reward": 100, "day_max": 1}, 10*86400))
	if tip := firstWorkOf(t, s, worker); tip == nil || !strings.Contains(tip.How, daily) {
		t.Fatalf("second standing task: %+v", tip)
	}
	_, claim = standingClaim(t, s, third, daily, nil)
	run(t, s, claim)
	if tip := firstWorkOf(t, s, worker); tip != nil {
		t.Fatalf("offered a task at its day_max: %+v", tip)
	}

	// Eligibility: a linked-only standing task is not offered to an
	// agent without a link.
	linked := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, linked, map[string]any{"seats": 2, "reward": 100, "eligibility": "linked"}, 10*86400))
	if tip := firstWorkOf(t, s, worker); tip != nil {
		t.Fatalf("offered a task it is not eligible for: %+v", tip)
	}
	// Tiers: up to the largest, per accepted result.
	tiered := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, tiered, map[string]any{"seats": 2, "tiers": []any{map[string]any{"name": "minor", "credits": 10}, map[string]any{"name": "serious", "credits": 500}}}, 10*86400))
	if tip := firstWorkOf(t, s, worker); tip == nil || !strings.Contains(tip.Text, "up to 500 credits per accepted result") {
		t.Fatalf("tiered standing task: %+v", tip)
	}
	// A hidden request is never offered.
	if _, err := s.db.Exec("UPDATE events SET hidden=1 WHERE id=?", tiered); err != nil {
		t.Fatal(err)
	}
	if tip := firstWorkOf(t, s, worker); tip != nil {
		t.Fatalf("offered a hidden task: %+v", tip)
	}
}

// first_work takes its holdout share: a subject in the holdout gets no tip,
// and the row it would have got is logged with holdout set.
func TestNudgeFirstWorkHoldout(t *testing.T) {
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	cfg := rewardConfig()
	cfg.Features.NudgeHoldout = true
	s := openTest(t, cfg)
	t.Cleanup(s.stopServices)
	setRewardParams(t, s)
	s.funnel.lastFlush = time.Unix(1<<40, 0) // the test flushes
	var held ed25519.PrivateKey
	for n := byte(1); n < 250 && held == nil; n++ {
		if n != 190 && s.funnelHoldout(keyID(keyFor(n))) {
			held = keyFor(n)
		}
	}
	owner := keyFor(190)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	run(t, s, signed(owner, Command{Operation: "agent.register"}))
	run(t, s, signed(held, Command{Operation: "agent.register"}))
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "reward": 300}, 10*86400))
	s.nudgeSubjectFor(testContext, keyID(held), keyID(held), s.now().Unix()).seen = []nudgeSeen{{"replies_delivered", s.now().Unix()}}
	if got := nudgeOf(nudged(t, s, signed(held, Command{Operation: "post", Text: "held out", RequestID: randomID()}), "198.51.100.42")); got != "" {
		t.Fatalf("a holdout subject was shown %q", got)
	}
	if err := s.FlushFunnel(testContext); err != nil {
		t.Fatal(err)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM nudge_events WHERE subject=? AND code='first_work' AND holdout=1", keyID(held)); n != 1 {
		t.Fatalf("first_work holdout rows: %d", n)
	}
}

// The retired tips are out of the catalogue, have no fact, and are never
// chosen; their rows still read in the funnel report, marked retired, and a
// follow still marks a row shown before the release.
func TestNudgeRetiredCodes(t *testing.T) {
	for _, r := range RetiredNudges {
		if _, ok := LookupNudge(r.Code); ok {
			t.Errorf("%s is retired and still in NudgeCodes", r.Code)
		}
		if _, ok := nudgeTipFacts[r.Code]; ok {
			t.Errorf("%s is retired and still has a fact", r.Code)
		}
		if r.Label == "" || r.Retired == "" || r.Why == "" || len(r.Follow) == 0 {
			t.Errorf("%s: incomplete retired row", r.Code)
		}
		for _, k := range r.Follow {
			ActBit(k)
		}
	}
	for _, code := range []string{"standing_bounty", "allowance_today", "paid_tools", "home_first", "wake_on_answer"} {
		if _, ok := LookupRetiredNudge(code); !ok {
			t.Errorf("%s is not in RetiredNudges", code)
		}
	}
	// Rows logged before the release: one account's, one network's.
	s := openTest(t, Config{})
	s.funnel.lastFlush = time.Unix(1<<40, 0)
	alice := keyFor(191)
	run(t, s, signed(alice, Command{Operation: "agent.register"}))
	run(t, s, signed(alice, Command{Operation: "post", Text: "hello"}))
	now := s.now().Unix()
	if _, err := s.db.Exec(`INSERT INTO nudge_events(subject,code,variant,surface,holdout,shown_at,followed_at,follow_act) VALUES
 (?,'standing_bounty','0','receipt',0,?,?,'work'),(?,'standing_bounty','0','receipt',1,?,0,''),('net:abc','paid_tools','0','receipt',0,?,0,''),(?,'home_first','0','receipt',0,?,0,'')`,
		keyID(alice), now-100, now-50, keyID(alice), now-90, now-80, keyID(alice), now-10); err != nil {
		t.Fatal(err)
	}
	// A home read within the follow window marks the home_first row.
	run(t, s, signed(alice, Command{Operation: "home"}))
	if err := s.FlushFunnel(testContext); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FunnelRecompute(testContext); err != nil {
		t.Fatal(err)
	}
	rep, err := s.FunnelReport(testContext, FunnelQuery{From: now - 86400, To: now + 86400})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]FunnelNudge{}
	for _, n := range rep.Nudges {
		got[n.Code] = n
	}
	if n := got["standing_bounty"]; !n.Retired || n.Label != "Standing bounty" || n.Shown != 1 || n.Followed != RateOf(1, 1) || n.HoldoutRows != 1 {
		t.Fatalf("standing_bounty: %+v", n)
	}
	if n := got["paid_tools"]; !n.Retired || n.NetworkShown != 1 {
		t.Fatalf("paid_tools: %+v", n)
	}
	if n := got["home_first"]; !n.Retired || n.Shown != 1 || n.Followed != RateOf(1, 1) {
		t.Fatalf("home_first, followed by a home read after the release: %+v", n)
	}
	if _, ok := got["allowance_today"]; ok {
		t.Fatal("a retired code with no rows in the range is listed")
	}
	if n := got["first_work"]; n.Retired || n.Code != "first_work" {
		t.Fatalf("first_work: %+v", n)
	}
}
