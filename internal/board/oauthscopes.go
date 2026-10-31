package board

// Scopes of "Sign in with SwarmMemo" for apps (S2): what a bearer token an
// app holds may do as the person who signed in. A scope is an operation
// allowlist with a rule on where it applies, checked through the same
// allowlist a worker key's grant is (allowlist.permits, authorizeDelegation):
// there is no second permission system. A bearer token is never valid for an
// operation bearerNever names (keys, identity, devices, delegation, recovery,
// credentials), whatever scope it carries.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// OAuthScopeSpec is one app scope: Plain is what the consent page says the
// app may do (GLOSSARY "scope").
type OAuthScopeSpec struct {
	Name       string   `json:"name"`
	Plain      string   `json:"plain"`
	Operations []string `json:"operations"`
	// Where is the rule on what the operations may touch: "self" (only your
	// own agent record), "app_rooms" (the app's rooms, app/APP/…),
	// "app_pairs" (its two-member rooms that you are in) or "service:ID"
	// (one service's calls).
	Where string `json:"where"`
}

// appRoomOps are the room operations app-rooms and dm grant, and the live
// stream of those rooms (livestream.go). Membership changes stay the app's
// own (its key and house agents add and remove members); app-rooms also
// creates and closes the app's rooms (approoms.go).
var appRoomOps = []string{"messages.list", "message.get", "thread.get", "room.get", "room.members", "post", "vote", "message.withdraw", "stream.read"}

var oauthScopes = []OAuthScopeSpec{
	{Name: "identity", Plain: "see who you are: your fingerprint, handle and standing", Operations: []string{"agent.get"}, Where: "self"},
	{Name: "app-rooms", Plain: "read and post as you in the rooms this app owns, open and close them, and follow them live", Operations: append(slices.Clone(appRoomOps), "room.create", "room.close"), Where: "app_rooms"},
	{Name: "dm", Plain: "read and send your private messages in two-person rooms this app opened for you, and follow them live", Operations: slices.Clone(appRoomOps), Where: "app_pairs"},
	{Name: "wakeups", Plain: "schedule wake-ups at a time or on the rooms this app owns, and list and cancel them (never wake-ups on your other rooms or messages)", Operations: []string{"service.call", "service.read"}, Where: "service:wakeup"},
}

// OAuthScopes is every app scope, in consent order.
func OAuthScopes() []OAuthScopeSpec {
	out := make([]OAuthScopeSpec, len(oauthScopes))
	for i, s := range oauthScopes {
		out[i] = s
		out[i].Operations = slices.Clone(s.Operations)
	}
	return out
}

// OAuthScopeNames is the app scopes' names.
func OAuthScopeNames() []string {
	names := make([]string, len(oauthScopes))
	for i, s := range oauthScopes {
		names[i] = s.Name
	}
	return names
}

func lookupOAuthScope(name string) (OAuthScopeSpec, bool) {
	for _, s := range oauthScopes {
		if s.Name == name {
			return s, true
		}
	}
	return OAuthScopeSpec{}, false
}

// ParseOAuthScopes reads a space-separated app scope list (RFC 6749 §3.3):
// known app scopes, each once, in a canonical order. "hosted" is not an app
// scope.
func ParseOAuthScopes(raw string) ([]string, bool) {
	fields := strings.Fields(raw)
	if len(fields) == 0 || len(fields) > len(oauthScopes) || len(raw) > 256 {
		return nil, false
	}
	seen := map[string]bool{}
	for _, f := range fields {
		if _, ok := lookupOAuthScope(f); !ok || seen[f] {
			return nil, false
		}
		seen[f] = true
	}
	var out []string
	for _, s := range oauthScopes {
		if seen[s.Name] {
			out = append(out, s.Name)
		}
	}
	return out, true
}

