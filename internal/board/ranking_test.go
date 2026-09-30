package board

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/moderation"
)

// The board's post actuator keeps the screen's quality scores and flags.
var (
	_ moderation.QualityRecorder = postActuator{}
	_ moderation.FlagRecorder    = postActuator{}
)

func q(v float64) *float64 { return &v }

// The one ranking function, case by case.
func TestRankingFunction(t *testing.T) {
	r := Ranking
	hour := int64(3600)
	for _, c := range []struct {
		name      string
		high, low float64
		highBeats bool
	}{
		{"bias 0 is merit", r.Rank(2, nil, 0, 1000*hour, 0), r.Merit(2, nil, 0), false},
		{"useful beats unscored beats filler, same age", r.Rank(0, q(0.9), 0, hour, 1.5), r.Rank(0, nil, 0, hour, 1.5), true},
		{"unscored beats filler", r.Rank(0, nil, 0, hour, 1.5), r.Rank(0, q(0.1), 0, hour, 1.5), true},
		{"three net votes outweigh the quality gap", r.Rank(3, q(0.1), 0, hour, 1.5), r.Rank(0, q(0.9), 0, hour, 1.5), true},
		{"two do not", r.Rank(0, q(0.9), 0, hour, 1.5), r.Rank(2, q(0.1), 0, hour, 1.5), true},
		{"a vote counts one point", r.Merit(1, q(0.5), 0) - r.Merit(0, q(0.5), 0), 1, false},
		{"replies from distinct agents count", r.Rank(0, nil, 3, hour, 1.5), r.Rank(0, nil, 0, hour, 1.5), true},
		{"replies are capped", r.Merit(0, nil, 1000), r.Merit(0, nil, r.ReplyAgentsMax), false},
		{"negative reply counts are zero", r.Merit(0, nil, -5), r.Merit(0, nil, 0), false},
		{"recency wins at the default bias", r.Rank(0, nil, 0, 0, BiasDefault), r.Rank(3, q(0.9), 0, 100*hour, BiasDefault), true},
		{"downvoted below unscored", r.Rank(0, nil, 0, hour, 1.5), r.Rank(-3, q(0.9), 0, hour, 1.5), true},
		{"future posts count as age 0", r.Rank(0, nil, 0, -hour, 1.5), r.Rank(0, nil, 0, 0, 1.5), false},
	} {
		if c.highBeats && !(c.high > c.low) || !c.highBeats && c.high != c.low {
			t.Errorf("%s: %v vs %v", c.name, c.high, c.low)
		}
	}
	if got := r.Merit(0, nil, 0); got != r.QualityWeight*r.QualityNeutral {
		t.Errorf("an unscored post's merit is the neutral prior: %v", got)
	}
}

// Which reads get the first-contact order.
func TestFirstContact(t *testing.T) {
	key := keyFor(60)
	for _, c := range []struct {
		name string
		cmd  Command
		hot  bool
	}{
		{"bare read", Command{Operation: "messages.list"}, true},
		{"one room", Command{Operation: "messages.list", Room: "lobby", Page: "main", Limit: 5}, true},
		{"cursor", Command{Operation: "messages.list", Cursor: "start"}, false},
		{"explicit order", Command{Operation: "messages.list", Data: `{"sort":"new"}`}, false},
		{"search", Command{Operation: "messages.list", Query: "x"}, false},
		{"inbox", Command{Operation: "messages.list", To: strings.Repeat("a", 64)}, false},
		{"author history", Command{Operation: "messages.list", Target: strings.Repeat("a", 64)}, false},
		{"kind", Command{Operation: "messages.list", Kind: "request"}, false},
		{"signed", signed(key, Command{Operation: "messages.list"}), false},
		{"updates", Command{Operation: "updates.get"}, false},
		{"thread", Command{Operation: "thread.get", MessageID: "x"}, false},
	} {
		if got := FirstContact(c.cmd).Data == firstContactSort; got != c.hot {
			t.Errorf("%s: hot=%v", c.name, got)
		}
	}
}

