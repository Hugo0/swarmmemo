package services

// The x402 wire format (docs.x402.org; x402 specification v1 and v2, HTTP
// transport): a 402 response carries PaymentRequired, the retry carries a
// PaymentPayload, and the paid response may carry a SettlementResponse.
// Everything here is pure: bounded parsing of what an upstream sent, and
// building what we send back.

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// x402 wire bounds.
const (
	// x402HeaderBytes bounds a base64 PAYMENT-REQUIRED or PAYMENT-RESPONSE header.
	x402HeaderBytes = 16 << 10
	// x402RequiredBodyBytes bounds a 402 response body we read.
	x402RequiredBodyBytes = 16 << 10
	// x402AcceptsMax bounds the payment options we look at.
	x402AcceptsMax = 16
	// x402RawFieldBytes bounds the resource and extensions objects we echo.
	x402RawFieldBytes = 4 << 10
)

// Header names, per transport version.
const (
	x402V2Required  = "PAYMENT-REQUIRED"
	x402V2Signature = "PAYMENT-SIGNATURE"
	x402V2Response  = "PAYMENT-RESPONSE"
	x402V1Signature = "X-PAYMENT"
	x402V1Response  = "X-PAYMENT-RESPONSE"
)

// x402V1Networks maps the v1 network names to CAIP-2 identifiers, for the
// EVM networks x402's reference implementation lists.
var x402V1Networks = map[string]string{
	"base": "eip155:8453", "base-sepolia": "eip155:84532",
	"polygon": "eip155:137", "polygon-amoy": "eip155:80002",
	"avalanche": "eip155:43114", "avalanche-fuji": "eip155:43113",
	"arbitrum": "eip155:42161", "arbitrum-sepolia": "eip155:421614",
	"ethereum": "eip155:1", "sepolia": "eip155:11155111",
}

// x402Required is a parsed PaymentRequired (v2) or payment requirements
// response (v1).
type x402Required struct {
	Version    int
	Resource   json.RawMessage // v2 ResourceInfo, echoed in the payload
	Accepts    []json.RawMessage
	Extensions json.RawMessage // v2, echoed in the payload
}

type x402RequiredWire struct {
	Version    json.Number       `json:"x402Version"`
	Resource   json.RawMessage   `json:"resource"`
	Accepts    []json.RawMessage `json:"accepts"`
	Extensions json.RawMessage   `json:"extensions"`
}

// x402Requirement is one entry of "accepts". v1 names the amount
// maxAmountRequired; v2 names it amount (some v2 servers send both).
type x402Requirement struct {
	Scheme            string          `json:"scheme"`
	Network           string          `json:"network"`
	Amount            string          `json:"amount"`
	MaxAmountRequired string          `json:"maxAmountRequired"`
	Asset             string          `json:"asset"`
	PayTo             string          `json:"payTo"`
	MaxTimeoutSeconds json.Number     `json:"maxTimeoutSeconds"`
	Extra             json.RawMessage `json:"extra"`
}

type x402Extra struct {
	Name                string `json:"name"`
	Version             string `json:"version"`
	AssetTransferMethod string `json:"assetTransferMethod"`
}

func decodeBase64JSON(s string) ([]byte, bool) {
	if s == "" || len(s) > x402HeaderBytes {
		return nil, false
	}
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, true
		}
	}
	return nil, false
}

// boundedJSON decodes a bounded, valid UTF-8 JSON object leniently: an
// upstream's unknown fields are ignored, never trusted.
func boundedJSON(raw []byte, max int, dst any) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > max || raw[0] != '{' || !utf8.Valid(raw) || !json.Valid(raw) {
		return false
	}
	if checkStructure(raw) != nil { // one object, no duplicate keys, bounded depth
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(dst) == nil
}

// parseX402Required reads a 402 response: the v2 PAYMENT-REQUIRED header when
// present, else the body (v1, or v2 servers that also send it there).
func parseX402Required(header string, body []byte) (x402Required, bool) {
	var w x402RequiredWire
	ok := false
	if header != "" {
		if raw, decoded := decodeBase64JSON(header); decoded {
			ok = boundedJSON(raw, x402HeaderBytes, &w)
		}
	}
	if !ok {
		w = x402RequiredWire{}
		ok = boundedJSON(body, x402RequiredBodyBytes, &w)
	}
	if !ok {
		return x402Required{}, false
	}
	v, err := strconv.Atoi(w.Version.String())
	if err != nil || (v != 1 && v != 2) || len(w.Accepts) == 0 {
		return x402Required{}, false
	}
	if len(w.Accepts) > x402AcceptsMax {
		w.Accepts = w.Accepts[:x402AcceptsMax]
	}
	r := x402Required{Version: v, Accepts: w.Accepts}
	if v == 2 {
		if isObject(w.Resource) && len(w.Resource) <= x402RawFieldBytes {
			r.Resource = w.Resource
		}
		if isObject(w.Extensions) && len(w.Extensions) <= x402RawFieldBytes {
			r.Extensions = w.Extensions
		}
	}
	return r, true
}

func isObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 1 && raw[0] == '{'
}

var atomicAmountRE = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)

// x402Choice is the payment option we will sign for.
type x402Choice struct {
	Version     int
	Network     string // as the upstream named it (v1 name or CAIP-2)
	Accepted    json.RawMessage
	Amount      int64
	PayTo       EVMAddress
	TimeoutSecs int64
}

// x402Want is what an option must match to be payable: our network, asset
// and token domain, the allowlisted recipient, and at most Max atomic units.
type x402Want struct {
	Network      string // CAIP-2
	Asset        EVMAddress
	AssetName    string
	AssetVersion string
	PayTo        EVMAddress
	Max          int64
}

