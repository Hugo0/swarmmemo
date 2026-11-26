package board

import "testing"

// The wakeups scope may cancel only time wake-ups or wake-ups on the app's
// own rooms. It must not cancel the user's wake-ups on other rooms or events.
func TestAuditWakeupsScopeCannotCancelOtherWakeups(t *testing.T) {
	a := newAppTestOn(t, openTest(t, Config{HostedKEKFile: writeKEK(t, 0o600), Features: Features{Services: []string{"wakeup"}}}), wakeupClientJSON)
	s := a.s
	account, tokens := a.signIn(t, a.client.ID, s.OAuthAppResource(keyID(a.app)), "wakeups")
	schedule := func(key, args, requestID string) Command {
		return Command{Operation: "service.call", Target: "wakeup", RequestID: requestID, Data: `{"schema":1,"method":"schedule","args":{"key":"` + key + `",` + args + `},"max_cost":1}`}
	}
	own := hostedKeyOf(t, s, account)
	run(t, s, signed(own, Command{Operation: "room.create", Room: "lobby", RequestID: "own-lobby-room"}))
	run(t, s, signed(own, schedule("private-lobby", `"on":"room","room":"lobby"`, "own-lobby")))
	run(t, s, signed(own, schedule("private-replies", `"on":"reply"`, "own-replies")))

	cancelPrivate := Command{Operation: "service.call", Target: "wakeup", RequestID: "cancel-private", Data: `{"schema":1,"method":"cancel","args":{"key":"private-lobby"},"max_cost":1}`}
	if _, err := bearer(s, tokens.AccessToken, cancelPrivate); appErrCode(err) != "insufficient_scope" {
		t.Fatalf("app token canceled another room's wake-up: %v", appErrCode(err))
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM wakeups WHERE account=? AND key='private-lobby' AND state='active'", account); n != 1 {
		t.Fatalf("private lobby wake-up changed: %d active rows", n)
	}

	cancelReplies := Command{Operation: "service.call", Target: "wakeup", RequestID: "cancel-replies", Data: `{"schema":1,"method":"cancel","args":{"key":"private-replies"},"max_cost":1}`}
	if _, err := bearer(s, tokens.AccessToken, cancelReplies); appErrCode(err) != "insufficient_scope" {
		t.Fatalf("app token canceled reply wake-up: %v", appErrCode(err))
	}

	if _, err := bearer(s, tokens.AccessToken, schedule("app-room", `"on":"room","room":"`+appTable+`"`, "app-room")); err != nil {
		t.Fatalf("app token could not schedule its own room wake-up: %v", err)
	}
	cancelApp := Command{Operation: "service.call", Target: "wakeup", RequestID: "cancel-app", Data: `{"schema":1,"method":"cancel","args":{"key":"app-room"},"max_cost":1}`}
	if _, err := bearer(s, tokens.AccessToken, cancelApp); err != nil {
		t.Fatalf("app token could not cancel its own room wake-up: %v", err)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM wakeups WHERE account=? AND key='app-room' AND state='cancelled'", account); n != 1 {
		t.Fatalf("app room wake-up not cancelled: %d rows", n)
	}
}
