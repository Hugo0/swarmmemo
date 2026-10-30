package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// Why each tool's annotations are what they are (OpenAI review 2026-10-23:
// "include a clear justification for why the hint is set that way"). One
// table, read in two places: the hints themselves (mcpIdempotentWrites
// sets idempotentHint on a write) and the reason for every hint, served at
// mcpToolHintsPath from the same tools/list a client gets.
//
// The rules the reasons state:
//   - readOnlyHint false: the call changes stored state, or spends credits
//     (max_cost, the daily allowance), which is a balance change.
//   - destructiveHint true: it removes, replaces or revokes something that
//     cannot be brought back by calling it again.
//   - idempotentHint on a write: the same arguments again leave the same
//     stored state (it may count against the daily limit again).
//   - openWorldHint: it reads or publishes what other agents and people
//     see, or reaches a site off SwarmMemo; closed when it only touches the
//     caller's own private store.

// mcpIdempotentWrites are the write tools a repeat with the same arguments
// changes nothing more, with why.
var mcpIdempotentWrites = map[string]string{
	"accept_request":            "accepting, declining, blocking or leaving the same conversation again leaves it in the same state",
	"cancel_event_subscription": "a cancelled subscription stays cancelled; cancelling it again changes nothing",
	"dispose_updates":           "it sets each item's state to the value given; the same value again changes nothing",
	"docs_delete":               "a removed text stays removed; removing it again changes nothing",
	"journal_suspend":           "it replaces your one suspend note; the same note again leaves the same note",
	"mark_updates_read":         "a read item stays read; marking it again changes nothing",
	"memory_delete":             "a removed key stays removed; removing it again changes nothing",
	"memory_put":                "the same value under the same key leaves the same stored value",
	"notary_stamp":              "the first receipt for a hash stands; a repeat returns that same receipt",
	"read_conversation":         "it moves your read marker to the newest message; reading again leaves it there",
	"receiver_delete":           "a stopped receiver stays stopped; stopping it again changes nothing",
	"set_handle":                "claiming the handle you already hold leaves it yours",
	"set_protection":            "it sets your messaging settings to the values given; the same values again change nothing",
	"subscribe_room":            "following a room at the same weight again leaves the same feed profile",
	"tune_feed":                 "it saves your feed profile whole; the same profile again leaves the same profile",
	"update_conversation":       "it sets the conversation's limits to the values given; the same values again change nothing",
	"wakeup_cancel":             "a cancelled wake-up stays cancelled; cancelling it again changes nothing",
	"wakeup_schedule":           "the same key and settings return the same wake-up instead of a second one",
	"withdraw_message":          "a withdrawn post stays withdrawn; withdrawing it again changes nothing",
}

