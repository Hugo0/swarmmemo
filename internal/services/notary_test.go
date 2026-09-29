package services_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
)

func receiptOf(t *testing.T, v any) services.NotaryReceipt {
	t.Helper()
	raw, _ := json.Marshal(v)
	var r services.NotaryReceipt
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// flipFirst changes the first character of a base64url string.
func flipFirst(s string) string {
	if s[0] == 'A' {
		return "B" + s[1:]
	}
	return "A" + s[1:]
}

func TestNotaryStampVerifyAndReadBack(t *testing.T) {
	r := newWakeRig(t)
	anon := allowance.Subject{ID: "anon:x"}
	key, err := r.read(anon, "notary", "key", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	public, _ := get(key, "result", "public_key").(string)
	if len(public) != 43 || get(key, "result", "algorithm") != "ed25519" || get(key, "result", "service_id") != "swarmmemo.com" {
		t.Fatalf("key: %+v", key)
	}

	hash := strings.Repeat("ab", 32)
	out := r.mustCall("alice", "notary", "stamp", map[string]any{"hash": hash}, 1)
	rec := receiptOf(t, get(out, "result", "receipt"))
	if rec.Hash != hash || rec.Time != wakeT0 || rec.Seq != 1 || rec.ServiceID != "swarmmemo.com" || rec.PublicKey != public || get(out, "call", "cost") != float64(1) {
		t.Fatalf("receipt: %+v", out)
	}
	if !services.VerifyNotaryReceipt(public, rec) {
		t.Fatalf("the receipt must verify offline: %+v", rec)
	}
	if !strings.Contains(rec.Payload, `"hash":"`+hash+`"`) || strings.Contains(rec.Payload, "alice") {
		t.Fatalf("payload names the hash, never the caller: %s", rec.Payload)
	}
	// Tampering with any field breaks verification.
	for _, bad := range []func(*services.NotaryReceipt){
		func(x *services.NotaryReceipt) { x.Time++ },
		func(x *services.NotaryReceipt) { x.Hash = strings.Repeat("cd", 32) },
		func(x *services.NotaryReceipt) { x.Payload = strings.Replace(x.Payload, `"seq":1`, `"seq":2`, 1) },
		func(x *services.NotaryReceipt) { x.Payload = strings.Replace(x.Payload, `,"seq"`, `, "seq"`, 1) },
		func(x *services.NotaryReceipt) { x.Signature = flipFirst(x.Signature) },
		func(x *services.NotaryReceipt) { x.KeyID = strings.Repeat("0", 64) },
	} {
		tampered := rec
		bad(&tampered)
		if services.VerifyNotaryReceipt(public, tampered) {
			t.Fatalf("a tampered receipt verified: %+v", tampered)
		}
	}

	// Publicly readable by hash, by anyone, unsigned.
	back, err := r.read(anon, "notary", "get", map[string]any{"hash": hash})
	if err != nil || receiptOf(t, get(back, "result", "receipt")) != rec {
		t.Fatalf("read back: %+v %v", back, err)
	}
	if _, err = r.read(anon, "notary", "get", map[string]any{"hash": strings.Repeat("0", 64)}); code(err) != "notary_not_found" {
		t.Fatalf("unknown hash: %v", err)
	}
	// The first receipt for a hash stands: a later stamp returns it, for the
	// least a write costs (security review 1.20, M6).
	r.now += 100
	again := r.mustCall("bob", "notary", "stamp", map[string]any{"hash": hash}, 1)
	if receiptOf(t, get(again, "result", "receipt")) != rec || get(again, "result", "duplicate") != true || get(again, "call", "cost") != float64(1) {
		t.Fatalf("duplicate stamp: %+v", again)
	}
	// Text is hashed as exact UTF-8 bytes and never stored.
	text := "résumé v2\n"
	out = r.mustCall("alice", "notary", "stamp", map[string]any{"text": text}, 1)
	sum := sha256.Sum256([]byte(text))
	textRec := receiptOf(t, get(out, "result", "receipt"))
	if textRec.Hash != hex.EncodeToString(sum[:]) || textRec.Seq != 2 || !services.VerifyNotaryReceipt(public, textRec) {
		t.Fatalf("text stamp: %+v", textRec)
	}
	var stored int
	if err = r.db.QueryRow("SELECT count(*) FROM notary_receipts WHERE payload LIKE '%résumé%'").Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("text must not be stored: %d %v", stored, err)
	}
	var public1 string
	if err = r.db.QueryRow("SELECT public FROM service_calls WHERE service='notary' LIMIT 1").Scan(&public1); err != nil || public1 != "{}" {
		t.Fatalf("the public call record shows nothing: %q %v", public1, err)
	}
	// The key survives a restart.
	r.e = r.open()
	if key2, _ := r.read(anon, "notary", "key", map[string]any{}); get(key2, "result", "public_key") != public {
		t.Fatalf("the notary key changed: %+v", key2)
	}

	for _, bad := range []map[string]any{
		{"hash": strings.ToUpper(hash)},
		{"hash": hash[:63]},
		{"hash": hash, "text": "x"},
		{},
		{"text": strings.Repeat("x", services.NotaryTextBytes+1)},
		{"text": nil},
		{"hash": hash, "extra": 1},
	} {
		if _, err := r.call("alice", "notary", "stamp", bad, 1); code(err) != "invalid_service_data" {
			t.Errorf("%v must be invalid_service_data: %v", bad, err)
		}
	}
	if _, err := r.call("alice", "notary", "stamp", map[string]any{"text": strings.Repeat("\x01", services.NotaryTextBytes)}, 1); err != nil {
		t.Fatalf("a full text of control characters fits: %v", err)
	}
	if _, err := r.call("alice", "notary", "stamp", map[string]any{"hash": strings.Repeat("ef", 32)}, 0); code(err) != "price_exceeds_max" {
		t.Fatalf("priced through the catalogue: %v", err)
	}
}

func TestNotaryDailyCap(t *testing.T) {
	r := newWakeRig(t)
	for i := 0; i < services.NotaryPerAccountDay; i++ {
		sum := sha256.Sum256([]byte{byte(i), byte(i >> 8)})
		r.mustCall("alice", "notary", "stamp", map[string]any{"hash": hex.EncodeToString(sum[:])}, 1)
	}
	if _, err := r.call("alice", "notary", "stamp", map[string]any{"text": "one more"}, 1); code(err) != "notary_limit" {
		t.Fatalf("daily cap: %v", err)
	}
	r.mustCall("bob", "notary", "stamp", map[string]any{"text": "one more"}, 1)
	r.now += 86400
	r.mustCall("alice", "notary", "stamp", map[string]any{"text": "next day"}, 1)
}

func FuzzNotaryArgs(f *testing.F) {
	for _, s := range []string{
		`{"hash":"` + strings.Repeat("a", 64) + `"}`, `{"text":"hello"}`, `{"text":""}`, `{"text":null}`,
		`{"hash":"` + strings.Repeat("A", 64) + `"}`, `{"hash":"x","text":"y"}`, `{"text":"\u0000"}`, `{}`, `[]`,
	} {
		f.Add(s)
	}
	hexRE := regexp.MustCompile(`^[0-9a-f]{64}$`)
	f.Fuzz(func(t *testing.T, raw string) {
		hash, err := services.ParseStampForTest(json.RawMessage(raw))
		if err != nil {
			if code(err) != "invalid_service_data" {
				t.Fatalf("refusal must be invalid_service_data: %v", err)
			}
			return
		}
		if !hexRE.MatchString(hash) || !json.Valid([]byte(raw)) {
			t.Fatalf("accepted %q as %q", raw, hash)
		}
		var a struct {
			Hash string  `json:"hash"`
			Text *string `json:"text"`
		}
		if err = json.Unmarshal([]byte(raw), &a); err != nil {
			t.Fatal(err)
		}
		if a.Text != nil {
			sum := sha256.Sum256([]byte(*a.Text))
			if len(*a.Text) > services.NotaryTextBytes || hash != hex.EncodeToString(sum[:]) || a.Hash != "" {
				t.Fatalf("text %q hashed to %q", raw, hash)
			}
		} else if hash != a.Hash {
			t.Fatalf("hash %q returned %q", raw, hash)
		}
	})
}
