package board

// Hardening of Sign in with SwarmMemo (S2 review): money and value
// operations never run on an app's token, every operation is classified,
// shared hosting suffixes are no app's homepage, exchanged tokens revoke,
// linked devices never sign actor tokens, and the wakeups scope sees only
// wake-ups on the app's rooms or at a time.

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"slices"
	"strings"
	"testing"
)

// bearerReviewedOps are the operations no scope grants today and that are
// not bearerNever: each was reviewed as safe to grant later only through a
// scope with a room or self rule (reads, and writes confined to a room).
// A new operation must be added to bearerNeverOps, a scope, or here.
var bearerReviewedOps = []string{
	"feed.get", "feed.profile.get", "feed.profile.put", "feed.profile.fork", "room.subscribe", "room.unsubscribe",
	"updates.get", "updates.dispose", "updates.mark_read", "journal.get", "journal.suspend", "home", "room.pages", "rooms.list",
	"room.policy.set", "room.moderator.add", "room.moderator.remove", "room.hide", "room.restore",
	"room.style.set", "room.style.clear", "room.style.check", "room.modlog", "agents.list", "agent.posts",
	"verify.publish", "verify.get", "verify.list", "blob.put", "blob.get", "blob.delete", "quota.get", "report", "feedback",
	"stats", "export", "lease.acquire", "lease.release", "work.claim", "work.renew", "work.submit", "work.get", "works.list",
	"work.history", "work.reveal", "webhook.list", "push.list", "offering.list", "offering.get", "offering.calls",
	"offering.call.get", "services.list", "subject.resolve", "subject.get", "subjects.list", "find", "trust.get",
	"trust.penalties", "standing.ways", "standing.challenge", "standing.work", "gate.challenge", "gate.verify",
	"conversations.list", "conversation.get", "conversation.open", "conversation.respond", "conversation.seal",
}

func TestBearerNeverMovesValue(t *testing.T) {
	for _, op := range []string{"x402.submit", "x402.withdraw", "offering.buy", "offering.publish", "offering.refund", "offering.answer",
		"offering.retire", "offering.claim", "offering.decline", "work.create", "work.accept", "work.reject", "work.cancel", "work.promise",
		"work.settle", "work.reviewer.set", "work.successor.set", "credit.transfer", "allowance.transfer", "allowance.transfer.cancel",
		"credits.topup", "room.member.add", "room.member.remove", "room.invite.create", "room.invite.accept", "room.owner.transfer"} {
		if !bearerNever(op) {
			t.Errorf("a bearer token could run %s", op)
		}
	}
	// service.call runs on a bearer token only for the wakeup service.
	if !bearerNeverCommand(Command{Operation: "service.call", Target: "memory"}) || bearerNeverCommand(Command{Operation: "service.call", Target: "wakeup"}) {
		t.Errorf("service.call: only the wakeup service is open to a bearer token")
	}
}

// Every operation is bearerNever, granted by a scope, or reviewed.
func TestEveryOperationClassifiedForBearers(t *testing.T) {
	scoped := map[string]bool{}
	for _, spec := range OAuthScopes() {
		for _, op := range spec.Operations {
			scoped[op] = true
		}
	}
	for _, o := range operations {
		never, reviewed := bearerNever(o.Name), slices.Contains(bearerReviewedOps, o.Name)
		switch {
		case never && (scoped[o.Name] || reviewed):
			t.Errorf("%s is bearerNever and also scoped or reviewed", o.Name)
		case !never && !scoped[o.Name] && !reviewed:
			t.Errorf("%s is new to bearer review: add it to bearerNeverOps (oauthscopes.go), a scope, or bearerReviewedOps", o.Name)
		}
	}
	for _, op := range bearerReviewedOps {
		if _, ok := LookupOperation(op); !ok {
			t.Errorf("bearerReviewedOps lists unknown %s", op)
		}
	}
}

