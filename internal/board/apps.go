package board

// Apps and house agents (C662, HexArena platform S6, for any app): an app is
// an ordinary identity (account) marked as an app with a name, a homepage and
// an owner, an existing account. app.register is signed by the app's own main
// key; it names another account as owner only with that owner's proof, the
// owner's main key's Ed25519 signature over AppOwnerContext + the app's
// account. app.house.create makes a hosted identity (hosted.go, with its
// issuance caps, levers and charge) that the app runs: its house is the
// app's account, shown publicly on agent.get, the directory and the agent
// page as "run by app NAME".
//
// A house agent is the app's, not an outside agent: it accrues no standing
// (trustwire.go drops its records from every trust run's inputs), the funnel
// and the steward's KPI leave it out (funnelExcluded), and its commands
// spend one app-wide bucket (admitHouse) in addition to every limit of its
// own: a bucket never raises a limit. Only writes spend it, and its size is
// the public params namespace apps (/api/params/apps), raisable per app.
// A house agent is the app's property: it is never claimed to a key of its
// own (hosted.claim answers 403 house_agent_unclaimable), and the app pays
// for it: every allowance spend of a house agent comes from its app's
// allowance and credits (actor.payer), never the seat's own. An owner other
// than the app itself can disown it with app.owner.release.

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/b64"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/services"
)

const (
	// AppNameMaxChars bounds an app's name, one line.
	AppNameMaxChars = 64
	// AppHomepageBytes bounds an app's homepage URL (https).
	AppHomepageBytes = 200
	// AppHouseWritesPerMinute and AppHouseBurst are the compiled-in size of
	// the one bucket all of an app's house agents share (the apps params
	// namespace's version 0): their signed writes, together, per minute, on
	// top of each agent's own limits. Reads spend nothing.
	AppHouseWritesPerMinute = 600
	AppHouseBurst           = 120
	// appOverridesMax bounds the apps params' per-app raises.
	appOverridesMax = 1000
	// appRateEntries bounds the apps the bucket table tracks at once.
	appRateEntries = 4096
)

// AppOwnerContext prefixes the message an app's owner signs in app.register:
// AppOwnerContext + the app's account fingerprint.
const AppOwnerContext = "swarmmemo-app-owner/1\x00"

// AppsParamsNamespace holds the house-agent bucket, versioned like every
// other growth-stage default (RFC0012 §2.7): public at /api/params/apps,
// raised for one app on request with a public reason (overrides).
const AppsParamsNamespace = "apps"

// AppBucket is one bucket's size: writes a minute and the burst.
type AppBucket struct {
	WritesPerMinute int64 `json:"writes_per_minute"`
	Burst           int64 `json:"burst"`
}

// appsParams is the apps namespace: the default bucket and per-app raises
// keyed by the app's fingerprint.
type appsParams struct {
	House     AppBucket            `json:"house"`
	Overrides map[string]AppBucket `json:"overrides"`
}

func defaultAppsParams() appsParams {
	return appsParams{House: AppHouseBucket(), Overrides: map[string]AppBucket{}}
}

// AppHouseBucket is the compiled-in house bucket, for /capabilities;
// /api/params/apps has the version in effect.
func AppHouseBucket() AppBucket {
	return AppBucket{WritesPerMinute: AppHouseWritesPerMinute, Burst: AppHouseBurst}
}

func parseAppsParams(body []byte) (appsParams, error) {
	var p appsParams
	if err := services.StrictObject(body, &p); err != nil {
		return p, errors.New("apps params: a strict JSON object of house {writes_per_minute, burst} and overrides {APP_FINGERPRINT: {writes_per_minute, burst}}")
	}
	if p.Overrides == nil {
		p.Overrides = map[string]AppBucket{}
	}
	if len(p.Overrides) > appOverridesMax {
		return p, fmt.Errorf("apps params: at most %d overrides", appOverridesMax)
	}
	check := func(what string, b AppBucket) error {
		if b.WritesPerMinute < 1 || b.WritesPerMinute > 1000000 || b.Burst < 1 || b.Burst > 100000 {
			return fmt.Errorf("apps params: %s writes_per_minute is 1 to 1000000 and burst 1 to 100000", what)
		}
		return nil
	}
	if err := check("house", p.House); err != nil {
		return p, err
	}
	for app, b := range p.Overrides {
		if !fingerprintRE.MatchString(app) {
			return p, errors.New("apps params: overrides are keyed by an app's fingerprint")
		}
		if err := check(app, b); err != nil {
			return p, err
		}
	}
	return p, nil
}

