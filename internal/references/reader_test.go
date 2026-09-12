package references

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const fixtureNow = int64(1788566400)

func mustCanonical(t testing.TB, value any) []byte {
	t.Helper()
	raw, err := Canonical(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func fixture(t testing.TB) ([]byte, []byte, *Snapshot) {
	t.Helper()
	permissions := map[string]any{}
	for _, right := range []string{"automated_collection", "public_archive", "full_text_storage"} {
		permissions[right] = map[string]any{"approved": true, "reviewed_at": "2026-09-04T00:00:00Z", "expires_at": "2026-09-06T00:00:00Z", "scope": "PRIVATE OPERATOR SCOPE CANARY", "evidence_url": "https://example.com/ticket?private-token=CANARY"}
	}
	registryValue := map[string]any{"version": 1, "sources": []any{map[string]any{"id": "fixture", "name": "External fixture", "enabled": true, "adapter": "jsonfeed-1.1", "feed_url": "https://example.com/feed.json", "permissions": permissions}}}
	registryRaw, err := json.MarshalIndent(registryValue, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	suppression := mustCanonical(t, map[string]any{"version": 1, "ids": []string{}})
	id, err := ReferenceID("fixture", "external/one")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &Snapshot{Version: 1, State: "ready", RegistrySHA256: hash(registryRaw), SuppressionSHA256: hash(suppression), GeneratedAt: fixtureNow, ValidUntil: fixtureNow + 900,
		Sources:    []Source{{ID: "fixture", Name: "External fixture", FeedURL: "https://example.com/feed.json", Adapter: "jsonfeed-1.1", LastAttemptedAt: fixtureNow - 60, LastSuccessfulAt: fixtureNow - 60, Status: "ok", AttributionBasis: "source_declared"}},
		References: []Reference{{ID: id, SourceID: "fixture", ExternalID: "external/one", URL: "https://example.com/article", Title: "An external title", Excerpt: "Source excerpt <script>not executed</script> 雪", ExcerptAvailable: true, Authors: []Author{{Name: "External author", URL: "https://example.com/author"}}, SourcePublishedAt: "2026-09-04T12:00:00+02:00", SourcePublicationTimezoneKnown: true, FirstObservedAt: fixtureNow - 120, LastObservedAt: fixtureNow - 60, ContentHash: strings.Repeat("a", 64), UntrustedContent: true}}}
	return registryRaw, suppression, snapshot
}

func TestCanonicalAndReferenceIdentity(t *testing.T) {
	for _, test := range []struct{ source, external, expected string }{
		{"cuttle-worklogs", "building-6529-001", "f112f9011f18df257bf6c31f93a4e9be0290664616739192b65831b9de9c29ee"},
		{"fixture", "雪/\"\\\n\u2028", "7786d371fab6e20a271f6fc787be699b614c9cad4f669c896e4bde539677db43"},
		{"a", "bc", "1dedaeefe1ea581cb497882a3813f86b940d629afab6eeccd0825d4ef98fdbd3"},
		{"ab", "c", "524ed60abf3ce5a851a9e740b327edc13a8d61109a11d8e23152af1b3c3a0704"},
	} {
		got, err := ReferenceID(test.source, test.external)
		if err != nil || got != test.expected {
			t.Fatalf("ID = %q %v", got, err)
		}
	}
	raw := mustCanonical(t, []string{"swarmmemo.reference.v1", "fixture", "雪/\"\\\n\u2028"})
	if hex.EncodeToString(raw) != "5b22737761726d6d656d6f2e7265666572656e63652e7631222c2266697874757265222c22e99baa2f5c225c5c5c6e5c7532303238225d" {
		t.Fatal(string(raw))
	}
	blocked := mustCanonical(t, map[string]any{"version": 1, "state": "blocked"})
	if string(blocked) != `{"state":"blocked","version":1}` || hash(blocked) != "b8a746c4a18b03ad10e9d8400b4080f40bf6e6f3720be8862c3819a1864e0fac" {
		t.Fatal(string(blocked))
	}
	for _, value := range []any{1.0, -1, json.Number("-0"), json.Number("1e0"), uint64(maxSafeInteger) + 1, string([]byte{0xff})} {
		if _, err := Canonical(value); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("accepted invalid canonical value %T", value)
		}
	}
	for _, raw := range []string{`{"a":1,"a":2}`, `{"a":1,"\u0061":2}`, `"\ud800"`, `"\udfff"`, `"\ud800\u0041"`, `1e0`, `-0`, `NaN`, `9007199254740992`, `{} {}`, strings.Repeat("[", 40) + strings.Repeat("]", 40)} {
		if _, err := parseJSON([]byte(raw)); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("accepted malformed JSON %q", raw)
		}
	}
	value, err := parseJSON([]byte(`"\ud83d\ude00"`))
	if err != nil || value != "😀" {
		t.Fatal(value, err)
	}
	if string(mustCanonical(t, map[string]any{"text": "<>&/\u2028\u2029"})) != `{"text":"<>&/\u2028\u2029"}` {
		t.Fatal("encoder changed HTML/unicode profile")
	}
	for _, depth := range []int{32, 33} {
		raw := strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth)
		value, err := parseJSON([]byte(raw))
		if depth == 33 {
			if err == nil {
				t.Fatal("accepted excess JSON depth")
			}
		} else if err != nil || string(mustCanonical(t, value)) != raw {
			t.Fatal("canonical reflection changed JSON depth limit", err)
		}
	}
}

