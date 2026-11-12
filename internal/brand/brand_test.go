package brand

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/theme"
)

// wantField is the mark as a person reads it, '#' for a lit piece and '.' for
// an unlit one, one string per row. It is written out rather than computed so
// the test is a second copy of the drawing: a changed glyph table fails here
// and the reviewer can see what the mark is supposed to look like.
var wantField = [Rows]string{
	"......#####......",
	"......#..........",
	".####.####..####.",
	"#.........#.#...#",
	".###......#.#...#",
	"....#.#...#.#...#",
	"####...###..#...#",
}

// TestFieldIsTheBrandTable prints the bitfield and compares it to the literal
// above. The columns between the glyphs are unlit pieces, not gaps: that is
// what makes three letters read as one continuous field.
func TestFieldIsTheBrandTable(t *testing.T) {
	field := Field()
	var got [Rows]string
	for row := 0; row < Rows; row++ {
		var b strings.Builder
		for col := 0; col < Cols; col++ {
			if field[row][col] == Unlit {
				b.WriteByte('.')
				continue
			}
			b.WriteByte('#')
		}
		got[row] = b.String()
	}
	for row := 0; row < Rows; row++ {
		t.Logf("%s", got[row])
		if got[row] != wantField[row] {
			t.Errorf("row %d = %q, want %q", row, got[row], wantField[row])
		}
	}
	if len(field) != Rows || len(field[0]) != Cols {
		t.Fatalf("field is %dx%d, want %dx%d", len(field[0]), len(field), Cols, Rows)
	}
	// The separator columns are unlit in every row, and nothing else is: a lit
	// piece in column 5 would join the s to the 5.
	for row := 0; row < Rows; row++ {
		for _, col := range []int{5, 11} {
			if field[row][col] != Unlit {
				t.Errorf("row %d column %d is lit: the glyphs run together", row, col)
			}
		}
	}
}

// darkLook is the s5n dark variant's two-tone, the drawing every pixel test
// below reads.
func darkLook() Look { return LookOf(theme.Default().Dark) }

