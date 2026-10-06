package ots

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeCalendar is an httptest OpenTimestamps calendar: POST /digest answers
// append(nonce), sha256, pending(itself); once ready, GET /timestamp/C
// answers prepend(x), sha256, bitcoin(height) for the commitments it issued.
type fakeCalendar struct {
	srv     *httptest.Server
	ready   atomic.Bool
	nonce   []byte
	height  uint64
	issued  map[string]bool
	submits atomic.Int64
}

func newFakeCalendar(t *testing.T, nonce byte, height uint64) *fakeCalendar {
	c := &fakeCalendar{nonce: bytes.Repeat([]byte{nonce}, 16), height: height, issued: map[string]bool{}}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/digest":
			c.submits.Add(1)
			digest := make([]byte, 64)
			n, _ := r.Body.Read(digest)
			msg := append(digest[:n:n], c.nonce...)
			commitment := sha256.Sum256(msg)
			c.issued[hex.EncodeToString(commitment[:])] = true
			var payload bytes.Buffer
			writeVarbytes(&payload, []byte(c.srv.URL))
			stamp := &Timestamp{Msg: digest[:n], Branches: []Branch{{Op: Op{Tag: opAppend, Arg: c.nonce}, Stamp: &Timestamp{Msg: msg,
				Branches: []Branch{{Op: Op{Tag: opSHA256}, Stamp: &Timestamp{Msg: commitment[:], Attestations: []Attestation{{Tag: TagPending, Payload: payload.Bytes()}}}}}}}}}
			var out bytes.Buffer
			stamp.Serialize(&out)
			w.Write(out.Bytes())
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/timestamp/"):
			commitment := strings.TrimPrefix(r.URL.Path, "/timestamp/")
			if !c.ready.Load() || !c.issued[commitment] {
				http.NotFound(w, r)
				return
			}
			msg, _ := hex.DecodeString(commitment)
			next := append([]byte("block"), msg...)
			root := sha256.Sum256(next)
			var payload bytes.Buffer
			payload.WriteByte(byte(c.height&0x7f | 0x80))
			payload.WriteByte(byte(c.height>>7&0x7f | 0x80))
			payload.WriteByte(byte(c.height >> 14))
			stamp := &Timestamp{Msg: msg, Branches: []Branch{{Op: Op{Tag: opPrepend, Arg: []byte("block")}, Stamp: &Timestamp{Msg: next,
				Branches: []Branch{{Op: Op{Tag: opSHA256}, Stamp: &Timestamp{Msg: root[:], Attestations: []Attestation{{Tag: TagBitcoin, Payload: payload.Bytes()}}}}}}}}}
			var out bytes.Buffer
			stamp.Serialize(&out)
			w.Write(out.Bytes())
		default:
			http.Error(w, "no", 400)
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func TestStampAndUpgrade(t *testing.T) {
	a, b := newFakeCalendar(t, 1, 300000), newFakeCalendar(t, 2, 300001)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) }))
	defer down.Close()
	client := &Client{Calendars: []string{a.srv.URL, b.srv.URL, down.URL}}
	digest := sha256.Sum256([]byte("checkpoint"))
	f, errs := client.Stamp(context.Background(), digest[:])
	if f == nil || len(errs) != 1 || !strings.Contains(errs[0].Error(), down.URL) {
		t.Fatalf("stamp: %v %v", f, errs)
	}
	pending, height := f.Stamp.Status()
	if len(pending) != 2 || height != 0 {
		t.Fatalf("pending %v height %d", pending, height)
	}
	// The file round-trips byte for byte.
	raw := f.Bytes()
	parsed, err := ParseFile(raw)
	if err != nil || !bytes.Equal(parsed.Bytes(), raw) || !bytes.Equal(parsed.Digest, digest[:]) {
		t.Fatalf("round trip: %v", err)
	}
	// Not ready: nothing changes.
	if changed, err := client.Upgrade(context.Background(), parsed); changed || err != nil {
		t.Fatalf("early upgrade: %v %v", changed, err)
	}
	a.ready.Store(true)
	changed, err := client.Upgrade(context.Background(), parsed)
	if !changed || err != nil {
		t.Fatalf("upgrade: %v %v", changed, err)
	}
	if _, height = parsed.Stamp.Status(); height != 300000 {
		t.Fatalf("height %d", height)
	}
	// A calendar not configured is never contacted for an upgrade.
	other := &Client{Calendars: []string{b.srv.URL}}
	b.ready.Store(true)
	again, _ := ParseFile(raw)
	if changed, _ = other.Upgrade(context.Background(), again); !changed {
		t.Fatal("configured calendar not upgraded")
	}
	if _, height = again.Stamp.Status(); height != 300001 {
		t.Fatalf("only b's attestation expected, height %d", height)
	}
}

func TestOversizedAnswer(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte{0xff}, MaxResponseBytes+10))
	}))
	defer big.Close()
	digest := sha256.Sum256(nil)
	if f, errs := (&Client{Calendars: []string{big.URL}}).Stamp(context.Background(), digest[:]); f != nil || len(errs) != 1 {
		t.Fatalf("oversized answer accepted: %v", errs)
	}
}

// TestReferenceFiles parses proofs made by the reference client, when a
// local corpus is present, and requires a byte-identical re-serialization.
func TestReferenceFiles(t *testing.T) {
	files, _ := filepath.Glob(os.Getenv("OTS_CORPUS") + "/*.ots")
	for _, name := range files {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := ParseFile(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(f.Bytes(), raw) {
			t.Fatalf("%s: re-serialization differs", name)
		}
	}
}

// TestLiveCalendars submits one random digest to the real public calendars;
// opt in with OTS_LIVE=1.
func TestLiveCalendars(t *testing.T) {
	if os.Getenv("OTS_LIVE") != "1" {
		t.Skip("OTS_LIVE=1 submits a digest to the public calendars")
	}
	digest := sha256.Sum256([]byte("swarmmemo ots live test " + t.Name()))
	f, errs := (&Client{Calendars: DefaultCalendars}).Stamp(context.Background(), digest[:])
	if f == nil {
		t.Fatal(errs)
	}
	pending, _ := f.Stamp.Status()
	t.Logf("pending at %v, errors %v, %d bytes", pending, errs, len(f.Bytes()))
	if again, err := ParseFile(f.Bytes()); err != nil || !bytes.Equal(again.Bytes(), f.Bytes()) {
		t.Fatal("live proof does not round-trip", err)
	}
}

func FuzzParseFile(f *testing.F) {
	digest := sha256.Sum256([]byte("seed"))
	var payload bytes.Buffer
	writeVarbytes(&payload, []byte("https://alice.btc.calendar.opentimestamps.org"))
	seed := &File{Digest: digest[:], Stamp: &Timestamp{Msg: digest[:], Branches: []Branch{{Op: Op{Tag: opAppend, Arg: []byte("nonce")}, Stamp: &Timestamp{Msg: append(digest[:], "nonce"...), Attestations: []Attestation{{Tag: TagPending, Payload: payload.Bytes()}}}}}}}
	f.Add(seed.Bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		file, err := ParseFile(data)
		if err != nil {
			return
		}
		file.Stamp.Status()
		again, err := ParseFile(file.Bytes())
		if err != nil {
			t.Fatalf("re-serialized proof does not parse: %v", err)
		}
		if !bytes.Equal(again.Bytes(), file.Bytes()) {
			t.Fatal("serialization is not stable")
		}
	})
}
