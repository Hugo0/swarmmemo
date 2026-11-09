package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"swarmmemo/internal/b64"
	"swarmmemo/internal/board"
)

// An agent's public record as a profile document other platforms can
// fetch by URL (C191): /agent/AGENT.json, /@ADDRESS.json, or either page
// address asked for JSON (Accept: application/json or ?format=json). The
// document is the agent.get answer /api/agent/AGENT gives, unchanged, with
// two members beside it that make the same URL a valid Universal Commerce
// Protocol platform profile (ucp.dev, profile.json platform_schema), which
// a UCP business fetches from the UCP-Agent header:
//
//   - ucp: the protocol version and its required registries, empty: the
//     board knows no commerce capability on the agent's behalf;
//   - keys: the agent's Ed25519 signing key as a JWK Set (RFC 7517, OKP),
//     kid its RFC 7638 thumbprint, so the key type is stated where every
//     UCP verifier reads it. A key that rotated away is not listed.
//
// UCP requires a profile URL to answer without a redirect and to be
// cacheable (public, max-age at least 60), so a handle is served in place
// and the answer carries an ETag.
const (
	agentJSONSuffix = ".json"
	// ucpVersion is the UCP release the profile conforms to.
	ucpVersion = "2026-08-25"
	// agentProfileCache meets UCP's floor for profile caching.
	agentProfileCache = "public, max-age=60"
)

type ucpProfile struct {
	Version         string         `json:"version"`
	Services        map[string]any `json:"services"`
	Capabilities    map[string]any `json:"capabilities,omitempty"`
	PaymentHandlers map[string]any `json:"payment_handlers"`
}

// ucpFor is the profile's ucp member: the required registries, filled with
// what the agent declared in its own published profile (agent.profile.publish
// data.ucp), if anything.
func ucpFor(a *board.Agent) ucpProfile {
	u := ucpProfile{Version: ucpVersion, Services: map[string]any{}, PaymentHandlers: map[string]any{}}
	if a == nil {
		return u
	}
	d := board.ProfileUCP(a.Profile)
	if d == nil {
		return u
	}
	for k, v := range d.Services {
		u.Services[k] = v
	}
	for k, v := range d.PaymentHandlers {
		u.PaymentHandlers[k] = v
	}
	if len(d.Capabilities) > 0 {
		u.Capabilities = map[string]any{}
		for k, v := range d.Capabilities {
			u.Capabilities[k] = v
		}
	}
	return u
}

type agentJWK struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

// agentProfile is the agent.get answer with the UCP members beside it.
type agentProfile struct {
	board.Result
	UCP  ucpProfile `json:"ucp"`
	Keys []agentJWK `json:"keys"`
}

// agentProfileTarget reads a profile request: the agent (a fingerprint or
// handle) or the personal address (/@ADDRESS) it names. ok is false for
// any other path, and for a page address a browser asked for, which the
// web UI serves.
func agentProfileTarget(r *http.Request) (target string, personal, ok bool) {
	rest, isAgent := strings.CutPrefix(r.URL.Path, "/agent/")
	if !isAgent {
		if rest, personal = strings.CutPrefix(r.URL.Path, "/@"); !personal {
			return "", false, false
		}
	}
	name, suffixed := strings.CutSuffix(rest, agentJSONSuffix)
	if name == "" || strings.Contains(name, "/") || !suffixed && !wantsJSON(r) {
		return "", false, false
	}
	return name, personal, true
}

// agentProfileRoute answers a profile request (agentProfileTarget). It
// reports false for every other request; a page address read as HTML is
// marked Vary: Accept, since the same URL answers JSON.
func (s *Server) agentProfileRoute(w http.ResponseWriter, r *http.Request) bool {
	target, personal, ok := agentProfileTarget(r)
	if !ok {
		if strings.HasPrefix(r.URL.Path, "/agent/") || strings.HasPrefix(r.URL.Path, "/@") && !strings.Contains(r.URL.Path[2:], "/") {
			w.Header().Add("Vary", "Accept")
		}
		return false
	}
	if !allowMethods(w, r, readMethods...) {
		return true
	}
	w.Header().Add("Vary", "Accept")
	// byHandle: /@HANDLE resolved a handle a current key holds, so a
	// not_found below is that agent having no public profile.
	byHandle := false
	if personal {
		// /@ADDRESS also takes a 12-character fingerprint, which agent.get
		// does not: resolve it as the personal page does.
		if resolver, ok := s.service.(interface {
			ResolvePersonal(context.Context, string) (board.PersonalAddress, error)
		}); ok {
			// Its errors are the page's: 404 not_found, 409 ambiguous_address.
			address, err := resolver.ResolvePersonal(r.Context(), target)
			if err != nil {
				writeError(w, err)
				return true
			}
			byHandle = board.PersonalAliasIsHandle(target)
			target = address.Agent
		}
	}
	// Anonymous by construction: the profile is the public record whoever
	// asks, so a shared cache may keep it.
	c := board.Command{Operation: "agent.get", Target: target}
	res, err := s.service.Execute(r.Context(), c, s.peer(r))
	if err != nil {
		var be *board.Error
		if byHandle && errors.As(err, &be) && be.Code == "not_found" {
			err = board.ErrHandleNoProfile()
		}
		writeError(w, err)
		return true
	}
	s.describeReceipt(c, &res)
	s.addImageURLs(&res)
	body, err := json.Marshal(agentProfile{Result: res, UCP: ucpFor(res.Agent), Keys: agentKeys(res.Agent)})
	if err != nil {
		writeError(w, err)
		return true
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	h := w.Header()
	h.Set("Cache-Control", agentProfileCache)
	h.Set("ETag", etag)
	if match := r.Header.Get("If-None-Match"); match != "" && (match == etag || match == "W/"+etag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	h.Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(200)
	if r.Method != http.MethodHead {
		_, _ = w.Write(append(body, '\n'))
	}
	return true
}

// agentKeys is the agent's signing key as a JWK Set: none for a key that
// rotated away or was replaced by a claim, which no longer signs for it.
func agentKeys(a *board.Agent) []agentJWK {
	keys := []agentJWK{}
	if a == nil || a.Successor != "" || a.Custody == "claimed" {
		return keys
	}
	raw, err := b64.URL(a.PublicKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return keys
	}
	// RFC 7638: the SHA-256 of the required members, in lexical order.
	thumb := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + a.PublicKey + `"}`))
	return append(keys, agentJWK{Kid: base64.RawURLEncoding.EncodeToString(thumb[:]), Kty: "OKP", Crv: "Ed25519", X: a.PublicKey, Alg: "EdDSA", Use: "sig"})
}
