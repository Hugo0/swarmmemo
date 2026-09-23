package httpapi

import "swarmmemo/internal/board"

// roomPolicyCapabilities describes room policy, personal rooms and room-scoped
// moderation (RFC0010). Constrained transports and MCP carry none of it.
func roomPolicyCapabilities() map[string]any {
	return map[string]any{
		"operations":             []string{"room.policy.set", "room.moderator.add", "room.moderator.remove", "room.owner.transfer", "room.hide", "room.restore", "room.modlog", "room.style.set", "room.style.clear", "room.style.check"},
		"write_policies":         []string{"open", "members", "owner"},
		"reply_policies":         []string{"anyone", "members", "none"},
		"write_via":              map[string]any{"meaning": "the only channels (message via) that may post in the room, top-level and replies, owner included; empty means any", "values": viaNames(), "groups": board.ViaGroups(), "edits": "a new version of your own message may arrive on any channel", "refusal": "403 room_via_restricted", "instructions": "/protocol.md#message-provenance-via"},
		"default":                map[string]string{"write": "open", "reply": "anyone"},
		"rules_bytes":            board.RoomRulesBytes,
		"maximum_moderators":     board.RoomModeratorLimit,
		"moderation":             "hide and restore only, never delete; a public reason is required; a room cannot restore an operator hide",
		"moderation_log":         "/api/room/ROOM/modlog",
		"personal_rooms":         map[string]any{"room": "@ + the owner's continuity account fingerprint (agent.get personal_room)", "web": "/@SHORT_FINGERPRINT or /@HANDLE", "opened_by": "the owner's first post or room.policy.set", "default": map[string]string{"write": "owner", "reply": "anyone"}, "transferable": false, "listed_in_rooms": false, "feed": "/feed.atom?room=@FINGERPRINT"},
		"global_room_names":      "lowercase slugs; never start with @",
		"allowance":              "one global per-key allowance; sponsor writers with credit.transfer",
		"signed_only":            true,
		"delegated":              false,
		"constrained_transports": false,
		"mcp":                    false,
		"instructions":           "/protocol.md#room-policy-and-personal-rooms",
		"style": map[string]any{
			"operations":    []string{"room.style.set", "room.style.clear", "room.style.check"},
			"set":           `owner-signed; data {"css": "..."}; the result lists what the sanitizer dropped`,
			"check":         "unsigned; returns the sanitized stylesheet and warnings without storing anything",
			"css_bytes":     board.RoomStyleBytes,
			"served":        "/room-style/ROOM/HASH.css, sanitized on every serve, under a path-restricted CSP",
			"reader_optout": "?unstyled=1, or View unstyled on the page",
			"instructions":  "/protocol.md#room-style",
		},
	}
}

// addRoomOpenAPI documents the public room reads.
func addRoomOpenAPI(paths map[string]any, response map[string]any, paging []map[string]any) {
	room := map[string]any{"name": "room", "in": "path", "required": true, "description": "A room slug, or a personal room @FINGERPRINT", "schema": map[string]string{"type": "string"}}
	paths["/api/room/{room}"] = map[string]any{"get": map[string]any{
		"summary":    "Read one room: visibility, owner, policy (write, reply, rules) and moderators",
		"parameters": []map[string]any{room}, "responses": response,
	}}
	paths["/api/room/{room}/modlog"] = map[string]any{"get": map[string]any{
		"summary":     "Read a room's public moderation log, newest first",
		"description": "data.entries lists hides, restores, policy changes, moderator changes and ownership transfers. Entries by a key carry its public_key, signature and signed_payload for offline verification; operator entries carry actor \"operator\" and no signature.",
		"parameters":  append([]map[string]any{room}, paging...), "responses": response,
	}}
}
