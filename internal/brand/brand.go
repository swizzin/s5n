// Package brand draws the s5n mark: `s5n` spelled in a torrent piece bitfield,
// three 5x7 glyphs with one unlit column between them, so it reads as one field
// of pieces rather than a word. The glyph tables below are the mark itself; a
// table that drifts is a different mark.
//
// Two-tone is canonical: the 5 in the theme's accent, s and n in its fg. Every
// drawing is one variant's palette, drawn for the page that variant paints, so no
// drawing has to read on two backgrounds. Nothing here imports lipgloss, huh or
// Bubble Tea: this is pixel work.
package brand

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/theme"
)

// The mark's field, in cells.
const (
	// Cols is three 5-cell glyphs and the two unlit columns between them.
	Cols = 17
	// Rows is the 5's height: every glyph is 5x7.
	Rows = 7
)

// Piece is what one cell of the field is: the glyph it belongs to, or Unlit for
// the columns between letters and the counters inside them.
type Piece uint8

// The four things a cell can be.
const (
	Unlit Piece = iota
	S
	Five
	N
)

// glyphs are the 5x7 bitmaps of the s5n mark, '#' lit and '.' unlit.
//
//nolint:goconst // the glyph rows are the mark's drawing; a repeat across two glyphs is the drawing, not a shared meaning worth a name.
var glyphs = map[Piece][Rows]string{
	S: {
		".....",
		".....",
		".####",
		"#....",
		".###.",
		"....#",
		"####.",
	},
	Five: {
		"#####",
		"#....",
		"####.",
		"....#",
		"....#",
		"#...#",
		".###.",
	},
	N: {
		".....",
		".....",
		"####.",
		"#...#",
		"#...#",
		"#...#",
		"#...#",
	},
}

// words is the reading order of the field: each glyph starts at column i*6,
// and column 5 and column 11 are the unlit pieces between them.
var words = [3]Piece{S, Five, N}

// Field is the mark, row-major: Field()[row][col].
func Field() [Rows][Cols]Piece {
	var out [Rows][Cols]Piece
	for row := 0; row < Rows; row++ {
		for i, p := range words {
			bits := glyphs[p][row]
			for j := 0; j < 5; j++ {
				if bits[j] == '#' {
					out[row][i*6+j] = p
				}
			}
		}
	}
	return out
}

// Look is one drawing of the mark: the ink for the 5, for s and n, and for the
// unlit pieces, which are the variant's dim. Every piece is opaque, because the
// page behind the field is the variant's own background.
type Look struct {
	Five    color.NRGBA
	Letters color.NRGBA
	Unlit   color.NRGBA
}

// LookOf is a variant's own two-tone: the 5 in the accent, s and n in the fg,
// unlit pieces in the dim.
func LookOf(v theme.Variant) Look {
	return Look{
		Five:    mustHex(v.Accent),
		Letters: mustHex(v.FG),
		Unlit:   mustHex(v.Dim),
	}
}

// minLitContrast is the WCAG ratio a lit cell must reach on its page: 3:1 is the
// threshold for a graphic, and the mark is a picture, not text.
const minLitContrast = 3.0

// MarkPNG draws the mark at one pixel per cell within a cell-sized square, on a
// transparent background.
//
// No gap between cells: at the 64 px width Authelia draws the logo
// (web/src/layouts/LoginLayout.tsx:47-51, `className="mx-auto my-2 w-16"`, and
// MinimalLayout.tsx:48), a 1 px gap on a 17-cell field is a sub-pixel smear. The
// SVG keeps the gap, because the README draws it at 40 px a cell.
func MarkPNG(l Look, cell int) ([]byte, error) {
	if cell < 1 {
		return nil, errCellSize(cell)
	}
	img := image.NewNRGBA(image.Rect(0, 0, Cols*cell, Rows*cell))
	field := Field()
	for row := 0; row < Rows; row++ {
		for col := 0; col < Cols; col++ {
			fill(img, col*cell, row*cell, cell, l.ink(field[row][col]))
		}
	}
	return encodePNG(img)
}

// MarkSVG is the mark as one SVG document for a README: rounded pieces with a gap,
// on a transparent background. The radius is 0.17 of a cell, which makes a lit
// cell read as a piece rather than a pixel at this size.
func MarkSVG(l Look, cell, gap int) ([]byte, error) {
	if cell < 1 || gap < 0 {
		return nil, errCellSize(cell)
	}
	width := (Cols-1)*(cell+gap) + cell
	height := (Rows-1)*(cell+gap) + cell
	radius := strconv.FormatFloat(0.17*float64(cell), 'f', 2, 64)
	field := Field()
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" width="`)
	b.WriteString(strconv.Itoa(width))
	b.WriteString(`" height="`)
	b.WriteString(strconv.Itoa(height))
	b.WriteString(`" viewBox="0 0 `)
	b.WriteString(strconv.Itoa(width))
	b.WriteString(" ")
	b.WriteString(strconv.Itoa(height))
	b.WriteString(`">`)
	for row := 0; row < Rows; row++ {
		for col := 0; col < Cols; col++ {
			b.WriteString("\n  <rect x=\"")
			b.WriteString(strconv.Itoa(col * (cell + gap)))
			b.WriteString(`" y="`)
			b.WriteString(strconv.Itoa(row * (cell + gap)))
			b.WriteString(`" width="`)
			b.WriteString(strconv.Itoa(cell))
			b.WriteString(`" height="`)
			b.WriteString(strconv.Itoa(cell))
			b.WriteString(`" rx="`)
			b.WriteString(radius)
			b.WriteString(`" `)
			b.WriteString(svgAttrs(l.ink(field[row][col])))
			b.WriteString(`/>`)
		}
	}
	b.WriteString("\n</svg>\n")
	return []byte(b.String()), nil
}

