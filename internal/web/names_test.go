package web

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
)

func TestAgentNicknameIsStableAndDerivedOnly(t *testing.T) {
	const fingerprint = "9cf9c2894cac0f1c2d3e4f5061728394a5b6c7d8e9fa0b1c2d3e4f5061728394"
	first := AgentNickname(fingerprint)
	if first == "" || !strings.Contains(first, "-") {
		t.Fatalf("no name for a valid fingerprint: %q", first)
	}
	if again := AgentNickname(fingerprint); again != first {
		t.Fatalf("name is not stable: %q then %q", first, again)
	}
	// Case must not change the name: the same key written either way is the same key.
	if upper := AgentNickname(strings.ToUpper(fingerprint)); upper != first {
		t.Errorf("case changed the name: %q vs %q", first, upper)
	}
	// Only the leading bytes decide, so a name says nothing about the rest of the key.
	if tail := AgentNickname(fingerprint[:8] + strings.Repeat("0", 56)); tail != first {
		t.Errorf("name depends on bytes beyond the prefix: %q vs %q", first, tail)
	}
	for _, bad := range []string{"", "short", "zzzzzzzz", "9cf9c28"} {
		if name := AgentNickname(bad); name != "" {
			t.Errorf("named an unusable fingerprint %q as %q", bad, name)
		}
	}
}

func TestAgentNicknamesSpreadAcrossKeys(t *testing.T) {
	// Names exist to tell participants apart in one conversation. With 4096 names,
	// collisions are expected across a large sample and acceptable because the
	// fingerprint is always shown; what would not be acceptable is clustering.
	seen := map[string]int{}
	for i := 0; i < 2000; i++ {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		name := AgentNickname(hex.EncodeToString(raw))
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
