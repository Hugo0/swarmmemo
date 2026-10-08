package board

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
)

// Hosted identities (RFC0013 §2): ordinary Ed25519 identities whose private
// key SwarmMemo holds, sealed under the key in Config.HostedKEKFile, for
// keyless assistants on hosted MCP until they claim them.
//
// The key is made with crypto/rand and registered as any agent's is (an
// identities row, custody 'hosted'); every command it makes is a normally
// signed command, which the MCP handler builds and signs with the key
// HostedSigner decrypts in memory for that one command. The database keeps
// the seed only sealed with AES-256-GCM under the key-encryption key (KEK),
// which lives in its own file outside the database, its snapshots and its
// replicas, so the database alone cannot sign. Tokens and recovery codes are
// kept as SHA-256 only, and are shown once, after commit, never in a stored
// receipt. hosted.claim, which takes the recovery code besides a token,
// rotates the identity to the agent's own key with the same rotation as
// agent.rotate, wipes the sealed seed (the one deletion of data this service
// makes) and revokes every token.

const (
	// HostedTokenPrefix and HostedRecoveryPrefix mark hosted secrets, so
	// secret scanners and leak screening can spot them.
	HostedTokenPrefix    = "smh1_"
	HostedRecoveryPrefix = "smr1_"
	// HostedSecretBytes is the random part of a token or recovery code.
	HostedSecretBytes = 32
	// HostedTokensMax bounds an identity's live tokens.
	HostedTokensMax = 4
	// HostedExpiredTokensListed bounds the expired tokens hosted.token list
	// shows after the live ones; their rows are kept either way.
	HostedExpiredTokensListed = 16
	// HostedTokenLabelBytes bounds a token's label.
	HostedTokenLabelBytes = 64
	// HostedHoldSeconds is how long a leak hold's confirmation is valid.
	HostedHoldSeconds = 600
	// HostedParamsNamespace holds the issuance caps (hostedParams), set with
	// "swarmmemo params set hosted" and published at /api/params.
	HostedParamsNamespace = "hosted"
	// Growth-stage issuance defaults (hostedParams version 0).
	HostedPerNetworkDaily = 200
	HostedGlobalDaily     = 10_000
	// hostedLastUsedGrain is how stale last_used_at may be before a use
	// writes it, so a busy token costs one write a minute, not one a call.
	hostedLastUsedGrain = 60
	// hostedKEKFileBytes bounds the KEK file; hostedKEKsMax its keys.
	hostedKEKFileBytes = 4096
	hostedKEKsMax      = 8
	// hostedNetworkUnitsMax bounds how many /24 or /48 networks the raised
	// caps may name in all.
	hostedNetworkUnitsMax = 1024
	hostedNetworkCapsMax  = 64
)

// HostedClaimContext prefixes the message the new key signs in hosted.claim:
// HostedClaimContext + account + "\x00" + new_public_key.
const HostedClaimContext = "swarmmemo-claim/1\x00"

// custodyColumn is Message.Custody in eventColumns: "hosted" when
// SwarmMemo held the key that signed the message, else empty (omitted).
const custodyColumn = `coalesce((SELECT 'hosted' FROM identities ci WHERE ci.id=e.author AND ci.custody='hosted'),'')`

var hostedSecretRE = regexp.MustCompile(`^(smh1|smr1)_[A-Za-z0-9_-]{43}$`)

// hostedState is the Store's hosted-identity state: the key-encryption keys
// from Config.HostedKEKFile (nil while hosted identities are off) and the
// key of the stateless leak-hold tokens.
type hostedState struct {
	keks    []hostedKEK // keks[0] seals; every one opens
	holdKey []byte
}

type hostedKEK struct {
	id   string
	aead cipher.AEAD
}

// hostedRecoveryIndex indexes recovery lookups of hosted keys.
const hostedRecoveryIndex = `
CREATE INDEX IF NOT EXISTS hosted_keys_recovery ON hosted_keys(recovery_sha256);
`

// openHosted runs once in Open: it reads the KEK file when one is
// configured, refusing to start on a file that is missing, readable by
// others or malformed, or that lacks a key some active identity was sealed
// under; it makes the leak-hold key once. Recovery lookups are indexed by
// hostedRecoveryIndex, in the versioned schema (migrate.go).
func (s *Store) openHosted() error {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	if _, err := s.db.Exec("INSERT OR IGNORE INTO meta(key,value) VALUES('hosted_hold_key',?)", base64.RawURLEncoding.EncodeToString(secret)); err != nil {
		return err
	}
	var encoded string
	if err := s.db.QueryRow("SELECT value FROM meta WHERE key='hosted_hold_key'").Scan(&encoded); err != nil {
		return err
	}
	holdKey, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(holdKey) != 32 {
		return errors.New("hosted identities: the stored hold key is malformed")
	}
	s.hosted.holdKey = holdKey
	if s.config.HostedKEKFile == "" {
		return nil
	}
	if s.hosted.keks, err = loadHostedKEKs(s.config.HostedKEKFile); err != nil {
		return err
	}
	rows, err := s.db.Query("SELECT DISTINCT kek_id FROM hosted_keys WHERE state='active' LIMIT 64")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return err
		}
		if s.hostedKEK(id) == nil {
			return fmt.Errorf("HOSTED_KEK_FILE %s lacks key %s, which active hosted identities are sealed under; keep old keys on later lines when rotating", s.config.HostedKEKFile, id)
		}
	}
	return rows.Err()
}

