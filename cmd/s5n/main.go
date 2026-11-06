// Command s5n is the one binary the chart ships: the wiring Job, the
// qBittorrent credential seed, and (later) the box CLI all live here.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/swizzin/s5n/internal/qbtconf"
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
	tmp := *conf + ".s5n-tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, *conf); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", *conf, err)
	}
	fmt.Printf("qbt-config: seeded WebUI credential into %s\n", *conf)
	return nil
}

// runTautulliConfig is Tautulli's init container: it seeds config.ini's
// api_key before the app's first start, mirroring runQbtConfig. Tautulli
// generates its own key only when config.ini has none, so seeding a known
// one is what lets the wiring Job authenticate to an app it has no other way
// to read credentials back from (no mount into Tautulli's own /config).
func runTautulliConfig(args []string) error {
	fs := flag.NewFlagSet("tautulli-config", flag.ExitOnError)
	conf := fs.String("conf", "/config/config.ini", "path to Tautulli's config.ini")
	apiKeyFile := fs.String("api-key-file", "", "file holding the api_key to seed")
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
	out, changed := ttlconf.Seal(existing, apiKey)
	if !changed {
		fmt.Printf("tautulli-config: %s already holds the configured api_key\n", *conf)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(*conf), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(*conf), err)
	}
	tmp := *conf + ".s5n-tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, *conf); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", *conf, err)
	}
	fmt.Printf("tautulli-config: seeded api_key into %s\n", *conf)
	return nil
}