// ink is the colour a cell is drawn in.
func (l Look) ink(p Piece) color.NRGBA {
	switch p {
	case Five:
		return l.Five
	case S, N:
		return l.Letters
	default:
		return l.Unlit
	}
}

// The favicon is the 5 alone, pixel-exact so nothing is antialiased: 2 px cells
// with no gap at 16, 3 px with a 1 px gap at 32. Below 64 px the field's unlit
// pieces drop out, so the favicon is one glyph, not the word.
var faviconCells = map[int]struct{ cell, gap int }{
	16: {2, 0},
	32: {3, 1},
}

// FaviconSizes are the sizes an ICO carries, smallest first.
var FaviconSizes = []int{16, 32}

// FaviconPNG draws the 5 in one ink on a transparent tile. Only 16 and 32 exist:
// any other size is a call with no drawing behind it.
func FaviconPNG(ink color.NRGBA, size int) ([]byte, error) {
	geom, ok := faviconCells[size]
	if !ok {
		return nil, errFaviconSize(size)
	}
	glyph := glyphs[Five]
	w := 5*geom.cell + 4*geom.gap
	h := 7*geom.cell + 6*geom.gap
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	ox := (size - w) / 2
	oy := (size - h) / 2
	for row := 0; row < 7; row++ {
		for col := 0; col < 5; col++ {
			if glyph[row][col] != '#' {
				continue
			}
			fill(img, ox+col*(geom.cell+geom.gap), oy+row*(geom.cell+geom.gap), geom.cell, ink)
		}
	}
	return encodePNG(img)
}

// FaviconICO is favicon.ico: an ICONDIR with one PNG payload per size. It is
// written by hand because the standard library has no ICO encoder; a browser
// reads a PNG payload inside an ICO (allowed since Windows Vista), and Authelia
// serves the file as-is (internal/server/handlers.go:171, `r.GET("/favicon.ico",
// ...)`).
func FaviconICO(ink color.NRGBA) ([]byte, error) {
	payloads := make([][]byte, 0, len(FaviconSizes))
	for _, size := range FaviconSizes {
		data, err := FaviconPNG(ink, size)
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, data)
	}
	var b bytes.Buffer
	// ICONDIR: reserved, type (1 = icon), count.
	putU16(&b, 0)
	putU16(&b, 1)
	putU16(&b, uint16(len(payloads)))
	offset := 6 + 16*len(payloads)
	for i, data := range payloads {
		size := FaviconSizes[i]
		b.WriteByte(byte(size)) // width; 0 would mean 256
		b.WriteByte(byte(size)) // height
		b.WriteByte(0)          // palette size
		b.WriteByte(0)          // reserved
		putU16(&b, 1)           // colour planes
		putU16(&b, 32)          // bits per pixel
		putU32(&b, uint32(len(data)))
		putU32(&b, uint32(offset))
		offset += len(data)
	}
	for _, data := range payloads {
		b.Write(data)
	}
	return b.Bytes(), nil
}

// fill paints a square of one ink, no blending, so every edge lands on a pixel
// boundary and the drawing is exact at 16 px.
func fill(img *image.NRGBA, x, y, size int, ink color.NRGBA) {
	for dy := 0; dy < size; dy++ {
		for dx := 0; dx < size; dx++ {
			img.SetNRGBA(x+dx, y+dy, ink)
		}
	}
}

func encodePNG(img image.Image) ([]byte, error) {
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func putU16(b *bytes.Buffer, v uint16) {
	var raw [2]byte
	binary.LittleEndian.PutUint16(raw[:], v)
	b.Write(raw[:])
}

func putU32(b *bytes.Buffer, v uint32) {
	var raw [4]byte
	binary.LittleEndian.PutUint32(raw[:], v)
	b.Write(raw[:])
}

// Error is a drawing request the design has no answer for.
type Error struct{ msg string }

func (e *Error) Error() string { return "brand: " + e.msg }

func errCellSize(cell int) error {
	return &Error{msg: "cell size " + strconv.Itoa(cell) + ": the mark is drawn at one pixel per cell or more"}
}

func errFaviconSize(size int) error {
	return &Error{msg: "favicon size " + strconv.Itoa(size) + ": the drawing exists at 16 and 32 only"}
}
