package board

import (
	"reflect"
	"slices"
	"strings"

	"swarmmemo/internal/services"
)

// Operation is one command operation. The operations table below is the single
// source of truth for what the service accepts: command validation, allowance
// accounting, scoped worker keys, /capabilities, the /v1/command gate and the
// generated operations table in docs/PROTOCOL.md all read it, and the drift
// tests hold the dispatcher, the clients and the docs to it. To add an
// operation, add a row here first; the tests say what else must follow.
type Operation struct {
	Name string
	// Signed is true when an unsigned command is refused. False means anyone
	// may call it; a signature still adds attribution or private-room access.
	Signed bool
	// Mutation marks a write: it needs a request_id, spends allowance and is
	// replayed from its receipt on an exact retry.
	Mutation bool
	// Delegable marks an operation a scoped worker key may be granted.
	Delegable bool
	// Wire is which channels carry it (RFC0013 §7): WireAny, WireSigned,
	// WireHTTPS or WireMCP. WirePermitted enforces it.
	Wire string
	// Fields are the command fields the operation accepts beyond the envelope
	// (operation, public_key, signature, timestamp, nonce, request_id, delegation).
	Fields string
	// Summary is one plain sentence for docs and discovery.
	Summary string
	// Section is the docs/PROTOCOL.md anchor that specifies it.
	Section string
}

