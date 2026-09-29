package cards

import (
	"embed"
	"encoding/binary"
	"fmt"
	"sort"
)

// The local renderer's two faces are fixed bitmap fonts compiled into the
// binary (see font/OFL.txt for their origin and license), so a card looks the
// same on every host and drawing one reads no file and loads no library.
//
// Format (little endian): "SMF1", u8 width, u8 height, u16 glyphs, u16 runes,
// then runes entries of (u32 rune, u16 glyph) sorted by rune, then the glyph
// bitmaps, ceil(width/8) bytes per row, most significant bit leftmost.

//go:embed font/regular.smf font/bold.smf
var fontFiles embed.FS

type face struct {
	w, h, stride int
	runes        []rune
	index        []uint16
	glyphs       []byte
	fallback     int
}

var regular, bold = mustFace("font/regular.smf"), mustFace("font/bold.smf")

func mustFace(name string) *face {
	raw, err := fontFiles.ReadFile(name)
	if err != nil {
		panic(err)
	}
	f, err := parseFace(raw)
	if err != nil {
		panic(fmt.Sprintf("%s: %v", name, err))
	}
	return f
}

func parseFace(raw []byte) (*face, error) {
	if len(raw) < 10 || string(raw[:4]) != "SMF1" {
		return nil, fmt.Errorf("not a card font")
	}
	f := &face{w: int(raw[4]), h: int(raw[5])}
	f.stride = (f.w + 7) / 8
	glyphs, runes := int(binary.LittleEndian.Uint16(raw[6:])), int(binary.LittleEndian.Uint16(raw[8:]))
	table := raw[10:]
	if len(table) != runes*6+glyphs*f.stride*f.h || f.w == 0 || f.h == 0 {
		return nil, fmt.Errorf("truncated card font")
	}
	for i := 0; i < runes; i++ {
		entry := table[i*6:]
		g := binary.LittleEndian.Uint16(entry[4:])
		if int(g) >= glyphs {
			return nil, fmt.Errorf("glyph index out of range")
		}
		f.runes = append(f.runes, rune(binary.LittleEndian.Uint32(entry)))
		f.index = append(f.index, g)
	}
	if !sort.SliceIsSorted(f.runes, func(i, j int) bool { return f.runes[i] < f.runes[j] }) {
		return nil, fmt.Errorf("unsorted card font")
	}
	f.glyphs = table[runes*6:]
	f.fallback = -1
	for _, r := range []rune{'�', '?'} {
		if g, ok := f.lookup(r); ok {
			f.fallback = g
			break
		}
	}
	if f.fallback < 0 {
		return nil, fmt.Errorf("card font has no fallback glyph")
	}
	return f, nil
}

func (f *face) lookup(r rune) (int, bool) {
	i := sort.Search(len(f.runes), func(i int) bool { return f.runes[i] >= r })
	if i < len(f.runes) && f.runes[i] == r {
		return int(f.index[i]), true
	}
	return 0, false
}

// bitmap is a rune's glyph rows; a rune the font lacks draws the fallback.
func (f *face) bitmap(r rune) []byte {
	g, ok := f.lookup(r)
	if !ok {
		g = f.fallback
	}
	size := f.stride * f.h
	return f.glyphs[g*size : (g+1)*size]
}