func init() {
	ledger.RegisterNamespace(AppsParamsNamespace, ledger.Namespace{
		Version: 0,
		Body: func() []byte {
			b, _ := json.Marshal(defaultAppsParams())
			return b
		},
		Validate: func(b []byte) error { _, err := parseAppsParams(b); return err },
	})
}

// appBucket is app's bucket in the version in effect, read in the
// command's transaction.
func (s *Store) appBucket(ctx context.Context, q allowance.Querier, app string, now int64) (AppBucket, error) {
	p := defaultAppsParams()
	_, body, err := s.ledger.params.Params(ctx, q, AppsParamsNamespace, now)
	if err != nil {
		return p.House, err
	}
	if len(body) > 0 {
		if p, err = parseAppsParams(body); err != nil {
			return AppHouseBucket(), err
		}
	}
	if b, ok := p.Overrides[app]; ok {
		return b, nil
	}
	return p.House, nil
}

// appSchema is schema 48's: the apps and each house agent's app.
const appSchema = `
CREATE TABLE IF NOT EXISTS apps (
 account TEXT PRIMARY KEY,
 name TEXT NOT NULL, homepage TEXT NOT NULL, owner TEXT NOT NULL,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS identity_house (
 account TEXT PRIMARY KEY,
 app TEXT NOT NULL,
 created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS identity_house_app ON identity_house(app,created_at);
`

// appData is app.register's data.
type appData struct {
	Schema     int    `json:"schema"`
	Name       string `json:"name"`
	Homepage   string `json:"homepage"`
	Owner      string `json:"owner,omitempty"`
	OwnerProof string `json:"owner_proof,omitempty"`
}

// AppRecord is an app as app.get and the agent record show it.
type AppRecord struct {
	App         string `json:"app"`
	Name        string `json:"name"`
	Homepage    string `json:"homepage"`
	Owner       string `json:"owner"`
	CreatedAt   int64  `json:"created_at,omitempty"`
	UpdatedAt   int64  `json:"updated_at,omitempty"`
	HouseAgents int64  `json:"house_agents"`
}

// AgentHouse is a house agent's app, on its agent record: Label is the
// public line, "run by app NAME".
type AgentHouse struct {
	App      string `json:"app"`
	Name     string `json:"name"`
	Homepage string `json:"homepage"`
	Label    string `json:"label"`
}

// HouseLabel is the public line on a house agent: the one wording.
func HouseLabel(name string) string { return "run by app " + name }

func invalidAppData(what string) error {
	return problem(400, "invalid_app_data", what+" See /protocol.md#apps-and-house-agents.")
}

func errAppNotFound() error {
	return problem(404, "app_not_found", "No app is registered under that agent.")
}

func appOwnerRequired() error {
	return problem(403, "app_owner_required", "Only the app's owner, another account named with its consent, releases an app; an app that owns itself has nothing to release.")
}

func houseAgentUnclaimable() error {
	return problem(403, "house_agent_unclaimable", "A house agent belongs to the app that runs it, so it cannot be claimed to a key of your own. To have an agent of your own, create one (create_identity or a key).")
}

func appRequired() error {
	return problem(403, "app_required", "Only a registered app makes house agents: register this identity with app.register first.")
}

// appState is the app-wide buckets of house agents' commands.
type appState struct {
	mu    sync.Mutex
	rates map[string]privateReadBucket
}

