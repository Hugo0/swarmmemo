package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// C97: a verifier's optional per-property checks, strictly validated,
// signed inside the command data, and read back from the signed bytes.

var sampleChecks = []VerdictCheck{
	{Property: "integrity", State: "pass", SubjectSHA256: strings.Repeat("ab", 32), Tool: "sha256sum@9.4", Evidence: strings.Repeat("c", 32)},
	{Property: "issuer_auth", State: "not_checkable", Evidence: "https://example.org/proof?x=1"},
	{Property: "conformance", State: "not_checked"},
}

func TestParseVerdictChecks(t *testing.T) {
	raw, _ := json.Marshal(sampleChecks)
	got, why := parseVerdictChecks(raw)
	if why != "" || !reflect.DeepEqual(got, sampleChecks) {
		t.Fatalf("valid list: %v %q", got, why)
	}
	max := strings.TrimSuffix(strings.Repeat(`{"property":"a","state":"pass"},`, VerdictChecksMax), ",")
	if got, why = parseVerdictChecks(json.RawMessage("[" + max + "]")); why != "" || len(got) != VerdictChecksMax {
		t.Fatalf("16 entries: %d %q", len(got), why)
	}
	for _, tc := range []struct{ raw, want string }{
		{`[` + max + `,{"property":"a","state":"pass"}]`, "checks has more than 16 entries."},
		{`[]`, "checks, when sent, needs at least one entry."},
		{`{}`, "checks must be an array."},
		{`null`, "checks must be an array."},
		{`["pass"]`, "checks[0] must be an object."},
		{`[{"property":"a","state":"pass","verdict":"x"}]`, "checks[0].verdict is not a field a check takes."},
		{`[{"property":"a","state":"pass"},{"property":"a","state":"pass","<b>":1}]`, "checks[1] has a field a check does not take."},
		{`[{"property":"a","state":"pass","state":"fail"}]`, "checks[0] repeats a field."},
		{`[{"property":"a"}]`, "checks[0] needs property and state."},
		{`[{"property":"A","state":"pass"}]`, "checks[0].property is not valid."},
		{`[{"property":"` + strings.Repeat("a", 41) + `","state":"pass"}]`, "checks[0].property is not valid."},
		{`[{"property":"a","state":"ok"}]`, "checks[0].state is not valid."},
		{`[{"property":"a","state":"pass","subject_sha256":"ABC"}]`, "checks[0].subject_sha256 is not valid."},
		{`[{"property":"a","state":"pass","tool":"` + strings.Repeat("t", 81) + `"}]`, "checks[0].tool is not valid."},
		{`[{"property":"a","state":"pass","tool":""}]`, "checks[0].tool is not valid."},
		{`[{"property":"a","state":"pass","evidence":"a\nb"}]`, "checks[0].evidence is not valid."},
		{`[{"property":"a","state":"pass","evidence":"` + strings.Repeat("e", 201) + `"}]`, "checks[0].evidence is not valid."},
		{`[{"property":"a","state":"pass","evidence":null}]`, "checks[0].evidence is not valid."},
		{`[{"property":"a","state":1}]`, "checks[0].state must be a string."},
		{`[{"property":"a","state":"pass"}] x`, "checks must be an array."},
	} {
		if _, why := parseVerdictChecks(json.RawMessage(tc.raw)); why != tc.want {
			t.Errorf("%s: got %q, want %q", tc.raw, why, tc.want)
		}
	}
}

// FuzzParseVerdictChecks: any input either refuses with a reason or yields
// a bounded list that re-encodes to a list it accepts unchanged.
func FuzzParseVerdictChecks(f *testing.F) {
	raw, _ := json.Marshal(sampleChecks)
	for _, seed := range []string{string(raw), `[]`, `[{"property":"a","state":"pass"}]`, `[{"property":"a","state":"pass","tool":"\u0085"}]`, `[{"property":"a","property":"b"}]`, `[[`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		checks, why := parseVerdictChecks(json.RawMessage(in))
		if why != "" {
			if checks != nil {
				t.Fatal("a refusal returned checks")
			}
			return
		}
		if len(checks) < 1 || len(checks) > VerdictChecksMax {
			t.Fatalf("%d checks", len(checks))
		}
		again, _ := json.Marshal(checks)
		if back, why := parseVerdictChecks(again); why != "" || !reflect.DeepEqual(back, checks) {
			t.Fatalf("round trip: %q %v", why, back)
		}
	})
}

