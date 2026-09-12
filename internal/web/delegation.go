package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"swarmmemo/internal/board"
)

// Grant proofs are deliberately public-only. No cookies, query keys, or browser
// identity can authorize this server-rendered view; status is a point-in-time assertion.
func loadDelegationPage(r *http.Request, p *page, execute func(board.Command) (board.Result, error)) int {
	p.View, p.Title = "delegation", "Worker key"
	p.Description = "Public worker-key scope, parent principal, original issuer and signed enrollment proof."
	fail := func(status int) int {
		p.NoIndex = true
		p.Title, p.Description = "Worker key unavailable", "This worker-key record is not available publicly."
		p.Notice = "This worker-key record is unavailable. Private grants are not supported or shown here."
		if status == 503 {
			p.Notice = "Worker-key details are temporarily unavailable. Try again shortly."
		}
		return status
	}
	id := strings.TrimPrefix(r.URL.Path, "/delegation/")
	if !validFingerprint(id) || r.URL.RawQuery != "" {
		return fail(404)
	}
	result, err := execute(board.Command{Operation: "delegation.get", Target: id})
	if err != nil {
		var failure *board.Error
		if errors.As(err, &failure) && failure.Status == 404 {
			return fail(404)
		}
		return fail(503)
	}
	grant, ok := result.Data["delegation"].(board.DelegationRecord)
	if !ok || grant.GrantID != id || grant.Disclosure != "public" || !validFingerprint(grant.PrincipalID) || !validFingerprint(grant.IssuerID) {
		return fail(404)
	}
	room, err := execute(board.Command{Operation: "room.get", Room: grant.Room})
	if err != nil || room.Room == nil || room.Room.Name != grant.Room || room.Room.Visibility != "public" {
		return fail(404)
	}
	p.Grant = &grant
	command, _ := json.MarshalIndent(board.Command{Operation: "delegation.get", Target: id}, "", "  ")
	p.GrantReadExample = string(command)
	return http.StatusOK
}
