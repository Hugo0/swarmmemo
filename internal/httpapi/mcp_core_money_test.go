package httpapi

// The core profile moves no money (mcp_core.go, board/moneyfree.go): the
// payment tools are out, the money arguments are out of its schemas and
// refused when sent anyway, the board refuses money commands made on it,
// the ways to raise standing that are paid point to /mcp, and no
// description asks a model to pay. /mcp is unchanged.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

// toolSchemas is a profile's tools/list answer by name, schemas kept.
func toolSchemas(t *testing.T, s *Server, path string) map[string]map[string]any {
	t.Helper()
	out := mcpRequest(t, s, path, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := map[string]map[string]any{}
	for _, raw := range dig(out, "result", "tools").([]any) {
		tool := raw.(map[string]any)
		tools[tool["name"].(string)] = tool
	}
	return tools
}

// schemaProperty is a tool's input schema property, nil when it has none.
func schemaProperty(tool map[string]any, name string) map[string]any {
	p, _ := dig(tool, "inputSchema", "properties", name).(map[string]any)
	return p
}

func TestCoreProfileMoneyToolsAndSchemas(t *testing.T) {
	s := pinServer(t)
	core, full := toolSchemas(t, s, mcpProfileCore), toolSchemas(t, s, "/mcp")
	for _, left := range coreLeftOutTools {
		if core[left.Name] != nil {
			t.Errorf("the core profile lists %s", left.Name)
		}
		if left.Why == "" {
			t.Errorf("%s is left out without a reason", left.Name)
		}
		// Each is a tool /mcp lists (top-ups are off in this server).
		if full[left.Name] == nil && left.Name != creditsTopupTool.Name {
			t.Errorf("%s is left out of the core profile but /mcp does not list it either: stale", left.Name)
		}
	}
	for _, n := range coreNarrowed {
		c, f := core[n.tool], full[n.tool]
		if c == nil || f == nil {
			t.Fatalf("%s: core %v, /mcp %v", n.tool, c != nil, f != nil)
		}
		for _, arg := range n.args {
			if schemaProperty(c, arg) != nil {
				t.Errorf("the core profile's %s takes %s", n.tool, arg)
			}
			if schemaProperty(f, arg) == nil {
				t.Errorf("/mcp's %s has no %s: the narrowing is stale", n.tool, arg)
			}
		}
		for arg, desc := range n.restate {
			if got := dig(schemaProperty(c, arg), "description"); got != desc {
				t.Errorf("the core profile's %s %s: %v, want %q", n.tool, arg, got, desc)
			}
			if dig(schemaProperty(f, arg), "description") == desc {
				t.Errorf("/mcp's %s %s already reads %q: nothing to restate", n.tool, arg, desc)
			}
		}
		if req, _ := dig(c, "inputSchema", "required").([]any); slices.ContainsFunc(req, func(r any) bool { return slices.Contains(n.args, fmt.Sprint(r)) }) {
			t.Errorf("the core profile's %s requires a left-out argument: %v", n.tool, req)
		}
	}
	// The paid choices are gone from what the arguments say.
	for _, c := range []struct{ tool, arg, word string }{{"manage_work", "action", "settle"}, {"manage_work", "action", "promise"}, {"raise_standing", "action", "refund"}, {"create_work", "tiers", "usdc"}} {
		if strings.Contains(fmt.Sprint(dig(schemaProperty(core[c.tool], c.arg), "description")), c.word) {
			t.Errorf("the core profile's %s %s still offers %s", c.tool, c.arg, c.word)
		}
		if !strings.Contains(fmt.Sprint(dig(schemaProperty(full[c.tool], c.arg), "description")), c.word) {
			t.Errorf("/mcp's %s %s no longer offers %s", c.tool, c.arg, c.word)
		}
	}
	// manage_work's description names no action the board refuses here,
	// and every action it names (and its action argument lists) is one the
	// core profile takes: all from manageWorkActions (C721).
	desc := core["manage_work"]["description"].(string)
	for _, a := range manageWorkActions {
		money := board.MoneyOperation(a.operation) != ""
		if named := strings.Contains(desc, a.name+" ("); named == money {
			t.Errorf("the core profile's manage_work description names %s %v, its command moves money %v: %q", a.name, named, money, desc)
		}
		if !strings.Contains(full["manage_work"]["description"].(string), a.name+" (") {
			t.Errorf("/mcp's manage_work description no longer names %s", a.name)
		}
		for _, arg := range a.args {
			if money && schemaProperty(core["manage_work"], arg) != nil && !slices.ContainsFunc(coreManageWorkActions(), func(k manageWorkAction) bool { return slices.Contains(k.args, arg) }) {
				t.Errorf("the core profile's manage_work takes %s, which only %s reads", arg, a.name)
			}
		}
	}
	if !strings.Contains(desc, "reveal (") || schemaProperty(core["manage_work"], "answer") == nil {
		t.Errorf("the core profile's manage_work lost reveal, which moves no money: %q", desc)
	}
	// Every action a core argument lists is one the profile takes: no
	// narrowing refuses it, nor the board as money.
	for _, n := range coreNarrowed {
		listed, ok := n.restate["action"]
		if !ok {
			continue
		}
		for _, action := range regexp.MustCompile(`, | or `).Split(listed, -1) {
			action = strings.TrimSuffix(action, " (the default)")
			if what := n.refuse(map[string]any{"action": action}); what != "" {
				t.Errorf("the core profile's %s lists action %s but refuses it: %s", n.tool, action, what)
			}
			if n.tool == "manage_work" {
				i := slices.IndexFunc(manageWorkActions, func(a manageWorkAction) bool { return a.name == action })
				if i < 0 || board.MoneyOperation(manageWorkActions[i].operation) != "" {
					t.Errorf("the core profile's manage_work lists action %q, which it does not take", action)
				}
			}
		}
	}

	// A second list is the same: the narrowing never changes the schemas
	// the server registered (they are shared by every request).
	again := toolSchemas(t, s, mcpProfileCore)
	for _, n := range coreNarrowed {
		a, _ := json.Marshal(core[n.tool])
		b, _ := json.Marshal(again[n.tool])
		if string(a) != string(b) {
			t.Errorf("%s lists differently the second time:\n %s\n %s", n.tool, a, b)
		}
	}
	if again := toolSchemas(t, s, "/mcp"); schemaProperty(again["raise_standing"], "usdc") == nil || schemaProperty(again["manage_work"], "tx_hash") == nil {
		t.Errorf("/mcp lost a money argument after a core list")
	}

	// /capabilities says the same, from the same tables.
	caps := s.coreCapabilities()
	if caps["moves_money"] != false || caps["payment_tools"] != false || len(caps["excluded_tools"].([]map[string]string)) != len(coreLeftOutTools) ||
		len(caps["narrowed_tools"].([]map[string]any)) != len(coreNarrowed) || slices.Contains(caps["tools"].([]string), "buy_offering") {
		t.Fatalf("/capabilities core_profile: %v", caps)
	}
}

// coreMoneyWordRE are the words of paying: no core description or argument
// says one except in coreMoneyMentions; usdc inside a name (amount_usdc)
// counts.
var coreMoneyWordRE = regexp.MustCompile(`(?i)\b(pay|pays|paying|payment|payments|payout|payouts|\w*usdc\w*|x402|mpp|wallets?|settle|settles|settled|settlement|tx_hash|top[- ]?ups?|buy|buying|bought|bonds?|crypto|purchase)\b`)

// coreMoneyMentions are the legitimate mentions: what a thing is or where
// it is, never a direction to pay. Each must still occur (no stale entry).
var coreMoneyMentions = []string{
	"Final; pays any reward to the worker.",                   // accept_work: a credit reward here
	"a recurring one pays for all its firings when set",       // wakeup_schedule: credits
	"its writer pays what it cost",                            // docs_open screen: credits
	"its author pays what it cost",                            // docs_read screen: credits
	"x402 or mpp",                                             // find protocol: a search filter
	"agent,human,wallet,key",                                  // find door: a search filter
	"tasks agents do for a listed USDC price, bought on /mcp", // find_offerings: where buying is
	"price, wallet, input schema",                             // read_offering: what a listing shows
	"A bond, paid in USDC, is on /mcp.",                       // raise_standing: where a bond is
	"such as wallet, github",                                  // raise_standing kind: a wallet link proves an address, nothing is paid
	"a domain, wallet, GitHub",                                // standing_ways: the same links
	"a bond, proof of work",                                   // standing_ways: a way it lists
	"reward_usdc",                                             // read_work: a field it shows
	"No wallet or account required.",                          // post_message
}

func TestCoreDescriptionsNeverAskToPay(t *testing.T) {
	s := pinServer(t)
	used := map[string]bool{}
	check := func(where, text string) {
		t.Helper()
		for _, ok := range coreMoneyMentions {
			if strings.Contains(text, ok) {
				used[ok] = true
				text = strings.ReplaceAll(text, ok, "")
			}
		}
		if m := coreMoneyWordRE.FindString(text); m != "" {
			t.Errorf("%s speaks of paying (%q): %q", where, m, text)
		}
	}
	for name, tool := range toolSchemas(t, s, mcpProfileCore) {
		check(name, tool["description"].(string))
		props, _ := dig(tool, "inputSchema", "properties").(map[string]any)
		for arg, p := range props {
			if coreMoneyWordRE.MatchString(arg) {
				t.Errorf("%s takes %s", name, arg)
			}
			check(name+" "+arg, fmt.Sprint(dig(p, "description")))
		}
	}
	for _, ok := range coreMoneyMentions {
		if !used[ok] {
			t.Errorf("the mention %q occurs in no core description: stale", ok)
		}
	}
	// The instructions say where paying is, once, and nothing else of it.
	initialized := mcpRequest(t, s, mcpProfileCore, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	instructions := fmt.Sprint(dig(initialized, "result", "instructions"))
	if strings.Count(instructions, coreElsewhere) != 1 || !strings.Contains(instructions, "no money or crypto moves") {
		t.Fatalf("core instructions: %q", instructions)
	}
	if m := coreMoneyWordRE.FindString(strings.NewReplacer(coreElsewhere, "", "no money or crypto moves", "").Replace(instructions)); m != "" {
		t.Errorf("core instructions speak of paying (%q): %q", m, instructions)
	}
}

// moneyServer is a hosted store with USDC work rewards (a chain reader that
// is never called), the ledger and trust on.
func moneyServer(t *testing.T) (*board.Store, *Server) {
	t.Helper()
	chain, err := services.NewUSDCChain("https://rpc.example.org/base")
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kek := filepath.Join(t.TempDir(), "hosted-kek")
	if err := os.WriteFile(kek, []byte(base64.RawURLEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := board.Features{Ledger: board.LedgerOn, Trust: board.TrustOn}
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", HostedKEKFile: kek, WorkUSDC: chain, Features: f})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, New(store, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", Features: f})
}

// refusedAsMoney checks a refusal is the money-free one, naming what.
func refusedAsMoney(t *testing.T, label, failure, what string) {
	t.Helper()
	if !strings.HasPrefix(failure, "400 unsupported_operation: "+what) || !strings.Contains(failure, "/mcp/core never does") || !strings.Contains(failure, board.MoneyElsewhere) {
		t.Errorf("%s on the core profile: %q, want the money refusal of %q", label, failure, what)
	}
}

func TestCoreProfileRefusesMoney(t *testing.T) {
	_, s := moneyServer(t)
	requester := newIdentity(t, s, "core-requester")
	asRequester := "Bearer " + requester["token"].(string)
	worker := newIdentity(t, s, "core-worker")
	asWorker := "Bearer " + worker["token"].(string)
	post := func(as, text string) string {
		t.Helper()
		return mustTool(t, s, "/mcp", as, "post_message", map[string]any{"room": "gigs", "kind": "request", "text": text})["receipt"].(map[string]any)["id"].(string)
	}
	credits := post(asRequester, "Summarise a thread.")
	usdc := post(asRequester, "Translate a paragraph.")
	work := func(id string, extra map[string]any) map[string]any {
		args := map[string]any{"message_id": id, "title": "A task", "capabilities": []any{"writing"}}
		for k, v := range extra {
			args[k] = v
		}
		return args
	}

	// Money arguments and values, refused before the tool runs, whatever
	// the schema said.
	for _, c := range []struct {
		tool string
		args map[string]any
		what string
	}{
		{"raise_standing", map[string]any{"action": "challenge", "kind": "bond", "usdc": "2.00"}, "The usdc argument of raise_standing"},
		{"raise_standing", map[string]any{"action": "challenge", "kind": "bond", "payment": "e30"}, "The payment argument of raise_standing"},
		{"raise_standing", map[string]any{"action": "challenge", "kind": "bond"}, "A standing bond"},
		{"raise_standing", map[string]any{"action": "refund"}, "A bond refund"},
		{"create_work", work(usdc, map[string]any{"reward": map[string]any{"credits": 1, "usdc": "0.10"}}), "A USDC reward"},
		{"create_work", work(usdc, map[string]any{"reward": map[string]any{"usdc": "0.10"}}), "A USDC reward"},
		{"create_work", work(usdc, map[string]any{"seats": 2, "tiers": []any{map[string]any{"name": "ok", "credits": 1, "usdc": "0.10"}}}), "A USDC tier reward"},
		{"create_work", work(usdc, map[string]any{"seats": 2, "reward": 1, "usdc_day_max": "1.00"}), "The usdc_day_max argument of create_work"},
		{"create_work", work(usdc, map[string]any{"seats": 2, "reward": 1, "usdc_per_agent_day": "1.00"}), "The usdc_per_agent_day argument of create_work"},
		{"manage_work", map[string]any{"message_id": usdc, "action": "settle"}, board.MoneyOperation(board.WorkSettle)},
		{"manage_work", map[string]any{"message_id": usdc, "action": "promise"}, board.MoneyOperation(board.WorkPromise)},
		{"manage_work", map[string]any{"message_id": usdc, "action": "promise", "amount_usdc": "0.10"}, "The amount_usdc argument of manage_work"},
		{"manage_work", map[string]any{"message_id": usdc, "action": "settle", "tx_hash": "0x" + strings.Repeat("c", 64)}, "The tx_hash argument of manage_work"},
		{"claim_work", map[string]any{"message_id": usdc, "payout_address": "0x00000000000000000000000000000000000000aa"}, "The payout_address argument of claim_work"},
		{"submit_work", map[string]any{"message_id": usdc, "result_id": usdc, "payout_address": "0x00000000000000000000000000000000000000aa"}, "The payout_address argument of submit_work"},
	} {
		_, failure := callTool(t, s, mcpProfileCore, asRequester, c.tool, c.args)
		refusedAsMoney(t, c.tool+" "+fmt.Sprint(c.args), failure, c.what)
	}
	// A null is an argument left out (C125), not a payment.
	if ways := mustTool(t, s, mcpProfileCore, asWorker, "raise_standing", map[string]any{"action": "ways", "usdc": nil, "payment": nil}); dig(ways, "data", "ways") == nil {
		t.Fatalf("raise_standing ways with null money arguments: %v", ways)
	}

	// Credits are not money: credit work runs on the core profile as on /mcp.
	if a := mustTool(t, s, mcpProfileCore, asRequester, "create_work", work(credits, map[string]any{"ttl": 86400})); dig(a, "data", "ack", "state") != "open" {
		t.Fatalf("create_work on the core profile: %v", a)
	}
	// Every action the core manage_work names runs there: the work may
	// refuse it (wrong state), never as money.
	for _, a := range coreManageWorkActions() {
		if _, failure := callTool(t, s, mcpProfileCore, asRequester, "manage_work", map[string]any{"message_id": credits, "action": a.name}); strings.Contains(failure, "unsupported_operation") {
			t.Errorf("manage_work %s on the core profile is refused as money: %q", a.name, failure)
		}
	}

	// /mcp is unchanged: the same USDC reward is created there.
	if a := mustTool(t, s, "/mcp", asRequester, "create_work", work(usdc, map[string]any{"reward": map[string]any{"usdc": "0.25"}})); dig(a, "data", "ack", "state") != "open" {
		t.Fatalf("a USDC reward on /mcp: %v", a)
	}
	_, failure := callTool(t, s, "/mcp", asRequester, "raise_standing", map[string]any{"action": "challenge", "kind": "bond", "usdc": "2.00"})
	if strings.Contains(failure, "unsupported_operation") {
		t.Fatalf("a bond on /mcp got the core refusal: %q", failure)
	}
	// The board refuses what the arguments cannot show: this task's reward
	// is USDC, so the core profile neither claims nor accepts it.
	result := mustTool(t, s, "/mcp", asWorker, "post_message", map[string]any{"reply_to": usdc, "text": "Traduit."})["receipt"].(map[string]any)["id"].(string)
	_, failure = callTool(t, s, mcpProfileCore, asWorker, "claim_work", map[string]any{"message_id": usdc, "result_id": result})
	refusedAsMoney(t, "claim_work of USDC work", failure, "This task's USDC reward")
	payee := "0x00000000000000000000000000000000000000aa"
	mustTool(t, s, "/mcp", asWorker, "claim_work", map[string]any{"message_id": usdc, "result_id": result, "payout_address": payee})
	_, failure = callTool(t, s, mcpProfileCore, asRequester, "accept_work", map[string]any{"message_id": usdc, "result_id": result})
	refusedAsMoney(t, "accept_work of USDC work", failure, "Accepting a task with a USDC reward")
	accepted := mustTool(t, s, "/mcp", asRequester, "accept_work", map[string]any{"message_id": usdc, "result_id": result})
	if accepted["ok"] != true {
		t.Fatalf("accept_work on /mcp: %v", accepted)
	}
	read := mustTool(t, s, mcpProfileCore, "", "read_work", map[string]any{"message_id": usdc})
	if dig(read, "data", "work", "reward_usdc", "state") != "payable" {
		t.Fatalf("read_work on the core profile (it reads, as before): %v", dig(read, "data", "work", "reward_usdc"))
	}
}

// The board's own refusal: every money operation made on a money-free
// channel, whatever built it, signed or not.
func TestMoneyFreeChannelRefusedByBoard(t *testing.T) {
	store, _ := moneyServer(t)
	ctx := board.WithoutMoney(t.Context(), mcpProfileCore)
	for _, c := range []board.Command{
		{Operation: "credits.topup", Data: `{"schema":1}`},
		{Operation: "offering.buy", Target: "pythia/oracle", Data: `{"schema":1,"input":{}}`},
		{Operation: "offering.claim", Target: "oc_x"},
		{Operation: "offering.refund", Target: "oc_x"},
		{Operation: "x402.submit", Target: "GET https://example.com/"},
		{Operation: board.WorkSettle, MessageID: "x"},
		{Operation: "standing.challenge", Data: `{"schema":1,"kind":"bond","usdc":"2.00"}`},
		{Operation: "standing.challenge", Data: `{"schema":1,"kind":"bond","action":"refund"}`},
	} {
		_, err := store.Execute(ctx, c, "core-test")
		var be *board.Error
		if !errors.As(err, &be) || be.Code != "unsupported_operation" || !strings.Contains(be.Message, "/mcp/core never does") {
			t.Errorf("%s %s on a money-free channel: %v", c.Operation, c.Data, err)
		}
		// Elsewhere the same command reaches its own checks.
		if _, err := store.Execute(t.Context(), c, "core-test"); errors.As(err, &be) && be.Code == "unsupported_operation" && strings.Contains(be.Message, "never does") {
			t.Errorf("%s off the money-free channel got its refusal", c.Operation)
		}
	}
	// What moves no money is untouched.
	if _, err := store.Execute(ctx, board.Command{Operation: "standing.challenge", Data: `{"schema":1,"kind":"pow"}`}, "core-test"); err != nil && strings.Contains(err.Error(), "never does") {
		t.Fatalf("a pow challenge on a money-free channel: %v", err)
	}
}

// The paid ways to raise standing stay listed with where they are taken,
// and the climb routes around them: no route, summary or next step on the
// core profile sends an agent to pay. /mcp keeps them.
func TestCoreStandingWaysPointElsewhere(t *testing.T) {
	_, s := moneyServer(t)
	me := newIdentity(t, s, "core-climber")
	as := "Bearer " + me["token"].(string)
	agent := dig(mustTool(t, s, "/mcp", as, "whoami", map[string]any{}), "agent", "id")
	mustTool(t, s, "/mcp", as, "post_message", map[string]any{"text": "Hello from a climber."}) // a public agent now
	ways := func(path string, res map[string]any) map[string]map[string]any {
		t.Helper()
		out := map[string]map[string]any{}
		list, _ := dig(res, "data", "ways").([]any)
		for _, w := range list {
			out[w.(map[string]any)["kind"].(string)] = w.(map[string]any)
		}
		if len(out) == 0 {
			t.Fatalf("%s lists no ways: %v", path, res)
		}
		return out
	}
	paidStep := func(steps any) bool {
		list, _ := steps.([]any)
		return slices.ContainsFunc(list, func(st any) bool {
			k := dig(st, "kind")
			cost, priced := dig(st, "cost_cents").(float64) // route steps; next_steps are ways
			return k == "money" || k == "bond" || priced && cost != 0
		})
	}
	for _, core := range []map[string]any{
		mustTool(t, s, mcpProfileCore, "", "standing_ways", map[string]any{"agent": agent}),
		mustTool(t, s, mcpProfileCore, as, "raise_standing", map[string]any{}),
	} {
		w := ways(mcpProfileCore, core)
		for _, kind := range []string{"money", "bond"} {
			if w[kind] == nil || w[kind]["action"] != board.MoneyWayAction {
				t.Errorf("the core profile's %s way: %v", kind, w[kind])
			}
		}
		if w["pow"] == nil || w["pow"]["action"] == board.MoneyWayAction {
			t.Errorf("the core profile's pow way: %v", w["pow"])
		}
		if dig(core, "data", "climb", "pay_cents") != float64(0) || paidStep(dig(core, "data", "climb", "route")) {
			t.Errorf("the core profile's climb pays: %v", dig(core, "data", "climb"))
		}
	}
	trust := mustTool(t, s, mcpProfileCore, "", "trust", map[string]any{"agent": agent})
	if summary := fmt.Sprint(dig(trust, "data", "summary")); coreMoneyWordRE.MatchString(summary) || paidStep(dig(trust, "data", "next_steps")) || dig(trust, "data", "climb", "pay_cents") != float64(0) {
		t.Errorf("trust on the core profile: summary %q, next_steps %v, climb %v", summary, dig(trust, "data", "next_steps"), dig(trust, "data", "climb"))
	}
	full := mustTool(t, s, "/mcp", "", "standing_ways", map[string]any{"agent": agent})
	if w := ways("/mcp", full); w["money"]["action"] == board.MoneyWayAction || dig(full, "data", "climb", "pay_cents") == float64(0) {
		t.Errorf("/mcp's ways changed: money %v, climb %v", w["money"], dig(full, "data", "climb"))
	}
	if !strings.Contains(fmt.Sprint(dig(mustTool(t, s, "/mcp", "", "trust", map[string]any{"agent": agent}), "data", "summary")), "pay") {
		t.Errorf("/mcp's trust summary no longer offers paying")
	}
}

// The core profile never leads to work it cannot claim: home's open_task,
// the tips on its answers and find_work leave out any task with a USDC
// reward (find_work's data.filters says so); read_work still reads it, and
// /mcp still offers it.
func TestCoreLeadsToNoUSDCWork(t *testing.T) {
	store, s := moneyServer(t)
	// The requester signs with a key of its own: a hosted identity cannot
	// hold credits in escrow until it is claimed.
	requester := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	exec := func(c board.Command) board.Result {
		t.Helper()
		res, err := store.Execute(t.Context(), signService(requester, c), "core-test")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		return res
	}
	exec(board.Command{Operation: "agent.register"})
	poster := fingerprintOf(requester)
	if err := store.GrantAllowance(t.Context(), poster, allowance.Credit, allowance.Paid, 5000, "core test"); err != nil {
		t.Fatal(err)
	}
	worker := newIdentity(t, s, "core-seeker")
	asWorker := "Bearer " + worker["token"].(string)
	mustTool(t, s, "/mcp", asWorker, "post_message", map[string]any{"text": "Looking for work."})
	request := func(text string) string {
		return exec(board.Command{Operation: "post", Room: "gigs", Kind: "request", Text: text}).Receipt.ID
	}
	create := func(id string, reward any) {
		t.Helper()
		feed, err := store.Execute(t.Context(), board.Command{Operation: "messages.list"}, "core-test")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(map[string]any{"schema": 1, "generation": feed.Generation, "title": "Task " + id[:6], "capabilities": []string{"writing"}, "reward": reward})
		exec(board.Command{Operation: "work.create", MessageID: id, Data: string(data)})
	}
	openTask := func(path string) string {
		t.Helper()
		items, _ := dig(mustTool(t, s, path, asWorker, "home", map[string]any{}), "data", "items").([]any)
		for _, it := range items {
			if dig(it, "kind") == "open_task" {
				return fmt.Sprint(dig(it, "tool", "arguments", "message_id"))
			}
		}
		return ""
	}
	listed := func(path string) ([]string, any) {
		t.Helper()
		res := mustTool(t, s, path, asWorker, "find_work", map[string]any{"room": "gigs"})
		var ids []string
		for _, w := range dig(res, "data", "works").([]any) {
			ids = append(ids, fmt.Sprint(dig(w, "id")))
		}
		return ids, dig(res, "data", "filters")
	}

	usdc := request("Translate a paragraph.")
	create(usdc, map[string]any{"usdc": "0.25"})
	if got := openTask("/mcp"); got != usdc {
		t.Fatalf("/mcp home open_task: %q, want %s", got, usdc)
	}
	if got := openTask(mcpProfileCore); got != "" {
		t.Fatalf("core home names %q with only USDC work open", got)
	}
	credits := request("Summarise a thread.")
	create(credits, 20)
	if got := openTask(mcpProfileCore); got != credits {
		t.Fatalf("core home open_task: %q, want the credit task %s", got, credits)
	}
	// No answer on the core profile (its tips included) names the USDC task.
	for i := range 3 {
		raw, _ := json.Marshal(mustTool(t, s, mcpProfileCore, asWorker, "post_message", map[string]any{"text": fmt.Sprintf("Still looking %d.", i)}))
		if strings.Contains(string(raw), usdc) {
			t.Fatalf("a core answer names the USDC task: %s", raw)
		}
	}

	core, filters := listed(mcpProfileCore)
	if slices.Contains(core, usdc) || !slices.Contains(core, credits) || dig(filters, "money_free") != board.MoneyFreeWorkFilter {
		t.Fatalf("core find_work: %v, filters %v", core, filters)
	}
	full, filters := listed("/mcp")
	if !slices.Contains(full, usdc) || !slices.Contains(full, credits) || filters != nil {
		t.Fatalf("/mcp find_work: %v, filters %v", full, filters)
	}
	if read := mustTool(t, s, mcpProfileCore, "", "read_work", map[string]any{"message_id": usdc}); dig(read, "data", "work", "reward_usdc", "amount") != "0.25" {
		t.Fatalf("core read_work of the USDC task: %v", dig(read, "data", "work"))
	}
}
