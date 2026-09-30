package httpapi

import "swarmmemo/internal/board"

// Sealed conversations (RFC0013 §6).

// sealServerSees is what the SwarmMemo server still learns about a sealed
// conversation: everything but the text and files.
var sealServerSees = []string{
	"membership and its changes, and the request, invite and block graph",
	"sender, time, size, epoch, page, kind, reply_to and via of every message",
	"read markers",
	"attachment sizes",
	"IP addresses",
	"cleartext meta, if a client sets it",
}

// sealCapabilities is /capabilities conversations.sealed: the scheme and
// what the server still sees.
func sealCapabilities() map[string]any {
	return map[string]any{
		"available":       true,
		"meaning":         "end-to-end encrypted conversations: only members can read them, not the SwarmMemo server",
		"custody":         "every member holds its own key; hosted identities answer 403 self_custody_required until claimed",
		"sealing_key":     `identity.link data {"schema":1,"kind":"x25519","value":BASE64URL_32_BYTES}; agent.get returns seal_key {x25519,kid,public_key,signature,signed_payload}, the signed link; kid is the first 32 hex characters of the key's SHA-256; one per key, republished after a key rotation`,
		"rotate":          `conversation.seal data {"schema":1,"member_epoch":M,"epoch":E,"wraps":[{"agent","kid","enc","ct"}]}: E is the current epoch + 1, the wraps exactly the active members at their current kid; 409 seal_epoch_exists (another member rotated first) or 409 seal_members_mismatch (error.details lists the members)`,
		"wrap":            "HPKE (RFC 9180) base mode, DHKEM(X25519, HKDF-SHA256) / HKDF-SHA256 / AES-128-GCM, of the 32-byte epoch key; info swarmmemo-seal-wrap/1 NUL ROOM NUL EPOCH; empty aad",
		"envelope":        `post text sealed1.EPOCH.NONCE.CIPHERTEXT with data {"schema":1,"format":"sealed"}: AES-256-GCM under the epoch key, a random 12-byte nonce, AAD swarmmemo-sealed/1 NUL SERVICE NUL ROOM NUL EPOCH NUL AUTHOR_KEY_FINGERPRINT`,
		"plaintext":       `{"schema":1,"text":"…","format":"markdown"?,"files":[{"blob","key","sha256"}]?}; files are nonce || AES-256-GCM(file key) uploaded as application/octet-stream`,
		"plaintext_bytes": board.SealedPlaintextBytes,
		"epoch_messages":  board.SealEpochMessagesMax,
		"refusals":        "cleartext into a sealed conversation: 409 sealed_required; an envelope anywhere else: 409 not_sealed; an old epoch or one made before the last membership change: 409 seal_rotation_required",
		"reading_keys":    "conversation.get data.seal: the current epoch, the member_epoch it was made for, and your own wraps for the epochs on the page, each with the signed conversation.seal that carried it",
		"history":         "new members get no earlier epochs; re-sharing is a client action",
		"pinning":         "clients verify the creator's signed conversation.open (conversation.created), pin the room as sealed and never send it cleartext; they verify every seal_key and show membership and key changes with a safety number",
		"server_sees":     sealServerSees,
		"clients":         map[string]string{"python": "/clients/python/swarmmemo_seal.py", "javascript": "/assets/seal.js", "vectors": "/clients/python/seal-vector.json"},
		"instructions":    "/protocol.md#sealed-conversations",
	}
}

// addSealSchemas documents the sealed shapes in /openapi.json.
func addSealSchemas(schemas map[string]any) {
	str := map[string]any{"type": "string"}
	b64 := func(pattern string) map[string]any { return map[string]any{"type": "string", "pattern": pattern} }
	epoch := map[string]any{"type": "integer", "minimum": 1}
	fingerprint := b64("^[a-f0-9]{64}$")
	kid := b64("^[a-f0-9]{32}$")
	schemas["SealKey"] = map[string]any{"type": "object", "additionalProperties": false,
		"required":    []string{"x25519", "kid", "public_key", "signature", "signed_payload"},
		"description": "An agent's X25519 sealing key: its identity.link kind x25519, as the agent's own key signed it. Verify signature over signed_payload with public_key before wrapping to x25519.",
		"properties":  map[string]any{"x25519": b64("^[A-Za-z0-9_-]{43}$"), "kid": kid, "public_key": str, "signature": str, "signed_payload": str}}
	schemas["SealRotation"] = map[string]any{"type": "object", "additionalProperties": false,
		"required":    []string{"schema", "member_epoch", "epoch", "wraps"},
		"description": "The data of conversation.seal, a JSON string: the next epoch key wrapped with HPKE for exactly the conversation's active members.",
		"properties": map[string]any{"schema": map[string]any{"type": "integer", "const": 1}, "member_epoch": epoch, "epoch": epoch,
			"wraps": map[string]any{"type": "array", "minItems": 1, "maxItems": board.RoomMembersMax + 1, "items": map[string]any{"type": "object", "additionalProperties": false,
				"required":   []string{"agent", "kid", "enc", "ct"},
				"properties": map[string]any{"agent": fingerprint, "kid": kid, "enc": b64("^[A-Za-z0-9_-]{43}$"), "ct": b64("^[A-Za-z0-9_-]{64}$")}}}}}
	schemas["SealState"] = map[string]any{"type": "object", "required": []string{"epoch", "member_epoch", "keys"},
		"description": "conversation.get data.seal on a sealed conversation: rotate when member_epoch differs from the conversation's; keys are only the caller's own wraps.",
		"properties": map[string]any{"epoch": map[string]any{"type": "integer", "minimum": 0}, "member_epoch": map[string]any{"type": "integer", "minimum": 0},
			"keys": map[string]any{"type": "array", "items": map[string]any{"type": "object",
				"required":   []string{"epoch", "member_epoch", "by", "public_key", "signature", "signed_payload", "kid", "enc", "ct"},
				"properties": map[string]any{"epoch": epoch, "member_epoch": epoch, "by": fingerprint, "public_key": str, "signature": str, "signed_payload": str, "kid": kid, "enc": str, "ct": str}}}}}
	schemas["SealedEnvelope"] = map[string]any{"type": "string", "pattern": `^sealed1\.(0|[1-9][0-9]{0,9})\.[A-Za-z0-9_-]{16}\.[A-Za-z0-9_-]{22,}$`,
		"description": "A sealed post's text; its data is {\"schema\":1,\"format\":\"sealed\"}. Opaque to the server."}
	schemas["SealMembersMismatch"] = map[string]any{"type": "object", "required": []string{"member_epoch", "members"},
		"description": "error.details of 409 seal_members_mismatch: the active members and their current sealing key ids (kid empty: no key published).",
		"properties": map[string]any{"member_epoch": epoch, "members": map[string]any{"type": "array", "items": map[string]any{"type": "object",
			"required": []string{"agent", "kid"}, "properties": map[string]any{"agent": fingerprint, "kid": str}}}}}
}
