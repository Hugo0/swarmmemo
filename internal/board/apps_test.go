package board

import (
	"bytes"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

const appJSON = `{"schema":1,"name":"Hex Arena","homepage":"https://hexarena.example"}`

// An app registers itself, names an owner only with the owner's proof, and
// makes house agents that every agent read shows as run by it.
func TestAppRegisterAndHouseAgents(t *testing.T) {
	s := openHostedTest(t)
	app, owner, stranger := keyFor(171), keyFor(172), keyFor(173)
	register(t, s, owner)
	fails(t, s, signed(app, Command{Operation: "app.house.create", Handle: "seat-0", RequestID: "h0"}), "app_required")
	fails(t, s, signed(app, Command{Operation: "app.register", Data: `{"schema":1,"name":"","homepage":"https://hexarena.example"}`, RequestID: "r0"}), "invalid_app_data")
	fails(t, s, signed(app, Command{Operation: "app.register", Data: `{"schema":1,"name":"Hex","homepage":"http://hexarena.example"}`, RequestID: "r1"}), "invalid_app_data")
	fails(t, s, signed(app, Command{Operation: "app.register", Data: `{"schema":1,"name":"Hex","homepage":"https://hexarena.example","extra":1}`, RequestID: "r2"}), "invalid_app_data")

	r := run(t, s, signed(app, Command{Operation: "app.register", Data: appJSON, RequestID: "r3"}))
	rec := r.Data["app"].(AppRecord)
	if rec.App != keyID(app) || rec.Owner != keyID(app) || rec.Name != "Hex Arena" || rec.HouseAgents != 0 {
		t.Fatalf("app.register = %+v", rec)
	}

	// Another owner needs that owner's proof over the app's fingerprint.
	bad := base64.RawURLEncoding.EncodeToString(ed25519.Sign(stranger, []byte(AppOwnerContext+keyID(app))))
	fails(t, s, signed(app, Command{Operation: "app.register", Data: `{"schema":1,"name":"Hex Arena","homepage":"https://hexarena.example","owner":"` + keyID(owner) + `","owner_proof":"` + bad + `"}`, RequestID: "r4"}), "invalid_app_owner_proof")
	good := base64.RawURLEncoding.EncodeToString(ed25519.Sign(owner, []byte(AppOwnerContext+keyID(app))))
	r = run(t, s, signed(app, Command{Operation: "app.register", Data: `{"schema":1,"name":"Hex Arena","homepage":"https://hexarena.example","owner":"` + keyID(owner) + `","owner_proof":"` + good + `"}`, RequestID: "r5"}))
	if got := r.Data["app"].(AppRecord).Owner; got != keyID(owner) {
		t.Fatalf("owner = %s, want %s", got, keyID(owner))
	}

	created := run(t, s, signed(app, Command{Operation: "app.house.create", Handle: "seat-1", RequestID: "h1"}))
	house, _ := created.Data["agent"].(string)
	token, _ := created.Data["token"].(string)
	if house == "" || token == "" || created.Data["house"] != keyID(app) || created.Data["recovery_code"] == nil {
		t.Fatalf("app.house.create = %v", created.Data)
	}
	got := run(t, s, Command{Operation: "app.get", Target: keyID(app)}).Data["app"].(AppRecord)
	if got.HouseAgents != 1 {
		t.Fatalf("app.get house_agents = %d, want 1", got.HouseAgents)
	}
	fails(t, s, Command{Operation: "app.get", Target: keyID(owner)}, "app_not_found")

	seat := hostedKey(t, s, token)
	postAs(t, s, seat, Command{Text: "seat one is here", RequestID: "seat-post"})
	agent := run(t, s, Command{Operation: "agent.get", Target: house}).Agent
	if agent.House == nil || agent.House.App != keyID(app) || agent.House.Label != "run by app Hex Arena" {
		t.Fatalf("agent.get house = %+v", agent.House)
	}
	if a := run(t, s, Command{Operation: "agent.get", Target: keyID(app)}).Agent; a.App == nil || a.App.HouseAgents != 1 || a.House != nil {
		t.Fatalf("app's agent.get = app %+v house %+v", a.App, a.House)
	}
	// The directory leaves house agents out unless asked (include house),
	// in every order.
	for _, kind := range []string{"new", "active", "hot", ""} {
		for _, a := range run(t, s, Command{Operation: "agents.list", Kind: kind}).Agents {
			if a.ID == house {
				t.Fatalf("agents.list %q lists the house agent without include house", kind)
			}
		}
		listed := false
		for _, a := range run(t, s, Command{Operation: "agents.list", Kind: kind, Data: `{"schema":1,"include":"house"}`}).Agents {
			if a.ID == house {
				listed = a.House != nil && a.House.Name == "Hex Arena"
			}
		}
		if !listed {
			t.Fatalf("agents.list %q with include house does not show the house agent's app", kind)
		}
	}
	fails(t, s, Command{Operation: "agents.list", Data: `{"schema":1,"include":"bots"}`}, "invalid_list_options")
	// A house agent registers no app and makes none.
	fails(t, s, signedNow(s, seat, Command{Operation: "app.register", Data: appJSON, RequestID: "hr"}), "app_required")
	fails(t, s, signedNow(s, seat, Command{Operation: "app.house.create", RequestID: "hh"}), "app_required")
}

// House agents give and earn no standing: no record of theirs reaches a
// trust run; the funnel and KPI leave them out, the operator list does not
// gain them; and they share their app's bucket, which never lifts a limit.
func TestHouseAgentsExcludedAndBucketed(t *testing.T) {
	s := openTest(t, Config{HostedKEKFile: writeKEK(t, 0o600), Features: Features{Trust: TrustOn}})
	app, outsider := keyFor(181), keyFor(182)
	run(t, s, signed(app, Command{Operation: "app.register", Data: appJSON, RequestID: "r"}))
	created := run(t, s, signed(app, Command{Operation: "app.house.create", RequestID: "h"}))
	house := created.Data["agent"].(string)
	seat := hostedKey(t, s, created.Data["token"].(string))
	root := postAs(t, s, seat, Command{Text: "a house post", RequestID: "house-post"})
	postAs(t, s, outsider, Command{Text: "an outside reply", ReplyTo: root, RequestID: "outside-reply"})

	s.now = func() time.Time { return time.Unix(testTime+86400+3600, 0) }
	var buf bytes.Buffer
	if err := s.WriteTrustInputs(testContext, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, house) {
		t.Fatalf("the trust inputs name the house agent:\n%s", out)
	}
	if !strings.Contains(out, keyID(outsider)) {
		t.Fatalf("the outsider's reply is missing from the trust inputs:\n%s", out)
	}

	excluded, err := s.funnelExcluded(testContext)
	if err != nil || !excluded[house] {
		t.Fatalf("funnelExcluded = %v, %v; want the house agent", excluded, err)
	}
	if list, _ := s.FunnelExcludeList(testContext); len(list) != 0 {
		t.Fatalf("the operator exclude list gained %v", list)
	}

	// Drain the app's bucket: the house agent is refused, an outsider not.
	for s.admitAppBucket(keyID(app), AppHouseBucket()) {
	}
	fails(t, s, signedNow(s, seat, Command{Operation: "post", Text: "over the app bucket", RequestID: "over"}), "app_rate_limited")
	postAs(t, s, outsider, Command{Text: "outsiders are not in the bucket", RequestID: "fine"})
	s.now = func() time.Time { return time.Unix(testTime+86400+3700, 0) }
	postAs(t, s, seat, Command{Text: "the bucket refilled", RequestID: "refilled"})
}

// An owner disowns its app; a house agent is never claimed; the app pays
// for its house agents; only writes spend the bucket.
func TestAppOwnerReleaseClaimAndPayer(t *testing.T) {
	s := openHostedTest(t)
	app, owner, stranger := keyFor(191), keyFor(192), keyFor(193)
	register(t, s, owner)
	register(t, s, stranger)
	fails(t, s, signed(owner, Command{Operation: "app.owner.release", Target: keyID(app), RequestID: "x0"}), "app_not_found")
	proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(owner, []byte(AppOwnerContext+keyID(app))))
	run(t, s, signed(app, Command{Operation: "app.register", Data: `{"schema":1,"name":"Hex Arena","homepage":"https://hexarena.example","owner":"` + keyID(owner) + `","owner_proof":"` + proof + `"}`, RequestID: "r"}))
	fails(t, s, signed(stranger, Command{Operation: "app.owner.release", Target: keyID(app), RequestID: "x1"}), "app_owner_required")
	fails(t, s, signed(app, Command{Operation: "app.owner.release", Target: keyID(app), RequestID: "x2"}), "app_owner_required")
	released := run(t, s, signed(owner, Command{Operation: "app.owner.release", Target: keyID(app), RequestID: "x3"})).Data["app"].(AppRecord)
	if released.Owner != keyID(app) || released.Name != "Hex Arena" {
		t.Fatalf("app.owner.release = %+v", released)
	}
	fails(t, s, signed(owner, Command{Operation: "app.owner.release", Target: keyID(app), RequestID: "x4"}), "app_owner_required")

	created := run(t, s, signed(app, Command{Operation: "app.house.create", Handle: "seat-9", RequestID: "h"}))
	house := created.Data["agent"].(string)
	seat := hostedKey(t, s, created.Data["token"].(string))
	ownPublic := base64.RawURLEncoding.EncodeToString(keyFor(194).Public().(ed25519.PublicKey))
	claimProof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(keyFor(194), []byte(HostedClaimContext+house+"\x00"+ownPublic)))
	fails(t, s, signedNow(s, seat, Command{Operation: "hosted.claim", Data: `{"schema":1,"recovery_code":"` + created.Data["recovery_code"].(string) + `","new_public_key":"` + ownPublic + `","proof":"` + claimProof + `"}`}), "house_agent_unclaimable")

	// The seat's post is paid from the app's allowance, not its own.
	used := func(account string) int64 {
		var n int64
		err := s.db.QueryRow("SELECT coalesce(sum(used),0) FROM quota WHERE actor=?", account).Scan(&n)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		return n
	}
	appBefore := used(keyID(app))
	postAs(t, s, seat, Command{Text: "paid by the app", RequestID: "paid"})
	if used(house) != 0 || used(keyID(app)) <= appBefore {
		t.Fatalf("house used %d, app used %d (before %d): the app pays", used(house), used(keyID(app)), appBefore)
	}

	// A drained bucket refuses the seat's writes, never its reads.
	for s.admitAppBucket(keyID(app), AppHouseBucket()) {
	}
	fails(t, s, signedNow(s, seat, Command{Operation: "post", Text: "over", RequestID: "over"}), "app_rate_limited")
	run(t, s, signedNow(s, seat, Command{Operation: "updates.get"}))
}
