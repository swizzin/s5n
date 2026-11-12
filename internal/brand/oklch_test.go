package brand

import (
	"image/color"
	"math"
)

// The OKLCH conversion below exists only so a test can recompute Authelia's page
// colours from its own CSS (oklch values) and catch them drifting from the hex
// the contrast checks use. Nothing box draws needs it.

// srgbOfOKLCH is the colour at one lightness with a hue and chroma, chroma
// reduced as far as the sRGB gamut needs so the result is a colour and not a
// clamped one wearing its hue.
func srgbOfOKLCH(l, chroma, hue float64) color.NRGBA {
	rad := hue * math.Pi / 180
	cos, sin := math.Cos(rad), math.Sin(rad)

	// The largest chroma that stays inside the gamut, by bisection: 30 steps
	// resolve far below an 8-bit channel.
	lo, hi := 0.0, chroma
	for i := 0; i < 30; i++ {
		mid := (lo + hi) / 2
		if inGamut(oklabToLinear(l, mid*cos, mid*sin)) {
			lo = mid
		} else {
			hi = mid
		}
	}
	r, g, b := oklabToLinear(l, lo*cos, lo*sin)
	return color.NRGBA{R: encode(r), G: encode(g), B: encode(b), A: 0xFF}
}

// oklabToLinear is OKLab to linear sRGB, unclamped so the caller can see
// whether a chroma is inside the gamut.
func oklabToLinear(l, a, b float64) (r, g, bl float64) {
	ll := l + 0.3963377774*a + 0.2158037573*b
	mm := l - 0.1055613458*a - 0.0638541728*b
	ss := l - 0.0894841775*a - 1.2914855480*b

	ll, mm, ss = ll*ll*ll, mm*mm*mm, ss*ss*ss

	return 4.0767416621*ll - 3.3077115913*mm + 0.2309699292*ss,
		-1.2684380046*ll + 2.6097574011*mm - 0.3413193965*ss,
		-0.0041960863*ll - 0.7034186147*mm + 1.7076147010*ss
}

func inGamut(r, g, b float64) bool {
	const eps = 1e-6
	for _, v := range []float64{r, g, b} {
		if v < -eps || v > 1+eps {
			return false
		}
	}
	return true
}

// encode is sRGB's transfer function, linear to encoded, clamped to a channel.
func encode(f float64) uint8 {
	f = math.Max(0, math.Min(1, f))
	if f <= 0.0031308 {
		return uint8(math.Round(f * 12.92 * 255))
	}
	return uint8(math.Round((1.055*math.Pow(f, 1/2.4) - 0.055) * 255))
}
