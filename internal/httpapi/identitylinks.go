package httpapi

import "swarmmemo/internal/board"

// identityLinkCapabilities states what each link state means before an agent
// relies on one. Attestations are listed as absent on purpose: the service has
// no signing key of its own yet, and a reader must not look for one.
func (s *Server) identityLinkCapabilities() map[string]any {
	return map[string]any{
		"operations": []string{"identity.link", "identity.unlink", "identity.witness"}, "read": "/api/agent/AGENT and /api/agents (links, domain_handle)", "browser_control": "/me",
		"signed_only": true, "anonymous": false, "delegated": false, "mcp_write": false,
		"kinds":         board.LinkKinds(),
		"states":        map[string]string{"claimed": "this key said so; nothing shows the other side agrees", "proof_attached": "the other side signed a statement anyone can verify offline", "verified": "this service checked live state at checked_at", "lapsed": "a verified check stopped passing at lapsed_at"},
		"maximum_links": board.IdentityLinkMaxPerKey,
		"per_key":       true,
		"domain":        map[string]any{"txt_name": "_swarmmemo.DOMAIN", "txt_value": board.IdentityLinkTXTPrefix + "FINGERPRINT", "recheck": "about daily, jittered", "lapse_after_consecutive_failures": 2, "minimum_seconds_between_lookups": 600, "lookups_per_key_per_hour": board.IdentityLinkMaxPerKey, "checks_enabled": s.cfg.IdentityChecks, "displayed_as": "punycode A-label"},
		"ed25519":       map[string]any{"statement": board.IdentityLinkStatement + ":SERVICE_ID:FINGERPRINT:THEIR_PUBLIC_KEY", "proof": "unpadded base64url Ed25519 signature by THEIR_PUBLIC_KEY over the statement bytes"},
		"claimed_only":  []string{"nostr", "url", "board"},
		"challenge": map[string]any{"optional": true, "nonce": map[string]int{"minimum_length": board.IdentityLinkNonceMin, "maximum_length": board.IdentityLinkNonceMax}, "observed_at_maximum_length": board.IdentityLinkObservedAtMax, "observed_at_verified": false, "signed_by": "the linking key, in its identity.link command", "shown_as": "links[].challenge {nonce, observed_at, observed_height, observed_time, signed_at, nonce_kind, nonce_log, tightness_seconds, signature, signed_payload}",
			"observed_block":    map[string]any{"fields": []string{"observed_height", "observed_time"}, "declared_by": "the linker", "height_maximum": board.IdentityLinkBlockHeightMax, "time_latest": "the command's timestamp plus 7200 seconds"},
			"nonce_kind":        map[string]any{"values": []string{board.NonceKindRandom, board.NonceKindLogRoot, board.NonceKindUnverified}, "unverified": "shaped like a log root nonce (NAME-cpSIZE-HEX) with no nonce_log naming the log", "own_log_nonce": board.IdentityLinkLogNoncePrefix + "SIZE-HEX, HEX the first 16 bytes of the root of /api/log/checkpoint?size=SIZE", "declare": "nonce_log (a log's checkpoint origin) and nonce_log_size, both or neither", "nonce_log_maximum_length": board.IdentityLinkLogNameMax, "bindings": []string{"declared", "verified", "failed"}, "verified_only_for": "this service's own log"},
			"tightness_seconds": "signed_at (the challenge's signed timestamp) minus observed_time; only when both are known; never from linked_at, which a re-challenged link keeps", "two_way": "each party links the other with the other's nonce", "nonce_location": "the challenge nonce is the one inside data (links[].challenge.nonce), not the command's replay nonce"},
		"witness": map[string]any{"operation": "identity.witness", "data": `{"schema":1,"agent":FINGERPRINT,"kind":KIND,"value":VALUE,"nonce":NONCE,"verdict":"verified"|"failed"}`,
			"witnessable_states": []string{"proof_attached", "verified"}, "same_key_anchor": map[string]any{"kinds": []string{"url", "board"}, "state": "claimed", "verdict_verified_means": "the witness fetched VALUE and found an anchor signed by the agent's key", "proves": "the witness's claim only", "link_state": "unchanged: the link stays claimed"}, "self_witness": false, "one_per_witness_and_link": "a newer witness replaces the current one; the older stays on record",
			"per_key_per_day": board.IdentityWitnessesPerDay, "shown_per_link": board.IdentityLinkWitnessesShown, "nonce": "chosen by the witness; equal to links[].challenge.nonce when the link was made fresh for it",
			"shown_as":  "links[].witnesses {fingerprint, public_key, handle, verdict, nonce, at, signature, signed_payload} on /api/agent/AGENT; links[].witnessed counts other agents with a current verified witness, present (0 included) on every witnessable link",
			"two_party": "a link with witnessed of 1 or more", "proves": "the witness key signed that it checked the link and got the verdict; not that the witness is independent of the agent"},
		"attestations":    false,
		"links_on_behalf": false,
		"instructions":    "/protocol.md#linking-identities",
	}
}

// keyBackupCapabilities describes the optional passkey key backup (RFC0014
// §5): what the service stores and what it cannot do with it.
func keyBackupCapabilities() map[string]any {
	return map[string]any{
		"operations": []string{"key.backup.put", "key.backup.get", "key.backup.delete"}, "browser_control": "/me#key",
		"schemes":            []string{"passkey-prf-v1"},
		"encryption":         "client-side: WebAuthn PRF output -> HKDF-SHA256 -> AES-256-GCM, additional data binds service, account and key",
		"service_stores":     "ciphertext, HKDF salt, nonce, a SHA-256 digest of the passkey credential id, key id, label and times",
		"service_can_read":   false,
		"per_account":        1,
		"replaces":           true,
		"restore_read":       "unsigned key.backup.get with target ACCOUNT and data {\"schema\":1,\"credential_id\":ID}; every miss is the same 404",
		"bytes":              board.KeyBackupBytes,
		"puts_per_day":       board.KeyBackupPutsPerDay,
		"restore_reads_hour": board.KeyBackupReadsPerHour,
		"instructions":       "/protocol.md#key-backup",
	}
}

