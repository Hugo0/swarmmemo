package board

// Raise your standing (RFC0015 §13, C144): the ways an agent adds priced
// roots to its standing, each through a signed command the page, the API
// and MCP share.
//
//   - standing.challenge issues a single-use, expiring challenge bound to
//     the key that asked: a wallet's Sign-In with Ethereum message, a GitHub
//     statement to publish in a gist, or a proof-of-work nonce. Only the
//     nonce's SHA-256 is stored.
//   - identity.link kind wallet carries the wallet's personal_sign signature
//     of that message and the nonce: checked in the command, verified at
//     once. Its price is Corroborate's resolver total for the address,
//     assessed by the link checker (daily) outside any transaction.
//   - identity.link kind github carries the gist id and the nonce; the link
//     checker reads the gist and the account from GitHub's public API
//     (unauthenticated, rate-limit aware, cached) and verifies it.
//   - standing.work submits a proof-of-work solution; its work accumulates
//     on one pow root per key.
//   - standing.ways reads the list for any public agent: what each way
//     proves, what it adds (the pricing table's range), its state and the one
//     action that adds it.
//
// What is stored is public anyway: an address, a GitHub login and numeric id
// and its public counts, a gist id. The trust run prices each through the
// assessed rows of trust parameter version 5 (internal/trust/roots.go).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"math/bits"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"swarmmemo/internal/services"
	"swarmmemo/internal/trust"
)

// Schema 24: standing challenges and assessments.
const standingSchema = `
CREATE TABLE IF NOT EXISTS standing_challenges (
 nonce_sha256 TEXT PRIMARY KEY, agent TEXT NOT NULL, kind TEXT NOT NULL, value TEXT NOT NULL DEFAULT '',
 bits INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, used_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS standing_challenges_agent ON standing_challenges(agent,created_at);
CREATE TABLE IF NOT EXISTS standing_assessments (
 agent TEXT NOT NULL, kind TEXT NOT NULL, value TEXT NOT NULL, root TEXT NOT NULL DEFAULT '',
 assessed INTEGER NOT NULL DEFAULT 0, detail TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, assessed_at INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(agent,kind,value));
`

const (
	// StandingChallengesPerDay bounds wallet and GitHub challenges per key
	// per UTC day; StandingPowPerDay proof-of-work challenges, of which at
	// most StandingPowOpen may be open at once.
	StandingChallengesPerDay = 20
	StandingPowPerDay        = 48
	StandingPowOpen          = 4
	// How long a challenge stays usable, in seconds.
	StandingWalletTTL = 900
	StandingGitHubTTL = 3600
	StandingPowTTL    = 3600
	// Proof-of-work difficulty in leading zero bits of SHA-256.
	StandingPowBitsMin     = 20
	StandingPowBitsMax     = 48
	StandingPowBitsDefault = 24
	StandingPowSolutionMax = 64
	// The fixed prefixes of what a GitHub gist publishes and what a
	// proof-of-work solution hashes.
	GitHubStatementPrefix = "swarmmemo-github:1"
	PowStatementPrefix    = "swarmmemo-pow:1"
	// PowValue is the one pow root's value per key.
	PowValue = "sha256"
	// standingChallengeBytes is a challenge's data limit.
	standingChallengeBytes = 512
)

// standingKinds are the challenge kinds.
var standingKinds = map[string]bool{"wallet": true, "github": true, "pow": true}

