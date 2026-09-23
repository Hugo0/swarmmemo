package nostr

import (
	"bytes"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

func TestBIP340Vectors(t *testing.T) {
	f, err := os.Open("testdata/bip340-vectors.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.Comment = '#'
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	signed, verified := 0, 0
	for _, row := range rows[1:] {
		name, secret, public, aux, message, signature, want := row[0], row[1], row[2], row[3], row[4], row[5], row[6] == "TRUE"
		key, _ := hex.DecodeString(public)
		msg, _ := hex.DecodeString(message)
		sig, _ := hex.DecodeString(signature)
		if got := VerifySignature(key, msg, sig); got != want {
			t.Errorf("row %s: verify = %v, want %v", name, got, want)
		}
		verified++
		if secret == "" {
			continue
		}
		k, err := KeyFromHex(strings.ToLower(secret))
		if err != nil {
			t.Fatalf("row %s: %v", name, err)
		}
		if k.PublicHex() != strings.ToLower(public) {
			t.Errorf("row %s: public key %s, want %s", name, k.PublicHex(), public)
		}
		var auxBytes [32]byte
		raw, _ := hex.DecodeString(aux)
		copy(auxBytes[:], raw)
		s, err := schnorr.Sign(k.priv, msg, schnorr.CustomNonce(auxBytes))
		if err != nil {
			t.Fatalf("row %s: %v", name, err)
		}
		if got := strings.ToUpper(hex.EncodeToString(s.Serialize())); got != signature {
			t.Errorf("row %s: signature %s, want %s", name, got, signature)
		}
		signed++
	}
	if signed < 4 || verified < 13 {
		t.Fatalf("too few vectors: %d signed, %d verified", signed, verified)
	}
}

// The ids come from Python's JSON encoder, an independent implementation of
// the escaping NIP-01 specifies (testdata/nip01-ids.json).
func TestNIP01EventIDs(t *testing.T) {
	raw, err := os.ReadFile("testdata/nip01-ids.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Event
		Serialized string `json:"serialized"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 5 {
		t.Fatal("too few vectors")
	}
	for i, c := range cases {
		if got := string(Serialize(c.Event)); got != c.Serialized {
			t.Errorf("case %d: serialized\n%s\nwant\n%s", i, got, c.Serialized)
		}
		if got := ComputeID(c.Event); got != c.ID {
			t.Errorf("case %d: id %s, want %s", i, got, c.ID)
		}
	}
}

func testKey(t *testing.T) Key {
	t.Helper()
	k, err := KeyFromHex("b7e151628aed2a6abf7158809cf4f3c762e7160f38b4da56a784d9045190cfef")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSignVerifyAndTamper(t *testing.T) {
	k := testKey(t)
	e := Event{CreatedAt: 1727000000, Kind: 1, Tags: [][]string{{"t", "swarmmemo"}}, Content: "hello \"world\"\n"}
	if err := k.Sign(&e); err != nil {
		t.Fatal(err)
	}
	if err := Verify(e); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(e)
	parsed, err := ParseEvent(raw)
	if err != nil || Verify(parsed) != nil {
		t.Fatalf("round trip: %v", err)
	}
	changed := e
	changed.Content = "hello"
	if Verify(changed) == nil {
		t.Fatal("changed content verified")
	}
	changed.ID = ComputeID(changed) // a matching id, but the signature is over the old one
	if Verify(changed) == nil {
		t.Fatal("re-hashed event verified under the old signature")
	}
	other, _ := GenerateKey()
	stolen := e
	stolen.PubKey = other.PublicHex()
	stolen.ID = ComputeID(stolen)
	if Verify(stolen) == nil {
		t.Fatal("signature verified under another key")
	}
}

func TestParseEventRefusesMalformed(t *testing.T) {
	k := testKey(t)
	good := Event{CreatedAt: 1727000000, Kind: 1, Tags: [][]string{}, Content: "x"}
	if err := k.Sign(&good); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(good)
	base := string(raw)
	if _, err := ParseEvent(raw); err != nil {
		t.Fatal(err)
	}
	many := make([][]string, MaxTags+1)
	for i := range many {
		many[i] = []string{"t", "x"}
	}
	tooMany := good
	tooMany.Tags = many
	manyRaw, _ := json.Marshal(tooMany)
	wide := good
	wide.Tags = [][]string{make([]string, MaxTagValues+1)}
	wideRaw, _ := json.Marshal(wide)
	cases := map[string]string{
		"unknown field":   strings.Replace(base, `{"id"`, `{"extra":1,"id"`, 1),
		"trailing data":   base + " {}",
		"two objects":     base + base,
		"uppercase id":    strings.Replace(base, good.ID, strings.ToUpper(good.ID), 1),
		"short sig":       strings.Replace(base, good.Sig, good.Sig[:126], 1),
		"null tags":       strings.Replace(base, `"tags":[]`, `"tags":null`, 1),
		"missing tags":    strings.Replace(base, `"tags":[],`, ``, 1),
		"empty tag":       strings.Replace(base, `"tags":[]`, `"tags":[[]]`, 1),
		"negative time":   strings.Replace(base, `"created_at":1727000000`, `"created_at":-1`, 1),
		"float kind":      strings.Replace(base, `"kind":1`, `"kind":1.5`, 1),
		"huge kind":       strings.Replace(base, `"kind":1`, `"kind":70000`, 1),
		"not an object":   `["EVENT"]`,
		"invalid utf8":    strings.Replace(base, `"content":"x"`, "\"content\":\"\xff\"", 1),
		"too many tags":   string(manyRaw),
		"too many values": string(wideRaw),
		"oversize":        strings.Replace(base, `"content":"x"`, `"content":"`+strings.Repeat("a", MaxEventBytes)+`"`, 1),
	}
	for name, input := range cases {
		if input == base {
			t.Fatalf("%s: case did not change the event", name)
		}
		if _, err := ParseEvent([]byte(input)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNIP19(t *testing.T) {
	// The NIP-19 specification's own example.
	const hexKey = "7e7e9c42a91bfef19fa929e5fda1b72e0ebc1a4c1141673e2794234d86addf4e"
	const npub = "npub10elfcs4fr0l0r8af98jlmgdh9c8tcxjvz9qkw038js35mp4dma8qzvjptg"
	if got := Npub(hexKey); got != npub {
		t.Fatalf("npub %s, want %s", got, npub)
	}
	if got, err := DecodeNpub(npub); err != nil || got != hexKey {
		t.Fatalf("decode %s %v", got, err)
	}
	if _, err := DecodeNpub(npub[:len(npub)-1] + "q"); err == nil {
		t.Fatal("bad checksum accepted")
	}
	id := strings.Repeat("ab", 32)
	ne := NEvent(id, hexKey, 1)
	if !strings.HasPrefix(ne, "nevent1") {
		t.Fatal(ne)
	}
	gotID, gotAuthor, gotKind, err := DecodeNEvent(ne)
	if err != nil || gotID != id || gotAuthor != hexKey || gotKind != 1 {
		t.Fatalf("nevent round trip: %s %s %d %v", gotID, gotAuthor, gotKind, err)
	}
	if Npub("zz") != "" || NEvent("zz", hexKey, 1) != "" {
		t.Fatal("encoded non-hex")
	}
}

func TestKeyRange(t *testing.T) {
	for _, bad := range []string{
		strings.Repeat("0", 64),
		"fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141", // the curve order
		strings.Repeat("f", 64),
		"abcd",
		strings.Repeat("g", 64),
	} {
		if _, err := KeyFromHex(bad); err == nil {
			t.Errorf("accepted secret key %s", bad)
		}
	}
}

func TestKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.key")
	npub, err := WriteKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode %v", info.Mode())
	}
	k, err := LoadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if Npub(k.PublicHex()) != npub {
		t.Fatal("loaded key does not match the printed npub")
	}
	if _, err := WriteKeyFile(path); err == nil {
		t.Fatal("overwrote an existing key file")
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyFile(path); err == nil {
		t.Fatal("loaded a group-readable key file")
	}
	var kf keyFile
	raw, _ := os.ReadFile(path)
	_ = json.Unmarshal(raw, &kf)
	other, _ := GenerateKey()
	kf.PublicKey = other.PublicHex()
	raw, _ = json.Marshal(kf)
	mismatched := filepath.Join(dir, "mismatched.key")
	if err := os.WriteFile(mismatched, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyFile(mismatched); err == nil {
		t.Fatal("loaded a key file whose public key does not match")
	}
}

func FuzzParseEvent(f *testing.F) {
	k, _ := KeyFromHex("b7e151628aed2a6abf7158809cf4f3c762e7160f38b4da56a784d9045190cfef")
	e := Event{CreatedAt: 1727000000, Kind: 1, Tags: [][]string{{"t", "swarmmemo"}, {"t", "swarmmemo-lobby"}}, Content: "hi \"there\"\n\u0001"}
	_ = k.Sign(&e)
	raw, _ := json.Marshal(e)
	f.Add(raw)
	f.Add([]byte(`{"id":"","pubkey":"","created_at":0,"kind":1,"tags":[],"content":"","sig":""}`))
	f.Add([]byte(`["EVENT","s",{}]`))
	f.Add([]byte(`{"tags":[[]]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		e, err := ParseEvent(data)
		if err != nil {
			return
		}
		// A parsed event is well formed: re-encoding it parses back to the same
		// event, and its id and signature can be checked without failing hard.
		again, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if len(again) > MaxEventBytes {
			return // Go escapes more than the input had to; the cap is on input
		}
		e2, err := ParseEvent(again)
		if err != nil || ComputeID(e2) != ComputeID(e) || e2.Sig != e.Sig {
			t.Fatalf("re-encoded event differs: %v", err)
		}
		if Verify(e) == nil && ComputeID(e) != e.ID {
			t.Fatal("verified an event whose id does not match")
		}
	})
}

// The id preimage is JSON that decodes back to exactly the event's fields, for
// any content and tag values: the hand-written escaper neither drops nor adds
// a character, so two parsers can never disagree about what was signed.
func FuzzSerializeRoundTrip(f *testing.F) {
	for _, s := range []string{"", "hi", "\"\\\n\r\t\b\f\x00\x1f\x7f", "  </script>&<>", "é🙂"} {
		f.Add(s, s)
	}
	f.Fuzz(func(t *testing.T, content, tag string) {
		if !utf8.ValidString(content) || !utf8.ValidString(tag) {
			return
		}
		e := Event{PubKey: strings.Repeat("a", 64), CreatedAt: 1727000000, Kind: 1, Tags: [][]string{{"t", tag}, {tag}}, Content: content}
		var got []any
		dec := json.NewDecoder(bytes.NewReader(Serialize(e)))
		dec.UseNumber()
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("preimage is not JSON: %v", err)
		}
		tags, _ := json.Marshal(got[4])
		want, _ := json.Marshal(e.Tags)
		if len(got) != 6 || got[0] != json.Number("0") || got[1] != e.PubKey || got[2] != json.Number("1727000000") || got[3] != json.Number("1") || got[5] != content || string(tags) != string(want) {
			t.Fatalf("preimage decodes to %#v", got)
		}
	})
}