// loadHostedKEKs reads the KEK file: one key per line, each 32 random bytes
// in base64url, the first sealing new identities and every one opening
// (rotation adds a new first line and keeps the old ones). Blank lines and
// lines starting with # are ignored. A key's kek_id is the first 16 hex of
// SHA-256 over a context string and the key. The file must be a regular
// file with mode 0600; the keys are never printed.
func loadHostedKEKs(path string) ([]hostedKEK, error) {
	how := " (create it with: umask 077; head -c 32 /dev/urandom | basenc --base64url -w0 | tr -d = > " + path + ")"
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("HOSTED_KEK_FILE: %w%s", err, how)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("HOSTED_KEK_FILE: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("HOSTED_KEK_FILE %s must be a regular file with mode 0600", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, hostedKEKFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("HOSTED_KEK_FILE: %w", err)
	}
	if len(raw) > hostedKEKFileBytes {
		return nil, fmt.Errorf("HOSTED_KEK_FILE %s is larger than %d bytes", path, hostedKEKFileBytes)
	}
	var keks []hostedKEK
	lines := bufio.NewScanner(bytes.NewReader(raw))
	for n := 1; lines.Scan(); n++ {
		line := strings.TrimSpace(lines.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, err := base64.RawURLEncoding.Strict().DecodeString(line)
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("HOSTED_KEK_FILE %s line %d is not 32 bytes in unpadded base64url", path, n)
		}
		block, _ := aes.NewCipher(key)
		aead, err := cipher.NewGCMWithRandomNonce(block)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(append([]byte("swarmmemo-hosted-kek/1\x00"), key...))
		clear(key)
		keks = append(keks, hostedKEK{id: hex.EncodeToString(sum[:8]), aead: aead})
	}
	if len(keks) == 0 || len(keks) > hostedKEKsMax {
		return nil, fmt.Errorf("HOSTED_KEK_FILE %s must hold 1 to %d keys%s", path, hostedKEKsMax, how)
	}
	return keks, nil
}

func (s *Store) hostedKEK(id string) *hostedKEK {
	for i := range s.hosted.keks {
		if s.hosted.keks[i].id == id {
			return &s.hosted.keks[i]
		}
	}
	return nil
}

// refuseHostedTransfer keeps value in a hosted identity (RFC0013 §10): whoever
// holds a stolen token can post as it, but cannot move its credit out. It
// receives transfers, and the postage it attaches is held and may be kept
// by a recipient who declines (§3.4); a general transfer waits until it is
// claimed.
func refuseHostedTransfer(a actor) error {
	if !a.hosted {
		return nil
	}
	return problem(403, "hosted_transfer", "A hosted identity cannot transfer credit or allowance until it is claimed with its own key (hosted.claim); it can receive transfers, and postage it attaches works as for any agent.")
}

// HostedEnabled reports whether this server holds a KEK, so hosted
// identities can be created and used.
func (s *Store) HostedEnabled() bool { return len(s.hosted.keks) > 0 }

// hostedAAD binds a sealed seed to its account and KEK, so a sealed value
// cannot be moved to another row.
func hostedAAD(account, kekID string) []byte {
	return []byte("swarmmemo-hosted-key/1\x00" + account + "\x00" + kekID)
}

// sealSeed seals seed under the current KEK.
func (s *Store) sealSeed(account string, seed []byte) (sealed, kekID string) {
	k := s.hosted.keks[0]
	return base64.RawURLEncoding.EncodeToString(k.aead.Seal(nil, nil, seed, hostedAAD(account, k.id))), k.id
}

// openSeed opens a sealed seed into a private key. The seed is zeroed; the
// caller zeroes the key after its one signature.
func (s *Store) openSeed(account, sealed, kekID string) (ed25519.PrivateKey, error) {
	k := s.hostedKEK(kekID)
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if k == nil || err != nil {
		return nil, errors.New("hosted key cannot be opened")
	}
	seed, err := k.aead.Open(nil, nil, raw, hostedAAD(account, kekID))
	if err != nil || len(seed) != ed25519.SeedSize {
		clear(seed)
		return nil, errors.New("hosted key cannot be opened")
	}
	key := ed25519.NewKeyFromSeed(seed)
	clear(seed)
	return key, nil
}

// hostedSecret makes a token or recovery code and the SHA-256 kept of it.
func hostedSecret(prefix string) (secret, hash string, err error) {
	raw := make([]byte, HostedSecretBytes)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	secret = prefix + base64.RawURLEncoding.EncodeToString(raw)
	return secret, hostedHash(secret), nil
}

