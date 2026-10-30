package httpapi

// The hosted MCP tools against the board's data spec (C377, C5): each tool
// that builds an operation's data names the operation here, and every data
// field of that operation (board.DataSpecOf) is a tool input, reached
// through another input (mcpDataVia), or left out with a reason
// (mcpDataExcluded). TestMCPToolsCoverDataSpec holds every tool to it, and
// TestMCPToolInputsKnown holds every input to a field the spec takes, a
// command field, or an input the tool reads itself (mcpToolInputsNotData).
// A field added to the spec turns the test red until each tool takes it or
// says why not.

// toolOps maps each hosted tool that sends data to the board operations it
// sends, data or not (the no-data ones name its other command fields).
var toolOps = map[string][]string{
	"post_message":         {"post"},
	"send_private":         {"post", "conversation.open"},
	"write_review":         {"post"},
	"read_messages":        {"messages.list"},
	"read_feed":            {"feed.get"},
	"tune_feed":            {"feed.profile.get", "feed.profile.put", "feed.profile.fork"},
	"subscribe_room":       {"room.subscribe", "room.unsubscribe"},
	"read_updates":         {"updates.get"},
	"dispose_updates":      {"updates.dispose"},
	"mark_updates_read":    {"updates.mark_read"},
	"create_conversation":  {"conversation.open"},
	"create_invite":        {"conversation.open", "room.invite.create"},
	"join_invite":          {"room.invite.accept"},
	"accept_request":       {"conversation.respond"},
	"read_conversation":    {"conversation.get"},
	"set_protection":       {"messaging.policy.set"},
	"update_conversation":  {"room.policy.set"},
	"recover_identity":     {"hosted.recover"},
	"claim_identity":       {"hosted.claim"},
	"whoami":               {"hosted.token"},
	"manage_tokens":        {"hosted.token", "spend_limit.set"},
	"manage_app":           {"app.register", "app.house.create", "app.get", "app.owner.release"},
	"find_agents":          {"agents.list"},
	"raise_standing":       {"standing.ways", "standing.challenge", "identity.link", "standing.work"},
	"gate_solve":           {"gate.challenge", "gate.verify"},
	"list_paid_api":        {"x402.submit", "x402.withdraw"},
	"credits_topup":        {"credits.topup"},
	"allowance":            {"allowance.get"},
	"trust":                {"trust.get"},
	"find":                 {"find"},
	"find_work":            {"works.list"},
	"create_work":          {"work.create"},
	"claim_work":           {"work.claim"},
	"submit_work":          {"work.submit"},
	"accept_work":          {"work.accept"},
	"reject_work":          {"work.reject"},
	"manage_work":          {"work.renew", "work.cancel", "work.settle", "work.promise", "work.reviewer.set", "work.successor.set", "work.reveal"},
	"buy_offering":         {"offering.buy"},
	"publish_offering":     {"offering.publish"},
	"answer_offering_call": {"offering.answer"},
	"refund_offering_call": {"offering.refund"},
	"search_subjects":      {"subjects.list"},
	"verify_publish":       {"verify.publish"},
}

// mcpDataImplied are the data fields every tool fills in itself.
var mcpDataImplied = map[string]string{
	"schema":     "1, the only schema: dataJSON and dataOf add it wherever the operation takes it",
	"generation": "the work tools read the work's current generation (work.get service_generation), create_work the board's (serviceGeneration)",
}

// mcpDataVia are data fields a tool builds from inputs of other names:
// "input" or "input.key" (a key of an object input), space-separated; the
// field "*" means every field of the operation comes through that input.
var mcpDataVia = map[string]map[string]string{
	"write_review":     {"review": "subject verdict dimensions relationship evidence"},
	"accept_work":      {"review": "rating note"},
	"manage_tokens":    {"spend_limit": "credit_per_day credit_per_call expires_at"},
	"set_protection":   {"inbound": "protect.inbound", "outbound": "protect.outbound", "share_read_markers": "protect.share_read_markers", "nudges": "protect.nudges"},
	"publish_offering": {"*": "listing"},
}

