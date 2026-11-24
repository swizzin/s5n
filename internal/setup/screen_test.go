package setup

import (
	"strings"
	"testing"
)

// The init screen is what `box init` says about the answers it took: where the
// two paths are and what is on them, which certificate each name gets, the port
// peers reach the box on, what the two files got, and what the hand-over does.
// One row per fact, and nothing a person would have to translate.
func TestTheInitScreenIsOneRowPerFact(t *testing.T) {
	h := newHarness(t)
	h.prompt.multis = [][]string{{"sonarr"}}
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	if _, err := h.run(h.options()); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	want := []string{
		"· box.example.com  self-signed",
		"owner: uid 1000, gid 1000",
		"✓ grant",
		"· values",
		"✓ directories",
	}
	// The path block is the prompt's note: the line prompt prints it, and a
	// terminal draws the same table.
	paths := strings.Join(h.prompt.notes, "\n")
	for _, want := range []string{"where things live", "  data    /srv/s5n/data    exists", "  config  /srv/s5n/config  exists"} {
		if !strings.Contains(paths, want) {
			t.Fatalf("the path block is missing %q:\n%s", want, paths)
		}
	}
	for _, want := range want {
		if !strings.Contains(h.out.String(), want) {
			t.Fatalf("the screen is missing %q:\n%s", want, h.out)
		}
	}
	// The two files are rows, not sentences: a count of keys or the word for
	// nothing to do.
	if !strings.Contains(h.out.String(), "✓ grant          10 keys") {
		t.Fatalf("the grant is not one row:\n%s", h.out)
	}
	// No line is a bare sentence about the run's plumbing: the files' paths and
	// each directory are the log's.
	for _, gone := range []string{"edited 12 keys", "created /srv/s5n/config"} {
		if strings.Contains(h.out.String(), gone) {
			t.Fatalf("the screen still carries %q:\n%s", gone, h.out)
		}
	}
}
