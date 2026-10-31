package board

// The board's half of the proof providers (C326, internal/proofs): the link
// kinds they add, the rechecker's generic check, the OAuth and Telegram
// browser flows, and the bond's way.
//
// Every provider's check runs outside any transaction (the rechecker, or
// the OAuth callback between its two short transactions) and stores only
// what the provider returns: the link's value (the account's stable id, a
// name or a package), its root and its value inputs, in identity_links and
// standing_assessments. The trust run reads them as it reads a GitHub
// link's (trustwire.go): verified, checked within proof_fresh_days, priced
// by the kind's row.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"

	"swarmmemo/internal/b64"
	"swarmmemo/internal/proofs"
	"swarmmemo/internal/services"
	"swarmmemo/internal/trust"
)

// browserKinds are the proof kinds only a browser flow links: identity.link
// cannot claim them.
var browserKinds = []string{"google", "x", "discord", "telegram", "reddit", "huggingface"}

// accountIDRE is a provider's stable account id.
var accountIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func init() {
	method := func(kind string) string {
		p, _ := proofs.Default.Get(kind)
		return p.Spec().Method
	}
	for _, kind := range []string{"farcaster", "npm", "pypi"} {
		normalize := proofs.NormalizeFID
		switch kind {
		case "npm":
			normalize = proofs.NormalizeNPM
		case "pypi":
			normalize = proofs.NormalizePyPI
		}
		linkKinds[kind] = linkKind{normalize: normalize, attach: claimOnly, challenge: challengeStatement, check: proofChecker(kind), method: method(kind)}
	}
	linkKinds["ens"] = linkKind{normalize: proofs.NormalizeENS, attach: claimOnly, check: proofChecker("ens"), method: method("ens")}
	for _, kind := range browserKinds {
		linkKinds[kind] = linkKind{normalize: func(v string) (string, bool) { return v, accountIDRE.MatchString(v) }, attach: browserOnly,
			check: proofChecker(kind), method: method(kind)}
	}
}

// browserOnly refuses identity.link of a kind the browser flow links.
func browserOnly(_ *Store, _, _, _ string) (string, error) {
	return "", problem(400, "proof_browser_only", `This kind is linked by signing in with the provider: standing.challenge {"schema":1,"kind":KIND} answers auth_url to open in a browser.`)
}

// challengeStatement is identity.link of a statement kind (farcaster, npm,
// pypi), in the command: the key's unused challenge for this value. The
// link stays claimed until the checker finds the statement (a cast, a
// custody signature or package metadata); the statement and the proof are
// kept with it so anyone can check them again.
func challengeStatement(ctx context.Context, tx *sql.Tx, s *Store, a actor, d linkData, value string, now int64) (string, string, error) {
	if d.Nonce == "" {
		return "", "", linkError("invalid_link_proof")
	}
	switch d.Kind {
	case "farcaster":
		if d.Proof != "cast" && !walletSignatureRE.MatchString(d.Proof) {
			return "", "", linkError("invalid_link_proof")
		}
	default:
		if d.Proof != "" {
			return "", "", linkError("invalid_link_proof")
		}
	}
	if _, _, _, err := useStandingChallenge(ctx, tx, a.id, d.Kind, value, d.Nonce, now); err != nil {
		return "", "", err
	}
	return "claimed", proofs.Statement(d.Kind, s.config.ServiceID, a.id, value, d.Nonce), nil
}

// walletSignatureRE is a personal_sign signature.
var walletSignatureRE = regexp.MustCompile(`^0x[0-9a-fA-F]{130}$`)

// proofDetail is a provider proof's stored assessment detail: its value
// inputs and when it was verified. Nothing else (no token, name or email).
type proofDetail struct {
	Source     string        `json:"source"`
	Inputs     proofs.Inputs `json:"inputs"`
	VerifiedAt int64         `json:"verified_at"`
}

// proofChecker is a provider kind's live check for the rechecker: Verify
// the first time (a claimed link), Recheck once the key has a stored proof.
// A pass stores the fresh inputs and assessment; a definite refusal fails
// the link; anything else is inconclusive.
func proofChecker(kind string) linkChecker {
	return func(s *Store, ctx context.Context, fingerprint, value, stored string) linkOutcome {
		prov, ok := s.proofRegistry().Get(kind)
		if !ok {
			return linkFailed
		}
		e := proofs.Evidence{Fingerprint: fingerprint, Value: value, Now: s.now()}
		if r, ok := decodeLinkRecord(stored); ok {
			e.Statement, e.Proof = r.Message, r.Proof
		}
		if kind == "ens" {
			wallets, err := s.verifiedWallets(ctx, fingerprint)
			if err != nil {
				return linkUnreachable
			}
			e.Wallets = wallets
		}
		prior, found := s.storedProof(ctx, fingerprint, kind, value)
		var pr proofs.Proof
		var err error
		if found {
			pr, err = prov.Recheck(ctx, prior, e)
		} else {
			pr, err = proofs.Check(ctx, prov, e)
		}
		switch {
		case errors.Is(err, proofs.ErrFailed):
			return linkFailed
		case err != nil:
			return linkUnreachable
		}
		now := s.now()
		_ = s.saveAssessment(ctx, fingerprint, kind, value, pr.Root, prov.Assess(pr, now), proofDetail{Source: kind, Inputs: pr.Inputs, VerifiedAt: now.Unix()}, now.Unix())
		return linkPassed
	}
}

