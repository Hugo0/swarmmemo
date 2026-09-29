package services

// Security review of screen.text (branch screen-service, 5a09344): the
// shared signing helpers keep old receipts valid, and receipts of one schema
// never verify as another's.

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"
)

// Signed on main (1.21.0) with encodePayload/encodeRunPayload and the seed
// 1..32; the service ID carries <, > and & to pin the no-HTML-escaping rule.
const (
	reviewGoldenNotary = `{"schema":"swarmmemo-notary/1","hash":"ab12","time":1790000000,"seq":7,"service_id":"swarm\u003cmemo\u003e\u0026.com","key_id":"65b60673d6ed884bf01c2c222d82ada0740f29ac3355d6a925c81f17f47a27b8","public_key":"ebVWLo_mVPlAeLES6KmLp5AfhTrmlb7X4OORC60ElmQ","payload":"{\"schema\":\"swarmmemo-notary/1\",\"service_id\":\"swarm\u003cmemo\u003e\u0026.com\",\"key_id\":\"65b60673d6ed884bf01c2c222d82ada0740f29ac3355d6a925c81f17f47a27b8\",\"seq\":7,\"time\":1790000000,\"hash\":\"ab12\"}","signature":"u7QNAbJ5Elt5FNgdSwxDGRHN-1VtA9BXPshhADcEiweukzoX5D8szB6CVLk-v1TxgNslKYNGCyujm2OtdjGyBQ"}`
	reviewGoldenRun    = `{"schema":"swarmmemo-run/1","run_id":"r1","key_id":"65b60673d6ed884bf01c2c222d82ada0740f29ac3355d6a925c81f17f47a27b8","public_key":"ebVWLo_mVPlAeLES6KmLp5AfhTrmlb7X4OORC60ElmQ","payload":"{\"schema\":\"swarmmemo-run/1\",\"service_id\":\"swarm\u003cmemo\u003e\u0026.com\",\"key_id\":\"65b60673d6ed884bf01c2c222d82ada0740f29ac3355d6a925c81f17f47a27b8\",\"run_id\":\"r1\",\"time\":1790000000,\"language\":\"python\",\"network\":false,\"status\":\"ok\",\"cpu_ms\":12,\"egress_bytes\":0,\"code_sha256\":\"c\",\"input_sha256\":\"i\",\"output_sha256\":\"o\",\"egress_sha256\":\"e\"}","signature":"D2iMSZHB_C93s48N8iQMdY5qCv64ygHCu4iq30XDlfhD_tbFpYjug6AD2eOUQaGeWS18rZr6mEkF3SrTlojaAw"}`
)

func reviewKey() ed25519.PrivateKey {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func TestReviewOldReceiptsStillVerify(t *testing.T) {
	key := reviewKey()
	var nr NotaryReceipt
	var rr RunReceipt
	if json.Unmarshal([]byte(reviewGoldenNotary), &nr) != nil || json.Unmarshal([]byte(reviewGoldenRun), &rr) != nil {
		t.Fatal("golden receipts do not parse")
	}
	if !VerifyNotaryReceipt(nr.PublicKey, nr) || !VerifyRunReceipt(rr.PublicKey, rr) {
		t.Fatal("a receipt signed by 1.21.0 no longer verifies")
	}
	// signPayload makes the same bytes and signature 1.21.0 did.
	var np NotaryPayload
	var rp RunReceiptPayload
	_ = json.Unmarshal([]byte(nr.Payload), &np)
	_ = json.Unmarshal([]byte(rr.Payload), &rp)
	if p, s := signPayload(key, np); p != nr.Payload || s != nr.Signature {
		t.Fatalf("notary payload bytes changed: %s", p)
	}
	if p, s := signPayload(key, rp); p != rr.Payload || s != rr.Signature {
		t.Fatalf("run payload bytes changed: %s", p)
	}
}

func TestReviewReceiptsDoNotCrossSchemas(t *testing.T) {
	key := reviewKey()
	pub := key.Public().(ed25519.PublicKey)
	public := base64.RawURLEncoding.EncodeToString(pub)
	var nr NotaryReceipt
	var rr RunReceipt
	_ = json.Unmarshal([]byte(reviewGoldenNotary), &nr)
	_ = json.Unmarshal([]byte(reviewGoldenRun), &rr)
	sp := ScreenPayload{Schema: ScreenSchema, ServiceID: "swarmmemo.com", KeyID: keyID(pub), Time: 1, TextSHA256: "ab12", TextBytes: 1, Source: "web",
		Categories: map[string]float64{"injection": 0, "exfiltration": 0, "phishing": 0, "malware": 0}, Verdict: "pass", Threshold: 0.6, Model: "jev-1.13.0"}
	payload, sig := signPayload(key, sp)
	if _, ok := VerifyScreenReceipt(public, ScreenReceipt{Schema: ScreenSchema, KeyID: sp.KeyID, PublicKey: public, Payload: payload, Signature: sig}); !ok {
		t.Fatal("a good screen receipt failed")
	}
	// Every schema's payload, under every other schema's receipt and name.
	for _, schema := range []string{NotarySchema, RunReceiptSchema, ScreenSchema} {
		if VerifyNotaryReceipt(public, NotaryReceipt{Schema: schema, KeyID: sp.KeyID, Time: sp.Time, ServiceID: sp.ServiceID, PublicKey: public, Payload: payload, Signature: sig}) {
			t.Fatalf("a screen receipt verified as a notary receipt (%s)", schema)
		}
		if VerifyRunReceipt(public, RunReceipt{Schema: schema, KeyID: sp.KeyID, PublicKey: public, Payload: payload, Signature: sig}) {
			t.Fatalf("a screen receipt verified as a run receipt (%s)", schema)
		}
		for _, other := range []struct{ payload, sig string }{{nr.Payload, nr.Signature}, {rr.Payload, rr.Signature}} {
			if _, ok := VerifyScreenReceipt(public, ScreenReceipt{Schema: schema, KeyID: sp.KeyID, PublicKey: public, Payload: other.payload, Signature: other.sig}); ok {
				t.Fatalf("a notary or run receipt verified as a screen receipt (%s)", schema)
			}
		}
	}
}
