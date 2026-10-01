package leakscan

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Secrets are assembled at run time, so no literal here trips a repository's
// secret scanner.
func rep(s string, n int) string { return strings.Repeat(s, n) }

var (
	ghToken     = "gh" + "p_" + rep("aB3", 12)
	anthKey     = "sk-" + "ant-api03-" + rep("Xy9_", 10)
	openaiKey   = "sk-" + "proj-" + rep("Ab1-", 8)
	openaiOld   = "sk-" + rep("Ab12", 12)
	slackToken  = "xox" + "b-1234567890-" + rep("ab", 6)
	slackHook   = "https://hooks.slack.com/services/" + "T01ABCDEF/B01ABCDEF/" + rep("abcd", 6)
	stripeKey   = "sk_" + "live_" + rep("4eC3", 6)
	googleKey   = "AI" + "za" + rep("Sy1_-", 7)
	jwtToken    = "eyJ" + "hbGciOiJIUzI1NiJ9.eyJ" + "zdWIiOiIxIn0." + rep("sig_", 4)
	hostedToken = "sm" + "h1_" + rep("Qw-_", 11)
	recovery    = "sm" + "r1_" + rep("Zz09", 11)
	pkcs8       = "MC4CAQAwBQYDK2Vw" + "BCIEI" + rep("A", 43)
	pkcs8Hex    = "302e020100300506032b65700422" + "0420" + rep("ab", 32)
	pem         = "-----BEGIN RSA " + "PRIVATE KEY-----\nMIIEow" + rep("Q", 60) + "\n-----END RSA PRIVATE KEY-----"
)

// leakCases are hits and non-hits for every rule; want is the rule's spans as
// "start:end", and a non-hit has none of that rule.
var leakCases = []struct {
	rule, text string
	want       []string
}{
	{"pem_private_key", "key: " + pem + " done", []string{fmt.Sprintf("5:%d", 5+len(pem))}},
	{"pem_private_key", "-----BEGIN PUBLIC KEY-----\nMIIB", nil},
	{"aws_access_key", "id AKIA" + "IOSFODNN7EXAMPLE here", []string{"3:23"}},
	{"aws_access_key", "XAKIA" + "IOSFODNN7EXAMPLE and AKIA123", nil},
	{"github_token", "use " + ghToken, []string{fmt.Sprintf("4:%d", 4+len(ghToken))}},
	{"github_token", "ghp_short and gh_foo", nil},
	{"anthropic_key", anthKey, []string{fmt.Sprintf("0:%d", len(anthKey))}},
	{"anthropic_key", "sk-ant- and sk-ant-api03-short and " + openaiKey, nil},
	{"openai_key", anthKey + " task-list sk-abc", nil},
	{"openai_key", openaiKey + " " + openaiOld, []string{fmt.Sprintf("0:%d", len(openaiKey)), fmt.Sprintf("%d:%d", len(openaiKey)+1, len(openaiKey)+1+len(openaiOld))}},
	{"slack_token", slackToken + " " + slackHook, []string{fmt.Sprintf("0:%d", len(slackToken)), fmt.Sprintf("%d:%d", len(slackToken)+9, len(slackToken)+1+len(slackHook))}},
	{"slack_token", "xoxo hugs", nil},
	{"stripe_live", "k=" + stripeKey, []string{fmt.Sprintf("2:%d", 2+len(stripeKey))}},
	{"stripe_live", "sk_" + "test_" + rep("4eC3", 6), nil},
	{"google_api_key", googleKey, []string{"0:39"}},
	{"google_api_key", "AIza123", nil},
	{"jwt", "t " + jwtToken, []string{fmt.Sprintf("2:%d", 2+len(jwtToken))}},
	{"jwt", "eyJ.eyJ.x and eyJhbGciOiJ9", nil},
	{"swarmmemo_hosted_token", hostedToken + "\n" + recovery, []string{"0:49", "50:99"}},
	{"swarmmemo_hosted_token", "sm" + "o1_" + rep("Qw-_", 11), []string{"0:49"}},
	{"swarmmemo_hosted_token", "smh1_short", nil},
	{"browser_pkcs8_ed25519", pkcs8 + " " + pkcs8Hex, []string{"0:64", "65:161"}},
	{"browser_pkcs8_ed25519", "MC4CAQ and 302e0201", nil},
	{"url_credentials", "postgres://app:s3cretpw@db.example.com/x", []string{"15:23"}},
	{"url_credentials", "https://example.com/a:b@c and mailto:x@y", nil},
	{"bearer_token", "Authorization: Bearer " + rep("abc123", 4), []string{"22:46"}},
	{"bearer_token", "the bearer of bad news", nil},
	{"generic_secret_assignment", "DB_PASSWORD=hunter2hunter", []string{"12:25"}},
	{"generic_secret_assignment", `{"api_key": "abcd1234efgh"}`, []string{"13:25"}},
	{"generic_secret_assignment", "export GITHUB_TOKEN='x9" + rep("y", 10) + "'", []string{"21:33"}},
	{"generic_secret_assignment", `password = os.getenv("X1"); max_tokens=4096; password: ${DB_PASS1}; token = "<your token 1>"; password: ********; token = get_token(user1)`, nil},
	{"card_number", "card 4111 1111 1111 1111, 4111-1111-1111-1111 or 5555555555554444", []string{"5:24", "26:45", "49:65"}},
	{"card_number", "4111 1111 1111 1112 at 1788566400000 ms", nil},
	{"iban", "GB82 WEST 1234 5698 7654 32 and DE89370400440532013000", []string{"0:27", "32:54"}},
	{"iban", "GB82 WEST 1234 5698 7654 33 and AB12CDEF3456GHIJ", nil},
	{"email", "reach jane.doe@example.org now", []string{"6:26"}},
	{"email", "@handle and user@localhost", nil},
	{"phone_e164", "+14155552671, +44 20 7946 0958 or +33 1 23 45 67 89", []string{"0:12", "14:30", "34:51"}},
	{"phone_e164", "+1 and x+10 and version +2.0", nil},
	{"private_ipv4", "10.0.0.1 172.16.5.4 192.168.1.10 169.254.169.254 100.64.0.1", []string{"0:8", "9:19", "20:32", "33:48", "49:59"}},
	{"private_ipv4", "8.8.8.8 172.32.0.1 192.169.1.1 10.0.0.256 100.128.0.1", nil},
	{"internal_hostname", "db.prod.internal, printer.local, git.corp and NAS.LAN", []string{"0:16", "18:31", "33:41", "46:53"}},
	{"internal_hostname", "example.com and foo.language", nil},
}