func TestSharedSuffixIsNoAppHomepage(t *testing.T) {
	s := openHostedTest(t)
	s.SetOAuthIssuer(appTestIssuer)
	for i, home := range []string{"https://com", "https://github.io", "https://vercel.app", "https://pages.dev", "https://co.uk", "https://localhost"} {
		k := keyFor(byte(200 + i))
		fails(t, s, signed(k, Command{Operation: "app.register", Data: `{"schema":1,"name":"X","homepage":"` + home + `"}`, RequestID: "h"}), "invalid_app_data")
	}
	alice := keyFor(210)
	run(t, s, signed(alice, Command{Operation: "app.register", Data: `{"schema":1,"name":"Alice","homepage":"https://alice.github.io"}`, RequestID: "a"}))
	client := func(resource string) string {
		return `{"schema":1,"name":"c","redirect_uris":["https://alice.github.io/cb"],"scopes":["identity"],"resources":["` + resource + `"]}`
	}
	run(t, s, signed(alice, Command{Operation: "oauth.client.register", Data: client("https://mcp.alice.github.io/mcp"), RequestID: "c1"}))
	fails(t, s, signed(alice, Command{Operation: "oauth.client.register", Data: client("https://bob.github.io/mcp"), RequestID: "c2"}), "invalid_client_data")
	// An app registered before the rule with a bare suffix claims nothing.
	if _, err := s.db.Exec("UPDATE apps SET homepage='https://com' WHERE account=?", keyID(alice)); err != nil {
		t.Fatal(err)
	}
	fails(t, s, signed(alice, Command{Operation: "oauth.client.register", Data: client("https://victim.com/mcp"), RequestID: "c3"}), "invalid_client_data")
}

func exchangeFor(t *testing.T, a appTestSetup) (OAuthTokens, OAuthTokens) {
	t.Helper()
	_, subject := a.signIn(t, "smcl_agent", "https://hexarena.example/mcp", "identity app-rooms")
	got, err := a.s.OAuthExchange(testContext, OAuthExchangeRequest{ClientID: a.client.ID, SubjectToken: subject.AccessToken, SubjectTokenType: OAuthTokenTypeAccess,
		ActorToken: SignActorToken(a.app, a.client.ID, a.s.OAuthTokenEndpoint(), testTime), ActorTokenType: OAuthTokenTypeJWT}, testTime)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	return subject, got
}

func TestRevokeExchangedToken(t *testing.T) {
	a := newAppTest(t, appClientJSON)
	s := a.s
	subject, got := exchangeFor(t, a)
	// Another client's revocation changes nothing.
	if err := s.OAuthRevoke(testContext, got.AccessToken, "smapp_other", testTime); err != nil {
		t.Fatal(err)
	}
	if _, err := bearer(s, got.AccessToken, Command{Operation: "room.get", Room: appTable}); err != nil {
		t.Fatalf("revoked by another client: %v", err)
	}
	if err := s.OAuthRevoke(testContext, got.AccessToken, a.client.ID, testTime); err != nil {
		t.Fatal(err)
	}
	if _, err := bearer(s, got.AccessToken, Command{Operation: "room.get", Room: appTable}); appErrCode(err) != "invalid_token" {
		t.Fatalf("revoked exchanged token: %s", appErrCode(err))
	}
	if in, _ := s.OAuthIntrospection(testContext, got.AccessToken, testTime); in["active"] != false {
		t.Fatalf("introspection of a revoked exchanged token: %v", in)
	}
	// The subject's own connection is the agent's: it stays.
	if in, _ := s.OAuthIntrospection(testContext, subject.AccessToken, testTime); in["active"] != true {
		t.Fatalf("subject after revoking the exchanged token: %v", in)
	}
}

func TestDeviceKeyIsNoActor(t *testing.T) {
	a := newAppTest(t, appClientJSON)
	s := a.s
	_, subject := a.signIn(t, "smcl_agent", "https://hexarena.example/mcp", "identity")
	device := keyFor(211)
	if _, err := s.db.Exec("INSERT INTO identities(id,public_key,account,created_at,last_seen,role,label) VALUES(?,?,?,?,?,'device','laptop')",
		keyID(device), base64.RawURLEncoding.EncodeToString(device.Public().(ed25519.PublicKey)), keyID(a.app), testTime, testTime); err != nil {
		t.Fatal(err)
	}
	_, err := s.OAuthExchange(testContext, OAuthExchangeRequest{ClientID: a.client.ID, SubjectToken: subject.AccessToken, SubjectTokenType: OAuthTokenTypeAccess,
		ActorToken: SignActorToken(device, a.client.ID, s.OAuthTokenEndpoint(), testTime), ActorTokenType: OAuthTokenTypeJWT}, testTime)
	if appErrCode(err) != "invalid_client" {
		t.Fatalf("a linked device's actor token: %s", appErrCode(err))
	}
}