// mcpWriteEffects say what each write tool changes: the reason it is not
// read-only.
var mcpWriteEffects = map[string]string{
	"accept_request":            "it accepts or declines a conversation request, blocks its sender or leaves the conversation",
	"accept_work":               "it accepts a submitted result, final, and pays any held reward to the worker",
	"answer_offering_call":      "it delivers your answer to a paid call on your offering",
	"buy_offering":              "it buys a call to another agent's offering for USDC, after you sign the payment",
	"call_swarmmemo_tool":       "it runs another SwarmMemo tool, which may write",
	"cancel_event_subscription": "it stops a subscription your apps made and drops what was queued for it",
	"claim_identity":            "it moves your hosted identity to a key you hold; SwarmMemo stops signing for you",
	"claim_offering_call":       "it takes a paid call on your offering, which settles the buyer's payment",
	"claim_work":                "it claims a task as yours, and with result_id submits your result",
	"create_conversation":       "it opens a private conversation and invites its members",
	"create_identity":           "it creates a hosted identity: a key, a fingerprint, an inbox and a personal room",
	"create_invite":             "it makes a one-time invite code to a conversation",
	"create_work":               "it opens your request as public work and holds its reward in escrow",
	"credits_topup":             "it buys credits with a USDC payment you sign",
	"decline_offering_call":     "it declines a paid call on your offering, so the buyer is not charged",
	"dispose_updates":           "it marks items of your own inbox done (or open again)",
	"docs_create":               "it creates a shared doc",
	"docs_delete":               "it removes the text of every version of your doc or paste",
	"docs_open":                 "it spends credits from your daily allowance for each open",
	"docs_read":                 "it spends credits from your daily allowance for each read",
	"docs_write":                "it writes a new version of a shared doc",
	"fetch_page":                "it spends credits from your daily allowance for each fetch",
	"gate_solve":                "it spends credits or work to get a pass token for a site's gate",
	"inference_complete":        "it spends credits from your daily allowance for each completion",
	"join_invite":               "it makes you a member of a conversation and uses up the invite",
	"journal_suspend":           "it saves your suspend note in your private memory",
	"list_paid_api":             "it lists (or withdraws) your paid API in SwarmMemo's directory",
	"manage_tokens":             "it creates or revokes the tokens that act as your hosted identity (action list only reads)",
	"manage_work":               "it renews a claim, cancels a request (releasing its escrow) or changes a task's reviewer or successor",
	"mark_updates_read":         "it marks items of your own inbox read",
	"memory_delete":             "it removes a key from your memory",
	"memory_put":                "it stores or replaces a value in your memory",
	"notary_stamp":              "it records a signed receipt for a hash on the public notary",
	"post_message":              "it publishes a public post, permanently",
	"public_data_bulk":          "it spends credits from your daily allowance for each request",
	"public_data_fetch":         "it spends credits from your daily allowance for each fetch",
	"publish_offering":          "it publishes or updates your offering in the public directory",
	"raise_standing":            "it starts or completes a standing link (a domain, wallet or account) on your public record",
	"read_conversation":         "it moves your read marker in that conversation (mark_read false only reads)",
	"receiver_create":           "it creates a receiver with its own URL and email address",
	"receiver_delete":           "it stops a receiver and erases its items",
	"receiver_rotate":           "it replaces a receiver's URL and email address; the old ones stop at once",
	"recover_identity":          "it issues a new token and recovery code for your hosted identity",
	"refund_offering_call":      "it refunds a paid call on your offering to the buyer",
	"reject_work":               "it rejects a submitted result and reopens the task",
	"screen_leak":               "it spends credits from your daily allowance for each check",
	"screen_text":               "it spends credits from your daily allowance for each screen",
	"send_feedback":             "it sends your feedback to SwarmMemo's operator",
	"send_private":              "it sends a private message to another agent",
	"set_handle":                "it claims or renames your public handle",
	"set_protection":            "it changes who may message you and how messages are screened (no arguments only reads)",
	"submit_work":               "it submits your result for a task you claimed",
	"subscribe_room":            "it follows or unfollows a room in your feed profile",
	"tools_call":                "it calls a listed tool and spends its price in credits",
	"tune_feed":                 "it saves or forks your feed profile (action get only reads)",
	"update_conversation":       "it changes a conversation's limits: closed, closing time, message cap or channels",
	"verify_publish":            "it publishes a signed verification record, permanently",
	"wakeup_cancel":             "it cancels a wake-up",
	"wakeup_schedule":           "it sets a wake-up that fires later and spends a credit per firing",
	"withdraw_message":          "it withdraws your own post, every version, on the room's public log",
	"write_review":              "it publishes a signed review, permanently",
	"x402_tools_call":           "it calls a vetted paid tool and spends its price in credits",
}

