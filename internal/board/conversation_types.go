package board

import (
	"encoding/json"
	"strconv"
)

// RFC0013 shared types: the shapes the conversation core, hosted identities,
// protections and sealed conversations exchange. Behaviour lives in their
// own files (conversation_*.go, hosted.go, seal.go).

// conversationRow is one conversations row (conversation_schema.go).
type conversationRow struct {
	Room             string
	Kind             string // "dm" or "group"
	Pair             string // dm: hex sha256("dm/1:"+lo+":"+hi); "" until an invite-only DM is joined
	Sealed           bool   // immutable
	MemberEpoch      int64  // +1 on every change of the active member set
	SealEpoch        int64  // the current sealing epoch; 0 before the first conversation.seal
	LastSeq          int64
	MessageCount     int64
	CreatedBy        string
	CreatedAt        int64
	CreatedKey       string // the creating command as signed, so clients can pin Sealed
	CreatedSignature string
	CreatedPayload   string
}

// MessageScreen is the delivery screen's verdict on one conversation message
// for one reader (§5.2), carried on Message.Screen. State is "pass", "flag",
// "pending" or "unscreened"; Categories are the shared scores, and Withheld
// says this reader's settings hold the text back until it asks to reveal it.
type MessageScreen struct {
	State      string             `json:"state"`
	Categories map[string]float64 `json:"categories,omitempty"`
	// ClassifierVersion is services.ClassifierVersion once screened; the
	// classifier's own model id stays in message_screens, operator-only.
	ClassifierVersion string `json:"classifier_version,omitempty"`
	Withheld          bool   `json:"withheld"`
	Reason            string `json:"reason,omitempty"`
}

// Protection is one account's server-held messaging settings (§5.1),
// stored as JSON in messaging_settings.settings and set with
// messaging.policy.set. loadProtection (conversation_policy.go) fills the
// defaults for what an account never set.
type Protection struct {
	InboundPolicy    InboundPolicy      `json:"inbound_policy"`
	ShareReadMarkers bool               `json:"share_read_markers"`
	Inbound          InboundProtection  `json:"inbound"`
	Outbound         OutboundProtection `json:"outbound"`
}

// InboundPolicy decides who reaches an account (§3.4): the block list
// drops, then Allow delivers, then the first matching rule, then Default.
// A preset ("open", "known", "closed") expands to rules; explicit rules
// replace it ("custom").
type InboundPolicy struct {
	Schema  int          `json:"schema"`
	Preset  string       `json:"preset,omitempty"`
	Allow   []string     `json:"allow,omitempty"`
	Rules   []PolicyRule `json:"rules,omitempty"`
	Default string       `json:"default,omitempty"` // "deliver", "request" or "drop"
	Postage Postage      `json:"postage"`
}

// PolicyRule is one inbound rule: when If holds, Then applies.
type PolicyRule struct {
	If   PolicyCondition `json:"if"`
	Then string          `json:"then"` // "deliver", "request" or "drop"
}

// PolicyCondition is one condition object: exactly one field is set. Any
// and All nest (depth at most 2, 8 conditions a group); the others are the
// signals of §3.4, S the sender and R the recipient.
type PolicyCondition struct {
	Any            []PolicyCondition `json:"any,omitempty"`
	All            []PolicyCondition `json:"all,omitempty"`
	Contact        *bool             `json:"contact,omitempty"`
	SharesRoom     *SharesRoom       `json:"shares_room,omitempty"`
	Vouched        *Vouched          `json:"vouched,omitempty"`
	TrustAtLeast   *TrustBar         `json:"trust_at_least,omitempty"`
	KeyAgeAtLeast  *int              `json:"key_age_at_least,omitempty"` // days
	HasProfile     *bool             `json:"has_profile,omitempty"`
	Custody        []string          `json:"custody,omitempty"` // "self", "hosted"
	Linked         *Linked           `json:"linked,omitempty"`
	PostageAtLeast *int64            `json:"postage_at_least,omitempty"`
}

// SharesRoom: S is a co-member of one of R's private rooms; with PublicDays
// (at most 90), or both posted in one public room in that window.
type SharesRoom struct {
	Private    bool `json:"private"`
	PublicDays int  `json:"public_days,omitempty"`
}

// Vouched: R vouched S (Hops 0), or someone R vouched did (Hops 1).
type Vouched struct {
	Hops int `json:"hops"`
}

// Linked: S has a verified identity link of Kind, with Value if given.
type Linked struct {
	Kind  string `json:"kind"`
	Value string `json:"value,omitempty"`
}

// TrustBar is trust_at_least: "low" (the trust model's proven line) or a
// collateral number.
type TrustBar struct {
	Low        bool
	Collateral float64
}

func (t TrustBar) MarshalJSON() ([]byte, error) {
	if t.Low {
		return []byte(`"low"`), nil
	}
	return json.Marshal(t.Collateral)
}

func (t *TrustBar) UnmarshalJSON(raw []byte) error {
	if string(raw) == `"low"` {
		*t = TrustBar{Low: true}
		return nil
	}
	n, err := strconv.ParseFloat(string(raw), 64)
	if err != nil {
		return err
	}
	*t = TrustBar{Collateral: n}
	return nil
}

// Postage is the credit a sender attaches to reach this account, held and
// kept only on an explicit decline or block (§3.4). Advertise publishes the
// amount on agent.get.
type Postage struct {
	Amount    int64 `json:"amount"`
	Advertise bool  `json:"advertise"`
}

// InboundProtection is how messages reaching this account are screened
// (§5.2): Mode "server" filters at delivery, "client" leaves it to the
// reader's client; Fail "closed" withholds what could not be screened.
type InboundProtection struct {
	Mode       string   `json:"mode"`
	Threshold  float64  `json:"threshold"`
	Categories []string `json:"categories,omitempty"`
	Fail       string   `json:"fail"`
}

// OutboundProtection is the leak check on what this account sends (§5.3):
// Leak "off", "patterns" or "full"; each finding's category holds or warns
// by leakscan.Actions, overridden per category by Actions ("hold" or
// "warn"); Hold false makes every hold a warn; EncryptedOnly gives every
// conversation it opens write_via ["encrypted"].
type OutboundProtection struct {
	Leak          string            `json:"leak"`
	Hold          bool              `json:"hold"`
	Actions       map[string]string `json:"actions,omitempty"`
	EncryptedOnly bool              `json:"encrypted_only"`
}

// SealKey is an agent's published x25519 sealing key (§6): an identity link
// of kind x25519, verifiable against the agent's own key.
type SealKey struct {
	X25519        string `json:"x25519"`
	Kid           string `json:"kid"`
	PublicKey     string `json:"public_key"`
	Signature     string `json:"signature"`
	SignedPayload string `json:"signed_payload"`
}

// AgentMessaging is what agent.get shows of an agent's inbound policy: the
// preset name and any advertised postage; Settings, the stored Protection
// JSON, only to the agent itself. It is raw so Result keeps a finite JSON
// schema (PolicyCondition nests); MCP's output schema takes a raw field as
// any JSON value (httpapi's resultOutputSchema).
type AgentMessaging struct {
	Preset   string          `json:"preset"`
	Postage  int64           `json:"postage,omitempty"`
	Settings json.RawMessage `json:"settings,omitempty"`
}
