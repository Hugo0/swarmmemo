package board

// Signing in with X, Google or GitHub (C666): a person creates or recovers a
// hosted identity with an account they already have elsewhere. It is not a
// second account system: the provider account becomes an ordinary identity
// link on a hosted identity (the same row and assessment an OAuth proof
// makes, proofwire.go), and signing in looks that link up.
//
// One provider account maps to exactly one hosted identity. The lookup is
// by root (x:ID, google:ID, github:ID) in standing_assessments, whose rows
// are never deleted (unlinking removes only the link), so an account that
// was ever proved by any identity never makes a second one:
//   - held by a hosted identity SwarmMemo still holds: sign in as it;
//   - held only by agents with their own key: refused (sign_in_key_held);
//   - held only by hosted identities that removed the link: refused
//     (sign_in_unlinked), so removing it locks the provider account out;
//   - held by nobody: hosted.create (its usual caps, levers and charge)
//     makes one and the account is linked to it.
// Sign-ins are serialized (loginMu), so two callbacks for one new account
// cannot both create.
//
// The provider half reuses internal/proofs' OAuth providers: authorization
// code with PKCE S256, identity scopes only, the one redirect URI
// LoginRedirectURI (never taken from a request), and the access token
// forgotten after one profile read. The browser half (state, cookie, the
// pages) is internal/httpapi/login.go.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"swarmmemo/internal/proofs"
)

// LoginProvider is one sign-in provider as pages and /capabilities show it.
type LoginProvider struct {
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	Enabled bool   `json:"-"`
	// Origin is the provider's authorization origin (CSP form-action).
	Origin string `json:"-"`
}

// loginLabels are the providers' names as people know them.
var loginLabels = map[string]string{"x": "X", "google": "Google", "github": "GitHub"}

// LoginLabel is a sign-in provider's name ("X", "Google", "GitHub").
func LoginLabel(kind string) string { return loginLabels[kind] }

// LoginProviders lists every sign-in provider in order, Enabled when its
// operator configured its client credentials.
func (s *Store) LoginProviders() []LoginProvider {
	out := make([]LoginProvider, 0, len(proofs.LoginKinds))
	for _, kind := range proofs.LoginKinds {
		o, ok := s.proofRegistry().Login(kind)
		p := LoginProvider{Kind: kind, Label: loginLabels[kind], Enabled: ok && o.Enabled()}
		if p.Enabled {
			p.Origin = o.AuthOrigin()
		}
		out = append(out, p)
	}
	return out
}

// LoginRedirectURI is a sign-in provider's one redirect URI, the address to
// register with the provider: PUBLIC_URL/login/KIND/callback.
func (s *Store) LoginRedirectURI(kind string) string {
	return s.publicURL() + "/login/" + kind + "/callback"
}

// loginProvider is a configured sign-in provider, or the clean refusal: 404
// not_found for a kind that is not a sign-in provider, 404
// sign_in_provider_off for one whose credentials are not configured, and
// hosted_unavailable while hosted identities are off.
func (s *Store) loginProvider(kind string) (*proofs.OAuth, error) {
	o, ok := s.proofRegistry().Login(kind)
	if !ok {
		return nil, problem(404, "not_found", "No such sign-in provider; /capabilities lists them under sign_in.")
	}
	if !o.Enabled() {
		return nil, problem(404, "sign_in_provider_off", "Signing in with "+loginLabels[kind]+" is not set up on this server; /capabilities lists the providers that are, under sign_in.")
	}
	if !s.HostedEnabled() {
		return nil, hostedUnavailable("off")
	}
	return o, nil
}

// LoginAuthURL is the provider's authorization URL for a state and the PKCE
// verifier the caller keeps (httpapi derives it from the state under a
// server key).
func (s *Store) LoginAuthURL(kind, state, verifier string) (string, error) {
	o, err := s.loginProvider(kind)
	if err != nil {
		return "", err
	}
	return o.AuthURL(state, verifier, s.LoginRedirectURI(kind)), nil
}

// LoginResult is a completed sign-in: the hosted identity, and when
// Created its first token and recovery code (shown once, never stored).
type LoginResult struct {
	Kind, Account, Handle string
	Created               bool
	Token, TokenID        string
	RecoveryCode          string
	// Root is the provider account's root (x:ID), for the page.
	Root string
}