func TestValidEmptyMetadataOnlyAndRawRegistryBinding(t *testing.T) {
	registryRaw, suppression, snapshot := fixture(t)
	loaded, err := validate(registryRaw, suppression, mustCanonical(t, snapshot), fixtureNow)
	if err != nil || len(loaded.References) != 1 {
		t.Fatal(loaded, err)
	}
	if strings.Contains(string(mustCanonical(t, loaded)), "CANARY") {
		t.Fatal("operator evidence escaped projection")
	}
	if _, err := validate(append(registryRaw, '\n'), suppression, mustCanonical(t, snapshot), fixtureNow); err == nil {
		t.Fatal("registry whitespace change not detected")
	}
	for _, empty := range []bool{false, true} {
		parsed, err := parseJSON(registryRaw)
		if err != nil {
			t.Fatal("fixture")
		}
		reg := parsed.(map[string]any)
		policy := reg["sources"].([]any)[0].(map[string]any)
		policy["permissions"].(map[string]any)["full_text_storage"] = map[string]any{"approved": false}
		registryRaw = mustCanonical(t, reg)
		snapshot.RegistrySHA256 = hash(registryRaw)
		snapshot.References[0].Excerpt = ""
		snapshot.References[0].ExcerptAvailable = false
		if empty {
			snapshot.Sources = []Source{}
			snapshot.References = []Reference{}
		}
		if _, err := validate(registryRaw, suppression, mustCanonical(t, snapshot), fixtureNow); err != nil {
			t.Fatalf("empty=%v: %v", empty, err)
		}
	}
}

