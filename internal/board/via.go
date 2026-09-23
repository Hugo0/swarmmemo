package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
)

// Provenance: which channel carried a message to the board ("via"). The server
// sets it from the route a request actually arrived on, never from a command
// field, so it is not signed and a client cannot choose it. Two values rest on
// something the server cannot see for itself, and say so in their Carrier: "ui"
// is inferred from the browser's own same-origin fetch metadata, and "email"
// relayed over HTTP is the claim of the operator's mail bridge, which presented
// its secret. "nostr" is not stored here at all: it is read from the forward
// record the in-process Nostr bridge writes (forwarded.go). Vias is the single source: the
// stored values, the badge labels, room policy write_via, /capabilities and the
// generated PROTOCOL list all read it.

// Via is one channel a message can arrive on.
type Via struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	// Carrier is the route that sets it, as a reader would reproduce it.
	Carrier string `json:"carrier"`
	// Bridge marks a value an operator bridge claims with its own secret
	// (the file named by BRIDGE_TOKEN_<NAME>_FILE); the board cannot observe the far side.
	Bridge bool `json:"bridge,omitempty"`
	// Transports are the /capabilities transports that carry it. A via with
	// none is served by HTTP itself and always available; one with some is
	// available only while one of them is enabled for writing.
	Transports []string `json:"transports,omitempty"`
}

var vias = []Via{
	{Name: "ui", Label: "UI", Carrier: "the site's own composer: a same-origin browser request (`Sec-Fetch-Site: same-origin`) to `POST /v1/command` or the no-script form"},
	{Name: "get", Label: "GET", Carrier: "`GET /w/ROOM/PAGE?text=…` or `GET /w64/ROOM/PAGE/PAYLOAD`"},
	{Name: "post", Label: "POST", Carrier: "`POST /w/ROOM/PAGE` with a text, form or JSON body"},
	{Name: "put", Label: "PUT", Carrier: "`PUT /w/ROOM/PAGE` or `PUT /v1/events/REQUEST_ID`"},
	{Name: "mkcol", Label: "MKCOL", Carrier: "`MKCOL /w64/ROOM/PAGE/PAYLOAD`"},
	{Name: "x-text", Label: "X-Text", Carrier: "an `X-Text` header on `/w/ROOM/PAGE`, whatever the method"},
	{Name: "c64", Label: "c64", Carrier: "`GET` or `POST /c64/COMMAND`"},
	{Name: "command", Label: "command", Carrier: "`POST /v1/command` from anything but the site's own pages"},
	{Name: "mcp", Label: "MCP", Carrier: "the hosted MCP tool `post_message` at `/mcp`"},
	{Name: "dns", Label: "DNS", Carrier: "a signed command in DNS TXT queries (DNS write)", Transports: []string{"dns"}},
	{Name: "tcp", Label: "netcat", Carrier: "the TCP line protocol: `POST` or `CMD` over netcat", Transports: []string{"tcp"}},
	{Name: "gemini", Label: "Gemini", Carrier: "a Gemini input prompt", Transports: []string{"gemini"}},
	{Name: "email", Label: "email", Carrier: "mail to `ROOM@post.HOST`: the SMTP listener, or the operator's email bridge", Bridge: true, Transports: []string{"smtp", "email"}},
	{Name: "nostr", Label: "Nostr", Carrier: "a Nostr note the in-process Nostr bridge reissued; read back from the message's `forwarded.origin_service`", Transports: []string{"nostr"}},
}

// viaGroups are shorthands a room policy may name; they expand when checked.
var viaGroups = map[string][]string{
	"http": {"get", "post", "put", "mkcol", "x-text", "c64", "command"},
}

// Vias returns every channel value, in display order.
func Vias() []Via { return slices.Clone(vias) }

// ViaGroups returns the policy shorthands and what each stands for.
func ViaGroups() map[string][]string {
	out := map[string][]string{}
	for name, members := range viaGroups {
		out[name] = slices.Clone(members)
	}
	return out
}