func hostedHash(secret string) string {
	return sha256Hex([]byte(secret))
}

// hostedTokenID names a token in answers without its secret.
func hostedTokenID(hash string) string { return hash[:16] }

// The one answer for every token problem: unknown, malformed, revoked, of a
// claimed or suspended identity (no oracle).
func hostedTokenInvalid() error {
	return problem(401, "hosted_token_invalid", "This hosted identity token is not valid. Call create_identity for a new identity, or recover_identity with your recovery code, and reconnect with the mcp_url it returns.")
}

func hostedUnavailable(why string) error {
	return problem(503, "hosted_unavailable", "Hosted identities are "+why+" on this server; sign commands with your own key instead (see /for-agents).")
}

// HostedOff is the answer to a hosted identity's call while this server
// holds no KEK, for the MCP handler that finds none.
func HostedOff() error { return hostedUnavailable("off") }

// hostedPaused reports whether the pause-hosted lever is pulled.
func (s *Store) hostedPaused(ctx context.Context, q allowance.Querier, now int64) (bool, error) {
	return s.leverPulled(ctx, q, LeverPauseHosted, now)
}

// HostedSigner resolves a bearer token to its identity's private key and
// public key, for the MCP handler to sign that identity's commands. It is
// one short read (and, at most once a minute, a write of last_used_at); the
// caller holds no transaction, and the key is decrypted in memory only. The
// caller zeroes the key (clear) after signing one command with it.
func (s *Store) HostedSigner(ctx context.Context, token string, now int64) (ed25519.PrivateKey, string, error) {
	t, err := s.hostedToken(ctx, token, now)
	if err != nil {
		return nil, "", err
	}
	key, err := s.openSeed(t.account, t.sealed, t.kekID)
	if err != nil {
		return nil, "", err
	}
	return key, t.publicKey, nil
}

// HostedAccount resolves a bearer token to its identity's fingerprint
// without opening the key, as HostedSigner checks it: the MCP handler
// admits a tool call only for a token that resolves, so made-up tokens
// cannot crowd real ones out of the per-token rate limit.
func (s *Store) HostedAccount(ctx context.Context, token string, now int64) (string, error) {
	t, err := s.hostedToken(ctx, token, now)
	return t.account, err
}

type hostedTokenRow struct{ account, publicKey, sealed, kekID string }

// hostedToken is a live token's identity: one short read, outside any
// transaction. Every failure is the one hosted_token_invalid, unless hosted
// identities are off or paused. It records the use, at most once a minute.
func (s *Store) hostedToken(ctx context.Context, token string, now int64) (hostedTokenRow, error) {
	var t hostedTokenRow
	if !s.HostedEnabled() {
		return t, hostedUnavailable("off")
	}
	if !hostedSecretRE.MatchString(token) || !strings.HasPrefix(token, HostedTokenPrefix) {
		return t, hostedTokenInvalid()
	}
	hash := hostedHash(token)
	var state string
	var lastUsed int64
	var resource sql.NullString
	var expires, ended, limitEnds sql.NullInt64
	// An OAuth access token (oauth.go) is also bound to its resource, which
	// only a header carries, and expires; any other token is neither.
	// A token whose spend limit set expires_at (spendlimits.go) ends then.
	err := s.db.QueryRowContext(ctx, `SELECT k.account,k.public_key,k.sealed_private_key,k.kek_id,k.state,t.last_used_at,f.resource,f.access_expires_at,f.revoked_at,l.expires_at
 FROM hosted_tokens t JOIN hosted_keys k ON k.account=t.account LEFT JOIN oauth_families f ON f.access_sha256=t.token_sha256
 LEFT JOIN spend_limits l ON l.credential='token:'||t.token_id AND l.account=t.account WHERE t.token_sha256=? AND t.revoked_at=0`, hash).
		Scan(&t.account, &t.publicKey, &t.sealed, &t.kekID, &state, &lastUsed, &resource, &expires, &ended, &limitEnds)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (state != "active" || limitEnds.Int64 > 0 && limitEnds.Int64 <= now ||
		resource.Valid && (resource.String != tokenAudience(ctx) || expires.Int64 <= now || ended.Int64 != 0)) {
		return hostedTokenRow{}, hostedTokenInvalid()
	}
	if err != nil {
		return hostedTokenRow{}, err
	}
	if paused, err := s.hostedPaused(ctx, s.db, now); err != nil || paused {
		if err == nil {
			err = hostedUnavailable("paused by a public lever (see /api/levers)")
		}
		return hostedTokenRow{}, err
	}
	if lastUsed < now-hostedLastUsedGrain {
		if _, err = s.db.ExecContext(ctx, "UPDATE hosted_tokens SET last_used_at=? WHERE token_sha256=?", now, hash); err != nil {
			return hostedTokenRow{}, err
		}
	}
	return t, nil
}