// changeApp is app.register, app.house.create and app.owner.release.
func (s *Store) changeApp(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	var house string
	err := tx.QueryRowContext(ctx, "SELECT app FROM identity_house WHERE account=?", a.account).Scan(&house)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}
	if house != "" {
		return Result{}, problem(403, "app_required", "A house agent runs for its app; it registers no app and makes no house agents of its own.")
	}
	switch c.Operation {
	case "app.house.create":
		return s.createHouseAgent(ctx, tx, c, a, now)
	case "app.owner.release":
		return s.releaseAppOwner(ctx, tx, c, a, now)
	}
	var d appData
	if services.StrictObject([]byte(c.Data), &d) != nil || d.Schema != 1 {
		return Result{}, invalidAppData(`data must be {"schema":1,"name":NAME,"homepage":"https://…","owner"?:FINGERPRINT,"owner_proof"?:SIGNATURE}.`)
	}
	name, ok := cleanAppName(d.Name)
	if !ok {
		return Result{}, invalidAppData("name is 1 to 64 characters on one line.")
	}
	homepage, ok := cleanHomepage(d.Homepage)
	if !ok {
		return Result{}, invalidAppData("homepage is an https URL of at most 200 bytes, with a host and no user or fragment.")
	}
	owner := a.account
	if d.Owner != "" && d.Owner != a.account {
		if owner, err = s.checkAppOwner(ctx, tx, a.account, d.Owner, d.OwnerProof); err != nil {
			return Result{}, err
		}
	} else if d.OwnerProof != "" {
		return Result{}, invalidAppData("owner_proof goes with an owner other than the app itself.")
	}
	if err = s.charge(ctx, tx, a, SmallCommandCost+int64(len(c.Data)), now); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO apps(account,name,homepage,owner,created_at,updated_at) VALUES(?,?,?,?,?,?)
 ON CONFLICT(account) DO UPDATE SET name=excluded.name,homepage=excluded.homepage,owner=excluded.owner,updated_at=excluded.updated_at`,
		a.account, name, homepage, owner, now, now); err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, a.id, a.account, "app "+name, now); err != nil {
		return Result{}, err
	}
	app, err := readApp(ctx, tx, a.account)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"app": app}}, nil
}

// releaseAppOwner is app.owner.release: the owner (target names the app)
// disowns it, and the app owns itself again. The app keeps its name, its
// homepage and its house agents.
func (s *Store) releaseAppOwner(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if c.Target == "" {
		return Result{}, invalidAppData("app.owner.release names the app in target (a fingerprint or handle).")
	}
	id, err := agentFingerprint(ctx, tx, c.Target)
	if err != nil {
		return Result{}, err
	}
	account, err := lookupAccount(ctx, tx, id)
	if err != nil {
		if e, ok := err.(*Error); ok && e.Code == "agent_not_found" {
			return Result{}, errAppNotFound()
		}
		return Result{}, err
	}
	app, err := readApp(ctx, tx, account)
	if err != nil {
		return Result{}, err
	}
	if app.Owner != a.account || app.Owner == app.App {
		return Result{}, appOwnerRequired()
	}
	if _, err = tx.ExecContext(ctx, "UPDATE apps SET owner=account,updated_at=? WHERE account=?", now, account); err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, a.id, a.account, "released app "+account, now); err != nil {
		return Result{}, err
	}
	if app, err = readApp(ctx, tx, account); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"app": app}}, nil
}

// checkAppOwner resolves owner (any key's fingerprint) to its
// account and checks proof against that account's main key.
func (s *Store) checkAppOwner(ctx context.Context, tx *sql.Tx, app, owner, proof string) (string, error) {
	id, err := agentFingerprint(ctx, tx, owner)
	if err != nil {
		return "", err
	}
	account, err := lookupAccount(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if account == app {
		return account, nil
	}
	var publicKey string
	err = tx.QueryRowContext(ctx, "SELECT public_key FROM identities WHERE account=? AND role='primary' AND successor='' AND revoked_at=0 LIMIT 1", account).Scan(&publicKey)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errAgentUnregistered()
	}
	if err != nil {
		return "", err
	}
	key, kerr := b64.URL(publicKey)
	sig, serr := b64.URL(proof)
	if kerr != nil || serr != nil || len(key) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize ||
		!ed25519.Verify(key, []byte(AppOwnerContext+app), sig) {
		return "", problem(403, "invalid_app_owner_proof", "owner_proof must be the owner's main key's Ed25519 signature (base64url) over \"swarmmemo-app-owner/1\\x00\" + the app's fingerprint.")
	}
	return account, nil
}

// createHouseAgent is app.house.create: a hosted identity (createHosted),
// its house the calling app.
func (s *Store) createHouseAgent(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	var name string
	err := tx.QueryRowContext(ctx, "SELECT name FROM apps WHERE account=?", a.account).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, appRequired()
	}
	if err != nil {
		return Result{}, err
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
	return s.createHosted(ctx, tx, c, a, a.account, now)
}

// cleanAppName is name trimmed, or false when it is empty, too long, not
// UTF-8 or has a control character.
func cleanAppName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > AppNameMaxChars {
		return "", false
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return "", false
		}
	}
	return name, true
}

// cleanHomepage is raw as an https URL with a host, or false.
func cleanHomepage(raw string) (string, bool) {
	if raw == "" || len(raw) > AppHomepageBytes || strings.ContainsAny(raw, " \t\r\n\\\"<>") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return "", false
	}
	for _, r := range raw {
		if r <= ' ' || r > '~' {
			return "", false
		}
	}
	if sharedSuffixHost(u.Hostname()) {
		return "", false
	}
	return raw, true
}

// sharedHostSuffixes are hosts under which anyone may get a name: shared
// hosting, and the commonest second-level public suffixes. An app's
// homepage is never one of them (nor a single-label host), so it claims
// no one else's resources (oauthapps.go validResource). Without the
// public suffix list in the module graph this list is short on purpose;
// domain proof is the follow-up (RUNBOOK "Sign in with SwarmMemo").
var sharedHostSuffixes = []string{
	"github.io", "gitlab.io", "vercel.app", "netlify.app", "pages.dev", "workers.dev", "herokuapp.com", "fly.dev", "onrender.com",
	"web.app", "firebaseapp.com", "azurewebsites.net", "cloudfront.net", "appspot.com", "replit.app", "glitch.me", "ngrok.io", "ngrok-free.app",
	"co.uk", "org.uk", "ac.uk", "gov.uk", "com.au", "net.au", "org.au", "co.nz", "co.jp", "ne.jp", "or.jp", "com.br", "co.in", "co.za", "com.cn", "com.mx", "com.tr",
}

// sharedSuffixHost reports a host that is a shared suffix itself, or a
// single label (a top-level domain, localhost).
func sharedSuffixHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return !strings.Contains(host, ".") || slices.Contains(sharedHostSuffixes, host)
}

// readApp is account's app record, or 404 app_not_found.
func readApp(ctx context.Context, q queryer, account string) (AppRecord, error) {
	r := AppRecord{App: account}
	err := q.QueryRowContext(ctx, "SELECT name,homepage,owner,created_at,updated_at,(SELECT count(*) FROM identity_house h WHERE h.app=apps.account) FROM apps WHERE account=?", account).
		Scan(&r.Name, &r.Homepage, &r.Owner, &r.CreatedAt, &r.UpdatedAt, &r.HouseAgents)
	if errors.Is(err, sql.ErrNoRows) {
		return r, errAppNotFound()
	}
	return r, err
}

// readAppCommand is app.get: target an app's fingerprint or handle.
func (s *Store) readAppCommand(ctx context.Context, tx *sql.Tx, c Command, a actor) (Result, error) {
	target := c.Target
	if target == "" {
		if !a.signed {
			return Result{}, invalidAppData("app.get names the app in target (a fingerprint or handle).")
		}
		target = a.id
	}
	id, err := agentFingerprint(ctx, tx, target)
	if err != nil {
		return Result{}, err
	}
	account, err := lookupAccount(ctx, tx, id)
	if err != nil {
		if e, ok := err.(*Error); ok && e.Code == "agent_not_found" {
			return Result{}, errAppNotFound()
		}
		return Result{}, err
	}
	app, err := readApp(ctx, tx, account)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"app": app}}, nil
}

// attachApps sets House on house agents and App on apps among agents (by
// key fingerprint), one query each for the page.
func attachApps(ctx context.Context, q allowance.Querier, agents []Agent) error {
	if len(agents) == 0 {
		return nil
	}
	ids := make([]any, len(agents))
	at := map[string]int{}
	for i, a := range agents {
		ids[i] = a.ID
		at[a.ID] = i
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	rows, err := q.QueryContext(ctx, `SELECT i.id,h.app,p.name,p.homepage FROM identities i JOIN identity_house h ON h.account=i.account JOIN apps p ON p.account=h.app WHERE i.id IN (`+marks+`)`, ids...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var h AgentHouse
		if err = rows.Scan(&id, &h.App, &h.Name, &h.Homepage); err != nil {
			rows.Close()
			return err
		}
		h.Label = HouseLabel(h.Name)
		agents[at[id]].House = &h
	}
	if err = rows.Close(); err != nil {
		return err
	}
	rows, err = q.QueryContext(ctx, `SELECT i.id,p.account,p.name,p.homepage,p.owner,p.created_at,p.updated_at,(SELECT count(*) FROM identity_house h WHERE h.app=p.account) FROM identities i JOIN apps p ON p.account=i.account WHERE i.id IN (`+marks+`)`, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var r AppRecord
		if err = rows.Scan(&id, &r.App, &r.Name, &r.Homepage, &r.Owner, &r.CreatedAt, &r.UpdatedAt, &r.HouseAgents); err != nil {
			return err
		}
		agents[at[id]].App = &r
	}
	return rows.Err()
}

// houseAccounts is every house agent's account: the funnel and KPI leave
// them out (funnelExcluded) and trust runs read none of their records.
func houseAccounts(ctx context.Context, q allowance.Querier) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT account FROM identity_house")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var account string
		if err = rows.Scan(&account); err != nil {
			return nil, err
		}
		out[account] = true
	}
	return out, rows.Err()
}

// houseApp is account's app when account is a house agent, else "".
func houseApp(ctx context.Context, q allowance.Querier, account string) (string, error) {
	var app string
	err := q.QueryRowContext(ctx, "SELECT app FROM identity_house WHERE account=?", account).Scan(&app)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return app, err
}

// appPayer is the house actor's paying side: the app account's current
// main key, read once per command (admitHouse).
type appPayer struct {
	account, id, publicKey string
	hosted                 bool
}

// admitHouse runs in executeCommand after the agent's own checks: for a
// house agent's signed command it names the app as the payer (a.house),
// and a write spends one token of the app-wide bucket. Over it, 429
// app_rate_limited: the app's house agents wait together. Reads spend
// nothing.
func (s *Store) admitHouse(ctx context.Context, tx *sql.Tx, cmd Command, a *actor, now int64) error {
	if !a.signed || a.account == "" {
		return nil
	}
	app, err := houseApp(ctx, tx, a.account)
	if err != nil || app == "" {
		return err
	}
	p := &appPayer{account: app}
	var custody string
	err = tx.QueryRowContext(ctx, "SELECT id,public_key,custody FROM identities WHERE account=? AND role='primary' AND successor='' AND revoked_at=0 LIMIT 1", app).Scan(&p.id, &p.publicKey, &custody)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if p.id == "" {
		p.id = app
	}
	p.hosted = custody == "hosted"
	a.house = p
	if !mutation(cmd.Operation) {
		return nil
	}
	bucket, err := s.appBucket(ctx, tx, app, now)
	if err != nil {
		return err
	}
	if s.admitAppBucket(app, bucket) {
		return nil
	}
	return &Error{Status: 429, Code: "app_rate_limited", Message: fmt.Sprintf("This app's house agents together make up to %d writes a minute (bursts of %d), so nothing was done; retry after retry_after seconds. Reads are not limited by it. An app needing more asks for a raise (/api/params/apps).", bucket.WritesPerMinute, bucket.Burst), RetryAfter: 1}
}

// payer is who pays a's allowance spends: a house agent's app (one budget
// for all its house agents, its own allowance and credits), else a itself.
// The seat's own grant, credential and spend limits do not follow.
func (a actor) payer() actor {
	if a.house == nil {
		return a
	}
	return actor{id: a.house.id, account: a.house.account, publicKey: a.house.publicKey, signed: true, hosted: a.house.hosted,
		operation: a.operation, canonical: a.canonical, requestNamespace: a.requestNamespace}
}

func (s *Store) admitAppBucket(app string, size AppBucket) bool {
	s.apps.mu.Lock()
	defer s.apps.mu.Unlock()
	if s.apps.rates == nil {
		s.apps.rates = map[string]privateReadBucket{}
	}
	now := s.now()
	previous, known := s.apps.rates[app]
	bucket := privateBucket(now, previous, float64(size.WritesPerMinute)/60.0, float64(size.Burst))
	if bucket.tokens < 1 {
		s.apps.rates[app] = bucket
		return false
	}
	if !known && len(s.apps.rates) >= appRateEntries {
		for k, b := range s.apps.rates {
			if now.Sub(b.seen) >= time.Minute {
				delete(s.apps.rates, k)
			}
		}
	}
	bucket.tokens--
	s.apps.rates[app] = bucket
	return true
}
