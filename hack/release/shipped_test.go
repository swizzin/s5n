package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDecideOnTheShippedPlatformFile runs the decision over the platform.yaml
// the chart actually ships, not a fixture copy: the release job and box read the
// same file, so a format the decision can't parse would stop every automatic
// release the first time a pin moves.
func TestDecideOnTheShippedPlatformFile(t *testing.T) {
	old, err := os.ReadFile(filepath.Join("..", "..", platformFile))
	if err != nil {
		t.Fatal(err)
	}
	pins, err := parsePins(old)
	if err != nil {
		t.Fatalf("the shipped %s does not parse: %v", platformFile, err)
	}
	metallb, ok := pins["metallb"]
	if !ok {
		t.Fatalf("the shipped %s has no metallb pin: %v", platformFile, pins)
	}
	parts := strings.Split(metallb, ".")
	if len(parts) != 3 {
		t.Fatalf("metallb pin %q is not major.minor.patch", metallb)
	}
	bump := func(i int) []byte {
		p := append([]string(nil), parts...)
		p[i] += "9" // a larger number in that position, whatever it was
		if i == 1 {
			p[2] = "0"
		}
		line := "metallb: " + metallb
		next := "metallb: " + strings.Join(p, ".")
		if !strings.Contains(string(old), line) {
			t.Fatalf("no %q line in the shipped %s", line, platformFile)
		}
		return []byte(strings.Replace(string(old), line, next, 1))
	}
	variants := []byte("{}\n")
	changed := []string{platformFile}

	d, err := decideRange(changed, variants, variants, old, bump(2), "0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Release || d.Version != "0.3.1" {
		t.Errorf("a metallb patch on the shipped file: %+v, want a 0.3.1 release", d)
	}
	d, err = decideRange(changed, variants, variants, old, bump(1), "0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	if d.Release {
		t.Errorf("a metallb minor on the shipped file released: %+v", d)
	}
}
