package web

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
var (
	nameAdjectives = strings.Fields(`amber ashen auburn azure bright brisk bronze calm candid cedar civic clear cobalt coral crisp dusk early ember fair fleet frost gilded glass grave hazel hollow humble ivory jade keen lucid lunar mellow mild moss nimble north opal patient pewter placid plain prompt quiet rapid river rowan russet sable sage sallow silent slate small sober solar steady stone swift tidal umber upright vivid warm`)
	nameNouns      = strings.Fields(`acorn anchor arbor aspen badger basin beacon bellows birch bramble bridge burrow canyon cedar cinder cistern compass conduit cove crane current delta dial dovecote ember estuary falcon fathom ferry finch forge gantry harbor heron hollow inlet kestrel lantern ledger lever lichen mallard marsh meadow mill orchard otter pillar plume quarry reef rookery rudder sextant shoal sluice sparrow spindle tally tarn thicket trestle vane warren`)
)

// AgentNickname returns the readable name for a fingerprint, or "" when there is nothing
// to name. Only the fingerprint decides, so the same key reads the same everywhere and
// across restarts, with no state to keep.
func AgentNickname(fingerprint string) string {
	if len(fingerprint) < 8 {
		return ""
	}
	var adjective, noun int
	for i := 0; i < 4; i++ {
		value, ok := hexValue(fingerprint[i])
		if !ok {
			return ""
		}
		adjective = adjective<<4 | value
	}
	for i := 4; i < 8; i++ {
		value, ok := hexValue(fingerprint[i])
		if !ok {
			return ""
		}
		noun = noun<<4 | value
	}
	return nameAdjectives[adjective%len(nameAdjectives)] + "-" + nameNouns[noun%len(nameNouns)]
}

func hexValue(c byte) (int, bool) {
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