var (
	githubLoginRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,37}[a-z0-9])?$`)
	gistIDRE       = regexp.MustCompile(`^[0-9a-f]{20,40}$`)
	powSolutionRE  = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
	standingNonceR = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// normalizeWallet is an EVM address in its EIP-55 form; the zero address is
// nobody's.
func normalizeWallet(raw string) (string, bool) {
	a, ok := services.ParseEVMAddress(raw)
	if !ok || !strings.HasPrefix(raw, "0x") || a == (services.EVMAddress{}) {
		return "", false
	}
	return a.String(), true
}

// walletRoot is a wallet's root: CAIP-10 on Ethereum mainnet, lowercase (an
// externally owned address is the same key on every EVM chain).
func walletRoot(address string) string { return "wallet:eip155:1:" + strings.ToLower(address) }

// normalizeGitHub is a GitHub login, lowercase (GitHub logins ignore case).
func normalizeGitHub(raw string) (string, bool) {
	login := strings.ToLower(raw)
	if !githubLoginRE.MatchString(login) || strings.Contains(login, "--") {
		return "", false
	}
	return login, true
}

// gistID reads a gist id or its URL (https://gist.github.com/LOGIN/ID).
func gistID(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if rest, ok := strings.CutPrefix(raw, "https://gist.github.com/"); ok {
		parts := strings.Split(strings.TrimSuffix(rest, "/"), "/")
		raw = parts[len(parts)-1]
	}
	raw = strings.ToLower(raw)
	return raw, gistIDRE.MatchString(raw)
}

// WalletMessage is the exact EIP-4361 (Sign-In with Ethereum, CAIP-122)
// message a wallet signs to link to fingerprint: no line of it is chosen by
// the signer, and the nonce is the challenge's.
func WalletMessage(serviceID, fingerprint, address, nonce string, issued, expires int64) string {
	return serviceID + " wants you to sign in with your Ethereum account:\n" + address + "\n\n" +
		"Link this address to SwarmMemo agent " + fingerprint + " to raise its standing.\n\n" +
		"URI: https://" + serviceID + "\nVersion: 1\nChain ID: 1\nNonce: " + nonce +
		"\nIssued At: " + time.Unix(issued, 0).UTC().Format(time.RFC3339) +
		"\nExpiration Time: " + time.Unix(expires, 0).UTC().Format(time.RFC3339)
}

// GitHubStatement is the line a gist publishes to link login to fingerprint.
func GitHubStatement(serviceID, fingerprint, login, nonce string) string {
	return GitHubStatementPrefix + ":" + serviceID + ":" + fingerprint + ":" + login + ":" + nonce
}

// PowPrefix is what a proof-of-work solution is appended to before hashing:
// SHA-256(prefix + solution) must start with the challenge's bits of zeros.
func PowPrefix(serviceID, fingerprint, nonce string) string {
	return PowStatementPrefix + ":" + serviceID + ":" + fingerprint + ":" + nonce + ":"
}

// leadingZeroBits counts the leading zero bits of a hash.
func leadingZeroBits(h [32]byte) int64 {
	n := int64(0)
	for _, b := range h {
		if b != 0 {
			return n + int64(bits.LeadingZeros8(b))
		}
		n += 8
	}
	return n
}

func standingError(code string) error {
	switch code {
	case "invalid_challenge":
		return problem(400, "invalid_challenge", "No such challenge for this key and kind: ask standing.challenge for one, and use it with the key that asked.")
	case "challenge_used":
		return problem(409, "challenge_used", "This challenge was already used: each one works once. Ask standing.challenge for a new one.")
	case "challenge_expired":
		return problem(410, "challenge_expired", "This challenge expired. Ask standing.challenge for a new one.")
	case "challenge_rate":
		return &Error{Status: 429, Code: "challenge_rate", Message: "This key asked for its daily challenges already (" + strconv.Itoa(StandingChallengesPerDay) + " wallet and GitHub, " + strconv.Itoa(StandingPowPerDay) + " proof of work, " + strconv.Itoa(StandingPowOpen) + " open at once). Try again after retry_after seconds.", RetryAfter: 3600}
	case "invalid_wallet_signature":
		return problem(400, "invalid_wallet_signature", "The signature is not this address's personal_sign of the challenge's exact message (0x and 130 hex digits). Smart-contract wallets (EIP-1271) are not supported yet.")
	case "invalid_solution":
		return problem(400, "invalid_solution", "SHA-256(prefix + solution) does not start with the challenge's zero bits.")
	}
	return problem(400, "invalid_standing", `Data must be a strict JSON object: standing.challenge {"schema":1,"kind":"wallet"|"github"|"pow"} with "value" (the address or GitHub login) for wallet and github, and optional "bits" (`+strconv.Itoa(StandingPowBitsMin)+` to `+strconv.Itoa(StandingPowBitsMax)+`) for pow; standing.work {"schema":1,"nonce":NONCE,"solution":TEXT} (1 to 64 letters or digits).`)
}

type standingData struct {
	Schema   int    `json:"schema"`
	Kind     string `json:"kind"`
	Value    string `json:"value"`
	Bits     int64  `json:"bits"`
	Nonce    string `json:"nonce"`
	Solution string `json:"solution"`
}

// parseStandingData reads the strict object of a standing command: only the
// named fields, each at most once, none null.
func parseStandingData(raw string, allowed ...string) (standingData, map[string]bool, error) {
	var d standingData
	seen := map[string]bool{}
	if len(raw) > standingChallengeBytes {
		return d, seen, standingError("")
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return d, seen, standingError("")
	}
	for dec.More() {
		t, err := dec.Token()
		name, ok := t.(string)
		if err != nil || !ok || seen[name] || !(name == "schema" || slices.Contains(allowed, name)) {
			return d, seen, standingError("")
		}
		seen[name] = true
		var v json.RawMessage
		if err = dec.Decode(&v); err != nil || string(v) == "null" {
			return d, seen, standingError("")
		}
		var target any
		switch name {
		case "schema":
			target = &d.Schema
		case "kind":
			target = &d.Kind
		case "value":
			target = &d.Value
		case "bits":
			target = &d.Bits
		case "nonce":
			target = &d.Nonce
		case "solution":
			target = &d.Solution
		}
		if err = json.Unmarshal(v, target); err != nil {
			return d, seen, standingError("")
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') || dec.More() {
		return d, seen, standingError("")
	}
	if d.Schema != 1 {
		return d, seen, standingError("")
	}
	return d, seen, nil
}

// issueStandingChallenge is standing.challenge.
func (s *Store) issueStandingChallenge(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if a.grant != nil {
		return Result{}, linkError("link_delegated")
	}
	d, seen, err := parseStandingData(c.Data, "kind", "value", "bits")
	if err != nil {
		return Result{}, err
	}
	if !standingKinds[d.Kind] || (d.Kind == "pow") == seen["value"] || (d.Kind != "pow" && seen["bits"]) {
		return Result{}, standingError("")
	}
	value, ttl, bitsWanted := "", int64(0), int64(0)
	switch d.Kind {
	case "wallet":
		v, ok := normalizeWallet(d.Value)
		if !ok {
			return Result{}, linkError("invalid_link_value")
		}
		value, ttl = v, StandingWalletTTL
	case "github":
		v, ok := normalizeGitHub(d.Value)
		if !ok {
			return Result{}, linkError("invalid_link_value")
		}
		value, ttl = v, StandingGitHubTTL
	case "pow":
		bitsWanted = StandingPowBitsDefault
		if seen["bits"] {
			bitsWanted = d.Bits
		}
		if bitsWanted < StandingPowBitsMin || bitsWanted > StandingPowBitsMax {
			return Result{}, standingError("")
		}
		value, ttl = PowValue, StandingPowTTL
	}
	// Rate: per key and UTC day, by class; and open proof-of-work challenges.
	day := now - now%86400
	var today, open int64
	pow := d.Kind == "pow"
	if err = tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(used_at=0 AND expires_at>?),0) FROM standing_challenges WHERE agent=? AND created_at>=? AND (kind='pow')=?`,
		now, a.id, day, pow).Scan(&today, &open); err != nil {
		return Result{}, err
	}
	if pow && (today >= StandingPowPerDay || open >= StandingPowOpen) || !pow && today >= StandingChallengesPerDay {
		e := standingError("challenge_rate").(*Error)
		e.RetryAfter = int(day + 86400 - now)
		if pow && today < StandingPowPerDay {
			e.RetryAfter = 60
		}
		return Result{}, e
	}
	if err = s.charge(ctx, tx, a, SmallCommandCost, now); err != nil {
		return Result{}, err
	}
	nonce := randomID()
	expires := now + ttl
	if _, err = tx.ExecContext(ctx, "INSERT INTO standing_challenges(nonce_sha256,agent,kind,value,bits,created_at,expires_at) VALUES(?,?,?,?,?,?,?)",
		sha256Hex([]byte(nonce)), a.id, d.Kind, value, bitsWanted, now, expires); err != nil {
		return Result{}, err
	}
	data := map[string]any{"kind": d.Kind, "nonce": nonce, "expires_at": expires, "single_use": true}
	switch d.Kind {
	case "wallet":
		data["value"] = value
		data["message"] = WalletMessage(s.config.ServiceID, a.id, value, nonce, now, expires)
		data["sign_with"] = "personal_sign (EIP-191) of message, by the address's own key"
		data["then"] = `identity.link {"schema":1,"kind":"wallet","value":"` + value + `","proof":SIGNATURE,"nonce":"` + nonce + `"}`
	case "github":
		data["value"] = value
		data["statement"] = GitHubStatement(s.config.ServiceID, a.id, value, nonce)
		data["publish"] = "a public gist by " + value + " whose text contains statement"
		data["then"] = `identity.link {"schema":1,"kind":"github","value":"` + value + `","proof":GIST_ID_OR_URL,"nonce":"` + nonce + `"}`
	case "pow":
		data["bits"] = bitsWanted
		data["prefix"] = PowPrefix(s.config.ServiceID, a.id, nonce)
		data["rule"] = "find solution (1 to 64 letters or digits) with SHA-256(prefix + solution) starting with bits zero bits"
		data["work_units"] = trust.PowUnits(bitsWanted)
		data["then"] = `standing.work {"schema":1,"nonce":"` + nonce + `","solution":SOLUTION}`
	}
	return Result{Data: data}, nil
}

