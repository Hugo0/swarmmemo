package board

// Offering calls (RFC 0017): the quote, the intake, the provider's claim and
// decline, the lapse sweep and the operator's resolve.
//
// Money path. offering.buy without a payment answers 402 with one x402 v2
// requirement: the listing's price to its pay_to (the provider's verified
// wallet link), maxTimeoutSeconds the claim window, and a quote (an HMAC
// under the top-up quote key, domain-separated) binding the provider, the
// name, the revision, the amount, the recipient, the input's SHA-256 and the
// signed caller. The retry carries the signed EIP-3009 authorization. In the
// command's transaction: the quote ours and unexpired, the payment exactly
// what it asked, the caps, and the authorization nonce never seen (UNIQUE):
// a call is inserted verifying, its authorization sealed. After commit,
// holding no connection, the facilitator verifies it (/verify only: nothing
// moves), and one short transaction marks it authorized (the provider's
// inbox entry and on:"received" wake-ups) or failed. The provider's
// offering.claim moves it to settling; after commit the facilitator settles
// (/settle), and one short transaction records paid (due by the SLA),
// failed or unknown (the operator resolves it: swarmmemo offering resolve).
// The authorization is wiped once settled, declined, lapsed or failed. A
// call left settling by a crash reads as unknown at the next start.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"swarmmemo/internal/services"
)

// inboxOfferingCall is the provider's inbox entry kind for a call awaiting
// its claim: a pointer (the call id), never the input.
const inboxOfferingCall = "offering_call"

func randRead(b []byte) (int, error) { return rand.Read(b) }

// offeringSettleTimeout bounds the facilitator round trip after commit,
// detached from the caller like a top-up's.
const offeringSettleTimeout = 75 * time.Second

// ---- input schema --------------------------------------------------------

// inputSchema is the JSON Schema subset an offering's input follows: an
// object of at most OfferingSchemaProperties scalar properties.
type inputSchema struct {
	props      map[string]inputProp
	required   []string
	additional bool
}

type inputProp struct {
	typ                  string
	enum                 []json.RawMessage
	minLength, maxLength int64
	minimum, maximum     *float64
}

func invalidInputSchema(detail string) error {
	return problem(400, "invalid_input_schema", `input is a JSON Schema subset: {"type":"object","properties":{NAME:{"type":"string"|"number"|"integer"|"boolean", optional "description","enum","minLength","maxLength","minimum","maximum"}},"required":[NAMES],"additionalProperties":true|false}, at most `+strconv.Itoa(OfferingSchemaProperties)+" properties. "+detail)
}

func parseInputSchema(raw json.RawMessage) (inputSchema, error) {
	sc := inputSchema{props: map[string]inputProp{}, additional: true}
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil || top == nil {
		return sc, invalidInputSchema("input is a JSON object.")
	}
	for k, v := range top {
		switch k {
		case "type":
			var t string
			if json.Unmarshal(v, &t) != nil || t != "object" {
				return sc, invalidInputSchema(`Its type is "object".`)
			}
		case "title", "description", "$schema":
			var t string
			if json.Unmarshal(v, &t) != nil {
				return sc, invalidInputSchema(k + " is a string.")
			}
		case "additionalProperties":
			if json.Unmarshal(v, &sc.additional) != nil {
				return sc, invalidInputSchema("additionalProperties is true or false.")
			}
		case "required":
			if json.Unmarshal(v, &sc.required) != nil {
				return sc, invalidInputSchema("required is a list of property names.")
			}
		case "properties":
			var props map[string]json.RawMessage
			if json.Unmarshal(v, &props) != nil || len(props) > OfferingSchemaProperties {
				return sc, invalidInputSchema("properties is an object.")
			}
			for name, p := range props {
				if !offeringPropRE.MatchString(name) {
					return sc, invalidInputSchema(fmt.Sprintf("Property %q is not a plain name (letters, digits, _).", name))
				}
				prop, err := parseInputProp(name, p)
				if err != nil {
					return sc, err
				}
				sc.props[name] = prop
			}
		default:
			return sc, invalidInputSchema(fmt.Sprintf("%q is not in the subset.", k))
		}
	}
	if _, ok := top["type"]; !ok {
		return sc, invalidInputSchema(`Its type is "object".`)
	}
	for _, r := range sc.required {
		if _, ok := sc.props[r]; !ok {
			return sc, invalidInputSchema(fmt.Sprintf("Required %q is not a property.", r))
		}
	}
	return sc, nil
}

func parseInputProp(name string, raw json.RawMessage) (inputProp, error) {
	var p inputProp
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return p, invalidInputSchema(fmt.Sprintf("Property %q is an object.", name))
	}
	for k, v := range m {
		var err error
		switch k {
		case "type":
			err = json.Unmarshal(v, &p.typ)
			if err == nil && p.typ != "string" && p.typ != "number" && p.typ != "integer" && p.typ != "boolean" {
				err = errors.New("type")
			}
		case "title", "description":
			var t string
			err = json.Unmarshal(v, &t)
		case "enum":
			err = json.Unmarshal(v, &p.enum)
			if err == nil && (len(p.enum) == 0 || len(p.enum) > 32) {
				err = errors.New("enum")
			}
		case "minLength":
			err = json.Unmarshal(v, &p.minLength)
		case "maxLength":
			err = json.Unmarshal(v, &p.maxLength)
		case "minimum":
			err = json.Unmarshal(v, &p.minimum)
		case "maximum":
			err = json.Unmarshal(v, &p.maximum)
		default:
			err = errors.New("keyword")
		}
		if err != nil {
			return p, invalidInputSchema(fmt.Sprintf("Property %q: %q is not in the subset or is malformed.", name, k))
		}
	}
	if p.typ == "" {
		return p, invalidInputSchema(fmt.Sprintf("Property %q needs a type.", name))
	}
	if p.minLength < 0 || p.maxLength < 0 || (p.maxLength > 0 && p.minLength > p.maxLength) {
		return p, invalidInputSchema(fmt.Sprintf("Property %q: minLength and maxLength are 0 or more, min at most max.", name))
	}
	return p, nil
}

func invalidInput(detail string) error {
	return problem(400, "invalid_offering_input", "The input does not follow this offering's input schema (offering.get shows it): "+detail)
}

