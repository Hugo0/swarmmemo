package services_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
)

// memBlobs is a BlobKeeper in memory: what blob.put would store, by ID.
type memBlobs struct {
	mu    sync.Mutex
	files map[string]services.BlobKeep
	fail  error // refuse every keep with this
}

func (m *memBlobs) KeepBlob(_ context.Context, k services.BlobKeep) (services.KeptBlob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return services.KeptBlob{}, m.fail
	}
	id := fmt.Sprintf("%032x", len(m.files)+1)
	k.Data = append([]byte(nil), k.Data...)
	m.files[id] = k
	sum := sha256.Sum256(k.Data)
	return services.KeptBlob{ID: id, Room: k.Room, SHA256: hex.EncodeToString(sum[:]), URL: "https://swarmmemo.com/a/" + id, Size: int64(len(k.Data)), Cost: int64(len(k.Data) + len(k.Filename) + len(k.MediaType) + 512)}, nil
}

// newKeepRig is newFetchRig with a blob keeper (nil: keeping unavailable).
func newKeepRig(t *testing.T, s *site, blobs services.BlobKeeper) *fetchRig {
	t.Helper()
	cfg, err := services.FetchConfigForTest([]byte(`{"schema":1}`), map[string][]net.IP{"site.test": {publicIP}}, loopbackOK, s.srv.Listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	services.SetFetchIntervalForTest(cfg, 10*time.Millisecond)
	r := &fetchRig{t: t, db: openDB(t), meter: servicestest.NewMeter(1 << 30), jev: &fakeScreener{cost: 30}, now: wakeT0}
	reg := services.NewBuiltinRegistry([]string{"fetch"}, services.Deps{DB: r.db, Fetch: cfg, TextScreener: r.jev, ServiceID: "swarmmemo.com", Blobs: blobs})
	r.e = services.NewEngine(services.Config{DB: r.db, Registry: reg, Meter: r.meter, Now: func() int64 { return r.now }})
	t.Cleanup(r.e.Stop)
	return r
}

const rssFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Feed</title><item><title>First &amp; best</title><link>https://site.test/1</link></item></channel></rss>
`

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestFetchReadsFeedsAndHashesTheRawBytes(t *testing.T) {
	latin := append([]byte(`<?xml version="1.0" encoding="ISO-8859-1"?><feed xmlns="http://www.w3.org/2005/Atom"><title>Caf`), 0xe9, '<', '/', 't', 'i', 't', 'l', 'e', '>', '<', '/', 'f', 'e', 'e', 'd', '>')
	html := `<html><head><title>T</title></head><body><p>Hi</p></body></html>`
	s := newSite(t, false, map[string]http.HandlerFunc{
		"/feed.rss": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
			fmt.Fprint(w, rssFeed)
		},
		"/atom": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/atom+xml")
			w.Write(latin)
		},
		"/sitemap.xml": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/xml")
			fmt.Fprint(w, `<urlset><url><loc>https://site.test/</loc></url></urlset>`)
		},
		"/page": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, html)
		},
		"/image.png": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("\x89PNG\r\n"))
		},
		"/archive.zip": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/zip")
			w.Write([]byte("PK\x03\x04"))
		},
	})
	r := newKeepRig(t, s, nil)

	// RSS comes back as it is, as text, with the SHA-256 of the bytes served.
	res := get(r.must("alice", noScreen("http://site.test/feed.rss")), "result").(map[string]any)
	if res["format"] != "xml" || res["text"] != rssFeed || res["content_type"] != "application/rss+xml" {
		t.Fatalf("rss: %+v", res)
	}
	if res["raw_sha256"] != sha([]byte(rssFeed)) || res["raw_bytes"] != float64(len(rssFeed)) {
		t.Fatalf("raw hash of the served bytes: %v %v, want %s", res["raw_sha256"], res["raw_bytes"], sha([]byte(rssFeed)))
	}
	// Atom in Latin-1 (its XML declaration says so): the text is decoded,
	// the hash is of the bytes as received.
	res = get(r.must("alice", noScreen("http://site.test/atom")), "result").(map[string]any)
	if res["format"] != "xml" || res["raw_sha256"] != sha(latin) || res["raw_bytes"] != float64(len(latin)) {
		t.Fatalf("atom: %+v", res)
	}
	if text, _ := res["text"].(string); text != `<?xml version="1.0" encoding="ISO-8859-1"?><feed xmlns="http://www.w3.org/2005/Atom"><title>Café</title></feed>` {
		t.Fatalf("latin-1 decoded: %q", text)
	}
	res = get(r.must("alice", noScreen("http://site.test/sitemap.xml")), "result").(map[string]any)
	if res["format"] != "xml" || res["content_type"] != "text/xml" {
		t.Fatalf("text/xml: %+v", res)
	}
	// Every accepted type carries the hash: HTML is hashed before extraction.
	res = get(r.must("alice", noScreen("http://site.test/page")), "result").(map[string]any)
	if res["format"] != "markdown" || res["raw_sha256"] != sha([]byte(html)) || res["raw_bytes"] != float64(len(html)) {
		t.Fatalf("html raw hash: %+v", res)
	}
	// A cached answer carries the same hash.
	res = get(r.must("bob", noScreen("http://site.test/feed.rss")), "result").(map[string]any)
	if res["cached"] != true || res["raw_sha256"] != sha([]byte(rssFeed)) {
		t.Fatalf("cached: %+v", res)
	}
	// Other types are still refused.
	for _, path := range []string{"/image.png", "/archive.zip"} {
		if _, err := r.fetch("alice", noScreen("http://site.test"+path)); code(err) != "fetch_unsupported_type" {
			t.Fatalf("%s: %v", path, err)
		}
	}
}

