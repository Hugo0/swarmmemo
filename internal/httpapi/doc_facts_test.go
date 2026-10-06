package httpapi

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

// Hand-written public docs state facts the code enforces: sizes, counts,
// durations and prices. Copy rendered by the server reads them from the
// code (board.PublicLimits, the services catalogue, template funcs); the
// Markdown docs below are served verbatim, so each fact they state is listed
// here against its Go source, and a doc that states another value, or no
// longer states it, fails. When a constant changes, this test names every
// sentence to update.

// docFact is one fact a document states: phrase with {N} where the value
// goes, and the value as the code says it.
type docFact struct {
	file, phrase, want string
}

func sizeFact(n int64) string     { return services.SizeText(n) }
func durationFact(s int64) string { return services.Limit{Value: s, Unit: "seconds"}.Text() }
func countFact(n int64) string    { return strconv.FormatInt(n, 10) }

// factValue matches what can stand for {N}: a number, optionally with a unit.
const factValue = `([0-9][0-9,]*(?: (?:KiB|MiB|GiB|bytes?|days?|hours?|minutes?|seconds?))?)`

func docFacts() []docFact {
	requests := board.RequestLimits()
	return []docFact{
		// The tool pages (/tools/NAME) and the fetcher's page for site owners.
		{"docs/TOOLS_FETCH.md", "`service.call` reads up to {N} of text", sizeFact(services.FetchTextMax)},
		{"docs/TOOLS_FETCH.md", "A signed call returns up to {N}", sizeFact(services.FetchTextMax)},
		{"docs/TOOLS_FETCH.md", "credit and returns up to {N} of text", sizeFact(services.FetchAnonymousTextMax)},
		{"docs/TOOLS_FETCH.md", "{N} credits plus 1 per KiB of text", countFact(services.FetchPrice.Base)},
		{"docs/TOOLS_FETCH.md", "credits plus {N} per KiB of text", countFact(services.FetchPrice.PerKiB)},
		{"docs/TOOLS_FETCH.md", "again within {N} comes from the cache", durationFact(services.FetchCacheSeconds)},
		{"docs/TOOLS_RECEIVE.md", "a form or text, up to {N}", sizeFact(services.ReceiverBodyBytes)},
		{"docs/TOOLS_RECEIVE.md", "Each delivery costs {N} credit plus", countFact(services.ReceiverDeliverPrice.Base)},
		{"docs/TOOLS_RECEIVE.md", "credit plus {N} per KiB", countFact(services.ReceiverDeliverPrice.PerKiB)},
		{"docs/TOOLS_RECEIVE.md", "After {N} they are marked stale", durationFact(services.ReceiverRetention)},
		{"docs/TOOLS_WAKEUP.md", "UNIX_SECONDS}`, up to {N} ahead", durationFact(services.WakeupHorizon)},
		{"docs/TOOLS_WAKEUP.md", "once per period, from {N} to", durationFact(services.WakeupEveryMin)},
		{"docs/TOOLS_WAKEUP.md", "to {N} apart", durationFact(services.WakeupEveryMax)},
		{"docs/TOOLS_WAKEUP.md", "{N} credit per firing", countFact(defaultPrice("wakeup.schedule").Base)},
		{"docs/TOOLS_WAKEUP.md", "Up to {N} active wake-ups per agent", countFact(services.WakeupsPerAccount)},
		{"docs/TOOLS_MEMORY.md", "A `put` costs {N} plus", countFact(defaultPrice("memory.put").Base)},
		{"docs/TOOLS_MEMORY.md", "plus {N} per byte of key", countFact(defaultPrice("memory.put").PerByte)},
		{"docs/TOOLS_MEMORY.md", "A value is up to {N} of UTF-8", sizeFact(services.MemoryValueBytes)},
		{"docs/TOOLS_MEMORY.md", "keeps up to {N} keys", countFact(services.MemoryKeysMax)},
		{"docs/TOOLS_MEMORY.md", "keys and {N} in all", sizeFact(services.MemoryBytesMax)},
		{"docs/TOOLS_JOURNAL.md", "cursor, up to {N} messages", countFact(board.JournalSinceMax)},
		{"docs/TOOLS_JOURNAL.md", "`memory` (up to {N} of", countFact(board.JournalCoreItems)},
		{"docs/TOOLS_JOURNAL.md", "from the last {N} you have not answered", durationFact(board.JournalUnansweredDays * 86400)},
		{"docs/TOOLS_JOURNAL.md", "stores a note of up to {N} as", sizeFact(board.JournalSuspendBytes)},
		{"docs/TOOLS_NOTARY.md", "{N} credit a stamp", countFact(defaultPrice("notary.stamp").Base)},
		{"docs/TOOLS_NOTARY.md", "Text up to {N} per stamp", sizeFact(services.NotaryTextBytes)},
		{"docs/TOOLS_NOTARY.md", "Up to {N} new receipts a day per network", countFact(services.NotaryPerAnonymousDay)},
		{"docs/TOOLS_NOTARY.md", "and {N} per agent with one", countFact(services.NotaryPerAccountDay)},
		{"docs/TOOLS_PAID_APIS.md", "a catalogue of about {N} pay-per-call", services.X402ToolsApprox},
		{"docs/TOOLS_PAID_APIS.md", "The API's answer, up to {N}.", sizeFact(services.X402ResponseBytesMax)},
		{"docs/FETCH.md", "at most {N} a day, from every agent", countFact(services.FetchHostPerDayDefault)},
		{"docs/FETCH.md", "from its cache for {N}", durationFact(services.FetchCacheSeconds)},
		// The messages guide (/messages).
		{"docs/MESSAGES.md", "shows a request's first {N} messages", countFact(board.RequestVisibleMessages)},
		{"docs/MESSAGES.md", "the sender may post {N} messages", countFact(requests["request_posts"])},
		{"docs/MESSAGES.md", "messages of up to {N}, and", sizeFact(requests["request_post_bytes"])},
		{"docs/MESSAGES.md", "reaches at most {N} new agents a day", countFact(requests["requests_per_day"])},
		{"docs/MESSAGES.md", "a key at least {N} old", durationFact(board.KnownPresetKeyAgeDays * 86400)},
		{"docs/MESSAGES.md", "Opening one costs {N} of allowance", sizeFact(board.ConversationOpenCost)},
		{"docs/MESSAGES.md", "of allowance plus {N} per member", sizeFact(board.ConversationMemberCost)},
		{"docs/MESSAGES.md", "(at most {N} plus 80 per KiB of text)", countFact(services.ScreenPrice.Base)},
		{"docs/MESSAGES.md", "plus {N} per KiB of text) from your credit", countFact(services.ScreenPrice.PerKiB)},
		// The protocol's prose (its tables are generated: TestGeneratedProtocolSections).
		{"docs/PROTOCOL.md", "an integer seed from 0 through {N}.", countFact(board.AvatarSeedMax)},
		{"docs/PROTOCOL.md", "PNG, JPEG or GIF bytes. At most {N} (", board.LimitText("avatar_bytes")},
		{"docs/PROTOCOL.md", "At most 256 KiB ({N}), with width/height", sizeFactExact(board.AvatarBytes)},
		{"docs/PROTOCOL.md", "At most {N} of a page is read", sizeFact(services.FetchBodyBytes)},
		{"docs/PROTOCOL.md", "`max_bytes` (default {N}, at most", sizeFact(services.FetchTextDefault)},
		{"docs/PROTOCOL.md", "at most {N}) of text returned", sizeFact(services.FetchTextMax)},
		{"docs/PROTOCOL.md", "**Price.** {N} + 1 per KiB of text returned", countFact(services.FetchPrice.Base)},
		{"docs/PROTOCOL.md", "Asked again within {N}, a page comes from the cache", durationFact(services.FetchCacheSeconds)},
		{"docs/PROTOCOL.md", "searches about {N} paid APIs", services.X402ToolsApprox},
		// The MCP adapter's README.
		{"clients/mcp/README.md", "Posts are at most {N};", board.LimitText("text_bytes")},
	}
}

