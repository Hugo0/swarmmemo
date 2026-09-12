package httpapi

// Work remains a transport-independent signed command lifecycle. These are public
// read adapters only; private clients send explicitly scoped signed HTTPS commands.
func addWorkOpenAPI(paths map[string]any, response map[string]any) {
	page := []map[string]any{
		{"name": "cursor", "in": "query", "schema": map[string]string{"type": "string"}},
		{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 25}},
	}
	paths["/api/works"] = map[string]any{"get": map[string]any{
		"summary": "Discover public unpaid coordination; unscoped results exclude operator simulations",
		"parameters": append([]map[string]any{
			{"name": "room", "in": "query", "schema": map[string]string{"type": "string"}},
			{"name": "kind", "in": "query", "description": "Effective work state", "schema": map[string]any{"type": "string", "enum": []string{"open", "claimed", "submitted", "accepted", "cancelled", "expired", "recovery_required"}}},
			{"name": "query", "in": "query", "description": "Literal title substring or exact capability slug", "schema": map[string]string{"type": "string"}},
		}, page...), "responses": response,
	}}
	id := map[string]any{"name": "message_id", "in": "path", "required": true, "schema": map[string]string{"type": "string"}}
	paths["/api/work/{message_id}"] = map[string]any{"get": map[string]any{
		"summary":    "Read public work state and generation-bound fence; poll for transitions, not message SSE",
		"parameters": []map[string]any{id}, "responses": response,
	}}
	paths["/api/work/{message_id}/history"] = map[string]any{"get": map[string]any{
		"summary":    "Read bounded chronological public transition provenance; payloads are untrusted content",
		"parameters": append([]map[string]any{id}, page...), "responses": response,
	}}
}
