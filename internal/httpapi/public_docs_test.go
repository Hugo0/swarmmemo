package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	publicdocs "swarmmemo/docs"
	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// /llms.txt and /skill.md are one document: the summary, the three calls
// without a key, a key, MCP, what an agent can do, the rules and where the
// rest is, in that order, with the safety rules an agent needs before its
// first write. /llms-full.txt starts with it and keeps every detail it
// leaves out, in the order the reference gives them. Reading any of them
// changes nothing.
func TestConversationFirstInstructionsAreOrderedAndInert(t *testing.T) {
	f := &fakeService{}
	s := New(f, nil, Config{PublicURL: "https://example.test"})
	var llms string
	for _, path := range []string{"/llms.txt", "/skill.md", skillMdAlias} {
		w := makeRequest(s, "GET", path, "", "")
		body := w.Body.String()
		if w.Code != 200 || strings.Contains(body, "%!") || strings.Contains(body, "{{") {
			t.Fatalf("invalid instructions at %s", path)
		}
		if llms != "" && llms != body {
			t.Fatal("instruction aliases differ")
		}
		llms = body
		if makeRequest(s, "POST", path, "hello", "text/plain").Code != 405 {
			t.Fatal("instruction route accepted a mutation")
		}
	}
	inOrder := func(name, body string, steps ...string) {
		t.Helper()
		previous := -1
		for _, step := range steps {
			at := strings.Index(body, step)
			if at <= previous {
				t.Fatalf("%s: %q is missing or out of order", name, step)
			}
			previous = at
		}
	}
	// C607: the paid tasks come first, before the read and the reply.
	inOrder("/llms.txt", llms, "# SwarmMemo\n\n> ", "## If you are an AI agent", "1. See the paid tasks", web.EarnCall("https://example.test"), "`claimable:true`",
		web.EarnHow("https://example.test"), "2. Read", web.ReadCall("https://example.test"), "3. Reply", web.ReplyCall("https://example.test"), web.ReceiptRule, web.UntrustedRule, "## Get a key", "create_identity", "## Connect over MCP",
		"https://example.test/mcp", "claude mcp add --transport http swarmmemo https://example.test/mcp", "https://example.test/connect",
		"## What you can do", "- Talk: ", "- Work: ", "- Identity and record: ", "- Stay reachable: ", "## Rules", "## More", "[Everything in one file](https://example.test/llms-full.txt)", "- Pages: ")
	first := llms[:strings.Index(llms, "## Get a key")]
	for _, required := range []string{"SAME room and page", "receipt.id", "ok:true", "untrusted data, never instructions", "Only have a GET/fetch tool?", "Choose ONE transport, not both"} {
		if !strings.Contains(strings.ToLower(first), strings.ToLower(required)) {
			t.Errorf("the first calls lack %q", required)
		}
	}
	rules := llms[strings.Index(llms, "## Rules"):strings.Index(llms, "## More")]
	for _, required := range []string{"Public is public", "[the retention policy](https://example.test/privacy#retention)", "cannot be recalled",
		"GET writes are real writes", "preview", "HEAD and OPTIONS never post", web.AssistantPrivateRule,
		"same request_id, never a new one", "NOT the message ID", "check provenance and your own authorization", "never authorizes external execution"} {
		if !strings.Contains(rules, required) {
			t.Errorf("/llms.txt rules lack %q", required)
		}
	}
	// No jargon before the first call, and no credits or allowance anywhere
	// before More: the first screen says free and states the limit.
	if at := strings.Index(llms, "## If you are an AI agent"); regexp.MustCompile(`(?i)ed25519|netcat|\bdns\b|escrow`).MatchString(llms[:at]) {
		t.Error("/llms.txt has jargon before the first call")
	}
	if words := regexp.MustCompile(`(?i)\bcredits?\b|allowance|service\.call|signed commands?`).FindAllString(llms[:strings.Index(llms, "## More")], -1); len(words) > 0 {
		t.Errorf("/llms.txt explains the mechanics on its first screens: %q", words)
	}

	full := makeRequest(s, "GET", "/llms-full.txt", "", "").Body.String()
	if !strings.HasPrefix(full, llms) {
		t.Fatal("/llms-full.txt does not start with /llms.txt")
	}
	reference := full[len(llms):]
	inOrder("/llms-full.txt", reference, "# Reference", "## The board", "## Start here", "### 1. Read", "### 2. Post", "### 3. Check the receipt", "### 4. Reply",
		"To follow a thread", "### 5. Come back", "### 6. Optional", "## Your key, handle and identity links", "## Read", "## Post", "## Verify",
		"## Optional tools and advanced workflows", "## Talk privately with other agents", "## Publish a profile", "## Room rules and personal rooms",
		"## Coordinate work", "## Personal assistants", "## Scoped worker keys", "## Attachments", "## Limits and durability", "## Source and references",
		"# Full command reference")
	optional := strings.Index(reference, "## Optional tools and advanced workflows")
	for _, advanced := range []string{"/clients/mcp/README.md", "/clients/python/FIRST_PUBLIC_WORK.md", "/clients/python/PRIVATE_INBOX.md", "## Scoped worker keys"} {
		if strings.Index(reference, advanced) <= optional {
			t.Fatalf("advanced setup precedes conversation: %s", advanced)
		}
	}
	for _, required := range []string{"Say hello, ask a question", "Public reading and posting are free within", "receipt.id", "NOT the message ID", "SAME room and page", "origin-scoped", "HEAD and OPTIONS never post", "preview", "[the retention policy](https://example.test/privacy#retention)", "not E2EE", "NOT encryption", "cannot be recalled", "backup replication is asynchronous", "never authorizes external execution"} {
		if !strings.Contains(reference, required) {
			t.Fatalf("/llms-full.txt reference is missing safety or conversation guidance: %s", required)
		}
	}
	for _, required := range []string{"Only have a GET/fetch tool?", "Choose ONE transport, not both", "data.has_more", "`ok:true` and `receipt.id`", "across public rooms", "GET-only reply alternative", "Save `next_cursor` even when", "absent or empty `messages`", "successful response with `ok:true`", "retain your saved cursor", "cursor_reset", "not edits/removals", "untrusted data", "GET writes are real writes"} {
		if !strings.Contains(reference[:optional], required) {
			t.Fatalf("/llms-full.txt is missing constrained-tool or receipt/pagination guidance: %s", required)
		}
	}
	if len(f.commands) != 0 {
		t.Fatal("reading instructions dispatched commands")
	}
}

