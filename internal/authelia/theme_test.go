package authelia

import (
	"bytes"
	"errors"
	"image/png"
	"io"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/brand"
	"github.com/swizzin/s5n/internal/theme"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// TestThemeMappingIsEveryNameInTheRegistry is the whole mapping, over every
// name internal/theme accepts: a pair is the dark page drawn in the pair's dark
// variant, a pinned dark variant pins the dark page, a pinned light variant
// pins the light one. Authelia's `auto` is never an answer.
//
// The table is built from the registry rather than written out, so a new theme
// cannot ship without an answer here, and the names are checked through
// theme.Parse: a variant whose name equals its sibling's would map ambiguously,
// and that is a registry bug this catches.
func TestThemeMappingIsEveryNameInTheRegistry(t *testing.T) {
	pairs := theme.Themes()
	if len(pairs) == 0 {
		t.Fatal("the theme registry is empty")
	}
	for _, th := range pairs {
		if th.Dark.Name == th.Light.Name {
			t.Fatalf("%s: both variants are named %q", th.Name, th.Dark.Name)
		}
		cases := []struct {
			name    string
			base    string
			palette theme.Variant
		}{
			{th.Name, BaseDark, th.Dark},
			{th.Name + "-" + th.Dark.Name, BaseDark, th.Dark},
			{th.Name + "-" + th.Light.Name, BaseLight, th.Light},
		}
		for _, c := range cases {
			got, err := ResolveTheme(c.name)
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
				continue
			}
			if got.Base != c.base {
				t.Errorf("%s maps to %q, want %q", c.name, got.Base, c.base)
			}
			if got.Palette.Name != c.palette.Name || got.Palette.Accent != c.palette.Accent {
				t.Errorf("%s is drawn in %s/%s, want %s/%s", c.name, got.Palette.Name, got.Palette.Accent, c.palette.Name, c.palette.Accent)
			}
			if got.Base == "auto" {
				t.Errorf("%s maps to auto: the login is one page and box draws one picture", c.name)
			}
		}
	}
	// Every name Names() lists has an answer, and nothing it lists is missing
	// from the table above.
	for _, name := range theme.Names() {
		if _, err := ResolveTheme(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// An unset key is the chart's default, which is a pair.
	got, err := ResolveTheme("")
	if err != nil {
		t.Fatalf("the empty theme: %v", err)
	}
	if got.Base != BaseDark || got.Palette.Name != theme.Default().Dark.Name {
		t.Fatalf("the empty theme resolved to %+v, want %s's dark variant on the dark page", got, theme.Default().Name)
	}
	if _, err := ResolveTheme("amber"); err == nil {
		t.Fatal("an unknown theme was accepted")
	}
}

// TestLoginThemeLook: every resolved theme is drawn in a variant's own palette
// -- the pinned one, or the pair's dark variant -- and never in a colour no
// palette has.
func TestLoginThemeLook(t *testing.T) {
	for _, th := range theme.Themes() {
		cases := []struct {
			name string
			want theme.Variant
		}{
			{th.Name, th.Dark},
			{th.Name + "-" + th.Dark.Name, th.Dark},
			{th.Name + "-" + th.Light.Name, th.Light},
		}
		for _, c := range cases {
			lt, err := ResolveTheme(c.name)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if got, want := lt.Look(), brand.LookOf(c.want); got != want {
				t.Errorf("%s: the look is %+v, want %s's own", c.name, got, c.want.Name)
			}
			if lt.Look().Unlit.A != 0xFF {
				t.Errorf("%s: the drawing has translucent pieces, and every page here is the variant's own", c.name)
			}
		}
	}
}

// TestRenderConfigurationCarriesTheTheme is the template's half of the mapping:
// a pair writes dark -- the same line its dark variant writes -- and a pinned
// light variant writes light, and every rendering names the asset directory,
// without which Authelia serves its own logo and box's mark never appears.
func TestRenderConfigurationCarriesTheTheme(t *testing.T) {
	cases := []struct{ theme, want string }{
		{"s5n", "dark"},
		{"s5n-dark", "dark"},
		{"s5n-light", "light"},
		{"catppuccin", "dark"},
		{"catppuccin-mocha", "dark"},
		{"dracula-alucard", "light"},
	}
	for _, c := range cases {
		lt, err := ResolveTheme(c.theme)
		if err != nil {
			t.Fatalf("%s: %v", c.theme, err)
		}
		cfg := testConfiguration()
		cfg.Theme = lt.Base
		rendered, err := RenderConfiguration(cfg)
		if err != nil {
			t.Fatalf("%s: %v", c.theme, err)
		}
		text := string(rendered)
		for _, want := range []string{"theme: '" + c.want + "'", "asset_path: '" + AssetPath + "'"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s: the configuration does not contain %q:\n%s", c.theme, want, text)
			}
		}
		if strings.Contains(text, "theme: 'auto'") {
			t.Errorf("%s: the configuration hands the page to the browser:\n%s", c.theme, text)
		}
	}
}

// TestRenderConfigurationRefusesABaseAutheliaWouldNot: a theme Authelia does not
// know stops its startup, and a login that never starts serves nothing, so box
// refuses the file before it writes it. `auto` is refused with grey and oled:
// box resolves every name onto a page, and a file that hands that choice to the
// browser is one the mark was not drawn for.
func TestRenderConfigurationRefusesABaseAutheliaWouldNot(t *testing.T) {
	cases := map[string]func(*Configuration){
		"no theme":     func(c *Configuration) { c.Theme = "" },
		"auto":         func(c *Configuration) { c.Theme = "auto" },
		"grey":         func(c *Configuration) { c.Theme = "grey" },
		"oled":         func(c *Configuration) { c.Theme = "oled" },
		"unknown":      func(c *Configuration) { c.Theme = "amber" },
		"no asset dir": func(c *Configuration) { c.AssetPath = "" },
		"relative dir": func(c *Configuration) { c.AssetPath = "assets" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testConfiguration()
			mutate(&cfg)
			if _, err := RenderConfiguration(cfg); err == nil {
				t.Fatal("rendered a configuration Authelia would refuse at startup")
			}
		})
	}
}