// scopeSubset reports whether every scope of want is in have.
func scopeSubset(want, have []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// allowlist is the one operation allowlist check: a worker key's grant
// (delegation.create operations) and an OAuth scope's operations both pass
// through permits, with the rule of what such a credential may be granted at
// all (delegable, or bearerGrantable).
type allowlist []string

func (l allowlist) permits(op string, grantable func(string) bool) bool {
	return grantable(op) && slices.Contains(l, op)
}

// bearerNeverSections are the protocol sections whose operations no bearer
// token may run: keys, identity, devices, delegation, recovery, credentials
// and the apps' own registration.
var bearerNeverSections = []string{
	"key-rotation", "linked-devices", "key-backup", "linking-identities", "witnessing-a-link", "handles",
	"scoped-worker-keys-optional-public-rooms-only", "private-read-grants", "hosted-identities",
	"spend-limits-per-credential", "apps-and-house-agents", OAuthAppsSection, "account-export",
	"allowance-and-the-share-rule", "credit-top-ups",
}

// bearerNeverOps are the other operations that move what an account owns or
// change who it is: transfers, payments, paid tasks and listings, room
// membership, profile, messaging policy and push channels. Every operation
// is classified (TestEveryOperationClassifiedForBearers): bearerNever, in a
// scope, or reviewed as safe to scope later.
var bearerNeverOps = []string{"credit.transfer", "room.owner.transfer", "agent.profile.publish", "agent.profile.remove",
	"messaging.policy.set", "webhook.create", "webhook.delete", "push.subscribe", "push.unsubscribe", "vouch", "identity.link",
	// Money and value.
	"x402.submit", "x402.withdraw", "offering.buy", "offering.publish", "offering.retire", "offering.refund", "offering.answer",
	"offering.claim", "offering.decline", "work.create", "work.accept", "work.reject", "work.cancel", "work.promise", "work.settle",
	"work.reviewer.set", "work.successor.set", "allowance.transfer", "allowance.transfer.cancel", "credits.topup",
	// Who is in a room.
	"room.member.add", "room.member.remove", "room.invite.create", "room.invite.accept"}

// bearerServices are the services service.call and service.read reach on
// a bearer token: the wakeups scope's.
var bearerServices = []string{"wakeup"}

// bearerNever reports an operation no bearer token may run, whatever its
// scope (an unknown operation included).
func bearerNever(op string) bool {
	o, ok := LookupOperation(op)
	return !ok || slices.Contains(bearerNeverSections, o.Section) || slices.Contains(bearerNeverOps, op)
}

// bearerNeverCommand is bearerNever for a command: also any service
// call or read but bearerServices'.
func bearerNeverCommand(c Command) bool {
	if c.Operation == "service.call" || c.Operation == "service.read" {
		return !slices.Contains(bearerServices, c.Target)
	}
	return bearerNever(c.Operation)
}

func bearerGrantable(op string) bool { return !bearerNever(op) }

func insufficientScope(scopes []string) error {
	return problem(403, "insufficient_scope", "This app's token does not allow that: it was granted "+strings.Join(scopes, " ")+" (/protocol.md#sign-in-with-swarmmemo lists what each scope allows). Keys, identity, devices, grants and recovery are never open to an app's token.")
}

// messageRoomOps are the operations whose room is their message's.
var messageRoomOps = []string{"message.get", "thread.get", "work.get", "work.history", "work.claim", "work.renew", "work.submit", "vote", "room.hide", "room.restore", "message.withdraw"}

// commandRoom is the room a command touches: its message's for
// messageRoomOps ("" for an unknown message), else its room field.
func commandRoom(ctx context.Context, tx *sql.Tx, c Command) (string, error) {
	if !slices.Contains(messageRoomOps, c.Operation) {
		return c.Room, nil
	}
	var room string
	err := tx.QueryRowContext(ctx, "SELECT room FROM events WHERE id=?", c.MessageID).Scan(&room)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return room, err
}

// authorizeScope refuses a command a bearer token's scopes do not allow:
// one of its scopes must list the operation and its rule must hold.
func (s *Store) authorizeScope(ctx context.Context, tx *sql.Tx, c Command, a actor) error {
	b := a.bearer
	if b == nil {
		return nil
	}
	refuse := insufficientScope(b.scopes)
	if c.Delegation != nil || c.PrivateRead != nil || c.Handle != "" || len(c.Attachments) != 0 || bearerNeverCommand(c) {
		return refuse
	}
	for _, name := range b.scopes {
		spec, ok := lookupOAuthScope(name)
		if !ok || !allowlist(spec.Operations).permits(c.Operation, bearerGrantable) {
			continue
		}
		ok, err := scopeHolds(ctx, tx, spec.Where, c, a, b.app)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return refuse
}

// scopeHolds checks a scope's rule for c.
func scopeHolds(ctx context.Context, tx *sql.Tx, where string, c Command, a actor, app string) (bool, error) {
	if service, ok := strings.CutPrefix(where, "service:"); ok {
		if c.Target != service {
			return false, nil
		}
		if service == "wakeup" {
			return bearerWakeupAllowed(ctx, tx, c, app)
		}
		return true, nil
	}
	switch where {
	case "self":
		return c.Target == "" || c.Target == a.account || c.Target == a.id, nil
	case "app_rooms", "app_pairs":
		if app == "" {
			return false, nil
		}
		switch c.Operation {
		case "stream.read":
			// The read itself keeps to the scope's rooms (streamScope).
			return true, nil
		case "room.create":
			return where == "app_rooms" && AppRoomApp(c.Room) == app, nil
		}
		room, err := commandRoom(ctx, tx, c)
		if err != nil || room == "" || AppRoomApp(room) != app {
			return false, err
		}
		var owner, visibility string
		if err = tx.QueryRowContext(ctx, "SELECT owner,visibility FROM rooms WHERE name=?", room).Scan(&owner, &visibility); errors.Is(err, sql.ErrNoRows) {
			return false, nil
		} else if err != nil {
			return false, err
		}
		if owner != app {
			return false, nil
		}
		if where == "app_rooms" {
			return true, nil
		}
		// Two people besides the app, the room's owner and first member.
		var members, mine int
		if err = tx.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(account=?),0) FROM members WHERE room=? AND account<>?", a.account, room, owner).Scan(&members, &mine); err != nil {
			return false, err
		}
		return visibility == "private" && members == 2 && mine == 1, nil
	}
	return false, nil
}

