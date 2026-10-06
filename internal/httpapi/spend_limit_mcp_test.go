package httpapi

// Spend limits on hosted tokens through hosted MCP: a token handed to
// another app with a limit is refused over it on x402_tools_call, while the
// identity's other token is not; a limited token cannot raise its own limit
// or mint an unlimited token, and sees its own budget in manage_tokens list.
// Real store, real ledger, a fake paid-tools API.

import (
	"strings"
	"testing"
)

func TestHostedTokenSpendLimit(t *testing.T) {
	s, api := hostedServicesServer(t)
	me := newIdentity(t, s, "")
	owner := "/mcp/t/" + me["token"].(string)
	made := mustTool(t, s, owner, "", "manage_tokens", map[string]any{"action": "create", "label": "sub-agent", "credit_per_call": 10000})
	limit, _ := dig(made, "data", "spend_limit").(map[string]any)
	if limit == nil || limit["credit_per_call"] != float64(10000) || limit["credit_per_day"] != nil {
		t.Fatalf("create with a limit: %v", made)
	}
	sub, subID := "/mcp/t/"+dig(made, "data", "token").(string), dig(made, "data", "token_id").(string)
	mustTool(t, s, owner, "", "x402_tools_search", map[string]any{"query": "weather"})
	call := map[string]any{"resource": "tool:" + paidToolOK, "body": map[string]any{"city": "Paris"}, "max_cost": 30000}

	// The per-call ceiling refuses a high max_cost, nothing paid or charged.
	if _, failure := callTool(t, s, sub, "", "x402_tools_call", call); !strings.Contains(failure, "429 spend_limit") || !strings.Contains(failure, "credit_per_call 10000") {
		t.Fatalf("over the per-call limit: %q", failure)
	}
	if api.count() != 0 {
		t.Fatal("a refused call reached the paid API")
	}
	// The owner's token is not limited.
	got := mustTool(t, s, owner, "", "x402_tools_call", call)
	cost, _ := dig(got, "data", "call", "cost").(float64)
	if cost <= 0 || api.count() != 1 {
		t.Fatalf("owner call: %v", got)
	}

	// A limited token cannot raise its own limit, mint a token or revoke one.
	for _, args := range []map[string]any{
		{"action": "limit", "target": subID, "credit_per_call": 100000},
		{"action": "limit", "target": subID},
		{"action": "create", "label": "escape"},
		{"action": "revoke", "target": "all"},
	} {
		if _, failure := callTool(t, s, sub, "", "manage_tokens", args); !strings.Contains(failure, "403 credential_limited") {
			t.Fatalf("limited token %v: %q", args, failure)
		}
	}

	// The owner sets a daily limit too small for the call: refused with
	// retry_after at midnight, naming the daily limit.
	set := mustTool(t, s, owner, "", "manage_tokens", map[string]any{"action": "limit", "target": subID, "credit_per_day": 1, "credit_per_call": 100000})
	if dig(set, "data", "kind") != "hosted_token" || dig(set, "data", "spend_limit", "credit_per_day") != float64(1) {
		t.Fatalf("limit: %v", set)
	}
	if _, failure := callTool(t, s, sub, "", "x402_tools_call", call); !strings.Contains(failure, "429 spend_limit") || !strings.Contains(failure, "credit_per_day 1)") {
		t.Fatalf("over the daily limit: %q", failure)
	}
	// Raised by the owner, the call goes through and counts against it.
	mustTool(t, s, owner, "", "manage_tokens", map[string]any{"action": "limit", "target": subID, "credit_per_day": 50000})
	got = mustTool(t, s, sub, "", "x402_tools_call", call)
	if dig(got, "data", "call", "state") != "done" || api.count() != 2 {
		t.Fatalf("within the raised limit: %v", got)
	}
	spent, _ := dig(got, "data", "call", "cost").(float64)

	// The limited token reads its own budget, marked current; the owner sees
	// both tokens' spend today.
	list := mustTool(t, s, sub, "", "manage_tokens", map[string]any{"action": "list"})
	tokens, _ := dig(list, "data", "tokens").([]any)
	var mine, owners map[string]any
	for _, x := range tokens {
		tok := x.(map[string]any)
		if tok["token_id"] == subID {
			mine = tok
		} else {
			owners = tok
		}
	}
	if mine == nil || mine["current"] != true || dig(mine, "spend_limit", "credit_per_day") != float64(50000) ||
		dig(mine, "spend_limit", "credit_spent_today") != spent || dig(mine, "spend_limit", "credit_remaining_today") != 50000-spent {
		t.Fatalf("own budget: %v", mine)
	}
	if owners == nil || owners["current"] != nil || dig(owners, "spend_limit", "credit_per_day") != nil || dig(owners, "spend_limit", "credit_spent_today") != cost {
		t.Fatalf("the owner's token: %v", owners)
	}
	// Another identity cannot limit this one's token: it is not found.
	other := newIdentity(t, s, "")
	if _, failure := callTool(t, s, "/mcp/t/"+other["token"].(string), "", "manage_tokens", map[string]any{"action": "limit", "target": subID, "credit_per_day": 1}); !strings.Contains(failure, "404 not_found") {
		t.Fatalf("another identity's token: %q", failure)
	}
}
