package board

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/b64"
)

// Linked devices (RFC0014 §3, C576). An account has one main key (role
// 'primary', the key agent.rotate moves) and up to DeviceMax device keys
// (role 'device'). A device key signs as the account: a.account is the
// account and a.id stays the device's own fingerprint, so every signature is
// checked against the key that made it and a post says which key signed it.
// Standing, allowance, inbox, handle and rooms stay per account; a device
// adds none of them.
//
// Linking, without moving a private key:
//
//  1. The new device makes its own key and signs device.request with it
//     (data {"schema":1,"label":"Chrome on Windows"}). The answer is a
//     one-time code (8 characters, Crockford base32, shown as K7QM-4XPD)
//     that expires in DeviceCodeTTL; only its SHA-256 is stored.
//  2. The main key reads it with device.request.get {"code":...} (the
//     device's fingerprint, label and match words, which both screens show)
//     and approves by signing device.add: target = the new public key, data
//     {"schema":1,"label":...,"code_sha256":HEX}. Sent without a proof,
//     device.add is not run: the signed command is kept on the request.
//  3. The new device reads the request again, sees the command names its own
//     key, signs the same canonical bytes as proof and sends the very same
//     device.add with that proof (the agent.rotate both-keys pattern). It has
//     SignatureWindowSeconds from the approval to do so.
//
// device.add with a proof and no code_sha256 links a key whose holder signs
// both at once (a CLI with both keys at hand). device.revoke removes a
// device: the main key removes any, a device removes itself, at once (401
// key_revoked after). A device is held for DeviceHoldSeconds after it is
// linked: no payouts, handle changes, links, grants or device adds.
// Sealed conversations are not shared with a device (v1): it re-joins what
// it needs. Hosted identities link no devices; a second app takes a second
// token (manage_tokens).

const deviceRequestSchema = `
CREATE TABLE IF NOT EXISTS device_requests (
 code_sha256 TEXT PRIMARY KEY, device TEXT NOT NULL, public_key TEXT NOT NULL, label TEXT NOT NULL,
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('pending','approved','linked')),
 account TEXT NOT NULL DEFAULT '', approved_command TEXT NOT NULL DEFAULT '', approved_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS device_requests_device ON device_requests(device);
`

// deviceColumns are the identities columns a device needs (schema 47).
var deviceColumns = []struct{ name, definition string }{
	{"role", "TEXT NOT NULL DEFAULT 'primary' CHECK(role IN ('primary','device'))"},
	{"label", "TEXT NOT NULL DEFAULT ''"},
	{"revoked_at", "INTEGER NOT NULL DEFAULT 0"},
}

// migrateDevices47 adds deviceColumns, their index and device_requests;
// keyed on the columns, so a second run changes nothing.
func migrateDevices47(tx *sql.Tx) error {
	for _, column := range deviceColumns {
		var exists int
		if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info('identities') WHERE name=?", column.name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := tx.Exec("ALTER TABLE identities ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
		}
	}
	_, err := tx.Exec(`CREATE INDEX IF NOT EXISTS identity_devices ON identities(account) WHERE role='device';` + deviceRequestSchema)
	return err
}