func TestProjectionRejectsMalformedOrUnauthorizedFields(t *testing.T) {
	cases := map[string]func(*Snapshot){
		"wrong state": func(s *Snapshot) { s.State = "blocked" }, "wrong registry": func(s *Snapshot) { s.RegistrySHA256 = strings.Repeat("0", 64) },
		"future": func(s *Snapshot) { s.GeneratedAt = fixtureNow + 61 }, "expired": func(s *Snapshot) { s.ValidUntil = fixtureNow },
		"overlong lifetime": func(s *Snapshot) { s.ValidUntil = s.GeneratedAt + 901 }, "source rebind": func(s *Snapshot) { s.Sources[0].FeedURL = "https://example.com/other" },
		"name rebind": func(s *Snapshot) { s.Sources[0].Name = "invented" }, "bad status": func(s *Snapshot) { s.Sources[0].Status = "private error" },
		"bad basis": func(s *Snapshot) { s.Sources[0].AttributionBasis = "verified" }, "stale source": func(s *Snapshot) { s.Sources[0].LastSuccessfulAt = fixtureNow - 86400 },
		"source chronology": func(s *Snapshot) { s.Sources[0].LastAttemptedAt = s.Sources[0].LastSuccessfulAt - 1 },
		"id mismatch":       func(s *Snapshot) { s.References[0].ID = strings.Repeat("f", 64) }, "unknown source": func(s *Snapshot) { s.References[0].SourceID = "absent" },
		"unsafe URL":     func(s *Snapshot) { s.References[0].URL = "https://example.com/w/lobby?text=bad" },
		"oversize title": func(s *Snapshot) { s.References[0].Title = strings.Repeat("a", 513) }, "excerpt codepoints": func(s *Snapshot) { s.References[0].Excerpt = strings.Repeat("a", 513) },
		"unavailable excerpt": func(s *Snapshot) { s.References[0].ExcerptAvailable = false }, "native flag": func(s *Snapshot) { s.References[0].NativeIdentity = true },
		"claimable flag": func(s *Snapshot) { s.References[0].ClaimableJob = true }, "HF flag": func(s *Snapshot) { s.References[0].HuggingFaceEligible = true },
		"trusted flag": func(s *Snapshot) { s.References[0].UntrustedContent = false }, "zero first observation": func(s *Snapshot) { s.References[0].FirstObservedAt = 0 },
		"future item": func(s *Snapshot) { s.References[0].LastObservedAt = fixtureNow }, "author query": func(s *Snapshot) { s.References[0].Authors[0].URL = "https://example.com/?token=secret" },
		"date-zone mismatch": func(s *Snapshot) { s.References[0].SourcePublicationTimezoneKnown = false },
		"invalid date":       func(s *Snapshot) { s.References[0].SourcePublishedAt = "2026-02-30T12:00:00Z" },
		"too many references": func(s *Snapshot) {
			for len(s.References) <= 1000 {
				s.References = append(s.References, s.References[0])
			}
		},
	}
	for name, modify := range cases {
		t.Run(name, func(t *testing.T) {
			reg, supp, s := fixture(t)
			modify(s)
			if _, err := validate(reg, supp, mustCanonical(t, s), fixtureNow); !errors.Is(err, ErrUnavailable) {
				t.Fatal("accepted", err)
			}
		})
	}
	reg, supp, s := fixture(t)
	raw := mustCanonical(t, s)
	for _, changed := range [][]byte{append(append([]byte{}, raw...), '\n'), bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":true`), 1), bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1e0`), 1), bytes.Replace(raw, []byte(`"state":"ready"`), []byte(`"state":"ready","unknown":"secret"`), 1), []byte(`{"state":"blocked","version":1}`)} {
		if _, err := validate(reg, supp, changed, fixtureNow); err == nil {
			t.Fatal("accepted noncanonical/blocked/unknown projection")
		}
	}
}

func TestCurrentPolicySuppressionAndOrdering(t *testing.T) {
	for _, mode := range []string{"disabled", "removed", "storage denied", "collection denied", "public denied", "expired", "future approval", "bad UTC", "bool version"} {
		t.Run(mode, func(t *testing.T) {
			reg, supp, s := fixture(t)
			parsed, err := parseJSON(reg)
			if err != nil {
				t.Fatal(err)
			}
			value := parsed.(map[string]any)
			source := value["sources"].([]any)[0].(map[string]any)
			permissions := source["permissions"].(map[string]any)
			switch mode {
			case "disabled":
				source["enabled"] = false
			case "removed":
				value["sources"] = []any{}
			case "storage denied":
				permissions["full_text_storage"] = map[string]any{"approved": false}
			case "collection denied":
				permissions["automated_collection"] = map[string]any{"approved": false}
			case "public denied":
				permissions["public_archive"] = map[string]any{"approved": false}
			case "expired":
				permissions["public_archive"].(map[string]any)["expires_at"] = "2026-09-05T00:00:00Z"
			case "future approval":
				permissions["public_archive"].(map[string]any)["reviewed_at"] = "2026-09-05T12:00:00Z"
			case "bad UTC":
				permissions["public_archive"].(map[string]any)["expires_at"] = "2026-09-06T00:00:00+00:00"
			case "bool version":
				value["version"] = true
			}
			reg = mustCanonical(t, value)
			s.RegistrySHA256 = hash(reg)
			if _, err := validate(reg, supp, mustCanonical(t, s), fixtureNow); err == nil {
				t.Fatal("accepted policy")
			}
		})
	}
	reg, supp, s := fixture(t)
	supp = mustCanonical(t, map[string]any{"version": 1, "ids": []string{s.References[0].ID}})
	s.SuppressionSHA256 = hash(supp)
	if _, err := validate(reg, supp, mustCanonical(t, s), fixtureNow); err == nil {
		t.Fatal("suppressed reference returned")
	}
	for _, value := range []any{map[string]any{"version": 1, "ids": []string{strings.Repeat("a", 64), strings.Repeat("a", 64)}}, map[string]any{"version": 1, "ids": []string{strings.Repeat("b", 64), strings.Repeat("a", 64)}}, map[string]any{"version": 1, "ids": []string{}, "reason": "secret"}} {
		if _, err := suppressions(mustCanonical(t, value)); err == nil {
			t.Fatal("bad suppression")
		}
	}
	reg, supp, s = fixture(t)
	second := s.References[0]
	second.ExternalID = "two"
	second.ID, _ = ReferenceID("fixture", "two")
	s.References = append(s.References, second)
	sort.Slice(s.References, func(i, j int) bool { return s.References[i].ID < s.References[j].ID })
	if _, err := validate(reg, supp, mustCanonical(t, s), fixtureNow); err != nil {
		t.Fatal(err)
	}
	s.References[0], s.References[1] = s.References[1], s.References[0]
	if _, err := validate(reg, supp, mustCanonical(t, s), fixtureNow); err == nil {
		t.Fatal("unsorted references")
	}
}