// chooseX402 picks the first "exact" EIP-3009 option that matches want. It
// refuses with x402_price_changed when a matching option asks more than
// want.Max, and x402_not_payable when nothing matches.
func chooseX402(r x402Required, want x402Want) (x402Choice, error) {
	tooDear := false
	for _, raw := range r.Accepts {
		var q x402Requirement
		if !boundedJSON(raw, x402RawFieldBytes, &q) || q.Scheme != "exact" {
			continue
		}
		network := q.Network
		if r.Version == 1 {
			network = x402V1Networks[q.Network]
		}
		if network == "" || network != want.Network {
			continue
		}
		asset, ok := ParseEVMAddress(q.Asset)
		if !ok || asset != want.Asset {
			continue
		}
		payTo, ok := ParseEVMAddress(q.PayTo)
		if !ok || payTo != want.PayTo {
			continue
		}
		if len(q.Extra) > 0 && string(q.Extra) != "null" {
			var extra x402Extra
			if !boundedJSON(q.Extra, x402RawFieldBytes, &extra) {
				continue
			}
			if (extra.Name != "" && extra.Name != want.AssetName) || (extra.Version != "" && extra.Version != want.AssetVersion) ||
				(extra.AssetTransferMethod != "" && extra.AssetTransferMethod != "eip3009") {
				continue
			}
		}
		amountText := q.Amount
		if amountText == "" {
			amountText = q.MaxAmountRequired
		}
		if !atomicAmountRE.MatchString(amountText) {
			continue
		}
		amount, err := strconv.ParseInt(amountText, 10, 64)
		if err != nil {
			continue
		}
		if amount > want.Max {
			tooDear = true
			continue
		}
		timeout := int64(60)
		if t, err := strconv.ParseInt(q.MaxTimeoutSeconds.String(), 10, 64); err == nil && t > 0 {
			timeout = t
		}
		return x402Choice{Version: r.Version, Network: q.Network, Accepted: raw, Amount: amount, PayTo: payTo, TimeoutSecs: timeout}, nil
	}
	if tooDear {
		return x402Choice{}, refusal("x402_price_changed")
	}
	return x402Choice{}, refusal("x402_not_payable")
}

type x402AuthorizationWire struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Value       string `json:"value"`
	ValidAfter  string `json:"validAfter"`
	ValidBefore string `json:"validBefore"`
	Nonce       string `json:"nonce"`
}

type x402ExactPayload struct {
	Signature     string                `json:"signature"`
	Authorization x402AuthorizationWire `json:"authorization"`
}

type x402PayloadV2 struct {
	Version    int              `json:"x402Version"`
	Resource   json.RawMessage  `json:"resource,omitempty"`
	Accepted   json.RawMessage  `json:"accepted"`
	Payload    x402ExactPayload `json:"payload"`
	Extensions json.RawMessage  `json:"extensions,omitempty"`
}

type x402PayloadV1 struct {
	Version int              `json:"x402Version"`
	Scheme  string           `json:"scheme"`
	Network string           `json:"network"`
	Payload x402ExactPayload `json:"payload"`
}

// x402PaymentHeader is the header name and base64 value that carry a signed
// authorization back to the resource server.
func x402PaymentHeader(r x402Required, c x402Choice, a TransferAuthorization, sig [65]byte) (string, string, error) {
	p := x402ExactPayload{
		Signature: "0x" + hex.EncodeToString(sig[:]),
		Authorization: x402AuthorizationWire{
			From: a.From.String(), To: a.To.String(), Value: strconv.FormatInt(a.Value, 10),
			ValidAfter: strconv.FormatInt(a.ValidAfter, 10), ValidBefore: strconv.FormatInt(a.ValidBefore, 10),
			Nonce: "0x" + hex.EncodeToString(a.Nonce[:]),
		},
	}
	var body []byte
	var err error
	name := x402V2Signature
	if c.Version == 1 {
		name = x402V1Signature
		body, err = json.Marshal(x402PayloadV1{Version: 1, Scheme: "exact", Network: c.Network, Payload: p})
	} else {
		var accepted bytes.Buffer
		if err = json.Compact(&accepted, c.Accepted); err != nil {
			return "", "", err
		}
		body, err = json.Marshal(x402PayloadV2{Version: 2, Resource: r.Resource, Accepted: accepted.Bytes(), Payload: p, Extensions: r.Extensions})
	}
	if err != nil {
		return "", "", err
	}
	return name, base64.StdEncoding.EncodeToString(body), nil
}

// x402Settlement is the useful part of a SettlementResponse.
type x402Settlement struct {
	Success     bool
	Transaction string // "0x" + 64 hex, or ""
	Reason      string // a sanitised errorReason, or ""
}

var (
	txHashRE = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)
	reasonRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
)

// parseX402Settlement reads PAYMENT-RESPONSE (v2) or X-PAYMENT-RESPONSE (v1).
// Anything malformed reads as "no settlement information".
func parseX402Settlement(v2, v1 string) x402Settlement {
	h := v2
	if h == "" {
		h = v1
	}
	raw, ok := decodeBase64JSON(h)
	if !ok {
		return x402Settlement{}
	}
	var w struct {
		Success     bool   `json:"success"`
		Transaction string `json:"transaction"`
		ErrorReason string `json:"errorReason"`
	}
	if !boundedJSON(raw, x402HeaderBytes, &w) {
		return x402Settlement{}
	}
	s := x402Settlement{Success: w.Success}
	if txHashRE.MatchString(w.Transaction) {
		s.Transaction = strings.ToLower(w.Transaction)
	}
	if reasonRE.MatchString(w.ErrorReason) {
		s.Reason = w.ErrorReason
	}
	return s
}
