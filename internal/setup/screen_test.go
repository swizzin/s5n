package setup

import (
	"strings"
	"testing"
)

// The init screen says what box init took: the paths and what is on them, each
// name's certificate, the peer port, what the two files got, and the hand-over --
// one row per fact.
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
	// The path block is the prompt's note; a terminal draws the same table.
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
	// The two files are rows: a count of keys or the word for nothing to do.
	if !strings.Contains(h.out.String(), "✓ grant          10 keys") {
		t.Fatalf("the grant is not one row:\n%s", h.out)
	}
	// No line is a bare sentence about plumbing: paths and directories are the log's.
	for _, gone := range []string{"edited 12 keys", "created /srv/s5n/config"} {
		if strings.Contains(h.out.String(), gone) {
			t.Fatalf("the screen still carries %q:\n%s", gone, h.out)
		}
	}
}