// verifiedWallets are a key's verified wallet links, read with no
// transaction held.
func (s *Store) verifiedWallets(ctx context.Context, agent string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT value FROM identity_links WHERE agent=? AND kind='wallet' AND state='verified' LIMIT 8", agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// storedProof is the key's stored proof of a provider kind, when it has one.
func (s *Store) storedProof(ctx context.Context, agent, kind, value string) (proofs.Proof, bool) {
	var root, detail string
	if s.db.QueryRowContext(ctx, "SELECT root,detail FROM standing_assessments WHERE agent=? AND kind=? AND value=?", agent, kind, value).Scan(&root, &detail) != nil || root == "" {
		return proofs.Proof{}, false
	}
	var d proofDetail
	if json.Unmarshal([]byte(detail), &d) != nil {
		return proofs.Proof{}, false
	}
	if d.Inputs == nil {
		d.Inputs = proofs.Inputs{}
	}
	return proofs.Proof{Kind: kind, Value: value, Root: root, Inputs: d.Inputs, VerifiedAt: d.VerifiedAt}, true
}

// recheckAfter is how long a passing link of kind waits for its next check:
// the provider's interval (GitHub's small unauthenticated budget: weekly),
// else the rechecker's daily default.
func (s *Store) recheckAfter(kind string) int64 {
	if p, ok := s.proofRegistry().Get(kind); ok && p.Spec().RecheckSeconds > 0 {
		return p.Spec().RecheckSeconds
	}
	return 0
}

// ---- the OAuth and Telegram browser flow -----------------------------------

// publicURL is the service's public origin.
func (s *Store) publicURL() string {
	if s.config.PublicURL != "" {
		return strings.TrimRight(s.config.PublicURL, "/")
	}
	return "https://" + s.config.ServiceID
}

// proofStartURL is the start page a challenge's auth_url names.
func (s *Store) proofStartURL(kind, state string) string {
	return s.publicURL() + "/proofs/oauth/" + kind + "/start?state=" + url.QueryEscape(state)
}

// ProofRedirectURI is the one redirect URI of a kind: the URL to register
// with the provider. It is never taken from a request.
func (s *Store) ProofRedirectURI(kind string) string {
	return s.publicURL() + "/proofs/oauth/" + kind + "/callback"
}

// openProofs makes the PKCE key once: verifiers are derived from it and the
// state, so none is stored and an intercepted code is useless without it.
func (s *Store) openProofs() error {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	if err := s.ensureMeta("proof_pkce_key", base64.RawURLEncoding.EncodeToString(secret)); err != nil {
		return err
	}
	var encoded string
	if err := s.db.QueryRow("SELECT value FROM meta WHERE key='proof_pkce_key'").Scan(&encoded); err != nil {
		return err
	}
	key, err := b64.URL(encoded)
	if err != nil || len(key) != 32 {
		return errors.New("proofs: the stored PKCE key is malformed")
	}
	s.proofKey = key
	return s.openBonds()
}

// ProofStartView is the start page: which agent the account will be linked
// to, and where to go next.
type ProofStartView struct {
	Kind, Label, Agent, Handle string
	// AuthURL is the provider's authorization URL (OAuth); Bot and
	// CallbackURL the Telegram widget's.
	AuthURL     string
	Bot         string
	CallbackURL string
	ExpiresAt   int64
}

// ProofLinked is a completed browser flow.
type ProofLinked struct {
	Kind, Label, Agent, Value, Root string
	AddsCents                       int64
}

func browserProvider(s *Store, kind string) (proofs.Provider, error) {
	prov, ok := s.proofRegistry().Get(kind)
	if !ok || (prov.Spec().Flow != proofs.FlowOAuth && prov.Spec().Flow != proofs.FlowWidget) {
		return nil, problem(404, "not_found", "No such proof kind.")
	}
	if !prov.Enabled() {
		return nil, standingError("proof_disabled")
	}
	return prov, nil
}

// ProofStart reads a browser flow's state without spending it: open, of
// this kind, unexpired.
func (s *Store) ProofStart(ctx context.Context, kind, state string) (ProofStartView, error) {
	prov, err := browserProvider(s, kind)
	if err != nil {
		return ProofStartView{}, err
	}
	if !standingNonceR.MatchString(state) {
		return ProofStartView{}, standingError("invalid_challenge")
	}
	var agent, gotKind, handle string
	var expires, used int64
	err = s.db.QueryRowContext(ctx, `SELECT c.agent,c.kind,c.expires_at,c.used_at,coalesce(i.handle,'') FROM standing_challenges c JOIN identities i ON i.id=c.agent
 WHERE c.nonce_sha256=? AND i.successor='' AND i.revoked_at=0`, sha256Hex([]byte(state))).Scan(&agent, &gotKind, &expires, &used, &handle)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && gotKind != kind:
		return ProofStartView{}, standingError("invalid_challenge")
	case err != nil:
		return ProofStartView{}, err
	case used > 0:
		return ProofStartView{}, standingError("challenge_used")
	case s.now().Unix() >= expires:
		return ProofStartView{}, standingError("challenge_expired")
	}
	v := ProofStartView{Kind: kind, Label: prov.Spec().Label, Agent: agent, Handle: handle, ExpiresAt: expires}
	switch p := prov.(type) {
	case *proofs.OAuth:
		v.AuthURL = p.AuthURL(state, proofs.PKCEVerifier(s.proofKey, state), s.ProofRedirectURI(kind))
	case *proofs.Telegram:
		v.Bot, v.CallbackURL = p.Bot(), s.ProofRedirectURI(kind)+"?state="+url.QueryEscape(state)
	}
	return v, nil
}

