package board

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"swarmmemo/internal/services"
)

// Verdict checks (C97): an optional, bounded list a verifier signs beside its
// overall verdict, saying per property what it checked and what it did not.
// identity.witness and work.accept / work.reject data take it as "checks".
// The list lives inside the signed command data, so it is covered by the
// command's signature and stored with it (link_witnesses.signed_payload,
// work_transitions.payload); reads derive it from those bytes. The overall
// verdict keeps its meaning: checks only add detail.

const (
	// VerdictChecksMax bounds the entries in one checks list.
	VerdictChecksMax = 16
	// VerdictChecksBytesMax bounds the checks list's raw JSON.
	VerdictChecksBytesMax = 8192
	// VerdictCheckToolMax and VerdictCheckEvidenceMax bound those fields, in
	// characters.
	VerdictCheckToolMax     = 80
	VerdictCheckEvidenceMax = 200
)

// VerdictCheckStates is every state a check may report.
func VerdictCheckStates() []string {
	return []string{"pass", "fail", "not_checkable", "not_checked"}
}

// VerdictCheckProperties are suggested property tokens; any token matching
// verdictCheckPropertyRE is accepted.
func VerdictCheckProperties() []string {
	return []string{"integrity", "signature", "issuer_auth", "anchor", "conformance"}
}

var verdictCheckPropertyRE = regexp.MustCompile(`^[a-z0-9_.-]{1,40}$`)

// VerdictCheck is one property a verifier reports on.
type VerdictCheck struct {
	Property      string `json:"property"`
	State         string `json:"state"`
	SubjectSHA256 string `json:"subject_sha256,omitempty"`
	Tool          string `json:"tool,omitempty"`
	Evidence      string `json:"evidence,omitempty"`
}

// VerdictChecksRule is the whole rule, the tail of every checks refusal.
var VerdictChecksRule = fmt.Sprintf(`checks is an optional list of 1 to %d objects {"property","state","subject_sha256","tool","evidence"}: property a token matching ^[a-z0-9_.-]{1,40}$ (e.g. integrity, signature, issuer_auth, anchor, conformance), state pass, fail, not_checkable or not_checked, and optionally subject_sha256 (64 lowercase hex), tool (name@version, at most %d characters) and evidence (a message ID, URL or SHA-256, at most %d characters); no control characters, no other fields.`, VerdictChecksMax, VerdictCheckToolMax, VerdictCheckEvidenceMax)

// checkText is a short display string: valid UTF-8, 1 to max characters, no
// control characters, no leading or trailing space.
func checkText(s string, max int) bool {
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > max || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return false
		}
	}
	return true
}

// parseVerdictChecks validates a checks list strictly. On a refusal it
// returns the reason, naming the entry and field where it can (an unknown
// field name is echoed only when services.EchoesArg allows it); the caller
// wraps it in its own error code.
func parseVerdictChecks(raw json.RawMessage) ([]VerdictCheck, string) {
	if len(raw) > VerdictChecksBytesMax {
		return nil, fmt.Sprintf("checks is longer than %d bytes.", VerdictChecksBytesMax)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if token, err := dec.Token(); err != nil || token != json.Delim('[') {
		return nil, "checks must be an array."
	}
	checks := []VerdictCheck{}
	for dec.More() {
		at := fmt.Sprintf("checks[%d]", len(checks))
		if len(checks) == VerdictChecksMax {
			return nil, fmt.Sprintf("checks has more than %d entries.", VerdictChecksMax)
		}
		if token, err := dec.Token(); err != nil || token != json.Delim('{') {
			return nil, at + " must be an object."
		}
		var c VerdictCheck
		seen := map[string]bool{}
		for dec.More() {
			token, err := dec.Token()
			name, ok := token.(string)
			if err != nil || !ok {
				return nil, at + " must be an object."
			}
			if seen[name] {
				return nil, at + " repeats a field."
			}
			seen[name] = true
			var dest *string
			switch name {
			case "property":
				dest = &c.Property
			case "state":
				dest = &c.State
			case "subject_sha256":
				dest = &c.SubjectSHA256
			case "tool":
				dest = &c.Tool
			case "evidence":
				dest = &c.Evidence
			default:
				if !services.EchoesArg(name) {
					return nil, at + " has a field a check does not take."
				}
				return nil, at + "." + name + " is not a field a check takes."
			}
			var value json.RawMessage
			if err = dec.Decode(&value); err != nil || json.Unmarshal(value, dest) != nil {
				return nil, at + "." + name + " must be a string."
			}
			var ok2 bool
			switch name {
			case "property":
				ok2 = verdictCheckPropertyRE.MatchString(c.Property)
			case "state":
				ok2 = validCheckState(c.State)
			case "subject_sha256":
				ok2 = fingerprintRE.MatchString(c.SubjectSHA256)
			case "tool":
				ok2 = checkText(c.Tool, VerdictCheckToolMax)
			case "evidence":
				ok2 = checkText(c.Evidence, VerdictCheckEvidenceMax)
			}
			if !ok2 {
				return nil, at + "." + name + " is not valid."
			}
		}
		if token, err := dec.Token(); err != nil || token != json.Delim('}') {
			return nil, at + " must be an object."
		}
		if !seen["property"] || !seen["state"] {
			return nil, at + " needs property and state."
		}
		checks = append(checks, c)
	}
	if token, err := dec.Token(); err != nil || token != json.Delim(']') {
		return nil, "checks must be an array."
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, "checks must be an array."
	}
	if len(checks) == 0 {
		return nil, "checks, when sent, needs at least one entry."
	}
	return checks, ""
}

func validCheckState(s string) bool {
	for _, state := range VerdictCheckStates() {
		if s == state {
			return true
		}
	}
	return false
}

// signedChecks reads the checks list out of a stored signed command (its
// canonical bytes): the list the signer signed, already validated when the
// command was accepted. nil when the command carried none.
func signedChecks(payload string) []VerdictCheck {
	if !strings.Contains(payload, "checks") {
		return nil
	}
	var envelope struct {
		Command struct {
			Data string `json:"data"`
		} `json:"command"`
	}
	if json.Unmarshal([]byte(payload), &envelope) != nil {
		return nil
	}
	var data struct {
		Checks json.RawMessage `json:"checks"`
	}
	if json.Unmarshal([]byte(envelope.Command.Data), &data) != nil || len(data.Checks) == 0 {
		return nil
	}
	checks, problem := parseVerdictChecks(data.Checks)
	if problem != "" {
		return nil
	}
	return checks
}