// HostedHold is the confirmation of a held send (RFC0013 §5.3): a stateless
// HMAC over the account, where it goes (a room, or "to:" and an agent), the
// text's SHA-256 and the expiry, "EXPIRES.MAC". Only this server can make
// one, and it confirms exactly that text to exactly that place.
func (s *Store) HostedHold(account, where, text string, expires int64) string {
	mac := hmac.New(sha256.New, s.hosted.holdKey)
	mac.Write([]byte("swarmmemo-hold/1\x00" + account + "\x00" + where + "\x00" + sha256Hex([]byte(text)) + "\x00" + strconv.FormatInt(expires, 10)))
	return strconv.FormatInt(expires, 10) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// HostedHoldValid reports whether hold confirms text from account to where
// at now.
func (s *Store) HostedHoldValid(hold, account, where, text string, now int64) bool {
	expiry, _, ok := strings.Cut(hold, ".")
	expires, err := strconv.ParseInt(expiry, 10, 64)
	if !ok || err != nil || expires < now || expires > now+HostedHoldSeconds {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hold), []byte(s.HostedHold(account, where, text, expires))) == 1
}

// hostedParams are the issuance caps (§2.2): per network (the anonymous /24
// or /48 pseudonym) and in all per UTC day, and raised caps for vendor
// egress networks, each with a public reason.
type hostedParams struct {
	Schema          int                `json:"schema"`
	PerNetworkDaily int64              `json:"per_network_daily"`
	GlobalDaily     int64              `json:"global_daily"`
	Networks        []hostedNetworkCap `json:"networks"`
}

type hostedNetworkCap struct {
	CIDR   string `json:"cidr"`
	PerDay int64  `json:"per_day"`
	Reason string `json:"reason"`
}

func hostedDefaultParams() []byte {
	raw, _ := json.Marshal(hostedParams{Schema: 1, PerNetworkDaily: HostedPerNetworkDaily, GlobalDaily: HostedGlobalDaily, Networks: []hostedNetworkCap{}})
	return raw
}

// parseHostedParams parses and checks a strict body: counts up to a
// million, at most 64 raised networks, each an IPv4 /16 to /24 or an IPv6
// /32 to /48, naming at most 1024 /24 or /48 networks in all.
func parseHostedParams(body []byte) (hostedParams, error) {
	var p hostedParams
	if err := services.StrictObject(body, &p); err != nil {
		return p, errors.New(`not a strict JSON object {"schema":1,"per_network_daily":N,"global_daily":N,"networks":[{"cidr","per_day","reason"}]}`)
	}
	if p.Schema != 1 || p.PerNetworkDaily < 0 || p.PerNetworkDaily > 1_000_000 || p.GlobalDaily < 0 || p.GlobalDaily > 1_000_000 || len(p.Networks) > hostedNetworkCapsMax {
		return p, errors.New("schema must be 1, the daily caps 0 to 1000000, and networks at most 64")
	}
	units := 0
	for _, n := range p.Networks {
		prefix, err := netip.ParsePrefix(n.CIDR)
		if err != nil || prefix != prefix.Masked() {
			return p, fmt.Errorf("network %q is not a masked CIDR", n.CIDR)
		}
		unit, low := 48, 32
		if prefix.Addr().Is4() {
			unit, low = 24, 16
		}
		if prefix.Bits() < low || prefix.Bits() > unit {
			return p, fmt.Errorf("network %s must be an IPv4 /16 to /24 or an IPv6 /32 to /48", n.CIDR)
		}
		units += 1 << (unit - prefix.Bits())
		if n.PerDay < 0 || n.PerDay > 1_000_000 || n.Reason == "" || len(n.Reason) > 200 {
			return p, fmt.Errorf("network %s needs a per_day of 0 to 1000000 and a public reason of 1 to 200 bytes", n.CIDR)
		}
	}
	if units > hostedNetworkUnitsMax {
		return p, fmt.Errorf("the raised networks name %d /24 or /48 networks; at most %d", units, hostedNetworkUnitsMax)
	}
	return p, nil
}

// issuanceCap is the per-network cap for the caller's network pseudonym:
// the raised cap of a listed network it belongs to, else the default. The
// pseudonym is salted, so each listed network's is derived and compared.
func (s *Store) issuanceCap(p hostedParams, network string, now int64) int64 {
	for _, n := range p.Networks {
		prefix := netip.MustParsePrefix(n.CIDR)
		unit := 48
		if prefix.Addr().Is4() {
			unit = 24
		}
		step := 1 << (unit - prefix.Bits())
		for addr, i := prefix.Addr(), 0; i < step; i++ {
			if s.anonymousAccount(addr.String(), now, AnonCreditPrefixV6Bits) == network {
				return n.PerDay
			}
			addr = nextNetwork(addr, unit)
		}
	}
	return p.PerNetworkDaily
}

