package board

import (
	"crypto/ed25519"
	"slices"
	"strings"
	"testing"

	"swarmmemo/internal/find"
	"swarmmemo/internal/reviews"
)

// Find on the real engine: its sources (agents with their profiles and
// standing, listed subjects with the reviews core's aggregate and lab
// assessment, reviewed x402 APIs outside the catalogue), the find read
// unsigned, its filters and refusals.
func TestFindOnTheBoard(t *testing.T) {
	s := reviewsStore(t)
	bot, coder, alice, bob, lab := keyFor(90), keyFor(91), keyFor(92), keyFor(93), keyFor(94)
	run(t, s, signed(bot, Command{Operation: "agent.register", Handle: "weatherbot"}))
	run(t, s, signed(bot, Command{Operation: "agent.profile.publish", Data: `{"schema":1,"description":"I answer weather questions for Lisbon and Porto.","capabilities":["forecast"],"availability":"available"}`}))
	run(t, s, signed(coder, Command{Operation: "agent.register", Handle: "coder"}))
	run(t, s, signed(coder, Command{Operation: "agent.profile.publish", Data: testPeerData}))
	register(t, s, alice)
	register(t, s, bob)
	register(t, s, lab)
	// A weather MCP server two accounts reviewed (listed), one mostly bad.
	mcpURI := "mcp:https://mcp.example.com/weather"
	run(t, s, reviewPost(t, alice, mcpURI, map[string]any{}, ""))
	run(t, s, reviewPost(t, bob, mcpURI, map[string]any{"verdict": "mixed"}, ""))
	// An x402 weather API outside the catalogue, reviewed and assessed by the lab: avoid.
	apiURI := "x402:GET https://weatherly.example.com/v1/forecast"
	run(t, s, reviewPost(t, alice, apiURI, map[string]any{"verdict": "bad"}, ""))
	run(t, s, reviewPost(t, bob, apiURI, map[string]any{"verdict": "bad"}, ""))
	// The lab's assessment counts once its record recomputes (C197): a lab
	// key on the labs list, a real observations record, the recompute run.
	allowAccounts(t, s, LabsParamsNamespace, lab)
	labFields := labRecord(t, s, lab, labObservations(t, testTime-3600, strings.TrimPrefix(apiURI, "x402:"), nil))
	run(t, s, reviewPost(t, lab, apiURI, labFields, ""))
	recompute(t, s)
	labVerdict, labGrade, labLower := reviews.LabVerdicts[labFields["verdict"].(string)], labFields["grade"].(string), labFields["band"].([]int)[0]
	if labGrade == "" {
		t.Fatal("the lab record stores no grade")
	}
	// A site one account reviewed: not listed, never indexed.
	run(t, s, reviewPost(t, alice, "site:https://weather-unlisted.example.com/", map[string]any{}, ""))

	res := run(t, s, Command{Operation: "find", Query: "weather", Data: `{"schema":1,"explain":true}`})
	hits := res.Data["results"].([]find.Hit)
	byID := map[string]find.Hit{}
	for _, h := range hits {
		byID[h.ID] = h
	}
	bot1 := byID[keyAccount(t, s, bot)]
	if bot1.Type != find.TypeAgent || bot1.Name != "weatherbot" || bot1.URL != "/@weatherbot" || bot1.State != "up" || !bot1.Untrusted {
		t.Fatalf("agent hit %+v (all %+v)", bot1, hits)
	}
	mcpHit := byID[reviewSubjectOf(t, mcpURI).URI()]
	if mcpHit.Type != find.TypeMCP || mcpHit.Reviews == nil || mcpHit.Reviews.Count != 2 || mcpHit.URL != "/s/"+reviewSubjectOf(t, mcpURI).ID {
		t.Fatalf("mcp hit %+v", mcpHit)
	}
	// The aggregate is the core's own: the same verdict mean /s/ID shows.
	agg := readSubjectData(t, s, mcpURI)["aggregate"].(ReviewAggregate)
	if mcpHit.Reviews.Mean != agg.VerdictMean {
		t.Fatalf("find mean %v, core %v", mcpHit.Reviews.Mean, agg.VerdictMean)
	}
	api := byID[reviewSubjectOf(t, apiURI).URI()]
	if api.Type != find.TypeService || api.Kind != find.KindX402 || api.Lab == nil || api.Lab.Verdict != labVerdict || *api.Lab.Lower != labLower ||
		// good is unmeasured (no paid call): the stored grade shows as no grade (C752)
		api.Lab.Grade != "" || api.Lab.GradeNote != "no grade (good not measured)" ||
		!strings.Contains(api.Why, "lab: "+labVerdict+", no grade (good not measured)") || !strings.Contains(api.Why, "not in SwarmMemo's catalogue") {
		t.Fatalf("x402 hit %+v %+v", api, api.Explain)
	}
	// A reviewed endpoint outside the catalogue keeps its protocol family, so
	// a protocol filter finds it, and the other family does not (df927e0f).
	inFamily := func(protocol string) bool {
		for _, h := range run(t, s, Command{Operation: "find", Query: "weather", Data: `{"schema":1,"protocol":"` + protocol + `"}`}).Data["results"].([]find.Hit) {
			if h.ID == reviewSubjectOf(t, apiURI).URI() {
				return true
			}
		}
		return false
	}
	if !inFamily("x402") || inFamily("mpp") {
		t.Fatalf("protocol filter on a reviewed endpoint: x402 %v mpp %v", inFamily("x402"), inFamily("mpp"))
	}
	for _, h := range hits {
		if strings.Contains(h.ID, "weather-unlisted") {
			t.Fatal("an unlisted subject is never indexed")
		}
		if h.ID == keyAccount(t, s, coder) {
			t.Fatal("coder does not match weather")
		}
	}
	if res.Data["parsed_by"] != "rules" || res.Data["filters"].(find.Filters).Types == nil {
		t.Fatalf("parse %v %v", res.Data["parsed_by"], res.Data["filters"])
	}
	// Types narrow; a sentence without a model falls back to the rules and says so.
	res = run(t, s, Command{Operation: "find", Query: "an agent for weather in Lisbon", Kind: "agents"})
	hits = res.Data["results"].([]find.Hit)
	if len(hits) != 1 || hits[0].Name != "weatherbot" || res.Data["parse_note"] != "no model configured" {
		t.Fatalf("agents: %+v %v", hits, res.Data["parse_note"])
	}
	// A filter set to any clears what the words implied.
	res = run(t, s, Command{Operation: "find", Query: "weather mcp servers", Data: `{"schema":1,"type":"any"}`})
	if f := res.Data["filters"].(find.Filters); len(f.Types) != 0 || len(res.Data["results"].([]find.Hit)) < 3 {
		t.Fatalf("cleared: %+v", f)
	}
	res = run(t, s, Command{Operation: "find", Query: "weather mcp servers"})
	if f := res.Data["filters"].(find.Filters); !slices.Equal(f.Types, []string{find.TypeMCP}) {
		t.Fatalf("implied: %+v", f)
	}
	// Refusals.
	fails(t, s, Command{Operation: "find", Kind: "people"}, "invalid_find")
	fails(t, s, Command{Operation: "find", Data: `{"schema":1,"max_price":"cheap"}`}, "invalid_find")
	fails(t, s, Command{Operation: "find", Data: `{"schema":1,"sort":"price"}`}, "invalid_find")
	fails(t, s, Command{Operation: "find", Query: strings.Repeat("a", find.QueryMaxBytes+1)}, "invalid_find")
	fails(t, s, Command{Operation: "find", Cursor: "999"}, "invalid_find")
	if c := s.FindCapabilities(); c["read"] != "find" || c["order"] != find.OrderNote {
		t.Fatalf("capabilities %v", c)
	}
	// What was just published is found by the next search.
	fresh := keyFor(96)
	run(t, s, signed(fresh, Command{Operation: "agent.register", Handle: "zephyrine"}))
	if hits := run(t, s, Command{Operation: "find", Query: "zephyrine"}).Data["results"].([]find.Hit); len(hits) != 1 || hits[0].Name != "zephyrine" {
		t.Fatalf("fresh agent: %+v", hits)
	}
	// A site only an operator importer rated is listed (C215) and found.
	importer := keyFor(97)
	register(t, s, importer)
	allowAccounts(t, s, ImportersParamsNamespace, importer)
	stamp := strings.Repeat("ef", 32)
	if _, err := s.db.Exec("INSERT INTO notary_receipts(hash,time,account) VALUES(?,?,?)", stamp, testTime, keyID(importer)); err != nil {
		t.Fatal(err)
	}
	siteURI := "site:https://quillfeather.example.com/"
	run(t, s, reviewPost(t, importer, siteURI, map[string]any{"relationship": "outside_source", "evidence": []string{"stamp:" + stamp},
		"source": map[string]any{"name": "Index", "url": "https://index.example/r/2", "terms": "CC BY", "grade": "OPEN"}}, ""))
	var listedAt int64
	if err := s.db.QueryRow("SELECT listed_at FROM subjects WHERE id=?", reviewSubjectOf(t, siteURI).ID).Scan(&listedAt); err != nil || listedAt == 0 {
		t.Fatalf("imported site not listed: %v %d", err, listedAt)
	}
	// Rebuilds after a change are spaced (the agent above just took one).
	if err := s.find.engine.Load().Rebuild(testContext); err != nil {
		t.Fatal(err)
	}
	if hits := run(t, s, Command{Operation: "find", Query: "quillfeather"}).Data["results"].([]find.Hit); len(hits) != 1 || hits[0].Type != find.TypeSite {
		t.Fatalf("imported site: %+v", hits)
	}
}

