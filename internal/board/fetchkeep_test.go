package board

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
)

// fetch's keep=blob over the real board: the bytes go through blob.put (its
// room access and its storage charge), and blob.get returns bytes that
// re-hash to the answer's raw_sha256.
func TestFetchKeepBlobThroughBlobPut(t *testing.T) {
	const feed = `<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><title>Feed</title></channel></rss>`
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, feed)
	}))
	t.Cleanup(site.Close)
	cfg, err := services.ParseFetchConfig([]byte(`{"schema":1,"screen":"off"}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.UseTestUpstream(map[string][]net.IP{"site.example": {net.ParseIP("192.0.2.80")}}, site.Listener.Addr().String())
	s := openTest(t, Config{Features: Features{Services: []string{"fetch"}}, Fetch: cfg})
	s.UseServiceMeter(servicestest.NewMeter(1<<30), &servicestest.Params{})
	t.Cleanup(s.stopServices)
	owner, stranger := keyFor(71), keyFor(72)
	register(t, s, stranger)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "evidence"}))
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "vault", Visibility: "private"}))

	before := globalUsed(t, s)
	out := run(t, s, svcCall(owner, "fetch", "page", map[string]any{"url": "http://site.example/feed.rss", "keep": "blob", "room": "evidence"}, 1<<20, "keep-1"))
	sum := sha256.Sum256([]byte(feed))
	if svcField(t, out.Data, "result", "raw_sha256") != hex.EncodeToString(sum[:]) || svcField(t, out.Data, "result", "format") != "xml" {
		t.Fatalf("answer: %+v", out.Data)
	}
	id, _ := svcField(t, out.Data, "result", "blob_id").(string)
	if len(id) != 32 || svcField(t, out.Data, "result", "blob", "url") != "https://swarmmemo.com/a/"+id {
		t.Fatalf("blob: %+v", out.Data)
	}
	cost, _ := svcField(t, out.Data, "result", "blob", "cost").(float64)
	if used := globalUsed(t, s) - before; int64(cost) != blobPutCost(len(feed), "fetch-"+hex.EncodeToString(sum[:])[:16]+".xml", "application/rss+xml") || used < int64(cost) {
		t.Fatalf("charged as blob.put: cost %v, storage used %d", cost, used)
	}
	got, err := s.Execute(testContext, Command{Operation: "blob.get", MessageID: id}, "test-origin")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(got.Data["data"].(string))
	if again := sha256.Sum256(raw); again != sum {
		t.Fatalf("the kept bytes re-hash to raw_sha256: %q", raw)
	}

	// blob.put's room rule holds: a private room the caller is not in is
	// refused before anything is fetched or reserved.
	fails(t, s, svcCall(stranger, "fetch", "page", map[string]any{"url": "http://site.example/other", "keep": "blob", "room": "vault"}, 1<<20, "keep-2"), "fetch_keep_refused")
	fails(t, s, svcCall(stranger, "fetch", "page", map[string]any{"url": "http://site.example/other", "keep": "blob", "room": "nowhere"}, 1<<20, "keep-3"), "fetch_keep_refused")
	// Its owner keeps a file there, with no public URL.
	private := run(t, s, svcCall(owner, "fetch", "page", map[string]any{"url": "http://site.example/feed.rss", "keep": "blob", "room": "vault"}, 1<<20, "keep-4"))
	if svcField(t, private.Data, "result", "blob", "url") != "" || svcField(t, private.Data, "result", "blob", "room") != "vault" {
		t.Fatalf("private keep: %+v", private.Data)
	}
}