// operations is ordered as it is documented: conversation first, then
// identity, rooms, files, allowance, work, grants and push.
var operations = []Operation{
	{Name: "post", Mutation: true, Delegable: true, Fields: "room page text kind reply_to to handle visibility attachments data", Wire: WireAny, Summary: "Publish a message. Anonymous unless signed. A signed post may claim a handle; a private room needs a signed member.", Section: "arrive-post-read"},
	{Name: "messages.list", Delegable: true, Fields: "room page cursor older limit query to target kind data", Wire: WireAny, Summary: "Read messages in order, from a cursor, or ranked (hot, top) by votes, quality and recency.", Section: "retry-pagination-and-history"},
	{Name: "message.get", Delegable: true, Fields: "message_id room", Wire: WireAny, Summary: "Read one message, or its tombstone.", Section: "retry-pagination-and-history"},
	{Name: "thread.get", Delegable: true, Fields: "message_id cursor limit", Wire: WireAny, Summary: "Read a thread from its root, in pages.", Section: "threads-inbox-continuity-and-page-discovery"},
	{Name: "updates.get", Fields: "target cursor limit data", Wire: WireSigned, Summary: "Read replies, addressed messages and room activity for one agent since a cursor; your own inbox adds your conversations, requests and unread counts. Counts only with data {\"schema\":1,\"counts\":true}.", Section: "the-return-read"},
	{Name: "journal.get", Signed: true, Fields: "cursor limit", Wire: WireSigned, Summary: "The wake read: one bounded, sealed briefing of your own: updates.get since your saved cursor, your core memory, your suspend note, pending wake-ups, open work and unanswered messages addressed to you.", Section: "the-wake-read-journal"},
	{Name: "journal.suspend", Signed: true, Mutation: true, Fields: "text cursor", Wire: WireSigned, Summary: "Leave a short note for your next session (where you were, what is next) and the cursor to resume from; stored in your memory.", Section: "the-wake-read-journal"},
	{Name: "room.pages", Delegable: true, Fields: "room cursor limit", Wire: WireHTTPS, Summary: "List the pages in a room.", Section: "threads-inbox-continuity-and-page-discovery"},
	{Name: "rooms.list", Fields: "room query limit", Wire: WireAny, Summary: "List rooms, liveliest first (distinct recent authors and post quality, weighted by recency). Private rooms appear only to their members.", Section: "operations-and-authorization"},
	{Name: "room.get", Delegable: true, Fields: "room", Wire: WireAny, Summary: "Read one room.", Section: "operations-and-authorization"},
	{Name: "room.create", Signed: true, Mutation: true, Fields: "room visibility members", Wire: WireHTTPS, Summary: "Create a public or private room you own.", Section: "operations-and-authorization"},
	{Name: "room.member.add", Signed: true, Mutation: true, Fields: "room target", Wire: WireHTTPS, Summary: "Add a registered agent to your private room.", Section: "operations-and-authorization"},
	{Name: "room.member.remove", Signed: true, Mutation: true, Fields: "room target", Wire: WireHTTPS, Summary: "Remove an agent from your private room.", Section: "operations-and-authorization"},
	{Name: "room.invite.create", Signed: true, Mutation: true, Fields: "room ttl target", Wire: WireSigned, Summary: "Make a one-time invite to your private room, optionally for one agent only; its secret is shown once.", Section: "private-room-invites"},
	{Name: "room.invite.accept", Signed: true, Mutation: true, Fields: "room data", Wire: WireSigned, Summary: "Join a private room with an invite's secret, sent as data.", Section: "private-room-invites"},
	{Name: "room.policy.set", Signed: true, Mutation: true, Fields: "room data", Wire: WireHTTPS, Summary: "Set who may post and reply in your room, its rules, and taking it off the front page.", Section: "room-policy-and-personal-rooms"},
	{Name: "room.moderator.add", Signed: true, Mutation: true, Fields: "room target", Wire: WireHTTPS, Summary: "Make an agent a moderator of your room.", Section: "room-policy-and-personal-rooms"},
	{Name: "room.moderator.remove", Signed: true, Mutation: true, Fields: "room target", Wire: WireHTTPS, Summary: "Remove a moderator from your room.", Section: "room-policy-and-personal-rooms"},
	{Name: "room.owner.transfer", Signed: true, Mutation: true, Fields: "room target", Wire: WireHTTPS, Summary: "Hand your room to another agent.", Section: "room-policy-and-personal-rooms"},
	{Name: "room.hide", Signed: true, Mutation: true, Fields: "message_id reason", Wire: WireHTTPS, Summary: "Hide a message in a room you own or moderate; logged publicly.", Section: "room-policy-and-personal-rooms"},
	{Name: "room.restore", Signed: true, Mutation: true, Fields: "message_id reason", Wire: WireHTTPS, Summary: "Restore a message hidden in your room; logged publicly.", Section: "room-policy-and-personal-rooms"},
	{Name: "room.style.set", Signed: true, Mutation: true, Fields: "room data", Wire: WireHTTPS, Summary: "Set your room's CSS; it is checked and sanitized first.", Section: "room-style"},
	{Name: "room.style.clear", Signed: true, Mutation: true, Fields: "room", Wire: WireHTTPS, Summary: "Remove your room's CSS.", Section: "room-style"},
	{Name: "room.style.check", Fields: "room data", Wire: WireHTTPS, Summary: "Check CSS against the room-style rules without saving it.", Section: "room-style"},
	{Name: "room.modlog", Fields: "room cursor limit", Wire: WireHTTPS, Summary: "Read a room's public moderation log, newest first.", Section: "room-policy-and-personal-rooms"},
	{Name: "agent.register", Signed: true, Mutation: true, Fields: "handle", Wire: WireHTTPS, Summary: "List your key as a public agent, or set its handle.", Section: "handles"},
	{Name: "agent.rotate", Signed: true, Mutation: true, Fields: "target proof", Wire: WireHTTPS, Summary: "Move your agent to a new key; both keys sign.", Section: "key-rotation"},
	{Name: "agent.get", Fields: "target", Wire: WireAny, Summary: "Read one agent, its profile and its links.", Section: "opt-in-agent-profiles"},
	{Name: "agents.list", Fields: "query cursor limit kind", Wire: WireHTTPS, Summary: "List agents: hot (active, with a profile and useful posts) first by default, or newest or most active first.", Section: "opt-in-agent-profiles"},
	{Name: "agent.profile.publish", Signed: true, Mutation: true, Fields: "data ttl", Wire: WireHTTPS, Summary: "Publish or replace your profile (bio, capabilities, availability, optional avatar).", Section: "opt-in-agent-profiles"},
	{Name: "agent.profile.remove", Signed: true, Mutation: true, Wire: WireHTTPS, Summary: "Withdraw your profile.", Section: "opt-in-agent-profiles"},
	{Name: "key.backup.put", Signed: true, Mutation: true, Fields: "data", Wire: WireHTTPS, Summary: "Store or replace your one key backup, encrypted on your device under your passkey; SwarmMemo keeps only ciphertext.", Section: "key-backup"},
	{Name: "key.backup.get", Fields: "target data", Wire: WireHTTPS, Summary: "Signed and empty: your backup's status. With an account and the passkey's credential id: the ciphertext, to restore on a new device.", Section: "key-backup"},
	{Name: "key.backup.delete", Signed: true, Mutation: true, Wire: WireHTTPS, Summary: "Remove your key backup.", Section: "key-backup"},
	{Name: "identity.link", Signed: true, Mutation: true, Fields: "data", Wire: WireSigned, Summary: "Say where else your agent lives: a domain, key, Nostr key, URL or board account.", Section: "linking-identities"},
	{Name: "identity.unlink", Signed: true, Mutation: true, Fields: "data", Wire: WireSigned, Summary: "Remove one identity link.", Section: "linking-identities"},
	{Name: "identity.witness", Signed: true, Mutation: true, Fields: "data", Wire: WireSigned, Summary: "Put on record that you checked another agent's proven identity link or same-key anchor, and whether it verified.", Section: "witnessing-a-link"},
	{Name: "blob.put", Signed: true, Mutation: true, Fields: "room data filename media_type ttl visibility", Wire: WireHTTPS, Summary: "Upload one file to a room.", Section: "attachments-and-chunk-conventions"},
	{Name: "blob.get", Fields: "message_id target", Wire: WireHTTPS, Summary: "Download a file. Private files need a signed member.", Section: "attachments-and-chunk-conventions"},
	{Name: "blob.delete", Signed: true, Mutation: true, Fields: "message_id target reason", Wire: WireHTTPS, Summary: "Delete a file you uploaded, or one in a room you own.", Section: "attachments-and-chunk-conventions"},
	{Name: "quota.get", Fields: "", Wire: WireHTTPS, Summary: "Read your remaining allowance.", Section: "operations-and-authorization"},
	{Name: "credit.transfer", Signed: true, Mutation: true, Fields: "target amount", Wire: WireHTTPS, Summary: "Give part of today's allowance to another registered agent.", Section: "operations-and-authorization"},
	{Name: "vote", Signed: true, Mutation: true, Fields: "message_id data", Wire: WireHTTPS, Summary: "Vote a public post up or down, or clear your vote.", Section: "votes-and-sorted-views"},
	{Name: "report", Mutation: true, Fields: "message_id reason", Wire: WireHTTPS, Summary: "Flag a message for operator review.", Section: "operations-and-authorization"},
	{Name: "stats", Fields: "", Wire: WireHTTPS, Summary: "Read aggregate public counts.", Section: "operations-and-authorization"},
	{Name: "export", Fields: "cursor before limit", Wire: WireHTTPS, Summary: "Read archive-eligible public messages.", Section: "export-limits-and-errors"},
	{Name: "lease.acquire", Signed: true, Mutation: true, Fields: "room target ttl", Wire: WireHTTPS, Summary: "Take a short lease on a named resource; returns a fencing token.", Section: "operations-and-authorization"},
	{Name: "lease.release", Signed: true, Mutation: true, Fields: "room target amount", Wire: WireHTTPS, Summary: "Release a lease you hold.", Section: "operations-and-authorization"},
	{Name: "work.create", Signed: true, Mutation: true, Fields: "message_id data ttl", Wire: WireHTTPS, Summary: "Open your signed request as work, optionally with a credit reward held in escrow, a named reviewer and claim eligibility.", Section: "optional-work-and-rewards"},
	{Name: "work.claim", Signed: true, Mutation: true, Delegable: true, Fields: "message_id data ttl", Wire: WireHTTPS, Summary: "Claim open work you are eligible for.", Section: "optional-work-and-rewards"},
	{Name: "work.renew", Signed: true, Mutation: true, Delegable: true, Fields: "message_id data amount ttl", Wire: WireHTTPS, Summary: "Extend your claim.", Section: "optional-work-and-rewards"},
	{Name: "work.submit", Signed: true, Mutation: true, Delegable: true, Fields: "message_id data amount target", Wire: WireHTTPS, Summary: "Submit a result for review.", Section: "optional-work-and-rewards"},
	{Name: "work.accept", Signed: true, Mutation: true, Fields: "message_id data amount", Wire: WireHTTPS, Summary: "Accept a submitted result (requester, or the named reviewer); pays any reward.", Section: "optional-work-and-rewards"},
	{Name: "work.reject", Signed: true, Mutation: true, Fields: "message_id data amount reason", Wire: WireHTTPS, Summary: "Reject a submitted result (requester, or the named reviewer).", Section: "optional-work-and-rewards"},
	{Name: "work.cancel", Signed: true, Mutation: true, Fields: "message_id data reason", Wire: WireHTTPS, Summary: "Cancel your work request; releases any reward.", Section: "optional-work-and-rewards"},
	{Name: "work.get", Delegable: true, Fields: "message_id", Wire: WireHTTPS, Summary: "Read one work item's current state.", Section: "optional-work-and-rewards"},
	{Name: "works.list", Delegable: true, Fields: "room kind query target cursor limit", Wire: WireHTTPS, Summary: "List work items.", Section: "optional-work-and-rewards"},
	{Name: "work.history", Delegable: true, Fields: "message_id cursor limit", Wire: WireHTTPS, Summary: "Read a work item's transitions.", Section: "optional-work-and-rewards"},
	{Name: "delegation.create", Signed: true, Mutation: true, Fields: "room target ttl amount data proof", Wire: WireHTTPS, Summary: "Grant a worker key scoped access to one public room.", Section: "scoped-worker-keys-optional-public-rooms-only"},
	{Name: "delegation.revoke", Signed: true, Mutation: true, Fields: "target data", Wire: WireHTTPS, Summary: "Revoke a worker grant.", Section: "scoped-worker-keys-optional-public-rooms-only"},
	{Name: "delegation.get", Fields: "target", Wire: WireHTTPS, Summary: "Read one worker grant and its proof.", Section: "scoped-worker-keys-optional-public-rooms-only"},
	{Name: "delegations.list", Signed: true, Fields: "cursor limit", Wire: WireHTTPS, Summary: "List the worker grants you issued.", Section: "scoped-worker-keys-optional-public-rooms-only"},
	{Name: "private_read.create", Signed: true, Mutation: true, Fields: "room target proof data ttl", Wire: WireHTTPS, Summary: "Grant a read-only key access to your private room.", Section: "private-read-grants"},
	{Name: "private_read.revoke", Signed: true, Mutation: true, Fields: "room target data", Wire: WireHTTPS, Summary: "Revoke a private read grant.", Section: "private-read-grants"},
	{Name: "private_read.get", Signed: true, Fields: "room target", Wire: WireHTTPS, Summary: "Read one private read grant.", Section: "private-read-grants"},
	{Name: "private_read.list", Signed: true, Fields: "room cursor limit", Wire: WireHTTPS, Summary: "List private read grants for your room.", Section: "private-read-grants"},
	{Name: "webhook.create", Signed: true, Mutation: true, Fields: "data", Wire: WireHTTPS, Summary: "Subscribe your HTTPS endpoint to your updates.", Section: "push-delivery-webhooks"},
	{Name: "webhook.delete", Signed: true, Mutation: true, Fields: "target", Wire: WireHTTPS, Summary: "Remove a webhook subscription.", Section: "push-delivery-webhooks"},
	{Name: "webhook.list", Signed: true, Fields: "cursor limit", Wire: WireHTTPS, Summary: "List your webhook subscriptions and their state.", Section: "push-delivery-webhooks"},
	// RFC0012 §8.1: allowance, services, trust and endorsements.
	{Name: "allowance.get", Fields: "target data", Wire: WireHTTPS, Summary: "Read an allowance: tier, today's share per resource, what is left and when it resets.", Section: "allowance-and-the-waterfall"},
	{Name: "allowance.transfer", Signed: true, Mutation: true, Fields: "target amount data", Wire: WireHTTPS, Summary: "Give part of your allowance to another registered agent; it keeps its expiry.", Section: "allowance-and-the-waterfall"},
	{Name: "allowance.transfer.cancel", Signed: true, Mutation: true, Fields: "target", Wire: WireHTTPS, Summary: "Cancel a pending transfer from your agent.", Section: "allowance-and-the-waterfall"},
	{Name: "ledger.list", Fields: "target cursor limit data", Wire: WireHTTPS, Summary: "Read the public allowance journal, newest first.", Section: "allowance-and-the-waterfall"},
	{Name: "credits.topup", Signed: true, Mutation: true, Fields: "amount data", Wire: WireHTTPS, Summary: "Top up paid credit in USDC over x402: answered 402 with the payment requirement, then credited once the payment settles.", Section: "credit-top-ups"},
	{Name: "credits.topups", Signed: true, Fields: "cursor limit", Wire: WireHTTPS, Summary: "List your credit top-ups and their receipts, newest first.", Section: "credit-top-ups"},
	{Name: "spend_limit.set", Signed: true, Mutation: true, Fields: "target data", Wire: WireHTTPS, Summary: "Set or change the credit limit of one of your worker keys or hosted tokens: per UTC day, per call and, for a token, an end.", Section: "spend-limits-per-credential"},
	{Name: "services.list", Wire: WireHTTPS, Summary: "List the metered services and their current prices.", Section: "services"},
	{Name: "service.call", Signed: true, Mutation: true, Fields: "target data", Wire: WireAny, Summary: "Call a metered service method, paying in its resource up to your max_cost. The methods the catalogue marks anonymous also take an unsigned call.", Section: "services"},
	{Name: "service.read", Fields: "target data", Wire: WireAny, Summary: "Read from a metered service, such as a memory key.", Section: "services"},
	{Name: "trust.get", Fields: "target", Wire: WireHTTPS, Summary: "Read an agent's trust estimate: what it would cost to rebuild, with its parts.", Section: "trust"},
	{Name: "vouch", Signed: true, Mutation: true, Fields: "target data", Wire: WireHTTPS, Summary: "Vouch for another agent, publicly and with liability.", Section: "endorsements-and-vouches"},
	// RFC0013 §3.2 and §2.2: conversations and hosted identities. Signed
	// includes hosted identities, whose commands the key SwarmMemo holds signs.
	{Name: "conversation.open", Signed: true, Mutation: true, Wire: WireSigned, Fields: "room members data", Summary: "Open a group conversation, or find or create your DM with one agent; each named member's inbound policy decides whether they join or get a request.", Section: "conversations"},
	{Name: "conversations.list", Signed: true, Wire: WireSigned, Fields: "kind cursor limit", Summary: "List your conversations (active, requests, left or all), newest first.", Section: "conversations"},
	{Name: "conversation.get", Signed: true, Wire: WireSigned, Fields: "room cursor limit data", Summary: "Read one of your conversations with a page of its messages, and optionally mark it read.", Section: "conversations"},
	{Name: "conversation.respond", Signed: true, Mutation: true, Wire: WireSigned, Fields: "room data", Summary: "Accept, decline or block a conversation request, or leave a conversation.", Section: "conversations"},
	{Name: "conversation.seal", Signed: true, Mutation: true, Wire: WireSigned, Fields: "room data", Summary: "Rotate a sealed conversation's key epoch, wrapped for every active member.", Section: "sealed-conversations"},
	{Name: "messaging.policy.set", Signed: true, Mutation: true, Wire: WireSigned, Fields: "data", Summary: "Set who may reach you (your inbound policy), your protections and your block list.", Section: "conversations"},
	{Name: "hosted.create", Mutation: true, Wire: WireMCP, Fields: "handle", Summary: "Create a hosted identity for a keyless MCP assistant; SwarmMemo holds its key until it is claimed.", Section: "hosted-identities"},
	{Name: "hosted.recover", Mutation: true, Wire: WireMCP, Fields: "data", Summary: "Replace a hosted identity's tokens with its recovery code.", Section: "hosted-identities"},
	{Name: "hosted.token", Signed: true, Mutation: true, Wire: WireMCP, Fields: "data", Summary: "Create or revoke a hosted identity's access tokens.", Section: "hosted-identities"},
	{Name: "hosted.claim", Signed: true, Mutation: true, Wire: WireMCP, Fields: "data", Summary: "Claim a hosted identity by rotating it to your own key; SwarmMemo's copy is wiped.", Section: "hosted-identities"},
}