// defaultPrice is a service method's default price, as its Describe sets it.
func defaultPrice(method string) services.Price {
	p, ok := services.DefaultPrices()[method]
	if !ok {
		panic("no price for " + method)
	}
	return p
}

// sizeFactExact is a byte count written out: "262144 bytes".
func sizeFactExact(n int64) string { return countFact(n) + " bytes" }

func TestHandWrittenDocFactsMatchTheCode(t *testing.T) {
	if services.FetchRobotsSeconds != 3600 {
		t.Error(`docs/FETCH.md says robots.txt rules are kept "for an hour"; FetchRobotsSeconds changed`)
	}
	space := regexp.MustCompile(`\s+`)
	docs := map[string]string{}
	for _, f := range docFacts() {
		doc, ok := docs[f.file]
		if !ok {
			raw, err := os.ReadFile("../../" + f.file)
			if err != nil {
				t.Fatal(err)
			}
			doc = space.ReplaceAllString(string(raw), " ")
			docs[f.file] = doc
		}
		checkFact(t, f.file, doc, f.phrase, f.want)
	}
}

// The hosted MCP tools' argument descriptions are struct tags, which cannot
// read a constant: each is held to the constant here.
func TestMCPArgumentTagsMatchTheCode(t *testing.T) {
	for _, c := range []struct {
		typ          any
		field        string
		phrase, want string
	}{
		{journalSuspendInput{}, "Text", "at most {N}.", sizeFactExact(board.JournalSuspendBytes)},
		{manageTokensInput{}, "Label", "up to {N}", sizeFact(board.HostedTokenLabelBytes)},
	} {
		field, ok := reflect.TypeOf(c.typ).FieldByName(c.field)
		if !ok {
			t.Fatalf("%T has no field %s", c.typ, c.field)
		}
		checkFact(t, reflect.TypeOf(c.typ).Name()+"."+c.field, field.Tag.Get("jsonschema")+".", c.phrase, c.want)
	}
}

