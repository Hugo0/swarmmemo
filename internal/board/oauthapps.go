package board

// Sign in with SwarmMemo for apps (S2): an app (apps.go) registers OAuth
// clients of its own, each with a fixed redirect allowlist, a scope ceiling
// and optionally resources it serves (RFC 8707, such as its MCP server).
// Signing in through one still IS a hosted identity (oauth.go): the same
// authorization page, codes, token families, refresh rotation and
// revocation; only the access token differs, a short-lived JWT with the
// granted scopes (oauthjwt.go), checked on every command through the
// scope allowlist (oauthscopes.go). The person lists and revokes the
// connections an identity has with oauth.connections.list and
// oauth.connection.revoke.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"slices"
	"strings"

	"swarmmemo/internal/services"
)

const (
	// OAuthAppsSection is the protocol section of the app client operations.
	OAuthAppsSection = "sign-in-with-swarmmemo"
	// OAuthAppClientPrefix starts a registered app client's client_id.
	OAuthAppClientPrefix = "smapp_"
	// OAuthAppClientsMax bounds an app's live clients.
	OAuthAppClientsMax = 8
	// OAuthAppResourcesMax bounds the resources one client serves.
	OAuthAppResourcesMax = 4
)

// oauthAppSchema is schema 50's: registered clients of apps. The signing
// keys and the used actor-token ids are oauthJWTSchema.
const oauthAppSchema = `
CREATE TABLE IF NOT EXISTS oauth_app_clients (
 client_id TEXT PRIMARY KEY, app TEXT NOT NULL, name TEXT NOT NULL, redirect_uris TEXT NOT NULL,
 scopes TEXT NOT NULL, resources TEXT NOT NULL DEFAULT '[]', dev_redirects INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, retired_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS oauth_app_clients_app ON oauth_app_clients(app,retired_at);
`

// OAuthAppClient is a registered client as its app and the sign-in see it.
type OAuthAppClient struct {
	ID           string   `json:"client_id"`
	App          string   `json:"app"`
	Name         string   `json:"name"`
	RedirectURIs []string `json:"redirect_uris"`
	Scopes       []string `json:"scopes"`
	Resources    []string `json:"resources"`
	DevRedirects bool     `json:"dev_redirects"`
	CreatedAt    int64    `json:"created_at"`
	UpdatedAt    int64    `json:"updated_at"`
	RetiredAt    int64    `json:"retired_at,omitempty"`
}

// oauthClientData is oauth.client.register's data.
type oauthClientData struct {
	Schema       int      `json:"schema"`
	Name         string   `json:"name"`
	RedirectURIs []string `json:"redirect_uris"`
	Scopes       []string `json:"scopes"`
	Resources    []string `json:"resources,omitempty"`
	DevRedirects bool     `json:"dev_redirects,omitempty"`
}

func invalidClientData(what string) error {
	return problem(400, "invalid_client_data", what+" See /protocol.md#sign-in-with-swarmmemo.")
}

func clientNotFound() error {
	return problem(404, "client_not_found", "No live client of your app has that client_id.")
}

// ValidRedirectURI is an absolute https URI, or http on a loopback host,
// with no fragment, credentials or control characters (MCP: redirect URIs
// are localhost or HTTPS). Dynamic and app clients both use it.
func ValidRedirectURI(raw string) bool {
	if raw == "" || len(raw) > OAuthRedirectURIBytes || strings.ContainsFunc(raw, func(r rune) bool { return r <= 0x20 || r == 0x7f || r == '\\' }) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || strings.Contains(raw, "#") || u.User != nil || u.Opaque != "" || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		return LoopbackHost(u.Hostname())
	}
	return false
}

// LoopbackHost is localhost or a loopback address.
func LoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// devRedirect is a redirect URI on this computer: allowed only while the
// client's dev_redirects is on.
func devRedirect(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && LoopbackHost(u.Hostname())
}

// LiveRedirectURIs is the client's redirect allowlist as the authorization
// page matches it: the development ones only while dev_redirects is on.
func (c OAuthAppClient) LiveRedirectURIs() []string {
	if c.DevRedirects {
		return slices.Clone(c.RedirectURIs)
	}
	return slices.DeleteFunc(slices.Clone(c.RedirectURIs), devRedirect)
}