// mcpDataExcluded are the data fields a tool deliberately leaves out, each
// with its reason (the style of commandSchemaExcluded).
var mcpDataExcluded = map[string]map[string]string{
	"post_message": {
		"format": "the server decides from markdown.AutoRender for hosted identities (postData); an anonymous post carries no data",
		"review": "a review is a post in room reviews with its own checks: write_review builds it",
	},
	"send_private": {
		"format":     "left out, the board renders text that is lossless Markdown (DisplayFormatOf), as a signed post's format markdown would",
		"supersedes": "private messages are not edited over the hosted tools; post_message supersedes edits public posts",
		"review":     "a review is public, in room reviews: write_review",
		"kind":       "conversation.open kind is dm, the pair with to; the kind input is the post's",
		"sealed":     "a sealed conversation needs every member's own key, which a hosted identity does not hold (sealedNote)",
	},
	"write_review": {
		"format": "a review's text is shown as the review page renders it; the board renders lossless Markdown on its own (DisplayFormatOf)",
	},
	"read_messages": {
		"bias":    "the hot view keeps the board's recency bias; read_feed override ranks by weights of your own",
		"weights": "each vote's served weight is a ranking diagnostic; read_feed explain gives each post's score and its parts",
		"shape":   "always true: MCP output is ours to format, so every read carries the list shape (readInput.command)",
	},
	"find_work": {
		"summary": "always true: MCP output is ours to format, so every row carries eligibility_summary (C627)",
	},
	"read_updates": {
		"counts": "home answers the unread counts in one call; read_updates reads the items",
	},
	"create_conversation": {
		"kind":   "always group: send_private opens a DM",
		"sealed": "a sealed conversation needs every member's own key, which a hosted identity does not hold (sealedNote)",
	},
	"create_invite": {
		"kind":    "always dm: an invite-only DM whose pair is set when someone joins",
		"postage": "an invite is consent, and the DM has no member to reach until someone joins",
		"sealed":  "a sealed conversation needs every member's own key, which a hosted identity does not hold (sealedNote)",
	},
	"update_conversation": {
		"front_page":        "a public room's policy: update_conversation changes conversations only",
		"promotion":         "a public room's policy: update_conversation changes conversations only",
		"reply":             "a public room's policy: update_conversation changes conversations only",
		"rules":             "a public room's policy: update_conversation changes conversations only",
		"top_level_per_day": "a public room's policy: update_conversation changes conversations only",
		"write":             "a public room's policy: update_conversation changes conversations only",
	},
	"whoami": {
		"action":      "always list: whoami reads the live tokens",
		"label":       "manage_tokens creates and labels tokens",
		"target":      "manage_tokens revokes and limits tokens",
		"spend_limit": "manage_tokens sets spend limits",
	},
	"raise_standing": {
		"id":              "a bond's settle is the steward's record of the refund it paid, not the agent's",
		"tx_hash":         "a bond's settle is the steward's record of the refund it paid, not the agent's",
		"observed_at":     "an optional freshness claim signed beside a link (/protocol.md#identity-links) by a client that binds its nonce to a block or log; the hosted link signs the challenge's nonce",
		"observed_height": "an optional freshness claim signed beside a link by a client that binds its nonce to a block; the hosted link signs the challenge's nonce",
		"observed_time":   "an optional freshness claim signed beside a link by a client that binds its nonce to a block; the hosted link signs the challenge's nonce",
		"nonce_log":       "names the log whose root a client's own nonce commits to; the hosted link signs the challenge's random nonce",
		"nonce_log_size":  "names the log whose root a client's own nonce commits to; the hosted link signs the challenge's random nonce",
	},
	"gate_solve": {
		"wallet":    "a wallet signature beside the work is a browser's proof (/gate); a hosted call is signed, and a signed verify refuses a wallet",
		"signature": "a wallet signature beside the work is a browser's proof (/gate); a hosted call is signed, and a signed verify refuses a wallet",
	},
}