const wakeupClientJSON = `{"schema":1,"name":"Hex Arena web","redirect_uris":["https://hexarena.example/cb"],"scopes":["wakeups"]}`

func TestWakeupsScopeSeesOnlyTheAppsRooms(t *testing.T) {
	a := newAppTestOn(t, openTest(t, Config{HostedKEKFile: writeKEK(t, 0o600), Features: Features{Services: []string{"wakeup"}}}), wakeupClientJSON)
	s := a.s
	account, tokens := a.signIn(t, a.client.ID, s.OAuthAppResource(keyID(a.app)), "wakeups")
	schedule := func(key, args string) Command {
		return Command{Operation: "service.call", Target: "wakeup", RequestID: "w-" + key, Data: `{"schema":1,"method":"schedule","args":{"key":"` + key + `",` + args + `},"max_cost":1}`}
	}
	// The person's own wake-ups on other rooms and messages.
	own := hostedKeyOf(t, s, account)
	run(t, s, signed(own, Command{Operation: "room.create", Room: "lobby", RequestID: "mine-room"}))
	run(t, s, signed(own, schedule("mine-lobby", `"on":"room","room":"lobby"`)))
	run(t, s, signed(own, schedule("mine-replies", `"on":"reply"`)))
	// The app may schedule on its own room or at a time, nothing else.
	if _, err := bearer(s, tokens.AccessToken, schedule("table", `"on":"room","room":"`+appTable+`"`)); err != nil {
		t.Fatalf("wake-up on the app's room: %v", err)
	}
	if _, err := bearer(s, tokens.AccessToken, schedule("soon", `"at":`+strconv.FormatInt(int64(testTime)+3600, 10))); err != nil {
		t.Fatalf("time wake-up: %v", err)
	}
	for name, c := range map[string]Command{
		"another room": schedule("lobby", `"on":"room","room":"lobby"`),
		"replies":      schedule("replies", `"on":"reply"`),
		"messages":     schedule("dms", `"on":"message"`),
		"status":       {Operation: "service.read", Target: "wakeup", Data: `{"schema":1,"method":"status","args":{"request_id":"w-mine-lobby"}}`},
	} {
		if _, err := bearer(s, tokens.AccessToken, c); appErrCode(err) != "insufficient_scope" {
			t.Errorf("%s: %s, want insufficient_scope", name, appErrCode(err))
		}
	}
	res, err := bearer(s, tokens.AccessToken, Command{Operation: "service.read", Target: "wakeup", Data: `{"schema":1,"method":"list"}`})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	raw, _ := json.Marshal(res.Data)
	if strings.Contains(string(raw), "mine-") || strings.Contains(string(raw), "lobby") || !strings.Contains(string(raw), "table") || !strings.Contains(string(raw), "soon") {
		t.Fatalf("an app's wake-up list: %s", raw)
	}
	// Notices: a firing on another room is not the app's to see.
	notices := json.RawMessage(`{"notices":[{"seq":1,"id":"a","on":"room","fired_at":1,"event":"e1","room":"lobby"},{"seq":2,"id":"b","on":"room","fired_at":1,"event":"e2","room":"`+appTable+`"},{"seq":3,"id":"c","on":"reply","fired_at":1,"event":"e3"},{"seq":4,"id":"d","on":"time","fired_at":1,"at":5}],"next_after":4}`)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	out, err := bearerWakeupView(testContext, tx, notices, keyID(a.app))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); strings.Contains(got, "lobby") || strings.Contains(got, "e3") || !strings.Contains(got, "e2") || !strings.Contains(got, `"id":"d"`) || !strings.Contains(got, `"next_after":4`) {
		t.Fatalf("filtered notices: %s", got)
	}
}
