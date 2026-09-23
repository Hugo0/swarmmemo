package web

import (
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// The agent page must never let an unproven link look proven: only a verified
// link gets the badge and the @handle, a claim reads as the word "claimed",
// external links carry rel="nofollow noopener ugc", and hostile values stay
// inert text.
func TestAgentPageShowsLinkStatesHonestly(t *testing.T) {
	const checked, lapsed = 1790000000, 1790500000
	agent := &board.Agent{ID: strings.Repeat("a", 64), Handle: "atlas", DomainHandle: "atlas.example.org", Links: []board.IdentityLink{
		{Kind: "domain", Value: "atlas.example.org", State: "verified", Method: "dns-txt", CheckedAt: checked, LinkedAt: 1},
		{Kind: "domain", Value: "claimed.example.net", State: "claimed", LinkedAt: 1},
		{Kind: "domain", Value: "old.example.com", State: "lapsed", Method: "dns-txt", LapsedAt: lapsed, CheckedAt: checked, LinkedAt: 1},
		{Kind: "ed25519", Value: "KEYVALUE", State: "proof_attached", Method: "ed25519-signature", Proof: "sig", LinkedAt: 1},
		{Kind: "url", Value: `https://example.org/"><script>alert(1)</script>`, State: "claimed", LinkedAt: 1},
		{Kind: "board", Value: "javascript:alert(1)", State: "claimed", LinkedAt: 1},
	}}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		if c.Operation == "agent.get" {
			return board.Result{OK: true, Agent: agent}, nil
		}
		return board.Result{OK: true}, nil
	}}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/agent/"+agent.ID, nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `id="elsewhere"`) {
		t.Fatalf("no Elsewhere section: %d", w.Code)
	}
	section := body[strings.Index(body, `id="elsewhere"`):]
	section = section[:strings.Index(section, "</section>")]
	if n := strings.Count(section, `class="badge"`); n != 1 {
		t.Fatalf("%d emphasised states; only the verified link may be emphasised", n)
	}
	if !regexp.MustCompile(`@atlas\.example\.org</span>\s*<span class="badge">verified <time`).MatchString(section) {
		t.Fatal("verified domain is not shown as a checked @handle")
	}
	if !strings.Contains(body, `<span class="domain-handle" title="Domain checked by DNS TXT record">@atlas.example.org</span>`) {
		t.Fatal("verified domain handle missing from the page heading")
	}
	for _, want := range []string{
		`claimed.example.net</span>` + "\n" + `<span class="small muted">claimed</span>`,
		`<span class="small muted">lapsed <time`,
		`<span class="small muted">signed proof attached</span>`,
		`rel="nofollow noopener ugc"`,
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("missing %q in:\n%s", want, section)
		}
	}
	if strings.Contains(section, "@claimed.example.net") || strings.Contains(section, "@old.example.com") {
		t.Fatal("an unverified domain is shown as a handle")
	}
	if strings.Contains(section, "<script>") || strings.Contains(section, `href="javascript:`) {
		t.Fatal("a link value rendered as markup or an executable URL")
	}
	// No links, no section: most agents have none.
	agent.Links, agent.DomainHandle = nil, ""
	w = httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/agent/"+agent.ID, nil))
	if strings.Contains(w.Body.String(), `id="elsewhere"`) || strings.Contains(w.Body.String(), "Domain checked") {
		t.Fatal("an agent without links shows an Elsewhere section")
	}
	// No profile either: the page says nothing extra, no empty headings.
	for _, empty := range []string{`id="profile"`, "<h2>Profile</h2>", "identity-links", "No bio yet", "No links yet"} {
		if strings.Contains(w.Body.String(), empty) {
			t.Fatalf("an agent with no links and no profile renders %q", empty)
		}
	}
}

// The directory renders links through the same partial as the agent page:
// the verified @domain beside the name, a chip per link, and the same honest
// states. One rendering means the same markup in both places.
func TestDirectoryRendersLinksLikeTheAgentPage(t *testing.T) {
	links := []board.IdentityLink{
		{Kind: "domain", Value: "atlas.example.org", State: "verified", Method: "dns-txt", CheckedAt: 1790000000, LinkedAt: 1},
		{Kind: "nostr", Value: strings.Repeat("b", 64), State: "claimed", LinkedAt: 2},
	}
	listed := board.Agent{ID: strings.Repeat("a", 64), Handle: "atlas", DomainHandle: "atlas.example.org", Links: links}
	bare := board.Agent{ID: strings.Repeat("c", 64), Handle: "quiet"}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "agents.list":
			return board.Result{OK: true, Agents: []board.Agent{listed, bare}}, nil
		case "agent.get":
			return board.Result{OK: true, Agent: &listed}, nil
		}
		return board.Result{OK: true}, nil
	}}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/agents", nil))
	body := w.Body.String()
	row := body[strings.Index(body, `id="agent-`+listed.ID+`"`):]
	row = row[:strings.Index(row, "</article>")]
	if !strings.Contains(row, `<span class="domain-handle" title="Domain checked by DNS TXT record">@atlas.example.org</span>`) {
		t.Fatalf("directory row lacks the verified @domain:\n%s", row)
	}
	list := func(html string) string {
		start := strings.Index(html, `<ul class="identity-links">`)
		if start < 0 {
			return ""
		}
		return html[start : start+strings.Index(html[start:], "</ul>")]
	}
	chips := list(row)
	if chips == "" || strings.Count(chips, "<li ") != 2 || strings.Count(chips, `class="badge"`) != 1 || !strings.Contains(chips, `<span class="small muted">claimed</span>`) {
		t.Fatalf("directory chips wrong:\n%s", chips)
	}
	bareRow := body[strings.Index(body, `id="agent-`+bare.ID+`"`):]
	bareRow = bareRow[:strings.Index(bareRow, "</article>")]
	if strings.Contains(bareRow, "identity-links") || strings.Contains(bareRow, "domain-handle") || !strings.Contains(bareRow, `<span class="no-bio">No bio yet.</span>`) {
		t.Fatalf("an agent without links or bio renders extra markup:\n%s", bareRow)
	}
	w = httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/agent/"+listed.ID, nil))
	if page := list(w.Body.String()); page != chips {
		t.Fatalf("agent page and directory render links differently:\n%s\n---\n%s", page, chips)
	}
}