// LookupVia returns a channel by value.
func LookupVia(name string) (Via, bool) {
	for _, v := range vias {
		if v.Name == name {
			return v, true
		}
	}
	return Via{}, false
}

// BridgeVia reports whether name is a value only an operator bridge may claim.
func BridgeVia(name string) bool {
	v, ok := LookupVia(name)
	return ok && v.Bridge
}

type viaKey struct{}

// WithVia records the channel a request arrived on. Only adapters call it,
// from the route they served; nothing a client sends reaches it except a
// bridge claim the HTTP adapter has already checked.
func WithVia(ctx context.Context, via string) context.Context {
	return context.WithValue(ctx, viaKey{}, via)
}

// ViaFrom is the channel an adapter recorded, or "" if it recorded a value
// that is not in Vias (or none).
func ViaFrom(ctx context.Context) string {
	via, _ := ctx.Value(viaKey{}).(string)
	if _, ok := LookupVia(via); !ok {
		return ""
	}
	return via
}

// ViaAllowed reports whether a post that arrived on via may be published under
// a write_via list. An empty list allows every channel. A post with no known
// channel is refused by any list: an adapter that forgot to say how it was
// reached cannot slip through.
func ViaAllowed(list []string, via string) bool {
	if len(list) == 0 {
		return true
	}
	if via == "" {
		return false
	}
	for _, name := range list {
		if name == via || slices.Contains(viaGroups[name], via) {
			return true
		}
	}
	return false
}

// ViaLabels names a write_via list for people: "DNS or TCP".
func ViaLabels(list []string) string {
	names := []string{}
	for _, name := range list {
		if members, ok := viaGroups[name]; ok {
			names = append(names, "HTTP ("+strings.Join(members, ", ")+")")
			continue
		}
		if v, ok := LookupVia(name); ok {
			names = append(names, v.Label)
		}
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

// parseWriteVia validates a policy's write_via: known channel values or group
// names, no repeats. It is stored as given, so a group keeps its short name.
func parseWriteVia(raw json.RawMessage) ([]string, error) {
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil || len(list) > len(vias)+len(viaGroups) {
		return nil, problem(400, "invalid_policy", `write_via must be a list of channel names; see "vias" in /capabilities.`)
	}
	seen := map[string]bool{}
	for _, name := range list {
		_, known := LookupVia(name)
		_, group := viaGroups[name]
		if !known && !group {
			return nil, problem(400, "invalid_policy", "write_via names an unknown channel; the channels and groups are listed under \"vias\" in /capabilities.")
		}
		if seen[name] {
			return nil, problem(400, "invalid_policy", "write_via names a channel twice.")
		}
		seen[name] = true
	}
	return list, nil
}

// messageVia is a stored message's channel: the recorded one, or for a
// message a bridge reissued, its origin network when that is a known via.
func messageVia(stored string, f *Forwarded) string {
	if stored == "" && f != nil {
		if _, ok := LookupVia(f.OriginService); ok {
			return f.OriginService
		}
	}
	return stored
}

func encodeWriteVia(list []string) string { return strings.Join(list, " ") }
func decodeWriteVia(s string) []string    { return strings.Fields(s) }

// migrateVia adds events.via and room_policies.write_via. Keyed on the columns
// themselves, not on user_version, so it can be renumbered or merged beside
// another branch's migration. Earlier messages keep an empty via: nobody
// recorded how they arrived, and nothing is guessed.
func migrateVia(tx *sql.Tx) error {
	for _, column := range []struct{ table, name string }{{"events", "via"}, {"room_policies", "write_via"}} {
		var exists int
		if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info(?) WHERE name=?", column.table, column.name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := tx.Exec("ALTER TABLE " + column.table + " ADD COLUMN " + column.name + " TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
		}
	}
	return nil
}
