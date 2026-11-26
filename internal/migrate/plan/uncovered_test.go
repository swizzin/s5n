package plan

import (
	"strings"
	"testing"
)

func TestUncoveredErrorListsEveryGapSorted(t *testing.T) {
	err := &UncoveredError{Paths: []Uncovered{
		{App: "sonarr-4k", File: "b.fastresume", Key: "save_path", Path: "/mnt/one/a.mkv"},
		{App: "sonarr-4k", File: "a.fastresume", Key: "save_path", Path: "/mnt/two/b.mkv"},
		{App: "qbittorrent", File: "c.fastresume", Key: "mapped_files[2]", Path: "/mnt/three/c.mkv"},
		// The same gap found twice is one decision.
		{App: "qbittorrent", File: "c.fastresume", Key: "mapped_files[2]", Path: "/mnt/three/c.mkv"},
	}}
	got := err.Error()
	want := `3 paths no rule covers; add each to its app's paths in the plan:
  qbittorrent c.fastresume mapped_files[2]: /mnt/three/c.mkv
  sonarr-4k a.fastresume save_path: /mnt/two/b.mkv
  sonarr-4k b.fastresume save_path: /mnt/one/a.mkv`
	if got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestUncoveredErrorReadsAsOnePath(t *testing.T) {
	err := &UncoveredError{Paths: []Uncovered{{App: "qbittorrent", Path: "/mnt/one/a.mkv"}}}
	if got, want := err.Error(), "1 path no rule covers; add each to its app's paths in the plan:\n  qbittorrent: /mnt/one/a.mkv"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestUncoveredStringOmitsWhatItDoesNotKnow(t *testing.T) {
	tests := []struct {
		in   Uncovered
		want string
	}{
		{Uncovered{App: "qbittorrent", Path: "/a"}, "qbittorrent: /a"},
		{Uncovered{App: "qbittorrent", Key: "save_path", Path: "/a"}, "qbittorrent save_path: /a"},
		{Uncovered{App: "qbittorrent", File: "x.fastresume", Path: "/a"}, "qbittorrent x.fastresume: /a"},
	}
	for _, tt := range tests {
		if got := tt.in.String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}

func TestErrorJoinsProblems(t *testing.T) {
	one := &Error{Path: "plan.yaml", Problems: []string{"version 2: this box reads version 1"}}
	if got, want := one.Error(), "plan plan.yaml: version 2: this box reads version 1"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	many := &Error{Problems: []string{"one", "two"}}
	if got := many.Error(); !strings.HasPrefix(got, "plan:\n  one\n  two") {
		t.Errorf("Error() = %q, want the problems on their own lines", got)
	}
}