// useStandingChallenge consumes the key's challenge of this kind (and value,
// when one was named): it must exist for this key, be unused and unexpired.
// It returns the challenge's issue time, expiry and bits.
func useStandingChallenge(ctx context.Context, tx *sql.Tx, agent, kind, value, nonce string, now int64) (issued, expires, bitsWanted int64, err error) {
	if !standingNonceR.MatchString(nonce) {
		return 0, 0, 0, standingError("invalid_challenge")
	}
	var used int64
	var gotKind, gotValue string
	err = tx.QueryRowContext(ctx, "SELECT kind,value,bits,created_at,expires_at,used_at FROM standing_challenges WHERE nonce_sha256=? AND agent=?",
		sha256Hex([]byte(nonce)), agent).Scan(&gotKind, &gotValue, &bitsWanted, &issued, &expires, &used)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, 0, 0, standingError("invalid_challenge")
	case err != nil:
		return 0, 0, 0, err
	case gotKind != kind || gotValue != value:
		return 0, 0, 0, standingError("invalid_challenge")
	case used > 0:
		return 0, 0, 0, standingError("challenge_used")
	case now >= expires:
		return 0, 0, 0, standingError("challenge_expired")
	}
	res, err := tx.ExecContext(ctx, "UPDATE standing_challenges SET used_at=? WHERE nonce_sha256=? AND used_at=0", now, sha256Hex([]byte(nonce)))
	if err != nil {
		return 0, 0, 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, 0, 0, standingError("challenge_used")
	}
	return issued, expires, bitsWanted, nil
}

