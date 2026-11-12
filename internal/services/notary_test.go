package services_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

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
	// C697: seq and hash sit on the result too, beside the receipt.
	if get(out, "result", "seq") != float64(1) || get(out, "result", "hash") != hash {
		t.Fatalf("seq and hash must be on the result: %+v", out)
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
		// C446: the same signature bytes spelled another way never verify.
		func(x *services.NotaryReceipt) { x.Signature = trailingBitsTwin(x.Signature) },
		func(x *services.NotaryReceipt) { x.Signature = x.Signature[:40] + "\n" + x.Signature[40:] },
		func(x *services.NotaryReceipt) { x.Signature += "==" },
	} {
		tampered := rec
		bad(&tampered)
		if services.VerifyNotaryReceipt(public, tampered) {
			t.Fatalf("a tampered receipt verified: %+v", tampered)
		}
	}

	if services.VerifyNotaryReceipt(trailingBitsTwin(public), rec) {
		t.Fatal("a non-canonical spelling of the notary key verified")
	}

	// Publicly readable by hash, by anyone, unsigned.
	back, err := r.read(anon, "notary", "get", map[string]any{"hash": hash})
	if err != nil || receiptOf(t, get(back, "result", "receipt")) != rec {
		t.Fatalf("read back: %+v %v", back, err)
	}
	if get(back, "result", "seq") != float64(1) || get(back, "result", "hash") != hash {
		t.Fatalf("notary.get must carry seq and hash on the result: %+v", back)
	}
	// C926: the stamp and get answers link the key and carry the key read's
	// verify rule, beside the receipt, never inside the signed bytes.
	for _, ans := range []map[string]any{out, back} {
		if get(ans, "result", "key") != "/api/notary/key" || get(ans, "result", "verify") != get(key, "result", "verify") || get(ans, "result", "verify") != services.ReceiptVerifyRule {
			t.Fatalf("a receipt answer must link the key and carry the verify rule: %+v", ans)
		}
	}
	if strings.Contains(rec.Payload, "verify") || strings.Contains(rec.Payload, "/api/notary/key") {
		t.Fatalf("key and verify stay out of the signed payload: %s", rec.Payload)
	}
	if _, err = r.read(anon, "notary", "get", map[string]any{"hash": strings.Repeat("0", 64)}); code(err) != "notary_not_found" {
		t.Fatalf("unknown hash: %v", err)
	}
	// The first receipt for a hash stands: a later stamp returns it, for the
	// least a write costs (security review 1.20, M6).
	r.now += 100
	again := r.mustCall("bob", "notary", "stamp", map[string]any{"hash": hash}, 1)
	if receiptOf(t, get(again, "result", "receipt")) != rec || get(again, "result", "duplicate") != true || !strings.Contains(get(again, "result", "note").(string), "first stamp") || get(again, "call", "cost") != float64(1) {
		t.Fatalf("duplicate stamp: %+v", again)
	}
	// It says plainly when the text was first stamped, and what that proves
	// (kosoyy C552: the docs' own example read as a clock bug).
	first := time.Unix(int64(get(again, "result", "receipt", "time").(float64)), 0).UTC().Format(time.RFC3339)
	if note, _ := get(again, "result", "note").(string); get(again, "result", "first_stamped_at") != first ||
		!strings.Contains(note, "first stamped at "+first) || !strings.Contains(note, "existed by then") {
		t.Fatalf("duplicate stamp does not say when it was first stamped: %+v", again)
	}
	// Text is hashed as exact UTF-8 bytes and never stored.
	text := "résumé v2\n"
	out = r.mustCall("alice", "notary", "stamp", map[string]any{"text": text}, 1)
	if get(out, "result", "duplicate") != false || get(out, "result", "first_stamped_at") != nil {
		t.Fatalf("a new stamp names a first stamp: %+v", out)
	}
	// C750: the result says how many bytes of text were stamped, so a text
	// cut short by an unencoded & on the GET form shows; a hash has none.
	if get(out, "result", "bytes") != float64(len(text)) || get(again, "result", "bytes") != nil {
		t.Fatalf("result.bytes: text %+v, hash %+v", get(out, "result", "bytes"), get(again, "result", "bytes"))
	}
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
		_, err := r.call("alice", "notary", "stamp", bad, 1)
		if code(err) != "invalid_service_data" {
			t.Errorf("%v must be invalid_service_data: %v", bad, err)
		}
		// C389: a bad or missing hash names the argument, not only the code.
		if _, long := bad["text"].(string); !long && bad["extra"] == nil {
			if e := (*allowance.Err)(nil); !errors.As(err, &e) || e.Message == "" {
				t.Errorf("%v: refusal without a field message: %v", bad, err)
			}
		}
	}
	// notary get names the hash rule too (GET /api/notary/00 answered the
	// generic service.call text).
	if _, err := r.read(anon, "notary", "get", map[string]any{"hash": "00"}); code(err) != "invalid_service_data" {
		t.Errorf("get 00: %v", err)
	} else if e := (*allowance.Err)(nil); !errors.As(err, &e) || !strings.Contains(e.Message, "64 hex digits, either case") {
		t.Errorf("get 00: refusal without the hash rule: %v", err)
	}
	if _, err := r.call("alice", "notary", "stamp", map[string]any{"text": strings.Repeat("\x01", services.NotaryTextBytes)}, 1); err != nil {
		t.Fatalf("a full text of control characters fits: %v", err)
	}
	if _, err := r.call("alice", "notary", "stamp", map[string]any{"hash": strings.Repeat("ef", 32)}, 0); code(err) != "price_exceeds_max" {
		t.Fatalf("priced through the catalogue: %v", err)
	}
}