const (
	// DeviceMax is how many device keys an account links besides its main key.
	DeviceMax = 8
	// DeviceCodeTTL is how long a link code lasts, in seconds.
	DeviceCodeTTL = 600
	// DeviceHoldSeconds is how long a newly linked device may not take
	// payouts, change the handle, link identities, issue grants or add devices.
	DeviceHoldSeconds = 86400
	// DeviceLabelMax bounds a device's label, in characters.
	DeviceLabelMax = 64
	// DeviceRequestsPerHour bounds link codes and code reads per network.
	DeviceRequestsPerHour = 30
	// deviceCodeLength is the code's length in Crockford base32 characters
	// (40 bits).
	deviceCodeLength   = 8
	deviceRateEntries  = 4096
	deviceCodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// DeviceHeldOps are refused from a device in its first DeviceHoldSeconds.
// work.claim and work.submit are held only when they name a payout_address.
var DeviceHeldOps = []string{"agent.register", "allowance.transfer", "credit.transfer", "identity.link", "identity.unlink", "delegation.create", "private_read.create", "spend_limit.set", "device.add"}

// DeviceMainKeyOps are the operations only the main key signs.
var DeviceMainKeyOps = []string{"agent.rotate", "device.add", "key.backup.put", "key.backup.delete", "room.owner.transfer", "hosted.token", "hosted.claim", "app.register", "app.owner.release"}

// deviceWords are the match words: four of them, from the device key's
// fingerprint, are shown on both screens so a person sees that the device
// they approve is the one in their hand.
var deviceWords = [64]string{
	"apple", "anchor", "arrow", "badge", "basket", "bell", "boat", "bridge",
	"brush", "cactus", "camera", "candle", "castle", "cloud", "comet", "compass",
	"crown", "desert", "dragon", "drum", "eagle", "engine", "falcon", "feather",
	"forest", "garden", "glacier", "guitar", "hammer", "harbor", "helmet", "island",
	"jungle", "kettle", "ladder", "lantern", "lemon", "magnet", "maple", "meadow",
	"mirror", "moon", "mountain", "orbit", "otter", "palace", "pepper", "piano",
	"pillow", "planet", "pocket", "rabbit", "river", "rocket", "saddle", "shell",
	"silver", "spider", "tiger", "tulip", "violin", "wagon", "whale", "zebra",
}

// DeviceMatchWords are the four match words of a device fingerprint.
func DeviceMatchWords(fp string) []string {
	if len(fp) < 6 {
		return nil
	}
	var bits uint32
	for _, c := range fp[:6] {
		bits <<= 4
		switch {
		case c >= '0' && c <= '9':
			bits |= uint32(c - '0')
		case c >= 'a' && c <= 'f':
			bits |= uint32(c-'a') + 10
		}
	}
	out := make([]string, 4)
	for i := range out {
		out[i] = deviceWords[(bits>>(18-6*i))&63]
	}
	return out
}

// deviceSourceKey carries the caller's network to the device operations,
// whose codes and reads are counted per network.
type deviceSourceKey struct{}

type deviceState struct {
	mu    sync.Mutex
	rates map[string]privateReadBucket
}

func deviceError(status int, code, message string) error { return problem(status, code, message) }

func deviceInvalid(message string) error { return deviceError(400, "invalid_device_data", message) }

func deviceRequestExpired() error {
	return deviceError(410, "device_request_expired", "This link code expired (codes last 10 minutes). Make a new one on the new device.")
}

func deviceRequestMissing() error {
	return deviceError(404, "device_request_not_found", "No link request has that code. It may be mistyped or already used; ask the new device for its code again.")
}

// normalizeDeviceCode is a typed code in its stored form: upper case, dashes
// and spaces dropped, and the look-alikes Crockford base32 folds (O to 0, I
// and L to 1). ok is false for anything else.
func normalizeDeviceCode(code string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		switch r {
		case '-', ' ':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		if !strings.ContainsRune(deviceCodeAlphabet, r) {
			return "", false
		}
		b.WriteRune(r)
	}
	out := b.String()
	return out, len(out) == deviceCodeLength
}

// DeviceCodeSHA256 is the code_sha256 a device.add approval names: SHA-256
// of the normalized code, in hex. "" for a malformed code.
func DeviceCodeSHA256(code string) string {
	n, ok := normalizeDeviceCode(code)
	if !ok {
		return ""
	}
	return sha256Hex([]byte(n))
}

func newDeviceCode() (string, error) {
	raw := make([]byte, deviceCodeLength)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	out := make([]byte, deviceCodeLength)
	for i, b := range raw {
		out[i] = deviceCodeAlphabet[b&31]
	}
	return string(out), nil
}

// displayDeviceCode is K7QM-4XPD.
func displayDeviceCode(code string) string { return code[:4] + "-" + code[4:] }

type deviceData struct {
	Schema     int    `json:"schema"`
	Label      string `json:"label,omitempty"`
	Code       string `json:"code,omitempty"`
	CodeSHA256 string `json:"code_sha256,omitempty"`
}

func parseDeviceData(op, data string) (deviceData, error) {
	var d deviceData
	usage := map[string]string{
		"device.request":     `device.request data is {"schema":1,"label":"Chrome on Windows"}: a short name for this device, private to you.`,
		"device.request.get": `device.request.get data is {"schema":1,"code":"K7QM-4XPD"}, the code the new device shows.`,
		"device.add":         `device.add data is {"schema":1,"label":LABEL,"code_sha256":HEX}: the request's label and the SHA-256 of its code; code_sha256 is left out only when both keys sign at once.`,
	}[op]
	if data == "" {
		if op == "device.request" {
			return deviceData{Schema: 1}, nil
		}
		return d, deviceInvalid(usage)
	}
	dec := json.NewDecoder(strings.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(&d) != nil || dec.More() || d.Schema != 1 {
		return d, deviceInvalid(usage)
	}
	switch op {
	case "device.request":
		if d.Code != "" || d.CodeSHA256 != "" {
			return d, deviceInvalid(usage)
		}
	case "device.request.get":
		if d.Label != "" || d.CodeSHA256 != "" || d.Code == "" {
			return d, deviceInvalid(usage)
		}
	case "device.add":
		if d.Code != "" {
			return d, deviceInvalid(usage)
		}
		if d.CodeSHA256 != "" && !fingerprintRE.MatchString(d.CodeSHA256) {
			return d, deviceInvalid("code_sha256 is the SHA-256 of the code (upper case, no dash), 64 lower-case hex digits.")
		}
	}
	d.Label = strings.TrimSpace(d.Label)
	if !utf8.ValidString(d.Label) || utf8.RuneCountInString(d.Label) > DeviceLabelMax || strings.ContainsAny(d.Label, "\x00\r\n\t") {
		return d, deviceInvalid("label is at most 64 characters on one line, such as \"Chrome on Windows\".")
	}
	return d, nil
}

// admitDevice takes one code or code read from the caller's network.
func (s *Store) admitDevice(ctx context.Context) bool {
	source, _ := ctx.Value(deviceSourceKey{}).(string)
	key := anonPrefixKey(source)
	s.devices.mu.Lock()
	defer s.devices.mu.Unlock()
	if s.devices.rates == nil {
		s.devices.rates = map[string]privateReadBucket{}
	}
	now := s.now()
	previous, known := s.devices.rates[key]
	bucket := privateBucket(now, previous, DeviceRequestsPerHour/3600.0, DeviceRequestsPerHour)
	if bucket.tokens < 1 {
		s.devices.rates[key] = bucket
		return false
	}
	if !known && len(s.devices.rates) >= deviceRateEntries {
		for k, b := range s.devices.rates {
			if now.Sub(b.seen) >= time.Hour {
				delete(s.devices.rates, k)
			}
		}
		for len(s.devices.rates) >= deviceRateEntries {
			var stalest string
			var oldest time.Time
			for k, b := range s.devices.rates {
				if stalest == "" || b.seen.Before(oldest) {
					stalest, oldest = k, b.seen
				}
			}
			delete(s.devices.rates, stalest)
		}
	}
	bucket.tokens--
	s.devices.rates[key] = bucket
	return true
}

func deviceRateLimited() error {
	return &Error{Status: 429, Code: "device_rate_limited", Message: "Too many link codes or code reads from this network; wait a few minutes.", RetryAfter: 120}
}

// freshKey is true when id is no agent, worker key or reader key yet.
func freshKey(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var exists int
	err := tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM identities WHERE id=?)+(SELECT count(*) FROM delegations WHERE child_id=?)+(SELECT count(*) FROM private_read_grants WHERE child_id=?)", id, id, id).Scan(&exists)
	return exists == 0, err
}

func (s *Store) deviceCommand(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if c.Operation == "devices.list" && c.Target != "" && !a.signed {
		return s.listDevices(ctx, tx, c, a, now)
	}
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if a.grant != nil {
		return Result{}, delegationError("delegation_forbidden")
	}
	switch c.Operation {
	case "device.request":
		return s.requestDevice(ctx, tx, c, a, now)
	case "device.request.get":
		return s.readDeviceRequest(ctx, tx, c, a, now)
	case "device.add":
		return s.addDevice(ctx, tx, c, a, now)
	case "device.revoke":
		return s.revokeDevice(ctx, tx, c, a, now)
	}
	return s.listDevices(ctx, tx, c, a, now)
}

// requestDevice is device.request, signed by a key with no agent yet.
func (s *Store) requestDevice(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	d, err := parseDeviceData(c.Operation, c.Data)
	if err != nil {
		return Result{}, err
	}
	fresh, err := freshKey(ctx, tx, a.id)
	if err != nil {
		return Result{}, err
	}
	if !fresh {
		return Result{}, problem(409, "agent_exists", "This key is already an agent or a worker key. A new device makes a new key and sends device.request with it.")
	}
	if !s.admitDevice(ctx) {
		return Result{}, deviceRateLimited()
	}
	code, err := newDeviceCode()
	if err != nil {
		return Result{}, err
	}
	// One open request per key: a new code replaces the last.
	if _, err = tx.ExecContext(ctx, "DELETE FROM device_requests WHERE device=? AND state<>'linked'", a.id); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO device_requests(code_sha256,device,public_key,label,created_at,expires_at,state) VALUES(?,?,?,?,?,?,'pending')",
		sha256Hex([]byte(code)), a.id, a.publicKey, d.Label, now, now+DeviceCodeTTL); err != nil {
		return Result{}, err
	}
	data := map[string]any{"device": a.id, "label": d.Label, "expires_at": now + DeviceCodeTTL, "match": DeviceMatchWords(a.id),
		"notice": "The code is shown once, in this answer; an exact retry answers without it. On the device you already use, open Me, Link a device, and type it or scan its link. Only link a device you are holding.",
		"next":   "device.request.get with data {\"schema\":1,\"code\":CODE} until approval is set, then send approval.command as it is with your proof: your signature over its canonical bytes."}
	link := strings.TrimRight(s.config.PublicURL, "/") + "/me#link="
	return Result{Data: data, afterCommit: func() (Result, error) {
		shown := make(map[string]any, len(data)+2)
		for k, v := range data {
			shown[k] = v
		}
		shown["code"], shown["link"] = displayDeviceCode(code), link+code
		return Result{Data: shown}, nil
	}}, nil
}