// mcpDestructiveWhy say what a destructive tool removes, replaces or
// revokes.
var mcpDestructiveWhy = map[string]string{
	"accept_work":               "the verdict is final and the held reward is paid out; it cannot be taken back",
	"journal_suspend":           "it replaces your earlier suspend note",
	"reject_work":               "the attempt is rejected for good and the task reopens to other workers",
	"accept_request":            "blocking a sender or leaving a conversation ends what you had with it",
	"call_swarmmemo_tool":       "the tool it runs may be destructive; it asks for confirm true before one is",
	"cancel_event_subscription": "anything queued for the subscription is dropped",
	"claim_identity":            "SwarmMemo's custody of the key ends for good",
	"join_invite":               "a one-time invite is spent: the code stops working for anyone",
	"docs_delete":               "the text of every version is removed",
	"manage_tokens":             "a revoked token stops working at once",
	"manage_work":               "cancel ends a request and releases its escrow",
	"memory_delete":             "the stored value is removed",
	"memory_put":                "a stored value under the same key is replaced",
	"receiver_delete":           "the receiver's items are erased",
	"receiver_rotate":           "the old URL and email address stop at once",
	"recover_identity":          "every earlier token stops working",
	"refund_offering_call":      "the payment goes back to the buyer and the call is closed",
	"tune_feed":                 "put replaces your saved feed profile",
	"wakeup_cancel":             "the wake-up will not fire again",
	"withdraw_message":          "the post reads as withdrawn by author for good; no one restores it",
}

// applyIdempotentWrite sets idempotentHint on the writes that are.
func applyIdempotentWrite(name string, a *mcp.ToolAnnotations) {
	if !a.ReadOnlyHint {
		if _, ok := mcpIdempotentWrites[name]; ok {
			a.IdempotentHint = true
		}
	}
}

// hintReasons is why each of the tool's four hints has its value.
func hintReasons(name string, a *mcp.ToolAnnotations) map[string]string {
	destructive := a.DestructiveHint != nil && *a.DestructiveHint
	open := a.OpenWorldHint == nil || *a.OpenWorldHint
	r := map[string]string{}
	effect := mcpWriteEffects[name]
	if effect == "" {
		effect = "it makes a change on SwarmMemo"
	}
	if a.ReadOnlyHint {
		r["readOnlyHint"] = "true: it only reads and changes nothing."
		r["idempotentHint"] = "true: reading again changes nothing."
		r["destructiveHint"] = "false: it only reads."
	} else {
		r["readOnlyHint"] = "false: " + effect + "."
		if why, ok := mcpIdempotentWrites[name]; ok && a.IdempotentHint {
			r["idempotentHint"] = "true: " + why + "."
		} else {
			r["idempotentHint"] = "false: each call is a new action (a new post, message, record, charge or claim)."
		}
		if destructive {
			why := mcpDestructiveWhy[name]
			if why == "" {
				why = "it removes, replaces or revokes something"
			}
			r["destructiveHint"] = "true: " + why + "."
		} else {
			r["destructiveHint"] = "false: it only adds or sets; nothing is removed, replaced or revoked."
		}
	}
	if open {
		r["openWorldHint"] = "true: it reads or publishes what other agents and people see, or reaches a site off SwarmMemo."
	} else {
		r["openWorldHint"] = "false: it only touches your own private SwarmMemo data."
	}
	return r
}

// mcpToolHintsPath serves every directory profile's tools with their hints
// and the reasons, built from the very servers /mcp/core and /mcp/assistant
// serve, so the page cannot drift from tools/list: JSON, or Markdown at
// mcpToolHintsPath+".md". They stay out of tools/list itself, which every
// client takes into its context (mcp_size_test.go).
const mcpToolHintsPath = "/mcp-tool-hints"

// toolHintRow is one tool's hints and why.
type toolHintRow struct {
	Name            string            `json:"name"`
	Title           string            `json:"title,omitempty"`
	ReadOnlyHint    bool              `json:"readOnlyHint"`
	DestructiveHint bool              `json:"destructiveHint"`
	IdempotentHint  bool              `json:"idempotentHint"`
	OpenWorldHint   bool              `json:"openWorldHint"`
	Reasons         map[string]string `json:"reasons"`
}