// check holds an input object to the schema.
func (sc inputSchema) check(input map[string]json.RawMessage) error {
	for _, r := range sc.required {
		if _, ok := input[r]; !ok {
			return invalidInput(fmt.Sprintf("%q is required.", r))
		}
	}
	for name, raw := range input {
		p, ok := sc.props[name]
		if !ok {
			if !sc.additional {
				return invalidInput(fmt.Sprintf("%q is not a property.", name))
			}
			continue
		}
		if len(p.enum) > 0 {
			found := false
			for _, e := range p.enum {
				if string(e) == string(raw) {
					found = true
				}
			}
			if !found {
				return invalidInput(fmt.Sprintf("%q is not one of its enum.", name))
			}
		}
		switch p.typ {
		case "string":
			var v string
			if json.Unmarshal(raw, &v) != nil {
				return invalidInput(fmt.Sprintf("%q is a string.", name))
			}
			n := int64(len([]rune(v)))
			if n < p.minLength || (p.maxLength > 0 && n > p.maxLength) {
				return invalidInput(fmt.Sprintf("%q has %d characters; it takes %d to %d.", name, n, p.minLength, p.maxLength))
			}
		case "number", "integer":
			var v float64
			if json.Unmarshal(raw, &v) != nil || (p.typ == "integer" && v != math.Trunc(v)) {
				return invalidInput(fmt.Sprintf("%q is a %s.", name, p.typ))
			}
			if (p.minimum != nil && v < *p.minimum) || (p.maximum != nil && v > *p.maximum) {
				return invalidInput(fmt.Sprintf("%q is out of its range.", name))
			}
		case "boolean":
			var v bool
			if json.Unmarshal(raw, &v) != nil {
				return invalidInput(fmt.Sprintf("%q is true or false.", name))
			}
		}
	}
	return nil
}

// ---- buy -----------------------------------------------------------------

// buyData is offering.buy's data: {"schema":1,"input":{…}} and, on the
// paid retry over a wire without headers, "payment".
type buyData struct {
	Schema  json.Number     `json:"schema"`
	Input   json.RawMessage `json:"input"`
	Payment string          `json:"payment,omitempty"`
}

// parseBuy checks the input against o's schema and returns it compacted,
// its SHA-256 and the payment data carried.
func parseBuy(raw string, o offeringRow) (string, string, string, error) {
	var d buyData
	if raw == "" || services.StrictObject([]byte(raw), &d) != nil || d.Schema.String() != "1" {
		return "", "", "", problem(400, "invalid_request", `offering.buy data is {"schema":1,"input":{…}} (the input the offering's schema asks for), and on the paid retry, without a PAYMENT-SIGNATURE header, "payment":"BASE64".`)
	}
	input := []byte(d.Input)
	if len(input) == 0 || string(input) == "null" {
		input = []byte("{}")
	}
	var compact strings.Builder
	var obj map[string]json.RawMessage
	if json.Unmarshal(input, &obj) != nil || obj == nil {
		return "", "", "", invalidInput("the input is a JSON object.")
	}
	out, err := compactJSON(input)
	if err != nil {
		return "", "", "", invalidInput("the input is a JSON object.")
	}
	compact.WriteString(out)
	if compact.Len() > OfferingInputBytes {
		return "", "", "", problem(413, "offering_input_too_large", "An offering call's input is at most "+strconv.Itoa(OfferingInputBytes)+" bytes "+SizeNote(compact.Len(), OfferingInputBytes, "bytes")+".")
	}
	if len(o.Listing.Input) > 0 {
		sc, err := parseInputSchema(o.Listing.Input)
		if err != nil {
			return "", "", "", err
		}
		if err = sc.check(obj); err != nil {
			return "", "", "", err
		}
	}
	return compact.String(), sha256Hex([]byte(compact.String())), d.Payment, nil
}

// offeringQuote is "EXPIRES.SALT.MAC" over every term the requirement and
// the call bind, under the top-up quote key, domain-separated from top-ups.
func (s *Store) offeringQuote(o offeringRow, maxTimeout int64, inputSHA, caller string, expires int64, salt string) string {
	cfg := s.config.Offerings
	mac := hmac.New(sha256.New, s.topupKey)
	for _, part := range []string{"swarmmemo-offering/1", o.Account, o.Name, strconv.FormatInt(o.Rev, 10), strconv.FormatInt(o.Price, 10),
		cfg.Network, cfg.Asset.String(), strings.ToLower(o.PayTo), strconv.FormatInt(maxTimeout, 10), inputSHA, caller, strconv.FormatInt(expires, 10), salt} {
		mac.Write([]byte(part))
		mac.Write([]byte{0})
	}
	return strconv.FormatInt(expires, 10) + "." + salt + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:24])
}

// offeringRequirement is the requirement a quote for o stands for.
func (s *Store) offeringRequirement(o offeringRow, quote string, expires int64) services.X402Requirement {
	payTo, _ := services.ParseEVMAddress(strings.ToLower(o.PayTo))
	return services.X402Requirement{PayTo: payTo, Amount: o.Price, Quote: quote, Expires: expires, MaxTimeout: s.effectiveClaimWindow(o.ClaimWindow)}
}

// checkOfferingQuote is the requirement quote stands for, when this server
// issued it for exactly this offering revision, input and caller.
func (s *Store) checkOfferingQuote(quote string, o offeringRow, inputSHA, caller string) (services.X402Requirement, error) {
	mismatch := problem(400, "payment_mismatch", "The payment does not accept a requirement this server issued for this offering, at its current revision, for this input and caller; ask again without a payment for a fresh one.")
	parts := strings.Split(quote, ".")
	if len(parts) != 3 {
		return services.X402Requirement{}, mismatch
	}
	expires, err := strconv.ParseInt(parts[0], 10, 64)
	window := s.effectiveClaimWindow(o.ClaimWindow)
	if err != nil || expires <= 0 || !hmac.Equal([]byte(s.offeringQuote(o, window, inputSHA, caller, expires, parts[1])), []byte(quote)) {
		return services.X402Requirement{}, mismatch
	}
	return s.offeringRequirement(o, quote, expires), nil
}

// quoteAllowed takes one token from the caller's and the offering's quote
// buckets.
func (s *Store) quoteAllowed(caller, offering string, now time.Time) bool {
	st := &s.offerings
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.rates == nil {
		st.rates = map[string]privateReadBucket{}
	}
	if len(st.rates) > 10_000 {
		for k, b := range st.rates {
			if now.Sub(b.seen) >= time.Minute {
				delete(st.rates, k)
			}
		}
	}
	ck, ok := "c:"+caller, "o:"+offering
	cb := privateBucket(now, st.rates[ck], OfferingQuotesPerMinute/60.0, OfferingQuotesPerMinute)
	ob := privateBucket(now, st.rates[ok], OfferingQuotesPerOfferingMinute/60.0, OfferingQuotesPerOfferingMinute)
	if cb.tokens < 1 || ob.tokens < 1 {
		st.rates[ck], st.rates[ok] = cb, ob
		return false
	}
	cb.tokens--
	ob.tokens--
	st.rates[ck], st.rates[ok] = cb, ob
	return true
}

