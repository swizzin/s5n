// Command brand writes the README's copies of the s5n mark.
//
//	go run ./hack/brand
//
// One file per page it is read on, because a mark is drawn in the palette of
// the page behind it: docs/brand/s5n-light.svg is the s5n light variant, and
// docs/brand/s5n-dark.svg the dark one. Both are two-tone -- the 5 in the
// accent, s and n in the fg -- and both have a transparent background, so a
// README can `<picture>` them by `prefers-color-scheme` instead of painting a
// rectangle that only matches one reader's page.
//
// The files are generated rather than drawn by hand: the glyph tables live in
// internal/brand, and an SVG checked in beside a table nothing reads is an SVG
// that stops being the mark the next time the table moves.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/swizzin/s5n/internal/authelia"
	"github.com/swizzin/s5n/internal/brand"
)

// outDir is where the two files land, relative to the repository root.
const outDir = "docs/brand"

// The README's geometry: 40 px cells with an 8 px gap, the size the brand
// sheets drew, where a piece reads as a piece rather than as a pixel.
const (
	cell = 40
	gap  = 8
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "brand: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	for _, variant := range []string{"s5n-dark", "s5n-light"} {
		lt, err := authelia.ResolveTheme(variant)
		if err != nil {
			return err
		}
		svg, err := brand.MarkSVG(lt.Look(), cell, gap)
		if err != nil {
			return err
		}
		path := filepath.Join(outDir, variant+".svg")
		if err := os.WriteFile(path, svg, 0o644); err != nil {
			return err
		}
		fmt.Println(path)
	}
	return nil
}