// validResource is an https resource an app serves: its homepage's host or
// a subdomain of it, no query, fragment or credentials.
func validResource(raw, homepage string) bool {
	if len(raw) > OAuthRedirectURIBytes || !ValidRedirectURI(raw) {
		return false
	}
	u, _ := url.Parse(raw)
	h, err := url.Parse(homepage)
	if err != nil || u.Scheme != "https" || u.RawQuery != "" || u.ForceQuery {
		return false
	}
	host, home := strings.ToLower(u.Hostname()), strings.ToLower(h.Hostname())
	// A homepage on a shared suffix (an app registered before the rule)
	// is no registrable domain: it claims nothing.
	if sharedSuffixHost(home) {
		return false
	}
	return host == home || strings.HasSuffix(host, "."+home)
}

// onHost reports a URI on origin's host or a subdomain of it.
func onHost(raw, origin string) bool {
	u, err1 := url.Parse(raw)
	o, err2 := url.Parse(origin)
	if err1 != nil || err2 != nil || o.Hostname() == "" {
		return false
	}
	host, home := strings.ToLower(u.Hostname()), strings.ToLower(o.Hostname())
	return host == home || strings.HasSuffix(host, "."+home)
}

const appClientColumns = "client_id,app,name,redirect_uris,scopes,resources,dev_redirects,created_at,updated_at,retired_at"

func scanAppClient(row scanner) (OAuthAppClient, error) {
	var c OAuthAppClient
	var redirects, scopes, resources string
	err := row.Scan(&c.ID, &c.App, &c.Name, &redirects, &scopes, &resources, &c.DevRedirects, &c.CreatedAt, &c.UpdatedAt, &c.RetiredAt)
	if err != nil {
		return c, err
	}
	if json.Unmarshal([]byte(redirects), &c.RedirectURIs) != nil || json.Unmarshal([]byte(scopes), &c.Scopes) != nil || json.Unmarshal([]byte(resources), &c.Resources) != nil {
		return c, errors.New("oauth app client: stored lists are malformed")
	}
	return c, nil
}

