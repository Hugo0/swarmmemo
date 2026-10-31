package httpapi

import (
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// mcpToolInputTypes is each toolOps tool's input struct, read for its input
// names (TestMCPToolInputTypesMatchListed holds it to tools/list).
var mcpToolInputTypes = map[string]any{
	"post_message": postInput{}, "send_private": sendPrivateInput{}, "write_review": writeReviewInput{},
	"read_messages": readInput{}, "read_feed": feedInput{}, "tune_feed": tuneFeedInput{}, "subscribe_room": subscribeRoomInput{},
	"manage_push": managePushInput{}, "read_updates": updatesInput{}, "dispose_updates": disposeUpdatesInput{}, "mark_updates_read": markUpdatesReadInput{},
	"create_conversation": createConversationInput{}, "create_invite": createInviteInput{}, "join_invite": joinInviteInput{},
	"accept_request": acceptRequestInput{}, "read_conversation": readConversationInput{}, "set_protection": setProtectionInput{},
	"update_conversation": updateConversationInput{}, "recover_identity": recoverIdentityInput{}, "claim_identity": claimIdentityInput{},
	"whoami": struct{}{}, "manage_tokens": manageTokensInput{}, "manage_app": manageAppInput{}, "find_agents": agentsInput{}, "raise_standing": raiseStandingInput{}, "gate_solve": gateSolveInput{},
	"list_paid_api": listPaidAPIInput{}, "credits_topup": creditsTopupInput{}, "allowance": allowanceInput{}, "trust": trustInput{},
	"find": findInput{}, "find_work": worksInput{}, "create_work": createWorkInput{}, "claim_work": claimWorkInput{},
	"submit_work": submitWorkInput{}, "accept_work": acceptWorkInput{}, "reject_work": rejectWorkInput{}, "manage_work": manageWorkInput{},
	"buy_offering": buyOfferingInput{}, "publish_offering": publishOfferingInput{}, "answer_offering_call": answerOfferingCallInput{},
	"refund_offering_call": refundOfferingCallInput{}, "search_subjects": searchSubjectsInput{}, "verify_publish": verifyPublishInput{},
}

// mcpToolsWithoutData are the hosted tools (beside the catalogue's) whose
// commands carry no data, or that read the store directly.
var mcpToolsWithoutData = []string{
	"read_thread", "list_pages", "list_rooms", "find_agents", "read_agent_posts", "read_agent", "read_work", "read_work_history",
	"log_proof", "agent_record", "work_proofs", "send_feedback", "home", "standing_ways", "list_services", "read_verification",
	"create_identity", "set_handle", "journal", "journal_suspend", "list_conversations", "list_event_subscriptions", "cancel_event_subscription",
	"find_offerings", "read_offering", "offering_call_status", "offering_calls", "claim_offering_call", "decline_offering_call",
	"resolve_subject", "read_subject", "withdraw_message",
}

// toolInputs are the json names of a tool's input struct.
func toolInputs(t *testing.T, tool string) []string {
	t.Helper()
	v, ok := mcpToolInputTypes[tool]
	if !ok {
		t.Fatalf("%s is in toolOps but not mcpToolInputTypes", tool)
	}
	var out []string
	for _, f := range reflect.VisibleFields(reflect.TypeOf(v)) {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if f.IsExported() && name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}

// viaInputs are the inputs a via names, without their keys.
func viaInputs(via string) []string {
	var out []string
	for _, w := range strings.Fields(via) {
		name, _, _ := strings.Cut(w, ".")
		out = append(out, name)
	}
	return out
}

// TestMCPToolsCoverDataSpec: every data field of every operation a tool
// sends is an input, built from inputs (mcpDataVia), filled in by every tool
// (mcpDataImplied), or excluded with a reason; and no via or exclusion names
// a field its operations do not take now.
func TestMCPToolsCoverDataSpec(t *testing.T) {
	for tool, ops := range toolOps {
		inputs := toolInputs(t, tool)
		taken := map[string]bool{}
		for _, op := range ops {
			if _, ok := board.LookupOperation(op); !ok {
				t.Errorf("%s: toolOps names %s, which is not an operation", tool, op)
				continue
			}
			spec, ok := board.DataSpecOf(op)
			if !ok {
				continue
			}
			for _, name := range spec.Names(false) {
				taken[name] = true
				via, viaOK := mcpDataVia[tool][name]
				if all, ok := mcpDataVia[tool]["*"]; ok && !viaOK {
					via, viaOK = all, true
				}
				_, excluded := mcpDataExcluded[tool][name]
				switch {
				case excluded, mcpDataImplied[name] != "":
				case viaOK:
					for _, in := range viaInputs(via) {
						if !slices.Contains(inputs, in) {
							t.Errorf("%s: %s data %s comes through %q, which is not an input", tool, op, name, in)
						}
					}
				case !slices.Contains(inputs, name):
					t.Errorf("%s: %s data %s is neither an input nor in mcpDataVia or mcpDataExcluded (with a reason)", tool, op, name)
				}
			}
		}
		for name, reason := range mcpDataExcluded[tool] {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%s: exclusion %s gives no reason", tool, name)
			}
			if !taken[name] {
				t.Errorf("%s: excludes %s, which no operation of %v takes now", tool, name, ops)
			}
		}
		for name := range mcpDataVia[tool] {
			if name != "*" && !taken[name] {
				t.Errorf("%s: mcpDataVia names %s, which no operation of %v takes now", tool, name, ops)
			}
		}
	}
	for tool := range mcpDataExcluded {
		if toolOps[tool] == nil {
			t.Errorf("mcpDataExcluded names %s, which is not in toolOps", tool)
		}
	}
	for tool := range mcpDataVia {
		if toolOps[tool] == nil {
			t.Errorf("mcpDataVia names %s, which is not in toolOps", tool)
		}
	}
}

// TestMCPToolInputsKnown: no tool input is named for a field its operations'
// spec marks removed, and each is a data field they take, a command field of
// one of them, a via's input, or an input the tool reads itself.
func TestMCPToolInputsKnown(t *testing.T) {
	for tool, ops := range toolOps {
		for _, in := range toolInputs(t, tool) {
			known := in == "request_id" || mcpToolInputsNotData[tool][in] != ""
			for _, op := range ops {
				spec, _ := board.DataSpecOf(op)
				if f, ok := spec.Field(in); ok && f.RemovedIn != "" {
					t.Errorf("%s: input %s is a field %s data stopped taking in %s", tool, in, op, f.RemovedIn)
				}
				operation, _ := board.LookupOperation(op)
				if spec.Takes(in) || slices.Contains(strings.Fields(operation.Fields), in) {
					known = true
				}
			}
			for _, via := range mcpDataVia[tool] {
				if slices.Contains(viaInputs(via), in) {
					known = true
				}
			}
			if !known {
				t.Errorf("%s: input %s is not a data or command field of %v; add it to mcpToolInputsNotData if the tool reads it itself", tool, in, ops)
			}
		}
		for in := range mcpToolInputsNotData[tool] {
			if !slices.Contains(toolInputs(t, tool), in) {
				t.Errorf("%s: mcpToolInputsNotData names %s, which is not an input", tool, in)
			}
		}
	}
	for tool := range mcpToolInputsNotData {
		if toolOps[tool] == nil {
			t.Errorf("mcpToolInputsNotData names %s, which is not in toolOps", tool)
		}
	}
}

// TestMCPDataOperationsHaveATool: every operation that takes data is sent by
// a tool in toolOps, or excluded with a reason; never both.
func TestMCPDataOperationsHaveATool(t *testing.T) {
	sent := map[string]bool{}
	for _, ops := range toolOps {
		for _, op := range ops {
			sent[op] = true
		}
	}
	for _, op := range board.Operations() {
		if _, ok := op.Data(); !ok {
			continue
		}
		reason, excluded := mcpDataOpsExcluded[op.Name]
		switch {
		case sent[op.Name] && excluded:
			t.Errorf("%s is sent by a tool and in mcpDataOpsExcluded", op.Name)
		case !sent[op.Name] && strings.TrimSpace(reason) == "":
			t.Errorf("%s takes data but no tool in toolOps sends it; add a tool or a reason to mcpDataOpsExcluded", op.Name)
		}
	}
	for op := range mcpDataOpsExcluded {
		if _, ok := board.DataSpecOf(op); !ok {
			t.Errorf("mcpDataOpsExcluded names %s, which takes no data", op)
		}
	}
}

// TestMCPToolInputTypesMatchListed: the input names read from each struct
// are the ones tools/list publishes, and every listed tool is accounted for:
// in toolOps, in mcpToolsWithoutData, or the catalogue's.
func TestMCPToolInputTypesMatchListed(t *testing.T) {
	s := pinServer(t)
	catalogue := map[string]bool{}
	for _, st := range serviceTools(s.staticCatalog()) {
		catalogue[st.spec.Name] = true
	}
	for _, st := range hostedServiceTools(s.staticCatalog()) {
		catalogue[st.spec.Name] = true
	}
	for tool := range mcpToolInputTypes {
		if toolOps[tool] == nil {
			t.Errorf("mcpToolInputTypes names %s, which is not in toolOps", tool)
		}
	}
	for _, path := range []string{"/mcp", mcpProfileCore, web.AssistantMCPPath} {
		_, tools := toolsListAnswer(t, s, path)
		for _, tool := range tools {
			name := tool["name"].(string)
			if toolOps[name] == nil {
				if !catalogue[name] && !slices.Contains(mcpToolsWithoutData, name) {
					t.Errorf("%s %s: not in toolOps, mcpToolsWithoutData or the catalogue", path, name)
				}
				continue
			}
			props, _ := dig(tool, "inputSchema", "properties").(map[string]any)
			var listed []string
			for k := range props {
				listed = append(listed, k)
			}
			want := toolInputs(t, name)
			if n := coreNarrowingOf(name); n != nil && path == mcpProfileCore {
				// The core profile takes the money arguments out (mcp_core.go).
				want = slices.DeleteFunc(want, func(arg string) bool { return slices.Contains(n.args, arg) })
			}
			sort.Strings(listed)
			sort.Strings(want)
			if !slices.Equal(listed, want) {
				t.Errorf("%s %s: tools/list inputs %v, struct %v", path, name, listed, want)
			}
		}
	}
}

// The data the closed gaps build (C377-3): a bond on raise_standing, postage
// on conversation.open and a hosted post's supersedes.
func TestMCPClosedGapsData(t *testing.T) {
	c, err := raiseStandingInput{Action: "challenge", Kind: "bond", USDC: "2.00", Payment: "cGF5"}.command("")
	if err != nil || c.Operation != "standing.challenge" || c.Data != `{"kind":"bond","payment":"cGF5","schema":1,"usdc":"2.00"}` {
		t.Fatalf("bond challenge: %v %+v", err, c)
	}
	if c, err = (raiseStandingInput{Action: "refund"}).command(""); err != nil || c.Data != `{"action":"refund","kind":"bond","schema":1}` {
		t.Fatalf("bond refund: %v %+v", err, c)
	}
	if got := openData("dm", 5); got != `{"kind":"dm","postage":5,"schema":1}` {
		t.Fatalf("openData with postage: %s", got)
	}
	if got := openData("group", 0); got != `{"kind":"group","schema":1}` {
		t.Fatalf("openData without postage: %s", got)
	}
	for _, c := range []struct {
		markdown   bool
		supersedes string
		want       string
	}{
		{false, "", ""},
		{true, "", board.MarkdownPostData},
		{false, "abc", `{"schema":1,"supersedes":"abc"}`},
		{true, "abc", `{"format":"markdown","schema":1,"supersedes":"abc"}`},
	} {
		if got := postData(c.markdown, c.supersedes); got != c.want {
			t.Errorf("postData(%v, %q) = %s, want %s", c.markdown, c.supersedes, got, c.want)
		}
	}
}