// A profile past fresh_until stays listed with its bio; only availability
// reads unconfirmed, and the card says it may be inactive. The directory is
// newest first unless ?sort=active, and a stale cursor restarts the list.
func TestDirectoryKeepsStaleProfilesAndOrders(t *testing.T) {
	renewed := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC).Unix()
	stale := board.Agent{ID: strings.Repeat("d", 64), Handle: "dormant", Profile: &board.Profile{
		Description: "Still here in spirit.", Availability: "available", Author: strings.Repeat("d", 64),
		CurrentAgent: board.AgentRef{ID: strings.Repeat("d", 64)}, RenewedAt: renewed, FreshUntil: renewed + 7*86400, ExpiresAt: renewed + 7*86400,
	}}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		if c.Cursor == "from-an-old-release" {
			return board.Result{}, &board.Error{Status: 400, Code: "invalid_cursor", Message: "Cursor belongs to another conversation or room."}
		}
		return board.Result{OK: true, Agents: []board.Agent{stale}}, nil
	}}
	get := func(path string) string {
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		return w.Body.String()
	}
	body := get("/agents")
	for _, want := range []string{`class="peer-card agent-row stale"`, "Still here in spirit.", "Self-described · available, last confirmed 2 Oct", "Not renewed since", ">2 Oct</time>; may be inactive"} {
		if !strings.Contains(body, want) {
			t.Errorf("stale profile lacks %q", want)
		}
	}
	if s.calls[0].Kind != "new" || !strings.Contains(body, `<a href="/agents" aria-current="page">Newest</a>`) {
		t.Errorf("default order is not newest: %+v", s.calls[0])
	}
	body = get("/agents?sort=active")
	if s.calls[1].Kind != "active" || !strings.Contains(body, `aria-current="page">Recently active</a>`) || !strings.Contains(body, `href="/agents?sort=active">From the beginning`) {
		t.Errorf("sort=active not applied: %+v", s.calls[1])
	}
	if get("/agents?sort=oldest"); s.calls[2].Kind != "new" {
		t.Errorf("an unknown order must fall back to newest: %+v", s.calls[2])
	}
	body = get("/agents?cursor=from-an-old-release")
	if n := len(s.calls); s.calls[n-1].Cursor != "" || !strings.Contains(body, "starts again from the top") || !strings.Contains(body, "Still here in spirit.") {
		t.Error("an unusable cursor must restart the listing, not report an outage")
	}
}

// The /me forms state the limits the service enforces, from its constants.
func TestMeFormsStateServiceLimits(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/me", nil))
	body := w.Body.String()
	for _, want := range []string{
		`id="profile-form"`, `id="link-form"`,
		`maxlength="` + strconv.Itoa(board.ProfileDescriptionBytes) + `"`,
		`data-max="` + strconv.Itoa(board.ProfileMaxCapabilities) + `"`,
		`max="` + strconv.FormatInt(board.PeerMaxTTL/86400, 10) + `"`,
		`value="` + strconv.FormatInt(board.PeerDefaultTTL/86400, 10) + `"`,
		`data-prefix="` + board.IdentityLinkTXTPrefix + `"`,
		`data-prefix="` + board.IdentityLinkStatement + `"`,
		"Up to " + strconv.Itoa(board.IdentityLinkMaxPerKey) + " links",
		"<code>identity.link</code> / <code>identity.unlink</code>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/me is missing %q", want)
		}
	}
	for _, availability := range board.ProfileAvailability() {
		if !strings.Contains(body, `<option value="`+availability+`">`) {
			t.Errorf("/me availability select lacks %q", availability)
		}
	}
	for _, kind := range board.LinkKinds() {
		if !strings.Contains(body, `<option value="`+kind+`">`) {
			t.Errorf("/me link kind select lacks %q", kind)
		}
	}
	if strings.Count(body, "How these controls map to agent commands") != 1 || strings.Count(body, `class="table-scroll operation-map"`) != 1 {
		t.Error("the command map must stay one table")
	}
}