type deviceRequestRow struct {
	device, publicKey, label, state, account, command string
	created, expires, approved                        int64
}

func readDeviceRequestRow(ctx context.Context, tx *sql.Tx, digest string) (deviceRequestRow, error) {
	var r deviceRequestRow
	err := tx.QueryRowContext(ctx, "SELECT device,public_key,label,state,account,approved_command,created_at,expires_at,approved_at FROM device_requests WHERE code_sha256=?", digest).
		Scan(&r.device, &r.publicKey, &r.label, &r.state, &r.account, &r.command, &r.created, &r.expires, &r.approved)
	return r, err
}

// readDeviceRequest is device.request.get: the main key's look before it
// approves, and the new device's poll for the approval.
func (s *Store) readDeviceRequest(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	d, err := parseDeviceData(c.Operation, c.Data)
	if err != nil {
		return Result{}, err
	}
	code, ok := normalizeDeviceCode(d.Code)
	if !ok {
		return Result{}, deviceInvalid("A code is 8 letters and digits, such as K7QM-4XPD.")
	}
	r, err := readDeviceRequestRow(ctx, tx, sha256Hex([]byte(code)))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}
	own := err == nil && a.id == r.device
	// The new device polls its own request freely; every other read, and
	// every miss, counts against the network, so codes cannot be guessed.
	if !own && !s.admitDevice(ctx) {
		return Result{}, deviceRateLimited()
	}
	if errors.Is(err, sql.ErrNoRows) || r.state == "linked" {
		return Result{}, deviceRequestMissing()
	}
	// An approved request stays readable by its device for the approval's
	// signature window, past the code's own expiry.
	if now >= r.expires && !(own && r.state == "approved" && now < r.approved+SignatureWindowSeconds) {
		return Result{}, deviceRequestExpired()
	}
	view := map[string]any{"device": r.device, "public_key": r.publicKey, "label": r.label, "match": DeviceMatchWords(r.device),
		"created_at": r.created, "expires_at": r.expires, "state": r.state, "code_sha256": sha256Hex([]byte(code))}
	if own && r.state == "approved" {
		var cmd Command
		if err = json.Unmarshal([]byte(r.command), &cmd); err != nil {
			return Result{}, err
		}
		handle, err := accountHandle(ctx, tx, r.account)
		if err != nil {
			return Result{}, err
		}
		view["approval"] = map[string]any{"account": r.account, "handle": handle, "approved_at": r.approved, "finish_by": r.approved + SignatureWindowSeconds,
			"command": cmd, "canonical": string(Canonical(s.config.ServiceID, cmd))}
	}
	return Result{Data: view}, nil
}

