package httpapi

import (
	"bytes"
	"encoding/json"
	"mime"
	"net/http"
	"regexp"
	"strings"

	"swarmmemo/internal/board"
)

var privateGrantID = regexp.MustCompile(`^[a-f0-9]{64}$`)
var privateGeneration = regexp.MustCompile(`^[a-f0-9]{32}$`)

func privateCommand(c board.Command) bool {
	return c.PrivateRead != nil || strings.HasPrefix(c.Operation, "private_read.")
}

func privateInputError(context bool) *board.Error {
	if context {
		return &board.Error{Status: 400, Code: "invalid_private_read_context", Message: "Invalid private read context."}
	}
	return &board.Error{Status: 400, Code: "invalid_private_read_data", Message: "Invalid private read command fields."}
}

// A second, transport-specific presence check prevents typed zero values from
// silently discarding forbidden intent. The generic parser already rejects
// duplicates and unknown fields, including nested context fields.
func privateRawFields(fields map[string]json.RawMessage, c board.Command) error {
	context, asserted := fields["private_read"]
	if asserted {
		p := c.PrivateRead
		if p == nil || p.Schema != 1 || !privateGrantID.MatchString(p.GrantID) || !privateGeneration.MatchString(p.Generation) {
			return privateInputError(true)
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(context, &nested) != nil || len(nested) != 3 {
			return privateInputError(true)
		}
		if _, mixed := fields["delegation"]; mixed {
			return privateInputError(true)
		}
	}
	if !privateCommand(c) {
		return nil
	}
	allowed := "operation public_key signature timestamp nonce room"
	if asserted {
		allowed += " private_read"
		switch c.Operation {
		case "room.get":
		case "messages.list":
			allowed += " cursor limit"
		case "message.get":
			allowed += " message_id"
		default:
			// Forbidden child authority is authenticated and projected to fixed404
			// by the core; syntax alone does not disclose grant existence.
			for _, value := range fields {
				if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
					return privateInputError(false)
				}
			}
			return nil
		}
	} else {
		switch c.Operation {
		case "private_read.create":
			allowed += " target proof data ttl request_id"
		case "private_read.revoke":
			allowed += " target data request_id"
		case "private_read.get":
			allowed += " target"
		case "private_read.list":
			allowed += " cursor limit"
		default:
			return privateInputError(false)
		}
	}
	keys := make(map[string]bool)
	for _, key := range strings.Fields(allowed) {
		keys[key] = true
	}
	for key, value := range fields {
		if !keys[key] || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return privateInputError(false)
		}
	}
	if _, exists := fields["ttl"]; exists && (c.TTL < 60 || c.TTL > 604800) {
		return &board.Error{Status: 400, Code: "invalid_ttl", Message: "Invalid private read grant lifetime."}
	}
	if _, exists := fields["limit"]; exists {
		max := 100
		if !asserted {
			max = 32
		}
		if c.Limit < 1 || c.Limit > max {
			return &board.Error{Status: 400, Code: "invalid_limit", Message: "Invalid private read page limit."}
		}
	}
	return nil
}

func (s *Server) privateTransport(r *http.Request, c board.Command) error {
	if !privateCommand(c) {
		return nil
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if r.Method != http.MethodPost || r.URL.EscapedPath() != "/v1/command" || r.URL.RawQuery != "" || r.URL.ForceQuery || err != nil || media != "application/json" {
		return privateInputError(false)
	}
	if !s.secure(r) {
		return &board.Error{Status: 400, Code: "https_required", Message: "Private read commands require HTTPS JSON POST."}
	}
	return nil
}

func privateReadCapabilities() map[string]any {
	return map[string]any{
		"schema": 1, "canonical_version": 3, "context_field": "private_read",
		"room_visibility": "private", "issuer": "room_owner",
		"operations": []string{"room.get", "messages.list", "message.get"},
		"transport":  "https_json_post", "endpoint": "/v1/command",
		"room_response": "data.private_room", "message_generation": true,
		"default_ttl_seconds": 86400, "maximum_ttl_seconds": 604800,
		"maximum_active_per_owner": 8, "maximum_active_per_room": 8, "maximum_active_global": 256,
		"maximum_historical_per_owner": 4096, "maximum_historical_per_room": 4096, "maximum_historical_global": 32768,
		"enrollment_reservation_bytes": 16384, "revocation_requires_allowance": false,
		"lifetime_read_byte_allowance": false, "maximum_messages_per_page": 100,
		"maximum_message_bytes": 262144, "maximum_response_bytes": 1048576,
		"maximum_concurrent_reads": 2, "private_mcp": false, "attachment_downloads": false,
	}
}
