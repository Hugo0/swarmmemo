package web

import (
	"fmt"
	"regexp"
	"strings"

	"swarmmemo/docs"
)

// The lexicon is docs/GLOSSARY.md's fenced blocks: the positioning lines
// every surface uses to say what SwarmMemo is, and the words kept out of an
// agent's first run. Code reads them here and nowhere else; the static files
// (READMEs, the registry record, the plugin) take them through go generate
// (TestLexiconStaticFiles), so a line is changed in the glossary only.
//
// The glossary is embedded, so the blocks parse the same in every build:
// TestLexiconBlocksParse fails on a broken block, and a binary built from
// one refuses to start (init panics) rather than serve empty copy.

// LexiconEntry is one `key => value` line of a glossary block.
type LexiconEntry struct{ Key, Value string }

// PositioningLines are the positioning block's values.
type PositioningLines struct {
	// Category is the tagline (Hugo, 2026-10-15), kept exactly.
	Category string
	// Descriptor is what SwarmMemo gives, in the words people search with.
	Descriptor string
	// Registry extends Category, unchanged, to what a directory listing
	// (the MCP Registry) needs in its own character limit.
	Registry string
	// Summary is the line under the home page's heading and its meta
	// description, within the 160 characters search results show.
	Summary string
	// Free is the cost in three words; Limit is the cost with the limit.
	Free, Limit string
	// Props are what an agent gets: reach, work, record, in that order.
	Props [3]Prop
	// Tools is the line for agents that can only open a URL.
	Tools string
}

// Prop is one value proposition: its lead (the words before the first
// colon, shown bold) and the rest.
type Prop struct{ Key, Lead, Rest string }

// Text is the prop as one sentence.
func (p Prop) Text() string { return p.Lead + ": " + p.Rest }

// propKeys are the positioning block's props, in order.
var propKeys = [3]string{"prop.reach", "prop.work", "prop.record"}

// positioningKeys are every key the positioning block must have.
var positioningKeys = append([]string{"category", "registry", "descriptor", "summary", "free", "limit", "tools"}, propKeys[:]...)

// FirstRunWord is one word that never appears before an agent's first
// success, with what to say instead.
type FirstRunWord struct{ Word, Instead string }

// Pattern matches the word whole and in any case.
func (w FirstRunWord) Pattern() *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(w.Word) + `\b`)
}

// lexicon is the glossary's blocks, parsed once.
var lexicon = func() struct {
	positioning PositioningLines
	firstRun    []FirstRunWord
} {
	glossary := string(docs.Glossary())
	p, err := parsePositioning(glossary)
	if err != nil {
		panic("docs/GLOSSARY.md: " + err.Error())
	}
	f, err := parseFirstRun(glossary)
	if err != nil {
		panic("docs/GLOSSARY.md: " + err.Error())
	}
	return struct {
		positioning PositioningLines
		firstRun    []FirstRunWord
	}{p, f}
}()

// Positioning is the glossary's positioning block.
func Positioning() PositioningLines { return lexicon.positioning }

// FirstRunWords is the glossary's first-run block.
func FirstRunWords() []FirstRunWord { return append([]FirstRunWord(nil), lexicon.firstRun...) }

// The positioning lines under the names the rest of the code uses.
var (
	// Tagline heads the home page, /for-agents, /docs, /llms.txt,
	// /capabilities, the server card and the A2A card; it
	// stays within 100 characters, the registry's limit for a description.
	Tagline = lexicon.positioning.Category
	// Registry is Tagline extended for a directory listing (the MCP
	// Registry record and the served MCP server card's description); it
	// stays within 100 characters, the registry's limit.
	Registry = lexicon.positioning.Registry
	// Descriptor is what SwarmMemo gives; the home page's <title> is
	// Tagline and Descriptor.
	Descriptor = lexicon.positioning.Descriptor
	// ShortDescription is the line under the home page's h1, its meta
	// description, /docs' lede, /llms.txt's summary and the MCP server's
	// serverInfo description.
	ShortDescription = lexicon.positioning.Summary
	// FreeShort is the cost in three words.
	FreeShort = lexicon.positioning.Free
	// FreeLimit is the cost with its limit.
	FreeLimit = lexicon.positioning.Limit
	// ToolsLine is the free tools, for agents that can only open a URL.
	ToolsLine = lexicon.positioning.Tools
)

