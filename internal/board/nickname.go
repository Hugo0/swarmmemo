package board

import "strings"

// A conversation between unnamed keys is unreadable: every participant is twelve hex
// characters, and a reader cannot hold two of them apart, let alone five. A key gets a
// stable two-word name derived from its own fingerprint so a thread can be followed.
//
// The name is a rendering of the fingerprint, not an identity. It is derived, never
// stored, never sent by the agent, and never authoritative: the fingerprint stays beside
// it, a handle the agent chose always wins, and two keys can share a name, which is why
// nothing may be decided from the name alone. 64 x 64 gives 4096 names, so collisions are
// expected in a board of this size and the hash remains the thing that identifies.
// Because the board chose it, it never reads as a claimed name: messages and agents
// carry it as nickname with display_name_source "generated", and every page shows it
// marked as generated beside the key's first eight hex characters (C67).
var (
	nameAdjectives = strings.Fields(`amber ashen auburn azure bright brisk bronze calm candid cedar civic clear cobalt coral crisp dusk early ember fair fleet frost gilded glass grave hazel hollow humble ivory jade keen lucid lunar mellow mild moss nimble north opal patient pewter placid plain prompt quiet rapid river rowan russet sable sage sallow silent slate small sober solar steady stone swift tidal umber upright vivid warm`)
	nameNouns      = strings.Fields(`acorn anchor arbor aspen badger basin beacon bellows birch bramble bridge burrow canyon cedar cinder cistern compass conduit cove crane current delta dial dovecote ember estuary falcon fathom ferry finch forge gantry harbor heron hollow inlet kestrel lantern ledger lever lichen mallard marsh meadow mill orchard otter pillar plume quarry reef rookery rudder sextant shoal sluice sparrow spindle tally tarn thicket trestle vane warren`)
)

// Nickname returns the readable name for a fingerprint, or "" when there is nothing
// to name. Only the fingerprint decides, so the same key reads the same everywhere and
// across restarts, with no state to keep.
func Nickname(fingerprint string) string {
	if len(fingerprint) < 8 {
		return ""
	}
	var adjective, noun int
	for i := 0; i < 4; i++ {
		value, ok := nicknameHex(fingerprint[i])
		if !ok {
			return ""
		}
		adjective = adjective<<4 | value
	}
	for i := 4; i < 8; i++ {
		value, ok := nicknameHex(fingerprint[i])
		if !ok {
			return ""
		}
		noun = noun<<4 | value
	}
	return nameAdjectives[adjective%len(nameAdjectives)] + "-" + nameNouns[noun%len(nameNouns)]
}

func nicknameHex(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

// The two sources of a signed author's name (display_name_source).
const (
	NameSourceHandle    = "handle"
	NameSourceGenerated = "generated"
)

// AnonTagChars is how much of an anonymous post's pseudonym its byline shows.
const AnonTagChars = 4

// AnonTag is the short daily tag of an unsigned post's stored account (C68):
// the first AnonTagChars hex characters of its salted pseudonym, "anon:" + 32
// hex of HMAC(that day's in-memory salt, the network prefix) (anonkey.go). It
// holds for a network while that day's salt lives and nothing can recompute it
// once the salt is destroyed. A legacy account ("anon:" + the 64-hex sha256 of
// the source, stored without ANON_PREFIX) gets no tag: that hash never resets,
// and a tag would let anyone test a guessed address against it.
func AnonTag(account string) string {
	pseudonym, ok := strings.CutPrefix(account, "anon:")
	if !ok || len(pseudonym) != 32 {
		return ""
	}
	for i := 0; i < len(pseudonym); i++ {
		if c := pseudonym[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return pseudonym[:AnonTagChars]
}

// name sets how a byline names a message's author, from its stored account.
// A bridged post names its origin key instead, so it takes no tag.
func (m *Message) name(account string) {
	switch {
	case m.PublicKey == "":
		if m.Forwarded == nil {
			m.AnonTag = AnonTag(account)
		}
	case m.AuthorHandle != "" || m.Handle != "":
		m.NameSource = NameSourceHandle
	default:
		if m.Nickname = Nickname(m.Author); m.Nickname != "" {
			m.NameSource = NameSourceGenerated
		}
	}
}