// addDevice is device.add: an approval kept on the request (no proof), or
// the link itself (both keys signed).
func (s *Store) addDevice(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	d, err := parseDeviceData(c.Operation, c.Data)
	if err != nil {
		return Result{}, err
	}
	key, err := b64.URL(c.Target)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return Result{}, problem(400, "invalid_target_key", "Target must be the new device's raw Ed25519 public key in unpadded base64url.")
	}
	if a.hosted {
		return Result{}, problem(403, "self_custody_required", "A hosted identity links no devices: for a second app, create another token with manage_tokens.")
	}
	id := fingerprint(key)
	fresh, err := freshKey(ctx, tx, id)
	if err != nil {
		return Result{}, err
	}
	if !fresh {
		return Result{}, problem(409, "agent_exists", "The new device's key is already an agent or a worker key; the device makes a new key and a new code.")
	}
	var active int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM identities WHERE account=? AND role='device' AND revoked_at=0", a.account).Scan(&active); err != nil {
		return Result{}, err
	}
	if active >= DeviceMax {
		return Result{}, deviceError(409, "device_limit", "This account has linked the most devices it can (8); remove one first.")
	}
	var req deviceRequestRow
	digest := ""
	if d.CodeSHA256 != "" {
		if req, err = readDeviceRequestRow(ctx, tx, d.CodeSHA256); errors.Is(err, sql.ErrNoRows) || err == nil && (req.state == "linked" || req.device != id) {
			return Result{}, deviceRequestMissing()
		} else if err != nil {
			return Result{}, err
		}
		// The approval needs a live code; the confirmation only the
		// approval's signature window, which the timestamp check enforces.
		if now >= req.expires && (c.Proof == "" || req.state != "approved") {
			return Result{}, deviceRequestExpired()
		}
		// An approval by another account is never replaced.
		if c.Proof == "" && req.state == "approved" && req.account != a.account {
			return Result{}, deviceRequestMissing()
		}
		digest = sha256Hex(a.canonical)
	}
	if c.Proof == "" {
		if d.CodeSHA256 == "" {
			return Result{}, deviceError(401, "invalid_device_proof", "device.add needs the new device's proof, or code_sha256 to approve a link request.")
		}
		// The approval: the signed command waits on the request for the new
		// device, which sends it back with its proof. Nothing is linked and no
		// receipt is kept, so that same command can run once, later.
		stored, err := json.Marshal(c)
		if err != nil {
			return Result{}, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE device_requests SET state='approved',account=?,approved_command=?,approved_at=? WHERE code_sha256=?", a.account, string(stored), now, d.CodeSHA256); err != nil {
			return Result{}, err
		}
		return Result{transient: true, Data: map[string]any{"state": "approved", "device": id, "label": req.label, "match": DeviceMatchWords(id), "finish_by": now + SignatureWindowSeconds,
			"next": "The new device confirms within five minutes; then it signs as this account."}}, nil
	}
	proof, err := b64.URL(c.Proof)
	if err != nil || len(proof) != ed25519.SignatureSize || !ed25519.Verify(key, a.canonical, proof) {
		return Result{}, deviceError(401, "invalid_device_proof", "The new device must sign the same canonical device.add command as proof.")
	}
	label := d.Label
	if d.CodeSHA256 != "" {
		var storedCmd Command
		if req.state != "approved" || json.Unmarshal([]byte(req.command), &storedCmd) != nil || sha256Hex(Canonical(s.config.ServiceID, storedCmd)) != digest || req.account != a.account {
			return Result{}, deviceError(409, "device_request_not_approved", "This link request is not approved by this command; approve it from the main key first.")
		}
		if _, err = tx.ExecContext(ctx, "UPDATE device_requests SET state='linked' WHERE code_sha256=?", d.CodeSHA256); err != nil {
			return Result{}, err
		}
		if label == "" {
			label = req.label
		}
	}
	if err = s.charge(ctx, tx, a, 512, now); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO identities(id,public_key,account,created_at,last_seen,role,label) VALUES(?,?,?,?,?,'device',?)",
		id, base64.RawURLEncoding.EncodeToString(key), a.account, now, now, label); err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, a.id, id, "device linked", now); err != nil {
		return Result{}, err
	}
	if err = s.onAccountChange(ctx, tx, accountChange{Account: a.account, Reason: "device.add", CancelKey: a.id}, now); err != nil {
		return Result{}, err
	}
	if err = s.deviceNotice(ctx, tx, a.account, id, "added", label, now); err != nil {
		return Result{}, err
	}
	handle, err := accountHandle(ctx, tx, a.account)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"device": id, "account": a.account, "handle": handle, "label": label, "added_at": now,
		"held_until": now + DeviceHoldSeconds, "devices": active + 1}}, nil
}