// ProofCallback completes a browser flow: it spends the state in one short
// transaction, has the provider verify the code (or the widget's fields)
// with no transaction held, then links the account in another.
func (s *Store) ProofCallback(ctx context.Context, kind, state, code string, fields url.Values) (ProofLinked, error) {
	prov, err := browserProvider(s, kind)
	if err != nil {
		return ProofLinked{}, err
	}
	if !standingNonceR.MatchString(state) {
		return ProofLinked{}, standingError("invalid_challenge")
	}
	agent, err := s.spendProofState(ctx, kind, state)
	if err != nil {
		return ProofLinked{}, err
	}
	pr, err := proofs.Check(ctx, prov, proofs.Evidence{Fingerprint: agent, Code: code, Verifier: proofs.PKCEVerifier(s.proofKey, state),
		RedirectURI: s.ProofRedirectURI(kind), Fields: fields, Now: s.now()})
	switch {
	case errors.Is(err, proofs.ErrFailed):
		return ProofLinked{}, problem(400, "proof_failed", "The provider did not confirm the account (a refused or expired sign-in, or a tampered answer). Nothing was linked: ask standing.challenge for a new auth_url.")
	case err != nil:
		return ProofLinked{}, &Error{Status: 503, Code: "proof_unavailable", Message: "The provider could not be reached; nothing was linked. Ask standing.challenge for a new auth_url and try again shortly.", RetryAfter: 30}
	}
	return s.linkBrowserProof(ctx, prov, agent, pr, "")
}

// spendProofState marks a browser flow's state used: once, by its kind,
// unexpired, for a current key.
func (s *Store) spendProofState(ctx context.Context, kind, state string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	now := s.now().Unix()
	var agent, gotKind string
	var expires, used int64
	err = tx.QueryRowContext(ctx, `SELECT c.agent,c.kind,c.expires_at,c.used_at FROM standing_challenges c JOIN identities i ON i.id=c.agent
 WHERE c.nonce_sha256=? AND i.successor='' AND i.revoked_at=0`, sha256Hex([]byte(state))).Scan(&agent, &gotKind, &expires, &used)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && gotKind != kind:
		return "", standingError("invalid_challenge")
	case err != nil:
		return "", err
	case used > 0:
		return "", standingError("challenge_used")
	case now >= expires:
		return "", standingError("challenge_expired")
	}
	res, err := tx.ExecContext(ctx, "UPDATE standing_challenges SET used_at=? WHERE nonce_sha256=? AND used_at=0", now, sha256Hex([]byte(state)))
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", standingError("challenge_used")
	}
	return agent, tx.Commit()
}

