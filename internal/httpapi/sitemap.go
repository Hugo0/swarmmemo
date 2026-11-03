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
	"strings"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// The sitemap offers every indexable page: the fixed pages, every listed room,
// every listed subject's /s/ID page while reviews are on, the newest
// verification records (/v/ID, not hidden, capped at
// board.SitemapVerificationsMax), and every listed thread root (board/sitemap.go says which). It used to list
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

// sitemapFresh is how long a built file is served as is (C726): a build
// reads every listed room, subject, record and thread root on the store's
// one connection, and a sitemap changes little within the hour, so a crawl
// that asks again and again costs one build per file per sitemapFresh. A
// variable only so a test can watch a new post arrive.
var sitemapFresh = 10 * time.Minute

var sitemapFile = regexp.MustCompile(`^/sitemap-([1-9][0-9]?)\.xml$`)

type sitemapStore interface {
	PublicSitemapCounts(context.Context) (int, int, error)
	PublicSitemapRooms(context.Context, int, int) ([]board.SitemapRoom, error)
	PublicSitemapPosts(context.Context, int, int, func(board.Message)) error
}

// sitemapSubjectStore lists the listed subjects (/s/ID), in the order they
// were listed, each with its newest review's time.
type sitemapSubjectStore interface {
	PublicSitemapSubjectCount(context.Context) (int, error)
	PublicSitemapSubjects(context.Context, int, int) ([]board.SitemapRoom, error)
}

// sitemapVerificationStore lists the verification records (/v/ID) that are
// not hidden, newest first, capped (board.SitemapVerificationsMax).
type sitemapVerificationStore interface {
	PublicSitemapVerificationCount(context.Context) (int, error)
	PublicSitemapVerifications(context.Context, int, int) ([]board.SitemapRoom, error)
}

func sitemapPath(p string) bool { return p == "/sitemap.xml" || sitemapFile.MatchString(p) }

// sitemapFixedPaths are the pages listed ahead of rooms and posts.
func (s *Server) sitemapFixedPaths(ctx context.Context) []string {
	// /work and /work/ID stay live -- with /delegation/ID they are the only
	// human-readable proof that the signed transition story is real. The
	// listing is not offered for indexing, but open work with a reward is:
	// "paid tasks for AI agents" is a search agents make (sitemapRewardedWork).
	// The fixed part is the one table of pages' indexable rows with their
	// families (web.SiteNav): the tools and jobs this deployment serves, the
	// /for pages and the guides not yet moved into posts.
	fixed := web.IndexedNavPaths(ctx, s.service, s.cfg.Features)
	return append(fixed, s.sitemapRewardedWork(ctx)...)
}

// sitemapRewardedWork is /work/ID for the first page of open work with a
// reward in public rooms, seeded demonstrations left out; none when the read
// fails, so the sitemap never waits on it.
func (s *Server) sitemapRewardedWork(ctx context.Context) []string {
	res, err := s.service.Execute(ctx, board.Command{Operation: "works.list", Kind: board.WorkKindRewarded, Limit: board.DirectoryPageMax}, "web-public-read")
	if err != nil {
		return nil
	}
	works, _ := res.Data["works"].([]board.Work)
	var out []string
	for _, item := range works {
		if !item.Simulated && item.Reward != nil && validWorkPathID(item.ID) {
			out = append(out, "/work/"+item.ID)
		}
	}
	return out
}

// validWorkPathID is a work's ID as /work/ID takes it: 32 lowercase hex.
func validWorkPathID(id string) bool {
	return len(id) == 32 && strings.Trim(id, "0123456789abcdef") == ""
}

type sitemapCopy struct {
	body []byte
	at   time.Time
}

