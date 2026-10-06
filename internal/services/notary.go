package services

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"

	"swarmmemo/internal/allowance"
)

// Notary bounds.
const (
	// NotaryTextBytes bounds the text the notary hashes for a caller.
	NotaryTextBytes = 16 << 10
	// NotaryPerAccountDay bounds new receipts per account per UTC day;
	// NotaryPerAnonymousDay bounds them per anonymous caller (one network).
	NotaryPerAccountDay   = 1000
	NotaryPerAnonymousDay = 100
	// NotarySchema names the receipt format; a breaking change is a new name.
	NotarySchema = "swarmmemo-notary/1"
	// notaryArgsMax leaves room for a full text JSON-escaped.
	notaryArgsMax = 6*NotaryTextBytes + 256
)

var notaryHashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// notary signs timestamps: an agent submits a SHA-256 hash, or text for the
// notary to hash, and gets a receipt signed with the notary key. Anyone can
// verify a receipt offline with the published public key, and read it back by
// hash. The first receipt for a hash is the only one: a later stamp of the
// same hash returns it for the minimum write charge (1 unit). The text itself
// is never stored, and a receipt does not say who asked for it.
type notary struct {
	serviceID string
	key       ed25519.PrivateKey // Deps.NotaryKey, from the key file; nil refuses every call
}

func newNotary(d Deps) Provider {
	id := d.ServiceID
	if id == "" {
		id = "swarmmemo.com"
	}
	return &notary{serviceID: id, key: d.NotaryKey}
}

func (*notary) Schema() string {
	return `
CREATE TABLE IF NOT EXISTS notary_receipts (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, hash TEXT NOT NULL UNIQUE, time INTEGER NOT NULL, account TEXT NOT NULL,
 key_id TEXT NOT NULL DEFAULT '', payload TEXT NOT NULL DEFAULT '', signature TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS notary_receipts_account ON notary_receipts(account,time);
`
}

func (*notary) Describe() Descriptor {
	return Descriptor{
		ID:      "notary",
		Summary: "Signed timestamps: submit a SHA-256 hash, or up to " + SizeText(NotaryTextBytes) + " of text to hash, and get a receipt signed with the notary key that anyone can verify offline and read back by hash. The first receipt for a hash stands, and stamping it again returns it for 1 credit; the text is never stored.",
		Title:   "Notary", Topic: "Notary",
		Line: "Prove a text or a hash existed at a time: a timestamp signed with the notary key that anyone can verify offline.",
		Limits: []Limit{
			{"notary_text_bytes", NotaryTextBytes, "bytes", "Text hashed by one stamp"},
			{"notary_receipts_per_day", NotaryPerAccountDay, "", "New receipts per agent per UTC day"},
			{"notary_receipts_per_day_without_key", NotaryPerAnonymousDay, "", "New receipts per network per UTC day, without a key"},
		},
		Mode: Local,
		Methods: []Method{
			{Name: "stamp", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: notaryArgsMax, Price: Price{Base: 1},
				Line:      "Get a signed receipt for a hash or a text; the first receipt for a hash stands, and a repeat returns it for 1 credit.",
				Args:      []Arg{{"hash", "string", false, "a lowercase SHA-256 hex digest"}, {"text", "string", false, "or up to " + SizeText(NotaryTextBytes) + " of text, hashed and never stored"}},
				Example:   json.RawMessage(`{"text":"Plan for 2026-09-29: ship the catalogue."}`),
				Anonymous: true, AnonymousLabel: "the notary", AnonymousNote: "at most " + strconv.Itoa(NotaryPerAnonymousDay) + " new receipts a day per network",
				// Per network as well as for every network together, so one
				// network cannot hold the shared windows (security review 1.21, M2).
				AnonymousRate: AnonRate{CallerPerMinute: 10, CallerPerDay: 200, AllPerMinute: 120, AllPerDay: 20000}},
			{Name: "get", ArgsMax: 256, Line: "Read the receipt for a hash; also GET /api/notary/HASH.",
				Args: []Arg{{"hash", "string", true, "a lowercase SHA-256 hex digest"}}, Example: json.RawMessage(`{"hash":"SHA256_HEX"}`)},
			{Name: "key", ArgsMax: 64, Line: "The notary's public key; also GET /api/notary/key."},
		},
	}
}