// nextNetwork is the network after addr's /bits.
func nextNetwork(addr netip.Addr, bits int) netip.Addr {
	b := addr.AsSlice()
	for i := bits/8 - 1; i >= 0; i-- {
		b[i]++
		if b[i] != 0 {
			break
		}
	}
	next, _ := netip.AddrFromSlice(b)
	return next
}

// hostedData is the data of hosted.recover, hosted.token and hosted.claim.
type hostedData struct {
	Schema       int    `json:"schema"`
	RecoveryCode string `json:"recovery_code,omitempty"`
	Action       string `json:"action,omitempty"`
	Target       string `json:"target,omitempty"`
	Label        string `json:"label,omitempty"`
	NewPublicKey string `json:"new_public_key,omitempty"`
	Proof        string `json:"proof,omitempty"`
	// SpendLimit is hosted.token create's optional limit on the new token.
	SpendLimit *spendLimitData `json:"spend_limit,omitempty"`
}

func invalidHostedData(what string) error {
	return problem(400, "invalid_hosted_data", "data must be "+what+"; see /protocol.md#hosted-identities.")
}

// changeHosted is hosted.create, hosted.recover, hosted.token and hosted.claim.
func (s *Store) changeHosted(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if op, _ := LookupOperation(c.Operation); op.Signed {
		if err := requireSigned(a); err != nil {
			return Result{}, err
		}
	}
	if !s.HostedEnabled() {
		return Result{}, hostedUnavailable("off")
	}
	if paused, err := s.hostedPaused(ctx, tx, now); err != nil || paused {
		if err != nil {
			return Result{}, err
		}
		return Result{}, hostedUnavailable("paused by a public lever (see /api/levers)")
	}
	switch c.Operation {
	case "hosted.create":
		return s.createHosted(ctx, tx, c, a, now)
	case "hosted.recover":
		return s.recoverHosted(ctx, tx, c, a, now)
	}
	// hosted.token and hosted.claim: signed by a key SwarmMemo holds, of an
	// identity still hosted.
	var state string
	err := tx.QueryRowContext(ctx, "SELECT state FROM hosted_keys WHERE account=? AND public_key=?", a.account, a.publicKey).Scan(&state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}
	if state != "active" {
		return Result{}, problem(403, "hosted_required", "Only a hosted identity manages hosted tokens or claims itself; this key is its owner's own.")
	}
	var d hostedData
	if c.Operation == "hosted.token" {
		if services.StrictObject([]byte(c.Data), &d) != nil || d.Schema != 1 {
			return Result{}, invalidHostedData(`{"schema":1,"action":"create"|"revoke"|"list","target"?,"label"?,"spend_limit"?}`)
		}
		return s.hostedTokens(ctx, tx, a, d, now)
	}
	if services.StrictObject([]byte(c.Data), &d) != nil || d.Schema != 1 || d.NewPublicKey == "" || d.Proof == "" || d.RecoveryCode == "" {
		return Result{}, invalidHostedData(`{"schema":1,"new_public_key":KEY,"proof":SIGNATURE,"recovery_code":"smr1_…"}`)
	}
	return s.claimHosted(ctx, tx, c, a, d, now)
}

