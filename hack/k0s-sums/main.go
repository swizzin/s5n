// Command k0s-sums checks the k0s checksums charts/s5n/platform.yaml pins
// beside k0s's version, and with --write sets them from the release itself.
//
// box downloads k0s onto a clean host and runs it as root, so the sum it
// verifies cannot be the one served beside the archive: that proves only that
// both came from the same place. The sum lives in the chart instead, in the
// same pull request as the version it describes, and that is only trustworthy
// if something outside a maintainer's memory hashes the real asset. This is
// that something: it reads the pinned version, downloads both architectures'
// assets from the release, hashes them, fetches the release's sha256sums.txt,
// and either checks the file against both or writes the two lines from them.
//
// --check is the default and what CI runs: it exits non-zero, naming the
// architecture, when a pinned sum is not the hash of the asset or not what the
// release publishes, so a k0s bump that did not write its sums fails its own
// pull request. --write rewrites the two sum lines, and only when the asset and
// the published sum agree; everything else in the file is left byte for byte as
// it was.
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
	// defaultFile is the chart's pins, relative to the repository root, which
	// is where CI and a maintainer run this from.
	defaultFile = "charts/s5n/platform.yaml"
	// k0sReleaseBase is where the k0s project publishes release assets: the
	// same base bootstrap builds the URL box downloads from, so a sum written
	// here is the sum of exactly the bytes box will fetch.
	k0sReleaseBase = "https://github.com/k0sproject/k0s/releases/download"
)

// arches is every architecture the chart pins a k0s checksum for, in the order
// this command reports them. The keys in the file are this plus the prefix, and
// readPins refuses a file that pins anything else, so a third architecture
// cannot slip past unchecked.
var arches = []string{"amd64", "arm64"}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "k0s-sums: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("k0s-sums", flag.ContinueOnError)
	// --check is the default rather than a flag the caller must remember: the
	// run with no arguments is the one CI makes, and it must be the check.
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
	// The fetch is box's own: the same downloader, with the same retries, so a
	// sum this command writes is the sum of the bytes box will get from the
	// same URL rather than of some other client's idea of them.
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

// writeSums replaces the two pinned sums, and says which line moved. It refuses
// to write anything when the release's own two accounts of its asset disagree:
// a sum copied from an inconsistent release would be a sum box could never
// verify.
func writeSums(file string, pins pinFile, asset, published map[string]string) error {
	// Only the release's own inconsistency blocks a write. A file whose sums
	// are simply stale is exactly what --write is for, so what is refused here
	// is the asset against the published sum, never the file's old value.
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

// assetSums downloads the release's asset for every architecture and returns
// its sha256. The bytes are hashed as they arrive, so a slow link costs time
// rather than memory.
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

// publishedSums fetches the release's own sha256sums.txt and returns the sum it
// publishes for each architecture's asset. A release that does not list one is
// an error: the file's sum cannot be checked against a release that does not
// describe the asset.
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

// assetURL is the release asset box downloads. The tag carries a '+', which a
// path segment may hold literally, so it goes in as published -- the same way
// bootstrap's k0sRelease.URL builds it.
func assetURL(version, arch string) string {
	return k0sReleaseBase + "/" + url.PathEscape(version) + "/" + assetName(version, arch)
}

// sumsURL is the release's own list of asset checksums.
func sumsURL(version string) string {
	return k0sReleaseBase + "/" + url.PathEscape(version) + "/sha256sums.txt"
}
