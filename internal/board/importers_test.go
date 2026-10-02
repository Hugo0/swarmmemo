package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"testing"
)

func TestImporterAllowlist(t *testing.T) {
	s := openTest(t, updatesConfig())
	key := keyFor(61)
	body := []byte(fmt.Sprintf(`{"schema":1,"accounts":[%q]}`, keyID(key)))
	if _, err := s.SetAllowanceParams(testContext, ImportersParamsNamespace, body, "site archive approved", 0); err != nil {
		t.Fatal(err)
	}
	post := Command{Operation: "post", Room: "lobby", Page: "article", Kind: "imported", Text: "An old public comment", Handle: "site-archive"}
	id := run(t, s, signed(key, post)).Receipt.ID
	got := run(t, s, Command{Operation: "message.get", MessageID: id}).Messages[0]
	if !got.ArchiveEligible || got.Curated || got.Kind != "imported" {
		t.Fatalf("wrong import provenance: %+v", got)
	}
	for _, c := range []Command{signed(keyFor(62), post), post} {
		_, err := s.Execute(testContext, c, "test-origin")
		e, ok := err.(*Error)
		if !ok || e.Code != "reserved_kind" || e.Status != 403 {
			t.Fatalf("unlisted importer: %v", err)
		}
	}
	if _, err := s.SetAllowanceParams(testContext, ImportersParamsNamespace, defaultImporterParams(), "revoke", 0); err != nil {
		t.Fatal(err)
	}
	fails(t, s, signed(key, post), "reserved_kind")
	// Even a corrupted stored body grants no import privilege.
	if _, err := s.db.Exec(`UPDATE params SET body='{"schema":1,"accounts":["bad"]}' WHERE namespace='importers'`); err != nil {
		t.Fatal(err)
	}
	fails(t, s, signed(key, post), "reserved_kind")
}

func TestImporterParamsFailClosed(t *testing.T) {
	s := openTest(t, updatesConfig())
	for _, body := range []string{`{}`, `{"schema":1,"accounts":[],"accounts":[]}`, `{"schema":2,"accounts":[]}`, `{"schema":1,"accounts":null}`, `{"schema":1,"accounts":["*"]}`, `{"schema":1,"accounts":[],"unknown":true}`, `{"schema":1,"accounts":[]} {}`, fmt.Sprintf(`{"schema":1,"accounts":[%q,%q]}`, keyID(keyFor(61)), keyID(keyFor(61)))} {
		if _, err := s.SetAllowanceParams(testContext, ImportersParamsNamespace, []byte(body), "invalid", 0); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestImporterAccountSurvivesRotationAndRejectsDelegation(t *testing.T) {
	s, parent, child, grant := delegationFixture(t)
	body := []byte(fmt.Sprintf(`{"schema":1,"accounts":[%q,%q]}`, keyID(parent), keyID(child)))
	if _, err := s.SetAllowanceParams(testContext, ImportersParamsNamespace, body, "approved parent and child accounts", 0); err != nil {
		t.Fatal(err)
	}
	post := Command{Operation: "post", Room: "grant-room", Visibility: "public", Kind: "imported", Text: "Imported public comment"}
	fails(t, s, childCommand(s, child, grant, post), "reserved_kind")
	next := keyFor(63)
	rotate := signed(parent, Command{Operation: "agent.rotate", Target: base64.RawURLEncoding.EncodeToString(next.Public().(ed25519.PublicKey))})
	rotate.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(next, Canonical("swarmmemo.com", rotate)))
	run(t, s, rotate)
	run(t, s, signed(next, post))
}
