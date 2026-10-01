package web

import "mime"

// The launch film (assets/film-*, templates/page.html "film", assets/film.js). The production image
// has no mime.types, so the static handler's types for the film's files are registered here rather
// than left to content sniffing.
func init() {
	for ext, typ := range map[string]string{".mp4": "video/mp4", ".txt": "text/plain; charset=utf-8"} {
		_ = mime.AddExtensionType(ext, typ)
	}
}

// FilmAssets are the film's files under /assets/, each with the type it is served as.
var FilmAssets = map[string]string{
	"film-16x9.mp4":        "video/mp4",
	"film-1x1.mp4":         "video/mp4",
	"film-poster-16x9.jpg": "image/jpeg",
	"film-poster-1x1.jpg":  "image/jpeg",
	"film-transcript.txt":  "text/plain; charset=utf-8",
	"film.js":              "text/javascript; charset=utf-8",
}
