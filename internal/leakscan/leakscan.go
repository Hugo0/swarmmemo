// Package leakscan finds secrets, personal data and private infrastructure in
// text an agent is about to send (RFC0013 §5.3): deterministic patterns, the
// free half of screen.leak. One list serves the server, the web composer
// (internal/web/assets/leak-patterns.json) and the Python CLI (its
// LEAK_PATTERNS block), both written from PatternsJSON by go generate, and
// GET /api/screen/leak-patterns serves it.
//
// Every pattern is in the subset RE2, ECMAScript and Python's re agree on: no
// lookaround, no backreferences, no inline flags, only ASCII classes and
// non-capturing groups, except a rule's Group. None nests a quantifier over
// an ambiguous class, so a backtracking engine stays linear too. It imports
// only the standard library.
package leakscan

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// Version names this list; it changes whenever a rule does, and is the ETag
// of GET /api/screen/leak-patterns.
const Version = 1

// Categories are what a finding can be; screen.leak's classifier adds
// excess_code, which no pattern finds.
const (
	Credentials           = "credentials"
	Financial             = "financial"
	PersonalData          = "personal_data"
	PrivateInfrastructure = "private_infrastructure"
)

// Categories lists them in the order PatternsJSON gives.
var Categories = []string{Credentials, Financial, PersonalData, PrivateInfrastructure}

// What a finding does to a message about to be sent: Hold stops it until
// the sender confirms; Warn shows the findings and sends it.
const (
	Hold = "hold"
	Warn = "warn"
)

// Actions is the one category → action table (steward, 2026-09-30):
// credentials and financial numbers hold; contact details and private
// infrastructure warn. The CLI, the web composer and hosted MCP's preflight
// all read it from PatternsJSON, and an agent overrides a category with its
// outbound.actions setting. A category it does not name, such as the
// classifier's excess_code, holds.
var Actions = map[string]string{Credentials: Hold, Financial: Hold, PersonalData: Warn, PrivateInfrastructure: Warn}

// Action is what category does under overrides (an agent's own table),
// then Actions; hold for anything neither names.
func Action(category string, overrides map[string]string) string {
	if a, ok := overrides[category]; ok && (a == Hold || a == Warn) {
		return a
	}
	if a, ok := Actions[category]; ok {
		return a
	}
	return Hold
}

// Verdict is "hold" when a finding, or a classifier category scored at or
// above threshold, holds; "warn" when all of them only warn; "pass" when
// there are none. scores may be nil (patterns only).
func Verdict(findings []Finding, scores map[string]float64, threshold float64, overrides map[string]string) string {
	verdict := "pass"
	found := func(category string) {
		if Action(category, overrides) == Hold {
			verdict = Hold
		} else if verdict == "pass" {
			verdict = Warn
		}
	}
	for _, f := range findings {
		found(f.Category)
	}
	for category, score := range scores {
		if score >= threshold {
			found(category)
		}
	}
	return verdict
}

// MaxFindings bounds what Scan returns: a text past it is held anyway.
const MaxFindings = 256

// Rule is one pattern. Luhn keeps a match only when its digits pass the Luhn
// check (card numbers), Mod97 only when it is a valid IBAN. Group, when 1,
// makes the finding the pattern's one capturing group (the value of an
// assignment) rather than the whole match.
type Rule struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Pattern  string `json:"pattern"`
	Note     string `json:"note"`
	Luhn     bool   `json:"luhn,omitempty"`
	Mod97    bool   `json:"mod97,omitempty"`
	Group    int    `json:"group,omitempty"`
}