// keyAccount is a key's account fingerprint.
func keyAccount(t *testing.T, s *Store, key ed25519.PrivateKey) string {
	t.Helper()
	var account string
	if err := s.db.QueryRow("SELECT account FROM identities WHERE id=?", keyID(key)).Scan(&account); err != nil {
		t.Fatal(err)
	}
	return account
}

// Find without REVIEWS still finds agents, and never reads subjects.
func TestFindReviewsOff(t *testing.T) {
	s := openTest(t, Config{})
	bot := keyFor(95)
	run(t, s, signed(bot, Command{Operation: "agent.register", Handle: "weatherbot"}))
	res := run(t, s, Command{Operation: "find", Query: "weatherbot"})
	if hits := res.Data["results"].([]find.Hit); len(hits) != 1 || hits[0].Type != find.TypeAgent {
		t.Fatalf("%+v", hits)
	}
	if strings.Contains(strings.Join(s.FindCapabilities()["index"].(map[string]any)["sources"].([]string), ","), "subjects") {
		t.Fatal("subjects listed as a source while REVIEWS is off")
	}
}

func TestFindQueryData(t *testing.T) {
	q := map[string][]string{"q": {"x"}, "type": {"agents"}, "max_price": {"any"}, "explain": {"true"}}
	data, err := FindQueryData(q)
	if err != nil || data != `{"explain":true,"max_price":"any","schema":1,"type":"agents"}` || len(q) != 1 {
		t.Fatalf("%s %v %v", data, err, q)
	}
	if _, err = FindQueryData(map[string][]string{"type": {"a", "b"}}); err == nil {
		t.Fatal("twice")
	}
}