// readAppClient is a registered client, retired or not; ok false if none.
func readAppClient(ctx context.Context, q queryer, id string) (OAuthAppClient, bool, error) {
	if !strings.HasPrefix(id, OAuthAppClientPrefix) || len(id) > 64 {
		return OAuthAppClient{}, false, nil
	}
	c, err := scanAppClient(q.QueryRowContext(ctx, "SELECT "+appClientColumns+" FROM oauth_app_clients WHERE client_id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

// OAuthAppClient is a live registered client and its app's record, for the
// authorization page. One short read, outside any transaction.
func (s *Store) OAuthAppClient(ctx context.Context, id string) (OAuthAppClient, AppRecord, bool, error) {
	c, ok, err := readAppClient(ctx, s.db, id)
	if err != nil || !ok || c.RetiredAt != 0 {
		return c, AppRecord{}, false, err
	}
	app, err := readApp(ctx, s.db, c.App)
	if err != nil {
		var be *Error
		if errors.As(err, &be) {
			return c, app, false, nil
		}
		return c, app, false, err
	}
	return c, app, true, nil
}

// OAuthResourceOwner is the app that serves an external resource (a live
// client of it lists the resource), its record and the scopes a sign-in for
// it may grant (that client's ceiling); ok false for none. One short read.
func (s *Store) OAuthResourceOwner(ctx context.Context, resource string) (AppRecord, []string, bool, error) {
	var app, scopes string
	err := s.db.QueryRowContext(ctx, "SELECT c.app,c.scopes FROM oauth_app_clients c, json_each(c.resources) r WHERE r.value=? AND c.retired_at=0 ORDER BY c.created_at LIMIT 1", resource).Scan(&app, &scopes)
	if errors.Is(err, sql.ErrNoRows) {
		return AppRecord{}, nil, false, nil
	}
	if err != nil {
		return AppRecord{}, nil, false, err
	}
	var ceiling []string
	if json.Unmarshal([]byte(scopes), &ceiling) != nil {
		return AppRecord{}, nil, false, errors.New("oauth app client: stored scopes are malformed")
	}
	rec, err := readApp(ctx, s.db, app)
	if err != nil {
		return rec, nil, false, nil
	}
	return rec, ceiling, true, nil
}

// resourceApp is the app a token's audience belongs to: SwarmMemo's own
// resource for an app, or a resource a live client of an app serves; "" for
// any other (an MCP profile).
func (s *Store) resourceApp(ctx context.Context, q queryer, resource string) (string, error) {
	if app, ok := strings.CutPrefix(resource, s.issuer()+"/app/"); ok && fingerprintRE.MatchString(app) {
		return app, nil
	}
	var app string
	err := q.QueryRowContext(ctx, "SELECT c.app FROM oauth_app_clients c, json_each(c.resources) r WHERE r.value=? AND c.retired_at=0 ORDER BY c.created_at LIMIT 1", resource).Scan(&app)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return app, err
}

// changeOAuthClient is oauth.client.register and oauth.client.retire.
func (s *Store) changeOAuthClient(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	var house string
	if err := tx.QueryRowContext(ctx, "SELECT app FROM identity_house WHERE account=?", a.account).Scan(&house); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}
	app, err := readApp(ctx, tx, a.account)
	if house != "" || err != nil {
		var be *Error
		if house != "" || errors.As(err, &be) && be.Code == "app_not_found" {
			return Result{}, problem(403, "app_required", "Only a registered app registers sign-in clients: register this identity with app.register first.")
		}
		return Result{}, err
	}
	var existing OAuthAppClient
	if c.Target != "" {
		var ok bool
		if existing, ok, err = readAppClient(ctx, tx, c.Target); err != nil {
			return Result{}, err
		}
		if !ok || existing.App != a.account || existing.RetiredAt != 0 {
			return Result{}, clientNotFound()
		}
	}
	if c.Operation == "oauth.client.retire" {
		if c.Target == "" {
			return Result{}, invalidClientData("oauth.client.retire names the client in target (its client_id).")
		}
		return s.retireOAuthClient(ctx, tx, a, existing, now)
	}
	var d oauthClientData
	if services.StrictObject([]byte(c.Data), &d) != nil || d.Schema != 1 {
		return Result{}, invalidClientData(`data must be {"schema":1,"name":NAME,"redirect_uris":[URI…],"scopes":[SCOPE…],"resources"?:[URI…],"dev_redirects"?:BOOL}.`)
	}
	name, ok := cleanAppName(d.Name)
	if !ok {
		return Result{}, invalidClientData("name is 1 to 64 characters on one line.")
	}
	if len(d.RedirectURIs) == 0 || len(d.RedirectURIs) > OAuthRedirectURIsMax {
		return Result{}, invalidClientData("redirect_uris lists 1 to 8 URIs.")
	}
	for i, u := range d.RedirectURIs {
		if !ValidRedirectURI(u) || slices.Contains(d.RedirectURIs[:i], u) {
			return Result{}, invalidClientData("Each redirect URI is https, or http on localhost for development, with no fragment or credentials, up to 512 bytes, each once.")
		}
		if devRedirect(u) && !d.DevRedirects {
			return Result{}, problem(400, "redirect_not_allowed", "A redirect URI on localhost works only while dev_redirects is true; send \"dev_redirects\":true for development, and register again without it for production.")
		}
	}
	scopes, ok := ParseOAuthScopes(strings.Join(d.Scopes, " "))
	if !ok || len(scopes) != len(d.Scopes) {
		return Result{}, invalidClientData("scopes lists app scopes, each once: " + strings.Join(OAuthScopeNames(), ", ") + ".")
	}
	if len(d.Resources) > OAuthAppResourcesMax {
		return Result{}, invalidClientData("resources lists at most 4 URIs.")
	}
	for i, r := range d.Resources {
		// Never one of SwarmMemo's own: its resources for apps
		// (OAuthAppResource) name their app in the path.
		if !validResource(r, app.Homepage) || onHost(r, s.issuer()) || slices.Contains(d.Resources[:i], r) {
			return Result{}, invalidClientData("Each resource is an https URI on your app's homepage host or a subdomain of it (never SwarmMemo's own), with no query, fragment or credentials, each once.")
		}
		var other string
		err := tx.QueryRowContext(ctx, "SELECT c.app FROM oauth_app_clients c, json_each(c.resources) r WHERE r.value=? AND c.retired_at=0 AND c.app<>? LIMIT 1", r, a.account).Scan(&other)
		if err == nil {
			return Result{}, problem(409, "resource_taken", "Another app's client already serves that resource; one resource belongs to one app.")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Result{}, err
		}
	}
	if d.Resources == nil {
		d.Resources = []string{}
	}
	if c.Target == "" {
		var live int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM oauth_app_clients WHERE app=? AND retired_at=0", a.account).Scan(&live); err != nil {
			return Result{}, err
		}
		if live >= OAuthAppClientsMax {
			return Result{}, problem(409, "client_limit", "An app has at most 8 live sign-in clients; retire one first (oauth.client.retire).")
		}
	}
	if err = s.charge(ctx, tx, a, SmallCommandCost+int64(len(c.Data)), now); err != nil {
		return Result{}, err
	}
	redirects, _ := json.Marshal(d.RedirectURIs)
	scopeList, _ := json.Marshal(scopes)
	resources, _ := json.Marshal(d.Resources)
	id := c.Target
	if id == "" {
		raw := make([]byte, 16)
		if _, err = rand.Read(raw); err != nil {
			return Result{}, err
		}
		id = OAuthAppClientPrefix + hex.EncodeToString(raw)
		_, err = tx.ExecContext(ctx, "INSERT INTO oauth_app_clients("+appClientColumns+") VALUES(?,?,?,?,?,?,?,?,?,0)",
			id, a.account, name, string(redirects), string(scopeList), string(resources), d.DevRedirects, now, now)
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE oauth_app_clients SET name=?,redirect_uris=?,scopes=?,resources=?,dev_redirects=?,updated_at=? WHERE client_id=?",
			name, string(redirects), string(scopeList), string(resources), d.DevRedirects, now, id)
		// A narrower ceiling applies to connections at their next refresh;
		// a connection whose scope is no longer within it ends now.
		if err == nil {
			err = s.endConnectionsOutside(ctx, tx, id, scopes, now)
		}
	}
	if err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, a.id, a.account, "client "+id, now); err != nil {
		return Result{}, err
	}
	client, _, err := readAppClient(ctx, tx, id)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"client": client}}, nil
}

