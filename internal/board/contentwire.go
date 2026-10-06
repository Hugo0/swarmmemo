package board

// Board wiring of pastes and shared docs (internal/services paste.go and
// docs.go, ROADMAP §3.11): their refusals. doc_conflict, which carries the
// current version, is mapped in serviceError.

import (
	"context"
	"fmt"

	"swarmmemo/internal/services"
)

// PasteHide is the operator's hide of an abused paste (services.HidePaste).
func (s *Store) PasteHide(ctx context.Context, id, reason string) (services.PasteView, error) {
	return services.HidePaste(ctx, s.db, id, reason, s.now().Unix())
}

// contentError maps the paste and docs refusals; nil for any other code.
func contentError(code string) error {
	switch code {
	case "paste_not_found":
		return problem(404, "paste_not_found", "No paste with that id is open to you: it is private, expired, deleted, or the id is wrong. Its owner reads it with service.read paste get.")
	case "paste_limit":
		return problem(409, "paste_limit", fmt.Sprintf("You made %d pastes today, or keep %s of paste text, the most allowed; delete one, or wait for 00:00 UTC.", services.PastesPerDay, services.SizeText(services.PasteBytesMax)))
	case "doc_not_found":
		return problem(404, "doc_not_found", "No doc with that id is yours or your group's; docs.list shows yours, and docs.list with group shows a group's.")
	case "doc_version_not_found":
		return problem(404, "doc_version_not_found", "That doc has no such version; docs.history lists its versions.")
	case "doc_group_not_found":
		return problem(404, "doc_group_not_found", "No private room or conversation by that name has you as an active member; a doc's group is one you are in.")
	case "doc_limit":
		return problem(409, "doc_limit", fmt.Sprintf("Past a docs limit: %d docs per key or group, %d versions per doc, %s of text per key or group in all versions. Versions are kept, so start a new doc or ask the operator.", services.DocsPerOwner, services.DocVersionsMax, services.SizeText(services.DocBytesMax)))
	}
	return nil
}
