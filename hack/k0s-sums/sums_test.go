package main

import (
	"strings"
	"testing"
)

// TestParseSumsTxt reads a release's sha256sums.txt: the plain two-column form,
// the "*" binary marker, CRLF and blank lines all mean the same thing, and
// anything else is refused rather than guessed at.
func TestParseSumsTxt(t *testing.T) {
	raw := "# a comment\n" +
		sum64("1") + "  k0s-v1.37.2+k0s.1-amd64\n" +
		sum64("2") + " *k0s-v1.37.2+k0s.1-arm64\r\n" +
		"\n"
	sums, err := parseSumsTxt([]byte(raw))
	if err != nil {
		t.Fatalf("parseSumsTxt: %v", err)
	}
	if sums["k0s-v1.37.2+k0s.1-amd64"] != sum64("1") || sums["k0s-v1.37.2+k0s.1-arm64"] != sum64("2") {
		t.Fatalf("parsed %v, want both assets keyed by name", sums)
	}

	for name, tc := range map[string]string{
		"a line that is not two columns": sum64("1") + "\n",
		"a sum that is not hex":          strings.Repeat("z", 64) + "  k0s-amd64\n",
		"a short sum":                    sum64("1")[:63] + "  k0s-amd64\n",
		"nothing at all":                 "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSumsTxt([]byte(tc)); err == nil {
				t.Fatalf("parseSumsTxt(%q) succeeded, want a refusal", tc)
			}
		})
	}
}

// TestCheckSums is the check's whole verdict: a file that agrees passes, a file
// that does not is reported per architecture with both sums, and a release whose
// asset and sha256sums.txt disagree is reported as the release's problem -- the
// file is not blamed for a download that is not the published file.
func TestCheckSums(t *testing.T) {
	good := map[string]string{"amd64": sum64("1"), "arm64": sum64("2")}
	pins := pinFile{Version: "v1.0.0", Sums: map[string]string{"amd64": sum64("1"), "arm64": sum64("2")}}

	if problems := checkSums(pins, good, good); len(problems) != 0 {
		t.Fatalf("checkSums on a file that matches = %v, want none", problems)
	}

	stale := pinFile{Version: "v1.0.0", Sums: map[string]string{"amd64": sum64("9"), "arm64": sum64("2")}}
	problems := checkSums(stale, good, good)
	if len(problems) != 1 || problems[0].Arch != "amd64" {
		t.Fatalf("checkSums on a stale sum = %v, want one problem for amd64", problems)
	}
	line := problems[0].String()
	for _, want := range []string{"k0sSHA256amd64", sum64("9"), sum64("1")} {
		if !strings.Contains(line, want) {
			t.Errorf("verdict %q does not name %q", line, want)
		}
	}
	if strings.Contains(line, "arm64") {
		t.Errorf("verdict %q blames arm64, which matches", line)
	}

	// The release's own two accounts disagree: nothing to judge the file
	// against, and the verdict says so rather than calling the pin wrong.
	broken := map[string]string{"amd64": sum64("1"), "arm64": sum64("8")}
	problems = checkSums(pins, broken, good)
	if len(problems) != 1 || problems[0].Arch != "arm64" {
		t.Fatalf("checkSums with an asset that does not match sha256sums.txt = %v, want one problem for arm64", problems)
	}
	if got := problems[0].String(); !strings.Contains(got, "sha256sums.txt") || !strings.Contains(got, "not the release's own file") {
		t.Errorf("verdict %q does not say the release is inconsistent", got)
	}
	if problems := releaseConsistent(broken, good); len(problems) != 1 || problems[0].Arch != "arm64" {
		t.Fatalf("releaseConsistent = %v, want the arm64 disagreement alone", problems)
	}
	if problems := releaseConsistent(good, good); len(problems) != 0 {
		t.Fatalf("releaseConsistent on a consistent release = %v, want none", problems)
	}
}

// TestAssetNamesAndURLs: the URL is the one box downloads from, built the way
// bootstrap builds it -- the tag as published, '+' and all.
func TestAssetNamesAndURLs(t *testing.T) {
	if got, want := assetName("v1.37.2+k0s.1", "amd64"), "k0s-v1.37.2+k0s.1-amd64"; got != want {
		t.Errorf("assetName = %q, want %q", got, want)
	}
	if got, want := assetURL("v1.37.2+k0s.1", "arm64"), "https://github.com/k0sproject/k0s/releases/download/v1.37.2+k0s.1/k0s-v1.37.2+k0s.1-arm64"; got != want {
		t.Errorf("assetURL = %q, want %q", got, want)
	}
	if got, want := sumsURL("v1.37.2+k0s.1"), "https://github.com/k0sproject/k0s/releases/download/v1.37.2+k0s.1/sha256sums.txt"; got != want {
		t.Errorf("sumsURL = %q, want %q", got, want)
	}
}
