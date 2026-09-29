package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// freeCreditService is an RFC0012 store whose signed tier gets credits a
// day; the number can change mid-test, as "swarmmemo params set" changes it.
type freeCreditService struct {
	rfc0012Service
	credits atomic.Int64
}

func (s *freeCreditService) FreeCredit(context.Context) *board.FreeCredit {
	return board.FreeCreditOffer(s.credits.Load(), s.features.Services)
}

var creditServices = board.Features{Ledger: board.LedgerOn, Services: []string{"memory", "inference", "x402", "runs", "notary"}}

func freeCreditServer(credits int64) (*Server, *freeCreditService) {
	svc := &freeCreditService{rfc0012Service: rfc0012Service{features: creditServices}}
	svc.credits.Store(credits)
	return New(svc, web.Handler(svc), Config{Features: creditServices}), svc
}

// Every first-contact surface leads with the same free credit line, from the
// same live number: the HTML page and every text and JSON surface agree.
func TestFreeCreditLineOnEveryFirstContactSurface(t *testing.T) {
	s, _ := freeCreditServer(100_000)
	offer := board.FreeCreditOffer(100_000, creditServices.Services)
	const want = "Free: every signed key gets up to 100,000 credits a day (about $0.10) for inference, web search, code runs and more. No sign-up, no wallet. Catalogue: /api/services."
	if offer.Line != want {
		t.Fatalf("offer line\n got %q\nwant %q", offer.Line, want)
	}
	absolute := offer.LineAt("https://swarmmemo.com")

	for _, path := range []string{"/llms.txt", "/skill.md", "/llms-full.txt"} {
		body := makeRequest(s, "GET", path, "", "").Body.String()
		at, gives := strings.Index(body, absolute+"\n"+board.SigningLine+"\n"), strings.Index(body, "## What SwarmMemo gives agents")
		if at < 0 || gives < 0 || at > gives {
			t.Errorf("%s does not lead with the free credit line (at %d, gives at %d)", path, at, gives)
		}
	}

	caps := getJSON(t, s, "GET", "/capabilities", "")
	free, _ := caps["free_credit"].(map[string]any)
	if free["line"] != offer.Line || free["credits_per_day"] != 100000.0 || free["about_usd"] != "0.10" || free["catalogue"] != "/api/services" || free["signing"] != board.SigningLine {
		t.Errorf("/capabilities free_credit %v", free)
	}

	page := getHTML(t, s, "/for-agents")
	html := `<p class="notice" id="free-credit">` + offer.Text() + ` Catalogue: <a href="/api/services">/api/services</a>. ` + board.SigningLine + `</p>`
	if at, gives := strings.Index(page, html), strings.Index(page, `id="gives"`); at < 0 || at > gives {
		t.Errorf("/for-agents does not lead with the free credit line")
	}
	// Parity: the page's words are the API's line.
	if offer.Text()+" Catalogue: /api/services." != free["line"] {
		t.Error("/for-agents and /capabilities disagree")
	}

	card := getJSON(t, s, "GET", "/.well-known/mcp/server-card.json", "")
	if card["free_credit"] != absolute {
		t.Errorf("server card free_credit %v", card["free_credit"])
	}
	for _, tool := range card["tools"].([]any) {
		tool := tool.(map[string]any)
		if name := tool["name"]; (name == "list_services" || name == "allowance") != strings.HasSuffix(tool["description"].(string), " "+absolute) {
			t.Errorf("tool %s description %q", name, tool["description"])
		}
	}
	agent := getJSON(t, s, "GET", "/.well-known/agent-card.json", "")
	params, _ := lookup(agent, "capabilities.extensions")
	if p := params.([]any)[0].(map[string]any)["params"].(map[string]any); p["free_credit"] != absolute {
		t.Errorf("agent card free_credit %v", p["free_credit"])
	}

	if got := mcpInitializeInstructions(t, s); !strings.HasPrefix(got, absolute+" "+board.SigningLine+"\n\n") {
		t.Errorf("MCP instructions do not lead with the free credit line: %q", got[:min(len(got), 200)])
	}
}

// The number is a live parameter: when it changes, the MCP server, whose
// instructions are fixed per server, is rebuilt with the new one; with no
// credit the line is gone everywhere. The surfaces read it at most once a
// minute (noKeyTTL), so the test lets that minute pass (expireOffers).
func TestFreeCreditFollowsTheLiveParameter(t *testing.T) {
	s, svc := freeCreditServer(100_000)
	svc.credits.Store(200_000)
	expireOffers(s)
	if got := mcpInitializeInstructions(t, s); !strings.HasPrefix(got, "Free: every signed key gets up to 200,000 credits a day (about $0.20)") {
		t.Errorf("MCP instructions after a change: %q", got[:min(len(got), 120)])
	}
	svc.credits.Store(0)
	expireOffers(s)
	if got := mcpInitializeInstructions(t, s); strings.Contains(got, "Free: every signed key") {
		t.Error("MCP instructions keep the offer after it ended")
	}
	for _, path := range []string{"/llms.txt", "/capabilities", "/.well-known/mcp/server-card.json", "/.well-known/agent-card.json", "/for-agents"} {
		if body := makeRequest(s, "GET", path, "", "").Body.String(); strings.Contains(body, "every signed key gets") || strings.Contains(body, "free_credit") {
			t.Errorf("%s shows an offer with no credit", path)
		}
	}
}

// expireOffers makes the next discovery request read the offers again, as
// it would a minute later.
func expireOffers(s *Server) {
	s.offerCache.mu.Lock()
	defer s.offerCache.mu.Unlock()
	s.offerCache.at = time.Time{}
}

func mcpInitializeInstructions(t *testing.T, s *Server) string {
	t.Helper()
	server := httptest.NewServer(s)
	defer server.Close()
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var envelope struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Result.Instructions
}
