package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sum64 is a sha256-shaped value built from one character, so a fixture reads as
// a checksum without spelling 64 hex digits out.
func sum64(c string) string { return strings.Repeat(c, 64) }

// shippedFile is charts/s5n/platform.yaml: the file this command exists to
// check, read from the checkout so a rewrite is proven against the shape the
// chart actually ships rather than a fixture that could drift from it.
func shippedFile(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "s5n", "platform.yaml"))
	if err != nil {
		t.Fatalf("read the chart's platform pins: %v", err)
	}
	return raw
}

// onlySumsChanged fails when anything but the value on a checksum line differs:
// the line's indentation, quotes, trailing comment and every other line must be
// the file's own, byte for byte.
func onlySumsChanged(t *testing.T, before, after []byte) {
	t.Helper()
	was := strings.Split(string(before), "\n")
	is := strings.Split(string(after), "\n")
	if len(was) != len(is) {
		t.Fatalf("line count changed: %d -> %d", len(was), len(is))
	}
	for i := range was {
		if was[i] == is[i] {
			continue
		}
		at := sumLine.FindStringSubmatchIndex(was[i])
		if at == nil {
			t.Fatalf("line %d changed and is not a checksum line:\n  before: %q\n  after:  %q", i+1, was[i], is[i])
		}
		if was[i][:at[6]] != is[i][:at[6]] || was[i][at[7]:] != is[i][at[7]:] {
			t.Fatalf("line %d changed outside the value:\n  before: %q\n  after:  %q", i+1, was[i], is[i])
		}
		if len(is[i]) != len(was[i]) {
			t.Fatalf("line %d changed length: %d -> %d", i+1, len(was[i]), len(is[i]))
		}
	}
}

// The shipped file rewrites to the same file with two new values and nothing
// else: that is the whole promise of --write.
func TestRewriteKeepsEveryOtherByte(t *testing.T) {
	before := shippedFile(t)
	pins, err := readPins(before)
	if err != nil {
		t.Fatalf("readPins: %v", err)
	}
	want := map[string]string{"amd64": sum64("a"), "arm64": sum64("b")}
	after, err := rewrite(before, want)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	onlySumsChanged(t, before, after)

	again, err := readPins(after)
	if err != nil {
		t.Fatalf("the rewritten file does not parse: %v", err)
	}
	if again.Sums["amd64"] != want["amd64"] || again.Sums["arm64"] != want["arm64"] {
		t.Fatalf("rewritten sums = %v, want %v", again.Sums, want)
	}
	if again.Version != pins.Version {
		t.Errorf("rewrite changed the k0s version: %q -> %q", pins.Version, again.Version)
	}
	// The rest of the file is still there, not merely unchanged in length.
	for _, want := range []string{"authelia:", "envoyGateway:", "metallb:", "certManager:", "# renovate:"} {
		if !strings.Contains(string(after), want) {
			t.Errorf("the rewritten file lost %q", want)
		}
	}
}

// A value that is already right is left exactly as it was, comment and all: the
// maintainer's formatting is not this command's to normalise.
func TestRewriteLeavesAnUnchangedLineAlone(t *testing.T) {
	raw := []byte("k0s: v1.0.0\n" +
		`k0sSHA256amd64: "` + sum64("1") + `"  # checked 2026-01-01` + "\n" +
		"k0sSHA256arm64: " + sum64("2") + "\r\n")
	same, err := rewrite(raw, map[string]string{"amd64": sum64("1"), "arm64": sum64("2")})
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if string(same) != string(raw) {
		t.Fatalf("rewrite of unchanged sums changed the file:\n  before: %q\n  after:  %q", raw, same)
	}

	// And a change keeps the quoting style, the comment and the CRLF.
	out, err := rewrite(raw, map[string]string{"amd64": sum64("3"), "arm64": sum64("4")})
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	onlySumsChanged(t, raw, out)
	if !strings.Contains(string(out), `k0sSHA256amd64: "`+sum64("3")+`"  # checked 2026-01-01`) {
		t.Errorf("rewrite lost the quote or the comment: %q", out)
	}
	if !strings.HasSuffix(string(out), "k0sSHA256arm64: "+sum64("4")+"\r\n") {
		t.Errorf("rewrite lost the CRLF or the bare value: %q", out)
	}
}

// A file with no line for an architecture is refused rather than appended to: a
// second line for one key would leave box and the file disagreeing about which
// sum is the pin.
func TestRewriteRefusesAMissingLine(t *testing.T) {
	raw := []byte("k0s: v1.0.0\nk0sSHA256amd64: \"" + sum64("1") + "\"\n")
	if _, err := rewrite(raw, map[string]string{"amd64": sum64("1"), "arm64": sum64("2")}); err == nil || !strings.Contains(err.Error(), "k0sSHA256arm64") {
		t.Fatalf("rewrite with no arm64 line = %v, want it named", err)
	}
	if _, err := rewrite(raw, map[string]string{"amd64": sum64("1")}); err == nil {
		t.Fatal("rewrite with no checksum for one of the file's lines succeeded")
	}
}

// readPins is box's parser plus the promise that every architecture box installs
// is covered: a file whose sums do not parse is refused here, before a network
// call, with the key that is wrong.
func TestReadPinsRefusesADamagedFile(t *testing.T) {
	doc := func(amd, arm string) []byte {
		return []byte("authelia: \"1.0.0@sha256:" + sum64("c") + "\"\n" +
			"k0s: v1.0.0\n" +
			"k0sSHA256amd64: \"" + amd + "\"\n" +
			"k0sSHA256arm64: \"" + arm + "\"\n" +
			"envoyGateway: 1.0.0\nmetallb: 1.0.0\ncertManager: v1.0.0\n")
	}
	if _, err := readPins(doc(sum64("1"), "")); err == nil || !strings.Contains(err.Error(), `"k0sSHA256arm64"`) {
		t.Fatalf("readPins of a file with an empty checksum = %v, want the key named", err)
	}
	if _, err := readPins(doc(strings.Repeat("A", 64), sum64("2"))); err == nil || !strings.Contains(err.Error(), "not a sha256") {
		t.Fatalf("readPins of an uppercase checksum = %v, want it refused", err)
	}
	short := "authelia: \"1.0.0@sha256:" + sum64("c") + "\"\nk0s: v1.0.0\n" +
		"k0sSHA256amd64: \"" + sum64("1") + "\"\n" +
		"envoyGateway: 1.0.0\nmetallb: 1.0.0\ncertManager: v1.0.0\n"
	if _, err := readPins([]byte(short)); err == nil || !strings.Contains(err.Error(), `no "k0sSHA256arm64"`) {
		t.Fatalf("readPins of a file with no arm64 checksum = %v, want the missing key named", err)
	}
	pins, err := readPins(shippedFile(t))
	if err != nil {
		t.Fatalf("readPins of the shipped file: %v", err)
	}
	if pins.Version == "" || len(pins.Sums) != 2 {
		t.Fatalf("readPins of the shipped file = %+v", pins)
	}
}