// TestMarkPNG checks the raster the login page shows: the sizes are the cell
// count times the cell, the background is transparent (the page shows through),
// and each piece is drawn in its own ink.
func TestMarkPNG(t *testing.T) {
	const cell = 8
	look := darkLook()
	data, err := MarkPNG(look, cell)
	if err != nil {
		t.Fatalf("MarkPNG: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := img.Bounds().Dx(), Cols*cell; got != want {
		t.Errorf("width = %d, want %d", got, want)
	}
	if got, want := img.Bounds().Dy(), Rows*cell; got != want {
		t.Errorf("height = %d, want %d", got, want)
	}

	// One pixel from the middle of a cell, not its corner, so a mistake in the
	// cell arithmetic cannot pass as a NEAREST-neighbour coincidence.
	at := func(row, col int) color.NRGBA {
		return color.NRGBAModel.Convert(img.At(col*cell+cell/2, row*cell+cell/2)).(color.NRGBA)
	}
	// Row 0: the 5's top bar is lit, and it is the accent.
	if got := at(0, 6); got != look.Five {
		t.Errorf("the 5's top bar = %s, want the accent %s", Hex(got), Hex(look.Five))
	}
	// Row 3, column 0 is the s's left stroke: the letters take the fg.
	if got := at(3, 0); got != look.Letters {
		t.Errorf("an s cell = %s, want the fg %s", Hex(got), Hex(look.Letters))
	}
	// Row 0, column 0 is an unlit piece above the s.
	if got := at(0, 0); got != look.Unlit {
		t.Errorf("an unlit cell = %s, want the dim %s", Hex(got), Hex(look.Unlit))
	}
	// The separator column too: unlit, drawn, not a hole in the field.
	if got := at(3, 5); got != look.Unlit {
		t.Errorf("the separator column = %s, want the dim %s", Hex(got), Hex(look.Unlit))
	}
	// Every cell is painted, so nothing in the canvas is transparent: the logo
	// is the field, and the page behind it is the background.
	for row := 0; row < Rows; row++ {
		for col := 0; col < Cols; col++ {
			if got := at(row, col); got.A != 0xFF {
				t.Fatalf("cell %d,%d is %d%% opaque, want the field to cover the canvas", row, col, got.A)
			}
		}
	}
}

// TestFaviconPNG is the pixel-exact favicon: 16 px is 2 px cells with no gap,
// 32 px is 3 px cells with a 1 px gap, and the 5 is centred in the tile.
func TestFaviconPNG(t *testing.T) {
	ink := darkLook().Five
	for _, size := range FaviconSizes {
		data, err := FaviconPNG(ink, size)
		if err != nil {
			t.Fatalf("%d: %v", size, err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%d: decode: %v", size, err)
		}
		if img.Bounds().Dx() != size || img.Bounds().Dy() != size {
			t.Fatalf("%d: size = %v", size, img.Bounds())
		}
		checkFaviconCells(t, img, size, ink)
		checkFaviconGap(t, img, size, ink)
	}
}

// checkFaviconCells: every lit cell of the 5 is the ink, every unlit cell of
// its 5x7 box is transparent, and the tile's corner -- outside the glyph -- is
// transparent too, so the browser's own chrome shows through.
func checkFaviconCells(t *testing.T, img image.Image, size int, ink color.NRGBA) {
	t.Helper()
	geom := faviconCells[size]
	ox, oy := faviconOrigin(size)
	glyph := glyphs[Five]
	for row := 0; row < 7; row++ {
		for col := 0; col < 5; col++ {
			x := ox + col*(geom.cell+geom.gap) + geom.cell/2
			y := oy + row*(geom.cell+geom.gap) + geom.cell/2
			got := pixel(img, x, y)
			if glyph[row][col] == '#' {
				if got != ink {
					t.Errorf("%d: lit cell %d,%d = %s, want the accent %s", size, row, col, Hex(got), Hex(ink))
				}
				continue
			}
			if got.A != 0 {
				t.Errorf("%d: unlit cell %d,%d is drawn (%d opaque)", size, row, col, got.A)
			}
		}
	}
	if got := pixel(img, 0, 0); got.A != 0 {
		t.Errorf("%d: the tile's corner is painted", size)
	}
	// The drawing is centred: its own bounds start where the centring says.
	minX, minY := size, size
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if pixel(img, x, y).A == 0 {
				continue
			}
			if x < minX {
				minX = x
			}
			if y < minY {
				minY = y
			}
		}
	}
	if minX != ox || minY != oy {
		t.Errorf("%d: the 5 starts at %d,%d, want the centred %d,%d", size, minX, minY, ox, oy)
	}
}

// checkFaviconGap is the one difference between the two sizes: at 16 px cells
// touch, so the pixel between two lit cells in the 5's top bar is lit; at 32 px
// that pixel is the gap, and it is transparent.
func checkFaviconGap(t *testing.T, img image.Image, size int, ink color.NRGBA) {
	t.Helper()
	geom := faviconCells[size]
	ox, oy := faviconOrigin(size)
	between := ox + geom.cell
	if geom.gap == 0 {
		if got := pixel(img, between, oy); got != ink {
			t.Errorf("%d: the no-gap drawing has a hole between cells (%s)", size, Hex(got))
		}
		return
	}
	if got := pixel(img, between, oy+geom.cell/2); got.A != 0 {
		t.Errorf("%d: the 1 px gap between cells is painted (%d opaque)", size, got.A)
	}
}

// faviconOrigin is where the 5 starts in a size-square tile: the rest of the
// tile is the margin that centres it.
func faviconOrigin(size int) (x, y int) {
	geom := faviconCells[size]
	w := 5*geom.cell + 4*geom.gap
	h := 7*geom.cell + 6*geom.gap
	return (size - w) / 2, (size - h) / 2
}

// pixel is one pixel as a non-premultiplied colour, which is what the PNG was
// written with.
func pixel(img image.Image, x, y int) color.NRGBA {
	return color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
}

