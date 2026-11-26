package services

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
)

// A refused call on record (C1157): a fetch the site itself refused
// (robots.txt, a bot challenge, 401/403, 429: siteRefusalCode) keeps its call
// id and a record, so a worker can cite that it tried. Like any call's
// entry, a signed caller's record becomes public only when its agent cites
// it (service.cite); an unsigned caller's, which no one can cite, is public
// from the start and names no one. The record is
// the call's service_calls.public, written when the call settles as failed
// (no new column): where (the URL's scheme, host and path, never its query
// string or fragment), why (the refusal code, and the robots.txt line or
// status the refusal names) and, from the row, when. It costs nothing, and
// the call counts toward the same daily and per-network limits as any call:
// its row and its place in the anonymous windows are taken when it is made.

// HistoryStateRefused is a refused call's entry state: the stored row is
// failed, its record says by whom and why.
const HistoryStateRefused = "refused"

// RefusalPublicKey marks, in a refusal's details, a record already public
// (an unsigned caller's): the board words the refusal by it and never sends
// it.
const RefusalPublicKey = "call_public"

// refusalTargetBytes bounds a record's target, so the target and the
// robots.txt line (robotsLineBytes) always fit in PublicBytes.
const refusalTargetBytes = 512

// callRefusal is a refused call's public record. A provider marks a refusal
// citable by returning it as Result.Public beside the error; the engine keeps
// it only when Refused names the very code the call failed with.
type callRefusal struct {
	Refused      string `json:"refused"`
	Target       string `json:"target"`
	RobotsLine   string `json:"robots_line,omitempty"`
	RobotsStatus int    `json:"robots_status,omitempty"`
}

// refusedPublic is the public record a failed call keeps: raw when it is a
// callRefusal for code that fits a record, else "{}".
func refusedPublic(raw json.RawMessage, code string) string {
	r, ok := refusalOf(string(raw))
	if !ok || r.Refused != code {
		return "{}"
	}
	return publicOf(raw)
}

// refusalOf reads a stored public record as a callRefusal; ok is false for
// any other record (a finished call's sizes, "{}", an erased one).
func refusalOf(public string) (callRefusal, bool) {
	var r callRefusal
	if !strings.Contains(public, `"refused"`) || json.Unmarshal([]byte(public), &r) != nil || r.Refused == "" {
		return callRefusal{}, false
	}
	return r, true
}

// refusedEntry turns a failed call's entry into a refused one when its row
// keeps a record: state refused, the target, the code (error) and the
// robots.txt line or status it names. Cost stays the row's: 0.
func refusedEntry(out map[string]any, public string) {
	r, ok := refusalOf(public)
	if !ok {
		return
	}
	out["state"], out["target"], out["error"] = HistoryStateRefused, clipTarget(r.Target), r.Refused
	if r.RobotsLine != "" {
		out["robots_line"] = r.RobotsLine
	}
	if r.RobotsStatus != 0 {
		out["robots_status"] = r.RobotsStatus
	}
}

// publicURL is a fetched URL as a record may show it: no port (80 or 443
// only), no query string (it may carry tokens), no fragment, no credentials.
// Refusals and history entries (a cited one is public) both use it.
func publicURL(u *url.URL) string {
	host := u.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return (&url.URL{Scheme: u.Scheme, Host: host, Path: u.Path, RawPath: u.RawPath}).String()
}

// fetchRefusal is the record of a fetch of u refused with code and the
// refusal's details (robots_line, robots_status); nil for any refusal that is
// not the site's own (an invalid URL, a private address, our own limits).
func fetchRefusal(u *url.URL, code string, details any) json.RawMessage {
	if u == nil || !siteRefusalCode(code) {
		return nil
	}
	target := publicURL(u)
	if len(target) > refusalTargetBytes {
		target = target[:refusalTargetBytes] // percent-encoded: ASCII, cut anywhere
	}
	r := callRefusal{Refused: code, Target: target}
	if d, ok := details.(map[string]any); ok {
		r.RobotsLine, _ = d["robots_line"].(string)
		r.RobotsStatus, _ = d["robots_status"].(int)
	}
	// Unescaped HTML characters keep the record under PublicBytes: the
	// target is percent-encoded and the robots line is printable ASCII.
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(r) != nil {
		return nil
	}
	return bytes.TrimSpace(b.Bytes())
}

// withCallID adds details.call_id to a refusal's details: a map gains the
// key; nil becomes {call_id}. Details of another shape are left alone.
func withCallID(details any, id string) any {
	switch d := details.(type) {
	case nil:
		return map[string]any{"call_id": id}
	case map[string]any:
		out := make(map[string]any, len(d)+1)
		for k, v := range d {
			out[k] = v
		}
		out["call_id"] = id
		return out
	}
	return details
}
