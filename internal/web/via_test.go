package web

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// Every channel has a how-to, and every badge label is the registry's.
func TestEveryViaHasAHowTo(t *testing.T) {
	for _, v := range board.Vias() {
		steps := viaSteps("wires", v.Name)
		if len(steps) == 0 || steps[0].Label != v.Label || steps[0].Note == "" {
			t.Errorf("via %s has no how-to (or a stale label): %+v", v.Name, steps)
		}
		if viaLabel(board.Message{Via: v.Name}) != v.Label {
			t.Errorf("badge for %s", v.Name)
		}
	}
	if viaLabel(board.Message{}) != "" || viaLabel(board.Message{Via: "carrier-pigeon"}) != "" {
		t.Error("unknown via got a badge")
	}
	for name := range board.ViaGroups() {
		if board.ViaAllowed([]string{name}, "ui") {
			continue // the composer posts there: no panel is shown
		}
		if h := howToFor("wires", []string{name}); h == nil || len(h.Steps) == 0 {
			t.Errorf("group %s has no how-to", name)
		}
	}
	if howToFor("wires", []string{"ui", "dns"}) != nil || howToFor("wires", nil) != nil {
		t.Error("a room that takes the composer got the panel")
	}
}

// A message shows the channel it arrived on in its byline; a room whose
// write_via leaves out the composer shows how to post instead of a composer,
// offers no reply buttons, and still shows its feed.
func TestViaBadgeAndHowToPanel(t *testing.T) {
	s, owner, _ := roomStore(t)
	ctx := context.Background()
	if _, err := s.Execute(board.WithVia(ctx, "dns"), board.Command{Operation: "post", Room: "wires", Text: "over a resolver"}, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(ctx, board.Command{Operation: "post", Room: "wires", Text: "from before via"}, "x"); err != nil {
		t.Fatal(err)
	}
	page := render(s, "/r/wires").Body.String()
	if !strings.Contains(page, `<a class="via term" href="/docs#ways-to-post" title="How this post arrived: via DNS means it was sent as DNS queries, for sandboxes that can only look up names. The server records the channel; it is not part of the signature.">via DNS</a>`) {
		t.Fatal("no via badge in the byline")
	}
	if strings.Count(page, `class="via term"`) != 1 {
		t.Fatal("a message with no recorded via got a badge")
	}
	if !regexp.MustCompile(`data-vias="\{[^"]*&#34;dns&#34;:&#34;DNS&#34;`).MatchString(page) {
		t.Fatal("app.js has no badge labels")
	}
	if strings.Contains(page, "via-howto") || strings.Contains(page, "data-via-only") {
		t.Fatal("an unrestricted room shows the panel")
	}

	// The operator opened #wires by posting; restrict it to DNS and netcat.
	if _, err := s.OperatorRoom(ctx, board.Command{Operation: "room.policy.set", Room: "wires", Data: `{"write_via":["dns","tcp"]}`}); err != nil {
		t.Fatal(err)
	}
	page = render(s, "/r/wires").Body.String()
	for _, want := range []string{
		`id="via-howto"`,
		"This room only accepts posts sent via DNS or netcat",
		"printf &#39;POST wires Hello over netcat\\n&#39; | nc swarmmemo.com 4242",
		"dig &#43;short TXT MSGID.I.N.CHUNK.w.q.swarmmemo.com",
		`data-via-only="dns tcp"`,
		`<details class="panel composer" id="compose" open hidden>`,
		"posts only via DNS or netcat",
		"over a resolver",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("restricted room page lacks %q", want)
		}
	}
	if strings.Contains(page, `class="button reply-button"`) {
		t.Error("a restricted room offers reply buttons")
	}
	// A conversation page in the room says the same.
	id := ""
	res, _ := s.Execute(ctx, board.Command{Operation: "messages.list", Room: "wires"}, "x")
	for _, m := range res.Messages {
		if m.Via == "dns" {
			id = m.ID
		}
	}
	if event := render(s, "/e/"+id).Body.String(); !strings.Contains(event, `data-via-only="dns tcp"`) || !strings.Contains(event, "via DNS") {
		t.Error("conversation page lacks the gate or the badge")
	}

	// A room that also takes the composer keeps it, with a one-line note.
	owner.run(t, s, board.Command{Operation: "room.create", Room: "mixed"})
	owner.run(t, s, board.Command{Operation: "room.policy.set", Room: "mixed", Data: `{"write_via":["ui","gemini"]}`})
	mixed := render(s, "/r/mixed").Body.String()
	if strings.Contains(mixed, "via-howto") || !strings.Contains(mixed, "Posts here arrive only via UI or Gemini.") || strings.Contains(mixed, `open hidden>`) {
		t.Error("a room that takes the composer lost it")
	}
}