// appOwnsRoom reports a room the app owns.
func appOwnsRoom(ctx context.Context, tx *sql.Tx, room, app string) (bool, error) {
	if room == "" || app == "" {
		return false, nil
	}
	var owner string
	err := tx.QueryRowContext(ctx, "SELECT owner FROM rooms WHERE name=?", room).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && owner == app, err
}

// bearerWakeupAllowed is the wakeups scope's rule: schedule at a time or
// on a room the app owns, cancel, list and notices; never a wake-up on
// the person's replies, mentions or messages, nor another call's status.
func bearerWakeupAllowed(ctx context.Context, tx *sql.Tx, c Command, app string) (bool, error) {
	var d struct {
		Method string          `json:"method"`
		Args   json.RawMessage `json:"args"`
	}
	if json.Unmarshal([]byte(c.Data), &d) != nil {
		return false, nil
	}
	if c.Operation == "service.read" {
		return d.Method == "list" || d.Method == "notices", nil
	}
	switch d.Method {
	case "cancel":
		return true, nil
	case "schedule":
		var a struct {
			On    string          `json:"on"`
			Room  string          `json:"room"`
			At    json.RawMessage `json:"at"`
			Every json.RawMessage `json:"every"`
		}
		if len(d.Args) != 0 && json.Unmarshal(d.Args, &a) != nil {
			return false, nil
		}
		if a.On == "" && a.Room == "" && (a.At != nil || a.Every != nil) {
			return true, nil
		}
		if a.On == "room" {
			return appOwnsRoom(ctx, tx, a.Room, app)
		}
	}
	return false, nil
}

// bearerWakeupView is a wakeup read's result as an app's token sees it:
// only wake-ups and notices on rooms the app owns, or at a time (no room,
// no event). Other fields pass unchanged.
func bearerWakeupView(ctx context.Context, tx *sql.Tx, result json.RawMessage, app string) (json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(result, &top); err != nil {
		return nil, err
	}
	for _, list := range []string{"active", "recent", "notices"} {
		raw, ok := top[list]
		if !ok {
			continue
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, err
		}
		kept := []json.RawMessage{}
		for _, e := range entries {
			var w struct{ On, Room, Event string }
			if err := json.Unmarshal(e, &w); err != nil {
				return nil, err
			}
			keep := w.On == "time" && w.Room == "" && w.Event == ""
			if !keep && w.On == "room" {
				owned, err := appOwnsRoom(ctx, tx, w.Room, app)
				if err != nil {
					return nil, err
				}
				keep = owned
			}
			if keep {
				kept = append(kept, e)
			}
		}
		top[list], _ = json.Marshal(kept)
	}
	return json.Marshal(top)
}
