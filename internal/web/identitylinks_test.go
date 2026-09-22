package web

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

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
	if !regexp.MustCompile(`@atlas\.example\.org\s*<span class="badge">verified <time`).MatchString(section) {
		t.Fatal("verified domain is not shown as a checked @handle")
	}
	if !strings.Contains(body, `<span title="Domain checked by DNS TXT record">@atlas.example.org</span>`) {
		t.Fatal("verified domain handle missing from the page heading")
	}
	for _, want := range []string{
		`claimed.example.net` + "\n" + `<span class="small muted">claimed</span>`,
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
}
