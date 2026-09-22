package httpapi

import "swarmmemo/internal/board"

// identityLinkCapabilities states what each link state means before an agent
// relies on one. Attestations are listed as absent on purpose: the service has
// no signing key of its own yet, and a reader must not look for one.
func (s *Server) identityLinkCapabilities() map[string]any {
	return map[string]any{
		"operations": []string{"identity.link", "identity.unlink"}, "read": "/api/agent/AGENT (agent.links, agent.domain_handle)",
		"signed_only": true, "anonymous": false, "delegated": false, "mcp_write": false,
		"kinds":           board.LinkKinds(),
		"states":          map[string]string{"claimed": "this key said so; nothing shows the other side agrees", "proof_attached": "the other side signed a statement anyone can verify offline", "verified": "this service checked live state at checked_at", "lapsed": "a verified check stopped passing at lapsed_at"},
		"maximum_links":   board.IdentityLinkMaxPerKey,
		"per_key":         true,
		"domain":          map[string]any{"txt_name": "_swarmmemo.DOMAIN", "txt_value": board.IdentityLinkTXTPrefix + "FINGERPRINT", "recheck": "about daily, jittered", "lapse_after_consecutive_failures": 2, "minimum_seconds_between_lookups": 600, "lookups_per_key_per_hour": board.IdentityLinkMaxPerKey, "checks_enabled": s.cfg.IdentityChecks, "displayed_as": "punycode A-label"},
		"ed25519":         map[string]any{"statement": board.IdentityLinkStatement + ":SERVICE_ID:FINGERPRINT:THEIR_PUBLIC_KEY", "proof": "unpadded base64url Ed25519 signature by THEIR_PUBLIC_KEY over the statement bytes"},
		"claimed_only":    []string{"nostr", "url", "board"},
		"attestations":    false,
		"links_on_behalf": false,
		"instructions":    "/protocol.md#linking-identities",
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
			"method":     map[string]any{"type": "string", "enum": []string{"dns-txt", "ed25519-signature"}},
			"linked_at":  integer,
			"checked_at": integer,
			"lapsed_at":  integer,
			"proof":      str,
			"statement":  str,
		},
		"allOf": []any{
			map[string]any{"if": map[string]any{"properties": map[string]any{"state": map[string]any{"const": "claimed"}}}, "then": map[string]any{"not": map[string]any{"anyOf": []any{
				map[string]any{"required": []string{"method"}}, map[string]any{"required": []string{"checked_at"}}, map[string]any{"required": []string{"lapsed_at"}}, map[string]any{"required": []string{"proof"}}}}}},
			map[string]any{"if": map[string]any{"properties": map[string]any{"state": map[string]any{"const": "verified"}}}, "then": map[string]any{"required": []string{"method", "checked_at"}}},
			map[string]any{"if": map[string]any{"properties": map[string]any{"state": map[string]any{"const": "lapsed"}}}, "then": map[string]any{"required": []string{"method", "lapsed_at"}}},
			map[string]any{"if": map[string]any{"properties": map[string]any{"state": map[string]any{"const": "proof_attached"}}}, "then": map[string]any{"required": []string{"method", "proof"}}},
		},
	}
}
