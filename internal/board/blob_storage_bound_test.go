package board

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// Files no longer expire, so the daily allowance is the only bound on stored
// file bytes. Many small files, identical re-uploads, fresh keys (Sybil) and
// long or absent ttls must all be charged in full: a day's stored file bytes
// never exceed the global daily allowance, and one key never exceeds its own.
func TestFileStorageStaysWithinDailyAllowance(t *testing.T) {
	const perKey, global = 16 << 10, 64 << 10
	s := openTest(t, Config{DailyBytes: perKey, GlobalDailyBytes: global})
	owner := keyFor(90)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "files", Visibility: "public"}))
	same := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 1000)))
	refused := func(err error) string {
		var e *Error
		if errors.As(err, &e) {
			return e.Code
		}
		t.Fatalf("unexpected error %v", err)
		return ""
	}
	globalRefusals := 0
	for n := byte(0); n < 40; n++ { // 40 fresh keys, each uploading until refused
		key := keyFor(100 + n)
		stored := int64(0)
		for i := 0; ; i++ {
			ttl := int64(0)
			if i%2 == 1 {
				ttl = AttachmentMaxTTL
			}
			_, err := s.Execute(testContext, signed(key, Command{Operation: "blob.put", Room: "files", Data: same, Filename: "f", MediaType: "text/plain", TTL: ttl}), "origin")
			if err != nil {
				if refused(err) == "global_quota_exhausted" {
					globalRefusals++
				}
				break
			}
			stored += 1000
			if i > 100 {
				t.Fatal("per-key allowance never refused an upload")
			}
		}
		if stored > perKey {
			t.Fatalf("key %d stored %d file bytes, over its %d allowance", n, stored, perKey)
		}
	}
	var total int64
	if err := s.db.QueryRow("SELECT coalesce(sum(size),0) FROM blobs").Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total > global || globalRefusals == 0 {
		t.Fatalf("stored %d file bytes against a %d global allowance (%d global refusals)", total, global, globalRefusals)
	}
}
