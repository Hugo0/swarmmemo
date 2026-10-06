package services

import (
	"strings"
)

// robots.txt for fetch (RFC 9309): the rules of the groups naming our
// product token (FetchRobotsToken), or, when none does, of the groups for
// "*"; the longest matching rule decides, Allow winning a tie; "*" matches
// any run of characters and a final "$" anchors the end. Bounded: the file
// is read up to fetchRobotsBytes, and at most robotsRulesMax rules of at
// most robotsRuleBytes each, robotsPatternBytes in all, are kept; a match
// costs at most len(pattern) × len(path) steps, so one decision is bounded
// by robotsPatternBytes × FetchURLBytes.

const (
	robotsRulesMax     = 2000
	robotsRuleBytes    = 1024
	robotsPatternBytes = 32 << 10
)

type robotsRule struct {
	allow   bool
	pattern string
}

// robotsRules are the rules that apply to us; none means everything is
// allowed, and disallowAll (the file could not be fetched) refuses all.
type robotsRules struct {
	rules       []robotsRule
	disallowAll bool
}

// parseRobots reads a robots.txt body for the product token agent
// (lowercase).
func parseRobots(body string, agent string) robotsRules {
	type group struct {
		agents []string
		rules  []robotsRule
	}
	var groups []*group
	var cur *group
	lastWasAgent := false
	total, kept := 0, 0
	for len(body) > 0 {
		line := body
		if i := strings.IndexAny(body, "\r\n"); i >= 0 {
			line, body = body[:i], body[i+1:]
		} else {
			body = ""
		}
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		switch key {
		case "user-agent":
			if !lastWasAgent || cur == nil {
				cur = &group{}
				groups = append(groups, cur)
			}
			token := strings.ToLower(value)
			if i := strings.IndexAny(token, "/ \t"); i >= 0 {
				token = token[:i]
			}
			cur.agents = append(cur.agents, token)
			lastWasAgent = true
		case "allow", "disallow":
			lastWasAgent = false
			if cur == nil || value == "" || len(value) > robotsRuleBytes || total >= robotsRulesMax || kept+len(value) > robotsPatternBytes {
				continue // an empty disallow allows everything: no rule
			}
			cur.rules = append(cur.rules, robotsRule{allow: key == "allow", pattern: value})
			total++
			kept += len(value)
		default:
			lastWasAgent = false
		}
	}
	var mine, star []robotsRule
	matched := false
	for _, g := range groups {
		// A group's rules count once, however often it names an agent.
		ours, all := false, false
		for _, a := range g.agents {
			ours = ours || a == agent
			all = all || a == "*"
		}
		if ours {
			mine = append(mine, g.rules...)
			matched = true
		}
		if all {
			star = append(star, g.rules...)
		}
	}
	if matched {
		return robotsRules{rules: mine}
	}
	return robotsRules{rules: star}
}

// allowed reports whether path (with its query, starting with "/") may be
// fetched.
func (r robotsRules) allowed(path string) bool {
	if r.disallowAll {
		return false
	}
	if path == "" {
		path = "/"
	}
	best, allow := -1, true
	for _, rule := range r.rules {
		if !robotsMatch(rule.pattern, path) {
			continue
		}
		n := len(rule.pattern)
		if n > best || (n == best && rule.allow) {
			best, allow = n, rule.allow
		}
	}
	return allow
}

// robotsMatch matches a rule's pattern from the start of path: "*" is any
// run of characters, and a "$" at the end anchors the end of the path.
// Greedy with one backtrack point, so it is O(len(pattern)*len(path)).
func robotsMatch(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = pattern[:len(pattern)-1]
	}
	p, s := 0, 0
	star, mark := -1, 0
	for s < len(path) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, s
			p++
		case p < len(pattern) && pattern[p] == path[s]:
			p++
			s++
		case p == len(pattern) && !anchored:
			return true // a prefix match
		case star >= 0:
			p = star + 1
			mark++
			s = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
