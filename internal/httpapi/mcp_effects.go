package httpapi

import (
	"slices"
	"strings"
	"sync"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

// Whether an MCP tool is destructive comes from the operations it can run,
// never from a list of tools (C709: subscribe_room and manage_app said
// "nothing is removed" while one of their actions removed something). One
// table, mcpOperationEffects, says of every operation a write tool can run
// what it removes, replaces or revokes; a tool is destructive iff any of
// its operations is, and its destructiveHint reason names those
// operations. A new tool or a new action names its operations
// (toolOps, mcpToolMoreOps or its catalogue method), and
// TestMCPDestructiveFromOperations turns red until each one is classified
// here.

// mcpOperationEffects are the operations write tools run, each with what it
// removes, replaces or revokes, a predicate that follows the operation's
// name in a reason, or "" when it only adds or sets what the same call can
// set back. Keys are board operations, and SERVICE.METHOD for a catalogue
// method a tool calls with service.call (no board operation has such a
// name; the test holds them apart).
var mcpOperationEffects = map[string]string{
	// Posts and private messages.
	"post":                 "",
	"message.withdraw":     "marks the post withdrawn by author for good; no one restores it",
	"feedback":             "",
	"conversation.open":    "",
	"conversation.get":     "",
	"conversation.respond": "can block a sender or leave a conversation, which ends what you had with it",
	"room.invite.create":   "",
	"room.invite.accept":   "spends a one-time invite: the code stops working for anyone",
	"room.policy.set":      "",
	"messaging.policy.set": "",
	"updates.dispose":      "",
	"updates.mark_read":    "",
	"journal.suspend":      "replaces your earlier suspend note",
	// Feeds.
	"feed.profile.get":  "",
	"feed.profile.put":  "replaces your saved feed profile",
	"feed.profile.fork": "copies another agent's public profile over yours",
	"room.subscribe":    "resets a followed room's weight and clears its mute",
	"room.unsubscribe":  "removes the room and its weight from your feed profile",
	// Identity, tokens and apps.
	"hosted.create":           "",
	"hosted.recover":          "issues a new token: every earlier token stops working",
	"hosted.claim":            "ends SwarmMemo's custody of the key for good",
	"hosted.token":            "can revoke a token, which stops working at once",
	"agent.register":          "can rename your handle, which releases the old one for another agent to take",
	"spend_limit.set":         "",
	"oauth.connections.list":  "",
	"oauth.connection.revoke": "ends a sign-in connection at once",
	"app.register":            "",
	"app.house.create":        "",
	"app.get":                 "",
	"app.owner.release":       "ends your ownership of the app: it owns itself again",
	"oauth.client.register":   "",
	"oauth.clients.list":      "",
	"oauth.client.retire":     "retires a sign-in client: it signs nobody in again and every sign-in made through it ends",
	"room.create":             "",
	"room.close":              "closes the room for good: it takes no more posts",
	"room.members":            "",
	"stream.read":             "",
	"push.list":               "",
	"push.unsubscribe":        "removes a browser, which gets no more notifications until it opts in again",
	"webhook.delete":          "drops anything queued for the subscription",
	// Standing, gates and money.
	"standing.ways":      "",
	"standing.challenge": "",
	"identity.link":      "",
	"standing.work":      "",
	"gate.challenge":     "",
	"gate.verify":        "",
	"x402.submit":        "",
	"x402.withdraw":      "removes your paid API from Find's catalogue",
	"credits.topup":      "",
	"verify.publish":     "",
	// Work.
	"work.create":          "",
	"work.claim":           "",
	"work.submit":          "",
	"work.accept":          "is final: the held reward is paid out and cannot be taken back",
	"work.reject":          "rejects the attempt for good and reopens the task to other workers",
	"work.renew":           "",
	"work.cancel":          "ends the request and releases its escrow",
	board.WorkSettle:       "",
	board.WorkPromise:      "",
	board.WorkReviewerSet:  "",
	board.WorkSuccessorSet: "",
	board.WorkReveal:       "",
	// Offerings.
	"offering.buy":     "",
	"offering.publish": "",
	"offering.claim":   "",
	"offering.answer":  "",
	"offering.decline": "closes the call for good and drops the buyer's payment authorization",
	"offering.refund":  "sends the payment back to the buyer and closes the call",
	// Catalogue methods (service.call).
	"docs.create":        "",
	"docs.write":         "",
	"docs.read":          "",
	"docs.open":          "",
	"docs.delete":        "removes the text of every version",
	"fetch.page":         "",
	"inference.complete": "",
	"memory.put":         "replaces a stored value under the same key",
	"memory.delete":      "removes the stored value",
	"notary.stamp":       "",
	"public_data.bulk":   "",
	"public_data.fetch":  "",
	"receiver.create":    "",
	"receiver.rotate":    "replaces the receiver's URL and email address; the old ones stop at once",
	"receiver.delete":    "stops the receiver and erases its items",
	"screen.leak":        "",
	"screen.text":        "",
	"tools.call":         "",
	"wakeup.schedule":    "",
	"wakeup.cancel":      "cancels the wake-up; it will not fire again",
	"x402.call":          "",
}

// mcpToolMoreOps are the operations of write tools toolOps leaves out
// (toolOps names the ones that build an operation's data), or runs beside
// it.
var mcpToolMoreOps = map[string][]string{
	"create_identity":           {"hosted.create"},
	"withdraw_message":          {"message.withdraw"},
	"set_handle":                {"agent.register"},
	"journal_suspend":           {"journal.suspend"},
	"cancel_event_subscription": {"webhook.delete"},
	"send_feedback":             {"feedback"},
	"claim_work":                {"work.submit"},
	"claim_offering_call":       {"offering.claim"},
	"decline_offering_call":     {"offering.decline"},
}

// callToolDestructiveWhy is the meta tool's reason: it runs any listed
// tool, destructive ones only with confirm true (mcp_dispatch.go).
const callToolDestructiveWhy = "the tool it runs may be destructive; it asks for confirm true before one is"

// catalogueToolOps maps each catalogue tool, public or hosted, of every
// built-in service to its SERVICE.METHOD.
var catalogueToolOps = sync.OnceValue(func() map[string]string {
	out := map[string]string{}
	for _, st := range serviceTools(services.Catalog(services.Known())) {
		out[st.spec.Name] = st.entry.ID + "." + st.method.Name
	}
	for _, st := range hostedServiceTools(services.Catalog(hostedServices)) {
		out[st.spec.Name] = st.entry.ID + "." + st.method.Name
	}
	return out
})

// mcpToolOperations are the operations tool name can run; on the core
// profile less those the board refuses there as money
// (board.MoneyOperation).
func mcpToolOperations(name string, core bool) []string {
	ops := slices.Concat(toolOps[name], mcpToolMoreOps[name])
	if op, ok := catalogueToolOps()[name]; ok {
		ops = append(ops, op)
	}
	if core {
		ops = slices.DeleteFunc(ops, func(op string) bool { return board.MoneyOperation(op) != "" })
	}
	return ops
}

// toolDestructiveWhy is what tool name removes, replaces or revokes, each
// destructive operation it can run named with its effect, or "" when it
// is not destructive.
func toolDestructiveWhy(name string, core bool) string {
	var whys []string
	for _, op := range mcpToolOperations(name, core) {
		if effect := mcpOperationEffects[op]; effect != "" {
			whys = append(whys, op+" "+effect)
		}
	}
	return strings.Join(whys, "; ")
}