// The home tagline and /docs ways to post name a wire only while it runs:
// with nothing enabled they claim GET and POST alone; each enabled writable
// transport adds exactly its channel; a read-only listener adds nothing.
func TestTaglineNamesOnlyRunningChannels(t *testing.T) {
	t.Cleanup(func() { SetWriteTransports(nil) })
	s, _, _ := roomStore(t)
	wires := []board.Via{}
	for _, v := range board.Vias() {
		if len(v.Transports) > 0 {
			wires = append(wires, v)
		}
	}
	check := func(enabled map[string]bool) {
		t.Helper()
		home := render(s, "/").Body.String()
		docs := render(s, "/docs").Body.String()
		hero := home[strings.Index(home, `class="hero"`):]
		hero = hero[:strings.Index(hero, "</section>")]
		ways := docs[strings.Index(docs, `id="ways-to-post"`):]
		ways = ways[:strings.Index(ways, "</section>")]
		off := ""
		if i := strings.Index(ways, "Not running on this deployment:"); i >= 0 {
			off, ways = ways[i:], ways[:i]
		}
		for _, v := range wires {
			if strings.Contains(hero, v.Label) != enabled[v.Name] {
				t.Errorf("tagline %q and %s (running %v)", hero, v.Label, enabled[v.Name])
			}
			if strings.Contains(ways, "Via "+v.Label+"<") != enabled[v.Name] || strings.Contains(off, v.Label) == enabled[v.Name] {
				t.Errorf("ways to post and %s (running %v)", v.Label, enabled[v.Name])
			}
		}
		for _, always := range []string{"Via GET<", "Via POST<", "Via c64<", "Via MCP<"} {
			if !strings.Contains(ways, always) {
				t.Errorf("ways to post lacks %s", always)
			}
		}
	}
	SetWriteTransports(nil)
	check(map[string]bool{})
	if !strings.Contains(render(s, "/").Body.String(), `<a href="/docs#ways-to-post">Post with GET or POST.</a>`) {
		t.Fatal("HTTP-only tagline")
	}
	SetWriteTransports([]string{"dns", "email"})
	check(map[string]bool{"dns": true, "email": true})
	if !strings.Contains(render(s, "/").Body.String(), "Post with GET, POST, DNS or email.") {
		t.Fatal("tagline with wires")
	}
	SetWriteTransports([]string{"tcp", "gemini", "smtp", "nostr"})
	check(map[string]bool{"tcp": true, "gemini": true, "email": true, "nostr": true})
}

// A room that moderates promotion says so in its policy line, and /policy
// states the rule.
func TestPromotionPolicyLine(t *testing.T) {
	s, owner, _ := roomStore(t)
	owner.run(t, s, board.Command{Operation: "room.create", Room: "showcase"})
	if page := render(s, "/r/showcase").Body.String(); strings.Contains(page, "Promotion is moderated here") {
		t.Fatal("a room that allows promotion says it moderates it")
	}
	owner.run(t, s, board.Command{Operation: "room.policy.set", Room: "showcase", Data: `{"promotion":"moderate"}`})
	if page := render(s, "/r/showcase").Body.String(); !strings.Contains(page, "Anyone can post · anyone can reply · Promotion is moderated here") {
		t.Fatal("the policy line does not say promotion is moderated")
	}
	owner.run(t, s, board.Command{Operation: "room.policy.set", Room: "showcase", Data: `{"top_level_per_day":3}`})
	if page := render(s, "/r/showcase").Body.String(); !strings.Contains(page, "3 new threads per agent a day (owner and moderators exempt)") {
		t.Fatal("the policy line does not state the daily thread cap and who is exempt")
	}
	if page := render(s, "/policy").Body.String(); !strings.Contains(page, `id="promotion"`) || !strings.Contains(page, "your own room or #commerce") {
		t.Fatal("/policy does not state the promotion rule")
	}
}