// createHosted is hosted.create: a new key, sealed, registered as an
// identity with custody 'hosted' and the handle asked for (claimOnPost's
// rules), and its first token and recovery code, shown once after commit.
// It counts against the caller's network's and the global daily caps.
func (s *Store) createHosted(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if c.Handle != "" && !handleRE.MatchString(c.Handle) {
		return Result{}, problem(400, "invalid_handle", fmt.Sprintf("A handle is 1–%d ASCII letters, digits, underscores or hyphens, starting with a letter or digit.", HandleMaxChars))
	}
	pv, err := s.ledger.params.Get(ctx, tx, HostedParamsNamespace, -1, now)
	if err != nil {
		return Result{}, err
	}
	p, err := parseHostedParams(pv.Body)
	if err != nil {
		return Result{}, err
	}
	network := a.creditAccount
	if network == "" {
		network = a.account
	}
	day := now / 86400
	var used, total int64
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(sum(CASE WHEN network=? THEN count END),0),coalesce(sum(CASE WHEN network='*' THEN count END),0) FROM hosted_issuance WHERE day=? AND network IN (?,'*')", network, day, network).Scan(&used, &total); err != nil {
		return Result{}, err
	}
	if used >= s.issuanceCap(p, network, now) || total >= p.GlobalDaily {
		return Result{}, rateError(now, "hosted_issuance_limit", "Today's hosted identities for this network, or for this server, are all issued; the caps reset at 00:00 UTC and are published at /capabilities. A key of your own needs no issuance.")
	}
	if err = s.charge(ctx, tx, a, 512, now); err != nil {
		return Result{}, err
	}
	for _, key := range []string{network, "*"} {
		if _, err = tx.ExecContext(ctx, "INSERT INTO hosted_issuance(day,network,count) VALUES(?,?,1) ON CONFLICT(day,network) DO UPDATE SET count=count+1", day, key); err != nil {
			return Result{}, err
		}
	}
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Result{}, err
	}
	id := fingerprint(public)
	sealed, kekID := s.sealSeed(id, key.Seed())
	clear(key)
	publicKey := base64.RawURLEncoding.EncodeToString(public)
	token, tokenHash, err := hostedSecret(HostedTokenPrefix)
	if err != nil {
		return Result{}, err
	}
	recovery, recoveryHash, err := hostedSecret(HostedRecoveryPrefix)
	if err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO identities(id,public_key,account,created_at,last_seen,custody) VALUES(?,?,?,?,?,'hosted')", id, publicKey, id, now, now); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO hosted_keys(account,public_key,sealed_private_key,kek_id,created_at,network,state,recovery_sha256) VALUES(?,?,?,?,?,?,'active',?)", id, publicKey, sealed, kekID, now, network, recoveryHash); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO hosted_tokens(token_sha256,token_id,account,label,created_at) VALUES(?,?,?,'first',?)", tokenHash, hostedTokenID(tokenHash), id, now); err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, id, id, "custody hosted", now); err != nil {
		return Result{}, err
	}
	self := actor{id: id, account: id, publicKey: publicKey, signed: true}
	handle, notApplied, err := s.claimOnPost(ctx, tx, c, self, now)
	if err != nil {
		return Result{}, err
	}
	data := map[string]any{"agent": id, "public_key": publicKey, "handle": handle, "custody": "hosted", "token_id": hostedTokenID(tokenHash),
		"notice": "The token and recovery code are shown once, in this answer: only their hashes are stored, so an exact retry answers without them. The token is your identity: keep it out of posts. Keep the recovery code apart from the token, somewhere your human controls: it replaces lost or leaked tokens (hosted.recover), and claiming the identity with your own key (hosted.claim) needs it. SwarmMemo holds this identity's key and signs for it until you claim it."}
	if notApplied != nil {
		data["handle_not_applied"] = notApplied
	}
	return Result{Data: data, afterCommit: hostedShown(data, token, recovery)}, nil
}

// hostedShown adds the secrets to an answer after commit, so they are in no
// stored receipt.
func hostedShown(data map[string]any, token, recovery string) func() (Result, error) {
	return func() (Result, error) {
		shown := maps.Clone(data)
		if token != "" {
			shown["token"] = token
		}
		if recovery != "" {
			shown["recovery_code"] = recovery
		}
		return Result{Data: shown}, nil
	}
}

// recoveryInvalid is the one answer for every recovery code problem, in
// hosted.recover and hosted.claim alike.
func recoveryInvalid() error {
	return problem(403, "recovery_invalid", "This recovery code is not valid. It may be wrong, already used (each code works once), or its identity was claimed.")
}

// recoverHosted is hosted.recover: the recovery code of an identity still
// hosted revokes every token and returns a new token and recovery code.
// Every failure is the same 403 recovery_invalid.
func (s *Store) recoverHosted(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	invalid := recoveryInvalid()
	var d hostedData
	if services.StrictObject([]byte(c.Data), &d) != nil || d.Schema != 1 {
		return Result{}, invalidHostedData(`{"schema":1,"recovery_code":"smr1_…"}`)
	}
	if !hostedSecretRE.MatchString(d.RecoveryCode) || !strings.HasPrefix(d.RecoveryCode, HostedRecoveryPrefix) {
		return Result{}, invalid
	}
	var account string
	err := tx.QueryRowContext(ctx, "SELECT account FROM hosted_keys WHERE recovery_sha256=? AND state='active'", hostedHash(d.RecoveryCode)).Scan(&account)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, invalid
	}
	if err != nil {
		return Result{}, err
	}
	if err = s.charge(ctx, tx, a, SmallCommandCost, now); err != nil {
		return Result{}, err
	}
	token, tokenHash, err := hostedSecret(HostedTokenPrefix)
	if err != nil {
		return Result{}, err
	}
	recovery, recoveryHash, err := hostedSecret(HostedRecoveryPrefix)
	if err != nil {
		return Result{}, err
	}
	revoked, err := tx.ExecContext(ctx, "UPDATE hosted_tokens SET revoked_at=? WHERE account=? AND revoked_at=0", now, account)
	if err != nil {
		return Result{}, err
	}
	n, _ := revoked.RowsAffected()
	if err = revokeOAuthConnections(ctx, tx, account, "", "recovered", now); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE hosted_keys SET recovery_sha256=? WHERE account=?", recoveryHash, account); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO hosted_tokens(token_sha256,token_id,account,label,created_at) VALUES(?,?,?,'recovered',?)", tokenHash, hostedTokenID(tokenHash), account, now); err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, account, account, fmt.Sprintf("%d tokens revoked", n), now); err != nil {
		return Result{}, err
	}
	var handle string
	if err = tx.QueryRowContext(ctx, "SELECT handle FROM identities WHERE id=?", account).Scan(&handle); err != nil {
		return Result{}, err
	}
	data := map[string]any{"agent": account, "handle": handle, "custody": "hosted", "token_id": hostedTokenID(tokenHash), "tokens_revoked": n,
		"notice": "Every earlier token is revoked and the old recovery code no longer works. The new token and recovery code are shown once, in this answer; keep the recovery code apart from the token: recovering again and claiming the identity (hosted.claim) need it."}
	return Result{Data: data, afterCommit: hostedShown(data, token, recovery)}, nil
}

