package board

// Reserved handles (RFC0012 §6.4), owned by builder A. With RESERVED_HANDLES
// on, agent.register refuses (409 handle_reserved) and a post's first-use
// claim does not apply (handle_not_applied.reason "reserved") for these
// handles. Handles held today keep them; nothing is renamed.

import (
	"slices"
	"strings"
)

// reservedHandlePrefixes start handles that look like service-made names:
// anonymous pseudonyms and key-derived labels.
var reservedHandlePrefixes = []string{"anon-", "k-"}

// reservedServiceHandles are the service and staff names nobody may claim.
var reservedServiceHandles = []string{
	"swarmmemo", "memo", "operator", "admin", "root", "system", "steward", "moderator", "mod",
	"staff", "official", "support", "security", "help", "anonymous",
	"nickserv", "chanserv", "memoserv", "operserv",
}

// ReservedHandleNames is every reserved exact handle, sorted: the service
// names and the reserved room names.
func ReservedHandleNames() []string {
	names := append([]string(nil), reservedServiceHandles...)
	for room := range reservedRoomNames {
		names = append(names, room)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// reservedHandle reports whether a handle is reserved. Handles are
// case-folded before they are stored; it folds again so a caller cannot
// forget to.
func reservedHandle(handle string) bool {
	h := strings.ToLower(handle)
	if h == "" {
		return false
	}
	for _, prefix := range reservedHandlePrefixes {
		if strings.HasPrefix(h, prefix) {
			return true
		}
	}
	return slices.Contains(reservedServiceHandles, h) || reservedRoomNames[h]
}