// Finding is one match: its rule, category and byte offsets in the text.
type Finding struct {
	Rule     string `json:"rule"`
	Category string `json:"category"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
}

// ci spells word case-insensitively with character classes: the one way all
// three engines agree on (none shares an inline flag).
func ci(word string) string {
	var b strings.Builder
	for _, r := range word {
		lo, up := strings.ToLower(string(r)), strings.ToUpper(string(r))
		if lo == up {
			b.WriteString(regexp.QuoteMeta(lo))
			continue
		}
		b.WriteString("[" + up + lo + "]")
	}
	return b.String()
}

func alt(words ...string) string {
	for i, w := range words {
		words[i] = ci(w)
	}
	return "(?:" + strings.Join(words, "|") + ")"
}

const (
	octet = `(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])`
	// A secret-looking value: letters and punctuation, then a digit, then
	// at least five more characters that are not quotes, spaces or a
	// template's "${", "<", "*" (placeholders are not secrets).
	secretValue = `([A-Za-z_./+=@#%!~^&-]{0,64}[0-9][^ \t\r\n"'$<{*,;]{5,256})`
)

// Rules are the patterns, most specific first: Redact labels overlapping
// findings with the earliest rule.
var Rules = []Rule{
	{ID: "pem_private_key", Category: Credentials, Note: "a PEM private key",
		Pattern: `-----BEGIN [A-Z0-9 ]{0,40}PRIVATE KEY(?: BLOCK)?-----[A-Za-z0-9+/=\r\n\t ]*(?:-----END [A-Z0-9 ]{0,40}PRIVATE KEY(?: BLOCK)?-----)?`},
	{ID: "aws_access_key", Category: Credentials, Note: "an AWS access key ID",
		Pattern: `\b(?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}\b`},
	{ID: "github_token", Category: Credentials, Note: "a GitHub token",
		Pattern: `\b(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat[_][A-Za-z0-9_]{22,255})`},
	{ID: "anthropic_key", Category: Credentials, Note: "an Anthropic API key",
		Pattern: `\bsk-ant-[a-z]{2,8}[0-9]{2}-[A-Za-z0-9_-]{20,}`},
	{ID: "openai_key", Category: Credentials, Note: "an OpenAI API key",
		Pattern: `\bsk-(?:(?:proj|svcacct|admin)-[A-Za-z0-9_-]{20,}|[A-Za-z0-9]{32,})`},
	{ID: "slack_token", Category: Credentials, Note: "a Slack token or webhook",
		Pattern: `\b(?:xox[abposr]-[A-Za-z0-9-]{10,}|hooks\.slack\.com/services/T[A-Za-z0-9]{6,}/B[A-Za-z0-9]{6,}/[A-Za-z0-9]{16,})`},
	{ID: "stripe_live", Category: Credentials, Note: "a Stripe live secret key",
		Pattern: `\b(?:sk|rk)_live_[A-Za-z0-9]{16,}`},
	{ID: "google_api_key", Category: Credentials, Note: "a Google API key",
		Pattern: `\bAIza[0-9A-Za-z_-]{35}`},
	{ID: "jwt", Category: Credentials, Note: "a JSON Web Token",
		Pattern: `\beyJ[A-Za-z0-9_-]{5,}\.eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{10,}`},
	{ID: "swarmmemo_hosted_token", Category: Credentials, Note: "a SwarmMemo hosted token or recovery code",
		Pattern: `\bsm[hr]1_[A-Za-z0-9_-]{40,}`},
	{ID: "browser_pkcs8_ed25519", Category: Credentials, Note: "an Ed25519 private key backup (PKCS8)",
		Pattern: `MC4CAQAwBQYDK2VwBCIEI[A-Za-z0-9+/_-]{43}|\b302e020100300506032b657004220420[0-9a-fA-F]{64}`},
	{ID: "url_credentials", Category: Credentials, Note: "a password in a URL", Group: 1,
		Pattern: `[A-Za-z][A-Za-z0-9+.-]{1,15}://[^/ \t\r\n:@"'<>]{1,64}:([^/ \t\r\n:@"'<>]{3,128})@`},
	{ID: "bearer_token", Category: Credentials, Note: "a bearer token", Group: 1,
		Pattern: `\b` + ci("bearer") + `[ \t]+([A-Za-z0-9._~+/-]{20,}=*)`},
	{ID: "generic_secret_assignment", Category: Credentials, Note: "a password, secret, token or key assigned a value", Group: 1,
		Pattern: `[A-Za-z0-9_.-]{0,32}` + alt("password", "passwd", "passphrase", "secret", "token", "api_key", "apikey", "api-key", "access_key", "private_key") +
			`[A-Za-z0-9_.-]{0,32}["']?[ \t]{0,4}[:=][ \t]{0,4}["']?` + secretValue},
	{ID: "card_number", Category: Financial, Note: "a payment card number", Luhn: true,
		Pattern: `\b[2-6][0-9](?:[ -]?[0-9]){11,17}\b`},
	{ID: "iban", Category: Financial, Note: "a bank account number (IBAN)", Mod97: true,
		Pattern: `\b[A-Z]{2}[0-9]{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{1,3})?\b`},
	{ID: "email", Category: PersonalData, Note: "an email address",
		Pattern: `\b[A-Za-z0-9._%+-]{1,64}@(?:[A-Za-z0-9-]{1,63}\.){1,8}[A-Za-z]{2,24}\b`},
	{ID: "phone_e164", Category: PersonalData, Note: "a phone number in international form",
		Pattern: `\+[1-9][0-9]{7,14}\b|\+[1-9][0-9]{0,2}[ .-][0-9]{1,5}(?:[ .-][0-9]{2,5}){2,4}\b`},
	{ID: "private_ipv4", Category: PrivateInfrastructure, Note: "a private, link-local or carrier-NAT IPv4 address",
		Pattern: `\b(?:10(?:\.` + octet + `){3}|172\.(?:1[6-9]|2[0-9]|3[01])(?:\.` + octet + `){2}|192\.168(?:\.` + octet + `){2}|169\.254(?:\.` + octet + `){2}|100\.(?:6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])(?:\.` + octet + `){2})\b`},
	{ID: "internal_hostname", Category: PrivateInfrastructure, Note: "an internal hostname (.internal, .local, .corp, .lan, .intranet)",
		Pattern: `\b(?:[A-Za-z0-9-]{1,63}\.){1,8}` + alt("internal", "local", "corp", "lan", "intranet") + `\b`},
}

var compiled = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(Rules))
	for i, r := range Rules {
		out[i] = regexp.MustCompile(r.Pattern)
		if out[i].NumSubexp() != r.Group {
			panic("leakscan: rule " + r.ID + " has capturing groups other than its Group")
		}
	}
	return out
}()