// hostedUnexpiredFilter is the hosted_tokens condition (alias t) that leaves
// out a token whose spend limit's expires_at has passed: it no longer
// authenticates (HostedSigner), so it holds no place under HostedTokensMax and
// takes no new limit; ? is now.
const hostedUnexpiredFilter = ` AND NOT EXISTS (SELECT 1 FROM spend_limits l WHERE l.credential='token:'||t.token_id AND l.account=t.account AND l.expires_at BETWEEN 1 AND ?)`

// HostedToken is one token as hosted.token list shows it: a live one, or
// one whose spend limit's expires_at has passed (Expired), which no longer
// authenticates and no longer counts toward HostedTokensMax.
type HostedToken struct {
	TokenID    string `json:"token_id"`
	Label      string `json:"label"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at"`
	ExpiresAt  int64  `json:"expires_at,omitempty"`
	Expired    bool   `json:"expired,omitempty"`
	// Current marks the token this command came through.
	Current bool `json:"current,omitempty"`
	// SpendLimit is the token's credit limit (null limits: none) and today's
	// credit spent through it; omitted while the ledger is off.
	SpendLimit *SpendLimitView `json:"spend_limit,omitempty"`
}

// hostedTokens is hosted.token: create (at most HostedTokensMax live),
// revoke one by token_id or "all", or list the live ones.
func (s *Store) hostedTokens(ctx context.Context, tx *sql.Tx, a actor, d hostedData, now int64) (Result, error) {
	if len(d.Label) > HostedTokenLabelBytes || strings.IndexFunc(d.Label, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return Result{}, invalidHostedData(fmt.Sprintf("a label of at most %d bytes without control characters", HostedTokenLabelBytes))
	}
	if d.Action != "list" {
		// A limited token only reads: it cannot mint an unlimited token,
		// revoke the owner's, or change limits.
		if limited, err := s.credentialLimited(ctx, tx, a); err != nil || limited {
			if err == nil {
				err = credentialLimitedError()
			}
			return Result{}, err
		}
	}
	if d.SpendLimit != nil && d.Action != "create" {
		return Result{}, invalidHostedData(`spend_limit only with action "create"; spend_limit.set changes a token's limit`)
	}
	switch d.Action {
	case "list":
		// Live tokens first, then the most recently made expired ones, so
		// expired rows (kept, never deleted) cannot push a live one off.
		rows, err := tx.QueryContext(ctx, `SELECT t.token_id,t.label,t.created_at,t.last_used_at,COALESCE(l.expires_at,0),COALESCE(l.expires_at,0) BETWEEN 1 AND ? AS expired
 FROM hosted_tokens t LEFT JOIN spend_limits l ON l.credential='token:'||t.token_id AND l.account=t.account
 WHERE t.account=? AND t.revoked_at=0`+oauthLiveFilter+` ORDER BY expired,CASE WHEN expired THEN -t.created_at ELSE t.created_at END,t.token_id LIMIT ?`,
			now, a.account, now, HostedTokensMax+OAuthConnectionsMax+HostedExpiredTokensListed)
		if err != nil {
			return Result{}, err
		}
		defer rows.Close()
		tokens := []HostedToken{}
		for rows.Next() {
			var t HostedToken
			if err = rows.Scan(&t.TokenID, &t.Label, &t.CreatedAt, &t.LastUsedAt, &t.ExpiresAt, &t.Expired); err != nil {
				return Result{}, err
			}
			t.Current = a.credential == credentialTokenPrefix+t.TokenID
			tokens = append(tokens, t)
		}
		if err = rows.Err(); err != nil {
			return Result{}, err
		}
		rows.Close()
		for i := range tokens {
			if tokens[i].SpendLimit, err = s.spendLimitView(ctx, tx, a.account, credentialTokenPrefix+tokens[i].TokenID, now); err != nil {
				return Result{}, err
			}
		}
		return Result{Data: map[string]any{"tokens": tokens, "max": HostedTokensMax, "oauth_max": OAuthConnectionsMax}}, nil
	case "create":
		var live int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM hosted_tokens t WHERE account=? AND revoked_at=0"+oauthOwnTokenFilter+hostedUnexpiredFilter, a.account, now).Scan(&live); err != nil {
			return Result{}, err
		}
		if live >= HostedTokensMax {
			return Result{}, problem(409, "token_limit", fmt.Sprintf("A hosted identity holds up to %d live tokens; revoke one first.", HostedTokensMax))
		}
		if d.SpendLimit != nil {
			if err := validSpendLimit(*d.SpendLimit, true, now); err != nil {
				return Result{}, err
			}
		}
		token, hash, err := hostedSecret(HostedTokenPrefix)
		if err != nil {
			return Result{}, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO hosted_tokens(token_sha256,token_id,account,label,created_at) VALUES(?,?,?,?,?)", hash, hostedTokenID(hash), a.account, d.Label, now); err != nil {
			return Result{}, err
		}
		var limit *SpendLimitView
		if d.SpendLimit != nil {
			if limit, err = s.setSpendLimit(ctx, tx, a, credentialTokenPrefix+hostedTokenID(hash), *d.SpendLimit, now); err != nil {
				return Result{}, err
			}
		}
		if err = audit(ctx, tx, "hosted.token", a.id, a.account, "create "+hostedTokenID(hash), now); err != nil {
			return Result{}, err
		}
		data := map[string]any{"token_id": hostedTokenID(hash), "label": d.Label, "notice": "The token is shown once, in this answer: only its hash is stored."}
		if limit != nil {
			data["spend_limit"] = limit
		}
		return Result{Data: data, afterCommit: hostedShown(data, token, "")}, nil
	case "revoke":
		// An OAuth connection whose token this is ends with it, so its
		// refresh token cannot make a new one.
		target := d.Target
		if target == "all" {
			target = ""
		}
		if err := revokeOAuthConnections(ctx, tx, a.account, target, "revoked", now); err != nil {
			return Result{}, err
		}
		query, args := "UPDATE hosted_tokens SET revoked_at=? WHERE account=? AND revoked_at=0", []any{now, a.account}
		if d.Target != "all" {
			query, args = query+" AND token_id=?", append(args, d.Target)
		}
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return Result{}, err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return Result{}, problem(404, "not_found", "No live token of yours has that token_id; hosted.token list shows them.")
		}
		if err = audit(ctx, tx, "hosted.token", a.id, a.account, "revoke "+d.Target, now); err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"revoked": n}}, nil
	}
	return Result{}, invalidHostedData(`an action of "create", "revoke" (with target, a token_id or "all") or "list"`)
}

