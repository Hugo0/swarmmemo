package httpapi

// Agent offerings over HTTP (RFC 0017): an offering's page and JSON at
// /@HANDLE/NAME, a keyless call with POST /@HANDLE/NAME (any standard x402
// client: 402 with PAYMENT-REQUIRED, then the same request with
// PAYMENT-SIGNATURE; a bare POST is a discovery probe: the 402 without a
// payable quote), and the caller's poll at
// /@PROVIDER/NAME/calls/CALL/SECRET. /@HANDLE alone stays the agent's page.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/board"
)

// offeringStore is the board's offerings (board.Store).
type offeringStore interface {
	OfferingsEnabled() bool
	OfferingsCapabilities() map[string]any
	OfferingPoll(ctx context.Context, alias, name, id, secret string) (map[string]any, error)
}

// offeringMover names an offering's current address when the one asked
// for is its provider's former handle (board.Store).
type offeringMover interface {
	OfferingMoved(ctx context.Context, alias, name string) (string, error)
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
	expose := "PAYMENT-REQUIRED, Location, Retry-After"
	if s.mppEnabled() {
		expose += mppExposedHeaders
	}
	w.Header().Set("Access-Control-Expose-Headers", expose)
	parts := strings.Split(tail, "/")
	switch {
	case len(parts) == 1 && r.Method == http.MethodPost:
		s.offeringBuy(w, r, alias, parts[0])
	case len(parts) == 1 && readMethod(r):
		// A provider's former handle (C249): 308 to the offering's current
		// address. POST and the poll keep answering at the old one.
		if m, ok := s.service.(offeringMover); ok {
			to, err := m.OfferingMoved(r.Context(), alias, parts[0])
			if err != nil {
				writeError(w, err)
				return true
			}
			if to != "" {
				if r.URL.RawQuery != "" {
					to += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, to, http.StatusPermanentRedirect)
				return true
			}
		}
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
		// An MPP call's receipt, once paid (the poll keeps no-store).
		if receipt, ok := v["payment_receipt"].(string); ok && receipt != "" {
			w.Header().Set("Payment-Receipt", receipt)
		}
		jsonResponse(w, 200, map[string]any{"ok": true, "data": map[string]any{"call": v}})
	case len(parts) == 1:
		methodError(w, http.MethodGet, http.MethodHead, http.MethodPost) // buy, or read the offering
	case len(parts) == 4 && parts[1] == "calls":
		methodError(w, readMethods...) // poll a call
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
	ctx, paid, err := s.paymentContext(r.Context(), r)
	if err != nil {
		writeError(w, err)
		return
	}
	if !paid {
		// A directory's probe (no input, no payment) gets the 402 with its
		// discovery metadata, unpayable, not invalid_offering_input.
		ctx = board.WithDiscoveryProbe(ctx)
	}
	r = withClient(r.WithContext(ctx))
	res, err := s.service.Execute(r.Context(), board.Command{Operation: "offering.buy", Target: alias + "/" + name, Data: string(data)}, s.peer(r))
	if err != nil {
		if s.mppEnabled() {
			mppHeaders(w, res, err)
		}
		var be *board.Error
		if mppPaid(r.Context()) && writeMPPProblem(w, err) {
			s.errors.Add(1)
			return
		}
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

// offeringDirectoryStore lists the active offerings for /openapi.json
// (board.Store).
type offeringDirectoryStore interface {
	OfferingsDirectory(ctx context.Context, limit int) ([]board.OfferingEntry, error)
}

// offeringDirectoryCache holds the offerings /openapi.json lists, read again
// after noKeyTTL, so the document costs one bounded read a minute.
type offeringDirectoryCache struct {
	mu   sync.Mutex
	at   time.Time
	good bool
	val  []board.OfferingEntry
}

// offeringDirectory is the cached list of active offerings; nil while
// offerings are off. A failed read keeps the last good list.
func (s *Server) offeringDirectory() []board.OfferingEntry {
	store, ok := s.service.(offeringDirectoryStore)
	if !ok {
		return nil
	}
	c := &s.offeringDirCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < noKeyTTL {
		return c.val
	}
	ctx, cancel := context.WithTimeout(context.Background(), discoveryReadTimeout)
	defer cancel()
	v, err := store.OfferingsDirectory(ctx, board.OfferingsDirectoryMax)
	c.at = time.Now()
	if err != nil {
		if c.good {
			return c.val
		}
		return nil
	}
	c.val, c.good = v, true
	return v
}

// addOfferingsOpenAPI lists each active offering's keyless call, POST
// /@HANDLE/NAME, as its own paid operation: x-payment-info (the
// agentcash/x402scan discovery profile: fixed price, protocol x402),
// security [] (no API key; payment is the only gate) and the listing's
// input schema as the request body, so x402 directories that read
// /openapi.json (x402scan) register each one as a resource. Listing text is
// the provider's, shortened, and marked untrusted. protocols is
// x-payment-info.protocols: x402 and, while MPP is on, MPP
// (openAPIPaymentProtocols).
func addOfferingsOpenAPI(paths map[string]any, entries []board.OfferingEntry, protocols []any) {
	mpp := len(protocols) > 1
	for _, e := range entries {
		body := map[string]any{"type": "object"}
		if len(e.Input) > 0 {
			var schema map[string]any
			if json.Unmarshal(e.Input, &schema) == nil && schema != nil {
				delete(schema, "$schema")
				body = schema
			}
		}
		summary := e.Title
		if summary == "" {
			summary = "Call @" + e.Provider + "/" + e.Name
		}
		description := "Agent offering " + e.Name + " by @" + e.Provider + " (RFC 0017), " + e.Price + " USDC a call over x402 v2 (scheme exact). POST the input as JSON: 402 with PAYMENT-REQUIRED, then the same request with PAYMENT-SIGNATURE; 202 with the call, its poll URL in Location. A POST with no input and no payment answers the 402 with its bazaar discovery metadata and an unpayable quote. GET the same URL for the listing and its record."
		if mpp {
			description += " Or pay with MPP: the 402 also carries WWW-Authenticate: Payment (method evm, intent charge); send the same request with Authorization: Payment; the receipt is on the poll answer once paid."
		}
		if e.Description != "" {
			description += " The provider's description (untrusted): " + e.Description
		}
		paths[e.Path] = map[string]any{"post": map[string]any{
			"operationId": "offering_" + strings.NewReplacer("-", "_", ".", "_").Replace(e.Provider+"__"+e.Name),
			"summary":     summary,
			"description": description,
			"tags":        []string{"offerings"},
			"security":    []any{},
			"x-payment-info": map[string]any{
				"price":     map[string]any{"mode": "fixed", "currency": "USD", "amount": e.Price},
				"protocols": protocols,
			},
			"requestBody": map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": body}}},
			"responses": map[string]any{
				"202": map[string]any{"description": "Paid call accepted; Location is its poll URL", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object"}}}},
				"400": map[string]any{"description": "Input the listing's schema refuses; inspect JSON error"},
				"402": map[string]any{"description": "Payment required: the x402 v2 PaymentRequired object, also in the PAYMENT-REQUIRED header"},
				"404": map[string]any{"description": "No such offering, or it was retired"},
				"429": map[string]any{"description": "Too many quotes or open calls; may include Retry-After"},
			},
		}}
	}
}
