package httpapi

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// The two outside reports C709 closes: subscribe_room overwrote a followed
// room's weight, cleared its mute and removed rooms, and manage_app could
// disown an app, retire a sign-in client or close a room, while both said
// "nothing is removed, replaced or revoked". Each is destructive now, its
// reason naming the operations that are.
func TestMCPDestructiveReportedTools(t *testing.T) {
	s := pinServer(t)
	full := listTools(t, s, "/mcp?toolsets=all")
	for name, ops := range map[string][]string{
		"subscribe_room": {"room.subscribe", "room.unsubscribe"},
		"manage_app":     {"app.owner.release", "oauth.client.retire", "room.close"},
	} {
		if !full[name].Annotations["destructiveHint"] {
			t.Errorf("%s destructiveHint false", name)
		}
		why := toolDestructiveWhy(name, false)
		for _, op := range ops {
			if !strings.Contains(why, op+" ") {
				t.Errorf("%s reason %q does not name %s", name, why, op)
			}
		}
		reason := hintReasons(name, false, &mcp.ToolAnnotations{DestructiveHint: boolPtr(true)})["destructiveHint"]
		if reason != "true: "+why+"." {
			t.Errorf("%s destructiveHint reason %q", name, reason)
		}
	}
	if why := toolDestructiveWhy("subscribe_room", false); !strings.Contains(why, "resets a followed room's weight and clears its mute") || !strings.Contains(why, "removes the room") {
		t.Errorf("subscribe_room reason %q", why)
	}
	// The read and list actions of manage_app are reads; the tool is still
	// destructive as a whole, as manage_tokens is for list.
	for _, op := range []string{"app.get", "oauth.clients.list", "room.members", "stream.read"} {
		if mcpOperationEffects[op] != "" {
			t.Errorf("%s is a read but classified destructive", op)
		}
	}
}

// Every tool on every profile is destructive exactly when an operation it
// can run is: a write tool names its operations (toolOps, mcpToolMoreOps or
// its catalogue method), each one is classified in mcpOperationEffects, and
// its destructiveHint and reason follow. A read-only tool is never
// destructive and runs no destructive operation. A new tool or action
// cannot drift.
func TestMCPDestructiveFromOperations(t *testing.T) {
	s := pinServer(t)
	used := map[string]bool{}
	for _, path := range []string{"/mcp?toolsets=all", mcpProfileCore, web.AssistantMCPPath + "?toolsets=all"} {
		core := path == mcpProfileCore
		for name, tool := range listTools(t, s, path) {
			got := tool.Annotations["destructiveHint"]
			if isMetaTool(name) {
				if got != (name == callToolName) {
					t.Errorf("%s %s destructiveHint %v", path, name, got)
				}
				continue
			}
			ops := mcpToolOperations(name, core)
			if tool.Annotations["readOnlyHint"] {
				// A read runs only the reading form of its operations
				// (whoami: hosted.token action list).
				if got {
					t.Errorf("%s %s reads only but is destructive", path, name)
				}
				continue
			}
			if len(ops) == 0 {
				t.Errorf("%s %s writes but names no operations (toolOps, mcpToolMoreOps or the catalogue)", path, name)
				continue
			}
			want := false
			var destructiveOps []string
			for _, op := range ops {
				effect, known := mcpOperationEffects[op]
				if !known {
					t.Errorf("%s %s runs %s, which mcpOperationEffects does not classify", path, name, op)
				}
				if effect != "" {
					want = true
					destructiveOps = append(destructiveOps, op)
				}
			}
			if got != want {
				t.Errorf("%s %s destructiveHint %v; its operations %v say %v", path, name, got, ops, want)
			}
			reason := hintReasons(name, core, &mcp.ToolAnnotations{DestructiveHint: &got})["destructiveHint"]
			if want {
				for _, op := range destructiveOps {
					if !strings.Contains(reason, op+" "+mcpOperationEffects[op]) {
						t.Errorf("%s %s reason %q leaves out %s", path, name, reason, op)
					}
				}
			} else if !strings.HasPrefix(reason, "false: ") {
				t.Errorf("%s %s reason %q", path, name, reason)
			}
		}
	}
	// No stale rows: every classified operation is one a tool runs (listed
	// here or behind a feature this server leaves off), and a key names a
	// board operation unless it is a catalogue method.
	methods := map[string]bool{}
	for _, op := range catalogueToolOps() {
		methods[op], used[op] = true, true
	}
	for _, table := range []map[string][]string{toolOps, mcpToolMoreOps} {
		for _, ops := range table {
			for _, op := range ops {
				used[op] = true
			}
		}
	}
	for op := range mcpOperationEffects {
		if !used[op] {
			t.Errorf("mcpOperationEffects classifies %s, which no listed tool runs", op)
		}
		if methods[op] == board.KnownOperation(op) {
			t.Errorf("%s: catalogue method %v, board operation %v; exactly one", op, methods[op], board.KnownOperation(op))
		}
	}
}

// Tools whose operations only add or set stay non-destructive, and the
// core profile drops the money operations its board refuses before
// deriving.
func TestMCPNonDestructiveToolsStayFalse(t *testing.T) {
	s := pinServer(t)
	full := listTools(t, s, "/mcp?toolsets=all")
	for _, name := range []string{"post_message", "send_private", "create_work", "claim_work", "submit_work", "create_conversation",
		"create_invite", "set_protection", "update_conversation", "publish_offering", "notary_stamp", "receiver_create", "wakeup_schedule", "docs_write"} {
		tool, ok := full[name]
		if !ok {
			t.Errorf("%s not listed", name)
			continue
		}
		if tool.Annotations["destructiveHint"] || toolDestructiveWhy(name, false) != "" {
			t.Errorf("%s destructive: %q", name, toolDestructiveWhy(name, false))
		}
		if reason := hintReasons(name, false, &mcp.ToolAnnotations{DestructiveHint: boolPtr(false)})["destructiveHint"]; !strings.HasPrefix(reason, "false: ") {
			t.Errorf("%s reason %q", name, reason)
		}
	}
	for _, op := range mcpToolOperations("manage_work", true) {
		if board.MoneyOperation(op) != "" {
			t.Errorf("core manage_work runs money operation %s", op)
		}
	}
}

func boolPtr(b bool) *bool { return &b }
