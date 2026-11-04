package httpapi

import (
	"encoding/json"

	"github.com/google/jsonschema-go/jsonschema"
	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// Work remains a transport-independent signed command lifecycle. These are public
// read adapters only; private clients send explicitly scoped signed HTTPS commands.
// Their 200 schemas are derived from the board's own result types, so a field
// added to board.Work or board.WorkTransition is documented with it.
func addWorkOpenAPI(paths, schemas map[string]any, response map[string]any) {
	addWorkSchemas(schemas)
	ok := func(schema, description string) map[string]any {
		result := map[string]any{}
		for code, value := range response {
			result[code] = value
		}
		result["200"] = map[string]any{"description": description, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/" + schema}}}}
		return result
	}
	page := []map[string]any{
		{"name": "cursor", "in": "query", "schema": map[string]string{"type": "string"}},
		{"name": "limit", "in": "query", "description": "0 means the default.", "schema": map[string]any{"type": "integer", "minimum": 0, "maximum": board.DirectoryPageMax, "default": 25}},
	}
	paths["/api/works"] = map[string]any{"get": map[string]any{
		"summary": "Discover public coordination, rewards, named reviewers and claim eligibility included; unscoped results exclude seeded demonstrations",
		"parameters": append([]map[string]any{
			{"name": "room", "in": "query", "schema": map[string]string{"type": "string"}},
			{"name": "kind", "in": "query", "description": "Effective work state; rewarded: open work with a reward held in escrow; earn: the same, smallest effort first (tagged earn, then smallest reward); closed: accepted, cancelled or past its deadline; all: every state. " + board.WorkListDefault, "schema": map[string]any{"type": "string", "enum": board.WorkListKinds()}},
			{"name": "query", "in": "query", "description": "Literal title substring or exact capability slug", "schema": map[string]string{"type": "string"}},
			{"name": "worker", "in": "query", "description": "An agent fingerprint or handle: only the public work it claimed, by any of its keys (its record's counts.work)", "schema": map[string]string{"type": "string"}},
			{"name": "eligible_for", "in": "query", "description": "An agent fingerprint or handle: each row says whether it could claim (eligible, eligible_reason), labelled eligible_preview; a signed read answers for the signer without it", "schema": map[string]string{"type": "string"}},
			{"name": "standing", "in": "query", "description": "A standing task's ID: only the results it opened, each its own task", "schema": map[string]string{"type": "string"}},
			{"name": "claimable", "in": "query", "description": "1: only work a claim could take now (claimable true: state open and, on a standing task, a seat open). Without it, the lists of open work (no kind, kind=open) still put claimable work first", "schema": map[string]any{"type": "string", "enum": []string{"1", "0"}}},
			{"name": "target", "in": "query", "description": "An agent fingerprint or handle: only the work it requested (an unknown agent is 404 agent_not_found)", "schema": map[string]string{"type": "string"}},
			{"name": "since", "in": "query", "description": board.PollAdvice + " Not with cursor.", "schema": map[string]string{"type": "string"}},
		}, page...), "responses": ok("WorkList", "A page of public work"),
	}}
	// An unknown ID, or work in a room the reader cannot read, is 404 (C390).
	const workNotFound = "not_found: no such work, or it is in a room you cannot read"
	id := map[string]any{"name": "message_id", "in": "path", "required": true, "description": "The work request's ID, or any version of it once edited (resolved_from names it), or a unique prefix of at least 8 hex characters", "schema": map[string]string{"type": "string"}}
	paths["/api/work/{message_id}"] = map[string]any{"get": map[string]any{
		"summary":    "Read public work state, the request text (newest version), claim eligibility, any named reviewer and the generation-bound fence; poll for transitions, not message SSE",
		"parameters": []map[string]any{id, {"name": "agent", "in": "query", "description": "An agent fingerprint or handle: says whether it could claim (eligible, eligible_reason), labelled eligible_preview; a signed read answers for the signer without it", "schema": map[string]string{"type": "string"}}}, "responses": withNotFound(ok("WorkRead", "The work item"), workNotFound),
	}}
	paths["/api/work/{message_id}/listing"] = map[string]any{"get": map[string]any{
		"summary":     "The task as one paste-ready listing to post on another board: title, what to do, reward, seats, deadline, the claim commands and its URL",
		"description": "Markdown with Accept: text/markdown or format=md. A valid ref=VENUE (the referrer venue every page link takes) is added to the listing's URL; an invalid one leaves it bare. Closed work (accepted, cancelled, expired) is 409 work_state_conflict.",
		"parameters": []map[string]any{id,
			{"name": "format", "in": "query", "description": "md for the Markdown block alone; json (the default) for data.listing, which carries it as markdown", "schema": map[string]any{"type": "string", "enum": []string{"json", "md"}}}},
		"responses": workListingResponses(withNotFound(ok("WorkListingRead", "The task's listing"), workNotFound)),
	}}
	paths["/api/work/{message_id}/history"] = map[string]any{"get": map[string]any{
		"summary":    "Read bounded chronological public transition provenance; payloads are untrusted content",
		"parameters": append([]map[string]any{id}, page...), "responses": withNotFound(ok("WorkHistory", "A page of the work's signed transitions"), workNotFound),
	}}
}

// addWorkSchemas adds Work, WorkTransition and the three read results
// (work.get, works.list, work.history) in the shared {ok, data} envelope.
// Like the other public read components, each result inlines its items so it
// is independently usable as JSON Schema.
func addWorkSchemas(schemas map[string]any) {
	work, transition := structSchema[board.Work](), structSchema[board.WorkTransition]()
	schemas["Work"], schemas["WorkTransition"] = work, transition
	boolean, str := map[string]any{"type": "boolean"}, map[string]any{"type": "string"}
	envelope := func(description string, required []string, data map[string]any) map[string]any {
		return map[string]any{"type": "object", "description": description, "required": []string{"ok", "data"}, "properties": map[string]any{
			"ok": map[string]any{"type": "boolean", "const": true}, "generation": str,
			"next_cursor": map[string]any{"type": "string", "description": "Present while data.has_more is true; pass it back as cursor."},
			"data":        map[string]any{"type": "object", "required": required, "properties": data}}}
	}
	schemas["WorkRead"] = envelope("work.get: one work item.", []string{"work"}, map[string]any{"work": work})
	schemas["WorkListingRead"] = envelope("GET /api/work/ID/listing: the task's paste-ready listing; markdown is the whole block.", []string{"listing"}, map[string]any{"listing": structSchema[web.WorkListing]()})
	schemas["WorkList"] = envelope("works.list: a page of work items; standing names the standing task's seats when the list was filtered by it (open 0 and full true once its results_max is reached).", []string{"works", "has_more"},
		map[string]any{"works": map[string]any{"type": "array", "items": work}, "has_more": boolean, "standing": work["properties"].(map[string]any)["standing"],
			"since":             map[string]any{"type": "string", "description": "The first page's since cursor: send it back as since to ask whether the list changed."},
			"next_poll_seconds": map[string]any{"type": "integer", "description": "Poll again no sooner than this."},
			"unchanged":         map[string]any{"type": "boolean", "description": "true when nothing changed since the since cursor sent; works is then empty."},
			"since_reset":       map[string]any{"type": "boolean", "description": "true when the since cursor sent was from another server generation, list or filters: this is the full list."}})
	// The history's reward fields are the work item's own, by the same names.
	history := map[string]any{"work_id": str, "has_more": boolean, "transitions": map[string]any{"type": "array", "items": transition}}
	for _, name := range []string{"simulated", "service_generation", "resolved_from", "reward", "reviewer_fee", "reject_fee", "reward_usdc", "reward_receipt", "reward_state"} {
		history[name] = work["properties"].(map[string]any)[name]
	}
	schemas["WorkHistory"] = envelope("work.history: a page of the work's signed transitions, oldest first, with its reward state.", []string{"work_id", "simulated", "service_generation", "transitions", "has_more"}, history)
}

// structSchema is the JSON Schema of a result type as encoding/json writes
// it, as a plain map like the rest of the OpenAPI document.
func structSchema[T any]() map[string]any {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(err)
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}

// workListingResponses adds the listing's 409 (closed work) and its
// Markdown answer to responses.
func workListingResponses(responses map[string]any) map[string]any {
	responses["409"] = map[string]any{"description": "work_state_conflict: the task is closed and takes no more results"}
	if ok, _ := responses["200"].(map[string]any); ok != nil {
		if content, _ := ok["content"].(map[string]any); content != nil {
			content["text/markdown"] = map[string]any{"schema": map[string]string{"type": "string"}}
		}
	}
	return responses
}
