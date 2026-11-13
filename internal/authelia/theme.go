package authelia

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/swizzin/s5n/internal/brand"
	"github.com/swizzin/s5n/internal/theme"
)

// Authelia's own theme names box writes. Its validator also knows grey and
// oled (internal/configuration/validator/const.go:548, `validThemeNames =
// []string{"light", "dark", "grey", "oled", auto}`), which box never writes: a
// seedbox has one page, and the mark is drawn for it.
const (
	BaseLight = "light"
	BaseDark  = "dark"
)

// LoginTheme is what the values file's `theme:` means to the login: Authelia's
// own setting, and the palette the mark and the favicon are drawn from.
//
// box draws its dialogs in whatever the terminal is, so a pair name has two
// palettes; Authelia has no terminal to ask, and the login is one page for
// everyone on the host, so a pair is the dark page drawn in that pair's dark
// variant. A variant name pins the page the operator chose, and the mark
// follows it: the drawing for a pinned light variant is the one that reads on
// a white page. Authelia's own `auto` is never written -- it would hand the
// page's colours to a browser box cannot see, and the one picture box ships
// would have to be legible on both pages at once.
type LoginTheme struct {
	// Base is Authelia's theme: light or dark.
	Base string
	// Palette is the s5n variant it is drawn in: the variant the name pinned,
	// or the pair's dark variant.
	Palette theme.Variant
}

// ErrTheme says the theme cannot be resolved: an unknown name is a startup
// error in box, and a login rendered with a name Authelia does not know is a
// login that never becomes Ready.
var ErrTheme = errors.New("authelia theme")

// ResolveTheme maps a `theme:` value onto Authelia's setting and the palette
// the mark is drawn in.
//
// The mapping is the whole decision:
//
//   - a pair name (s5n, catppuccin) is the dark page, drawn in the pair's dark
//     variant: the login is one page for a whole host, and the operator picked
//     a palette whose dark half is the one box's own dialogs default to;
//   - a pinned dark variant is `dark`, a pinned light variant is `light`.
func ResolveTheme(name string) (LoginTheme, error) {
	sel, err := theme.Parse(name)
	if err != nil {
		return LoginTheme{}, fmt.Errorf("%w: %v", ErrTheme, err)
	}
	switch {
	case sel.Variant == nil:
		return LoginTheme{Base: BaseDark, Palette: sel.Theme.Dark}, nil
	case sel.Variant.Name == sel.Theme.Dark.Name:
		return LoginTheme{Base: BaseDark, Palette: *sel.Variant}, nil
	case sel.Variant.Name == sel.Theme.Light.Name:
		return LoginTheme{Base: BaseLight, Palette: *sel.Variant}, nil
	}
	return LoginTheme{}, fmt.Errorf("%w: %q pins a variant %q does not have", ErrTheme, name, sel.Theme.Name)
}

// Look is the two-tone the mark and the favicon are drawn in: the palette the
// resolved theme carries, which is always one variant's own colours.
func (t LoginTheme) Look() brand.Look {
	return brand.LookOf(t.Palette)
}

// The login's two pictures. The names are Authelia's: its asset handler serves
// <asset_path>/logo.png for the route /static/media/logo.png and
// <asset_path>/favicon.ico for /favicon.ico, and serves its own picture when a
// file is absent (internal/server/handlers.go:170-174, internal/server/const.go:16).
const (
	AssetLogo    = "logo.png"
	AssetFavicon = "favicon.ico"
)

// AssetPath is where they are mounted, and what server.asset_path names.
//
// It is the configuration's own directory: the two pictures ride in the same
// ConfigMap as configuration.yml and the same read-only volume, so one apply
// writes the login's whole face and one hash rolls the pod. Authelia reads
// these files from the filesystem rather than from the rendering of the index
// (internal/server/template.go:47 stats <asset_path>/logo.png on every page
// render, and internal/middlewares/asset_override.go:24 stats the requested
// asset on every request), so an updated ConfigMap is picked up without a
// restart -- which is why only the configuration's hash is on the pod template.
const AssetPath = ServerPath

// LogoCell is the mark's cell size in logo.png: 17 cells of 8 px is 136 px
// wide, which Authelia draws at 64 (`className="... w-16"`), so a 2x display
// gets almost exactly one image pixel per device pixel.
const LogoCell = 8

// Assets is the login's two pictures, as the bytes the ConfigMap carries.
type Assets struct {
	Logo    []byte
	Favicon []byte
}

// ErrAssets says a picture could not be drawn, which is a box bug rather than
// anything an operator did.
var ErrAssets = errors.New("authelia assets")

// RenderAssets draws the mark and the favicon for a resolved theme.
func RenderAssets(t LoginTheme) (Assets, error) {
	look := t.Look()
	logo, err := brand.MarkPNG(look, LogoCell)
	if err != nil {
		return Assets{}, fmt.Errorf("%w: %v", ErrAssets, err)
	}
	// The favicon is the 5 alone in the accent: at 16 px the word is three
	// pixels a glyph and reads as a smudge, and the 5 already reads as an S.
	favicon, err := brand.FaviconICO(look.Five)
	if err != nil {
		return Assets{}, fmt.Errorf("%w: %v", ErrAssets, err)
	}
	return Assets{Logo: logo, Favicon: favicon}, nil
}

// BinaryData is the ConfigMap's binaryData field: the two files, base64 as the
// API server's JSON form wants them. A ConfigMap carries text in `data` and
// bytes here, and a PNG is bytes.
func (a Assets) BinaryData() map[string]string {
	return map[string]string{
		AssetLogo:    base64.StdEncoding.EncodeToString(a.Logo),
		AssetFavicon: base64.StdEncoding.EncodeToString(a.Favicon),
	}
}

// validBase reports whether Authelia would accept the theme setting. It is
// called before anything is rendered, because a configuration Authelia refuses
// is a login that never becomes Ready and a seedbox served by nothing.
func validBase(base string) bool {
	switch base {
	case BaseLight, BaseDark:
		return true
	}
	return false
}

// checkAssetPath is the same pre-flight for the asset directory: Authelia stats
// it at startup and refuses a path it cannot read, and box's is always the
// mounted configuration directory.
func checkAssetPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%w: no asset path", ErrConfiguration)
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("%w: asset path %q is not absolute", ErrConfiguration, path)
	}
	return nil
}