// NameAndDescriptor names SwarmMemo with what it gives, for a sentence
// that mentions it in passing: "SwarmMemo (rooms, ... for AI agents)".
var NameAndDescriptor = "SwarmMemo (" + Descriptor + ")"

// Intro is the category line and what SwarmMemo gives, in two sentences
// with no colon (a skill's YAML description, a plugin's long description).
func Intro() string { return Tagline + " SwarmMemo gives " + Descriptor + "." }

// PropNamed is the positioning prop named key (reach, work, record).
func PropNamed(key string) Prop {
	for _, p := range lexicon.positioning.Props {
		if p.Key == key {
			return p
		}
	}
	panic("no positioning prop " + key)
}

// lowerFirst lowercases a sentence's first letter, to follow a colon.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// Lead is the one-line introduction that opens the MCP instructions and the
// plugin: the name, the category line and the descriptor as a sentence,
// short enough for the instructions' byte budget.
func Lead() string {
	return "SwarmMemo: " + Tagline + " " + strings.ToUpper(Descriptor[:1]) + Descriptor[1:] + "."
}

// LexiconBlock returns the `key => value` lines of the glossary's fenced
// block named name, in order. Blank lines are skipped; any other line that
// is not `key => value`, and a key given twice, is an error.
func LexiconBlock(glossary, name string) ([]LexiconEntry, error) {
	fence := "```" + name + "\n"
	start := strings.Index(glossary, fence)
	if start < 0 {
		return nil, fmt.Errorf("no ```%s block", name)
	}
	body := glossary[start+len(fence):]
	end := strings.Index(body, "```")
	if end < 0 {
		return nil, fmt.Errorf("the ```%s block is not closed", name)
	}
	if strings.Contains(glossary[start+len(fence)+end+3:], fence) {
		return nil, fmt.Errorf("two ```%s blocks", name)
	}
	var out []LexiconEntry
	seen := map[string]bool{}
	for _, line := range strings.Split(body[:end], "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, ok := strings.Cut(line, " => ")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" || value == "" {
			return nil, fmt.Errorf("```%s line %q is not \"key => value\"", name, line)
		}
		if seen[strings.ToLower(key)] {
			return nil, fmt.Errorf("```%s names %q twice", name, key)
		}
		seen[strings.ToLower(key)] = true
		out = append(out, LexiconEntry{key, value})
	}
	return out, nil
}

// parsePositioning reads the positioning block: every key in
// positioningKeys, no other.
func parsePositioning(glossary string) (PositioningLines, error) {
	entries, err := LexiconBlock(glossary, "positioning")
	if err != nil {
		return PositioningLines{}, err
	}
	values := map[string]string{}
	for _, e := range entries {
		known := false
		for _, k := range positioningKeys {
			known = known || k == e.Key
		}
		if !known {
			return PositioningLines{}, fmt.Errorf("```positioning has an unknown key %q", e.Key)
		}
		values[e.Key] = e.Value
	}
	for _, k := range positioningKeys {
		if values[k] == "" {
			return PositioningLines{}, fmt.Errorf("```positioning has no %q", k)
		}
	}
	p := PositioningLines{Category: values["category"], Registry: values["registry"], Descriptor: values["descriptor"], Summary: values["summary"],
		Free: values["free"], Limit: values["limit"], Tools: values["tools"]}
	for i, k := range propKeys {
		lead, rest, ok := strings.Cut(values[k], ": ")
		if !ok {
			return PositioningLines{}, fmt.Errorf("```positioning %s has no \"Lead: rest\"", k)
		}
		p.Props[i] = Prop{Key: strings.TrimPrefix(k, "prop."), Lead: lead, Rest: rest}
	}
	return p, nil
}

// parseFirstRun reads the first-run block.
func parseFirstRun(glossary string) ([]FirstRunWord, error) {
	entries, err := LexiconBlock(glossary, "first-run")
	if err != nil {
		return nil, err
	}
	out := make([]FirstRunWord, 0, len(entries))
	for _, e := range entries {
		out = append(out, FirstRunWord{Word: e.Key, Instead: e.Value})
	}
	return out, nil
}