// TestFaviconICO is the container: two entries, 16 and 32, each a PNG payload
// of the size the entry claims and lands entirely inside the file.
func TestFaviconICO(t *testing.T) {
	ink := darkLook().Five
	data, err := FaviconICO(ink)
	if err != nil {
		t.Fatalf("FaviconICO: %v", err)
	}
	if len(data) < 6 {
		t.Fatalf("%d bytes: shorter than an ICONDIR", len(data))
	}
	if reserved := binary.LittleEndian.Uint16(data[0:]); reserved != 0 {
		t.Errorf("reserved = %d, want 0", reserved)
	}
	if typ := binary.LittleEndian.Uint16(data[2:]); typ != 1 {
		t.Errorf("type = %d, want 1 (icon)", typ)
	}
	count := int(binary.LittleEndian.Uint16(data[4:]))
	if count != len(FaviconSizes) {
		t.Fatalf("count = %d, want %d", count, len(FaviconSizes))
	}
	for i := 0; i < count; i++ {
		entry := data[6+16*i:]
		width, height := int(entry[0]), int(entry[1])
		size := FaviconSizes[i]
		if width != size || height != size {
			t.Errorf("entry %d is %dx%d, want %dx%d", i, width, height, size, size)
		}
		if bpp := binary.LittleEndian.Uint16(entry[6:]); bpp != 32 {
			t.Errorf("entry %d is %d bpp, want 32", i, bpp)
		}
		length := int(binary.LittleEndian.Uint32(entry[8:]))
		offset := int(binary.LittleEndian.Uint32(entry[12:]))
		if offset+length > len(data) {
			t.Fatalf("entry %d runs past the file: %d+%d > %d", i, offset, length, len(data))
		}
		payload, err := png.Decode(bytes.NewReader(data[offset : offset+length]))
		if err != nil {
			t.Fatalf("entry %d is not a PNG: %v", i, err)
		}
		if payload.Bounds().Dx() != size {
			t.Errorf("entry %d decodes to %d px, want %d", i, payload.Bounds().Dx(), size)
		}
	}
}

// TestMarkSVG is the README's copy: one rect per cell, the two-tone inks, all
// of them opaque.
func TestMarkSVG(t *testing.T) {
	look := darkLook()
	data, err := MarkSVG(look, 40, 8)
	if err != nil {
		t.Fatalf("MarkSVG: %v", err)
	}
	svg := string(data)
	if got, want := strings.Count(svg, "<rect "), Cols*Rows; got != want {
		t.Errorf("%d rects, want one per cell (%d)", got, want)
	}
	if !strings.HasPrefix(svg, "<svg xmlns=") || !strings.HasSuffix(svg, "</svg>\n") {
		t.Errorf("not a standalone SVG document: %.60q", svg)
	}
	// 17 cells of 40 with a gap of 8: 16*48+40 wide, 6*48+40 high.
	for _, want := range []string{`width="808"`, `height="328"`, `viewBox="0 0 808 328"`, `rx="6.80"`} {
		if !strings.Contains(svg, want) {
			t.Errorf("the SVG does not carry %s", want)
		}
	}
	// The two inks, and nothing translucent: an SVG is drawn on the page the
	// README is being read on, so the mark is the variant's own colours and a
	// fill-opacity would make the unlit pieces vanish into a dark viewer.
	for _, want := range []string{`fill="` + Hex(look.Five) + `"`, `fill="` + Hex(look.Letters) + `"`, `fill="` + Hex(look.Unlit) + `"`} {
		if !strings.Contains(svg, want) {
			t.Errorf("the SVG does not carry %s", want)
		}
	}
	if strings.Contains(svg, "fill-opacity") {
		t.Error("the SVG writes a fill-opacity: every piece is opaque")
	}
	if _, err := MarkSVG(darkLook(), 0, 0); err == nil {
		t.Error("a zero cell size was accepted")
	}
}