// Quality scores rank the hot view, ride on reads with their model, never on
// exports, and a post without one ranks by votes and recency.
func TestQualityRanksAndIsExposed(t *testing.T) {
	s := openTest(t, Config{})
	ctx := context.Background()
	post := func(text string) string {
		return run(t, s, Command{Operation: "post", Room: "lobby", Text: text}).Receipt.ID
	}
	ids := func(r Result) []string {
		out := []string{}
		for _, m := range r.Messages {
			out = append(out, m.ID)
		}
		return out
	}
	hot := func() Result {
		return run(t, s, Command{Operation: "messages.list", Room: "lobby", Data: `{"sort":"hot"}`})
	}
	// Without scores the hot view is newest first (all merit is neutral).
	a, b, c := post("first"), post("second"), post("third")
	if got := ids(hot()); strings.Join(got, ",") != strings.Join([]string{c, b, a}, ",") {
		t.Fatalf("unscored hot: %v", got)
	}
	// Scores reorder it: useful first, filler last, unscored between.
	if err := (postActuator{s}).RecordQuality(ctx, a, 0.95, "jev-1.13.0"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordQuality(ctx, c, 0.05, "jev-1.13.0"); err != nil {
		t.Fatal(err)
	}
	s.rankMu.Lock()
	s.rankCache = nil // scores reach rankings when their cache expires
	s.rankMu.Unlock()
	res := hot()
	if got := ids(res); strings.Join(got, ",") != strings.Join([]string{a, b, c}, ",") {
		t.Fatalf("scored hot: %v", got)
	}
	if m := res.Messages[0]; m.Quality == nil || m.Quality.Score != 0.95 || m.Quality.Model != "jev-1.13.0" {
		t.Fatalf("quality on the read: %+v", m.Quality)
	}
	if res.Messages[1].Quality != nil {
		t.Fatal("an unscored post carries a score")
	}
	if got := run(t, s, Command{Operation: "message.get", MessageID: c}).Messages[0].Quality; got == nil || got.Score != 0.05 {
		t.Fatalf("message.get quality %+v", got)
	}
	if res.Data["sort"] != "hot" || res.NextCursor == "" {
		t.Fatalf("hot data %v cursor %q", res.Data, res.NextCursor)
	}
	// The ranked read's cursor resumes the chronological feed from now.
	d := post("fourth")
	if next := run(t, s, Command{Operation: "messages.list", Room: "lobby", Cursor: res.NextCursor}); strings.Join(ids(next), ",") != d {
		t.Fatalf("resume from the ranked cursor: %v", ids(next))
	}
	exp, err := s.Execute(testContext, Command{Operation: "export", Before: testTime + 86400*400}, "test-origin")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range exp.Messages {
		if m.Quality != nil {
			t.Fatal("export carries quality")
		}
	}
	for _, bad := range []float64{-0.1, 1.1} {
		if s.RecordQuality(ctx, a, bad, "jev") == nil {
			t.Fatalf("accepted quality %v", bad)
		}
	}
	if s.RecordQuality(ctx, a, 0.5, "") == nil {
		t.Fatal("accepted a score without a model")
	}
	// A post made after a ranking was cached shows at once.
	e := post("fifth")
	if got := ids(hot()); len(got) != 5 || got[0] != a || got[1] != e {
		t.Fatalf("new post missing from the cached ranking: %v", got)
	}
	if _, _, _, err := s.BackfillQuality(ctx, 10); err == nil {
		t.Fatal("backfill ran with moderation off")
	}
}

// Replies from distinct signed agents a day old lift a post; the author's own
// replies, anonymous ones and fresh keys' do not.
func TestReplyAgentsLiftAPost(t *testing.T) {
	s := openTest(t, Config{})
	author := keyFor(61)
	seasoned(t, s, keyFor(62))
	older := postAs(t, s, author, Command{Room: "lobby", Text: "older", RequestID: "older"})
	newer := postAs(t, s, author, Command{Room: "lobby", Text: "newer", RequestID: "newer"})
	top := func() string {
		s.rankMu.Lock()
		s.rankCache = nil
		s.rankMu.Unlock()
		return run(t, s, Command{Operation: "messages.list", Room: "lobby", Data: `{"sort":"hot"}`}).Messages[0].ID
	}
	if top() != newer {
		t.Fatal("equal merit: newest first")
	}
	postAs(t, s, author, Command{Room: "lobby", Text: "self", ReplyTo: older, RequestID: "self"})
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "anon", ReplyTo: older})
	postAs(t, s, keyFor(65), Command{Room: "lobby", Text: "fresh key", ReplyTo: older, RequestID: "fresh"})
	if top() != newer {
		t.Fatal("self, anonymous or fresh-key replies lifted a post")
	}
	postAs(t, s, keyFor(62), Command{Room: "lobby", Text: "agree", ReplyTo: older, RequestID: "r1"})
	postAs(t, s, keyFor(62), Command{Room: "lobby", Text: "again", ReplyTo: older, RequestID: "r2"})
	if top() != older {
		t.Fatal("a reply from another agent did not lift the post")
	}
}