func TestURLAndPublicationDateProfile(t *testing.T) {
	for _, raw := range []string{"https://192.0.0.9/", "https://192.0.0.10/", "https://[2001:1::1]/", "https://[2001:3::1]/", "https://[2001:4:112::1]/", "https://[2001:20::1]/", "https://[2001:30::1]/", "https://[::ffff:8.8.8.8]/"} {
		if !allowedURL(raw, true) {
			t.Fatalf("public IP exception rejected: %s", raw)
		}
	}
	for _, raw := range []string{"https://[100::1]/", "https://[2001::1]/", "https://[2002::1]/", "https://[64:ff9b:1::1]/", "https://[3fff::1]/", "https://[fec0::1]/", "https://192.0.0.8/", "https://example.com/%0a", "https://example.com/%09", "https://example.com/%7f"} {
		if allowedURL(raw, true) {
			t.Fatalf("nonpublic IP or decoded control accepted: %s", raw)
		}
	}
	for _, host := range []string{"127.1", "2130706433", "0x7f000001", "017700000001", "0x7f.1", "1.2.3.999"} {
		if allowedURL("https://"+host+"/", true) {
			t.Fatalf("browser-numeric IPv4 alias accepted: %s", host)
		}
	}
	for _, raw := range []string{"https://example.com/post", "https://8.8.8.8/article", "https://example.com:443/post"} {
		if !allowedURL(raw, true) {
			t.Fatal(raw)
		}
	}
	for _, raw := range []string{"http://example.com/", "https://example.com?", "https://example.com#", "https://@example.com", "https://user:secret@example.com", "https://localhost/", "https://10.1.2.3/", "https://127.0.0.1/", "https://[::1]/", "https://[::ffff:127.0.0.1]/", "https://169.254.169.254/", "https://100.64.1.1/", "https://example.com/w/x", "https://example.com/%63%36%34/x", "https://example.com/a/%2e%2e/b", "https://example.com/a\\b", "https://example.com:80/x", "https://éxample.com/"} {
		if allowedURL(raw, true) {
			t.Fatalf("accepted %q", raw)
		}
	}
	for _, raw := range []string{"2026-09-05T12:00:00Z", "2026-09-05T12:00:00.123456789+23:59"} {
		if !publicationTime(raw, true, "jsonfeed-1.1") {
			t.Fatal(raw)
		}
	}
	for _, raw := range []string{"2026-09-05T12:00:00+24:00", "2026-09-05T12:00:00+00:60", "2026-09-05T12:00:00.1234567890Z", "0000-01-01T00:00:00Z", "2026-09-05t12:00:00z"} {
		if publicationTime(raw, true, "jsonfeed-1.1") {
			t.Fatal(raw)
		}
	}
	if !publicationTime("2026-09-05 12:00:00", false, cuttleAdapter) || publicationTime("2026-09-05 12:00:00", false, "jsonfeed-1.1") || !publicationTime("", false, "jsonfeed-1.1") {
		t.Fatal("timezone provenance")
	}
}