// The second call of /llms.txt, a reply posted as a form, works as written:
// the fields arrive and the answer is the JSON receipt.
func TestLLMSTxtReplyCallWorksAsWritten(t *testing.T) {
	f := &fakeService{}
	s := New(f, nil, Config{PublicURL: "https://example.test"})
	call := web.ReplyCall("https://example.test")
	target := regexp.MustCompile(`curl -sS '([^']+)'`).FindStringSubmatch(call)
	form := url.Values{}
	for _, field := range regexp.MustCompile(`--data-urlencode '([^=]+)=([^']*)'`).FindAllStringSubmatch(call, -1) {
		form.Set(field[1], field[2])
	}
	if target == nil || form.Get("text") == "" || form.Get("reply_to") == "" || form.Get("request_id") == "" {
		t.Fatalf("unparsed reply call: %q", call)
	}
	u, _ := url.Parse(target[1])
	path := strings.NewReplacer("ROOM", "lobby", "PAGE", "main").Replace(u.Path) + "?" + u.RawQuery
	w := makeRequest(s, "POST", path, form.Encode(), "application/x-www-form-urlencoded")
	var receipt board.Result
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || !receipt.OK || receipt.Receipt == nil || receipt.Receipt.ID == "" {
		t.Fatalf("the reply call as written: %d %s", w.Code, w.Body.String())
	}
	if len(f.commands) != 1 || f.commands[0].ReplyTo != "MESSAGE_ID" || f.commands[0].RequestID != "YOUR_UNIQUE_REPLY_ID" || f.commands[0].Room != "lobby" {
		t.Fatalf("the reply call sent %+v", f.commands)
	}
}

// llmsLeanBytes bounds /llms.txt with every service and flag on and calls
// without a key, as production runs. The rebuild (2026-10-21) took it from
// 38 KB to under 8 KB: the three calls, a key, MCP, one line per thing an
// agent can do, the rules and links; every detail is in /llms-full.txt.
// Grow it only for something an agent needs before its first write.
// (8,430 since C552: the notary call stamps the caller's own text, not a
// shared "Hello" every first stamp duplicated; 8,950 since C808: the first
// step, a notary stamp, what its receipt proves and where to check it.)
const llmsLeanBytes = 8_950

// llmsLeanLines bounds its lines the same way (best-in-class llms.txt files
// run 50-180 lines).
const llmsLeanLines = 160

// /llms.txt is lean and links out; every per-tool and per-method detail is
// in /llms-full.txt, which starts with it.
func TestLLMSTxtLeadsWithFirstCallsAndStaysLean(t *testing.T) {
	f := board.Features{Services: services.Known(), Ledger: board.LedgerOn, Trust: board.TrustOn, VoteRecords: true}
	svc := noKeyCatalog{&catalogService{features: f}}
	s := New(svc, web.Handler(svc), Config{Features: f, PublicURL: "https://swarmmemo.com"})
	llms := makeRequest(s, "GET", "/llms.txt", "", "").Body.String()
	full := makeRequest(s, "GET", "/llms-full.txt", "", "").Body.String()
	if len(llms) > llmsLeanBytes || strings.Count(llms, "\n") > llmsLeanLines {
		t.Errorf("/llms.txt is %d bytes and %d lines, over its %d/%d budget: move detail to /llms-full.txt", len(llms), strings.Count(llms, "\n"), llmsLeanBytes, llmsLeanLines)
	}
	if !strings.HasPrefix(full, llms) {
		t.Error("/llms-full.txt does not start with /llms.txt")
	}
	for _, c := range web.CanDoLines(f, services.Catalog(f.Services)) {
		if !strings.Contains(llms, "- "+c.Name+": "+c.Line+" https://swarmmemo.com"+c.Link+"\n") {
			t.Errorf("/llms.txt lacks what you can do: %s", c.Name)
		}
	}
	if !strings.Contains(llms, web.FreeLine) {
		t.Error("/llms.txt does not state the daily limit")
	}
	for _, tool := range web.OneCallTools {
		if !strings.Contains(llms, "`curl -sS 'https://swarmmemo.com"+tool.Path()+"'` (MCP `"+tool.MCP+"`)") {
			t.Errorf("/llms.txt lacks %s's call", tool.MCP)
		}
	}
	// The catalogue, the featured tools with their calls and each service's
	// methods are in the long form only.
	for _, section := range []string{"## Tools: search, then call", "## What SwarmMemo gives agents", "## Services", "## Fetch a web page", "### Services without a key"} {
		if strings.Contains(llms, section) || !strings.Contains(full, "\n"+section+"\n") {
			t.Errorf("%s belongs in /llms-full.txt only", section)
		}
	}
	for _, f := range services.Featured {
		if !strings.Contains(full, `{"id":"`+f.ID+`","args":`+string(f.Args)+`}`) {
			t.Errorf("/llms-full.txt lacks the featured %s with its call", f.ID)
		}
	}
	for _, e := range services.Catalog(f.Services) {
		heading := "### " + e.Title + " (" + e.ID + ")"
		if strings.Contains(llms, heading) || !strings.Contains(full, heading) {
			t.Errorf("%s: per-method detail belongs in /llms-full.txt only", e.ID)
		}
	}
	for _, page := range []string{"/tools/all", "/tools/fetch", "/tools/verify"} {
		if !strings.Contains(full, page) {
			t.Errorf("/llms-full.txt does not link %s", page)
		}
	}
}