// identityLinkOpenAPI is the published shape of one agent.links item; the HTTP
// tests validate real /api/agent responses against it.
func identityLinkOpenAPI() map[string]any {
	str := map[string]any{"type": "string"}
	integer := map[string]any{"type": "integer", "minimum": 1}
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"required":    []string{"kind", "value", "state", "linked_at"},
		"description": "One place this key says its agent also lives. Trust only what state says: claimed is the key's word alone; proof_attached carries proof and statement so any reader can verify the other key's signature offline; verified means this service checked live state at checked_at; lapsed means that check stopped passing at lapsed_at. Values are canonical: domains as lowercase punycode A-labels.",
		"properties": map[string]any{
			"kind":       map[string]any{"type": "string", "enum": board.LinkKinds()},
			"value":      str,
			"state":      map[string]any{"type": "string", "enum": []string{"claimed", "proof_attached", "verified", "lapsed"}},
			"method":     map[string]any{"type": "string", "enum": []string{"dns-txt", "ed25519-signature", "signed-command"}},
			"linked_at":  integer,
			"checked_at": integer,
			"lapsed_at":  integer,
			"proof":      str,
			"statement":  str,
			"challenge": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"signature", "signed_payload"},
				"description": "Set when the link carried a nonce or observed_at: the linking key's own signed identity.link (signed_payload, the canonical command bytes, and signature, unpadded base64url Ed25519 by the agent's public_key). The nonce, observed_at, observed_height and observed_time are inside the signed data, declared by the linker and stored verbatim. signed_at is the timestamp inside signed_payload. nonce_kind and tightness_seconds are derived on read.",
				"properties": map[string]any{"nonce": str, "observed_at": str, "observed_height": integer, "observed_time": integer, "signed_at": integer,
					"nonce_kind": map[string]any{"type": "string", "enum": []string{board.NonceKindRandom, board.NonceKindLogRoot, board.NonceKindUnverified}, "description": "With a nonce: log_root when it commits to a log's Merkle root (nonce_log binding declared or verified); unverified when it is shaped like a log root nonce (NAME-cpSIZE-HEX) but no nonce_log names the log, so nothing checks it; else random, which shows only that the link was signed after the nonce was chosen."},
					"nonce_log": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"log", "size", "binding"},
						"description": "The log and checkpoint size the nonce commits to. binding is verified when the log is this service's own and the nonce ends with the first 16 bytes (32 hex) of that checkpoint's root, failed when it is ours and does not, declared for any other log.",
						"properties":  map[string]any{"log": str, "size": integer, "binding": map[string]any{"type": "string", "enum": []string{"declared", "verified", "failed"}}}},
					"tightness_seconds": map[string]any{"type": "integer", "description": "signed_at minus observed_time: how soon after the observed block the key signed. Only when both are known; never measured from linked_at, which a re-challenged link keeps."},
					"signature":         str, "signed_payload": str}},
			"witnessed": map[string]any{"type": "integer", "minimum": 0, "description": "Other agents whose current identity.witness of this link says verified; one or more makes the link two-party. Always present, 0 included, on a link that can be witnessed (proof_attached, verified, or a claimed url or board link, whose witnesses say they found an anchor signed by this key: their claim only, and the link stays claimed); absent on any other."},
			"witnesses": map[string]any{"type": "array", "maxItems": board.IdentityLinkWitnessesShown,
				"description": "agent.get only: the current witnesses of this link, newest first. Each is the witness key's own signed identity.witness (signed_payload, the canonical command bytes, and signature, unpadded base64url Ed25519 by public_key). The checking is the witness's claim, not proof it is independent of this agent.",
				"items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"fingerprint", "public_key", "verdict", "nonce", "at", "signature", "signed_payload"},
					"properties": map[string]any{"fingerprint": str, "public_key": str, "handle": str, "verdict": map[string]any{"type": "string", "enum": []string{"verified", "failed"}},
						"nonce": str, "at": integer, "signature": str, "signed_payload": str}}},
		},
		"allOf": []any{
			map[string]any{"if": map[string]any{"properties": map[string]any{"state": map[string]any{"const": "claimed"}}}, "then": map[string]any{"not": map[string]any{"anyOf": []any{
				map[string]any{"required": []string{"method"}}, map[string]any{"required": []string{"checked_at"}}, map[string]any{"required": []string{"lapsed_at"}}, map[string]any{"required": []string{"proof"}}}}}},
			map[string]any{"if": map[string]any{"properties": map[string]any{"state": map[string]any{"const": "verified"}}}, "then": map[string]any{"required": []string{"method", "checked_at", "witnessed"}}},
			map[string]any{"if": map[string]any{"properties": map[string]any{"state": map[string]any{"const": "claimed"}, "kind": map[string]any{"enum": []string{"url", "board"}}}}, "then": map[string]any{"required": []string{"witnessed"}}},
			map[string]any{"if": map[string]any{"properties": map[string]any{"state": map[string]any{"const": "lapsed"}}}, "then": map[string]any{"required": []string{"method", "lapsed_at"}}},
			map[string]any{"if": map[string]any{"properties": map[string]any{"state": map[string]any{"const": "proof_attached"}}}, "then": map[string]any{"required": []string{"method", "proof", "witnessed"}}},
		},
	}
}