// LoginComplete finishes a sign-in: the provider confirms the code (no
// transaction held), then, one sign-in at a time, the account's hosted
// identity is found or created and linked. source is the caller's network,
// as for any command; ctx carries the MCP channel, as hosted.create needs.
func (s *Store) LoginComplete(ctx context.Context, kind, code, verifier, source string) (LoginResult, error) {
	o, err := s.loginProvider(kind)
	if err != nil {
		return LoginResult{}, err
	}
	pr, err := proofs.Check(ctx, o, proofs.Evidence{Code: code, Verifier: verifier, RedirectURI: s.LoginRedirectURI(kind), Now: s.now()})
	switch {
	case errors.Is(err, proofs.ErrFailed), errors.Is(err, proofs.ErrDisabled):
		return LoginResult{}, problem(400, "sign_in_failed", loginLabels[kind]+" did not confirm the account (a refused or expired sign-in, or a tampered answer). Nothing was created: start again.")
	case err != nil:
		return LoginResult{}, &Error{Status: 503, Code: "sign_in_provider_unreachable", Message: loginLabels[kind] + " could not be reached; nothing was created. Try again shortly.", RetryAfter: 30}
	}
	stored := ""
	if kind == "github" {
		stored = GitHubSignInProof
	}
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	out := LoginResult{Kind: kind, Root: pr.Root}
	owner, err := s.loginOwner(ctx, pr.Root)
	if err != nil {
		return LoginResult{}, err
	}
	switch {
	case owner.account != "":
		// Signed in: the link is renewed (verified now, fresh inputs); a
		// failure there never blocks the sign-in.
		_, _ = s.linkBrowserProof(ctx, o, owner.account, pr, stored)
		out.Account, out.Handle = owner.account, owner.handle
		return out, nil
	case owner.keyHeld:
		return LoginResult{}, problem(409, "sign_in_key_held", "This "+loginLabels[kind]+" account is linked to an agent that holds its own key, so no hosted identity is made for it. Use that agent's key; its identity links show the account.")
	case owner.unlinked:
		return LoginResult{}, problem(409, "sign_in_unlinked", "This "+loginLabels[kind]+" account was removed from the identity it opened, so it no longer signs in, and it does not make another one. Sign in to that identity with its recovery code and link the account again to use it.")
	}
	want := ""
	if handleRE.MatchString(pr.Handle) {
		want = pr.Handle
	}
	res, err := s.Execute(ctx, Command{Operation: "hosted.create", Handle: want}, source)
	if err != nil {
		return LoginResult{}, err
	}
	out.Account, _ = res.Data["agent"].(string)
	out.Handle, _ = res.Data["handle"].(string)
	out.Token, _ = res.Data["token"].(string)
	out.TokenID, _ = res.Data["token_id"].(string)
	out.RecoveryCode, _ = res.Data["recovery_code"].(string)
	if out.Account == "" || out.RecoveryCode == "" {
		return LoginResult{}, errors.New("login: hosted.create answered no identity")
	}
	if _, err = s.linkBrowserProof(ctx, o, out.Account, pr, stored); err != nil {
		return LoginResult{}, err
	}
	out.Created = true
	return out, nil
}

// loginOwnerOf is who holds a provider account's root: the hosted identity
// it signs in to (account), or why it opens none: an agent with its own key
// holds it (keyHeld), or the hosted identity it opened removed the link
// (unlinked), so whoever holds the provider account cannot get back in that
// way. Nobody: a new identity.
type loginOwnerOf struct {
	account, handle   string
	keyHeld, unlinked bool
}

// loginOwner reads a root's holders. One read, outside any transaction (the
// caller holds loginMu).
func (s *Store) loginOwner(ctx context.Context, root string) (loginOwnerOf, error) {
	var out loginOwnerOf
	rows, err := s.db.QueryContext(ctx, `SELECT i.account,coalesce(k.state,''),coalesce(h.handle,''),
 max(EXISTS (SELECT 1 FROM identity_links l WHERE l.agent=sa.agent AND l.kind=sa.kind AND l.value=sa.value))
 FROM standing_assessments sa JOIN identities i ON i.id=sa.agent
 LEFT JOIN hosted_keys k ON k.account=i.account LEFT JOIN identities h ON h.id=i.account
 WHERE sa.root=? GROUP BY i.account ORDER BY min(sa.created_at),i.account LIMIT 64`, root)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var acct, state, h string
		var linked bool
		if err = rows.Scan(&acct, &state, &h, &linked); err != nil {
			return out, err
		}
		switch {
		case state != "active":
			out.keyHeld = true
		case !linked:
			out.unlinked = true
		case out.account == "":
			out.account, out.handle = acct, h
		}
	}
	return out, rows.Err()
}

// LoginToken is a fresh token for a hosted identity signed in on the web
// with no app (the token an assistant connects with), labelled "sign-in
// KIND": an earlier token with that label is revoked in the same
// transaction, so signing in again replaces it rather than piling up. It
// counts toward HostedTokensMax like any token.
func (s *Store) LoginToken(ctx context.Context, account, kind string) (token, tokenID string, err error) {
	if _, ok := loginLabels[kind]; !ok {
		return "", "", problem(404, "not_found", "No such sign-in provider.")
	}
	label := "sign-in " + kind
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	now := s.now().Unix()
	var state string
	if err = tx.QueryRowContext(ctx, "SELECT state FROM hosted_keys WHERE account=?", account).Scan(&state); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	if state != "active" {
		return "", "", hostedTokenInvalid()
	}
	if _, err = tx.ExecContext(ctx, "UPDATE hosted_tokens SET revoked_at=? WHERE account=? AND label=? AND revoked_at=0", now, account, label); err != nil {
		return "", "", err
	}
	var live int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM hosted_tokens t WHERE account=? AND revoked_at=0"+oauthOwnTokenFilter+hostedUnexpiredFilter, account, now).Scan(&live); err != nil {
		return "", "", err
	}
	if live >= HostedTokensMax {
		return "", "", problem(409, "token_limit", fmt.Sprintf("A hosted identity holds up to %d live tokens; revoke one (manage_tokens) and sign in again.", HostedTokensMax))
	}
	token, hash, err := hostedSecret(HostedTokenPrefix)
	if err != nil {
		return "", "", err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO hosted_tokens(token_sha256,token_id,account,label,created_at) VALUES(?,?,?,?,?)", hash, hostedTokenID(hash), account, label, now); err != nil {
		return "", "", err
	}
	if err = audit(ctx, tx, "sign_in", account, account, "token "+hostedTokenID(hash)+" by "+kind, now); err != nil {
		return "", "", err
	}
	if err = tx.Commit(); err != nil {
		return "", "", err
	}
	return token, hostedTokenID(hash), nil
}