// revokeDevice is device.revoke: the main key removes any device, a device
// itself. target is the device's fingerprint; empty is the signing device.
func (s *Store) revokeDevice(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	target := c.Target
	if target == "" {
		target = a.id
	}
	if !a.device && target == a.id {
		return Result{}, deviceError(400, "invalid_device_data", "The main key is not a device; name the device to remove in target. To replace the main key, use agent.rotate.")
	}
	if a.device && target != a.id {
		return Result{}, deviceError(403, "primary_key_required", "A device removes only itself; the main key removes any device.")
	}
	var label string
	var revoked int64
	err := tx.QueryRowContext(ctx, "SELECT label,revoked_at FROM identities WHERE id=? AND account=? AND role='device'", target, a.account).Scan(&label, &revoked)
	if errors.Is(err, sql.ErrNoRows) || err == nil && revoked != 0 {
		return Result{}, deviceError(404, "device_not_found", "This account has no linked device with that fingerprint; devices.list shows them.")
	}
	if err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE identities SET revoked_at=? WHERE id=?", now, target); err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, a.id, target, "device removed", now); err != nil {
		return Result{}, err
	}
	if err = s.deviceNotice(ctx, tx, a.account, target, "removed", label, now); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"device": target, "account": a.account, "removed_at": now}}, nil
}