// contrastTable is the step-3 table: every variant's lit inks against the page
// box draws it on. A dark variant is drawn on Authelia's dark page and a light
// variant on its white one -- a pair name is the pair's dark variant, so those
// are the same row twice and there is nothing else the login can show.
func TestEveryVariantIsLegibleOnItsAutheliaPage(t *testing.T) {
	for _, th := range theme.Themes() {
		cases := []struct {
			name string
			look Look
			bg   Background
		}{
			{"pinned dark", LookOf(th.Dark), AutheliaDark},
			{"pinned light", LookOf(th.Light), AutheliaLight},
		}
		for _, c := range cases {
			row := fmt.Sprintf("%-22s %-12s", th.Name, c.name)
			worst := 21.0
			// An opaque ink on the page it is drawn on is what the eye gets;
			// Contrast ignores the alpha of a translucent one, and no drawing
			// here is translucent any more.
			five := Contrast(c.look.Five, c.bg.RGBA())
			letters := Contrast(c.look.Letters, c.bg.RGBA())
			row += fmt.Sprintf("  %s on %-5s 5:%5.2f s/n:%5.2f", Hex(c.look.Five), c.bg.Name, five, letters)
			if five < worst {
				worst = five
			}
			if letters < worst {
				worst = letters
			}
			if five+1e-9 < minLitContrast {
				t.Errorf("%s %s: the 5 is %.2f:1 on Authelia's %s page", th.Name, c.name, five, c.bg.Name)
			}
			if letters+1e-9 < minLitContrast {
				t.Errorf("%s %s: s and n are %.2f:1 on Authelia's %s page", th.Name, c.name, letters, c.bg.Name)
			}
			t.Logf("%s  worst %.2f:1", row, worst)
		}
	}
}

// TestPinnedLookIsTheVariant: a pinned variant draws its own palette, not a
// derivation. The 5 is the accent, s and n the fg, unlit pieces the dim.
func TestPinnedLookIsTheVariant(t *testing.T) {
	for _, th := range theme.Themes() {
		for _, v := range []theme.Variant{th.Dark, th.Light} {
			look := LookOf(v)
			if got, want := Hex(look.Five), strings.ToLower(v.Accent); got != want {
				t.Errorf("%s-%s: the 5 is %s, want the accent %s", th.Name, v.Name, got, want)
			}
			if got, want := Hex(look.Letters), strings.ToLower(v.FG); got != want {
				t.Errorf("%s-%s: s and n are %s, want the fg %s", th.Name, v.Name, got, want)
			}
			if got, want := Hex(look.Unlit), strings.ToLower(v.Dim); got != want {
				t.Errorf("%s-%s: unlit pieces are %s, want the dim %s", th.Name, v.Name, got, want)
			}
			if look.Unlit.A != 0xFF {
				t.Errorf("%s-%s: a pinned variant's unlit pieces are translucent", th.Name, v.Name)
			}
		}
	}
}

// TestAutheliaBackgroundsAreTheirOKLCH recomputes each page colour from the
// CSS value Authelia ships, so the constants the contrast test trusts cannot
// drift from the source they were read at.
func TestAutheliaBackgroundsAreTheirOKLCH(t *testing.T) {
	for _, bg := range AutheliaBackgrounds() {
		var l, c, h float64
		if _, err := fmt.Sscanf(bg.OKLCH, "oklch(%g %g %g)", &l, &c, &h); err != nil {
			t.Fatalf("%s: %q: %v", bg.Name, bg.OKLCH, err)
		}
		if got := Hex(srgbOfOKLCH(l, c, h)); !strings.EqualFold(got, bg.Hex) {
			t.Errorf("%s: %s converts to %s, want %s", bg.Name, bg.OKLCH, got, bg.Hex)
		}
	}
	if got := Hex(AutheliaDark.RGBA()); got != "#303030" {
		t.Errorf("the dark page is %s", got)
	}
}

// TestFaviconRejectsWhatItCannotDraw: a size with no cell geometry is a bug in
// the caller, not a picture to invent.
func TestFaviconRejectsWhatItCannotDraw(t *testing.T) {
	if _, err := FaviconPNG(darkLook().Five, 48); err == nil {
		t.Error("a 48 px favicon was drawn")
	}
	if _, err := MarkPNG(darkLook(), 0); err == nil {
		t.Error("a zero cell mark was drawn")
	}
	if _, err := ParseHex("3DD6B5"); err == nil {
		t.Error("a colour without # was accepted")
	}
}