func spans(findings []Finding, rule string) []string {
	var out []string
	for _, f := range findings {
		if f.Rule == rule {
			out = append(out, fmt.Sprintf("%d:%d", f.Start, f.End))
		}
	}
	return out
}

// Every rule finds its hits at the right byte offsets, and none of its
// non-hits; every rule has both.
func TestRules(t *testing.T) {
	hits, misses := map[string]bool{}, map[string]bool{}
	for _, c := range leakCases {
		got := spans(Scan(c.text), c.rule)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s in %q: got %v, want %v", c.rule, c.text, got, c.want)
		}
		if c.want == nil {
			misses[c.rule] = true
		} else {
			hits[c.rule] = true
		}
	}
	for _, r := range Rules {
		if !hits[r.ID] || !misses[r.ID] {
			t.Errorf("rule %s needs a hit and a non-hit case", r.ID)
		}
		if _, ok := Actions[r.Category]; !ok || r.Note == "" {
			t.Errorf("rule %s: category %q, note %q", r.ID, r.Category, r.Note)
		}
	}
}

// Redact replaces each finding with its label and merges overlaps; the
// offsets it is given are bytes, so text around multi-byte runes survives.
func TestRedact(t *testing.T) {
	text := "café key=" + stripeKey + " mail ana@example.org."
	findings := Scan(text)
	got := Redact(text, findings)
	if got != "café key=«REDACTED:stripe_live» mail «REDACTED:email»." {
		t.Fatalf("redacted: %q (%+v)", got, findings)
	}
	// Overlaps merge under the first label; spans outside the text are dropped.
	merged := Redact("abcdefghij", []Finding{{Rule: "a", Start: 1, End: 5}, {Rule: "b", Start: 3, End: 8}, {Rule: "c", Start: 9, End: 99}, {Rule: "d", Start: -1, End: 2}})
	if merged != "a«REDACTED:a»ij" {
		t.Fatalf("merged: %q", merged)
	}
	if Redact("nothing here", nil) != "nothing here" {
		t.Fatal("a text without findings changed")
	}
}