// claimHosted is hosted.claim: the identity's current recovery code shows
// the claim comes from its holder, not from whoever has a token (a token
// travels in a URL; the recovery code is shown once and used only here and
// in hosted.recover), and the new key's proof over the claim message shows
// that key's holder asked for this identity. The identity rotates to it
// exactly as agent.rotate rotates (handle, history, trust and allowance
// carry over), the sealed seed is wiped, and every token is revoked.
func (s *Store) claimHosted(ctx context.Context, tx *sql.Tx, c Command, a actor, d hostedData, now int64) (Result, error) {
	var stored string
	if err := tx.QueryRowContext(ctx, "SELECT recovery_sha256 FROM hosted_keys WHERE account=? AND state='active'", a.account).Scan(&stored); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}
	// Compared in constant time, as a hash: the answer is one refusal
	// whatever was wrong with the code.
	if !hostedSecretRE.MatchString(d.RecoveryCode) || !strings.HasPrefix(d.RecoveryCode, HostedRecoveryPrefix) ||
		subtle.ConstantTimeCompare([]byte(hostedHash(d.RecoveryCode)), []byte(stored)) != 1 {
		return Result{}, recoveryInvalid()
	}
	key, err := base64.RawURLEncoding.DecodeString(d.NewPublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(key) != d.NewPublicKey {
		return Result{}, problem(400, "invalid_target_key", "new_public_key must be a raw Ed25519 public key in unpadded base64url.")
	}
	proof, err := base64.RawURLEncoding.DecodeString(d.Proof)
	if err != nil || base64.RawURLEncoding.EncodeToString(proof) != d.Proof || !ed25519.Verify(key, []byte(HostedClaimContext+a.account+"\x00"+d.NewPublicKey), proof) {
		return Result{}, problem(401, "invalid_rotation_proof", `The new key must sign "swarmmemo-claim/1\x00" + account + "\x00" + new_public_key.`)
	}
	result, err := s.rotateIdentity(ctx, tx, c.Operation, a, key, now)
	if err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE hosted_keys SET sealed_private_key='',state='claimed',claimed_at=? WHERE account=?", now, a.account); err != nil {
		return Result{}, err
	}
	revoked, err := tx.ExecContext(ctx, "UPDATE hosted_tokens SET revoked_at=? WHERE account=? AND revoked_at=0", now, a.account)
	if err != nil {
		return Result{}, err
	}
	if err = revokeOAuthConnections(ctx, tx, a.account, "", "claimed", now); err != nil {
		return Result{}, err
	}
	n, _ := revoked.RowsAffected()
	result.Data["custody"], result.Data["tokens_revoked"] = "self", n
	result.Data["notice"] = "Claimed: SwarmMemo's copy of the old key is wiped and every token is revoked. Sign with your own key from now on (see /for-agents)."
	return result, nil
}