// Hidden posts never rank; simulations and imported summaries rank only when
// asked for by kind; the chronological feed still lists them.
func TestRankedViewsExcludeHiddenSimulationsAndImports(t *testing.T) {
	s := openTest(t, Config{})
	visible := run(t, s, Command{Operation: "post", Room: "lobby", Text: "real"}).Receipt.ID
	sim := run(t, s, Command{Operation: "post", Room: "lobby", Text: "simulated", Kind: "simulation"}).Receipt.ID
	imported := run(t, s, Command{Operation: "post", Room: "lobby", Text: "imported summary"}).Receipt.ID
	if _, err := s.db.Exec("UPDATE events SET kind='imported' WHERE id=?", imported); err != nil {
		t.Fatal(err)
	}
	hidden := run(t, s, Command{Operation: "post", Room: "lobby", Text: "removed"}).Receipt.ID
	if err := s.Moderate(testContext, hidden, "test", true); err != nil {
		t.Fatal(err)
	}
	got := run(t, s, Command{Operation: "messages.list", Room: "lobby", Data: `{"sort":"hot"}`}).Messages
	if len(got) != 1 || got[0].ID != visible {
		t.Fatalf("hot view: %+v", got)
	}
	if got := run(t, s, Command{Operation: "messages.list", Room: "lobby", Kind: "simulation", Data: `{"sort":"hot"}`}).Messages; len(got) != 1 || got[0].ID != sim {
		t.Fatalf("asked for simulations: %+v", got)
	}
	if got := run(t, s, Command{Operation: "messages.list", Room: "lobby", Data: `{"sort":"new"}`}).Messages; len(got) != 4 {
		t.Fatalf("the chronological feed lost posts: %d", len(got))
	}
}

// Explicit and cursor reads stay chronological; updates.get too.
func TestChronologicalReadsUnchanged(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(63)
	root := postAs(t, s, key, Command{Room: "lobby", Text: "root", RequestID: "root"})
	reply := postAs(t, s, keyFor(64), Command{Room: "lobby", Text: "reply", ReplyTo: root, RequestID: "reply"})
	last := postAs(t, s, key, Command{Room: "lobby", Text: "last", RequestID: "last"})
	want := strings.Join([]string{root, reply, last}, ",")
	order := func(r Result) string {
		out := []string{}
		for _, m := range r.Messages {
			out = append(out, m.ID)
		}
		return strings.Join(out, ",")
	}
	if got := order(run(t, s, Command{Operation: "messages.list", Room: "lobby", Cursor: "start"})); got != want {
		t.Fatalf("cursor read: %v", got)
	}
	if got := order(run(t, s, Command{Operation: "messages.list", Room: "lobby", Data: `{"sort":"new"}`})); got != want {
		t.Fatalf("sort=new: %v", got)
	}
	if got := order(run(t, s, Command{Operation: "messages.list", Room: "lobby"})); got != want {
		t.Fatalf("a board read without FirstContact changed order: %v", got)
	}
	up := run(t, s, Command{Operation: "updates.get"})
	if got := order(up); got != want {
		t.Fatalf("updates.get: %v", got)
	}
}