// endConnectionsOutside ends the client's connections granted any scope
// outside scopes.
func (s *Store) endConnectionsOutside(ctx context.Context, tx *sql.Tx, clientID string, scopes []string, now int64) error {
	families, err := liveFamilies(ctx, tx, "client_id=?", clientID, now)
	if err != nil {
		return err
	}
	for _, f := range families {
		granted, ok := ParseOAuthScopes(f.Scope)
		if !ok || !scopeSubset(granted, scopes) {
			if err = revokeFamily(ctx, tx, f.ID, "client_narrowed", now); err != nil {
				return err
			}
		}
	}
	return nil
}

// retireOAuthClient is oauth.client.retire: the client stops signing
// anyone in, and every connection made through it ends. The row is kept.
func (s *Store) retireOAuthClient(ctx context.Context, tx *sql.Tx, a actor, client OAuthAppClient, now int64) (Result, error) {
	if _, err := tx.ExecContext(ctx, "UPDATE oauth_app_clients SET retired_at=?,updated_at=? WHERE client_id=?", now, now, client.ID); err != nil {
		return Result{}, err
	}
	families, err := liveFamilies(ctx, tx, "client_id=?", client.ID, now)
	if err != nil {
		return Result{}, err
	}
	for _, f := range families {
		if err = revokeFamily(ctx, tx, f.ID, "client_retired", now); err != nil {
			return Result{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE oauth_codes SET used_at=? WHERE client_id=? AND used_at=0", now, client.ID); err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, "oauth.client.retire", a.id, a.account, "client "+client.ID, now); err != nil {
		return Result{}, err
	}
	client, _, err = readAppClient(ctx, tx, client.ID)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"client": client, "connections_ended": len(families)}}, nil
}

// readOAuthClients is oauth.clients.list: the caller app's clients, live
// ones first.
func (s *Store) readOAuthClients(ctx context.Context, tx *sql.Tx, a actor) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+appClientColumns+" FROM oauth_app_clients WHERE app=? ORDER BY retired_at<>0,created_at DESC,client_id LIMIT 64", a.account)
	if err != nil {
		return Result{}, err
	}
	defer rows.Close()
	clients := []OAuthAppClient{}
	for rows.Next() {
		c, err := scanAppClient(rows)
		if err != nil {
			return Result{}, err
		}
		clients = append(clients, c)
	}
	if err = rows.Err(); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"clients": clients}}, nil
}

