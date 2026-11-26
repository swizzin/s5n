// Command k0s-sums checks the k0s checksums charts/s5n/platform.yaml pins beside k0s's
// version, and with --write sets them from the release itself.
//
// box downloads k0s onto a clean host and runs it as root, so the sum cannot be the one
// served beside the archive -- that proves only that both came from the same place. The
// sum lives in the chart, in the same pull request as the version it describes, and is
// trustworthy only if something outside a maintainer's memory hashes the real asset: this
// reads the pinned version, downloads both architectures' assets, hashes them, and checks
// the file against the release's sha256sums.txt or writes the two lines from them.
//
// --check is the default and what CI runs: a k0s bump that did not write its sums fails its
// own pull request. --write rewrites only the two sum lines, and only when the asset and
// the published sum agree; everything else in the file is left byte for byte.
//
// Usage:
//
//	go run ./hack/k0s-sums [--check|--write] [path to platform.yaml]
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/swizzin/s5n/internal/bootstrap"
)

const (
	// defaultFile is the chart's pins, relative to the repository root.
	defaultFile = "charts/s5n/platform.yaml"
	// k0sReleaseBase is where k0s publishes assets: the base box's k0sRelease.URL builds
	// from, so a sum written here is of exactly the bytes box fetches.
	k0sReleaseBase = "https://github.com/k0sproject/k0s/releases/download"
)

// arches is every architecture the chart pins a checksum for, and readPins refuses a file
// pinning anything else, so a third architecture cannot slip past unchecked.
var arches = []string{"amd64", "arm64"}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "k0s-sums: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("k0s-sums", flag.ContinueOnError)
	// --check is the default rather than a flag to remember: CI's run is the check.
	check := fs.Bool("check", false, "check the pinned sums against the release and exit non-zero on a mismatch (the default)")
	write := fs.Bool("write", false, "rewrite the two checksum lines from the release's assets")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: k0s-sums [--check|--write] [path to platform.yaml]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *write && *check {
		return fmt.Errorf("--check and --write are different jobs: choose one")
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return fmt.Errorf("too many arguments: want one path to platform.yaml")
	}
	file := defaultFile
	if fs.NArg() == 1 {
		file = fs.Arg(0)
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	pinned, err := readPins(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}

	ctx := context.Background()
	// The fetch is box's own downloader, so a sum written here is of the bytes box will get.
	client := bootstrap.HTTPDownloader{}
	asset, err := assetSums(ctx, client, pinned.Version)
	if err != nil {
		return err
	}
	published, err := publishedSums(ctx, client, pinned.Version)
	if err != nil {
		return err
	}

	if *write {
		return writeSums(file, pinned, asset, published)
	}
	problems := checkSums(pinned, asset, published)
	if len(problems) > 0 {
		lines := make([]string, 0, len(problems)+1)
		lines = append(lines, fmt.Sprintf("%s: k0s %s does not match the release", file, pinned.Version))
		for _, p := range problems {
			lines = append(lines, "  "+p.String())
		}
		lines = append(lines, "  write them with: go run ./hack/k0s-sums --write")
		return fmt.Errorf("%s", strings.Join(lines, "\n"))
	}
	for _, arch := range arches {
		fmt.Printf("k0s %s: k0sSHA256%s matches the %s asset and the release's sha256sums.txt (%s)\n",
			pinned.Version, arch, arch, pinned.Sums[arch])
	}
	return nil
}

// writeSums replaces the two pinned sums and says which line moved. It refuses when the
// release's own two accounts of its asset disagree, which would be a sum box cannot verify.
func writeSums(file string, pins pinFile, asset, published map[string]string) error {
	// A file whose sums are simply stale is what --write is for: only the asset against the
	// published sum is refused.
	if problems := releaseConsistent(asset, published); len(problems) > 0 {
		return fmt.Errorf("%s: %s", file, problems[0].String())
	}
	out, err := rewrite(pins.Raw, asset)
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	if err := os.WriteFile(file, out, 0o644); err != nil {
		return err
	}
	for _, arch := range arches {
		was := pins.Sums[arch]
		switch was {
		case asset[arch]:
			fmt.Printf("k0s %s: k0sSHA256%s already %s\n", pins.Version, arch, was)
		default:
			fmt.Printf("k0s %s: k0sSHA256%s %s -> %s\n", pins.Version, arch, was, asset[arch])
		}
	}
	return nil
}

// assetSums hashes the release's asset for every architecture as it arrives, so a slow link
// costs time rather than memory.
func assetSums(ctx context.Context, client bootstrap.HTTPDownloader, version string) (map[string]string, error) {
	sums := make(map[string]string, len(arches))
	for _, arch := range arches {
		sum, err := hashURL(ctx, client, assetURL(version, arch))
		if err != nil {
			return nil, err
		}
		sums[arch] = sum
	}
	return sums, nil
}

// publishedSums returns the sum the release publishes for each architecture's asset; one it
// does not list cannot be checked.
func publishedSums(ctx context.Context, client bootstrap.HTTPDownloader, version string) (map[string]string, error) {
	body, err := client.Fetch(ctx, sumsURL(version))
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", sumsURL(version), err)
	}
	listed, err := parseSumsTxt(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", sumsURL(version), err)
	}
	sums := make(map[string]string, len(arches))
	for _, arch := range arches {
		name := assetName(version, arch)
		sum, ok := listed[name]
		if !ok {
			return nil, fmt.Errorf("%s lists no %s: the release does not describe the asset box would install", sumsURL(version), name)
		}
		sums[arch] = sum
	}
	return sums, nil
}

// hashURL streams one URL through sha256.
func hashURL(ctx context.Context, client bootstrap.HTTPDownloader, url string) (string, error) {
	body, err := client.Fetch(ctx, url)
	if err != nil {
		return "", err
	}
	defer func() { _ = body.Close() }()
	sum := sha256.New()
	if _, err := io.Copy(sum, body); err != nil {
		return "", fmt.Errorf("%s: %w", url, err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// assetName is the file k0s publishes for one architecture.
func assetName(version, arch string) string { return "k0s-" + version + "-" + arch }

// assetURL is the release asset box downloads. The tag carries a '+', which a path segment
// may hold literally, so it goes in as published -- as bootstrap's k0sRelease.URL does.
func assetURL(version, arch string) string {
	return k0sReleaseBase + "/" + url.PathEscape(version) + "/" + assetName(version, arch)
}

// sumsURL is the release's own list of asset checksums.
func sumsURL(version string) string {
	return k0sReleaseBase + "/" + url.PathEscape(version) + "/sha256sums.txt"
}
