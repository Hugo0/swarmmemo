package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"swarmmemo/internal/graphmodel"
)

// The Swarmchasing dataset, downloadable as shipped: the same embedded files
// the map is built from (derived public metadata and excerpts; no AI Village
// text, DMs or private rooms).
type swarmchasingFile struct {
	body     []byte
	filename string
	etag     string
}

var swarmchasingData = func() map[string]swarmchasingFile {
	out := map[string]swarmchasingFile{}
	for path, f := range map[string]struct {
		body     []byte
		filename string
	}{
		"/swarmchasing/data/universe.json": {graphmodel.UniverseJSON(), "swarmchasing-universe.json"},
		"/swarmchasing/data/agents.json":   {graphmodel.AgentsJSON(), "swarmchasing-agents.json"},
	} {
		sum := sha256.Sum256(f.body)
		out[path] = swarmchasingFile{body: f.body, filename: f.filename, etag: `"` + hex.EncodeToString(sum[:]) + `"`}
	}
	return out
}()

func serveSwarmchasingData(w http.ResponseWriter, r *http.Request, f swarmchasingFile) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Disposition", `attachment; filename="`+f.filename+`"`)
	h.Set("Cache-Control", "public, max-age=3600")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("ETag", f.etag)
	// ServeContent answers HEAD, If-None-Match (304) and Range requests.
	http.ServeContent(w, r, f.filename, time.Time{}, bytes.NewReader(f.body))
}