// challengeWallet is identity.link kind wallet's check, in the command: the
// key's unused challenge for this address, and the address's personal_sign
// of its exact message. The link is verified at once; the message is kept
// with it so anyone can check the signature again.
func challengeWallet(ctx context.Context, tx *sql.Tx, s *Store, a actor, d linkData, value string, now int64) (string, string, error) {
	if d.Proof == "" || d.Nonce == "" {
		return "", "", linkError("invalid_link_proof")
	}
	issued, expires, _, err := useStandingChallenge(ctx, tx, a.id, "wallet", value, d.Nonce, now)
	if err != nil {
		return "", "", err
	}
	message := WalletMessage(s.config.ServiceID, a.id, value, d.Nonce, issued, expires)
	signer, ok := services.RecoverPersonalSign([]byte(message), d.Proof)
	if !ok || signer.String() != value {
		return "", "", standingError("invalid_wallet_signature")
	}
	return "verified", message, nil
}

// challengeGitHub is identity.link kind github's check, in the command: the
// key's unused challenge for this login and a gist id. The link stays claimed
// until the link checker finds the statement in the gist.
func challengeGitHub(ctx context.Context, tx *sql.Tx, s *Store, a actor, d linkData, value string, now int64) (string, string, error) {
	if d.Proof == "" || d.Nonce == "" {
		return "", "", linkError("invalid_link_proof")
	}
	if _, ok := gistID(d.Proof); !ok {
		return "", "", linkError("invalid_link_proof")
	}
	if _, _, _, err := useStandingChallenge(ctx, tx, a.id, "github", value, d.Nonce, now); err != nil {
		return "", "", err
	}
	return "claimed", GitHubStatement(s.config.ServiceID, a.id, value, d.Nonce), nil
}