// Rooms rank by distinct recent authors weighted by their posts' quality.
func TestRoomsRankByAuthorsAndQuality(t *testing.T) {
	s := openTest(t, Config{})
	ctx := context.Background()
	for i, room := range []string{"useful", "filler"} {
		for j := byte(0); j < 2; j++ {
			id := postAs(t, s, keyFor(70+byte(i)*2+j), Command{Room: room, Text: "x", RequestID: room + string(rune('a'+j))})
			score := 0.9
			if room == "filler" {
				score = 0.05
			}
			if err := s.RecordQuality(ctx, id, score, "jev-1.13.0"); err != nil {
				t.Fatal(err)
			}
		}
	}
	names := []string{}
	for _, r := range run(t, s, Command{Operation: "rooms.list", Room: "", Query: "l"}).Rooms {
		names = append(names, r.Name)
	}
	if strings.Join(names, ",") != "useful,filler" {
		t.Fatalf("rooms: %v", names)
	}
}

// The agent directory's first page is hot: recently active agents with a
// profile and useful posts first.
func TestAgentsHotPage(t *testing.T) {
	s := openTest(t, Config{})
	ctx := context.Background()
	at := func(offset int64) { s.now = func() time.Time { return time.Unix(testTime+offset, 0) } }
	good, loud := keyFor(80), keyFor(81)
	at(-6 * 3600)
	id := postAs(t, s, good, Command{Room: "lobby", Text: "a finding", RequestID: "g"})
	run(t, s, signed(good, Command{Operation: "agent.profile.publish", Data: testPeerData, Timestamp: testTime - 6*3600}))
	if err := s.RecordQuality(ctx, id, 0.9, "jev-1.13.0"); err != nil {
		t.Fatal(err)
	}
	at(0)
	id = postAs(t, s, loud, Command{Room: "lobby", Text: "gm", RequestID: "l"})
	if err := s.RecordQuality(ctx, id, 0.02, "jev-1.13.0"); err != nil {
		t.Fatal(err)
	}
	res := run(t, s, Command{Operation: "agents.list"})
	if len(res.Agents) != 2 || res.Agents[0].ID != keyID(good) || res.Data["sort"] != "hot" || res.NextCursor != "" {
		t.Fatalf("hot page: %+v %v", res.Agents, res.Data)
	}
	if res := run(t, s, Command{Operation: "agents.list", Kind: "new"}); res.Agents[0].ID != keyID(loud) {
		t.Fatal("sort=new is not newest first")
	}
	fails(t, s, Command{Operation: "agents.list", Kind: "hot", Cursor: "x"}, "cursor_with_sort")
	// The page is shared for HotAgentsTTL: a new score shows after it.
	if err := s.RecordQuality(ctx, id, 1, "jev-1.13.0"); err != nil {
		t.Fatal(err)
	}
	if res := run(t, s, Command{Operation: "agents.list"}); res.Agents[0].ID != keyID(good) {
		t.Fatal("the cached hot page changed within its TTL")
	}
	at(int64(HotAgentsTTL / time.Second))
	if res := run(t, s, Command{Operation: "agents.list"}); res.Agents[0].ID != keyID(loud) {
		t.Fatal("the hot page did not refresh after its TTL")
	}
	// A profile change drops it at once.
	run(t, s, signed(loud, Command{Operation: "agent.profile.publish", Data: testPeerData, Timestamp: testTime + int64(HotAgentsTTL/time.Second)}))
	if res := run(t, s, Command{Operation: "agents.list"}); res.Agents[0].Profile == nil {
		t.Fatal("a profile change did not refresh the hot page")
	}
	// Quality counts over the window only: an agent's old useful posts do not
	// lift it now.
	s.rankMu.Lock()
	s.hotAgentsCached = nil
	s.rankMu.Unlock()
	old := keyFor(82)
	at(-40 * 86400)
	oid := postAs(t, s, old, Command{Room: "lobby", Text: "an old finding", RequestID: "o1"})
	if err := s.RecordQuality(ctx, oid, 1, "jev-1.13.0"); err != nil {
		t.Fatal(err)
	}
	at(int64(HotAgentsTTL/time.Second) + 60)
	oid = postAs(t, s, old, Command{Room: "lobby", Text: "gm", RequestID: "o2"})
	if err := s.RecordQuality(ctx, oid, 0, "jev-1.13.0"); err != nil {
		t.Fatal(err)
	}
	for _, a := range run(t, s, Command{Operation: "agents.list"}).Agents {
		if a.ID == keyID(old) {
			break
		}
		if a.ID == keyID(good) {
			return
		}
	}
	t.Fatal("an old post's quality lifted an agent")
}

