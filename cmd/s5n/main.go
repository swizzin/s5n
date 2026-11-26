// Command s5n is the one binary the chart ships: the wiring Job and the init
// containers that seed an app's own settings before it starts.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/swizzin/s5n/internal/atomicfile"
	"github.com/swizzin/s5n/internal/bazconf"
	"github.com/swizzin/s5n/internal/jellyconf"
	"github.com/swizzin/s5n/internal/qbtconf"
	"github.com/swizzin/s5n/internal/sabconf"
	"github.com/swizzin/s5n/internal/ttlconf"
	"github.com/swizzin/s5n/internal/wiring"
)

// version is stamped at build time.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "wiring":
		err = runWiring(os.Args[2:])
	case "qbt-config":
		err = runQbtConfig(os.Args[2:])
	case "tautulli-config":
		err = runTautulliConfig(os.Args[2:])
	case "bazarr-config":
		err = runBazarrConfig(os.Args[2:])
	case "sabnzbd-config":
		err = runSabnzbdConfig(os.Args[2:])
	case "jellyfin-config":
		err = runJellyfinConfig(os.Args[2:])
	case "probe":
		err = runProbe(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "s5n: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: s5n <command> [flags]

commands:
  wiring           connect qBittorrent, the Arrs, Prowlarr, Plex and Tautulli, and prove /data
  qbt-config       seed the qBittorrent WebUI username and password into qBittorrent.conf
  tautulli-config  seed Tautulli's api_key into config.ini
  bazarr-config    seed Bazarr's api key and URL base into config/config.yaml
  sabnzbd-config   seed SABnzbd's URL base, download paths and API key into sabnzbd.ini
  jellyfin-config  set the interfaces Jellyfin binds and the proxies it trusts in config/network.xml
  probe            dial one address and report whether it opened, for box's lock proof
  version          print the build version
`)
	os.Exit(2)
}

// runWiring is the Helm post-install/post-upgrade hook.
func runWiring(args []string) error {
	fs := flag.NewFlagSet("wiring", flag.ExitOnError)
	configPath := fs.String("config", "/etc/s5n/wiring/wiring.json", "rendered wiring config")
	noPublish := fs.Bool("no-publish", false, "do not write the result ConfigMap")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := wiring.Load(*configPath)
	if err != nil {
		return err
	}
	runner := &wiring.Runner{Config: *cfg, Log: os.Stdout}
	if !*noPublish {
		if cfg.ConfigMap == "" {
			return fmt.Errorf("configMap is empty: the release name is missing from %s", *configPath)
		}
		pub, err := wiring.InCluster()
		if err != nil {
			return err
		}
		runner.Publish = func(ctx context.Context, data map[string]string) error {
			return pub.Upsert(ctx, cfg.ConfigMap, data)
		}
		runner.LoadOwned = func(ctx context.Context) ([]byte, error) {
			v, ok, err := pub.Get(ctx, cfg.ConfigMap, "owned.json")
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, nil
			}
			return []byte(v), nil
		}
	}
	ctx, cancel := cfg.Context(context.Background())
	defer cancel()
	res, err := runner.Run(ctx)
	fmt.Print(wiring.Summary(res))
	return err
}

// runQbtConfig is the qBittorrent init container: it seals the WebUI credential
// into the config file before the app starts. Re-running it is a no-op.
func runQbtConfig(args []string) error {
	fs := flag.NewFlagSet("qbt-config", flag.ExitOnError)
	conf := fs.String("conf", "/config/qBittorrent/qBittorrent.conf", "path to qBittorrent.conf")
	username := fs.String("username", "admin", "WebUI username")
	passwordFile := fs.String("password-file", "", "file holding the WebUI password")
	port := fs.Int("port", 8080, "WebUI port qBittorrent listens on")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *passwordFile == "" {
		return fmt.Errorf("--password-file is required")
	}
	password, err := wiring.ReadSecret(*passwordFile)
	if err != nil {
		return err
	}

	existing, err := os.ReadFile(*conf)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", *conf, err)
	}
	out, changed, err := qbtconf.Seal(existing, *username, password, *port)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Printf("qbt-config: %s already holds the configured WebUI credential\n", *conf)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(*conf), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(*conf), err)
	}
	if err := atomicfile.Write(*conf, out, 0o600); err != nil {
		return err
	}
	fmt.Printf("qbt-config: seeded WebUI credential into %s\n", *conf)
	return nil
}

// runTautulliConfig seals config.ini before the first start: seeding the API key
// is how the wiring Job authenticates, since it cannot read one back. The URL
// root and login are re-sealed every start (internal/ttlconf).
func runTautulliConfig(args []string) error {
	fs := flag.NewFlagSet("tautulli-config", flag.ExitOnError)
	conf := fs.String("conf", "/config/config.ini", "path to Tautulli's config.ini")
	apiKeyFile := fs.String("api-key-file", "", "file holding the api_key to seed")
	httpRoot := fs.String("http-root", "/tautulli", "the URL prefix the app is served under, e.g. /tautulli")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *apiKeyFile == "" {
		return fmt.Errorf("--api-key-file is required")
	}
	apiKey, err := wiring.ReadSecret(*apiKeyFile)
	if err != nil {
		return err
	}

	existing, err := os.ReadFile(*conf)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", *conf, err)
	}
	out, changed := ttlconf.Seal(existing, apiKey, *httpRoot)
	if !changed {
		fmt.Printf("tautulli-config: %s already holds the sealed settings\n", *conf)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(*conf), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(*conf), err)
	}
	if err := atomicfile.Write(*conf, out, 0o600); err != nil {
		return err
	}
	fmt.Printf("tautulli-config: sealed %s (api_key, http_root %s, its own login off)\n", *conf, *httpRoot)
	return nil
}

// runJellyfinConfig writes network.xml before Jellyfin reads its bind list and
// trusted proxies once (internal/jellyconf). The bind list is always empty (every
// interface), so an app outside its pod stays reachable.
func runJellyfinConfig(args []string) error {
	fs := flag.NewFlagSet("jellyfin-config", flag.ExitOnError)
	conf := fs.String("conf", "/config/config/network.xml", "path to Jellyfin's config/network.xml")
	var proxies stringList
	fs.Var(&proxies, "known-proxy", "an address or subnet whose X-Forwarded-For Jellyfin honours (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	existing, err := os.ReadFile(*conf)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", *conf, err)
	}
	out, changed, err := jellyconf.Ensure(existing, proxies)
	if err != nil {
		return fmt.Errorf("%s: %w", *conf, err)
	}
	if !changed {
		fmt.Printf("jellyfin-config: %s already binds every interface and trusts %s\n", *conf, trustList(proxies))
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(*conf), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(*conf), err)
	}
	if err := atomicfile.Write(*conf, out, 0o600); err != nil {
		return err
	}
	fmt.Printf("jellyfin-config: %s binds every interface and trusts %s\n", *conf, trustList(proxies))
	return nil
}

// trustList renders the proxy list the file now holds: the app reads an empty
// element as "honour no X-Forwarded-For at all", which is worth saying out loud.
func trustList(proxies stringList) string {
	if len(proxies) == 0 {
		return "no proxy (X-Forwarded-For ignored)"
	}
	return strings.Join(proxies, ", ")
}

// stringList is a flag that may be given more than once, in order.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("an empty value")
	}
	*s = append(*s, v)
	return nil
}

// runBazarrConfig seals the API key and URL prefix into config.yaml before Bazarr
// reads them; neither can be set through the API. Bazarr rewrites the file from
// memory, which holds these keys, so they survive.
func runBazarrConfig(args []string) error {
	fs := flag.NewFlagSet("bazarr-config", flag.ExitOnError)
	conf := fs.String("conf", "/config/config/config.yaml", "path to Bazarr's config/config.yaml")
	apiKeyFile := fs.String("api-key-file", "", "file holding the API key to seed")
	baseURL := fs.String("base-url", "/bazarr", "the URL prefix the app is served under, e.g. /bazarr")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *apiKeyFile == "" {
		return fmt.Errorf("--api-key-file is required")
	}
	apiKey, err := wiring.ReadSecret(*apiKeyFile)
	if err != nil {
		return err
	}

	existing, err := os.ReadFile(*conf)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", *conf, err)
	}
	out, changed, err := bazconf.Seal(existing, apiKey, *baseURL)
	if err != nil {
		return fmt.Errorf("%s: %w", *conf, err)
	}
	if !changed {
		fmt.Printf("bazarr-config: %s already holds the sealed settings\n", *conf)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(*conf), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(*conf), err)
	}
	if err := atomicfile.Write(*conf, out, 0o600); err != nil {
		return err
	}
	fmt.Printf("bazarr-config: sealed %s (auth.apikey, general.base_url %s)\n", *conf, *baseURL)
	return nil
}

// runSabnzbdConfig seals the URL prefix, download paths, API key and XFF switch
// into sabnzbd.ini before the app reads them: the prefix mounts the API, and the
// XFF switch decides whether a Gateway request is answered at all.
func runSabnzbdConfig(args []string) error {
	fs := flag.NewFlagSet("sabnzbd-config", flag.ExitOnError)
	conf := fs.String("conf", "/config/sabnzbd.ini", "path to SABnzbd's sabnzbd.ini")
	apiKeyFile := fs.String("api-key-file", "", "file holding the API key to seed")
	urlBase := fs.String("url-base", "/sabnzbd", "the URL prefix the app is served under, e.g. /sabnzbd")
	completeDir := fs.String("complete-dir", "/data/downloads", "where finished downloads wait for an Arr to import")
	downloadDir := fs.String("download-dir", "/data/downloads/incomplete", "where a download is unpacked while it runs")
	verifyXFF := fs.Bool("verify-xff", false, "take the client address from X-Forwarded-For (off: trust the Gateway in front of this pod)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *apiKeyFile == "" {
		return fmt.Errorf("--api-key-file is required")
	}
	apiKey, err := wiring.ReadSecret(*apiKeyFile)
	if err != nil {
		return err
	}

	existing, err := os.ReadFile(*conf)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", *conf, err)
	}
	out, changed, err := sabconf.Seal(existing, sabconf.Settings{
		URLBase:     *urlBase,
		CompleteDir: *completeDir,
		DownloadDir: *downloadDir,
		APIKey:      apiKey,
		VerifyXFF:   *verifyXFF,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", *conf, err)
	}
	if !changed {
		fmt.Printf("sabnzbd-config: %s already holds the sealed settings\n", *conf)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(*conf), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(*conf), err)
	}
	if err := atomicfile.Write(*conf, out, 0o600); err != nil {
		return err
	}
	fmt.Printf("sabnzbd-config: sealed %s (url_base %s, downloads under %s)\n", *conf, *urlBase, *completeDir)
	return nil
}