// TestRenderAssets: the login gets its own mark and favicon, drawn from the
// theme, and both are files Authelia's handler can serve.
func TestRenderAssets(t *testing.T) {
	for _, name := range []string{"s5n", "s5n-dark", "s5n-light", "dracula-alucard"} {
		lt, err := ResolveTheme(name)
		if err != nil {
			t.Fatal(err)
		}
		assets, err := RenderAssets(lt)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		logo, err := png.Decode(bytes.NewReader(assets.Logo))
		if err != nil {
			t.Fatalf("%s: logo: %v", name, err)
		}
		if got, want := logo.Bounds().Dx(), brand.Cols*LogoCell; got != want {
			t.Errorf("%s: logo is %d px wide, want %d", name, got, want)
		}
		if got, want := logo.Bounds().Dy(), brand.Rows*LogoCell; got != want {
			t.Errorf("%s: logo is %d px high, want %d", name, got, want)
		}
		// An ICO: ICONDIR says two images, and the first payload is the 16 px
		// PNG. brand's own test reads the container in full; here the point is
		// that the login ships one.
		if got := int(assets.Favicon[4]) | int(assets.Favicon[5])<<8; got != 2 {
			t.Errorf("%s: favicon carries %d images, want 2", name, got)
		}
		data := assets.BinaryData()
		if len(data) != 2 || data[AssetLogo] == "" || data[AssetFavicon] == "" {
			t.Errorf("%s: binaryData = %v", name, data)
		}
	}
}

// TestRenderedDeploymentMountsTheAssetsReadOnly: the ConfigMap the assets ride
// in is the one mounted at server.asset_path, and it is mounted read-only --
// the login runs with a read-only root filesystem under Pod Security
// `restricted`, and a writable mount there would be the one hole in it.
func TestRenderedDeploymentMountsTheAssetsReadOnly(t *testing.T) {
	manifests, err := RenderManifests(testManifests())
	if err != nil {
		t.Fatalf("RenderManifests: %v", err)
	}
	deploy := docOfKind(t, manifests, "Deployment")
	spec, _ := deploy["spec"].(map[string]any)
	template, _ := spec["template"].(map[string]any)
	podSpec, _ := template["spec"].(map[string]any)

	volumes, _ := podSpec["volumes"].([]any)
	var found bool
	for _, raw := range volumes {
		volume, _ := raw.(map[string]any)
		if volume["name"] != "configuration" {
			continue
		}
		cm, _ := volume["configMap"].(map[string]any)
		if cm["name"] != ConfigMapName {
			t.Fatalf("the configuration volume mounts %v, want %s", cm["name"], ConfigMapName)
		}
		found = true
	}
	if !found {
		t.Fatal("the Deployment has no configuration volume: the login has no file to start with")
	}

	containers, _ := podSpec["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("Deployment carries %d containers", len(containers))
	}
	container, _ := containers[0].(map[string]any)
	mounts, _ := container["volumeMounts"].([]any)
	var mounted bool
	for _, raw := range mounts {
		mount, _ := raw.(map[string]any)
		if mount["name"] != "configuration" {
			continue
		}
		if mount["mountPath"] != AssetPath {
			t.Fatalf("the configuration is mounted at %v, but asset_path is %s: the mark would not be found", mount["mountPath"], AssetPath)
		}
		if on, _ := mount["readOnly"].(bool); !on {
			t.Fatal("the configuration and assets are mounted read-write")
		}
		mounted = true
	}
	if !mounted {
		t.Fatal("the Deployment does not mount the configuration ConfigMap")
	}
}

// docOfKind is one document out of a rendered multi-document stream.
func docOfKind(t *testing.T, manifests []byte, kind string) map[string]any {
	t.Helper()
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifests), 4096)
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if doc["kind"] == kind {
			return doc
		}
	}
	t.Fatalf("no %s among the rendered documents", kind)
	return nil
}
