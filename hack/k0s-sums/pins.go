package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/swizzin/s5n/internal/release"
)

// pinFile is charts/s5n/platform.yaml as this command reads it: the k0s version,
// the checksum pinned for each architecture, and the bytes, so --write can put a
// new value on its own line and change nothing else in the file.
type pinFile struct {
	Version string
	Sums    map[string]string
	Raw     []byte
}

// sumLine is one checksum line: the key, a quoted value, and nothing else worth
// preserving but a trailing comment if someone adds one. The value is captured
// on its own so a rewrite replaces exactly those 64 characters -- the quotes,
// the indentation and any comment survive byte for byte.
var sumLine = regexp.MustCompile(`^([ \t]*k0sSHA256([a-z0-9]+)[ \t]*:[ \t]*"?)([0-9a-f]{64})("?[ \t]*(?:#.*)?\r?)$`)

// readPins parses the file the way box does, so this command and box cannot
// disagree about what a pin is, and then insists on exactly the architectures
// box installs. A third architecture in the file is refused rather than
// skipped: a sum this command does not check is a sum CI promises and does not
// keep.
func readPins(raw []byte) (pinFile, error) {
	v, err := release.ParseVersions(raw)
	if err != nil {
		return pinFile{}, err
	}
	if len(v.K0sSHA256) != len(arches) {
		return pinFile{}, fmt.Errorf("the file pins %d k0s checksums, and this command checks %d (amd64, arm64): teach it the new key rather than leaving it unchecked", len(v.K0sSHA256), len(arches))
	}
	for _, arch := range arches {
		if v.K0sSHA256[arch] == "" {
			return pinFile{}, fmt.Errorf("the file pins no k0s checksum for %s", arch)
		}
	}
	return pinFile{Version: v.K0s, Sums: v.K0sSHA256, Raw: raw}, nil
}

// rewrite returns the file with the two checksum values replaced. Every other
// byte is the file's own: the lines are split and rejoined, and only the value
// inside a sum line's quotes is swapped. A line this command cannot recognise is
// an error, not something to append to -- the shape is the chart's, and writing
// a second line for one key would leave box and the file disagreeing.
func rewrite(raw []byte, sums map[string]string) ([]byte, error) {
	lines := strings.Split(string(raw), "\n")
	written := make(map[string]bool, len(arches))
	for i, line := range lines {
		at := sumLine.FindStringSubmatchIndex(line)
		if at == nil {
			continue
		}
		arch := line[at[4]:at[5]]
		sum, ok := sums[arch]
		if !ok {
			return nil, fmt.Errorf("no checksum to write for %q", arch)
		}
		lines[i] = line[:at[6]] + sum + line[at[7]:]
		written[arch] = true
	}
	for _, arch := range arches {
		if !written[arch] {
			return nil, fmt.Errorf("no k0sSHA256%s line: --write replaces a pin, it does not add one", arch)
		}
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// mismatch is one architecture whose checksums do not agree. Pinned is what the
// file says, Asset what the downloaded bytes hash to, Published what the
// release's sha256sums.txt says; a problem where Asset and Published differ is
// the release's, not the file's.
type mismatch struct {
	Arch      string
	Pinned    string
	Asset     string
	Published string
}

// String is the line --check prints for one architecture. It names the key, so
// the maintainer knows which line to look at, and both sources, so it is clear
// what the value should have been.
func (m mismatch) String() string {
	key := "k0sSHA256" + m.Arch
	if m.Asset != m.Published {
		return fmt.Sprintf("%s: the %s asset hashes to %s, but the release's sha256sums.txt says %s: the download is not the release's own file, so the file's %s was not judged against it",
			key, m.Arch, m.Asset, m.Published, m.Pinned)
	}
	return fmt.Sprintf("%s: %s is pinned, but the asset hashes to %s and the release's sha256sums.txt says %s", key, m.Pinned, m.Asset, m.Published)
}

// releaseConsistent reports the architectures where the bytes this run
// downloaded are not the file the release's own sha256sums.txt describes.
func releaseConsistent(asset, published map[string]string) []mismatch {
	var out []mismatch
	for _, arch := range arches {
		if asset[arch] == published[arch] {
			continue
		}
		out = append(out, mismatch{Arch: arch, Asset: asset[arch], Published: published[arch]})
	}
	return out
}

// checkSums compares the pinned sums with the hash of each asset and with the
// release's published sum. One verdict per architecture that disagrees, and
// none for the ones that do not.
func checkSums(pins pinFile, asset, published map[string]string) []mismatch {
	var out []mismatch
	for _, arch := range arches {
		m := mismatch{Arch: arch, Pinned: pins.Sums[arch], Asset: asset[arch], Published: published[arch]}
		switch {
		case m.Asset != m.Published:
			// The release's own two accounts disagree: the file has nothing
			// to be compared against until that is settled.
			out = append(out, m)
		case m.Pinned != m.Asset:
			out = append(out, m)
		}
	}
	return out
}

// parseSumsTxt reads a sha256sums.txt: one "<hex>  <name>" per line, the name
// optionally marked as binary with a leading "*", blank lines and comments
// skipped. A line that is neither is an error: a list this command half
// understands is one it could read the wrong sum out of.
func parseSumsTxt(b []byte) (map[string]string, error) {
	sums := make(map[string]string)
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d is %q, want \"<sha256>  <file>\"", i+1, line)
		}
		sum, name := fields[0], strings.TrimPrefix(fields[1], "*")
		if !isSum(sum) {
			return nil, fmt.Errorf("line %d: %q is not a sha256", i+1, sum)
		}
		sums[name] = sum
	}
	if len(sums) == 0 {
		return nil, fmt.Errorf("no checksums listed")
	}
	return sums, nil
}

// isSum is a checksum as the release writes it: 64 lowercase hex characters.
func isSum(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