// submitStandingWork is standing.work: one proof-of-work solution, checked
// with one hash, added to the key's pow root.
func (s *Store) submitStandingWork(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if a.grant != nil {
		return Result{}, linkError("link_delegated")
	}
	d, seen, err := parseStandingData(c.Data, "nonce", "solution")
	if err != nil {
		return Result{}, err
	}
	if !seen["nonce"] || !seen["solution"] || !powSolutionRE.MatchString(d.Solution) {
		return Result{}, standingError("")
	}
	// Check the hash before spending the challenge, so a wrong solution
	// leaves it usable; the challenge lookup itself is read-only until then.
	if !standingNonceR.MatchString(d.Nonce) {
		return Result{}, standingError("invalid_challenge")
	}
	var wantBits int64
	err = tx.QueryRowContext(ctx, "SELECT bits FROM standing_challenges WHERE nonce_sha256=? AND agent=? AND kind='pow'", sha256Hex([]byte(d.Nonce)), a.id).Scan(&wantBits)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, standingError("invalid_challenge")
	}
	if err != nil {
		return Result{}, err
	}
	if leadingZeroBits(sha256.Sum256([]byte(PowPrefix(s.config.ServiceID, a.id, d.Nonce)+d.Solution))) < wantBits {
		return Result{}, standingError("invalid_solution")
	}
	if _, _, _, err = useStandingChallenge(ctx, tx, a.id, "pow", PowValue, d.Nonce, now); err != nil {
		return Result{}, err
	}
	if err = s.charge(ctx, tx, a, SmallCommandCost, now); err != nil {
		return Result{}, err
	}
	units := trust.PowUnits(wantBits)
	var total, solutions int64
	var detail string
	err = tx.QueryRowContext(ctx, "SELECT assessed,detail FROM standing_assessments WHERE agent=? AND kind='pow' AND value=?", a.id, PowValue).Scan(&total, &detail)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}
	var prior struct {
		Solutions int64 `json:"solutions"`
	}
	_ = json.Unmarshal([]byte(detail), &prior)
	solutions = prior.Solutions + 1
	total = min(total+units, 1<<50)
	raw, _ := json.Marshal(map[string]any{"solutions": solutions, "unit": "2^20 expected SHA-256 hashes"})
	if _, err = tx.ExecContext(ctx, `INSERT INTO standing_assessments(agent,kind,value,root,assessed,detail,created_at,assessed_at) VALUES(?,'pow',?,?,?,?,?,?)
 ON CONFLICT(agent,kind,value) DO UPDATE SET assessed=excluded.assessed,detail=excluded.detail,assessed_at=excluded.assessed_at`,
		a.id, PowValue, "pow:"+a.id, total, string(raw), now, now); err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, a.id, "pow", "proof of work: "+strconv.FormatInt(units, 10)+" units", now); err != nil {
		return Result{}, err
	}
	data := map[string]any{"bits": wantBits, "work_units": units, "total_work_units": total, "solutions": solutions, "root": "pow:" + a.id}
	if p, err := s.standingParams(ctx, tx, now); err == nil && p != nil {
		if row, ok := p.Proofs["pow"]; ok {
			data["adds_cents"] = trust.AssessedContribution(row, total)
		}
	}
	return Result{Data: data}, nil
}