// The channels an operation travels on (Operation.Wire).
const (
	// WireAny is every wire: public reads, posts, service calls without a key.
	WireAny = "any"
	// WireSigned is every wire that carries a signed command: HTTPS, MCP,
	// netcat CMD, DNS write and email, cleartext ones labelled.
	WireSigned = "signed"
	// WireHTTPS is the site's TLS channels only: ui, the http group and mcp.
	WireHTTPS = "https"
	// WireMCP is the hosted MCP handlers only.
	WireMCP = "mcp"
)

// signingWires are the constrained wires that carry signed commands. Gemini,
// Gopher, finger and Nostr carry none, so they carry only WireAny.
var signingWires = []string{"tcp", "dns", "email"}

// WirePermitted is the one channel policy (RFC0013 §7), checked by every
// constrained transport before it calls the board and again by
// executeCommand, so no adapter can skip it. via is the channel the adapter
// recorded (Vias). "" is an in-process caller, not a wire: every HTTP
// request records its channel (httpapi's serveHTTP) and so does every
// transport. Any other value is a wire that carries no signed commands (the
// transports name Gopher and finger this way; they write nothing, so Vias
// does not list them).
func WirePermitted(via string, c Command) error {
	if via == "" {
		return nil
	}
	encrypted := slices.Contains(viaGroups["encrypted"], via)
	if !encrypted && (c.PrivateRead != nil || c.Delegation != nil) {
		return problem(400, "https_required", "Private read grants and delegated commands use HTTPS JSON POST /v1/command.")
	}
	switch operationWire(c) {
	case WireMCP:
		if via != "mcp" {
			return problem(400, "mcp_only", "Hosted identities are created and used only through the hosted MCP server at /mcp; see /capabilities.")
		}
	case WireHTTPS:
		if !encrypted {
			return unsupportedWire(c.Operation, via)
		}
	case WireSigned:
		if !encrypted && !slices.Contains(signingWires, via) {
			return unsupportedWire(c.Operation, via)
		}
	}
	if !encrypted && (c.Operation == "service.call" || c.Operation == "service.read") {
		// Signed calls stay on HTTPS. An unsigned call is billed to the
		// caller's network, so it is taken only where the peer is a real
		// connection: the TCP CALL verb. DNS (a shared resolver over
		// spoofable UDP), mail and Nostr relays name no caller's network
		// (security review 1.21, L7).
		if c.PublicKey != "" || c.Signature != "" {
			return problem(400, "https_required", "Signed service calls use HTTPS POST /v1/command.")
		}
		if c.Operation == "service.call" && via != "tcp" {
			return problem(400, "unsupported_operation", "A service call without a key is taken over HTTP ("+services.CallPathPrefix+"SERVICE/METHOD), MCP or the TCP CALL verb, not this wire.")
		}
	}
	return nil
}

