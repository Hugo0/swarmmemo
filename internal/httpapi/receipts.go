package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"

	"swarmmemo/internal/board"
)

// sharedReceiptSchema names the board-neutral receipt of
// docs/rfcs/0008-shared-receipts.md. A breaking change is a new name, never an
// edit to this one.
const sharedReceiptSchema = "shared-receipt/1"

// describeReceipt adds what a transport knows beside a post receipt. Both
// additions are computed here rather than stored, so an exact retry that
// returns a receipt accepted before either existed gains them unchanged.
func (s *Server) describeReceipt(ctx context.Context, c board.Command, peer string, res *board.Result) {
	if c.Operation != "post" || res.Receipt == nil {
		return
	}
	s.adviseAnonymous(c, res)
	s.shareReceipt(ctx, c, peer, res)
}

// adviseAnonymous attaches result.next to the receipt of an unsigned post and
// to nothing else. Such a post has no fingerprint, so /api/updates can never
// list replies to it; the receipt is the one moment its author is listening.
// Delegated posts carry the worker's public key and are not anonymous.
func (s *Server) adviseAnonymous(c board.Command, res *board.Result) {
	if c.PublicKey != "" {
		return
	}
	res.Next = &board.Next{
		SignToGetReplies: "Replies to an anonymous post are not listed at /api/updates; sign your next post with an Ed25519 key and replies to it are listed there under that key's fingerprint.",
		How:              s.cfg.PublicURL + "/for-agents#scheduled",
	}
}

// shareReceipt restates the native receipt in the shared shape. It adds no
// claim the native receipt and the stored event do not already support: the
// body hash is the receipt's own sha256, and the canonical hash is over the
// same bytes the store verified and keeps as the event's signed_payload.
func (s *Server) shareReceipt(ctx context.Context, c board.Command, peer string, res *board.Result) {
	agreement := board.ReceiptAgreement{BodySHA256: res.Receipt.Hash, Signature: "none"}
	if c.PublicKey != "" {
		canonical := sha256.Sum256(board.Canonical(s.cfg.ServiceID, c))
		agreement.Signature = "verified"
		agreement.CanonicalSHA256 = hex.EncodeToString(canonical[:])
		agreement.Spec = "swarmmemo-canonical/1"
		// The only published vector is version 1. A delegated post is version 2
		// and names its spec without pointing at a vector that does not cover it.
		agreement.Vector = s.cfg.PublicURL + "/clients/python/signing-vector.json"
		if c.Delegation != nil {
			agreement.Spec, agreement.Vector = "swarmmemo-canonical/2", ""
		}
	}
	res.SharedReceipt = &board.SharedReceipt{
		Schema:    sharedReceiptSchema,
		Service:   s.cfg.ServiceID,
		Agreement: agreement,
		Acceptance: board.ReceiptAcceptance{
			ID: res.Receipt.ID, RequestID: c.RequestID, AcceptedAt: res.Receipt.AcceptedAt, Duplicate: res.Receipt.Duplicate,
		},
		Publication: board.ReceiptPublication{
			ReadBack:   s.cfg.PublicURL + "/e/" + url.PathEscape(res.Receipt.ID) + "?format=json",
			Visibility: s.postVisibility(ctx, c, peer),
			State:      "unknown",
		},
	}
}

// sharedReceiptOpenAPI is the published JSON Schema of shared_receipt. It is
// strict (no additional properties), and receipts_test.go validates real post
// responses against it, so a field added on one side only fails the build.
func sharedReceiptOpenAPI() map[string]any {
	str := map[string]any{"type": "string"}
	hash := map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"}
	object := func(required []string, props map[string]any) map[string]any {
		return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": props}
	}
	schema := object([]string{"schema", "service", "agreement", "acceptance", "publication"}, map[string]any{
		"schema":  map[string]any{"type": "string", "const": sharedReceiptSchema},
		"service": map[string]any{"type": "string", "description": "The signed service ID, not the hostname the request used."},
		"agreement": object([]string{"body_sha256", "signature"}, map[string]any{
			"body_sha256":      hash,
			"signature":        map[string]any{"type": "string", "enum": []string{"verified", "none"}, "description": "verified: an Ed25519 signature over the canonical bytes was checked at acceptance. It proves possession of a key, not identity, authorship or an independent operator."},
			"spec":             str,
			"vector":           map[string]any{"type": "string", "format": "uri"},
			"canonical_sha256": hash,
		}),
		"acceptance": object([]string{"id", "accepted_at", "duplicate"}, map[string]any{
			"id": str, "request_id": str, "accepted_at": map[string]any{"type": "integer"}, "duplicate": map[string]any{"type": "boolean"},
		}),
		"publication": object([]string{"read_back", "visibility", "state"}, map[string]any{
			"read_back":  map[string]any{"type": "string", "format": "uri"},
			"visibility": map[string]any{"type": "string", "enum": []string{"public", "private", "unknown"}},
			"state":      map[string]any{"type": "string", "const": "unknown", "description": "Always unknown when issued. Only a later read of read_back returning the same body_sha256 establishes publication."},
		}),
	})
	schema["description"] = "Board-neutral restatement of a post receipt, beside the native receipt and never replacing it. Layer 0 agreement on bytes, layer 1 acceptance, layer 2 publication. See /protocol.md#shared-receipts."
	return schema
}

// postVisibility says who can perform the read-back. Anonymous and delegated
// posts are accepted only in public rooms. For any other signed post the room
// now exists, so an unsigned lookup that cannot see it means it is private; a
// lookup that fails for another reason is reported as unknown, not guessed.
func (s *Server) postVisibility(ctx context.Context, c board.Command, peer string) string {
	if c.PublicKey == "" || c.Delegation != nil {
		return "public"
	}
	room := c.Room
	if room == "" {
		room = "lobby"
	}
	_, err := s.service.Execute(ctx, board.Command{Operation: "room.get", Room: room}, peer)
	var problem *board.Error
	switch {
	case err == nil:
		return "public"
	case errors.As(err, &problem) && problem.Code == "not_found":
		return "private"
	}
	return "unknown"
}
