package httpapi

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

// receiverService is the board's delivery entry point (board.Store.Receive).
type receiverService interface {
	Receive(ctx context.Context, d services.Delivery) (services.DeliveryReceipt, error)
}

// receive is POST /in/ID/SECRET: one delivery to an agent's receiver
// (services/receiver.go). The body is read up to one byte past the limit and
// handed over as data; nothing here logs the URL, a header or the body, and
// the answer names only the stored item. Any other method is refused
// without looking the receiver up.
func (s *Server) receive(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, services.ReceiverPathPrefix)
	id, token, ok := strings.Cut(rest, "/")
	if !ok || strings.Contains(token, "/") {
		writeError(w, &board.Error{Status: 404, Code: "receiver_not_found", Message: "A receive URL is /in/ID/SECRET, as receiver create or rotate showed it."})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, &board.Error{Status: 405, Code: "method_not_allowed", Message: "A receive URL takes POST only: send the body (JSON, a form or text) as the request body."})
		return
	}
	svc, ok := s.service.(receiverService)
	if !ok {
		writeError(w, &board.Error{Status: 404, Code: "receiver_not_found", Message: "No receiver answers at this URL."})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, services.ReceiverBodyBytes+1))
	if err != nil {
		writeError(w, bad("The request body could not be read."))
		return
	}
	// The headers an item may keep (the service caps their count and size).
	headers := map[string]string{}
	for name, values := range r.Header {
		if len(values) > 0 && values[0] != "" && services.ReceiverKeepsHeader(name) {
			headers[strings.ToLower(name)] = values[0]
		}
	}
	receipt, err := svc.Receive(r.Context(), services.Delivery{ID: id, Token: token, ContentType: r.Header.Get("Content-Type"), Body: body,
		Signature: r.Header.Get("X-Hub-Signature-256"), Source: net.ParseIP(s.peer(r)), Headers: headers})
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]any{"ok": true, "item": receipt.Item, "bytes": receipt.Bytes})
}