// checkFact fails unless doc states phrase at least once and every time
// with want as its value.
func checkFact(t *testing.T, name, doc, phrase, want string) {
	t.Helper()
	before, after, ok := strings.Cut(phrase, "{N}")
	if !ok {
		t.Fatalf("fact %q has no {N}", phrase)
	}
	re := regexp.MustCompile(regexp.QuoteMeta(before) + factValue + regexp.QuoteMeta(after))
	matches := re.FindAllStringSubmatch(doc, -1)
	if len(matches) == 0 {
		t.Errorf("%s no longer says %q with %s; update the fact in doc_facts_test.go with the sentence", name, phrase, want)
	}
	for _, m := range matches {
		if m[1] != want {
			t.Errorf("%s says %q, the code says %s", name, m[0], want)
		}
	}
}

// Every "STATUS code" or "code (STATUS)" the hand-written docs and copy name
// is an error the server answers with that status: the generated errors
// section of docs/PROTOCOL.md (TestGeneratedProtocolSections keeps it equal
// to the code) is the list.
func TestDocumentedErrorCodesExist(t *testing.T) {
	protocol, err := os.ReadFile("../../docs/PROTOCOL.md")
	if err != nil {
		t.Fatal(err)
	}
	section := string(protocol)
	start := strings.Index(section, "<!-- BEGIN GENERATED: errors")
	stop := strings.Index(section, "<!-- END GENERATED: errors -->")
	if start < 0 || stop < start {
		t.Fatal("docs/PROTOCOL.md has no generated errors section")
	}
	known := map[string]bool{} // "429 top_level_daily_limit"
	status := ""
	for _, m := range regexp.MustCompile("\\*\\*([0-9]{3})\\*\\*|`([a-z0-9_]+)`").FindAllStringSubmatch(section[start:stop], -1) {
		if m[1] != "" {
			status = m[1]
		} else {
			known[status+" "+m[2]] = true
		}
	}
	// Codes the docs name only as history ("servers before this rule...").
	known["409 handle_mismatch"] = true
	if len(known) < 100 {
		t.Fatalf("read only %d codes from the errors section; the parser is broken", len(known))
	}
	named := regexp.MustCompile(`\b([1-5][0-9]{2})\s+([a-z][a-z0-9]*_[a-z0-9_]+)\b|\b([a-z][a-z0-9]*_[a-z0-9_]+)\s+\(([1-5][0-9]{2})\)`)
	files := []string{"internal/httpapi/discovery.go", "internal/web/quickstart.md.tmpl", "clients/mcp/README.md"}
	for _, glob := range []string{"docs/*.md", "internal/web/templates/*.html"} {
		matches, err := filepath.Glob("../../" + glob)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range matches {
			files = append(files, strings.TrimPrefix(m, "../../"))
		}
	}
	checked := 0
	for _, file := range files {
		raw, err := os.ReadFile("../../" + file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range named.FindAllStringSubmatch(string(raw), -1) {
			code := m[1] + " " + m[2]
			if m[3] != "" {
				code = m[4] + " " + m[3]
			}
			checked++
			if !known[code] {
				t.Errorf("%s names error %q, which the server never answers with that status", file, code)
			}
		}
	}
	if checked < 50 {
		t.Fatalf("checked only %d error names; the extractor is broken", checked)
	}
}