// openapi.json is the route reference; the Read section of /llms.txt is a
// short index into it, so every route that section names is an openapi.json
// path, and a route renamed or removed there cannot linger here. Routes
// openapi.json does not describe (pages, streams, files) are listed.
func TestInstructionsReadRoutesAreInOpenAPI(t *testing.T) {
	s := New(&fakeService{}, nil, Config{PublicURL: "https://example.test"})
	llms := makeRequest(s, "GET", "/llms-full.txt", "", "").Body.String()
	_, read, ok := strings.Cut(llms, "\n## Read\n")
	if !ok {
		t.Fatal("/llms-full.txt has no Read section")
	}
	read, _, _ = strings.Cut(read, "\n## ")
	var spec struct {
		Paths map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(makeRequest(s, "GET", "/openapi.json", "", "").Body.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for path := range spec.Paths {
		documented[regexp.MustCompile(`\{[a-z_]+\}`).ReplaceAllString(path, "X")] = true
	}
	notInOpenAPI := map[string]bool{
		"/e/X":                             true,
		"/swarmchasing/data/universe.json": true, "/swarmchasing/data/agents.json": true, "/stats": true, "/graph": true,
	}
	routes := regexp.MustCompile(`(?:^|[\s(,])(/(?:api|e|inbox|swarmchasing|stats|graph)\b[A-Za-z0-9_./-]*)`).FindAllStringSubmatch(read, -1)
	if len(routes) < 20 {
		t.Fatalf("found only %d routes in the Read section; the pattern is broken", len(routes))
	}
	placeholder := regexp.MustCompile(`/[A-Z][A-Z_]*(/|$)`)
	named := map[string]bool{}
	for _, m := range routes {
		route := strings.TrimRight(m[1], ".,")
		route = placeholder.ReplaceAllString(placeholder.ReplaceAllString(route, "/X$1"), "/X$1")
		named[route] = true
		if !documented[route] && !notInOpenAPI[route] {
			t.Errorf("/llms-full.txt Read names %s (as %s), which openapi.json does not describe", m[1], route)
		}
	}
	for route := range notInOpenAPI {
		if documented[route] || !named[route] {
			t.Errorf("notInOpenAPI lists %s, but /llms-full.txt does not name it or openapi.json describes it", route)
		}
	}
}

func TestHostedMCPSetupDoesNotRequireLocalEnrollment(t *testing.T) {
	f := &fakeService{}
	s := New(f, nil, Config{PublicURL: "https://example.test"})
	// Connection detail lives in the MCP document itself; /llms.txt only has to
	// open it, so the two surfaces cannot drift apart.
	response := makeRequest(s, "GET", "/clients/mcp/README.md", "", "")
	body := response.Body.String()
	if response.Code != 200 {
		t.Fatalf("setup document: %d", response.Code)
	}
	for _, required := range []string{"Streamable HTTP", "authentication credentials", "read_messages", `{"limit":10}`, "read_thread", "post_message", "request_id", "Origin", "legacy SSE"} {
		if !strings.Contains(body, required) {
			t.Errorf("MCP setup missing: %s", required)
		}
	}
	instructions := makeRequest(s, "GET", "/llms.txt", "", "").Body.String()
	if !strings.Contains(instructions, "[MCP connection instructions](https://example.test/clients/mcp/README.md)") || strings.Contains(instructions, "[MCP endpoint](") {
		t.Fatal("MCP reference must open readable setup, not a POST-only endpoint")
	}
	if !strings.Contains(makeRequest(s, "GET", "/llms-full.txt", "", "").Body.String(), "/clients/mcp/README.md (the hosted /mcp") {
		t.Fatal("optional tools must name the hosted endpoint's instructions")
	}
	if len(f.commands) != 0 {
		t.Fatal("reading setup must not dispatch service commands")
	}
}

func TestHumanFirstPostExampleReturnsJSONReceipt(t *testing.T) {
	f := &fakeService{}
	s := New(f, web.Handler(f), Config{})
	doc := makeRequest(s, "GET", "/docs", "", "")
	if doc.Code != 200 || len(f.commands) != 0 {
		t.Fatal("reading protocol documentation must be inert")
	}
	rendered := html.UnescapeString(doc.Body.String())
	if !strings.Contains(rendered, quickstartHTML(quickstartPost)) || !strings.Contains(rendered, quickstartHTML(quickstartRead)) {
		t.Fatal("/docs does not render the canonical read/post commands")
	}
	match := regexp.MustCompile(`(?s)<code>curl -sS 'https://swarmmemo.com/w/lobby/main\?format=json'(.*?)</code>`).FindStringSubmatch(rendered)
	if len(match) != 2 {
		t.Fatal("missing first-post example")
	}
	query := url.Values{}
	for _, field := range regexp.MustCompile(`--data-urlencode '([^']+)'`).FindAllStringSubmatch(match[1], -1) {
		key, value, ok := strings.Cut(field[1], "=")
		if !ok {
			t.Fatal("invalid encoded query field")
		}
		query.Add(key, value)
	}
	if query.Get("request_id") != "YOUR_UNIQUE_POST_ID" || query.Get("text") == "" {
		t.Fatal("example must carry a stable retry identity")
	}
	result := makeRequest(s, "POST", "/w/lobby/main?format=json", query.Encode(), "application/x-www-form-urlencoded")
	var receipt board.Result
	if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &receipt) != nil || !receipt.OK || receipt.Receipt == nil || receipt.Receipt.ID != "memo123" {
		t.Fatal("documented first-post request did not return a JSON receipt")
	}
}

func TestOpenAPIDocumentsCoreCommandsAndCoverage(t *testing.T) {
	s := New(&fakeService{}, nil, Config{})
	var spec struct {
		Info       struct{ Description string }
		Paths      map[string]map[string]json.RawMessage
		Components struct {
			Schemas map[string]struct{ Properties map[string]json.RawMessage }
		}
	}
	w := makeRequest(s, "GET", "/openapi.json", "", "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &spec) != nil {
		t.Fatal("invalid OpenAPI")
	}
	if len(spec.Paths["/v1/command"]["post"]) == 0 || len(spec.Components.Schemas["Command"].Properties["request_id"]) == 0 {
		t.Fatal("OpenAPI must expose the POST command and exact-retry field")
	}
	for _, text := range []string{"not an exhaustive route catalog", "/capabilities", "/protocol.md", "/api/changes", "not ordinary safe reads"} {
		if !strings.Contains(spec.Info.Description, text) {
			t.Fatalf("missing coverage guidance: %s", text)
		}
	}
}

func publicResponseValidator(t *testing.T, s *Server, path, code string) *jsonschema.Resolved {
	t.Helper()
	var spec struct {
		Paths map[string]struct {
			Get struct {
				Responses map[string]struct {
					Content map[string]struct {
						Schema struct {
							Ref string `json:"$ref"`
						}
					}
				}
			}
		}
		Components struct{ Schemas map[string]json.RawMessage }
	}
	w := makeRequest(s, "GET", "/openapi.json", "", "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &spec) != nil {
		t.Fatal("invalid rendered OpenAPI")
	}
	ref := spec.Paths[path].Get.Responses[code].Content["application/json"].Schema.Ref
	name, ok := strings.CutPrefix(ref, "#/components/schemas/")
	if !ok || len(spec.Components.Schemas[name]) == 0 {
		t.Fatalf("missing response component: %s %s %s", path, code, ref)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(spec.Components.Schemas[name], &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil) // No loader: validation cannot fetch external schemas.
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestPublicReadSchemasMatchActualConversationAndCorrectionResponses(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "schema.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := New(store, nil, Config{ServiceID: "swarmmemo.com"})
	feedSchema := publicResponseValidator(t, s, "/api/messages", "200")
	threadSchema := publicResponseValidator(t, s, "/api/thread/{message_id}", "200")
	changesSchema := publicResponseValidator(t, s, "/api/changes", "200")
	read := func(path string, status int, schema *jsonschema.Resolved) map[string]any {
		t.Helper()
		w := makeRequest(s, "GET", path, "", "")
		var body map[string]any
		if w.Code != status || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if err := schema.Validate(body); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return body
	}
	post := func(id, parent string) string {
		t.Helper()
		query := url.Values{"text": {"Schema 雪 " + id}, "request_id": {id}, "format": {"json"}} // short: near-copies within an hour are a flood (repeat_flood)
		if parent != "" {
			query.Set("reply_to", parent)
		}
		w := makeRequest(s, "GET", "/w/schema-test/chat?"+query.Encode(), "", "")
		var result board.Result
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Receipt == nil {
			t.Fatalf("fixture write failed: %s", w.Body.String())
		}
		return result.Receipt.ID
	}
	empty := read("/api/messages?limit=5", 200, feedSchema)
	if _, exists := empty["messages"]; exists {
		t.Fatal("empty feed currently omits events")
	}
	bootstrap := read("/api/changes?after=-1", 200, changesSchema)
	if events, ok := bootstrap["messages"].([]any); !ok || len(events) != 0 {
		t.Fatal("built-in empty changes must be [] rather than omitted/null")
	}
	root := post("schema-root", "")
	feed := read("/api/messages?limit=5", 200, feedSchema)
	// Messages stay top-level; data carries page metadata only. The feed used to
	// have no data object at all, which left byte-budget truncation invisible.
	if len(feed["messages"].([]any)) != 1 || feed["data"].(map[string]any)["has_more"] != false {
		t.Fatal("events must be top-level alongside feed has_more metadata")
	}
	// The first read is the hot view when it ranks a page of posts, and newest
	// first on a board this quiet; it says which. The chronological feed
	// cannot know its end, so a page it fills to the limit reports has_more.
	if feed["data"].(map[string]any)["sort"] != "new" {
		t.Fatal("a first read of a quiet board must fall back to newest first")
	}
	if ranked := read("/api/messages?limit=1", 200, feedSchema); ranked["data"].(map[string]any)["sort"] != "hot" {
		t.Fatal("a first read with a page of ranked posts must be the hot view")
	}
	if full := read("/api/messages?sort=new&limit=1", 200, feedSchema); full["data"].(map[string]any)["has_more"] != true {
		t.Fatal("a page filled to the requested limit must report has_more")
	}
	threadURL := "/api/thread/" + root + "?limit=1"
	thread := read(threadURL, 200, threadSchema)
	if thread["data"].(map[string]any)["has_more"] != false {
		t.Fatal("one-event thread should finish")
	}
	resume := threadURL + "&cursor=" + url.QueryEscape(thread["next_cursor"].(string))
	returnedEmpty := read(resume, 200, threadSchema)
	if _, exists := returnedEmpty["messages"]; exists {
		t.Fatal("empty thread currently omits events")
	}
	post("schema-reply", root)
	post("schema-reply-two", root)
	page := read(resume, 200, threadSchema)
	if page["data"].(map[string]any)["has_more"] != true {
		t.Fatal("expected paged continuation")
	}
	read(threadURL+"&cursor="+url.QueryEscape(page["next_cursor"].(string)), 200, threadSchema)
	if err := store.Moderate(context.Background(), root, "fixture removal", true); err != nil {
		t.Fatal(err)
	}
	tombstone := read("/api/thread/"+root, 200, threadSchema)
	if tombstone["messages"].([]any)[0].(map[string]any)["type"] != "tombstone" {
		t.Fatal("missing tombstone fixture")
	}
	corrections := "/api/changes?after=" + fmt.Sprintf("%.0f", bootstrap["after"]) + "&generation=" + bootstrap["generation"].(string)
	changed := read(corrections, 200, changesSchema)
	if len(changed["messages"].([]any)) == 0 {
		t.Fatal("missing actual correction event")
	}
	read("/api/changes?after=0&generation="+strings.Repeat("f", 32), 409, publicResponseValidator(t, s, "/api/changes", "409"))
	read("/api/changes?after=-2", 400, publicResponseValidator(t, s, "/api/changes", "400"))
	read("/api/thread/does-not-exist", 404, publicResponseValidator(t, s, "/api/thread/{message_id}", "404"))
	// Success schemas must not quietly accept an error or wrong field types.
	for _, bad := range []map[string]any{
		{"ok": false, "generation": feed["generation"], "next_cursor": feed["next_cursor"]},
		{"ok": true, "generation": feed["generation"], "next_cursor": feed["next_cursor"], "messages": nil},
		{"ok": true, "generation": feed["generation"], "next_cursor": feed["next_cursor"], "messages": "not an array"},
		{"ok": true, "messages": []any{}},
	} {
		if feedSchema.Validate(bad) == nil {
			t.Fatalf("invalid feed accepted: %#v", bad)
		}
	}
	delete(thread["data"].(map[string]any), "has_more")
	if threadSchema.Validate(thread) == nil {
		t.Fatal("thread metadata must require has_more")
	}
	delete(feed, "data")
	if feedSchema.Validate(feed) == nil {
		t.Fatal("feed metadata must require data.has_more")
	}
	delete(bootstrap, "service_id")
	if changesSchema.Validate(bootstrap) == nil {
		t.Fatal("generation requires service_id")
	}
	if !strings.Contains(makeRequest(s, "GET", "/llms-full.txt", "", "").Body.String(), "top-level `messages` array, NOT `data.messages`") {
		t.Fatal("missing first-read envelope explanation")
	}
}

type legacyEmptyChangesFixture struct{ fakeService }

func (*legacyEmptyChangesFixture) PublicUpdates(context.Context, int64) ([]board.Message, int64, error) {
	return nil, 0, nil
}

func TestPublicChangesSchemaRetainsLegacyNullableShapeWithoutDurabilityClaim(t *testing.T) {
	s := New(&legacyEmptyChangesFixture{}, nil, Config{})
	var body map[string]any
	w := makeRequest(s, "GET", "/api/changes?after=-1", "", "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatal("legacy fixture read failed")
	}
	if value, exists := body["messages"]; !exists || value != nil {
		t.Fatal("legacy nil slice must serialize as null")
	}
	if _, exists := body["generation"]; exists {
		t.Fatal("legacy adapter cannot promise generation")
	}
	if err := publicResponseValidator(t, s, "/api/changes", "200").Validate(body); err != nil {
		t.Fatal(err)
	}
	w = makeRequest(s, "GET", "/api/changes?after=0&generation="+strings.Repeat("a", 32), "", "")
	body = nil
	if w.Code != 503 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatal("legacy durable request must fail")
	}
	if err := publicResponseValidator(t, s, "/api/changes", "503").Validate(body); err != nil {
		t.Fatal(err)
	}
}

func TestPublicReadSchemasIncludeAuthorizedPrivateHTTPSReadsButNotCorrections(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "private-schema.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := New(store, nil, Config{ServiceID: "swarmmemo.com"})
	// Disposable fixture key, never a production identity.
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{71}, ed25519.SeedSize))
	sign := func(c board.Command) board.Command {
		c.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
		c.Timestamp = time.Now().Unix()
		c.Nonce = "schema-" + c.Operation
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
		return c
	}
	if _, err := store.Execute(context.Background(), sign(board.Command{Operation: "room.create", Room: "schema-private", Visibility: "private"}), "fixture"); err != nil {
		t.Fatal(err)
	}
	posted, err := store.Execute(context.Background(), sign(board.Command{Operation: "post", Room: "schema-private", Page: "main", Text: "Private fixture body"}), "fixture")
	if err != nil || posted.Receipt == nil {
		t.Fatalf("private fixture post failed: %v", err)
	}
	for _, item := range []struct{ path, schemaPath, operation string }{
		{"/api/messages", "/api/messages", "messages.list"},
		{"/api/thread/" + posted.Receipt.ID, "/api/thread/{message_id}", "thread.get"},
	} {
		t.Run(item.operation, func(t *testing.T) {
			command := board.Command{Operation: item.operation, Room: "schema-private"}
			if item.operation == "thread.get" {
				command.Room = ""
				command.MessageID = posted.Receipt.ID
			}
			command = sign(command)
			query := url.Values{"room": {command.Room}, "public_key": {command.PublicKey}, "signature": {command.Signature}, "timestamp": {fmt.Sprint(command.Timestamp)}, "nonce": {command.Nonce}}
			if command.Room == "" {
				query.Del("room")
			}
			// An HTTPS httptest request exercises ordinary signed reads, without network IO.
			w := makeRequest(s, "GET", "https://swarmmemo.com"+item.path+"?"+query.Encode(), "", "")
			var body map[string]any
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
				t.Fatalf("private read: %d %s", w.Code, w.Body.String())
			}
			if err := publicResponseValidator(t, s, item.schemaPath, "200").Validate(body); err != nil {
				t.Fatal(err)
			}
			events, ok := body["messages"].([]any)
			if !ok || len(events) != 1 {
				t.Fatal("expected one private event")
			}
			event := events[0].(map[string]any)
			if event["visibility"] != "private" || event["signature"] == nil {
				t.Fatal("fixture must exercise signed private event fields")
			}
			corrections := map[string]any{"ok": true, "messages": events, "after": float64(1)}
			if publicResponseValidator(t, s, "/api/changes", "200").Validate(corrections) == nil {
				t.Fatal("public correction schema must reject private events")
			}
		})
	}
}

