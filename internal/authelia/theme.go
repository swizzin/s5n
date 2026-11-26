package authelia

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/swizzin/s5n/internal/brand"
	"github.com/swizzin/s5n/internal/theme"
)

// Authelia's own theme names box writes. Its validator also knows grey and oled
// (internal/configuration/validator/const.go:548), which box never writes: a
// seedbox has one page, and the mark is drawn for it.
const (
	BaseLight = "light"
	BaseDark  = "dark"
)

// LoginTheme is what the values file's `theme:` means to the login.
//
// A pair name has two palettes; Authelia has no terminal to ask, and the login is
// one page for everyone, so a pair is the dark page drawn in that pair's dark
// variant. A pinned variant pins the page, and the mark follows. Authelia's `auto`
// is never written: it would hand the colours to a browser box cannot see.
type LoginTheme struct {
	// Base is Authelia's theme: light or dark.
	Base string
	// Palette is the s5n variant it is drawn in: the pinned variant, or the
	// pair's dark one.
	Palette theme.Variant
}

// ErrTheme says the theme cannot be resolved: an unknown name is a startup
// error in box, and a login rendered with a name Authelia does not know is a
// login that never becomes Ready.
var ErrTheme = errors.New("authelia theme")

// ResolveTheme maps a `theme:` value onto Authelia's setting and the palette the
// mark is drawn in: a pair name is the dark page in the pair's dark variant, a
// pinned dark variant is `dark` and a pinned light one `light`.
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
// <asset_path>/logo.png for /static/media/logo.png and <asset_path>/favicon.ico
// for /favicon.ico, falling back to its own picture (internal/server/handlers.go:170-174).
const (
	AssetLogo    = "logo.png"
	AssetFavicon = "favicon.ico"
)

// AssetPath is where the pictures are mounted, and what server.asset_path names:
// the configuration's own directory, so one apply writes the login's whole face
// and one hash rolls the pod. Authelia stats these files on every page render
// (internal/server/template.go:47) and every request
// (internal/middlewares/asset_override.go:24), so an updated ConfigMap is picked
// up without a restart.
const AssetPath = ServerPath

// LogoCell is the mark's cell size in logo.png: 17 cells of 8 px is 136 px, drawn
// at 64, so a 2x display gets almost one image pixel per device pixel.
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
	// The favicon is the 5 alone: at 16 px the word is a smudge, and the 5
	// already reads as an S.
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

// validBase reports whether Authelia would accept the theme setting: a
// configuration it refuses is a login that never becomes Ready.
func validBase(base string) bool {
	switch base {
	case BaseLight, BaseDark:
		return true
	}
	return false
}

// checkAssetPath is the same pre-flight for the asset directory: Authelia stats it
// at startup and refuses a path it cannot read.
func checkAssetPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%w: no asset path", ErrConfiguration)
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("%w: asset path %q is not absolute", ErrConfiguration, path)
	}
	return nil
}
