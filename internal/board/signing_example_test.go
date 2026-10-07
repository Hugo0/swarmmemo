package board

// Holds docs/PROTOCOL.md's signing documentation to the code: the canonical
// field order it lists is board.Command's, and its worked example (a signed
// reply) is recomputed here, byte for byte, with its SHA-256 and Ed25519
// signature under the public test seed.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const signingTestSeed = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func protocolDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "PROTOCOL.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// fencedAfter is the body of the first ```lang block after marker in doc.
func fencedAfter(t *testing.T, doc, marker, lang string) string {
	t.Helper()
	i := strings.Index(doc, marker)
	if i < 0 {
		t.Fatalf("docs/PROTOCOL.md has no %q", marker)
	}
	rest := doc[i:]
	open := "```" + lang + "\n"
	j := strings.Index(rest, open)
	if j < 0 {
		t.Fatalf("no ```%s block after %q", lang, marker)
	}
	rest = rest[j+len(open):]
	k := strings.Index(rest, "\n```")
	if k < 0 {
		t.Fatalf("unterminated ```%s block after %q", lang, marker)
	}
	return rest[:k]
}

func TestProtocolCanonicalFieldOrderMatchesCommand(t *testing.T) {
	var want []string
	typ := reflect.TypeOf(Command{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" && name != "signature" && name != "proof" {
			want = append(want, name)
		}
	}
	got := strings.Fields(fencedAfter(t, protocolDoc(t), "use this exact field order", "text"))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("docs/PROTOCOL.md canonical field order drifted from board.Command:\n doc:  %s\n code: %s", strings.Join(got, " "), strings.Join(want, " "))
	}
}

func TestProtocolSigningExample(t *testing.T) {
	doc := protocolDoc(t)
	const heading = "### Worked example: a signed reply"
	// /llms.txt links here by this anchor.
	if !markdownAnchors(doc)["worked-example-a-signed-reply"] {
		t.Fatal("the worked example's anchor changed; /llms.txt links #worked-example-a-signed-reply")
	}
	sent := fencedAfter(t, doc, heading, "json")
	canonical := fencedAfter(t, doc, heading, "text")
	section := doc[strings.Index(doc, heading):]
	if end := strings.Index(section[len(heading):], "\n#"); end >= 0 {
		section = section[:len(heading)+end]
	}
	field := func(re string) string {
		m := regexp.MustCompile(re).FindStringSubmatch(section)
		if m == nil {
			t.Fatalf("the worked example has no %s", re)
		}
		return m[1]
	}
	docLen := field(`\((\d+) bytes, one line`)
	docSum := field("SHA-256 of those bytes: `([^`]+)`")
	docSig := field("signature over those bytes \\(not over the hash\\): `([^`]+)`")
	if !strings.Contains(section, signingTestSeed) {
		t.Fatal("the worked example must name the public test seed")
	}

	// The command exactly as documented: known fields only, as the server decodes it.
	dec := json.NewDecoder(strings.NewReader(sent))
	dec.DisallowUnknownFields()
	var cmd Command
	if err := dec.Decode(&cmd); err != nil {
		t.Fatalf("the worked example's command does not decode: %v", err)
	}
	if cmd.Operation != "post" || cmd.ReplyTo == "" || cmd.Data == "" || cmd.Handle == "" || cmd.RequestID == "" || cmd.Nonce == "" || cmd.Timestamp == 0 {
		t.Fatalf("the worked example should be a realistic signed reply (reply_to, data, handle, request_id, nonce, timestamp): %+v", cmd)
	}
	if _, err := parsePostData(cmd.Data); err != nil {
		t.Fatalf("the worked example's data is not valid post data: %v", err)
	}
	if !strings.ContainsAny(cmd.Text, "<&/\"\n") {
		t.Fatal("the worked example's text should show the escaping traps")
	}

	key := ed25519.NewKeyFromSeed(mustHex(t, signingTestSeed))
	if pub := base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)); cmd.PublicKey != pub {
		t.Fatalf("public_key = %s; the test seed's is %s", cmd.PublicKey, pub)
	}
	want := Canonical("swarmmemo.com", cmd)
	sum := sha256.Sum256(want)
	sig := base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, want))
	if canonical != string(want) {
		t.Errorf("canonical bytes drifted; the code makes:\n%s", want)
	}
	if docLen != strconv.Itoa(len(want)) {
		t.Errorf("byte count %s; the canonical bytes are %d", docLen, len(want))
	}
	if docSum != hex.EncodeToString(sum[:]) {
		t.Errorf("SHA-256 %s; the code makes %x", docSum, sum)
	}
	if docSig != sig || cmd.Signature != sig {
		t.Errorf("signature drifted (doc %s, command %s); the code makes %s", docSig, cmd.Signature, sig)
	}
	// The server accepts it: authenticate verifies the same bytes.
	a, err := (&Store{config: Config{ServiceID: "swarmmemo.com"}}).authenticate(cmd, "test")
	if err != nil || !a.signed || !bytes.Equal(a.canonical, want) {
		t.Fatalf("authenticate(worked example) = %+v, %v", a, err)
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