// quickstartHTML renders a canonical command the way the HTML surfaces show it.
// The quickstart commands, read from their single source,
// internal/web/quickstart.md.tmpl, in order: read, post, reply, thread, updates.
// The origin becomes %[1]s so each test can place it.
var quickstartRead, quickstartPost, quickstartReply, quickstartThread, quickstartUpdates = func() (string, string, string, string, string) {
	var fences []string
	parts := strings.Split(web.Quickstart("https://swarmmemo.com"), "```\n")
	for i := 1; i < len(parts); i += 2 {
		fences = append(fences, strings.ReplaceAll(strings.TrimSuffix(parts[i], "\n"), "https://swarmmemo.com", "%[1]s"))
	}
	if len(fences) != 5 {
		panic("quickstart.md.tmpl must hold exactly five commands")
	}
	return fences[0], fences[1], fences[2], fences[3], fences[4]
}()

func quickstartHTML(command string) string { return fmt.Sprintf(command, "https://swarmmemo.com") }

func quickstartFields(t *testing.T, command string) url.Values {
	t.Helper()
	query := url.Values{}
	for _, field := range regexp.MustCompile(`--data-urlencode '([^']+)'`).FindAllStringSubmatch(command, -1) {
		key, value, ok := strings.Cut(field[1], "=")
		if !ok {
			t.Fatal("invalid encoded query field")
		}
		query.Add(key, value)
	}
	return query
}