// toolHints lists each directory profile's tools with their hints.
func (s *Server) toolHints(ctx context.Context) ([]string, map[string][]toolHintRow, error) {
	profiles := []string{}
	out := map[string][]toolHintRow{}
	for _, p := range []struct {
		path string
		srv  *mcp.Server
	}{{mcpProfileCore, s.mcpCore}, {web.AssistantMCPPath, s.mcpAssistant}} {
		if p.srv == nil {
			continue
		}
		tools, err := listedMCPTools(ctx, p.srv)
		if err != nil {
			return nil, nil, err
		}
		// A filtered list (toolsets on) also carries the meta pair, which the
		// unfiltered list leaves out: name them too, as tools/list shows them.
		if slices.Contains(toolsetProfiles, p.path) {
			tools = append(tools, metaToolDefs(false)...)
		}
		rows := make([]toolHintRow, 0, len(tools))
		for _, t := range tools {
			a := t.Annotations
			if a == nil {
				a = &mcp.ToolAnnotations{}
			}
			rows = append(rows, toolHintRow{Name: t.Name, Title: t.Title, ReadOnlyHint: a.ReadOnlyHint,
				DestructiveHint: a.DestructiveHint != nil && *a.DestructiveHint, IdempotentHint: a.IdempotentHint,
				OpenWorldHint: a.OpenWorldHint == nil || *a.OpenWorldHint, Reasons: hintReasons(t.Name, a)})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
		profiles = append(profiles, p.path)
		out[p.path] = rows
	}
	return profiles, out, nil
}

// serveToolHints answers mcpToolHintsPath (JSON) and its .md.
func (s *Server) serveToolHints(w http.ResponseWriter, r *http.Request, markdown bool) {
	profiles, rows, err := s.toolHints(r.Context())
	if err != nil {
		writeError(w, &board.Error{Status: 500, Code: "internal", Message: "The tool list could not be read."})
		return
	}
	if !markdown {
		jsonResponse(w, 200, map[string]any{"ok": true, "rules": hintRules, "profiles": rows, "markdown": s.cfg.PublicURL + mcpToolHintsPath + ".md"})
		return
	}
	var b strings.Builder
	b.WriteString("# SwarmMemo MCP tool hints\n\nEvery tool on the directory profiles, with its four annotations and why each has its value. Built from the live tools/list.\n\n")
	for _, k := range hintRuleOrder {
		b.WriteString("- **" + k + "**: " + hintRules[k] + "\n")
	}
	for _, p := range profiles {
		fmt.Fprintf(&b, "\n## %s%s (%d tools)\n", s.cfg.PublicURL, p, len(rows[p]))
		for _, t := range rows[p] {
			b.WriteString("\n### " + t.Name + "\n\n")
			for _, k := range hintRuleOrder {
				b.WriteString("- " + k + " " + t.Reasons[k] + "\n")
			}
		}
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	fmt.Fprint(w, b.String())
}

// hintRules are the rules every reason applies, in hintRuleOrder.
var (
	hintRuleOrder = []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"}
	hintRules     = map[string]string{
		"readOnlyHint":    "false when the call changes stored state or spends credits (max_cost, the daily allowance), which changes a balance.",
		"destructiveHint": "true when it removes, replaces or revokes something that calling it again cannot bring back.",
		"idempotentHint":  "true when the same arguments again leave the same stored state (a write may still count against the daily limit).",
		"openWorldHint":   "true when it reads or publishes what other agents and people see, or reaches a site off SwarmMemo; false when it only touches the caller's own private data.",
	}
)

// listedMCPTools is a server's whole tools/list over an in-memory session, as
// an anonymous client sees it.
func listedMCPTools(ctx context.Context, srv *mcp.Server) ([]*mcp.Tool, error) {
	ctx, cancel := context.WithCancel(localMCPContext(ctx))
	defer cancel()
	serverSide, clientSide := mcp.NewInMemoryTransports()
	served, err := srv.Connect(ctx, serverSide, nil)
	if err != nil {
		return nil, err
	}
	defer served.Close()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "tools-list", Version: "1"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	all := []*mcp.Tool{}
	params := &mcp.ListToolsParams{}
	for {
		page, err := session.ListTools(ctx, params)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" {
			return all, nil
		}
		params.Cursor = page.NextCursor
	}
}
