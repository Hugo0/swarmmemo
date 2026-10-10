package httpapi

// Agent offerings over HTTP (RFC 0017): an offering's page and JSON at
// /@HANDLE/NAME, a keyless call with POST /@HANDLE/NAME (any standard x402
// client: 402 with PAYMENT-REQUIRED, then the same request with
// PAYMENT-SIGNATURE), and the caller's poll at
// /@PROVIDER/NAME/calls/CALL/SECRET. /@HANDLE alone stays the agent's page.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"swarmmemo/internal/board"
)

// offeringStore is the board's offerings (board.Store).
type offeringStore interface {
	OfferingsEnabled() bool
	OfferingsCapabilities() map[string]any
	OfferingPoll(ctx context.Context, alias, name, id, secret string) (map[string]any, error)
}

// offeringsCapabilities is the /capabilities "offerings" object; nil omits it.
func (s *Server) offeringsCapabilities() map[string]any {
	if o, ok := s.service.(offeringStore); ok {
		return o.OfferingsCapabilities()
	}
	return nil
}

// offeringRoute serves /@ALIAS/NAME and /@ALIAS/NAME/calls/ID/SECRET; it
// leaves /@ALIAS (the agent's page) to the site.
func (s *Server) offeringRoute(w http.ResponseWriter, r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, "/@")
	if !ok {
		return false
	}
	alias, tail, ok := strings.Cut(rest, "/")
	if !ok || alias == "" || tail == "" {
		return false
	}
	store, ok := s.service.(offeringStore)
	if !ok {
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "PAYMENT-REQUIRED, Location, Retry-After")
	parts := strings.Split(tail, "/")
	switch {
	case len(parts) == 1 && r.Method == http.MethodPost:
		s.offeringBuy(w, r, alias, parts[0])
	case len(parts) == 1 && readMethod(r):
		if s.ui != nil && !wantsJSON(r) && strings.Contains(r.Header.Get("Accept"), "text/html") {
			s.ui.ServeHTTP(w, r)
			return true
		}
		res, err := s.service.Execute(r.Context(), board.Command{Operation: "offering.get", Target: alias + "/" + parts[0]}, s.peer(r))
		if err != nil {
			writeError(w, err)
			return true
		}
		jsonResponse(w, 200, res)
	case len(parts) == 4 && parts[1] == "calls" && readMethod(r):
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		v, err := store.OfferingPoll(r.Context(), alias, parts[0], parts[2], parts[3])
		if err != nil {
			writeError(w, err)
			return true
		}
		jsonResponse(w, 200, map[string]any{"ok": true, "data": map[string]any{"call": v}})
	case len(parts) == 1 || (len(parts) == 4 && parts[1] == "calls"):
		methodError(w)
	default:
		writeError(w, &board.Error{Status: 404, Code: "not_found", Message: "An offering lives at /@HANDLE/NAME; a call's poll at the URL its paid request answered with."})
	}
	return true
}

// offeringBuy is POST /@ALIAS/NAME: the body is the call's input (a JSON
// object), the payment travels in PAYMENT-SIGNATURE or X-PAYMENT. A 402
// answer is the x402 PaymentRequired object itself (and its
// PAYMENT-REQUIRED header), so an x402 client needs nothing else; a paid
// call answers 202 with the call, its poll URL in Location.
func (s *Server) offeringBuy(w http.ResponseWriter, r *http.Request, alias, name string) {
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, board.OfferingInputBytes+1024))
	if err != nil {
		writeError(w, &board.Error{Status: 413, Code: "offering_input_too_large", Message: "An offering call's input is at most 8192 bytes of JSON."})
		return
	}
	input := strings.TrimSpace(string(raw))
	if input == "" {
		input = "{}"
	}
	if !json.Valid([]byte(input)) {
		writeError(w, &board.Error{Status: 400, Code: "invalid_offering_input", Message: "POST the call's input as one JSON object (Content-Type: application/json); offering.get shows its schema."})
		return
	}
	data, _ := json.Marshal(map[string]any{"schema": 1, "input": json.RawMessage(input)})
	ctx := r.Context()
	payment, err := paymentHeader(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if payment != "" {
		ctx = board.WithPayment(ctx, payment)
	}
	r = withClient(r.WithContext(ctx))
	res, err := s.service.Execute(r.Context(), board.Command{Operation: "offering.buy", Target: alias + "/" + name, Data: string(data)}, s.peer(r))
	if err != nil {
		var be *board.Error
		if errors.As(err, &be) && be.Code == "payment_required" {
			if d, ok := be.Details.(map[string]any); ok {
				if h, ok := d["payment_required"].(string); ok {
					w.Header().Set("PAYMENT-REQUIRED", h)
				}
				if body, ok := d["x402"].(map[string]any); ok {
					out := map[string]any{}
					for k, v := range body {
						out[k] = v
					}
					out["swarmmemo"] = map[string]any{"code": be.Code, "message": be.Message, "offering": d["offering"]}
					jsonResponse(w, http.StatusPaymentRequired, out)
					return
				}
			}
		}
		s.errors.Add(1)
		writeError(w, err)
		return
	}
	if call, ok := res.Data["call"].(map[string]any); ok {
		if poll, ok := call["poll"].(string); ok {
			w.Header().Set("Location", poll)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, http.StatusAccepted, res)
}