// DeviceView is one key of an account in devices.list.
type DeviceView struct {
	ID         string `json:"id"`
	PublicKey  string `json:"public_key"`
	Role       string `json:"role"`
	Label      string `json:"label,omitempty"`
	AddedAt    int64  `json:"added_at"`
	LastSeen   int64  `json:"last_seen,omitempty"`
	RemovedAt  int64  `json:"removed_at,omitempty"`
	HeldUntil  int64  `json:"held_until,omitempty"`
	ThisDevice bool   `json:"this_device,omitempty"`
}

// listDevices is devices.list. Signed by one of the account's keys it is the
// owner's list, labels and last use included; with target (an agent) it is
// the public record verifiers need: each key, its role and when it was added
// or removed, never a label.
func (s *Store) listDevices(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	account, own := a.account, a.signed
	if c.Target != "" && (!a.signed || c.Target != a.id && c.Target != a.account) {
		other, err := lookupAccount(ctx, tx, c.Target)
		if err != nil {
			return Result{}, err
		}
		if !a.signed || other != a.account {
			var public bool
			if err = tx.QueryRowContext(ctx, "SELECT "+publicAccountSQL("?1"), other).Scan(&public); err != nil {
				return Result{}, err
			}
			if !public {
				return Result{}, errAgentUnregistered()
			}
			account, own = other, false
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,public_key,role,label,created_at,last_seen,revoked_at FROM identities
 WHERE account=? AND successor='' AND (role='primary' OR revoked_at=0 OR ?) ORDER BY role='device',created_at,id`, account, !own)
	if err != nil {
		return Result{}, err
	}
	list := []DeviceView{}
	for rows.Next() {
		var v DeviceView
		if err = rows.Scan(&v.ID, &v.PublicKey, &v.Role, &v.Label, &v.AddedAt, &v.LastSeen, &v.RemovedAt); err != nil {
			rows.Close()
			return Result{}, err
		}
		if v.Role == "device" && now < v.AddedAt+DeviceHoldSeconds && v.RemovedAt == 0 {
			v.HeldUntil = v.AddedAt + DeviceHoldSeconds
		}
		if own {
			v.ThisDevice = v.ID == a.id
		} else {
			v.Label, v.LastSeen, v.HeldUntil = "", 0, 0
		}
		list = append(list, v)
	}
	if err = closeRows(rows); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"account": account, "devices": list, "max": DeviceMax}}, nil
}

// inboxDevice is a device linked to or removed from the account.
const inboxDevice = "device"

// deviceNotice tells the account's inbox, which every one of its keys reads,
// that a device was linked or removed.
func (s *Store) deviceNotice(ctx context.Context, tx *sql.Tx, account, device, change, label string, now int64) error {
	if !s.inboxOn() {
		return nil
	}
	_, err := s.recordInbox(ctx, tx, inboxSource{kind: inboxDevice, account: account, subject: "device:" + device + "@" + change,
		actor: device, at: now, detail: map[string]any{"change": change, "label": label}})
	return err
}

// accountHandle is the account's handle: its main key's.
func accountHandle(ctx context.Context, q queryer, account string) (string, error) {
	var handle string
	err := q.QueryRowContext(ctx, "SELECT handle FROM identities WHERE account=? AND successor='' AND role='primary' LIMIT 1", account).Scan(&handle)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return handle, err
}

// handleKeySQL is the current handle of the key the SQL expression id names:
// a device shows its account's.
func handleKeySQL(id string) string {
	return `coalesce((SELECT CASE WHEN hk.role='device' THEN coalesce((SELECT hp.handle FROM identities hp WHERE hp.account=hk.account AND hp.successor='' AND hp.role='primary'),'') ELSE hk.handle END FROM identities hk WHERE hk.id=` + id + `),'')`
}

// mainKeyID is the id of the key that holds a's handle: a's own, or for a
// device its account's main key.
func mainKeyID(ctx context.Context, tx *sql.Tx, a actor) (string, error) {
	if !a.device {
		return a.id, nil
	}
	var id string
	err := tx.QueryRowContext(ctx, "SELECT id FROM identities WHERE account=? AND successor='' AND role='primary' LIMIT 1", a.account).Scan(&id)
	return id, err
}

// authorizeDevice applies a device key's limits in executeCommand: a removed
// device signs nothing, the main key's own operations refuse a device, and a
// new device waits out its hold.
func authorizeDevice(c Command, a actor, revoked, now int64) error {
	if revoked != 0 {
		return problem(401, "key_revoked", "This device was removed from its account and signs nothing more. Link it again from the main key.")
	}
	if !a.device {
		return nil
	}
	for _, op := range DeviceMainKeyOps {
		if c.Operation == op {
			return deviceError(403, "primary_key_required", "Only the account's main key does this; sign it on the device that holds the main key.")
		}
	}
	if now >= a.deviceSince+DeviceHoldSeconds {
		return nil
	}
	held := false
	for _, op := range DeviceHeldOps {
		held = held || c.Operation == op
	}
	if (c.Operation == "work.claim" || c.Operation == "work.submit") && strings.Contains(c.Data, `"payout_address"`) {
		held = true
	}
	if held {
		return deviceError(403, "device_hold", "A newly linked device waits 24 hours before payouts, handle changes, links, grants and device adds; the main key can do them now.")
	}
	return nil
}