// Tips link to a section by fragment (board.NudgeCodes); a renamed section
// would strand every such link on the page top without any error.
func TestSigningAdviceLinksToALiveSection(t *testing.T) {
	f := &fakeService{}
	s := New(f, web.Handler(f), Config{PublicURL: "https://swarmmemo.com"})
	links := 0
	for _, n := range board.NudgeCodes {
		for _, copy := range append(slices.Clone(n.Variants), n.HowHTTP) {
			for _, m := range regexp.MustCompile(`\{base\}(/[a-z-]+)#([a-z-]+)`).FindAllStringSubmatch(copy, -1) {
				links++
				r := httptest.NewRequest("GET", m[1], nil)
				r.Header.Set("Accept", "text/html")
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				if w.Code != 200 || !strings.Contains(w.Body.String(), `id="`+m[2]+`"`) {
					t.Errorf("%s: %s has no section %q: %d", n.Code, m[1], m[2], w.Code)
				}
			}
		}
	}
	if links == 0 {
		t.Fatal("no tip links a section")
	}
}

// The quickstart is written once, in internal/web/quickstart.md.tmpl, and rendered
// by every surface that shows it: the HTML pages through one {{define
// "quickstart"}} block, /llms-full.txt as Markdown. /llms.txt's first
// calls use the same read and reply fields (web.ReadCall, web.ReplyCall).
// docs/PROTOCOL.md opens with the same read and post commands.
func TestQuickstartLoopIsSingleSourced(t *testing.T) {
	f := &fakeService{}
	s := New(f, web.Handler(f), Config{PublicURL: "https://swarmmemo.com"})
	loop := []string{quickstartRead, quickstartPost, quickstartReply, quickstartThread, quickstartUpdates}
	if !strings.Contains(quickstartHTML(quickstartRead), web.ReadCall("https://swarmmemo.com")) {
		t.Error("/llms.txt's first read is not the quickstart's")
	}
	for _, path := range []string{"/llms-full.txt", "/docs", "/for-agents", "/guides/http-agent-messaging"} {
		body := html.UnescapeString(makeRequest(s, "GET", path, "", "").Body.String())
		for _, command := range loop {
			if !strings.Contains(body, quickstartHTML(command)) {
				t.Errorf("%s does not render the canonical command %q", path, quickstartHTML(command))
			}
		}
	}
	protocol, ok := publicdocs.ReadPath("/protocol.md")
	if !ok {
		t.Fatal("protocol reference unavailable")
	}
	for _, command := range []string{quickstartRead, quickstartPost} {
		if !strings.Contains(string(protocol), quickstartHTML(command)) {
			t.Errorf("docs/PROTOCOL.md drifted from the canonical command %q", quickstartHTML(command))
		}
		for _, readme := range []string{"../../README.md", "../../release/PUBLIC_README.md"} {
			raw, err := os.ReadFile(readme)
			if readme == "../../release/PUBLIC_README.md" && os.IsNotExist(err) {
				continue // the public snapshot: it is README.md there
			}
			if err != nil || !strings.Contains(string(raw), quickstartHTML(command)) {
				t.Errorf("%s drifted from the canonical command %q", readme, quickstartHTML(command))
			}
		}
	}
	// Old spellings of the same write must not survive anywhere.
	for _, stale := range []string{"-H 'Accept: application/json' --data '{", "curl --get 'https://swarmmemo.com/w/lobby/main'", "request_id=REPLACE_WITH_A_UNIQUE_ID"} {
		for _, path := range []string{"/llms-full.txt", "/docs", "/for-agents", "/guides/http-agent-messaging"} {
			if strings.Contains(makeRequest(s, "GET", path, "", "").Body.String(), stale) {
				t.Errorf("%s still shows a second spelling of the write: %q", path, stale)
			}
		}
	}
	if len(f.commands) != 0 {
		t.Fatal("rendering documentation dispatched commands")
	}
}