func TestFetchKeepBlobStoresTheRawBytes(t *testing.T) {
	s := newSite(t, false, map[string]http.HandlerFunc{
		"/feed.rss": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
			fmt.Fprint(w, rssFeed)
		},
	})
	blobs := &memBlobs{files: map[string]services.BlobKeep{}}
	r := newKeepRig(t, s, blobs)
	args := map[string]any{"url": "http://site.test/feed.rss", "screen": false, "keep": "blob", "room": "evidence"}
	res := get(r.must("alice", args), "result").(map[string]any)
	id, _ := res["blob_id"].(string)
	kept, ok := blobs.files[id]
	if id == "" || !ok {
		t.Fatalf("no blob kept: %+v", res)
	}
	if sha(kept.Data) != res["raw_sha256"] || string(kept.Data) != rssFeed {
		t.Fatalf("the kept bytes re-hash to raw_sha256: %s vs %v", sha(kept.Data), res["raw_sha256"])
	}
	if kept.Room != "evidence" || kept.MediaType != "application/rss+xml; charset=utf-8" || kept.Subject.ID != "alice" || !kept.Subject.Signed {
		t.Fatalf("kept as blob.put by the caller: %+v", kept)
	}
	if get(res, "blob", "url") != "https://swarmmemo.com/a/"+id || get(res, "blob", "sha256") != res["raw_sha256"] || get(res, "blob", "resource") != string(allowance.PostBytes) {
		t.Fatalf("blob in the answer: %+v", res["blob"])
	}
	// The fetch itself costs what it did: the file is blob.put's price, on
	// the storage allowance, not credit.
	out := r.must("bob", args)
	if get(out, "call", "cost") != float64(services.FetchPrice.For(int64(len(rssFeed)))) || len(blobs.files) != 2 {
		t.Fatalf("cost %v, files %d", get(out, "call", "cost"), len(blobs.files))
	}

	// keep needs room, takes only "blob", and room needs keep.
	for _, bad := range []map[string]any{
		{"url": "http://site.test/feed.rss", "keep": "blob"},
		{"url": "http://site.test/feed.rss", "keep": "disk", "room": "evidence"},
		{"url": "http://site.test/feed.rss", "room": "evidence"},
	} {
		if _, err := r.fetch("alice", bad); code(err) != "invalid_service_data" {
			t.Fatalf("%v: %v", bad, err)
		}
	}
	// blob.put refusing the file refuses the call, refunded.
	blobs.fail = &allowance.Err{Code: "fetch_keep_refused"}
	if _, err := r.fetch("carol", args); code(err) != "fetch_keep_refused" {
		t.Fatalf("refused keep: %v", err)
	}
	// Without blob storage, keep is refused before anything is fetched.
	s2 := newSite(t, false, nil)
	none := newKeepRig(t, s2, nil)
	if _, err := none.fetch("alice", map[string]any{"url": "http://site.test/x", "keep": "blob", "room": "evidence"}); code(err) != "fetch_keep_unavailable" || s2.hit("/x") != 0 {
		t.Fatalf("no keeper: %v, hits %d", err, s2.hit("/x"))
	}
}