func TestNotaryDailyCap(t *testing.T) {
	if testing.Short() {
		t.Skip("stamps a whole day's notary allowance")
	}
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

// trailingBitsTwin is canonical base64url s with its last character's unused
// bits set: the same bytes to a lax decoder, refused by the one rule (C446).
func trailingBitsTwin(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	return s[:len(s)-1] + string(alphabet[strings.IndexByte(alphabet, s[len(s)-1])|1])
}

// TestNotaryGetByPrefix: notary.get reads a receipt by a hash prefix of 8 to
// 63 hex digits, either case, when exactly one stamped hash starts with it
// (C912: an outside relay cited a receipt by 8 hex).
func TestNotaryGetByPrefix(t *testing.T) {
	r := newWakeRig(t)
	anon := allowance.Subject{ID: "anon:x"}
	unique := "0123abcd" + strings.Repeat("1", 56)
	twinA := "deadbeef" + strings.Repeat("a", 56)
	twinB := "deadbeef" + strings.Repeat("b", 56)
	for _, h := range []string{unique, twinA, twinB} {
		r.mustCall("alice", "notary", "stamp", map[string]any{"hash": h}, 1)
	}
	full, err := r.read(anon, "notary", "get", map[string]any{"hash": unique})
	if err != nil || get(full, "result", "hash") != unique {
		t.Fatalf("full hash: %+v %v", full, err)
	}
	for _, p := range []string{unique[:8], "0123ABCD", unique[:63], strings.ToUpper(unique[:20])} {
		out, err := r.read(anon, "notary", "get", map[string]any{"hash": p})
		if err != nil {
			t.Fatalf("prefix %s: %v", p, err)
		}
		if got, want := receiptOf(t, get(out, "result", "receipt")), receiptOf(t, get(full, "result", "receipt")); got != want || get(out, "result", "hash") != unique {
			t.Fatalf("prefix %s answered %+v, not the full hash's receipt", p, out)
		}
	}
	// With seq too, the seq's receipt must stamp a hash with the prefix.
	if out, err := r.read(anon, "notary", "get", map[string]any{"hash": unique[:8], "seq": 1}); err != nil || get(out, "result", "hash") != unique {
		t.Fatalf("prefix with its seq: %+v %v", out, err)
	}
	if _, err := r.read(anon, "notary", "get", map[string]any{"hash": unique[:8], "seq": 2}); code(err) != "notary_not_found" {
		t.Fatalf("prefix with another hash's seq: %v", err)
	}
	// No stamped hash starts with it: the full hash's not-found.
	if _, err := r.read(anon, "notary", "get", map[string]any{"hash": "99999999"}); code(err) != "notary_not_found" {
		t.Fatalf("no match: %v", err)
	}
	// Two do: notary_ambiguous naming both full hashes.
	_, err = r.read(anon, "notary", "get", map[string]any{"hash": "DEADBEEF"})
	var e *allowance.Err
	if !errors.As(err, &e) || e.Code != "notary_ambiguous" {
		t.Fatalf("ambiguous: %v", err)
	}
	if d, ok := e.Details.(services.NotaryAmbiguous); !ok || d.Prefix != "deadbeef" || len(d.Candidates) != 2 || d.Candidates[0] != twinA || d.Candidates[1] != twinB || d.More {
		t.Fatalf("ambiguous details: %+v", e.Details)
	}
	if out, err := r.read(anon, "notary", "get", map[string]any{"hash": twinB[:9]}); err != nil || get(out, "result", "hash") != twinB {
		t.Fatalf("one more character resolves: %+v %v", out, err)
	}
	// Too short or not hex: the hash rule, which names the prefix form.
	for _, bad := range []string{unique[:7], "0123abcz", "deadbeef-"} {
		_, err := r.read(anon, "notary", "get", map[string]any{"hash": bad})
		if e := (*allowance.Err)(nil); code(err) != "invalid_service_data" || !errors.As(err, &e) || !strings.Contains(e.Message, "prefix of one of at least 8 hex digits") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// C934: hex digits in either case, a full hash as a prefix.
	if out, err := r.read(anon, "notary", "get", map[string]any{"hash": strings.ToUpper(unique)}); err != nil || get(out, "result", "hash") != unique {
		t.Errorf("uppercase full hash: %+v %v", out, err)
	}
}
