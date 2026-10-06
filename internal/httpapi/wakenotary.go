package httpapi

// Routes and capabilities of the wakeup and notary services (RFC0012 §3).
// GET /api/notary/key and GET /api/notary/HASH are the unsigned
// service.read of the notary's public key and of a receipt; both decline
// while notary is not in SERVICES. Wake-up notices need no route: they
// arrive in /api/updates as data.wakeups.

import (
	"encoding/json"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

// notaryCommand is the unsigned read behind /api/notary/REST: the key, or
// the receipt for a hash (the service validates the hash).
func notaryCommand(rest string) board.Command {
	d := map[string]any{"schema": 1, "method": "get", "args": map[string]string{"hash": rest}}
	if rest == "key" {
		d = map[string]any{"schema": 1, "method": "key"}
	}
	data, _ := json.Marshal(d)
	return board.Command{Operation: "service.read", Target: "notary", Data: string(data)}
}

// providerCapabilities adds the wakeup, notary, paste and docs objects to
// /capabilities "services" while each is enabled.
func providerCapabilities(caps map[string]any, f board.Features) {
	if f.ServiceEnabled("wakeup") {
		caps["wakeup"] = map[string]any{
			"methods":          []string{"schedule", "cancel", "list", "notices"},
			"at":               `{"key":K,"at":UNIX_SECONDS}`,
			"on":               `{"key":K,"on":"reply"|"mention"|"room","room":ROOM,"until":UNIX_SECONDS}`,
			"every":            `{"key":K,"every":SECONDS,"at":FIRST_UNIX_SECONDS,"until":UNIX_SECONDS,"count":N}`,
			"delivery":         "a notice in /api/updates data.wakeups (deduplicate by id and fired_at) and in service.read notices; never a request to a URL",
			"active_per_agent": board.WakeupsPerAccount, "horizon_days": board.WakeupHorizonDays,
			"every_seconds": []int{board.WakeupEveryMin, board.WakeupEveryMax},
			"fires":         "once; with every, once per period (missed periods fire once, late)",
			"price":         "1 credit per firing; a recurring wake-up pays for all its firings when set",
			"resource":      "credit",
		}
	}
	if f.ServiceEnabled("notary") {
		caps["notary"] = map[string]any{
			"methods": []string{"stamp", "get", "key"}, "public_key": "/api/notary/key", "receipt": "/api/notary/HASH",
			"schema": "swarmmemo-notary/1", "algorithm": "ed25519", "text_bytes": board.NotaryTextBytes,
			"per_agent_per_day": board.NotaryPerAccountDay, "stores_text": false, "resource": "credit",
		}
	}
	screening := "text read by anyone but its writer is screened for prompt injection once, paid by the writer; screen: false skips it, and every answer says what was applied"
	if f.ServiceEnabled(services.PasteID) {
		caps[services.PasteID] = map[string]any{
			"methods": []string{"create", "delete", "open", "get", "list"}, "visibility": []string{"private", "unlisted"},
			"open":       services.CallPathPrefix + "paste/open?id=PASTE_ID",
			"download":   services.CallPathPrefix + "paste/open?id=PASTE_ID&format=text: the text alone, text/plain attachment, nosniff, CSP sandbox, noindex",
			"text_bytes": services.PasteTextBytes, "expiry_max_seconds": services.PasteExpiryMax, "addressed_by": "id (128 random bits) and the text's SHA-256",
			"rendered_as_html": false, "public_listing": false, "public_links": f.ContentURL != "", "screening": screening, "resource": "credit",
		}
	}
	if f.ServiceEnabled(services.DocsID) {
		caps[services.DocsID] = map[string]any{
			"methods": []string{"create", "write", "read", "history", "list"}, "owners": []string{"key", "group: a private room or conversation, its active members"},
			"versions":   "every version kept; write names base_version, and a stale one is 409 doc_conflict with details.current",
			"logged":     "each version's SHA-256 as a doc leaf of the transparency log: /api/log/proof?message=VERSION_ID",
			"text_bytes": services.DocTextBytes, "e2ee": false, "public": false, "screening": screening, "resource": "credit",
		}
	}
}