func fileFixture(t *testing.T) (*Reader, Config, *int64) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	reg, supp, s := fixture(t)
	config := Config{RegistryPath: filepath.Join(directory, "registry.json"), SuppressionPath: filepath.Join(directory, "suppression.json"), SnapshotPath: filepath.Join(directory, "projection.json"), OwnerUID: uint32(os.Getuid())}
	for path, raw := range map[string][]byte{config.RegistryPath: reg, config.SuppressionPath: supp, config.SnapshotPath: mustCanonical(t, s)} {
		if err := os.WriteFile(path, raw, 0640); err != nil {
			t.Fatal(err)
		}
	}
	r, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	now := int64(fixtureNow)
	r.now = func() time.Time { return time.Unix(now, 0) }
	return r, config, &now
}

func TestLoadAndFinalFence(t *testing.T) {
	for _, mode := range []string{"unchanged", "snapshot mutated", "snapshot blocked", "policy bytes changed", "suppression changed", "expired during render", "foreign reader", "forged digest"} {
		t.Run(mode, func(t *testing.T) {
			r, config, now := fileFixture(t)
			s, err := r.Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "snapshot mutated":
				s.References[0].Title = "changed after admission"
			case "snapshot blocked":
				os.WriteFile(config.SnapshotPath, []byte(`{"state":"blocked","version":1}`), 0640)
			case "policy bytes changed":
				raw, _ := os.ReadFile(config.RegistryPath)
				os.WriteFile(config.RegistryPath, append(raw, '\n'), 0640)
			case "suppression changed":
				os.WriteFile(config.SuppressionPath, mustCanonical(t, map[string]any{"version": 1, "ids": []string{strings.Repeat("0", 64)}}), 0640)
			case "expired during render":
				*now = s.ValidUntil
			case "foreign reader":
				r, _ = New(config)
				r.now = func() time.Time { return time.Unix(*now, 0) }
			case "forged digest":
				s.Digest = strings.Repeat("0", 64)
			}
			err = r.Fence(context.Background(), s)
			if mode == "unchanged" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrUnavailable) {
				t.Fatal("fence accepted", err)
			}
		})
	}
}

func TestConfigurationAndCanceledReads(t *testing.T) {
	if r, err := New(Config{}); r != nil || err != nil {
		t.Fatal(r, err)
	}
	if _, err := New(Config{RegistryPath: "/one"}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	r, config, _ := fileFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Load(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	config.RegistryPath = filepath.Join(t.TempDir(), "absent", "registry.json")
	r, err := New(config)
	if err != nil {
		t.Fatal("missing files broke startup", err)
	}
	if _, err = r.Load(context.Background()); !errors.Is(err, ErrUnavailable) || err.Error() != "references_unavailable" {
		t.Fatal(err)
	}
}

func TestExpiryDuringValidationAndFinalFence(t *testing.T) {
	r, _, _ := fileFixture(t)
	clockCalls := 0
	r.now = func() time.Time {
		clockCalls++
		if clockCalls == 1 {
			return time.Unix(fixtureNow+899, 0)
		}
		return time.Unix(fixtureNow+900, 0)
	}
	if _, err := r.Load(context.Background()); !errors.Is(err, ErrUnavailable) || clockCalls != 2 {
		t.Fatal("expiry during validation accepted", err, clockCalls)
	}
	r, _, _ = fileFixture(t)
	snapshot, err := r.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	clockCalls = 0
	r.now = func() time.Time {
		clockCalls++
		if clockCalls < 3 {
			return time.Unix(fixtureNow+899, 0)
		}
		return time.Unix(fixtureNow+900, 0)
	}
	if err := r.Fence(context.Background(), snapshot); !errors.Is(err, ErrUnavailable) || clockCalls != 3 {
		t.Fatal("expiry at final fence accepted", err, clockCalls)
	}
}