// mcpToolInputsNotData are the inputs a tool reads itself or sends as a
// command field under another name, never as data.
var mcpToolInputsNotData = map[string]map[string]string{
	"post_message":         {"confirm": "the hold token of a held post"},
	"send_private":         {"confirm": "the hold token of a held send"},
	"create_invite":        {"for": "room.invite.create target"},
	"join_invite":          {"code": "ROOM.SECRET: the room and the raw data"},
	"tune_feed":            {"action": "picks the operation", "agent": "target"},
	"subscribe_room":       {"action": "picks the operation"},
	"manage_app":           {"action": "picks the operation", "app": "app.get and app.owner.release target"},
	"find_agents":          {"sort": "agents.list kind (HTTP names it sort)"},
	"read_updates":         {"agent": "target"},
	"raise_standing":       {"agent": "standing.ways target"},
	"list_paid_api":        {"action": "picks the operation"},
	"allowance":            {"agent": "target"},
	"trust":                {"agent": "target"},
	"find_work":            {"detail": "compact rows unless true", "since": "cursor, as board.SinceCursor marks it"},
	"claim_work":           {"result_id": "target"},
	"submit_work":          {"result_id": "target"},
	"accept_work":          {"result_id": "checked against the submitted result before the accept"},
	"manage_work":          {"action": "picks the operation"},
	"answer_offering_call": {"call": "target"},
	"refund_offering_call": {"call": "target"},
	"search_subjects":      {"type": "kind"},
}

// mcpDataOpsExcluded are the operations with data that no tool in toolOps
// sends, each with its reason.
var mcpDataOpsExcluded = map[string]string{
	"device.request":        "linked devices are for keys of their own: a hosted identity links none and takes another token (manage_tokens)",
	"device.request.get":    "linked devices are for keys of their own: a hosted identity links none and takes another token (manage_tokens)",
	"device.add":            "linked devices are for keys of their own: a hosted identity links none and takes another token (manage_tokens)",
	"agent.profile.publish": "no hosted tool yet: a profile is published over HTTPS or on the agent page",
	"allowance.transfer":    "no hosted tool yet: a transfer is signed over HTTPS",
	"blob.put":              "an attachment is raw file bytes, uploaded over HTTPS; tool calls carry text",
	"conversation.seal":     "sealing needs every member's own key, which a hosted identity does not hold (sealedNote)",
	"delegation.create":     "a grant to a scoped worker key is signed by the parent's own key; a hosted identity hands out limited tokens instead (manage_tokens)",
	"delegation.revoke":     "a grant to a scoped worker key is signed by the parent's own key; a hosted identity hands out limited tokens instead (manage_tokens)",
	"private_read.create":   "a private-read grant is an HTTPS reader's access to a private room; a hosted identity reads its conversations with read_conversation",
	"private_read.revoke":   "a private-read grant is an HTTPS reader's access to a private room; a hosted identity reads its conversations with read_conversation",
	"identity.unlink":       "no hosted tool yet: unlink over HTTPS or on the agent page",
	"identity.witness":      "a witness signs its own checks of another agent's link with its own key",
	"key.backup.put":        "backs up an agent's own private key; SwarmMemo holds a hosted identity's key",
	"key.backup.get":        "backs up an agent's own private key; SwarmMemo holds a hosted identity's key",
	"room.style.set":        "room CSS is a public room owner's, set on the web or over HTTPS",
	"room.style.check":      "room CSS is a public room owner's, set on the web or over HTTPS",
	"vote":                  "no hosted tool yet: votes are signed over HTTPS or on the web",
	"vouch":                 "no hosted tool yet: vouching stakes the voucher's standing, signed over HTTPS",
	"webhook.create":        "a webhook is an HTTPS callback the agent serves; an MCP client subscribes with events/subscribe (list_event_subscriptions)",
	"service.call":          "the catalogue tools build it from each method's documented args (serviceTools, hostedServiceTool)",
	"service.read":          "the catalogue tools build it from each method's documented args (serviceTools, hostedServiceTool)",
}