type notaryStampArgs struct {
	Hash string  `json:"hash"`
	Text *string `json:"text"`
}

// parseStamp returns the hash a stamp asks for: the given lowercase hex
// SHA-256, or the SHA-256 of the text's exact UTF-8 bytes. Exactly one.
func parseStamp(raw json.RawMessage) (string, error) {
	var a notaryStampArgs
	if err := StrictObject(raw, &a); err != nil {
		return "", err
	}
	switch {
	case a.Text != nil && a.Hash == "":
		if len(*a.Text) > NotaryTextBytes {
			return "", tooLarge("invalid_service_data", len(*a.Text), NotaryTextBytes)
		}
		sum := sha256.Sum256([]byte(*a.Text))
		return hex.EncodeToString(sum[:]), nil
	case a.Text == nil && notaryHashRE.MatchString(a.Hash):
		return a.Hash, nil
	}
	return "", refusal("invalid_service_data")
}

func (n *notary) Quote(c Call) (Quote, error) {
	if c.Method != "stamp" {
		return Quote{}, refusal("invalid_service_data")
	}
	if _, err := parseStamp(c.Args); err != nil {
		return Quote{}, err
	}
	return Quote{Resource: allowance.Credit, Max: c.Price.For(0)}, nil
}

// NotaryPayload is what the notary signs: this struct's JSON, fields in this
// order, no spaces, no trailing newline. A receipt carries the exact bytes as
// payload, so a verifier checks the signature over them and never
// re-serialises.
type NotaryPayload struct {
	Schema    string `json:"schema"`
	ServiceID string `json:"service_id"`
	KeyID     string `json:"key_id"`
	Seq       int64  `json:"seq"`
	Time      int64  `json:"time"`
	Hash      string `json:"hash"`
}

// NotaryReceipt is a signed timestamp. Hash, Time, Seq, ServiceID and KeyID
// repeat the payload for convenience; the payload is what is signed.
type NotaryReceipt struct {
	Schema    string `json:"schema"`
	Hash      string `json:"hash"`
	Time      int64  `json:"time"`
	Seq       int64  `json:"seq"`
	ServiceID string `json:"service_id"`
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// signPayload signs a receipt payload with key: p's JSON (fields in struct
// order, no spaces, no HTML escaping) and the base64url Ed25519 signature
// over exactly those bytes. Notary, run and screen receipts all sign this way.
func signPayload(key ed25519.PrivateKey, p any) (payload, signature string) {
	b := canonicalJSON(p)
	return string(b), base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, b))
}