func checksData(t *testing.T, fields map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestWorkVerdictChecks(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker, second := keyFor(197), keyFor(198), keyFor(199)
	id := createTestWork(t, s, owner, "lobby", "request", 0)
	verdict := func(key ed25519.PrivateKey, c Command, fields map[string]any) Command {
		fields["schema"], fields["generation"] = 1, s.generation
		c.Data, c.Timestamp = checksData(t, fields), s.now().Unix()
		return signed(key, c)
	}
	attempt := func(key ed25519.PrivateKey, fence int64) {
		t.Helper()
		run(t, s, workCommand(s, key, Command{Operation: "work.claim", MessageID: id, TTL: 600}))
		result := workResult(t, s, key, id, "lobby")
		// Only a verdict takes checks: a submit naming them is refused.
		submit := verdict(key, Command{Operation: "work.submit", MessageID: id, Amount: fence, Target: result}, map[string]any{"checks": sampleChecks})
		if msg := failsWith(t, s, submit, "invalid_work_data"); !strings.HasPrefix(msg, "checks is not a field work.submit data takes.") {
			t.Fatalf("submit with checks: %q", msg)
		}
		run(t, s, workCommand(s, key, Command{Operation: "work.submit", MessageID: id, Amount: fence, Target: result}))
	}
	attempt(worker, 1)
	// A bad list is refused, naming the field, before anything moves.
	bad := verdict(owner, Command{Operation: "work.reject", MessageID: id, Amount: 1, Reason: "no"}, map[string]any{"checks": []map[string]any{{"property": "integrity", "state": "pass", "score": 3}}})
	if msg := failsWith(t, s, bad, "invalid_work_data"); !strings.HasPrefix(msg, "checks[0].score is not a field a check takes.") || !strings.HasSuffix(msg, VerdictChecksRule) {
		t.Fatalf("bad checks: %q", msg)
	}
	if w := getTestWork(t, s, id); w.State != "submitted" || w.VerdictChecks != nil {
		t.Fatalf("after a refused verdict: %+v", w)
	}
	rejectChecks := sampleChecks[2:]
	run(t, s, verdict(owner, Command{Operation: "work.reject", MessageID: id, Amount: 1, Reason: "conformance not checked yet"}, map[string]any{"checks": rejectChecks}))
	w := getTestWork(t, s, id)
	if w.VerdictChecks == nil || w.VerdictChecks.Operation != "work.reject" || w.VerdictChecks.Author != keyID(owner) || !reflect.DeepEqual(w.VerdictChecks.Checks, rejectChecks) {
		t.Fatalf("reject checks: %+v", w.VerdictChecks)
	}
	attempt(second, 2)
	run(t, s, verdict(owner, Command{Operation: "work.accept", MessageID: id, Amount: 2}, map[string]any{"checks": sampleChecks}))
	w = getTestWork(t, s, id)
	if w.State != "accepted" || w.VerdictChecks == nil || w.VerdictChecks.Operation != "work.accept" || !reflect.DeepEqual(w.VerdictChecks.Checks, sampleChecks) {
		t.Fatalf("accept checks: %+v", w.VerdictChecks)
	}
	// History: each verdict's own list, inside the bytes its signature covers.
	transitions := workHistoryOf(t, s, id).Data["transitions"].([]WorkTransition)
	seen := 0
	for _, tr := range transitions {
		switch tr.Operation {
		case "work.reject", "work.accept":
			seen++
			want := sampleChecks
			if tr.Operation == "work.reject" {
				want = rejectChecks
			}
			pub, _ := base64.RawURLEncoding.DecodeString(tr.PublicKey)
			sig, _ := base64.RawURLEncoding.DecodeString(tr.Signature)
			if !reflect.DeepEqual(tr.Checks, want) || !ed25519.Verify(pub, []byte(tr.SignedPayload), sig) || !strings.Contains(tr.SignedPayload, `\"checks\":`) {
				t.Fatalf("%s transition: %+v", tr.Operation, tr)
			}
		default:
			if tr.Checks != nil {
				t.Fatalf("%s carries checks", tr.Operation)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("verdicts in history: %d", seen)
	}
	// A verdict without checks keeps the old shape.
	plain := createTestWork(t, s, owner, "lobby", "request", 0)
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: plain, TTL: 600}))
	run(t, s, workCommand(s, worker, Command{Operation: "work.submit", MessageID: plain, Amount: 1, Target: workResult(t, s, worker, plain, "lobby")}))
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: plain, Amount: 1}))
	if w := getTestWork(t, s, plain); w.VerdictChecks != nil {
		t.Fatalf("plain accept: %+v", w.VerdictChecks)
	}
	raw, _ := json.Marshal(getTestWork(t, s, plain))
	if strings.Contains(string(raw), "verdict_checks") {
		t.Fatalf("plain work JSON names verdict_checks: %s", raw)
	}
}

