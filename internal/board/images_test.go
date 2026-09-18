package board

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
)

func sampleImage(t *testing.T, format string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(1, 1, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&buf, img)
	case "jpeg":
		err = jpeg.Encode(&buf, img, nil)
	case "gif":
		err = gif.Encode(&buf, img, nil)
	}
	if err != nil {
		t.Fatalf("encode %s: %v", format, err)
	}
	return buf.Bytes()
}

func TestImageMediaTypeTrustsBytesNotClaims(t *testing.T) {
	for format, want := range map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif"} {
		if got := ImageMediaType(sampleImage(t, format)); got != want {
			t.Errorf("%s: got %q, want %q", format, got, want)
		}
	}
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	html := []byte("<!doctype html><script>alert(1)</script>")
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 16)...)
	// A GIF polyglot: real GIF magic, then markup. It stays an image/gif, which is
	// the point — nothing downstream may treat it as a document.
	polyglot := append(sampleImage(t, "gif"), html...)
	for name, tc := range map[string]struct {
		data []byte
		want string
	}{
		"svg":              {svg, ""},
		"html":             {html, ""},
		"webp":             {webp, ""},
		"empty":            {nil, ""},
		"truncated png":    {sampleImage(t, "png")[:8], ""},
		"png magic only":   {[]byte("\x89PNG\r\n\x1a\nnot really a png"), ""},
		"gif with markup":  {polyglot, "image/gif"},
		"jpeg magic only":  {[]byte{0xFF, 0xD8, 0xFF, 0x00}, ""},
		"declared mislead": {sampleImage(t, "png"), "image/png"},
	} {
		if got := ImageMediaType(tc.data); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

func TestImageMediaTypeRefusesADecompressionBomb(t *testing.T) {
	// A tiny GIF header can declare a canvas of billions of pixels.
	bomb := []byte("GIF89a\xff\xff\xff\xff\x80\x00\x00\x00\x00\x00\xff\xff\xff,\x00\x00\x00\x00\xff\xff\xff\xff\x00\x02\x02D\x01\x00;")
	if config, _, err := image.DecodeConfig(bytes.NewReader(bomb)); err == nil {
		if int64(config.Width)*int64(config.Height) <= InlineImagePixels {
			t.Skipf("fixture is not oversized: %dx%d", config.Width, config.Height)
		}
	}
	if got := ImageMediaType(bomb); got != "" {
		t.Fatalf("oversized canvas served inline as %q", got)
	}
}

func TestInlineImageTypeNeverRendersSVG(t *testing.T) {
	for _, declared := range []string{"image/svg+xml", "image/svg+xml; charset=utf-8", "text/html", "image/webp", "application/octet-stream", ""} {
		if got := InlineImageType(declared); got != "" {
			t.Errorf("%q would render inline as %q", declared, got)
		}
	}
	if got := InlineImageType("IMAGE/PNG; charset=binary"); got != "image/png" {
		t.Errorf("declared png not recognised: %q", got)
	}
}
