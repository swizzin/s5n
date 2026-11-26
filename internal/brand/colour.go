package brand

import (
	"errors"
	"image/color"
	"math"
	"strconv"
)

// Background is one of Authelia's page colours: its name, the CSS value the page
// paints, and that value as sRGB, which is what a PNG can draw.
type Background struct {
	Name  string
	OKLCH string
	Hex   string
}

// RGBA is the background as a colour.
func (b Background) RGBA() color.NRGBA { return mustHex(b.Hex) }

// Authelia's page backgrounds at the tag s5n pins (authelia 4.39.28).
//
// web/src/themes/tailwind-themes.css defines `--background` per theme and
// web/src/index.css:15-16 applies it to `body`, so it is the colour behind the
// logo on the login page, which is the first child of the page's own column and
// not on a card (web/src/layouts/LoginLayout.tsx:97).
//
//	light: oklch(1 0 0)     = #FFFFFF   (tailwind-themes.css:17, [data-theme="light"])
//	dark:  oklch(0.311 0 0) = #303030   (tailwind-themes.css:66, [data-theme="dark"])
//
// Authelia also ships grey (:115) and oled (:164), which box never writes:
// RenderConfiguration refuses a theme that is not light or dark, so these two
// drawings are the ones the pages are checked against.
var (
	AutheliaLight = Background{Name: "light", OKLCH: "oklch(1 0 0)", Hex: "#FFFFFF"}
	AutheliaDark  = Background{Name: "dark", OKLCH: "oklch(0.311 0 0)", Hex: "#303030"}
)

// AutheliaBackgrounds is every page box can put the mark on, light first.
func AutheliaBackgrounds() []Background {
	return []Background{AutheliaLight, AutheliaDark}
}

// Contrast is WCAG 2.x's contrast ratio between two colours: 1 is the same and 21
// black on white. Alpha is ignored: the inks and pages here are opaque.
func Contrast(a, b color.NRGBA) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// luminance is WCAG's relative luminance: linearised channels, standard observer
// weights.
func luminance(c color.NRGBA) float64 {
	lin := func(v uint8) float64 {
		f := float64(v) / 255
		if f <= 0.03928 {
			return f / 12.92
		}
		return math.Pow((f+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.R) + 0.7152*lin(c.G) + 0.0722*lin(c.B)
}

// Hex is a colour as #rrggbb.
func Hex(c color.NRGBA) string {
	return "#" + hexByte(c.R) + hexByte(c.G) + hexByte(c.B)
}

// svgAttrs is a colour as an SVG fill; every piece is opaque, so there is no
// fill-opacity to write.
func svgAttrs(c color.NRGBA) string {
	return `fill="` + Hex(c) + `"`
}

// ParseHex reads #rrggbb.
func ParseHex(s string) (color.NRGBA, error) {
	if len(s) != 7 || s[0] != '#' {
		return color.NRGBA{}, errors.New("brand: " + strconv.Quote(s) + " is not #rrggbb")
	}
	var channels [3]uint8
	for i := range channels {
		v, err := strconv.ParseUint(s[1+i*2:3+i*2], 16, 8)
		if err != nil {
			return color.NRGBA{}, errors.New("brand: " + strconv.Quote(s) + " is not #rrggbb")
		}
		channels[i] = uint8(v)
	}
	return color.NRGBA{R: channels[0], G: channels[1], B: channels[2], A: 0xFF}, nil
}

// mustHex is ParseHex for a palette value, which internal/theme's own tests
// already refuse to let be malformed.
func mustHex(s string) color.NRGBA {
	c, err := ParseHex(s)
	if err != nil {
		panic(err)
	}
	return c
}

func hexByte(v uint8) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[v>>4], digits[v&0x0F]})
}
