package services

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzContentArgs feeds arbitrary args to every paste and docs parser: none
// panics, and whatever one accepts is within the published bounds (text,
// title, ids, expiry, versions).
func FuzzContentArgs(f *testing.F) {
	for _, seed := range []string{
		`{"text":"Build log for run 42: all green.","visibility":"unlisted","expires_in":86400}`,
		`{"text":"x","title":"t","notary":true}`, `{"id":"0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d","screen":false}`,
		`{"title":"Plan","text":"1.","group":"team"}`, `{"id":"0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c","base_version":1,"text":"v2","title":"T"}`,
		`{"id":"0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c","version":2}`, `{"text":"a\u0000b"}`, `{"text":"x","expires_in":1e9}`,
		`{"text":"x","text":"y"}`, `{"title":"‮","text":""}`, `[]`, `{"base_version":-1}`, `{"group":"~abcdefghijklmnopqrstuvwxyz"}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		args := json.RawMessage(raw)
		if s, err := parsePasteCreate(args); err == nil {
			if len(s.text) > PasteTextBytes || len(s.title) > contentTitleBytes || !utf8.ValidString(s.text) || strings.ContainsRune(s.text, 0) || !validTitle(s.title) {
				t.Fatalf("paste.create accepted %q", raw)
			}
			if s.visibility != "private" && s.visibility != "unlisted" || s.expiresIn != 0 && (s.expiresIn < PasteExpiryMin || s.expiresIn > PasteExpiryMax) {
				t.Fatalf("paste.create accepted %q", raw)
			}
		}
		if a, err := parsePasteOpen(args); err == nil && !contentIDRE.MatchString(a.ID) {
			t.Fatalf("paste.open accepted %q", raw)
		}
		if id, err := parsePasteRef(args); err == nil && !contentIDRE.MatchString(id) {
			t.Fatalf("paste.delete accepted %q", raw)
		}
		if a, err := parseDocCreate(args); err == nil {
			if len(*a.Text) > DocTextBytes || *a.Title == "" || len(*a.Title) > contentTitleBytes || !validContentText(*a.Text) || a.Group != "" && !docGroupRE.MatchString(a.Group) {
				t.Fatalf("docs.create accepted %q", raw)
			}
		}
		if a, base, err := parseDocWrite(args); err == nil {
			if !contentIDRE.MatchString(a.ID) || base < 1 || base > DocVersionsMax || len(*a.Text) > DocTextBytes || a.Title != nil && (*a.Title == "" || !validTitle(*a.Title)) {
				t.Fatalf("docs.write accepted %q", raw)
			}
		}
		if a, version, err := parseDocRead(args); err == nil && (!contentIDRE.MatchString(a.ID) || version < 0 || version > DocVersionsMax) {
			t.Fatalf("docs.read accepted %q", raw)
		}
		// The quotes take the same args and never price below the method's base.
		for _, m := range []string{"create", "delete", "open"} {
			if q, err := (&paste{}).Quote(Call{Method: m, Args: args, Price: Price{Base: 1, PerKiB: 1}}); err == nil && q.Max < 1 {
				t.Fatalf("paste.%s quoted %d for %q", m, q.Max, raw)
			}
		}
		for _, m := range []string{"create", "write", "read"} {
			if q, err := (&docs{}).Quote(Call{Method: m, Args: args, Price: Price{Base: 1, PerKiB: 1}}); err == nil && q.Max < 1 {
				t.Fatalf("docs.%s quoted %d for %q", m, q.Max, raw)
			}
		}
	})
}

// FuzzScreenFor checks that a read's screening never hands out a flagged
// verdict's text and always says what it did, whatever is kept.
func FuzzScreenFor(f *testing.F) {
	f.Add(true, true, `{"verdict":"flag","threshold":0.6,"categories":{},"model":"m"}`, "done")
	f.Add(false, true, `{"verdict":"pass"}`, "unavailable")
	f.Add(false, true, `not json`, "pending")
	f.Fuzz(func(t *testing.T, own, wants bool, kept, state string) {
		sc := screenFor(own, wants, kept, state, nil)
		if sc.Screen == "" && state != "" {
			t.Fatalf("no screen state for %v %v %q %q", own, wants, kept, state)
		}
		if sc.Verdict != nil && sc.Verdict.Verdict == "flag" && !sc.Withheld {
			t.Fatalf("a flagged text is not withheld: %+v", sc)
		}
		if own && (sc.Screen != "own" || sc.Withheld) || !own && !wants && (sc.Screen != "off" || sc.Withheld) {
			t.Fatalf("%v %v: %+v", own, wants, sc)
		}
	})
}