// sitemap serves the requested file's copy while it is sitemapFresh, else
// builds it; when a build fails (the database was busy past the read
// timeout) it serves the last good copy of that file instead of a 503, so
// crawlers rarely see an error.
func (s *Server) sitemap(w http.ResponseWriter, r *http.Request) {
	if cached, found := s.sitemaps.Load(r.URL.Path); found && time.Since(cached.(sitemapCopy).at) < sitemapFresh {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		if r.Method != http.MethodHead {
			_, _ = w.Write(cached.(sitemapCopy).body)
		}
		return
	}
	rec := &sitemapRecorder{header: http.Header{}}
	s.buildSitemap(rec, r)
	ok := rec.status == 0 || rec.status == http.StatusOK
	if ok {
		s.sitemaps.Store(r.URL.Path, sitemapCopy{bytes.Clone(rec.body.Bytes()), time.Now()})
	} else if cached, found := s.sitemaps.Load(r.URL.Path); found && rec.status != http.StatusNotFound {
		rec = &sitemapRecorder{header: http.Header{"Content-Type": {"application/xml; charset=utf-8"}}}
		rec.body.Write(cached.(sitemapCopy).body)
		ok = true
	}
	for k, v := range rec.header {
		w.Header()[k] = v
	}
	if !ok {
		w.WriteHeader(rec.status)
	}
	if r.Method != http.MethodHead || !ok {
		_, _ = w.Write(rec.body.Bytes())
	}
}

// sitemapRecorder captures one build so it can be cached.
type sitemapRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *sitemapRecorder) Header() http.Header         { return r.header }
func (r *sitemapRecorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *sitemapRecorder) WriteHeader(status int)      { r.status = status }

func (s *Server) buildSitemap(w http.ResponseWriter, r *http.Request) {
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
	rooms, subjects, posts := 0, 0, 0
	if ok {
		var err error
		if rooms, posts, err = store.PublicSitemapCounts(r.Context()); err != nil {
			sitemapUnavailable(w)
			return
		}
	}
	subjectStore, _ := s.service.(sitemapSubjectStore)
	if subjectStore != nil {
		var err error
		if subjects, err = subjectStore.PublicSitemapSubjectCount(r.Context()); err != nil {
			sitemapUnavailable(w)
			return
		}
	}
	verifications := 0
	verificationStore, _ := s.service.(sitemapVerificationStore)
	if verificationStore != nil {
		var err error
		if verifications, err = verificationStore.PublicSitemapVerificationCount(r.Context()); err != nil {
			sitemapUnavailable(w)
			return
		}
	}
	total := len(fixed) + rooms + subjects + verifications + posts
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
		if err := s.sitemapURLs(r.Context(), &out, store, subjectStore, verificationStore, fixed, rooms, subjects, verifications, start, end); err != nil {
			sitemapUnavailable(w)
			return
		}
		out.WriteString("</urlset>")
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = w.Write(out.Bytes())
}

// sitemapURLs writes addresses [start,end) of the one ordered list: fixed
// pages, then rooms by name, then listed subjects in the order they were
// listed, then verification records newest first, then thread roots in
// posting order.
func (s *Server) sitemapURLs(ctx context.Context, out *bytes.Buffer, store sitemapStore, subjectStore sitemapSubjectStore, verificationStore sitemapVerificationStore, fixed []string, rooms, subjects, verifications, start, end int) error {
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
	if from, to := max(start-head, 0), min(end-head, subjects); subjectStore != nil && from < to {
		list, err := subjectStore.PublicSitemapSubjects(ctx, from, to-from)
		if err != nil {
			return err
		}
		for _, sub := range list {
			entry("/s/"+url.PathEscape(sub.Name), sub.Modified)
		}
	}
	head += subjects
	if from, to := max(start-head, 0), min(end-head, verifications); verificationStore != nil && from < to {
		list, err := verificationStore.PublicSitemapVerifications(ctx, from, to-from)
		if err != nil {
			return err
		}
		for _, v := range list {
			if board.ValidVerificationID(v.Name) {
				entry(board.VerificationPath+v.Name, v.Modified)
			}
		}
	}
	head += verifications
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
