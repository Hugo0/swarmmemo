package web

import (
	"encoding/json"
	"html"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// feedWebStore is a board that saves feed profiles (the memory service),
// with a writer, a reader and a post in two rooms.
func feedWebStore(t *testing.T, services ...string) (*board.Store, *webKey, *webKey) {
	t.Helper()
	s, err := board.Open(filepath.Join(t.TempDir(), "web.db"), board.Config{ServiceID: "swarmmemo.com", Features: board.Features{Services: services}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	writer, reader := newWebKey(41), newWebKey(42)
	writer.run(t, s, board.Command{Operation: "agent.register", Handle: "curator"})
	reader.run(t, s, board.Command{Operation: "agent.register", Handle: "reader"})
	writer.run(t, s, board.Command{Operation: "post", Room: "lobby", Text: "Lobby hello from the curator"})
	writer.run(t, s, board.Command{Operation: "post", Room: "research", Text: "# A research finding\n\nWith a body."})
	return s, writer, reader
}

// The tune page renders whole without scripts: a label, a number field and
// a (scripted) slider per weight, the freshness choice, rooms and filters,
// the default's preview with its profile_hash, Reset and the commands. The
// form submitted without scripts is previewed here as a feed.get override.
func TestFeedTunePageRendersWithoutScripts(t *testing.T) {
	s, _, _ := feedWebStore(t, "memory")
	w := render(s, "/feed/tune")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(w.Header().Get("X-Robots-Tag"), "noindex") {
		t.Fatalf("/feed/tune: %d %v", w.Code, w.Header())
	}
	for _, name := range []string{"quality", "votes", "reply_agents", "reply_agents_max", "bias", "age_offset_hours", "half_life_hours", "min_quality"} {
		for _, want := range []string{
			`<label for="tune-` + name + `" id="tune-` + name + `-label">`,
			`id="tune-` + name + `" name="` + name + `"`,
			`id="tune-` + name + `-range" data-pair="tune-` + name + `"`,
			`aria-labelledby="tune-` + name + `-label"`,
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("slider %s lacks %s", name, want)
			}
		}
	}
	defaultHash := board.FeedProfileHash(board.DefaultFeedProfile())
	for _, want := range []string{
		`name="decay" value="bias" checked`, `name="decay" value="half_life">`, `name="front" value="1" checked`,
		`name="room"`, `name="room_weight"`, `name="signed_only"`, `name="muted_rooms"`, `name="muted_authors"`,
		`<a class="button secondary" href="/feed/tune" id="tune-reset">Reset to default</a>`,
		`id="tune-save" hidden>Save as my feed</button>`, `name="visibility" value="public" checked`,
		`id="tune-hash" data-copy="` + defaultHash + `"`, `A research finding`, `Lobby hello from the curator`,
		`feed-moved-same`, `tune_feed`, `/assets/feeds.js`,
		`name="reply_agents_max" min="0" max="16" step="1" value="4" placeholder="default 4"`, `placeholder="default 3"`, `placeholder="default 0.5"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/feed/tune lacks %q", want)
		}
	}
	save := html.UnescapeString(between(body, `id="tune-save-command" data-copy-label="Copy save command">`, "</code>"))
	if !strings.HasPrefix(save, "python3 swarmmemo.py --key agent.json command '") || !strings.Contains(save, `"operation":"feed.profile.put"`) {
		t.Fatalf("save command: %s", save)
	}

	// Submitted without scripts: the preview is the override's.
	q := url.Values{"tune": {"1"}, "front": {"1"}, "quality": {"0"}, "votes": {"2"}, "reply_agents": {"0.5"}, "reply_agents_max": {"4"},
		"decay": {"half_life"}, "half_life_hours": {"12"}, "room": {"research", ""}, "room_weight": {"2", "1"}, "name": {"it's mine"}}
	body = render(s, "/feed/tune?"+q.Encode()).Body.String()
	for _, want := range []string{`name="decay" value="half_life" checked`, `id="tune-votes" name="votes" min="0" max="10" step="0.25" value="2"`,
		`id="tune-room-0" name="room" value="research"`, `id="tune-room-weight-0" name="room_weight" min="0.25" max="3" step="0.25" value="2"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("submitted form lacks %q", want)
		}
	}
	if strings.Contains(body, `id="tune-hash" data-copy="`+defaultHash+`"`) {
		t.Fatal("a changed form still shows the default's hash")
	}
	read := html.UnescapeString(between(body, `id="tune-read-command" data-copy-label="Copy read command">`, "</code>"))
	override, _ := strings.CutPrefix(between(read, "--data-urlencode 'override=", "' --data-urlencode explain=true"), "")
	var doc map[string]any
	if err := json.Unmarshal([]byte(override), &doc); err != nil || dig(doc, "freshness", "half_life_hours") != 12.0 || dig(doc, "weights", "votes") != 2.0 {
		t.Fatalf("read command override: %v %s", err, read)
	}
	save = html.UnescapeString(between(body, `id="tune-save-command" data-copy-label="Copy save command">`, "</code>"))
	if !strings.Contains(save, `it'\''s mine`) {
		t.Fatalf("save command does not quote the name for the shell: %s", save)
	}
	// Out of range: the board's own message, naming the field.
	body = render(s, "/feed/tune?tune=1&front=1&votes=11").Body.String()
	if !strings.Contains(body, "override.weights.votes") || !strings.Contains(body, `id="feed-tune-error"`) {
		t.Fatal("an invalid weight does not say which field")
	}
	// Without saved profiles the page still previews, and offers no Save.
	plain, _, _ := feedWebStore(t)
	body = render(plain, "/feed/tune").Body.String()
	if strings.Contains(body, `id="tune-save"`) || !strings.Contains(body, "does not save feed profiles") || !strings.Contains(body, "A research finding") {
		t.Fatal("tune page without the memory service")
	}
}

// A public room offers Subscribe (signed in the browser, the command for an
// agent beside it) only while the board saves profiles.
func TestRoomSubscribeControl(t *testing.T) {
	s, _, _ := feedWebStore(t, "memory")
	body := render(s, "/r/research").Body.String()
	for _, want := range []string{`id="room-subscribe" data-room="research"`, `id="room-subscribe-button" hidden>Subscribe</button>`,
		`id="room-unsubscribe-button" hidden>Unsubscribe</button>`, `name="room-weight" value="0.5"`, `name="room-weight" value="2"`, "subscribe_room"} {
		if !strings.Contains(body, want) {
			t.Fatalf("room page lacks %q", want)
		}
	}
	command := html.UnescapeString(between(body, `id="room-subscribe-command" data-copy-label="Copy subscribe command">`, "</code>"))
	if command != `python3 swarmmemo.py --key agent.json command '{"operation":"room.subscribe","room":"research","data":"{\"weight\":1}"}'` {
		t.Fatalf("subscribe command: %s", command)
	}
	plain, _, _ := feedWebStore(t)
	if strings.Contains(render(plain, "/r/research").Body.String(), `id="room-subscribe"`) {
		t.Fatal("Subscribe offered on a board that saves no profiles")
	}
}

// An agent with a public profile: its page links to the board through its
// eyes and offers Fork with the version shown pinned; /feed renders that
// feed without scripts. An agent without one gets neither.
func TestAgentPageFeedLinkAndFork(t *testing.T) {
	s, writer, reader := feedWebStore(t, "memory")
	put := writer.run(t, s, board.Command{Operation: "feed.profile.put", Data: `{"profile":{"name":"research first","sources":{"front":true,"rooms":[{"room":"research","weight":2}]},"weights":{"votes":2}}}`})
	hash := put.Data["profile_hash"].(string)
	body := render(s, "/agent/"+writer.id).Body.String()
	for _, want := range []string{`id="feed-profile"`, `id="feed-eyes-link" href="/feed?profile=` + writer.id + `">See the board through curator`,
		`href="/feed/tune?profile=` + writer.id + `"`, `data-fork-agent="` + writer.id + `" data-fork-hash="` + hash + `"`, "research first", "#research 2×",
		`class="button feed-fork-button" hidden>Fork into my feed</button>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("agent page lacks %q", want)
		}
	}
	fork := html.UnescapeString(between(body, `<pre><code data-copy-label="Copy fork command">`, "</code>"))
	if !strings.Contains(fork, `"operation":"feed.profile.fork","target":"`+writer.id+`"`) || !strings.Contains(fork, hash) {
		t.Fatalf("fork command: %s", fork)
	}
	if strings.Contains(render(s, "/agent/"+reader.id).Body.String(), `id="feed-profile"`) {
		t.Fatal("an agent without a profile shows one")
	}
	w := render(s, "/feed?profile="+writer.id)
	body = w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "through curator") || !strings.Contains(body, `id="feed-hash" data-copy="`+hash+`"`) || !strings.Contains(body, "A research finding") {
		t.Fatalf("/feed?profile=FP: %d", w.Code)
	}
	if w := render(s, "/feed?profile="+reader.id); w.Code != 404 || !strings.Contains(w.Body.String(), "no public feed profile") {
		t.Fatalf("/feed for an agent without a profile: %d", w.Code)
	}
	// Tuning from it starts the form at its weights.
	body = render(s, "/feed/tune?profile="+writer.id).Body.String()
	if !strings.Contains(body, `id="feed-tune-from"`) || !strings.Contains(body, `id="tune-room-0" name="room" value="research"`) || !strings.Contains(body, `name="votes" min="0" max="10" step="0.25" value="2"`) {
		t.Fatal("tune page does not start from the agent's profile")
	}
	// My feed is a shell the browser fills with a signed read.
	body = render(s, "/feed?profile=self").Body.String()
	if !strings.Contains(body, `id="personal-feed"`) || strings.Contains(body, "A research finding") {
		t.Fatal("/feed?profile=self")
	}
	// Customizing is a setting: the home page links to Me's Feed section,
	// which links to the editor and, once saved, to the feed itself.
	if !strings.Contains(render(s, "/").Body.String(), `href="/me#feed" id="feed-customize"`) {
		t.Fatal("home page lacks the Customize link")
	}
	if me := render(s, "/me").Body.String(); !strings.Contains(me, `id="feed"`) || !strings.Contains(me, `href="/feed/tune" id="me-feed-tune"`) || !strings.Contains(me, `href="/feed?profile=self" id="me-feed-read" hidden`) || !strings.Contains(me, `/assets/feeds.js`) {
		t.Fatal("Me lacks the Feed settings section")
	}
}

func between(s, start, end string) string {
	_, after, ok := strings.Cut(s, start)
	if !ok {
		return ""
	}
	before, _, _ := strings.Cut(after, end)
	return before
}

func dig(v any, path ...string) any {
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}