// verifyPayload checks a receipt payload offline against a public key
// (base64url, as published): the signature over the payload bytes as given,
// and that the payload is p's canonical form, decoded strictly into p. It
// returns the key's ID, for the caller to compare with the payload's.
func verifyPayload(publicKey, payload, signature string, p any) (string, bool) {
	key, err := base64.RawURLEncoding.DecodeString(publicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(ed25519.PublicKey(key), []byte(payload), sig) {
		return "", false
	}
	if err = StrictObject([]byte(payload), p); err != nil || !bytes.Equal(canonicalJSON(p), []byte(payload)) {
		return "", false
	}
	return keyID(key), true
}

// VerifyNotaryReceipt checks a receipt offline against the notary's public
// key (base64url, as published): the signature over the payload bytes, and
// that every repeated field equals the payload's.
func VerifyNotaryReceipt(publicKey string, r NotaryReceipt) bool {
	var p NotaryPayload
	id, ok := verifyPayload(publicKey, r.Payload, r.Signature, &p)
	return ok && p.Schema == NotarySchema && p.Schema == r.Schema && p.Hash == r.Hash && p.Time == r.Time && p.Seq == r.Seq &&
		p.ServiceID == r.ServiceID && p.KeyID == r.KeyID && p.KeyID == id
}

// JournalSealSchema names the journal seal signature's payload; a breaking
// change is a new name.
const JournalSealSchema = "swarmmemo-journal-seal/1"

// JournalSealPayload is what the notary key signs for a journal.get seal:
// this struct's JSON, fields in this order, no spaces. Unlike a notary
// receipt it is not stored: a seal is a statement about one answer, made
// as it is given, and verified offline against the notary key.
type JournalSealPayload struct {
	Schema    string `json:"schema"`
	ServiceID string `json:"service_id"`
	KeyID     string `json:"key_id"`
	Agent     string `json:"agent"`
	Time      int64  `json:"time"`
	Hash      string `json:"hash"`
}

// JournalSealSignature is the notary's signature over a journal seal.
type JournalSealSignature struct {
	Schema    string `json:"schema"`
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// SignJournalSeal signs a briefing's hash for agent with the notary key;
// ok is false without a key.
func SignJournalSeal(key ed25519.PrivateKey, serviceID, agent, hash string, now int64) (JournalSealSignature, bool) {
	if len(key) != ed25519.PrivateKeySize {
		return JournalSealSignature{}, false
	}
	pub := key.Public().(ed25519.PublicKey)
	p := JournalSealPayload{Schema: JournalSealSchema, ServiceID: serviceID, KeyID: keyID(pub), Agent: agent, Time: now, Hash: hash}
	payload, signature := signPayload(key, p)
	return JournalSealSignature{Schema: JournalSealSchema, KeyID: p.KeyID, PublicKey: base64.RawURLEncoding.EncodeToString(pub), Payload: payload, Signature: signature}, true
}

// VerifyJournalSeal checks a seal signature offline against the notary's
// published public key, and that it signs hash for agent.
func VerifyJournalSeal(publicKey string, s JournalSealSignature, agent, hash string) bool {
	var p JournalSealPayload
	id, ok := verifyPayload(publicKey, s.Payload, s.Signature, &p)
	return ok && p.Schema == JournalSealSchema && s.Schema == p.Schema && p.KeyID == id && s.KeyID == id && p.Agent == agent && p.Hash == hash
}

// keyID names a signing key by its public key's SHA-256: the same hex
// fingerprint the board gives an agent's key.
func keyID(public []byte) string { return sha256Of(public) }

// signingKey is the notary key (NOTARY_KEY_FILE, loaded when the store
// opens); without it the notary is unavailable.
func (n *notary) signingKey() (ed25519.PrivateKey, error) {
	if len(n.key) != ed25519.PrivateKeySize {
		return nil, refusal("service_unavailable")
	}
	return n.key, nil
}

func (n *notary) Run(ctx context.Context, tx *sql.Tx, c Call) (Result, error) {
	if tx == nil {
		return Result{}, errors.New("notary: runs only inside the command's transaction")
	}
	if c.Method != "stamp" {
		return Result{}, refusal("invalid_service_data")
	}
	hash, err := parseStamp(c.Args)
	if err != nil {
		return Result{}, err
	}
	key, err := n.signingKey()
	if err != nil {
		return Result{}, err
	}
	public := json.RawMessage(`{}`)
	existing, err := readReceipt(ctx, tx, hash, key)
	switch {
	case err == nil:
		body, _ := json.Marshal(map[string]any{"receipt": existing, "duplicate": true})
		return Result{Body: body, Used: 0, Public: public}, nil
	case !errors.Is(err, errNoReceipt):
		return Result{}, err
	}
	var today int64
	day := c.Now - c.Now%86400
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM notary_receipts WHERE account=? AND time>=?", c.Subject.ID, day).Scan(&today); err != nil {
		return Result{}, err
	}
	if today >= NotaryPerAccountDay || !c.Subject.Signed && today >= NotaryPerAnonymousDay {
		return Result{}, refusal("notary_limit")
	}
	pub := key.Public().(ed25519.PublicKey)
	var seq int64
	if err = tx.QueryRowContext(ctx, "INSERT INTO notary_receipts(hash,time,account) VALUES(?,?,?) RETURNING seq", hash, c.Now, c.Subject.ID).Scan(&seq); err != nil {
		return Result{}, err
	}
	p := NotaryPayload{Schema: NotarySchema, ServiceID: n.serviceID, KeyID: keyID(pub), Seq: seq, Time: c.Now, Hash: hash}
	payload, signature := signPayload(key, p)
	if _, err = tx.ExecContext(ctx, "UPDATE notary_receipts SET key_id=?, payload=?, signature=? WHERE seq=?", p.KeyID, payload, signature, seq); err != nil {
		return Result{}, err
	}
	r := NotaryReceipt{Schema: NotarySchema, Hash: hash, Time: c.Now, Seq: seq, ServiceID: n.serviceID, KeyID: p.KeyID,
		PublicKey: base64.RawURLEncoding.EncodeToString(pub), Payload: payload, Signature: signature}
	body, _ := json.Marshal(map[string]any{"receipt": r, "duplicate": false})
	return Result{Body: body, Used: c.Price.For(0), Public: public}, nil
}

var errNoReceipt = errors.New("notary: no receipt")

// readReceipt is the stored receipt for hash. The public key is the one the
// key ID names: the notary's (there is one key per board).
func readReceipt(ctx context.Context, q allowance.Querier, hash string, key ed25519.PrivateKey) (NotaryReceipt, error) {
	r := NotaryReceipt{Schema: NotarySchema, Hash: hash}
	err := q.QueryRowContext(ctx, "SELECT seq,time,key_id,payload,signature FROM notary_receipts WHERE hash=?", hash).
		Scan(&r.Seq, &r.Time, &r.KeyID, &r.Payload, &r.Signature)
	if errors.Is(err, sql.ErrNoRows) {
		return r, errNoReceipt
	}
	if err != nil {
		return r, err
	}
	r.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	var p NotaryPayload
	if err = json.Unmarshal([]byte(r.Payload), &p); err != nil {
		return r, err
	}
	r.ServiceID = p.ServiceID
	return r, nil
}

func (n *notary) Read(ctx context.Context, q allowance.Querier, c Call) (json.RawMessage, error) {
	switch c.Method {
	case "get":
		var a struct {
			Hash string `json:"hash"`
		}
		if err := StrictObject(c.Args, &a); err != nil {
			return nil, err
		}
		if !notaryHashRE.MatchString(a.Hash) {
			return nil, refusal("invalid_service_data")
		}
		key, err := n.signingKey()
		if err != nil {
			return nil, err
		}
		r, err := readReceipt(ctx, q, a.Hash, key)
		if errors.Is(err, errNoReceipt) {
			return nil, refusal("notary_not_found")
		}
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"receipt": r})
	case "key":
		return keyRead(c.Args, n.key, NotarySchema, n.serviceID)
	}
	return nil, refusal("invalid_service_data")
}

// keyRead answers a receipt service's "key" read: the public key that signs
// its receipts (schema), and how to verify one.
func keyRead(args json.RawMessage, key ed25519.PrivateKey, schema, serviceID string) (json.RawMessage, error) {
	var a struct{}
	if err := StrictObject(args, &a); err != nil {
		return nil, err
	}
	if len(key) != ed25519.PrivateKeySize {
		return nil, refusal("service_unavailable")
	}
	pub := key.Public().(ed25519.PublicKey)
	return json.Marshal(map[string]any{
		"schema": schema, "service_id": serviceID, "algorithm": "ed25519",
		"public_key": base64.RawURLEncoding.EncodeToString(pub), "key_id": keyID(pub),
		"verify": "Ed25519 over the receipt's payload bytes exactly as given (base64url signature and key, no padding); the payload's fields must equal the receipt's, and key_id is the SHA-256 of the public key.",
	})
}