// sealedAuth is what is sealed at rest for a call: the payment as the
// caller sent it and the requirement terms the facilitator is shown again
// at settle.
type sealedAuth struct {
	Payment    string `json:"payment"`
	Quote      string `json:"quote"`
	Expires    int64  `json:"expires"`
	MaxTimeout int64  `json:"max_timeout"`
}

func offeringAAD(id string) []byte { return []byte("swarmmemo-offering-auth/1\x00" + id) }

func (s *Store) sealAuth(id string, v sealedAuth) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sealed := s.offerings.aead.Seal(nil, nil, plain, offeringAAD(id))
	clear(plain)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (s *Store) openAuth(id, sealed, keyID string) (sealedAuth, error) {
	var v sealedAuth
	if s.offerings.aead == nil || keyID != s.offerings.keyID {
		return v, errors.New("offering authorization: the sealing key is not loaded")
	}
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return v, errors.New("offering authorization is malformed")
	}
	plain, err := s.offerings.aead.Open(nil, nil, raw, offeringAAD(id))
	if err != nil {
		return v, errors.New("offering authorization cannot be opened")
	}
	err = json.Unmarshal(plain, &v)
	clear(plain)
	return v, err
}

// pollSecret is the caller's poll secret for a call: derived from the
// sealing key file (never in the database), the call and the quote, which
// only the caller and this server know. Only its SHA-256 is stored.
func (s *Store) pollSecret(id, quote string) string {
	mac := hmac.New(sha256.New, s.offerings.pollKey)
	mac.Write([]byte(id + "\x00" + quote))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// pollPath is a call's poll path, by the provider's fingerprint (stable
// across handle changes).
func pollPath(account, name, id, secret string) string {
	return "/@" + account + "/" + name + "/calls/" + id + "/" + secret
}

// offeringBuy is offering.buy.
func (s *Store) offeringBuy(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if !s.OfferingsEnabled() {
		return Result{}, offeringsUnavailable()
	}
	account, name, err := offeringTarget(ctx, tx, c.Target)
	if err != nil {
		return Result{}, err
	}
	o, err := loadOffering(ctx, tx, account, name)
	if err != nil {
		return Result{}, err
	}
	if o.State != "active" {
		return Result{}, problem(410, "offering_retired", "This offering is retired: it takes no new calls.")
	}
	input, inputSHA, payment, err := parseBuy(c.Data, o)
	if err != nil {
		return Result{}, err
	}
	if header := paymentFrom(ctx); header != "" {
		if payment != "" && payment != header {
			return Result{}, problem(400, "payment_invalid", "The payment header and data.payment differ; send one.")
		}
		payment = header
	}
	caller := ""
	if a.signed {
		caller = a.id
	}
	var open int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM offering_calls WHERE account=? AND name=? AND (state IN ('verifying','settling') OR (state='authorized' AND claim_by>?))", account, name, now).Scan(&open); err != nil {
		return Result{}, err
	}
	cfg := s.config.Offerings
	window := s.effectiveClaimWindow(o.ClaimWindow)
	if payment == "" {
		if open >= OfferingOpenCalls {
			return Result{}, &Error{Status: 429, Code: "offering_busy", Message: fmt.Sprintf("This offering already holds %d calls awaiting its provider; nothing was charged. Try again later.", OfferingOpenCalls), RetryAfter: 60}
		}
		if !s.quoteAllowed(a.account, account+"/"+name, s.now()) {
			return Result{}, &Error{Status: 429, Code: "offering_quote_rate", Message: fmt.Sprintf("At most %d offering quotes a minute per caller, and %d per offering; wait a few seconds.", OfferingQuotesPerMinute, OfferingQuotesPerOfferingMinute), RetryAfter: 10}
		}
		b := make([]byte, 9)
		_, _ = rand.Read(b)
		expires := now + services.TopupQuoteSeconds
		quote := s.offeringQuote(o, window, inputSHA, caller, expires, base64.RawURLEncoding.EncodeToString(b))
		req := s.offeringRequirement(o, quote, expires)
		_, handle, err := providerOf(ctx, tx, account)
		if err != nil {
			return Result{}, err
		}
		path := offeringPath(account, handle, name)
		body, header := cfg.PaymentRequiredFor(req, "https://"+s.config.ServiceID+path, fmt.Sprintf("%s (%s USDC, %s)", o.Listing.Title, o.Listing.Price, path))
		return Result{}, &Error{Status: 402, Code: "payment_required", Message: fmt.Sprintf("Pay %s USDC on %s to %s, the provider's wallet: sign the requirement in details.x402.accepts (x402 v2, exact, EIP-3009) within %d seconds and send the same request again with the payment in the PAYMENT-SIGNATURE header (or data.payment). The payment is only verified now; it settles when the provider claims the call, and nothing is charged if it is declined or not claimed within %d seconds.", o.Listing.Price, cfg.Network, o.Listing.PayTo, services.TopupQuoteSeconds, window),
			Details: map[string]any{"x402": body, "payment_required": header, "offering": map[string]any{"path": path, "rev": o.Rev, "price": o.Listing.Price, "claim_window": window, "sla": o.SLA, "input_sha256": inputSHA}}}
	}
	p, err := services.ParseTopupPayment(payment)
	if err != nil {
		return Result{}, offeringPaymentError(err)
	}
	req, err := s.checkOfferingQuote(p.Quote, o, inputSHA, caller)
	if err != nil {
		return Result{}, err
	}
	if err = cfg.CheckRequirement(p, req, now); err != nil {
		return Result{}, offeringPaymentError(err)
	}
	claimBy := min(now+window, p.ValidBefore-services.X402ValidBeforeMargin)
	if claimBy-now < OfferingClaimMinimum {
		return Result{}, problem(400, "payment_expired", fmt.Sprintf("The payment authorization must stay valid for the claim window (at least %d seconds from now, plus %d); sign one whose validBefore is now plus maxTimeoutSeconds.", OfferingClaimMinimum, services.X402ValidBeforeMargin))
	}
	quoteHash := sha256Hex([]byte("swarmmemo-offering-quote/1\x00" + p.Quote))
	// One authorization, one call: the same authorization and quote again
	// is the same call; a refusal the facilitator could not even give
	// (nothing moved) may be presented again.
	var existing struct {
		id, account, name, quoteHash, state string
		retryable                           bool
	}
	err = tx.QueryRowContext(ctx, "SELECT id,account,name,quote_hash,state,retryable FROM offering_calls WHERE auth_nonce=?", p.Nonce).
		Scan(&existing.id, &existing.account, &existing.name, &existing.quoteHash, &existing.state, &existing.retryable)
	switch {
	case err == nil && existing.quoteHash == quoteHash && existing.account == account && existing.name == name && !(existing.state == "failed" && existing.retryable):
		return s.offeringCallResult(ctx, tx, existing.id, p.Quote, now)
	case err == nil && !(existing.quoteHash == quoteHash && existing.state == "failed" && existing.retryable):
		return Result{}, problem(409, "payment_replayed", "This payment authorization was already presented for another call; one authorization pays for one call.")
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return Result{}, err
	}
	if open >= OfferingOpenCalls {
		return Result{}, &Error{Status: 429, Code: "offering_busy", Message: fmt.Sprintf("This offering already holds %d calls awaiting its provider; nothing was charged. Try again later.", OfferingOpenCalls), RetryAfter: 60}
	}
	var payerOpen int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM offering_calls WHERE payer=? AND (state IN ('verifying','settling') OR (state='authorized' AND claim_by>?))", p.From.String(), now).Scan(&payerOpen); err != nil {
		return Result{}, err
	}
	if payerOpen >= OfferingOpenCallsPerPayer {
		return Result{}, &Error{Status: 429, Code: "offering_payer_limit", Message: fmt.Sprintf("One paying wallet holds at most %d open offering calls; nothing was charged. Wait for a provider to claim, decline or let one lapse.", OfferingOpenCallsPerPayer), RetryAfter: 60}
	}
	id := existing.id
	if id == "" {
		id = "oc_" + randomID()
	}
	sealed, err := s.sealAuth(id, sealedAuth{Payment: payment, Quote: p.Quote, Expires: req.Expires, MaxTimeout: req.MaxTimeout})
	if err != nil {
		return Result{}, err
	}
	poll := s.pollSecret(id, p.Quote)
	if existing.id != "" {
		_, err = tx.ExecContext(ctx, `UPDATE offering_calls SET state='verifying',reason='',retryable=0,auth=?,auth_key=?,caller=?,input=?,claim_by=?,valid_before=?,created_at=?,closed_at=0 WHERE id=? AND state='failed' AND retryable=1`,
			sealed, s.offerings.keyID, caller, input, claimBy, p.ValidBefore, now, id)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO offering_calls(id,account,name,rev,caller,payer,amount,network,asset,pay_to,input,input_sha256,auth,auth_key,auth_nonce,quote_hash,valid_before,state,poll_hash,created_at,claim_by)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'verifying',?,?,?)`,
			id, account, name, o.Rev, caller, p.From.String(), o.Price, cfg.Network, cfg.Asset.String(), req.PayTo.String(), input, inputSHA, sealed, s.offerings.keyID, p.Nonce, quoteHash, p.ValidBefore, sha256Hex([]byte(poll)), now, claimBy)
	}
	if err != nil {
		return Result{}, err
	}
	call, err := loadCall(ctx, tx, id)
	if err != nil {
		return Result{}, err
	}
	// The stored receipt (requests) never holds the poll secret: it is
	// added after commit, from the quote the caller sent.
	return Result{Data: map[string]any{"call": call.callerView(now)}, afterCommit: func() (Result, error) {
		vctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), offeringSettleTimeout)
		defer cancel()
		return s.verifyCall(vctx, id, p, req)
	}}, nil
}

// verifyCall runs after the intake's commit, holding no connection: the
// facilitator verifies the authorization, then one short transaction marks
// the call authorized, tells the provider and fires its wake-ups, or fails
// it and wipes the authorization.
func (s *Store) verifyCall(ctx context.Context, id string, p services.TopupPayment, req services.X402Requirement) (Result, error) {
	verr := s.config.Offerings.Verify(ctx, s.webhookDial, p, req)
	now := s.now().Unix()
	if verr != nil {
		var te *services.TopupError
		if !errors.As(verr, &te) {
			te = &services.TopupError{Code: "payment_unsettled", Reason: services.TopupFacilitatorUnavailable, Definite: true}
		}
		retryable := 0
		if te.Reason == services.TopupFacilitatorUnavailable {
			retryable = 1
		}
		if _, err := s.db.ExecContext(ctx, "UPDATE offering_calls SET state='failed',reason=?,retryable=?,auth='',closed_at=? WHERE id=? AND state='verifying'", te.Reason, retryable, now, id); err != nil {
			slog.Error("Offering call outcome not recorded", "call", id, "error", err.Error())
		}
		return Result{}, offeringPaymentError(te)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE offering_calls SET state='authorized' WHERE id=? AND state='verifying'", id)
	if err != nil {
		return Result{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Result{}, fmt.Errorf("offering call %s is not verifying", id)
	}
	call, err := loadCall(ctx, tx, id)
	if err != nil {
		return Result{}, err
	}
	if err = s.tellProvider(ctx, tx, call, now); err != nil {
		return Result{}, err
	}
	if err = tx.Commit(); err != nil {
		return Result{}, err
	}
	s.signalChange()
	s.screenCallInput(call)
	return s.offeringCallResultDB(ctx, id, req.Quote, now)
}

// tellProvider is a fresh call's push, in tx: the provider's inbox entry
// (kind offering_call, a pointer), its webhooks that asked for that kind
// under INBOX_ENTRIES=read, and its on:"received" wake-ups.
func (s *Store) tellProvider(ctx context.Context, tx *sql.Tx, call callRow, now int64) error {
	actor := call.Caller
	entries, err := s.recordInbox(ctx, tx, inboxSource{kind: inboxOfferingCall, account: call.Account, subject: call.ID, actor: actor, actorAccount: "",
		needsAnswer: true, at: now, detail: map[string]any{"offering": call.Name, "amount": services.FormatUSDC(call.Amount), "claim_by": call.ClaimBy}})
	if err != nil {
		return err
	}
	if s.inboxRead(ctx) {
		if _, err = s.webhookEntries(ctx, tx, entries, nil, now); err != nil {
			return err
		}
	}
	if e := s.services.engine; e != nil {
		return e.FireReceived(ctx, tx, call.Account, now)
	}
	return nil
}

// screenCallInput screens a call's input once, after it is authorized, in
// the background: the provider reads the verdict before it claims. Without
// a screener the input is unscreened, at once.
func (s *Store) screenCallInput(call callRow) {
	if s.convScreen.screener == nil {
		_, _ = s.db.Exec("UPDATE offering_calls SET input_screen='unscreened' WHERE id=? AND input_screen='pending'", call.ID)
		return
	}
	s.offerings.screens.Add(1)
	go func() {
		defer s.offerings.screens.Done()
		ctx, cancel := context.WithTimeout(context.Background(), convScreenTimeout)
		defer cancel()
		m := s.screenText(ctx, call.Input, payerSwarmMemo)
		scores := ""
		if m.scores != nil {
			raw, _ := json.Marshal(m.scores)
			scores = string(raw)
		}
		if _, err := s.db.ExecContext(ctx, "UPDATE offering_calls SET input_screen=?,input_scores=? WHERE id=? AND input_screen='pending'", m.state, scores, call.ID); err != nil {
			slog.Warn("offering call input screen not stored", "error", err)
		}
	}()
}

// offeringCallResult is the caller's answer for a call: the call as it
// stands and, from the quote it paid under, its poll path.
func (s *Store) offeringCallResult(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id, quote string, now int64) (Result, error) {
	call, err := loadCall(ctx, q, id)
	if err != nil {
		return Result{}, err
	}
	switch call.State {
	case "verifying":
		return Result{}, &Error{Status: 409, Code: "request_in_flight", Message: "This call's payment is still being verified; retry after retry_after seconds.", RetryAfter: 5}
	case "failed":
		if call.Retryable {
			return Result{}, offeringPaymentError(&services.TopupError{Code: "payment_unsettled", Reason: services.TopupFacilitatorUnavailable, Definite: true})
		}
		return Result{}, offeringPaymentError(&services.TopupError{Code: "payment_rejected", Reason: call.Reason, Definite: true})
	}
	v := call.callerView(now)
	secret := s.pollSecret(id, quote)
	if sha256Hex([]byte(secret)) == call.PollHash {
		v["poll"] = pollPath(call.Account, call.Name, call.ID, secret)
	}
	return Result{Data: map[string]any{"call": v}}, nil
}

func (s *Store) offeringCallResultDB(ctx context.Context, id, quote string, now int64) (Result, error) {
	return s.offeringCallResult(ctx, s.db, id, quote, now)
}

// offeringRetry answers an exact retry of an accepted offering.buy or
// offering.claim: the call as it stands now (a buy's with its poll path,
// when the retry carries the same payment).
func (s *Store) offeringRetry(ctx context.Context, tx *sql.Tx, c Command, stored Result, now int64) (Result, error) {
	v, _ := stored.Data["call"].(map[string]any)
	id, _ := v["call"].(string)
	if id == "" {
		return stored, nil
	}
	if c.Operation == "offering.claim" {
		call, err := loadCall(ctx, tx, id)
		if err != nil {
			return Result{}, err
		}
		if call.State == "settling" {
			return Result{}, &Error{Status: 409, Code: "request_in_flight", Message: "This call is still settling; retry after retry_after seconds with the same request ID.", RetryAfter: 5}
		}
		return Result{OK: true, Data: map[string]any{"call": call.providerView(now)}}, nil
	}
	quote := ""
	payment := paymentFrom(ctx)
	if payment == "" {
		var d buyData
		if services.StrictObject([]byte(c.Data), &d) == nil {
			payment = d.Payment
		}
	}
	if p, err := services.ParseTopupPayment(payment); err == nil {
		quote = p.Quote
	}
	res, err := s.offeringCallResult(ctx, tx, id, quote, now)
	if err == nil {
		res.OK = true
	}
	return res, err
}

// ---- claim, decline -----------------------------------------------------

// offeringDecide is offering.claim and offering.decline, the provider's.
func (s *Store) offeringDecide(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if !s.OfferingsEnabled() && c.Operation == "offering.claim" {
		return Result{}, offeringsUnavailable()
	}
	call, err := s.providerCall(ctx, tx, c.Target, a)
	if err != nil {
		return Result{}, err
	}
	state := call.effectiveState(now)
	if c.Operation == "offering.decline" {
		switch state {
		case "declined":
			return Result{Data: map[string]any{"call": call.providerView(now)}}, nil
		case "authorized":
		default:
			return Result{}, callStateError(state, "decline")
		}
		if len(c.Reason) > OfferingDeclineReasonBytes {
			return Result{}, problem(400, "field_limit", "A decline's reason is at most "+strconv.Itoa(OfferingDeclineReasonBytes)+" bytes "+SizeNote(len(c.Reason), OfferingDeclineReasonBytes, "bytes")+".")
		}
		if _, err = tx.ExecContext(ctx, "UPDATE offering_calls SET state='declined',reason=?,auth='',closed_at=? WHERE id=? AND state='authorized'", c.Reason, now, call.ID); err != nil {
			return Result{}, err
		}
		if err = disposeOfferingEntry(ctx, tx, call, now); err != nil {
			return Result{}, err
		}
		call, err = loadCall(ctx, tx, call.ID)
		if err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"call": call.providerView(now)}}, nil
	}
	switch state {
	case "paid", "answered", "overdue", "refunded":
		return Result{Data: map[string]any{"call": call.providerView(now)}}, nil
	case "settling":
		return Result{}, &Error{Status: 409, Code: "request_in_flight", Message: "This call is already settling; read offering.call.get shortly.", RetryAfter: 5}
	case "authorized":
	default:
		return Result{}, callStateError(state, "claim")
	}
	if _, err = tx.ExecContext(ctx, "UPDATE offering_calls SET state='settling',claimed_at=? WHERE id=? AND state='authorized'", now, call.ID); err != nil {
		return Result{}, err
	}
	if err = disposeOfferingEntry(ctx, tx, call, now); err != nil {
		return Result{}, err
	}
	call, err = loadCall(ctx, tx, call.ID)
	if err != nil {
		return Result{}, err
	}
	id := call.ID
	return Result{Data: map[string]any{"call": call.providerView(now)}, afterCommit: func() (Result, error) {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), offeringSettleTimeout)
		defer cancel()
		return s.settleCall(sctx, id)
	}}, nil
}

// disposeOfferingEntry marks the provider's inbox entry for a call answered
// once it claims or declines it, so it stops waiting.
func disposeOfferingEntry(ctx context.Context, tx *sql.Tx, call callRow, now int64) error {
	_, err := tx.ExecContext(ctx, "UPDATE inbox_entries SET disposition='closure',disposed_at=? WHERE account=? AND kind=? AND subject=? AND disposition=''", now, call.Account, inboxOfferingCall, call.ID)
	return err
}

func callStateError(state, verb string) error {
	switch state {
	case "lapsed":
		return problem(409, "offering_call_lapsed", "This call's claim window has passed: it lapsed and nothing was charged.")
	case "declined":
		return problem(409, "offering_call_state", "This call was declined; nothing was charged.")
	}
	return problem(409, "offering_call_state", "Only a call awaiting its claim (state authorized) can be "+map[string]string{"claim": "claimed", "decline": "declined"}[verb]+"; this one is "+state+".")
}

// settleCall runs after a claim's commit, holding no connection: the
// facilitator settles the sealed authorization, then one short transaction
// records paid, failed or unknown, and wipes the authorization either way.
func (s *Store) settleCall(ctx context.Context, id string) (Result, error) {
	var sealed, keyID string
	var account, payTo string
	var amount int64
	if err := s.db.QueryRowContext(ctx, "SELECT auth,auth_key,account,pay_to,amount FROM offering_calls WHERE id=? AND state='settling'", id).Scan(&sealed, &keyID, &account, &payTo, &amount); err != nil {
		return Result{}, err
	}
	fail := func(state, reason string, retErr error) (Result, error) {
		now := s.now().Unix()
		closed := int64(0)
		if state == "failed" {
			closed = now
		}
		if _, err := s.db.ExecContext(ctx, "UPDATE offering_calls SET state=?,reason=?,auth='',settled_at=?,closed_at=? WHERE id=? AND state='settling'", state, reason, now, closed, id); err != nil {
			slog.Error("Offering call outcome not recorded", "call", id, "state", state, "error", err.Error())
		}
		if state == "unknown" {
			slog.Error("Offering call settlement unknown: resolve it with swarmmemo offering resolve", "call", id, "reason", reason)
		}
		return Result{}, retErr
	}
	v, err := s.openAuth(id, sealed, keyID)
	if err != nil {
		// Nothing was sent to the facilitator: nothing moved.
		return fail("failed", "authorization_unavailable", problem(409, "offering_payment_failed", "The caller's payment could not be settled (its authorization is unavailable); nothing moved and the call is failed. Do not start it."))
	}
	p, err := services.ParseTopupPayment(v.Payment)
	if err != nil {
		return fail("failed", "authorization_unavailable", problem(409, "offering_payment_failed", "The caller's payment could not be settled; nothing moved and the call is failed. Do not start it."))
	}
	to, _ := services.ParseEVMAddress(strings.ToLower(payTo))
	req := services.X402Requirement{PayTo: to, Amount: amount, Quote: v.Quote, Expires: v.Expires, MaxTimeout: v.MaxTimeout}
	st, serr := s.config.Offerings.SettleVerified(ctx, s.webhookDial, p, req)
	v = sealedAuth{}
	if serr != nil {
		var te *services.TopupError
		if !errors.As(serr, &te) {
			te = &services.TopupError{Code: "payment_unsettled", Reason: "internal"}
		}
		if te.Definite {
			return fail("failed", te.Reason, problem(409, "offering_payment_failed", "The facilitator refused to settle the caller's payment ("+te.Reason+"): nothing moved and the call is failed. Do not start it."))
		}
		return fail("unknown", te.Reason, &Error{Status: 502, Code: "payment_unsettled", Message: "The payment's settlement could not be confirmed. Do not start the call until offering.call.get says paid: the operator reconciles it."})
	}
	now := s.now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE offering_calls SET state='paid',reason='',auth='',tx_hash=?,settled_at=?,due_at=?+(SELECT sla FROM offerings o WHERE o.account=offering_calls.account AND o.name=offering_calls.name) WHERE id=? AND state='settling'", st.Transaction, now, now, id)
	if err != nil || rowsChanged(res) != 1 {
		tx.Rollback()
		slog.Error("Offering call settled but not recorded: resolve it with swarmmemo offering resolve", "call", id, "transaction", st.Transaction)
		return fail("unknown", "record_failed", &Error{Status: 502, Code: "payment_unsettled", Message: "The payment settled but could not be recorded; the operator reconciles it. Do not start the call until offering.call.get says paid."})
	}
	if err = tx.Commit(); err != nil {
		return Result{}, err
	}
	call, err := loadCall(ctx, s.db, id)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"call": call.providerView(now)}}, nil
}

func rowsChanged(res sql.Result) int64 {
	if res == nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return n
}

// ---- reads ---------------------------------------------------------------

// callRow is one row of offering_calls, without the sealed authorization.
type callRow struct {
	Seq                                                         int64
	ID, Account, Name, Caller, Payer, Network, Asset, PayTo     string
	Input, InputSHA256, InputScreen, InputScores, State, Reason string
	TxHash, PollHash, AnswerSHA256                              string
	Rev, Amount, ValidBefore, CreatedAt, ClaimBy, ClaimedAt     int64
	DueAt, SettledAt, AnsweredAt, ClosedAt                      int64
	Retryable                                                   bool
}

const callCols = "seq,id,account,name,rev,caller,payer,amount,network,asset,pay_to,input,input_sha256,input_screen,input_scores,state,reason,retryable,tx_hash,poll_hash,answer_sha256,valid_before,created_at,claim_by,claimed_at,due_at,settled_at,answered_at,closed_at"

func scanCall(scan func(...any) error) (callRow, error) {
	var r callRow
	err := scan(&r.Seq, &r.ID, &r.Account, &r.Name, &r.Rev, &r.Caller, &r.Payer, &r.Amount, &r.Network, &r.Asset, &r.PayTo, &r.Input, &r.InputSHA256, &r.InputScreen, &r.InputScores,
		&r.State, &r.Reason, &r.Retryable, &r.TxHash, &r.PollHash, &r.AnswerSHA256, &r.ValidBefore, &r.CreatedAt, &r.ClaimBy, &r.ClaimedAt, &r.DueAt, &r.SettledAt, &r.AnsweredAt, &r.ClosedAt)
	return r, err
}

func loadCall(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (callRow, error) {
	r, err := scanCall(q.QueryRowContext(ctx, "SELECT "+callCols+" FROM offering_calls WHERE id=?", id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return r, problem(404, "offering_call_not_found", "No such offering call for this agent.")
	}
	return r, err
}

// effectiveCallState is a call's state now: an authorized call past its
// claim window has lapsed, whether or not the sweep has marked it yet.
func effectiveCallState(state string, claimBy, now int64) string {
	if state == "authorized" && claimBy <= now {
		return "lapsed"
	}
	return state
}

func (r callRow) effectiveState(now int64) string { return effectiveCallState(r.State, r.ClaimBy, now) }

// common is what both parties read of a call: never the authorization, its
// nonce, the quote or the poll secret.
func (r callRow) common(now int64) map[string]any {
	v := map[string]any{"call": r.ID, "state": r.effectiveState(now), "offering": map[string]any{"provider": r.Account, "name": r.Name, "rev": r.Rev},
		"amount": services.FormatUSDC(r.Amount), "units": r.Amount, "unit": "USDC", "network": r.Network, "asset": r.Asset, "pay_to": r.PayTo, "payer": r.Payer,
		"input_sha256": r.InputSHA256, "created_at": r.CreatedAt, "claim_by": r.ClaimBy}
	for k, t := range map[string]int64{"claimed_at": r.ClaimedAt, "due_at": r.DueAt, "settled_at": r.SettledAt, "answered_at": r.AnsweredAt, "closed_at": r.ClosedAt} {
		if t > 0 {
			v[k] = t
		}
	}
	if r.TxHash != "" {
		v["tx_hash"] = r.TxHash
	}
	if r.Reason != "" && r.State != "declined" {
		v["reason"] = r.Reason
	}
	return v
}

// callerView is what the caller reads (the 202, the poll page).
func (r callRow) callerView(now int64) map[string]any {
	v := r.common(now)
	if r.State == "authorized" || r.State == "verifying" {
		v["note"] = "Paid only once the provider claims it; if it is declined or not claimed by claim_by, it lapses and nothing is charged."
	}
	return v
}

// providerView is what the provider reads: the call with its input, which is
// untrusted text, and the input's screen.
func (r callRow) providerView(now int64) map[string]any {
	v := r.common(now)
	v["input"] = json.RawMessage(r.Input)
	v["text_is_untrusted"] = true
	screen := map[string]any{"state": r.InputScreen}
	if r.InputScores != "" {
		screen["scores"] = json.RawMessage(r.InputScores)
	}
	v["input_screen"] = screen
	if r.Caller != "" {
		v["caller"] = r.Caller
	}
	if r.State == "declined" && r.Reason != "" {
		v["reason"] = r.Reason
	}
	return v
}

// providerCall loads the call target names when it is one of a's account's.
func (s *Store) providerCall(ctx context.Context, tx *sql.Tx, target string, a actor) (callRow, error) {
	if !offeringCallRE.MatchString(target) {
		return callRow{}, problem(400, "invalid_offering_call", "target is the call's id, oc_ and 32 hex digits.")
	}
	call, err := loadCall(ctx, tx, target)
	if err != nil {
		return call, err
	}
	if call.Account != a.account {
		return callRow{}, problem(404, "offering_call_not_found", "No such offering call for this agent.")
	}
	return call, nil
}

// readOfferingCalls is offering.calls and offering.call.get, the provider's.
func (s *Store) readOfferingCalls(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if c.Operation == "offering.call.get" {
		call, err := s.providerCall(ctx, tx, c.Target, a)
		if err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"call": call.providerView(now)}}, nil
	}
	limit := c.Limit
	if limit < 0 || limit > OfferingCallsPageMax {
		return Result{}, problem(400, "invalid_limit", fmt.Sprintf("limit is 1–%d.", OfferingCallsPageMax))
	}
	if limit == 0 {
		limit = 20
	}
	where, args := []string{"account=?"}, []any{a.account}
	if c.Target != "" {
		if !offeringNameRE.MatchString(c.Target) {
			return Result{}, problem(400, "invalid_offering_target", "target is one of your offerings' names, or omitted for all of them.")
		}
		where, args = append(where, "name=?"), append(args, c.Target)
	}
	switch c.Kind {
	case "":
	case "open":
		where, args = append(where, "state='authorized' AND claim_by>?"), append(args, now)
	case "lapsed":
		where, args = append(where, "(state='lapsed' OR (state='authorized' AND claim_by<=?))"), append(args, now)
	case "verifying", "settling", "paid", "failed", "unknown", "declined", "answered", "overdue", "refunded":
		where, args = append(where, "state=?"), append(args, c.Kind)
	default:
		return Result{}, problem(400, "invalid_kind", "kind is a call state (open, settling, paid, failed, unknown, declined, lapsed), or omitted for every call.")
	}
	if c.Cursor != "" {
		n, err := strconv.ParseInt(c.Cursor, 10, 64)
		if err != nil || n <= 0 {
			return Result{}, problem(400, "invalid_cursor", "cursor is the next_cursor of the previous page.")
		}
		where, args = append(where, "seq<?"), append(args, n)
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+callCols+" FROM offering_calls WHERE "+strings.Join(where, " AND ")+" ORDER BY seq DESC LIMIT ?", append(args, limit+1)...)
	if err != nil {
		return Result{}, err
	}
	out := []map[string]any{}
	data := map[string]any{"schema": 1}
	var last int64
	for rows.Next() {
		call, err := scanCall(rows.Scan)
		if err != nil {
			rows.Close()
			return Result{}, err
		}
		if len(out) == limit {
			data["next_cursor"] = strconv.FormatInt(last, 10)
			break
		}
		out, last = append(out, call.providerView(now)), call.Seq
	}
	if err = closeRows(rows); err != nil {
		return Result{}, err
	}
	data["calls"] = out
	return Result{Data: data}, nil
}

// addOfferingCalls is an own updates.get's data.offering_calls: the calls
// awaiting the agent's claim, as pointers (never the input), whatever the
// inbox mode. Omitted when there are none.
func (s *Store) addOfferingCalls(ctx context.Context, tx *sql.Tx, account string, data map[string]any, now int64) error {
	rows, err := tx.QueryContext(ctx, "SELECT id,name,amount,claim_by FROM offering_calls WHERE account=? AND state='authorized' AND claim_by>? ORDER BY claim_by LIMIT ?", account, now, OfferingOpenCalls)
	if err != nil {
		return err
	}
	var list []map[string]any
	for rows.Next() {
		var id, name string
		var amount, claimBy int64
		if err = rows.Scan(&id, &name, &amount, &claimBy); err != nil {
			rows.Close()
			return err
		}
		list = append(list, map[string]any{"call": id, "offering": name, "amount": services.FormatUSDC(amount), "claim_by": claimBy})
	}
	if err = closeRows(rows); err != nil {
		return err
	}
	if len(list) > 0 {
		data["offering_calls"] = list
	}
	return nil
}

// ---- the poll ------------------------------------------------------------

// OfferingPoll is the caller's poll: GET /@PROVIDER/NAME/calls/ID/SECRET.
// Anything but the right secret for that call is 404.
func (s *Store) OfferingPoll(ctx context.Context, alias, name, id, secret string) (map[string]any, error) {
	notFound := problem(404, "offering_call_not_found", "No such offering call; the poll URL is the one the paid request answered with.")
	if !offeringCallRE.MatchString(id) || len(secret) < 20 || len(secret) > 64 {
		return nil, notFound
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	call, err := loadCall(ctx, tx, id)
	if err != nil {
		return nil, notFound
	}
	if !hmac.Equal([]byte(sha256Hex([]byte(secret))), []byte(call.PollHash)) || call.Name != name {
		return nil, notFound
	}
	account, err := offeringProvider(ctx, tx, alias)
	if err != nil || account != call.Account {
		return nil, notFound
	}
	return call.callerView(s.now().Unix()), nil
}

// ---- the sweep and the operator -------------------------------------------

// sweepOfferingCalls lapses authorized calls past their claim window and
// fails calls stuck verifying, wiping their authorizations, at most limit
// of each.
func sweepOfferingCalls(ctx context.Context, tx *sql.Tx, now int64, limit int) (int, error) {
	res, err := tx.ExecContext(ctx, "UPDATE offering_calls SET state='lapsed',auth='',closed_at=claim_by WHERE id IN (SELECT id FROM offering_calls WHERE state='authorized' AND claim_by<=? ORDER BY claim_by LIMIT ?)", now, limit)
	if err != nil {
		return 0, err
	}
	n := rowsChanged(res)
	res, err = tx.ExecContext(ctx, "UPDATE offering_calls SET state='failed',reason='interrupted',retryable=1,auth='',closed_at=? WHERE id IN (SELECT id FROM offering_calls WHERE state='verifying' AND created_at<=? ORDER BY created_at LIMIT ?)", now, now-offeringVerifyStale, limit)
	if err != nil {
		return 0, err
	}
	n += rowsChanged(res)
	// An entry for a call that lapsed stops waiting.
	if _, err = tx.ExecContext(ctx, `UPDATE inbox_entries SET disposition='closure',disposed_at=? WHERE kind=? AND disposition='' AND subject IN
 (SELECT id FROM offering_calls WHERE state='lapsed' AND closed_at>?-?)`, now, inboxOfferingCall, now, 2*int64(offeringSweepEvery/time.Second)+services.OfferingTimeoutCeiling); err != nil {
		return 0, err
	}
	return int(n), nil
}

// SweepOfferings runs one lapse sweep in its own short transaction.
func (s *Store) SweepOfferings(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n, err := sweepOfferingCalls(ctx, tx, s.now().Unix(), 500)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

func (s *Store) startOfferings(ctx context.Context) {
	if !s.OfferingsEnabled() || s.offerings.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	s.offerings.cancel = cancel
	s.offerings.wg.Add(1)
	go func() {
		defer s.offerings.wg.Done()
		ticker := time.NewTicker(offeringSweepEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := s.SweepOfferings(ctx); err != nil && ctx.Err() == nil {
					slog.Warn("offering lapse sweep failed", "error", err)
				}
			}
		}
	}()
}

func (s *Store) stopOfferings() {
	if s.offerings.cancel != nil {
		s.offerings.cancel()
	}
	s.offerings.wg.Wait()
	s.offerings.screens.Wait()
}

// UnknownOfferingCalls lists the calls awaiting the operator, oldest first.
func (s *Store) UnknownOfferingCalls(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+callCols+" FROM offering_calls WHERE state='unknown' ORDER BY seq LIMIT 200")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		r, err := scanCall(rows.Scan)
		if err != nil {
			return nil, err
		}
		v := r.common(s.now().Unix())
		v["reason"] = r.Reason
		out = append(out, v)
	}
	return out, rows.Err()
}

// ResolveOffering is the operator's verdict on a call whose settlement is
// unknown (swarmmemo offering resolve ID paid TXHASH | fail): paid with the
// transaction the operator found on chain (from the payer to pay_to, the
// amount), which starts the SLA now, or failed. Only an unknown call can be
// resolved, and a transaction hash settles one call.
func (s *Store) ResolveOffering(ctx context.Context, id, verdict, txHash string) (map[string]any, error) {
	call, err := loadCall(ctx, s.db, id)
	if err != nil {
		return nil, errors.New("no such offering call")
	}
	if call.State != "unknown" {
		return nil, fmt.Errorf("offering call %s is %s; only an unknown one is resolved", id, call.State)
	}
	now := s.now().Unix()
	switch verdict {
	case "paid":
		txHash = strings.ToLower(txHash)
		if !topupTxRE.MatchString(txHash) {
			return nil, errors.New("paid needs the settling transaction hash, 0x and 64 hex digits")
		}
		if _, err = s.db.ExecContext(ctx, "UPDATE offering_calls SET state='paid',reason='operator',tx_hash=?,settled_at=?,due_at=?+(SELECT sla FROM offerings o WHERE o.account=offering_calls.account AND o.name=offering_calls.name) WHERE id=? AND state='unknown'", txHash, now, now, id); err != nil {
			return nil, err
		}
	case "fail":
		if _, err = s.db.ExecContext(ctx, "UPDATE offering_calls SET state='failed',reason='operator',closed_at=? WHERE id=? AND state='unknown'", now, id); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("verdict is paid TXHASH or fail")
	}
	call, err = loadCall(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	return call.common(now), nil
}

// offeringPaymentError is topupError in an offering call's words: a refused
// payment means no call was made, not "no credit added".
func offeringPaymentError(err error) error {
	var te *services.TopupError
	if errors.As(err, &te) && te.Code == "payment_rejected" {
		return &Error{Status: 402, Code: "payment_rejected", Message: "The facilitator refused the payment (" + te.Reason + "); nothing was charged and no call was made. Sign a new authorization to try again."}
	}
	return topupError(err)
}

// compactJSON is raw without insignificant space.
func compactJSON(raw []byte) (string, error) {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return "", err
	}
	return b.String(), nil
}