func TestWitnessChecks(t *testing.T) {
	s, _ := linkTest(t)
	a, other, b := keyFor(163), keyFor(164), keyFor(165)
	registerAll(t, s, a, b)
	nonce := "witness-checks-nonce-0123456"
	value := provenLink(t, s, a, other, nonce)
	witness := func(checks any) Command {
		return signed(b, Command{Operation: "identity.witness", Data: checksData(t, map[string]any{"schema": 1, "agent": keyID(a), "kind": "ed25519", "value": value, "nonce": nonce, "verdict": "verified", "checks": checks})})
	}
	msg := failsWith(t, s, witness([]map[string]any{{"property": "signature", "state": "maybe"}}), "invalid_witness")
	if !strings.HasPrefix(msg, "checks[0].state is not valid.") {
		t.Fatalf("bad state: %q", msg)
	}
	msg = failsWith(t, s, witness([]map[string]any{{"property": "signature", "state": "pass", "note": "x"}}), "invalid_witness")
	if !strings.HasPrefix(msg, "checks[0].note is not a field a check takes.") {
		t.Fatalf("unknown field: %q", msg)
	}
	checks := []VerdictCheck{{Property: "signature", State: "pass", SubjectSHA256: strings.Repeat("0f", 32), Tool: "swarmmemo.py@1.56"}, {Property: "anchor", State: "not_checked"}}
	res := run(t, s, witness(checks))
	if !reflect.DeepEqual(res.Data["checks"], checks) {
		t.Fatalf("witness result: %v", res.Data)
	}
	l := linkOf(t, s, a, "ed25519")
	if len(l.Witnesses) != 1 || !reflect.DeepEqual(l.Witnesses[0].Checks, checks) || !verifyWitness(l.Witnesses[0], keyID(a), "ed25519", value) {
		t.Fatalf("witness with checks: %+v", l.Witnesses)
	}
	// A full list fits: data beyond 1024 bytes is the checks list's.
	long := make([]VerdictCheck, VerdictChecksMax)
	for i := range long {
		long[i] = VerdictCheck{Property: "conformance." + strings.Repeat("x", 28), State: "pass", SubjectSHA256: strings.Repeat("ab", 32), Tool: strings.Repeat("t", VerdictCheckToolMax), Evidence: "https://example.org/" + strings.Repeat("e", VerdictCheckEvidenceMax-20)}
	}
	run(t, s, witness(long))
	if l = linkOf(t, s, a, "ed25519"); len(l.Witnesses) != 1 || len(l.Witnesses[0].Checks) != VerdictChecksMax {
		t.Fatalf("full list: %+v", l.Witnesses)
	}
	// A witness without checks has none.
	run(t, s, witnessCommand(b, keyID(a), "ed25519", value, nonce, "failed"))
	if l = linkOf(t, s, a, "ed25519"); l.Witnesses[0].Checks != nil {
		t.Fatalf("plain witness: %+v", l.Witnesses[0])
	}
}