// The one action table (steward, 2026-09-30): credentials and financial
// numbers hold, contact details and private infrastructure warn, anything
// unnamed holds; an agent's override wins; a classifier category counts at
// or above the threshold.
func TestActions(t *testing.T) {
	want := map[string]string{Credentials: Hold, Financial: Hold, PersonalData: Warn, PrivateInfrastructure: Warn, "excess_code": Hold}
	for category, action := range want {
		if got := Action(category, nil); got != action {
			t.Errorf("%s: %s, want %s", category, got, action)
		}
	}
	if Action(PersonalData, map[string]string{PersonalData: Hold}) != Hold || Action(Credentials, map[string]string{Credentials: Warn}) != Warn || Action(Credentials, map[string]string{Credentials: "ignore"}) != Hold {
		t.Fatal("overrides")
	}
	for _, c := range []struct {
		text      string
		scores    map[string]float64
		overrides map[string]string
		want      string
	}{
		{"nothing to see", nil, nil, "pass"},
		{"mail ana@example.org at db.prod.internal", nil, nil, Warn},
		{"mail ana@example.org", nil, map[string]string{PersonalData: Hold}, Hold},
		{"card 4111 1111 1111 1111", nil, nil, Hold},
		{"key " + stripeKey + " to ana@example.org", nil, nil, Hold},
		{"nothing to see", map[string]float64{PersonalData: 0.9}, nil, Warn},
		{"nothing to see", map[string]float64{PersonalData: 0.5, "excess_code": 0.7}, nil, Hold},
		{"nothing to see", map[string]float64{"excess_code": 0.5}, nil, "pass"},
	} {
		if got := Verdict(Scan(c.text), c.scores, 0.6, c.overrides); got != c.want {
			t.Errorf("%q %v %v: %s, want %s", c.text, c.scores, c.overrides, got, c.want)
		}
	}
}

// hostileInputs are the shapes that make a backtracking engine blow up,
// long enough to show it.
func hostileInputs() map[string]string {
	const n = 16 << 10
	return map[string]string{
		"letters":    rep("a", n),
		"dashes":     rep("-", n),
		"digits":     rep("4", n),
		"spaced":     rep("4 ", n/2),
		"at":         rep("a@", n/2),
		"dots":       rep("a.", n/2),
		"labels":     rep("aaaaaaaaa.", n/10),
		"assign":     rep("password=", n/9),
		"assign_val": "password=" + rep("a", n),
		"begin":      rep("-----BEGIN PRIVATE"+" KEY-----", n/27),
		"pem_open":   "-----BEGIN PRIVATE"+" KEY-----" + rep("A ", n/2),
		"url":        rep("ab://c:", n/7),
		"plus":       rep("+1 ", n/3),
		"sk":         rep("sk-", n/3),
		"eyj":        rep("eyJaaaaa.", n/9),
		"ip":         rep("10.", n/3),
		"bearer":     rep("Bearer ", n/7),
		"emails":     rep("a@b.co ", n/7),
	}
}

// Scan is linear and bounded on hostile input: each 16 KiB input in well
// under a second, and never more than MaxFindings findings.
func TestHostileInputsLinear(t *testing.T) {
	for name, text := range hostileInputs() {
		start := time.Now()
		f := Scan(text)
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s: %v", name, d)
		}
		if len(f) > MaxFindings {
			t.Errorf("%s: %d findings", name, len(f))
		}
	}
}

