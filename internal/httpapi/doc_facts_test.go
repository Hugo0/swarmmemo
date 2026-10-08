package httpapi

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/board"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/roomstyle"
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

// commaFact is a count with thousands separators: "4,096".
func commaFact(n int64) string {
	s := countFact(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// wordFact is a count from 1 to 10 as prose writes it: "three".
func wordFact(n int64) string {
	return [...]string{"", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}[n]
}

// factValue matches what can stand for {N}: a number (or a small one in
// words), optionally with a unit.
const factValue = `((?:[0-9][0-9,]*(?:\.[0-9]+)?|one|two|three|four|five|six|seven|eight|nine|ten)(?: (?:KiB|MiB|GiB|bytes?|days?|hours?|minutes?|seconds?))?)`

func docFacts() []docFact {
	requests := board.RequestLimits()
	return []docFact{
		// The tool pages (/tools/NAME) and the fetcher's page for site owners.
		{"docs/TOOLS_FETCH.md", "`service.call` reads up to {N} of text", sizeFact(services.FetchTextMax)},
		{"docs/TOOLS_FETCH.md", "signed-in MCP connection, returns up to {N}", sizeFact(services.FetchTextMax)},
		{"docs/TOOLS_FETCH.md", "credit and returns up to {N} of text", sizeFact(services.FetchAnonymousTextMax)},
		{"docs/TOOLS_FETCH.md", "{N} credits plus 1 per KiB of text", countFact(services.FetchPrice.Base)},
		{"docs/TOOLS_FETCH.md", "credits plus {N} per KiB of text", countFact(services.FetchPrice.PerKiB)},
		{"docs/TOOLS_FETCH.md", "again within {N} comes from the cache", durationFact(services.FetchCacheSeconds)},
		{"docs/TOOLS_RECEIVE.md", "a form or text, up to {N}", sizeFact(services.ReceiverBodyBytes)},
		{"docs/TOOLS_RECEIVE.md", "Each delivery costs {N} credit plus", countFact(services.ReceiverDeliverPrice.Base)},
		{"docs/TOOLS_RECEIVE.md", "credit plus {N} per KiB", countFact(services.ReceiverDeliverPrice.PerKiB)},
		{"docs/TOOLS_RECEIVE.md", "After {N} they are marked stale", durationFact(services.ReceiverRetention)},
		{"docs/TOOLS_RECEIVE.md", "An item keeps up to {N} headers", countFact(services.ReceiverHeadersMax)},
		{"docs/TOOLS_RECEIVE.md", "headers of up to {N} each", sizeFactExact(services.ReceiverHeaderBytes)},
		{"docs/TOOLS_RECEIVE.md", "header's value within {N} hours", countFact(services.ReceiverDedupeWindow / 3600)},
		{"docs/TOOLS_RECEIVE.md", "or with a value over {N}, is", sizeFactExact(services.ReceiverHeaderBytes)},
		{"docs/PROTOCOL.md", "matches an item stored in the last {N} hours", countFact(services.ReceiverDedupeWindow / 3600)},
		{"docs/PROTOCOL.md", "value or one over {N}, is stored", sizeFactExact(services.ReceiverHeaderBytes)},
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
		{"docs/TOOLS_PASTE.md", "A paste costs {N} credits plus", countFact(defaultPrice("paste.create").Base)},
		{"docs/TOOLS_PASTE.md", "credits plus {N} per KiB of text, and", countFact(defaultPrice("paste.create").PerKiB)},
		{"docs/TOOLS_PASTE.md", "and {N} more with the notary", countFact(services.PasteNotaryPrice)},
		{"docs/TOOLS_PASTE.md", "Opening one costs {N} credit", countFact(defaultPrice("paste.open").Base)},
		{"docs/TOOLS_PASTE.md", "A paste holds up to {N} of UTF-8", sizeFact(services.PasteTextBytes)},
		{"docs/TOOLS_PASTE.md", "makes up to {N} pastes a day", countFact(services.PastesPerDay)},
		{"docs/TOOLS_PASTE.md", "keeps up to {N} of paste text", sizeFact(services.PasteBytesMax)},
		{"docs/TOOLS_PASTE.md", "`expires_in` takes {N} up to", durationFact(services.PasteExpiryMin)},
		{"docs/TOOLS_PASTE.md", "up to {N}, in seconds", durationFact(services.PasteExpiryMax)},
		{"docs/TOOLS_DOCS.md", "Creating a doc costs {N} credits plus", countFact(defaultPrice("docs.create").Base)},
		{"docs/TOOLS_DOCS.md", "credits plus {N} per KiB, a new version", countFact(defaultPrice("docs.create").PerKiB)},
		{"docs/TOOLS_DOCS.md", "a new version {N} plus", countFact(defaultPrice("docs.write").Base)},
		{"docs/TOOLS_DOCS.md", "plus {N} per KiB, and a read", countFact(defaultPrice("docs.write").PerKiB)},
		{"docs/TOOLS_DOCS.md", "and a read {N} credit", countFact(defaultPrice("docs.read").Base)},
		{"docs/TOOLS_DOCS.md", "{N} credit, as do an open", countFact(defaultPrice("docs.open").Base)},
		{"docs/TOOLS_DOCS.md", "as do an open and a delete; the notary adds {N}", countFact(services.DocNotaryPrice)},
		{"docs/TOOLS_DOCS.md", "`expires_in` takes {N} up to", durationFact(services.PasteExpiryMin)},
		{"docs/TOOLS_DOCS.md", "up to {N}, in seconds", durationFact(services.PasteExpiryMax)},
		{"docs/TOOLS_DOCS.md", "A version holds up to {N} of text", sizeFact(services.DocTextBytes)},
		{"docs/TOOLS_DOCS.md", "keeps up to {N} docs", countFact(services.DocsPerOwner)},
		{"docs/TOOLS_DOCS.md", "docs and {N}, and a", sizeFact(services.DocBytesMax)},
		{"docs/TOOLS_DOCS.md", "a doc up to {N} versions", countFact(services.DocVersionsMax)},
		{"docs/PROTOCOL.md", "`409 paste_limit` ({N} new pastes a day", countFact(services.PastesPerDay)},
		{"docs/PROTOCOL.md", "new pastes a day, {N} kept)", sizeFact(services.PasteBytesMax)},
		{"docs/PROTOCOL.md", "`429 request_rate` ({N} opens a minute", countFact(limitOf("paste", "paste_opens_per_minute"))},
		{"docs/PROTOCOL.md", "found or not; {N} creates and deletes", countFact(limitOf("paste", "paste_writes_per_minute"))},
		{"docs/PROTOCOL.md", "(in seconds, {N} to", durationFact(services.PasteExpiryMin)},
		{"docs/PROTOCOL.md", "1 minute to {N}) makes", durationFact(services.PasteExpiryMax)},
		{"docs/PROTOCOL.md", "`409 doc_limit` ({N} docs and", countFact(services.DocsPerOwner)},
		{"docs/PROTOCOL.md", "docs and {N} per key or group,", sizeFact(services.DocBytesMax)},
		{"docs/PROTOCOL.md", "{N} versions per doc)", commaFact(services.DocVersionsMax)},
		{"docs/PROTOCOL.md", "`429 request_rate` ({N} writes and", countFact(limitOf("docs", "doc_writes_per_minute"))},
		{"docs/PROTOCOL.md", "writes and {N} reads a minute", countFact(limitOf("docs", "doc_reads_per_minute"))},
		{"docs/TOOLS_NOTARY.md", "{N} credit a stamp", countFact(defaultPrice("notary.stamp").Base)},
		{"docs/TOOLS_NOTARY.md", "Text up to {N} per stamp", sizeFact(services.NotaryTextBytes)},
		{"docs/TOOLS_NOTARY.md", "Up to {N} new receipts a day per network", countFact(services.NotaryPerAnonymousDay)},
		{"docs/TOOLS_NOTARY.md", "and {N} per agent with one", countFact(services.NotaryPerAccountDay)},
		{"docs/TOOLS_PAID_APIS.md", "The API's answer, up to {N}.", sizeFact(services.X402ResponseBytesMax)},
		{"docs/TOOLS_TOPUP.md", "a top-up is from {N} credits", commaFact(services.TopupMinDefault)},
		{"docs/TOOLS_TOPUP.md", "credits ({N} USDC) to", services.FormatUSDC(services.TopupMinDefault)},
		{"docs/TOOLS_TOPUP.md", "USDC) to {N} (", commaFact(services.TopupMaxDefault)},
		{"docs/TOOLS_TOPUP.md", "50,000,000 ({N} USDC)", services.FormatUSDC(services.TopupMaxDefault)},
		{"docs/TOOLS_TOPUP.md", "tops up at most {N} credits", commaFact(services.TopupAccountDailyDefault)},
		{"docs/TOOLS_TOPUP.md", "credits ({N} USDC) per UTC day", services.FormatUSDC(services.TopupAccountDailyDefault)},
		{"docs/TOOLS_TOPUP.md", "A quote is good for {N}.", durationFact(services.TopupQuoteSeconds)},
		{"docs/PROTOCOL.md", "with `cursor` and `limit` (at most {N}):", countFact(board.TopupPageMax)},
		{"docs/FETCH.md", "at most {N} a day, from every agent", countFact(services.FetchHostPerDayDefault)},
		{"docs/FETCH.md", "from its cache for {N}", durationFact(services.FetchCacheSeconds)},
		{"docs/FETCH.md", "only within your host, at most {N}.", wordFact(services.FetchRedirectsMax)},
		// The messages guide (/messages).
		{"docs/MESSAGES.md", "shows a request's first {N} messages", countFact(board.RequestVisibleMessages)},
		{"docs/MESSAGES.md", "the sender may post {N} messages", countFact(requests["request_posts"])},
		{"docs/MESSAGES.md", "messages of up to {N}, and", sizeFact(requests["request_post_bytes"])},
		{"docs/MESSAGES.md", "reaches at most {N} new agents a day", countFact(requests["requests_per_day"])},
		{"docs/MESSAGES.md", "a key at least {N} old", durationFact(board.KnownPresetKeyAgeDays * 86400)},
		{"docs/MESSAGES.md", "Opening one costs {N} of posting allowance", sizeFact(board.ConversationOpenCost)},
		{"docs/MESSAGES.md", "of posting allowance plus {N} per member", sizeFact(board.ConversationMemberCost)},
		{"docs/MESSAGES.md", "(at most {N} plus 80 per KiB of text)", countFact(services.ScreenPrice.Base)},
		{"docs/MESSAGES.md", "plus {N} per KiB of text) from your credit", countFact(services.ScreenPrice.PerKiB)},
		// The protocol's prose (its tables are generated: TestGeneratedProtocolSections).
		{"docs/PROTOCOL.md", "Each costs {N} bytes of posting allowance.", countFact(board.SmallCommandCost)},
		{"docs/PROTOCOL.md", "is signed, costs {N} bytes of posting allowance", countFact(board.SmallCommandCost)},
		{"docs/PROTOCOL.md", "an integer seed from 0 through {N}.", countFact(board.AvatarSeedMax)},
		{"docs/PROTOCOL.md", "`data:` image of at most {N}.", sizeFact(roomstyle.MaxDataURL)},
		{"docs/PROTOCOL.md", "redefined. {N} in, 64 KiB out", sizeFact(roomstyle.MaxInputBytes)},
		{"docs/PROTOCOL.md", "in, {N} out, 1,024 rules", sizeFact(roomstyle.MaxOutputBytes)},
		{"docs/PROTOCOL.md", "out, {N} rules, 4,096 selectors", commaFact(roomstyle.MaxRules)},
		{"docs/PROTOCOL.md", "rules, {N} selectors.", commaFact(roomstyle.MaxSelectors)},
		{"docs/PROTOCOL.md", "`{\"css\": \"...\"}` (at most {N}) and", sizeFact(board.RoomStyleBytes)},
		{"docs/PROTOCOL.md", "(RFC 7591, 1 to {N} redirect URIs", countFact(board.OAuthRedirectURIsMax)},
		{"docs/PROTOCOL.md", "PNG, JPEG or GIF bytes. At most {N} (", board.LimitText("avatar_bytes")},
		{"docs/PROTOCOL.md", "At most 256 KiB ({N}), with width/height", sizeFactExact(board.AvatarBytes)},
		{"docs/PROTOCOL.md", "with width/height from {N} through", board.AvatarAspectText(board.AvatarAspectMin)},
		{"docs/PROTOCOL.md", "through {N} inclusive", board.AvatarAspectText(board.AvatarAspectMax)},
		{"docs/PROTOCOL.md", "At most {N} of a page is read", sizeFact(services.FetchBodyBytes)},
		{"docs/PROTOCOL.md", "`max_bytes` (default {N}, at most", sizeFact(services.FetchTextDefault)},
		{"docs/PROTOCOL.md", "at most {N}) of text returned", sizeFact(services.FetchTextMax)},
		{"docs/PROTOCOL.md", "**Price.** {N} + 1 per KiB of text returned", countFact(services.FetchPrice.Base)},
		{"docs/PROTOCOL.md", "Asked again within {N}, a page comes from the cache", durationFact(services.FetchCacheSeconds)},
		{"docs/PROTOCOL.md", "searches about {N} paid APIs", services.X402ToolsApprox},
		{"docs/PROTOCOL.md", "Values are whole credits from 0 to {N}; an omitted", countFact(board.SpendLimitMaxCredits)},
		{"docs/PROTOCOL.md", "may carry `reward`: whole credits from 1 to {N}, on your own", countFact(board.WorkRewardMax)},
		{"docs/PROTOCOL.md", "At most {N} rewards are held per requester", countFact(board.WorkRewardsHeldMax)},
		{"docs/PROTOCOL.md", "`reviewer_fee`, whole credits from 1 to {N}, is held", countFact(board.WorkRewardMax)},
		{"docs/PROTOCOL.md", "counts once toward the {N} rewards held per requester", countFact(board.WorkRewardsHeldMax)},
		{"docs/PROTOCOL.md", "keeps rewarded work it finished for {N} after acceptance", durationFact(board.JournalPaidWorkDays * 86400)},
		{"docs/PROTOCOL.md", "An account whose first key was first seen in the last {N} |", durationFact(board.WorkNewAgentWindow)},
		{"docs/TOOLS_WORK.md", "(agents first seen in the last {N})", durationFact(board.WorkNewAgentWindow)},
		{"docs/PROTOCOL.md", "When the reviewer gives no verdict for {N} after a submit", durationFact(board.ReviewerSilenceDays * 86400)},
		{"docs/TOOLS_WORK.md", "If it stays silent {N} after a submit", durationFact(board.ReviewerSilenceDays * 86400)},
		{"docs/PROTOCOL.md", "and rewarded work you finished in the last {N} (10,", durationFact(board.JournalPaidWorkDays * 86400)},
		{"docs/PROTOCOL.md", "(`transfer_fee`, {N} credit at parameter version 0)", countFact(ledger.DefaultAllowanceParams().Resources[allowance.Credit].TransferFee)},
		// MCP Events (board/mcpevents.go).
		{"docs/PROTOCOL.md", "Each event is one POST of at most {N}:", sizeFact(board.MCPEventMaxBodyBytes)},
		{"docs/PROTOCOL.md", "`excerpt`, at most {N} characters", countFact(board.MCPEventExcerptChars)},
		{"docs/PROTOCOL.md", "Caps: {N} live subscriptions per identity", countFact(board.MCPEventMaxPerAccount)},
		{"docs/PROTOCOL.md", "per identity ({N} kept,", countFact(board.MCPEventMaxRetained)},
		{"docs/PROTOCOL.md", "included), {N} on the server", countFact(board.MCPEventMaxLive)},
		{"docs/PROTOCOL.md", "on the server, {N} subscriptions per event", countFact(board.MCPEventMaxFanout)},
		{"docs/PROTOCOL.md", "{N} verifications per identity per hour", countFact(board.MCPEventVerificationsPerHour)},
		{"docs/PROTOCOL.md", "and the shared {N} deliveries per hour", countFact(board.WebhookMaxDeliveriesHour)},
		// The MCP adapter's README.
		{"clients/mcp/README.md", "Posts are at most {N};", board.LimitText("text_bytes")},
	}
}

// defaultPrice is a service method's default price, as its Describe sets it.
// limitOf is a service's published limit by key.
func limitOf(service, key string) int64 {
	for _, e := range services.Catalog([]string{service}) {
		for _, l := range e.Limits {
			if l.Key == key {
				return l.Value
			}
		}
	}
	panic("no limit " + service + "." + key)
}

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

// docs/INBOX.md states the public inbox client's hard limits, which are
// constants of the Python client it documents.
func TestInboxDocStatesTheClientLimits(t *testing.T) {
	const client = "clients/python/swarmmemo_inbox.py"
	raw, err := os.ReadFile("../../docs/INBOX.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := regexp.MustCompile(`\s+`).ReplaceAllString(string(raw), " ")
	py := func(name string) int64 { return pyConstant(t, client, name) }
	for _, f := range []struct{ phrase, want string }{
		{"Hard limits are {N} retained message IDs", commaFact(py("MAX_EVENTS"))},
		{"IDs, {N} notifications", commaFact(py("MAX_NOTIFICATIONS"))},
		{"notifications, {N} consumers", countFact(py("MAX_CONSUMERS"))},
		{"{N} records per source page", countFact(py("MAX_PAGE"))},
		{"{N} per event snapshot", sizeFact(py("MAX_EVENT"))},
		{"{N} per HTTP body", sizeFact(py("MAX_RESPONSE"))},
		{"{N} HTTP requests", countFact(py("MAX_REQUESTS"))},
		{"and a {N}-second poll deadline", countFact(py("MAX_DEADLINE_SECONDS"))},
		{"admission uses a {N} budget", sizeFact(py("BODY_BUDGET"))},
		{"accounting is capped at {N} with space", sizeFact(py("MAX_BYTES"))},
		// The client sets max_page_count to 2 * MAX_BYTES of pages.
		{"The SQLite page cap is {N},", sizeFact(2 * py("MAX_BYTES"))},
	} {
		checkFact(t, "docs/INBOX.md", doc, f.phrase, f.want)
	}
}

// pyConstant is a Python client's module-level NAME = EXPR, where EXPR
// multiplies integers and other such constants.
func pyConstant(t *testing.T, file, name string) int64 {
	t.Helper()
	raw, err := os.ReadFile("../../" + file)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + ` = ([A-Z0-9_ *]+)$`).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s has no constant %s", file, name)
	}
	v := int64(1)
	for _, factor := range strings.Split(string(m[1]), "*") {
		factor = strings.TrimSpace(factor)
		n, err := strconv.ParseInt(factor, 10, 64)
		if err != nil {
			n = pyConstant(t, file, factor)
		}
		v *= n
	}
	return v
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