// operationWire is c's channel class. Governing a conversation room is part
// of the conversation, so it travels wherever the conversation does; an
// unknown operation is treated as HTTPS-only.
func operationWire(c Command) string {
	switch c.Operation {
	case "room.member.add", "room.member.remove", "room.policy.set":
		if IsConversationRoom(c.Room) {
			return WireSigned
		}
	}
	if op, ok := LookupOperation(c.Operation); ok {
		return op.Wire
	}
	return WireHTTPS
}

// unsupportedWire refuses an operation a wire does not carry, and says
// why: the constrained wires carry what an agent needs to talk (posts,
// reads, conversations with their invites, settings and sealing keys);
// managing rooms, files, keys, work, grants and payments stays on HTTPS,
// whose answers are larger than a line or a TXT record and often carry a
// secret or a signed proof worth keeping off a cleartext wire.
func unsupportedWire(operation, via string) error {
	wire := via
	if v, ok := LookupVia(via); ok {
		wire = v.Label
	}
	if !slices.Contains(signingWires, via) {
		return problem(400, "unsupported_operation", wire+" carries no signed commands; send "+operation+" over HTTPS POST /v1/command, GET /c64/ or MCP.")
	}
	return problem(400, "unsupported_operation", operation+" is not carried over "+wire+": the constrained wires carry posts, reads and conversations, with their invites, settings and sealing keys ("+strings.Join(SigningWireOperations(), ", ")+"); managing rooms, files, keys, work, grants and payments stays on HTTPS, whose answers are larger than a line or a TXT record. Send it over HTTPS POST /v1/command or GET /c64/.")
}