// PatternsJSON is the list, as clients read it.
func TestPatternsJSON(t *testing.T) {
	var p Patterns
	if err := json.Unmarshal(PatternsJSON(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Schema != 1 || p.Version != Version || len(p.Rules) != len(Rules) || !reflect.DeepEqual(p.Categories, Categories) || !reflect.DeepEqual(p.Actions, Actions) {
		t.Fatalf("patterns: %+v", p)
	}
	if !json.Valid(PatternsJSON()) || !strings.HasSuffix(string(PatternsJSON()), "}\n") {
		t.Fatal("not one JSON document with a final newline")
	}
}

// portableOverride is the agent's own table the portability test also
// applies: it turns contact details into a hold.
var portableOverride = map[string]string{PersonalData: Hold}

// portableScripts run each client's own scanner and action table over texts
// and print, per text, [findings, verdict, verdict under portableOverride]:
// the findings as [[rule,start,end],...], offsets in UTF-8 bytes like Scan's.
// node runs the web composer's assets/leak.js on the served list; python3
// runs the CLI's leak_scan and leak_action in clients/python/swarmmemo.py
// (its generated LEAK_PATTERNS). So every client that holds a send finds and
// decides exactly what the server does.
var portableScripts = map[string][]string{
	"node": {"node", "-e", `
const fs = require('fs'), path = require('path'), {pathToFileURL} = require('url');
(async () => {
  const leak = await import(pathToFileURL(path.join(process.argv[3], '..', '..', 'internal', 'web', 'assets', 'leak.js')).href);
  const p = JSON.parse(fs.readFileSync(process.argv[1], 'utf8')), rules = leak.compile(p);
  const texts = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
  const out = texts.map(t => { const started = Date.now(), bytes = i => Buffer.byteLength(t.slice(0, i)), found = leak.scan(t, rules);
    const f = found.map(x => [x.rule, bytes(x.start), bytes(x.end)]);
    if (Date.now() - started > 2000) f.push(['slow', 0, 0]);
    return [f, leak.verdict(found, p.actions), leak.verdict(found, p.actions, {personal_data: 'hold'})]; });
  process.stdout.write(JSON.stringify(out));
})().catch(e => { console.error(e); process.exit(1); });`},
	"python3": {"python3", "-c", `
import json, sys, time
sys.path.insert(0, sys.argv[3])
import swarmmemo
def verdict(found, overrides=None):
    actions = [swarmmemo.leak_action(x['category'], overrides) for x in found]
    return 'hold' if 'hold' in actions else 'warn' if actions else 'pass'
out = []
for t in json.load(open(sys.argv[2])):
    started = time.time()
    found = swarmmemo.leak_scan(t)
    f = [[x['rule'], x['start'], x['end']] for x in found]
    if time.time() - started > 2: f.append(['slow', 0, 0])
    out.append([f, verdict(found), verdict(found, {'personal_data': 'hold'})])
sys.stdout.write(json.dumps(out))`},
}

// The same list gives the same findings in ECMAScript (the web composer) and
// the Python CLI as in Go, on every case, a multi-byte text and every hostile
// input, and neither engine backtracks badly. Skipped where node or python3
// is missing.
func TestPatternsPortable(t *testing.T) {
	texts := []string{}
	for _, c := range leakCases {
		texts = append(texts, c.text)
	}
	for _, text := range hostileInputs() {
		texts = append(texts, text)
	}
	texts = append(texts, "café key="+stripeKey+" mail ana@example.org, ☃ card 4111 1111 1111 1111")
	client, err := filepath.Abs("../../clients/python")
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		findings           [][3]any
		verdict, overridden string
	}
	var want []outcome
	for _, text := range texts {
		found := Scan(text)
		row := [][3]any{}
		for _, f := range found {
			row = append(row, [3]any{f.Rule, float64(f.Start), float64(f.End)})
		}
		want = append(want, outcome{row, Verdict(found, nil, 1, nil), Verdict(found, nil, 1, portableOverride)})
	}
	dir := t.TempDir()
	patterns, textsFile := filepath.Join(dir, "patterns.json"), filepath.Join(dir, "texts.json")
	raw, _ := json.Marshal(texts)
	if err := os.WriteFile(patterns, PatternsJSON(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(textsFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	for engine, script := range portableScripts {
		if _, err := exec.LookPath(script[0]); err != nil {
			t.Logf("%s not installed; skipped", engine)
			continue
		}
		out, err := exec.Command(script[0], append(script[1:], patterns, textsFile, client)...).Output()
		if err != nil {
			t.Fatalf("%s: %v", engine, err)
		}
		var got [][3]json.RawMessage
		if err = json.Unmarshal(out, &got); err != nil || len(got) != len(texts) {
			t.Fatalf("%s: %v %s", engine, err, out)
		}
		verdicts := map[string]bool{}
		for i := range texts {
			var findings [][3]any
			var verdict, overridden string
			if json.Unmarshal(got[i][0], &findings) != nil || json.Unmarshal(got[i][1], &verdict) != nil || json.Unmarshal(got[i][2], &overridden) != nil {
				t.Fatalf("%s: %s", engine, out)
			}
			if len(findings) > 0 || len(want[i].findings) > 0 {
				if !reflect.DeepEqual(findings, want[i].findings) {
					t.Errorf("%s on %.60q: %v, Go %v", engine, texts[i], findings, want[i].findings)
				}
			}
			if verdict != want[i].verdict || overridden != want[i].overridden {
				t.Errorf("%s on %.60q: verdicts %s and %s, Go %s and %s", engine, texts[i], verdict, overridden, want[i].verdict, want[i].overridden)
			}
			verdicts[verdict+"/"+overridden] = true
		}
		// The texts reach every verdict, and the override changes one.
		for _, v := range []string{"pass/pass", "warn/hold", "warn/warn", "hold/hold"} {
			if !verdicts[v] {
				t.Errorf("%s: no text has verdicts %s", engine, v)
			}
		}
	}
}
