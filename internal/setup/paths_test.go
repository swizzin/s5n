package setup

import (
	"strings"
	"testing"
)

// TestPathCheckTextIsUnchanged pins the plain path report byte for byte. It is
// what setup.LinePrompt is handed (through Note) and what every existing test
// asserts fragments of; a terminal UI draws the same structure instead, but the
// text a line prompt prints must not move.
func TestPathCheckTextIsUnchanged(t *testing.T) {
	check := PathCheck{
		Reports: []PathReport{
			{
				Kind:  "data",
				Path:  "/srv/s5n/data",
				Mount: Mount{Major: 8, Minor: 2, Point: "/srv", FSType: "btrfs", Source: "/dev/sdb1"},
				Free:  900 * 1024 * 1024 * 1024,
			},
			{
				Kind:    "config",
				Path:    "/srv/s5n/config",
				Exists:  true,
				Owner:   "1000:1000",
				Mount:   Mount{Major: 8, Minor: 2, Point: "/srv", FSType: "btrfs", Source: "/dev/sdb1"},
				Free:    900 * 1024 * 1024 * 1024,
				Summary: "empty",
			},
		},
		Footer: pathCheckFooter,
	}
	const want = `paths
  data   /srv/s5n/data
          does not exist yet: it would be a new directory on device 8:2, mounted at /srv (btrfs), 900.0 GiB free
  config /srv/s5n/config
          device 8:2, mounted at /srv (btrfs), 900.0 GiB free
          owner 1000:1000
          empty
  downloads/<category> and media/<category> are made on the first start, owned as above`
	if got := check.Text(); got != want {
		t.Fatalf("the plain report changed:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestLinePromptStaysOnTheTextPath pins the other half of the seam: LinePrompt
// does not implement PathChecker, so init hands it Note(check.Text()) and its
// output stays exactly what it was before the structured report existed.
func TestLinePromptStaysOnTheTextPath(t *testing.T) {
	var p Prompter = &LinePrompt{}
	if _, ok := p.(PathChecker); ok {
		t.Fatal("LinePrompt now implements PathChecker: the line prompt's report would change")
	}
}

// The unattended init path uses a zero-value prompt: it asks nothing, but the
// path report still reaches Note. That must be silent, not a nil dereference.
func TestZeroLinePromptNoteDoesNotPanic(t *testing.T) {
	if err := (&LinePrompt{}).Note("a report"); err != nil {
		t.Fatalf("Note: %v", err)
	}
}

// TestInspectMarksTheRootDevice covers the warning a terminal UI draws: a path
// on a separate mount that is the same device as /. It is not in the plain
// text, so nothing here may leak into PathReport.String.
func TestInspectMarksTheRootDevice(t *testing.T) {
	h := newFakeHost()
	h.mounts = []Mount{
		{Major: 8, Minor: 1, Point: "/", FSType: "ext4"},
		{Major: 8, Minor: 1, Point: "/srv", FSType: "ext4"},
		{Major: 8, Minor: 2, Point: "/mnt/disk", FSType: "btrfs"},
	}
	h.dirs["/srv"] = true
	h.dirs["/mnt/disk"] = true
	d := &Decision{Host: h}

	same, err := d.inspect("data", "/srv/s5n/data", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if same.SameDevice != true || same.OnRoot != false {
		t.Fatalf("a path on /srv (8:1, like /) should share the root device: %+v", same)
	}
	other, err := d.inspect("data", "/mnt/disk/s5n/data", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if other.SameDevice || other.OnRoot {
		t.Fatalf("a path on another disk should be quiet: %+v", other)
	}
	if got := other.String(); strings.Contains(got, "shares") || strings.Contains(got, "root filesystem") {
		t.Fatalf("the plain report grew a warning it did not have before:\n%s", got)
	}
}