// Scan finds every rule's matches in text, sorted by offset: at most
// MaxFindings. Matching is linear in the text (RE2).
func Scan(text string) []Finding {
	var out []Finding
	for i, re := range compiled {
		r := Rules[i]
		for _, m := range re.FindAllStringSubmatchIndex(text, MaxFindings) {
			start, end := m[0], m[1]
			if r.Group == 1 {
				start, end = m[2], m[3]
			}
			if start < 0 || start == end || r.Luhn && !luhn(text[start:end]) || r.Mod97 && !mod97(text[start:end]) {
				continue
			}
			out = append(out, Finding{Rule: r.ID, Category: r.Category, Start: start, End: end})
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Start != out[b].Start {
			return out[a].Start < out[b].Start
		}
		return out[a].End > out[b].End
	})
	if len(out) > MaxFindings {
		out = out[:MaxFindings]
	}
	return out
}

// Redact replaces each finding's span with «REDACTED:rule». Overlapping
// findings become one span, labelled with the first; findings outside the
// text are ignored.
func Redact(text string, findings []Finding) string {
	spans := make([]Finding, 0, len(findings))
	for _, f := range findings {
		if f.Start >= 0 && f.Start < f.End && f.End <= len(text) {
			spans = append(spans, f)
		}
	}
	sort.SliceStable(spans, func(a, b int) bool { return spans[a].Start < spans[b].Start })
	var b strings.Builder
	at := 0
	for i := 0; i < len(spans); {
		label, start, end := spans[i].Rule, spans[i].Start, spans[i].End
		for i++; i < len(spans) && spans[i].Start < end; i++ {
			end = max(end, spans[i].End)
		}
		b.WriteString(text[at:start])
		b.WriteString("«REDACTED:" + label + "»")
		at = end
	}
	b.WriteString(text[at:])
	return b.String()
}

// Patterns is the list as JSON: {"schema":1,"version":N,"categories":[…],
// "actions":{…},"rules":[…]}, the bytes every client reads.
type Patterns struct {
	Schema     int               `json:"schema"`
	Version    int               `json:"version"`
	Categories []string          `json:"categories"`
	Actions    map[string]string `json:"actions"`
	Rules      []Rule            `json:"rules"`
}

// PatternsJSON is Patterns, indented, with a final newline: the served body
// and the generated file are the same bytes.
func PatternsJSON() []byte {
	body, err := json.MarshalIndent(Patterns{Schema: 1, Version: Version, Categories: Categories, Actions: Actions, Rules: Rules}, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(body, '\n')
}

// digits are s's ASCII digits.
func digits(s string) []int {
	var out []int
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			out = append(out, int(s[i]-'0'))
		}
	}
	return out
}

// luhn says whether s's 13 to 19 digits pass the Luhn check.
func luhn(s string) bool {
	d := digits(s)
	if len(d) < 13 || len(d) > 19 {
		return false
	}
	sum := 0
	for i := range d {
		v := d[len(d)-1-i]
		if i%2 == 1 {
			if v *= 2; v > 9 {
				v -= 9
			}
		}
		sum += v
	}
	return sum%10 == 0
}

// mod97 says whether s, spaces removed, is 15 to 34 characters with a valid
// IBAN check (ISO 13616: the first four moved to the end, letters as 10–35,
// the number mod 97 is 1).
func mod97(s string) bool {
	s = strings.ReplaceAll(s, " ", "")
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	rem := 0
	for _, c := range s[4:] + s[:4] {
		switch {
		case c >= '0' && c <= '9':
			rem = (rem*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			rem = (rem*100 + int(c-'A') + 10) % 97
		default:
			return false
		}
	}
	return rem == 1
}
