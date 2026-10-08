package board

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
)

func TestNicknameIsStableAndDerivedOnly(t *testing.T) {
	const fingerprint = "9cf9c2894cac0f1c2d3e4f5061728394a5b6c7d8e9fa0b1c2d3e4f5061728394"
	first := Nickname(fingerprint)
	if first == "" || !strings.Contains(first, "-") {
		t.Fatalf("no name for a valid fingerprint: %q", first)
	}
	if again := Nickname(fingerprint); again != first {
		t.Fatalf("name is not stable: %q then %q", first, again)
	}
	// Case must not change the name: the same key written either way is the same key.
	if upper := Nickname(strings.ToUpper(fingerprint)); upper != first {
		t.Errorf("case changed the name: %q vs %q", first, upper)
	}
	// Only the leading bytes decide, so a name says nothing about the rest of the key.
	if tail := Nickname(fingerprint[:8] + strings.Repeat("0", 56)); tail != first {
		t.Errorf("name depends on bytes beyond the prefix: %q vs %q", first, tail)
	}
	for _, bad := range []string{"", "short", "zzzzzzzz", "9cf9c28"} {
		if name := Nickname(bad); name != "" {
			t.Errorf("named an unusable fingerprint %q as %q", bad, name)
		}
	}
}

func TestNicknamesSpreadAcrossKeys(t *testing.T) {
	// Names exist to tell participants apart in one conversation. With 4096 names,
	// collisions are expected across a large sample and acceptable because the
	// fingerprint is always shown; what would not be acceptable is clustering.
	seen := map[string]int{}
	for i := 0; i < 2000; i++ {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		name := Nickname(hex.EncodeToString(raw))
		if name == "" {
			t.Fatal("random fingerprint produced no name")
		}
		seen[name]++
	}
	if len(seen) < 1200 {
		t.Errorf("only %d distinct names across 2000 keys; the mapping is clustering", len(seen))
	}
	worst := 0
	for _, count := range seen {
		if count > worst {
			worst = count
		}
	}
	if worst > 12 {
		t.Errorf("one name claimed %d of 2000 keys; the mapping is uneven", worst)
	}
}

func TestNicknameWordListsAreUsable(t *testing.T) {
	if len(nameAdjectives) != 64 || len(nameNouns) != 64 {
		t.Fatalf("expected 64 of each word, got %d and %d", len(nameAdjectives), len(nameNouns))
	}
	for _, list := range [][]string{nameAdjectives, nameNouns} {
		seen := map[string]bool{}
		for _, word := range list {
			if seen[word] {
				t.Errorf("duplicate word %q wastes a name", word)
			}
			seen[word] = true
			if word != strings.ToLower(word) || strings.ContainsAny(word, " -") {
				t.Errorf("word %q is not a plain lowercase token", word)
			}
		}
	}
}

func TestAnonTagOnlyFromADailyPseudonym(t *testing.T) {
	for account, want := range map[string]string{
		"anon:d092" + strings.Repeat("a", 28): "d092",
		"anon:" + strings.Repeat("0f", 32):    "", // legacy: unsalted sha256 of the source
		"anon:D092" + strings.Repeat("a", 28): "",
		"anon:d09":                            "",
		"d092" + strings.Repeat("a", 33):      "",
		"anon:zz92" + strings.Repeat("a", 28): "",
		strings.Repeat("a", 64):               "",
	} {
		if got := AnonTag(account); got != want {
			t.Errorf("AnonTag(%q) = %q, want %q", account, got, want)
		}
	}
}

// C67/C68: a read says where a byline's name came from. A claimed handle is a
// handle; a key without one is named by the board and says so; an unsigned
// post carries only a short tag of its stored daily pseudonym, the same for one
// network all day and never more than AnonTagChars of it.
func TestMessageNamesSayWhereTheNameCameFrom(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1, Features: Features{AnonPrefix: true}})
	unnamed, named := keyFor(61), keyFor(62)
	run(t, s, signed(unnamed, Command{Operation: "post", Text: "no handle"}))
	run(t, s, signed(named, Command{Operation: "post", Text: "with handle", Handle: "claimed-name"}))
	for _, source := range []string{"198.51.100.7", "198.51.100.200", "203.0.113.9"} {
		if _, err := s.Execute(testContext, Command{Operation: "post", Text: "from " + source}, source); err != nil {
			t.Fatal(err)
		}
	}
	stored := map[string]string{}
	rows, err := s.db.Query("SELECT text,account FROM events")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var text, account string
		if err = rows.Scan(&text, &account); err != nil {
			t.Fatal(err)
		}
		stored[text] = account
	}
	rows.Close()
	got := map[string]Message{}
	for _, m := range run(t, s, Command{Operation: "messages.list"}).Messages {
		got[m.Text] = m
	}
	if m := got["no handle"]; m.NameSource != NameSourceGenerated || m.Nickname != Nickname(keyID(unnamed)) || m.Nickname == "" || m.AnonTag != "" {
		t.Fatalf("unnamed key: %+v", m)
	}
	if m := got["with handle"]; m.NameSource != NameSourceHandle || m.Nickname != "" || m.AuthorHandle != "claimed-name" {
		t.Fatalf("named key: %+v", m)
	}
	same, other := got["from 198.51.100.7"], got["from 203.0.113.9"]
	if same.AnonTag == "" || same.AnonTag != got["from 198.51.100.200"].AnonTag || len(same.AnonTag) != AnonTagChars {
		t.Fatalf("one /24 on one day must share a tag: %q %q", same.AnonTag, got["from 198.51.100.200"].AnonTag)
	}
	if other.AnonTag != stored["from 203.0.113.9"][len("anon:"):][:AnonTagChars] || same.AnonTag != stored["from 198.51.100.7"][len("anon:"):][:AnonTagChars] {
		t.Fatal("the tag must be the stored pseudonym's prefix, nothing computed anew")
	}
	if same.NameSource != "" || same.Nickname != "" {
		t.Fatalf("an unsigned post claims no name source: %+v", same)
	}
	agent := run(t, s, Command{Operation: "agent.get", Target: keyID(unnamed)}).Agent
	if agent.NameSource != NameSourceGenerated || agent.Nickname != Nickname(keyID(unnamed)) {
		t.Fatalf("agent without a handle: %+v", agent)
	}
	if agent = run(t, s, Command{Operation: "agent.get", Target: keyID(named)}).Agent; agent.NameSource != NameSourceHandle || agent.Nickname != "" {
		t.Fatalf("agent with a handle: %+v", agent)
	}
}