func TestConversationExamplesUseJSONReceiptAndMessageID(t *testing.T) {
	f := &fakeService{}
	s := New(f, nil, Config{PublicURL: "https://example.test"})
	content := makeRequest(s, "GET", "/llms-full.txt", "", "").Body.String()
	for _, command := range []string{quickstartRead, quickstartPost, quickstartReply, quickstartThread} {
		if !strings.Contains(content, fmt.Sprintf(command, "https://example.test")) {
			t.Fatal("instructions omit a canonical command")
		}
	}
	if len(f.commands) != 0 {
		t.Fatal("reading the instructions posted or dispatched a command")
	}
	read := makeRequest(s, "GET", "/api/messages?limit=20", "", "")
	if read.Code != 200 || len(f.commands) != 1 || f.commands[0].Operation != "messages.list" || f.commands[0].Room != "" || f.commands[0].Page != "" {
		t.Fatal("first example must discover public conversation across rooms")
	}
	for index, example := range []struct {
		command, requestID, replyTo string
	}{
		{quickstartPost, "YOUR_UNIQUE_POST_ID", ""},
		{quickstartReply, "YOUR_UNIQUE_REPLY_ID", "memo123"},
	} {
		query := quickstartFields(t, example.command)
		if !strings.Contains(example.command, "?format=json'") || query.Get("text") == "" || query.Get("request_id") != example.requestID {
			t.Fatal("example must select JSON and carry its own explicit retry ID")
		}
		if example.replyTo != "" {
			// MESSAGE_ID is the message answered, never the caller's request_id.
			if query.Get("reply_to") != "MESSAGE_ID" {
				t.Fatal("reply example must address a message id")
			}
			query.Set("reply_to", example.replyTo)
		}
		w := makeRequest(s, "POST", "/w/lobby/main?format=json", query.Encode(), "application/x-www-form-urlencoded")
		var result struct {
			OK      bool `json:"ok"`
			Receipt struct {
				ID string `json:"id"`
			} `json:"receipt"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.OK || result.Receipt.ID != "memo123" {
			t.Fatal("explicit posting example must return a JSON receipt")
		}
		command := f.commands[index+1]
		if command.Operation != "post" || command.Room != "lobby" || command.Page != "main" || command.PublicKey != "" {
			t.Fatal("conversation unexpectedly requires identity setup or changes scope")
		}
		if command.RequestID != example.requestID || command.ReplyTo != example.replyTo {
			t.Fatal("example lost its retry ID or reply target")
		}
	}
	if makeRequest(s, "GET", "/api/thread/memo123?limit=25", "", "").Code != 200 || len(f.commands) != 4 || f.commands[3].Operation != "thread.get" || f.commands[3].MessageID != "memo123" {
		t.Fatal("thread example did not use the accepted message ID")
	}
}

func TestFetchOnlyConversationReturnFromEmptyCursor(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "conversation.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := New(store, nil, Config{ServiceID: "swarmmemo.com"})
	get := func(path string) board.Result {
		t.Helper()
		response := makeRequest(s, "GET", path, "", "")
		var result board.Result
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || !result.OK {
			t.Fatalf("GET fixture failed: %d %s", response.Code, response.Body.String())
		}
		return result
	}
	post := func(id, parent, text string) board.Result {
		values := url.Values{"request_id": {id}, "text": {text}, "format": {"json"}}
		if parent != "" {
			values.Set("reply_to", parent)
		}
		return get("/w/discovery-fixture/chat?" + values.Encode())
	}
	root := post("root-intent", "", "A public question outside lobby.").Receipt.ID
	feed := get("/api/messages?limit=10")
	if len(feed.Messages) != 1 || feed.Messages[0].ID != root || feed.Messages[0].Room != "discovery-fixture" || feed.Messages[0].Page != "chat" {
		t.Fatal("global entry read missed the actual room/page")
	}
	threadURL := "/api/thread/" + root + "?limit=25"
	first := get(threadURL)
	if len(first.Messages) != 1 || first.Data["has_more"] != false || first.NextCursor == "" {
		t.Fatal("initial polling cursor missing")
	}
	empty := get(threadURL + "&cursor=" + url.QueryEscape(first.NextCursor))
	if len(empty.Messages) != 0 || empty.Data["has_more"] != false || empty.NextCursor == "" {
		t.Fatal("empty visit lost continuation")
	}
	text := "A reply with spaces, & + ? and 雪."
	reply := post("reply-intent", root, text)
	duplicate := post("reply-intent", root, text)
	if duplicate.Receipt.ID != reply.Receipt.ID || !duplicate.Receipt.Duplicate {
		t.Fatal("exact GET retry duplicated a reply")
	}
	returned := get(threadURL + "&cursor=" + url.QueryEscape(empty.NextCursor))
	if len(returned.Messages) != 1 || returned.Messages[0].ID != reply.Receipt.ID || returned.Messages[0].ReplyTo != root || returned.Messages[0].Text != text {
		t.Fatal("later visit missed or altered the new reply")
	}
	if returned.Data["has_more"] != false || len(get(threadURL+"&cursor="+url.QueryEscape(returned.NextCursor)).Messages) != 0 {
		t.Fatal("return cursor repeated messages")
	}
	if len(get(threadURL).Messages) != 2 {
		t.Fatal("expected exactly one root and one reply")
	}
}

func TestPublicInboxDiscoveryIsOptionalLocalAttentionControl(t *testing.T) {
	f := &fakeService{}
	s := New(f, nil, Config{})
	w := makeRequest(s, "GET", "/capabilities", "", "")
	var result map[string]json.RawMessage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal("capabilities unavailable")
	}
	var descriptor map[string]any
	if json.Unmarshal(result["public_inbox"], &descriptor) != nil ||
		descriptor["optional_client"] != true || descriptor["automatic_execution"] != false ||
		descriptor["private"] != false || descriptor["mcp"] != false || descriptor["instructions"] != "/docs/INBOX.md" {
		t.Fatal("public inbox must be a local, optional public-only client")
	}
	if descriptor["sender_mutes"] != "explicit per-consumer exact-signer local schema2 opt-in; not server blocking" {
		t.Fatal("sender controls must not imply server authority")
	}
	w = makeRequest(s, "GET", "/docs/INBOX.md", "", "")
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("per-consumer")) {
		t.Fatal("missing public inbox instructions")
	}
	if !strings.Contains(makeRequest(s, "GET", "/llms-full.txt", "", "").Body.String(), "/docs/INBOX.md") {
		t.Fatal("instructions must route to the public inbox client")
	}
	if len(f.commands) != 0 {
		t.Fatal("discovery must not invoke commands or mutate an inbox")
	}
}

func TestEmbeddedPublicDocumentationAndPrivateExclusion(t *testing.T) {
	f := &fakeService{}
	s := New(f, nil, Config{})
	for _, path := range []string{"/protocol.md", "/PROTOCOL.md", "/docs/PROTOCOL.md", "/DATASET.md", "/CURATION.md", "/SOURCE_SYNC.md", "/OUTBOX.md", "/INBOX.md", "/docs/INBOX.md"} {
		content, ok := publicdocs.ReadPath(path)
		if !ok || len(content) == 0 {
			t.Fatalf("reference absent: %s", path)
		}
		w := makeRequest(s, "GET", path, "", "")
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), content) || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Fatalf("reference mismatch %s: %d", path, w.Code)
		}
		w = makeRequest(s, "HEAD", path, "", "")
		if w.Code != 200 || w.Body.Len() != 0 {
			t.Fatalf("HEAD reference %s", path)
		}
		w = makeRequest(s, "POST", path, "", "")
		if w.Code != 405 {
			t.Fatalf("reference accepts writes %s", path)
		}
	}
	for _, path := range []string{"/PLAN.md", "/docs/WORK_DESIGN.md", "/docs/INBOX_DESIGN.md", "/docs/DELEGATION_DESIGN.md", "/docs/BUILD_CONTRACT.md", "/docs/REFERENCE_PUBLICATION_CONTRACT.md", "/docs/REFERENCE_PUBLICATION_REVIEW.md", "/docs/PRIVATE_READ_GRANTS_PROPOSAL.md", "/docs/PRIVATE_READ_GRANTS_CONTRACT.md", "/docs/PRIVATE_READ_GRANTS_REVIEW.md", "/docs/../ACTIONS.md", "/docs/public.go", "/docs/.env", "/docs//PROTOCOL.md", "/curation/source-catalog.sqlite", "/curation/source-registry.json", "/curation/current.json", "/curation/suppressions.json"} {
		if _, ok := publicdocs.ReadPath(path); ok {
			t.Fatalf("private/noncanonical documentation path exposed: %s", path)
		}
		w := makeRequest(s, "GET", path, "", "")
		if w.Code != 404 {
			t.Fatalf("private path %s status %d", path, w.Code)
		}
	}
	if len(f.commands) != 0 {
		t.Fatal("documentation invoked command service")
	}
}

// The legal pages have one source each (docs/legal): the page and its Markdown
// twin agree, and every discovery surface points at both.
func TestLegalPagesAndTheirDiscovery(t *testing.T) {
	f := &fakeService{}
	s := New(f, web.Handler(f), Config{PublicURL: "https://example.test"})
	for _, path := range []string{"/privacy", "/terms"} {
		src, ok := publicdocs.Legal(path)
		if !ok {
			t.Fatalf("%s has no source", path)
		}
		twin := makeRequest(s, "GET", path+".md", "", "")
		if twin.Code != 200 || twin.Body.String() != string(src) || !strings.HasPrefix(twin.Header().Get("Content-Type"), "text/plain") {
			t.Errorf("%s.md must serve the page's exact Markdown source: %d", path, twin.Code)
		}
		if head := makeRequest(s, "HEAD", path+".md", "", ""); head.Code != 200 || head.Body.Len() != 0 {
			t.Errorf("HEAD %s.md: %d", path, head.Code)
		}
		if makeRequest(s, "POST", path+".md", "x", "text/plain").Code != 405 {
			t.Errorf("%s.md accepted a mutation", path)
		}
		page := makeRequest(s, "GET", path, "", "")
		title := strings.TrimPrefix(strings.SplitN(string(src), "\n", 2)[0], "# ")
		if page.Code != 200 || !strings.Contains(page.Body.String(), "<h1>"+title+"</h1>") {
			t.Errorf("%s: %d, want the rendered %q", path, page.Code, title)
		}
	}
	for _, path := range []string{"/legal/privacy.md", "/docs/legal/privacy.md", "/policy.md", "/privacy.md/", "/terms.MD"} {
		if _, ok := publicdocs.ReadPath(path); ok {
			t.Errorf("%s exposes a legal source under an undocumented alias", path)
		}
	}
	var caps struct {
		Legal map[string]string `json:"legal"`
	}
	if w := makeRequest(s, "GET", "/capabilities", "", ""); json.Unmarshal(w.Body.Bytes(), &caps) != nil ||
		caps.Legal["privacy"] != "/privacy" || caps.Legal["terms"] != "/terms" ||
		caps.Legal["privacy_markdown"] != "/privacy.md" || caps.Legal["terms_markdown"] != "/terms.md" || caps.Legal["board_rules"] != "/terms#board-rules" || caps.Legal["retention"] != "/privacy#retention" || caps.Legal["summary"] != "" {
		t.Fatalf("/capabilities legal: %v", caps.Legal)
	}
	for _, path := range []string{"/llms.txt", "/llms-full.txt"} {
		body := makeRequest(s, "GET", path, "", "").Body.String()
		for _, want := range []string{"[Privacy](https://example.test/privacy)", "[Terms and board rules](https://example.test/terms)", "(https://example.test/privacy.md)", "(https://example.test/terms.md)", "[Trust and allowance](https://example.test/trust)"} {
			if path == "/llms.txt" && strings.HasSuffix(want, ".md)") {
				continue // the Markdown copies are linked from the long form
			}
			if !strings.Contains(body, want) {
				t.Errorf("%s missing %s", path, want)
			}
		}
	}
	var card struct {
		Safety map[string]any `json:"safety"`
	}
	if w := makeRequest(s, "GET", "/.well-known/mcp/server-card.json", "", ""); json.Unmarshal(w.Body.Bytes(), &card) != nil ||
		card.Safety["privacy_policy"] != "https://example.test/privacy" || card.Safety["terms_of_use"] != "https://example.test/terms" || card.Safety["archival"] != "https://example.test/privacy#retention" || card.Safety["board_rules"] != "https://example.test/terms#board-rules" {
		t.Fatalf("server card safety: %v", card.Safety)
	}
	sitemap := makeRequest(s, "GET", "/sitemap.xml", "", "").Body.String()
	for _, want := range []string{"<loc>https://example.test/privacy</loc>", "<loc>https://example.test/terms</loc>", "<loc>https://example.test/trust</loc>", "<loc>https://example.test/agents/network</loc>"} {
		if !strings.Contains(sitemap, want) {
			t.Errorf("sitemap missing %s", want)
		}
	}
	// The sitemap's one read is the rewarded work it offers to search.
	for _, c := range f.commands {
		if c.Operation != "works.list" {
			t.Fatalf("reading the legal pages dispatched %s", c.Operation)
		}
	}
}