// OAuthConnection is one live sign-in of an identity in an app, as
// oauth.connections.list shows it (never a token).
type OAuthConnection struct {
	ID         string `json:"connection_id"`
	ClientID   string `json:"client_id"`
	ClientName string `json:"client_name"`
	App        string `json:"app,omitempty"`
	AppName    string `json:"app_name,omitempty"`
	Scope      string `json:"scope"`
	Resource   string `json:"resource"`
	CreatedAt  int64  `json:"created_at"`
	RenewedAt  int64  `json:"renewed_at,omitempty"`
	ExpiresAt  int64  `json:"expires_at"`
}

// liveFamilies are the live connections matching where (one ? argument).
func liveFamilies(ctx context.Context, tx *sql.Tx, where, arg string, now int64) ([]OAuthConnection, error) {
	rows, err := tx.QueryContext(ctx, `SELECT f.family_id,f.client_id,f.client_name,coalesce(c.app,''),coalesce(a.name,''),f.scope,f.resource,f.created_at,f.rotated_at,f.refresh_expires_at
 FROM oauth_families f LEFT JOIN oauth_app_clients c ON c.client_id=f.client_id LEFT JOIN apps a ON a.account=c.app
 WHERE f.`+where+` AND f.revoked_at=0 AND f.refresh_expires_at>? ORDER BY f.created_at DESC,f.family_id LIMIT 256`, arg, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OAuthConnection{}
	for rows.Next() {
		var c OAuthConnection
		if err = rows.Scan(&c.ID, &c.ClientID, &c.ClientName, &c.App, &c.AppName, &c.Scope, &c.Resource, &c.CreatedAt, &c.RenewedAt, &c.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// oauthConnections is oauth.connections.list and oauth.connection.revoke,
// signed by the identity itself.
func (s *Store) oauthConnections(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if c.Operation == "oauth.connection.revoke" {
		var account string
		var revoked int64
		err := tx.QueryRowContext(ctx, "SELECT account,revoked_at FROM oauth_families WHERE family_id=?", c.Target).Scan(&account, &revoked)
		if errors.Is(err, sql.ErrNoRows) || err == nil && (account != a.account || revoked != 0) {
			return Result{}, problem(404, "connection_not_found", "No live sign-in of yours has that connection_id (oauth.connections.list lists them).")
		}
		if err != nil {
			return Result{}, err
		}
		if err = revokeFamily(ctx, tx, c.Target, "revoked_by_owner", now); err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"connection_id": c.Target, "revoked": true}}, nil
	}
	list, err := liveFamilies(ctx, tx, "account=?", a.account, now)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"connections": list}}, nil
}

// oauthGrantApp is the app a grant's tokens are for: its registered
// client's, or the owner of its resource; "" for a dynamic client of an MCP
// profile.
func (s *Store) oauthGrantApp(ctx context.Context, tx *sql.Tx, clientID, resource string) (string, error) {
	if client, ok, err := readAppClient(ctx, tx, clientID); err != nil || ok {
		if ok && client.RetiredAt != 0 {
			return "", invalidGrant()
		}
		return client.App, err
	}
	return s.resourceApp(ctx, tx, resource)
}

// appAccessToken is a connection's access token when its scope is an app
// scope list: a JWT for the family's resource, made in tx.
func (s *Store) appAccessToken(ctx context.Context, tx *sql.Tx, account, family, clientID, resource, scope string, now int64) (string, error) {
	app, err := s.oauthGrantApp(ctx, tx, clientID, resource)
	if err != nil {
		return "", err
	}
	if app == "" {
		return "", invalidGrant()
	}
	idRaw := make([]byte, 16)
	if _, err = rand.Read(idRaw); err != nil {
		return "", err
	}
	return s.signJWT(ctx, tx, BearerClaims{Issuer: s.issuer(), Subject: account, Audience: resource, Expires: now + OAuthJWTAccessSeconds, IssuedAt: now,
		ID: hex.EncodeToString(idRaw), Scope: scope, ClientID: clientID, Session: family, App: app}, now)
}
