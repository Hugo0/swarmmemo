package board

import (
	"bytes"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strings"
)

// Inline rendering is the only place uploaded bytes reach a browser as something
// other than a download, so the type is decided by the bytes and never by what
// the uploader declared. Three raster formats, all decodable by the standard
// library: SVG is excluded because it is a document that can carry script, and
// WebP because decoding it would mean a third-party decoder.
const (
	// InlineImagePixels bounds a decompression bomb: a small file can declare an
	// enormous canvas, and the browser, not the server, pays for it.
	InlineImagePixels = 40 << 20

	inlineImageFormats = "PNG, JPEG or GIF"
)

// ImageMediaType returns the media type to serve bytes as, or "" when they are
// not an image this service will render inline. It reads headers only.
func ImageMediaType(data []byte) string {
	var declared string
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		declared = "image/png"
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		declared = "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		declared = "image/gif"
	default:
		return ""
	}
	// DecodeConfig parses the header without materialising pixels, so a bomb is
	// rejected on its declared dimensions rather than by allocating them.
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || "image/"+format != declared {
		return ""
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > InlineImagePixels {
		return ""
	}
	return declared
}

// InlineImageType reports the renderable type for a declared media type, for
// callers that hold metadata but not bytes (the page renderer). The bytes are
// still authoritative at download time; a mismatch degrades to a download, so
// this can be optimistic without being unsafe.
func InlineImageType(mediaType string) string {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(mediaType, ";", 2)[0]))
	switch base {
	case "image/png", "image/jpeg", "image/gif":
		return base
	}
	return ""
}
