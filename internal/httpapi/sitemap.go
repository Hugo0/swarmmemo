package httpapi

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// The sitemap offers every indexable page: the fixed pages, every listed room
// and every listed thread root (board/sitemap.go says which). It used to list
// only whichever of the newest 100 messages were visible roots, an arbitrary
// window from the first implementation, so most of the board was never offered.
//
// Up to sitemapURLsPerFile addresses, /sitemap.xml is one <urlset>. Beyond
// that it becomes a <sitemapindex> of /sitemap-1.xml ... /sitemap-N.xml, each
// well under the protocol's 50,000 URLs and 50 MB. N is capped, so a request
// can never ask for unbounded work, and at most sitemapBuilds are built at
// once; the rest are told to retry.

const (
	sitemapMaxFiles = 64
	sitemapBuilds   = 2
)

// sitemapURLsPerFile is a variable only so a test can page a small board.
var sitemapURLsPerFile = 40000

var sitemapFile = regexp.MustCompile(`^/sitemap-([1-9][0-9]?)\.xml$`)

type sitemapStore interface {
	PublicSitemapCounts(context.Context) (int, int, error)
	PublicSitemapRooms(context.Context, int, int) ([]board.SitemapRoom, error)
	PublicSitemapPosts(context.Context, int, int, func(board.Message)) error
}

func sitemapPath(p string) bool { return p == "/sitemap.xml" || sitemapFile.MatchString(p) }

// sitemapFixedPaths are the pages listed ahead of rooms and posts.
func (s *Server) sitemapFixedPaths(ctx context.Context) []string {
	// /work and /work/ID stay live -- with /delegation/ID they are the only
	// human-readable proof that the signed transition story is real -- but the
	// board is a place to talk, so work is no longer offered for indexing.
	return append([]string{"/", "/for-agents", "/agents", "/rooms", "/docs", "/policy", "/limits", "/stats"}, web.IndexedGuidePaths(ctx, s.service)...)
}

func (s *Server) sitemap(w http.ResponseWriter, r *http.Request) {
	select {
	case s.sitemapBuilds <- struct{}{}:
		defer func() { <-s.sitemapBuilds }()
	default:
		w.Header().Set("Retry-After", "5")
		writeError(w, &board.Error{Status: 503, Code: "busy", Message: "The sitemap is being built for other readers. Retry shortly.", RetryAfter: 5})
		return
	}
	file := 0
	if m := sitemapFile.FindStringSubmatch(r.URL.Path); m != nil {
		file, _ = strconv.Atoi(m[1])
	}
	fixed := s.sitemapFixedPaths(r.Context())
	store, ok := s.service.(sitemapStore)
	rooms, posts := 0, 0
	if ok {
		var err error
		if rooms, posts, err = store.PublicSitemapCounts(r.Context()); err != nil {
			sitemapUnavailable(w)
			return
		}
	}
	total := len(fixed) + rooms + posts
	files := min((total+sitemapURLsPerFile-1)/sitemapURLsPerFile, sitemapMaxFiles)
	var out bytes.Buffer
	out.WriteString(xml.Header)
	switch {
	case file == 0 && files > 1:
		out.WriteString(`<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
		for i := 1; i <= files; i++ {
			out.WriteString("<sitemap><loc>")
			_ = xml.EscapeText(&out, []byte(s.cfg.PublicURL+"/sitemap-"+strconv.Itoa(i)+".xml"))
			out.WriteString("</loc></sitemap>")
		}
		out.WriteString("</sitemapindex>")
	case file == 0 || file <= files && files > 1:
		start, end := 0, total
		if file > 0 {
			start, end = (file-1)*sitemapURLsPerFile, min(file*sitemapURLsPerFile, total)
		}
		out.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
		if err := s.sitemapURLs(r.Context(), &out, store, fixed, rooms, start, end); err != nil {
			sitemapUnavailable(w)
			return
		}
		out.WriteString("</urlset>")
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	if r.Method != http.MethodHead {
		_, _ = w.Write(out.Bytes())
	}
}

// sitemapURLs writes addresses [start,end) of the one ordered list: fixed
// pages, then rooms by name, then thread roots in posting order.
func (s *Server) sitemapURLs(ctx context.Context, out *bytes.Buffer, store sitemapStore, fixed []string, rooms, start, end int) error {
	entry := func(path string, modified int64) {
		out.WriteString("<url><loc>")
		_ = xml.EscapeText(out, []byte(s.cfg.PublicURL+path))
		out.WriteString("</loc>")
		if modified > 0 {
			fmt.Fprintf(out, "<lastmod>%s</lastmod>", time.Unix(modified, 0).UTC().Format(time.RFC3339))
		}
		out.WriteString("</url>")
	}
	for i := start; i < min(end, len(fixed)); i++ {
		entry(fixed[i], 0)
	}
	if store == nil {
		return nil
	}
	if from, to := max(start-len(fixed), 0), min(end-len(fixed), rooms); from < to {
		list, err := store.PublicSitemapRooms(ctx, from, to-from)
		if err != nil {
			return err
		}
		for _, room := range list {
			entry("/r/"+url.PathEscape(room.Name), room.Modified)
		}
	}
	head := len(fixed) + rooms
	if from, to := max(start-head, 0), end-head; from < to {
		return store.PublicSitemapPosts(ctx, from, to-from, func(m board.Message) {
			path := "/e/" + url.PathEscape(m.ID)
			if m.Format == board.PostFormatMarkdown {
				path = web.ArticlePath(m)
			}
			entry(path, m.CreatedAt)
		})
	}
	return nil
}

func sitemapUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	writeError(w, &board.Error{Status: 503, Code: "storage_unavailable", Message: "The sitemap is temporarily unavailable.", RetryAfter: 60})
}