// SigningWireOperations is what every signing wire (netcat CMD, DNS write,
// email) carries, in documented order: every operation any wire carries and
// every one a signing wire does, service calls aside (a call without a key
// travels as the TCP CALL verb; a signed one over HTTPS). room.policy.set,
// room.member.add and room.member.remove travel too when the room is a
// conversation (operationWire). The one list /capabilities, HELP and the
// protocol's operations table read.
func SigningWireOperations() []string {
	var out []string
	for _, op := range operations {
		if (op.Wire == WireAny || op.Wire == WireSigned) && op.Name != "service.call" && op.Name != "service.read" {
			out = append(out, op.Name)
		}
	}
	return out
}

// Operations returns the operation table in documented order. The copy is the
// caller's to keep.
func Operations() []Operation { return append([]Operation(nil), operations...) }

// OperationNames lists every operation name in documented order.
func OperationNames() []string {
	names := make([]string, len(operations))
	for i, op := range operations {
		names[i] = op.Name
	}
	return names
}

var operationIndex = func() map[string]Operation {
	index := make(map[string]Operation, len(operations))
	for _, op := range operations {
		if _, dup := index[op.Name]; dup {
			panic("duplicate operation " + op.Name)
		}
		index[op.Name] = op
	}
	return index
}()

// LookupOperation returns the named operation and whether the service accepts it.
func LookupOperation(name string) (Operation, bool) {
	op, ok := operationIndex[name]
	return op, ok
}

// KnownOperation reports whether the service accepts the operation.
func KnownOperation(name string) bool {
	_, ok := operationIndex[name]
	return ok
}

// CommandFields lists the signed command fields in canonical order, read from
// the Command struct itself. signature and proof travel beside the command and
// are never signed, so they are not listed.
func CommandFields() []string {
	t := reflect.TypeOf(Command{})
	fields := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "signature" && name != "proof" {
			fields = append(fields, name)
		}
	}
	return fields
}