// linkBrowserProof records a verified browser proof: the link (verified now,
// rechecked on schedule) and its assessment, in one transaction. stored is
// the link's proof column ("" for a provider that rechecks itself;
// GitHubSignInProof for a GitHub sign-in).
func (s *Store) linkBrowserProof(ctx context.Context, prov proofs.Provider, agent string, pr proofs.Proof, stored string) (ProofLinked, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProofLinked{}, err
	}
	defer tx.Rollback()
	now := s.now().Unix()
	kind := prov.Spec().Kind
	var account string
	if err = tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=? AND successor='' AND revoked_at=0", agent).Scan(&account); err != nil {
		return ProofLinked{}, standingError("invalid_challenge")
	}
	var found int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM identity_links WHERE agent=? AND kind=? AND value=?", agent, kind, pr.Value).Scan(&found); err != nil {
		return ProofLinked{}, err
	}
	if found == 0 {
		var held int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM identity_links WHERE agent=? AND NOT "+retiredSealKey, agent).Scan(&held); err != nil {
			return ProofLinked{}, err
		}
		if held >= IdentityLinkMaxPerKey {
			return ProofLinked{}, linkError("link_limit")
		}
	}
	next := now + linkRecheckEvery
	if _, err = tx.ExecContext(ctx, `INSERT INTO identity_links(agent,kind,value,proof,state,created_at,checked_at,next_check_at) VALUES(?,?,?,?,'verified',?,?,?)
 ON CONFLICT(agent,kind,value) DO UPDATE SET proof=CASE WHEN identity_links.state='verified' AND identity_links.proof<>'' THEN identity_links.proof ELSE excluded.proof END,state='verified',checked_at=excluded.checked_at,lapsed_at=0,failures=0,last_error='',next_check_at=excluded.next_check_at`,
		agent, kind, pr.Value, stored, now, now, next); err != nil {
		return ProofLinked{}, err
	}
	assessed := prov.Assess(pr, s.now())
	raw, _ := json.Marshal(proofDetail{Source: kind, Inputs: pr.Inputs, VerifiedAt: now})
	if _, err = tx.ExecContext(ctx, `INSERT INTO standing_assessments(agent,kind,value,root,assessed,detail,created_at,assessed_at) VALUES(?,?,?,?,?,?,?,?)
 ON CONFLICT(agent,kind,value) DO UPDATE SET root=excluded.root,assessed=excluded.assessed,detail=excluded.detail,assessed_at=excluded.assessed_at`,
		agent, kind, pr.Value, pr.Root, max(0, assessed), string(raw), now, now); err != nil {
		return ProofLinked{}, err
	}
	if err = audit(ctx, tx, "identity.link", agent, kind, "identity link verified: "+kind+" "+pr.Value, now); err != nil {
		return ProofLinked{}, err
	}
	if err = s.onAccountChange(ctx, tx, accountChange{Account: account, Reason: "identity.link", Kind: kind}, now); err != nil {
		return ProofLinked{}, err
	}
	if err = tx.Commit(); err != nil {
		return ProofLinked{}, err
	}
	out := ProofLinked{Kind: kind, Label: prov.Spec().Label, Agent: agent, Value: pr.Value, Root: pr.Root}
	if p, err := s.trustParamsAt(ctx, s.db, now); err == nil {
		out.AddsCents = proofs.Price(&p, prov, pr, s.now())
	}
	return out, nil
}

// bondWay fills standing.ways' bond row from the account's latest bond.
func (s *Store) bondWay(ctx context.Context, q *sql.Tx, w *StandingWay, account string, counted map[string]int64, now int64) error {
	if !s.BondEnabled() {
		w.Status = "disabled-no-credentials"
	}
	if w.AddsCents != nil {
		// A bond adds at least the smallest deposit taken.
		w.AddsCents.Min = min(s.bondConfig().MinUnits/10_000, w.AddsCents.Max)
	}
	var id string
	err := q.QueryRowContext(ctx, "SELECT id FROM proof_bonds WHERE account=? AND state IN ('held','refund_owed','refunded','burned') ORDER BY created_at DESC LIMIT 1", account).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	b, err := loadBond(ctx, q, id)
	if err != nil {
		return err
	}
	w.Value, w.Root = services.FormatUSDC(b.Amount)+" USDC", "bond:"+b.ID
	switch b.State {
	case "held":
		w.State = "verified"
		if burned, err := bondBurned(ctx, q, w.Root); err != nil {
			return err
		} else if burned {
			w.State = "burned"
		}
		if w.State == "verified" {
			if p, err := s.trustParamsAt(ctx, q, now); err == nil {
				v := trust.AssessedContribution(p.Proofs["bond"], b.Cents())
				w.AssessedCents = &v
			}
		}
	case "refund_owed":
		w.State = "refund_owed"
		if burned, err := bondBurned(ctx, q, w.Root); err != nil {
			return err
		} else if burned {
			w.State = "burned" // burned after the refund was asked for: not refunded
		}
	default:
		w.State = b.State
	}
	if c, ok := counted[w.Root]; ok {
		w.CountedCents = &c
	}
	return nil
}