// fakeScorer scores posts for the backfill: fail lists texts it cannot score.
type fakeScorer struct {
	fail  map[string]bool
	asked []string
}

func (f *fakeScorer) SpendToday(context.Context) (moderation.Spend, error) {
	return moderation.Spend{CapMicroUSD: 1000}, nil
}

func (f *fakeScorer) ScoreQuality(_ context.Context, _ moderation.Subject, text string) (float64, string, error) {
	f.asked = append(f.asked, text)
	if f.fail[text] {
		return 0, "", errors.New("jev: malformed answer")
	}
	return 0.8, "jev-1.13.0", nil
}

// The backfill scores only posts a ranking shows (no replies, simulations or
// imported summaries), skips a post it cannot score and goes on, gives up
// after a run of failures, and never scores more than QualityBackfillMax.
func TestBackfillQualitySkipsAndContinues(t *testing.T) {
	s := openTest(t, Config{})
	ctx := context.Background()
	a := run(t, s, Command{Operation: "post", Room: "lobby", Text: "a"}).Receipt.ID
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "bad"})
	c := run(t, s, Command{Operation: "post", Room: "lobby", Text: "c"}).Receipt.ID
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "reply", ReplyTo: a})
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "sim", Kind: "simulation"})
	f := &fakeScorer{fail: map[string]bool{"bad": true}}
	scored, skipped, stopped, err := s.backfillQuality(ctx, f, 10)
	if err != nil || scored != 2 || skipped != 1 || !strings.Contains(stopped, "every public post") {
		t.Fatalf("scored %d skipped %d stopped %q err %v", scored, skipped, stopped, err)
	}
	if got := strings.Join(f.asked, ","); got != "c,bad,a" {
		t.Fatalf("asked %s", got)
	}
	for _, id := range []string{a, c} {
		if q := run(t, s, Command{Operation: "message.get", MessageID: id}).Messages[0].Quality; q == nil || q.Score != 0.8 {
			t.Fatalf("%s: %+v", id, q)
		}
	}
	// A run of failures ends the run with the error.
	for i := 0; i < qualityBackfillErrors+2; i++ {
		run(t, s, Command{Operation: "post", Room: "lobby", Text: "down"})
	}
	f = &fakeScorer{fail: map[string]bool{"down": true, "bad": true}}
	if _, skipped, _, err := s.backfillQuality(ctx, f, QualityBackfillMax+500); err == nil || skipped != qualityBackfillErrors {
		t.Fatalf("skipped %d err %v", skipped, err)
	}
	if n, _, _, _ := s.backfillQuality(ctx, &fakeScorer{}, -1); n != 0 {
		t.Fatal("a negative limit scored posts")
	}
}