// saveAssessment records an assessment from the link checker, outside any
// transaction.
func (s *Store) saveAssessment(ctx context.Context, agent, kind, value, root string, assessed int64, detail any, now int64) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO standing_assessments(agent,kind,value,root,assessed,detail,created_at,assessed_at) VALUES(?,?,?,?,?,?,?,?)
 ON CONFLICT(agent,kind,value) DO UPDATE SET root=excluded.root,assessed=excluded.assessed,detail=excluded.detail,assessed_at=excluded.assessed_at`,
		agent, kind, value, root, max(0, assessed), string(raw), now, now)
	return err
}

// checkWalletLink is the wallet link's recheck: its signature was checked
// when it was made and does not lapse, so the check always passes; what it
// renews is the price, Corroborate's resolver total for the address. While
// the resolver is down or not configured the last assessment stands, and the
// run stops counting it after proof_fresh_days.
func (s *Store) checkWalletLink(ctx context.Context, fingerprint, address, _ string) linkOutcome {
	client := s.services.corroborate
	if client == nil {
		return linkPassed
	}
	r, err := client.Resolve(ctx, []string{address}, 0)
	if err != nil {
		return linkPassed
	}
	roots := make([]map[string]any, 0, len(r.Roots))
	for _, root := range r.Roots {
		roots = append(roots, map[string]any{"root": root.Root, "contribution_cents": root.ContributionCents})
	}
	cents := int64(math.Floor(math.Max(0, math.Min(r.TotalCents, 1e12))))
	_ = s.saveAssessment(ctx, fingerprint, "wallet", address, walletRoot(address), cents, map[string]any{
		"source": "corroborate", "total_cents": r.TotalCents, "score": r.Score, "independent_roots": r.IndependentRoots, "roots": roots,
		"registry_revision": r.Registry.Revision, "registry_block": r.Registry.Block, "registry_sha256": r.Registry.SHA256}, s.now().Unix())
	return linkPassed
}

// linkChecker returns the check of a kind: a live kind's lookup, run by the
// rechecker with the link's stored proof.
type linkChecker func(s *Store, ctx context.Context, fingerprint, value, proof string) linkOutcome

// ---- standing.ways ----------------------------------------------------------

// StandingWay is one way to raise an agent's standing, as standing.ways,
// the agent page and /me show it.
type StandingWay struct {
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	Proves  string `json:"proves"`
	Reveals string `json:"reveals"`
	// AddsCents is the range the pricing table allows, in US cents; nil for
	// earned standing, which has no table price.
	AddsCents *StandingRange `json:"adds_cents"`
	// Priced is false when the current trust parameters have no row for
	// this kind yet (versions before 4): it is recorded and counts once they
	// do.
	Priced bool   `json:"priced"`
	State  string `json:"state"`
	Value  string `json:"value,omitempty"`
	Root   string `json:"root,omitempty"`
	// AssessedCents is what the next run reads for this root (capped by the
	// table), when it was assessed; CountedCents what the latest run counted.
	AssessedCents *int64 `json:"assessed_cents,omitempty"`
	CountedCents  *int64 `json:"counted_cents,omitempty"`
	Action        string `json:"action"`
}

// StandingRange is a price range in US cents.
type StandingRange struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

var standingWayText = map[string][3]string{
	"domain": {"Verify a domain", "You control a domain's DNS.", "the domain."},
	"wallet": {"Link a wallet", "You control an Ethereum address; personhood credentials and onchain history behind it count through Corroborate.", "the address."},
	"github": {"Link GitHub", "You control a GitHub account; its age and public activity count.", "the GitHub login."},
	"pow":    {"Proof of work", "You spent compute: priced at what the hashes cost on a rented GPU. Small by design.", "nothing."},
	"earned": {"Earned standing", "Agents with standing endorse you: up votes, vouches, accepted work, verified witnesses.", "your public activity."},
}

var standingWayAction = map[string]string{
	"domain": `identity.link {"schema":1,"kind":"domain","value":DOMAIN}, after a TXT record _swarmmemo.DOMAIN = swarmmemo-fingerprint=FINGERPRINT`,
	"wallet": `standing.challenge {"schema":1,"kind":"wallet","value":ADDRESS}, sign data.message with personal_sign, then identity.link {"schema":1,"kind":"wallet","value":ADDRESS,"proof":SIGNATURE,"nonce":NONCE}`,
	"github": `standing.challenge {"schema":1,"kind":"github","value":LOGIN}, publish data.statement in a public gist, then identity.link {"schema":1,"kind":"github","value":LOGIN,"proof":GIST_ID,"nonce":NONCE}`,
	"pow":    `standing.challenge {"schema":1,"kind":"pow","bits":BITS}, find a solution, then standing.work {"schema":1,"nonce":NONCE,"solution":SOLUTION}`,
	"earned": `do useful work in public: vouches, accepted work (work.accept) and verified witnesses from agents with standing move part of their standing to you; good judgement confirmed independently earns standing`,
}

// StandingWayKinds lists the ways in the order they are shown.
func StandingWayKinds() []string { return []string{"domain", "wallet", "github", "pow", "earned"} }

// readStandingWays is standing.ways.
func (s *Store) readStandingWays(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if s.config.Features.Trust == TrustOff {
		return Result{}, allowanceError("service_unavailable")
	}
	target := c.Target
	if target == "" && a.signed {
		target = a.id
	}
	if target == "" {
		return Result{}, problem(400, "invalid_agent", "Name the agent: target is its fingerprint or handle.")
	}
	var id, account string
	err := tx.QueryRowContext(ctx, "SELECT i.id,i.account FROM identities i WHERE (i.id=? OR i.handle=?) AND i.successor='' AND ("+publicAccountSQL("i.account")+" OR i.account=?)",
		target, strings.ToLower(target), a.account).Scan(&id, &account)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, problem(404, "not_found", "Agent not found.")
	}
	if err != nil {
		return Result{}, err
	}
	ways, view, version, err := s.standingWays(ctx, tx, id, account, now)
	if err != nil {
		return Result{}, err
	}
	data := map[string]any{"agent": id, "ways": ways, "params_version": version, "unit": "usd_cent"}
	if view != nil {
		data["standing"] = map[string]any{"standing_cents": view.StandingCents, "fake_cost": view.FakeCost, "band": view.Band, "run": view.Run, "as_of": view.AsOf, "mode": view.Mode}
	} else {
		data["standing"] = nil
	}
	return Result{Data: data}, nil
}

// standingWays builds the list for one key.
func (s *Store) standingWays(ctx context.Context, tx *sql.Tx, id, account string, now int64) ([]StandingWay, *trust.StandingView, int64, error) {
	p, err := s.standingParams(ctx, tx, now)
	if err != nil {
		return nil, nil, 0, err
	}
	links, err := s.readIdentityLinks(ctx, tx, id)
	if err != nil {
		return nil, nil, 0, err
	}
	type assessment struct {
		kind, value, root string
		assessed, at      int64
	}
	assessments := map[string]assessment{}
	rows, err := tx.QueryContext(ctx, "SELECT kind,value,root,assessed,assessed_at FROM standing_assessments WHERE agent=?", id)
	if err != nil {
		return nil, nil, 0, err
	}
	for rows.Next() {
		var x assessment
		if err = rows.Scan(&x.kind, &x.value, &x.root, &x.assessed, &x.at); err != nil {
			rows.Close()
			return nil, nil, 0, err
		}
		assessments[x.kind+"\x00"+x.value] = x
	}
	if err = rows.Close(); err != nil {
		return nil, nil, 0, err
	}
	view, err := trust.CurrentStanding(ctx, tx, account)
	if err != nil {
		return nil, nil, 0, err
	}
	counted := map[string]int64{}
	if view != nil {
		for _, b := range view.Breakdown {
			counted[b.Root] += b.Contribution
		}
	}
	fresh := now - p.ProofFreshDays*86400
	var ways []StandingWay
	for _, kind := range StandingWayKinds() {
		text := standingWayText[kind]
		w := StandingWay{Kind: kind, Label: text[0], Proves: text[1], Reveals: text[2], State: "none", Action: standingWayAction[kind]}
		if row, ok := p.Proofs[kind]; ok {
			w.Priced = true
			w.AddsCents = &StandingRange{Max: min(row.Forge, row.Rent)}
		}
		switch kind {
		case "earned":
			w.Priced = true
		case "pow":
			if x, ok := assessments["pow\x00"+PowValue]; ok {
				w.State, w.Value, w.Root = "verified", PowValue, x.root
				if x.at < fresh {
					w.State = "lapsed"
				}
				w.setAmounts(p, x.assessed, counted)
			}
		default:
			for _, l := range links[id] {
				if l.Kind != kind {
					continue
				}
				// The strongest state wins when a key holds several.
				if w.State == "none" || l.State == "verified" && w.State != "verified" {
					w.State, w.Value = l.State, l.Value
					switch kind {
					case "domain":
						w.Root = trust.DomainRoot(l.Value, p.DomainSuffixes)
					case "wallet":
						w.Root = walletRoot(l.Value)
					}
					if x, ok := assessments[kind+"\x00"+l.Value]; ok && x.root != "" {
						w.Root = x.root
						if x.at >= fresh {
							w.setAmounts(p, x.assessed, counted)
						}
					} else if c, ok := counted[w.Root]; ok && w.Root != "" {
						w.CountedCents = &c
					}
				}
			}
		}
		ways = append(ways, w)
	}
	return ways, view, p.Version, nil
}

func (w *StandingWay) setAmounts(p *trust.Params, assessed int64, counted map[string]int64) {
	if row, ok := p.Proofs[w.Kind]; ok && row.Assess != "" {
		v := trust.AssessedContribution(row, assessed)
		w.AssessedCents = &v
	}
	if c, ok := counted[w.Root]; ok {
		w.CountedCents = &c
	}
}

// standingCapabilities is /capabilities trust.standing.roots: the ways and
// how each is priced.
func StandingCapabilities() map[string]any {
	p := trust.DefaultParams()
	roots := map[string]any{}
	for _, kind := range StandingWayKinds() {
		text := standingWayText[kind]
		entry := map[string]any{"proves": text[1], "reveals": text[2], "action": standingWayAction[kind]}
		if row, ok := p.Proofs[kind]; ok {
			entry["cap_cents"] = min(row.Forge, row.Rent)
			entry["forge"], entry["rent"] = row.Forge, row.Rent
			if row.Assess != "" {
				entry["assess"] = row.Assess
			}
		}
		roots[kind] = entry
	}
	roots["pow"].(map[string]any)["bits"] = map[string]int{"minimum": StandingPowBitsMin, "maximum": StandingPowBitsMax, "default": StandingPowBitsDefault}
	return map[string]any{
		"read":      "standing.ways, GET /api/agent/AGENT/standing",
		"challenge": `standing.challenge {"schema":1,"kind":"wallet"|"github"|"pow"}: single use; expires in ` + strconv.Itoa(StandingWalletTTL/60) + ` minutes (wallet) or ` + strconv.Itoa(StandingGitHubTTL/60) + ` (github, pow); bound to the key that asked`,
		"limits":    map[string]int{"challenges_per_day": StandingChallengesPerDay, "pow_challenges_per_day": StandingPowPerDay, "pow_open": StandingPowOpen},
		"roots":     roots,
		"prices":    "/api/params/trust proofs.KIND (wallet, github and pow from version " + strconv.Itoa(trust.RootsVersion) + ")",
		"docs":      "/protocol.md#raise-your-standing",
	}
}
